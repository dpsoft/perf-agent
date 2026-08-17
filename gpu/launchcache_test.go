package gpu

import (
	"strconv"
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
