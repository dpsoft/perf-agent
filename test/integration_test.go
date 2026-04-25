package test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/perfagent"
	perfprofile "github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehcompile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"

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

			// S9: verify pprof fidelity guarantees
			assertPprofFidelity(t, outputFile)
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

// assertPprofFidelity verifies the S9 pprof fidelity guarantees on a
// captured profile: >=2 real (non-sentinel) mappings, at least one
// mapping with a non-empty BuildID, every user-space Location has a
// non-zero Address. Skips kernel and JIT sentinel mappings.
func assertPprofFidelity(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	defer f.Close()

	p, err := profile.Parse(f)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if err := p.CheckValid(); err != nil {
		t.Fatalf("pprof invalid: %v", err)
	}

	var real int
	var hasBuildID bool
	for _, m := range p.Mapping {
		if m.File != "" && m.File != "[kernel]" && m.File != "[jit]" {
			real++
		}
		if m.BuildID != "" {
			hasBuildID = true
		}
	}
	if real < 2 {
		t.Errorf("expected >=2 real mappings, got %d: %+v", real, p.Mapping)
	}
	if !hasBuildID {
		t.Errorf("expected at least one mapping with non-empty BuildID")
	}

	for _, loc := range p.Location {
		if loc.Mapping == nil {
			continue
		}
		m := loc.Mapping
		if m.File == "" || m.File == "[kernel]" || m.File == "[jit]" {
			continue
		}
		if loc.Address == 0 {
			t.Errorf("Location %d in %s has Address=0", loc.ID, m.File)
		}
	}
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

// TestPerfDwarfWalker drives the S3 DWARF-walker pipeline end-to-end: start
// the Rust cpu_bound workload, ehcompile its CFI, install it into the BPF
// maps, attach per-CPU perf events, and verify the ringbuf receives samples
// with DWARF-unwound chains.
func TestPerfDwarfWalker(t *testing.T) {
	// Unlike the other integration tests (which spawn perf-agent as a
	// subprocess and thus need the caller to be root), this test loads
	// BPF in-process, so setcap on the test binary is sufficient. Accept
	// either root or a process with CAP_BPF in its permitted set.
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, err := caps.GetFlag(cap.Permitted, cap.BPF)
		if err != nil || !have {
			t.Skip("requires root or CAP_BPF in permitted set")
		}
	}

	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			_ = workload.Process.Kill()
			_ = workload.Wait()
		}
	}()
	time.Sleep(2 * time.Second) // let workload start

	objs, err := perfprofile.LoadPerfDwarf(false)
	require.NoError(t, err)
	defer objs.Close()

	require.NoError(t, objs.AddPID(uint32(workload.Process.Pid)))

	// Compile CFI from the Rust binary.
	entries, classifications, err := ehcompile.Compile(binPath)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "ehcompile produced no CFI entries")

	buildID, err := ehmaps.ReadBuildID(binPath)
	require.NoError(t, err)
	tableID := ehmaps.TableIDForBuildID(buildID)

	// Install maps.
	require.NoError(t, ehmaps.PopulateCFI(ehmaps.PopulateCFIArgs{
		TableID: tableID, Entries: entries,
		OuterMap: objs.CFIRulesMap(), LengthMap: objs.CFILengthsMap(),
	}))
	require.NoError(t, ehmaps.PopulateClassification(ehmaps.PopulateClassificationArgs{
		TableID: tableID, Entries: classifications,
		OuterMap: objs.CFIClassificationMap(), LengthMap: objs.CFIClassificationLengthsMap(),
	}))

	mappings, err := ehmaps.LoadProcessMappings(workload.Process.Pid, binPath, tableID)
	require.NoError(t, err)
	require.NotEmpty(t, mappings, "no matching mappings in /proc/<pid>/maps")
	require.NoError(t, ehmaps.PopulatePIDMappings(ehmaps.PopulatePIDMappingsArgs{
		PID: uint32(workload.Process.Pid), Mappings: mappings,
		OuterMap: objs.PIDMappingsMap(), LengthMap: objs.PIDMappingLengthsMap(),
	}))

	// Per-CPU perf events at 99 Hz.
	ncpu := runtime.NumCPU()
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: 99,
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	var links []link.Link
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
	}()
	var fds []int
	defer func() {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}()
	// pid=-1, cpu=N samples all threads running on that CPU — the BPF-side
	// `pids` map (populated via objs.AddPID above) restricts emission to the
	// workload's TGID. Using pid=workloadPID here would sample ONLY that
	// specific TID, missing the worker threads where the actual CPU load runs.
	for cpu := range ncpu {
		fd, err := unix.PerfEventOpen(attr, -1, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			t.Fatalf("perf_event_open cpu=%d: %v", cpu, err)
		}
		fds = append(fds, fd)
		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target:  fd,
			Program: objs.Program(),
			Attach:  ebpf.AttachPerfEvent,
		})
		require.NoError(t, err)
		links = append(links, rl)
		require.NoError(t, unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0))
	}
	require.NotEmpty(t, fds, "no perf events attached — workload PID may have exited")

	// Consume ringbuf.
	rd, err := ringbuf.NewReader(objs.RingbufMap())
	require.NoError(t, err)
	defer rd.Close()

	deadline := time.Now().Add(5 * time.Second)
	var samples, dwarfSamples, maxFrames int
	flagCounts := map[byte]int{}
	var samplePrinted bool
	for samples < 40 && time.Now().Before(deadline) {
		rd.SetDeadline(time.Now().Add(500 * time.Millisecond))
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			break
		}
		samples++
		if len(rec.RawSample) < 32 {
			continue
		}
		nPCs := int(rec.RawSample[25])
		walkerFlags := rec.RawSample[26]
		flagCounts[walkerFlags]++
		if nPCs > maxFrames {
			maxFrames = nPCs
		}
		// bit 1 = WALKER_FLAG_DWARF_USED
		if walkerFlags&0x02 != 0 {
			dwarfSamples++
		}
		// Dump one sample's PC chain for diagnostics.
		if !samplePrinted && nPCs >= 1 {
			samplePrinted = true
			t.Logf("first sample: nPCs=%d walker_flags=%#x", nPCs, walkerFlags)
			for i := range nPCs {
				off := 32 + i*8
				if off+8 > len(rec.RawSample) {
					break
				}
				pc := binary.LittleEndian.Uint64(rec.RawSample[off : off+8])
				t.Logf("  [%d] %#016x", i, pc)
			}
		}
	}

	t.Logf("samples=%d dwarf_samples=%d max_frames=%d flag_counts=%v", samples, dwarfSamples, maxFrames, flagCounts)
	require.Greater(t, samples, 5, "no samples consumed — perf events may not have fired")
	require.Greater(t, maxFrames, 2, "chains too shallow — walker producing tiny stacks")
	require.Greater(t, dwarfSamples, 0, "DWARF path never fired — libstd/Rust frames should be FP-less in release")
}

