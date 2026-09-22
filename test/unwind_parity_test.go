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
// The assertion is on the OUTERMOST frame, because that is what an order
// mistake moves. Both unwinders must place a process entry point there.
func TestUnwindersAgreeOnStackOrder(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	const window = 5 * time.Second
	outermost := func(p *profile.Profile) map[string]int {
		got := map[string]int{}
		for _, s := range p.Sample {
			if len(s.Location) == 0 {
				continue
			}
			// Root-first is this repo's convention: Location[0] is outermost.
			loc := s.Location[0]
			if len(loc.Line) == 0 || loc.Line[0].Function == nil {
				got["<unsymbolized>"]++
				continue
			}
			got[loc.Line[0].Function.Name]++
		}
		return got
	}
	// Strictly the frames a thread can actually START at. An earlier version
	// of this test accepted any "runtime." prefix, which a Go workload also
	// has at its LEAVES (runtime.epollwait, runtime.futex...) -- so
	// leaf-first stacks scored hits and the test passed on a binary with
	// issue #155 still in it.
	isEntryPoint := func(n string) bool {
		switch n {
		// Thread and goroutine roots.
		case "_start", "runtime.goexit", "runtime.mstart", "runtime.rt0_go", "main":
			return true
		// g0-stack roots. runtime.mcall and runtime.systemstack switch stacks,
		// and no unwinder can walk across the switch -- so when a sample lands
		// on the g0 stack these ARE the outermost visible frame, not evidence
		// of a reversed profile. Measured on arm64 CI: goexit 7, systemstack 5,
		// mcall 3 out of 15 samples, all three correct.
		case "runtime.mcall", "runtime.systemstack":
			return true
		}
		return strings.HasPrefix(n, "__libc_start")
	}

	for _, unwind := range []string{"fp", "dwarf"} {
		p := captureCPU(t, cmd.Process.Pid, unwind, window)
		roots := outermost(p)
		if len(roots) == 0 {
			t.Skipf("no samples from --unwind %s; nothing to assert", unwind)
		}
		var entry, other int
		for n, c := range roots {
			if isEntryPoint(n) {
				entry += c
			} else {
				other += c
			}
		}
		// A PROPORTION, not "at least one". An existence check is what let
		// TestKernelStackResolution sit green through issue #156 for as long
		// as it did: a handful of samples can root correctly by accident
		// while the profile as a whole is stored the wrong way round.
		const floor = 0.5
		total := entry + other
		if total == 0 {
			t.Skipf("--unwind %s: no usable samples; nothing to assert", unwind)
		}
		if got := float64(entry) / float64(total); got < floor {
			t.Fatalf("--unwind %s: only %d of %d samples (%.0f%%) have a process entry point as "+
				"their OUTERMOST frame, want >= %.0f%%; the profile is stored leaf-first while "+
				"this repo's consumers read root-first (issue #155). outermost frames seen: %v",
				unwind, entry, total, got*100, floor*100, roots)
		}
		t.Logf("%s: %d of %d samples root at an entry point", unwind, entry, total)
	}
}
