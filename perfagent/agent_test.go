package perfagent

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	goprofile "github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		wantErr string
	}{
		{
			name:    "requires at least one mode",
			opts:    []Option{WithSystemWide()},
			wantErr: "at least one of",
		},
		{
			name:    "requires pid or system-wide",
			opts:    []Option{WithPMU()},
			wantErr: "either PID or system-wide",
		},
		{
			name: "last option wins - system-wide after pid",
			opts: []Option{WithPID(123), WithSystemWide(), WithPMU()},
			// No error: WithSystemWide() resets PID to 0, so config is valid
		},
		{
			name: "valid system-wide PMU",
			opts: []Option{WithSystemWide(), WithPMU()},
		},
		{
			name: "valid PID profile",
			opts: []Option{WithPID(1), WithCPUProfile("")},
		},
		{
			name: "valid system-wide CPU profile",
			opts: []Option{WithSystemWide(), WithCPUProfile("")},
		},
		{
			name: "valid system-wide off-CPU profile",
			opts: []Option{WithSystemWide(), WithOffCPUProfile("")},
		},
		{
			name: "valid GPU stream mode",
			opts: []Option{WithGPUStreamInput(strings.NewReader(""))},
		},
		{
			name: "valid AMD sample gpu mode",
			opts: []Option{WithGPUAMDSampleInput(strings.NewReader(""))},
		},
		{
			name: "valid linuxdrm gpu mode",
			opts: []Option{WithPID(123), WithGPULinuxDRM()},
		},
		{
			name: "valid linuxkfd gpu mode",
			opts: []Option{WithPID(123), WithGPULinuxKFD()},
		},
		{
			name: "valid GPU host replay plus stream mode",
			opts: []Option{
				WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
				WithGPUStreamInput(strings.NewReader("")),
			},
		},
		{
			name: "valid GPU HIP host plus stream mode",
			opts: []Option{
				WithPID(123),
				WithGPUHostHIP("/opt/rocm/lib/libamdhip64.so", "hipLaunchKernel"),
				WithGPUStreamInput(strings.NewReader("")),
			},
		},
		{
			name:    "per-pid requires system-wide",
			opts:    []Option{WithPID(1), WithPMU(), WithPerPID()},
			wantErr: "per-PID requires system-wide",
		},
		{
			name:    "per-pid requires PMU",
			opts:    []Option{WithSystemWide(), WithCPUProfile(""), WithPerPID()},
			wantErr: "per-PID is only valid with PMU",
		},
		{
			name: "rejects multiple GPU sources",
			opts: []Option{
				WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "flash_attn.json")),
				WithGPUStreamInput(strings.NewReader("")),
			},
			wantErr: "gpu source",
		},
		{
			name: "rejects AMD sample plus stream mode",
			opts: []Option{
				WithGPUAMDSampleInput(strings.NewReader("")),
				WithGPUStreamInput(strings.NewReader("")),
			},
			wantErr: "gpu source",
		},
		{
			name:    "linuxdrm requires pid",
			opts:    []Option{WithGPULinuxDRM()},
			wantErr: "linuxdrm backend requires pid",
		},
		{
			name:    "linuxkfd requires pid",
			opts:    []Option{WithGPULinuxKFD()},
			wantErr: "linuxkfd backend requires pid",
		},
		{
			name:    "linuxdrm rejects system-wide",
			opts:    []Option{WithSystemWide(), WithGPULinuxDRM()},
			wantErr: "linuxdrm backend does not support system-wide mode",
		},
		{
			name:    "linuxkfd rejects system-wide",
			opts:    []Option{WithSystemWide(), WithGPULinuxKFD()},
			wantErr: "linuxkfd backend does not support system-wide mode",
		},
		{
			name: "hip host requires pid",
			opts: []Option{
				WithGPUHostHIP("/opt/rocm/lib/libamdhip64.so", "hipLaunchKernel"),
				WithGPUStreamInput(strings.NewReader("")),
			},
			wantErr: "hip host source requires pid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts...)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, 99, cfg.SampleRate)
	assert.Equal(t, "profile.pb.gz", cfg.CPUProfilePath)
	assert.Equal(t, "offcpu.pb.gz", cfg.OffCPUProfilePath)
	assert.Nil(t, cfg.CPUProfileWriter)
	assert.Nil(t, cfg.OffCPUProfileWriter)
}

