package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func frameNames(frames []pp.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Name)
	}
	return out
}

func TestProjectionPutsStackInFramesAndDetailInLabels(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 10)
	l.Launch.CPUStack = pp.FramesFromNames([]string{"train_step", "cudaLaunchKernel"})
	l.Launch.Tags = map[string]string{"pod_uid": "pod-a"}
	require.NoError(t, tl.EmitLaunch(l))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		PCOffset:    0x1a40, StallReason: "long_scoreboard", Count: 5, TimeNs: 25,
	}))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)

	assert.Equal(t,
		[]string{"train_step", "cudaLaunchKernel", "[gpu:launch]", "[gpu:kernel:k_a]"},
		frameNames(samples[0].Stack),
		"frames carry the CPU stack, the boundary marker and the kernel - nothing else")

	assert.Equal(t, "long_scoreboard", samples[0].Labels["gpu_stall"])
	assert.Equal(t, "0x1a40", samples[0].Labels["gpu_pc"])
	assert.Equal(t, "pod-a", samples[0].Labels["pod_uid"])
}

func TestProjectionKeepsPCOutOfStackIdentity(t *testing.T) {
	// Two PC samples from one kernel must share a stack and differ only by
	// label. Putting the PC in a frame would make every sampled instruction a
	// distinct flame-graph leaf.
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 10)
	l.Launch.CPUStack = pp.FramesFromNames([]string{"train_step"})
	require.NoError(t, tl.EmitLaunch(l))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))
	for _, pc := range []uint64{0x100, 0x200} {
		require.NoError(t, tl.EmitPCSample(GPUPCSample{
			Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
			PCOffset:    pc, StallReason: "barrier", Count: 1, TimeNs: 25,
		}))
	}

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 2)
	assert.Equal(t, frameNames(samples[0].Stack), frameNames(samples[1].Stack),
		"PC must not appear in frames")
	assert.NotEqual(t, samples[0].Labels["gpu_pc"], samples[1].Labels["gpu_pc"])
}

func TestProjectionReservedLabelsWinOverTags(t *testing.T) {
	// A launch's Tags are producer-supplied (CLI --tag flags, cgroup/k8s
	// attribution) and can carry any key, including one that collides with a
	// name this package itself derives from the joined execution/PC sample.
	// Reserved gpu_* names must always report the real, profiler-derived
	// value - a tag must never be able to forge it.
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 10)
	l.Launch.Tags = map[string]string{
		"gpu_queue":       "HIJACKED",
		"gpu_device":      "HIJACKED",
		"gpu_correlation": "HIJACKED",
		"gpu_stall":       "HIJACKED",
		"gpu_pc":          "HIJACKED",
	}
	require.NoError(t, tl.EmitLaunch(l))

	exec := execFor("a", 20, 30)
	exec.Queue = GPUQueueRef{Backend: BackendCUPTI, QueueID: "q1", Device: GPUDeviceRef{DeviceID: "dev1"}}
	require.NoError(t, tl.EmitExec(exec))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		PCOffset:    0x1a40, StallReason: "long_scoreboard", Count: 1, TimeNs: 25,
	}))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)

	assert.Equal(t, "q1", samples[0].Labels["gpu_queue"], "a tag named gpu_queue must not override the real queue")
	assert.Equal(t, "dev1", samples[0].Labels["gpu_device"], "a tag named gpu_device must not override the real device")
	assert.Equal(t, "cupti:a", samples[0].Labels["gpu_correlation"], "a tag named gpu_correlation must not override the real correlation")
	assert.Equal(t, "long_scoreboard", samples[0].Labels["gpu_stall"], "a tag named gpu_stall must not override the real stall reason")
	assert.Equal(t, "0x1a40", samples[0].Labels["gpu_pc"], "a tag named gpu_pc must not override the real pc")
}

func TestProjectionSetsPidSampleTypeAndAggregationFromLaunch(t *testing.T) {
	// PID must come from the joined launch's LaunchContext.PID specifically,
	// not TID - launch()'s default fixture sets both to 1, which would hide
	// a PID/TID mix-up, so this test deliberately makes them differ.
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 10)
	l.Launch.PID = 42
	l.Launch.TID = 99
	require.NoError(t, tl.EmitLaunch(l))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)

	assert.Equal(t, uint32(42), samples[0].Pid, "pid must come from the joined launch's PID, not TID or anything else")
	assert.Equal(t, pp.SampleTypeCpu, samples[0].SampleType)
	assert.Equal(t, pp.SampleAggregated, samples[0].Aggregation)
}

func TestProjectionFallsBackToExecutionDuration(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 90)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, uint64(70), samples[0].Value,
		"with no PC samples the execution interval is the weight")
}

func TestProjectionHandlesUnmatchedExecution(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(execFor("ghost", 20, 30)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, []string{"[gpu:launch]", "[gpu:kernel:k_ghost]"}, frameNames(samples[0].Stack),
		"an execution with no launch still projects, without a fabricated CPU stack")
	assert.Equal(t, uint32(0), samples[0].Pid,
		"no launch means no pid to read - pinned at 0 rather than fabricated")
	assert.Equal(t, pp.SampleTypeCpu, samples[0].SampleType)
	assert.Equal(t, pp.SampleAggregated, samples[0].Aggregation)
}
