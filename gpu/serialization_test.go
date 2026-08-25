package gpu

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The serialization disclosure, end to end through Timeline.
//
// The assertion these tests exist for is negative and is repeated in several
// shapes on purpose: "unknown" must never come out as "false". Every other
// property here is in service of that one, because a profile that says "this
// duration was not perturbed" when it means "I cannot tell" is precisely what
// spec §4 forbids — and it is the failure mode fifteen defects on this project
// have taken: a counter or a check reading green exactly when things were
// worst.

const tierAPID = uint32(4242)

func tierATimeline() *Timeline {
	return NewTimeline(TimelineConfig{PCSampling: PCSamplingSerialized})
}

// serializedExec is an execution from tierAPID over [startNs, endNs]. The PID
// is on the correlation because that is where the disclosure reads it from —
// windows are per-process, and an execution that does not name its process
// cannot be placed against any of them.
func serializedExec(value string, startNs, endNs uint64) GPUKernelExec {
	return GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, PID: tierAPID, Value: value},
		KernelName:  "k_" + value,
		StartNs:     startNs,
		EndNs:       endNs,
	}
}

func burst(pid uint32, startNs, endNs uint64) GPUSamplingWindow {
	return GPUSamplingWindow{
		Backend:     BackendCUPTI,
		PID:         pid,
		ClockDomain: ClockDomainCPUMonotonic,
		StartNs:     startNs,
		EndNs:       endNs,
		Mode:        SamplingModeKernelSerialized,
	}
}

// emitBurst delivers a burst the way the producer does: an OPEN record the
// instant it starts, a CLOSED record with the same start when it stops.
func emitBurst(t *testing.T, tl *Timeline, pid, startNs, endNs uint64) {
	t.Helper()
	require.NoError(t, tl.EmitSamplingWindow(burst(uint32(pid), startNs, 0)))
	require.NoError(t, tl.EmitSamplingWindow(burst(uint32(pid), startNs, endNs)))
}

// states pulls the disclosure off a snapshot in execution order.
func states(snap Snapshot) []string {
	out := make([]string, len(snap.Executions))
	for i, v := range snap.Executions {
		out[i] = v.Serialized.String()
	}
	return out
}

// assertSumIdentity is the discipline gpu_join's three outcomes already carry,
// applied to the three the disclosure adds. It is called from every test below
// rather than from one of them: a shortfall means some execution reached the
// profile carrying no disclosure at all, and that is exactly the condition
// that would otherwise be invisible.
func assertSumIdentity(t *testing.T, snap Snapshot) {
	t.Helper()
	sum := snap.ExecutionsSerialized + snap.ExecutionsNotSerialized +
		snap.ExecutionsSerializationUnknown
	require.Equal(t, uint64(len(snap.Executions)), sum,
		"the three gpu_serialized outcomes must sum to the executions in the snapshot")
}

// ---------------------------------------------------------------------------
// With Tier A off, "false" is correct and unconditional.

func TestSerializationIsFalseUnconditionallyWhenTierAWasNotSelected(t *testing.T) {
	tl := NewTimeline(TimelineConfig{}) // the default: no serialized sampling
	require.NoError(t, tl.EmitExec(serializedExec("a", 100, 200)))
	require.NoError(t, tl.EmitExec(serializedExec("b", 300, 400)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"false", "false"}, states(snap))
	assert.Equal(t, uint64(2), snap.ExecutionsNotSerialized)
	assert.Zero(t, snap.ExecutionsSerializationUnknown)
	assertSumIdentity(t, snap)
}

