package gpu

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSink struct {
	launches int
	execs    int
}

func (r *recordingSink) EmitLaunch(GPUKernelLaunch) error { r.launches++; return nil }
func (r *recordingSink) EmitExec(GPUKernelExec) error     { r.execs++; return nil }
func (r *recordingSink) EmitPCSample(GPUPCSample) error   { return nil }
func (r *recordingSink) EmitModule(GPUModule) error       { return nil }
func (r *recordingSink) EmitEvent(GPUTimelineEvent) error { return nil }

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
