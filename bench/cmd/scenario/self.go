package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
)

// runSelf implements the "self" scenario: perf-agent profiles a CPU
// workload while a SECOND perf-agent profiles the first. Captures:
//
//   - perf-agent's own CPU overhead (ratio of agent samples to
//     workload samples — directly compares two PIDs sampled at the
//     same rate over the same window)
//   - perf-agent's kernel-symbol resolution rate (fraction of
//     kernel-side Locations that resolved to a named symbol vs the
//     raw-hex fallback path) — this is the canary that would have
//     caught the v1.2.0 lockdown bug at PR time.
//
// Both perf-agents run for the same duration (--duration) so a
// straight sample-count ratio is meaningful. Budget gates can fail
// the scenario when --cpu-budget or --resolution-budget are set.
// runSelf returns true when every run meets both budget gates;
// callers exit non-zero on false so CI surfaces budget breaches.
func runSelf(doc *schema.Document, workloadDir, agentPath string, runs int, dropCache bool, duration time.Duration, cpuBudget, resolutionBudget float64) bool {
	doc.Config.WorkloadMix = map[string]int{"go": 1} // cpu_bound

	cpuBound := filepath.Join(workloadDir, "go", "cpu_bound")
	if _, err := os.Stat(cpuBound); err != nil {
		log.Fatalf("cpu_bound workload not built at %s: %v (run `make test-workloads`)", cpuBound, err)
	}

	allMet := true
	for i := 1; i <= runs; i++ {
		if dropCache {
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				log.Printf("drop_caches: %v (continuing)", err)
			}
		}
		run := measureSelf(i, cpuBound, agentPath, duration, cpuBudget, resolutionBudget)
		if !run.Self.CPUOverheadBudgetMet || !run.Self.ResolutionRateBudgetMet {
			allMet = false
		}
		doc.Runs = append(doc.Runs, run)
	}
	return allMet
}

