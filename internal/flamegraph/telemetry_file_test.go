package flamegraph

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capturemeta"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
)

func telemetryFixture(t *testing.T) (capturemeta.Window, []byte) {
	t.Helper()
	c := capturemeta.Window{ID: "capture-test", StartedAt: time.Unix(1700000000, 0), EndedAt: time.Unix(1700000002, 0)}
	var buf bytes.Buffer
	e := metrics.GPUJSONLExporter{Writer: &buf}
	for _, v := range []struct {
		offset time.Duration
		value  float64
	}{{-time.Second, 900}, {0, 10}, {time.Second, 30}, {3 * time.Second, 900}} {
		require.NoError(t, e.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{CaptureID: c.ID, Kind: "sample", StartedAt: c.StartedAt, Interval: time.Second, Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-a", Name: "GPU <script>unsafe</script>", Timestamp: c.StartedAt.Add(v.offset), Values: map[string]float64{"gpu.power_watts": v.value}}}}}))
	}
	require.NoError(t, e.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{CaptureID: c.ID, Kind: "summary", StartedAt: c.StartedAt, EndedAt: c.EndedAt.Add(time.Hour), Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-a", Summary: map[string]metrics.GaugeSummary{"gpu.power_watts": {Count: 50, Mean: 999}}}}}}))
	return c, buf.Bytes()
}

func TestTelemetrySidecarOnlyUsesMatchingWindowObservations(t *testing.T) {
	c, data := telemetryFixture(t)
	r, err := readTelemetry(bytes.NewReader(data), c)
	require.NoError(t, err)
	require.Equal(t, uint64(2), r.Devices[0].Readings)
	require.Equal(t, 20.0, r.Devices[0].Metrics["gpu.power_watts"].Mean)
	c.StartedAt = c.EndedAt.Add(time.Hour)
	c.EndedAt = c.StartedAt.Add(time.Second)
	empty, err := readTelemetry(bytes.NewReader(data), c)
	require.NoError(t, err)
	require.Empty(t, empty.Devices, "must not use the continuous summary as a fallback")
}

func TestTelemetrySidecarRejectsForeignMalformedAndLegacyFiles(t *testing.T) {
	c, data := telemetryFixture(t)
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"foreign", strings.ReplaceAll(string(data), c.ID, "another"), "capture_id mismatch"},
		{"legacy", strings.ReplaceAll(string(data), `"capture_id":"capture-test",`, ""), "capture_id mismatch"},
		{"truncated", string(data[:len(data)-8]), "line"},
		{"schema", strings.ReplaceAll(string(data), `"schema_version":1`, `"schema_version":2`), "schema"},
		{"empty", "", "no records"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readTelemetry(strings.NewReader(tc.data), c)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestProfileAndTelemetryIdentityAndWindowAreRequired(t *testing.T) {
	c, data := telemetryFixture(t)
	r, err := readTelemetry(bytes.NewReader(data), c)
	require.NoError(t, err)
	p := &profile.Profile{TimeNanos: c.StartedAt.UnixNano(), DurationNanos: c.EndedAt.Sub(c.StartedAt).Nanoseconds(), Comments: []string{capturemeta.CaptureIDComment + c.ID}}
	require.NoError(t, attachTelemetry(&Options{GPUTelemetry: r}, p))
	p.DurationNanos++
	require.ErrorContains(t, attachTelemetry(&Options{GPUTelemetry: r}, p), "does not match")
	p.Comments = nil
	require.Error(t, attachTelemetry(&Options{GPUTelemetry: r}, p))
}

func TestGPUDataIsEmbeddedSafelyAndAbsentByDefault(t *testing.T) {
	c, data := telemetryFixture(t)
	r, err := readTelemetry(bytes.NewReader(data), c)
	require.NoError(t, err)
	// The JSON payload must remain data even if a GPU name contains HTML.
	var out bytes.Buffer
	ew := &errWriter{w: &out}
	writeGPUView(ew, Options{GPUTelemetry: r})
	require.NoError(t, ew.err)
	require.NotContains(t, out.String(), "<script>unsafe</script>")
	require.Contains(t, out.String(), `\u003cscript\u003e`)
	raw := strings.Split(strings.Split(out.String(), `id="gpu-data">`)[1], "</script>")[0]
	var decoded metrics.GPUReport
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	require.Equal(t, r.Devices[0].Name, decoded.Devices[0].Name)
	out.Reset()
	writeGPUView(ew, Options{})
	require.Empty(t, out.String())
}

func TestForeignSidecarDoesNotClobberExistingHTML(t *testing.T) {
	c, data := telemetryFixture(t)
	dir := t.TempDir()
	sidecar := filepath.Join(dir, "gpu.jsonl")
	require.NoError(t, os.WriteFile(sidecar, data, 0600))
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, TimeNanos: c.StartedAt.UnixNano(), DurationNanos: int64(time.Second), Comments: []string{capturemeta.CaptureIDComment + "other"}}
	path := filepath.Join(dir, "profile.gz")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, p.Write(f))
	require.NoError(t, f.Close())
	html := filepath.Join(dir, "profile.html")
	require.NoError(t, os.WriteFile(html, []byte("keep"), 0600))
	_, err = FromProfileFile(path, html, Options{GPUTelemetryPath: sidecar})
	require.Error(t, err)
	preserved, err := os.ReadFile(html)
	require.NoError(t, err)
	require.Equal(t, "keep", string(preserved))
}

func TestFusedTelemetryNeedsOneSharedCaptureWindow(t *testing.T) {
	c, data := telemetryFixture(t)
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "gpu.jsonl")
	require.NoError(t, os.WriteFile(jsonl, data, 0600))
	write := func(name, id string, duration time.Duration) string {
		path := filepath.Join(dir, name)
		p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}, TimeNanos: c.StartedAt.UnixNano(), DurationNanos: int64(duration), Comments: []string{capturemeta.CaptureIDComment + id}}
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, p.Write(f))
		require.NoError(t, f.Close())
		return path
	}
	a := write("a.gz", c.ID, 2*time.Second)
	b := write("b.gz", c.ID, 2*time.Second)
	_, err := FuseProfileFiles([]FuseFile{{Path: a}, {Path: b}}, filepath.Join(dir, "fused.html"), Options{GPUTelemetryPath: jsonl})
	require.NoError(t, err)
	b = write("b.gz", c.ID, time.Second)
	_, err = FuseProfileFiles([]FuseFile{{Path: a}, {Path: b}}, filepath.Join(dir, "bad.html"), Options{GPUTelemetryPath: jsonl})
	require.ErrorContains(t, err, "share capture identity and window")
}
