package interp

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cilium/ebpf"
)

// fakeModule refuses with ErrRetryable until the Nth Enroll, then succeeds --
// which is the shape a real interpreter has on a launch-and-enrol path.
type fakeModule struct {
	mu        sync.Mutex
	calls     int
	succeedAt int
}

func (f *fakeModule) ID() uint32                  { return 1 }
func (f *fakeModule) Name() string                { return "fake" }
func (f *fakeModule) ProgramName(Flavour) string  { return "" }
func (f *fakeModule) Bind(*ebpf.Collection) error { return nil }
func (f *fakeModule) Detach(uint32) error         { return nil }
func (f *fakeModule) Counters(bool) string        { return "" }
func (f *fakeModule) Close() error                { return nil }

func (f *fakeModule) Spec() (*ebpf.CollectionSpec, error) {
	return nil, errors.New("unused")
}

func (f *fakeModule) Enroll(uint32) (Range, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls < f.succeedAt {
		return Range{}, true, fmt.Errorf("%w: no thread holds state yet", ErrRetryable)
	}
	return Range{TableID: 7, Spans: []Span{{Lo: 1, Hi: 2}}}, true, nil
}

func (f *fakeModule) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// A retryable refusal must not end the enrolment.
//
// MEASURED ON A LIVE PYTORCH PROCESS, launched as the profiler's own child so
// the leader is excluded exactly as the GPU path excludes it:
//
//	t=    0ms  threads=1   candidates=0   qualifying=-1
//	t= 1310ms  threads=28  candidates=27  qualifying=-1
//	  ... every look refuses, for 26 seconds ...
//	t=26294ms  threads=29  candidates=28  qualifying=27   SUCCESS
//
// The first look CANNOT succeed: the application's own Python thread does not
// exist yet, and the main thread -- which would have answered -- is excluded
// because stopping our own child's leader corrupts os/exec's bookkeeping. So
// without a retry the GPU path can never enrol a target it launched itself.
//
// The same run is why the thread search is bounded by TIME rather than by a
// count of 16: the qualifying thread was number 27 of 28.
func TestARetryableRefusalIsRetriedUntilItSucceeds(t *testing.T) {
	prev := retrySchedule
	retrySchedule = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	defer func() { retrySchedule = prev }()

	f := &fakeModule{succeedAt: 3}
	s := &Set{stop: make(chan struct{}), entries: []*entry{{mod: f}}}
	defer func() { _ = s.Close() }()

	if !s.Enroll(1234, func(string, ...any) {}) {
		t.Fatal("Enroll reported the process as unrecognised")
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := f.count(); n < 3 {
		t.Fatalf("module was asked %d times; a retryable refusal ended the enrolment", n)
	}
}

// Close must stop retries PROMPTLY and also wait for them: they write into the
// driver's maps, which the caller closes the moment Close returns. A retry
// asleep on the schedule must not hold shutdown for the length of that sleep.
func TestCloseStopsRetriesPromptly(t *testing.T) {
	prev := retrySchedule
	retrySchedule = []time.Duration{time.Hour}
	defer func() { retrySchedule = prev }()

	f := &fakeModule{succeedAt: 1 << 30} // never succeeds
	s := &Set{stop: make(chan struct{}), entries: []*entry{{mod: f}}}
	s.Enroll(1234, func(string, ...any) {})

	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return: a retry sleeping on the schedule blocks shutdown")
	}
}

// A Set closed WITHOUT anyone rendering its counters must render them anyway.
//
// This is the structural half of a lesson that cost four debugging sessions on
// this branch: three of them were a path that HAD the counters and never
// printed them. Most recently the GPU driver held a Set, filled its counters,
// and closed it in silence while the run produced no interpreter frames --
// leaving three different bugs indistinguishable. Remembering to call
// LogCounters on every path is not a mechanism; this is.
func TestCloseRendersCountersNobodyElseDid(t *testing.T) {
	var lines []string
	s := &Set{stop: make(chan struct{})}
	s.logSink = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("Close rendered nothing: a driver can hold a Set, fill its counters, and say nothing")
	}
}

// And a caller that renders them itself is not doubled up on: its line is
// better (it knows its own prefix and whether the target was enrolled).
func TestCloseDoesNotRepeatCountersACallerAlreadyRendered(t *testing.T) {
	var closeLines, callerLines int
	s := &Set{stop: make(chan struct{})}
	s.logSink = func(string, ...any) { closeLines++ }

	s.LogCounters(true, func(string, ...any) { callerLines++ })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if callerLines == 0 {
		t.Fatal("the caller's own LogCounters rendered nothing")
	}
	if closeLines != 0 {
		t.Errorf("Close rendered %d more lines after the caller had already done it", closeLines)
	}
}
