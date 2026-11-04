package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
	"github.com/stretchr/testify/require"
)

func TestTelemetryOutputCollisionBeforeTruncation(t *testing.T) {
	oldEnable, oldPMU, oldOutput, oldPMUOutput := *flagGPUTelemetry, *flagPMU, *flagGPUTelemetryOutput, *flagPMUOutput
	t.Cleanup(func() {
		*flagGPUTelemetry, *flagPMU, *flagGPUTelemetryOutput, *flagPMUOutput = oldEnable, oldPMU, oldOutput, oldPMUOutput
	})
	*flagGPUTelemetry, *flagPMU = true, true
	path := filepath.Join(t.TempDir(), "capture")
	require.NoError(t, os.WriteFile(path, []byte("existing"), 0600))
	*flagPMUOutput = path
	*flagGPUTelemetryOutput = path
	require.ErrorContains(t, validateTelemetryFlags(true), "must differ")
	alias := path + "-alias"
	require.NoError(t, os.Link(path, alias))
	*flagGPUTelemetryOutput = alias
	require.ErrorContains(t, validateTelemetryFlags(true), "must differ")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "existing", string(content))
}

func TestTelemetryOnlyValidatesDeviceScope(t *testing.T) {
	oldEnable, oldPID, oldDevices, oldInterval := *flagGPUTelemetry, *flagPID, *flagGPUTelemetryDevices, *flagGPUTelemetryInterval
	t.Cleanup(func() {
		*flagGPUTelemetry, *flagPID, *flagGPUTelemetryDevices, *flagGPUTelemetryInterval = oldEnable, oldPID, oldDevices, oldInterval
	})
	*flagGPUTelemetry = true
	*flagPID = 0
	require.NoError(t, validateTelemetryFlags(false))
	*flagPID = 42
	require.ErrorContains(t, validateTelemetryFlags(false), "device scope")
	*flagPID = 0
	*flagGPUTelemetryDevices = "MIG-a"
	require.ErrorContains(t, validateTelemetryFlags(false), "physical GPU UUID")
	*flagGPUTelemetryDevices = "GPU-a, GPU-b"
	require.Equal(t, []string{"GPU-a", "GPU-b"}, telemetryUUIDs())
	*flagGPUTelemetryInterval = 0 * time.Second
	require.ErrorContains(t, validateTelemetryFlags(false), "positive")
}

func TestSlowTelemetryDoesNotDelayProfilerStop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error)
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- waitForCapture(ctx, result, func() error { close(stopped); return nil }) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("profiling waited for stalled telemetry")
	}
	result <- errors.New("write failed")
	require.ErrorContains(t, <-done, "write failed")
}

func TestTelemetryFailureDoesNotEndCombinedCaptureEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error)
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- waitForCapture(ctx, result, func() error { close(stopped); return nil }) }()
	result <- errors.New("source unavailable")
	select {
	case <-stopped:
		t.Fatal("telemetry failure stopped profiling early")
	default:
	}
	cancel()
	require.ErrorContains(t, <-done, "source unavailable")
	<-stopped
}

func TestExplicitDefaultIntervalNeedsTelemetry(t *testing.T) {
	oldFlags := flag.CommandLine
	oldEnabled, oldOutput := *flagGPUTelemetry, *flagGPUTelemetryOutput
	t.Cleanup(func() {
		flag.CommandLine = oldFlags
		*flagGPUTelemetry, *flagGPUTelemetryOutput = oldEnabled, oldOutput
	})
	*flagGPUTelemetry, *flagGPUTelemetryOutput = false, ""
	flag.CommandLine = flag.NewFlagSet("telemetry", flag.ContinueOnError)
	flag.CommandLine.Duration("gpu-telemetry-interval", time.Second, "")
	require.NoError(t, flag.CommandLine.Parse([]string{"--gpu-telemetry-interval=1s"}))
	require.ErrorContains(t, validateTelemetryFlags(true), "require --gpu-telemetry")
}

