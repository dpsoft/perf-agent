package gpuprobe

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/symbolize"
)

func memoFrames(names ...string) []symbolize.Frame {
	out := make([]symbolize.Frame, 0, len(names))
	for _, n := range names {
		out = append(out, symbolize.Frame{Name: n})
	}
	return out
}

func TestStackMemoHitRequiresTheExactKey(t *testing.T) {
	m := newStackMemo(16, time.Second)
	m.put(1, []uint64{0x10, 0x20}, memoFrames("a", "b"), 0)

	f, ok := m.get(1, []uint64{0x10, 0x20}, 1)
	require.True(t, ok)
	assert.Equal(t, "a", f[0].Name)

	_, ok = m.get(2, []uint64{0x10, 0x20}, 1)
	assert.False(t, ok, "another process's identical addresses are another mapping")
	_, ok = m.get(1, []uint64{0x10}, 1)
	assert.False(t, ok, "a prefix is a different stack")
	_, ok = m.get(1, []uint64{0x20, 0x10}, 1)
	assert.False(t, ok)
	assert.Equal(t, uint64(1), m.hits)
	assert.Equal(t, uint64(3), m.misses)
}

// The caller's IP slice is reused for the next capture; the memo must keep
// its own copy of the key.
func TestStackMemoCopiesTheKey(t *testing.T) {
	m := newStackMemo(16, time.Second)
	ips := []uint64{0x10, 0x20}
	m.put(1, ips, memoFrames("a", "b"), 0)
	ips[0] = 0x99
	_, ok := m.get(1, []uint64{0x10, 0x20}, 1)
	assert.True(t, ok)
	_, ok = m.get(1, []uint64{0x99, 0x20}, 1)
	assert.False(t, ok)
}

// An address can come to mean something else (dlclose then dlopen at the
// same place, a JIT rewriting its perf map), so a memoized name is trusted
// only for ttl.
func TestStackMemoEntriesExpire(t *testing.T) {
	m := newStackMemo(16, time.Second)
	ttl := time.Second.Nanoseconds()
	m.put(1, []uint64{0x10}, memoFrames("old"), 0)
	_, ok := m.get(1, []uint64{0x10}, ttl)
	require.True(t, ok, "valid up to ttl")
	_, ok = m.get(1, []uint64{0x10}, ttl+1)
	assert.False(t, ok)
	assert.Equal(t, uint64(1), m.expired)
	assert.Zero(t, m.n, "an expired entry is dropped, not kept")
}

// Colliding keys share a bucket and are told apart by the full comparison.
func TestStackMemoSurvivesHashCollisions(t *testing.T) {
	m := newStackMemo(16, time.Second)
	k := memoKey(1, []uint64{0x10})
	// Force a second entry into the same bucket.
	m.m[k] = append(m.m[k], memoEntry{pid: 7, ips: []uint64{0xdead}, frames: memoFrames("other"), expires: 1 << 62})
	m.n++
	m.put(1, []uint64{0x10}, memoFrames("mine"), 0)

	f, ok := m.get(1, []uint64{0x10}, 1)
	require.True(t, ok)
	assert.Equal(t, "mine", f[0].Name)
}

func TestStackMemoIsBounded(t *testing.T) {
	m := newStackMemo(4, time.Second)
	for i := range uint64(10) {
		m.put(1, []uint64{i}, memoFrames("f"), 0)
		assert.LessOrEqual(t, m.n, 4)
	}
	assert.Equal(t, uint64(2), m.clears)
	_, ok := m.get(1, []uint64{9}, 1)
	assert.True(t, ok, "the most recent insert survives a clear")
}

func TestStackMemoDisabledIsNil(t *testing.T) {
	m := newStackMemo(-1, 0)
	assert.Nil(t, m)
	m.put(1, []uint64{1}, memoFrames("f"), 0)
	_, ok := m.get(1, []uint64{1}, 0)
	assert.False(t, ok)
}

// Through the consumer: the second capture of the same call path skips the
// symbolizer and still attaches an identical stack.
func TestRepeatedCallPathIsSymbolizedOnce(t *testing.T) {
	sink := &recordingSink{}
	c, sm, sym := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000, 0x2000)
	sm.put(2, 0x1000, 0x2000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, sampledBatchWith(4242, 8, 2, 8))
	apply(t, c, launchBatchWith(4242, 8))
	c.Flush()

	require.Len(t, sink.launches, 2)
	assert.Len(t, sym.ips, 1, "the second capture is served from the memo")
	assert.Equal(t, sink.launches[0].Launch.CPUStack, sink.launches[1].Launch.CPUStack)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StackMemoHits)
	assert.Equal(t, uint64(1), st.StackMemoMisses)
	assert.Equal(t, uint64(2), st.StacksAttached)
}

// A failed symbolization is not remembered: the next capture tries again.
func TestSymbolizationFailureIsNotMemoized(t *testing.T) {
	sink := &recordingSink{}
	sym := &fakeSymbolizer{err: assert.AnError}
	c, sm, _ := stackConsumer(t, sink, Config{Symbolizer: sym})
	sm.put(1, 0x1000)
	sm.put(2, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, sampledBatchWith(4242, 8, 2, 8))
	apply(t, c, launchBatchWith(4242, 8))
	c.Flush()

	assert.Len(t, sym.ips, 2)
	assert.Equal(t, uint64(2), c.Stats().SymbolizeFailed)
}

// A symbolization that resolved no frame is not remembered either, though it
// returns no error: the symbolizer reports a failed batch as address-only
// frames, and a later call may resolve them.
func TestUnresolvedSymbolizationIsNotMemoized(t *testing.T) {
	sink := &recordingSink{}
	sym := &fakeSymbolizer{unresolved: func(uint64) bool { return true }}
	c, sm, _ := stackConsumer(t, sink, Config{Symbolizer: sym})
	sm.put(1, 0x1000, 0x2000)
	sm.put(2, 0x1000, 0x2000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, sampledBatchWith(4242, 8, 2, 8))
	apply(t, c, launchBatchWith(4242, 8))
	c.Flush()

	assert.Len(t, sym.ips, 2, "the second capture goes back to the symbolizer")
	assert.Zero(t, c.Stats().StackMemoHits)
}

// A partly resolved stack is remembered: a library without symbols would
// otherwise keep every call path through it out of the memo.
func TestPartlyResolvedSymbolizationIsMemoized(t *testing.T) {
	sink := &recordingSink{}
	sym := &fakeSymbolizer{unresolved: func(ip uint64) bool { return ip == 0x2000 }}
	c, sm, _ := stackConsumer(t, sink, Config{Symbolizer: sym})
	sm.put(1, 0x1000, 0x2000)
	sm.put(2, 0x1000, 0x2000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, sampledBatchWith(4242, 8, 2, 8))
	apply(t, c, launchBatchWith(4242, 8))
	c.Flush()

	assert.Len(t, sym.ips, 1)
	assert.Equal(t, uint64(1), c.Stats().StackMemoHits)
}

func TestStackMemoCanBeDisabled(t *testing.T) {
	sink := &recordingSink{}
	c, sm, sym := stackConsumer(t, sink, Config{StackMemoEntries: -1})
	sm.put(1, 0x1000)
	sm.put(2, 0x1000)
	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, sampledBatchWith(4242, 8, 2, 8))
	apply(t, c, launchBatchWith(4242, 8))
	c.Flush()
	assert.Len(t, sym.ips, 2)
	assert.Zero(t, c.Stats().StackMemoHits)
}
