package debuginfod

import "testing"

func TestStatsAtomicAccumulates(t *testing.T) {
	var as atomicStats
	as.cacheHits.Add(3)
	as.cacheMisses.Add(2)
	as.fetch404s.Add(1)
	got := as.snapshot()
	if got.CacheHits != 3 {
		t.Fatalf("CacheHits = %d, want 3", got.CacheHits)
	}
	if got.CacheMisses != 2 {
		t.Fatalf("CacheMisses = %d, want 2", got.CacheMisses)
	}
	if got.Fetch404s != 1 {
		t.Fatalf("Fetch404s = %d, want 1", got.Fetch404s)
	}
}
