//go:build integration

package perfagent_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"perf-agent/perfagent"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
)

func TestLibraryUsageWithStreaming(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	var profileBuf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithCPUProfileWriter(&profileBuf),
		perfagent.WithSampleRate(99),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(2 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Verify profile data was written
	require.Greater(t, profileBuf.Len(), 0, "profile buffer should have data")

	// Parse the profile to verify it's valid
	prof, err := profile.Parse(&profileBuf)
	require.NoError(t, err)
	require.NotNil(t, prof)
}

func TestLibraryUsageWithOffCPUStreaming(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	var profileBuf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithOffCPUProfileWriter(&profileBuf),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(2 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Off-CPU profile may or may not have data depending on system activity
	// Just verify no errors occurred
	if profileBuf.Len() > 0 {
		prof, err := profile.Parse(&profileBuf)
		require.NoError(t, err)
		require.NotNil(t, prof)
	}
}

func TestLibraryUsageWithPMU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithPMU(),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(2 * time.Second)

	metrics, err := agent.GetMetrics()
	require.NoError(t, err)
	require.NotNil(t, metrics)
	require.True(t, metrics.SystemWide)

	require.NoError(t, agent.Stop(ctx))
}

func TestLibraryUsageCombinedStreaming(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	var cpuBuf, offcpuBuf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithCPUProfileWriter(&cpuBuf),
		perfagent.WithOffCPUProfileWriter(&offcpuBuf),
		perfagent.WithSampleRate(99),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(2 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Verify CPU profile
	require.Greater(t, cpuBuf.Len(), 0, "CPU profile buffer should have data")
	cpuProf, err := profile.Parse(&cpuBuf)
	require.NoError(t, err)
	require.NotNil(t, cpuProf)

	// Off-CPU may or may not have data
	if offcpuBuf.Len() > 0 {
		offcpuProf, err := profile.Parse(&offcpuBuf)
		require.NoError(t, err)
		require.NotNil(t, offcpuProf)
	}
}

func TestLibraryUsageWithPIDStreaming(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	var profileBuf bytes.Buffer

	// Profile ourselves
	pid := os.Getpid()

	agent, err := perfagent.New(
		perfagent.WithPID(pid),
		perfagent.WithCPUProfileWriter(&profileBuf),
		perfagent.WithSampleRate(99),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	// Do some CPU work
	sum := 0
	for i := 0; i < 10000000; i++ {
		sum += i
	}
	_ = sum

	time.Sleep(2 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Profile may or may not have data depending on if test process was sampled
	if profileBuf.Len() > 0 {
		prof, err := profile.Parse(&profileBuf)
		require.NoError(t, err)
		require.NotNil(t, prof)
	}
}

func TestAgentStartStopCycle(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithPMU(),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()

	// First start/stop cycle
	require.NoError(t, agent.Start(ctx))
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, agent.Stop(ctx))

	// Double stop should error
	err = agent.Stop(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not started")

	// Double start should error (after stop, agent is no longer started)
	err = agent.Start(ctx)
	require.Error(t, err)
}

func TestAgentDoubleStart(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithPMU(),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()

	require.NoError(t, agent.Start(ctx))
	defer agent.Stop(ctx)

	// Second start should error
	err = agent.Start(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already started")
}

func TestGetMetricsRequiresPMU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root privileges")
	}

	var buf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithCPUProfileWriter(&buf),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))
	defer agent.Stop(ctx)

	_, err = agent.GetMetrics()
	require.Error(t, err)
	require.Contains(t, err.Error(), "PMU monitor not enabled")
}

func TestGetMetricsRequiresStarted(t *testing.T) {
	agent, err := perfagent.New(
		perfagent.WithSystemWide(),
		perfagent.WithPMU(),
	)
	require.NoError(t, err)
	defer agent.Close()

	_, err = agent.GetMetrics()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not started")
}
