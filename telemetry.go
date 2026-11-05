package main

import (
	"github.com/dpsoft/perf-agent/gpu/execlog"

	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/gputelemetry"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/metrics/otlp"
	"github.com/dpsoft/perf-agent/perfagent"
)

var (
	flagContinuous        = flag.Bool("continuous", false, "Collect GPU telemetry until SIGINT/SIGTERM; optional profiling still ends after --duration")
	flagTelemetryWindow   = flag.Duration("gpu-telemetry-window", time.Minute, "Continuous summary and NVML rediscovery window")
	flagTelemetryMaxBytes = flag.Int64("gpu-telemetry-max-bytes", 64<<20, "Continuous JSONL size per file (active file plus two backups)")
	flagOTLP              = flag.Bool("otlp", false, "Export GPU sensor metrics via OTLP/HTTP protobuf (also enables GPU telemetry)")
	flagOTLPEndpoint      = flag.String("otlp-endpoint", "", "OTLP metrics URL; empty uses OTEL_EXPORTER_OTLP_* environment configuration")
	flagOTLPInstance      = flag.String("otlp-instance-id", "", "Explicit service.instance.id for intentionally separate metric writers")
	flagOTLPLockDir       = flag.String("otlp-lock-dir", "", "Shared private directory for cooperative per-GPU writer locks")

	flagGPUTelemetry         = flag.Bool("gpu-telemetry", false, "Collect NVIDIA device sensors during the capture; may run without a PID or profiling mode")
	flagGPUTelemetryOutput   = flag.String("gpu-telemetry-output", "", "Write GPU sensor samples and summary to JSONL (also enables GPU telemetry)")
	flagGPUTelemetryInterval = flag.Duration("gpu-telemetry-interval", time.Second, "GPU sensor polling interval")
	flagGPUTelemetryDevices  = flag.String("gpu-telemetry-devices", "", "Comma-separated physical GPU UUIDs (default: all visible physical GPUs)")
)

func gpuTelemetryEnabled() bool {
	return *flagGPUTelemetry || *flagGPUTelemetryOutput != "" || *flagOTLP
}

func validateTelemetryFlags(profiling bool) error {
	if !*flagOTLP && (*flagOTLPEndpoint != "" || *flagOTLPInstance != "" || *flagOTLPLockDir != "" || flagWasSet("otlp-endpoint") || flagWasSet("otlp-instance-id") || flagWasSet("otlp-lock-dir")) {
		return errors.New("OTLP settings require --otlp")
	}
	if *flagContinuous && !gpuTelemetryEnabled() {
		return errors.New("--continuous requires GPU telemetry or --otlp")
	}
	if !*flagContinuous && (flagWasSet("gpu-telemetry-window") || flagWasSet("gpu-telemetry-max-bytes")) {
		return errors.New("telemetry window and rotation settings require --continuous")
	}
	if *flagContinuous && (*flagTelemetryWindow <= 0 || *flagTelemetryMaxBytes <= 0) {
		return errors.New("continuous telemetry window and size limit must be positive")
	}
	if *flagContinuous && !profiling && flagWasSet("duration") {
		return errors.New("--duration applies to profiling with --continuous; omit it for continuous telemetry alone")
	}

	if !gpuTelemetryEnabled() {
		if flagWasSet("gpu-telemetry-devices") || flagWasSet("gpu-telemetry-interval") || *flagGPUTelemetryDevices != "" || *flagGPUTelemetryInterval != time.Second {
			return errors.New("GPU telemetry settings require --gpu-telemetry or --gpu-telemetry-output")
		}
		return nil
	}
	if *flagDuration <= 0 || *flagGPUTelemetryInterval <= 0 {
		return errors.New("GPU telemetry duration and interval must be positive")
	}
	if !profiling {
		if *flagPID != 0 || *flagAll {
			return errors.New("GPU telemetry has device scope; use --gpu-telemetry-devices to select GPUs, or enable a profiling mode for --pid/-a")
		}
		if *flagGPUShim != "" || *flagMetricsListen != "" || *flagPerPID || *flagFlamegraph != "" {
			return errors.New("--gpu-shim, --metrics-listen, --per-pid and --flamegraph-output require a profiling mode")
		}
	}
	for _, uuid := range telemetryUUIDs() {
		if !strings.HasPrefix(uuid, "GPU-") {
			return fmt.Errorf("GPU telemetry needs physical GPU UUIDs, got %q", uuid)
		}
	}
	return validateTelemetryOutput()
}

