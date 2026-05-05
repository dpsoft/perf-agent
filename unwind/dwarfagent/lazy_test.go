package dwarfagent_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"kernel.org/pub/linux/libs/security/libcap/cap"

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
	prof, err := dwarfagent.NewProfilerWithMode(0, true, cpus, nil, 99, nil, dwarfagent.ModeLazy, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewProfilerWithMode: %v", err)
	}

	// Attach window: 5 seconds. Walker fires repeatedly; each FP_LESS+miss
	// emits a rate-limited ringbuf event; drainer resolves and compiles.
	time.Sleep(5 * time.Second)

	// Snapshot stats before close so we don't race with drainer teardown.
	pre := prof.MissStats()

	if err := prof.Close(); err != nil {
		t.Logf("prof.Close (non-fatal): %v", err)
	}

	post := prof.MissStats()

	if post.Received == 0 {
		t.Errorf("MissStats.Received == 0; expected at least one CFI miss event")
	}
	if post.Resolved == 0 {
		t.Errorf("MissStats.Resolved == 0; expected at least one lazy compile to succeed")
	}
	t.Logf("MissStats pre/post: pre.Received=%d post.Received=%d post.Resolved=%d post.PoisonedKeys=%d",
		pre.Received, post.Received, post.Resolved, post.PoisonedKeys)
}
