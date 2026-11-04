package perfagent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capturemeta"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
)

type captureTestProfiler struct{ capture pprof.CaptureWindow }

func (p *captureTestProfiler) SetCapture(c pprof.CaptureWindow) { p.capture = c }
func (p *captureTestProfiler) Close()                           {}
func (p *captureTestProfiler) Collect(w io.Writer) error {
	b := pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 99, Capture: &p.capture})
	sample := &pprof.ProfileSample{SampleType: pprof.SampleTypeCpu, Value: 42, Stack: []pprof.Frame{{Name: "work"}}}
	builder := b.BuilderForSample(sample)
	builder.CreateSample(sample)
	_, err := builder.Write(w)
	return err
}
func (p *captureTestProfiler) CollectAndWrite(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return p.Collect(f)
}

func TestAgentCaptureWindowSharedWithAutomaticGPUHTML(t *testing.T) {
	for _, telemetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "profile only", true: "profile with telemetry"}[telemetry], func(t *testing.T) {
			start := time.Now().Add(-time.Second)
			dir := t.TempDir()
			path := filepath.Join(dir, "cpu.gz")
			html := filepath.Join(dir, "cpu.html")
			cfg := &Config{CaptureID: "capture-test", CPUProfilePath: path, CPUFlamegraphPath: html}
			var c *metrics.GPUReportCollector
			if telemetry {
				c = metrics.NewGPUReportCollector(cfg.CaptureID, start, time.Time{})
				cfg.GPUTelemetryReport = c
				require.NoError(t, c.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{CaptureID: cfg.CaptureID, Kind: "sample", Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-a", Timestamp: start.Add(time.Millisecond), Values: map[string]float64{"gpu.temperature_celsius": 42}}}}}))
			}
			a := &Agent{config: cfg, started: true, capture: pprof.CaptureWindow{ID: cfg.CaptureID, StartedAt: start}, cpuProfiler: &captureTestProfiler{}}
			captureLog(t)
			beforeStop := time.Now()
			require.NoError(t, a.Stop(t.Context()))
			afterStop := time.Now()
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			p, err := profile.ParseData(raw)
			require.NoError(t, err)
			window, err := capturemeta.FromProfile(p)
			require.NoError(t, err)
			require.WithinDuration(t, start, window.StartedAt, 0)
			require.False(t, window.EndedAt.Before(beforeStop))
			require.False(t, window.EndedAt.After(afterStop))
			page, err := os.ReadFile(html)
			require.NoError(t, err)
			if telemetry {
				require.Contains(t, string(page), `id="gpu-data"`)
				require.Contains(t, string(page), `"gpu.temperature_celsius"`)
				require.Equal(t, window.EndedAt.UnixNano(), a.gpuReport.EndedAt.UnixNano())
				// Late continuous records cannot alter the already rendered capture.
				require.NoError(t, c.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{CaptureID: cfg.CaptureID, Kind: "sample", Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-a", Timestamp: afterStop, Values: map[string]float64{"gpu.temperature_celsius": 99}}}}}))
				require.Equal(t, 42.0, c.Finish().Devices[0].Metrics["gpu.temperature_celsius"].Mean)
			} else {
				require.NotContains(t, string(page), `id="gpu-data"`)
			}
		})
	}
}

func TestGPUReportFailureStillWritesTheProfile(t *testing.T) {
	start := time.Now().Add(-time.Second)
	c := metrics.NewGPUReportCollector("capture", start, time.Time{})
	require.NoError(t, c.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{CaptureID: "capture", Kind: "sample", Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-a", Timestamp: time.Now().Add(time.Hour), Values: map[string]float64{"gpu.power_watts": 20}}}}}))
	path := filepath.Join(t.TempDir(), "cpu.gz")
	a := &Agent{started: true, capture: pprof.CaptureWindow{ID: "capture", StartedAt: start}, config: &Config{CPUProfilePath: path, GPUTelemetryReport: c}, cpuProfiler: &captureTestProfiler{}}
	captureLog(t)
	require.ErrorContains(t, a.Stop(t.Context()), "exceeds capture boundary")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = profile.ParseData(data)
	require.NoError(t, err)
}

func TestInvalidCaptureIdentityRejectedBeforeStarting(t *testing.T) {
	for _, id := range []string{"bad\nid", strings.Repeat("x", 257)} {
		_, err := New(WithCPUProfile("unused.gz"), WithPID(1234), WithCaptureID(id))
		require.ErrorContains(t, err, "capture ID")
	}
}