func validateTelemetryOutput() error {
	if *flagGPUTelemetryOutput == "" {
		return nil
	}
	outputs := []struct {
		enabled bool
		path    string
	}{
		{*flagPMU, *flagPMUOutput}, {*flagProfile, *flagProfileOutput},
		{*flagOffCpu, *flagOffcpuOutput}, {*flagGPU, *flagGPUOutput},
		{*flagProfile, *flagPerfDataOutput}, {true, *flagFlamegraph},
		{*flagGPU, *flagGPUExecutionsOutput},
	}
	paths := []string{*flagGPUTelemetryOutput}
	if *flagContinuous {
		paths = append(paths, *flagGPUTelemetryOutput+".1", *flagGPUTelemetryOutput+".2")
	}
	for _, path := range paths {
		for _, output := range outputs {
			if !output.enabled || output.path == "" || output.path == "auto" {
				continue
			}
			if execlog.SameDestination(path, output.path) {
				return fmt.Errorf("GPU telemetry output must differ from other capture outputs: %s", output.path)
			}
		}
	}
	return nil
}

func telemetryUUIDs() []string {
	if *flagGPUTelemetryDevices == "" {
		return nil
	}
	var uuids []string
	for uuid := range strings.SplitSeq(*flagGPUTelemetryDevices, ",") {
		uuids = append(uuids, strings.TrimSpace(uuid))
	}
	return uuids
}

