package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"sync"
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
	go func() {
		done <- waitForCapture(ctx, result, nil, finalSampleGrace, func() error { close(stopped); return nil })
	}()
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
	go func() {
		done <- waitForCapture(ctx, result, nil, finalSampleGrace, func() error { close(stopped); return nil })
	}()
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

// The GPU executions log is told about the files main writes itself, and the
// telemetry output is compared through symlinked directories.
func TestCaptureOutputsAreComparedThroughAliases(t *testing.T) {
	defer func(pmu bool, pmuOut, tel string, cont bool, gpu bool, execOut string) {
		*flagPMU, *flagPMUOutput, *flagGPUTelemetryOutput, *flagContinuous, *flagGPU, *flagGPUExecutionsOutput = pmu, pmuOut, tel, cont, gpu, execOut
	}(*flagPMU, *flagPMUOutput, *flagGPUTelemetryOutput, *flagContinuous, *flagGPU, *flagGPUExecutionsOutput)

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}

	*flagPMU, *flagPMUOutput = true, filepath.Join(dir, "pmu.txt")
	*flagGPUTelemetryOutput, *flagContinuous = filepath.Join(dir, "tel.jsonl"), true
	want := []string{*flagPMUOutput, *flagGPUTelemetryOutput, *flagGPUTelemetryOutput + ".1", *flagGPUTelemetryOutput + ".2"}
	if got := externalOutputs(*flagPMUOutput); !slices.Equal(got, want) {
		t.Errorf("externalOutputs() = %q, want %q", got, want)
	}

	*flagPMU, *flagContinuous = false, false
	*flagGPU, *flagGPUExecutionsOutput = true, filepath.Join(realDir, "same")
	*flagGPUTelemetryOutput = filepath.Join(alias, "same")
	if validateTelemetryOutput() == nil {
		t.Error("telemetry output accepted over the executions log through an aliased directory")
	}
}

// A GPU executions log at the PMU output's path is rejected before the PMU
// file is created and truncated, also when the PMU name is automatic and
// matches a file that already exists.
func TestGPUExecutionsAtThePMUOutputIsRejectedFirst(t *testing.T) {
	defer func(pmu bool, pmuOut string, gpu bool, execOut string) {
		*flagPMU, *flagPMUOutput, *flagGPU, *flagGPUExecutionsOutput = pmu, pmuOut, gpu, execOut
	}(*flagPMU, *flagPMUOutput, *flagGPU, *flagGPUExecutionsOutput)

	dir := t.TempDir()
	t.Chdir(dir)
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	*flagGPU, *flagPMU = true, true

	*flagPMUOutput, *flagGPUExecutionsOutput = filepath.Join(realDir, "pmu.txt"), filepath.Join(alias, "pmu.txt")
	if checkExecutionsOffPMU(pmuOutputPath()) == nil {
		t.Error("executions log accepted at the PMU output's path")
	}

	*flagPMUOutput = "auto"
	auto := pmuOutputPath()
	if auto == "" || auto == "auto" {
		t.Fatalf("automatic PMU name not resolved: %q", auto)
	}
	if err := os.WriteFile(auto, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	*flagGPUExecutionsOutput = auto
	if checkExecutionsOffPMU(auto) == nil {
		t.Error("executions log accepted at the resolved automatic PMU name")
	}

	*flagGPUExecutionsOutput = filepath.Join(realDir, "gpu.exec")
	if err := checkExecutionsOffPMU(auto); err != nil {
		t.Errorf("distinct paths: %v", err)
	}
	*flagPMU = false
	if got := pmuOutputPath(); got != "" {
		t.Errorf("PMU off: pmuOutputPath() = %q", got)
	}
}

// At the deadline the profile is stopped only after the telemetry's final
// sample is published, so the HTML report and the JSONL hold the same
// samples. Before, both happened at once and the report often missed it.
func TestTheFinalTelemetrySampleIsPublishedBeforeTheProfileStops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error, 1)
	final := make(chan struct{})
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	go func() {
		time.Sleep(50 * time.Millisecond) // the final NVML read
		note("final sample")
		close(final)
		result <- nil
	}()
	err := waitForCapture(ctx, result, final, 5*time.Second, func() error { note("profile stopped"); return nil })
	require.NoError(t, err)
	require.Equal(t, []string{"final sample", "profile stopped"}, order)
}

// A final sample that never comes delays the profile's stop by the grace
// period at most.
func TestAStalledFinalSampleDelaysTheProfileByTheGraceOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error)
	stopped := make(chan time.Time, 1)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- waitForCapture(ctx, result, make(chan struct{}), 100*time.Millisecond, func() error { stopped <- time.Now(); return nil })
	}()
	select {
	case at := <-stopped:
		require.Less(t, at.Sub(start), time.Second)
		require.GreaterOrEqual(t, at.Sub(start), 100*time.Millisecond)
	case <-time.After(2 * time.Second):
		t.Fatal("the profile waited for the stalled telemetry past its grace")
	}
	result <- nil
	require.NoError(t, <-done)
}
