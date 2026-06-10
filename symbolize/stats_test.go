package symbolize

import (
	"errors"
	"strings"
	"testing"
)

// stubKernelSymbolizer builds a LocalKernelSymbolizer whose blazesym
// seam is replaced with call, so the raw-address backstop and stats
// bookkeeping can be exercised without a real blazesym handle.
func stubKernelSymbolizer(call func(ips []uint64) ([]Frame, error)) *LocalKernelSymbolizer {
	s := &LocalKernelSymbolizer{}
	s.symbolize = call
	return s
}

// TestCounters_SnapshotZero asserts a freshly-constructed Counters
// reports all-zeros.
func TestCounters_SnapshotZero(t *testing.T) {
	var c Counters
	s := c.Snapshot()
	if s.KernelBatches != 0 || s.KernelBatchFailures != 0 ||
		s.KernelRawAddrFrames != 0 || s.KernelInputIPs != 0 {
		t.Errorf("zero snapshot non-zero: %+v", s)
	}
}

// TestCounters_StringContainsBumpedFields asserts the human-readable
// String() form surfaces every counter that's been bumped — used for
// the end-of-run log line so operators see failure counts without
// having to add a /metrics scrape.
func TestCounters_StringContainsBumpedFields(t *testing.T) {
	var c Counters
	c.KernelBatches.Add(5)
	c.KernelRawAddrFrames.Add(42)
	c.KernelLockdownEPERM.Add(7)
	c.KernelOtherErr.Add(2)

	out := c.Snapshot().String()
	for _, want := range []string{
		"batches=5", "raw_addr_frames=42", "eperm=7", "other_err=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot string missing %q: %s", want, out)
		}
	}
}

// TestLocalKernelSymbolizer_StatsHappyPath asserts that on a
// successful blazesym batch the input-IPs and batches counters move,
// nothing else.
func TestLocalKernelSymbolizer_StatsHappyPath(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64) ([]Frame, error) {
		return []Frame{{Address: ips[0], Name: "ok"}}, nil
	})
	_, _ = s.SymbolizeKernel([]uint64{0xffffffff80001000, 0xffffffff80002000})
	got := s.Stats()
	if got.KernelBatches != 1 {
		t.Errorf("KernelBatches = %d, want 1", got.KernelBatches)
	}
	if got.KernelInputIPs != 2 {
		t.Errorf("KernelInputIPs = %d, want 2", got.KernelInputIPs)
	}
	if got.KernelRawAddrFrames != 0 {
		t.Errorf("KernelRawAddrFrames = %d, want 0", got.KernelRawAddrFrames)
	}
}

// TestLocalKernelSymbolizer_StatsRawAddrFramesOnFailure asserts that
// when blazesym fails, the IPs fall to the raw-hex backstop and both
// raw_addr_frames and batch_failures reflect it.
func TestLocalKernelSymbolizer_StatsRawAddrFramesOnFailure(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64) ([]Frame, error) {
		return nil, errors.New("blazesym broken")
	})
	ips := []uint64{0xffffffff80001000, 0xffffffff80002000, 0xffffffff80003000}
	frames, _ := s.SymbolizeKernel(ips)
	if len(frames) != len(ips) {
		t.Fatalf("got %d frames, want %d", len(frames), len(ips))
	}
	for i, f := range frames {
		if f.Reason != FailureMissingSymbols || f.Address != ips[i] {
			t.Errorf("frame %d = %+v, want raw-addr backstop for %#x", i, f, ips[i])
		}
	}
	got := s.Stats()
	if got.KernelRawAddrFrames != uint64(len(ips)) {
		t.Errorf("KernelRawAddrFrames = %d, want %d", got.KernelRawAddrFrames, len(ips))
	}
	if got.KernelBatchFailures != 1 {
		t.Errorf("KernelBatchFailures = %d, want 1", got.KernelBatchFailures)
	}
}
