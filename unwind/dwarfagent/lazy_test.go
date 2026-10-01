package dwarfagent_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

// TestLazyMode_FiresAndCompilesOnMiss verifies the full lazy round-trip:
// ScanAndEnroll populates pid_mappings, first sample miss fires a
// ringbuf event, drainer compiles, MissStats reflects Resolved >= 1
// after a settle window.
//
// Caps-gated. Built with the CGO + rpath flags so it works after setcap.
func TestLazyMode_FiresAndCompilesOnMiss(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}

	// Spawn a workload to ensure /proc has at least one non-self PID
	// visible during the lazy scan. Even though we're going system-wide,
	// some test environments may have minimal /proc; spawning ensures
	// there's at least one binary to enroll + compile.
	sleepPath := "/usr/bin/sleep"
	if _, err := os.Stat(sleepPath); err != nil {
		sleepPath = "/bin/sleep"
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for /proc visibility.
	pid := cmd.Process.Pid
	for range 50 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/maps", pid)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Construct system-wide lazy profiler. Per-PID lazy falls back to
	// eager inside NewProfilerWithMode, so we use systemWide=true.
	cpus := make([]uint, runtime.NumCPU())
	for i := range cpus {
		cpus[i] = uint(i)
	}
	prof, err := dwarfagent.NewProfilerWithMode(0, true, cpus, nil, 99, nil, dwarfagent.ModeLazy, nil, nil, nil, newTestSymbolizer(t), symbolize.NoopKernelSymbolizer{}, false)
	if err != nil {
		t.Fatalf("NewProfilerWithMode: %v", err)
	}

	// Attach window: 5 seconds. Walker fires repeatedly; each FP_LESS+miss
	// emits a rate-limited ringbuf event; drainer resolves and compiles.
	time.Sleep(5 * time.Second)

	// Enrollment counts, snapshotted with the miss stats.
	//
	// A miss can only fire when the sampled PC's mapping is in pid_mappings
	// AND cfi_lengths has no table for it yet (bpf/unwind_common.h:816).
	// Zero misses therefore has three possible causes, and the raw counter
	// cannot tell them apart:
	//
	//   1. nothing was enrolled      -> binaryCount == 0
	//   2. everything was compiled   -> binaries enrolled, no miss window
	//   3. nothing was sampled in an enrolled mapping
	//
	// Without these numbers the failure says only "Received == 0", which is
	// what made issue #179 a guess. On arm64 this test fails and on amd64 it
	// passes; which of the three it is decides whether that is correct
	// behaviour or a real gap in lazy mode.
	pidCount, binaryCount := prof.AttachStats()
	// Snapshot stats before close so we don't race with drainer teardown.
	pre := prof.MissStats()

	if err := prof.Close(); err != nil {
		t.Logf("prof.Close (non-fatal): %v", err)
	}

	post := prof.MissStats()

	t.Logf("arch=%s enrolled: pids=%d binaries=%d", runtime.GOARCH, pidCount, binaryCount)
	t.Logf("MissStats pre/post: pre.Received=%d post.Received=%d post.Resolved=%d post.PoisonedKeys=%d",
		pre.Received, post.Received, post.Resolved, post.PoisonedKeys)

	if binaryCount == 0 {
		t.Fatalf("no binaries were enrolled on %s, so no CFI miss could fire regardless of "+
			"lazy mode: pids=%d. This is an enrollment failure, not a lazy-mode one (#179)",
			runtime.GOARCH, pidCount)
	}
	if post.Received == 0 {
		t.Errorf("MissStats.Received == 0 with %d binaries enrolled across %d pids; "+
			"expected at least one CFI miss event. Either every enrolled binary was already "+
			"compiled (no lazy window) or no sample landed in one (#179)",
			binaryCount, pidCount)
	}
	if post.Resolved == 0 && post.Received > 0 {
		t.Errorf("MissStats.Resolved == 0 with Received=%d; misses fired but no lazy compile "+
			"succeeded", post.Received)
	}
}
