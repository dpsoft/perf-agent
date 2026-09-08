package gpu

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthySnapshot is 512 executions all joined exactly, 256 launches all
// matched, nothing evicted - what a clean CUPTI run looks like.
func healthySnapshot() Snapshot {
	return Snapshot{
		Executions: make([]ExecutionView, 512),
		JoinStats: JoinStats{
			LaunchCount:             256,
			MatchedLaunchCount:      256,
			ExactExecutionJoinCount: 512,
		},
		LaunchCache: LaunchCacheStats{Live: 256},
		// PC sampling off, so every execution is "false" unconditionally and
		// correctly: nothing was ever serialized. Set here rather than left
		// zero because the zero value of SerializationState is "unknown" and
		// the three counters must sum to len(Executions) — the same identity
		// the join outcomes carry, checked in the same place.
		ExecutionsNotSerialized: 512,
	}
}

// anomalousSnapshot is the same run gone wrong in every way the counters
// can express, so one rendering exercises every branch.
func anomalousSnapshot() Snapshot {
	return Snapshot{
		Executions: make([]ExecutionView, 512),
		JoinStats: JoinStats{
			LaunchCount:                  260,
			MatchedLaunchCount:           240,
			UnmatchedLaunchCount:         20,
			ExactExecutionJoinCount:      470,
			HeuristicExecutionJoinCount:  22,
			AmbiguousHeuristicMatchCount: 4,
			UnmatchedExecutionCount:      20,
			OutOfWindowDropCount:         7,
		},
		LaunchCache: LaunchCacheStats{
			Live:            250,
			EvictedCapacity: 47,
			// The subset that actually lost an attribution. Set here so the
			// many-anomalies fixture still exercises the capacity line: it now
			// fires on the loss rather than on the eviction total (#137).
			EvictedCapacityUnjoined: 47,
			EvictedHorizon:          3,
			Replaced:                2,
			AnomalousTimestamp:      1,
		},
		Dropped: TimelineDropStats{
			EvictedExecutions:     9,
			EvictedEvents:         11,
			EvictedModules:        1,
			EvictedPendingSamples: 128,
		},
		SinkStats: SinkStats{
			PCSamples: EventKindStats{Accepted: 900, DroppedFull: 60, DroppedInvalid: 4},
		},
		AttributedPCSamples: 900,
		PendingSamples:      12,
		PendingCorrelations: 5,

		// Tier A gone wrong in all three ways at once: bursts perturbed some
		// executions, an unbroken history proved others were untouched, and a
		// window that never closed leaves the rest unplaceable.
		//
		// The tier is set, and it has to be: the three counters below are
		// reachable ONLY under PCSamplingSerialized (the other two tiers
		// answer "false" unconditionally and never consult the window store),
		// so a fixture that carried them with the tier off would be a run that
		// cannot happen — and would quietly stop exercising the standing
		// warning that a real one carries.
		PCSampling:                     PCSamplingSerialized,
		SamplingWindowsReceived:        41,
		SamplingWindowsHeld:            21,
		SamplingWindowsOpen:            1,
		ExecutionsSerialized:           120,
		ExecutionsNotSerialized:        300,
		ExecutionsSerializationUnknown: 92,
	}
}

func TestJoinHealthHealthyRunIsOneShortLine(t *testing.T) {
	lines := JoinHealth(healthySnapshot())

	require.Len(t, lines, 1, "a clean run must not print anything a reader learns to skip")
	assert.Equal(t,
		"gpu join: 512 executions, all exact; 256 launches, all matched; cache 256 live; no anomalies",
		lines[0])
}

