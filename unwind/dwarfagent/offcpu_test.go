package dwarfagent_test

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

// TestOffCPUProfilerEndToEnd runs the dwarfagent off-CPU profiler
// against the rust-workload and asserts the resulting pprof has
// samples with non-zero blocking-ns. Uses rust (not go) because Go
// binaries have no .eh_frame (Go uses .gopclntab) — ehcompile needs
// .eh_frame to produce CFI. Even though rust-workload is CPU-bound,
// its threads get context-switched by the kernel scheduler routinely
// enough to fire off-CPU samples.
func TestOffCPUProfilerEndToEnd(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	binPath := "../../test/workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "10", "2")
	if err := workload.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(1 * time.Second)

	p, err := dwarfagent.NewOffCPUProfiler(workload.Process.Pid, nil)
	if err != nil {
		t.Fatalf("NewOffCPUProfiler: %v", err)
	}

	// Let off-CPU samples accumulate. 3 seconds of wall-time gives the
	// scheduler plenty of opportunity to context-switch our CPU-bound
	// workers (timer tick, kernel workqueues, other userspace activity).
	time.Sleep(3 * time.Second)

	var buf bytes.Buffer
	if err := p.Collect(&buf); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Logf("Close (non-fatal): %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("Collect produced 0 bytes")
	}
	prof, err := profile.Parse(&buf)
	if err != nil {
		t.Fatalf("parse pprof: %v", err)
	}
	if len(prof.Sample) == 0 {
		t.Fatal("pprof has no samples")
	}
	var totalNs int64
	for _, s := range prof.Sample {
		for _, v := range s.Value {
			totalNs += v
		}
	}
	if totalNs == 0 {
		t.Fatal("pprof samples all have zero value — no blocking time accumulated")
	}
	t.Logf("off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}
