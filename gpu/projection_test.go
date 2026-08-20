package gpu

import (
	"slices"
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
	assert.Equal(t, pp.SampleTypeGpu, samples[0].SampleType,
		"GPU samples must use SampleTypeGpu (period 1), not SampleTypeCpu - see review Critical 4")
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

// TestProjectionRendersMicrosecondKernelInNanoseconds is the regression test
// for review Critical 4: executionWeight already returned nanoseconds, but
// every sample - including this one - went out as SampleTypeCpu, which
// pprof/pprof.go's BuilderForSample multiplies by Profile.Period (~10.1ms at
// 99Hz). A 70us kernel rendered as 707.1 seconds - off by roughly 10
// million. This asserts the gpu package's half of the fix (SampleTypeGpu
// instead of SampleTypeCpu on the ProfileSample); pprof's
// TestGpuValueNotScaledByCpuPeriod asserts the other half (period 1, no
// rescaling once it reaches the builder).
func TestProjectionRendersMicrosecondKernelInNanoseconds(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 0)))
	require.NoError(t, tl.EmitExec(execFor("a", 1_000, 71_000))) // a 70us kernel

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, uint64(70_000), samples[0].Value, "a 70us kernel must project as ~70000ns")
	assert.Equal(t, pp.SampleTypeGpu, samples[0].SampleType)
}

// TestProjectionDistributesExecutionWeightAcrossPCSamples is the second
// Critical 4 regression test: pcSampleWeight used to return each PC sample's
// own Count, mixing "a count of samples" and "a duration in nanoseconds" in
// the same value dimension, and made a kernel's total attributed time equal
// its sample count rather than its actual duration. The fix distributes the
// execution's real duration across its PC samples in proportion to Count,
// so the parts sum to exactly the whole (remainder to the last sample).
// Three samples with counts 1:2:1 (total 4) against a 100ns execution:
// expected shares are 25, 50, 25 - evenly divisible, so this alone wouldn't
// catch the remainder-handling; TestProjectionDistributionHandlesRemainder
// below covers that separately. Mutation this catches: reverting to
// per-sample Count as the weight (would yield 1, 2, 1 - nowhere near 100),
// or distributing without proportion to Count (e.g. splitting evenly
// regardless of Count, which coincidentally would also look like 33/33/33
// here - the differing counts below rule that out).
func TestProjectionDistributesExecutionWeightAcrossPCSamples(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 0)))
	require.NoError(t, tl.EmitExec(execFor("a", 0, 100)))
	corr := CorrelationID{Backend: BackendCUPTI, Value: "a"}
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: 1, PCOffset: 0x1, Count: 1}))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: 2, PCOffset: 0x2, Count: 2}))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: 3, PCOffset: 0x3, Count: 1}))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 3)

	var total uint64
	for _, s := range samples {
		total += s.Value
	}
	assert.Equal(t, uint64(100), total, "the PC samples' weights must sum to the execution's actual duration")
	assert.Equal(t, []uint64{25, 50, 25}, []uint64{samples[0].Value, samples[1].Value, samples[2].Value},
		"weights must be proportional to each sample's Count (1:2:1 of a 100ns duration)")
}

// TestProjectionDistributionHandlesRemainder targets the explicit rounding
// requirement: integer division of a duration that doesn't divide evenly
// across sample counts must not silently drop the remainder - it goes to
// the last sample, so the parts still sum to the whole exactly. 100ns split
// three ways evenly (counts 1:1:1) gives 33+33+33=99, one short; the
// remainder (1) must land on the last sample. Mutation this catches:
// dropping the "give the remainder to the last sample" step, which would
// leave the sum at 99 instead of 100.
func TestProjectionDistributionHandlesRemainder(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 0)))
	require.NoError(t, tl.EmitExec(execFor("a", 0, 100)))
	corr := CorrelationID{Backend: BackendCUPTI, Value: "a"}
	for i := 0; i < 3; i++ {
		require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: uint64(i + 1), PCOffset: uint64(i), Count: 1}))
	}

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 3)

	var total uint64
	for _, s := range samples {
		total += s.Value
	}
	assert.Equal(t, uint64(100), total, "the remainder must be accounted for, not dropped")
	assert.Equal(t, uint64(34), samples[2].Value, "the remainder must land on the last sample (33+33+34=100)")
}

