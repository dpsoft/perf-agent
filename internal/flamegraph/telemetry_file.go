package flamegraph

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/dpsoft/perf-agent/internal/capturemeta"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/google/pprof/profile"
)

// loadTelemetry requires explicit capture identity. Time overlap alone cannot
// establish that two artifacts belong together, even on the same machine.
func loadTelemetry(path string, p *profile.Profile) (*metrics.GPUReport, error) {
	capture, err := capturemeta.FromProfile(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readTelemetry(f, capture)
}

func readTelemetry(r io.Reader, c capturemeta.Window) (*metrics.GPUReport, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	collector := metrics.NewGPUReportCollector(c.ID, c.StartedAt, c.EndedAt)
	const maxBytes = 256 << 20
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	matched := false
	for line := 1; scanner.Scan(); line++ {
		if limited.N <= 0 {
			return nil, fmt.Errorf("GPU telemetry exceeds 256 MiB input limit")
		}
		var row struct {
			SchemaVersion int    `json:"schema_version"`
			Source        string `json:"source"`
			Scope         string `json:"scope"`
			metrics.GPUMetrics
			Device *metrics.GPUDeviceMetrics `json:"device"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("GPU telemetry line %d: %w", line, err)
		}
		if row.SchemaVersion != 1 || row.Source != "nvml" || row.Scope != "device" {
			return nil, fmt.Errorf("unsupported GPU telemetry schema/source/scope at line %d", line)
		}
		if row.CaptureID != c.ID {
			return nil, fmt.Errorf("GPU telemetry capture_id mismatch at line %d", line)
		}
		matched = true
		row.Devices = nil
		if row.Device != nil {
			row.Devices = []metrics.GPUDeviceMetrics{*row.Device}
		}
		if err := collector.Export(context.Background(), &metrics.MetricsSnapshot{GPU: &row.GPUMetrics}); err != nil {
			return nil, fmt.Errorf("GPU telemetry line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("GPU telemetry: %w", err)
	}
	if !matched {
		return nil, fmt.Errorf("GPU telemetry file contains no records")
	}
	return collector.Finish(), nil
}

func attachTelemetry(opts *Options, p *profile.Profile) error {
	if opts.GPUTelemetryPath != "" {
		if opts.GPUTelemetry != nil {
			return fmt.Errorf("choose either a GPU telemetry file or report")
		}
		report, err := loadTelemetry(opts.GPUTelemetryPath, p)
		if err != nil {
			return err
		}
		opts.GPUTelemetry = report
	}
	if opts.GPUTelemetry != nil {
		c, err := capturemeta.FromProfile(p)
		if err != nil {
			return err
		}
		r := opts.GPUTelemetry
		if r.CaptureID != c.ID || !r.StartedAt.Equal(c.StartedAt) || !r.EndedAt.Equal(c.EndedAt) {
			return fmt.Errorf("GPU telemetry report does not match profile capture identity and window")
		}
	}
	return nil
}
