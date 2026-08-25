package gpu

import (
	"fmt"
	"slices"
	"strings"
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
	//
	// The gpu_src_* family is the sharpest case: those labels name a file and
	// a line in the profiled program's own source, which is the single most
	// believable thing a profile can say. A tag that could set them would let
	// a producer point every stalled instruction at a source line of its
	// choosing. See TestProjectionSourceLabelsCannotBeForgedByAbsence for the
	// other direction - the cases where this package derives no value at all.
	st, idx := projStore(t)
	tl := NewTimeline(TimelineConfig{Modules: st})
	l := launch("a", 10)
	l.Launch.Tags = map[string]string{
		"gpu_queue":       "HIJACKED",
		"gpu_device":      "HIJACKED",
		"gpu_correlation": "HIJACKED",
		"gpu_stall":       "HIJACKED",
		"gpu_pc":          "HIJACKED",
		"gpu_pid":         "HIJACKED",
		"gpu_pc_attrib":   "HIJACKED",
		"gpu_src_status":  "HIJACKED",
		"gpu_src_file":    "HIJACKED",
		"gpu_src_line":    "HIJACKED",
		"gpu_src_func":    "HIJACKED",
	}
	require.NoError(t, tl.EmitLaunch(l))

	exec := execFor("a", 20, 30)
	exec.Queue = GPUQueueRef{Backend: BackendCUPTI, QueueID: "q1", Device: GPUDeviceRef{DeviceID: "dev1"}}
	require.NoError(t, tl.EmitExec(exec))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		Module:      ModuleRef{Backend: BackendCUPTI, CRC: projCRC}, FunctionIndex: idx,
		PCOffset: 0x10, StallReason: "long_scoreboard", Count: 1, TimeNs: 25,
	}))

	samples, _ := ProjectExecutionsWith(tl.Snapshot(), ProjectionConfig{Modules: st})
	require.Len(t, samples, 1)

	assert.Equal(t, "q1", samples[0].Labels["gpu_queue"], "a tag named gpu_queue must not override the real queue")
	assert.Equal(t, "dev1", samples[0].Labels["gpu_device"], "a tag named gpu_device must not override the real device")
	assert.Equal(t, "cupti:a", samples[0].Labels["gpu_correlation"], "a tag named gpu_correlation must not override the real correlation")
	assert.Equal(t, "long_scoreboard", samples[0].Labels["gpu_stall"], "a tag named gpu_stall must not override the real stall reason")
	assert.Equal(t, "0x10", samples[0].Labels["gpu_pc"], "a tag named gpu_pc must not override the real pc")
	assert.Equal(t, "1", samples[0].Labels["gpu_pid"], "a tag named gpu_pid must not override the real process")
	assert.Equal(t, "exact", samples[0].Labels["gpu_pc_attrib"], "a tag named gpu_pc_attrib must not override how the sample was joined")
	assert.Equal(t, "resolved", samples[0].Labels["gpu_src_status"], "a tag named gpu_src_status must not override what the module store decided")
	assert.Equal(t, "single.cu", samples[0].Labels["gpu_src_file"], "a tag named gpu_src_file must not override the resolved source file")
	assert.Equal(t, "6", samples[0].Labels["gpu_src_line"], "a tag named gpu_src_line must not override the resolved line")
	assert.Equal(t, "addOne", samples[0].Labels["gpu_src_func"], "a tag named gpu_src_func must not override the resolved function")
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
		// Same process as "older": issue #52 confines a heuristic join to one
		// process, so two candidates from two processes are no longer
		// ambiguous - they are in different candidate groups. The ambiguity
		// this test is about is two launches from the SAME process, which is
		// the only ambiguity the heuristic can still produce.
		Launch: LaunchContext{PID: 1, TID: 1, TimeNs: 15},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		// No correlation value, so the heuristic runs at all; pid 1 so it is
		// allowed to run against pid 1's launches (issue #52).
		Correlation: noCorrelation(1),
		KernelName:  "k_x",
		StartNs:     20, EndNs: 30,
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

