package gpuprobe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
)

// The same pins kernelnames_test.go puts on the kernel-name table, on the
// stall-reason table. The two stores are the same shape on purpose, and a
// shape that is only correct in one of them is not the shape it claims to
// be.

// A FIFO whose positions are not reclaimed grows with the number of records
// seen rather than with the work it tracks. A re-intern replaces in place
// without appending, so an index appears at most once.
func TestStallNameOrderTracksLiveEntriesOnly(t *testing.T) {
	tbl := newStallNameTable(4)
	for i := range 20000 {
		tbl.put(uint32(i), stallName{name: "s"})
		tbl.put(1, stallName{name: "replayed"}) // the replay case
	}
	assert.LessOrEqual(t, tbl.len(), 4)
	assert.Equal(t, tbl.len(), len(tbl.order)-tbl.head,
		"every order position must describe a live entry")
	assert.LessOrEqual(t, len(tbl.order), 4*tbl.capacity)
	assert.LessOrEqual(t, cap(tbl.order), 16*tbl.capacity)
}

// Eviction is by first insertion and a replay must not renew an entry's
// position: the producer replays its whole stall map on every late attach,
// and a renewing replay would make the first-seen indices immortal.
func TestStallNameEvictionIsByFirstInsertion(t *testing.T) {
	tbl := newStallNameTable(2)
	tbl.put(1, stallName{name: "one"})
	tbl.put(2, stallName{name: "two"})
	require.Zero(t, tbl.put(1, stallName{name: "one-replayed"}))

	assert.Equal(t, 1, tbl.put(3, stallName{name: "three"}))
	_, ok := tbl.get(1)
	assert.False(t, ok, "index 1 was interned first, so it goes first despite the replay")
	_, ok = tbl.get(2)
	assert.True(t, ok)
}

// Index 0 is an ordinary vendor stall index, not a sentinel. kernelNameTable
// can short-circuit on id 0 because the ABI defines it as "no kernel"; there
// is no such definition here, and treating 0 as absent would silently blank
// one real stall reason on every device where it happens to be index 0.
func TestStallNameIndexZeroIsAnOrdinaryEntry(t *testing.T) {
	tbl := newStallNameTable(4)
	tbl.put(0, stallName{name: "selected"})
	s, ok := tbl.get(0)
	require.True(t, ok)
	assert.Equal(t, "selected", s.resolved())
}

// A truncated name is marked wherever it is read, so no consumer has to
// remember to check a flag that does not travel with the string.
func TestTruncatedStallNameResolvesWithItsMarker(t *testing.T) {
	assert.Equal(t, "mio_throttle", stallName{name: "mio_throttle"}.resolved())
	assert.Equal(t, "mio"+truncatedStallSuffix, stallName{name: "mio", truncated: true}.resolved())
}

// Overflow RELEASES the oldest sample rather than dropping it. A missing
// stall name must never cost a record.
func TestPendingStallSamplesReleaseOldestOnOverflow(t *testing.T) {
	p := newPendingStallSamples(2)
	_, ok := p.push(unresolvedSample{stallIndex: 1, sample: gpu.GPUPCSample{PCOffset: 1}})
	assert.False(t, ok)
	_, ok = p.push(unresolvedSample{stallIndex: 1, sample: gpu.GPUPCSample{PCOffset: 2}})
	assert.False(t, ok)

	released, ok := p.push(unresolvedSample{stallIndex: 1, sample: gpu.GPUPCSample{PCOffset: 3}})
	require.True(t, ok, "a full queue hands back its oldest instead of dropping the new one")
	assert.Equal(t, uint64(1), released.sample.PCOffset)
	assert.Equal(t, 2, p.len())
}

// takeByIndex removes exactly the samples waiting on one index, oldest
// first, and leaves the rest in arrival order.
func TestPendingStallSamplesTakeByIndexIsSelectiveAndOrdered(t *testing.T) {
	p := newPendingStallSamples(8)
	for i, idx := range []uint32{7, 9, 7, 9, 7} {
		p.push(unresolvedSample{stallIndex: idx, sample: gpu.GPUPCSample{PCOffset: uint64(i)}})
	}

	got := p.takeByIndex(7)
	require.Len(t, got, 3)
	assert.Equal(t, []uint64{0, 2, 4}, []uint64{
		got[0].sample.PCOffset, got[1].sample.PCOffset, got[2].sample.PCOffset,
	})
	assert.Equal(t, 2, p.len())

	rest := p.drain()
	require.Len(t, rest, 2)
	assert.Equal(t, uint64(1), rest[0].sample.PCOffset)
	assert.Equal(t, uint64(3), rest[1].sample.PCOffset)
	assert.Nil(t, p.takeByIndex(7), "nothing is left waiting on 7")
}

// The buffer must not grow with the number of samples that have passed
// through it, only with what it currently holds.
func TestPendingStallSamplesBufferDoesNotGrowWithChurn(t *testing.T) {
	p := newPendingStallSamples(4)
	for i := range 20000 {
		p.push(unresolvedSample{stallIndex: uint32(i % 3), sample: gpu.GPUPCSample{PCOffset: uint64(i)}})
		if i%7 == 0 {
			p.takeByIndex(uint32(i % 3))
		}
	}
	assert.LessOrEqual(t, p.len(), 4)
	assert.LessOrEqual(t, cap(p.buf), 16*p.capacity)
}
