package gpu

import (
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func launch(value string, timeNs uint64) GPUKernelLaunch {
	return GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: value},
		KernelName:  "k_" + value,
		TimeNs:      timeNs,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: timeNs},
	}
}

func TestLaunchCacheGetsWhatItPut(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	c.Put(launch("a", 10))

	got, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	require.True(t, ok)
	assert.Equal(t, "k_a", got.KernelName)
}

func TestLaunchCacheMissIsNotAnError(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "nope"})
	assert.False(t, ok, "a miss must be reported, not fabricated")
}

func TestLaunchCacheEvictsOldestOverCapacity(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 2})
	c.Put(launch("a", 10))
	c.Put(launch("b", 20))
	c.Put(launch("c", 30))

	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	assert.False(t, ok, "oldest entry must be evicted first")
	_, ok = c.Get(CorrelationID{Backend: BackendCUPTI, Value: "c"})
	assert.True(t, ok)

	assert.Equal(t, uint64(1), c.Stats().EvictedCapacity)
	assert.Equal(t, 2, c.Stats().Live)
}

func TestLaunchCacheEvictsBeyondHorizon(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 100, HorizonNs: 50})
	c.Put(launch("old", 10))
	c.Put(launch("new", 100)) // 100-10 = 90 > horizon 50

	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "old"})
	assert.False(t, ok, "entries older than the horizon must be evicted")
	assert.Equal(t, uint64(1), c.Stats().EvictedHorizon)
}

func TestLaunchCacheMemoryIsBounded(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 16})
	for i := 0; i < 100000; i++ {
		// Unique correlation per launch: a repeated ID would exercise
		// replacement rather than the capacity bound this test exists for.
		c.Put(launch(strconv.Itoa(i), uint64(i)))
	}
	assert.LessOrEqual(t, c.Len(), 16, "cache must stay bounded under sustained load")
	assert.Greater(t, c.Stats().EvictedCapacity, uint64(0), "evictions must be counted, not silent")
}

func TestLaunchCacheReplacesDuplicateCorrelation(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	c.Put(launch("a", 10))
	c.Put(launch("a", 20))

	got, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	require.True(t, ok)
	assert.Equal(t, uint64(20), got.TimeNs, "a repeated correlation ID must take the newer launch")
	assert.Equal(t, 1, c.Stats().Live, "a replacement must not grow the cache")
	assert.Equal(t, uint64(1), c.Stats().Replaced)
}

func TestLaunchCacheRefreshSurvivesCapacityEviction(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 2})
	c.Put(launch("a", 10))
	c.Put(launch("b", 20))
	c.Put(launch("a", 30)) // refresh: 'a' is now the newest entry
	c.Put(launch("c", 40)) // forces one capacity eviction

	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	assert.True(t, ok, "a refreshed entry must not be evicted ahead of an older untouched one")
	_, ok = c.Get(CorrelationID{Backend: BackendCUPTI, Value: "b"})
	assert.False(t, ok, "the older untouched entry must be evicted first")
	_, ok = c.Get(CorrelationID{Backend: BackendCUPTI, Value: "c"})
	assert.True(t, ok)
}

func TestLaunchCacheAnomalousTimestampDoesNotWipeCache(t *testing.T) {
	// MaxAdvanceNs deliberately unset: the default must protect callers who
	// set HorizonNs without opting in explicitly, since those are exactly the
	// callers exposed to this hazard.
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 1000, HorizonNs: 1000})
	for i := 0; i < 10; i++ {
		c.Put(launch(strconv.Itoa(i), uint64(i)))
	}
	require.Equal(t, 10, c.Stats().Live)

	c.Put(launch("anomaly", 1<<62))

	assert.Equal(t, 11, c.Stats().Live, "an anomalous timestamp must not evict previously-live entries")
	assert.Equal(t, uint64(1), c.Stats().AnomalousTimestamp)
	assert.Equal(t, uint64(0), c.Stats().EvictedHorizon, "the anomaly must not trigger horizon eviction of untouched entries")
}

