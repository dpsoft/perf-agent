// Command scenario runs a perf-agent --unwind dwarf startup benchmark
// against a synthetic process fleet, recording per-binary CFI compile
// timings via dwarfagent.Hooks.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/bench/internal/fleet"
	"github.com/dpsoft/perf-agent/bench/internal/schema"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

func modeFromFlag(s string) dwarfagent.Mode {
	switch s {
	case "dwarf":
		return dwarfagent.ModeEager
	case "auto":
		return dwarfagent.ModeLazy
	default:
		log.Fatalf("invalid --unwind %q (want auto or dwarf)", s)
		return dwarfagent.ModeEager // unreachable
	}
}

func main() {
	var (
		scenario     = flag.String("scenario", "", "pid-large | system-wide-mixed | self (required)")
		processes   = flag.Int("processes", 30, "fleet size for system-wide-mixed")
		runs        = flag.Int("runs", 5, "iterations per scenario")
		dropCache   = flag.Bool("drop-cache", false, "drop page cache between runs (root-only)")
		outPath     = flag.String("out", "", "output JSON path (default ./bench-{scenario}-{ts}.json)")
		workloadDir = flag.String("workloads-dir", "", "test/workloads dir (default auto-detect)")
		unwind      = flag.String("unwind", "auto", "unwind mode passed to dwarfagent: auto (lazy) | dwarf (eager)")
		// self-scenario specific flags
		agentPath        = flag.String("agent", "", "path to perf-agent binary for the self scenario (default ./perf-agent)")
		selfDuration     = flag.Duration("self-duration", 10*time.Second, "capture window for each perf-agent in the self scenario")
		cpuBudget        = flag.Float64("cpu-budget", 0, "self scenario: max allowed CPU overhead ratio (agent samples / workload samples); 0 disables the gate")
		resolutionBudget = flag.Float64("resolution-budget", 0, "self scenario: min allowed kernel-symbol resolution rate; 0 disables the gate")
	)
	// NOTE: --inject-python was previously plumbed here, but the bench
	// constructs dwarfagent.NewProfilerWithMode directly rather than going
	// through perfagent.Agent, so the flag had no behavioural effect — it
	// only landed in the JSON. Re-add when the bench is reworked around
	// perfagent.Agent (which owns the python.Manager wiring).
	flag.Parse()

	if *scenario == "" {
		_, _ = fmt.Fprintln(os.Stderr, "--scenario is required")
		os.Exit(2)
	}

	// The "self" scenario is pure orchestration — it spawns
	// perf-agent subprocesses which carry their own file caps.
	// Other scenarios use dwarfagent.NewProfilerWithMode in-process
	// and need caps on this binary itself.
	if *scenario != "self" && !checkCaps() {
		_, _ = fmt.Fprintln(os.Stdout, "BENCH_SKIPPED: missing required capabilities (CAP_PERFMON, CAP_BPF, CAP_SYS_ADMIN, CAP_SYS_PTRACE, CAP_CHECKPOINT_RESTORE)")
		os.Exit(0)
	}

	dir := *workloadDir
	if dir == "" {
		var err error
		dir, err = autodetectWorkloadDir()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "auto-detect workloads: %v\n", err)
			os.Exit(2)
		}
	}

	doc := &schema.Document{
		Scenario:  *scenario,
		StartedAt: time.Now().UTC(),
		Config: schema.Config{
			Processes:  *processes,
			Runs:       *runs,
			DropCache:  *dropCache,
			UnwindMode: *unwind,
		},
		System: gatherSystemInfo(),
	}

	switch *scenario {
	case "pid-large":
		runPIDLarge(doc, dir, *runs, *dropCache, *unwind)
	case "system-wide-mixed":
		runSystemWideMixed(doc, dir, *processes, *runs, *dropCache, *unwind)
	case "self":
		ap := *agentPath
		if ap == "" {
			ap = "./perf-agent"
		}
		abs, err := filepath.Abs(ap)
		if err != nil {
			log.Fatalf("resolve agent path: %v", err)
		}
		if _, err := os.Stat(abs); err != nil {
			log.Fatalf("perf-agent binary not found at %s (set --agent or run `make build`): %v", abs, err)
		}
		selfBudgetsMet = runSelf(doc, dir, abs, *runs, *dropCache, *selfDuration, *cpuBudget, *resolutionBudget)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
		os.Exit(2)
	}

	out := *outPath
	if out == "" {
		out = fmt.Sprintf("bench-%s-%d.json", *scenario, time.Now().Unix())
	}
	f, err := os.Create(out)
	if err != nil {
		log.Fatalf("create %s: %v", out, err)
	}
	defer func() { _ = f.Close() }()
	if err := schema.Write(f, doc); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "wrote %s\n", out)

	// Non-zero exit on budget breach so CI can gate on the result.
	// Only the "self" scenario currently has budgets — others have
	// no concept of pass/fail (they're measurement-only).
	if *scenario == "self" && !selfBudgetsMet {
		_, _ = fmt.Fprintln(os.Stderr, "self scenario: one or more budget gates breached (see `cpu_overhead_budget_met` / `resolution_rate_budget_met` in the output JSON)")
		os.Exit(3)
	}
}