// --- Phase 6: the PC-sample label set -------------------------------------

// projCRC is the CRC the projection tests store their cubin under. Its value
// is arbitrary; what matters is that the sample and the store agree, exactly
// as cubin_crc makes them agree on the wire.
const projCRC = 0xABCDEF

// projStore holds one real -lineinfo cubin and returns the symbol index its
// kernel occupies. The index is read out of the fixture rather than
// hard-coded, for the reason modulestore_test.go's symIndexOf gives: whether
// CUPTI's functionIndex IS the .symtab index is measured on hardware, and
// these tests assert only that the projection reports whatever the store
// resolved.
func projStore(t *testing.T) (*ModuleStore, uint32) {
	t.Helper()
	b := fixture(t, "single_lineinfo.cubin")
	st := NewModuleStore(ModuleStoreConfig{Capacity: 8})
	require.NoError(t, st.Put(projCRC, b))
	return st, symIndexOf(t, b, "addOne")
}

// pcSampleAt is one PC sample against a module, with a stall reason so that
// every test below also carries the label the cap must never touch.
func pcSampleAt(crc uint64, fnIndex uint32, pcOffset uint64) GPUPCSample {
	return GPUPCSample{
		Module:        ModuleRef{Backend: BackendCUPTI, CRC: crc},
		FunctionIndex: fnIndex,
		PCOffset:      pcOffset,
		StallReason:   "long_scoreboard",
		Count:         1,
	}
}

// pcView is an execution carrying PC samples, with the attribution the join
// would have decided for it.
func pcView(attrib PCAttrib, pcs ...GPUPCSample) ExecutionView {
	return ExecutionView{
		Exec:      GPUKernelExec{StartNs: 0, EndNs: 100, KernelName: "addOne"},
		PCSamples: pcs,
		PCAttrib:  attrib,
	}
}

// TestProjectionEmitsAllFourSrcStatuses is the core table: every value of
// gpu_src_status reaches a projected sample, from a real fixture through the
// real store, and the source labels ride only under "resolved".
//
// The unconditional half is the load-bearing one. An ABSENT source label reads
// as "this sample was never source-mapped"; an explicit status reads as
// "sampled, and here is precisely why there is no location" - a build flag, a
// missing cubin, or an instruction the compiler emitted no line for. Those are
// three different actions for the reader, and a status that could go missing
// would collapse them into one shrug.
//
// Mutations this catches: making gpu_src_status conditional on anything;
// emitting gpu_src_file/_line/_func under a status other than resolved;
// renaming any of the four wire spellings.
func TestProjectionEmitsAllFourSrcStatuses(t *testing.T) {
	withInfo := fixture(t, "single_lineinfo.cubin")
	noInfo := fixture(t, "single_nolineinfo.cubin")

	const (
		crcWithInfo = 0x1111
		crcNoInfo   = 0x2222
		crcAbsent   = 0x3333
	)
	st := NewModuleStore(ModuleStoreConfig{Capacity: 8})
	require.NoError(t, st.Put(crcWithInfo, withInfo))
	require.NoError(t, st.Put(crcNoInfo, noInfo))
	idx := symIndexOf(t, withInfo, "addOne")
	noInfoIdx := symIndexOf(t, noInfo, "addOne")

	cases := []struct {
		name   string
		sample GPUPCSample
		want   SrcStatus
	}{
		{"line table covers this pc", pcSampleAt(crcWithInfo, idx, 0x10), SrcResolved},
		{"module built without -lineinfo", pcSampleAt(crcNoInfo, noInfoIdx, 0x10), SrcNoLineInfo},
		{"cubin never reached the agent", pcSampleAt(crcAbsent, idx, 0x10), SrcNoModule},
		{"pc past the end of the function", pcSampleAt(crcWithInfo, idx, 0x180), SrcUnmapped},
	}

	views := make([]ExecutionView, 0, len(cases))
	for _, tc := range cases {
		views = append(views, pcView(PCAttribKernel, tc.sample))
	}
	samples, _ := ProjectExecutionsWith(Snapshot{Executions: views}, ProjectionConfig{Modules: st})
	require.Len(t, samples, len(cases))

	seen := make(map[SrcStatus]bool)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := samples[i].Labels
			require.Contains(t, labels, "gpu_src_status",
				"gpu_src_status is unconditional: an absent label reads as 'not sampled', which is a different fact")
			assert.Equal(t, tc.want.String(), labels["gpu_src_status"])

			if tc.want != SrcResolved {
				assert.NotContains(t, labels, "gpu_src_file",
					"a location may only ride under a resolved status")
				assert.NotContains(t, labels, "gpu_src_line")
				assert.NotContains(t, labels, "gpu_src_func")
				return
			}
			assert.Equal(t, "single.cu", labels["gpu_src_file"])
			assert.Equal(t, "6", labels["gpu_src_line"])
			assert.Equal(t, "addOne", labels["gpu_src_func"])
		})
		seen[tc.want] = true
	}

	for _, s := range SrcStatuses() {
		assert.True(t, seen[s], "gpu_src_status %s is not reachable from this table", s)
	}
}

