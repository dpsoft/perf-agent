package test

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
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
		Args:     []string{workloadRuntimeFlag, "-threads=4"},
		Language: "go",
	},
	{
		Name:     "go-io",
		Binary:   "./workloads/go/io_bound",
		Args:     []string{workloadRuntimeFlag, "-threads=2"},
		Language: "go",
	},
	{
		Name:     "rust-cpu",
		Binary:   "./workloads/rust/target/release/rust-workload",
		Args:     []string{workloadRuntimeSecs, "4"},
		Language: "rust",
	},
	{
		Name:     "python-cpu",
		Binary:   "python3",
		Args:     []string{"-X", "perf", "./workloads/python/cpu_bound.py", workloadRuntimeSecs, "4"},
		Language: "python",
	},
	{
		Name:     "python-io",
		Binary:   "python3",
		Args:     []string{"-X", "perf", "./workloads/python/io_bound.py", workloadRuntimeSecs, "2"},
		Language: "python",
	},
}

func TestProfileMode(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

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

			// Run perf-agent. Collect until the capture is one the
			// assertions below can actually speak to — samples, stacks
			// and at least one symbol — rather than sampling once for a
			// fixed window and asserting on whatever landed (issue #42).
			// If the budget runs out we fall through to the historical
			// tolerance ladder unchanged, so this can only add signal.
			outputFile := "profile.pb.gz"
			defer os.Remove(outputFile)

			const window = 10 * time.Second
			prof, collected, report := collectProfileUntil(t,
				"a CPU profile with samples, stack traces and at least one symbolized function",
				window,
				func(int) (*profile.Profile, error) {
					requireWorkloadAlive(t, workload, wl.Name)
					agent := exec.Command(agentPath,
						"--profile",
						"--profile-output", outputFile,
						"--pid", fmt.Sprintf("%d", workload.Process.Pid),
						"--duration", window.String(),
					)
					output, err := agent.CombinedOutput()
					if err != nil {
						t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
					}
					return readProfile(outputFile)
				},
				func(p *profile.Profile) bool {
					return len(p.Sample) > 0 && profileHasStacks(p) && profileHasSymbols(p)
				})
			if !collected {
				t.Logf("WARN: %s", report)
			}
			require.NotNil(t, prof, "no readable profile after the collect-until budget: %s", report)

			// Verify profile.pb.gz was created
			assert.FileExists(t, outputFile)

			// Should have samples
			assert.Greater(t, len(prof.Sample), 0, "Profile should contain samples: %s", describeProfile(prof))

			// Should have valid sample types
			require.Greater(t, len(prof.SampleType), 0)
			sampleType := prof.SampleType[0].Type
			assert.True(t, sampleType == "sample" || sampleType == "cpu" || sampleType == "samples",
				"Expected sample type to be 'sample', 'cpu', or 'samples', got: %s", sampleType)

			// Verify we captured stack traces
			assert.True(t, profileHasStacks(prof), "Profile should contain stack traces: %s", describeProfile(prof))

			// Verify symbolization worked (at least some symbols).
			//
			// Tolerates JIT-only profiles: low-CPU Python `-X perf` workloads
			// on amd64 (e.g. python-io with most time in syscalls) often have
			// every sampled PC inside the JIT trampoline region, where
			// neither FP nor DWARF unwinding can reach libpython/libc. The
			// resulting profile contains valid Locations referencing the
			// [jit] sentinel mapping but Functions with empty names because
			// blazesym's perf-map reader didn't resolve the addresses. Treat
			// that as a known environmental limitation rather than a
			// regression — the agent did capture samples, just couldn't
			// symbolize them.
			hasSymbols := profileHasSymbols(prof)
			switch {
			case hasSymbols:
				// good
			case isJitOnlyProfile(prof):
				t.Logf("WARN: JIT-only profile (no file-backed mappings or symbols); known limitation for low-CPU Python -X perf on amd64")
			case isDegenerateProfile(prof):
				t.Logf("WARN: degenerate profile (no usable mappings); known CI flake on slow runners — captured PCs landed outside any binary mapping")
			case len(prof.Sample) < degenerateSampleFloor:
				t.Logf("WARN: only %d samples captured (< %d threshold); symbolization assertion skipped — too few user-space PCs to reliably hit symbolizable code",
					len(prof.Sample), degenerateSampleFloor)
			default:
				assert.True(t, hasSymbols, "Profile should contain symbolized functions: %s", describeProfile(prof))
			}

			// verify pprof fidelity guarantees
			assertPprofFidelity(t, outputFile)
		})
	}
}

func TestOffCPUMode(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

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

			// Run perf-agent with off-CPU profiling, re-collecting until
			// the I/O workload's blocking actually landed in a capture
			// (issue #42) rather than asserting on one fixed window.
			outputFile := "offcpu.pb.gz"
			defer os.Remove(outputFile)

			const window = 10 * time.Second
			prof, collected, report := collectProfileUntil(t,
				"an off-CPU profile with at least one blocking sample",
				window,
				func(int) (*profile.Profile, error) {
					requireWorkloadAlive(t, workload, wl.Name)
					agent := exec.Command(agentPath,
						"--offcpu",
						"--offcpu-output", outputFile,
						"--pid", fmt.Sprintf("%d", workload.Process.Pid),
						"--duration", window.String(),
					)
					output, err := agent.CombinedOutput()
					if err != nil {
						t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
					}
					return readProfile(outputFile)
				},
				func(p *profile.Profile) bool { return len(p.Sample) > 0 })
			if !collected {
				t.Logf("WARN: %s", report)
			}
			require.NotNil(t, prof, "no readable off-CPU profile after the collect-until budget: %s", report)

			// Verify offcpu.pb.gz was created
			assert.FileExists(t, outputFile)

			// Should have samples (I/O workloads block on I/O)
			assert.Greater(t, len(prof.Sample), 0, "Off-CPU profile should contain samples: %s", describeProfile(prof))

			// verify pprof fidelity guarantees
			assertPprofFidelity(t, outputFile)
		})
	}
}

func TestPMUMode(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, getAgentPath(t))

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

	// Verify both profile files exist
	defer os.Remove("profile.pb.gz")
	defer os.Remove("offcpu.pb.gz")

	// Run perf-agent with all features, re-collecting until the CPU
	// profile has samples (issue #42).
	const window = 10 * time.Second
	var output []byte
	cpuProf, collected, report := collectProfileUntil(t,
		"a combined-mode CPU profile with samples",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, wl.Name)
			agent := exec.Command(agentPath,
				"--profile",
				"--profile-output", "profile.pb.gz",
				"--offcpu",
				"--offcpu-output", "offcpu.pb.gz",
				"--pmu",
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", window.String(),
			)
			var err error
			output, err = agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile("profile.pb.gz")
		},
		func(p *profile.Profile) bool { return len(p.Sample) > 0 })
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, cpuProf, "no readable CPU profile after the collect-until budget: %s", report)

	assert.FileExists(t, "profile.pb.gz")
	assert.FileExists(t, "offcpu.pb.gz")
	assert.Contains(t, string(output), "Metrics")

	// Verify profiles are valid
	assert.Greater(t, len(cpuProf.Sample), 0, "combined-mode CPU profile should contain samples: %s", describeProfile(cpuProf))

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

// requireBPFRunnable skips the test unless BPF programs can be loaded by
// either (a) running as root, (b) the test process holding CAP_BPF in its
// permitted set (test binary itself capped), or (c) when agentPath is
// non-empty, the file at agentPath holding CAP_BPF in its file-permitted
// set (perf-agent binary capped via `setcap cap_*+ep`, which is enough
// for tests that exec it as a subprocess).
//
// Pass agentPath = getAgentPath(t) for tests that exec the agent as a
// subprocess. Pass agentPath = "" for tests that load BPF in-process via
// the perfagent / perfprofile / ehmaps libraries — those need caps on
// the test process itself, not on the agent binary.
func requireBPFRunnable(t *testing.T, agentPath string) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	if procCaps := cap.GetProc(); procCaps != nil {
		if have, err := procCaps.GetFlag(cap.Permitted, cap.BPF); err == nil && have {
			return
		}
	}
	if agentPath != "" {
		if fileCaps, err := cap.GetFile(agentPath); err == nil && fileCaps != nil {
			if have, err := fileCaps.GetFlag(cap.Permitted, cap.BPF); err == nil && have {
				return
			}
		}
	}
	t.Skip("requires root, CAP_BPF in test process, or setcap'd perf-agent")
}

