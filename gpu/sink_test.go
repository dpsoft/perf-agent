package gpu

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSink struct {
	launches  int
	execs     int
	pcSamples int
	modules   int
	events    int
}

func (r *recordingSink) EmitLaunch(GPUKernelLaunch) error { r.launches++; return nil }
func (r *recordingSink) EmitExec(GPUKernelExec) error     { r.execs++; return nil }
func (r *recordingSink) EmitPCSample(GPUPCSample) error   { r.pcSamples++; return nil }
func (r *recordingSink) EmitModule(GPUModule) error       { r.modules++; return nil }
func (r *recordingSink) EmitEvent(GPUTimelineEvent) error { r.events++; return nil }

// erroringSink lets a test control exactly what the downstream sink returns,
// to exercise the reserve/delegate/settle path when delivery fails.
type erroringSink struct {
	err error
}

func (e *erroringSink) EmitLaunch(GPUKernelLaunch) error { return e.err }
func (e *erroringSink) EmitExec(GPUKernelExec) error     { return e.err }
func (e *erroringSink) EmitPCSample(GPUPCSample) error   { return e.err }
func (e *erroringSink) EmitModule(GPUModule) error       { return e.err }
func (e *erroringSink) EmitEvent(GPUTimelineEvent) error { return e.err }

// atomicSink is a genuinely concurrency-safe EventSink, used only by the
// concurrent-access test so a data race in the test double itself can never
// be mistaken for a race in CountingSink.
type atomicSink struct {
	launches atomic.Int64
}

func (a *atomicSink) EmitLaunch(GPUKernelLaunch) error { a.launches.Add(1); return nil }
func (a *atomicSink) EmitExec(GPUKernelExec) error     { return nil }
func (a *atomicSink) EmitPCSample(GPUPCSample) error   { return nil }
func (a *atomicSink) EmitModule(GPUModule) error       { return nil }
func (a *atomicSink) EmitEvent(GPUTimelineEvent) error { return nil }

// fakeClock is an injectable, manually-advanced clock so the token-bucket
// refill can be tested deterministically instead of racing the wall clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func TestCountingSinkForwardsAndCounts(t *testing.T) {
	inner := &recordingSink{}
	s := NewCountingSink(inner, 10)

	require.NoError(t, s.EmitLaunch(launch("a", 10)))
	require.NoError(t, s.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		StartNs:     20, EndNs: 30,
	}))

	assert.Equal(t, 1, inner.launches)
	assert.Equal(t, 1, inner.execs)
	assert.Equal(t, uint64(1), s.Stats().Launches)
	assert.Equal(t, uint64(1), s.Stats().Execs)
}

func TestCountingSinkReturnsErrSinkFullAtCapacity(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 2)

	require.NoError(t, s.EmitLaunch(launch("a", 10)))
	require.NoError(t, s.EmitLaunch(launch("b", 20)))
	err := s.EmitLaunch(launch("c", 30))

	require.Error(t, err, "a sink at capacity must push back, not absorb silently")
	assert.True(t, errors.Is(err, ErrSinkFull))
	assert.Equal(t, uint64(1), s.Stats().DroppedFull, "the drop must be counted")
}

func TestCountingSinkRejectsUnsupportedClockDomain(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 10)

	l := launch("a", 10)
	l.ClockDomain = ClockDomainGPUDevice
	err := s.EmitLaunch(l)

	require.Error(t, err, "producers must convert device clocks before emitting")
	assert.Equal(t, uint64(1), s.Stats().DroppedInvalid)
	assert.Equal(t, uint64(0), s.Stats().Launches, "a rejected event is not counted as accepted")
}

func TestCountingSinkZeroCapacityIsUnbounded(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 0)
	for i := 0; i < 1000; i++ {
		require.NoError(t, s.EmitLaunch(launch("x", uint64(i))))
	}
	assert.Equal(t, uint64(0), s.Stats().DroppedFull)
}

