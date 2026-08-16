package symbolize

import (
	"math"
	"slices"
	"sync"
)

// LatencyHist tracks per-call durations for batch operations and
// computes percentiles on demand. Uses a fixed-size ring buffer
// (no growing, no GC pressure on the hot path) plus min/max/sum
// for unbounded summary stats.
//
// Sized for "useful percentiles" rather than "perfect fidelity":
// 1024 samples is enough for stable p50/p99 over recent activity
// without making Record contend on a giant mutex. For
// SymbolizeKernel batch durations (≪ thousands/sec even on busy
// system-wide captures), the mutex is not a hot point.
//
// The histogram intentionally tracks RECENT activity (sliding
// window via ring buffer) rather than all-time history — that
// way an early burst of slow batches doesn't permanently bias
// the percentile readout.
type LatencyHist struct {
	mu        sync.Mutex
	ring      [latencyHistSize]uint64 // microseconds
	pos       int
	full      bool
	count     uint64
	sumUs     uint64
	minUs     uint64
	maxUs     uint64
}

const latencyHistSize = 1024

// Record stamps a single observed duration (in microseconds).
// Safe for concurrent callers.
func (h *LatencyHist) Record(us uint64) {
	h.mu.Lock()
	h.ring[h.pos] = us
	h.pos++
	if h.pos == latencyHistSize {
		h.pos = 0
		h.full = true
	}
	h.count++
	h.sumUs += us
	if h.count == 1 || us < h.minUs {
		h.minUs = us
	}
	if us > h.maxUs {
		h.maxUs = us
	}
	h.mu.Unlock()
}

// LatencyHistSnapshot is a value-type point-in-time view of a
// LatencyHist. Returned by (*LatencyHist).Snapshot for safe
// concurrent reads.
type LatencyHistSnapshot struct {
	Count uint64
	MinUs uint64
	MaxUs uint64
	MeanUs uint64
	P50Us uint64
	P99Us uint64
}

// Snapshot returns a percentile summary of recent activity.
// Computes p50/p99 by sorting a copy of the current ring; cost
// is O(N log N) where N ≤ latencyHistSize. Called from the
// /metrics handler / end-of-run log — not on the hot path.
func (h *LatencyHist) Snapshot() LatencyHistSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return LatencyHistSnapshot{}
	}
	// Copy the live portion of the ring out so we can sort
	// without affecting Record contention.
	n := latencyHistSize
	if !h.full {
		n = h.pos
	}
	tmp := make([]uint64, n)
	copy(tmp, h.ring[:n])
	slices.Sort(tmp)
	return LatencyHistSnapshot{
		Count:  h.count,
		MinUs:  h.minUs,
		MaxUs:  h.maxUs,
		MeanUs: h.sumUs / h.count,
		P50Us:  percentile(tmp, 0.50),
		P99Us:  percentile(tmp, 0.99),
	}
}

// percentile takes a SORTED slice and returns the value at the
// given percentile using nearest-rank. Empty slice → 0.
func percentile(sorted []uint64, p float64) uint64 {
	if len(sorted) == 0 {
		return 0
	}
	// nearest-rank: index = ceil(p * N) - 1 (0-based)
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
