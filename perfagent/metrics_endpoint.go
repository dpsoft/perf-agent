package perfagent

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on its own mux
	"time"

	"github.com/dpsoft/perf-agent/symbolize"
)

// metricsServer wraps the optional HTTP server exposing /metrics
// and /debug/pprof. Lifecycle is tied to Agent.Start /
// Agent.cleanup — see startMetricsListener / stopMetricsListener.
type metricsServer struct {
	httpSrv *http.Server
	addr    string  // resolved listener address (after net.Listen, for "got :0" cases in tests)
	listener net.Listener
}

// metricsHandlerFor returns a Prometheus-text handler that
// snapshots the given symbolizer's counters per request. Exposed
// as a function (not a method) so it can be unit-tested without
// spinning up the full Agent + HTTP server.
func metricsHandlerFor(getCounters func() symbolize.CountersSnapshot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s := getCounters()
		writeMetricLine(w, "perf_agent_symbolize_kernel_batches_total",
			"counter", "number of SymbolizeKernel calls reaching the symbolize layer", s.KernelBatches)
		writeMetricLine(w, "perf_agent_symbolize_kernel_input_ips_total",
			"counter", "total kernel IPs handed into SymbolizeKernel", s.KernelInputIPs)
		writeMetricLine(w, "perf_agent_symbolize_kernel_batch_failures_total",
			"counter", "batches where every symbolizer (blazesym + kallsyms) failed", s.KernelBatchFailures)
		writeMetricLine(w, "perf_agent_symbolize_kernel_fallback_engaged",
			"gauge", "1 when symbolizer switched to pure-Go kallsyms (lockdown-class hosts)", s.KernelFallbackEngaged)
		writeMetricLine(w, "perf_agent_symbolize_kernel_raw_addr_frames_total",
			"counter", "kernel IPs that fell to raw-hex synthesis (both symbolizers failed)", s.KernelRawAddrFrames)
		writeMetricLine(w, "perf_agent_symbolize_kernel_lockdown_eperm_total",
			"counter", "BLAZE_ERR_PERMISSION_DENIED events from blazesym (canonical lockdown signature)", s.KernelLockdownEPERM)
		writeMetricLine(w, "perf_agent_symbolize_kernel_other_err_total",
			"counter", "non-EPERM blazesym kernel failures (unexpected, deserves a look)", s.KernelOtherErr)
		// Batch-duration histogram fields (roadmap #2). Exposed
		// as separate gauges for p50/p99/max so Prometheus
		// scrapers can graph each percentile independently
		// without parsing a histogram body.
		writeMetricLine(w, "perf_agent_symbolize_kernel_batch_p50_microseconds",
			"gauge", "p50 of SymbolizeKernel batch wall-clock duration over recent window", s.KernelBatchHist.P50Us)
		writeMetricLine(w, "perf_agent_symbolize_kernel_batch_p99_microseconds",
			"gauge", "p99 of SymbolizeKernel batch wall-clock duration over recent window", s.KernelBatchHist.P99Us)
		writeMetricLine(w, "perf_agent_symbolize_kernel_batch_max_microseconds",
			"gauge", "max of SymbolizeKernel batch wall-clock duration (lifetime, not window)", s.KernelBatchHist.MaxUs)
	}
}

// writeMetricLine emits one Prometheus exposition fragment with
// type + help + value (no labels — all of our counters are
// agent-process-global so labels would just be noise).
func writeMetricLine(w io.Writer, name, kind, help string, value uint64) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, kind, name, value)
}

// startMetricsListener spins up the HTTP server when
// config.MetricsListen is non-empty. Safe to call regardless of
// whether MetricsListen is set — returns nil server when disabled,
// which stopMetricsListener gracefully handles.
//
// The server uses a dedicated mux (NOT http.DefaultServeMux directly)
// for /metrics; /debug/pprof handlers are still on the default
// mux per the net/http/pprof package's init(), so we delegate
// /debug/pprof routes to DefaultServeMux from our own mux.
func startMetricsListener(addr string, getCounters func() symbolize.CountersSnapshot) (*metricsServer, error) {
	if addr == "" {
		return nil, nil
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("perfagent: bind %s for /metrics: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metricsHandlerFor(getCounters))
	// /debug/pprof/* lives on DefaultServeMux courtesy of net/http/pprof.
	// Mount it on our mux too so a single addr serves both.
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Printf("perfagent: metrics listener exited: %v", err)
		}
	}()
	return &metricsServer{httpSrv: srv, addr: lis.Addr().String(), listener: lis}, nil
}

// stopMetricsListener shuts the server down with a short grace
// period so any in-flight scrape finishes cleanly. nil-safe.
func stopMetricsListener(s *metricsServer) {
	if s == nil || s.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
}