// TestCountingSinkDownstreamFailureIsNotCountedAsAccepted pins the
// reserve/delegate/settle contract: a delivery inner rejects must not be
// credited to the per-type accepted counter, must be visible in
// DroppedDownstream, and must not permanently consume the sink's capacity -
// the reserved slot has to come back so a downstream hiccup doesn't
// masquerade as a shrinking budget.
func TestCountingSinkDownstreamFailureIsNotCountedAsAccepted(t *testing.T) {
	innerErr := errors.New("downstream unavailable")
	inner := &erroringSink{err: innerErr}
	s := NewCountingSink(inner, 2)

	err := s.EmitLaunch(launch("a", 10))

	require.Error(t, err)
	assert.True(t, errors.Is(err, innerErr), "the downstream error must reach the caller")
	assert.Equal(t, uint64(0), s.Stats().Launches, "a delivery inner rejects is not an accepted event")
	assert.Equal(t, uint64(1), s.Stats().DroppedDownstream)

	// The reservation must have been released: two more emissions still fit
	// in a capacity-2 sink even though the first attempt never delivered.
	inner.err = nil
	require.NoError(t, s.EmitLaunch(launch("b", 20)))
	require.NoError(t, s.EmitLaunch(launch("c", 30)))
	err = s.EmitLaunch(launch("d", 40))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSinkFull))
}

// TestCountingSinkTokenBucketRefillsOverTime pins the fix for the lifetime-
// budget defect: capacity must recover over time rather than expiring for
// the life of the process. A fake clock makes the recovery deterministic.
func TestCountingSinkTokenBucketRefillsOverTime(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	s := NewCountingSinkWithRate(&recordingSink{}, 1, 1, clock.Now) // burst 1, 1 token/sec

	require.NoError(t, s.EmitLaunch(launch("a", 10)), "the initial burst token must be available")

	err := s.EmitLaunch(launch("b", 20))
	require.Error(t, err, "the burst is exhausted and no time has passed to refill it")
	assert.True(t, errors.Is(err, ErrSinkFull))

	clock.Advance(time.Second)

	require.NoError(t, s.EmitLaunch(launch("c", 30)),
		"a lifetime budget could never recover; a token bucket must after a full refill interval")
}

func TestCountingSinkEmitPCSampleForwardsCountsAndEnforcesCapacity(t *testing.T) {
	inner := &recordingSink{}
	s := NewCountingSink(inner, 1)

	require.NoError(t, s.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		TimeNs:      10,
	}))
	assert.Equal(t, 1, inner.pcSamples)
	assert.Equal(t, uint64(1), s.Stats().PCSamples)

	err := s.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "b"},
		TimeNs:      20,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSinkFull))
	assert.Equal(t, uint64(1), s.Stats().DroppedFull)
}

func TestCountingSinkEmitEventForwardsCountsAndEnforcesCapacity(t *testing.T) {
	inner := &recordingSink{}
	s := NewCountingSink(inner, 1)

	require.NoError(t, s.EmitEvent(GPUTimelineEvent{
		Backend: BackendCUPTI, Kind: TimelineEventRuntime, TimeNs: 10,
	}))
	assert.Equal(t, 1, inner.events)
	assert.Equal(t, uint64(1), s.Stats().Events)

	err := s.EmitEvent(GPUTimelineEvent{
		Backend: BackendCUPTI, Kind: TimelineEventRuntime, TimeNs: 20,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSinkFull))
	assert.Equal(t, uint64(1), s.Stats().DroppedFull)
}

