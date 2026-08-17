package gpu

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orderedFIFOIsLive builds an isLive callback bound to a plain map the test
// owns directly - the same shape LaunchCache.isLiveLocked and
// Timeline.isPendingLiveLocked use in production, minus the mutex, since
// these tests exercise orderedFIFO single-threaded except where noted.
func orderedFIFOIsLive(byKey map[string]uint64) func(string, uint64) bool {
	return func(key string, seq uint64) bool {
		s, ok := byKey[key]
		return ok && s == seq
	}
}

// TestOrderedFIFOStalePositionSurvivesCorrelationIDReuse is the direct,
// generalized regression test for both prior bugs (LaunchCache's and
// Timeline.pending's): a key is consumed (its map entry removed, its order
// position left behind), the same key is then reused (a fresh insert, a new
// generation), and genuinely older keys are added in between. Eviction
// pressure must take the genuinely oldest *live* entry, not mistake the
// reused key's stale pre-consumption position for its live one.
//
// Mutation this catches: isLive's `s == seq` check weakened to a bare
// presence check (`ok` alone). With that mutation, the stale position left
// behind by "x"'s consumption matches "x"'s reused entry by presence alone
// (since "x" is in byKey again, just under a new seq), so the first
// evictOldestLive call would return "x" - evicting the freshest entry -
// instead of "w", the genuinely oldest live one.
func TestOrderedFIFOStalePositionSurvivesCorrelationIDReuse(t *testing.T) {
	f := newOrderedFIFO[string](0)
	byKey := map[string]uint64{}
	insert := func(k string) { byKey[k] = f.insert(k) }
	isLive := orderedFIFOIsLive(byKey)

	insert("x")        // seq 1
	delete(byKey, "x") // "x" consumed: map entry gone, order position (x,1) left stale
	insert("w")        // seq 2 - genuinely older than the reused "x" below
	insert("q")        // seq 3 - also genuinely older
	insert("x")        // seq 4 - "x" reused: a fresh, live generation

	key, ok := f.evictOldestLive(isLive)
	require.True(t, ok)
	assert.Equal(t, "w", key,
		"the genuinely oldest live entry must be evicted, not the reused key's stale pre-consumption position")
	delete(byKey, key)

	assert.Equal(t, uint64(4), byKey["x"], "the reused, freshest entry must survive untouched")
	_, qStillLive := byKey["q"]
	assert.True(t, qStillLive, "eviction must stop after taking exactly the oldest live entry")
}

// TestOrderedFIFOSupersededPositionsSkippedWithoutCounting verifies that a
// position superseded by a later insert for the same key is walked past
// silently: it advances the internal head (so it is never revisited) but is
// never itself returned as an eviction, so a caller counting losses from
// evictOldestLive's return values never over-counts.
//
// Mutation this catches: evictOldestLive treating every position as live
// regardless of what isLive reports (i.e. the superseded-skip check being
// dropped) would report 3 evictions here - ["a", "a", "b"], the dead
// pre-replace position counted alongside the two genuinely live entries -
// instead of exactly ["a", "b"].
func TestOrderedFIFOSupersededPositionsSkippedWithoutCounting(t *testing.T) {
	f := newOrderedFIFO[string](0)
	byKey := map[string]uint64{}
	insert := func(k string) { byKey[k] = f.insert(k) }
	isLive := orderedFIFOIsLive(byKey)

	insert("a") // seq 1
	insert("a") // seq 2 - replaces "a": order position (a,1) is now superseded
	insert("b") // seq 3

	var evicted []string
	for {
		key, ok := f.evictOldestLive(isLive)
		if !ok {
			break
		}
		delete(byKey, key)
		evicted = append(evicted, key)
	}

	assert.Equal(t, []string{"a", "b"}, evicted,
		"exactly the two genuinely live entries must be evicted, in FIFO order")
	assert.Equal(t, 3, f.head,
		"the superseded position must still be walked past (consumed from the order slice) even though it was never reported")
}

// TestOrderedFIFOCompactionBoundsOrderSlice is the generalized version of
// TestLaunchCacheMemoryIsBounded, aimed directly at the order slice rather
// than the owner's map: under sustained insert/evict churn far exceeding
// capacity, the order slice's backing array must stay bounded by
// compaction, not grow linearly with total operations performed over the
// structure's lifetime.
//
// Mutation this catches: removing the compact() call from evictOldestLive
// (or breaking its head>=1024 && head*2>=len(order) condition) would let
// order grow to roughly the full 100000-operation count instead of staying
// within a small bounded multiple of capacity.
func TestOrderedFIFOCompactionBoundsOrderSlice(t *testing.T) {
	const capacity = 16
	const iterations = 100000

	f := newOrderedFIFO[string](capacity)
	byKey := map[string]uint64{}
	isLive := orderedFIFOIsLive(byKey)

	for i := 0; i < iterations; i++ {
		k := strconv.Itoa(i) // unique key per iteration: exercises capacity eviction, not replace
		byKey[k] = f.insert(k)
		for len(byKey) > capacity {
			key, ok := f.evictOldestLive(isLive)
			if !ok {
				break
			}
			delete(byKey, key)
		}
	}

	assert.LessOrEqual(t, len(byKey), capacity, "the owner's map must stay bounded by capacity")
	assert.Less(t, len(f.order), 5000,
		"the order slice must stay bounded by compaction, not grow with the total number of operations performed")
}

// TestOrderedFIFOConcurrencyContract exercises orderedFIFO exactly as its
// doc comment requires: NOT internally synchronized, with the caller
// providing its own lock around every call. Run with -race, this is the
// assertion that the documented contract - external locking is sufficient,
// and mandatory - actually holds. It does not, on its own, prove LaunchCache
// or Timeline honor that contract correctly; TestLaunchCacheConcurrentPutGetIsRaceFree
// and TestTimelineConcurrentIngestionIsRaceFree (both pre-existing, both
// still race-clean after this refactor) are what verify that; see this
// package's boundedfifo.go doc comment for the explicit contract statement
// both owners are required to match.
func TestOrderedFIFOConcurrencyContract(t *testing.T) {
	const capacity = 64
	const writers = 4
	const opsPerWriter = 2000

	f := newOrderedFIFO[string](capacity)
	byKey := make(map[string]uint64)
	isLive := orderedFIFOIsLive(byKey)
	var mu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < opsPerWriter; i++ {
				k := strconv.Itoa(w) + "-" + strconv.Itoa(i)
				mu.Lock()
				byKey[k] = f.insert(k)
				for len(byKey) > capacity {
					key, ok := f.evictOldestLive(isLive)
					if !ok {
						break
					}
					delete(byKey, key)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.LessOrEqual(t, len(byKey), capacity, "concurrent, externally-locked use must still stay bounded")
}
