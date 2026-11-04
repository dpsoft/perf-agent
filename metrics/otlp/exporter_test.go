package otlp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func sample(value float64, at time.Time) *metrics.MetricsSnapshot {
	return &metrics.MetricsSnapshot{Timestamp: at, GPU: &metrics.GPUMetrics{Kind: "sample", Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-test", Name: "Test GPU", DriverVersion: "test-driver", MemoryAPIVersion: "nvmlDeviceGetMemoryInfo_v2", Timestamp: at, Values: map[string]float64{"gpu.temperature_celsius": value, "gpu.power_watts": 0, "gpu.memory_activity_ratio": 0.25}, Unavailable: map[string]string{"gpu.sm_clock_hertz": "GPU_IS_LOST"}}}}}
}
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT"} {
		t.Setenv(key, "")
	}
}
func TestHTTPPayloadAndShortCaptureShutdown(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "sensor-test")
	received := make(chan *collector.ExportMetricsServiceRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("missing protobuf content type")
		}
		if r.Header.Get("X-Test") != "secret" {
			t.Error("headers not configured from environment")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		msg := new(collector.ExportMetricsServiceRequest)
		if err := proto.Unmarshal(body, msg); err != nil {
			t.Error(err)
			return
		}
		received <- msg
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "X-Test=secret")
	e, err := New(t.Context(), Options{Endpoint: server.URL, LockDir: filepath.Join(t.TempDir(), "locks"), Version: "test"})
	require.NoError(t, err)
	at := time.Unix(123456, 123)
	require.NoError(t, e.Export(t.Context(), sample(42, at)))
	// Summary values must never be interpreted as a sensor reading.
	require.NoError(t, e.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{Kind: "summary", Devices: []metrics.GPUDeviceMetrics{{UUID: "GPU-test", Values: map[string]float64{"gpu.temperature_celsius": 999}}}}}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx))
	msg := <-received
	require.Len(t, msg.ResourceMetrics, 1)
	rm := msg.ResourceMetrics[0]
	resources := map[string]string{}
	for _, kv := range rm.Resource.Attributes {
		resources[kv.Key] = kv.Value.GetStringValue()
	}
	require.Equal(t, "sensor-test", resources["service.name"])
	require.NotEmpty(t, resources["host.name"])
	require.NotContains(t, resources, "capture_id")
	require.NotContains(t, resources, "service.instance.id")
	found := map[string]float64{}
	units := map[string]string{}
	for _, m := range rm.ScopeMetrics[0].Metrics {
		require.NotNil(t, m.GetGauge())
		for _, p := range m.GetGauge().DataPoints {
			require.Equal(t, uint64(at.UnixNano()), p.TimeUnixNano)
			require.Equal(t, uint64(0), p.StartTimeUnixNano)
			attrs := map[string]string{}
			for _, kv := range p.Attributes {
				attrs[kv.Key] = kv.Value.GetStringValue()
			}
			require.Equal(t, "GPU-test", attrs["hw.id"])
			if m.Name == "perf_agent.gpu.info" {
				require.Equal(t, "test-driver", attrs["gpu.driver.version"])
				require.Equal(t, "nvmlDeviceGetMemoryInfo_v2", attrs["gpu.memory.api"])
			}
			found[m.Name] = p.GetAsDouble()
			units[m.Name] = m.Unit
		}
	}
	require.Equal(t, 42.0, found["perf_agent.gpu.temperature"])
	require.Equal(t, "Cel", units["perf_agent.gpu.temperature"])
	require.Contains(t, found, "perf_agent.gpu.power")
	require.Zero(t, found["perf_agent.gpu.power"])
	require.NotContains(t, found, "perf_agent.gpu.clock.sm")
	require.Equal(t, 0.25, found["perf_agent.gpu.memory.activity"])
	require.Equal(t, 1.0, found["perf_agent.gpu.telemetry.read_errors"])
	require.Len(t, received, 0)
	require.Positive(t, e.Stats().SentPoints)
}

type blockingTransport struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	batches []*metricdata.ResourceMetrics
}