// TestProjectionEmitsGpuJoinExact is part of the regression test pair for
// review Critical 1: ExecutionView carried Join/Heuristic/Ambiguous, but
// grep for those identifiers in projection.go returned nothing - the join
// marking was computed and then discarded on the way to a pprof sample, so
// a heuristic (guessed) join was indistinguishable from an exact
// (vendor-provided) one in the output, including which container's
// pod_uid/container_id Tags rode along. Mutation this catches: the
// gpu_join assignment being removed, or an exact join emitting anything
// other than "exact".
func TestProjectionEmitsGpuJoinExact(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, "exact", samples[0].Labels["gpu_join"])
	_, hasAmbiguous := samples[0].Labels["gpu_ambiguous"]
	assert.False(t, hasAmbiguous, "an exact join must not carry gpu_ambiguous")
}

// TestProjectionEmitsGpuJoinHeuristicAndAmbiguous is the second half of the
// Critical 1 regression pair: a real heuristic join (exec Correlation left
// at its zero value - see review Critical 2 - with two candidate launches
// so Ambiguous is also genuinely exercised) must carry both gpu_join and
// gpu_ambiguous through to the projected sample. Before the fix, a
// heuristic join's launch (and therefore its Tags - pod_uid/container_id)
// projected identically to an exact join's, silently billing GPU time to a
// container chosen by a guess. Mutation this catches: gpu_join reading
// "exact" (or being absent) for a heuristic join, or gpu_ambiguous not
// being set when view.Ambiguous is true.
func TestProjectionEmitsGpuJoinHeuristicAndAmbiguous(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "older"},
		KernelName:  "k_x",
		TimeNs:      10,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 10, Tags: map[string]string{"pod_uid": "guessed-pod"}},
	}))
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "newer"},
		KernelName:  "k_x",
		TimeNs:      15,
		Launch:      LaunchContext{PID: 2, TID: 2, TimeNs: 15},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		// Correlation deliberately zero-valued so the heuristic runs at all.
		KernelName: "k_x",
		StartNs:    20, EndNs: 30,
	}))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, "heuristic", samples[0].Labels["gpu_join"])
	assert.Equal(t, "true", samples[0].Labels["gpu_ambiguous"])
}

func TestProjectionHandlesUnmatchedExecution(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(execFor("ghost", 20, 30)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, []string{"[gpu:launch unsampled]", "[gpu:kernel:k_ghost]"}, frameNames(samples[0].Stack),
		"an execution with no launch still projects, without a fabricated CPU stack - and under the "+
			"unsampled marker, because no CPU call path is being claimed for its time")
	assert.Equal(t, uint32(0), samples[0].Pid,
		"no launch means no pid to read - pinned at 0 rather than fabricated")
	assert.Equal(t, pp.SampleTypeGpu, samples[0].SampleType)
	assert.Equal(t, pp.SampleAggregated, samples[0].Aggregation)
	assert.Equal(t, "unmatched", samples[0].Labels["gpu_join"],
		"an unmatched execution's gpu_join must read 'unmatched', never absent (which could be misread as exact)")
}

// TestProjectionDistributionSurvivesOverflow pins proportionality for an
// execution interval large enough that execWeight*Count exceeds uint64.
// Nothing validates EndNs-StartNs against a malformed producer, and a wrapped
// product is silently destructive rather than loud: every share computes as 0
// and the residue guard hands the entire duration to the last sample, so the
// weights still sum correctly while the distribution is completely wrong.
//
// Mutation caught: replacing bits.Mul64/Div64 with `execWeight * c / totalCount`
// makes the two equal-count samples receive 0 and execWeight respectively.
func TestProjectionDistributionSurvivesOverflow(t *testing.T) {
	execWeight := uint64(1) << 62
	pcs := []GPUPCSample{{Count: 8}, {Count: 8}}

	weights := distributeExecutionWeight(execWeight, pcs)

	require.Len(t, weights, 2)
	assert.Equal(t, weights[0], weights[1],
		"equal sample counts must receive equal shares; unequal means the product wrapped")

	var sum uint64
	for _, w := range weights {
		sum += w
	}
	assert.Equal(t, execWeight, sum, "distributed weights must sum to the execution duration")
}

// --- Phase 4a: the two populations stay separable and stay exact ----------

