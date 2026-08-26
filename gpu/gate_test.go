package gpu

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// TestPhase6Gate is the GPU-free half of the Phase 6 phase gate, assembled in
// the package whose behaviour it is about.
//
// # Why the gate lives in two places and why this half exists at all
//
// The plan's gate (Task 13, assertions 1-12) is written as an extension of
// gpuprobe's TestStubDrivesThePipelineToPprofWithoutAGPU, which needs CAP_BPF,
// CAP_PERFMON and CAP_CHECKPOINT_RESTORE to attach the uprobes and symbolize
// the producer's stacks. On a machine without those it SKIPS - and a gate that
// skips is a gate that cannot fail. Nineteen defects on this project have been
// checks that read green when things were worst; a twelve-point gate nobody can
// run is the most expensive available instance of that.
//
// So every assertion of the twelve that is a statement about the gpu package -
// which is nine of them - is also asserted HERE, where it needs no capability
// and runs on every `go test ./gpu/`. The privileged end-to-end gate in
// gpuprobe/gate_test.go adds what only a real producer can add: a real CPU
// stack walked by BPF, a real cubin crossing a real socket, real executions.
//
// # Why this composes rather than restates
//
// Every property below is already asserted by a task's own test. Restating it
// here would give the project two assertions of one fact, which is how two
// assertions of one fact drift apart. So this calls them. What that buys, over
// leaving them scattered:
//
//   - deleting or weakening any one of them fails THE GATE, by name, with the
//     assertion number attached, rather than removing a test nobody notices;
//   - the twelve are enumerated in one place, so an assertion that has no test
//     behind it is visible as a gap rather than as an absence;
//   - the sub-test names carry the gate numbering, so a failure reads as
//     "assertion 7 failed" instead of as a test name that has to be traced back
//     to the plan.
//
// Two of the twelve are NOT compositions, because no test asserted them:
// assertion 2 (a source line reached from a CPU stack, through the join and the
// projection together) and assertion 3's aggregation clause. Both are written
// out below.
//
// Assertions 10, 10a, 11 and 12 are consumer-side and live in
// gpuprobe/gate_compose_test.go. Assertions 13-16 need an RTX 3090 and are
// stated as outstanding in .superpowers/sdd/task-13-gate-report.md.
func TestPhase6Gate(t *testing.T) {
	// 1. Frames stop at the kernel. The §8 pin, and first for a reason: at PC
	//    sampling rates one frame per PC destroys aggregation and fragments the
	//    kernel's own block, so every other assertion here is about a label.
	//    Catches: promoting the PC offset, the stall reason, the source
	//    location or the attribution quality to a frame, in any spelling.
	t.Run("assertion-01-frames-stop-at-the-kernel", TestProjectionAddsNoFrames)
	t.Run("assertion-01-pc-is-not-stack-identity", TestProjectionKeepsPCOutOfStackIdentity)

	// 2. A source line is reached from a CPU stack. The Phase 6 exit
	//    condition, satisfied through labels rather than frames. New here -
	//    see the test's own comment for what no existing test covered.
	t.Run("assertion-02-source-line-from-a-cpu-stack", TestGateASourceLineIsReachedFromACPUStack)

	// 3. No -lineinfo, no invention.
	//    Catches: synthesizing a nearest line, or a function's first line, for
	//    a module that carries no line table at all.
	t.Run("assertion-03-no-lineinfo-no-invention", TestProjectionEmitsAllFourSrcStatuses)
	t.Run("assertion-03-populations-aggregate-at-the-kernel",
		TestGateResolvedAndNoLineinfoAggregateAtTheSameKernel)

	// 4. All four gpu_src_status values reachable, each by the fixture that
	//    should produce it. Catches: a fifth value; a silent default; the two
	//    "cannot resolve" statuses collapsing into one.
	t.Run("assertion-04-four-statuses-in-the-store", TestModuleStoreAllFourStatusesAreReachable)
	t.Run("assertion-04-four-statuses-in-the-labels", TestProjectionEmitsAllFourSrcStatuses)
	t.Run("assertion-04-status-enum-is-exhaustive", TestSrcStatusesIsExhaustiveAndStable)
	t.Run("assertion-04-zero-value-is-not-a-status", TestSrcStatusZeroValueIsNotAStatus)

	// 5. Reconciliation: every emitted PC record lands in exactly one of
	//    attributed-exact, attributed-kernel, pending, evicted.
	//    Catches: a join that attaches samples without counting them; a
	//    refusal path that leaves a group pending without incrementing any
	//    counter - the easiest and most invisible mistake in that function.
	t.Run("assertion-05-reconcile-pending-and-evicted",
		TestConformancePCSampleReconciliationCoversPendingAndEvicted)
	t.Run("assertion-05-reconcile-attributed-by-kernel",
		TestConformancePCSampleReconciliationCoversAttributedByKernel)
	t.Run("assertion-05-tier-b-samples-reconcile", TestTierBSamplesReconcile)
	t.Run("assertion-05-join-accounts-for-every-group", TestTierBJoinAccountsForEveryGroup)

	// 6. Tier B does not collapse: 10,000 samples across 50 (crc,
	//    functionIndex) pairs attribute without loss to EvictedPendingSamples.
	//    Catches: the Task 8a pathology returning - a Tier B sample keyed on
	//    its (empty) correlation value, collapsing an entire process onto one
	//    pending entry and evicting the rest.
	t.Run("assertion-06-tier-b-does-not-collapse", TestTierBPCSamplesDoNotCollapseOntoOneKey)
	t.Run("assertion-06-distinct-function-indexes-split", TestTierBDistinctFunctionIndexSplitsGroups)

	// 7. Ambiguity is marked in its OWN label, and leaves gpu_ambiguous unset.
	//    Catches: reusing ExecutionView.Ambiguous for PC ambiguity, which would
	//    emit gpu_join="exact" gpu_ambiguous="true" on one sample - two
	//    unrelated facts on one boolean, and a counter that stops meaning what
	//    its name says.
	t.Run("assertion-07-ambiguity-has-its-own-label",
		TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous)
	t.Run("assertion-07-never-coincides-with-gpu-ambiguous",
		TestProjectionKernelAmbiguousNeverCoincidesWithGpuAmbiguous)

	// 8. Tier A disclosure: "true" inside a window, "false" outside, and
	//    "unknown" - never "false" - when Tier A ran and no window arrived.
	//    Catches: the one answer that must never be reachable by accident, a
	//    profile saying "not perturbed" when it means "cannot tell".
	t.Run("assertion-08-true-inside-a-burst", TestSerializationMarksExecutionsOverlappingABurst)
	t.Run("assertion-08-false-in-a-proven-gap", TestSerializationIsFalseInAProvenGap)
	t.Run("assertion-08-unknown-when-no-window-arrived", TestSerializationIsUnknownWhenNoWindowsArrived)
	t.Run("assertion-08-false-only-from-positive-evidence",
		TestSerializationFalseIsOnlyEverReachedFromPositiveEvidence)
	t.Run("assertion-08-label-is-unconditional-and-three-valued",
		TestSerializedLabelIsUnconditionalAndHasThreeValues)

	// 9. Cardinality cap: past the ceiling gpu_pc is suppressed and NOTHING
	//    else, with an exact ProjectionPCLabelsSuppressed.
	//    Catches: a cap that also drops gpu_stall or gpu_src_* (the coarser,
	//    more actionable labels), and a suppression that is silent - a profile
	//    that lost its PC labels looks identical to one that never had any.
	t.Run("assertion-09-cap-suppresses-gpu-pc-and-only-gpu-pc",
		TestProjectionCapSuppressesGpuPCAndOnlyGpuPC)
	t.Run("assertion-09-suppression-is-surfaced", TestProjectionCapIsSurfacedInJoinHealth)

	// 10b. Out-of-scope conditions refuse rather than guess. The graph clause
	//      is asserted at the level the product implements it - see the test.
	t.Run("assertion-10b-two-devices-are-marked", TestTierBMultiDeviceProcessIsMarked)
	t.Run("assertion-10b-multidevice-outranks-ambiguity", TestMultiDeviceOutranksAmbiguity)
	t.Run("assertion-10b-open-window-is-unknown-never-false",
		TestSerializationOpenWindowMakesEverythingFromItsStartUnknownAndNeverFalse)
	t.Run("assertion-10b-tier-a-refusal-names-cuda-graphs",
		TestSerializedIsRefusedWithoutAnExplicitAcknowledgement)
	// The first clause of 10b - "a graph execution makes Tier A refuse to
	// start" - is NOT implemented by the product. This pins that, and fails
	// when it becomes implementable so the gate is updated rather than left
	// claiming an assertion it never made.
	t.Run("assertion-10b-graph-refusal-is-outstanding",
		TestGateGraphExecutionRefusalIsNotAssertableYet)
}