func TestOptionsApply(t *testing.T) {
	cfg := DefaultConfig()

	opts := []Option{
		WithPID(1234),
		WithSampleRate(199),
		WithTags("key=value", "env=test"),
	}

	for _, opt := range opts {
		opt(cfg)
	}

	assert.Equal(t, 1234, cfg.PID)
	assert.Equal(t, 199, cfg.SampleRate)
	assert.Equal(t, []string{"key=value", "env=test"}, cfg.Tags)
}

func TestWithCPUProfileWriter(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()

	WithCPUProfileWriter(&buf)(cfg)

	assert.True(t, cfg.EnableCPUProfile)
	assert.Equal(t, &buf, cfg.CPUProfileWriter)
}

func TestWithOffCPUProfileWriter(t *testing.T) {
	var buf bytes.Buffer
	cfg := DefaultConfig()

	WithOffCPUProfileWriter(&buf)(cfg)

	assert.True(t, cfg.EnableOffCPUProfile)
	assert.Equal(t, &buf, cfg.OffCPUProfileWriter)
}

func TestWithGPUHIPLinuxDRMJoinWindow(t *testing.T) {
	cfg := DefaultConfig()

	WithGPUHIPLinuxDRMJoinWindow(25 * time.Millisecond)(cfg)

	assert.Equal(t, 25*time.Millisecond, cfg.GPUHIPLinuxDRMJoinWindow)
}

func TestWithPIDDisablesSystemWide(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SystemWide = true

	WithPID(123)(cfg)

	assert.Equal(t, 123, cfg.PID)
	assert.False(t, cfg.SystemWide)
}

func TestWithSystemWideDisablesPID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PID = 123

	WithSystemWide()(cfg)

	assert.Equal(t, 0, cfg.PID)
	assert.True(t, cfg.SystemWide)
}

func TestWithCPUProfileSetsPath(t *testing.T) {
	cfg := DefaultConfig()

	WithCPUProfile("/custom/path.pb.gz")(cfg)

	assert.True(t, cfg.EnableCPUProfile)
	assert.Equal(t, "/custom/path.pb.gz", cfg.CPUProfilePath)
}

func TestWithCPUProfileEmptyPathUsesDefault(t *testing.T) {
	cfg := DefaultConfig()
	defaultPath := cfg.CPUProfilePath

	WithCPUProfile("")(cfg)

	assert.True(t, cfg.EnableCPUProfile)
	assert.Equal(t, defaultPath, cfg.CPUProfilePath)
}

func TestWithOffCPUProfileSetsPath(t *testing.T) {
	cfg := DefaultConfig()

	WithOffCPUProfile("/custom/offcpu.pb.gz")(cfg)

	assert.True(t, cfg.EnableOffCPUProfile)
	assert.Equal(t, "/custom/offcpu.pb.gz", cfg.OffCPUProfilePath)
}

func TestGPUManagerConfigForHIPLinuxDRMDefaultsWindow(t *testing.T) {
	agent, err := New(
		WithPID(123),
		WithGPULinuxDRM(),
		WithGPUHostHIP("/opt/rocm/lib/libamdhip64.so", "hipLaunchKernel"),
	)
	require.NoError(t, err)

	cfg := agent.gpuManagerConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, uint64(defaultGPUHIPLinuxDRMJoinWindow), cfg.LaunchEventJoinWindowNs)
}

func TestGPUManagerConfigForHIPLinuxDRMUsesOverride(t *testing.T) {
	agent, err := New(
		WithPID(123),
		WithGPULinuxDRM(),
		WithGPUHostHIP("/opt/rocm/lib/libamdhip64.so", "hipLaunchKernel"),
		WithGPUHIPLinuxDRMJoinWindow(2*time.Millisecond),
	)
	require.NoError(t, err)

	cfg := agent.gpuManagerConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, uint64(2*time.Millisecond), cfg.LaunchEventJoinWindowNs)
}

