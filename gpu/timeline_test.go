package gpu

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execFor(value string, startNs, endNs uint64) GPUKernelExec {
	return GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: value},
		KernelName:  "k_" + value,
		StartNs:     startNs,
		EndNs:       endNs,
	}
}

func TestTimelineJoinsExactByCorrelationID(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.NotNil(t, snap.Executions[0].Launch)
	assert.Equal(t, JoinExact, snap.Executions[0].Join)
	assert.False(t, snap.Executions[0].Heuristic, "an exact join must never be marked heuristic")
	assert.Equal(t, uint64(1), snap.JoinStats.ExactExecutionJoinCount)
}

func TestTimelineReportsUnmatchedExecution(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(execFor("ghost", 20, 30)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch, "an unmatched execution must not invent a launch")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
}

func TestTimelineAttachesPCSamplesByCorrelation(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		PCOffset:    0x1a40, StallReason: "long_scoreboard", Count: 5, TimeNs: 25,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.Len(t, snap.Executions[0].PCSamples, 1)
	assert.Equal(t, uint64(0x1a40), snap.Executions[0].PCSamples[0].PCOffset)
}

func TestTimelineDegradesWhenLaunchEvicted(t *testing.T) {
	// A PC sample whose launch has aged out must become unattributed, never
	// mis-attributed to a different launch.
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 1}})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitLaunch(launch("b", 20))) // evicts "a"
	require.NoError(t, tl.EmitExec(execFor("a", 30, 40)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch, "an evicted launch must not be replaced by another")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
	assert.Equal(t, uint64(1), snap.LaunchCache.EvictedCapacity,
		"the eviction that caused the miss must be visible in the snapshot")
}

func TestTimelineSnapshotIsBoundedUnderLoad(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 64}})
	for i := 0; i < 50000; i++ {
		// Unique correlation per launch. Reusing one ID would make every Put a
		// replacement, leaving Live at 1 and passing this assertion vacuously.
		require.NoError(t, tl.EmitLaunch(launch(strconv.Itoa(i), uint64(i))))
	}
	snap := tl.Snapshot()
	assert.Equal(t, 64, snap.LaunchCache.Live, "cache must fill to capacity and hold there")
	assert.Greater(t, snap.LaunchCache.EvictedCapacity, uint64(0), "evictions must be counted")
}

// TestTimelineBoundsExecutionsAndCountsEviction is not one of the brief's five
// tests. It targets the bounded exec ring the brief's Structure section calls
// for ("append to the bounded exec slice, evicting oldest and counting when
// over capacity") but that none of the five given tests exercise. Each exec
// carries a unique correlation ID, so this catches an implementation that (a)
// never evicts and grows unbounded, (b) evicts but forgets to count it, or
// (c) evicts the wrong (non-oldest) entry.
func TestTimelineBoundsExecutionsAndCountsEviction(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 4}})
	for i := 0; i < 10; i++ {
		require.NoError(t, tl.EmitExec(execFor(strconv.Itoa(i), uint64(i*10), uint64(i*10+5))))
	}

	snap := tl.Snapshot()
	assert.Len(t, snap.Executions, 4, "exec storage must stay bounded under load")
	assert.Equal(t, uint64(6), snap.Dropped.EvictedExecutions,
		"every evicted execution must be counted, not silently dropped")
	for _, ev := range snap.Executions {
		assert.NotEqual(t, "k_0", ev.Exec.KernelName, "the oldest execution should have been evicted, not retained")
	}
}