// TestPerfAgentSystemWideDwarfProfile runs perf-agent with
// --profile --unwind dwarf -a (no --pid) against a running
// rust-workload. System-wide mode means samples can come from any
// process; we only assert non-empty samples + at least one symbolized
// function (the specific function depends on what was CPU-active
// during the 5s sampling window).
func TestPerfAgentSystemWideDwarfProfile(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "profile-dwarf-sys.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--profile",
		"--profile-output", outputFile,
		"--unwind", "dwarf",
		"-a",
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)
	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "system-wide profile should have samples")
	require.Greater(t, len(prof.Function), 0, "system-wide profile should have at least one symbolized function")

	// S9: verify pprof fidelity guarantees
	assertPprofFidelity(t, outputFile)
}

// TestPerfAgentSystemWideDwarfOffCPU runs perf-agent with --offcpu
// --unwind dwarf -a. System-wide means any blocking activity anywhere
// contributes samples — we just need non-zero blocking-ns total.
func TestPerfAgentSystemWideDwarfOffCPU(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "offcpu-dwarf-sys.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--offcpu",
		"--offcpu-output", outputFile,
		"--unwind", "dwarf",
		"-a",
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)
	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "system-wide off-CPU profile should have samples")

	var totalNs int64
	for _, s := range prof.Sample {
		for _, v := range s.Value {
			totalNs += v
		}
	}
	require.Greater(t, totalNs, int64(0), "system-wide off-CPU profile should have non-zero blocking-ns")
	t.Logf("system-wide off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}

// TestPerfDwarfMmap2Tracking validates the S4 flow: after starting the
// rust workload with --dlopen-delay, MmapWatcher + PIDTracker should
// pick up the probe.so mapping AUTOMATICALLY and install a second
// cfi_lengths entry (main binary + probe.so).
func TestPerfDwarfMmap2Tracking(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	binPath := "./workloads/rust/target/release/rust-workload"
	probePath := "./workloads/rust/probe/target/release/libprobe.so"
	for _, p := range []string{binPath, probePath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("workload %s not built: %v", p, err)
		}
	}

	// Start the workload with a 4s dlopen delay — gives us a wide window
	// to bring up the BPF maps, tracker, and watcher before the dlopen
	// MMAP2 fires.
	workload := exec.Command(binPath, "20", "2", "--dlopen", probePath, "--dlopen-delay", "4")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(500 * time.Millisecond) // let workload print its PID banner

	objs, err := perfprofile.LoadPerfDwarf(false)
	require.NoError(t, err)
	defer objs.Close()
	require.NoError(t, objs.AddPID(uint32(workload.Process.Pid)))

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap())
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())
	require.NoError(t, tracker.Attach(uint32(workload.Process.Pid), binPath))

	// Start the watcher BEFORE the dlopen fires. The 4s delay in the
	// workload above gives us time to get here.
	w, err := ehmaps.NewMmapWatcher(uint32(workload.Process.Pid))
	require.NoError(t, err)
	defer w.Close()

	runCtx, cancelRun := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() {
		tracker.Run(runCtx, w)
		close(runDone)
	}()

	// Wait for the dlopen + Attach to land. 6s covers the 4s pre-dlopen
	// delay plus a generous margin.
	deadline := time.After(6 * time.Second)
	var installed int