func TestAgentGPUEventBackendsForLinuxDRMMode(t *testing.T) {
	agent, err := New(
		WithPID(123),
		WithGPULinuxDRM(),
	)
	require.NoError(t, err)

	got, err := agent.GPUEventBackends()
	require.NoError(t, err)
	assert.Equal(t, []gpu.GPUBackendID{gpu.BackendLinuxDRM, gpu.BackendLinuxKFD}, got)
}

func TestAgentGPUEventBackendsForLinuxKFDMode(t *testing.T) {
	agent, err := New(
		WithPID(123),
		WithGPULinuxKFD(),
	)
	require.NoError(t, err)

	got, err := agent.GPUEventBackends()
	require.NoError(t, err)
	assert.Equal(t, []gpu.GPUBackendID{gpu.BackendLinuxKFD}, got)
}

func TestAgentGPUEventBackendsForStreamModeIsDynamic(t *testing.T) {
	agent, err := New(
		WithGPUStreamInput(strings.NewReader("")),
	)
	require.NoError(t, err)

	got, err := agent.GPUEventBackends()
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestWithCPUs(t *testing.T) {
	cfg := DefaultConfig()

	WithCPUs([]uint{0, 2, 4})(cfg)

	assert.Equal(t, []uint{0, 2, 4}, cfg.CPUs)
}

func TestAgentConfigCopy(t *testing.T) {
	agent, err := New(
		WithSystemWide(),
		WithPMU(),
		WithSampleRate(49),
	)
	require.NoError(t, err)

	cfg := agent.Config()

	assert.True(t, cfg.SystemWide)
	assert.True(t, cfg.EnablePMU)
	assert.Equal(t, 49, cfg.SampleRate)

	// Verify it's a copy (modifying doesn't affect original)
	cfg.SampleRate = 100
	originalCfg := agent.Config()
	assert.Equal(t, 49, originalCfg.SampleRate)
}

func TestAgentGPUReplayMode(t *testing.T) {
	agent, err := New(
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "flash_attn.json")),
		WithGPURawOutput(io.Discard),
		WithGPUProfileOutput(io.Discard),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
}

func TestAgentGPUStreamMode(t *testing.T) {
	var raw bytes.Buffer
	var folded bytes.Buffer
	var profile bytes.Buffer

	agent, err := New(
		WithGPUStreamInput(strings.NewReader(
			"{\"kind\":\"launch\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":100}\n"+
				"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
		)),
		WithGPURawOutput(&raw),
		WithGPUFoldedOutput(&folded),
		WithGPUProfileOutput(&profile),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	assert.Contains(t, raw.String(), "flash_attn_fwd")
	assert.Contains(t, folded.String(), "[gpu:kernel:flash_attn_fwd]")
	assert.NotZero(t, profile.Len())
}

func TestAgentHostReplayPlusGPUStreamMode(t *testing.T) {
	var raw bytes.Buffer
	var profile bytes.Buffer

	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUStreamInput(strings.NewReader(
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
		)),
		WithGPURawOutput(&raw),
		WithGPUProfileOutput(&profile),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	assert.Contains(t, raw.String(), "flash_attn_fwd")
	assert.Contains(t, raw.String(), "train_step")
	assert.NotZero(t, profile.Len())
}