// TestUnsampledExecutionsGetTheirOwnNodeNotAFabricatedStack is the honesty
// rule of the whole sampled-stack design, as a test.
//
// Stack capture is sampled; execution measurement is not. So a profile
// contains two populations - executions whose launch carried a sampled CPU
// stack, and executions whose launch did not - and the rules are:
// neither population's value is ever multiplied by the sample period (that
// would turn a measured duration into an estimate and print it as fact),
// and the unsampled population never borrows the sampled one's call path.
// Their values sum to exactly the GPU time that was measured.
//
// Mutations this catches: scaling the sampled sample's value by
// SamplePeriod (attributed would read 800, and the total 1000, neither of
// which was ever measured); projecting the stackless execution under
// [gpu:launch] with the sibling's frames (the NotContains fails); dropping
// the gpu_sample_period label, which is what lets a consumer compute the
// extrapolation deliberately rather than being handed one silently.
func TestUnsampledExecutionsGetTheirOwnNodeNotAFabricatedStack(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 100, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{KernelName: "kAdd", Launch: LaunchContext{
				CPUStack: []pp.Frame{{Name: "main"}, {Name: "run"}}, SamplePeriod: 8}}},
		{Exec: GPUKernelExec{StartNs: 100, EndNs: 300, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{KernelName: "kAdd"}}, // joined, no stack
	}}

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 2)

	var attributed, unattributed uint64
	for _, s := range samples {
		names := frameNames(s.Stack)
		if slices.Contains(names, FrameLaunchUnsampled) {
			unattributed += s.Value
			assert.NotContains(t, names, "main", "an unsampled launch must not borrow a stack")
		} else {
			attributed += s.Value
			assert.Contains(t, names, "main")
			assert.Equal(t, "8", s.Labels["gpu_sample_period"])
		}
	}
	assert.Equal(t, uint64(100), attributed, "durations are never scaled")
	assert.Equal(t, uint64(200), unattributed)
	assert.Equal(t, uint64(300), attributed+unattributed, "the GPU total stays exact")
}

// A sampled launch's frames nest under the real call path, in the same
// root-first order the pre-sampling projection used: stack, boundary,
// kernel. The unsampled sibling gets exactly two frames, and the marker
// differs so a flame graph separates the two populations by shape - not by
// a label a viewer may not render.
func TestSampledAndUnsampledPopulationsHaveDifferentShapes(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 10, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{Launch: LaunchContext{
				CPUStack: pp.FramesFromNames([]string{"main", "train_step"}), SamplePeriod: 4}}},
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 10, KernelName: "kAdd"}},
	}}

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 2)
	assert.Equal(t, []string{"main", "train_step", FrameLaunch, "[gpu:kernel:kAdd]"},
		frameNames(samples[0].Stack))
	assert.Equal(t, []string{FrameLaunchUnsampled, "[gpu:kernel:kAdd]"},
		frameNames(samples[1].Stack))
}

// The sample period is only meaningful next to a stack. A launch the
// consumer marked sampled but whose capture or symbolization failed carries
// a period and no stack; it is unattributed like any other stackless
// execution, and must not advertise a period as if a call path had been
// claimed - a consumer multiplying by it would scale nothing at all.
func TestSamplePeriodLabelOnlyRidesWithARealStack(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 10, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{Launch: LaunchContext{SamplePeriod: 16}}},
	}}

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 1)
	assert.Equal(t, []string{FrameLaunchUnsampled, "[gpu:kernel:kAdd]"}, frameNames(samples[0].Stack))
	assert.NotContains(t, samples[0].Labels, "gpu_sample_period",
		"a period with no stack attributes nothing; advertising it invites a scale of zero data")
}

// A producer-supplied tag must not be able to forge the sample period, the
// same rule every other gpu_* label follows.
func TestSamplePeriodLabelBeatsAForgedTag(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 10, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{Launch: LaunchContext{
				CPUStack:     pp.FramesFromNames([]string{"main"}),
				SamplePeriod: 32,
				Tags:         map[string]string{"gpu_sample_period": "1"},
			}}},
	}}

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 1)
	assert.Equal(t, "32", samples[0].Labels["gpu_sample_period"],
		"a tag named gpu_sample_period must not override the real period")
}

// PC samples split an execution's duration; they must not change which
// population it belongs to, and the split parts must still sum to the
// execution's own unscaled duration.
func TestSampledExecutionWithPCSamplesStaysExactAndAttributed(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 100, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{Launch: LaunchContext{
				CPUStack: pp.FramesFromNames([]string{"main"}), SamplePeriod: 8}},
			PCSamples: []GPUPCSample{{PCOffset: 0x10, Count: 1}, {PCOffset: 0x20, Count: 3}}},
	}}

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 2)
	var total uint64
	for _, s := range samples {
		total += s.Value
		assert.Contains(t, frameNames(s.Stack), "main")
		assert.Equal(t, "8", s.Labels["gpu_sample_period"])
	}
	assert.Equal(t, uint64(100), total, "the split parts still sum to the measured duration, unscaled")
}