// TestProjectionWithoutAModuleStoreStillAnswersEveryPCSample pins the nil
// case. With no store nothing can be resolved, and the honest answer is
// "no-module" on every sample - the same fact as a cubin that never arrived.
//
// The failure this prevents is the quiet one: if the labels simply vanished
// when no store was configured, a profile taken with the store unwired would
// be byte-identical to one taken before this phase existed, and nothing would
// point at the missing store. "no-module" on every sample points straight at
// it.
func TestProjectionWithoutAModuleStoreStillAnswersEveryPCSample(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		pcView(PCAttribKernel, pcSampleAt(projCRC, 7, 0x10), pcSampleAt(projCRC, 7, 0x20)),
	}}

	samples, _ := ProjectExecutionsWith(snap, ProjectionConfig{})
	require.Len(t, samples, 2)
	for _, s := range samples {
		assert.Equal(t, "no-module", s.Labels["gpu_src_status"],
			"no store means no usable module bytes, which is exactly what no-module says")
		assert.NotContains(t, s.Labels, "gpu_src_file")
	}

	// And the plain entry point behaves identically - it is the same call.
	plain := ProjectExecutions(snap)
	require.Len(t, plain, 2)
	assert.Equal(t, "no-module", plain[0].Labels["gpu_src_status"])
}

// TestProjectionSrcFileIsABasenameNotABuildHostPath pins the directory
// decision. The fixture's line table carries an absolute build-host path
// (/tmp/perf-agent-cubin-fixtures/single.cu), so this test fails the moment
// the projection passes the file through unchanged.
//
// Three reasons the directory goes nowhere, all of which this asserts the
// consequence of: the path varies per build, so the same kernel built twice
// would produce two label values for one file; it is a long string in the
// pprof string table that no reader acts on; and it leaks the build
// environment's layout into a profile that may be shared. No OTHER label may
// smuggle it back in either, which is what the second loop checks.
func TestProjectionSrcFileIsABasenameNotABuildHostPath(t *testing.T) {
	st, idx := projStore(t)

	samples, _ := ProjectExecutionsWith(
		Snapshot{Executions: []ExecutionView{pcView(PCAttribKernel, pcSampleAt(projCRC, idx, 0x10))}},
		ProjectionConfig{Modules: st})
	require.Len(t, samples, 1)

	assert.Equal(t, "single.cu", samples[0].Labels["gpu_src_file"])
	for k, v := range samples[0].Labels {
		assert.NotContains(t, v, "/", "no label may carry a directory: %s=%q", k, v)
		assert.NotContains(t, v, "perf-agent-cubin-fixtures",
			"no label may leak the build host's layout: %s=%q", k, v)
	}
}