func TestContinuousProfileWindowDoesNotStopTelemetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	profileCtx, endProfile := context.WithCancel(t.Context())
	defer endProfile()
	results := make(chan error, 1)
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- waitForContinuous(ctx, profileCtx, results, func() error { close(stopped); return nil })
	}()
	endProfile()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("profile did not stop")
	}
	select {
	case <-done:
		t.Fatal("telemetry stopped with finite profile")
	default:
	}
	cancel()
	results <- nil
	require.NoError(t, <-done)
}
func TestContinuousLocalFailureStopsProfileImmediately(t *testing.T) {
	results := make(chan error, 1)
	results <- errors.New("disk full")
	stopped := false
	err := waitForContinuous(t.Context(), t.Context(), results, func() error { stopped = true; return nil })
	require.True(t, stopped)
	require.ErrorContains(t, err, "disk full")
}
func TestOTLPEnvironmentDoesNotEnableTelemetry(t *testing.T) {
	oldEnable, oldOutput, oldOTLP := *flagGPUTelemetry, *flagGPUTelemetryOutput, *flagOTLP
	t.Cleanup(func() { *flagGPUTelemetry, *flagGPUTelemetryOutput, *flagOTLP = oldEnable, oldOutput, oldOTLP })
	*flagGPUTelemetry, *flagGPUTelemetryOutput, *flagOTLP = false, "", false
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	require.False(t, gpuTelemetryEnabled())
}

type failingProfile struct{ steps []string }

func (p *failingProfile) Stop(context.Context) error {
	p.steps = append(p.steps, "stop")
	return errors.New("profile write failed")
}
func (p *failingProfile) Close() error { p.steps = append(p.steps, "close"); return nil }
func TestProfileWindowReleasesResourcesEvenIfWritingFails(t *testing.T) {
	p := &failingProfile{}
	require.ErrorContains(t, stopAndCloseProfile(t.Context(), p), "write failed")
	require.Equal(t, []string{"stop", "close"}, p.steps)
}

type consoleRecorder struct{ errors int }

func (*consoleRecorder) Name() string { return "console-test" }
func (e *consoleRecorder) Export(_ context.Context, s *metrics.MetricsSnapshot) error {
	if s.GPU.Kind == "unavailable" {
		e.errors++
	}
	return nil
}
func TestContinuousConsoleReportsOnlyErrorTransitions(t *testing.T) {
	sink := &consoleRecorder{}
	e := &transitionConsole{Exporter: sink}
	failed := &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{Kind: "unavailable", Error: "same failure"}}
	for range 4 {
		require.NoError(t, e.Export(t.Context(), failed))
	}
	require.Equal(t, 1, sink.errors)
	require.NoError(t, e.Export(t.Context(), &metrics.MetricsSnapshot{GPU: &metrics.GPUMetrics{Kind: "initial"}}))
	require.NoError(t, e.Export(t.Context(), failed))
	require.Equal(t, 2, sink.errors)
}

// The GPU sample-shape flags need --gpu; without it they used to be ignored.
func TestGPUSampleFlagsRequireGPU(t *testing.T) {
	defer func(g bool, e string, p bool) {
		*flagGPU, *flagGPUExecutionsOutput, *flagGPUPerExecSamples = g, e, p
	}(*flagGPU, *flagGPUExecutionsOutput, *flagGPUPerExecSamples)

	*flagGPU, *flagGPUExecutionsOutput, *flagGPUPerExecSamples = false, "gpu.exec", false
	if validateGPUSampleFlags() == nil {
		t.Error("--gpu-executions-output accepted without --gpu")
	}
	*flagGPUExecutionsOutput, *flagGPUPerExecSamples = "", true
	if validateGPUSampleFlags() == nil {
		t.Error("--gpu-per-execution-samples accepted without --gpu")
	}
	*flagGPU = true
	if err := validateGPUSampleFlags(); err != nil {
		t.Errorf("with --gpu: %v", err)
	}
}
