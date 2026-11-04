// Package otlp exports GPU sensor readings over OTLP/HTTP without blocking sampling.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

const maxDevices = 128

type Options struct {
	Warn func(string)
	// Empty values use the official exporter's OTEL_EXPORTER_OTLP_* configuration.
	Endpoint, InstanceID, LockDir, Version string
	DeviceUUIDs                            []string
}

type Stats struct {
	SentPoints, FailedPoints, CoalescedPoints, DroppedPoints, ContendedPoints uint64
	UnavailableRecords, SentSensorPoints                                      uint64
	Requests, FailedRequests                                                  uint64
}

type reading struct {
	device metrics.GPUDeviceMetrics
	reason string
}

type transport interface {
	Export(context.Context, *metricdata.ResourceMetrics) error
	Shutdown(context.Context) error
}

// Exporter has one in-flight batch and at most one pending reading per device.
// Sampling never waits on HTTP. Superseded pending readings are counted.
// The owner must call Shutdown after the last Export; Collector does not own it.
type Exporter struct {
	mu        sync.Mutex
	client    transport
	resource  *resource.Resource
	version   string
	timeout   time.Duration
	pending   map[string]reading
	locks     *deviceLocks
	stats     Stats
	closing   bool
	wake      chan struct{}
	done      chan struct{}
	workerCtx context.Context
	cancel    context.CancelFunc
}

// New is deliberately called only after explicit opt-in by the caller.
func New(ctx context.Context, opts Options) (*Exporter, error) {
	timeout := 3 * time.Second
	rawTimeout := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT")
	if rawTimeout == "" {
		rawTimeout = os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT")
	}
	if rawTimeout != "" {
		millis, err := strconv.ParseInt(rawTimeout, 10, 64)
		if err != nil || millis < 0 {
			return nil, errors.New("OTLP timeout must be nonnegative integer milliseconds")
		}
		if millis > 0 && millis < 3000 {
			timeout = time.Duration(millis) * time.Millisecond
		}
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		if opts.Endpoint == "" {
			return nil, fmt.Errorf("OTLP metrics supports http/protobuf, got %q", protocol)
		}
		if opts.Warn != nil {
			opts.Warn("explicit endpoint selects http/protobuf; ignoring OTEL protocol setting")
		}
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "perf-agent"), attribute.String("service.version", opts.Version)), resource.WithHost(), resource.WithFromEnv())
	if err != nil {
		return nil, err
	}
	if opts.InstanceID != "" {
		res, err = resource.Merge(res, resource.NewSchemaless(attribute.String("service.instance.id", opts.InstanceID)))
		if err != nil {
			return nil, err
		}
	}
	instance, _ := res.Set().Value(attribute.Key("service.instance.id"))
	locks, err := newDeviceLocks(opts.LockDir, instance.AsString())
	if err != nil {
		return nil, err
	}
	for _, uuid := range opts.DeviceUUIDs {
		if _, err := locks.acquire(uuid); err != nil {
			locks.close()
			return nil, err
		}
	}
	config := []otlpmetrichttp.Option{otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: true, InitialInterval: 200 * time.Millisecond, MaxInterval: time.Second, MaxElapsedTime: 3 * time.Second})}
	if opts.Endpoint != "" {
		endpoint, err := url.Parse(opts.Endpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
			locks.close()
			return nil, errors.New("OTLP endpoint must be an absolute http or https URL")
		}
		if endpoint.Path == "" || endpoint.Path == "/" {
			endpoint.Path = "/v1/metrics"
		}
		config = append(config, otlpmetrichttp.WithEndpointURL(endpoint.String()))
	}
	client, err := otlpmetrichttp.New(ctx, config...)
	if err != nil {
		locks.close()
		return nil, err
	}
	return newExporter(client, res, locks, opts.Version, timeout), nil
}

func newExporter(client transport, res *resource.Resource, locks *deviceLocks, version string, timeout time.Duration) *Exporter {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Exporter{client: client, resource: res, version: version, timeout: timeout, locks: locks, pending: make(map[string]reading), wake: make(chan struct{}, 1), done: make(chan struct{}), workerCtx: ctx, cancel: cancel}
	go e.run()
	return e
}
func (*Exporter) Name() string { return "otlp-http" }

