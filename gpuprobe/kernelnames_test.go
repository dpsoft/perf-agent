package gpuprobe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The lesson from pendingStacks, applied to the other bounded store: a FIFO
// whose positions are not reclaimed grows with the number of records seen
// rather than with the work it tracks. This table avoids the problem by
// construction - a re-intern replaces in place without appending, so an id
// appears at most once - and this pins that, under both churn (new ids
// forcing eviction) and replay (the same id over and over).
func TestKernelNameOrderTracksLiveEntriesOnly(t *testing.T) {
	tbl := newKernelNameTable(4)
	for i := range 20000 {
		tbl.put(uint64(i), kernelName{name: "k"})
		tbl.put(1, kernelName{name: "replayed"}) // the replay case
	}
	assert.LessOrEqual(t, tbl.len(), 4)
	assert.Equal(t, tbl.len(), len(tbl.order)-tbl.head,
		"every order position must describe a live entry")
	assert.LessOrEqual(t, len(tbl.order), 4*tbl.capacity)
	assert.LessOrEqual(t, cap(tbl.order), 16*tbl.capacity)
}

// Eviction is by first insertion and a replay must not renew an entry's
// position: a name re-sent every 100ms would otherwise be immortal while
// the kernels actually in use aged out around it.
func TestKernelNameEvictionIsByFirstInsertion(t *testing.T) {
	tbl := newKernelNameTable(2)
	tbl.put(1, kernelName{name: "one"})
	tbl.put(2, kernelName{name: "two"})
	require.Zero(t, tbl.put(1, kernelName{name: "one-replayed"}))

	assert.Equal(t, 1, tbl.put(3, kernelName{name: "three"}))
	_, ok := tbl.get(1)
	assert.False(t, ok, "kernel 1 was interned first, so it goes first despite the replay")
	_, ok = tbl.get(2)
	assert.True(t, ok)
}

// A truncated name is marked wherever it is read, so no consumer has to
// remember to check a flag that does not travel with the string.
func TestTruncatedNameResolvesWithItsMarker(t *testing.T) {
	assert.Equal(t, "kAdd", kernelName{name: "kAdd"}.resolved())
	assert.Equal(t, "kAdd"+truncatedNameSuffix, kernelName{name: "kAdd", truncated: true}.resolved())
}
