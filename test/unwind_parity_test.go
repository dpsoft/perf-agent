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
		case "_start", "runtime.goexit", "runtime.mstart", "runtime.rt0_go", "main":
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
