package metrics

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func reportReading(id string, at time.Time, value float64) *MetricsSnapshot {
	return &MetricsSnapshot{GPU: &GPUMetrics{CaptureID: id, Kind: "sample", Interval: time.Second, Devices: []GPUDeviceMetrics{{UUID: "GPU-a", Timestamp: at, Values: map[string]float64{"gpu.power_watts": value}}}}}
}

func TestGPUReportFiltersWindowAndIgnoresContinuousSummaries(t *testing.T) {
	start := time.Unix(1000, 0)
	end := start.Add(3 * time.Second)
	c := NewGPUReportCollector("capture", start, end)
	for _, v := range []struct {
		offset time.Duration
		value  float64
	}{{-time.Second, 999}, {0, 0}, {time.Second, 10}, {3 * time.Second, 20}, {4 * time.Second, 999}} {
		require.NoError(t, c.Export(t.Context(), reportReading("capture", start.Add(v.offset), v.value)))
	}
	require.NoError(t, c.Export(t.Context(), &MetricsSnapshot{GPU: &GPUMetrics{CaptureID: "capture", Kind: "summary", Devices: []GPUDeviceMetrics{{UUID: "GPU-a", Summary: map[string]GaugeSummary{"gpu.power_watts": {Count: 100, Mean: 999}}}}}}))
	report := c.Finish()
	require.Len(t, report.Devices, 1)
	s := report.Devices[0].Metrics["gpu.power_watts"]
	require.Equal(t, uint64(3), s.Count)
	require.Equal(t, 10.0, s.Mean)
	require.Equal(t, 0.0, s.Min)
	require.Equal(t, 20.0, s.Last)
	require.Equal(t, end, s.LastAt)
}

func TestGPUReportMissingIsNotZeroAndUsesPerFieldCounts(t *testing.T) {
	start := time.Unix(1000, 0)
	c := NewGPUReportCollector("capture", start, start.Add(time.Minute))
	require.NoError(t, c.Export(t.Context(), reportReading("capture", start, 20)))
	require.NoError(t, c.Export(t.Context(), &MetricsSnapshot{GPU: &GPUMetrics{CaptureID: "capture", Kind: "sample", Devices: []GPUDeviceMetrics{{UUID: "GPU-a", Timestamp: start.Add(time.Second), Values: map[string]float64{"gpu.temperature_celsius": 50}, Unavailable: map[string]string{"gpu.power_watts": "GPU_IS_LOST"}, Unsupported: map[string]string{"gpu.clock_event_active.board_limit": "NOT_SUPPORTED"}}}}}))
	d := c.Finish().Devices[0]
	require.Equal(t, uint64(2), d.Readings)
	require.Equal(t, uint64(1), d.Metrics["gpu.power_watts"].Count)
	require.Equal(t, 20.0, d.Metrics["gpu.power_watts"].Mean)
	require.Equal(t, uint64(1), d.FailedReadings["gpu.power_watts"])
	require.NotContains(t, d.Metrics, "gpu.clock_event_active.board_limit")
}

func TestGPUReportStopSeparatesContinuousExporterLifetime(t *testing.T) {
	start := time.Now().Add(-time.Second)
	c := NewGPUReportCollector("capture", time.Time{}, time.Time{})
	require.NoError(t, c.Begin("capture", start))
	require.NoError(t, c.Export(t.Context(), reportReading("capture", start, 10)))
	c.Stop()
	end := time.Now()
	// The continuous source keeps publishing, but this finite consumer is closed.
	require.NoError(t, c.Export(t.Context(), reportReading("capture", end.Add(time.Second), 999)))
	r, err := c.FinishAt(end)
	require.NoError(t, err)
	require.Equal(t, end, r.EndedAt)
	require.Equal(t, 10.0, r.Devices[0].Metrics["gpu.power_watts"].Mean)
	r.Devices[0].Metrics["gpu.power_watts"] = GPUFieldSummary{}
	again, err := c.FinishAt(end)
	require.NoError(t, err)
	require.Equal(t, uint64(1), again.Devices[0].Metrics["gpu.power_watts"].Count)
}

func TestGPUReportRejectsAmbiguousOrInvalidReadings(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, tc := range []struct {
		name   string
		second *MetricsSnapshot
		want   string
	}{
		{"wrong capture", reportReading("different", start.Add(time.Second), 2), "capture_id"},
		{"duplicate", reportReading("capture", start, 2), "increasing"},
		{"nonfinite", reportReading("capture", start.Add(time.Second), math.Inf(1)), "non-finite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewGPUReportCollector("capture", start, start.Add(time.Minute))
			require.NoError(t, c.Export(t.Context(), reportReading("capture", start, 1)))
			require.ErrorContains(t, c.Export(t.Context(), tc.second), tc.want)
		})
	}
}

func TestLiveGPUReportValidationDoesNotStopOtherExporters(t *testing.T) {
	start := time.Unix(1000, 0)
	c := NewGPUReportCollector("capture", start, start.Add(time.Minute))
	var jsonl bytes.Buffer
	exporters := []Exporter{c.LiveExporter(), &GPUJSONLExporter{Writer: &jsonl}}
	for range 2 {
		for _, e := range exporters {
			require.NoError(t, e.Export(t.Context(), reportReading("capture", start, 10)))
		}
	}
	require.Equal(t, 2, strings.Count(jsonl.String(), "\n"), "duplicate timestamp must still reach JSONL")
	report := c.Finish()
	require.Equal(t, uint64(1), report.Devices[0].Readings)
	require.Contains(t, strings.Join(report.Errors, " "), "omitted")
}

// Begun closes when Begin binds the report to its capture, and a repeated
// Begin does not close it twice.
func TestBegunClosesOnBegin(t *testing.T) {
	c := NewGPUReportCollector("cap", time.Time{}, time.Time{})
	select {
	case <-c.Begun():
		t.Fatal("Begun closed before Begin")
	default:
	}
	if err := c.Begin("cap", time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Begun():
	default:
		t.Fatal("Begun still open after Begin")
	}
	_ = c.Begin("cap", time.Now()) // must not panic
}