// gateCubinCRCs are the CRCs this file stores its fixtures under. The values
// are arbitrary - what matters is that the sample and the store agree, exactly
// as cubin_crc makes them agree on the wire.
const (
	gateCRCLineInfo   uint64 = 0x6A7E0001
	gateCRCNoLineInfo uint64 = 0x6A7E0002
)

// fixtureSourceLines reads the .cu the cubin fixtures were built from, so an
// assertion about a source LINE can be checked against the source rather than
// against a number someone copied out of a run.
func fixtureSourceLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err, "fixture source %s", name)
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// TestGateASourceLineIsReachedFromACPUStack is gate assertion 2, and the Phase
// 6 exit condition: an instruction sampled inside a GPU kernel is reported at a
// named line of the CUDA source that produced it, on a sample whose frames are
// the CPU call path that launched the kernel.
//
// # Why this is not covered by anything that already exists
//
// Three tests each cover a segment and none covers the join of them:
//
//   - TestTierBSampleReachesItsExecution drives a Tier B sample to its
//     execution through the module, but the execution has no launch and
//     therefore no CPU stack, and it never projects to a pprof sample;
//   - TestProjectionEmitsAllFourSrcStatuses reaches "resolved" with real file
//     and line, but from a hand-built ExecutionView with no Launch at all;
//   - TestProjectionAddsNoFrames has a Launch with a CPU stack, but asserts the
//     NEGATIVE - that nothing about the PC reached the frames.
//
// The exit condition is the conjunction: one pprof sample carrying BOTH. It is
// asserted here through a real Timeline, so the module join and the source
// resolution run for real rather than being assumed by a literal ExecutionView.
//
// The line is checked against internal/cubin/testdata/single.cu itself - not
// against a number copied out of a previous run - so "a real line of the
// fixture's source" is a fact about the fixture rather than a constant that
// could go stale with it.
//
// Mutations this catches: a projection that resolves source labels but loses
// the launch's frames (or vice versa); a join that reaches the execution but
// drops PCSamples so nothing projects; a store keyed on something the sample
// does not carry; a resolution that reports a line the cubin's table does not
// contain.
func TestGateASourceLineIsReachedFromACPUStack(t *testing.T) {
	const pid = 4242
	b := fixture(t, "single_lineinfo.cubin")
	store := NewModuleStore(ModuleStoreConfig{})
	require.NoError(t, store.Put(gateCRCLineInfo, b))
	fnIndex := symIndexOf(t, b, "addOne")

	tl := NewTimeline(TimelineConfig{Modules: store})

	// A real CPU call path. The privileged half of the gate replaces these
	// names with frames the DWARF walker produced from a live process; here
	// what matters is that they are the LAUNCH's frames and that they survive
	// to the projected sample unchanged.
	corr := CorrelationID{Backend: BackendCUPTI, PID: pid, Value: "17"}
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: corr,
		KernelName:  "addOne",
		TimeNs:      10,
		Launch: LaunchContext{
			PID:          pid,
			TimeNs:       10,
			CPUStack:     pp.FramesFromNames([]string{"main", "run_training_step", "cudaLaunchKernel"}),
			SamplePeriod: 8,
		},
	}))

	// Tier B: no correlation value on the sample. It reaches the execution
	// through cubin_crc -> module -> function name, and nothing else.
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation:   CorrelationID{Backend: BackendCUPTI, PID: pid},
		Module:        ModuleRef{Backend: BackendCUPTI, CRC: gateCRCLineInfo},
		FunctionIndex: fnIndex,
		TimeNs:        20,
		PCOffset:      0x10,
		StallReason:   "long_scoreboard",
		Count:         1,
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: corr,
		KernelName:  "addOne",
		StartNs:     30,
		EndNs:       130,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	require.Len(t, view.PCSamples, 1,
		"the sample never reached its execution, so nothing downstream can be the exit condition")
	require.NotNil(t, view.Launch, "the execution lost the launch that carries the CPU stack")

	samples, _ := ProjectExecutionsWith(snap, ProjectionConfig{Modules: store})
	require.Len(t, samples, 1)
	s := samples[0]

	// --- the CPU stack half.
	names := frameNames(s.Stack)
	assert.Equal(t,
		[]string{"main", "run_training_step", "cudaLaunchKernel", FrameLaunch, "[gpu:kernel:addOne]"},
		names,
		"the sample's frames must be the launching CPU call path, the boundary marker and the kernel - nothing more and nothing less")

	// --- the source-line half.
	require.Equal(t, "resolved", s.Labels["gpu_src_status"],
		"the -lineinfo fixture's line table covers this pcOffset; anything else means the store never saw the module or never parsed it")
	assert.Equal(t, "single.cu", s.Labels["gpu_src_file"],
		"the basename, never the build-host path")
	assert.Equal(t, "addOne", s.Labels["gpu_src_func"])
	require.Contains(t, s.Labels, "gpu_src_line")

	// The line names a real line of the fixture's own source. Read from the
	// .cu rather than pinned as a constant: a fixture rebuilt from edited
	// source would otherwise keep this assertion green while the label pointed
	// somewhere else.
	line, err := strconv.Atoi(s.Labels["gpu_src_line"])
	require.NoError(t, err)
	src := fixtureSourceLines(t, "single.cu")
	require.Positive(t, line)
	require.LessOrEqual(t, line, len(src),
		"gpu_src_line=%d is past the end of single.cu (%d lines): the label does not name a line of the source it claims",
		line, len(src))
	body := strings.TrimSpace(src[line-1])
	assert.NotEmpty(t, body, "gpu_src_line names a blank line of single.cu")
	t.Logf("assertion 2: %s -> %s:%s  %q  (stall=%s, pc=%s, attrib=%s)",
		strings.Join(names, " -> "), s.Labels["gpu_src_file"], s.Labels["gpu_src_line"],
		body, s.Labels["gpu_stall"], s.Labels["gpu_pc"], s.Labels["gpu_pc_attrib"])

	// And the attribution quality is stated, not implied: one execution of this
	// kernel was in the horizon, so the module join is not an inference about
	// WHICH invocation.
	assert.Equal(t, string(PCAttribKernel), s.Labels["gpu_pc_attrib"])
	assert.NotContains(t, s.Labels, "gpu_ambiguous",
		"gpu_ambiguous means a heuristic LAUNCH join and nothing else; this join was exact")
}

