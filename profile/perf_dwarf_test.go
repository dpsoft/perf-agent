package profile

import (
	"os"
	"testing"

	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// TestPerfDwarfLoads loads the perf_dwarf BPF program and asserts the kernel
// verifier accepts it. Run via `sudo make test-unit` or on a capped binary;
// skipped otherwise.
//
// This is the S2 smoke test: it exercises nothing user-visible, but catches
// arch-specific verifier regressions (e.g. an arm64 port that passes
// `go generate` but emits verifier-invalid BPF). Per-arch coverage comes
// from running this test under the matching arch's CI runner.
func TestPerfDwarfLoads(t *testing.T) {
	requireBPFCaps(t)

	objs, err := LoadPerfDwarf()
	if err != nil {
		t.Fatalf("load perf_dwarf: %v", err)
	}
	t.Cleanup(func() { _ = objs.Close() })

	if objs.Program() == nil {
		t.Fatal("Program() returned nil after successful load")
	}
	if objs.RingbufMap() == nil {
		t.Fatal("RingbufMap() returned nil after successful load")
	}
}

// requireBPFCaps skips the test unless the process can load BPF programs.
// Root bypasses the check; otherwise we look for CAP_BPF in the permitted
// set, which is what's needed to raise it into effective.
func requireBPFCaps(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	caps := cap.GetProc()
	have, err := caps.GetFlag(cap.Permitted, cap.BPF)
	if err != nil {
		t.Skipf("check caps: %v", err)
	}
	if !have {
		t.Skip("CAP_BPF not in permitted set; run as root or setcap the test binary")
	}
	// Having CAP_BPF alone isn't enough — LoadPerfDwarf also raises
	// CAP_PERFMON/SYS_PTRACE/etc. Check those too to avoid confusing
	// "set capabilities" failures inside the loader.
	for _, c := range []cap.Value{cap.SYS_ADMIN, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE} {
		have, err := caps.GetFlag(cap.Permitted, c)
		if err != nil {
			t.Skipf("check caps: %v", err)
		}
		if !have {
			t.Skipf("%v not in permitted set", c)
		}
	}
}
