package symbolize

import (
	"sync"
	"testing"
)

// borrowChecker stands in for blazesym's RefCell: it does not block, it
// records that two callers were inside at once. A real concurrent borrow
// aborts the process from inside Rust, which no Go test can recover from,
// so the contract is checked against a stand-in that can report instead.
type borrowChecker struct {
	mu       sync.Mutex
	inside   int
	overlaps int
}

func (b *borrowChecker) enter() {
	b.mu.Lock()
	b.inside++
	if b.inside > 1 {
		b.overlaps++
	}
	b.mu.Unlock()
}

func (b *borrowChecker) leave() {
	b.mu.Lock()
	b.inside--
	b.mu.Unlock()
}

func (b *borrowChecker) SymbolizeProcess(uint32, []uint64) ([]Frame, error) {
	b.enter()
	defer b.leave()
	// Long enough that unserialized callers overlap reliably.
	for range 2000 {
		_ = make([]byte, 16)
	}
	return nil, nil
}

func (b *borrowChecker) Close() error { return nil }

// TestSerializedKeepsCallersOutOfEachOther is the regression test for the
// crash that shipped with the GPU collector: blazesym holds its caches in
// RefCell, and a second concurrent borrow aborts the process.
//
//	thread '<unnamed>' panicked at src/insert_map.rs:61:32:
//	RefCell already borrowed
//	fatal runtime error: failed to initiate panic, error 5, aborting
func TestSerializedKeepsCallersOutOfEachOther(t *testing.T) {
	b := &borrowChecker{}
	s := NewSerialized(b)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_, _ = s.SymbolizeProcess(1, []uint64{0x1000})
			}
		})
	}
	wg.Wait()

	if b.overlaps != 0 {
		t.Fatalf("%d concurrent entries into the symbolizer; blazesym aborts the process on the first one", b.overlaps)
	}
}

// TestUnserializedCallersDoOverlap shows the test above is not vacuous: the
// same stand-in without the wrapper records overlaps. Without this, a
// Serialized that forgot to lock would still pass.
func TestUnserializedCallersDoOverlap(t *testing.T) {
	b := &borrowChecker{}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_, _ = b.SymbolizeProcess(1, []uint64{0x1000})
			}
		})
	}
	wg.Wait()

	if b.overlaps == 0 {
		t.Skip("no overlap observed without the wrapper; the race window did not open on this machine")
	}
}

// TestNewSerializedPassesNilThrough: a typed nil in an interface field is
// not nil, and the agent checks `symbolizer != nil` in several places.
func TestNewSerializedPassesNilThrough(t *testing.T) {
	if s := NewSerialized(nil); s != nil {
		t.Fatal("NewSerialized(nil) must return nil, not a wrapper around nothing")
	}
}

type statless struct{ borrowChecker }

type withStats struct {
	borrowChecker
	st LocalStats
}

func (w *withStats) Stats() LocalStats { return w.st }

// TestSerializedForwardsStats: LogSymbolizationCost finds the counters by
// asserting on an anonymous interface, so a wrapper that did not forward
// would silently stop the cost logging (issue #109's shape).
func TestSerializedForwardsStats(t *testing.T) {
	inner := &withStats{st: LocalStats{Calls: 7}}
	if got := NewSerialized(inner).Stats().Calls; got != 7 {
		t.Fatalf("Stats().Calls = %d, want 7 forwarded from the inner symbolizer", got)
	}
	// And a symbolizer that keeps none reports nothing rather than panicking.
	if got := NewSerialized(&statless{}).Stats().Calls; got != 0 {
		t.Fatalf("a statless symbolizer should report zero calls, got %d", got)
	}
}
