package gpu

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CUDA-graph refusal — issue #94.
//
// A CUDA graph launch fires ONE runtime callback for N kernels and gpu_exec_v1
// carries no graph id, so all N executions arrive under one correlation. Tier
// A's entire claim is exact launch attribution, so in such a process that claim
// is false — and false in the worst available way, because nothing about it
// looks wrong: gpu_join reads "exact", gpu_pc_attrib reads "exact", every join
// counter reads green, and N kernels' time and samples are billed to one CPU
// call site.
//
// The tests below assert the refusal in both of its halves (it never starts;
// once started it is withdrawn), its LOUDNESS (counted, in the snapshot, in
// joinhealth, on the labels), and the two things it must NOT do: weaken Tier B,
// which joins through the module rather than through the launch and is
// genuinely unaffected, and discard the measurements it refuses to attribute.

const graphPID = uint32(9001)

// graphTimeline is a Tier A timeline; graphTimelineIn is one in any tier, so a
// test can assert the same input produces opposite behaviour in Tier B.
func graphTimeline() *Timeline { return graphTimelineIn(PCSamplingSerialized) }

func graphTimelineIn(tier PCSamplingTier) *Timeline {
	return NewTimeline(TimelineConfig{PCSampling: tier})
}

// graphExec drives one launch, one exact-correlation PC sample and one
// execution through a Timeline the way the consumer does, so the resulting view
// carries PCAttribExact — the claim the refusal exists to withdraw.
func graphExec(t *testing.T, tl *Timeline, pid uint32, value string, startNs uint64) {
	t.Helper()
	corr := CorrelationID{Backend: BackendCUPTI, PID: pid, Value: value}
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: corr,
		ClockDomain: ClockDomainCPUMonotonic,
		TimeNs:      startNs,
		Launch:      LaunchContext{PID: pid, TimeNs: startNs},
	}))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: corr,
		Module:      ModuleRef{Backend: BackendCUPTI, CRC: 0xC0DE},
		ClockDomain: ClockDomainCPUMonotonic,
		TimeNs:      startNs + 10,
		PCOffset:    0x40,
		StallReason: "long_scoreboard",
		Count:       1,
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: corr,
		ClockDomain: ClockDomainCPUMonotonic,
		StartNs:     startNs,
		EndNs:       startNs + 100,
		KernelName:  "addOne",
	}))
}

func graphReport(pid uint32, count uint64) GPUGraphExecutions {
	return GPUGraphExecutions{Backend: BackendCUPTI, PID: pid, Count: count}
}

// ---------------------------------------------------------------------------
// The START half: Tier A never runs at all
// ---------------------------------------------------------------------------

// TestGraphExecutionsMakeTierARefuseToStart is the plan's Task 10 sentence,
// asserted: "Tier A refuses to start in a process where graph executions have
// been observed."
//
// It is a REFUSAL and not a downgrade, and that is the half worth pinning. A
// silent fall back to "continuous" would leave the operator reading an inferred
// profile while believing they had asked for and received an exact one, and
// nothing in the output would say which they got — indistinguishable from Tier
// A working, which is the specific failure the plan calls out.
func TestGraphExecutionsMakeTierARefuseToStart(t *testing.T) {
	tier, err := PCSamplingRequest{
		Flag:                    PCSamplingNameSerialized,
		AcknowledgePerturbation: true,
		GraphExecutionsObserved: true,
	}.Select()

	require.Error(t, err)
	require.ErrorIs(t, err, ErrPCSamplingGraphExecutions)
	assert.Equal(t, PCSamplingOff, tier,
		"a refused Tier A must resolve to OFF, never to continuous: a downgrade the operator "+
			"did not choose is indistinguishable from Tier A working")
	assert.NotErrorIs(t, err, ErrPCSamplingNotAcknowledged,
		"the graph refusal must be its own sentinel, so a driver can tell 'acknowledge it' from "+
			"'this tier cannot work here' — the first has a remedy the operator can apply")
	for _, want := range []string{"CUDA graph", "exact", "continuous"} {
		assert.Contains(t, err.Error(), want,
			"the refusal must name the condition, the claim it breaks and the tier that still works")
	}
}