func (c *blockingTransport) Export(ctx context.Context, m *metricdata.ResourceMetrics) error {
	first := false
	c.once.Do(func() { close(c.entered); first = true })
	if first {
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	c.batches = append(c.batches, m)
	c.mu.Unlock()
	return nil
}
func (*blockingTransport) Shutdown(context.Context) error { return nil }
func newTestExporter(t *testing.T, c transport, dir, instance string) *Exporter {
	t.Helper()
	locks, err := newDeviceLocks(dir, instance)
	require.NoError(t, err)
	e := newExporter(c, resource.Empty(), locks, "test", 3*time.Second)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return e
}
func TestSlowRemoteCoalescesWithoutBlockingAndKeepsLastTimestamp(t *testing.T) {
	c := &blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	e := newTestExporter(t, c, filepath.Join(t.TempDir(), "locks"), "")
	require.NoError(t, e.Export(t.Context(), sample(1, time.Unix(1, 0))))
	<-c.entered
	for i := 2; i <= 1000; i++ {
		require.NoError(t, e.Export(t.Context(), sample(float64(i), time.Unix(int64(i), 0))))
	}
	require.Len(t, e.pending, 1)
	require.Positive(t, e.Stats().CoalescedPoints)
	close(c.release)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx))
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.batches, 2)
	for _, m := range c.batches[1].ScopeMetrics[0].Metrics {
		if m.Name == "perf_agent.gpu.temperature" {
			p := m.Data.(metricdata.Gauge[float64]).DataPoints[0]
			require.Equal(t, 1000.0, p.Value)
			require.Equal(t, time.Unix(1000, 0), p.Time)
		}
	}
}
func TestShutdownCancelsBlockedRequest(t *testing.T) {
	c := &blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	e := newTestExporter(t, c, filepath.Join(t.TempDir(), "locks"), "")
	require.NoError(t, e.Export(t.Context(), sample(1, time.Now())))
	<-c.entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.ErrorIs(t, e.Shutdown(ctx), context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after cancellation")
	}
	require.Positive(t, e.Stats().FailedRequests)
}
func TestOwnershipContentionNoTakeoverAndRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	a, err := newDeviceLocks(dir, "")
	require.NoError(t, err)
	b, err := newDeviceLocks(dir, "")
	require.NoError(t, err)
	defer b.close()
	ok, err := a.acquire("GPU-a")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = b.acquire("GPU-a")
	require.NoError(t, err)
	require.False(t, ok)
	a.close()
	ok, err = b.acquire("GPU-a")
	require.NoError(t, err)
	require.False(t, ok, "no automatic takeover")
	c, err := newDeviceLocks(dir, "")
	require.NoError(t, err)
	defer c.close()
	ok, err = c.acquire("GPU-a")
	require.NoError(t, err)
	require.True(t, ok)
	distinct, err := newDeviceLocks(dir, "explicit-second-writer")
	require.NoError(t, err)
	defer distinct.close()
	ok, err = distinct.acquire("GPU-a")
	require.NoError(t, err)
	require.True(t, ok)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 2, "release never unlinks ownership files")
}
func TestUnavailableClearsPendingSensorReadings(t *testing.T) {
	c := &blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	e := newTestExporter(t, c, filepath.Join(t.TempDir(), "locks"), "")
	require.NoError(t, e.Export(t.Context(), sample(1, time.Now())))
	<-c.entered
	require.NoError(t, e.Export(t.Context(), sample(2, time.Now())))
	at := time.Now()
	require.NoError(t, e.Export(t.Context(), &metrics.MetricsSnapshot{Timestamp: at, GPU: &metrics.GPUMetrics{Kind: "unavailable", Error: "driver disappeared"}}))
	require.NotContains(t, e.pending["GPU-test"].device.Values, "gpu.temperature_celsius")
	require.Equal(t, 1.0, e.pending["GPU-test"].device.Values["gpu.telemetry.state.source_unavailable"])
	close(c.release)
}
func TestDeviceBudgetIsBounded(t *testing.T) {
	locks, err := newDeviceLocks(filepath.Join(t.TempDir(), "locks"), "")
	require.NoError(t, err)
	defer locks.close()
	for i := range maxDevices {
		ok, err := locks.acquire(string(rune(i + 100)))
		require.NoError(t, err)
		require.True(t, ok)
	}
	_, err = locks.acquire("overflow")
	require.ErrorContains(t, err, "limit")
	require.Len(t, locks.files, maxDevices)
}

