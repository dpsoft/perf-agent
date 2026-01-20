package test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

			// For Python, check if perf map file exists (requires Python 3.12+ with -X perf)
			if wl.Language == "python" {
				perfMapPath := fmt.Sprintf("/tmp/perf-%d.map", workload.Process.Pid)
				if _, err := os.Stat(perfMapPath); err == nil {
					t.Logf("✓ Python perf map found at %s", perfMapPath)
					// Read and check for user functions
					if data, err := os.ReadFile(perfMapPath); err == nil {
						lines := strings.Split(string(data), "\n")
						if len(lines) > 0 && lines[0] != "" {
							t.Logf("  Sample entry: %s", lines[0])
						}
						// Check for actual user functions after warmup
						mapContent := string(data)
						hasUserFunctions := strings.Contains(mapContent, "cpu_work") ||
							strings.Contains(mapContent, "io_work") ||
							strings.Contains(mapContent, "main")
						if hasUserFunctions {
							t.Logf("✓ User functions found in perf map after warmup")
						} else {
							t.Logf("⚠ Perf map exists but user functions not yet JIT-compiled")
						}
					}
				} else {
					t.Logf("⚠ WARNING: Python perf map not found at %s", perfMapPath)
					t.Logf("  Python version may not support -X perf (requires 3.12+)")
				}
			}

			// Run perf-agent
			outputFile := "profile.pb.gz"
			defer os.Remove(outputFile)

			agent := exec.Command(agentPath,
				"--profile",
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", "10s",
			)

			output, err := agent.CombinedOutput()
			t.Logf("perf-agent output:\n%s", string(output))
			require.NoError(t, err, "perf-agent should complete successfully")

			// Verify profile.pb.gz was created
			assert.FileExists(t, outputFile)

			// Parse and validate profile
			prof := parseProfile(t, outputFile)
			require.NotNil(t, prof)

			// Should have samples
			assert.Greater(t, len(prof.Sample), 0, "Profile should contain samples")

			// Should have valid sample types
			require.Greater(t, len(prof.SampleType), 0)
			// Sample type can be "sample" or "cpu" or "samples" depending on pprof version
			sampleType := prof.SampleType[0].Type
			t.Logf("Sample type: %s", sampleType)
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
			hasPythonSymbols := false
			for _, fn := range prof.Function {
				if fn.Name != "" && fn.Name != "??" {
					hasSymbols = true
					t.Logf("Found symbol: %s (file: %s)", fn.Name, fn.Filename)

					// For Python, check if we have actual Python function symbols
					if wl.Language == "python" {
						if fn.Filename != "" && strings.HasSuffix(fn.Filename, ".py") {
							hasPythonSymbols = true
							t.Logf("✓ Found Python-specific symbol: %s in %s", fn.Name, fn.Filename)
						} else if strings.Contains(fn.Name, "cpu_work") || strings.Contains(fn.Name, "py:") {
							hasPythonSymbols = true
							t.Logf("✓ Found Python function: %s", fn.Name)
						}
					}
					break
				}
			}
			assert.True(t, hasSymbols, "Profile should contain symbolized functions")

			// Additional validation for Python
			if wl.Language == "python" {
				if hasPythonSymbols {
					t.Logf("✓ Python symbolization working correctly")
				} else {
					t.Logf("⚠ No Python-specific symbols found (may need Python 3.12+ with -X perf)")
					t.Logf("  Profile contains system symbols only, which is expected without perf map support")
				}
			}
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
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", "10s",
			)

			output, err := agent.CombinedOutput()
			t.Logf("perf-agent output:\n%s", string(output))
			require.NoError(t, err)

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
	t.Logf("perf-agent output:\n%s", string(output))
	require.NoError(t, err)

	outputStr := string(output)

	// Verify PMU metrics are present
	assert.Contains(t, outputStr, "Metrics")
	assert.Contains(t, outputStr, "Samples:")
	assert.Contains(t, outputStr, "Scheduling Latency")

	// Check for percentile data
	assert.Contains(t, outputStr, "P50:")
	assert.Contains(t, outputStr, "P95:")
	assert.Contains(t, outputStr, "P99:")

	// Hardware counters (may not be available in VMs)
	if strings.Contains(outputStr, "Hardware Counters:") {
		if !strings.Contains(outputStr, "not available") {
			assert.Contains(t, outputStr, "Total Cycles:")
			assert.Contains(t, outputStr, "Total Instructions:")
			assert.Contains(t, outputStr, "IPC")
		}
	}
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
		"--offcpu",
		"--pmu",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "10s",
	)

	output, err := agent.CombinedOutput()
	t.Logf("perf-agent output:\n%s", string(output))
	require.NoError(t, err)

	// Verify both profile files exist
	defer os.Remove("profile.pb.gz")
	defer os.Remove("offcpu.pb.gz")

	assert.FileExists(t, "profile.pb.gz")
	assert.FileExists(t, "offcpu.pb.gz")

	// Verify PMU output
	outputStr := string(output)
	assert.Contains(t, outputStr, "Metrics")

	// Verify profiles are valid
	cpuProf := parseProfile(t, "profile.pb.gz")
	assert.Greater(t, len(cpuProf.Sample), 0)

	offcpuProf := parseProfile(t, "offcpu.pb.gz")
	// Off-CPU may have 0 samples for CPU-bound workload, that's OK
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
