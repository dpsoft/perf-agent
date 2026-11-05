package metrics

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// GPUReport is a bounded, offline view of physical-device observations within a
// profile's capture lifecycle. It never attributes a sensor to a stack frame.
type GPUReport struct {
	CaptureID string            `json:"capture_id"`
	StartedAt time.Time         `json:"started_at"`
	EndedAt   time.Time         `json:"ended_at"`
	Interval  time.Duration     `json:"interval_ns"`
	Devices   []GPUReportDevice `json:"devices"`
	Errors    []string          `json:"errors,omitempty"`
}

type GPUReportDevice struct {
	UUID             string                     `json:"uuid"`
	Name             string                     `json:"name"`
	DriverVersion    string                     `json:"driver_version"`
	MemoryAPIVersion string                     `json:"memory_api_version"`
	Readings         uint64                     `json:"readings"`
	FirstAt          time.Time                  `json:"first_at"`
	LastAt           time.Time                  `json:"last_at"`
	Metrics          map[string]GPUFieldSummary `json:"metrics"`
	Unsupported      map[string]string          `json:"unsupported,omitempty"`
	Unavailable      map[string]string          `json:"unavailable,omitempty"`
	FailedReadings   map[string]uint64          `json:"failed_readings,omitempty"`
}

type GPUFieldSummary struct {
	GaugeSummary
	LastAt time.Time `json:"last_at"`
}

// GPUReportCollector retains aggregates, not a time series. Finish closes this
// consumer while the same source may continue exporting JSONL and OTLP forever.
// The device/key/error limits also bound rediscovery and untrusted offline input.
type GPUReportCollector struct {
	mu      sync.Mutex
	report  GPUReport
	devices map[string]*GPUReportDevice
	closed  bool
	begun   chan struct{}
}

func NewGPUReportCollector(id string, start, end time.Time) *GPUReportCollector {
	return &GPUReportCollector{report: GPUReport{CaptureID: id, StartedAt: start, EndedAt: end}, devices: make(map[string]*GPUReportDevice), begun: make(chan struct{})}
}

// Begun is closed once Begin has bound the report to its capture's start, so
// a sampler can take its first reading inside the window rather than after
// whatever setup follows the start.
func (c *GPUReportCollector) Begun() <-chan struct{} { return c.begun }

func (c *GPUReportCollector) Name() string { return "gpu-report" }

func (c *GPUReportCollector) Export(ctx context.Context, snapshot *MetricsSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot.GPU == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	g := snapshot.GPU
	if c.report.StartedAt.IsZero() {
		return fmt.Errorf("GPU report has not begun")
	}

	if g.CaptureID != c.report.CaptureID {
		return fmt.Errorf("GPU telemetry capture_id does not match profile")
	}
	if g.Kind == "summary" {
		return nil
	} // Recompute from observations in THIS window.
	if g.Kind == "unavailable" {
		if c.contains(g.StartedAt) {
			c.addError(g.Error)
		}
		return nil
	}
	switch g.Kind {
	case "initial", "sample", "final":
	default:
		return fmt.Errorf("unknown GPU telemetry record kind %q", g.Kind)
	}
	if g.Interval > 0 {
		c.report.Interval = g.Interval
	}
	for _, d := range g.Devices {
		if !c.contains(d.Timestamp) {
			continue
		}
		if !strings.HasPrefix(d.UUID, "GPU-") || len(d.UUID) > 128 || len(d.Name) > 512 || len(d.DriverVersion) > 128 || len(d.MemoryAPIVersion) > 128 {
			return fmt.Errorf("GPU telemetry requires a physical device UUID")
		}
		if len(d.Values)+len(d.Unavailable)+len(d.Unsupported) > 128 {
			return fmt.Errorf("too many GPU telemetry fields")
		}
		// Validate before changing any aggregate for this observation.
		for name, value := range d.Values {
			if len(name) > 256 || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("invalid/non-finite GPU value or name for %s", name)
			}
		}
		for name, why := range d.Unsupported {
			if len(name) > 256 || len(why) > 1024 {
				return fmt.Errorf("GPU unsupported reason too long")
			}
		}
		for name, why := range d.Unavailable {
			if len(name) > 256 || len(why) > 1024 {
				return fmt.Errorf("GPU unavailable reason too long")
			}
		}

		entry := c.devices[d.UUID]
		if entry == nil {
			if len(c.devices) >= 64 {
				c.addError("Device limit reached; additional GPUs omitted")
				continue
			}
			entry = &GPUReportDevice{UUID: d.UUID, Name: d.Name, DriverVersion: d.DriverVersion, MemoryAPIVersion: d.MemoryAPIVersion, FirstAt: d.Timestamp, Metrics: make(map[string]GPUFieldSummary), Unsupported: make(map[string]string), Unavailable: make(map[string]string), FailedReadings: make(map[string]uint64)}
			c.devices[d.UUID] = entry
		}
		if !entry.LastAt.IsZero() && d.Timestamp.UnixNano() <= entry.LastAt.UnixNano() {
			return fmt.Errorf("GPU %s observations must have increasing timestamps", d.UUID)
		}
		entry.LastAt = d.Timestamp
		entry.Readings++
		// Metadata is taken from the most recent reading, not inferred from values.
		entry.Name, entry.DriverVersion, entry.MemoryAPIVersion = d.Name, d.DriverVersion, d.MemoryAPIVersion
		for name, value := range d.Values {
			if _, exists := entry.Metrics[name]; !exists && len(entry.Metrics) >= 128 {
				c.addError("Metric limit reached; additional fields omitted")
				continue
			}
			v := entry.Metrics[name]
			v.Observe(value)
			v.LastAt = d.Timestamp
			entry.Metrics[name] = v
		}
		for name, why := range d.Unsupported {
			if len(entry.Unsupported) < 128 {
				entry.Unsupported[name] = why
			}
		}
		for name, why := range d.Unavailable {
			if len(entry.Unavailable) < 128 {
				entry.Unavailable[name] = why
				entry.FailedReadings[name]++
			}
		}
	}
	return nil
}