// TestTheGraphRefusalIsNotOverridableByTheAcknowledgement pins the ORDER of the
// two Tier A gates.
//
// The perturbation acknowledgement is a trade: the operator agrees to have
// their kernels serialized in exchange for exact attribution. In a graph-using
// process they would pay that cost and receive attribution that is
// exact-looking and many-to-one, which is not the trade they agreed to. So the
// graph refusal is checked FIRST and there is no flag that turns it off.
func TestTheGraphRefusalIsNotOverridableByTheAcknowledgement(t *testing.T) {
	for _, ack := range []bool{false, true} {
		_, err := PCSamplingRequest{
			Flag:                    PCSamplingNameSerialized,
			AcknowledgePerturbation: ack,
			GraphExecutionsObserved: true,
		}.Select()
		require.ErrorIs(t, err, ErrPCSamplingGraphExecutions,
			"acknowledging the perturbation must not buy past the graph refusal (ack=%v)", ack)
	}
}

// The refusal gates Tier A and NOTHING else. Tier B is unaffected by graphs
// because it joins through the module rather than through the launch, and "off"
// has no claim to withdraw; refusing either would be a false alarm that makes
// the real one easier to ignore.
func TestTheGraphRefusalGatesTierAAndNoOtherTier(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want PCSamplingTier
	}{
		{PCSamplingNameOff, PCSamplingOff},
		{PCSamplingNameContinuous, PCSamplingContinuous},
	} {
		tier, err := PCSamplingRequest{Flag: tc.flag, GraphExecutionsObserved: true}.Select()
		require.NoError(t, err, "%s must not be refused for CUDA graphs", tc.flag)
		assert.Equal(t, tc.want, tier)
	}
}

// ---------------------------------------------------------------------------
// The MID-RUN half: the claim is withdrawn
// ---------------------------------------------------------------------------

// TestGraphExecutionWithdrawsTierAExactAttribution is the mid-run refusal: a
// process's first graph launch can be minutes into a run that started
// legitimately, so Select cannot be the whole answer.
//
// The same Timeline is driven twice with identical events, differing only in
// whether a graph execution was reported, and the two snapshots are compared.
// That shape is the point: a refusal asserted only in its own scenario cannot
// show that the CONTROL run is green, and "green when healthy, loud when not"
// is the whole property.
func TestGraphExecutionWithdrawsTierAExactAttribution(t *testing.T) {
	clean := graphTimeline()
	graphExec(t, clean, graphPID, "1", 1_000)
	cleanSnap := clean.Snapshot()

	require.Len(t, cleanSnap.Executions, 1)
	require.Equal(t, PCAttribExact, cleanSnap.Executions[0].PCAttrib,
		"the control run must actually reach the exact claim, or this test proves nothing")
	assert.Zero(t, cleanSnap.GraphExecutions,
		"a run with no CUDA graph must read EXACTLY zero here")
	assert.Zero(t, cleanSnap.ExecutionsGraphRefused)
	assert.Zero(t, cleanSnap.PCJoin.GraphRefusedAttributions)
	assert.False(t, cleanSnap.Executions[0].GraphRefused)
	assert.False(t, cleanSnap.TierAGraphRefused())

	refused := graphTimeline()
	require.NoError(t, refused.EmitGraphExecutions(graphReport(graphPID, 7)))
	graphExec(t, refused, graphPID, "1", 1_000)
	snap := refused.Snapshot()

	require.Len(t, snap.Executions, 1)
	assert.Equal(t, PCAttribGraphRefused, snap.Executions[0].PCAttrib,
		"gpu_pc_attrib must stop saying \"exact\" the moment a graph makes it false")
	assert.True(t, snap.Executions[0].GraphRefused)
	assert.True(t, snap.TierAGraphRefused())
	assert.Equal(t, uint64(7), snap.GraphExecutions)
	assert.Equal(t, uint64(1), snap.GraphExecProcesses)
	assert.Equal(t, uint64(1), snap.ExecutionsGraphRefused)
	assert.Equal(t, uint64(1), snap.PCJoin.GraphRefusedAttributions)
	assert.Zero(t, snap.GraphExecUnscoped)
	assert.Zero(t, snap.GraphExecTrackingCapped)
}