func runCapture() (code int) {
	profiling := *flagProfile || *flagOffCpu || *flagGPU || *flagPMU
	if err := validateTelemetryFlags(profiling); err != nil {
		fmt.Fprintln(os.Stderr, "perf-agent:", err)
		return 2
	}
	captureID := rand.Text()
	var reportCollector *metrics.GPUReportCollector
	var agent *perfagent.Agent
	if profiling {
		var err error
		options := append(buildOptions(), perfagent.WithCaptureID(captureID))
		if *flagFlamegraph != "" {
			reportCollector = metrics.NewGPUReportCollector(captureID, time.Time{}, time.Time{})
			options = append(options, perfagent.WithGPUTelemetryReport(reportCollector))
		}
		agent, err = perfagent.New(options...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Failed to create agent:", err)
			return 1
		}
		defer func() {
			if agent != nil {
				if err := agent.Close(); err != nil {
					fmt.Fprintln(os.Stderr, "Error closing agent:", err)
					code = 1
				}
			}
		}()
	}
	if pmuFile != nil {
		defer func() {
			if pmuFile != nil {
				if err := pmuFile.Close(); err != nil {
					fmt.Fprintln(os.Stderr, "Error closing PMU output:", err)
					code = 1
				}
			}
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var console metrics.Exporter = metrics.NewConsoleExporter(false)
	if *flagContinuous {
		console = &transitionConsole{Exporter: console}
	}
	exporters := []metrics.Exporter{console}
	if *flagGPUTelemetryOutput != "" {
		var output io.WriteCloser
		var err error
		if *flagContinuous {
			output, err = metrics.NewRotatingWriter(*flagGPUTelemetryOutput, *flagTelemetryMaxBytes)
		} else {
			output, err = os.Create(*flagGPUTelemetryOutput)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "GPU telemetry output:", err)
			return 1
		}
		defer func() {
			if err := output.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "Error closing GPU telemetry output:", err)
				code = 1
			}
		}()
		exporters = append(exporters, &metrics.GPUJSONLExporter{Writer: output})
	}
	if *flagOTLP {
		version := "devel"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			version = info.Main.Version
		}
		remote, err := otlp.New(ctx, otlp.Options{Endpoint: *flagOTLPEndpoint, InstanceID: *flagOTLPInstance, LockDir: *flagOTLPLockDir, Version: version, DeviceUUIDs: telemetryUUIDs(), Warn: func(message string) { fmt.Fprintln(os.Stderr, "OTLP:", message) }})
		if err != nil {
			fmt.Fprintln(os.Stderr, "OTLP:", err)
			return 1
		}
		exporters = append(exporters, remote)
		reportCtx, stopReport := context.WithCancel(context.Background())
		reportDone := make(chan struct{})
		go func() {
			defer close(reportDone)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-reportCtx.Done():
					return
				case <-ticker.C:
					reportOTLP(remote.Stats())
				}
			}
		}()
		defer func() {
			stopReport()
			<-reportDone
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := remote.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintln(os.Stderr, "OTLP shutdown:", err)
			}
			stats := remote.Stats()
			reportOTLP(stats)
			if stats.SentSensorPoints == 0 && *flagGPUTelemetryOutput == "" && !profiling {
				fmt.Fprintln(os.Stderr, "OTLP: no sensor points delivered")
				code = 1
			}
		}()
	}
	finalSampled := make(chan struct{})
	collector := &gputelemetry.Collector{CaptureID: captureID, Source: gputelemetry.NewNVML(), UUIDs: telemetryUUIDs(), Interval: *flagGPUTelemetryInterval, Exporters: exporters}
	if !*flagContinuous {
		// Continuous telemetry outlives the profile; its final sample is not
		// the profile's.
		collector.FinalSampled = finalSampled
	}
	if agent != nil && *flagFlamegraph != "" {
		collector.Exporters = append([]metrics.Exporter{reportCollector.LiveExporter()}, collector.Exporters...)
		// The profile's window opens when the agent starts, before attaching,
		// which can take seconds (a Python process). Sampling starts with it,
		// not after, so the report covers the whole window.
		collector.StartAfter = reportCollector.Begun()
	}
	// The telemetry runs while the agent attaches. A finite capture's
	// deadline is still counted from the end of Start, as before; the
	// telemetry is cancelled at that same deadline.
	run := collector.Run
	if *flagContinuous {
		run = func(ctx context.Context) error { return collector.RunContinuous(ctx, *flagTelemetryWindow) }
	}
	cancelTelemetry, result := startTelemetry(ctx, run)
	defer cancelTelemetry()
	// Keep the profiler context alive through its final collection.
	profileCtx, cancelProfile := context.WithCancel(context.Background())
	defer cancelProfile()
	if agent != nil {
		if err := agent.Start(profileCtx); err != nil {
			fmt.Fprintln(os.Stderr, "Failed to start agent:", err)
			cancelTelemetry()
			<-result
			return 1
		}
	}
	captureCtx, cancel := context.WithTimeout(ctx, *flagDuration)
	defer cancel()
	if !*flagContinuous {
		stopTelemetry := context.AfterFunc(captureCtx, cancelTelemetry)
		defer stopTelemetry()
	}
	if *flagContinuous {
		fmt.Println("Collecting GPU telemetry until SIGINT/SIGTERM...")
	} else {
		fmt.Printf("Collecting data for %v (Ctrl+C to stop early)...\n", *flagDuration)
	}
	var stopProfiler func() error
	if agent != nil {
		stopProfiler = func() error {
			defer cancelProfile()
			err := stopAndCloseProfile(profileCtx, agent)
			agent = nil
			if pmuFile != nil {
				err = errors.Join(err, pmuFile.Close())
				pmuFile = nil
			}
			return err
		}
	}
	var err error
	if *flagContinuous {
		err = waitForContinuous(ctx, captureCtx, result, stopProfiler)
	} else {
		err = waitForCapture(captureCtx, result, finalSampled, finalSampleGrace, stopProfiler)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Capture:", err)
		code = 1
	}

	return code
}

