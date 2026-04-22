package bpfstack

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeStack packs a list of IPs into the 1016-byte layout that
// BPF_MAP_TYPE_STACK_TRACE produces (127 slots × 8 bytes). Slots past
// the supplied list are zeroed — the kernel's "end of stack" signal.
func makeStack(ips []uint64) []byte {
	const total = MaxFrames * 8
	b := make([]byte, total)
	for i, ip := range ips {
		if i >= MaxFrames {
			break
		}
		binary.LittleEndian.PutUint64(b[i*8:], ip)
	}
	return b
}

func TestExtractIPs_EmptyStack(t *testing.T) {
	assert.Empty(t, ExtractIPs(makeStack(nil)))
}

func TestExtractIPs_StopsAtFirstZero(t *testing.T) {
	got := ExtractIPs(makeStack([]uint64{0x1000, 0x2000, 0x3000}))
	assert.Equal(t, []uint64{0x1000, 0x2000, 0x3000}, got)
}

func TestExtractIPs_InteriorZeroTerminates(t *testing.T) {
	// Zero in the middle ends the stack even if later slots are non-zero
	// (defensive — shouldn't happen from the kernel).
	got := ExtractIPs(makeStack([]uint64{0x1000, 0x2000, 0, 0x4000}))
	assert.Equal(t, []uint64{0x1000, 0x2000}, got)
}

func TestExtractIPs_FullStack(t *testing.T) {
	ips := make([]uint64, MaxFrames)
	for i := range ips {
		ips[i] = uint64(0x1000 + i)
	}
	got := ExtractIPs(makeStack(ips))
	assert.Equal(t, ips, got)
	assert.Len(t, got, MaxFrames)
}

func TestExtractIPs_ShortBuffer(t *testing.T) {
	// Buffer smaller than a full stackmap entry — 16 bytes (2 slots).
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], 0x1000)
	binary.LittleEndian.PutUint64(b[8:], 0x2000)
	got := ExtractIPs(b)
	assert.Equal(t, []uint64{0x1000, 0x2000}, got)
}

func TestExtractIPs_BeyondMaxSlotsTruncated(t *testing.T) {
	// Defensive: an oversized buffer is capped at MaxFrames.
	oversized := make([]byte, (MaxFrames+10)*8)
	for i := 0; i < MaxFrames+10; i++ {
		binary.LittleEndian.PutUint64(oversized[i*8:], uint64(0x1000+i))
	}
	got := ExtractIPs(oversized)
	require.Len(t, got, MaxFrames)
	assert.Equal(t, uint64(0x1000), got[0])
	assert.Equal(t, uint64(0x1000+MaxFrames-1), got[MaxFrames-1])
}

func BenchmarkExtractIPs_Shallow(b *testing.B) {
	stack := makeStack([]uint64{0x1000, 0x2000, 0x3000, 0x4000, 0x5000})
	b.ReportAllocs()
	for b.Loop() {
		_ = ExtractIPs(stack)
	}
}

func BenchmarkExtractIPs_Deep(b *testing.B) {
	ips := make([]uint64, 100)
	for i := range ips {
		ips[i] = uint64(0x1000 + i)
	}
	stack := makeStack(ips)
	b.ReportAllocs()
	for b.Loop() {
		_ = ExtractIPs(stack)
	}
}

func BenchmarkExtractIPs_Full(b *testing.B) {
	ips := make([]uint64, MaxFrames)
	for i := range ips {
		ips[i] = uint64(0x1000 + i)
	}
	stack := makeStack(ips)
	b.ReportAllocs()
	for b.Loop() {
		_ = ExtractIPs(stack)
	}
}