// selfBudgetsMet is set by the "self" scenario; only consulted when
// --scenario=self is in effect. Package-level so the budget-exit
// check at the end of main() can read it without threading.
var selfBudgetsMet = true

// runPIDLarge spawns one Rust workload, attaches dwarfagent --pid,
// and records per-binary timings across N runs.
func runPIDLarge(doc *schema.Document, workloadDir string, runs int, dropCache bool, unwind string) {
	doc.Config.WorkloadMix = map[string]int{"rust": 1}

	flt, err := fleet.Spawn(fleet.Opts{
		Mix:         map[string]int{"rust": 1},
		WorkloadDir: workloadDir,
	})
	if err != nil {
		log.Fatalf("spawn fleet: %v", err)
	}
	defer func() { _ = flt.Stop() }()

	if err := flt.Wait(10 * time.Second); err != nil {
		log.Fatalf("fleet wait: %v", err)
	}
	pids := flt.PIDs()
	if len(pids) != 1 {
		log.Fatalf("expected 1 PID, got %d", len(pids))
	}
	pid := pids[0]

	for i := 1; i <= runs; i++ {
		if dropCache {
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				log.Printf("drop_caches: %v (continuing — measurement is warm-cache for this run)", err)
			}
		}
		run := measureOnePID(pid, i, unwind)
		doc.Runs = append(doc.Runs, run)
	}
}

// measureOnePID times one NewProfilerWithMode(pid=...) call and the
// per-binary breakdown collected via OnCompile.
func measureOnePID(pid, runN int, unwind string) schema.Run {
	var (
		mu      sync.Mutex
		entries []schema.Binary
	)
	hooks := &dwarfagent.Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			entries = append(entries, schema.Binary{
				Path:         path,
				BuildID:      buildID,
				EhFrameBytes: ehFrameBytes,
				CompileMs:    float64(dur.Microseconds()) / 1000.0,
			})
		},
	}
	sym, err := symbolize.NewLocalSymbolizer()
	if err != nil {
		log.Fatalf("NewLocalSymbolizer (run %d): %v", runN, err)
	}
	t0 := time.Now()
	prof, err := dwarfagent.NewProfilerWithMode(pid, false, []uint{0}, nil, 99, hooks, modeFromFlag(unwind), nil, nil, nil, sym, symbolize.NoopKernelSymbolizer{}, false)
	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		log.Fatalf("NewProfilerWithMode (run %d): %v", runN, err)
	}
	pidCount, binCount := prof.AttachStats()
	_ = prof.Close()

	mu.Lock()
	defer mu.Unlock()
	out := schema.Run{
		RunN:                runN,
		TotalMs:             totalMs,
		PIDCount:            pidCount,
		DistinctBinaryCount: binCount,
		PerBinary:           append([]schema.Binary(nil), entries...),
	}
	return out
}