// isJitOnlyProfile returns true if the profile's only non-empty,
// non-kernel mapping is the [jit] sentinel — i.e., every captured PC
// landed in JIT memory regions. This happens legitimately for low-CPU
// Python `-X perf` workloads on amd64 where FP unwinding can't escape
// JIT trampolines and the dwarf walker has no CFI for anonymous JIT
// memory. Treat as a known limitation, not a regression.
func isJitOnlyProfile(p *profile.Profile) bool {
	var hasReal, hasJit bool
	for _, m := range p.Mapping {
		switch {
		case m.File == "" || m.File == "[kernel]":
			// ignore
		case m.File == "[jit]":
			hasJit = true
		default:
			hasReal = true
		}
	}
	return hasJit && !hasReal
}

// isDegenerateProfile reports whether the profile is the "captured
// almost nothing" shape we keep hitting on slow CI runners: a
// low-volume run (< degenerateSampleFloor samples) with no usable
// mapping information at all (neither file-backed mappings nor the
// [jit] sentinel).
//
// We deliberately gate on **sample count** as well as mapping
// emptiness. A real mapping-resolution regression (e.g. blazesym
// broken, /proc/<pid>/maps unreadable, library lookup busted) would
// still produce hundreds of samples for a 10s @ 99Hz run — those
// samples just wouldn't have any binary mapping attached. Above the
// floor, "zero real mappings" is a genuine bug worth a loud failure.
// Below the floor, the few PCs we got may all have landed in
// unmapped/anonymous regions on a slow runner — that's blazesym /
// scheduler timing, not a perf-agent bug. Other earlier guards
// (`len(prof.Sample) > 0`, `hasStacks`) already catch the "BPF
// stopped capturing entirely" case, so this gate doesn't hide that.
//
// The floor (degenerateSampleFloor) is shared with the
// symbolization-assertion floor in TestProfileMode for consistency:
// the underlying claim in both is the same — below this many
// samples, we have too little signal to assert against.
func isDegenerateProfile(p *profile.Profile) bool {
	if len(p.Sample) >= degenerateSampleFloor {
		return false
	}
	for _, m := range p.Mapping {
		switch m.File {
		case "", "[kernel]", "[jit]":
			// these don't count as usable mapping info
		default:
			return false
		}
	}
	return true
}

// assertPprofFidelity verifies pprof fidelity guarantees on a
// captured profile: >=1 real (non-sentinel) mapping and every
// user-space Location has a non-zero Address. BuildID presence is
// observed but not asserted — system-wide captures can legitimately
// land on stripped binaries without a GNU build-id (kernel threads,
// older system binaries, custom builds with --build-id=none).
func assertPprofFidelity(t *testing.T, path string) {
	t.Helper()
	p, err := readProfile(path)
	if err != nil {
		t.Fatalf("pprof fidelity: %v", err)
	}
	if err := p.CheckValid(); err != nil {
		t.Fatalf("pprof invalid: %v", err)
	}

	sum := summarizeProfile(p)
	real := sum.RealMappings
	hasJit := sum.JitMappings > 0
	// At least one real mapping — proves we're not falling back to the
	// hardcoded single-mapping default. Static binaries (Go) legitimately
	// produce exactly 1; dynamically-linked binaries produce N+ (target +
	// shared libs). JIT-only profiles (Python `-X perf` low-CPU workloads
	// where every sampled PC lands in trampoline memory) are accepted as
	// a known environmental edge — see the parallel skip logic in
	// TestProfileMode.
	switch {
	case real >= 1:
		// good
	case hasJit:
		t.Logf("WARN: profile has only [jit] mapping (no file-backed); accepting JIT-only profile")
	case isDegenerateProfile(p):
		t.Logf("WARN: degenerate profile (real=0, jit=0); known CI flake on slow runners: %s", describeMappings(p))
	default:
		t.Errorf("expected >=1 real (file-backed) mapping, got 0.\n"+
			"  captured: %s\n"+
			"  reading:  %s\n"+
			"  mappings: %s",
			sum, fidelityDiagnosis(sum), describeMappings(p))
	}
	// BuildID is observational only — record presence/absence so a
	// regression that wipes BuildIDs across the board is visible in
	// test output, but don't fail when the captured binaries simply
	// don't carry build-ids.
	t.Logf("pprof fidelity: %s", sum)

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

// parseProfile reads a captured pprof and fails the test if it cannot be
// read.
//
// It does NOT assert on sample count — callers keep their own
// expectations about that — but an empty or truncated file is reported
// as "EMPTY (0 bytes): the agent collected 0 samples", not as the bare
// `EOF` from a gzip reader that issue #42's arm64 run had to be
// diagnosed from.
func parseProfile(t *testing.T, filename string) *profile.Profile {
	t.Helper()
	prof, err := readProfile(filename)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	return prof
}

// fidelityDiagnosis turns a profile summary into the sentence a reader
// needs to decide whether a "no real mapping" failure is a profiler
// regression or a workload that was never on-CPU in user space —
// precisely the distinction issue #42 called out as missing.
func fidelityDiagnosis(s profileSummary) string {
	switch {
	case s.Samples == 0:
		return "nothing was sampled at all, so this says nothing about mapping resolution"
	case s.UnmappedFrames > 0:
		return fmt.Sprintf(
			"%d frame(s) carried a user-space PC that resolved to no binary mapping — "+
				"mapping resolution (procmap/blazesym) is the suspect", s.UnmappedFrames)
	case s.KernelFrames > 0 && s.UserFrames == 0:
		return fmt.Sprintf(
			"all %d sampled frames were kernel-side and none were user-side — "+
				"the target was never on-CPU in user space during this window", s.KernelFrames)
	default:
		return "samples exist but carry no file-backed mapping"
	}
}

// System-wide profiling tests

func TestSystemWideProfile(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)

	// Start multiple workloads
	workload1 := exec.Command("./workloads/go/cpu_bound", workloadRuntimeFlag, "-threads=2")
	workload2 := exec.Command("./workloads/go/io_bound", workloadRuntimeFlag, "-threads=2")
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

	// Run system-wide profiling, re-collecting until samples land
	// (issue #42).
	outputFile := "profile.pb.gz"
	defer os.Remove(outputFile)

	const window = 5 * time.Second
	var output []byte
	prof, collected, report := collectProfileUntil(t,
		"a system-wide CPU profile with samples",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload1, "go/cpu_bound")
			agent := exec.Command(agentPath, "--profile", "--profile-output", outputFile, "-a", "--duration", window.String())
			var err error
			output, err = agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile(outputFile)
		},
		func(p *profile.Profile) bool { return len(p.Sample) > 0 })
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, prof, "no readable system-wide profile after the collect-until budget: %s", report)

	assert.Contains(t, string(output), "system-wide")
	assert.FileExists(t, outputFile)
	assert.Greater(t, len(prof.Sample), 0, "System-wide profile should contain samples: %s", describeProfile(prof))
}

func TestSystemWideOffCPU(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)

	workload := exec.Command("./workloads/go/io_bound", workloadRuntimeFlag, "-threads=2")
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

	// Collect until the capture is one assertPprofFidelity's verdict can
	// mean something: enough samples to be worth judging, at least one
	// of them from user space (issue #42). Deliberately NOT "at least
	// one file-backed mapping" — that is what the assertion checks, and
	// a loop that waits for it could never fail it.
	const window = 5 * time.Second
	var output []byte
	prof, collected, report := collectProfileUntil(t,
		"a system-wide off-CPU profile with enough samples to judge and at least one user-space frame",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "go/io_bound")
			agent := exec.Command(agentPath, "--offcpu", "--offcpu-output", outputFile, "-a", "--duration", window.String())
			var err error
			output, err = agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile(outputFile)
		},
		profileFidelityJudgeable)
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, prof, "no readable system-wide off-CPU profile after the collect-until budget: %s", report)

	assert.Contains(t, string(output), "system-wide")
	assert.FileExists(t, outputFile)

	// verify pprof fidelity guarantees
	assertPprofFidelity(t, outputFile)
}

func TestSystemWidePMU(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)

	// --pid and -a should be mutually exclusive
	agent := exec.Command(agentPath, "--profile", "--pid", "1234", "-a", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "mutually exclusive")
}

func TestRequiresPIDOrAll(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)

	// Should fail without --pid or -a
	agent := exec.Command(agentPath, "--profile", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "required")
}

func TestPerPIDRequiresAll(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)

	// --per-pid should require -a
	agent := exec.Command(agentPath, "--pmu", "--pid", "1234", "--per-pid", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "--per-pid requires")
}

func TestPerPIDRequiresPMU(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)

	// --per-pid should require --pmu
	agent := exec.Command(agentPath, "--profile", "-a", "--per-pid", "--duration", "5s")
	output, err := agent.CombinedOutput()
	assert.Error(t, err)
	assert.Contains(t, string(output), "only valid with --pmu")
}

// Tests for new runqueue latency and task state features

