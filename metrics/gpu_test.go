package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONLDeviceRecordsAndUnknownValues(t *testing.T) {
	var out bytes.Buffer
	e := GPUJSONLExporter{Writer: &out}
	s := &MetricsSnapshot{GPU: &GPUMetrics{Kind: "initial", Devices: []GPUDeviceMetrics{
		{UUID: "GPU-a", Values: map[string]float64{"gpu.utilization_ratio": 0}, Unavailable: map[string]string{"gpu.temperature_celsius": "NOT_SUPPORTED"}},
		{UUID: "GPU-b", Values: map[string]float64{"gpu.temperature_celsius": 50}},
	}}}
	require.NoError(t, e.Export(t.Context(), s))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	var first struct {
		Schema int              `json:"schema_version"`
		Scope  string           `json:"scope"`
		Device GPUDeviceMetrics `json:"device"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.Equal(t, 1, first.Schema)
	require.Equal(t, "device", first.Scope)
	require.Equal(t, 0.0, first.Device.Values["gpu.utilization_ratio"])
	require.Contains(t, first.Device.Values, "gpu.utilization_ratio")
	require.NotContains(t, first.Device.Values, "gpu.temperature_celsius")
	require.Equal(t, "NOT_SUPPORTED", first.Device.Unavailable["gpu.temperature_celsius"])
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestGPUExportWriteErrors(t *testing.T) {
	s := &MetricsSnapshot{GPU: &GPUMetrics{Kind: "summary"}}
	require.ErrorContains(t, (&GPUJSONLExporter{Writer: failedWriter{}}).Export(t.Context(), s), "disk full")
	require.ErrorContains(t, (&ConsoleExporter{Writer: failedWriter{}}).Export(t.Context(), s), "disk full")
}

func TestGPUConsoleSummaryAndExistingPMUOutput(t *testing.T) {
	var out bytes.Buffer
	e := ConsoleExporter{Writer: &out}
	require.NoError(t, e.Export(t.Context(), &MetricsSnapshot{GPU: &GPUMetrics{Kind: "initial"}}))
	require.Empty(t, out.String())
	require.NoError(t, e.Export(t.Context(), &MetricsSnapshot{GPU: &GPUMetrics{Kind: "summary", Devices: []GPUDeviceMetrics{{UUID: "GPU-a", Summary: map[string]GaugeSummary{"gpu.power_watts": {Count: 2, Min: 10, Max: 20, Mean: 15, Last: 20}}}}}}))
	require.Contains(t, out.String(), "GPU-a")
	require.Contains(t, out.String(), "mean=15")
	require.NotContains(t, out.String(), "No PMU")
	out.Reset()
	require.NoError(t, e.Export(t.Context(), NewMetricsSnapshot(false)))
	require.Contains(t, out.String(), "No PMU metrics collected")
	require.NoError(t, (&GPUJSONLExporter{Writer: io.Discard}).Export(t.Context(), NewMetricsSnapshot(false)))
}
