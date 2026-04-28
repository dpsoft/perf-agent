package ehmaps

import (
	"testing"
)

// BenchmarkScanAndEnroll_BuildIDCacheHit measures the per-call cost of
// ScanAndEnroll on a synthetic 100-PID × 5-binary tree.
//
// Reports buildid_reads/op (= cache size after one scan, which equals
// the number of distinct binaries the cache actually serviced). With
// the cache working correctly this should be 5 — proving the cache
// caps build-id reads at K regardless of N.
func BenchmarkScanAndEnroll_BuildIDCacheHit(b *testing.B) {
	procRoot := buildSyntheticProcTree(b, 100, 5)
	store := NewTableStore(nil, nil, nil, nil)
	tracker := NewPIDTracker(store, nil, nil)

	var lastTables int
	b.ResetTimer()
	for b.Loop() {
		_, tables, _ := ScanAndEnrollFromTree(procRoot, tracker)
		lastTables = tables
	}
	b.ReportMetric(float64(lastTables), "buildid_reads/op")
}