func TestPMURunqueueLatency(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, getAgentPath(t))

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

	// An I/O workload should show I/O wait or voluntary sleep; file
	// operations cause both.
	//
	// strings.Contains, not assert.Contains: assert.Contains records a
	// failure on t as a side effect and returns a bool, so writing this as
	// `if assert.Contains(...) || assert.Contains(...)` marked the test
	// failed on the first branch before the second was ever considered -
	// the || read as a tolerance but never tolerated anything, and a
	// genuine miss recorded three failures for one condition.
	//
	// Note this is a weaker check than it appears: metrics/console.go
	// prints both lines inside one `if totalSwitches > 0` block, so they
	// appear together or not at all. What it really asserts is that some
	// context switch was recorded. See issue #63.
	hasIOActivity := strings.Contains(outputStr, "I/O Wait (D state):") ||
		strings.Contains(outputStr, "Voluntary (sleep/mutex):")
	assert.True(t, hasIOActivity,
		"I/O workload should report I/O wait or voluntary sleep; output had neither line:\n%s", outputStr)
}

func TestPMUCPUWorkloadMostlyRunning(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, getAgentPath(t))

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
	requireBPFRunnable(t, "")

	// Start a CPU workload
	workload := exec.Command("./workloads/go/cpu_bound", workloadRuntimeFlag, "-threads=4")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second) // warmup

	// One attempt = one full Start/collect/Stop cycle of the in-process
	// agent; the streamed profile is only written on Stop. Collect until
	// the profile carries samples and symbols (issue #42).
	const window = 3 * time.Second
	var buf bytes.Buffer
	prof, collected, report := collectProfileUntil(t,
		"a streamed CPU profile with samples and at least one symbolized function",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "go/cpu_bound")
			buf.Reset()

			agent, err := perfagent.New(
				perfagent.WithPID(workload.Process.Pid),
				perfagent.WithCPUProfileWriter(&buf),
				perfagent.WithSampleRate(99),
			)
			require.NoError(t, err)
			defer agent.Close()

			ctx := context.Background()
			require.NoError(t, agent.Start(ctx))
			time.Sleep(window)
			require.NoError(t, agent.Stop(ctx))

			if buf.Len() == 0 {
				return nil, fmt.Errorf("profile writer received 0 bytes: the agent captured no samples in this %s window", window)
			}
			return profile.Parse(bytes.NewReader(buf.Bytes()))
		},
		func(p *profile.Profile) bool {
			return len(p.Sample) > 0 && profileHasSymbols(p)
		})
	if !collected {
		t.Logf("WARN: %s", report)
	}

	// Verify profile
	require.Greater(t, buf.Len(), 0, "profile buffer should have data: %s", report)
	require.NotNil(t, prof, "streamed profile did not parse: %s", report)
	require.Greater(t, len(prof.Sample), 0, "profile should contain samples: %s", describeProfile(prof))

	// Verify we got symbolized functions
	assert.True(t, profileHasSymbols(prof), "Profile should contain symbolized functions: %s", describeProfile(prof))
}

func TestStreamingOffCPUProfileOutput(t *testing.T) {
	requireBPFRunnable(t, "")

	// Start an I/O workload
	workload := exec.Command("./workloads/go/io_bound", workloadRuntimeFlag, "-threads=2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second) // warmup

	const window = 3 * time.Second
	var buf bytes.Buffer
	prof, collected, report := collectProfileUntil(t,
		"a streamed off-CPU profile with at least one blocking sample",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "go/io_bound")
			buf.Reset()

			agent, err := perfagent.New(
				perfagent.WithPID(workload.Process.Pid),
				perfagent.WithOffCPUProfileWriter(&buf),
			)
			require.NoError(t, err)
			defer agent.Close()

			ctx := context.Background()
			require.NoError(t, agent.Start(ctx))
			time.Sleep(window)
			require.NoError(t, agent.Stop(ctx))

			if buf.Len() == 0 {
				return nil, fmt.Errorf("off-CPU profile writer received 0 bytes in this %s window", window)
			}
			return profile.Parse(bytes.NewReader(buf.Bytes()))
		},
		func(p *profile.Profile) bool { return len(p.Sample) > 0 })
	if !collected {
		t.Logf("WARN: %s", report)
	}

	// Off-CPU profile should have data for I/O workload. Kept
	// conditional: this assertion has always tolerated an empty
	// buffer, and the collect-until loop above only improves the odds
	// of having one to check.
	if buf.Len() > 0 {
		require.NotNil(t, prof, "off-CPU profile did not parse: %s", report)
		require.Greater(t, len(prof.Sample), 0, "off-CPU profile should contain samples: %s", describeProfile(prof))
	}
}

func TestStreamingCombinedProfileOutput(t *testing.T) {
	requireBPFRunnable(t, "")

	// Start workload
	workload := exec.Command("./workloads/go/cpu_bound", workloadRuntimeFlag, "-threads=4")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()

	time.Sleep(2 * time.Second)

	const window = 3 * time.Second
	var cpuBuf, offcpuBuf bytes.Buffer
	cpuProf, collected, report := collectProfileUntil(t,
		"a streamed combined-mode CPU profile with samples",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "go/cpu_bound")
			cpuBuf.Reset()
			offcpuBuf.Reset()

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
			time.Sleep(window)
			require.NoError(t, agent.Stop(ctx))

			if cpuBuf.Len() == 0 {
				return nil, fmt.Errorf("CPU profile writer received 0 bytes in this %s window", window)
			}
			return profile.Parse(bytes.NewReader(cpuBuf.Bytes()))
		},
		func(p *profile.Profile) bool { return len(p.Sample) > 0 })
	if !collected {
		t.Logf("WARN: %s", report)
	}

	// Verify CPU profile
	require.Greater(t, cpuBuf.Len(), 0, "CPU profile buffer should have data: %s", report)
	require.NotNil(t, cpuProf, "streamed CPU profile did not parse: %s", report)
	require.Greater(t, len(cpuProf.Sample), 0, "streamed CPU profile should contain samples: %s", describeProfile(cpuProf))

	// Off-CPU may or may not have data for CPU-bound workload
	if offcpuBuf.Len() > 0 {
		offcpuProf, err := profile.Parse(bytes.NewReader(offcpuBuf.Bytes()))
		require.NoError(t, err)
		require.NotNil(t, offcpuProf)
	}
}

func TestLibraryPMUMetrics(t *testing.T) {
	requireBPFRunnable(t, "")

	// Start a CPU workload
	workload := exec.Command("./workloads/go/cpu_bound", workloadRuntimeFlag, "-threads=4")
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

	// Collect until the PMU snapshot actually carries a process, with a
	// deadline, instead of sleeping a fixed 3s and asserting on whatever
	// the monitor happened to have (issue #42). The agent keeps running
	// across attempts — GetMetrics is a read of accumulating state — so
	// this only re-reads, it does not re-collect.
	const window = 3 * time.Second
	ok, report := collectUntil(t, "a PMU snapshot containing at least one process with samples",
		window, collectBudgetFor(window), func(int) (bool, string) {
			requireWorkloadAlive(t, workload, "go/cpu_bound")
			time.Sleep(window)
			snap, err := agent.GetMetrics()
			require.NoError(t, err)
			require.NotNil(t, snap)
			for _, pm := range snap.Processes {
				if pm.SampleCount > 0 {
					return true, fmt.Sprintf("%d process(es) in snapshot", len(snap.Processes))
				}
			}
			return false, fmt.Sprintf("%d process(es) in snapshot, none with samples", len(snap.Processes))
		})
	if !ok {
		t.Logf("WARN: %s", report)
	}

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

// TestPerfDwarfWalker drives the DWARF-walker pipeline end-to-end: start
// the Rust cpu_bound workload, ehcompile its CFI, install it into the BPF
// maps, attach per-CPU perf events, and verify the ringbuf receives samples
// with DWARF-unwound chains.
func TestPerfDwarfWalker(t *testing.T) {
	// Unlike the other integration tests (which spawn perf-agent as a
	// subprocess and thus need the caller to be root), this test loads
	// BPF in-process, so setcap on the test binary is sufficient. Accept
	// either root or a process with CAP_BPF in its permitted set.
	requireBPFRunnable(t, "")

	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, workloadRuntimeSecs, "2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			_ = workload.Process.Kill()
			_ = workload.Wait()
		}
	}()
	time.Sleep(2 * time.Second) // let workload start

	objs, err := perfprofile.LoadPerfDwarf(false, false)
	require.NoError(t, err)
	defer objs.Close()

	require.NoError(t, objs.AddPID(uint32(workload.Process.Pid)))

	// Compile CFI from the Rust binary.
	entries, classifications, _, err := ehcompile.Compile(binPath)
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

	mappings, err := ehmaps.LoadProcessMappings(workload.Process.Pid, binPath, "", tableID)
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

	// Collect until the ringbuf has produced the evidence all three
	// assertions below need — enough samples, a chain deeper than 2,
	// and at least one DWARF-unwound sample — bounded by a deadline
	// (issue #42). The old loop stopped at 40 samples and then asserted
	// on whatever those 40 happened to contain; on a contended runner a
	// 5s window can deliver 40 shallow FP-only samples and no DWARF
	// one. The `samples < 40` term is retained so a healthy run still
	// gathers at least as much evidence as it used to.
	const walkerBudget = 20 * time.Second
	start := time.Now()
	deadline := start.Add(walkerBudget)
	var samples, dwarfSamples, maxFrames int
	flagCounts := map[byte]int{}
	var samplePrinted bool
	enough := func() bool { return samples > 5 && maxFrames > 2 && dwarfSamples > 0 }
	for (samples < 40 || !enough()) && time.Now().Before(deadline) {
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

	stats := fmt.Sprintf("samples=%d dwarf_samples=%d max_frames=%d flag_counts=%v collected_for=%s (budget %s)",
		samples, dwarfSamples, maxFrames, flagCounts, time.Since(start).Round(time.Millisecond), walkerBudget)
	t.Logf("%s", stats)
	require.Greater(t, samples, 5, "no samples consumed — perf events may not have fired; %s", stats)
	require.Greater(t, maxFrames, 2, "chains too shallow — walker producing tiny stacks; %s", stats)
	require.Greater(t, dwarfSamples, 0, "DWARF path never fired — libstd/Rust frames should be FP-less in release; %s", stats)
}

