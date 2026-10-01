package dwarfagent_test

import (
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

	// The workload is spawned AFTER the profiler below, not here, and it
	// burns CPU rather than sleeping. Both of those are the fix for #179.
	//
	// This used to spawn `sleep 30`, with the comment that it was there "to
	// ensure /proc has at least one non-self PID visible during the lazy
	// scan" -- enrollment. But a sleeping process is never ON CPU, so it is
	// never sampled, so no PC ever lands in its mapping and it can never
	// produce a CFI miss. Every miss this test observed came incidentally
	// from whatever else happened to be running on the machine, which on a
	// quiet CI runner is nothing. That is why it passed on amd64 and failed
	// on arm64 in the same commit, and then passed on arm64 in the next
	// run: the arch was never the variable, machine activity was.
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

	// NOW spawn the workload, so its binary is enrolled LAZILY -- the case
	// this mode exists for -- rather than during the profiler's startup
	// scan. rust-workload is CPU-bound and carries .eh_frame, so it is both
	// sampled and in need of CFI, and it is not already running on the
	// machine, so its tables cannot have been compiled beforehand.
	binPath := "../../test/workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	workload := exec.Command(binPath, "20", "2")
	if err := workload.Start(); err != nil {
		t.Fatalf("start rust workload: %v", err)
	}
	t.Cleanup(func() { _ = workload.Process.Kill(); _ = workload.Wait() })

	// Poll rather than sleep a fixed window. The miss is a race by nature --
	// it exists only between a binary being enrolled and its CFI being
	// compiled -- so a fixed sleep either wastes time or lands outside the
	// window. Waiting for the condition with a deadline does neither, and a
	// profiler that genuinely never fires one still fails, at the deadline,
	// with the counters printed.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st := prof.MissStats(); st.Received > 0 && st.Resolved > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

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
