package test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/perfagent"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkload represents a test workload
type TestWorkload struct {
	Name     string
	Binary   string
	Args     []string
	Language string
}

var workloads = []TestWorkload{
	{
		Name:     "go-cpu",
		Binary:   "./workloads/go/cpu_bound",
		Args:     []string{"-duration=20s", "-threads=4"},
		Language: "go",
	},
	{
		Name:     "go-io",
		Binary:   "./workloads/go/io_bound",
		Args:     []string{"-duration=20s", "-threads=2"},
		Language: "go",
	},
	{
		Name:     "rust-cpu",
		Binary:   "./workloads/rust/target/release/rust-workload",
		Args:     []string{"20", "4"},
		Language: "rust",
	},
	{
		Name:     "python-cpu",
		Binary:   "python3",
		Args:     []string{"-X", "perf", "./workloads/python/cpu_bound.py", "20", "4"},
		Language: "python",
	},
	{
		Name:     "python-io",
		Binary:   "python3",
		Args:     []string{"-X", "perf", "./workloads/python/io_bound.py", "20", "2"},
		Language: "python",
	},
}

func TestProfileMode(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	for _, wl := range workloads {
		t.Run(wl.Name, func(t *testing.T) {
			// Start workload
			workload := exec.Command(wl.Binary, wl.Args...)
			require.NoError(t, workload.Start())
			defer func() {
				if workload.Process != nil {
					workload.Process.Kill()
					workload.Wait()
				}
			}()

			// Python workloads now have built-in warmup, so we can use shorter wait
			if wl.Language == "python" {
				// Reduced from 5s to 3s because warmup is now internal
				time.Sleep(3 * time.Second) // Wait for warmup to complete
			} else {
				time.Sleep(2 * time.Second) // Let workload stabilize
			}

			// Run perf-agent
			outputFile := "profile.pb.gz"
			defer os.Remove(outputFile)

			agent := exec.Command(agentPath,
				"--profile",
				"--profile-output", outputFile,
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", "10s",
			)

			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}

			// Verify profile.pb.gz was created
			assert.FileExists(t, outputFile)

			// Parse and validate profile
			prof := parseProfile(t, outputFile)
			require.NotNil(t, prof)

			// Should have samples
			assert.Greater(t, len(prof.Sample), 0, "Profile should contain samples")

			// Should have valid sample types
			require.Greater(t, len(prof.SampleType), 0)
			sampleType := prof.SampleType[0].Type
			assert.True(t, sampleType == "sample" || sampleType == "cpu" || sampleType == "samples",
				"Expected sample type to be 'sample', 'cpu', or 'samples', got: %s", sampleType)

			// Verify we captured stack traces
			hasStacks := false
			for _, sample := range prof.Sample {
				if len(sample.Location) > 0 {
					hasStacks = true
					break
				}
			}
			assert.True(t, hasStacks, "Profile should contain stack traces")

			// Verify symbolization worked (at least some symbols)
			hasSymbols := false
			for _, fn := range prof.Function {
				if fn.Name != "" && fn.Name != "??" {
					hasSymbols = true
					break
				}
			}
			assert.True(t, hasSymbols, "Profile should contain symbolized functions")
		})
	}
}

func TestOffCPUMode(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// Test only I/O workloads for off-CPU
	ioWorkloads := []TestWorkload{workloads[1], workloads[4]}

	for _, wl := range ioWorkloads {
		t.Run(wl.Name, func(t *testing.T) {
			// Start workload
			workload := exec.Command(wl.Binary, wl.Args...)
			require.NoError(t, workload.Start())
			defer func() {
				if workload.Process != nil {
					workload.Process.Kill()
					workload.Wait()
				}
			}()

			time.Sleep(2 * time.Second)

			// Run perf-agent with off-CPU profiling
			outputFile := "offcpu.pb.gz"
			defer os.Remove(outputFile)

			agent := exec.Command(agentPath,
				"--offcpu",
				"--offcpu-output", outputFile,
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", "10s",
			)

			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}

			// Verify offcpu.pb.gz was created
			assert.FileExists(t, outputFile)

			// Parse and validate profile
			prof := parseProfile(t, outputFile)
			require.NotNil(t, prof)

			// Should have samples (I/O workloads block on I/O)
			assert.Greater(t, len(prof.Sample), 0, "Off-CPU profile should contain samples")
		})
	}
}