// TestTimelineConcurrentIngestionIsRaceFree exercises the mutex guard the
// brief calls for directly: concurrent Emit* calls across all five EventSink
// methods, plus concurrent Snapshot reads, on one shared Timeline. The
// existing single-goroutine tests would pass -race even with the lock
// removed entirely - this is the one that would actually catch it. Not a
// correctness assertion (interleaving is nondeterministic); the point is
// -race finding nothing while every ingress path is hit concurrently.
func TestTimelineConcurrentIngestionIsRaceFree(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 128}})

	const writers = 4
	const opsPerGoroutine = 500

	var wg sync.WaitGroup
	wg.Add(writers*5 + 1)

	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := strconv.Itoa(w) + "-" + strconv.Itoa(i)
				require.NoError(t, tl.EmitLaunch(launch(id, uint64(i+1))))
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := strconv.Itoa(w) + "-" + strconv.Itoa(i)
				require.NoError(t, tl.EmitExec(execFor(id, uint64(i+1), uint64(i+2))))
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := strconv.Itoa(w) + "-" + strconv.Itoa(i)
				require.NoError(t, tl.EmitPCSample(GPUPCSample{
					Correlation: CorrelationID{Backend: BackendCUPTI, Value: id},
					TimeNs:      uint64(i + 1),
				}))
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				require.NoError(t, tl.EmitModule(GPUModule{LoadedNs: uint64(i + 1)}))
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				require.NoError(t, tl.EmitEvent(GPUTimelineEvent{TimeNs: uint64(i + 1)}))
			}
		}(w)
	}
	go func() {
		defer wg.Done()
		for i := 0; i < opsPerGoroutine; i++ {
			_ = tl.Snapshot()
		}
	}()
	wg.Wait()
}

// TestTimelinePendingSamplesDoNotAccumulateAcrossSnapshots is the regression
// test for review Critical 1: EmitPCSample only ever appended to t.pending,
// and nothing ever removed an entry once its exec attached it - not even
// samples that were successfully joined - so the map grew with all-time
// sample count regardless of correctness. Reproduced pre-fix (with the
// reviewer's exact repro: launch cache capacity 8, 5000 fully-matched
// samples): t.pending held all 5000 entries.
//
// This deliberately uses the default (large) pending capacity rather than a
// small one: with a small pendingCap, the orphan-eviction path added
// alongside this fix would mask the bug on its own (evicting down to
// pendingCap regardless of whether attach-consumption works), which is
// exactly the kind of test that would pass whether or not the real fix is
// present. With 1000 samples and a ~65536 pending capacity, orphan eviction
// never fires (asserted via EvictedPendingSamples == 0 below), so the only
// thing that can be keeping t.pending small is consumption at attach time.
// 1000 rather than the reviewer's 5000 to keep this fast under -race, where
// each Snapshot call's O(live launches) queue-grouping makes the whole loop
// O(n^2) - the growth pattern that would expose the bug is identical either
// way; only the iteration count changes.
//
// Each iteration emits a launch, its exec, and a PC sample correlating to
// that same exec, then immediately snapshots - the exec is still live in the
// ring at that point, so its sample must be attached and consumed on the
// spot. Mutation this catches: removing the `delete(t.pending,
// exec.Correlation)` in Snapshot (or reverting to a map-copy that doesn't
// consume) makes len(tl.pending) grow to 1000 instead of staying near 0.
func TestTimelinePendingSamplesDoNotAccumulateAcrossSnapshots(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	var lastDropped uint64
	for i := 0; i < 1000; i++ {
		id := strconv.Itoa(i)
		require.NoError(t, tl.EmitLaunch(launch(id, uint64(i))))
		require.NoError(t, tl.EmitExec(execFor(id, uint64(i), uint64(i+1))))
		require.NoError(t, tl.EmitPCSample(GPUPCSample{
			Correlation: CorrelationID{Backend: BackendCUPTI, Value: id},
			TimeNs:      uint64(i),
		}))
		snap := tl.Snapshot()
		lastDropped = snap.Dropped.EvictedPendingSamples
	}
	require.Equal(t, uint64(0), lastDropped,
		"orphan eviction must not have fired - this test isolates attach-time consumption, not the capacity bound")
	assert.LessOrEqual(t, len(tl.pending), 10,
		"matched samples must be consumed at attach time, not retained forever")
}