func TestHTTPOutageThenRecovery(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "30")
	var failed atomic.Bool
	failed.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	e, err := New(t.Context(), Options{Endpoint: server.URL, LockDir: filepath.Join(t.TempDir(), "locks")})
	require.NoError(t, err)
	require.NoError(t, e.Export(t.Context(), sample(1, time.Now())))
	require.Eventually(t, func() bool { return e.Stats().FailedRequests > 0 }, time.Second, time.Millisecond)
	require.Zero(t, e.Stats().SentSensorPoints)
	failed.Store(false)
	require.NoError(t, e.Export(t.Context(), sample(2, time.Now())))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, e.Shutdown(ctx))
	require.Positive(t, e.Stats().FailedPoints)
	require.Positive(t, e.Stats().SentSensorPoints)
}
func TestExplicitInstanceDistinguishesResource(t *testing.T) {
	cleanEnv(t)
	e, err := New(t.Context(), Options{InstanceID: "operator-chosen", LockDir: filepath.Join(t.TempDir(), "locks")})
	require.NoError(t, err)
	value, ok := e.resource.Set().Value("service.instance.id")
	require.True(t, ok)
	require.Equal(t, "operator-chosen", value.AsString())
	require.Equal(t, "operator-chosen", e.locks.instance)
	require.NoError(t, e.Shutdown(t.Context()))
}
func TestConfiguredUUIDUnavailableBeforeFirstSuccessfulNVMLInit(t *testing.T) {
	cleanEnv(t)
	e, err := New(t.Context(), Options{DeviceUUIDs: []string{"GPU-selected"}, Endpoint: "http://127.0.0.1:1", LockDir: filepath.Join(t.TempDir(), "locks")})
	require.NoError(t, err)
	// Inspect owned UUID before network work begins.
	require.NotNil(t, e.locks.files["GPU-selected"])
	require.NoError(t, e.Shutdown(t.Context()))
}

func TestExplicitEndpointOverridesForeignProtocolEnvironment(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	warned := false
	e, err := New(t.Context(), Options{Endpoint: "http://127.0.0.1:4318", LockDir: filepath.Join(t.TempDir(), "locks"), Warn: func(string) { warned = true }})
	require.NoError(t, err)
	require.True(t, warned)
	require.NoError(t, e.Shutdown(t.Context()))
	_, err = New(t.Context(), Options{LockDir: filepath.Join(t.TempDir(), "locks")})
	require.ErrorContains(t, err, "http/protobuf")
}

func TestHTTPNeverRespondsShutdownReleasesOwnership(t *testing.T) {
	cleanEnv(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	dir := filepath.Join(t.TempDir(), "locks")
	e, err := New(t.Context(), Options{Endpoint: server.URL, LockDir: dir})
	require.NoError(t, err)
	require.NoError(t, e.Export(t.Context(), sample(1, time.Now())))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("receiver never got request")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	require.ErrorIs(t, e.Shutdown(ctx), context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not release ownership")
	}
	successor, err := newDeviceLocks(dir, "")
	require.NoError(t, err)
	defer successor.close()
	ok, err := successor.acquire("GPU-test")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestCompetingExportersCountContentionWithoutSending(t *testing.T) {
	cleanEnv(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "locks")
	a, err := New(t.Context(), Options{Endpoint: server.URL, LockDir: dir})
	require.NoError(t, err)
	b, err := New(t.Context(), Options{Endpoint: server.URL, LockDir: dir})
	require.NoError(t, err)
	require.NoError(t, a.Export(t.Context(), sample(1, time.Now())))
	require.Eventually(t, func() bool { return a.Stats().SentSensorPoints > 0 }, time.Second, time.Millisecond)
	require.NoError(t, b.Export(t.Context(), sample(2, time.Now())))
	require.NoError(t, b.Shutdown(t.Context()))
	require.NoError(t, a.Shutdown(t.Context()))
	require.Positive(t, b.Stats().ContendedPoints)
	require.Zero(t, b.Stats().Requests)
	require.Equal(t, int64(1), requests.Load())
}