func TestLaunchCacheMaxAdvanceNsSentinelDisablesClamping(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 1000, HorizonNs: 1000, MaxAdvanceNs: math.MaxUint64})
	for i := 0; i < 10; i++ {
		c.Put(launch(strconv.Itoa(i), uint64(i)))
	}
	require.Equal(t, 10, c.Stats().Live)

	c.Put(launch("far-future", 1<<62))

	assert.Equal(t, uint64(0), c.Stats().AnomalousTimestamp, "the sentinel must disable anomaly clamping")
	assert.Equal(t, uint64(10), c.Stats().EvictedHorizon, "with clamping disabled, the far-future timestamp genuinely advances the anchor and evicts the older entries")
}

// TestLaunchCacheConcurrentPutGetIsRaceFree exercises the cache's actual
// concurrency contract: producer goroutines calling Put while readers call
// Get/Stats/Len at snapshot time. Earlier race-detector runs only
// re-executed single-goroutine tests, which proves nothing about concurrent
// access. Run with -race; that is the assertion that matters here.
func TestLaunchCacheConcurrentPutGetIsRaceFree(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 256, HorizonNs: 0})

	const writers = 4
	const readers = 4
	const opsPerGoroutine = 3000

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := strconv.Itoa(w) + "-" + strconv.Itoa(i)
				c.Put(launch(id, uint64(i+1)))
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		go func(r int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				id := strconv.Itoa(r%writers) + "-" + strconv.Itoa(i)
				c.Get(CorrelationID{Backend: BackendCUPTI, Value: id})
				_ = c.Stats()
				_ = c.Len()
			}
		}(r)
	}
	wg.Wait()

	assert.LessOrEqual(t, c.Len(), 256, "cache must stay bounded under concurrent load")
}

// TestLaunchCacheEntriesReturnsWhatWasPut is a direct test of Entries(),
// added for review Important 7: Entries() (added to support Timeline's
// heuristic join) was previously exercised only indirectly, and never
// through a scenario with more than one live entry. Mutation this catches:
// Entries() returning the wrong set (empty, partial, or with stale values).
func TestLaunchCacheEntriesReturnsWhatWasPut(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	c.Put(launch("a", 10))
	c.Put(launch("b", 20))

	entries := c.Entries()
	require.Len(t, entries, 2)
	byValue := make(map[string]uint64, len(entries))
	for _, e := range entries {
		byValue[e.Correlation.Value] = e.TimeNs
	}
	assert.Equal(t, map[string]uint64{"a": 10, "b": 20}, byValue)
}

// TestLaunchCacheEntriesReflectsEviction catches Entries() returning a
// launch that capacity eviction already dropped (e.g. a cached/stale
// snapshot taken before eviction, rather than reading current state).
func TestLaunchCacheEntriesReflectsEviction(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 1})
	c.Put(launch("a", 10))
	c.Put(launch("b", 20)) // evicts "a"

	entries := c.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "b", entries[0].Correlation.Value, "Entries must not return an evicted launch")
}

// TestLaunchCacheEntriesIsACopyNotAView catches Entries() handing out a
// slice or elements that alias the cache's internal storage - e.g. returning
// a view into the cache's own order/backing array - by mutating the
// returned slice and its first element, then confirming neither the cache's
// stored value nor its size changed.
func TestLaunchCacheEntriesIsACopyNotAView(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	c.Put(launch("a", 10))

	entries := c.Entries()
	entries[0].KernelName = "tampered"
	grown := append(entries, launch("injected", 999)) //nolint:gocritic // appending to the caller's copy is the point
	require.Len(t, grown, 2, "the caller's own slice must be growable independently of the cache")

	got, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	require.True(t, ok)
	assert.Equal(t, "k_a", got.KernelName, "mutating the returned slice must not affect the cache's stored value")
	assert.Equal(t, 1, c.Len(), "appending to the returned slice must not grow the cache")
}