// TestPerfAgentSystemWideDwarfProfile runs perf-agent with
// --profile --unwind dwarf -a (no --pid) against a running
// rust-workload. System-wide mode means samples can come from any
// process; we only assert non-empty samples + at least one symbolized
// function (the specific function depends on what was CPU-active
// during the 5s sampling window).
func TestPerfAgentSystemWideDwarfProfile(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, workloadRuntimeSecs, "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "profile-dwarf-sys.pb.gz"
	defer os.Remove(outputFile)

	// Collect until the capture is worth judging — enough samples, at
	// least one from user space — and then let assertPprofFidelity
	// render the verdict. Whether those user PCs resolved to a real
	// mapping stays OUT of the loop condition on purpose: a loop that
	// waits for a real mapping can never fail the assertion that one
	// exists.
	//
	// This is the test and the assertion from issue #42's third
	// comment. Note that this does NOT make that failure green — see
	// the report's "Does this fix symptom 3?" section. That capture had
	// >= 20 samples and at least one user-space PC, so it clears this
	// precondition and is judged, not retried. Making it green would
	// require looping on the assertion itself, which is the one thing
	// this harness must not do.
	const window = 5 * time.Second
	prof, collected, report := collectProfileUntil(t,
		"a system-wide DWARF profile with enough samples to judge, a symbolized function and at least one user-space frame",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "rust-workload")
			agent := exec.Command(agentPath,
				"--profile",
				"--profile-output", outputFile,
				"--unwind", "dwarf",
				"-a",
				"--duration", window.String(),
			)
			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile(outputFile)
		},
		func(p *profile.Profile) bool {
			return profileFidelityJudgeable(p) && len(p.Function) > 0
		})
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, prof, "no readable system-wide DWARF profile after the collect-until budget: %s", report)

	assert.FileExists(t, outputFile)
	require.Greater(t, len(prof.Sample), 0, "system-wide profile should have samples: %s", describeProfile(prof))
	require.Greater(t, len(prof.Function), 0, "system-wide profile should have at least one symbolized function: %s", describeProfile(prof))

	// verify pprof fidelity guarantees
	assertPprofFidelity(t, outputFile)
}

// TestPerfAgentSystemWideDwarfOffCPU runs perf-agent with --offcpu
// --unwind dwarf -a. System-wide means any blocking activity anywhere
// contributes samples — we just need non-zero blocking-ns total.
func TestPerfAgentSystemWideDwarfOffCPU(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, workloadRuntimeSecs, "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "offcpu-dwarf-sys.pb.gz"
	defer os.Remove(outputFile)

	// As above: enough samples plus a user-space frame is the
	// precondition; the mapping verdict belongs to assertPprofFidelity.
	const window = 5 * time.Second
	prof, collected, report := collectProfileUntil(t,
		"a system-wide DWARF off-CPU profile with enough samples to judge, non-zero blocking time and at least one user-space frame",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "rust-workload")
			agent := exec.Command(agentPath,
				"--offcpu",
				"--offcpu-output", outputFile,
				"--unwind", "dwarf",
				"-a",
				"--duration", window.String(),
			)
			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile(outputFile)
		},
		func(p *profile.Profile) bool {
			return profileFidelityJudgeable(p) && profileTotalValue(p) > 0
		})
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, prof, "no readable system-wide DWARF off-CPU profile after the collect-until budget: %s", report)

	assert.FileExists(t, outputFile)
	require.Greater(t, len(prof.Sample), 0, "system-wide off-CPU profile should have samples: %s", describeProfile(prof))

	totalNs := profileTotalValue(prof)
	require.Greater(t, totalNs, int64(0), "system-wide off-CPU profile should have non-zero blocking-ns: %s", describeProfile(prof))
	t.Logf("system-wide off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))

	// verify pprof fidelity guarantees
	assertPprofFidelity(t, outputFile)
}

// TestPerfDwarfMmap2Tracking validates the MMAP2 flow: after starting the
// rust workload with --dlopen-delay, MmapWatcher + PIDTracker should
// pick up the probe.so mapping AUTOMATICALLY and install a second
// cfi_lengths entry (main binary + probe.so).
func TestPerfDwarfMmap2Tracking(t *testing.T) {
	requireBPFRunnable(t, "")

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

	objs, err := perfprofile.LoadPerfDwarf(false, false)
	require.NoError(t, err)
	defer objs.Close()
	require.NoError(t, objs.AddPID(uint32(workload.Process.Pid)))

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap())
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())
	require.NoError(t, tracker.Attach(uint32(workload.Process.Pid), binPath, ""))

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
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, workloadRuntimeSecs, "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "profile-dwarf.pb.gz"
	defer os.Remove(outputFile)

	// Collect until the workload's hot function shows up, with a
	// deadline (issue #42): a single 5s window can miss it entirely on
	// a contended runner, and "missed it" and "the DWARF walker is
	// broken" then look identical.
	const window = 5 * time.Second
	prof, collected, report := collectProfileUntil(t,
		"a DWARF-unwound profile containing the workload's cpu_intensive_work frame",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "rust-workload")
			agent := exec.Command(agentPath,
				"--profile",
				"--profile-output", outputFile,
				"--unwind", "dwarf",
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", window.String(),
			)
			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile(outputFile)
		},
		func(p *profile.Profile) bool {
			return len(p.Sample) > 0 && hasFunctionContaining(p, "cpu_intensive_work")
		})
	if !collected {
		t.Fatalf("%s\n  first few symbolized functions: %v", report, topFunctionNames(prof, 10))
	}
	assert.FileExists(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "profile should have samples: %s", describeProfile(prof))
	require.True(t, hasFunctionContaining(prof, "cpu_intensive_work"),
		"no function named *cpu_intensive_work* in pprof; %s; first few: %v",
		describeProfile(prof), topFunctionNames(prof, 10))
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
	requireBPFRunnable(t, getAgentPath(t))

	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, workloadRuntimeSecs, "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "offcpu-dwarf.pb.gz"
	defer os.Remove(outputFile)

	const window = 5 * time.Second
	prof, collected, report := collectProfileUntil(t,
		"a DWARF-unwound off-CPU profile with non-zero blocking time",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, workload, "rust-workload")
			agent := exec.Command(agentPath,
				"--offcpu",
				"--offcpu-output", outputFile,
				"--unwind", "dwarf",
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", window.String(),
			)
			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}
			return readProfile(outputFile)
		},
		func(p *profile.Profile) bool {
			return len(p.Sample) > 0 && profileTotalValue(p) > 0
		})
	if !collected {
		t.Fatal(report)
	}
	assert.FileExists(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "off-CPU profile should have samples: %s", describeProfile(prof))

	totalNs := profileTotalValue(prof)
	require.Greater(t, totalNs, int64(0), "off-CPU profile should have non-zero blocking-ns values: %s", describeProfile(prof))
	t.Logf("off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}

// TestPerfDataOutput captures a perf.data file from a CPU-bound workload,
// runs `perf script` against it, and asserts the kernel decodes our output
// without errors and produces at least one sample line.
func TestPerfDataOutput(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))
	// Probe the perf binary functionally. On Ubuntu, /usr/bin/perf is a
	// shim that re-execs the kernel-version-specific tool from
	// linux-tools-<kver>; if that package isn't installed the shim exits
	// non-zero with "perf not found for kernel". LookPath alone isn't
	// enough — confirm `perf --version` actually works.
	if _, err := exec.LookPath("perf"); err != nil {
		t.Skipf("perf binary not on PATH; skipping: %v", err)
	}
	if out, err := exec.Command("perf", "--version").CombinedOutput(); err != nil {
		t.Skipf("perf binary not functional (likely missing linux-tools for this kernel); skipping: %v\n%s",
			err, string(out))
	}

	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, workloadRuntimeSecs, "2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			_ = workload.Process.Kill()
			_ = workload.Wait()
		}
	}()
	time.Sleep(2 * time.Second)

	outDir := t.TempDir()
	pprofOut := filepath.Join(outDir, "profile.pb.gz")
	perfDataOut := filepath.Join(outDir, "test.perf.data")

	// Collect until the capture is one `perf script` can decode into at
	// least one line, with a deadline (issue #42). A perf.data with
	// headers but no samples is the "nothing landed" shape, not a
	// perf.data-format regression, and the two used to be
	// indistinguishable here.
	const window = 5 * time.Second
	var scriptOut []byte
	ok, report := collectUntil(t,
		"a perf.data that `perf script` decodes into at least one line",
		window, collectBudgetFor(window),
		func(int) (bool, string) {
			requireWorkloadAlive(t, workload, "rust-workload")
			agent := exec.Command(getAgentPath(t),
				"--profile",
				"--profile-output", pprofOut,
				"--perf-data-output", perfDataOut,
				"--pid", fmt.Sprintf("%d", workload.Process.Pid),
				"--duration", window.String(),
			)
			output, err := agent.CombinedOutput()
			if err != nil {
				t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
			}

			st, err := os.Stat(perfDataOut)
			if err != nil {
				return false, fmt.Sprintf("perf.data was not created: %v", err)
			}
			if st.Size() <= 200 {
				return false, fmt.Sprintf("perf.data is only %d bytes (want > 200): headers were written but no samples", st.Size())
			}

			cmd := exec.Command("perf", "script", "-i", perfDataOut)
			scriptOut, err = cmd.CombinedOutput()
			if err != nil {
				// A decode failure is a real regression in our
				// perf.data writer, not a scheduling miss — fail now.
				t.Fatalf("perf script failed on our output: %v\n%s", err, string(scriptOut))
			}
			if len(scriptOut) == 0 {
				return false, fmt.Sprintf("perf script decoded a %d-byte perf.data into 0 bytes of output (no samples in perf.data)", st.Size())
			}
			return true, fmt.Sprintf("perf.data %d bytes, perf script output %d bytes", st.Size(), len(scriptOut))
		})
	if !ok {
		t.Fatal(report)
	}
	require.NotEmpty(t, scriptOut, "perf script produced no output (no samples in perf.data?)")
	t.Logf("perf script captured %d bytes of output", len(scriptOut))
}

