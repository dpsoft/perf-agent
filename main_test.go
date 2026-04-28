package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/perfagent"
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
		"--gpu-attribution-output /tmp/gpu-live-demo/live_hip_linuxdrm.attributions.json",
		"--gpu-folded-output /tmp/gpu-live-demo/live_hip_linuxdrm.folded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in output:\n%s", want, got)
		}
	}
}