func TestPMUMode(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)
	wl := workloads[0] // Go CPU workload

	// Start workload
	workload := exec.Command(wl.Binary, wl.Args...)
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run perf-agent with PMU
	agent := exec.Command(agentPath,
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)

	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)

	// Verify PMU metrics are present
	assert.Contains(t, outputStr, "Metrics")
	assert.Contains(t, outputStr, "Samples:")
	assert.Contains(t, outputStr, "On-CPU Time")
	assert.Contains(t, outputStr, "P50:")
	assert.Contains(t, outputStr, "P99:")

	// Verify new runqueue latency metrics
	assert.Contains(t, outputStr, "Runqueue Latency")

	// Verify context switch reasons
	assert.Contains(t, outputStr, "Context Switch Reasons")
	assert.Contains(t, outputStr, "Preempted")
	assert.Contains(t, outputStr, "Voluntary")
}

func TestCombinedMode(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)
	wl := workloads[0] // Go CPU workload

	// Start workload
	workload := exec.Command(wl.Binary, wl.Args...)
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run perf-agent with all features
	agent := exec.Command(agentPath,
		"--profile",
		"--profile-output", "profile.pb.gz",
		"--offcpu",
		"--offcpu-output", "offcpu.pb.gz",
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "10s",
	)

	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	// Verify both profile files exist
	defer os.Remove("profile.pb.gz")
	defer os.Remove("offcpu.pb.gz")

	assert.FileExists(t, "profile.pb.gz")
	assert.FileExists(t, "offcpu.pb.gz")
	assert.Contains(t, string(output), "Metrics")

	// Verify profiles are valid
	cpuProf := parseProfile(t, "profile.pb.gz")
	assert.Greater(t, len(cpuProf.Sample), 0)

	offcpuProf := parseProfile(t, "offcpu.pb.gz")
	assert.NotNil(t, offcpuProf)
}

func getAgentPath(t *testing.T) string {
	// Look for perf-agent binary in parent directory
	agentPath := "../perf-agent"
	if _, err := os.Stat(agentPath); os.IsNotExist(err) {
		t.Fatalf("perf-agent binary not found at %s. Run 'go build' first.", agentPath)
	}
	abs, err := filepath.Abs(agentPath)
	require.NoError(t, err)
	return abs
}

func parseProfile(t *testing.T, filename string) *profile.Profile {
	f, err := os.Open(filename)
	require.NoError(t, err)
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer gzr.Close()

	data, err := io.ReadAll(gzr)
	require.NoError(t, err)

	prof, err := profile.Parse(bytes.NewReader(data))
	require.NoError(t, err)

	return prof
}

// System-wide profiling tests

func TestSystemWideProfile(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// Start multiple workloads
	workload1 := exec.Command("./workloads/go/cpu_bound", "-duration=15s", "-threads=2")
	workload2 := exec.Command("./workloads/go/io_bound", "-duration=15s", "-threads=2")
	require.NoError(t, workload1.Start())
	require.NoError(t, workload2.Start())
	defer func() {
		if workload1.Process != nil {
			workload1.Process.Kill()
			workload1.Wait()
		}
		if workload2.Process != nil {
			workload2.Process.Kill()
			workload2.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run system-wide profiling
	outputFile := "profile.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath, "--profile", "--profile-output", outputFile, "-a", "--duration", "5s")
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	assert.Contains(t, string(output), "system-wide")
	assert.FileExists(t, outputFile)

	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	assert.Greater(t, len(prof.Sample), 0, "System-wide profile should contain samples")
}

func TestSystemWideOffCPU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	workload := exec.Command("./workloads/go/io_bound", "-duration=15s", "-threads=2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	outputFile := "offcpu.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath, "--offcpu", "--offcpu-output", outputFile, "-a", "--duration", "5s")
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	assert.Contains(t, string(output), "system-wide")
	assert.FileExists(t, outputFile)
}

func TestSystemWidePMU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	workload := exec.Command("./workloads/go/cpu_bound", "-duration=15s", "-threads=2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	agent := exec.Command(agentPath, "--pmu", "-a", "--duration", "5s")
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)
	assert.Contains(t, outputStr, "System-Wide")
	assert.Contains(t, outputStr, "Processes profiled")
	assert.NotContains(t, outputStr, "--- PID")
}

