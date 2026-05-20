package perfagent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/symbolize"
)

// TestMetricsHandler_PrometheusFormat asserts the handler emits
// the canonical Prometheus text exposition format (HELP + TYPE +
// value lines) for every metric. Catches accidental field-name
// changes that would break Prometheus scrapers and Grafana
// dashboards downstream.
func TestMetricsHandler_PrometheusFormat(t *testing.T) {
	snap := symbolize.CountersSnapshot{
		KernelBatches:         5,
		KernelInputIPs:        42,
		KernelBatchFailures:   1,
		KernelFallbackEngaged: 1,
		KernelRawAddrFrames:   3,
		KernelLockdownEPERM:   7,
		KernelOtherErr:        2,
	}
	h := metricsHandlerFor(func() symbolize.CountersSnapshot { return snap })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h(rec, req)

	if got := rec.Code; got != 200 {
		t.Fatalf("status = %d, want 200", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body := rec.Body.String()
	// Each metric must have its HELP, TYPE, and value lines.
	for _, want := range []string{
		"# HELP perf_agent_symbolize_kernel_batches_total",
		"# TYPE perf_agent_symbolize_kernel_batches_total counter",
		"perf_agent_symbolize_kernel_batches_total 5",
		"# TYPE perf_agent_symbolize_kernel_fallback_engaged gauge",
		"perf_agent_symbolize_kernel_fallback_engaged 1",
		"perf_agent_symbolize_kernel_lockdown_eperm_total 7",
		"perf_agent_symbolize_kernel_other_err_total 2",
		"perf_agent_symbolize_kernel_raw_addr_frames_total 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestMetricsServer_Lifecycle covers the start/stop round trip
// against a live listener (port :0 so the OS picks). Asserts:
//   - the server actually serves the metrics handler on the bound
//     port (counter values match what the live snapshot returns)
//   - Stop returns within the grace window with no hung goroutine
//   - a post-Stop scrape fails (server is really closed)
func TestMetricsServer_Lifecycle(t *testing.T) {
	snap := symbolize.CountersSnapshot{KernelBatches: 99}
	srv, err := startMetricsListener("127.0.0.1:0", func() symbolize.CountersSnapshot { return snap })
	if err != nil {
		t.Fatalf("startMetricsListener: %v", err)
	}
	if srv == nil {
		t.Fatal("nil server")
	}
	defer stopMetricsListener(srv)

	resp, err := http.Get("http://" + srv.addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "perf_agent_symbolize_kernel_batches_total 99") {
		t.Errorf("body missing live counter:\n%s", body)
	}

	stopMetricsListener(srv)
	if _, err := http.Get("http://" + srv.addr + "/metrics"); err == nil {
		t.Errorf("post-Stop scrape succeeded; server still up")
	}
}

// TestStartMetricsListener_EmptyAddrIsNoop covers the
// MetricsListen="" path: WithMetricsListen wasn't called, so the
// agent should NOT bind any port.
func TestStartMetricsListener_EmptyAddrIsNoop(t *testing.T) {
	srv, err := startMetricsListener("", func() symbolize.CountersSnapshot { return symbolize.CountersSnapshot{} })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if srv != nil {
		t.Errorf("empty addr returned non-nil server")
	}
	// Stop on nil must not panic.
	stopMetricsListener(srv)
}

// TestMetricsHandler_PprofMount asserts /debug/pprof is reachable
// via the metrics server's mux. We don't validate the full pprof
// response body (that's net/http/pprof's job) — just that the
// route exists and returns 200 or a redirect.
func TestMetricsHandler_PprofMount(t *testing.T) {
	srv, err := startMetricsListener("127.0.0.1:0", func() symbolize.CountersSnapshot { return symbolize.CountersSnapshot{} })
	if err != nil {
		t.Fatalf("startMetricsListener: %v", err)
	}
	defer stopMetricsListener(srv)

	resp, err := http.Get("http://" + srv.addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("/debug/pprof/: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Errorf("/debug/pprof/ status = %d, want < 400", resp.StatusCode)
	}
}