// TestGraphRefusalKeepsTheMeasurementsItRefusesToAttribute is the explicit
// answer to "what happens to the samples collected before the refusal fired".
//
// KEPT, AND MARKED. Not discarded, and not left unmarked.
//
//   - Discarding them is the silent-downgrade shape wearing different clothes:
//     a profile with the graph process's GPU time removed is indistinguishable
//     from a profile of a process that did no GPU work. It would also destroy
//     the gpu_serialized="true" disclosure for kernels that really were
//     serialized — the profiler perturbed that workload, and deleting the
//     evidence does not un-perturb it, it only hides that the damage was done.
//   - Leaving them unmarked is the defect itself.
//
// So the durations, the sample weights, the stall reasons and the serialization
// disclosure all survive untouched. Exactly one thing is taken away: the claim
// about WHICH LAUNCH they belong to.
func TestGraphRefusalKeepsTheMeasurementsItRefusesToAttribute(t *testing.T) {
	tl := graphTimeline()
	emitBurst(t, tl, uint64(graphPID), 900, 1_500)
	graphExec(t, tl, graphPID, "1", 1_000)
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 3)))
	snap := tl.Snapshot()

	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	require.Len(t, view.PCSamples, 1, "the PC samples must be KEPT, not dropped with the claim")
	assert.Equal(t, uint64(1_000), view.Exec.StartNs)
	assert.Equal(t, uint64(1_100), view.Exec.EndNs, "the measured duration is a measurement and survives")
	assert.Equal(t, uint64(1), snap.AttributedPCSamples)
	assert.Equal(t, SerializationSerialized, view.Serialized,
		"the perturbation disclosure survives the refusal: those kernels really did run serialized, "+
			"and dropping the mark would hide damage the profiler actually did")
	assert.Equal(t, uint64(1), snap.ExecutionsSerialized)

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 1)
	assert.Equal(t, "true", samples[0].Labels["gpu_serialized"])
	assert.Equal(t, "true", samples[0].Labels["gpu_graph_refused"])
	assert.Equal(t, string(PCAttribGraphRefused), samples[0].Labels["gpu_pc_attrib"])
	assert.Equal(t, "long_scoreboard", samples[0].Labels["gpu_stall"])
	assert.Positive(t, samples[0].Value, "the sample keeps its full share of the execution's duration")
}

// TestGraphRefusalIsRetroactiveOverEverythingTheTimelineStillHolds covers the
// measured wrinkle from the Task 10 report: g_exec_from_graph is set from
// CUpti_ActivityKernel12.graphId on the ACTIVITY path, which reaches the agent
// on the producer's drain tick — up to 100 ms, and one or two bursts, after the
// first graph kernel ran.
//
// So executions ARRIVE BEFORE the report that condemns them. The mark is a
// property of the process and is applied at Snapshot, not at ingest, so every
// execution the Timeline still holds is marked however late the report came. A
// driver that snapshots once at the end of a run — which both drivers in this
// tree do — therefore has no unmarked executions at all.
//
// What is NOT covered, and cannot be from here, is a driver that snapshots
// periodically: a snapshot already emitted before the first report carries
// executions this one would have refused. joinhealth says so in its own line.
func TestGraphRefusalIsRetroactiveOverEverythingTheTimelineStillHolds(t *testing.T) {
	tl := graphTimeline()
	for _, v := range []string{"1", "2", "3"} {
		graphExec(t, tl, graphPID, v, 1_000)
	}
	// The report arrives LAST, after every execution it condemns.
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 3)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 3)
	for i, v := range snap.Executions {
		assert.True(t, v.GraphRefused, "execution %d arrived before the report and must still be marked", i)
		assert.Equal(t, PCAttribGraphRefused, v.PCAttrib)
	}
	assert.Equal(t, uint64(3), snap.ExecutionsGraphRefused)
}

// ---------------------------------------------------------------------------
// What the refusal must NOT do
// ---------------------------------------------------------------------------

