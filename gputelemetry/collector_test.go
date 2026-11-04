package gputelemetry

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
	"github.com/stretchr/testify/require"
)

type fakeSource struct {
	devices []Device
	openErr error
	read    func(Device) metrics.GPUDeviceMetrics
	closed  int
}

func (s *fakeSource) Open() ([]Device, error)                { return s.devices, s.openErr }
func (s *fakeSource) Read(d Device) metrics.GPUDeviceMetrics { return s.read(d) }
func (s *fakeSource) Close() error                           { s.closed++; return nil }

type recordingExporter struct {
	snapshots []*metrics.MetricsSnapshot
	err       error
}

func (e *recordingExporter) Name() string { return "record" }
func (e *recordingExporter) Export(_ context.Context, s *metrics.MetricsSnapshot) error {
	e.snapshots = append(e.snapshots, s)
	return e.err
}

func TestFinalSampleAndSummaryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	source := &fakeSource{devices: []Device{{UUID: "GPU-a"}, {UUID: "GPU-b"}}}
	source.read = func(d Device) metrics.GPUDeviceMetrics {
		require.Equal(t, "GPU-b", d.UUID)
		calls++
		cancel()
		return metrics.GPUDeviceMetrics{UUID: d.UUID, Timestamp: time.Now(), Values: map[string]float64{"power": float64(calls-1) * 10}, Unavailable: map[string]string{"temperature": "NOT_SUPPORTED"}}
	}
	e := &recordingExporter{}
	c := Collector{Source: source, UUIDs: []string{"GPU-b", "GPU-b"}, Interval: time.Second, Exporters: []metrics.Exporter{e}}
	require.NoError(t, c.Run(ctx))
	require.Equal(t, 1, source.closed)
	require.Equal(t, 2, calls)
	require.Len(t, e.snapshots, 3)
	require.Equal(t, "initial", e.snapshots[0].GPU.Kind)
	require.Equal(t, "final", e.snapshots[1].GPU.Kind)
	s := e.snapshots[2].GPU
	require.Equal(t, "summary", s.Kind)
	require.False(t, s.EndedAt.Before(s.StartedAt))
	require.Equal(t, metrics.GaugeSummary{Count: 2, Min: 0, Max: 10, Mean: 5, Last: 10}, s.Devices[0].Summary["power"])
	require.Equal(t, uint64(2), s.Devices[0].FailedReadings["temperature"])
	require.NotContains(t, s.Devices[0].Summary, "temperature")
	require.NotEmpty(t, e.snapshots[0].GPU.Devices[0].Values, "summary must not mutate previously exported readings")
}

func TestUnavailableSourceIsExplicit(t *testing.T) {
	var output bytes.Buffer
	s := &fakeSource{openErr: errors.New("missing NVML")}
	c := Collector{Source: s, Interval: time.Second, Exporters: []metrics.Exporter{&metrics.GPUJSONLExporter{Writer: &output}}}
	require.ErrorContains(t, c.Run(t.Context()), "missing NVML")
	require.Contains(t, output.String(), `"kind":"unavailable"`)
	require.Contains(t, output.String(), `"error":"missing NVML"`)
	require.NotContains(t, output.String(), `"values"`)
	require.Zero(t, s.closed, "an unsuccessful Open owns its cleanup")
}

func TestExporterErrorStopsSamplingAndClosesSource(t *testing.T) {
	s := &fakeSource{devices: []Device{{UUID: "GPU-a"}}, read: func(d Device) metrics.GPUDeviceMetrics { return metrics.GPUDeviceMetrics{UUID: d.UUID} }}
	e := &recordingExporter{err: errors.New("disk full")}
	c := Collector{Source: s, Interval: time.Second, Exporters: []metrics.Exporter{e}}
	require.ErrorContains(t, c.Run(t.Context()), "disk full")
	require.Equal(t, 1, s.closed)
	require.Len(t, e.snapshots, 1)
}

func TestUnknownUUIDDoesNotSampleOtherDevices(t *testing.T) {
	s := &fakeSource{devices: []Device{{UUID: "GPU-a"}}}
	e := &recordingExporter{}
	c := Collector{Source: s, UUIDs: []string{"GPU-missing"}, Interval: time.Second, Exporters: []metrics.Exporter{e}}
	require.ErrorContains(t, c.Run(t.Context()), "not found")
	require.Equal(t, 1, s.closed)
	require.Equal(t, "unavailable", e.snapshots[0].GPU.Kind)
}

func TestSlowSamplingCountsMissedTicksWithoutBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	s := &fakeSource{devices: []Device{{UUID: "GPU-a"}}}
	s.read = func(d Device) metrics.GPUDeviceMetrics {
		calls++
		if calls == 1 {
			time.Sleep(15 * time.Millisecond)
		} else {
			cancel()
		}
		return metrics.GPUDeviceMetrics{UUID: d.UUID, Timestamp: time.Now()}
	}
	e := &recordingExporter{}
	c := Collector{Source: s, Interval: time.Millisecond, Exporters: []metrics.Exporter{e}}
	require.NoError(t, c.Run(ctx))
	require.Equal(t, 3, calls)
	require.GreaterOrEqual(t, e.snapshots[len(e.snapshots)-1].GPU.MissedTicks, uint64(10))
}

func TestMissedTicksIncludeReadThatCrossesDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	s := &fakeSource{devices: []Device{{UUID: "GPU-a"}}}
	s.read = func(d Device) metrics.GPUDeviceMetrics {
		time.Sleep(20 * time.Millisecond)
		return metrics.GPUDeviceMetrics{UUID: d.UUID, Timestamp: time.Now()}
	}
	e := &recordingExporter{}
	c := Collector{Source: s, Interval: time.Millisecond, Exporters: []metrics.Exporter{e}}
	require.NoError(t, c.Run(ctx))
	last := e.snapshots[len(e.snapshots)-1].GPU
	require.GreaterOrEqual(t, last.MissedTicks, uint64(8))
	require.LessOrEqual(t, last.MissedTicks, uint64(10))
	require.Less(t, last.EndedAt.Sub(last.StartedAt), 15*time.Millisecond, "final read must not extend capture window")
}
