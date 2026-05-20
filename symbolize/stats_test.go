package symbolize

import (
	"errors"
	"strings"
	"testing"
)

// TestCounters_SnapshotZero asserts a freshly-constructed Counters
// reports all-zeros.
func TestCounters_SnapshotZero(t *testing.T) {
	var c Counters
	s := c.Snapshot()
	if s.KernelBatches != 0 || s.KernelBatchFailures != 0 ||
		s.KernelFallbackEngaged != 0 || s.KernelRawAddrFrames != 0 ||
		s.KernelInputIPs != 0 {
		t.Errorf("zero snapshot non-zero: %+v", s)
	}
}

// TestCounters_StringContainsBumpedFields asserts the human-readable
// String() form surfaces every counter that's been bumped — used for
// the end-of-run log line so operators see fallback engagement and
// failure counts without having to add a /metrics scrape.
func TestCounters_StringContainsBumpedFields(t *testing.T) {
	var c Counters
	c.KernelBatches.Add(5)
	c.KernelFallbackEngaged.Add(1)
	c.KernelRawAddrFrames.Add(42)

	out := c.Snapshot().String()
	for _, want := range []string{"batches=5", "fallback_engaged=1", "raw_addr_frames=42"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot string missing %q: %s", want, out)
		}
	}
}

// TestLocalKernelSymbolizer_StatsHappyPath asserts that on a
// successful blazesym batch the input-IPs and batches counters move,
// nothing else.
func TestLocalKernelSymbolizer_StatsHappyPath(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
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
	if got.KernelFallbackEngaged != 0 {
		t.Errorf("KernelFallbackEngaged = %d, want 0", got.KernelFallbackEngaged)
	}
	if got.KernelRawAddrFrames != 0 {
		t.Errorf("KernelRawAddrFrames = %d, want 0", got.KernelRawAddrFrames)
	}
}

// TestLocalKernelSymbolizer_StatsFallbackEngages asserts the
// fallback_engaged counter bumps exactly once when the default path
// first returns permission-denied, and stays at 1 on subsequent
// batches (sticky).
func TestLocalKernelSymbolizer_StatsFallbackEngages(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		if !useFallback {
			return nil, errBlazePermissionDenied
		}
		return []Frame{{Address: ips[0], Name: "ok"}}, nil
	})
	for i := range 3 {
		_, _ = s.SymbolizeKernel([]uint64{uint64(0xffffffff80001000) + uint64(i)})
	}
	got := s.Stats()
	if got.KernelFallbackEngaged != 1 {
		t.Errorf("KernelFallbackEngaged = %d, want 1 (sticky)", got.KernelFallbackEngaged)
	}
	if got.KernelBatches != 3 {
		t.Errorf("KernelBatches = %d, want 3", got.KernelBatches)
	}
}

// TestLocalKernelSymbolizer_StatsForcedFallbackBumpsCounter asserts
// that pre-seeding fallback mode (the PERFAGENT_FORCE_KERNEL_FALLBACK=1
// path) also marks fallback_engaged > 0 — without this, the
// end-of-run log would say fallback_engaged=0 even when the
// symbolizer ran entirely on the kallsyms path, leaving operators
// unable to tell which mode produced their pprof.
func TestLocalKernelSymbolizer_StatsForcedFallbackBumpsCounter(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		return []Frame{{Address: ips[0], Name: "ok"}}, nil
	})
	// Simulate constructor-time forced fallback.
	s.fallback.Store(true)
	s.stats.KernelFallbackEngaged.Add(1)
	_, _ = s.SymbolizeKernel([]uint64{0xffffffff80001000})
	got := s.Stats()
	if got.KernelFallbackEngaged != 1 {
		t.Errorf("KernelFallbackEngaged = %d, want 1 (forced-fallback)", got.KernelFallbackEngaged)
	}
}

// TestLocalKernelSymbolizer_StatsRawAddrFramesOnTotalFailure asserts
// that when both blazesym and the fallback fail, raw_addr_frames
// reflects the number of IPs that fell to the raw-hex path.
func TestLocalKernelSymbolizer_StatsRawAddrFramesOnTotalFailure(t *testing.T) {
	s := stubKernelSymbolizer(func(ips []uint64, useFallback bool) ([]Frame, error) {
		return nil, errors.New("blazesym broken")
	})
	ips := []uint64{0xffffffff80001000, 0xffffffff80002000, 0xffffffff80003000}
	_, _ = s.SymbolizeKernel(ips)
	got := s.Stats()
	if got.KernelRawAddrFrames != uint64(len(ips)) {
		t.Errorf("KernelRawAddrFrames = %d, want %d", got.KernelRawAddrFrames, len(ips))
	}
	if got.KernelBatchFailures != 1 {
		t.Errorf("KernelBatchFailures = %d, want 1", got.KernelBatchFailures)
	}
}
