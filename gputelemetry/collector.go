// Package gputelemetry collects device telemetry independently of profiling.
package gputelemetry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
)

type Device struct {
	UUID, Name, DriverVersion string
}

type Source interface {
	Open() ([]Device, error)
	Read(Device) metrics.GPUDeviceMetrics
	Close() error
}

type Collector struct {
	Source    Source
	UUIDs     []string
	Interval  time.Duration
	Exporters []metrics.Exporter
}

// Run collects an initial sample, periodic samples, and a final sample when ctx
// ends. Exporters run serially: slow outputs skip ticks instead of buffering an
// unbounded series. A native driver call or writer must return before shutdown.
func (c *Collector) Run(ctx context.Context) (err error) {
	if c.Source == nil || c.Interval <= 0 || len(c.Exporters) == 0 {
		return errors.New("GPU telemetry requires a source, positive interval and exporter")
	}
	for _, exporter := range c.Exporters {
		if exporter == nil {
			return errors.New("GPU telemetry exporter is nil")
		}
	}
	started := time.Now()
	cancelledAt := make(chan time.Time, 1)
	stopCancellationClock := context.AfterFunc(ctx, func() { cancelledAt <- time.Now() })
	defer stopCancellationClock()
	publish := func(gpu *metrics.GPUMetrics) error {
		var errs []error
		for _, exporter := range c.Exporters {
			// The capture context is cancelled at the duration boundary. Final
			// records still have to reach the local writers at that point.
			if err := exporter.Export(context.WithoutCancel(ctx), &metrics.MetricsSnapshot{Timestamp: time.Now(), GPU: gpu}); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", exporter.Name(), err))
			}
		}
		return errors.Join(errs...)
	}
	unavailable := func(cause error) error {
		return errors.Join(cause, publish(&metrics.GPUMetrics{Kind: "unavailable", StartedAt: started, Error: cause.Error()}))
	}
	devices, err := c.Source.Open()
	if err != nil {
		return unavailable(err)
	}
	defer func() { err = errors.Join(err, c.Source.Close()) }()
	if len(c.UUIDs) != 0 {
		selected := make([]Device, 0, len(c.UUIDs))
		for _, uuid := range c.UUIDs {
			i := slices.IndexFunc(devices, func(d Device) bool { return d.UUID == uuid })
			if i == -1 {
				return unavailable(fmt.Errorf("GPU UUID %q not found", uuid))
			}
			if !slices.ContainsFunc(selected, func(d Device) bool { return d.UUID == uuid }) {
				selected = append(selected, devices[i])
			}
		}
		devices = selected
	}
	if len(devices) == 0 {
		return unavailable(errors.New("no physical NVIDIA GPUs available"))
	}
	summaries := make([]metrics.GPUDeviceMetrics, len(devices))
	var missed uint64
	sample := func(kind string) error {
		readings := make([]metrics.GPUDeviceMetrics, 0, len(devices))
		for i, device := range devices {
			d := c.Source.Read(device)
			readings = append(readings, d)
			s := &summaries[i]
			if s.Summary == nil {
				*s = d
				s.Values = nil
				s.Summary = make(map[string]metrics.GaugeSummary)
				s.Unavailable = make(map[string]string)
				s.FailedReadings = make(map[string]uint64)
			}
			s.Timestamp = d.Timestamp
			s.ReadDuration = d.ReadDuration
			for name, value := range d.Values {
				g := s.Summary[name]
				g.Observe(value)
				s.Summary[name] = g
			}
			for name, why := range d.Unavailable {
				s.FailedReadings[name]++
				s.Unavailable[name] = why
			}
		}
		return publish(&metrics.GPUMetrics{Kind: kind, StartedAt: started, MissedTicks: missed, Devices: readings})
	}
	if err := sample("initial"); err != nil {
		return err
	}
	next := started.Add(c.Interval)
	for {
		timer := time.NewTimer(max(0, time.Until(next)))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			ended := <-cancelledAt
			if deadline, ok := ctx.Deadline(); ok && deadline.Before(ended) {
				ended = deadline
			}
			if ended.After(next) {
				missed += uint64((ended.Sub(next)-1)/c.Interval) + 1
			}
			if err := sample("final"); err != nil {
				return err
			}
			return publish(&metrics.GPUMetrics{Kind: "summary", StartedAt: started, EndedAt: ended, MissedTicks: missed, Devices: summaries})
		}
		late := time.Since(next) / c.Interval
		missed += uint64(max(0, late))
		next = next.Add((max(0, late) + 1) * c.Interval)
		if err := sample("sample"); err != nil {
			return err
		}
	}
}