// runSystemWideMixed spawns a fleet matching the proportional mix and
// times newSession in system-wide mode for each run.
func runSystemWideMixed(doc *schema.Document, workloadDir string, processes, runs int, dropCache bool, unwind string) {
	mix := computeMix(processes)
	doc.Config.WorkloadMix = mix

	flt, err := fleet.Spawn(fleet.Opts{Mix: mix, WorkloadDir: workloadDir})
	if err != nil {
		log.Fatalf("spawn fleet: %v", err)
	}
	defer func() { _ = flt.Stop() }()

	if err := flt.Wait(10 * time.Second); err != nil {
		log.Fatalf("fleet wait: %v", err)
	}

	for i := 1; i <= runs; i++ {
		if dropCache {
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				log.Printf("drop_caches: %v", err)
			}
		}
		run := measureSystemWide(i, unwind)
		doc.Runs = append(doc.Runs, run)
	}
}

// computeMix distributes N processes across {go, python, rust, node}
// using ratios 1/3 : 1/3 : 1/6 : 1/6 with largest-remainder rounding so
// totals always equal N.
func computeMix(n int) map[string]int {
	gp := n / 3
	pp := n / 3
	rp := n / 6
	np := n - gp - pp - rp
	return map[string]int{"go": gp, "python": pp, "rust": rp, "node": np}
}

// measureSystemWide times one NewProfilerWithMode in systemWide=true mode.
func measureSystemWide(runN int, unwind string) schema.Run {
	var (
		mu      sync.Mutex
		entries []schema.Binary
	)
	hooks := &dwarfagent.Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			entries = append(entries, schema.Binary{
				Path:         path,
				BuildID:      buildID,
				EhFrameBytes: ehFrameBytes,
				CompileMs:    float64(dur.Microseconds()) / 1000.0,
			})
		},
	}
	cpus := allCPUs()
	sym, err := symbolize.NewLocalSymbolizer()
	if err != nil {
		log.Fatalf("NewLocalSymbolizer (run %d, system-wide): %v", runN, err)
	}
	t0 := time.Now()
	prof, err := dwarfagent.NewProfilerWithMode(0, true, cpus, nil, 99, hooks, modeFromFlag(unwind), nil, nil, nil, sym, symbolize.NoopKernelSymbolizer{}, false)
	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		log.Fatalf("NewProfilerWithMode (run %d, system-wide): %v", runN, err)
	}
	pidCount, binCount := prof.AttachStats()
	_ = prof.Close()

	mu.Lock()
	defer mu.Unlock()
	return schema.Run{
		RunN:                runN,
		TotalMs:             totalMs,
		PIDCount:            pidCount,
		DistinctBinaryCount: binCount,
		PerBinary:           append([]schema.Binary(nil), entries...),
	}
}

func allCPUs() []uint {
	out := make([]uint, runtime.NumCPU())
	for i := range out {
		out[i] = uint(i)
	}
	return out
}

// autodetectWorkloadDir walks up from CWD looking for test/workloads/.
func autodetectWorkloadDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cur := wd
	for {
		cand := filepath.Join(cur, "test", "workloads")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("test/workloads not found above %s", wd)
		}
		cur = parent
	}
}

// checkCaps returns true if the binary has the full set of capabilities
// the perf-agent BPF programs need. Mirrors `perfagent/agent.go`'s set
// (cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE).
func checkCaps() bool {
	if os.Geteuid() == 0 {
		return true
	}
	caps := cap.GetProc()
	for _, c := range []cap.Value{cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE} {
		have, err := caps.GetFlag(cap.Permitted, c)
		if err != nil || !have {
			return false
		}
	}
	return true
}

func gatherSystemInfo() schema.System {
	out := schema.System{
		NCPU:      runtime.NumCPU(),
		GoVersion: runtime.Version(),
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		out.Kernel = strings.TrimSpace(string(data))
	}
	if cmd := exec.Command("git", "rev-parse", "--short", "HEAD"); cmd != nil {
		if b, err := cmd.Output(); err == nil {
			out.PerfAgentCommit = strings.TrimSpace(string(b))
		}
	}
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					out.CPUModel = strings.TrimSpace(line[i+1:])
				}
				break
			}
		}
	}
	return out
}