func TestAgentHostReplayPlusGPUEventReplayMode(t *testing.T) {
	dir := t.TempDir()
	hostPath := filepath.Join(dir, "host.json")
	gpuPath := filepath.Join(dir, "gpu.json")

	hostFixture := `[
  {
    "backend": "stream",
    "pid": 4242,
    "tid": 4243,
    "time_ns": 100,
    "cpu_stack": [
      { "Name": "train_step" },
      { "Name": "cudaLaunchKernel" }
    ],
    "kernel_name": "flash_attn_fwd",
    "queue_id": "q7",
    "context_id": "ctx0",
    "correlation_id": "c1",
    "tags": {
      "cgroup_id": "9876",
      "pod_uid": "pod-abc"
    },
    "source": "host-replay"
  }
]`
	if err := os.WriteFile(hostPath, []byte(hostFixture), 0o644); err != nil {
		t.Fatalf("WriteFile host: %v", err)
	}

	gpuFixture := `[
  {
    "kind": "event",
    "event": {
      "backend": "linuxdrm",
      "kind": "submit",
      "name": "amdgpu-cs",
      "time_ns": 130,
      "duration_ns": 13,
      "pid": 4242,
      "tid": 4243,
      "source": "replay"
    }
  }
]`
	if err := os.WriteFile(gpuPath, []byte(gpuFixture), 0o644); err != nil {
		t.Fatalf("WriteFile gpu: %v", err)
	}

	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(hostPath),
		WithGPUReplayInput(gpuPath),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	assert.Contains(t, folded.String(), "train_step;cudaLaunchKernel;[gpu:cgroup:9876];[gpu:pod:pod-abc];[gpu:launch];[gpu:event:submit:amdgpu-cs] 13")
}

func TestAgentHostReplayPlusCheckedInGPUEventReplayMode(t *testing.T) {
	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.json")),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.folded"))
	require.NoError(t, err)
	assert.Equal(t, string(want), folded.String())
}

func TestAgentHostReplayPlusCheckedInGPUEventReplayRawJSONGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.json")),
		WithGPURawOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.raw.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInGPUEventReplayAttributionGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.json")),
		WithGPUAttributionOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.attributions.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUEventReplayRawJSONGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.json")),
		WithGPURawOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.raw.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUEventReplayAttributionGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.json")),
		WithGPUAttributionOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.attributions.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUEventReplayFoldedGolden(t *testing.T) {
	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.json")),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.folded"))
	require.NoError(t, err)
	assert.Equal(t, string(want), folded.String())
}

func TestAgentHostReplayPlusGPUStreamRawJSONGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUStreamInput(strings.NewReader(
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
		)),
		WithGPURawOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.raw.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInGPUExecutionReplayRawJSONGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.json")),
		WithGPURawOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.raw.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInGPUExecutionReplayAttributionGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.json")),
		WithGPUAttributionOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.attributions.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUExecutionReplayRawJSONGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.json")),
		WithGPURawOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.raw.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUExecutionReplayAttributionGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.json")),
		WithGPUAttributionOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.attributions.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusGPUStreamFoldedGolden(t *testing.T) {
	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUStreamInput(strings.NewReader(
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
		)),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.folded"))
	require.NoError(t, err)
	assert.Equal(t, string(want), folded.String())
}

func TestAgentHostReplayPlusCheckedInGPUExecutionReplayFoldedGolden(t *testing.T) {
	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.json")),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.folded"))
	require.NoError(t, err)
	assert.Equal(t, string(want), folded.String())
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUExecutionReplayFoldedGolden(t *testing.T) {
	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.json")),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.folded"))
	require.NoError(t, err)
	assert.Equal(t, string(want), folded.String())
}

