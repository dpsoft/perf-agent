package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/gputelemetry"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/perfagent"
)

var (
	flagGPUTelemetry         = flag.Bool("gpu-telemetry", false, "Collect NVIDIA device sensors during the capture; may run without a PID or profiling mode")
	flagGPUTelemetryOutput   = flag.String("gpu-telemetry-output", "", "Write GPU sensor samples and summary to JSONL (also enables GPU telemetry)")
	flagGPUTelemetryInterval = flag.Duration("gpu-telemetry-interval", time.Second, "GPU sensor polling interval")
	flagGPUTelemetryDevices  = flag.String("gpu-telemetry-devices", "", "Comma-separated physical GPU UUIDs (default: all visible physical GPUs)")
)

func gpuTelemetryEnabled() bool { return *flagGPUTelemetry || *flagGPUTelemetryOutput != "" }

func validateTelemetryFlags(profiling bool) error {
	if !gpuTelemetryEnabled() {
		if *flagGPUTelemetryDevices != "" || *flagGPUTelemetryInterval != time.Second {
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
	}
	telemetryPath, err := filepath.Abs(*flagGPUTelemetryOutput)
	if err != nil {
		return err
	}
	telemetryInfo, _ := os.Stat(telemetryPath)
	for _, output := range outputs {
		if !output.enabled || output.path == "" || output.path == "auto" {
			continue
		}
		other, err := filepath.Abs(output.path)
		if err != nil {
			return err
		}
		info, _ := os.Stat(other)
		if other == telemetryPath || (info != nil && telemetryInfo != nil && os.SameFile(info, telemetryInfo)) {
			return fmt.Errorf("GPU telemetry output must differ from other capture outputs: %s", output.path)
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
	var agent *perfagent.Agent
	if profiling || !gpuTelemetryEnabled() {
		var err error
		agent, err = perfagent.New(buildOptions()...)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Failed to create agent:", err)
			return 1
		}
		defer func() {
			if err := agent.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "Error closing agent:", err)
				code = 1
			}
		}()
	}
	if pmuFile != nil {
		defer func() {
			if err := pmuFile.Close(); err != nil {
				fmt.Fprintln(os.Stderr, "Error closing PMU output:", err)
				code = 1
			}
		}()
	}
	var collector *gputelemetry.Collector
	if gpuTelemetryEnabled() {
		exporters := []metrics.Exporter{metrics.NewConsoleExporter(false)}
		if *flagGPUTelemetryOutput != "" {
			file, err := os.Create(*flagGPUTelemetryOutput)
			if err != nil {
				fmt.Fprintln(os.Stderr, "GPU telemetry output:", err)
				return 1
			}
			defer func() {
				if err := file.Close(); err != nil {
					fmt.Fprintln(os.Stderr, "Error closing GPU telemetry output:", err)
					code = 1
				}
			}()
			exporters = append(exporters, &metrics.GPUJSONLExporter{Writer: file})
		}
		collector = &gputelemetry.Collector{Source: gputelemetry.NewNVML(), UUIDs: telemetryUUIDs(), Interval: *flagGPUTelemetryInterval, Exporters: exporters}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Keep the profiler context alive through its final collection.
	profileCtx, cancelProfile := context.WithCancel(context.Background())
	defer cancelProfile()
	if agent != nil {
		if err := agent.Start(profileCtx); err != nil {
			fmt.Fprintln(os.Stderr, "Failed to start agent:", err)
			return 1
		}
	}
	captureCtx, cancel := context.WithTimeout(ctx, *flagDuration)
	defer cancel()
	var result chan error
	if collector != nil {
		result = make(chan error, 1)
		go func() { result <- collector.Run(captureCtx) }()
	}
	fmt.Printf("Collecting data for %v (Ctrl+C to stop early)...\n", *flagDuration)
	var stopProfiler func() error
	if agent != nil {
		stopProfiler = func() error {
			defer cancelProfile()
			return agent.Stop(profileCtx)
		}
	}
	if err := waitForCapture(captureCtx, result, stopProfiler); err != nil {
		fmt.Fprintln(os.Stderr, "Capture:", err)
		code = 1
	}
	return code
}

func waitForCapture(ctx context.Context, result <-chan error, stopProfiler func() error) error {
	var telemetryErr error
	select {
	case <-ctx.Done():
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