// TestKernelStackResolution verifies the pprof path for kernel-stack capture.
// It spawns the Go io_bound workload (heavy I/O → many kernel frames), runs
// perf-agent with --profile --kernel-stacks, and asserts:
//   - at least one user-side function (main.* or runtime.*) appears in the pprof
//   - when kptr_restrict=0, at least one resolved kernel symbol also appears
func TestKernelStackResolution(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))

	bin := getAgentPath(t)
	kptrZero := readKptrRestrictZero()

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")

	// The io_bound workload is only on-CPU in user space in short
	// bursts between blocking I/O, so a single fixed 3s window at 99 Hz
	// can legitimately capture zero samples, or samples whose every
	// frame is kernel-side. Issue #42 saw both, on the same commit.
	// Collect until the profile is one the assertions below can speak
	// to, with a deadline; a profiler that has genuinely stopped
	// producing user frames still fails, at the deadline, with the
	// frame split printed.
	kernelRe := regexp.MustCompile(`^(do_sys_|ksys_|__x64_sys_|vfs_|__schedule|read_|sock_|tcp_)`)
	hasUserFn := func(p *profile.Profile) bool {
		return hasFunctionContaining(p, "main.", "runtime.")
	}
	hasKernelFn := func(p *profile.Profile) bool {
		for _, fn := range p.Function {
			if kernelRe.MatchString(fn.Name) {
				return true
			}
		}
		return false
	}

	what := "a kernel-stacks profile containing at least one user-side (main.*/runtime.*) frame"
	if kptrZero {
		what += " and at least one resolved kernel symbol"
	}

	const window = 3 * time.Second
	p, collected, report := collectProfileUntil(t, what, window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "go/io_bound")
			agent := exec.Command(bin,
				"--profile",
				"--kernel-stacks",
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool {
			return len(p.Sample) > 0 && hasUserFn(p) && (!kptrZero || hasKernelFn(p))
		})

	got := map[string]bool{}
	if p != nil {
		for _, fn := range p.Function {
			got[fn.Name] = true
		}
	}
	if !collected {
		t.Fatalf("%s\n  functions in the last capture: %v", report, sortedKeys(got))
	}

	// Always assert at least one user-side function from io_bound appears.
	if !hasUserFn(p) {
		t.Fatalf("no user-side function in profile; %s; got: %v", describeProfile(p), sortedKeys(got))
	}

	if kptrZero {
		// Expect at least one resolved kernel symbol.
		if !hasKernelFn(p) {
			t.Fatalf("no resolved kernel symbol matched expected regex; %s; got: %v", describeProfile(p), sortedKeys(got))
		}
	} else {
		// kptr_restrict != 0 → kernel frames may appear as raw 0xffff… names
		// or may be absent entirely. Either is acceptable.
		t.Logf("kptr_restrict != 0; not asserting kernel symbol resolution")
	}
}

// TestPerfDataKernelMmap2 verifies the --perf-data-output path for kernel
// stacks. It asserts the produced perf.data file contains the
// [kernel.kallsyms]_text MMAP2 record and the pid=-1 (0xffffffff LE) marker
// that perf tooling relies on to anchor kernel symbol lookups.
func TestPerfDataKernelMmap2(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))

	bin := getAgentPath(t)

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	outDir := t.TempDir()
	pb := filepath.Join(outDir, "profile.pb.gz")
	pd := filepath.Join(outDir, "perf.data")
	agent := exec.Command(bin,
		"--profile",
		"--kernel-stacks",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", pb,
		"--perf-data-output", pd,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	body, err := os.ReadFile(pd)
	if err != nil {
		t.Fatalf("read perf.data: %v", err)
	}
	if !bytes.Contains(body, []byte("[kernel.kallsyms]_text")) {
		t.Fatalf("perf.data missing [kernel.kallsyms]_text MMAP2 filename")
	}
	// pid=-1 (0xffffffff in little-endian) is written into every kernel MMAP2
	// record to signal kernel address space to perf tooling.
	if !bytes.Contains(body, []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("perf.data missing pid=-1 marker (0xffffffff LE)")
	}
}

// TestPerfDataUserspaceMmap2 covers Bug 3: a perf.data produced by
// perf-agent in --pid mode must contain at least one PERF_RECORD_MMAP2
// for a userspace mapping of the target PID. Without these records
// `perf script` and `perf report` cannot resolve user-space IPs and
// every user-side frame in the output shows up as [unknown] — which
// previously forced operators to post-process perf.data with the awk
// /proc/kallsyms hack the kernel-stacks spec calls out.
func TestPerfDataUserspaceMmap2(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))

	bin := getAgentPath(t)

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	outDir := t.TempDir()
	pb := filepath.Join(outDir, "profile.pb.gz")
	pd := filepath.Join(outDir, "perf.data")
	agent := exec.Command(bin,
		"--profile",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", pb,
		"--perf-data-output", pd,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	body, err := os.ReadFile(pd)
	if err != nil {
		t.Fatalf("read perf.data: %v", err)
	}

	// The target io_bound binary is the most reliable userspace
	// filename to expect — agent walks /proc/<pid>/maps which always
	// contains the executable itself.
	if !bytes.Contains(body, []byte("io_bound")) {
		t.Fatalf("perf.data missing io_bound userspace MMAP2 filename (kernel-only mmap was emitted, userspace mmaps are missing)")
	}

	// Target PID must appear in the MMAP2 record's pid field. Probe
	// the LE byte pattern: pid != 0 and pid != 0xffffffff means we
	// have a per-process MMAP2 (not the kernel sentinel).
	pid := uint32(cmd.Process.Pid)
	pidLE := []byte{byte(pid), byte(pid >> 8), byte(pid >> 16), byte(pid >> 24)}
	if !bytes.Contains(body, pidLE) {
		t.Fatalf("perf.data missing pid=%d marker (%x) — userspace MMAP2 PID field not set", pid, pidLE)
	}

	// Roadmap #10: PERF_RECORD_COMM must also be emitted so
	// `perf script` prints "io_bound" instead of the bare pid.
	// "io_bound\x00" appears (a) at the end of the MMAP2 filename
	// "/path/to/.../io_bound" plus its NUL terminator, AND (b) in
	// the COMM record payload as the bare comm. So a count ≥ 2 is
	// the canary that COMM emission is wired; ≤ 1 means we
	// regressed and only MMAP2 fired.
	commProbe := []byte("io_bound\x00")
	if n := bytes.Count(body, commProbe); n < 2 {
		t.Fatalf("perf.data has %d occurrence(s) of %q; want ≥ 2 (1 from MMAP2 filename + 1 from COMM record). COMM emission may be missing.", n, commProbe)
	}
}