func (e *Exporter) Export(ctx context.Context, snapshot *metrics.MetricsSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot == nil || snapshot.GPU == nil {
		return nil
	}
	switch snapshot.GPU.Kind {
	case "initial", "sample", "final", "unavailable":
	default:
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closing {
		return errors.New("OTLP exporter is closed")
	}
	if snapshot.GPU.Kind == "unavailable" {
		e.stats.UnavailableRecords++
		for uuid, f := range e.locks.files {
			if f != nil {
				e.enqueue(reading{device: metrics.GPUDeviceMetrics{UUID: uuid, Timestamp: snapshot.Timestamp, Values: map[string]float64{"gpu.telemetry.available": 0}}, reason: "source_unavailable"})
			}
		}
	}
	if snapshot.GPU.Kind == "initial" {
		present := make(map[string]bool, len(snapshot.GPU.Devices))
		for _, d := range snapshot.GPU.Devices {
			present[d.UUID] = true
		}
		for uuid, f := range e.locks.files {
			if f != nil && !present[uuid] {
				e.enqueue(reading{device: metrics.GPUDeviceMetrics{UUID: uuid, Timestamp: snapshot.Timestamp, Values: map[string]float64{"gpu.telemetry.available": 0}}, reason: "not_present"})
			}
		}
	}
	for _, d := range snapshot.GPU.Devices {
		values := make(map[string]float64)
		for name, value := range d.Values {
			if _, ok := definitions[name]; ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
				values[name] = value
			}
		}
		if d.UUID == "" || d.Timestamp.IsZero() {
			e.stats.DroppedPoints += uint64(len(values))
			continue
		}
		allowed, err := e.locks.acquire(d.UUID)
		if err != nil {
			e.stats.DroppedPoints += uint64(len(values))
			continue
		}
		if !allowed {
			e.stats.ContendedPoints += uint64(len(values))
			continue
		}
		reason := "ok"
		available := 1.0
		if len(values) == 0 {
			available = 0
			reason = "unsupported"
			if len(d.Unavailable) != 0 {
				reason = "read_failed"
			}
		}
		values["gpu.telemetry.available"] = available
		values["gpu.telemetry.read_errors"] = float64(len(d.Unavailable))
		d.Values = values
		d.Summary, d.Unavailable, d.Unsupported, d.FailedReadings = nil, nil, nil, nil
		e.enqueue(reading{device: d, reason: reason})
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return nil
}

func (e *Exporter) enqueue(r reading) {
	for _, reason := range []string{"ok", "unsupported", "read_failed", "source_unavailable", "not_present"} {
		value := 0.0
		if r.reason == reason {
			value = 1
		}
		r.device.Values["gpu.telemetry.state."+reason] = value
	}

	if old, ok := e.pending[r.device.UUID]; ok {
		e.stats.CoalescedPoints += uint64(len(old.device.Values))
	}
	e.pending[r.device.UUID] = r
}

func (e *Exporter) Stats() Stats { e.mu.Lock(); defer e.mu.Unlock(); return e.stats }

// Shutdown drains pending readings within ctx's deadline, then cancels HTTP.
// The worker releases device ownership only after its final request ends.
func (e *Exporter) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		e.cancel()
		return ctx.Err()
	}
}

func (e *Exporter) run() {
	defer close(e.done)
	defer e.locks.close()
	defer e.cancel()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = e.client.Shutdown(ctx)
	}()
	for {
		select {
		case <-e.wake:
		case <-e.workerCtx.Done():
		}
		e.mu.Lock()
		batch := e.pending
		e.pending = make(map[string]reading)
		closing := e.closing
		e.mu.Unlock()
		payload, count := e.payload(batch)
		var sensors uint64
		for _, r := range batch {
			for name := range r.device.Values {
				if !strings.HasPrefix(name, "gpu.telemetry.") {
					sensors++
				}
			}
		}
		if e.workerCtx.Err() != nil {
			e.mu.Lock()
			e.stats.DroppedPoints += count
			e.mu.Unlock()
			return
		}
		if count != 0 {
			ctx, cancel := context.WithTimeout(e.workerCtx, e.timeout)
			err := e.client.Export(ctx, payload)
			cancel()
			e.mu.Lock()
			e.stats.Requests++
			if err != nil {
				e.stats.FailedRequests++
				e.stats.FailedPoints += count
			} else {
				e.stats.SentPoints += count
				e.stats.SentSensorPoints += sensors
			}
			e.mu.Unlock()
		}
		if closing {
			return
		}
	}
}

