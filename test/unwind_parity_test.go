package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

// kernelMapping is the sentinel mapping perf-agent routes kernel frames
// through. It is the only channel that identifies a kernel frame from the
// profile itself rather than by guessing at a symbol name.
const kernelMapping = "[kernel]"

// sampleReachesUserspace reports whether a sample has at least one frame
// from a user mapping. A kernel address is not userspace however well it
// symbolizes, which is the whole point of issue #156.
func sampleReachesUserspace(s *profile.Sample) bool {
	for _, loc := range s.Location {
		for _, ln := range loc.Line {
			if ln.Function == nil {
				continue
			}
			n := ln.Function.Name
			if strings.HasPrefix(n, "main.") || strings.HasPrefix(n, "runtime.") ||
				n == "main" || n == "_start" || strings.HasPrefix(n, "__libc_start") {
				return true
			}
		}
	}
	return false
}

// userspaceReach is the fraction of samples whose stack reaches userspace.
//
// A fraction, not a boolean. The pre-existing TestKernelStackResolution
// asserts only that SOME user frame exists anywhere in the profile, and
// retries until it finds one -- so it passed throughout issue #156, when
// measurement showed 1 sample in 51 reaching userspace. An assertion that
// cannot fail is not covering anything.
func userspaceReach(p *profile.Profile) (reached, total int) {
	for _, s := range p.Sample {
		total++
		if sampleReachesUserspace(s) {
			reached++
		}
	}
	return reached, total
}

func captureCPU(t *testing.T, pid int, unwind string, window time.Duration) *profile.Profile {
	t.Helper()
	out := filepath.Join(t.TempDir(), "cpu-"+unwind+".pb.gz")
	agent := exec.Command(getAgentPath(t),
		"--profile", "--kernel-stacks",
		"--unwind", unwind,
		"--pid", strconv.Itoa(pid),
		"--duration", window.String(),
		"--profile-output", out,
	)
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	agent.Env = append(os.Environ(), "DEBUGINFOD_URLS=")
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent --unwind %s: %v", unwind, err)
	}
	p, err := readProfile(out)
	if err != nil {
		t.Fatalf("read %s profile: %v", unwind, err)
	}
	return p
}

// TestDwarfKeepsUserspaceOnSyscallBoundWorkload is the regression test for
// issue #156.
//
// A perf_event program's ctx holds the registers at the instant the counter
// overflowed. On a syscall-bound workload that instant is usually inside the
// kernel, so unwinding the "user" stack from ctx->regs starts at a kernel IP,
// finds no user mapping, and yields nothing -- while the kernel half still
// symbolizes perfectly, which is what made the profile look healthy.
// bpf_task_pt_regs() returns the task's user-mode registers whatever mode the
// sample landed in.
//
// The workload is deliberately I/O bound: a CPU-bound one samples in user
// mode almost every time and cannot distinguish the two register sources.
func TestDwarfKeepsUserspaceOnSyscallBoundWorkload(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	const window = 5 * time.Second
	p := captureCPU(t, cmd.Process.Pid, "dwarf", window)

	reached, total := userspaceReach(p)
	if total == 0 {
		t.Skipf("no samples in %s; nothing to assert", window)
	}
	// Some samples legitimately have no user half -- a task can be sampled
	// deep in kernel work with no user frame worth naming. The defect was a
	// near-total loss, so the bar is a clear majority rather than perfection.
	const floor = 0.5
	if got := float64(reached) / float64(total); got < floor {
		t.Fatalf("only %d of %d DWARF samples (%.0f%%) reach userspace, want >= %.0f%%; "+
			"issue #156: the user walk is starting from kernel registers. %s",
			reached, total, got*100, floor*100, describeProfile(p))
	}
	t.Logf("dwarf: %d of %d samples (%.0f%%) reach userspace", reached, total,
		float64(reached)/float64(total)*100)
}

// TestUnwindersAgreeOnStackOrder is the regression test for issue #155.
//
// The FP builder called pprof.Reverse and the DWARF builder did not, so the
// two shipped profiles in opposite orders while every consumer -- including
// internal/foldedstacks and the flame graph renderer -- assumed one of them.
// Nothing compared the two, so the divergence was invisible: each profile is
// internally consistent and only a cross-check can see it.
//
// The assertion is that no sample's OUTERMOST frame lies in the kernel. A
// user task's stack cannot BEGIN in the kernel -- userspace calls in, never
// the reverse -- so a kernel-mapped root is proof the stack is stored the
// wrong way round. perf-agent routes kernel frames through a "[kernel]"
// sentinel mapping, so this reads the mapping rather than guessing from
// symbol names.
//
// Earlier drafts of this test asserted that a majority of samples root at a
// known entry point, which was both too weak and too brittle: too weak
// because "at least one" passes on a wholly reversed profile, and too
// brittle because the set of legitimate Go roots is open-ended --
// runtime.goexit, runtime.mcall, runtime.systemstack and runtime.morestack
// all switch stacks, and a walk cannot cross a stack switch. Chasing that
// list failed on arm64 CI against the FP unwinder, which was never wrong.
// The kernel-root invariant needs no such list and admits no threshold.
func TestUnwindersAgreeOnStackOrder(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	const window = 5 * time.Second
	for _, unwind := range []string{"fp", "dwarf"} {
		p := captureCPU(t, cmd.Process.Pid, unwind, window)
		if len(p.Sample) == 0 {
			t.Skipf("--unwind %s: no samples; nothing to assert", unwind)
		}

		roots := map[string]int{}
		var kernelRooted int
		var example *profile.Sample
		for _, s := range p.Sample {
			if len(s.Location) == 0 {
				continue
			}
			file := "<no mapping>"
			if m := s.Location[0].Mapping; m != nil {
				file = m.File
			}
			roots[file]++
			if file == kernelMapping {
				kernelRooted++
				if example == nil {
					example = s
				}
			}
		}

		if kernelRooted > 0 {
			t.Errorf("--unwind %s: %d of %d samples have a KERNEL frame as their outermost "+
				"location, which a user task's stack can never have -- the profile is stored "+
				"leaf-first while this repo's consumers read root-first (issue #155).\n"+
				"  root mappings: %v\n  one such stack, as stored:\n%s",
				unwind, kernelRooted, len(p.Sample), roots, renderSample(example))
			continue
		}
		t.Logf("%s: %d samples, every one rooted in a user mapping %v", unwind, len(p.Sample), roots)
	}
}
