package symbolize

import (
	"testing"
)

// TestAllocsBudget_LatencyHistRecord caps the per-Record cost
// of the histogram on the hot path. Record runs under a mutex
// but should never allocate — the ring buffer is fixed-size.
func TestAllocsBudget_LatencyHistRecord(t *testing.T) {
	const budget = 0
	var h LatencyHist
	got := testing.AllocsPerRun(1000, func() {
		h.Record(42)
	})
	if got > float64(budget) {
		t.Errorf("LatencyHist.Record allocs/op = %.2f, want <= %d", got, budget)
	}
}
