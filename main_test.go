package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