// Windows can arrive even with the agent configured for continuous collection
// (a producer left over from another run, a system-wide attach). They are
// ingested and counted — but they cannot make an execution "true", because
// nothing this agent asked for serializes anything.
func TestSerializationIgnoresWindowsWhenTierAWasNotSelected(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	emitBurst(t, tl, uint64(tierAPID), 100, 200)
	require.NoError(t, tl.EmitExec(serializedExec("inside", 120, 180)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"false"}, states(snap))
	assert.Equal(t, uint64(2), snap.SamplingWindowsReceived,
		"the window is still ingested and counted; only the answer is unconditional")
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// The three values.

func TestSerializationMarksExecutionsOverlappingABurst(t *testing.T) {
	tl := tierATimeline()
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	emitBurst(t, tl, uint64(tierAPID), 4000, 5000)

	// Deliberately includes executions that only PARTLY overlap: a kernel
	// straddling a burst boundary ran serialized for part of its life, which
	// is enough to make its measured duration perturbed.
	require.NoError(t, tl.EmitExec(serializedExec("wholly-inside", 1200, 1800)))
	require.NoError(t, tl.EmitExec(serializedExec("straddles-start", 900, 1100)))
	require.NoError(t, tl.EmitExec(serializedExec("straddles-end", 1900, 2100)))
	require.NoError(t, tl.EmitExec(serializedExec("spans-a-burst", 800, 2200)))
	require.NoError(t, tl.EmitExec(serializedExec("in-the-gap", 2500, 3500)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"true", "true", "true", "true", "false"}, states(snap))
	assert.Equal(t, uint64(4), snap.ExecutionsSerialized)
	assert.Equal(t, uint64(1), snap.ExecutionsNotSerialized)
	assert.Zero(t, snap.ExecutionsSerializationUnknown)
	assertSumIdentity(t, snap)
}

// "straddles-start" above begins before the first window and is still "true".
// That is not an accident of the coverage rule — it is the rule that matters,
// so it gets its own assertion: an execution that provably overlapped a burst
// is perturbed whatever else is unknown about the rest of its interval.
func TestSerializationTrueOutranksUnknown(t *testing.T) {
	tl := tierATimeline()
	// One closed burst, then one that never closed.
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	require.NoError(t, tl.EmitSamplingWindow(burst(tierAPID, 3000, 0)))

	// This execution overlaps the closed burst AND runs past the open one's
	// start. It is definitely perturbed.
	require.NoError(t, tl.EmitExec(serializedExec("both", 1500, 3500)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"true"}, states(snap))
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// "unknown", and the ways it is reached.

// The plan's first case: Tier A was selected and no window records arrived at
// all — a dropped batch, a late attach, a producer that never fired the probe.
func TestSerializationIsUnknownWhenNoWindowsArrived(t *testing.T) {
	tl := tierATimeline()
	require.NoError(t, tl.EmitExec(serializedExec("a", 100, 200)))
	require.NoError(t, tl.EmitExec(serializedExec("b", 300, 400)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"unknown", "unknown"}, states(snap))
	assert.Equal(t, uint64(2), snap.ExecutionsSerializationUnknown)
	assert.Zero(t, snap.ExecutionsNotSerialized,
		`"no window arrived" must never be reported as "not serialized"`)
	assert.Zero(t, snap.SamplingWindowsReceived)
	assertSumIdentity(t, snap)
}

// An execution outside the span the agent holds an unbroken history for is
// unplaceable in either direction. Before the first window this is not merely
// conservative, it is correct: the shim cannot emit a window before a consumer
// attaches, so everything before the first one it sees is genuinely unknown.
func TestSerializationIsUnknownOutsideTheCoveredSpan(t *testing.T) {
	tl := tierATimeline()
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)

	require.NoError(t, tl.EmitExec(serializedExec("before", 100, 200)))
	require.NoError(t, tl.EmitExec(serializedExec("inside-coverage", 1000, 2000)))
	require.NoError(t, tl.EmitExec(serializedExec("after", 9000, 9500)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"unknown", "true", "unknown"}, states(snap))
	assertSumIdentity(t, snap)
}

// A gap between two known bursts IS proven, and must read "false" — otherwise
// the tier discloses nothing usable and every execution is "unknown".
func TestSerializationIsFalseInAProvenGap(t *testing.T) {
	tl := tierATimeline()
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	emitBurst(t, tl, uint64(tierAPID), 4000, 5000)

	require.NoError(t, tl.EmitExec(serializedExec("gap", 2500, 3500)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"false"}, states(snap))
	assert.Equal(t, uint64(1), snap.ExecutionsNotSerialized)
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// The open window. This is the test the plan singles out.

// A window with end_ns == 0 is OPEN, not zero-length. Every execution from its
// start_ns onward is "unknown" and NEVER "false" — treating it as zero-length
// would mark a whole perturbed tail "not serialized".
//
// Swept rather than sampled: the assertion is over every execution position
// from well before the open window to well after it, and the negative half of
// it ("never false") is checked on every single one.
func TestSerializationOpenWindowMakesEverythingFromItsStartUnknownAndNeverFalse(t *testing.T) {
	tl := tierATimeline()
	// A complete burst first, so the store has genuine coverage and a naive
	// implementation would happily answer "false" after it.
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	// Then a burst that opens and never closes: the hard-exit case.
	const openAt = uint64(4000)
	require.NoError(t, tl.EmitSamplingWindow(burst(tierAPID, openAt, 0)))

	for start := uint64(0); start <= 10000; start += 250 {
		require.NoError(t, tl.EmitExec(
			serializedExec(fmt.Sprintf("e%d", start), start, start+100)))
	}

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 41)
	for _, v := range snap.Executions {
		if v.Exec.EndNs >= openAt {
			assert.NotEqual(t, SerializationNotSerialized, v.Serialized,
				"execution [%d,%d] touches or follows an OPEN window at %d: it must never "+
					"read \"false\"", v.Exec.StartNs, v.Exec.EndNs, openAt)
			assert.Equal(t, SerializationUnknown, v.Serialized,
				"execution [%d,%d] touches or follows an OPEN window at %d",
				v.Exec.StartNs, v.Exec.EndNs, openAt)
		}
	}
	assert.Positive(t, snap.ExecutionsSerializationUnknown)
	assert.Equal(t, 1, snap.SamplingWindowsOpen,
		"the open burst is a gauge the operator can read, not only an internal state")
	assertSumIdentity(t, snap)
}

// The other half of the open-window contract: the CLOSED record supersedes its
// own burst's open record, so an ordinary duty cycle does not leave a trail of
// permanently-open windows behind it. Both delivery orders are checked,
// because a lossy transport produces both.
func TestSerializationClosedWindowSupersedesItsOwnOpenRecord(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order []GPUSamplingWindow
	}{
		{"open then closed", []GPUSamplingWindow{
			burst(tierAPID, 1000, 0), burst(tierAPID, 1000, 2000)}},
		{"closed then open", []GPUSamplingWindow{
			burst(tierAPID, 1000, 2000), burst(tierAPID, 1000, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := tierATimeline()
			for _, w := range tc.order {
				require.NoError(t, tl.EmitSamplingWindow(w))
			}
			emitBurst(t, tl, uint64(tierAPID), 4000, 5000)
			require.NoError(t, tl.EmitExec(serializedExec("gap", 2500, 3500)))

			snap := tl.Snapshot()
			assert.Equal(t, 2, snap.SamplingWindowsHeld, "two bursts, not four records")
			assert.Zero(t, snap.SamplingWindowsOpen,
				"an open record must never survive its own burst's close")
			assert.Equal(t, []string{"false"}, states(snap),
				"a closed burst either side of the gap is what proves the gap")
			assertSumIdentity(t, snap)
		})
	}
}

// ---------------------------------------------------------------------------
// A hole in the window history.

// A sequence gap means records between the last window and this one were lost,
// so the interval they covered cannot be shown to be a gap. Coverage restarts;
// executions before the hole degrade from "false" to "unknown", and the
// windows already held still prove "true" for anything that overlapped them.
func TestSerializationSequenceGapRestartsCoverageRatherThanSpanningIt(t *testing.T) {
	tl := tierATimeline()
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	emitBurst(t, tl, uint64(tierAPID), 3000, 4000)
	// Two records lost, then a burst much later. Nothing says what happened
	// in between.
	lost := burst(tierAPID, 20000, 0)
	lost.Lost = 2
	require.NoError(t, tl.EmitSamplingWindow(lost))
	require.NoError(t, tl.EmitSamplingWindow(burst(tierAPID, 20000, 21000)))
	emitBurst(t, tl, uint64(tierAPID), 23000, 24000)

	require.NoError(t, tl.EmitExec(serializedExec("old-gap", 2200, 2800)))
	require.NoError(t, tl.EmitExec(serializedExec("old-burst", 1200, 1800)))
	require.NoError(t, tl.EmitExec(serializedExec("across-the-hole", 5000, 19000)))
	require.NoError(t, tl.EmitExec(serializedExec("new-gap", 21500, 22500)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{
		"unknown", // a gap that WAS proven, before the hole: no longer provable
		"true",    // still positive evidence; the hole does not erase a burst
		"unknown", // squarely inside the hole
		"false",   // after the restart, between two known bursts
	}, states(snap))
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// Process isolation.

// Windows are per-process, and the PID is IN the key rather than in a check
// performed elsewhere (issue #52's discipline). One process bursting must
// never mark another process's executions perturbed, and must never let
// another process's executions read "false" either.
func TestSerializationWindowsNeverCrossProcesses(t *testing.T) {
	tl := tierATimeline()
	const other = uint32(777)
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	emitBurst(t, tl, uint64(tierAPID), 4000, 5000)

	mine := serializedExec("mine", 1200, 1800)
	theirs := GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, PID: other, Value: "theirs"},
		KernelName:  "k_theirs",
		StartNs:     1200,
		EndNs:       1800,
	}
	// An execution that does not name its process at all cannot be placed
	// against anybody's windows.
	anonymous := GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "anon"},
		KernelName:  "k_anon",
		StartNs:     2500,
		EndNs:       3500,
	}
	require.NoError(t, tl.EmitExec(mine))
	require.NoError(t, tl.EmitExec(theirs))
	require.NoError(t, tl.EmitExec(anonymous))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"true", "unknown", "unknown"}, states(snap))
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// The store's own bounds, and the direction they may move an answer.

