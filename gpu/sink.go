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

// SinkStats is the ingestion-side loss record. JoinStats reports what could not
// be correlated; this reports what never arrived.
type SinkStats struct {
	Launches  uint64 `json:"launches,omitempty"`
	Execs     uint64 `json:"execs,omitempty"`
	PCSamples uint64 `json:"pc_samples,omitempty"`
	Modules   uint64 `json:"modules,omitempty"`
	Events    uint64 `json:"events,omitempty"`

	DroppedFull    uint64 `json:"dropped_full,omitempty"`
	DroppedInvalid uint64 `json:"dropped_invalid,omitempty"`

	// DroppedDownstream counts events that passed admission (clock domain
	// and capacity) but that inner failed to accept. They are not counted in
	// the per-type accepted counters above, and the capacity they reserved
	// is returned to the bucket - a downstream hiccup must not permanently
	// shrink what the sink can admit.
	DroppedDownstream uint64 `json:"dropped_downstream,omitempty"`
}

// CountingSink wraps a sink with admission control and accounting. Admission
// is a token bucket: burst is the maximum number of events admitted at once,
// refilled continuously at refillRate tokens/second. capacity <= 0 means
// unbounded - no admission limit is ever applied.
//
// A lifetime budget that only ever decreases would be wrong for a
// continuously-running profiler: once spent it would reject forever. The
// token bucket recovers over time instead, the way a rate limiter does.
type CountingSink struct {
	mu    sync.Mutex
	inner EventSink

	unbounded  bool
	burst      float64
	tokens     float64
	refillRate float64 // tokens per second
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
		s.unbounded = true
	} else {
		s.burst = float64(capacity)
		s.tokens = float64(capacity)
	}
	return s
}

func (s *CountingSink) Stats() SinkStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// refill adds tokens for elapsed time since the last refill, capped at
// burst. Caller holds mu.
func (s *CountingSink) refill() {
	if s.unbounded {
		return
	}
	now := s.now()
	elapsed := now.Sub(s.lastRefill)
	if elapsed <= 0 {
		return
	}
	s.lastRefill = now
	s.tokens += elapsed.Seconds() * s.refillRate
	if s.tokens > s.burst {
		s.tokens = s.burst
	}
}

// admitCapacity applies the token-bucket bound only - the one rule EmitModule
// shares with every other event type. On success it has reserved one token;
// call release if the reservation turns out not to be used. Caller holds mu.
func (s *CountingSink) admitCapacity() error {
	if s.unbounded {
		return nil
	}
	s.refill()
	if s.tokens < 1 {
		s.stats.DroppedFull++
		return ErrSinkFull
	}
	s.tokens--
	return nil
}

// release returns a reserved token after inner fails to accept a delegated
// event, so a downstream rejection does not permanently shrink capacity.
// Caller holds mu.
func (s *CountingSink) release() {
	if s.unbounded {
		return
	}
	s.tokens++
	if s.tokens > s.burst {
		s.tokens = s.burst
	}
}

// admit applies the clock-domain contract, then the capacity bound. Caller
// holds mu. EmitModule bypasses this and calls admitCapacity directly: a
// GPUModule has no ClockDomain field to validate.
func (s *CountingSink) admit(domain ClockDomain) error {
	if err := ValidateSupportedClockDomain(domain); err != nil {
		s.stats.DroppedInvalid++
		return fmt.Errorf("gpu: rejected event: %w", err)
	}
	return s.admitCapacity()
}

func (s *CountingSink) EmitLaunch(l GPUKernelLaunch) error {
	s.mu.Lock()
	if err := s.admit(l.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitLaunch(l)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release()
		s.stats.DroppedDownstream++
		return err
	}
	s.stats.Launches++
	return nil
}

func (s *CountingSink) EmitExec(e GPUKernelExec) error {
	s.mu.Lock()
	if err := s.admit(e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitExec(e)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release()
		s.stats.DroppedDownstream++
		return err
	}
	s.stats.Execs++
	return nil
}

func (s *CountingSink) EmitPCSample(p GPUPCSample) error {
	s.mu.Lock()
	if err := s.admit(p.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitPCSample(p)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release()
		s.stats.DroppedDownstream++
		return err
	}
	s.stats.PCSamples++
	return nil
}

// EmitModule deliberately skips the clock-domain check: GPUModule is
// symbolization metadata with no ClockDomain field. It still goes through
// the same token-bucket admission (admitCapacity) and the same
// reserve/delegate/settle sequence as every other event type.
func (s *CountingSink) EmitModule(m GPUModule) error {
	s.mu.Lock()
	if err := s.admitCapacity(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitModule(m)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release()
		s.stats.DroppedDownstream++
		return err
	}
	s.stats.Modules++
	return nil
}

func (s *CountingSink) EmitEvent(e GPUTimelineEvent) error {
	s.mu.Lock()
	if err := s.admit(e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	err := s.inner.EmitEvent(e)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.release()
		s.stats.DroppedDownstream++
		return err
	}
	s.stats.Events++
	return nil
}
