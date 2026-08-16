package symbolize

import (
	"fmt"
	"testing"
)

// TestRawUserAddrFrames asserts the helper synthesizes one Frame per
// IP with hex-string Name and FailureMissingSymbols Reason — the
// same posture rawKernelAddrFrames uses for the kernel side. Symmetric
// because both serve the same purpose (preserve stack shape when the
// resolver fails) and operators should be able to read both halves
// with the same mental model.
func TestRawUserAddrFrames(t *testing.T) {
	ips := []uint64{0x55fa00001234, 0x7fa9c0005678, 0x40005678}
	frames := rawUserAddrFrames(ips)
	if len(frames) != len(ips) {
		t.Fatalf("got %d frames, want %d", len(frames), len(ips))
	}
	for i, f := range frames {
		wantName := fmt.Sprintf("0x%x", ips[i])
		if f.Name != wantName {
			t.Errorf("frame[%d].Name = %q, want %q", i, f.Name, wantName)
		}
		if f.Reason != FailureMissingSymbols {
			t.Errorf("frame[%d].Reason = %v, want FailureMissingSymbols", i, f.Reason)
		}
		if f.Address != ips[i] {
			t.Errorf("frame[%d].Address = %#x, want %#x", i, f.Address, ips[i])
		}
	}
}

// TestRawUserAddrFramesEmpty asserts the zero-IP edge case returns
// an empty slice without panic.
func TestRawUserAddrFramesEmpty(t *testing.T) {
	frames := rawUserAddrFrames(nil)
	if len(frames) != 0 {
		t.Errorf("nil IPs produced %d frames, want 0", len(frames))
	}
}