// TestSrcFileBaseRejectsWhatIsNotAName covers srcFileBase directly, including
// the inputs a line table should never produce but which must not turn into a
// label value of "." or "/" if it ever does.
func TestSrcFileBaseRejectsWhatIsNotAName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/home/build/src/kernels/matmul.cu", "matmul.cu"},
		{"kernels/matmul.cu", "matmul.cu"},
		{"matmul.cu", "matmul.cu"},
		{"", ""},
		{".", ""},
		{"/", ""},
	} {
		assert.Equal(t, tc.want, srcFileBase(tc.in), "srcFileBase(%q)", tc.in)
	}
}

// TestProjectionEmitsAllFourPCAttribValues walks the enum itself rather than a
// hand-copied list, so a fifth value added to PCAttribs() without a projection
// case fails here rather than shipping as a label nobody rendered.
func TestProjectionEmitsAllFourPCAttribValues(t *testing.T) {
	attribs := PCAttribs()
	require.Len(t, attribs, 4)

	views := make([]ExecutionView, 0, len(attribs))
	for _, a := range attribs {
		views = append(views, pcView(a, pcSampleAt(projCRC, 7, 0x10)))
	}
	samples, _ := ProjectExecutionsWith(Snapshot{Executions: views}, ProjectionConfig{})
	require.Len(t, samples, len(attribs))

	for i, a := range attribs {
		assert.Equal(t, string(a), samples[i].Labels["gpu_pc_attrib"],
			"gpu_pc_attrib must report exactly what the join decided")
	}
	assert.Equal(t, "exact", samples[0].Labels["gpu_pc_attrib"],
		"the one value that is not an inference must be spelled exactly this way")
}

// TestProjectionPCAttribIsNeverSilentlyAbsent is the honesty half of the
// label. A view carrying PC samples with no attribution is a join bug; the
// projection must make that visible rather than omit the label, because an
// absent gpu_pc_attrib is readable as "exact" - the only one of the four that
// claims vendor-provided truth - by a consumer who does not know to check.
//
// Mutation this catches: `if view.PCAttrib != "" { labels[...] = ... }`.
func TestProjectionPCAttribIsNeverSilentlyAbsent(t *testing.T) {
	samples, _ := ProjectExecutionsWith(
		Snapshot{Executions: []ExecutionView{pcView("", pcSampleAt(projCRC, 7, 0x10))}},
		ProjectionConfig{})
	require.Len(t, samples, 1)

	got, ok := samples[0].Labels["gpu_pc_attrib"]
	require.True(t, ok, "a PC sample with no attribution must still carry the label")
	for _, a := range PCAttribs() {
		assert.NotEqual(t, string(a), got,
			"a join bug must not render as one of the four real values, least of all %s", PCAttribExact)
	}
}

// TestProjectionKernelAmbiguousNeverCoincidesWithGpuAmbiguous is the
// de-overloading assertion, driven end to end through the real join rather
// than a hand-built view.
//
// Two executions of one kernel are in the horizon, so which invocation the
// samples came from is an inference. It is marked in gpu_pc_attrib, and
// gpu_ambiguous - which means "the heuristic LAUNCH join chose between
// candidate launches" and feeds AmbiguousHeuristicMatchCount - stays absent.
// Emitting both on one sample would put two unrelated facts on one flag.
func TestProjectionKernelAmbiguousNeverCoincidesWithGpuAmbiguous(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	f.sample(t, pid, 11)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 40, 50)))

	snap := f.tl.Snapshot()
	samples, _ := ProjectExecutionsWith(snap, ProjectionConfig{Modules: f.store})

	var ambiguous int
	for _, s := range samples {
		if s.Labels["gpu_pc_attrib"] != string(PCAttribKernelAmbiguous) {
			continue
		}
		ambiguous++
		assert.NotContains(t, s.Labels, "gpu_ambiguous",
			"PC ambiguity and heuristic-launch ambiguity are different joins with different failure "+
				"modes; one flag cannot carry both")
	}
	assert.Equal(t, 2, ambiguous, "both samples of the ambiguous group must carry the mark")
	assert.Zero(t, snap.JoinStats.AmbiguousHeuristicMatchCount,
		"no launch was joined heuristically here, so that counter must not have moved")
}