// TestCountingSinkEmitModuleForwardsCountsAndEnforcesCapacity pins both
// halves of GPUModule's contract: it still enforces capacity like every
// other event type, but - because GPUModule carries no ClockDomain field -
// there is structurally nothing for a clock-domain check to reject, so
// admission for it is capacity-only.
func TestCountingSinkEmitModuleForwardsCountsAndEnforcesCapacity(t *testing.T) {
	inner := &recordingSink{}
	s := NewCountingSink(inner, 1)

	require.NoError(t, s.EmitModule(GPUModule{
		Ref:       ModuleRef{Backend: BackendCUPTI, CRC: 1},
		SizeBytes: 100,
		LoadedNs:  10,
	}))
	assert.Equal(t, 1, inner.modules)
	assert.Equal(t, uint64(1), s.Stats().Modules)

	err := s.EmitModule(GPUModule{Ref: ModuleRef{Backend: BackendCUPTI, CRC: 2}, LoadedNs: 20})
	require.Error(t, err, "capacity must still be enforced for modules")
	assert.True(t, errors.Is(err, ErrSinkFull))
	assert.Equal(t, uint64(1), s.Stats().DroppedFull)
}

// TestCountingSinkConcurrentEmitAndStats exercises CountingSink from many
// goroutines at once - the point a clean sequential -race run cannot make.
// It uses atomicSink so any race reported by -race can only be inside
// CountingSink itself, never in the test double.
func TestCountingSinkConcurrentEmitAndStats(t *testing.T) {
	inner := &atomicSink{}
	s := NewCountingSink(inner, 0) // unbounded: this test is about the lock, not the token bucket

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n + 1)

	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			// assert, not require: FailNow (which require calls on failure)
			// must only be called from the test's own goroutine, not a
			// spawned one.
			assert.NoError(t, s.EmitLaunch(launch("concurrent", uint64(i))))
		}()
	}
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			_ = s.Stats()
		}
	}()

	wg.Wait()

	assert.Equal(t, int64(n), inner.launches.Load())
	assert.Equal(t, uint64(n), s.Stats().Launches)
}

// TestCountingSinkAnchorClassSurvivesDataOverload is the regression test for
// review Important 5: a single, undifferentiated token bucket meant a flood
// of PC samples (high-volume, per spec §7/§12 plausibly the bulk of GPU
// event traffic) could exhaust the sink's entire admission budget, dropping
// launches exactly as readily as samples. Per review Critical 2, a dropped
// launch turns every subsequent exec referencing it into a permanent
// unattributed miss - losing an anchor is categorically worse than losing
// one data point, so anchors (launches, modules) must have their own budget
// that data-class volume can never touch.
//
// capacity 2 (2 tokens for classAnchor's bucket): exhaust the *data* class
// with PC samples first, then confirm launches still admit normally.
// Mutation this catches: reverting to one shared token bucket for every
// event kind - the launch below would then fail with ErrSinkFull because
// the PC-sample flood already spent the shared budget.
func TestCountingSinkAnchorClassSurvivesDataOverload(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 2)

	// Flood and exhaust the data class - well past its own capacity, to be
	// sure it is genuinely spent.
	for i := 0; i < 10; i++ {
		_ = s.EmitPCSample(GPUPCSample{Correlation: CorrelationID{Backend: BackendCUPTI, Value: "x"}, TimeNs: uint64(i)})
	}
	dataExhausted := s.EmitExec(GPUKernelExec{Correlation: CorrelationID{Backend: BackendCUPTI, Value: "y"}})
	require.Error(t, dataExhausted, "sanity: the data class must actually be exhausted by the flood above")
	assert.True(t, errors.Is(dataExhausted, ErrSinkFull))

	// Anchors must be completely unaffected: both of classAnchor's tokens
	// are still available.
	require.NoError(t, s.EmitLaunch(launch("a", 10)), "a launch must not be dropped because data-class volume exhausted its own bucket")
	require.NoError(t, s.EmitModule(GPUModule{Ref: ModuleRef{CRC: 1}, LoadedNs: 10}))

	assert.Equal(t, uint64(1), s.Stats().Launches)
	assert.Equal(t, uint64(1), s.Stats().Modules)
}