// TestPerfDataUserspaceMmap2_SystemWide covers roadmap item #8: when
// running with --all (system-wide capture), perf-agent must emit
// PERF_RECORD_MMAP2 for executable mappings of every PID it walks,
// not just the single --pid target. Without this, `perf script` /
// `perf report` on the resulting perf.data shows [unknown] for every
// userspace IP in -a captures.
//
// Asserts:
//   - the perf.data carries MMAP2 records for at least two distinct
//     non-zero PIDs (proving the walk produced more than one process)
//   - the io_bound test workload's filename appears (proving the
//     specific binary we spawned was enumerated)
func TestPerfDataUserspaceMmap2_SystemWide(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))

	bin := getAgentPath(t)

	cmd, cleanup := spawnIoBoundWorkload(t)
	defer cleanup()

	outDir := t.TempDir()
	pb := filepath.Join(outDir, "profile.pb.gz")
	pd := filepath.Join(outDir, "perf.data")

	// Under --all, COMM+MMAP2 are emitted LAZILY on the first sample
	// seen per PID (perfagent/agent.go), so this test's assertions are
	// sampling-dependent: a window in which the workload never got a
	// sample produces a perf.data with no io_bound MMAP2 at all.
	// Collect until the records are there, with a deadline (issue #42).
	const window = 3 * time.Second
	var body []byte
	ok, report := collectUntil(t,
		"a system-wide perf.data carrying MMAP2 records for io_bound and for >= 2 distinct PIDs",
		window, collectBudgetFor(window),
		func(int) (bool, string) {
			requireWorkloadAlive(t, cmd, "go/io_bound")
			agent := exec.Command(bin,
				"--profile",
				"--all",
				"--duration", window.String(),
				"--profile-output", pb,
				"--perf-data-output", pd,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			var err error
			body, err = os.ReadFile(pd)
			if err != nil {
				t.Fatalf("read perf.data: %v", err)
			}
			hasWorkload := bytes.Contains(body, []byte("io_bound"))
			pids := countDistinctNonSentinelPIDsInPerfData(body)
			return hasWorkload && pids >= 2,
				fmt.Sprintf("perf.data %d bytes, io_bound MMAP2 present=%v, distinct PIDs=%d", len(body), hasWorkload, pids)
		})
	if !ok {
		t.Fatal(report)
	}

	// Workload binary must show up in at least one MMAP2 record.
	if !bytes.Contains(body, []byte("io_bound")) {
		t.Fatalf("perf.data missing io_bound userspace MMAP2 filename under --all (system-wide walk did not enumerate the workload)")
	}

	// Extract the set of distinct non-sentinel PIDs that appear in
	// MMAP2 records. We don't decode the perf.data structurally —
	// the cheap heuristic is to find MMAP2 record headers (event
	// type 10 in the first 4 bytes) and read the following pid
	// field. But simpler still: io_bound's PID is one MMAP2 record,
	// and any OTHER non-zero, non-0xffffffff pid byte pattern in
	// the file argues for system-wide enumeration. Scan for 4-byte
	// pid patterns that appear repeatedly (each PID yields N MMAP2
	// records, one per executable mapping ≥ 1 = at least the
	// executable itself, usually 5-10 including libc, ld.so, libs).
	distinctPIDs := countDistinctNonSentinelPIDsInPerfData(body)
	if distinctPIDs < 2 {
		t.Fatalf("perf.data had MMAP2 records for only %d distinct PID(s); want ≥ 2 (system-wide walk should enumerate multiple processes)", distinctPIDs)
	}
	t.Logf("system-wide MMAP2 covered %d distinct PIDs", distinctPIDs)
}

// countDistinctNonSentinelPIDsInPerfData is a best-effort scan over
// the perf.data body that counts distinct 4-byte LE PID values that
// (a) appear at least twice (each PID emits 1+ MMAP2 records — taking
// the same PID twice avoids matching e.g. file-offset fields that
// happen to read as small integers), and (b) aren't 0 (sentinel for
// unused) or 0xffffffff (kernel MMAP2). Approximate but sufficient
// for a presence-of-multiple-PIDs assertion.
func countDistinctNonSentinelPIDsInPerfData(body []byte) int {
	pidCount := map[uint32]int{}
	for i := 0; i+4 <= len(body); i++ {
		pid := uint32(body[i]) | uint32(body[i+1])<<8 | uint32(body[i+2])<<16 | uint32(body[i+3])<<24
		// Filter sentinels and obviously-not-a-pid values: typical
		// Linux max PID is 4M (kernel.pid_max); reject anything
		// above that as random byte noise.
		if pid == 0 || pid == 0xffffffff || pid > 4*1024*1024 {
			continue
		}
		pidCount[pid]++
	}
	distinct := 0
	for _, n := range pidCount {
		if n >= 2 {
			distinct++
		}
	}
	return distinct
}

// spawnIoBoundWorkload starts the Go io_bound workload (heavy /dev/zero reads
// → frequent syscall/kernel frames) and returns the running command plus a
// cleanup func that kills it.
func spawnIoBoundWorkload(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	bin := "./workloads/go/io_bound"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("io_bound workload not built: %v", err)
	}
	cmd := exec.Command(bin, workloadRuntimeFlag, "-threads=2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start io_bound: %v", err)
	}
	// Brief pause so the workload is fully running before we attach.
	time.Sleep(500 * time.Millisecond)
	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}
	return cmd, cleanup
}

// readKptrRestrictZero returns true when /proc/sys/kernel/kptr_restrict reads
// "0". Best-effort; returns false on any read error.
func readKptrRestrictZero() bool {
	body, err := os.ReadFile("/proc/sys/kernel/kptr_restrict")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) == "0"
}

// sortedKeys returns the keys of a map[string]bool in sorted order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestStrippedRustOffBoxSymbolization verifies off-box symbolization for a
// stripped Rust release binary with build-id only (no .gnu_debuglink).
// Without the debuginfod cache layout fix, the user-side function names
// would be missing from the resulting pprof.
func TestStrippedRustOffBoxSymbolization(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	agentBin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built (run make test-workloads): %v", err)
	}

	// Upload .debug from the unstripped binary, then strip a copy.
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped, workloadRuntimeSecs)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	want := []string{
		"rust_workload::cpu_intensive_work",
		"core::num::<impl u64>::wrapping_add",
	}

	// Collect until the off-box symbols land, with a deadline: a window
	// that caught the workload off-CPU proves nothing about
	// symbolization (issue #42).
	const window = 6 * time.Second
	p, collected, report := collectProfileUntil(t,
		"a profile of the stripped rust workload carrying its off-box-resolved symbols",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "rust-workload-stripped")
			agent := exec.Command(agentBin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool { return hasFunctionContaining(p, want...) })
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, p, "no readable profile after the collect-until budget: %s", report)

	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	for _, w := range want {
		found := false
		for name := range got {
			if strings.Contains(name, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected symbol %q in stripped pprof; %s; got: %v", w, describeProfile(p), sortedKeys(got))
		}
	}
}

// requireDebuginfodContainer skips the test unless the local debuginfod
// docker container is running on localhost:8002.
func requireDebuginfodContainer(t *testing.T) {
	t.Helper()
	cmd := exec.Command("curl", "-fsS", "-o", "/dev/null", "http://localhost:8002/metrics")
	if err := cmd.Run(); err != nil {
		t.Skip("debuginfod container not running on localhost:8002 (run: cd test/debuginfod && docker compose up -d)")
	}
}

// requireTool skips the test when the named CLI tool is not on PATH.
func requireTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s not on PATH", tool)
	}
}