// TestProjectionCapSuppressesGpuPCAndOnlyGpuPC is the cardinality budget.
//
// Five distinct instruction offsets against a ceiling of two: the first two
// distinct values are emitted, the remaining three samples go out without an
// offset, and ProjectionPCLabelsSuppressed reads exactly three. Every one of
// the five keeps its stall reason, its attribution, its source status and its
// full share of the execution's duration - the label that gives way under
// pressure is the numerous one, not the actionable ones.
//
// Mutations this catches: capping the wrong label; dropping the sample instead
// of the label (the weights would no longer sum to the duration); counting
// distinct suppressed offsets rather than suppressed samples.
func TestProjectionCapSuppressesGpuPCAndOnlyGpuPC(t *testing.T) {
	st, idx := projStore(t)
	pcs := []GPUPCSample{
		pcSampleAt(projCRC, idx, 0x10),
		pcSampleAt(projCRC, idx, 0x20),
		pcSampleAt(projCRC, idx, 0x30),
		pcSampleAt(projCRC, idx, 0x40),
		pcSampleAt(projCRC, idx, 0x50),
	}
	snap := Snapshot{Executions: []ExecutionView{pcView(PCAttribKernel, pcs...)}}

	samples, stats := ProjectExecutionsWith(snap, ProjectionConfig{Modules: st, MaxDistinctPCLabels: 2})
	require.Len(t, samples, 5)

	assert.Equal(t, uint64(3), stats.PCLabelsSuppressed,
		"three samples past a ceiling of two distinct offsets")
	assert.Equal(t, uint64(2), stats.DistinctPCLabels)
	assert.Equal(t, uint64(2), stats.PCLabelCap)

	var total uint64
	for i, s := range samples {
		total += s.Value
		if i < 2 {
			assert.Equal(t, fmt.Sprintf("%#x", pcs[i].PCOffset), s.Labels["gpu_pc"],
				"the first distinct offsets inside the budget are emitted")
		} else {
			assert.NotContains(t, s.Labels, "gpu_pc", "sample %d is past the ceiling", i)
		}
		assert.Equal(t, "long_scoreboard", s.Labels["gpu_stall"], "the cap must not touch gpu_stall")
		assert.Equal(t, "kernel", s.Labels["gpu_pc_attrib"], "the cap must not touch gpu_pc_attrib")
		assert.Equal(t, "resolved", s.Labels["gpu_src_status"], "the cap must not touch gpu_src_status")
		assert.Equal(t, "single.cu", s.Labels["gpu_src_file"], "the cap must not touch the source location")
		assert.Equal(t, "addOne", s.Labels["gpu_src_func"])
		assert.Contains(t, s.Labels, "gpu_src_line")
	}
	assert.Equal(t, uint64(100), total,
		"a suppressed label must not cost the sample its weight; the parts still sum to the duration")
}

