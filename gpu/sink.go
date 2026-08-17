package gpu

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrSinkFull is returned when the sink has reached capacity. A backend
// receiving it should drop the event and count it locally rather than block:
// blocking a producer inside a profiled application is worse than losing a
// sample, and the loss is visible in SinkStats either way.
var ErrSinkFull = errors.New("gpu: sink full")

// EventKindStats is the ingestion outcome breakdown for one event kind -
// review Important 2. SinkStats used to keep DroppedFull/DroppedInvalid/
// DroppedDownstream as three aggregate counters shared by all five event
// kinds, so a drop could never be attributed to (say) launches vs PC
// samples - exactly the information an operator needs to tell "we're
// losing correlation anchors" from "we're losing sampling detail, joins are
// still fine".
type EventKindStats struct {
	Accepted       uint64 `json:"accepted,omitempty"`
	DroppedFull    uint64 `json:"dropped_full,omitempty"`
	DroppedInvalid uint64 `json:"dropped_invalid,omitempty"`
	// DroppedDownstream counts events of this kind that passed admission
	// (clock domain and capacity) but that inner failed to accept. Not
	// counted in Accepted, and the capacity they reserved is returned to
	// the bucket - a downstream hiccup must not permanently shrink what the
	// sink can admit.
	DroppedDownstream uint64 `json:"dropped_downstream,omitempty"`
}

// SinkStats is the ingestion-side loss record, broken down per event kind.
// JoinStats reports what could not be correlated; this reports what never
// arrived.
type SinkStats struct {
	Launches  EventKindStats `json:"launches,omitempty"`
	Execs     EventKindStats `json:"execs,omitempty"`
	PCSamples EventKindStats `json:"pc_samples,omitempty"`
	Modules   EventKindStats `json:"modules,omitempty"`
	Events    EventKindStats `json:"events,omitempty"`
}

// eventKind identifies which EventKindStats an admission outcome is
// recorded against. Distinct from eventClass: eventClass groups kinds by
// which token bucket they draw from (review Important 5), eventKind
// identifies exactly one of the five EventSink methods for accounting
// (review Important 2). EmitLaunch is both classAnchor and kindLaunch.
type eventKind int

const (
	kindLaunch eventKind = iota
	kindExec
	kindPCSample
	kindModule
	kindEvent
)

// statsFor returns the EventKindStats field to record kind's outcome
// against. Caller holds mu.
func (s *CountingSink) statsFor(kind eventKind) *EventKindStats {
	switch kind {
	case kindLaunch:
		return &s.stats.Launches
	case kindExec:
		return &s.stats.Execs
	case kindPCSample:
		return &s.stats.PCSamples
	case kindModule:
		return &s.stats.Modules
	default:
		return &s.stats.Events
	}
}

// eventClass groups EventSink methods by admission priority - review
// Important 5. A single, undifferentiated token bucket meant launches and
// modules (the correlation anchors every later exec/PC-sample join depends
// on) were dropped exactly as readily as the much higher-volume exec/
// sample/event traffic under overload. Per review Critical 2, a dropped
// launch turns every subsequent exec referencing it into a permanent miss -
// so losing an anchor is structurally more expensive than losing one data
// point, and admission must reflect that rather than treat all five event
// kinds as fungible.
type eventClass int

const (
	// classAnchor is EmitLaunch/EmitModule: correlation anchors that later
	// exact and heuristic joins depend on. Given its own token bucket so
	// exec/sample/event volume can never starve it.
	classAnchor eventClass = iota
	// classData is EmitExec/EmitPCSample/EmitEvent: everything joined
	// against an anchor rather than being one itself.
	classData
)

// tokenBucket is one admission budget: burst is the maximum admitted at
// once, refilled continuously at the CountingSink's shared refillRate.
// unbounded means no admission limit is ever applied (capacity <= 0).
type tokenBucket struct {
	unbounded bool
	burst     float64
	tokens    float64
}