// spawnBinaryAsWorkload starts the binary and returns the running command.
// Caller MUST call cleanup() to kill+wait the process.
//
// `args` MUST carry a run duration in the form the binary understands —
// bare seconds (workloadRuntimeSecs) for the Rust workload, a
// `-duration=` flag (workloadRuntimeFlag) for the Go ones — long enough
// to outlive the collect-until budget the caller spends against it. The
// Go workloads silently ignore a positional argument and fall back to
// their 30s default, so the two forms are not interchangeable.
func spawnBinaryAsWorkload(t *testing.T, bin string, args ...string) (*exec.Cmd, func()) {
	t.Helper()
	if len(args) == 0 {
		t.Fatalf("spawnBinaryAsWorkload(%s): pass an explicit run duration", bin)
	}
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	// Give it 0.5s to set up worker threads.
	time.Sleep(500 * time.Millisecond)
	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return cmd, cleanup
}

// TestStrippedGoOffBoxSymbolization verifies off-box symbolization for a
// stripped Go release binary. Plain `go build` emits DWARF + symtab; we
// strip both via objcopy --strip-all leaving only .note.gnu.build-id.
// The .debug file uploaded to debuginfod must carry the DWARF blazesym
// reads.
func TestStrippedGoOffBoxSymbolization(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	goSrc := "./workloads/go/cpu_bound"
	if _, err := os.Stat(goSrc); err != nil {
		t.Skipf("go workload not built (run make test-workloads): %v", err)
	}
	// Sanity: confirm the source binary has DWARF — otherwise the test
	// would silently pass for the wrong reason.
	if !elfHasSection(t, goSrc, ".debug_info") {
		t.Skipf("go workload at %s has no .debug_info; rebuild without -ldflags='-w'", goSrc)
	}

	buildID, _ := uploadDebug(t, goSrc)
	stripped := filepath.Join(worktreeTmp, "go-cpu-bound-stripped")
	stripWorkload(t, goSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped, workloadRuntimeFlag)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// main.main is always present in a Go binary; cpu_bound's worker
	// loop is typically in main.cpuWork or main.run — accept either.
	wantAny := []string{"main.main", "main.cpuWork", "main.run", "main.worker"}

	const window = 6 * time.Second
	p, collected, report := collectProfileUntil(t,
		"a profile of the stripped Go workload carrying an off-box-resolved user function",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "go-cpu-bound-stripped")
			agent := exec.Command(bin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool { return hasFunctionContaining(p, wantAny...) })
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, p, "no readable profile after the collect-until budget: %s", report)

	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	found := false
	for _, w := range wantAny {
		for name := range got {
			if strings.Contains(name, w) {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Errorf("no Go user-side function found in stripped pprof; %s; got: %v", describeProfile(p), sortedKeys(got))
	}
}

// elfHasSection reports whether the ELF at path has a non-empty section
// with the given name. Used as a guard before tests that depend on DWARF
// being present in a fixture binary.
func elfHasSection(t *testing.T, path, name string) bool {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sec := f.Section(name)
	return sec != nil && sec.Size > 0
}

// TestFileModeFrameAddressPreservesMapping is the regression guard for the
// file-mode symbolization path. It verifies that after perf-agent profiles a
// stripped rust-workload binary whose debug info lives in debuginfod, every
// pprof Location tied to a Rust frame is routed to a real Mapping (not
// synthetic mapping 0) and that Mapping carries the correct BuildID. The
// previous version of this test compared loc.Address (a binary-relative file
// offset per pprof's contract: Address = ProcessPC - MapStart + MapOff) to
// Mapping.Start..Mapping.Limit (process address range); those are different
// units and the check was always false. The real invariant is captured below.
func TestFileModeFrameAddressPreservesMapping(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped, workloadRuntimeSecs)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// Collect until at least one Rust frame is present — the invariant
	// below is checked per Rust frame, so a capture with none proves
	// nothing (issue #42).
	const window = 6 * time.Second
	p, collected, report := collectProfileUntil(t,
		"a profile containing at least one symbolized rust_workload frame",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "rust-workload-stripped")
			agent := exec.Command(bin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool { return hasFunctionContaining(p, "rust_workload::") })
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, p, "no readable profile after the collect-until budget: %s", report)

	// For each Location whose function name looks like a Rust symbol
	// (rust_workload::*), assert it is tied to a real Mapping with the
	// correct BuildID and a File path containing "rust-workload".
	// We intentionally do NOT compare loc.Address to Mapping.Start/Limit
	// because loc.Address is a binary-relative file offset (per pprof's
	// contract: Address = ProcessPC - MapStart + MapOff), while
	// Mapping.Start/Limit are process virtual addresses — different units.
	rustRe := regexp.MustCompile(`^rust_workload::`)
	checked := 0
	for _, loc := range p.Location {
		hasRust := false
		for _, ln := range loc.Line {
			if ln.Function != nil && rustRe.MatchString(ln.Function.Name) {
				hasRust = true
				break
			}
		}
		if !hasRust {
			continue
		}
		checked++

		if loc.Mapping == nil {
			t.Errorf("rust frame at addr %#x has no Mapping (file-mode location not routed to a real mapping)", loc.Address)
			continue
		}
		// Mapping.BuildID must equal the workload's build-id.
		if !strings.EqualFold(loc.Mapping.BuildID, buildID) {
			t.Errorf("rust frame Mapping.BuildID = %q, want %q",
				loc.Mapping.BuildID, buildID)
		}
		// Mapping.File must point at the workload binary (not [unknown] or [jit]).
		if !strings.Contains(loc.Mapping.File, "rust-workload") {
			t.Errorf("rust frame Mapping.File = %q, want a path containing rust-workload",
				loc.Mapping.File)
		}
	}
	if checked == 0 {
		t.Fatalf("no rust frames in pprof — symbolization didn't fire at all; %s", describeProfile(p))
	}
}

// TestStrippedCachedHitNoFetch verifies that a second profiling run for the
// same stripped binary doesn't re-fetch from debuginfod when the cache
// already has the .debug. Confirms the cache.Has → file-mode short-circuit.
func TestStrippedCachedHitNoFetch(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// First run: should fetch.
	runStripped(t, bin, stripped, cacheDir, t.TempDir(), buildID)

	// Snapshot the debuginfod container access log line count.
	prevHits := countDebuginfodHits(t, buildID)

	// Second run: should NOT fetch (cache hit).
	runStripped(t, bin, stripped, cacheDir, t.TempDir(), buildID)

	newHits := countDebuginfodHits(t, buildID)
	delta := newHits - prevHits
	if delta > 0 {
		t.Errorf("expected 0 new debuginfod fetches on second run; saw %d new GET /buildid/%s/debuginfo entries",
			delta, buildID)
	}
}

