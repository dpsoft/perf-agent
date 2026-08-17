package gpu

import (
	"errors"
	"fmt"
	"sync"
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
}

// CountingSink wraps a sink with admission control and accounting. capacity is
// the total accepted-event budget; zero means unbounded.
type CountingSink struct {
	mu       sync.Mutex
	inner    EventSink
	capacity int
	accepted int
	stats    SinkStats
}

func NewCountingSink(inner EventSink, capacity int) *CountingSink {
	return &CountingSink{inner: inner, capacity: capacity}
}

func (s *CountingSink) Stats() SinkStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// admit applies the clock-domain contract and the capacity bound. It returns
// nil when the event may proceed, and has already counted the drop otherwise.
func (s *CountingSink) admit(domain ClockDomain) error {
	if err := ValidateSupportedClockDomain(domain); err != nil {
		s.stats.DroppedInvalid++
		return fmt.Errorf("gpu: rejected event: %w", err)
	}
	if s.capacity > 0 && s.accepted >= s.capacity {
		s.stats.DroppedFull++
		return ErrSinkFull
	}
	s.accepted++
	return nil
}

func (s *CountingSink) EmitLaunch(l GPUKernelLaunch) error {
	s.mu.Lock()
	if err := s.admit(l.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.Launches++
	s.mu.Unlock()
	return s.inner.EmitLaunch(l)
}

func (s *CountingSink) EmitExec(e GPUKernelExec) error {
	s.mu.Lock()
	if err := s.admit(e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.Execs++
	s.mu.Unlock()
	return s.inner.EmitExec(e)
}

func (s *CountingSink) EmitPCSample(p GPUPCSample) error {
	s.mu.Lock()
	if err := s.admit(p.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.PCSamples++
	s.mu.Unlock()
	return s.inner.EmitPCSample(p)
}

func (s *CountingSink) EmitModule(m GPUModule) error {
	s.mu.Lock()
	if s.capacity > 0 && s.accepted >= s.capacity {
		s.stats.DroppedFull++
		s.mu.Unlock()
		return ErrSinkFull
	}
	s.accepted++
	s.stats.Modules++
	s.mu.Unlock()
	return s.inner.EmitModule(m)
}

func (s *CountingSink) EmitEvent(e GPUTimelineEvent) error {
	s.mu.Lock()
	if err := s.admit(e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.Events++
	s.mu.Unlock()
	return s.inner.EmitEvent(e)
}
