package ehmaps_test

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// TestRefcountIncrement verifies that Acquire on the same tableID from
// multiple PIDs increments the refcount rather than re-compiling.
func TestRefcountIncrement(t *testing.T) {
	rc := ehmaps.NewRefcountTable()
	const tid uint64 = 0x42

	if got := rc.Acquire(tid, 100); got != 1 {
		t.Fatalf("first acquire: refcount=%d, want 1", got)
	}
	if got := rc.Acquire(tid, 200); got != 2 {
		t.Fatalf("second acquire: refcount=%d, want 2", got)
	}
	if got := rc.Release(tid, 100); got != 1 {
		t.Fatalf("release pid 100: refcount=%d, want 1", got)
	}
	if got := rc.Release(tid, 200); got != 0 {
		t.Fatalf("release pid 200: refcount=%d, want 0 (evictable)", got)
	}
}

// TestRefcountDoubleAcquireSamePID is a no-op by design — acquiring
// the same (tid, pid) pair twice counts as one reference.
func TestRefcountDoubleAcquireSamePID(t *testing.T) {
	rc := ehmaps.NewRefcountTable()
	const tid uint64 = 0x42
	rc.Acquire(tid, 100)
	if got := rc.Acquire(tid, 100); got != 1 {
		t.Fatalf("re-acquire same pid: refcount=%d, want 1 (idempotent)", got)
	}
	if got := rc.Release(tid, 100); got != 0 {
		t.Fatalf("release after re-acquire: refcount=%d, want 0", got)
	}
}

// TestRefcountReleaseUntracked returns 0 without error.
func TestRefcountReleaseUntracked(t *testing.T) {
	rc := ehmaps.NewRefcountTable()
	if got := rc.Release(0x99, 42); got != 0 {
		t.Fatalf("release untracked: refcount=%d, want 0", got)
	}
}