func TestSystemWidePMUPerPID(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	workload1 := exec.Command("./workloads/go/cpu_bound", "-duration=15s", "-threads=2")
	workload2 := exec.Command("./workloads/go/io_bound", "-duration=15s", "-threads=2")
	require.NoError(t, workload1.Start())
	require.NoError(t, workload2.Start())
	defer func() {
		if workload1.Process != nil {
			workload1.Process.Kill()
			workload1.Wait()
		}
		if workload2.Process != nil {
			workload2.Process.Kill()
			workload2.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	agent := exec.Command(agentPath, "--pmu", "-a", "--per-pid", "--duration", "5s")
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)
	assert.Contains(t, outputStr, "System-Wide, Per-PID")
	assert.Contains(t, outputStr, "--- PID")
}

func TestMutuallyExclusiveFlags(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// --pid and -a should be mutually exclusive
	agent := exec.Command(agentPath, "--profile", "--pid", "1234", "-a", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "mutually exclusive")
}

func TestRequiresPIDOrAll(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// Should fail without --pid or -a
	agent := exec.Command(agentPath, "--profile", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "required")
}

func TestPerPIDRequiresAll(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// --per-pid should require -a
	agent := exec.Command(agentPath, "--pmu", "--pid", "1234", "--per-pid", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "--per-pid requires")
}

func TestPerPIDRequiresPMU(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// --per-pid should require --pmu
	agent := exec.Command(agentPath, "--profile", "-a", "--per-pid", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "only valid with --pmu")
}

// Tests for new runqueue latency and task state features

func TestPMURunqueueLatency(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)
	wl := workloads[0] // Go CPU workload

	// Start workload
	workload := exec.Command(wl.Binary, wl.Args...)
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run perf-agent with PMU
	agent := exec.Command(agentPath,
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)

	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)

	// Verify runqueue latency histogram is present
	assert.Contains(t, outputStr, "Runqueue Latency (time waiting for CPU)")

	// Verify percentile values are present for runqueue latency
	// The output should have two sets of percentiles: On-CPU and Runqueue
	assert.Contains(t, outputStr, "Min:")
	assert.Contains(t, outputStr, "Max:")
	assert.Contains(t, outputStr, "Mean:")
	assert.Contains(t, outputStr, "P50:")
	assert.Contains(t, outputStr, "P95:")
	assert.Contains(t, outputStr, "P99:")
	assert.Contains(t, outputStr, "P99.9:")
}

func TestPMUTaskStateClassification(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	// Use I/O workload to ensure we see different task states
	wl := workloads[1] // Go I/O workload

	// Start workload
	workload := exec.Command(wl.Binary, wl.Args...)
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run perf-agent with PMU
	agent := exec.Command(agentPath,
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)

	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)

	// Verify context switch reasons are classified
	assert.Contains(t, outputStr, "Context Switch Reasons:")
	assert.Contains(t, outputStr, "Preempted (running):")
	assert.Contains(t, outputStr, "Voluntary (sleep/mutex):")
	assert.Contains(t, outputStr, "I/O Wait (D state):")

	// Verify percentages are shown
	assert.Contains(t, outputStr, "%")
	assert.Contains(t, outputStr, "times)")
}

func TestPMUIOWorkloadHasIOWait(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)
	wl := workloads[1] // Go I/O workload

	// Start I/O-bound workload
	workload := exec.Command(wl.Binary, wl.Args...)
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run perf-agent with PMU
	agent := exec.Command(agentPath,
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)

	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)

	// I/O workload should show some I/O wait or voluntary sleep
	// (file operations cause both)
	hasIOActivity := false
	if assert.Contains(t, outputStr, "I/O Wait (D state):") ||
		assert.Contains(t, outputStr, "Voluntary (sleep/mutex):") {
		hasIOActivity = true
	}
	assert.True(t, hasIOActivity, "I/O workload should show I/O or voluntary sleep activity")
}

func TestPMUCPUWorkloadMostlyRunning(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)
	wl := workloads[0] // Go CPU workload

	// Start CPU-bound workload
	workload := exec.Command(wl.Binary, wl.Args...)
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	// Run perf-agent with PMU
	agent := exec.Command(agentPath,
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)

	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)
	t.Logf("Output:\n%s", outputStr)

	// CPU-bound workload should show preempted switches
	// (it gets preempted because it never voluntarily yields)
	assert.Contains(t, outputStr, "Preempted (running):")
}

