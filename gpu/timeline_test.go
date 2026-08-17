package gpu

import (
	"strconv"
	"sync"
	"testing"

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