func TestJoinHealthAnomaliesEachGetTheirOwnLine(t *testing.T) {
	lines := JoinHealth(anomalousSnapshot())

	require.Greater(t, len(lines), 1)
	warnings := 0
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, PCSamplingWarningPrefix+": ") {
			warnings++
			continue
		}
		assert.True(t, strings.HasPrefix(l, joinAnomalyPrefix+": "), "line %q", l)
		assert.Contains(t, l, " — ", "every anomaly says what the number means when it is bad")
	}
	assert.Equal(t, len(PCSamplingStandingWarning(PCSamplingSerialized)), warnings,
		"a Tier A snapshot carries its whole standing warning, every render")

	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"20 of 512 executions unmatched",
		"22 of 512 executions joined heuristically",
		"4 heuristic joins flagged ambiguous",
		"7 of the unmatched executions had a candidate launch",
		"launch cache evicted 47 launches at capacity BEFORE",
		"launch cache evicted 3 launches past HorizonNs",
		"launch cache replaced 2 live entries",
		"1 launch carried an out-of-range timestamp",
		"128 PC samples evicted",
		"9 executions evicted from the timeline ring",
		"11 timeline events evicted",
		"1 module evicted before this snapshot — kernels from evicted modules resolve",
		"sink dropped 64 PC samples at admission",
		"120 of 512 executions ran while GPU kernels were SERIALIZED",
		"92 of 512 executions cannot be said to have run unperturbed",
		"1 sampling window still open",
		"serialization 120 true, 300 false, 92 unknown over 21 bursts",
	} {
		assert.Contains(t, joined, want)
	}
}

// The summary's trailing counts are the only derived figures here; they must
// never read green when things are worst, and together they must account for
// EVERY line below the summary. A standing warning line that no count covered
// would be a line the summary implicitly denies exists.
func TestJoinHealthSummaryCountMatchesTheLinesBelowIt(t *testing.T) {
	tierA := healthySnapshot()
	tierA.PCSampling = PCSamplingSerialized
	for name, snap := range map[string]Snapshot{
		"healthy":          healthySnapshot(),
		"anomalous":        anomalousSnapshot(),
		"empty":            {},
		"tier A, no fault": tierA,
	} {
		t.Run(name, func(t *testing.T) {
			lines := JoinHealth(snap)

			warnings := 0
			for _, l := range lines[1:] {
				if strings.HasPrefix(l, PCSamplingWarningPrefix+": ") {
					warnings++
				}
			}
			anomalies := len(lines) - 1 - warnings

			switch warnings {
			case 0:
				assert.NotContains(t, lines[0], "standing warning")
			case 1:
				assert.Contains(t, lines[0], "; 1 standing warning line")
			default:
				assert.Contains(t, lines[0], "; "+strconv.Itoa(warnings)+" standing warning lines")
			}

			switch anomalies {
			case 0:
				assert.Contains(t, lines[0], "; no anomalies")
			case 1:
				assert.Contains(t, lines[0], "; 1 anomaly")
			default:
				assert.Contains(t, lines[0], "; "+strconv.Itoa(anomalies)+" anomalies")
			}
		})
	}
}

// The warning STANDS. It is rendered on a Tier A run in which nothing at all
// went wrong — no perturbed execution yet, no unknown, no window even — and
// that is the case it exists for: a burst-free interval, a graph refusal that
// stopped bursts, or simply the first snapshot of a run. A disclosure that
// appeared only once some counter moved would be absent from exactly the
// profiles whose readers had no other way to learn the tier was on.
func TestTheTierAWarningStandsOnAnOtherwisePerfectRun(t *testing.T) {
	snap := healthySnapshot()
	snap.PCSampling = PCSamplingSerialized
	// Nothing is amiss: every join exact, no window, no serialized execution.
	snap.ExecutionsNotSerialized = uint64(len(snap.Executions))

	lines := JoinHealth(snap)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, lines[0], "; pc sampling serialized")
	assert.Contains(t, lines[0], "; no anomalies")
	assert.Contains(t, joined, "CARRY NO MARKING AT ALL")
	assert.Contains(t, joined, "CUDA GRAPHS")
	assert.Equal(t, PCSamplingStandingWarning(PCSamplingSerialized), lines[1:],
		"the warning is the whole warning, in order, immediately under the summary")
}

// And it is absent for the two tiers that do not perturb anything. A warning
// on every run is a warning readers learn to skip.
func TestNoStandingWarningWhenNothingIsPerturbed(t *testing.T) {
	for _, tier := range []PCSamplingTier{PCSamplingOff, PCSamplingContinuous} {
		t.Run(tier.String(), func(t *testing.T) {
			snap := healthySnapshot()
			snap.PCSampling = tier
			lines := JoinHealth(snap)
			require.Len(t, lines, 1)
			assert.NotContains(t, lines[0], "standing warning")
			if tier == PCSamplingOff {
				assert.NotContains(t, lines[0], "pc sampling")
			} else {
				assert.Contains(t, lines[0], "; pc sampling continuous")
			}
		})
	}
}

