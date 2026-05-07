package cache

import (
	"path/filepath"
	"testing"
	"time"
)

func newSQLiteIdx(t *testing.T) Index {
	t.Helper()
	dir := t.TempDir()
	idx, err := NewSQLiteIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSQLiteIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestSQLiteIndexTouchAndTotal(t *testing.T) {
	idx := newSQLiteIdx(t)
	if err := idx.Touch("aabb", KindDebuginfo, 100); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := idx.Touch("ccdd", KindExecutable, 250); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	total, err := idx.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	if total != 350 {
		t.Fatalf("TotalBytes = %d, want 350", total)
	}
}

func TestSQLiteIndexTouchUpdatesAccess(t *testing.T) {
	idx := newSQLiteIdx(t)
	if err := idx.Touch("aabb", KindDebuginfo, 100); err != nil {
		t.Fatalf("first Touch: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	before := time.Now()
	time.Sleep(2 * time.Millisecond) // ensure wall clock advances strictly past `before`
	if err := idx.Touch("aabb", KindDebuginfo, 100); err != nil {
		t.Fatalf("second Touch: %v", err)
	}
	var seen int
	var lastAccess time.Time
	if err := idx.Iter(func(e Entry) bool {
		seen++
		lastAccess = e.LastAccess
		if e.BuildID != "aabb" {
			t.Fatalf("BuildID = %q", e.BuildID)
		}
		return true
	}); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if seen != 1 {
		t.Fatalf("Iter visited %d entries, want 1 (Touch must upsert)", seen)
	}
	if !lastAccess.After(before) {
		t.Fatalf("LastAccess = %v, want strictly after %v (Touch must update last_access)", lastAccess, before)
	}
}

func TestSQLiteIndexEvictToOldestFirst(t *testing.T) {
	idx := newSQLiteIdx(t)
	mustTouch(t, idx, "first", KindDebuginfo, 100)
	time.Sleep(2 * time.Millisecond)
	mustTouch(t, idx, "second", KindDebuginfo, 100)
	time.Sleep(2 * time.Millisecond)
	mustTouch(t, idx, "third", KindDebuginfo, 100)

	evicted, err := idx.EvictTo(150)
	if err != nil {
		t.Fatalf("EvictTo: %v", err)
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %d entries, want 2", len(evicted))
	}
	if evicted[0].BuildID != "first" || evicted[1].BuildID != "second" {
		t.Fatalf("eviction order: %+v", evicted)
	}
	total, _ := idx.TotalBytes()
	if total != 100 {
		t.Fatalf("TotalBytes after evict = %d, want 100", total)
	}
}

func TestSQLiteIndexForget(t *testing.T) {
	idx := newSQLiteIdx(t)
	mustTouch(t, idx, "aa", KindDebuginfo, 50)
	if err := idx.Forget("aa", KindDebuginfo); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	total, _ := idx.TotalBytes()
	if total != 0 {
		t.Fatalf("TotalBytes after Forget = %d, want 0", total)
	}
}

func mustTouch(t *testing.T, idx Index, b string, k Kind, n int64) {
	t.Helper()
	if err := idx.Touch(b, k, n); err != nil {
		t.Fatalf("Touch(%s): %v", b, err)
	}
}
