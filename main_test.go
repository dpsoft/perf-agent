package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/perfagent"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

func TestBuildOptionsGPUStreamMode(t *testing.T) {
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUStreamStdin = true
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 0
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPUHostReplayPlusStreamMode(t *testing.T) {
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUStreamStdin = true
	*flagGPUHostReplayInput = "gpu/testdata/host/replay/flash_attn_launches.json"
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 0
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPULinuxDRMMode(t *testing.T) {
	prevLinuxDRM := *flagGPULinuxDRM
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPULinuxDRM = prevLinuxDRM
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPULinuxDRM = true
	*flagGPUStreamStdin = false
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 123
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPULinuxKFDMode(t *testing.T) {
	prevLinuxKFD := *flagGPULinuxKFD
	prevLinuxDRM := *flagGPULinuxDRM
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPULinuxKFD = prevLinuxKFD
		*flagGPULinuxDRM = prevLinuxDRM
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPULinuxKFD = true
	*flagGPULinuxDRM = false
	*flagGPUStreamStdin = false
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 123
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPUAMDSampleMode(t *testing.T) {
	prevAMDSample := *flagGPUAMDSampleStdin
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUAMDSampleStdin = prevAMDSample
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUAMDSampleStdin = true
	*flagGPUStreamStdin = false
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 0
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPUHostHIPPlusStreamMode(t *testing.T) {
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevHostHIPLibrary := *flagGPUHostHIPLibrary
	prevHostHIPSymbol := *flagGPUHostHIPSymbol
	prevGPUFoldedOutput := *flagGPUFoldedOutput
	prevLinuxDRM := *flagGPULinuxDRM
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagGPUHostHIPLibrary = prevHostHIPLibrary
		*flagGPUHostHIPSymbol = prevHostHIPSymbol
		*flagGPUFoldedOutput = prevGPUFoldedOutput
		*flagGPULinuxDRM = prevLinuxDRM
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUStreamStdin = true
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagGPUHostHIPLibrary = "/opt/rocm/lib/libamdhip64.so"
	*flagGPUHostHIPSymbol = "hipLaunchKernel"
	*flagGPUFoldedOutput = ""
	*flagGPULinuxDRM = false
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 123
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPUHostHIPPlusLinuxDRMMode(t *testing.T) {
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevHostHIPLibrary := *flagGPUHostHIPLibrary
	prevHostHIPSymbol := *flagGPUHostHIPSymbol
	prevHIPLinuxDRMJoin := *flagGPUHIPLinuxDRMJoin
	prevLinuxDRM := *flagGPULinuxDRM
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagGPUHostHIPLibrary = prevHostHIPLibrary
		*flagGPUHostHIPSymbol = prevHostHIPSymbol
		*flagGPUHIPLinuxDRMJoin = prevHIPLinuxDRMJoin
		*flagGPULinuxDRM = prevLinuxDRM
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUStreamStdin = false
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagGPUHostHIPLibrary = "/opt/rocm/lib/libamdhip64.so"
	*flagGPUHostHIPSymbol = "hipLaunchKernel"
	*flagGPUHIPLinuxDRMJoin = 3 * time.Millisecond
	*flagGPULinuxDRM = true
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 123
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestGPUEventBackendLineForLinuxDRMMode(t *testing.T) {
	agent, err := perfagent.New(
		perfagent.WithPID(123),
		perfagent.WithGPULinuxDRM(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer agent.Close()

	got, err := gpuEventBackendLine(agent)
	if err != nil {
		t.Fatalf("gpuEventBackendLine: %v", err)
	}
	const want = "GPU event backends: linuxdrm, linuxkfd"
	if got != want {
		t.Fatalf("gpuEventBackendLine()=%q want %q", got, want)
	}
}

func TestGPUEventBackendLineForLinuxKFDMode(t *testing.T) {
	agent, err := perfagent.New(
		perfagent.WithPID(123),
		perfagent.WithGPULinuxKFD(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer agent.Close()

	got, err := gpuEventBackendLine(agent)
	if err != nil {
		t.Fatalf("gpuEventBackendLine: %v", err)
	}
	const want = "GPU event backends: linuxkfd"
	if got != want {
		t.Fatalf("gpuEventBackendLine()=%q want %q", got, want)
	}
}

func TestGPUEventBackendLineForDynamicGPUStreamMode(t *testing.T) {
	agent, err := perfagent.New(perfagent.WithGPUStreamInput(strings.NewReader("")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer agent.Close()

	got, err := gpuEventBackendLine(agent)
	if err != nil {
		t.Fatalf("gpuEventBackendLine: %v", err)
	}
	if got != "" {
		t.Fatalf("gpuEventBackendLine()=%q want empty string", got)
	}
}

func TestGPUOfflineDemoScriptDryRunHostExec(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"host-exec",
		"/tmp/gpu-demo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run host-exec: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"--gpu-host-replay-input gpu/testdata/host/replay/flash_attn_launches.json",
		"--gpu-replay-input gpu/testdata/replay/host_exec_sample.json",
		"--gpu-attribution-output /tmp/gpu-demo/host_exec_sample.attributions.json",
		"--gpu-folded-output /tmp/gpu-demo/host_exec_sample.folded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptDryRunHIPAMDSample(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"hip-amd-sample",
		"/tmp/gpu-amd-demo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run hip-amd-sample: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"--gpu-host-replay-input gpu/testdata/host/replay/hip_kfd_launches.json",
		"--gpu-amd-sample-stdin",
		"--gpu-attribution-output /tmp/gpu-amd-demo/amd_sample_exec.attributions.json",
		"--gpu-folded-output /tmp/gpu-amd-demo/amd_sample_exec.folded",
		"< gpu/testdata/replay/amd_sample_exec.ndjson",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptDryRunLiveHIPAMDSample(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"live-hip-amdsample",
		"/tmp/gpu-live-amd-demo",
		"--pid",
		"4242",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run live-hip-amdsample: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"--pid 4242",
		"--gpu-amd-sample-stdin",
		"--gpu-host-hip-library /opt/rocm/lib/libamdhip64.so",
		"--gpu-attribution-output /tmp/gpu-live-amd-demo/live_hip_amdsample.attributions.json",
		"--gpu-folded-output /tmp/gpu-live-amd-demo/live_hip_amdsample.folded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptDryRunLiveHIPLinuxDRM(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"live-hip-linuxdrm",
		"/tmp/gpu-live-demo",
		"--pid",
		"4242",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--join-window",
		"7ms",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run live-hip-linuxdrm: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"--pid 4242",
		"--gpu-linux-drm",
		"--gpu-host-hip-library /opt/rocm/lib/libamdhip64.so",
		"--gpu-hip-linuxdrm-join-window 7ms",
		"--gpu-attribution-output /tmp/gpu-live-demo/live_hip_linuxdrm.attributions.json",
		"--gpu-folded-output /tmp/gpu-live-demo/live_hip_linuxdrm.folded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptDryRunLiveHIPLinuxDRMUsesEnvLibrary(t *testing.T) {
	fakeDir := t.TempDir()
	fakeLib := filepath.Join(fakeDir, "libamdhip64.so")
	if err := os.WriteFile(fakeLib, []byte(""), 0o644); err != nil {
		t.Fatalf("write fake hip library: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"live-hip-linuxdrm",
		"/tmp/gpu-live-demo",
		"--pid",
		"4242",
	)
	cmd.Env = append(os.Environ(), "PERF_AGENT_HIP_LIBRARY="+fakeLib)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run live-hip-linuxdrm env lib: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--gpu-host-hip-library "+fakeLib) {
		t.Fatalf("missing env-discovered hip library in output:\n%s", out)
	}
}

func TestGPUOfflineDemoScriptDryRunLiveHIPLinuxKFD(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"live-hip-linuxkfd",
		"/tmp/gpu-live-demo",
		"--pid",
		"4242",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--join-window",
		"7ms",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run live-hip-linuxkfd: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"--pid 4242",
		"--gpu-linux-kfd",
		"--gpu-host-hip-library /opt/rocm/lib/libamdhip64.so",
		"--gpu-hip-linuxdrm-join-window 7ms",
		"--gpu-attribution-output /tmp/gpu-live-demo/live_hip_linuxkfd.attributions.json",
		"--gpu-folded-output /tmp/gpu-live-demo/live_hip_linuxkfd.folded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptRecordsRunnerFailure(t *testing.T) {
	tmpDir := t.TempDir()
	fakeGo := filepath.Join(tmpDir, "go")
	fakeGoScript := `#!/bin/sh
echo fake go runner failed >&2
exit 19
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}

	outDir := filepath.Join(tmpDir, "out")
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"host-exec",
		outDir,
	)
	cmd.Env = append(os.Environ(), "PATH="+tmpDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected runner failure, got success:\n%s", out)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 19 {
		t.Fatalf("exit code = %d, want 19\n%s", exitErr.ExitCode(), out)
	}

	logData, err := os.ReadFile(filepath.Join(outDir, "host_exec_sample.runner.log"))
	if err != nil {
		t.Fatalf("read runner log: %v", err)
	}
	got := string(logData)
	for _, want := range []string{
		"runner command: go run .",
		"fake go runner failed",
		"runner exit status: 19",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in runner log:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptLiveHIPLinuxDRMSmoke(t *testing.T) {
	requireBPFCapsForRootTest(t)

	hipLib, err := firstHIPLibraryPath()
	if err != nil {
		t.Skipf("no HIP library path: %v", err)
	}

	workload, args, err := firstAMDGPUWorkloadTool()
	if err != nil {
		t.Skipf("no amdgpu workload tool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	workloadArgs := append([]string{"-lc", `sleep 1; exec "$0" "$@"`, workload}, args...)
	workloadCmd := exec.CommandContext(ctx, "/bin/sh", workloadArgs...)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()
	workloadCmd.Stdout = devNull
	workloadCmd.Stderr = devNull
	if err := workloadCmd.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	defer func() {
		if workloadCmd.ProcessState == nil || !workloadCmd.ProcessState.Exited() {
			_ = workloadCmd.Process.Kill()
			_, _ = workloadCmd.Process.Wait()
		}
	}()

	outDir := t.TempDir()
	scriptCmd := exec.CommandContext(
		ctx,
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"live-hip-linuxdrm",
		outDir,
		"--pid",
		strconv.Itoa(workloadCmd.Process.Pid),
		"--hip-library",
		hipLib,
		"--duration",
		"2s",
	)
	scriptCmd.Env = append(os.Environ(),
		"GOCACHE=/tmp/perf-agent-gocache",
		"GOMODCACHE=/tmp/perf-agent-gomodcache",
		"GOTOOLCHAIN=auto",
		"LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release",
		"CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include",
		"CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic",
	)
	out, err := scriptCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live helper smoke: %v\n%s", err, out)
	}

	for _, name := range []string{
		"live_hip_linuxdrm.raw.json",
		"live_hip_linuxdrm.attributions.json",
		"live_hip_linuxdrm.folded",
		"live_hip_linuxdrm.pb.gz",
	} {
		path := filepath.Join(outDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v\n%s", path, err, out)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty\n%s", path, out)
		}
	}
}

func TestGPUOfflineDemoScriptLiveHIPLinuxKFDSmoke(t *testing.T) {
	requireBPFCapsForRootTest(t)

	hipLib, err := firstHIPLibraryPath()
	if err != nil {
		t.Skipf("no HIP library path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	binaryPath := buildHIPLaunchShim(t, t.TempDir())
	shimLogPath := filepath.Join(t.TempDir(), "hip-shim.log")
	shimCmd := exec.CommandContext(ctx, binaryPath)
	shimLog, err := os.Create(shimLogPath)
	if err != nil {
		t.Fatalf("create shim log: %v", err)
	}
	defer shimLog.Close()
	shimCmd.Stdout = shimLog
	shimCmd.Stderr = shimLog
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

	outDir := t.TempDir()
	scriptCmd := exec.CommandContext(
		ctx,
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"live-hip-linuxkfd",
		outDir,
		"--pid",
		strconv.Itoa(shimCmd.Process.Pid),
		"--hip-library",
		hipLib,
		"--duration",
		"2s",
	)
	scriptCmd.Env = append(os.Environ(),
		"GOCACHE=/tmp/perf-agent-gocache",
		"GOMODCACHE=/tmp/perf-agent-gomodcache",
		"GOTOOLCHAIN=auto",
		"LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release",
		"CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include",
		"CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic",
	)
	out, err := scriptCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live kfd helper smoke: %v\n%s", err, out)
	}

	for _, name := range []string{
		"live_hip_linuxkfd.raw.json",
		"live_hip_linuxkfd.attributions.json",
		"live_hip_linuxkfd.folded",
		"live_hip_linuxkfd.pb.gz",
	} {
		path := filepath.Join(outDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v\n%s", path, err, out)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty\n%s", path, out)
		}
	}

	rawBytes, err := os.ReadFile(filepath.Join(outDir, "live_hip_linuxkfd.raw.json"))
	if err != nil {
		t.Fatalf("read raw snapshot: %v", err)
	}
	var snap gpu.Snapshot
	if err := json.Unmarshal(rawBytes, &snap); err != nil {
		t.Fatalf("unmarshal raw snapshot: %v\n%s", err, rawBytes)
	}
	if len(snap.EventBackends) != 1 || snap.EventBackends[0] != gpu.BackendLinuxKFD {
		t.Fatalf("event_backends=%v want [%q]", snap.EventBackends, gpu.BackendLinuxKFD)
	}
	if snap.JoinStats.LaunchCount == 0 {
		t.Fatalf("expected at least one captured HIP launch, join_stats=%+v", snap.JoinStats)
	}
	if snap.JoinStats.UnmatchedCandidateEventCount == 0 {
		t.Fatalf("expected linuxkfd candidate events, join_stats=%+v", snap.JoinStats)
	}
	var foundKFD bool
	for _, event := range snap.Events {
		if event.Backend == gpu.BackendLinuxKFD {
			foundKFD = true
			break
		}
	}
	if !foundKFD {
		t.Fatalf("expected at least one linuxkfd event in snapshot: %+v", snap.Events)
	}

	attrBytes, err := os.ReadFile(filepath.Join(outDir, "live_hip_linuxkfd.attributions.json"))
	if err != nil {
		t.Fatalf("read attribution snapshot: %v", err)
	}
	var attributions []gpu.WorkloadAttribution
	if err := json.Unmarshal(attrBytes, &attributions); err != nil {
		t.Fatalf("unmarshal attributions: %v\n%s", err, attrBytes)
	}
	if len(attributions) == 0 {
		t.Fatal("expected at least one workload attribution")
	}
	if attributions[0].LaunchCount == 0 {
		t.Fatalf("expected launch attribution in first workload: %+v", attributions[0])
	}
	if !slices.Contains(attributions[0].Backends, gpu.BackendHIP) {
		t.Fatalf("expected hip backend in attribution backends: %+v", attributions[0].Backends)
	}
	if !slices.Contains(attributions[0].Backends, gpu.BackendLinuxKFD) {
		t.Fatalf("expected linuxkfd backend in attribution backends: %+v", attributions[0].Backends)
	}
}

func TestGPUOfflineDemoScriptHostExecReportsJoinInspection(t *testing.T) {
	outDir := t.TempDir()
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"host-exec",
		outDir,
	)
	cmd.Env = append(os.Environ(),
		"GOCACHE=/tmp/perf-agent-gocache",
		"GOMODCACHE=/tmp/perf-agent-gomodcache",
		"GOTOOLCHAIN=auto",
		"LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release",
		"CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include",
		"CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host-exec helper: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"Inspect join diagnostics with:",
		"jq '.join_stats' " + filepath.Join(outDir, "host_exec_sample.raw.json"),
		"Inspect workload attribution with:",
		"jq '.' " + filepath.Join(outDir, "host_exec_sample.attributions.json"),
		"join summary:",
		"launches matched: 1/1",
		"exact execution joins: 1",
		"heuristic event joins: 0",
		"unmatched launches: 0",
		"unmatched candidate events: 0",
		"tuning hint:",
		"join activity looks healthy; only widen --join-window if you still see missing lifecycle matches",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptHIPAMDSampleReportsJoinInspection(t *testing.T) {
	outDir := t.TempDir()
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"hip-amd-sample",
		outDir,
	)
	cmd.Env = append(os.Environ(),
		"GOCACHE=/tmp/perf-agent-gocache",
		"GOMODCACHE=/tmp/perf-agent-gomodcache",
		"GOTOOLCHAIN=auto",
		"LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release",
		"CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include",
		"CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hip-amd-sample helper: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"Inspect join diagnostics with:",
		"jq '.join_stats' " + filepath.Join(outDir, "amd_sample_exec.raw.json"),
		"Inspect workload attribution with:",
		"jq '.' " + filepath.Join(outDir, "amd_sample_exec.attributions.json"),
		"join summary:",
		"launches matched: 1/1",
		"exact execution joins: 1",
		"heuristic event joins: 0",
		"unmatched launches: 0",
		"unmatched candidate events: 0",
		"tuning hint:",
		"join activity looks healthy; only widen --join-window if you still see missing lifecycle matches",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPLinuxDRMWrapperDryRunWithPID(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-linuxdrm.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--pid",
		"4242",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--join-window",
		"7ms",
		"--duration",
		"3s",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with pid: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"sudo /usr/bin/env",
		"scripts/gpu-offline-demo.sh",
		"live-hip-linuxdrm",
		"/tmp/gpu-live-wrapper",
		"--pid 4242",
		"--hip-library /opt/rocm/lib/libamdhip64.so",
		"--join-window 7ms",
		"--duration 3s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPLinuxDRMWrapperDryRunAutoTarget(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-linuxdrm.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run auto target: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"dry-run placeholder: pass --pid <live-hip-process-pid> for a real run",
		"--pid",
		"scripts/gpu-offline-demo.sh",
		"live-hip-linuxdrm",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPLinuxKFDWrapperDryRunWithPID(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-linuxkfd.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--pid",
		"4242",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--join-window",
		"7ms",
		"--duration",
		"3s",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with pid: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"sudo /usr/bin/env",
		"scripts/gpu-offline-demo.sh",
		"live-hip-linuxkfd",
		"/tmp/gpu-live-wrapper",
		"--pid 4242",
		"--hip-library /opt/rocm/lib/libamdhip64.so",
		"--join-window 7ms",
		"--duration 3s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithPID(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--pid",
		"4242",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--sample-command",
		"cat gpu/testdata/replay/amd_sample_exec.ndjson",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with pid: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"bash -lc cat\\ gpu/testdata/replay/amd_sample_exec.ndjson |",
		"scripts/gpu-offline-demo.sh live-hip-amdsample /tmp/gpu-live-wrapper",
		"--pid 4242",
		"--hip-library /opt/rocm/lib/libamdhip64.so",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPLinuxDRMWrapperRejectsMissingPID(t *testing.T) {
	fakeDir := t.TempDir()
	fakeHipLib := filepath.Join(fakeDir, "libamdhip64.so")
	if err := os.WriteFile(fakeHipLib, []byte(""), 0o644); err != nil {
		t.Fatalf("write fake hip library: %v", err)
	}
	fakeSudo := filepath.Join(fakeDir, "sudo")
	if err := os.WriteFile(fakeSudo, []byte("#!/bin/sh\necho unexpected sudo >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-linuxdrm.sh"),
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--pid",
		"999999",
		"--hip-library",
		fakeHipLib,
	)
	cmd.Env = append(os.Environ(), "PATH="+fakeDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected missing pid failure, got success:\n%s", out)
	}
	got := string(out)
	if strings.Contains(got, "unexpected sudo") {
		t.Fatalf("wrapper reached sudo before missing pid preflight:\n%s", got)
	}
	if !strings.Contains(got, "does not exist") {
		t.Fatalf("missing pid preflight message not found:\n%s", got)
	}
}

func TestGPULiveHIPLinuxDRMWrapperRejectsNonHIPPID(t *testing.T) {
	fakeDir := t.TempDir()
	fakeHipLib := filepath.Join(fakeDir, "libamdhip64.so")
	if err := os.WriteFile(fakeHipLib, []byte(""), 0o644); err != nil {
		t.Fatalf("write fake hip library: %v", err)
	}
	fakeSudo := filepath.Join(fakeDir, "sudo")
	if err := os.WriteFile(fakeSudo, []byte("#!/bin/sh\necho unexpected sudo >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-linuxdrm.sh"),
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--pid",
		strconv.Itoa(os.Getpid()),
		"--hip-library",
		fakeHipLib,
	)
	cmd.Env = append(os.Environ(), "PATH="+fakeDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-hip pid failure, got success:\n%s", out)
	}
	got := string(out)
	if strings.Contains(got, "unexpected sudo") {
		t.Fatalf("wrapper reached sudo before hip maps preflight:\n%s", got)
	}
	if !strings.Contains(got, "does not map libamdhip64") {
		t.Fatalf("non-hip pid preflight message not found:\n%s", got)
	}
}

func TestGPULiveHIPLinuxDRMWrapperRecordsWrappedFailure(t *testing.T) {
	tmpDir := t.TempDir()
	fakeHipLib := filepath.Join(tmpDir, "libamdhip64.so")
	if err := os.WriteFile(fakeHipLib, []byte(""), 0o644); err != nil {
		t.Fatalf("write fake hip library: %v", err)
	}

	fakeSudo := filepath.Join(tmpDir, "sudo")
	fakeSudoScript := `#!/bin/sh
if [ "$1" = "grep" ]; then
  exit 0
fi
echo fake sudo wrapper ran >&2
exit 23
`
	if err := os.WriteFile(fakeSudo, []byte(fakeSudoScript), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}

	outDir := filepath.Join(tmpDir, "out")
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-linuxdrm.sh"),
		"--outdir",
		outDir,
		"--pid",
		strconv.Itoa(os.Getpid()),
		"--hip-library",
		fakeHipLib,
		"--duration",
		"1ms",
	)
	cmd.Env = append(os.Environ(), "PATH="+tmpDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected wrapped sudo failure, got success:\n%s", out)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 23 {
		t.Fatalf("exit code = %d, want 23\n%s", exitErr.ExitCode(), out)
	}

	logData, err := os.ReadFile(filepath.Join(outDir, "live_hip_linuxdrm_wrapper.log"))
	if err != nil {
		t.Fatalf("read wrapper log: %v", err)
	}
	got := string(logData)
	for _, want := range []string{
		"wrapper command:",
		"fake sudo wrapper ran",
		"wrapper exit status: 23",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in wrapper log:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRun(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-shim-demo",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--join-window",
		"7ms",
		"--duration",
		"3s",
		"--sleep-before-ms",
		"1500",
		"--sleep-after-ms",
		"2500",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"cc -O2 -g -Wall -Wextra",
		"scripts/hip-launch-shim.c",
		"HIP_LAUNCH_SHIM_LIBRARY=/opt/rocm/lib/libamdhip64.so",
		"HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS=1500",
		"HIP_LAUNCH_SHIM_SLEEP_AFTER_MS=2500",
		"scripts/gpu-live-hip-linuxdrm.sh --outdir /tmp/gpu-live-shim-demo",
		"--pid",
		"--join-window 7ms",
		"--duration 3s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForLinuxKFD(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-shim-demo",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--linux-surface",
		"kfd",
		"--join-window",
		"7ms",
		"--duration",
		"3s",
		"--sleep-before-ms",
		"1500",
		"--sleep-after-ms",
		"2500",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run kfd: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"cc -O2 -g -Wall -Wextra",
		"scripts/hip-launch-shim.c",
		"HIP_LAUNCH_SHIM_LIBRARY=/opt/rocm/lib/libamdhip64.so",
		"HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS=1500",
		"HIP_LAUNCH_SHIM_SLEEP_AFTER_MS=2500",
		"scripts/gpu-live-hip-linuxkfd.sh --outdir /tmp/gpu-live-shim-demo",
		"--pid",
		"--join-window 7ms",
		"--duration 3s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSample(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-command cat\\ gpu/testdata/replay/amd_sample_exec.ndjson",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoRecordsWrapperFailure(t *testing.T) {
	tmpDir := t.TempDir()
	fakeHipLib := filepath.Join(tmpDir, "libamdhip64.so")
	if err := os.WriteFile(fakeHipLib, []byte("not-a-real-library"), 0o644); err != nil {
		t.Fatalf("write fake hip library: %v", err)
	}

	fakeWrapper := filepath.Join(tmpDir, "fake-wrapper.sh")
	if err := os.WriteFile(fakeWrapper, []byte("#!/bin/sh\necho fake wrapper ran >&2\nexit 17\n"), 0o755); err != nil {
		t.Fatalf("write fake wrapper: %v", err)
	}

	fakeSudo := filepath.Join(tmpDir, "sudo")
	if err := os.WriteFile(fakeSudo, []byte("#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then exit 0; fi\necho unexpected sudo >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}

	outDir := filepath.Join(tmpDir, "out")
	binaryPath := filepath.Join(tmpDir, "gpu-hip-launch-shim")
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--outdir",
		outDir,
		"--binary",
		binaryPath,
		"--hip-library",
		fakeHipLib,
		"--duration",
		"1ms",
		"--sleep-before-ms",
		"1",
		"--sleep-after-ms",
		"1",
	)
	cmd.Env = append(os.Environ(),
		"PATH="+tmpDir+":"+os.Getenv("PATH"),
		"PERF_AGENT_GPU_LIVE_WRAPPER_SCRIPT="+fakeWrapper,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected wrapper failure, got success:\n%s", out)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 17 {
		t.Fatalf("exit code = %d, want 17\n%s", exitErr.ExitCode(), out)
	}

	logData, err := os.ReadFile(filepath.Join(outDir, "gpu_live_wrapper.log"))
	if err != nil {
		t.Fatalf("read wrapper log: %v", err)
	}
	got := string(logData)
	for _, want := range []string{
		"wrapper command:",
		"fake wrapper ran",
		"wrapper exit status: 17",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in wrapper log:\n%s", want, got)
		}
	}
}

func TestHIPLaunchShimBinaryRuns(t *testing.T) {
	hipLib, err := firstHIPLibraryPath()
	if err != nil {
		t.Skipf("no hip library path: %v", err)
	}

	tmpDir := t.TempDir()
	binaryPath := buildHIPLaunchShim(t, tmpDir)

	runCmd := exec.Command(binaryPath)
	runCmd.Env = append(os.Environ(),
		"HIP_LAUNCH_SHIM_LIBRARY="+hipLib,
		"HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS=10",
		"HIP_LAUNCH_SHIM_SLEEP_AFTER_MS=10",
	)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run hip shim: %v\n%s", err, runOut)
	}
	got := string(runOut)
	for _, want := range []string{
		"hipGetDeviceCount ->",
		"hipLaunchKernel ->",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
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
		filepath.Join("scripts", "hip-launch-shim.c"),
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

func firstAMDGPUWorkloadTool() (string, []string, error) {
	for _, bin := range []string{"rocminfo", "amdgpu-arch"} {
		path, err := exec.LookPath(bin)
		if err == nil {
			return path, nil, nil
		}
	}
	return "", nil, errors.New("no amdgpu workload tool")
}
