package gpu

// orderedFIFO tracks bounded FIFO insertion order for a sequence-numbered
// key space with lazy deletion. It is the single implementation of the
// mechanism LaunchCache and Timeline's pending-sample store each grew a
// hand-written, near-identical copy of: a FIFO of keys where a later insert
// (a replace, or a reappearance after consumption/eviction) may reuse an
// earlier key, and where eviction must walk from the oldest position, skip
// any position a later insert has superseded, and stop at the first
// genuinely live one - without ever mistaking mere presence-in-the-map for
// liveness. That last part is exactly the bug both LaunchCache and
// Timeline.pending independently hit and had to fix separately: a key
// deleted (consumed) and then reused leaves its old order position behind
// with no map entry (or, worse, a map entry belonging to its own next
// generation); a naive presence check can't tell that stale position apart
// from the key's brand new, live generation, and evicts the wrong one.
//
// orderedFIFO deliberately does not own value storage. The owner -
// LaunchCache or Timeline - keeps its own map from key to whatever it
// stores (the value, plus the sequence number orderedFIFO handed out for
// that entry, plus - for an owner that needs O(1) horizon eviction rather
// than rescanning its value on every check - a timestamp anchor), and
// answers orderedFIFO's isLive callback by checking that map directly. This
// keeps each owner's aliasing contract and field shapes entirely its own;
// orderedFIFO only ever sees keys and sequence numbers. It matters
// concretely for Timeline: its existing tests reach into its pending map
// directly (map indexing, len, ranging over samples), so that map has to
// stay a real, owner-held map rather than storage hidden inside a generic
// type.
//
// NOT internally synchronized. Every method requires the caller to already
// hold its own lock; both LaunchCache and Timeline call orderedFIFO only
// while holding their own mutex, and never share one orderedFIFO across
// goroutines without that lock.
type orderedFIFO[K comparable] struct {
	order []seqPos[K]
	head  int
	seq   uint64
}

// seqPos names one FIFO position: the key inserted and the sequence number
// it was inserted (or reinserted) with. A position is live only as long as
// the owner's map still holds an entry for key stamped with exactly this
// seq; a later insert for the same key bumps the sequence and appends a new
// position, leaving this one to be recognized as superseded and skipped.
type seqPos[K comparable] struct {
	key K
	seq uint64
}

// newOrderedFIFO constructs an orderedFIFO. capacityHint preallocates the
// order slice when positive (an owner with a normalized capacity bound,
// e.g. LaunchCache); zero leaves it nil, growing organically, matching a
// store with no capacity hint of its own to preallocate against (e.g.
// Timeline.pending, which is bounded by cardinality but was never
// preallocated for it before this refactor and should not start being so
// now).
func newOrderedFIFO[K comparable](capacityHint int) *orderedFIFO[K] {
	f := &orderedFIFO[K]{}
	if capacityHint > 0 {
		f.order = make([]seqPos[K], 0, capacityHint)
	}
	return f
}

// insert bumps the sequence counter and records a new order position for
// key at that sequence, returning it so the owner can stamp it onto the
// value it stores. Call this every time a key's stored value is being
// fully replaced (LaunchCache.Put's contract), or only on the
// absent-to-present transition for a store where a live entry instead
// accumulates in place across many calls (Timeline.pending's contract) -
// see the divergence documented on LaunchCache.Put vs Timeline.EmitPCSample,
// which is deliberate and must not be "fixed" into uniformity.
func (f *orderedFIFO[K]) insert(key K) uint64 {
	f.seq++
	f.order = append(f.order, seqPos[K]{key: key, seq: f.seq})
	return f.seq
}

// evictOldestLive walks forward from the current head, permanently skipping
// (never reporting, never counting as an eviction) any position isLive
// resolves as superseded, and pops the first position isLive confirms is
// still live: the head advances past it and its key is returned. isLive is
// called with the position's own (key, seq) and must answer against the
// owner's own map: true only if the owner still holds an entry for key
// stamped with exactly that seq. ok is false once every remaining position
// has been exhausted with nothing live left to evict. Compaction runs as a
// side effect - the caller never has to think about it, or repeat the
// magic-number compaction threshold.
func (f *orderedFIFO[K]) evictOldestLive(isLive func(key K, seq uint64) bool) (key K, ok bool) {
	for f.head < len(f.order) {
		pos := f.order[f.head]
		f.head++
		if isLive(pos.key, pos.seq) {
			f.compact()
			return pos.key, true
		}
	}
	f.compact()
	var zero K
	return zero, false
}

// peekOldestLive is evictOldestLive's non-destructive counterpart: it still
// permanently advances past superseded positions (that cleanup is correct
// to do regardless of what the caller decides next), but leaves the oldest
// live position's slot at head instead of consuming it, so a caller
// enforcing a horizon can resolve the returned key against its own map
// (e.g. read its anchor timestamp) and decide whether to evict at all
// before calling evictOldestLive to actually pop it.
func (f *orderedFIFO[K]) peekOldestLive(isLive func(key K, seq uint64) bool) (key K, ok bool) {
	for f.head < len(f.order) {
		pos := f.order[f.head]
		if isLive(pos.key, pos.seq) {
			f.compact()
			return pos.key, true
		}
		f.head++
	}
	f.compact()
	var zero K
	return zero, false
}

// compact reclaims the dead prefix of order once it dominates the slice, so
// a long-running owner's order slice does not grow without bound under
// sustained churn. It is not tight: compaction only fires once head >= 1024
// && head*2 >= len(order), so in steady state under sustained load order
// oscillates between roughly the live count and roughly 2x it. That is
// bounded, but len(order) should never be read as a proxy for live count -
// use the owner's own map length instead.
func (f *orderedFIFO[K]) compact() {
	if f.head < 1024 || f.head*2 < len(f.order) {
		return
	}
	rest := f.order[f.head:]
	f.order = append(f.order[:0], rest...)
	f.head = 0
}
