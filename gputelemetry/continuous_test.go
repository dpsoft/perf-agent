package gputelemetry

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
	"github.com/stretchr/testify/require"
)

type recoveringSource struct {
	fakeSource
	opens int
}

func (s *recoveringSource) Open() ([]Device, error) {
	s.opens++
	if s.opens == 1 {
		return nil, errors.New("driver unavailable")
	}
	return []Device{{UUID: "GPU-a"}}, nil
}
func TestContinuousRecoversAndResetsSummaryWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &recoveringSource{}
	s.read = func(d Device) metrics.GPUDeviceMetrics {
		if s.opens == 4 {
			cancel()
		}
		return metrics.GPUDeviceMetrics{UUID: d.UUID, Timestamp: time.Now(), Values: map[string]float64{"power": float64(s.opens)}}
	}
	e := &recordingExporter{}
	c := Collector{Source: s, Interval: time.Hour, Exporters: []metrics.Exporter{e}}
	require.NoError(t, c.runContinuous(ctx, 5*time.Millisecond, time.Millisecond, 2*time.Millisecond))
	require.Equal(t, 4, s.opens)
	require.Equal(t, 3, s.closed)
	require.Equal(t, "unavailable", e.snapshots[0].GPU.Kind)
	summaries := 0
	for _, snapshot := range e.snapshots {
		if snapshot.GPU.Kind != "summary" {
			continue
		}
		summaries++
		g := snapshot.GPU.Devices[0].Summary["power"]
		require.Equal(t, uint64(2), g.Count)
		require.Equal(t, float64(summaries+1), g.Mean)
	}
	require.Equal(t, 3, summaries)
}
func TestContinuousDoesNotRetryLocalWriteFailures(t *testing.T) {
	s := &recoveringSource{}
	e := &recordingExporter{err: errors.New("disk full")}
	c := Collector{Source: s, Interval: time.Second, Exporters: []metrics.Exporter{e}}
	require.ErrorContains(t, c.RunContinuous(t.Context(), time.Second), "disk full")
	require.Equal(t, 1, s.opens)
}
func TestContinuousCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s := &recoveringSource{}
	c := Collector{Source: s, Interval: time.Second, Exporters: []metrics.Exporter{&recordingExporter{}}}
	require.NoError(t, c.RunContinuous(ctx, time.Minute))
	require.Zero(t, s.opens)
}

type backoffSource struct {
	opened []time.Time
	cancel context.CancelFunc
}

func (s *backoffSource) Open() ([]Device, error) {
	s.opened = append(s.opened, time.Now())
	if len(s.opened) <= 4 {
		return nil, errors.New("same initialization failure")
	}
	return []Device{{UUID: "GPU-a"}}, nil
}
func (s *backoffSource) Read(d Device) metrics.GPUDeviceMetrics {
	s.cancel()
	return metrics.GPUDeviceMetrics{UUID: d.UUID, Timestamp: time.Now()}
}
func (*backoffSource) Close() error { return nil }
func TestContinuousBackoffDoublesAndCaps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := &backoffSource{cancel: cancel}
		c := Collector{Source: source, Interval: time.Second, Exporters: []metrics.Exporter{&recordingExporter{}}}
		require.NoError(t, c.runContinuous(ctx, time.Minute, time.Second, 4*time.Second))
		require.Len(t, source.opened, 5)
		for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
			require.Equal(t, want, source.opened[i+1].Sub(source.opened[i]))
		}
	})
}