// startTelemetry runs the collector in the background on a context of its
// own, derived from ctx, in finite and continuous mode alike: cancelling it
// must stop the collector, which is how a failed agent start is unwound.
func startTelemetry(ctx context.Context, run func(context.Context) error) (cancel context.CancelFunc, result <-chan error) {
	telemetryCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- run(telemetryCtx) }()
	return cancel, done
}

// finalSampleGrace bounds how long the end of a finite capture waits for the
// telemetry's final sample before stopping the profile. That sample is taken
// at the same deadline and normally lands within milliseconds; a stalled
// driver must not hold the profile open.
const finalSampleGrace = 500 * time.Millisecond

// waitForCapture ends a finite capture: at the deadline it gives the
// telemetry up to grace to publish its final sample (finalSampled), so the
// profile's report holds the same observations as the telemetry output, then
// stops the profile and waits for the telemetry to finish.
func waitForCapture(ctx context.Context, result <-chan error, finalSampled <-chan struct{}, grace time.Duration, stopProfiler func() error) error {
	var telemetryErr error
	select {
	case <-ctx.Done():
		if stopProfiler != nil {
			timer := time.NewTimer(grace)
			select {
			case <-finalSampled:
			case telemetryErr = <-result:
				result = nil
			case <-timer.C:
			}
			timer.Stop()
		}
	case telemetryErr = <-result:
		result = nil
		if stopProfiler != nil {
			<-ctx.Done()
		}
	}
	var profileErr error
	if stopProfiler != nil {
		profileErr = stopProfiler()
	}
	if result != nil {
		telemetryErr = <-result
	}
	return errors.Join(profileErr, telemetryErr)
}

func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// The finite profile and continuous telemetry have separate lifetimes. A fatal
// local telemetry error stops the profile immediately; its normal deadline does
// not terminate telemetry. SIGINT/SIGTERM closes both.
func waitForContinuous(ctx, profileDeadline context.Context, result <-chan error, stopProfiler func() error) error {
	var profileErr error
	var deadline <-chan struct{}
	if stopProfiler != nil {
		deadline = profileDeadline.Done()
	}
	stop := func() {
		if stopProfiler != nil {
			profileErr = stopProfiler()
			stopProfiler = nil
			deadline = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			stop()
			return errors.Join(profileErr, <-result)
		case err := <-result:
			stop()
			return errors.Join(profileErr, err)
		case <-deadline:
			stop()
		}
	}
}

func reportOTLP(s otlp.Stats) {
	fmt.Fprintf(os.Stderr, "OTLP: sent_points=%d sent_sensor_points=%d failed_points=%d coalesced_points=%d dropped_points=%d contended_points=%d requests=%d failed_requests=%d unavailable_records=%d\n", s.SentPoints, s.SentSensorPoints, s.FailedPoints, s.CoalescedPoints, s.DroppedPoints, s.ContendedPoints, s.Requests, s.FailedRequests, s.UnavailableRecords)
}

type transitionConsole struct {
	metrics.Exporter
	lastError string
}

func (e *transitionConsole) Export(ctx context.Context, s *metrics.MetricsSnapshot) error {
	if s.GPU != nil {
		if s.GPU.Kind == "unavailable" {
			if s.GPU.Error == e.lastError {
				return nil
			}
			e.lastError = s.GPU.Error
		} else if e.lastError != "" {
			fmt.Fprintln(os.Stderr, "GPU telemetry source recovered")
			e.lastError = ""
		}
	}
	return e.Exporter.Export(ctx, s)
}

type captureAgent interface {
	Stop(context.Context) error
	Close() error
}

func stopAndCloseProfile(ctx context.Context, agent captureAgent) error {
	// Stop writes the final profile; Close detaches BPF/PMU and closes writers.
	// Telemetry may continue indefinitely, so defer alone is insufficient.
	return errors.Join(agent.Stop(ctx), agent.Close())
}
