package dwarfagent_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

// TestProfilerEndToEnd runs the full dwarfagent stack against the
// rust-workload and asserts that the resulting pprof contains at
// least one sample naming cpu_intensive_work.
func TestProfilerEndToEnd(t *testing.T) {
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

	cpus := make([]uint, 0)
	for i := range numOnlineCPUs() {
		cpus = append(cpus, uint(i))
	}

	p, err := dwarfagent.NewProfiler(workload.Process.Pid, false, cpus, nil, 99, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}

	// Sample for 3s.
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
	hit := false
	for _, fn := range prof.Function {
		if strings.Contains(fn.Name, "cpu_intensive_work") {
			hit = true
			break
		}
	}
	if !hit {
		names := make([]string, 0, min(10, len(prof.Function)))
		for i, fn := range prof.Function {
			if i >= 10 {
				break
			}
			names = append(names, fn.Name)
		}
		t.Fatalf("no function named *cpu_intensive_work* in pprof; first few: %v", names)
	}
}

// TestNewProfilerWithHooks_FiresOnCompile spawns a victim PID and
// verifies that NewProfilerWithHooks routes per-binary compile events
// through to the user-supplied OnCompile callback. This is the only
// end-to-end test of the hook plumbing across NewProfilerWithHooks →
// newSession → TableStore.AcquireBinary → ehcompile.Compile → OnCompile.
func TestNewProfilerWithHooks_FiresOnCompile(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}

	// Spawn a tiny workload we can attach to. /usr/bin/sleep is universal
	// enough; on systems without it, fall back to /bin/sleep.
	sleepPath := "/usr/bin/sleep"
	if _, err := os.Stat(sleepPath); err != nil {
		sleepPath = "/bin/sleep"
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for the process to be visible in /proc.
	pid := cmd.Process.Pid
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/maps", pid)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var (
		mu      sync.Mutex
		fires   int
		paths   []string
		ehBytes int
		anyDur  bool
	)
	hooks := &dwarfagent.Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			fires++
			paths = append(paths, path)
			ehBytes += ehFrameBytes
			if dur > 0 {
				anyDur = true
			}
		},
	}

	prof, err := dwarfagent.NewProfilerWithHooks(pid, false, []uint{0}, nil, 99, hooks, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewProfilerWithHooks: %v", err)
	}
	// Close before reading recorded state — drains the tracker goroutine
	// so OnCompile cannot fire concurrently with our assertions.
	if err := prof.Close(); err != nil {
		t.Logf("prof.Close (non-fatal): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if fires == 0 {
		t.Fatalf("OnCompile never fired (expected ≥1 for sleep + its shared libs)")
	}
	if len(paths) != fires {
		t.Errorf("len(paths)=%d != fires=%d — TableStore dedupe invariant violated", len(paths), fires)
	}
	if !anyDur {
		t.Errorf("all OnCompile durations were zero — timing wrap may not be in effect")
	}
	if ehBytes == 0 {
		t.Errorf("OnCompile total ehFrameBytes was 0 — section size not propagating")
	}
	t.Logf("OnCompile fired %d times across %d binaries, total .eh_frame bytes %d", fires, len(paths), ehBytes)
}

// numOnlineCPUs reads /sys/devices/system/cpu/online and returns the
// count of online CPUs. Falls back to 1 on error.
func numOnlineCPUs() int {
	data, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return 1
	}
	count := 0
	for _, part := range strings.Split(strings.TrimSpace(string(data)), ",") {
		if hy := strings.Index(part, "-"); hy >= 0 {
			a := part[:hy]
			b := part[hy+1:]
			var ai, bi int
			for _, c := range a {
				ai = ai*10 + int(c-'0')
			}
			for _, c := range b {
				bi = bi*10 + int(c-'0')
			}
			count += bi - ai + 1
		} else {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}