func (e *Exporter) payload(batch map[string]reading) (*metricdata.ResourceMetrics, uint64) {
	grouped := make(map[string][]metricdata.DataPoint[float64])
	var count uint64
	var infos []metricdata.DataPoint[float64]
	for _, r := range batch {
		d := r.device
		if d.DriverVersion != "" {
			infos = append(infos, metricdata.DataPoint[float64]{Time: d.Timestamp, Value: 1, Attributes: attribute.NewSet(attribute.String("hw.id", d.UUID), attribute.String("gpu.name", d.Name), attribute.String("gpu.driver.version", d.DriverVersion), attribute.String("gpu.memory.api", d.MemoryAPIVersion))})
			count++
		}
		attrs := attribute.NewSet(attribute.String("hw.id", d.UUID), attribute.String("hw.type", "gpu"))
		for name, value := range d.Values {
			grouped[name] = append(grouped[name], metricdata.DataPoint[float64]{Attributes: attrs, Time: d.Timestamp, Value: value})
			count++
		}
	}
	scope := metricdata.ScopeMetrics{Scope: instrumentation.Scope{Name: "github.com/dpsoft/perf-agent/gputelemetry", Version: e.version}}
	if len(infos) != 0 {
		scope.Metrics = append(scope.Metrics, metricdata.Metrics{Name: "perf_agent.gpu.info", Unit: "1", Description: "Device and raw memory accounting provenance", Data: metricdata.Gauge[float64]{DataPoints: infos}})
	}
	for name := range maps.Keys(grouped) {
		def := definitions[name]
		scope.Metrics = append(scope.Metrics, metricdata.Metrics{Name: def.name, Unit: def.unit, Description: def.description, Data: metricdata.Gauge[float64]{DataPoints: grouped[name]}})
	}
	return &metricdata.ResourceMetrics{Resource: e.resource, ScopeMetrics: []metricdata.ScopeMetrics{scope}}, count
}

type definition struct{ name, unit, description string }

var definitions = func() map[string]definition {
	d := map[string]definition{
		"gpu.telemetry.available":   {"perf_agent.gpu.telemetry.available", "1", "Whether at least one supported sensor reading is available; state is reported separately"},
		"gpu.telemetry.read_errors": {"perf_agent.gpu.telemetry.read_errors", "{reading}", "Number of transient sensor read errors in this poll; unsupported fields excluded"},
		"gpu.temperature_celsius":   {"perf_agent.gpu.temperature", "Cel", "GPU temperature"},
		"gpu.power_watts":           {"perf_agent.gpu.power", "W", "NVML power usage; averaging window depends on device and driver"},
		"gpu.power_limit_watts":     {"perf_agent.gpu.power.limit", "W", "Configured power management limit"},
		"gpu.utilization_ratio":     {"perf_agent.gpu.utilization", "1", "Fraction of NVML sample window with GPU kernels executing"},
		"gpu.memory_activity_ratio": {"perf_agent.gpu.memory.activity", "1", "Fraction of NVML sample window with memory reads or writes; not capacity utilization"},
		"gpu.sm_clock_hertz":        {"perf_agent.gpu.clock.sm", "Hz", "SM clock frequency"},
		"gpu.memory_clock_hertz":    {"perf_agent.gpu.clock.memory", "Hz", "Memory clock frequency"},
	}
	for _, reason := range []string{"ok", "unsupported", "read_failed", "source_unavailable", "not_present"} {
		d["gpu.telemetry.state."+reason] = definition{"perf_agent.gpu.telemetry.state." + reason, "1", "One-hot availability state; 1 for current state, otherwise 0"}
	}
	for _, kind := range []string{"total", "used", "free", "reserved"} {
		d["gpu.memory."+kind+"_bytes"] = definition{"perf_agent.gpu.memory." + kind, "By", "Raw nvmlDeviceGetMemoryInfo_v2 " + kind + " memory; driver accounting is preserved"}
	}
	for reason := range strings.FieldsSeq("idle application_clocks power_cap hardware_slowdown sync_boost software_thermal hardware_thermal hardware_power_brake display_clock board_limit reliability") {
		d["gpu.clock_event_active."+reason] = definition{"perf_agent.gpu.clock_event_active." + reason, "1", "NVML clock event state (0 or 1); idle is not thermal throttling"}
	}
	return d
}()