func TestSystemWidePMUWithNewMetrics(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	agentPath := getAgentPath(t)

	workload := exec.Command("./workloads/go/cpu_bound", "-duration=15s", "-threads=2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	agent := exec.Command(agentPath, "--pmu", "-a", "--duration", "5s")
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}

	outputStr := string(output)

	// System-wide aggregate should include context switch reasons
	assert.Contains(t, outputStr, "Context Switch Reasons (aggregate):")
	assert.Contains(t, outputStr, "Preempted (running):")
	assert.Contains(t, outputStr, "Voluntary (sleep/mutex):")
	assert.Contains(t, outputStr, "I/O Wait (D state):")
}

// Library streaming tests

func TestStreamingProfileOutput(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	// Start a CPU workload
	workload := exec.Command("./workloads/go/cpu_bound", "-duration=20s", "-threads=4")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second) // warmup

	var buf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithPID(workload.Process.Pid),
		perfagent.WithCPUProfileWriter(&buf),
		perfagent.WithSampleRate(99),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(3 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Verify profile
	require.Greater(t, buf.Len(), 0, "profile buffer should have data")

	prof, err := profile.Parse(&buf)
	require.NoError(t, err)
	require.Greater(t, len(prof.Sample), 0, "profile should contain samples")

	// Verify we got symbolized functions
	hasSymbols := false
	for _, fn := range prof.Function {
		if fn.Name != "" && fn.Name != "??" {
			hasSymbols = true
			break
		}
	}
	assert.True(t, hasSymbols, "Profile should contain symbolized functions")
}

func TestStreamingOffCPUProfileOutput(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	// Start an I/O workload
	workload := exec.Command("./workloads/go/io_bound", "-duration=20s", "-threads=2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second) // warmup

	var buf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithPID(workload.Process.Pid),
		perfagent.WithOffCPUProfileWriter(&buf),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(3 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Off-CPU profile should have data for I/O workload
	if buf.Len() > 0 {
		prof, err := profile.Parse(&buf)
		require.NoError(t, err)
		require.Greater(t, len(prof.Sample), 0, "off-CPU profile should contain samples")
	}
}

func TestStreamingCombinedProfileOutput(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	// Start workload
	workload := exec.Command("./workloads/go/cpu_bound", "-duration=20s", "-threads=4")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	var cpuBuf, offcpuBuf bytes.Buffer

	agent, err := perfagent.New(
		perfagent.WithPID(workload.Process.Pid),
		perfagent.WithCPUProfileWriter(&cpuBuf),
		perfagent.WithOffCPUProfileWriter(&offcpuBuf),
		perfagent.WithSampleRate(99),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	time.Sleep(3 * time.Second)

	require.NoError(t, agent.Stop(ctx))

	// Verify CPU profile
	require.Greater(t, cpuBuf.Len(), 0, "CPU profile buffer should have data")
	cpuProf, err := profile.Parse(&cpuBuf)
	require.NoError(t, err)
	require.NotNil(t, cpuProf)

	// Off-CPU may or may not have data for CPU-bound workload
	if offcpuBuf.Len() > 0 {
		offcpuProf, err := profile.Parse(&offcpuBuf)
		require.NoError(t, err)
		require.NotNil(t, offcpuProf)
	}
}

func TestLibraryPMUMetrics(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	// Start a CPU workload
	workload := exec.Command("./workloads/go/cpu_bound", "-duration=20s", "-threads=4")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second) // warmup

	// Test library usage with PMU
	agent, err := perfagent.New(
		perfagent.WithPID(workload.Process.Pid),
		perfagent.WithPMU(),
	)
	require.NoError(t, err)
	defer agent.Close()

	ctx := context.Background()
	require.NoError(t, agent.Start(ctx))

	// Collect for a few seconds
	time.Sleep(3 * time.Second)

	// Test GetMetrics() API
	snapshot, err := agent.GetMetrics()
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	// Verify snapshot contains data
	if snapshot != nil {
		// Should have at least one process with metrics
		if len(snapshot.Processes) > 0 {
			for pid, pm := range snapshot.Processes {
				t.Logf("PID %d: Samples=%d, Preempted=%d, Voluntary=%d, IOWait=%d",
					pid, pm.SampleCount, pm.ContextSwitches.PreemptedCount,
					pm.ContextSwitches.VoluntaryCount, pm.ContextSwitches.IOWaitCount)
				assert.Greater(t, pm.SampleCount, uint64(0))

				// Verify new metrics are present
				assert.Greater(t, pm.RunqueueStats.Count, int64(0), "should have runqueue latency data")
				assert.Greater(t, pm.OnCPUStats.Count, int64(0), "should have on-CPU time data")
			}
		}
	}

	require.NoError(t, agent.Stop(ctx))
}