// TestProjectionCapReadmitsAnOffsetItAlreadyEmitted pins the rule that the cap
// bounds DISTINCT values, not samples. A repeat of an offset already in the
// profile costs the pprof string table nothing, so refusing it would drop
// information already paid for while leaving the bound exactly where it was.
func TestProjectionCapReadmitsAnOffsetItAlreadyEmitted(t *testing.T) {
	offsets := []uint64{0x10, 0x20, 0x10, 0x30}
	pcs := make([]GPUPCSample, 0, len(offsets))
	for _, off := range offsets {
		pcs = append(pcs, pcSampleAt(projCRC, 7, off))
	}

	samples, stats := ProjectExecutionsWith(
		Snapshot{Executions: []ExecutionView{pcView(PCAttribKernel, pcs...)}},
		ProjectionConfig{MaxDistinctPCLabels: 2})
	require.Len(t, samples, 4)

	assert.Equal(t, "0x10", samples[2].Labels["gpu_pc"],
		"an offset already in the string table costs nothing to repeat")
	assert.NotContains(t, samples[3].Labels, "gpu_pc", "a third DISTINCT value is what the cap refuses")
	assert.Equal(t, uint64(1), stats.PCLabelsSuppressed)
	assert.Equal(t, uint64(2), stats.DistinctPCLabels)
}

// TestProjectionCapIsSurfacedInJoinHealth closes the loop the design demands:
// a profile that silently lost its PC labels looks identical to one that never
// had any, so the suppression has to be readable somewhere outside the
// profile.
func TestProjectionCapIsSurfacedInJoinHealth(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		pcView(PCAttribKernel, pcSampleAt(projCRC, 7, 0x10), pcSampleAt(projCRC, 7, 0x20)),
	}}
	_, stats := ProjectExecutionsWith(snap, ProjectionConfig{MaxDistinctPCLabels: 1})
	require.Equal(t, uint64(1), stats.PCLabelsSuppressed)

	withProj := strings.Join(JoinHealthWith(snap, stats), "\n")
	assert.Contains(t, withProj, "without gpu_pc",
		"the operator must be told the labels were dropped; nothing in the profile says so")
	assert.Contains(t, withProj, "anomal")

	assert.NotContains(t, strings.Join(JoinHealth(snap), "\n"), "without gpu_pc",
		"a caller that suppressed nothing must not be told it did")
}

// TestProjectionAddsNoFrames is the negative assertion the design requires:
// this phase adds labels and NOTHING to stack identity. At PC-sampling rates a
// frame per instruction or per source line destroys aggregation and fragments
// the kernel's own block, so frames stay exhaustively
// <CPU stack> -> [gpu:launch] -> [gpu:kernel:<name>].
//
// Mutation this catches: promoting the PC, the stall reason or the source line
// to a frame, in any spelling.
func TestProjectionAddsNoFrames(t *testing.T) {
	st, idx := projStore(t)
	view := pcView(PCAttribKernelAmbiguous, pcSampleAt(projCRC, idx, 0x10), pcSampleAt(projCRC, idx, 0x40))
	view.Launch = &GPUKernelLaunch{Launch: LaunchContext{CPUStack: pp.FramesFromNames([]string{"main"})}}

	samples, _ := ProjectExecutionsWith(Snapshot{Executions: []ExecutionView{view}},
		ProjectionConfig{Modules: st})
	require.Len(t, samples, 2)

	// The kernel name is deliberately NOT on this list: [gpu:kernel:<name>] is
	// one of the three frames the design fixes, and it is the deepest one. The
	// forbidden strings are the per-sample detail this phase adds - the offset,
	// the stall reason, the source location and the attribution quality -
	// every one of which would fragment that kernel's own block if promoted.
	forbidden := []string{
		"gpu:pc", "gpu:src", "long_scoreboard", "single.cu",
		"kernel-ambiguous", "resolved", "0x10", "0x40",
	}
	for _, s := range samples {
		names := frameNames(s.Stack)
		assert.Equal(t, []string{"main", FrameLaunch, "[gpu:kernel:addOne]"}, names)
		for _, name := range names {
			for _, bad := range forbidden {
				assert.NotContains(t, name, bad, "frame %q must not carry per-sample detail", name)
			}
		}
	}
	assert.Equal(t, frameNames(samples[0].Stack), frameNames(samples[1].Stack),
		"two PC samples from one kernel must share one stack and differ only by label")
}