// CountingSink wraps a sink with admission control and accounting. Admission
// is a token bucket per eventClass (see its doc comment): burst is the
// maximum number of events of that class admitted at once, refilled
// continuously at refillRate tokens/second. capacity <= 0 means unbounded -
// no admission limit is ever applied, for either class.
//
// A lifetime budget that only ever decreases would be wrong for a
// continuously-running profiler: once spent it would reject forever. The
// token bucket recovers over time instead, the way a rate limiter does.
type CountingSink struct {
	mu    sync.Mutex
	inner EventSink

	anchor tokenBucket
	data   tokenBucket

	refillRate float64 // tokens per second, shared by both buckets
	lastRefill time.Time
	now        func() time.Time

	stats SinkStats
}

// defaultRefillRate is used by NewCountingSink when no explicit rate is
// given: the bucket refills fully within one second of being exhausted, so a
// producer that pauses briefly regains its full burst rather than being
// throttled forever. Callers that need a different steady-state rate should
// use NewCountingSinkWithRate.
func defaultRefillRate(capacity int) float64 {
	return float64(capacity)
}

// NewCountingSink constructs a CountingSink whose admission control is a
// token bucket with capacity as the burst size, refilled at
// defaultRefillRate (capacity tokens/second) using the real wall clock.
// capacity <= 0 means unbounded, matching the historical "0 means unbounded"
// contract - a negative capacity is not a smaller-than-zero budget, it is
// simply also unbounded.
func NewCountingSink(inner EventSink, capacity int) *CountingSink {
	return NewCountingSinkWithRate(inner, capacity, defaultRefillRate(capacity), nil)
}

// NewCountingSinkWithRate is NewCountingSink with the refill rate (tokens per
// second) and clock made explicit, for callers that need a different
// steady-state rate or deterministic tests. now defaults to time.Now when
// nil. capacity <= 0 means unbounded, independent of refillPerSecond.
//
// Each eventClass (see its doc comment) gets its own independent token
// bucket, each sized at capacity/refillPerSecond - review Important 5. This
// is deliberately not capacity split across the two classes: launches/
// modules must never be crowded out by exec/sample/event volume, so the
// anchor class needs its own full, untouched budget regardless of how
// saturated the data class is.
func NewCountingSinkWithRate(inner EventSink, capacity int, refillPerSecond float64, now func() time.Time) *CountingSink {
	if now == nil {
		now = time.Now
	}
	s := &CountingSink{
		inner:      inner,
		refillRate: refillPerSecond,
		now:        now,
		lastRefill: now(),
	}
	if capacity <= 0 {
		// Explicit rather than relying on a ">0" guard at every call site,
		// so the unbounded case can't silently diverge if the guard changes.
		s.anchor.unbounded = true
		s.data.unbounded = true
	} else {
		s.anchor.burst, s.anchor.tokens = float64(capacity), float64(capacity)
		s.data.burst, s.data.tokens = float64(capacity), float64(capacity)
	}
	return s
}

func (s *CountingSink) Stats() SinkStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// bucketFor returns the token bucket for class. Caller holds mu.
func (s *CountingSink) bucketFor(class eventClass) *tokenBucket {
	if class == classAnchor {
		return &s.anchor
	}
	return &s.data
}