// Eviction drops the OLDEST bursts, which moves the coverage start forward.
// That can only turn "false" into "unknown" — never the reverse — and this
// asserts the direction rather than trusting the comment.
func TestSerializationEvictionDegradesTowardsUnknownNeverTowardsFalse(t *testing.T) {
	tl := NewTimeline(TimelineConfig{PCSampling: PCSamplingSerialized, MaxSamplingWindowsPerPID: 4})
	// The gap between bursts 1 and 2 is provable while both are held.
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	emitBurst(t, tl, uint64(tierAPID), 3000, 4000)
	require.NoError(t, tl.EmitExec(serializedExec("early-gap", 2200, 2800)))
	early := tl.Snapshot()
	require.Equal(t, []string{"false"}, states(early))

	// Six more bursts push the first two out of a four-entry store.
	for i := uint64(0); i < 6; i++ {
		emitBurst(t, tl, uint64(tierAPID), 10000+i*2000, 11000+i*2000)
	}
	require.NoError(t, tl.EmitExec(serializedExec("early-gap-again", 2200, 2800)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"unknown"}, states(snap),
		"once the bursts either side of it are gone, the gap is no longer proven")
	assert.Positive(t, snap.Dropped.EvictedSamplingWindows,
		"an eviction that costs certainty must be counted, not silent")
	assertSumIdentity(t, snap)
}