func (c *GPUReportCollector) contains(t time.Time) bool {
	return !t.IsZero() && t.UnixNano() >= c.report.StartedAt.UnixNano() && (c.report.EndedAt.IsZero() || t.UnixNano() <= c.report.EndedAt.UnixNano())
}

func (c *GPUReportCollector) addError(message string) {
	if len(message) > 1024 {
		message = message[:1024] + "…"
	}
	if message != "" && len(c.report.Errors) < 32 && !slices.Contains(c.report.Errors, message) {
		c.report.Errors = append(c.report.Errors, message)
	}
}

// Finish fixes an open-ended report's end under the same lock as Export, so no
// later continuous samples can leak into the finite profile's aggregates.
func (c *GPUReportCollector) Finish() *GPUReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.report.EndedAt.IsZero() {
		c.report.EndedAt = time.Now()
	}
	for _, d := range c.devices {
		if d.FirstAt.Sub(c.report.StartedAt) > c.report.Interval || c.report.EndedAt.Sub(d.LastAt) > c.report.Interval {
			c.addError("Observations cover only part of the capture window. Check first/last reading times; offline JSONL may be missing rotated segments.")
			break
		}
	}
	result := c.report
	result.Errors = slices.Clone(c.report.Errors)
	result.Devices = make([]GPUReportDevice, 0, len(c.devices))
	for _, uuid := range slices.Sorted(maps.Keys(c.devices)) {
		d := *c.devices[uuid]
		d.Metrics = maps.Clone(d.Metrics)
		d.Unsupported = maps.Clone(d.Unsupported)
		d.Unavailable = maps.Clone(d.Unavailable)
		d.FailedReadings = maps.Clone(d.FailedReadings)
		result.Devices = append(result.Devices, d)
	}
	return &result
}

// Begin binds an as-yet-unused report to the agent's actual lifecycle start.
func (c *GPUReportCollector) Begin(id string, start time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.devices) > 0 || c.report.CaptureID != id || start.IsZero() {
		return fmt.Errorf("GPU report cannot begin with this capture")
	}
	c.report.StartedAt = start
	select {
	case <-c.begun:
	default:
		close(c.begun)
	}
	return nil
}

// Stop prevents future observations. The agent records its end AFTER this
// barrier, then calls FinishAt; the telemetry never defines the profile window.
func (c *GPUReportCollector) Stop() { c.mu.Lock(); defer c.mu.Unlock(); c.closed = true }

func (c *GPUReportCollector) FinishAt(end time.Time) (*GPUReport, error) {
	c.mu.Lock()
	if !c.closed || end.UnixNano() <= c.report.StartedAt.UnixNano() {
		c.mu.Unlock()
		return nil, fmt.Errorf("GPU report must be stopped before finalizing the capture window")
	}
	for _, d := range c.devices {
		if d.LastAt.UnixNano() > end.UnixNano() {
			c.mu.Unlock()
			return nil, fmt.Errorf("GPU observation timestamp exceeds capture boundary")
		}
	}
	c.report.EndedAt = end
	c.mu.Unlock()
	return c.Finish(), nil
}

// LiveExporter isolates a secondary HTML consumer from live JSONL/OTLP. Offline
// readers use Export directly and reject invalid records instead of degrading.
func (c *GPUReportCollector) LiveExporter() Exporter { return gpuLiveReportExporter{c} }

type gpuLiveReportExporter struct{ report *GPUReportCollector }

func (e gpuLiveReportExporter) Name() string { return e.report.Name() }
func (e gpuLiveReportExporter) Export(ctx context.Context, s *MetricsSnapshot) error {
	if err := e.report.Export(ctx, s); err != nil {
		e.report.mu.Lock()
		if !e.report.closed {
			e.report.addError("Some sensor observations were omitted from this report: " + err.Error())
		}
		e.report.mu.Unlock()
	}
	return nil
}
