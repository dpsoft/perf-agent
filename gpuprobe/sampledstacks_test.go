package gpuprobe

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
	pp "github.com/dpsoft/perf-agent/pprof"
)

func testCorr(i int) gpu.CorrelationID {
	return gpu.CorrelationID{Backend: gpu.BackendCUPTI, PID: 1, Value: strconv.Itoa(i)}
}

// The steady state of the sampled-first path is park-then-take with the map
// hovering near empty, so eviction never runs. Nothing in that path may
// accumulate: order positions left behind by take are dead weight, and a
// slice of them that only ever grows is exactly the leak driven by the
// profiled application's launch rate that this store exists to avoid - one
// position plus a retained correlation string per sampled launch, held for
// the life of the process.
//
// Pre-fix this failed with len(order) == 20000 against a capacity of 2.
func TestParkedStackOrderStaysBoundedWhenNothingIsEverEvicted(t *testing.T) {
	p := newPendingStacks(2)
	frames := pp.FramesFromNames([]string{"main"})

	for i := range 20000 {
		corr := testCorr(i)
		require.Zero(t, p.park(corr, frames, 8), "capacity is never exceeded, so nothing may be evicted")
		_, ok := p.take(corr)
		require.True(t, ok)
	}

	assert.Zero(t, p.len(), "every parked stack was taken")
	assert.LessOrEqual(t, len(p.order), 4*p.capacity,
		"the order slice must track live work, not the number of launches seen")
	assert.LessOrEqual(t, cap(p.order), 16*p.capacity,
		"a slice sliced back down still owns its backing array")
}

// The bound must not depend on the access pattern. One stack parked and
// never taken (its launch batch was dropped) sits at the head of the FIFO
// forever, so head-advancing alone cannot reclaim anything: the dead
// positions pile up *behind* a live one.
func TestParkedStackOrderStaysBoundedBehindAStuckEntry(t *testing.T) {
	p := newPendingStacks(4)
	frames := pp.FramesFromNames([]string{"main"})
	p.park(testCorr(-1), frames, 8) // never taken

	for i := range 20000 {
		corr := testCorr(i)
		p.park(corr, frames, 8)
		_, ok := p.take(corr)
		require.True(t, ok)
	}

	assert.Equal(t, 1, p.len(), "the stuck entry is still live and still evictable in FIFO order")
	assert.LessOrEqual(t, len(p.order), 4*p.capacity)
}

// Reclaiming dead positions must not disturb which live entry is oldest:
// eviction stays FIFO across a rebuild.
func TestOrderReclamationPreservesEvictionOrder(t *testing.T) {
	p := newPendingStacks(4)
	frames := pp.FramesFromNames([]string{"main"})
	for i := range 3 {
		p.park(testCorr(i), frames, 8)
	}
	// Churn a fourth correlation enough times to force reclamation to run
	// many times over. Three parked plus one churning is exactly capacity,
	// so nothing is ever evicted here - asserted, because an eviction would
	// make the ordering assertion below vacuous.
	for i := range 20000 {
		corr := testCorr(1000 + i)
		require.Zero(t, p.park(corr, frames, 8))
		p.take(corr)
	}
	// Three live plus one more is still exactly capacity; the one after
	// that overflows, and the oldest of the three originals must be what
	// goes - not whichever position happened to survive reclamation.
	require.Zero(t, p.park(testCorr(98), frames, 8))
	evicted := p.park(testCorr(99), frames, 8)
	assert.Equal(t, 1, evicted)
	_, ok := p.take(testCorr(0))
	assert.False(t, ok, "correlation 0 was the oldest and must be the one evicted")
	for _, i := range []int{1, 2, 98, 99} {
		_, ok := p.take(testCorr(i))
		assert.Truef(t, ok, "correlation %d must survive", i)
	}
}