func TestAgentHostReplayPlusCheckedInGPUEventReplayProfileGolden(t *testing.T) {
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.json")),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)

	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_driver_submit.pprof.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUEventReplayProfileGolden(t *testing.T) {
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.json")),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)

	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_submit.pprof.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestAgentHostReplayPlusGPUStreamProfileGolden(t *testing.T) {
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUStreamInput(strings.NewReader(
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
		)),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)

	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.pprof.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestAgentHostReplayPlusGPUAMDSampleOutputsExecutionFrames(t *testing.T) {
	var foldedBuf bytes.Buffer
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "hip_kfd_launches.json")),
		WithGPUAMDSampleInput(strings.NewReader(
			"{\"kind\":\"exec\",\"execution\":{\"backend\":\"amdsample\",\"device_id\":\"gfx1103:0\",\"queue_id\":\"compute:0\",\"context_id\":\"ctx0\",\"exec_id\":\"dispatch-1\"},\"correlation\":{\"backend\":\"hip\",\"value\":\"hip:555:555:100\"},\"queue\":{\"backend\":\"amdsample\",\"device\":{\"backend\":\"amdsample\",\"device_id\":\"gfx1103:0\",\"name\":\"AMD Radeon 780M Graphics\"},\"queue_id\":\"compute:0\"},\"kernel_name\":\"hip_launch_shim_kernel\",\"start_ns\":120,\"end_ns\":260}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"hip\",\"value\":\"hip:555:555:100\"},\"device\":{\"backend\":\"amdsample\",\"device_id\":\"gfx1103:0\",\"name\":\"AMD Radeon 780M Graphics\"},\"time_ns\":150,\"kernel_name\":\"hip_launch_shim_kernel\",\"stall_reason\":\"memory_wait\",\"weight\":11}\n"+
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"hip\",\"value\":\"hip:555:555:100\"},\"device\":{\"backend\":\"amdsample\",\"device_id\":\"gfx1103:0\",\"name\":\"AMD Radeon 780M Graphics\"},\"time_ns\":210,\"kernel_name\":\"hip_launch_shim_kernel\",\"stall_reason\":\"wave_barrier\",\"weight\":5}\n",
		)),
		WithGPUFoldedOutput(&foldedBuf),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	folded := foldedBuf.String()
	assert.Contains(t, folded, "train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:memory_wait] 11")
	assert.Contains(t, folded, "train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:wave_barrier] 5")

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)
	assert.Contains(t, got, "train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:memory_wait]")
	assert.Contains(t, got, "train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:wave_barrier]")
}

func TestAgentHostReplayPlusCheckedInGPUExecutionReplayProfileGolden(t *testing.T) {
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.json")),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)

	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "host_exec_sample.pprof.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestAgentHostReplayPlusCheckedInHIPKFDReplayRawJSONGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "hip_kfd_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.json")),
		WithGPURawOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.raw.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInHIPKFDReplayAttributionGolden(t *testing.T) {
	var raw bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "hip_kfd_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.json")),
		WithGPUAttributionOutput(&raw),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.attributions.json"))
	require.NoError(t, err)
	assert.Equal(t, string(want), raw.String())
}

func TestAgentHostReplayPlusCheckedInHIPKFDReplayFoldedGolden(t *testing.T) {
	var folded bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "hip_kfd_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.json")),
		WithGPUFoldedOutput(&folded),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.folded"))
	require.NoError(t, err)
	assert.Equal(t, string(want), folded.String())
}

func TestAgentHostReplayPlusCheckedInHIPKFDReplayProfileGolden(t *testing.T) {
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "hip_kfd_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.json")),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)

	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "hip_kfd_memory.pprof.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func TestAgentHostReplayPlusCheckedInMultiWorkloadGPUExecutionReplayProfileGolden(t *testing.T) {
	var profileBuf bytes.Buffer
	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "multi_workload_launches.json")),
		WithGPUReplayInput(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.json")),
		WithGPUProfileOutput(&profileBuf),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))

	prof, err := goprofile.Parse(&profileBuf)
	require.NoError(t, err)
	got := flattenedSampleStacks(prof)

	want, err := os.ReadFile(filepath.Join("..", "gpu", "testdata", "replay", "multi_workload_exec.pprof.txt"))
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func flattenedSampleStacks(prof *goprofile.Profile) string {
	var b strings.Builder
	for _, sample := range prof.Sample {
		var frames []string
		for _, loc := range sample.Location {
			if len(loc.Line) == 0 || loc.Line[0].Function == nil {
				continue
			}
			frames = append(frames, loc.Line[0].Function.Name)
		}
		b.WriteString(strings.Join(frames, ";"))
		b.WriteByte(' ')
		if len(sample.Value) > 0 {
			b.WriteString(strconv.FormatInt(sample.Value[0], 10))
		} else {
			b.WriteByte('0')
		}
		b.WriteByte('\n')
	}
	return b.String()
}