// TestTimelineBoundsModulesAndCountsEviction is the regression test for
// review Important 4: EmitModule appended to an uncapped slice with no
// eviction and no counter. Mirrors
// TestTimelineBoundsExecutionsAndCountsEviction. Mutation this catches:
// module storage growing unbounded, an eviction not counted in
// Dropped.EvictedModules, or eviction of the wrong (non-oldest) entry.
func TestTimelineBoundsModulesAndCountsEviction(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 4}})
	for i := 0; i < 10; i++ {
		require.NoError(t, tl.EmitModule(GPUModule{Ref: ModuleRef{CRC: uint64(i)}, LoadedNs: uint64(i)}))
	}

	snap := tl.Snapshot()
	assert.Len(t, snap.Modules, 4, "module storage must stay bounded under load")
	assert.Equal(t, uint64(6), snap.Dropped.EvictedModules,
		"every evicted module must be counted, not silently dropped")
	for _, m := range snap.Modules {
		assert.NotEqual(t, uint64(0), m.Ref.CRC, "the oldest module should have been evicted, not retained")
	}
}

// TestTimelinePendingOrphansAreBoundedAndCounted is the second regression
// test for review Critical 1: it isolates the orphan-eviction path (samples
// whose exec never arrives) from TestTimelinePendingSamplesDoNotAccumulateAcrossSnapshots's
// consume-on-attach path above. Every correlation ID here is unique and
// never matched to an exec - a repeated ID would exercise append growth of
// one group's slice, not the distinct-correlation capacity bound this test
// targets. Mutation this catches: pending growing past pendingCap, or an
// eviction not reaching Dropped.EvictedPendingSamples.
func TestTimelinePendingOrphansAreBoundedAndCounted(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 4}})
	for i := 0; i < 20; i++ {
		id := strconv.Itoa(i)
		require.NoError(t, tl.EmitPCSample(GPUPCSample{
			Correlation: CorrelationID{Backend: BackendCUPTI, Value: id},
			TimeNs:      uint64(i),
		}))
	}

	snap := tl.Snapshot()
	assert.LessOrEqual(t, len(tl.pending), 4, "orphaned pending samples must stay bounded")
	assert.Greater(t, snap.Dropped.EvictedPendingSamples, uint64(0),
		"evicted orphan samples must be counted, not silently dropped")
}

