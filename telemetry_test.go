package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