// TestGraphExecutionsDoNotWeakenTierB is the asymmetry, and it is deliberate
// rather than lucky: Tier B reaches a PC sample's kernel through the cubin CRC
// and the function index, never through the launch, so a graph-launched kernel
// resolves to its own kernel exactly as any other does.
//
// Nothing about Tier B is false in a graph-using process. Marking it would be a
// false alarm on a healthy run, and a false alarm is how the real one stops
// being read.
func TestGraphExecutionsDoNotWeakenTierB(t *testing.T) {
	for _, tier := range []PCSamplingTier{PCSamplingOff, PCSamplingContinuous} {
		t.Run(tier.String(), func(t *testing.T) {
			tl := graphTimelineIn(tier)
			require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 11)))
			graphExec(t, tl, graphPID, "1", 1_000)
			snap := tl.Snapshot()

			require.Len(t, snap.Executions, 1)
			assert.Equal(t, PCAttribExact, snap.Executions[0].PCAttrib,
				"a graph does not make THIS tier's attribution false; downgrading it would weaken "+
					"the tier that ships toward always-on for a condition that does not affect it")
			assert.False(t, snap.Executions[0].GraphRefused)
			assert.Zero(t, snap.ExecutionsGraphRefused)
			assert.Zero(t, snap.PCJoin.GraphRefusedAttributions)
			assert.False(t, snap.TierAGraphRefused())

			// The fact is still RECORDED, in every tier. Ingest is not gated;
			// only the consequence is.
			assert.Equal(t, uint64(11), snap.GraphExecutions,
				"the report must still be counted: a producer emitting it and an agent silently "+
					"discarding it leaves the two ends out of step")

			lines := JoinHealth(snap)
			assert.NotContains(t, strings.Join(lines, "\n"), joinAnomalyPrefix+": 11 kernel executions",
				"a graph is not an anomaly in this tier")
			for _, l := range lines[1:] {
				assert.NotContains(t, l, "WITHDRAWN")
			}
			assert.Contains(t, lines[0], "unaffected",
				"the summary should say so once, so an operator learns not to look for the refusal here")

			samples := ProjectExecutions(snap)
			require.Len(t, samples, 1)
			assert.NotContains(t, samples[0].Labels, "gpu_graph_refused")
			assert.Equal(t, string(PCAttribExact), samples[0].Labels["gpu_pc_attrib"])
		})
	}
}

// TestGraphRefusalDoesNotTouchModuleKeyedAttribution is the Tier B asymmetry at
// the level of one label, under Tier A itself: even where the refusal IS armed,
// a sample that reached its execution through the module rather than through
// the launch keeps its own attribution. Only "exact" is withdrawn.
//
// Mutation this catches: implementing the refusal with worsePCAttrib, which
// would rank "graph-refused" over every module-keyed value and bury the
// executions whose claim really did become false among ones whose did not.
func TestGraphRefusalDoesNotTouchModuleKeyedAttribution(t *testing.T) {
	for _, keep := range []PCAttrib{PCAttribKernel, PCAttribKernelAmbiguous, PCAttribKernelMultiDevice} {
		view := pcView(keep, pcSampleAt(0xC0DE, 7, 0x10))
		view.GraphRefused = true
		samples := ProjectExecutions(Snapshot{Executions: []ExecutionView{view}})
		require.Len(t, samples, 1)
		assert.Equal(t, string(keep), samples[0].Labels["gpu_pc_attrib"],
			"%s reached its execution through the module, which a CUDA graph does not damage", keep)
		assert.Equal(t, "true", samples[0].Labels["gpu_graph_refused"],
			"the process-level fact still rides, because gpu_join is still many-to-one there")
	}
}

// TestGraphRefusalLeavesModuleKeyedAttributionAloneThroughTheJoin is the same
// asymmetry as the test above, but driven through Timeline.Snapshot rather
// than through a hand-built view — because the refusal is APPLIED in Snapshot,
// and a test that only exercises the projection cannot catch a Snapshot that
// applies it too widely.
//
// Mutation this catches, and the one above does not: relaxing Snapshot's
// `view.PCAttrib == PCAttribExact` guard to `view.PCAttrib != ""`, which would
// turn every Tier B attribution in a graph-using process into "graph-refused"
// — weakening the tier that is genuinely unaffected, and burying the
// executions whose claim really did become false among ones whose did not.
func TestGraphRefusalLeavesModuleKeyedAttributionAloneThroughTheJoin(t *testing.T) {
	const pid = uint32(4242)
	// Tier A selected, so the refusal IS armed - but these samples carry no
	// correlation and reach their execution through the module, exactly as
	// they would in Tier B.
	f := newPCJoinFixture(t, TimelineConfig{PCSampling: PCSamplingSerialized})
	require.NoError(t, f.tl.EmitGraphExecutions(graphReport(pid, 4)))
	f.sample(t, pid, 10)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	require.Len(t, view.PCSamples, 1)
	assert.Equal(t, PCAttribKernel, view.PCAttrib,
		"a module-keyed attribution is not damaged by a CUDA graph and must survive the refusal intact")
	assert.Zero(t, snap.PCJoin.GraphRefusedAttributions,
		"nothing here claimed exact attribution, so nothing was withdrawn")

	// The process-level fact still rides, because gpu_join on this execution
	// is still describing a launch join a graph makes many-to-one.
	assert.True(t, view.GraphRefused)
	assert.Equal(t, uint64(1), snap.ExecutionsGraphRefused)
	assertPCJoinIdentities(t, snap)
}