// TestTimelinePendingEvictionSurvivesCorrelationIDReuse is the regression
// test for round-2 review: pendingOrder previously carried no sequence
// discriminator, and eviction treated mere presence in t.pending as
// liveness. Sequence of events that broke it: a correlation's samples are
// consumed by Snapshot (its map entry deleted, its old pendingOrder position
// left behind); the same correlation ID is then reused by a fresh
// EmitPCSample; when the stale old position reaches the eviction head, it
// finds the *new* generation present under that key and deletes it -
// evicting the freshest entry while genuinely older orphans survive. The
// trigger (correlation-ID reuse within a process run) is foreseeable for
// this phase: 32-bit vendor correlation IDs wrap around over a long-running
// session.
//
// This reproduces the coordinator's exact scenario: consume "x", add two
// genuinely older orphans "w" and "q", reuse "x", then apply eviction
// pressure. Mutation this catches: reverting evictPendingLocked's
// `cur.seq != e.seq` check back to a bare presence check (`!live`) -
// without the sequence check, "x"'s stale pre-consumption position matches
// its reused entry by presence alone and evicts it, while "w" (genuinely
// oldest) survives - the inverse of correct behavior.
func TestTimelinePendingEvictionSurvivesCorrelationIDReuse(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 3}})
	xID := CorrelationID{Backend: BackendCUPTI, Value: "x"}
	wID := CorrelationID{Backend: BackendCUPTI, Value: "w"}
	qID := CorrelationID{Backend: BackendCUPTI, Value: "q"}

	// 1. Insert "x" and consume it via a matching exec + Snapshot: its map
	// entry is deleted, but its pendingOrder position (seq 1) remains.
	require.NoError(t, tl.EmitLaunch(launch("x", 1)))
	require.NoError(t, tl.EmitExec(execFor("x", 2, 3)))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: xID, TimeNs: 1}))
	snap := tl.Snapshot()
	require.Len(t, snap.Executions[0].PCSamples, 1, "sanity: x's sample was attached and consumed")

	// 2. Two genuinely older orphans that are never consumed.
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: wID, TimeNs: 2}))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: qID, TimeNs: 3}))

	// 3. Reuse "x"'s correlation ID with a fresh sample - a new generation,
	// the freshest entry in the store, and must survive eviction.
	require.NoError(t, tl.EmitPCSample(GPUPCSample{Correlation: xID, TimeNs: 4}))

	// 4. Apply eviction pressure: pendingCap is 3 (from LaunchCache.Capacity
	// above); pending now holds w, q, x (3 distinct live correlations) plus
	// x's stale pre-consumption order slot at the head. One more distinct
	// correlation pushes the live count over pendingCap and triggers
	// eviction.
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "extra"}, TimeNs: 5,
	}))

	_, xSurvived := tl.pending[xID]
	_, wSurvived := tl.pending[wID]
	_, qSurvived := tl.pending[qID]
	assert.True(t, xSurvived,
		"the freshest re-inserted entry must survive eviction, not be mistaken for its own stale pre-consumption slot")
	assert.False(t, wSurvived, "the genuinely oldest orphan must be the one evicted")
	assert.True(t, qSurvived, "eviction must stop once back at capacity, not over-evict")
}

// TestTimelineEventFieldsDoNotAliasCaller is the regression test for review
// Important 5: GPUTimelineEvent carries Device/Queue/Attributes by pointer
// or map, and Timeline used to store the struct by value, aliasing whatever
// the backend passed in. This simulates a backend reusing its event buffer
// immediately after Emit returns. Mutation this catches: removing
// cloneTimelineEvent (or cloning only some of the three fields) from
// EmitEvent.
func TestTimelineEventFieldsDoNotAliasCaller(t *testing.T) {
	device := &GPUDeviceRef{DeviceID: "dev-0"}
	attrs := map[string]string{"k": "v"}
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitEvent(GPUTimelineEvent{
		TimeNs:     10,
		Device:     device,
		Attributes: attrs,
	}))

	// Simulate the backend reusing its own buffers after Emit returns.
	device.DeviceID = "corrupted"
	attrs["k"] = "corrupted"

	snap := tl.Snapshot()
	require.Len(t, snap.Events, 1)
	require.NotNil(t, snap.Events[0].Device)
	assert.Equal(t, "dev-0", snap.Events[0].Device.DeviceID,
		"the stored event must not alias the caller's Device pointer")
	assert.Equal(t, "v", snap.Events[0].Attributes["k"],
		"the stored event must not alias the caller's Attributes map")
}

// TestTimelineHeuristicJoinsSingleCandidate is the first of four regression
// tests for review Critical 3: no prior test ever drove the heuristic join
// to an actual hit, so Join, Heuristic and Ambiguous on that path were
// entirely unverified.
//
// The launch's correlation deliberately differs from the exec's, so the
// exact lookup is guaranteed to miss and only the heuristic (same queue,
// compatible kernel name, launch precedes exec) can produce the match.
// Mutation this catches: the heuristic path never running at all, or
// running but not setting Join/Heuristic, or attaching no launch, or the
// wrong launch.
func TestTimelineHeuristicJoinsSingleCandidate(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "launch-a"},
		KernelName:  "k_x",
		TimeNs:      10,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 10},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "exec-a"},
		KernelName:  "k_x",
		StartNs:     20, EndNs: 30,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	ev := snap.Executions[0]
	require.NotNil(t, ev.Launch, "the single qualifying candidate must be attached")
	assert.Equal(t, JoinHeuristic, ev.Join)
	assert.True(t, ev.Heuristic)
	assert.False(t, ev.Ambiguous, "exactly one candidate must not be marked ambiguous")
	assert.Equal(t, "launch-a", ev.Launch.Correlation.Value)
	assert.Equal(t, uint64(1), snap.JoinStats.HeuristicExecutionJoinCount)
}