wait:
	for {
		installed = countMapEntries(t, objs.CFILengthsMap())
		if installed >= 2 {
			break wait
		}
		select {
		case <-deadline:
			break wait
		case <-time.After(200 * time.Millisecond):
		}
	}
	cancelRun()
	<-runDone

	if installed < 2 {
		t.Fatalf("expected >= 2 tables installed (main + probe.so), got %d", installed)
	}
}

// countMapEntries iterates a u64→u32 HASH map and returns the number of
// populated keys. Safe to call while other goroutines write (cilium/ebpf
// Iterate may skip or re-report keys under concurrent mutation — for
// this test we only need monotonic "at least 2").
func countMapEntries(t *testing.T, m *ebpf.Map) int {
	t.Helper()
	it := m.Iterate()
	var key uint64
	var val uint32
	n := 0
	for it.Next(&key, &val) {
		n++
	}
	if err := it.Err(); err != nil {
		t.Logf("iterate: %v (continuing)", err)
	}
	return n
}

// TestPerfAgentDwarfUnwind runs the full perf-agent binary end-to-end
// with --unwind dwarf against the rust workload, then parses the
// resulting pprof.pb.gz and asserts cpu_intensive_work shows up as
// a symbolized function name.
func TestPerfAgentDwarfUnwind(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "profile-dwarf.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--profile",
		"--profile-output", outputFile,
		"--unwind", "dwarf",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)

	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "profile should have samples")

	hit := false
	for _, fn := range prof.Function {
		if strings.Contains(fn.Name, "cpu_intensive_work") {
			hit = true
			break
		}
	}
	if !hit {
		names := make([]string, 0, min(10, len(prof.Function)))
		for i, fn := range prof.Function {
			if i >= 10 {
				break
			}
			names = append(names, fn.Name)
		}
		t.Fatalf("no function named *cpu_intensive_work* in pprof; first few: %v", names)
	}
}

// TestPerfAgentOffCPUDwarfUnwind runs the full perf-agent binary with
// --offcpu --unwind dwarf against the rust-workload and verifies the
// resulting off-CPU pprof has samples with non-zero blocking-ns.
//
// rust-workload (not go io_bound) because Go binaries have no .eh_frame
// — ehcompile needs .eh_frame to produce CFI. rust-workload is
// CPU-bound but its threads context-switch routinely, firing enough
// off-CPU samples to validate the pipeline end-to-end.
func TestPerfAgentOffCPUDwarfUnwind(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "offcpu-dwarf.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--offcpu",
		"--offcpu-output", outputFile,
		"--unwind", "dwarf",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)

	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "off-CPU profile should have samples")

	var totalNs int64
	for _, s := range prof.Sample {
		for _, v := range s.Value {
			totalNs += v
		}
	}
	require.Greater(t, totalNs, int64(0), "off-CPU profile should have non-zero blocking-ns values")
	t.Logf("off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}
