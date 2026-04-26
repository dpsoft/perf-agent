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
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

func main() {
	var (
		scenario    = flag.String("scenario", "", "pid-large | system-wide-mixed (required)")
		processes   = flag.Int("processes", 30, "fleet size for system-wide-mixed")
		runs        = flag.Int("runs", 5, "iterations per scenario")
		dropCache   = flag.Bool("drop-cache", false, "drop page cache between runs (root-only)")
		outPath     = flag.String("out", "", "output JSON path (default ./bench-{scenario}-{ts}.json)")
		workloadDir = flag.String("workloads-dir", "", "test/workloads dir (default auto-detect)")
	)
	flag.Parse()

	if *scenario == "" {
		fmt.Fprintln(os.Stderr, "--scenario is required")
		os.Exit(2)
	}

	if !checkCaps() {
		fmt.Fprintln(os.Stdout, "BENCH_SKIPPED: missing required capabilities (CAP_PERFMON, CAP_BPF, CAP_SYS_ADMIN, CAP_SYS_PTRACE, CAP_CHECKPOINT_RESTORE)")
		os.Exit(0)
	}

	dir := *workloadDir
	if dir == "" {
		var err error
		dir, err = autodetectWorkloadDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "auto-detect workloads: %v\n", err)
			os.Exit(2)
		}
	}

	doc := &schema.Document{
		Scenario:  *scenario,
		StartedAt: time.Now().UTC(),
		Config: schema.Config{
			Processes: *processes,
			Runs:      *runs,
			DropCache: *dropCache,
		},
		System: gatherSystemInfo(),
	}

	switch *scenario {
	case "pid-large":
		runPIDLarge(doc, dir, *runs, *dropCache)
	case "system-wide-mixed":
		runSystemWideMixed(doc, dir, *processes, *runs, *dropCache)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
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
	defer f.Close()
	if err := schema.Write(f, doc); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	fmt.Fprintf(os.Stdout, "wrote %s\n", out)
}

// runPIDLarge spawns one Rust workload, attaches dwarfagent --pid,
// and records per-binary timings across N runs.
func runPIDLarge(doc *schema.Document, workloadDir string, runs int, dropCache bool) {
	doc.Config.WorkloadMix = map[string]int{"rust": 1}

	flt, err := fleet.Spawn(fleet.Opts{
		Mix:         map[string]int{"rust": 1},
		WorkloadDir: workloadDir,
	})
	if err != nil {
		log.Fatalf("spawn fleet: %v", err)
	}
	defer flt.Stop()

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
		run := measureOnePID(pid, i)
		doc.Runs = append(doc.Runs, run)
	}
}

// measureOnePID times one NewProfilerWithHooks(pid=...) call and the
// per-binary breakdown collected via OnCompile.
func measureOnePID(pid, runN int) schema.Run {
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
	t0 := time.Now()
	prof, err := dwarfagent.NewProfilerWithHooks(pid, false, []uint{0}, nil, 99, hooks)
	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		log.Fatalf("NewProfilerWithHooks (run %d): %v", runN, err)
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
func runSystemWideMixed(doc *schema.Document, workloadDir string, processes, runs int, dropCache bool) {
	mix := computeMix(processes)
	doc.Config.WorkloadMix = mix

	flt, err := fleet.Spawn(fleet.Opts{Mix: mix, WorkloadDir: workloadDir})
	if err != nil {
		log.Fatalf("spawn fleet: %v", err)
	}
	defer flt.Stop()

	if err := flt.Wait(10 * time.Second); err != nil {
		log.Fatalf("fleet wait: %v", err)
	}

	for i := 1; i <= runs; i++ {
		if dropCache {
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				log.Printf("drop_caches: %v", err)
			}
		}
		run := measureSystemWide(i)
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

// measureSystemWide times one NewProfilerWithHooks in systemWide=true mode.
func measureSystemWide(runN int) schema.Run {
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
	t0 := time.Now()
	prof, err := dwarfagent.NewProfilerWithHooks(0, true, cpus, nil, 99, hooks)
	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		log.Fatalf("NewProfilerWithHooks (run %d, system-wide): %v", runN, err)
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
