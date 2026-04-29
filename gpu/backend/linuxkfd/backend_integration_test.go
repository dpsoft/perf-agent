package linuxkfd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

type eventSink struct {
	mu     sync.Mutex
	events []gpu.GPUTimelineEvent
}

func (s *eventSink) EmitLaunch(gpu.GPUKernelLaunch)   {}
func (s *eventSink) EmitExec(gpu.GPUKernelExec)       {}
func (s *eventSink) EmitCounter(gpu.GPUCounterSample) {}
func (s *eventSink) EmitSample(gpu.GPUSample)         {}
func (s *eventSink) EmitEvent(event gpu.GPUTimelineEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *eventSink) snapshot() []gpu.GPUTimelineEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]gpu.GPUTimelineEvent, len(s.events))
	copy(out, s.events)
	return out
}

func TestLinuxKFDLiveSmoke(t *testing.T) {
	requireBPFCapsForRootTest(t)

	hipLib, err := firstHIPLibraryPath()
	if err != nil {
		t.Skipf("no hip library path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	shimPath := buildHIPLaunchShim(t, tmpDir)
	logPath := filepath.Join(tmpDir, "hip-kfd-shim.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	defer logFile.Close()

	shimCmd := exec.CommandContext(ctx, shimPath)
	shimCmd.Stdout = logFile
	shimCmd.Stderr = logFile
	shimCmd.Env = append(os.Environ(),
		"HIP_LAUNCH_SHIM_LIBRARY="+hipLib,
		"HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS=10000",
		"HIP_LAUNCH_SHIM_SLEEP_AFTER_MS=60000",
	)
	if err := shimCmd.Start(); err != nil {
		t.Fatalf("start hip shim: %v", err)
	}
	defer func() {
		if shimCmd.ProcessState == nil || !shimCmd.ProcessState.Exited() {
			_ = shimCmd.Process.Kill()
			_, _ = shimCmd.Process.Wait()
		}
	}()

	backend, err := New(Config{PID: shimCmd.Process.Pid})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer backend.Close()

	sink := &eventSink{}
	if err := backend.Start(ctx, sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(3 * time.Second)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := backend.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	events := sink.snapshot()
	if len(events) == 0 {
		t.Fatal("expected linuxkfd events")
	}
	var foundKFD bool
	for _, event := range events {
		if event.Backend != gpu.BackendLinuxKFD {
			t.Fatalf("event backend=%q want %q", event.Backend, gpu.BackendLinuxKFD)
		}
		if event.Family == "kfd" {
			foundKFD = true
		}
	}
	if !foundKFD {
		t.Fatalf("expected at least one kfd-family event, got %v", events)
	}
}

func requireBPFCapsForRootTest(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	caps := cap.GetProc()
	hasBPF, err := caps.GetFlag(cap.Permitted, cap.BPF)
	if err != nil || !hasBPF {
		t.Skip("CAP_BPF not in permitted set")
	}
	hasPerfmon, err := caps.GetFlag(cap.Permitted, cap.PERFMON)
	if err != nil || !hasPerfmon {
		t.Skip("CAP_PERFMON not in permitted set")
	}
}

func firstHIPLibraryPath() (string, error) {
	candidates := []string{
		"/usr/local/lib/ollama/rocm/libamdhip64.so.6.3.60303",
		"/usr/local/lib/ollama/rocm/libamdhip64.so.6",
		"/opt/rocm/lib/libamdhip64.so",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no hip library found")
}

func buildHIPLaunchShim(t *testing.T, dir string) string {
	t.Helper()

	binaryPath := filepath.Join(dir, "gpu-hip-launch-shim")
	buildCmd := exec.Command(
		"cc",
		"-O2",
		"-g",
		"-Wall",
		"-Wextra",
		filepath.Join("..", "..", "..", "scripts", "hip-launch-shim.c"),
		"-ldl",
		"-o",
		binaryPath,
	)
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build hip shim: %v\n%s", err, buildOut)
	}
	return binaryPath
}