// Tier A selected and NOT ONE window record received: every execution is
// "unknown", and that is the worst available state of the disclosure rather
// than a quiet one. It must be raised, and the line must say that no window
// arrived at all rather than offering the ordinary lossy-transport causes.
func TestJoinHealthRaisesTierAWithNoWindowAtAll(t *testing.T) {
	snap := healthySnapshot()
	snap.PCSampling = PCSamplingSerialized
	snap.ExecutionsSerializationUnknown = uint64(len(snap.Executions))
	snap.ExecutionsNotSerialized = 0

	joined := strings.Join(JoinHealth(snap), "\n")
	assert.Contains(t, joined, "NOT ONE window record reached the agent")
	assert.Contains(t, joined, "MUST NOT be read")
}

// A snapshot with nothing in it is the degenerate worst case: every ratio a
// health figure could compute is 0/0. It must read as an anomaly, not as a
// clean run.
func TestJoinHealthEmptySnapshotIsAnAnomalyNotAllExact(t *testing.T) {
	lines := JoinHealth(Snapshot{})

	require.Len(t, lines, 2)
	assert.Equal(t, "gpu join: no executions; no launches; cache 0 live; 1 anomaly", lines[0])
	assert.Contains(t, lines[1], "no executions in this snapshot")
	assert.NotContains(t, lines[0], "all exact")
}

// A run whose executions all joined but whose cache was thrashing is the
// case the summary line alone would call fine; the eviction lines are what
// make it visible.
// A LOST launch is raised even when every join in this snapshot was exact.
//
// The original point of this test stands and is the reason it is not simply
// deleted: eviction health must not be inferred from the join outcomes of the
// snapshot in front of us. A launch evicted before its execution arrived is a
// loss whether or not the executions that DID arrive all joined perfectly --
// the execution that lost its launch is in a later snapshot, or was never
// counted here at all.
//
// What changed is which counter carries that meaning. It is now a property of
// the evicted ENTRIES (were they ever joined?) rather than the raw eviction
// total, which on any long run is dominated by entries that had already done
// their job. See TestABoundedCacheReachingItsBoundIsNotAnAnomaly.
func TestJoinHealthLostLaunchesAreRaisedEvenWhenEveryJoinWasExact(t *testing.T) {
	snap := healthySnapshot()
	snap.LaunchCache.EvictedCapacity = 47
	snap.LaunchCache.EvictedCapacityUnjoined = 47
	snap.LaunchCache.EvictedCapacityUnjoined = 47

	lines := JoinHealth(snap)

	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "512 executions, all exact")
	assert.Contains(t, lines[0], "; 1 anomaly")
	assert.Contains(t, lines[1], "launch cache evicted 47 launches at capacity BEFORE")
	assert.Contains(t, lines[1], "TimelineConfig.LaunchCache.Capacity")
}

// The regression this pins (issue #137): a bounded LRU reaching its bound is
// what the bound is FOR, and is not by itself a defect.
//
// LaunchCache.Get does not delete -- the cache serves repeated correlations --
// so a launch stays live long after its execution has joined, and capacity
// eviction reaches it eventually on any run with more than Capacity launches.
// Firing on the raw total made the alarm permanent on long runs: measured on
// an RTX 3090, a 20 s attach reported 45,172 capacity evictions while
// projecting 111,056 samples from ~110,851 launches and joining every
// execution in its final snapshot exactly.
//
// A permanent alarm is worse than a missing one. An operator who sees the same
// line every interval learns to skip it, and skips the next one with it.
func TestABoundedCacheReachingItsBoundIsNotAnAnomaly(t *testing.T) {
	snap := healthySnapshot()
	snap.LaunchCache.EvictedCapacity = 45172 // every one of them already joined
	snap.LaunchCache.EvictedCapacityUnjoined = 0

	lines := JoinHealth(snap)

	require.Len(t, lines, 1, "a healthy run must produce the summary line and nothing else")
	assert.Contains(t, lines[0], "no anomalies")
}

