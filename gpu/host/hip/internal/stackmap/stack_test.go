package stackmap

import (
	"encoding/binary"
	"testing"
)

func makeStack(ips []uint64) []byte {
	const total = MaxFrames * 8
	buf := make([]byte, total)
	for i, ip := range ips {
		if i >= MaxFrames {
			break
		}
		binary.LittleEndian.PutUint64(buf[i*8:], ip)
	}
	return buf
}

func TestExtractIPsStopsAtZero(t *testing.T) {
	got := ExtractIPs(makeStack([]uint64{0x1000, 0x2000, 0x3000}))
	if len(got) != 3 || got[0] != 0x1000 || got[2] != 0x3000 {
		t.Fatalf("ips=%#v", got)
	}
}

func TestExtractIPsTruncatesOversizedBuffers(t *testing.T) {
	buf := make([]byte, (MaxFrames+8)*8)
	for i := 0; i < MaxFrames+8; i++ {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(0x1000+i))
	}
	got := ExtractIPs(buf)
	if len(got) != MaxFrames {
		t.Fatalf("len=%d want %d", len(got), MaxFrames)
	}
}