// TestGateResolvedAndNoLineinfoAggregateAtTheSameKernel is the second half of
// gate assertion 3: "the kernel frame is unchanged, so the two populations
// still aggregate together at the kernel level".
//
// TestProjectionEmitsAllFourSrcStatuses already asserts that a no-lineinfo
// sample carries the explicit status and no location. What it does not assert -
// and what the gate's own wording asks for - is that the resolvable and
// unresolvable populations still SHARE A STACK. That is the property that
// decides whether a flame graph of a partially-built-with-lineinfo workload
// shows one kernel block or two, and it is not implied by the labels: a
// projection that appended the source location, or the status, or a
// "[gpu:src:unknown]" placeholder to the frames would satisfy every label
// assertion and split the block in half.
//
// Mutation this catches: any frame that varies with gpu_src_status.
func TestGateResolvedAndNoLineinfoAggregateAtTheSameKernel(t *testing.T) {
	withInfo := fixture(t, "single_lineinfo.cubin")
	noInfo := fixture(t, "single_nolineinfo.cubin")
	st := NewModuleStore(ModuleStoreConfig{Capacity: 8})
	require.NoError(t, st.Put(gateCRCLineInfo, withInfo))
	require.NoError(t, st.Put(gateCRCNoLineInfo, noInfo))

	// One execution of one kernel, sampled twice: once in a module built with
	// -lineinfo and once in a module built without. On real hardware this is a
	// process that links one library built each way.
	view := pcView(PCAttribKernel,
		pcSampleAt(gateCRCLineInfo, symIndexOf(t, withInfo, "addOne"), 0x10),
		pcSampleAt(gateCRCNoLineInfo, symIndexOf(t, noInfo, "addOne"), 0x10),
	)
	view.Launch = &GPUKernelLaunch{Launch: LaunchContext{
		CPUStack: pp.FramesFromNames([]string{"main"}),
	}}

	samples, _ := ProjectExecutionsWith(Snapshot{Executions: []ExecutionView{view}},
		ProjectionConfig{Modules: st})
	require.Len(t, samples, 2)

	assert.Equal(t, "resolved", samples[0].Labels["gpu_src_status"])
	assert.Equal(t, "no-lineinfo", samples[1].Labels["gpu_src_status"])
	for _, forbidden := range []string{"gpu_src_file", "gpu_src_line", "gpu_src_func"} {
		assert.NotContains(t, samples[1].Labels, forbidden,
			"no-lineinfo must invent nothing: %s rode on a sample with no line table behind it", forbidden)
	}

	assert.Equal(t, frameNames(samples[0].Stack), frameNames(samples[1].Stack),
		"the resolvable and unresolvable populations must aggregate at the same kernel; a frame that varies with gpu_src_status splits the kernel's own block in two")
	assert.Equal(t, []string{"main", FrameLaunch, "[gpu:kernel:addOne]"}, frameNames(samples[0].Stack))
}