// refill adds tokens for elapsed time since the last refill to both buckets,
// each capped at its own burst. Both buckets refill from the same shared
// refillRate and share one lastRefill clock reading (a single elapsed
// duration applied to both), so neither class's replenishment rate depends
// on the other's admission traffic. Caller holds mu.
func (s *CountingSink) refill() {
	now := s.now()
	elapsed := now.Sub(s.lastRefill)
	if elapsed <= 0 {
		return
	}
	s.lastRefill = now
	add := elapsed.Seconds() * s.refillRate
	for _, b := range []*tokenBucket{&s.anchor, &s.data} {
		if b.unbounded {
			continue
		}
		b.tokens += add
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
}

// admitCapacity applies class's own token-bucket bound - the one rule
// EmitModule shares with every other event type. On success it has reserved
// one token from class's bucket; call release(class) if the reservation
// turns out not to be used. kind attributes a drop to the right
// EventKindStats. Caller holds mu.
func (s *CountingSink) admitCapacity(kind eventKind, class eventClass) error {
	b := s.bucketFor(class)
	if b.unbounded {
		return nil
	}
	s.refill()
	if b.tokens < 1 {
		s.statsFor(kind).DroppedFull++
		return ErrSinkFull
	}
	b.tokens--
	return nil
}

// release returns a reserved token to class's bucket after inner fails to
// accept a delegated event, so a downstream rejection does not permanently
// shrink that class's capacity. Caller holds mu.
func (s *CountingSink) release(class eventClass) {
	b := s.bucketFor(class)
	if b.unbounded {
		return
	}
	b.tokens++
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// admit applies the clock-domain contract, then class's capacity bound.
// Caller holds mu. EmitModule bypasses this and calls admitCapacity
// directly: a GPUModule has no ClockDomain field to validate.
func (s *CountingSink) admit(kind eventKind, class eventClass, domain ClockDomain) error {
	if err := ValidateSupportedClockDomain(domain); err != nil {
		s.statsFor(kind).DroppedInvalid++
		return fmt.Errorf("gpu: rejected event: %w", err)
	}
	return s.admitCapacity(kind, class)
}

func (s *CountingSink) EmitLaunch(l GPUKernelLaunch) error {
	s.mu.Lock()
	if err := s.admit(kindLaunch, classAnchor, l.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitLaunch(l)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release(classAnchor)
		s.statsFor(kindLaunch).DroppedDownstream++
		return err
	}
	s.statsFor(kindLaunch).Accepted++
	return nil
}

func (s *CountingSink) EmitExec(e GPUKernelExec) error {
	s.mu.Lock()
	if err := s.admit(kindExec, classData, e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitExec(e)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release(classData)
		s.statsFor(kindExec).DroppedDownstream++
		return err
	}
	s.statsFor(kindExec).Accepted++
	return nil
}

func (s *CountingSink) EmitPCSample(p GPUPCSample) error {
	s.mu.Lock()
	if err := s.admit(kindPCSample, classData, p.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitPCSample(p)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release(classData)
		s.statsFor(kindPCSample).DroppedDownstream++
		return err
	}
	s.statsFor(kindPCSample).Accepted++
	return nil
}

// EmitModule deliberately skips the clock-domain check: GPUModule is
// symbolization metadata with no ClockDomain field. It still goes through
// the same token-bucket admission (admitCapacity, classAnchor - modules are
// correlation anchors for symbolization the same way launches are for
// execs) and the same reserve/delegate/settle sequence as every other event
// type.
func (s *CountingSink) EmitModule(m GPUModule) error {
	s.mu.Lock()
	if err := s.admitCapacity(kindModule, classAnchor); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitModule(m)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release(classAnchor)
		s.statsFor(kindModule).DroppedDownstream++
		return err
	}
	s.statsFor(kindModule).Accepted++
	return nil
}

func (s *CountingSink) EmitEvent(e GPUTimelineEvent) error {
	s.mu.Lock()
	if err := s.admit(kindEvent, classData, e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitEvent(e)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release(classData)
		s.statsFor(kindEvent).DroppedDownstream++
		return err
	}
	s.statsFor(kindEvent).Accepted++
	return nil
}

// SnapshotWith calls tl.Snapshot() and embeds s's own SinkStats into the
// result - review Important 2: Timeline has no reference to the
// CountingSink wrapping it (EventSink is the only contract between them),
// so it cannot fill Snapshot.SinkStats in on its own. A caller that wires
// producer -> CountingSink -> Timeline the way this package's own
// conformance harness does should call this instead of tl.Snapshot()
// directly whenever the result will be serialized or inspected for loss
// accounting, so admission-side losses (a full token bucket, a rejected
// clock domain) are reachable from the same Snapshot as everything else.
func (s *CountingSink) SnapshotWith(tl *Timeline) Snapshot {
	snap := tl.Snapshot()
	snap.SinkStats = s.Stats()
	return snap
}
