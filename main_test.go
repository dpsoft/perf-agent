package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpu/codec"
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
		"env LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOCACHE=/tmp/perf-agent-gocache GOMODCACHE=/tmp/perf-agent-gomodcache GOTOOLCHAIN=auto",
		"CGO_CFLAGS=",
		"CGO_LDFLAGS=",
		"go run .",
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
		"env LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOCACHE=/tmp/perf-agent-gocache GOMODCACHE=/tmp/perf-agent-gomodcache GOTOOLCHAIN=auto",
		"CGO_CFLAGS=",
		"CGO_LDFLAGS=",
		"go run .",
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

func TestGPUOfflineDemoScriptDryRunHIPAMDSampleRich(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"hip-amd-sample-rich",
		"/tmp/gpu-amd-rich-demo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run hip-amd-sample-rich: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"env LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOCACHE=/tmp/perf-agent-gocache GOMODCACHE=/tmp/perf-agent-gomodcache GOTOOLCHAIN=auto",
		"CGO_CFLAGS=",
		"CGO_LDFLAGS=",
		"go run .",
		"--gpu-host-replay-input gpu/testdata/host/replay/hip_kfd_launches.json",
		"--gpu-amd-sample-stdin",
		"--gpu-attribution-output /tmp/gpu-amd-rich-demo/amd_sample_exec_rich.attributions.json",
		"--gpu-folded-output /tmp/gpu-amd-rich-demo/amd_sample_exec_rich.folded",
		"< gpu/testdata/replay/amd_sample_exec_rich.ndjson",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPUOfflineDemoScriptDryRunHIPRocprofv2Rich(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"--dry-run",
		"hip-rocprofv2-rich",
		"/tmp/gpu-rocprof-rich-demo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run hip-rocprofv2-rich: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"GOCACHE=/tmp/perf-agent-gocache",
		"GOMODCACHE=/tmp/perf-agent-gomodcache",
		"GOTOOLCHAIN=auto",
		"PERF_AGENT_ROCPROFV2_PATH=/home/diego/github/perf-agent/.worktrees/gpu-profiling-spec/scripts/emit-rocprofv2-rich-fixture.sh",
		"PERF_AGENT_ROCPROFV2_OUTPUT_PATH=/tmp/gpu-rocprof-rich-demo/rocprofv2_native_rich.ndjson",
		"go run ./cmd/amd-sample-collector --mode real --real-source rocprofv2",
		"|",
		"--gpu-host-replay-input gpu/testdata/host/replay/hip_kfd_launches.json",
		"--gpu-amd-sample-stdin",
		"--gpu-attribution-output /tmp/gpu-rocprof-rich-demo/rocprofv2_sample_exec_rich.attributions.json",
		"--gpu-folded-output /tmp/gpu-rocprof-rich-demo/rocprofv2_sample_exec_rich.folded",
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
		"env LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOCACHE=/tmp/perf-agent-gocache GOMODCACHE=/tmp/perf-agent-gomodcache GOTOOLCHAIN=auto",
		"CGO_CFLAGS=",
		"CGO_LDFLAGS=",
		"go run .",
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
		"runner command: env LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release",
		"go run .",
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

func TestGPULiveHIPAMDSampleWrapperSmoke(t *testing.T) {
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
	cmd := exec.CommandContext(
		ctx,
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--outdir",
		outDir,
		"--pid",
		strconv.Itoa(shimCmd.Process.Pid),
		"--hip-library",
		hipLib,
		"--duration",
		"2s",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live amd sample wrapper smoke: %v\n%s", err, out)
	}

	for _, name := range []string{
		"live_hip_amdsample.raw.json",
		"live_hip_amdsample.attributions.json",
		"live_hip_amdsample.folded",
		"live_hip_amdsample.pb.gz",
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

	rawBytes, err := os.ReadFile(filepath.Join(outDir, "live_hip_amdsample.raw.json"))
	if err != nil {
		t.Fatalf("read raw snapshot: %v", err)
	}
	var snap gpu.Snapshot
	if err := json.Unmarshal(rawBytes, &snap); err != nil {
		t.Fatalf("unmarshal raw snapshot: %v\n%s", err, rawBytes)
	}
	if len(snap.Executions) != 1 {
		t.Fatalf("executions=%d want 1", len(snap.Executions))
	}
	if snap.JoinStats.HeuristicExecutionJoinCount != 1 || snap.JoinStats.ExactExecutionJoinCount != 0 {
		t.Fatalf("join_stats=%+v", snap.JoinStats)
	}
	if got := snap.Attributions; len(got) != 1 || got[0].SampleWeight != 16 {
		t.Fatalf("attributions=%+v", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperSmokeWithRocprofv2RealSource(t *testing.T) {
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
	cmd := exec.CommandContext(
		ctx,
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--outdir",
		outDir,
		"--pid",
		strconv.Itoa(shimCmd.Process.Pid),
		"--hip-library",
		hipLib,
		"--sample-mode",
		"real",
		"--real-source",
		"rocprofv2",
		"--rocprofv2-path",
		filepath.Join(".", "scripts", "emit-rocprofv2-rich-fixture.sh"),
		"--duration",
		"2s",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live amd sample wrapper rocprofv2 smoke: %v\n%s", err, out)
	}

	foldedPath := filepath.Join(outDir, "live_hip_amdsample.folded")
	folded, err := os.ReadFile(foldedPath)
	if err != nil {
		t.Fatalf("read folded: %v\n%s", err, out)
	}
	for _, want := range []string{
		"train_step",
		"hipLaunchKernel",
		"[gpu:function:flash_attn_fwd]",
		"[gpu:source:flash_attn.hip:77]",
		"[gpu:pc:0xabc]",
		"[gpu:function:flash_attn_epilogue]",
		"[gpu:source:flash_attn_epilogue.hip:91]",
		"[gpu:pc:0xdef]",
	} {
		if !strings.Contains(string(folded), want) {
			t.Fatalf("missing %q in folded output:\n%s", want, folded)
		}
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
		"exact execution joins: 0",
		"heuristic execution joins: 1",
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

func TestGPUOfflineDemoScriptHIPAMDSampleWritesCPUAndGPUFlamegraphArtifacts(t *testing.T) {
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
		filepath.Join(outDir, "amd_sample_exec.svg"),
		filepath.Join(outDir, "amd_sample_exec.html"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}

	svgPath := filepath.Join(outDir, "amd_sample_exec.svg")
	svg, err := os.ReadFile(svgPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", svgPath, err)
	}
	for _, want := range []string{
		"<svg",
		"train_step",
		"hipLaunchKernel",
		"[gpu:launch]",
		"[gpu:queue:compute:0]",
		"[gpu:kernel:hip_launch_shim_kernel]",
		"[gpu:stall:memory_wait]",
	} {
		if !strings.Contains(string(svg), want) {
			t.Fatalf("missing %q in svg:\n%s", want, svg)
		}
	}

	htmlPath := filepath.Join(outDir, "amd_sample_exec.html")
	htmlData, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", htmlPath, err)
	}
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<html",
		"<svg",
		"train_step",
		"[gpu:kernel:hip_launch_shim_kernel]",
	} {
		if !strings.Contains(string(htmlData), want) {
			t.Fatalf("missing %q in html:\n%s", want, htmlData)
		}
	}
}

func TestGPUOfflineDemoScriptHIPAMDSampleRichWritesBrendanStyleFrames(t *testing.T) {
	outDir := t.TempDir()
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"hip-amd-sample-rich",
		outDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hip-amd-sample-rich helper: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		filepath.Join(outDir, "amd_sample_exec_rich.svg"),
		filepath.Join(outDir, "amd_sample_exec_rich.html"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}

	svgPath := filepath.Join(outDir, "amd_sample_exec_rich.svg")
	svg, err := os.ReadFile(svgPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", svgPath, err)
	}
	for _, want := range []string{
		"<svg",
		"train_step",
		"hipLaunchKernel",
		"[gpu:function:flash_attn_fwd]",
		"[gpu:source:flash_attn.hip:77]",
		"[gpu:pc:0xabc]",
		"[gpu:function:flash_attn_epilogue]",
		"[gpu:source:flash_attn_epilogue.hip:91]",
		"[gpu:pc:0xdef]",
	} {
		if !strings.Contains(string(svg), want) {
			t.Fatalf("missing %q in svg:\n%s", want, svg)
		}
	}

	htmlPath := filepath.Join(outDir, "amd_sample_exec_rich.html")
	htmlData, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", htmlPath, err)
	}
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<html",
		"<svg",
		"[gpu:function:flash_attn_fwd]",
		"[gpu:source:flash_attn.hip:77]",
		"[gpu:pc:0xabc]",
	} {
		if !strings.Contains(string(htmlData), want) {
			t.Fatalf("missing %q in html:\n%s", want, htmlData)
		}
	}
}

func TestGPUOfflineDemoScriptHIPRocprofv2RichWritesBrendanStyleFrames(t *testing.T) {
	outDir := t.TempDir()
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-offline-demo.sh"),
		"hip-rocprofv2-rich",
		outDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hip-rocprofv2-rich helper: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		filepath.Join(outDir, "rocprofv2_sample_exec_rich.svg"),
		filepath.Join(outDir, "rocprofv2_sample_exec_rich.html"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}

	svgPath := filepath.Join(outDir, "rocprofv2_sample_exec_rich.svg")
	svg, err := os.ReadFile(svgPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", svgPath, err)
	}
	for _, want := range []string{
		"<svg",
		"train_step",
		"hipLaunchKernel",
		"[gpu:function:flash_attn_fwd]",
		"[gpu:source:flash_attn.hip:77]",
		"[gpu:pc:0xabc]",
		"[gpu:function:flash_attn_epilogue]",
		"[gpu:source:flash_attn_epilogue.hip:91]",
		"[gpu:pc:0xdef]",
	} {
		if !strings.Contains(string(svg), want) {
			t.Fatalf("missing %q in svg:\n%s", want, svg)
		}
	}

	htmlPath := filepath.Join(outDir, "rocprofv2_sample_exec_rich.html")
	htmlData, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", htmlPath, err)
	}
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<html",
		"<svg",
		"[gpu:function:flash_attn_fwd]",
		"[gpu:source:flash_attn.hip:77]",
		"[gpu:pc:0xabc]",
	} {
		if !strings.Contains(string(htmlData), want) {
			t.Fatalf("missing %q in html:\n%s", want, htmlData)
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

func TestGPULiveHIPAMDSampleWrapperDryRunWithCollectorPath(t *testing.T) {
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
		"--sample-collector-path",
		"/opt/rocm/bin/amd-sample-collector",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with collector path: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH=/opt/rocm/bin/amd-sample-collector",
		"bash -lc bash\\ scripts/amd-sample-adapter.sh |",
		"scripts/gpu-offline-demo.sh live-hip-amdsample /tmp/gpu-live-wrapper",
		"--pid 4242",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithCollectorCommand(t *testing.T) {
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
		"--sample-collector-command",
		"printf collector-command",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with collector command: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND=printf\\ collector-command",
		"bash -lc bash\\ scripts/amd-sample-adapter.sh |",
		"scripts/gpu-offline-demo.sh live-hip-amdsample /tmp/gpu-live-wrapper",
		"--pid 4242",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithSampleMode(t *testing.T) {
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
		"--sample-mode",
		"real",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with sample mode: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PERF_AGENT_AMD_SAMPLE_MODE=real") {
		t.Fatalf("missing collector mode env in output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithRealSource(t *testing.T) {
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
		"--sample-mode",
		"real",
		"--real-source",
		"rocm-smi",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with real source: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PERF_AGENT_AMD_SAMPLE_REAL_SOURCE=rocm-smi") {
		t.Fatalf("missing real source env in output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithROCMSMIPath(t *testing.T) {
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
		"--sample-mode",
		"real",
		"--rocm-smi-path",
		"/opt/rocm/bin/rocm-smi",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with rocm-smi path: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PERF_AGENT_ROCM_SMI_PATH=/opt/rocm/bin/rocm-smi") {
		t.Fatalf("missing rocm-smi path env in output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithRocprofv2Path(t *testing.T) {
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
		"--sample-mode",
		"real",
		"--real-source",
		"rocprofv2",
		"--rocprofv2-path",
		"/opt/rocm/bin/rocprofv2",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with rocprofv2 path: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PERF_AGENT_ROCPROFV2_PATH=/opt/rocm/bin/rocprofv2") {
		t.Fatalf("missing rocprofv2 path env in output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithRocprofv2OutputPath(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--dry-run",
		"--pid", "4242",
		"--hip-library", "/opt/rocm/lib/libamdhip64.so",
		"--sample-mode", "real",
		"--real-source", "rocprofv2",
		"--rocprofv2-output-path", "/tmp/rocprofv2.jsonl",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with rocprofv2 output path: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PERF_AGENT_ROCPROFV2_OUTPUT_PATH=/tmp/rocprofv2.jsonl") {
		t.Fatalf("missing rocprofv2 output path env in output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithRealPollInterval(t *testing.T) {
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
		"--sample-mode",
		"real",
		"--real-poll-interval",
		"25ms",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with real poll interval: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL=25ms") {
		t.Fatalf("missing poll interval env in output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithKernelName(t *testing.T) {
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
		"--kernel-name",
		"flash_attn_fwd",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with kernel name: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"PERF_AGENT_GPU_KERNEL_NAME=flash_attn_fwd",
		"scripts/gpu-offline-demo.sh live-hip-amdsample /tmp/gpu-live-wrapper",
		"--pid 4242",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithQueueAndDeviceContext(t *testing.T) {
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
		"--device-id",
		"gfx942:0",
		"--device-name",
		"MI300X",
		"--queue-id",
		"compute:7",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run with queue/device context: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"PERF_AGENT_GPU_DEVICE_ID=gfx942:0",
		"PERF_AGENT_GPU_DEVICE_NAME=MI300X",
		"PERF_AGENT_GPU_QUEUE_ID=compute:7",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperRejectsLegacySampleCommandEnv(t *testing.T) {
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
	)
	cmd.Env = append(os.Environ(), "PERF_AGENT_AMD_SAMPLE_COMMAND=printf legacy-command")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected legacy env failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "PERF_AGENT_AMD_SAMPLE_COMMAND is no longer supported") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGPULiveHIPAMDSampleWrapperRejectsCollectorPathWithSampleCommand(t *testing.T) {
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
		"printf explicit-command",
		"--sample-collector-path",
		"/opt/rocm/bin/amd-sample-collector",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected conflict failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot combine --sample-command with --sample-collector-path") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGPULiveHIPAMDSampleWrapperHelpOmitsLegacySampleCommandEnv(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--help",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper help: %v\n%s", err, out)
	}
	got := string(out)
	if strings.Contains(got, "PERF_AGENT_AMD_SAMPLE_COMMAND") {
		t.Fatalf("legacy env leaked into help output:\n%s", got)
	}
	if !strings.Contains(got, "PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND") {
		t.Fatalf("missing collector command env in help output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperRejectsCollectorCommandWithSampleCommand(t *testing.T) {
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
		"printf explicit-command",
		"--sample-collector-command",
		"printf collector-command",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected conflict failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot combine --sample-command with --sample-collector-command") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGPULiveHIPAMDSampleWrapperRejectsCollectorPathWithCollectorCommand(t *testing.T) {
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
		"--sample-collector-path",
		"/opt/rocm/bin/amd-sample-collector",
		"--sample-collector-command",
		"printf collector-command",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected conflict failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot combine --sample-collector-path with --sample-collector-command") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGPULiveHIPAMDSampleWrapperRejectsMissingCollectorPathBeforeSudo(t *testing.T) {
	fakeDir := t.TempDir()
	fakeHipLib := filepath.Join(fakeDir, "libamdhip64.so")
	if err := os.WriteFile(fakeHipLib, []byte(""), 0o644); err != nil {
		t.Fatalf("write fake hip library: %v", err)
	}
	fakeSudo := filepath.Join(fakeDir, "sudo")
	fakeSudoScript := `#!/bin/sh
if [ "$1" = "grep" ]; then
  exit 0
fi
echo unexpected sudo >&2
exit 42
`
	if err := os.WriteFile(fakeSudo, []byte(fakeSudoScript), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--pid",
		strconv.Itoa(os.Getpid()),
		"--hip-library",
		fakeHipLib,
		"--sample-collector-path",
		filepath.Join(fakeDir, "missing-collector"),
	)
	cmd.Env = append(os.Environ(), "PATH="+fakeDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected missing collector path failure, got success:\n%s", out)
	}
	got := string(out)
	if strings.Contains(got, "unexpected sudo") {
		t.Fatalf("wrapper reached sudo before collector path preflight:\n%s", got)
	}
	if !strings.Contains(got, "sample collector path is not executable") {
		t.Fatalf("unexpected output:\n%s", got)
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunDefaultsProducer(t *testing.T) {
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
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run default producer: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"PERF_AGENT_HIP_PID=4242",
		"PERF_AGENT_HIP_LIBRARY=/opt/rocm/lib/libamdhip64.so",
		"PERF_AGENT_HIP_SYMBOL=hipLaunchKernel",
		"PERF_AGENT_GPU_DURATION=2s",
		"PERF_AGENT_GPU_KERNEL_NAME=hip_launch_shim_kernel",
		"bash -lc bash\\ scripts/amd-sample-adapter.sh |",
		"scripts/gpu-offline-demo.sh live-hip-amdsample /tmp/gpu-live-wrapper",
		"--pid 4242",
		"--hip-library /opt/rocm/lib/libamdhip64.so",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithoutPIDShowsProducerContract(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run without pid: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"dry-run placeholder: pass --pid <live-hip-process-pid> for a real run",
		"PERF_AGENT_HIP_PID=\\<pid\\>",
		"PERF_AGENT_HIP_LIBRARY=/opt/rocm/lib/libamdhip64.so",
		"PERF_AGENT_HIP_SYMBOL=hipLaunchKernel",
		"PERF_AGENT_GPU_DURATION=2s",
		"PERF_AGENT_GPU_KERNEL_NAME=hip_launch_shim_kernel",
		"bash -lc bash\\ scripts/amd-sample-adapter.sh |",
		"scripts/gpu-offline-demo.sh live-hip-amdsample /tmp/gpu-live-wrapper",
		"--pid \\<pid\\>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPAMDSampleWrapperDryRunWithoutPIDShowsCollectorPath(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-amdsample.sh"),
		"--dry-run",
		"--outdir",
		"/tmp/gpu-live-wrapper",
		"--hip-library",
		"/opt/rocm/lib/libamdhip64.so",
		"--sample-collector-path",
		"/opt/rocm/bin/amd-sample-collector",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper dry-run without pid collector path: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"PERF_AGENT_HIP_PID=\\<pid\\>",
		"PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH=/opt/rocm/bin/amd-sample-collector",
		"bash -lc bash\\ scripts/amd-sample-adapter.sh |",
		"--pid \\<pid\\>",
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
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--sample-command") {
		t.Fatalf("default amdsample dry-run should rely on wrapper default producer:\n%s", got)
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleCollectorPath(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-collector-path",
		"/opt/rocm/bin/amd-sample-collector",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample collector path: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-collector-path /opt/rocm/bin/amd-sample-collector",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--sample-command") {
		t.Fatalf("collector-path dry-run should not force --sample-command:\n%s", got)
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleCollectorCommand(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-collector-command",
		"printf collector-command",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample collector command: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-collector-command printf\\ collector-command",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--sample-command") {
		t.Fatalf("collector-command dry-run should not force --sample-command:\n%s", got)
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleSampleMode(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-mode",
		"real",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample sample mode: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-mode real",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleRealSource(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-mode",
		"real",
		"--real-source",
		"rocm-smi",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample real source: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-mode real",
		"--real-source rocm-smi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleROCMSMIPath(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-mode",
		"real",
		"--rocm-smi-path",
		"/opt/rocm/bin/rocm-smi",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample rocm-smi path: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-mode real",
		"--rocm-smi-path /opt/rocm/bin/rocm-smi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleRocprofv2Path(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-mode",
		"real",
		"--real-source",
		"rocprofv2",
		"--rocprofv2-path",
		"/opt/rocm/bin/rocprofv2",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample rocprofv2 path: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-mode real",
		"--real-source rocprofv2",
		"--rocprofv2-path /opt/rocm/bin/rocprofv2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleRocprofv2OutputPath(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface", "amdsample",
		"--sample-mode", "real",
		"--real-source", "rocprofv2",
		"--rocprofv2-output-path", "/tmp/rocprofv2.jsonl",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample rocprofv2 output path: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"--real-source rocprofv2",
		"--rocprofv2-output-path /tmp/rocprofv2.jsonl",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleRealPollInterval(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-mode",
		"real",
		"--real-poll-interval",
		"25ms",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample real poll interval: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--sample-mode real",
		"--real-poll-interval 25ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleKernelName(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--kernel-name",
		"flash_attn_fwd",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample kernel name: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--kernel-name flash_attn_fwd",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoDryRunForAMDSampleQueueAndDeviceContext(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--device-id",
		"gfx942:0",
		"--device-name",
		"MI300X",
		"--queue-id",
		"compute:7",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim demo dry-run amdsample queue/device context: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"scripts/gpu-live-hip-amdsample.sh --outdir /tmp/gpu-live",
		"--device-id gfx942:0",
		"--device-name MI300X",
		"--queue-id compute:7",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in shim demo output:\n%s", want, got)
		}
	}
}

func TestGPULiveHIPShimDemoRejectsCollectorPathWithSampleCommand(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-command",
		"printf explicit-command",
		"--sample-collector-path",
		"/opt/rocm/bin/amd-sample-collector",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected conflict failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot combine --sample-command with --sample-collector-path") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGPULiveHIPShimDemoRejectsCollectorCommandWithSampleCommand(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--dry-run",
		"--linux-surface",
		"amdsample",
		"--sample-command",
		"printf explicit-command",
		"--sample-collector-command",
		"printf collector-command",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected conflict failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot combine --sample-command with --sample-collector-command") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGPULiveHIPShimDemoRejectsMissingCollectorPath(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "gpu-live-hip-shim-demo.sh"),
		"--linux-surface",
		"amdsample",
		"--sample-collector-path",
		"/tmp/definitely-missing-amd-sample-collector",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected missing collector path failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "sample collector path is not executable") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestAMDSampleProducerScriptEmitsProducerNativeNDJSON(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-producer.sh"),
		"--kernel-name",
		"hip_launch_shim_kernel",
		"--sleep-before-ms",
		"0",
		"--device-id",
		"gfx1103:0",
		"--queue-id",
		"compute:0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample producer: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}

	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Kind != codec.KindExec {
		t.Fatalf("kind=%q want exec", execEv.Kind)
	}
	if execEv.Exec.Correlation.Backend != gpu.BackendAMDSample || execEv.Exec.Correlation.Value == "" {
		t.Fatalf("exec correlation=%+v", execEv.Exec.Correlation)
	}
	if execEv.Exec.Correlation.Value == "hip:555:555:100" {
		t.Fatalf("exec correlation should be producer-native, got %+v", execEv.Exec.Correlation)
	}
	if execEv.Exec.Queue.Backend != gpu.BackendAMDSample || execEv.Exec.Queue.QueueID != "compute:0" {
		t.Fatalf("exec queue=%+v", execEv.Exec.Queue)
	}

	sample1, err := codec.DecodeLine([]byte(lines[1]))
	if err != nil {
		t.Fatalf("decode sample1 line: %v\n%s", err, lines[1])
	}
	sample2, err := codec.DecodeLine([]byte(lines[2]))
	if err != nil {
		t.Fatalf("decode sample2 line: %v\n%s", err, lines[2])
	}
	if sample1.Kind != codec.KindSample || sample2.Kind != codec.KindSample {
		t.Fatalf("kinds=%q,%q", sample1.Kind, sample2.Kind)
	}
	if sample1.Sample.Correlation.Backend != gpu.BackendAMDSample || sample2.Sample.Correlation.Backend != gpu.BackendAMDSample {
		t.Fatalf("sample correlations=%+v %+v", sample1.Sample.Correlation, sample2.Sample.Correlation)
	}
	if !(execEv.Exec.StartNs <= sample1.Sample.TimeNs && sample1.Sample.TimeNs < sample2.Sample.TimeNs && sample2.Sample.TimeNs <= execEv.Exec.EndNs) {
		t.Fatalf("unexpected time ordering: exec=%+v sample1=%+v sample2=%+v", execEv.Exec, sample1.Sample, sample2.Sample)
	}
}

func TestAMDSampleProducerScriptUsesHIPPIDContext(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-producer.sh"),
		"--sleep-before-ms",
		"0",
	)
	cmd.Env = append(os.Environ(), "PERF_AGENT_HIP_PID=4242")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample producer with pid env: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		t.Fatalf("expected producer output, got none")
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.Execution.ContextID != "pid-4242" {
		t.Fatalf("context_id=%q", execEv.Exec.Execution.ContextID)
	}
	if !strings.Contains(execEv.Exec.Execution.ExecID, "4242") {
		t.Fatalf("exec_id=%q", execEv.Exec.Execution.ExecID)
	}
}

func TestAMDSampleProducerScriptUsesDurationContext(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-producer.sh"),
		"--sleep-before-ms",
		"0",
	)
	cmd.Env = append(os.Environ(), "PERF_AGENT_GPU_DURATION=2s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample producer with duration env: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	sample1, err := codec.DecodeLine([]byte(lines[1]))
	if err != nil {
		t.Fatalf("decode sample1 line: %v\n%s", err, lines[1])
	}
	sample2, err := codec.DecodeLine([]byte(lines[2]))
	if err != nil {
		t.Fatalf("decode sample2 line: %v\n%s", err, lines[2])
	}
	if got := execEv.Exec.EndNs - execEv.Exec.StartNs; got != 2_000_000_000 {
		t.Fatalf("duration_ns=%d", got)
	}
	if !(execEv.Exec.StartNs < sample1.Sample.TimeNs && sample1.Sample.TimeNs < sample2.Sample.TimeNs && sample2.Sample.TimeNs < execEv.Exec.EndNs) {
		t.Fatalf("unexpected time ordering: exec=%+v sample1=%+v sample2=%+v", execEv.Exec, sample1.Sample, sample2.Sample)
	}
}

func TestAMDSampleProducerScriptUsesQueueAndDeviceContext(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-producer.sh"),
		"--sleep-before-ms",
		"0",
	)
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_GPU_DEVICE_ID=gfx942:0",
		"PERF_AGENT_GPU_DEVICE_NAME=MI300X",
		"PERF_AGENT_GPU_QUEUE_ID=compute:7",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample producer with queue/device env: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.Execution.DeviceID != "gfx942:0" {
		t.Fatalf("device_id=%q", execEv.Exec.Execution.DeviceID)
	}
	if execEv.Exec.Queue.Device.Name != "MI300X" {
		t.Fatalf("device_name=%q", execEv.Exec.Queue.Device.Name)
	}
	if execEv.Exec.Queue.QueueID != "compute:7" {
		t.Fatalf("queue_id=%q", execEv.Exec.Queue.QueueID)
	}
}

func TestAMDSampleCollectorBinaryUsesContext(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)

	cmd := exec.Command(binaryPath)
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_HIP_PID=4242",
		"PERF_AGENT_GPU_DURATION=2s",
		"PERF_AGENT_GPU_KERNEL_NAME=collector_kernel",
		"PERF_AGENT_GPU_DEVICE_ID=gfx942:0",
		"PERF_AGENT_GPU_DEVICE_NAME=MI300X",
		"PERF_AGENT_GPU_QUEUE_ID=compute:7",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample collector binary: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.KernelName != "collector_kernel" {
		t.Fatalf("kernel_name=%q", execEv.Exec.KernelName)
	}
	if execEv.Exec.Execution.ContextID != "pid-4242" {
		t.Fatalf("context_id=%q", execEv.Exec.Execution.ContextID)
	}
	if execEv.Exec.Execution.DeviceID != "gfx942:0" {
		t.Fatalf("device_id=%q", execEv.Exec.Execution.DeviceID)
	}
	if execEv.Exec.Queue.Device.Name != "MI300X" {
		t.Fatalf("device_name=%q", execEv.Exec.Queue.Device.Name)
	}
	if execEv.Exec.Queue.QueueID != "compute:7" {
		t.Fatalf("queue_id=%q", execEv.Exec.Queue.QueueID)
	}
	if got := execEv.Exec.EndNs - execEv.Exec.StartNs; got != 2_000_000_000 {
		t.Fatalf("duration_ns=%d", got)
	}
}

func TestAMDSampleCollectorBinaryRealModeUsesROCMSMI(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)
	rocmSMIPath := filepath.Join(tmpDir, "rocm-smi")
	rocmSMIScript := `#!/bin/sh
counter_file="$(dirname "$0")/rocm-smi.count"
count=0
if [ -f "${counter_file}" ]; then
  count="$(cat "${counter_file}")"
fi
count=$((count + 1))
printf '%s' "${count}" > "${counter_file}"
printf '%s\n' 'libdrm warning' >&2
if [ "${count}" -eq 1 ]; then
  printf '%s\n' '{"card1":{"Device Name":"MI300X","Device ID":"0x74a1","Current Socket Graphics Package Power (W)":"275.500","GPU use (%)":"73","Temperature (Sensor edge) (C)":"65.0","GPU Memory Allocated (VRAM%)":"44","GFX Version":"gfx942"}}'
elif [ "${count}" -eq 2 ]; then
  printf '%s\n' '{"card1":{"Device Name":"MI300X","Device ID":"0x74a1","Current Socket Graphics Package Power (W)":"301.100","GPU use (%)":"41","Temperature (Sensor edge) (C)":"67.0","GPU Memory Allocated (VRAM%)":"46","GFX Version":"gfx942"}}'
else
  printf '%s\n' '{"card1":{"Device Name":"MI300X","Device ID":"0x74a1","Current Socket Graphics Package Power (W)":"199.400","GPU use (%)":"18","Temperature (Sensor edge) (C)":"63.0","GPU Memory Allocated (VRAM%)":"39","GFX Version":"gfx942"}}'
fi
`
	if err := os.WriteFile(rocmSMIPath, []byte(rocmSMIScript), 0o755); err != nil {
		t.Fatalf("write fake rocm-smi: %v", err)
	}

	cmd := exec.Command(binaryPath, "--mode", "real")
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_HIP_PID=4242",
		"PERF_AGENT_GPU_DURATION=12ms",
		"PERF_AGENT_GPU_KERNEL_NAME=collector_kernel",
		"PERF_AGENT_GPU_QUEUE_ID=compute:7",
		"PERF_AGENT_ROCM_SMI_PATH="+rocmSMIPath,
		"PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL=5ms",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample collector real mode: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 13 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.KernelName != "collector_kernel" {
		t.Fatalf("kernel_name=%q", execEv.Exec.KernelName)
	}
	if execEv.Exec.Execution.ContextID != "pid-4242" {
		t.Fatalf("context_id=%q", execEv.Exec.Execution.ContextID)
	}
	if execEv.Exec.Execution.DeviceID != "gfx942:1" {
		t.Fatalf("device_id=%q", execEv.Exec.Execution.DeviceID)
	}
	if execEv.Exec.Queue.Device.Name != "MI300X" {
		t.Fatalf("device_name=%q", execEv.Exec.Queue.Device.Name)
	}
	if execEv.Exec.Queue.QueueID != "compute:7" {
		t.Fatalf("queue_id=%q", execEv.Exec.Queue.QueueID)
	}
	wantReasons := []string{
		"hardware_gpu_use",
		"hardware_socket_power_watts",
		"hardware_temperature_c",
		"hardware_vram_used_pct",
		"hardware_gpu_use",
		"hardware_socket_power_watts",
		"hardware_temperature_c",
		"hardware_vram_used_pct",
		"hardware_gpu_use",
		"hardware_socket_power_watts",
		"hardware_temperature_c",
		"hardware_vram_used_pct",
	}
	wantWeights := []uint64{73, 276, 65, 44, 41, 301, 67, 46, 18, 199, 63, 39}
	prevTime := execEv.Exec.StartNs
	for i := 1; i < len(lines); i++ {
		ev, err := codec.DecodeLine([]byte(lines[i]))
		if err != nil {
			t.Fatalf("decode sample line %d: %v\n%s", i, err, lines[i])
		}
		if ev.Sample.StallReason != wantReasons[i-1] || ev.Sample.Weight != wantWeights[i-1] {
			t.Fatalf("sample%d=%+v", i, ev.Sample)
		}
		if !(execEv.Exec.StartNs < ev.Sample.TimeNs && ev.Sample.TimeNs < execEv.Exec.EndNs) {
			t.Fatalf("sample%d time outside exec window: exec=%+v sample=%+v", i, execEv.Exec, ev.Sample)
		}
		if ev.Sample.TimeNs < prevTime {
			t.Fatalf("sample%d time regressed: prev=%d sample=%d", i, prevTime, ev.Sample.TimeNs)
		}
		prevTime = ev.Sample.TimeNs
	}
	countBytes, err := os.ReadFile(filepath.Join(tmpDir, "rocm-smi.count"))
	if err != nil {
		t.Fatalf("read rocm-smi count: %v", err)
	}
	if strings.TrimSpace(string(countBytes)) != "3" {
		t.Fatalf("rocm-smi invocation count=%q", strings.TrimSpace(string(countBytes)))
	}
}

func TestAMDSampleCollectorBinaryRejectsROCMSMIFailureInRealMode(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)
	rocmSMIPath := filepath.Join(tmpDir, "rocm-smi")
	rocmSMIScript := `#!/bin/sh
echo 'boom' >&2
exit 7
`
	if err := os.WriteFile(rocmSMIPath, []byte(rocmSMIScript), 0o755); err != nil {
		t.Fatalf("write fake rocm-smi: %v", err)
	}

	cmd := exec.Command(binaryPath, "--mode", "real")
	cmd.Env = append(os.Environ(), "PERF_AGENT_ROCM_SMI_PATH="+rocmSMIPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected real mode failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "rocm-smi query failed") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestAMDSampleCollectorBinaryRejectsUnsupportedRealSource(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)

	cmd := exec.Command(binaryPath, "--mode", "real", "--real-source", "madeup-source")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected unsupported real source failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "unsupported amd sample real source") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestAMDSampleCollectorBinaryRejectsMissingRocprofv2Executable(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)

	cmd := exec.Command(binaryPath, "--mode", "real", "--real-source", "rocprofv2")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected missing rocprofv2 executable failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "rocprofv2 source failed") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestAMDSampleCollectorBinaryUsesRocprofv2SourcePath(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)
	rocprofv2Path := filepath.Join(tmpDir, "rocprofv2")
	rocprofv2Script := `#!/bin/sh
cat <<EOF
{"type":"dispatch","dispatch_id":"dispatch-1","start_ns":100,"end_ns":200}
{"type":"sample","dispatch_id":"dispatch-1","sample_id":"sample-1","time_ns":125,"stall_reason":"memory_wait","weight":11,"pc":"0xabc","function":"flash_attn_fwd","file":"flash_attn.cu","line":77}
{"type":"sample","dispatch_id":"dispatch-1","sample_id":"sample-2","time_ns":175,"stall_reason":"wave_barrier","weight":5,"pc":"0xdef","function":"flash_attn_epilogue","file":"flash_attn_epilogue.cu","line":91}
EOF
`
	if err := os.WriteFile(rocprofv2Path, []byte(rocprofv2Script), 0o755); err != nil {
		t.Fatalf("write fake rocprofv2: %v", err)
	}

	cmd := exec.Command(binaryPath, "--mode", "real", "--real-source", "rocprofv2")
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_ROCPROFV2_PATH="+rocprofv2Path,
		"PERF_AGENT_HIP_PID=4242",
		"PERF_AGENT_GPU_KERNEL_NAME=collector_kernel",
		"PERF_AGENT_GPU_DEVICE_ID=gfx942:0",
		"PERF_AGENT_GPU_DEVICE_NAME=MI300X",
		"PERF_AGENT_GPU_QUEUE_ID=compute:7",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample collector rocprofv2 mode: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.KernelName != "collector_kernel" {
		t.Fatalf("kernel_name=%q", execEv.Exec.KernelName)
	}
	if execEv.Exec.Execution.ContextID != "pid-4242" {
		t.Fatalf("context_id=%q", execEv.Exec.Execution.ContextID)
	}
	if execEv.Exec.Execution.DeviceID != "gfx942:0" {
		t.Fatalf("device_id=%q", execEv.Exec.Execution.DeviceID)
	}
	if execEv.Exec.Queue.Device.Name != "MI300X" {
		t.Fatalf("device_name=%q", execEv.Exec.Queue.Device.Name)
	}
	if execEv.Exec.Queue.QueueID != "compute:7" {
		t.Fatalf("queue_id=%q", execEv.Exec.Queue.QueueID)
	}
	sample1, err := codec.DecodeLine([]byte(lines[1]))
	if err != nil {
		t.Fatalf("decode sample1 line: %v\n%s", err, lines[1])
	}
	if sample1.Sample.PC != 0xabc {
		t.Fatalf("sample1 pc=%#x", sample1.Sample.PC)
	}
	if sample1.Sample.Function != "flash_attn_fwd" {
		t.Fatalf("sample1 function=%q", sample1.Sample.Function)
	}
	if sample1.Sample.File != "flash_attn.cu" {
		t.Fatalf("sample1 file=%q", sample1.Sample.File)
	}
	if sample1.Sample.Line != 77 {
		t.Fatalf("sample1 line=%d", sample1.Sample.Line)
	}
	sample2, err := codec.DecodeLine([]byte(lines[2]))
	if err != nil {
		t.Fatalf("decode sample2 line: %v\n%s", err, lines[2])
	}
	if sample2.Sample.PC != 0xdef {
		t.Fatalf("sample2 pc=%#x", sample2.Sample.PC)
	}
	if sample2.Sample.Function != "flash_attn_epilogue" {
		t.Fatalf("sample2 function=%q", sample2.Sample.Function)
	}
	if sample2.Sample.File != "flash_attn_epilogue.cu" {
		t.Fatalf("sample2 file=%q", sample2.Sample.File)
	}
	if sample2.Sample.Line != 91 {
		t.Fatalf("sample2 line=%d", sample2.Sample.Line)
	}
}

func TestAMDSampleCollectorBinaryUsesAlternateRocprofv2NativeShape(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)
	rocprofv2Path := filepath.Join(tmpDir, "rocprofv2")
	rocprofv2Script := `#!/bin/sh
cat <<EOF
{"type":"dispatch","correlation_id":"dispatch-9","begin_ns":1000,"complete_ns":1400}
{"type":"sample","correlation_id":"dispatch-9","timestamp_ns":1100,"stall_reason":"inst_fetch","weight":3,"location":{"pc":"0x1234","function":"attention_kernel","file":"attention.hip","line":44}}
{"type":"sample","correlation_id":"dispatch-9","sample_id":"sample-9b","timestamp_ns":1300,"stall_reason":"memory_wait","weight":7,"location":{"pc":"0x5678","function":"attention_epilogue","file":"attention_epilogue.hip","line":58}}
EOF
`
	if err := os.WriteFile(rocprofv2Path, []byte(rocprofv2Script), 0o755); err != nil {
		t.Fatalf("write fake rocprofv2: %v", err)
	}

	cmd := exec.Command(binaryPath, "--mode", "real", "--real-source", "rocprofv2")
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_ROCPROFV2_PATH="+rocprofv2Path,
		"PERF_AGENT_HIP_PID=9898",
		"PERF_AGENT_GPU_KERNEL_NAME=alternate_kernel",
		"PERF_AGENT_GPU_DEVICE_ID=gfx950:1",
		"PERF_AGENT_GPU_DEVICE_NAME=MI325X",
		"PERF_AGENT_GPU_QUEUE_ID=compute:11",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample collector alternate rocprofv2 mode: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.Execution.ExecID != "dispatch-9" {
		t.Fatalf("exec_id=%q", execEv.Exec.Execution.ExecID)
	}
	if execEv.Exec.StartNs != 1000 || execEv.Exec.EndNs != 1400 {
		t.Fatalf("exec window=%d..%d", execEv.Exec.StartNs, execEv.Exec.EndNs)
	}
	sample1, err := codec.DecodeLine([]byte(lines[1]))
	if err != nil {
		t.Fatalf("decode sample1 line: %v\n%s", err, lines[1])
	}
	if sample1.Sample.Correlation.Value != "dispatch-9:1100" {
		t.Fatalf("sample1 correlation=%q", sample1.Sample.Correlation.Value)
	}
	if sample1.Sample.TimeNs != 1100 {
		t.Fatalf("sample1 time_ns=%d", sample1.Sample.TimeNs)
	}
	if sample1.Sample.PC != 0x1234 {
		t.Fatalf("sample1 pc=%#x", sample1.Sample.PC)
	}
	if sample1.Sample.Function != "attention_kernel" || sample1.Sample.File != "attention.hip" || sample1.Sample.Line != 44 {
		t.Fatalf("sample1 location=%q %q %d", sample1.Sample.Function, sample1.Sample.File, sample1.Sample.Line)
	}
	sample2, err := codec.DecodeLine([]byte(lines[2]))
	if err != nil {
		t.Fatalf("decode sample2 line: %v\n%s", err, lines[2])
	}
	if sample2.Sample.Correlation.Value != "sample-9b" {
		t.Fatalf("sample2 correlation=%q", sample2.Sample.Correlation.Value)
	}
	if sample2.Sample.TimeNs != 1300 {
		t.Fatalf("sample2 time_ns=%d", sample2.Sample.TimeNs)
	}
	if sample2.Sample.PC != 0x5678 {
		t.Fatalf("sample2 pc=%#x", sample2.Sample.PC)
	}
	if sample2.Sample.Function != "attention_epilogue" || sample2.Sample.File != "attention_epilogue.hip" || sample2.Sample.Line != 58 {
		t.Fatalf("sample2 location=%q %q %d", sample2.Sample.Function, sample2.Sample.File, sample2.Sample.Line)
	}
}

func TestAMDSampleCollectorBinaryUsesRocprofv2OutputPath(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)
	rocprofv2Path := filepath.Join(tmpDir, "rocprofv2")
	outputPath := filepath.Join(tmpDir, "rocprofv2.jsonl")
	rocprofv2Script := fmt.Sprintf(`#!/bin/sh
cat <<EOF >%q
{"type":"dispatch","correlation_id":"dispatch-file-1","begin_ns":200,"complete_ns":260}
{"type":"sample","correlation_id":"dispatch-file-1","timestamp_ns":220,"stall_reason":"memory_wait","weight":4,"location":{"pc":"0x111","function":"file_kernel","file":"file_kernel.hip","line":12}}
EOF
`, outputPath)
	if err := os.WriteFile(rocprofv2Path, []byte(rocprofv2Script), 0o755); err != nil {
		t.Fatalf("write fake rocprofv2: %v", err)
	}

	cmd := exec.Command(binaryPath, "--mode", "real", "--real-source", "rocprofv2")
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_ROCPROFV2_PATH="+rocprofv2Path,
		"PERF_AGENT_ROCPROFV2_OUTPUT_PATH="+outputPath,
		"PERF_AGENT_GPU_KERNEL_NAME=file_kernel_name",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample collector rocprofv2 output-path mode: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.Execution.ExecID != "dispatch-file-1" {
		t.Fatalf("exec_id=%q", execEv.Exec.Execution.ExecID)
	}
	if execEv.Exec.KernelName != "file_kernel_name" {
		t.Fatalf("kernel_name=%q", execEv.Exec.KernelName)
	}
	sampleEv, err := codec.DecodeLine([]byte(lines[1]))
	if err != nil {
		t.Fatalf("decode sample line: %v\n%s", err, lines[1])
	}
	if sampleEv.Sample.PC != 0x111 || sampleEv.Sample.Function != "file_kernel" || sampleEv.Sample.File != "file_kernel.hip" || sampleEv.Sample.Line != 12 {
		t.Fatalf("sample=%+v", sampleEv.Sample)
	}
}

func TestAMDSampleCollectorBinaryRejectsRocprofv2Failure(t *testing.T) {
	tmpDir := t.TempDir()
	binaryPath := buildAMDSampleCollector(t, tmpDir)
	rocprofv2Path := filepath.Join(tmpDir, "rocprofv2")
	rocprofv2Script := `#!/bin/sh
echo 'boom' >&2
exit 9
`
	if err := os.WriteFile(rocprofv2Path, []byte(rocprofv2Script), 0o755); err != nil {
		t.Fatalf("write fake rocprofv2: %v", err)
	}

	cmd := exec.Command(binaryPath, "--mode", "real", "--real-source", "rocprofv2")
	cmd.Env = append(os.Environ(), "PERF_AGENT_ROCPROFV2_PATH="+rocprofv2Path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected rocprofv2 failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "rocprofv2 source failed") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestAMDSampleAdapterScriptPassesCollectorModeToGoFallback(t *testing.T) {
	tmpDir := t.TempDir()
	fakeGo := filepath.Join(tmpDir, "go")
	fakeGoScript := `#!/bin/sh
printf '%s %s\n' "${PERF_AGENT_AMD_SAMPLE_MODE:-}" "$*"
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(
		os.Environ(),
		"PATH="+tmpDir+":"+os.Getenv("PATH"),
		"PERF_AGENT_AMD_SAMPLE_MODE=real",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample adapter go fallback with mode: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "real run ./cmd/amd-sample-collector" {
		t.Fatalf("output=%q", got)
	}
}

func TestAMDSampleAdapterScriptPassesRealSourceToGoFallback(t *testing.T) {
	tmpDir := t.TempDir()
	fakeGo := filepath.Join(tmpDir, "go")
	fakeGoScript := `#!/bin/sh
printf '%s %s %s\n' "${PERF_AGENT_AMD_SAMPLE_MODE:-}" "${PERF_AGENT_AMD_SAMPLE_REAL_SOURCE:-}" "$*"
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(
		os.Environ(),
		"PATH="+tmpDir+":"+os.Getenv("PATH"),
		"PERF_AGENT_AMD_SAMPLE_MODE=real",
		"PERF_AGENT_AMD_SAMPLE_REAL_SOURCE=rocm-smi",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample adapter go fallback with real source: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "real rocm-smi run ./cmd/amd-sample-collector" {
		t.Fatalf("output=%q", got)
	}
}

func TestAMDSampleAdapterScriptFallsBackToProducerWithKernelContext(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_GPU_KERNEL_NAME=adapter_kernel",
		"PERF_AGENT_GPU_DURATION=2s",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample adapter fallback: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.KernelName != "adapter_kernel" {
		t.Fatalf("kernel_name=%q", execEv.Exec.KernelName)
	}
	if got := execEv.Exec.EndNs - execEv.Exec.StartNs; got != 2_000_000_000 {
		t.Fatalf("duration_ns=%d", got)
	}
}

func TestAMDSampleAdapterScriptPrefersGoCollectorFallback(t *testing.T) {
	tmpDir := t.TempDir()
	fakeGo := filepath.Join(tmpDir, "go")
	fakeGoScript := `#!/bin/sh
printf 'fake-go:%s\n' "$*"
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(os.Environ(), "PATH="+tmpDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample adapter go collector fallback: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "fake-go:run ./cmd/amd-sample-collector" {
		t.Fatalf("output=%q", got)
	}
}

func TestAMDSampleAdapterScriptExecsCollectorPathBinary(t *testing.T) {
	tmpDir := t.TempDir()
	collector := buildAMDSampleCollector(t, tmpDir)

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH="+collector,
		"PERF_AGENT_HIP_PID=4242",
		"PERF_AGENT_GPU_KERNEL_NAME=collector_kernel",
		"PERF_AGENT_GPU_DEVICE_ID=gfx942:0",
		"PERF_AGENT_GPU_DEVICE_NAME=MI300X",
		"PERF_AGENT_GPU_QUEUE_ID=compute:7",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample adapter collector path binary: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	execEv, err := codec.DecodeLine([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode exec line: %v\n%s", err, lines[0])
	}
	if execEv.Exec.KernelName != "collector_kernel" {
		t.Fatalf("kernel_name=%q", execEv.Exec.KernelName)
	}
	if execEv.Exec.Execution.DeviceID != "gfx942:0" {
		t.Fatalf("device_id=%q", execEv.Exec.Execution.DeviceID)
	}
	if execEv.Exec.Queue.Device.Name != "MI300X" {
		t.Fatalf("device_name=%q", execEv.Exec.Queue.Device.Name)
	}
	if execEv.Exec.Queue.QueueID != "compute:7" {
		t.Fatalf("queue_id=%q", execEv.Exec.Queue.QueueID)
	}
}

func TestAMDSampleAdapterScriptRunsCollectorCommand(t *testing.T) {
	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(os.Environ(), "PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND=printf adapter-external")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("amd sample adapter collector command: %v\n%s", err, out)
	}
	if got := string(out); got != "adapter-external" {
		t.Fatalf("output=%q", got)
	}
}

func TestAMDSampleAdapterScriptRejectsCollectorPathWithCommand(t *testing.T) {
	tmpDir := t.TempDir()
	collector := filepath.Join(tmpDir, "collector.sh")
	if err := os.WriteFile(collector, []byte("#!/bin/sh\nprintf path-wins\n"), 0o755); err != nil {
		t.Fatalf("write collector: %v", err)
	}

	cmd := exec.Command(
		"bash",
		filepath.Join("scripts", "amd-sample-adapter.sh"),
	)
	cmd.Env = append(
		os.Environ(),
		"PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH="+collector,
		"PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND=printf command-loses",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected collector conflict failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "cannot combine PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH with PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND") {
		t.Fatalf("unexpected output:\n%s", out)
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

func buildAMDSampleCollector(t *testing.T, dir string) string {
	t.Helper()

	binaryPath := filepath.Join(dir, "amd-sample-collector")
	buildCmd := exec.Command(
		"go",
		"build",
		"-o",
		binaryPath,
		"./cmd/amd-sample-collector",
	)
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build amd sample collector: %v\n%s", err, buildOut)
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
