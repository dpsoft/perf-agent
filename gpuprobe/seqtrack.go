package gpuprobe

import "math/bits"

// seqWindow is how far behind the highest sequence number seen a batch may
// still arrive and be recognised as reordered rather than lost.
//
// Most kinds take their sequence number with an atomic fetch_add before the
// emit call the uprobe fires on, so a producer thread preempted between the
// two delivers its batch after batches numbered later. How far behind it can
// land depends on how many batches of the same kind other threads emit while
// it waits. A window of 1024 per stream costs 128 bytes of bitmap and covers
// that displacement under any realistic thread count; a batch later than
// this is counted as a gap and, if it then arrives, as stale.
const seqWindow = 1024

// seqStream is one (kind, pid) sequence-number stream.
//
// Every position at or above origin is exactly one of: arrived, a confirmed
// gap (gaps), or an open hole (unresolved). A hole is confirmed only when the
// window has to move past it, so a batch that arrives late but inside the
// window is reordering, never loss.
//
// The window covers positions [base, base+seqWindow). Everything below base
// has either arrived or been confirmed missing; seen holds which positions in
// the window have arrived, indexed by position modulo seqWindow.
type seqStream struct {
	started bool
	origin  uint64 // the first position seen; nothing before it is owed
	base    uint64 // lowest position not yet arrived or confirmed missing
	high    uint64 // highest position seen
	seen    [seqWindow / 64]uint64

	gaps      uint64 // confirmed missing positions
	reordered uint64 // arrivals behind the highest position seen, filling a hole
	stale     uint64 // duplicates, or arrivals for a position already confirmed missing
}

func (s *seqStream) bit(p uint64) (word int, mask uint64) {
	i := p % seqWindow
	return int(i / 64), 1 << (i % 64)
}

func (s *seqStream) has(p uint64) bool {
	w, m := s.bit(p)
	return s.seen[w]&m != 0
}

func (s *seqStream) set(p uint64) {
	w, m := s.bit(p)
	s.seen[w] |= m
}

func (s *seqStream) clear(p uint64) {
	w, m := s.bit(p)
	s.seen[w] &^= m
}

func (s *seqStream) popcount() uint64 {
	var n int
	for _, w := range s.seen {
		n += bits.OnesCount64(w)
	}
	return uint64(n)
}

// arrive records one batch and returns how many positions it skipped past the
// highest seen so far. That return is NOT confirmed loss: it is the forward
// jump, which is what a consumer that must stay conservative about an
// incomplete history (the sampling-window store) has to act on immediately,
// before the window can tell whether the skipped batches are only late.
func (s *seqStream) arrive(seq uint64) uint64 {
	if !s.started {
		s.started = true
		s.origin, s.high, s.base = seq, seq, seq+1
		return 0
	}
	if seq < s.base {
		if seq < s.origin {
			// In flight before the first batch seen. It is not owed, so its
			// absence was never charged, and its arrival is reordering.
			s.reordered++
		} else {
			s.stale++
		}
		return 0
	}
	if seq >= s.base+seqWindow {
		s.slide(seq - seqWindow + 1)
	}
	if s.has(seq) {
		s.stale++
		return 0
	}
	s.set(seq)
	var jump uint64
	if seq > s.high {
		jump = seq - s.high - 1
		s.high = seq
	} else {
		s.reordered++
	}
	for s.base <= s.high && s.has(s.base) {
		s.clear(s.base)
		s.base++
	}
	return jump
}

// slide moves base up to newBase, confirming every position it passes that
// has not arrived. Its cost is bounded by the window, not by the distance.
func (s *seqStream) slide(newBase uint64) {
	if newBase-s.base >= seqWindow {
		// The whole window goes. Nothing at or beyond base+seqWindow can
		// have arrived: arrivals are only ever recorded inside the window.
		s.gaps += newBase - s.base - s.popcount()
		s.seen = [seqWindow / 64]uint64{}
		s.base = newBase
		return
	}
	for ; s.base < newBase; s.base++ {
		if s.has(s.base) {
			s.clear(s.base)
		} else {
			s.gaps++
		}
	}
}

// unresolved is how many positions between base and the highest seen have not
// arrived yet. They may still be in flight; once the producer is stopped and
// the transport drained, nothing else is coming for them.
func (s *seqStream) unresolved() uint64 {
	if !s.started || s.high < s.base {
		return 0
	}
	return s.high - s.base + 1 - s.popcount()
}