// A process past the PID bound gets no window history, so its executions read
// "unknown". It must not inherit somebody else's.
func TestSerializationRefusedPIDReadsUnknown(t *testing.T) {
	tl := NewTimeline(TimelineConfig{PCSampling: PCSamplingSerialized, MaxSamplingWindowPIDs: 1})
	emitBurst(t, tl, 1, 1000, 2000)
	emitBurst(t, tl, 2, 1000, 2000) // refused: the store already holds one PID

	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, PID: 2, Value: "x"},
		StartNs:     1200, EndNs: 1800,
	}))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"unknown"}, states(snap))
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// Degenerate records.

// A window whose mode the producer left unset says nothing about whether
// kernels were serialized. It is not evidence of perturbation and it is not
// evidence of its absence, so the interval it covers is opaque.
func TestSerializationUnsetModeWindowIsOpaqueNotFalse(t *testing.T) {
	tl := tierATimeline()
	emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
	unset := burst(tierAPID, 3000, 4000)
	unset.Mode = SamplingModeUnset
	require.NoError(t, tl.EmitSamplingWindow(unset))
	emitBurst(t, tl, uint64(tierAPID), 5000, 6000)

	require.NoError(t, tl.EmitExec(serializedExec("in-the-unset", 3200, 3800)))
	require.NoError(t, tl.EmitExec(serializedExec("in-a-real-gap", 4200, 4800)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"unknown", "false"}, states(snap))
	assertSumIdentity(t, snap)
}

// A continuous-mode window says the producer was reporting but serialized
// nothing. It extends coverage and marks nothing perturbed.
func TestSerializationContinuousModeWindowMarksNothingPerturbed(t *testing.T) {
	tl := tierATimeline()
	cont := burst(tierAPID, 1000, 5000)
	cont.Mode = SamplingModeContinuous
	require.NoError(t, tl.EmitSamplingWindow(cont))

	require.NoError(t, tl.EmitExec(serializedExec("inside", 2000, 3000)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"false"}, states(snap))
	assertSumIdentity(t, snap)
}