// TestTimelineHeuristicMarksAmbiguousAndPicksMostRecent is the second
// Critical-3 regression test. Two launches both qualify (same queue,
// compatible kernel name, both precede the exec); combined with the
// single-candidate test above (which asserts Ambiguous == false), this pins
// the boundary at "more than one candidate" rather than "one or more".
// Mutation this catches: `candidateCount > 1` weakened to `>= 1` (caught by
// the single-candidate test, not this one, since both would report
// ambiguous here); `l.TimeNs > best.TimeNs` flipped to `<`, which would pick
// the oldest ("older") candidate instead of the most recent ("newer").
func TestTimelineHeuristicMarksAmbiguousAndPicksMostRecent(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "older"},
		KernelName:  "k_x",
		TimeNs:      10,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 10},
	}))
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "newer"},
		KernelName:  "k_x",
		TimeNs:      15,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 15},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "exec-a"},
		KernelName:  "k_x",
		StartNs:     20, EndNs: 30,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	ev := snap.Executions[0]
	require.NotNil(t, ev.Launch)
	assert.True(t, ev.Ambiguous, "two qualifying candidates must be marked ambiguous")
	assert.Equal(t, "newer", ev.Launch.Correlation.Value,
		"the most recent preceding launch must win, not the oldest")
	assert.Equal(t, uint64(1), snap.JoinStats.AmbiguousHeuristicMatchCount)
}

// TestTimelineHeuristicRejectsLaunchAfterExecStart is the third Critical-3
// regression test. The only candidate launch starts after the exec it might
// otherwise match, which is causally impossible - a launch cannot produce an
// execution that started before it did. Mutation this catches:
// `l.TimeNs > exec.StartNs` (reject future launches) flipped to `<` (reject
// past launches, accept future ones) at the ordering check in
// findLaunchHeuristic.
func TestTimelineHeuristicRejectsLaunchAfterExecStart(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "future"},
		KernelName:  "k_x",
		TimeNs:      100, // after the exec's start
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 100},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "exec-a"},
		KernelName:  "k_x",
		StartNs:     20, EndNs: 30,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch,
		"a launch that starts after the exec cannot have produced it")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
}

// TestTimelineHeuristicRejectsWrongQueueAndKernelName is the fourth
// Critical-3 regression test: two decoys, one filtered by queue, one by
// kernel name, neither should attach. Mutation this catches: the queue
// filter (via groupLaunchesByQueue/queueKeyOf) or the kernel-name filter
// (launchKernelNamesCompatible) being dropped from findLaunchHeuristic -
// either omission would let one of these two decoys match.
func TestTimelineHeuristicRejectsWrongQueueAndKernelName(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "wrong-queue"},
		KernelName:  "k_x",
		TimeNs:      10,
		Queue:       GPUQueueRef{Backend: BackendCUPTI, QueueID: "other-queue"},
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 10},
	}))
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "wrong-name"},
		KernelName:  "k_other",
		TimeNs:      10,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: 10},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		// Queue left zero-value: "wrong-queue"'s non-zero QueueID must not match.
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "exec-a"},
		KernelName:  "k_x",
		StartNs:     20, EndNs: 30,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch,
		"neither the wrong-queue nor the wrong-kernel-name launch should qualify")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
}