// TestGraphRefusalIsScopedToItsOwnProcess. PC-sampling collection mode is a
// property of the profiled process — the burst controller lives inside it — so
// one graph-using process in a system-wide profile must not withdraw the exact
// attribution of every other process being profiled beside it.
func TestGraphRefusalIsScopedToItsOwnProcess(t *testing.T) {
	const otherPID = uint32(9002)
	tl := graphTimeline()
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 5)))
	graphExec(t, tl, graphPID, "1", 1_000)
	graphExec(t, tl, otherPID, "2", 2_000)

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 2)
	byPID := map[uint32]ExecutionView{}
	for _, v := range snap.Executions {
		byPID[v.Exec.Correlation.PID] = v
	}
	assert.True(t, byPID[graphPID].GraphRefused)
	assert.Equal(t, PCAttribGraphRefused, byPID[graphPID].PCAttrib)
	assert.False(t, byPID[otherPID].GraphRefused,
		"a neighbour's CUDA graph must not withdraw this process's exact attribution")
	assert.Equal(t, PCAttribExact, byPID[otherPID].PCAttrib)
	assert.Equal(t, uint64(1), snap.ExecutionsGraphRefused)
	assert.Equal(t, uint64(1), snap.GraphExecProcesses)
}

// TestAnUnscopedGraphReportWidensRatherThanVanishes. A report that names no
// process is evidence whose SCOPE is missing, not missing evidence. Discarding
// it would throw away a proven refusal; widening it over-marks, which is the
// safe direction and is counted so a reader can tell it apart from "every
// process really did use graphs".
//
// This is the OPPOSITE resolution to the multi-device tracker's cap, where an
// untracked process is treated as single-device — there, absence of evidence
// for a second device is not evidence of one.
func TestAnUnscopedGraphReportWidensRatherThanVanishes(t *testing.T) {
	tl := graphTimeline()
	require.NoError(t, tl.EmitGraphExecutions(graphReport(0, 4)))
	graphExec(t, tl, graphPID, "1", 1_000)
	graphExec(t, tl, 9002, "2", 2_000)

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 2)
	for i, v := range snap.Executions {
		assert.True(t, v.GraphRefused, "execution %d must be marked when the scope is unknown", i)
	}
	assert.Equal(t, uint64(4), snap.GraphExecutions)
	assert.Equal(t, uint64(4), snap.GraphExecUnscoped)
	assert.Zero(t, snap.GraphExecProcesses, "no process was named, so none was tracked")
	assert.Equal(t, uint64(2), snap.ExecutionsGraphRefused)

	health := strings.Join(JoinHealth(snap), "\n")
	assert.Contains(t, health, "lost its per-process scope",
		"an operator looking at a profile where everything is marked must be able to tell "+
			"'they all used graphs' from 'we stopped being able to say which one did'")
}

// A report carrying zero arms nothing. The producer emits DELTAS, and a zero
// delta is not evidence; latching on a record's mere arrival would make "no
// graphs yet" indistinguishable from "graphs".
func TestAZeroGraphReportArmsNothing(t *testing.T) {
	tl := graphTimeline()
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 0)))
	graphExec(t, tl, graphPID, "1", 1_000)

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Zero(t, snap.GraphExecutions)
	assert.False(t, snap.TierAGraphRefused())
	assert.Equal(t, PCAttribExact, snap.Executions[0].PCAttrib)
}

// The counts ACCUMULATE. The producer reports only what it has not reported
// before, so a consumer that replaced rather than summed would under-report the
// condition by every report but the last.
func TestGraphExecutionReportsAccumulate(t *testing.T) {
	tl := graphTimeline()
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 2)))
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 3)))
	require.NoError(t, tl.EmitGraphExecutions(graphReport(9002, 1)))

	snap := tl.Snapshot()
	assert.Equal(t, uint64(6), snap.GraphExecutions)
	assert.Equal(t, uint64(2), snap.GraphExecProcesses)
}