// And the total is still reported when there IS a loss, because the ratio is
// what tells an operator whether the cache is marginally or wildly too small.
func TestTheLossIsReportedAgainstTheTotalItCameFrom(t *testing.T) {
	snap := healthySnapshot()
	snap.LaunchCache.EvictedCapacity = 45172
	snap.LaunchCache.EvictedCapacityUnjoined = 12

	lines := JoinHealth(snap)

	require.Len(t, lines, 2)
	assert.Contains(t, lines[1], "12 launches")
	assert.Contains(t, lines[1], "45172 evicted in total",
		"without the denominator, 12 lost launches reads the same whether the cache "+
			"missed by a hair or by an order of magnitude")
}

// The summary line quotes len(snap.Executions) as its denominator so the
// breakdown beside it is checkable. Nothing checks it by eye, so the
// renderer checks it: a counter that has drifted from the executions
// actually present is exactly the condition an operator cannot spot.
func TestJoinHealthRaisesWhenJoinOutcomesDoNotSumToExecutions(t *testing.T) {
	snap := healthySnapshot()
	snap.JoinStats.ExactExecutionJoinCount = 500 // 12 executions unaccounted for

	lines := JoinHealth(snap)

	require.Len(t, lines, 2)
	assert.Equal(t,
		"gpu join ANOMALY: join outcomes sum to 500 but the snapshot holds 512 executions — "+
			"the join counters disagree with what is actually present; treat every figure below as unreliable",
		lines[1],
		"both figures belong in the message; the reader must not have to compute the discrepancy")
	assert.Contains(t, lines[0], "; 1 anomaly")
}

// It has to fire in the other direction too - counters over-reporting is the
// same defect and reads just as authoritative.
func TestJoinHealthRaisesWhenJoinOutcomesExceedExecutions(t *testing.T) {
	snap := healthySnapshot()
	snap.JoinStats.HeuristicExecutionJoinCount = 3 // 515 outcomes over 512 executions

	lines := JoinHealth(snap)

	require.Len(t, lines, 3)
	assert.Contains(t, lines[1], "join outcomes sum to 515 but the snapshot holds 512 executions")
	assert.Contains(t, lines[2], "3 of 512 executions joined heuristically",
		"the reconciliation line comes before the conditions it casts doubt on")
}

// The reconciliation check must not fire on snapshots that do add up, or it
// becomes the noise it exists to prevent.
func TestJoinHealthReconciliationStaysQuietWhenTheCountersAgree(t *testing.T) {
	for name, snap := range map[string]Snapshot{
		"healthy":   healthySnapshot(),
		"anomalous": anomalousSnapshot(),
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, strings.Join(JoinHealth(snap), "\n"), "join outcomes sum to")
		})
	}
}

// The two drivers snapshot a real Timeline, so the renderer has to hold up
// against a Snapshot this package actually produced, not only hand-built
// structs.
func TestJoinHealthAgainstARealTimelineSnapshot(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 100)
	require.NoError(t, tl.EmitLaunch(l))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: l.Correlation,
		KernelName:  l.KernelName,
		StartNs:     200,
		EndNs:       300,
	}))
	// An execution whose correlation never had a launch: the shape issue #36
	// turned from a wrong exact join into an honest miss.
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "ghost"},
		KernelName:  "k_ghost",
		StartNs:     400,
		EndNs:       500,
	}))

	lines := JoinHealth(tl.Snapshot())

	require.Len(t, lines, 2)
	assert.Equal(t,
		"gpu join: 2 executions (1 exact, 0 heuristic, 1 unmatched); 1 launch, all matched; cache 1 live; 1 anomaly",
		lines[0])
	assert.Contains(t, lines[1], "1 of 2 executions unmatched")
	assert.Contains(t, lines[1], FrameLaunchUnsampled)
}

// TestJoinHealthRenderedOutput prints both renderings verbatim so the text a
// human will actually read can be reviewed with `go test -run
// TestJoinHealthRenderedOutput -v ./gpu/`.
func TestJoinHealthRenderedOutput(t *testing.T) {
	for _, c := range []struct {
		name string
		snap Snapshot
	}{
		{"healthy", healthySnapshot()},
		{"anomalous", anomalousSnapshot()},
		{"empty", Snapshot{}},
		{"tier A, nothing wrong", func() Snapshot {
			s := healthySnapshot()
			s.PCSampling = PCSamplingSerialized
			s.ExecutionsNotSerialized = uint64(len(s.Executions))
			return s
		}()},
	} {
		t.Log(c.name + ":\n" + strings.Join(JoinHealth(c.snap), "\n"))
	}
}
