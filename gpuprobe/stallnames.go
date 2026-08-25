package gpuprobe

import (
	"github.com/dpsoft/perf-agent/gpu"
)

// Stall reason names arrive on their own probe, out of band from the PC
// samples that reference them, exactly as kernel names do for launches and
// executions. This file is deliberately the same shape as kernelnames.go:
// one bounded index -> name table, one bounded FIFO of events waiting for a
// name, and one release path when the name lands. Where it diverges from
// that file it says so and why; a second, subtly different shape for the
// same problem is how the two drift apart.
//
// gpu_stall_reason_map_v1 carries stall_index -> name, emitted once when the
// producer queries the device's stall reasons and replayed in full when a
// consumer attaches late (spec §6.1's replay contract). A PC sample refers
// to a stall reason by its numeric index only, and that index is the
// VENDOR's, not ours: it is not stable across devices or driver versions, so
// it is not a portable identity and must never reach a label. The name is
// the only portable identity a stall reason has.
//
// Hence gpu.GPUPCSample.StallReason being a string, and hence an unresolved
// index becoming "" rather than "stall#17". A rendered index would put an
// unstable internal number in front of a human as though it meant
// something, and would aggregate two different stall reasons from two
// different driver versions into one label value. Stats.StallNamesMissing
// counts the empties, so an unresolved stall reason is visible rather than
// silently blank.

// truncatedStallSuffix marks a stall name the producer had to cut off at
// GPU_STALL_NAME_MAX (CUPTI's CUPTI_STALL_REASON_STRING_SIZE, 128). It is
// the same marker kernel names use, for the same reason: a truncated name
// that reads as complete is a lie about which stall reason was sampled.
// Truncation should never happen for a CUPTI device — the ABI's buffer is
// exactly CUPTI's own maximum — so a non-zero Stats.StallNamesTruncated
// means the producer is not the producer we think it is.
const truncatedStallSuffix = truncatedNameSuffix

// stallName is one interned stall reason name and whether it arrived
// complete. The twin of kernelName.
type stallName struct {
	name      string
	truncated bool
}

// resolved renders the name as it should appear downstream.
func (s stallName) resolved() string {
	if s.truncated {
		return s.name + truncatedStallSuffix
	}
	return s.name
}

// stallNameTable is the bounded stall_index -> name map.
//
// Eviction is FIFO by first insertion and a re-intern (the replay case)
// replaces the value in place without touching the order, so an index
// appears at most once in the FIFO and no position can go stale — the same
// invariant kernelNameTable keeps, which is what makes len(order)-head
// exactly equal to len(byIndex).
//
// The bound is a ceiling, not a dial. A device's stall reasons are a fixed
// enum (38 on GA102), so a healthy run uses a few dozen entries and never
// evicts; the bound exists because nothing on the wire promises that.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type stallNameTable struct {
	byIndex  map[uint32]stallName
	order    []uint32
	head     int
	capacity int
}

func newStallNameTable(capacity int) *stallNameTable {
	if capacity <= 0 {
		capacity = defaultStallNameCapacity
	}
	return &stallNameTable{byIndex: make(map[uint32]stallName), capacity: capacity}
}

// put interns a name, returning how many older names had to be evicted to
// make room. An evicted name is not record loss — the PC samples still flow
// — but every sample referring to that index from then on is unresolved, so
// the caller counts it.
func (t *stallNameTable) put(index uint32, s stallName) (evicted int) {
	if _, exists := t.byIndex[index]; !exists {
		t.order = append(t.order, index)
	}
	t.byIndex[index] = s
	for len(t.byIndex) > t.capacity && t.head < len(t.order) {
		oldest := t.order[t.head]
		t.head++
		delete(t.byIndex, oldest)
		evicted++
	}
	t.compact()
	return evicted
}

func (t *stallNameTable) get(index uint32) (stallName, bool) {
	s, ok := t.byIndex[index]
	return s, ok
}

func (t *stallNameTable) len() int { return len(t.byIndex) }

func (t *stallNameTable) compact() {
	if t.head == 0 || t.head*2 < len(t.order) {
		return
	}
	n := copy(t.order, t.order[t.head:])
	t.order = t.order[:n]
	t.head = 0
}

// unresolvedSample is one normalized PC sample held until its stall
// reason's name arrives. The twin of unnamedEvent — but a value rather than
// a pointer, because unlike unnamedEvent there is only one event type here,
// so there is no "size of both structs" to avoid.
type unresolvedSample struct {
	stallIndex uint32
	sample     gpu.GPUPCSample
}

// pendingStallSamples is the bounded FIFO of PC samples waiting for a stall
// name. As with pendingNames, overflow RELEASES the oldest sample rather
// than dropping it: a missing name must never cost a record.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type pendingStallSamples struct {
	buf      []unresolvedSample
	head     int
	capacity int
}

func newPendingStallSamples(capacity int) *pendingStallSamples {
	if capacity <= 0 {
		capacity = defaultPendingStallSampleCapacity
	}
	return &pendingStallSamples{capacity: capacity}
}

// push queues a sample, returning the oldest one for immediate release if
// the queue was already full.
func (p *pendingStallSamples) push(s unresolvedSample) (released unresolvedSample, ok bool) {
	if p.len() >= p.capacity {
		released, ok = p.pop()
	}
	p.buf = append(p.buf, s)
	return released, ok
}

func (p *pendingStallSamples) pop() (unresolvedSample, bool) {
	if p.head >= len(p.buf) {
		return unresolvedSample{}, false
	}
	s := p.buf[p.head]
	p.buf[p.head] = unresolvedSample{}
	p.head++
	p.compact()
	return s, true
}

// takeByIndex removes every sample waiting on this stall index, oldest
// first.
func (p *pendingStallSamples) takeByIndex(index uint32) []unresolvedSample {
	var out []unresolvedSample
	n := p.head
	for i := p.head; i < len(p.buf); i++ {
		if p.buf[i].stallIndex == index {
			out = append(out, p.buf[i])
			continue
		}
		p.buf[n] = p.buf[i]
		n++
	}
	if len(out) == 0 {
		return nil
	}
	clear(p.buf[n:])
	p.buf = p.buf[:n]
	return out
}

// drain empties the queue in arrival order.
func (p *pendingStallSamples) drain() []unresolvedSample {
	if p.len() == 0 {
		return nil
	}
	out := make([]unresolvedSample, p.len())
	copy(out, p.buf[p.head:])
	clear(p.buf)
	p.buf = p.buf[:0]
	p.head = 0
	return out
}

func (p *pendingStallSamples) len() int { return len(p.buf) - p.head }

func (p *pendingStallSamples) compact() {
	if p.head == 0 || p.head*2 < len(p.buf) {
		return
	}
	n := copy(p.buf, p.buf[p.head:])
	clear(p.buf[n:])
	p.buf = p.buf[:n]
	p.head = 0
}