// BenchmarkTimelineSnapshotAllMisses is the regression benchmark for review
// Critical 2: findLaunchHeuristic used to call LaunchCache.Entries() (a
// fresh allocation) inside the per-execution loop, so an all-miss Snapshot
// cost grew as O(misses x capacity) with an allocation on every miss.
// Measured before the fix, this exact scenario (capacity 2000, 500 missing
// execs): ~66.8ms/op, ~192.8MB/op, 510 allocs/op. See the fix-round report
// for the after numbers.
func BenchmarkTimelineSnapshotAllMisses(b *testing.B) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 2000}})
	for i := 0; i < 2000; i++ {
		if err := tl.EmitLaunch(launch(strconv.Itoa(i), uint64(i))); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 500; i++ {
		// Correlation and kernel name never match any launch: every exec
		// takes the full exact-miss + heuristic-scan path with zero result,
		// the worst case for the candidate scan.
		if err := tl.EmitExec(execFor("miss-"+strconv.Itoa(i), uint64(3000+i), uint64(3000+i+5))); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = tl.Snapshot()
	}
}

// TestTimelineHeuristicScalesWithSingleQueueSingleKernelName is the
// complexity regression test for round-3 review. Round 2's fix
// (BenchmarkTimelineSnapshotAllMisses above) removed the per-miss
// allocation but left the heuristic's *time* complexity at
// O(misses x candidates-in-group): grouping candidates by queue alone meant
// every launch on one queue landed in a single group, and each miss still
// scanned that whole group linearly. A single queue is not a pathological
// input - it is what a workload submitting on one stream looks like - and
// Task 6's BenchmarkSnapshotAtScale (1M launches, capacity 65536) measured
// this exact shape at ~30s per Snapshot before the binary-search fix in this
// commit, against a sub-second gate.
//
// The four heuristic tests above only check correctness (right launch
// attached, right Ambiguous flag) and cannot catch this: a
// reverted-to-linear-scan findLaunchHeuristic still returns the right
// answer, just slowly, at any scale small enough for those tests to finish
// in milliseconds either way. This test pins the complexity directly, via a
// generous wall-clock bound at a scale deliberately too large for a linear
// scan to hide in: one queue, one kernel name (so every candidate lands in
// the same group), 50,000 live launches, 10,000 exec misses that all share
// that one group. That is 5*10^8 candidate comparisons for a linear scan -
// which this exact setup measured in the tens of seconds against the
// pre-fix code (see the round-3 report) - versus roughly
// 10,000 * log2(50,000) ~= 170,000 comparisons for a binary search, which
// completes in low milliseconds.
//
// Mutation this catches: findLaunchHeuristic reverted from sort.Search back
// to a linear scan over the candidate group (or buildHeuristicCandidateIndex
// grouping by queue alone again, without kernel name, or without sorting).
func TestTimelineHeuristicScalesWithSingleQueueSingleKernelName(t *testing.T) {
	const candidates = 50_000
	const misses = 10_000

	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: candidates}})
	for i := 0; i < candidates; i++ {
		l := launch(strconv.Itoa(i), uint64(i))
		l.KernelName = "hot_kernel" // every launch shares one (queue, name) group
		require.NoError(t, tl.EmitLaunch(l))
	}
	for i := 0; i < misses; i++ {
		e := execFor("ghost-"+strconv.Itoa(i), uint64(i), uint64(i))
		e.KernelName = "hot_kernel" // same group; correlation never matches -> guaranteed exact-match miss
		require.NoError(t, tl.EmitExec(e))
	}

	done := make(chan struct{})
	var elapsed time.Duration
	go func() {
		start := time.Now()
		_ = tl.Snapshot()
		elapsed = time.Since(start)
		close(done)
	}()

	select {
	case <-done:
		assert.Less(t, elapsed, 5*time.Second,
			"a single-queue, single-kernel-name workload must not make the heuristic join scan linearly - "+
				"this exact shape measured tens of seconds before the binary-search fix")
	case <-time.After(25 * time.Second):
		t.Fatal("Snapshot did not return within 25s - the heuristic join is scanning its candidate group, not binary-searching it")
	}
}