// An inverted window is a producer contract violation. gpuabi refuses it at
// the wire boundary; the store refuses it again so a directly-constructed one
// (a replay fixture, a test) cannot put a negative interval into the evidence.
func TestSerializationInvertedWindowIsRefused(t *testing.T) {
	tl := tierATimeline()
	require.NoError(t, tl.EmitSamplingWindow(burst(tierAPID, 5000, 1000)))
	require.NoError(t, tl.EmitExec(serializedExec("x", 2000, 3000)))

	snap := tl.Snapshot()
	assert.Equal(t, []string{"unknown"}, states(snap))
	assert.Zero(t, snap.SamplingWindowsHeld)
	assertSumIdentity(t, snap)
}

// ---------------------------------------------------------------------------
// The invariant, stated as a test.

// Across every shape this file builds, "false" is reachable ONLY from positive
// evidence. This drives a matrix of window histories against a matrix of
// execution intervals and asserts the negative: wherever the store cannot
// prove containment in an unbroken span with no burst in it, the answer is not
// "false".
func TestSerializationFalseIsOnlyEverReachedFromPositiveEvidence(t *testing.T) {
	type histCase struct {
		name string
		emit func(*testing.T, *Timeline)
		// covered is the interval the history can speak for; executions
		// wholly inside it and clear of every burst may read "false".
		coverStart, coverEnd uint64
		bursts               [][2]uint64
		// anyFalse says whether this history leaves a provable gap at all. It
		// is false for "no windows" (nothing is provable) and for a single
		// burst whose extent IS the whole covered span (there is no gap
		// inside it), and those two are exactly the shapes where a "false"
		// appearing would be the defect.
		anyFalse bool
	}
	cases := []histCase{
		{name: "no windows at all", emit: func(*testing.T, *Timeline) {}},
		{
			name:       "one closed burst",
			emit:       func(t *testing.T, tl *Timeline) { emitBurst(t, tl, uint64(tierAPID), 1000, 2000) },
			coverStart: 1000, coverEnd: 2000,
			bursts: [][2]uint64{{1000, 2000}},
		},
		{
			name: "two closed bursts",
			emit: func(t *testing.T, tl *Timeline) {
				emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
				emitBurst(t, tl, uint64(tierAPID), 4000, 5000)
			},
			coverStart: 1000, coverEnd: 5000,
			bursts:   [][2]uint64{{1000, 2000}, {4000, 5000}},
			anyFalse: true,
		},
		{
			name: "a burst that never closed",
			emit: func(t *testing.T, tl *Timeline) {
				emitBurst(t, tl, uint64(tierAPID), 1000, 2000)
				require.NoError(t, tl.EmitSamplingWindow(burst(tierAPID, 4000, 0)))
			},
			coverStart: 1000, coverEnd: 4000,
			bursts:   [][2]uint64{{1000, 2000}},
			anyFalse: true,
		},
	}

	for _, hc := range cases {
		t.Run(hc.name, func(t *testing.T) {
			tl := tierATimeline()
			hc.emit(t, tl)
			for s := uint64(0); s <= 8000; s += 100 {
				for _, d := range []uint64{0, 150, 900, 3000} {
					require.NoError(t, tl.EmitExec(
						serializedExec(fmt.Sprintf("e_%d_%d", s, d), s, s+d)))
				}
			}
			snap := tl.Snapshot()
			assertSumIdentity(t, snap)
			require.NotEmpty(t, snap.Executions)

			var falses int
			for _, v := range snap.Executions {
				if v.Serialized != SerializationNotSerialized {
					continue
				}
				falses++
				s, e := v.Exec.StartNs, v.Exec.EndNs
				require.GreaterOrEqual(t, s, hc.coverStart,
					`"false" outside the covered span: [%d,%d]`, s, e)
				require.LessOrEqual(t, e, hc.coverEnd,
					`"false" outside the covered span: [%d,%d]`, s, e)
				for _, b := range hc.bursts {
					require.False(t, e >= b[0] && s <= b[1],
						`"false" for an execution [%d,%d] overlapping the burst [%d,%d]`,
						s, e, b[0], b[1])
				}
			}
			if hc.anyFalse {
				assert.Positive(t, falses,
					"the tier has to be able to prove SOMETHING, or it discloses nothing usable")
			} else {
				assert.Zero(t, falses,
					`this history proves no gap, so nothing may read "false"`)
			}
		})
	}
}