// ---------------------------------------------------------------------------
// Loudness
// ---------------------------------------------------------------------------

// TestGraphRefusalIsLoudInJoinHealth. "Loud and counted, not a silent downgrade
// to Tier B" is the plan's wording, and joinhealth is where an operator meets
// it. The anomaly must name the mechanism (one callback, N kernels), say what
// was done about it, and point at the tier that still works.
func TestGraphRefusalIsLoudInJoinHealth(t *testing.T) {
	tl := graphTimeline()
	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 12)))
	graphExec(t, tl, graphPID, "1", 1_000)
	snap := tl.Snapshot()

	lines := JoinHealth(snap)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"launched from CUDA GRAPHS",
		"ONE graph launch fires ONE runtime callback for N kernels",
		"FALSE here while still looking",
		"gpu_graph_refused",
		string(PCAttribGraphRefused),
		"--gpu-pc-sampling=continuous",
	} {
		assert.Contains(t, joined, want, "the anomaly must say this")
	}
	assert.Contains(t, lines[0], "WITHDRAWN",
		"the one line printed on every run must carry it too; an operator who reads only the "+
			"summary must not come away believing the attribution held")
	assert.NotContains(t, lines[0], "no anomalies")

	// And the pre-refusal window, stated rather than left to be discovered.
	assert.Contains(t, joined, "RETROACTIVE within a snapshot but not across one")
}

// The counter is assertable at ZERO on a healthy run and NON-ZERO the moment a
// graph execution appears. Nineteen defects on this project have been counters
// reading green exactly when things were worst; a counter that cannot be
// asserted in both directions is not a counter.
func TestGraphCountersAreZeroOnAHealthyRunAndNonZeroImmediately(t *testing.T) {
	tl := graphTimeline()
	graphExec(t, tl, graphPID, "1", 1_000)
	before := tl.Snapshot()
	assert.Zero(t, before.GraphExecutions)
	assert.Zero(t, before.GraphExecProcesses)
	assert.Zero(t, before.ExecutionsGraphRefused)
	assert.Zero(t, before.PCJoin.GraphRefusedAttributions)
	assert.Zero(t, before.GraphExecUnscoped)
	assert.Zero(t, before.GraphExecTrackingCapped)
	// The healthy run raises the ordinary Tier A "no window covered these
	// executions" anomaly and NOT the graph one, which is the distinction that
	// matters: the graph line must appear exactly when a graph appears.
	// "launched from CUDA GRAPHS" and not merely "CUDA GRAPHS": the standing
	// Tier A warning names them too, on every render, and matching that would
	// make this assertion pass for the wrong reason.
	assert.NotContains(t, strings.Join(JoinHealth(before), "\n"), "launched from CUDA GRAPHS")
	assert.NotContains(t, JoinHealth(before)[0], "WITHDRAWN")

	require.NoError(t, tl.EmitGraphExecutions(graphReport(graphPID, 1)))
	graphExec(t, tl, graphPID, "2", 2_000)
	after := tl.Snapshot()
	assert.Equal(t, uint64(1), after.GraphExecutions)
	assert.Equal(t, uint64(1), after.ExecutionsGraphRefused)
	assert.Contains(t, strings.Join(JoinHealth(after), "\n"), "launched from CUDA GRAPHS")
	assert.Contains(t, JoinHealth(after)[0], "WITHDRAWN")
}

// TierAGraphRefused is a METHOD and not a stored bool, so it cannot disagree
// with the two facts it is derived from. A hand-built Snapshot, a copy, or a
// forgotten assignment would otherwise be able to leave it false while
// GraphExecutions was non-zero — a silent downgrade wearing the shape of the
// refusal.
func TestTierAGraphRefusedIsDerivedNotStored(t *testing.T) {
	assert.True(t, Snapshot{PCSampling: PCSamplingSerialized, GraphExecutions: 1}.TierAGraphRefused())
	assert.False(t, Snapshot{PCSampling: PCSamplingSerialized}.TierAGraphRefused())
	assert.False(t, Snapshot{PCSampling: PCSamplingContinuous, GraphExecutions: 99}.TierAGraphRefused())
	assert.False(t, Snapshot{GraphExecutions: 99}.TierAGraphRefused())

	// It is not settable, which is the property. A field named for it would
	// be; a method is not.
	assert.False(t, errors.Is(nil, ErrPCSamplingGraphExecutions))
}