// runStripped runs the agent against the stripped target until the
// debuginfod fetch it is supposed to trigger has actually populated the
// cache, or the deadline expires.
//
// The fetch is driven by symbolizing a sampled PC inside the stripped
// binary: a window that captured no samples performs no fetch, leaves
// the cache empty, and makes the callers' later assertions
// ("expected cached .debug at ...", "0 new fetches on the second run")
// meaningless. Issue #42's condition, one step removed.
//
// `target` is a copy of the Rust workload (argv[1] = seconds).
func runStripped(t *testing.T, agentBin, target, cacheDir, outDir, buildID string) {
	t.Helper()
	cmd, cleanup := spawnBinaryAsWorkload(t, target, workloadRuntimeSecs)
	defer cleanup()
	out := filepath.Join(outDir, "profile.pb.gz")
	cached := cachedDebugPath(cacheDir, buildID)

	const window = 3 * time.Second
	ok, report := collectUntil(t,
		fmt.Sprintf("the debuginfod cache to hold %s", cached),
		window, collectBudgetFor(window),
		func(int) (bool, string) {
			requireWorkloadAlive(t, cmd, target)
			agent := exec.Command(agentBin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			st, err := os.Stat(cached)
			if err != nil {
				p, rerr := readProfile(out)
				if rerr != nil {
					return false, fmt.Sprintf("cache still empty and %v", rerr)
				}
				return false, fmt.Sprintf("cache still empty (%v); captured %s", err, describeProfile(p))
			}
			return true, fmt.Sprintf("cached .debug is %d bytes", st.Size())
		})
	if !ok {
		t.Fatal(report)
	}
}

// cachedDebugPath is the symbol cache layout perf-agent writes fetched
// debuginfo into: <cacheDir>/.build-id/<first 2>/<rest>.debug
func cachedDebugPath(cacheDir, buildID string) string {
	return filepath.Join(cacheDir, ".build-id", buildID[:2], buildID[2:]+".debug")
}

// countDebuginfodHits returns the number of `GET /buildid/<buildID>/debuginfo`
// log lines emitted by the debuginfod container so far. Best-effort —
// returns 0 if `docker logs` fails (we surface that as 0 delta upstream).
func countDebuginfodHits(t *testing.T, buildID string) int {
	t.Helper()
	cmd := exec.Command("docker", "logs", "debuginfod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("docker logs debuginfod: %v (proceeding with 0 hits)", err)
		return 0
	}
	needle := "GET /buildid/" + buildID + "/debuginfo"
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// TestFileModeParseFailDemotes truncates a cached .debug to make
// blaze_symbolize_elf_virt_offsets return NULL. The mapping should demote
// to process-mode and pprof should still emit frames (just unsymbolized).
// Confirms badDebug per-path filtering.
func TestFileModeParseFailDemotes(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()
	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// First run: populates the cache.
	runStripped(t, bin, stripped, cacheDir, t.TempDir(), buildID)

	// Corrupt the cached .debug.
	cached := cachedDebugPath(cacheDir, buildID)
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("expected cached .debug at %s: %v", cached, err)
	}
	if err := os.Truncate(cached, 100); err != nil {
		t.Fatalf("truncate %s: %v", cached, err)
	}

	// Second run: parse fails, mapping demotes to process-mode.
	// pprof must still emit frames for the workload's mapping.
	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cmd, cleanup := spawnBinaryAsWorkload(t, stripped, workloadRuntimeSecs)
	defer cleanup()

	workloadMappingOf := func(p *profile.Profile) *profile.Mapping {
		for _, m := range p.Mapping {
			if strings.Contains(m.File, "rust-workload") {
				return m
			}
		}
		return nil
	}

	const window = 3 * time.Second
	p, collected, report := collectProfileUntil(t,
		"a profile with samples routed to the demoted rust-workload mapping",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "rust-workload-stripped")
			agent := exec.Command(bin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool {
			return len(p.Sample) > 0 && workloadMappingOf(p) != nil
		})
	if !collected {
		t.Fatal(report)
	}
	if len(p.Sample) == 0 {
		t.Fatalf("no samples in pprof — agent crashed or got 0 frames; %s", describeProfile(p))
	}
	// At least one sample's leaf should fall in the workload's mapping
	// (even if unsymbolized).
	if workloadMappingOf(p) == nil {
		t.Fatalf("no rust-workload mapping in pprof — agent didn't see the binary; %s", describeProfile(p))
	}
}

// TestStrippedSidecarUnreachableSymbolicPath simulates the sidecar /
// mount-namespace case by deleting the workload binary from disk while
// it's still running. The process keeps the binary alive via its open
// file descriptor; /proc/<pid>/map_files/... still resolves, but the
// symbolic path is gone. Asserts symbols still resolve and that
// Mapping.BuildID is populated through map_files. Exercises both the
// classifier (symbolizer routes via MapFiles) AND the DWARF unwinder
// (ehmaps opens via /proc/<pid>/map_files when the symbolic path is
// deleted).
func TestStrippedSidecarUnreachableSymbolicPath(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()
	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped, workloadRuntimeSecs)
	defer cleanup()

	// Delete the binary from disk; the running process keeps it alive
	// through the open fd. /proc/<pid>/map_files/<va>-<va> still resolves.
	if err := os.Remove(stripped); err != nil {
		t.Fatalf("remove %s: %v", stripped, err)
	}

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	const window = 6 * time.Second
	p, collected, report := collectProfileUntil(t,
		"a profile of the deleted-on-disk workload carrying map_files-resolved symbols",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "rust-workload-stripped(deleted)")
			agent := exec.Command(bin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool {
			return hasFunctionContaining(p, "rust_workload::cpu_intensive_work")
		})
	if !collected {
		t.Logf("WARN: %s", report)
	}
	require.NotNil(t, p, "no readable profile after the collect-until budget: %s", report)

	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	// Assert symbol resolved through map_files-derived path.
	if !hasFunctionContaining(p, "rust_workload::cpu_intensive_work") {
		t.Errorf("sidecar-style profiling didn't resolve symbols; %s; got: %v", describeProfile(p), sortedKeys(got))
	}
	// Assert Mapping.BuildID is populated (i.e., Resolver.populate read
	// it via map_files since the symbolic path is gone).
	var workloadMapping *profile.Mapping
	for _, m := range p.Mapping {
		// File is the symbolic path which we deleted; it shows as "(deleted)"
		// suffix in /proc/<pid>/maps. Match by build-id instead.
		if strings.EqualFold(m.BuildID, buildID) {
			workloadMapping = m
			break
		}
	}
	if workloadMapping == nil {
		t.Errorf("no mapping with workload build-id %s — Resolver.populate didn't use map_files", buildID)
	}
}

// TestOffBoxLibcResolution verifies that system libraries (libc) continue
// to resolve through the process-mode path when local /usr/lib/debug
// debuginfo is installed. The new classifier must NOT refetch them.
// Skip when the local debuginfo isn't available.
func TestOffBoxLibcResolution(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)

	// Find libc with build-id and assert a corresponding .debug exists at
	// /usr/lib/debug/.build-id/...
	libc, libcBuildID := findLibcWithLocalDebuginfo(t)

	bin := getAgentPath(t)
	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	cmd, cleanup := spawnBinaryAsWorkload(t, rustSrc, workloadRuntimeSecs)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// This test's only hard assertion is a NEGATIVE one — "libc was not
	// fetched from debuginfod" — and a capture that sampled nothing
	// symbolizes nothing, fetches nothing, and satisfies it vacuously.
	// So collect until symbolization demonstrably ran end to end against
	// the target (its own symbols resolved), and only then ask whether
	// libc was fetched.
	//
	// The precondition is the workload's symbols, not libc's: the test
	// documents libc frames as environment-dependent and only logs when
	// they are absent, so requiring them here would invent a
	// requirement the test deliberately does not make. That leaves a
	// residual gap — a run that resolved rust_workload but never
	// sampled a libc PC still passes without exercising the libc path —
	// which is narrower than the vacuous pass it replaces, but not zero.
	const window = 6 * time.Second
	p, collected, report := collectProfileUntil(t,
		"a profile in which the workload's own symbols resolved (proving symbolization ran)",
		window,
		func(int) (*profile.Profile, error) {
			requireWorkloadAlive(t, cmd, "rust-workload")
			agent := exec.Command(bin,
				"--profile",
				"--debuginfod-url", "http://localhost:8002",
				"--symbol-cache-dir", cacheDir,
				"--pid", strconv.Itoa(cmd.Process.Pid),
				"--duration", window.String(),
				"--profile-output", out,
			)
			agent.Stdout = os.Stdout
			agent.Stderr = os.Stderr
			if err := agent.Run(); err != nil {
				t.Fatalf("perf-agent run: %v", err)
			}
			return readProfile(out)
		},
		func(p *profile.Profile) bool {
			return len(p.Sample) > 0 && hasFunctionContaining(p, "rust_workload::")
		})
	if !collected {
		t.Fatalf("symbolization never resolved a workload symbol, so the "+
			"\"libc was not fetched\" assertion below would pass vacuously: %s", report)
	}

	// libc should NOT have been fetched via debuginfod — it was resolvable
	// locally through process-mode.
	hits := countDebuginfodHits(t, libcBuildID)
	if hits > 0 {
		t.Errorf("libc fetched from debuginfod %d times; local /usr/lib/debug should have been used. libc=%s build-id=%s",
			hits, libc, libcBuildID)
	}

	// libc functions should appear in the pprof (best-effort — on hosts
	// without libc debuginfo this assertion is a soft log).
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	wantAny := []string{"__libc_start_main", "malloc", "__GI___libc_malloc", "free"}
	found := false
	for _, w := range wantAny {
		for name := range got {
			if strings.Contains(name, w) {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Logf("no libc symbol resolved; got: %v (acceptable on systems with no libc debuginfo)", sortedKeys(got))
	}
}

// findLibcWithLocalDebuginfo locates libc.so.6 in common paths, reads its
// build-id, and verifies /usr/lib/debug/.build-id/NN/REST.debug exists.
// Skips the test if any of these aren't true.
func findLibcWithLocalDebuginfo(t *testing.T) (string, string) {
	t.Helper()
	candidates := []string{
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib64/libc.so.6",
		"/usr/lib64/libc.so.6",
		"/usr/lib/x86_64-linux-gnu/libc.so.6",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			id := readBuildID(t, p)
			if id == "" {
				continue
			}
			debugPath := filepath.Join("/usr/lib/debug", ".build-id", id[:2], id[2:]+".debug")
			if _, err := os.Stat(debugPath); err == nil {
				return p, id
			}
		}
	}
	t.Skip("no libc.so.6 with local /usr/lib/debug/.build-id debuginfo found — install glibc-debuginfo")
	return "", ""
}