// TestGateGraphExecutionRefusalIsNotAssertableYet is gate assertion 10b's
// FIRST clause - "a graph execution makes Tier A refuse to start" - pinned as
// an outstanding gap rather than asserted, because the product does not
// implement it.
//
// # What exists and what does not
//
// The wire signal exists: the plan's Task 6 gave the adapter a
// classGraphExec drop class, `internal/gpuabi.DropClassGraphExec` decodes it
// and spells it "graph-exec", and the stub emits one such record so the class
// is reachable from a test. The operator warning names CUDA graphs, and the
// Tier A acknowledgement refusal names them too.
//
// What does not exist is the REFUSAL the plan specifies:
//
//	Tier A refuses to start in a process where graph executions have been
//	observed, because Tier A's whole claim is exact launch attribution and a
//	graph makes that claim false. The refusal is loud and counted, not a
//	silent downgrade to Tier B.
//
// Nothing in gpu/, gpuprobe/ or cmd/ consumes DropClassGraphExec. The
// consumer decodes gpu_dropped_v1 into batch.Drops and stops there (see
// Stats.Undecoded's own comment, which says normalizing a drop class is the
// task after Task 7). So there is no counter for graph executions, nothing on
// the Snapshot that names them, no joinhealth anomaly, and no input by which
// PCSamplingRequest could be told about them. Tier A therefore starts happily
// in a graph-using process and produces confident, exact-LOOKING attribution
// of N kernels to one call site - which is finding 4 of the plan, and the
// condition it says must be visible rather than merely out of scope.
//
// # Why this is a passing test rather than a failing one
//
// It is the shape issue #44 used and #45 inverted: an assertion that pins the
// CURRENT state, with the note that fixing the defect must fail it. Writing
// the real assertion now would leave the gate red on a branch that is not
// allowed to change product behaviour; leaving nothing at all would let the
// gate ship claiming twelve assertions while one of them was never written.
//
// When Task 10's refusal lands, this test fails - by name, in the gate's own
// file - and the person landing it replaces it with the real assertion:
// a stub reporting a graph execution makes Tier A refuse to start, loudly and
// counted.
func TestGateGraphExecutionRefusalIsNotAssertableYet(t *testing.T) {
	mentionsGraph := func(v any) []string {
		var out []string
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			if name := typ.Field(i).Name; strings.Contains(strings.ToLower(name), "graph") {
				out = append(out, name)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"Snapshot", Snapshot{}},
		{"TimelineDropStats", TimelineDropStats{}},
		{"PCJoinStats", PCJoinStats{}},
		{"TimelineConfig", TimelineConfig{}},
		{"PCSamplingRequest", PCSamplingRequest{}},
	} {
		assert.Empty(t, mentionsGraph(tc.v),
			"%s now names graph executions (%v). Gate assertion 10b's first clause has become "+
				"assertable: replace this test with the real one - a stub reporting a graph "+
				"execution makes Tier A refuse to start, loudly and counted.",
			tc.name, mentionsGraph(tc.v))
	}

	// The half that IS true today, so this test is not purely negative: the
	// operator is told, in the refusal they must read before Tier A can run at
	// all, that the tier is unavailable where graphs are in use.
	_, err := PCSamplingRequest{Flag: "serialized"}.Select()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUDA graphs",
		"the only place the graph limitation is stated to an operator is the Tier A acknowledgement refusal; if that text loses it, nothing anywhere names the condition")
	warning := strings.Join(PCSamplingStandingWarning(PCSamplingSerialized), "\n")
	assert.Contains(t, warning, "CUDA GRAPHS",
		"the standing warning must keep naming the third perturbation")
	t.Log("gate assertion 10b, first clause: OUTSTANDING - see this test's doc comment and " +
		".superpowers/sdd/task-13-gate-report.md")
}