// TestProjectionSourceLabelsCannotBeForgedByAbsence is the second half of the
// reserved-name discipline, and the half the conditional labels made
// necessary.
//
// Overwriting alone is not enough: gpu_src_file is emitted ONLY under a
// resolved status, so a producer tag of that name would survive untouched in
// exactly the cases where this package has no value of its own - a forged
// source location standing beside gpu_src_status="no-module", which is worse
// than any value it could overwrite. The same goes for gpu_pc past the
// cardinality cap, for gpu_stall when the producer named no reason, and for
// every one of these names on an execution that carries no PC samples at all.
// Reserved names win by absence too.
func TestProjectionSourceLabelsCannotBeForgedByAbsence(t *testing.T) {
	forged := map[string]string{
		"gpu_src_status": "resolved",
		"gpu_src_file":   "attacker.cu",
		"gpu_src_line":   "1",
		"gpu_src_func":   "attacker_kernel",
		"gpu_pc":         "0xdeadbeef",
		"gpu_stall":      "not_stalled",
		"gpu_pc_attrib":  "exact",
		"pod_uid":        "pod-a",
	}
	tagged := func(pcs ...GPUPCSample) ExecutionView {
		v := pcView(PCAttribKernel, pcs...)
		v.Launch = &GPUKernelLaunch{Launch: LaunchContext{Tags: forged}}
		return v
	}

	// No module for this CRC, so no location is derived; one PC sample past a
	// ceiling of zero distinct offsets... the ceiling cannot be zero (0 means
	// "default"), so the second sample is the one the cap refuses.
	unresolved := pcSampleAt(0xDEAD, 7, 0x10)
	unresolved.StallReason = "" // the producer named none
	second := pcSampleAt(0xDEAD, 7, 0x20)
	second.StallReason = ""

	snap := Snapshot{Executions: []ExecutionView{
		tagged(unresolved, second),
		func() ExecutionView { // an execution with no PC samples at all
			v := ExecutionView{Exec: GPUKernelExec{StartNs: 0, EndNs: 10, KernelName: "addOne"}}
			v.Launch = &GPUKernelLaunch{Launch: LaunchContext{Tags: forged}}
			return v
		}(),
	}}

	samples, stats := ProjectExecutionsWith(snap, ProjectionConfig{MaxDistinctPCLabels: 1})
	require.Len(t, samples, 3)
	require.Equal(t, uint64(1), stats.PCLabelsSuppressed)

	for i, s := range samples {
		assert.Equal(t, "pod-a", s.Labels["pod_uid"], "ordinary tags still ride, sample %d", i)
		assert.NotContains(t, s.Labels, "gpu_src_file",
			"a tag named gpu_src_file must not survive where no location was resolved (sample %d)", i)
		assert.NotContains(t, s.Labels, "gpu_src_line")
		assert.NotContains(t, s.Labels, "gpu_src_func")
		assert.NotContains(t, s.Labels, "gpu_stall",
			"a tag named gpu_stall must not survive where the producer named no reason (sample %d)", i)
	}
	assert.Equal(t, "no-module", samples[0].Labels["gpu_src_status"],
		"the derived status must overwrite a tag claiming 'resolved'")
	assert.Equal(t, "0x10", samples[0].Labels["gpu_pc"])
	assert.NotContains(t, samples[1].Labels, "gpu_pc",
		"a tag named gpu_pc must not survive the cardinality cap")
	assert.NotContains(t, samples[2].Labels, "gpu_pc",
		"an execution with no PC samples has no offset to report, forged or otherwise")
	assert.NotContains(t, samples[2].Labels, "gpu_src_status",
		"gpu_src_status is unconditional on PC-DERIVED samples; an execution with none has nothing to say")
	assert.NotContains(t, samples[2].Labels, "gpu_pc_attrib")
}
