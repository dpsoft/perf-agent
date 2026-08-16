package symbolize

import "testing"

// TestLatencyHist_ZeroState asserts a freshly-constructed
// LatencyHist reports all-zeros without panicking — important
// because /metrics scrapers may hit the handler before any
// SymbolizeKernel call has happened.
func TestLatencyHist_ZeroState(t *testing.T) {
	var h LatencyHist
	s := h.Snapshot()
	if s.Count != 0 || s.MinUs != 0 || s.MaxUs != 0 || s.P50Us != 0 || s.P99Us != 0 {
		t.Errorf("zero state non-zero: %+v", s)
	}
}

// TestLatencyHist_PercentileCorrectness pumps a known
// distribution through and verifies the percentile output matches
// nearest-rank semantics for that distribution.
func TestLatencyHist_PercentileCorrectness(t *testing.T) {
	var h LatencyHist
	// 100 samples evenly spaced 1..100 μs.
	for v := uint64(1); v <= 100; v++ {
		h.Record(v)
	}
	s := h.Snapshot()
	if s.Count != 100 {
		t.Errorf("Count = %d, want 100", s.Count)
	}
	if s.MinUs != 1 {
		t.Errorf("MinUs = %d, want 1", s.MinUs)
	}
	if s.MaxUs != 100 {
		t.Errorf("MaxUs = %d, want 100", s.MaxUs)
	}
	if s.MeanUs != 50 {
		t.Errorf("MeanUs = %d, want 50 (sum 5050 / 100)", s.MeanUs)
	}
	// nearest-rank p50 = sorted[ceil(0.50*100)-1] = sorted[49] = 50
	if s.P50Us != 50 {
		t.Errorf("P50Us = %d, want 50", s.P50Us)
	}
	// nearest-rank p99 = sorted[ceil(0.99*100)-1] = sorted[98] = 99
	if s.P99Us != 99 {
		t.Errorf("P99Us = %d, want 99", s.P99Us)
	}
}

// TestLatencyHist_RingOverwrite covers the sliding-window
// semantic: after more than latencyHistSize samples, the
// percentile reflects RECENT activity rather than all-time.
// Concretely: 2000 samples where the first 1024 are slow (1000
// μs) and the next 976 are fast (1 μs) — the percentile should
// be dominated by the fast tail, since the ring overwrote the
// slow head.
func TestLatencyHist_RingOverwrite(t *testing.T) {
	var h LatencyHist
	for range 1024 {
		h.Record(1000)
	}
	for range 976 {
		h.Record(1)
	}
	s := h.Snapshot()
	// All 2000 samples were observed — count tracks lifetime, not
	// ring depth.
	if s.Count != 2000 {
		t.Errorf("Count = %d, want 2000", s.Count)
	}
	// Ring is full at 1024; the most recent 1024 entries are
	// the last 976 ones (all == 1) + the trailing 48 from the
	// slow batch (all == 1000). So median of the ring is 1, and
	// p99 is 1000.
	if s.P50Us != 1 {
		t.Errorf("P50Us = %d, want 1 (recent tail dominates)", s.P50Us)
	}
	if s.P99Us != 1000 {
		t.Errorf("P99Us = %d, want 1000 (residual slow head)", s.P99Us)
	}
}
