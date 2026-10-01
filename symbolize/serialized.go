package symbolize

import "sync"

// Serialized wraps a Symbolizer so concurrent callers cannot enter it at
// once.
//
// This is not a performance decoration, it is a crash fix. blazesym's Rust
// core holds its per-process caches in RefCell, which assumes a single
// thread of control: a second concurrent borrow does not block or return an
// error, it panics inside Rust, and a Rust panic crossing a cgo boundary
// aborts the whole process:
//
//	thread '<unnamed>' panicked at src/insert_map.rs:61:32:
//	RefCell already borrowed
//	fatal runtime error: failed to initiate panic, error 5, aborting
//	SIGABRT: abort
//
// Nothing in symbolize/ or in blazesym's Go wrapper guards against that.
// The collectors that predate the GPU one never collided because they
// symbolize in batches from the collect path, one after another. The GPU
// pipeline is the first that symbolizes CONTINUOUSLY, from its own
// goroutine, while a capture is still running -- so it is the first to put
// two callers in blazesym at the same time.
//
// Serializing here rather than giving each collector its own symbolizer
// keeps the agent's one shared module index and symbol cache, which is why
// the agent owns a single resolver in the first place: a library already
// resolved for a CPU stack is warm for a GPU one.
type Serialized struct {
	mu    sync.Mutex
	inner Symbolizer
}

// NewSerialized returns s wrapped so that only one caller is inside it at a
// time. Returns nil when s is nil, so callers can pass an optional
// symbolizer through unchanged.
func NewSerialized(s Symbolizer) *Serialized {
	if s == nil {
		return nil
	}
	return &Serialized{inner: s}
}

func (s *Serialized) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.SymbolizeProcess(pid, ips)
}

// Close serializes against in-flight symbolization too: tearing down the
// Rust side while another goroutine is inside it is the same hazard.
func (s *Serialized) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Close()
}

// Unwrap exposes the underlying symbolizer for callers that need to reach
// implementation-specific methods. They take on the locking themselves.
func (s *Serialized) Unwrap() Symbolizer { return s.inner }

// Stats forwards the inner symbolizer's cost counters when it keeps any.
//
// It exists because LogSymbolizationCost finds them by asserting on an
// anonymous interface, and a wrapper that did not forward would make that
// assertion miss: the counters would keep incrementing and nothing would
// print them. That is the exact shape of issue #109, and it fails silently,
// so it is worth the small amount of plumbing to keep it working.
//
// A wrapped symbolizer that keeps no stats reports zero calls, which
// LogSymbolizationCost already treats as "nothing to say".
func (s *Serialized) Stats() LocalStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.inner.(interface{ Stats() LocalStats }); ok {
		return st.Stats()
	}
	return LocalStats{}
}