// measureSelf runs one iteration of the self scenario.
func measureSelf(runN int, cpuBound, agentPath string, duration time.Duration, cpuBudget, resolutionBudget float64) schema.Run {
	t0 := time.Now()

	// Stage 1: spawn the workload (CPU-bound Go binary).
	workload := exec.Command(cpuBound, "-duration="+fmt.Sprint(int(duration.Seconds())+5)+"s", "-threads=1")
	if err := workload.Start(); err != nil {
		log.Fatalf("self run %d: start workload: %v", runN, err)
	}
	workloadPID := workload.Process.Pid
	defer func() {
		_ = workload.Process.Kill()
		_, _ = workload.Process.Wait()
	}()
	time.Sleep(500 * time.Millisecond) // warm-up so first samples land

	// Stage 2: spawn perf-agent #1 against the workload + perf-agent
	// #2 against perf-agent #1, concurrently.
	tmpDir, err := os.MkdirTemp("", "perf-agent-self-bench-*")
	if err != nil {
		log.Fatalf("self run %d: mkdtemp: %v", runN, err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	agent1Pprof := filepath.Join(tmpDir, "agent1.pb.gz")
	agent2Pprof := filepath.Join(tmpDir, "agent2.pb.gz")

	ctx, cancel := context.WithTimeout(context.Background(), duration+30*time.Second)
	defer cancel()

	// Spawn agent #1 first; its PID becomes agent #2's target.
	agent1 := exec.CommandContext(ctx, agentPath,
		"--profile",
		"--kernel-stacks",
		"--pid", strconv.Itoa(workloadPID),
		"--duration", duration.String(),
		"--profile-output", agent1Pprof,
		"--sample-rate", "99",
	)
	agent1.Stdout = os.Stdout
	agent1.Stderr = os.Stderr
	if err := agent1.Start(); err != nil {
		log.Fatalf("self run %d: start agent1: %v", runN, err)
	}
	agent1PID := agent1.Process.Pid
	time.Sleep(200 * time.Millisecond) // give agent1's BPF programs a head start

	agent2 := exec.CommandContext(ctx, agentPath,
		"--profile",
		"--pid", strconv.Itoa(agent1PID),
		"--duration", duration.String(),
		"--profile-output", agent2Pprof,
		"--sample-rate", "99",
	)
	agent2.Stdout = os.Stdout
	agent2.Stderr = os.Stderr
	if err := agent2.Start(); err != nil {
		_ = agent1.Process.Kill()
		log.Fatalf("self run %d: start agent2: %v", runN, err)
	}

	var wg sync.WaitGroup
	var agent1Err, agent2Err error
	wg.Go(func() { agent1Err = agent1.Wait() })
	wg.Go(func() { agent2Err = agent2.Wait() })
	wg.Wait()
	if agent1Err != nil {
		log.Fatalf("self run %d: agent1 wait: %v", runN, agent1Err)
	}
	if agent2Err != nil {
		log.Fatalf("self run %d: agent2 wait: %v", runN, agent2Err)
	}

	// Stage 3: parse both pprofs.
	p1, err := loadPprof(agent1Pprof)
	if err != nil {
		log.Fatalf("self run %d: load agent1 pprof: %v", runN, err)
	}
	p2, err := loadPprof(agent2Pprof)
	if err != nil {
		log.Fatalf("self run %d: load agent2 pprof: %v", runN, err)
	}

	// In --pid mode, each perf-agent's pprof contains only its
	// target's samples (no PID label needed for filtering).
	workloadSamples := len(p1.Sample)
	agentSamples := len(p2.Sample)
	kernelTotal, kernelNamed := countKernelResolution(p1)

	var overheadRatio float64
	if workloadSamples > 0 {
		overheadRatio = float64(agentSamples) / float64(workloadSamples)
	}
	var resolutionRate float64
	if kernelTotal > 0 {
		resolutionRate = float64(kernelNamed) / float64(kernelTotal)
	} else {
		// No kernel frames at all (e.g., --kernel-stacks off): the
		// rate is undefined; report 1.0 so a min-rate budget gate
		// doesn't false-positive on workloads with little kernel
		// time.
		resolutionRate = 1.0
	}

	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	return schema.Run{
		RunN:    runN,
		TotalMs: totalMs,
		Self: schema.SelfMetrics{
			WorkloadPID:             workloadPID,
			AgentPID:                agent1PID,
			WorkloadCPUSamples:      workloadSamples,
			AgentCPUSamples:         agentSamples,
			CPUOverheadRatio:        overheadRatio,
			KernelLocationsTotal:    kernelTotal,
			KernelLocationsNamed:    kernelNamed,
			KernelResolutionRate:    resolutionRate,
			CPUOverheadBudgetMet:    cpuBudget <= 0 || overheadRatio <= cpuBudget,
			ResolutionRateBudgetMet: resolutionBudget <= 0 || resolutionRate >= resolutionBudget,
		},
	}
}

// loadPprof reads a gzip'd pprof file from disk.
func loadPprof(path string) (*profile.Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return profile.Parse(gz)
}

// countKernelResolution returns (total, named) counts over the
// pprof's kernel-side Locations. A Location is "kernel-side" when
// its Mapping has File "[kernel]" (the kernelSentinel mapping from
// pprof/pprof.go). A Location is "named" when its Line.Function.Name
// is non-empty and not a "0x<hex>" raw-address fallback.
func countKernelResolution(p *profile.Profile) (total, named int) {
	kernelMappingID := uint64(0)
	for _, m := range p.Mapping {
		if m.File == "[kernel]" {
			kernelMappingID = m.ID
			break
		}
	}
	if kernelMappingID == 0 {
		return 0, 0
	}
	for _, loc := range p.Location {
		if loc.Mapping == nil || loc.Mapping.ID != kernelMappingID {
			continue
		}
		total++
		if len(loc.Line) == 0 || loc.Line[0].Function == nil {
			continue
		}
		name := loc.Line[0].Function.Name
		if name != "" && !strings.HasPrefix(name, "0x") {
			named++
		}
	}
	return total, named
}
