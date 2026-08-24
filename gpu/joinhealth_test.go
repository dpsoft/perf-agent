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
			Live:               250,
			EvictedCapacity:    47,
			EvictedHorizon:     3,
			Replaced:           2,
			AnomalousTimestamp: 1,
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
	for _, l := range lines[1:] {
		assert.True(t, strings.HasPrefix(l, joinAnomalyPrefix+": "), "line %q", l)
		assert.Contains(t, l, " — ", "every anomaly says what the number means when it is bad")
	}

	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"20 of 512 executions unmatched",
		"22 of 512 executions joined heuristically",
		"4 heuristic joins flagged ambiguous",
		"7 of the unmatched executions had a candidate launch",
		"launch cache evicted 47 launches at capacity",
		"launch cache evicted 3 launches past HorizonNs",
		"launch cache replaced 2 live entries",
		"1 launch carried an out-of-range timestamp",
		"128 PC samples evicted",
		"9 executions evicted from the timeline ring",
		"11 timeline events evicted",
		"1 module evicted before this snapshot — kernels from evicted modules resolve",
		"sink dropped 64 PC samples at admission",
	} {
		assert.Contains(t, joined, want)
	}
}

// The summary's anomaly count is the one derived figure here; it must never
// read green when things are worst.
func TestJoinHealthSummaryCountMatchesTheLinesBelowIt(t *testing.T) {
	for name, snap := range map[string]Snapshot{
		"healthy":   healthySnapshot(),
		"anomalous": anomalousSnapshot(),
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			lines := JoinHealth(snap)
			switch n := len(lines) - 1; n {
			case 0:
				assert.Contains(t, lines[0], "; no anomalies")
			case 1:
				assert.Contains(t, lines[0], "; 1 anomaly")
			default:
				assert.Contains(t, lines[0], "; "+strconv.Itoa(n)+" anomalies")
			}
		})
	}
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
func TestJoinHealthEvictionsAreRaisedEvenWhenEveryJoinWasExact(t *testing.T) {
	snap := healthySnapshot()
	snap.LaunchCache.EvictedCapacity = 47

	lines := JoinHealth(snap)

	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], "512 executions, all exact")
	assert.Contains(t, lines[0], "; 1 anomaly")
	assert.Contains(t, lines[1], "launch cache evicted 47 launches at capacity")
	assert.Contains(t, lines[1], "TimelineConfig.LaunchCache.Capacity")
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
	} {
		t.Log(c.name + ":\n" + strings.Join(JoinHealth(c.snap), "\n"))
	}
}
