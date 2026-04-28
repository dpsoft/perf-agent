# Benchmarking the `--unwind auto` (= `--unwind dwarf`) Startup Cost

> **Status:** design approved, ready for implementation plan.
> **Companion to:** [unwind-auto-refinement-design.md](../../unwind-auto-refinement-design.md) — that doc lists three refinement paths (S8 MVP / Option A1 / Option A2 / Option B) and recommends benchmarking before committing. This spec defines that benchmark.

## Problem

S8 ships `--unwind auto` as an alias for `--unwind dwarf`: load `perf_dwarf.bpf.c`, eagerly compile CFI tables for every binary visible at startup. Anecdotal cost is ~40s on a ~500-process host (`-a`) and ~1–3s on a large Rust binary (`--pid`). For workloads that are fully FP-equipped, that cost buys nothing.

The refinement doc recommends measuring the real cost on representative workloads before deciding whether to implement Option A1 (lazy attach, no signal), Option A2 (lazy + miss-notify ringbuf), or Option B (binary-level FP/DWARF detection).

This spec defines a **repo-resident benchmark suite** that produces those numbers reproducibly and provides per-binary breakdown so the right refinement is chosen for the right reason.

## Goals

1. Measure end-to-end startup cost of `--unwind dwarf` on two scenarios: per-PID attach to a large binary, and system-wide attach to a fleet of mixed-runtime processes.
2. Produce **per-binary breakdown** (path, build-id, `.eh_frame` size, compile time) so the cost distribution is visible — informs whether A1/A2/B addresses the right tail.
3. Reproducible across machines: deterministic fleet, structured JSON output, system info captured per run.
4. CI-runnable for the corpus (per-binary) layer; opt-in (caps-gated) for the scenario layer.
5. Zero overhead on the production binary when the benchmark hooks are not wired in.

## Non-goals

- Measuring memory cost (peak RSS, BPF map row counts). Useful for Option B but doesn't change the A1/A2 vs B go/no-go decision; deferred until the data demands it.
- Real-host (`/proc` of the dev's actual machine) scenario mode. Anyone wanting it can run `perf-agent -a` on their host with `time(1)` — adds non-determinism without adding signal.
- HTML/chart rendering of results. Markdown tables are sufficient for the gating decision.
- Continuous regression dashboard / CI gating on bench numbers. The suite's job is to *answer questions*; if regressions become a concern later, that's a separate effort.
- Benchmarking the `--unwind fp` path. Its startup cost is ~0 by design; no measurement needed.

## Architecture

Two layers plus shared instrumentation.

```
┌──────────── corpus layer (no caps, fast) ────────────┐
│  unwind/ehcompile/ehcompile_bench_test.go (extended) │
│  go test -bench → benchstat-friendly                  │
└──────────────────────────────────────────────────────┘

┌──────────── scenario layer (caps required) ────────┐
│  bench/cmd/scenario/    custom harness binary       │
│   ├─ flags: --scenario, --processes, --runs, --out  │
│   ├─ uses bench/internal/fleet to spawn workloads   │
│   ├─ constructs dwarfagent.{Profiler,OffCPUProfiler}│
│   ├─ times newSession() end-to-end                  │
│   └─ writes JSON                                     │
│                                                       │
│  bench/cmd/report/      JSON → markdown aggregator   │
└──────────────────────────────────────────────────────┘

┌────────── shared production-code change ─────────┐
│  Hooks struct passed into dwarfagent constructors;│
│  propagated through TableStore → ehcompile.Compile│
│  Nil-safe; zero overhead when unset                │
└────────────────────────────────────────────────────┘
```

**Why two layers.** The corpus layer isolates per-binary `ehcompile.Compile` cost — fast, deterministic, CI-runnable, slots into the existing `ehcompile_bench_test.go` pattern. The scenario layer measures end-to-end startup including `/proc` walk, per-PID maps parse, and BPF map inserts — all of which `b.N` looping doesn't fit, and which require root + caps. Separating them lets each use the model that fits.

## Component 1: Hook plumbing (production-code change)

One new exported type, one new ctor variant per profiler. Existing callers untouched.

**New type** in `unwind/dwarfagent/hooks.go`:

```go
type Hooks struct {
    OnCompile func(path, buildID string, ehFrameBytes int, dur time.Duration)
}
```

**New ctor variants** (existing ctors delegate to these with `nil`):

```go
func NewProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks) (*Profiler, error)
func NewOffCPUProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, hooks *Hooks) (*OffCPUProfiler, error)
```

**Threading path:**

- `dwarfagent.newSession` (`unwind/dwarfagent/common.go:83`) accepts `hooks *Hooks`, passes to `ehmaps.NewTableStore`.
- `TableStore` stores `hooks`. In `AcquireBinary` (`unwind/ehmaps/store.go:101`), the call to `ehcompile.Compile` is wrapped: `t0 := time.Now()` before, `hooks.OnCompile(path, buildID, ehFrameBytes, time.Since(t0))` after. All hook calls are nil-guarded.
- `ehcompile.Compile` itself is **not** modified — it stays a pure compile primitive with no observability concerns. Timing wrap lives in the caller (`store.go`).
- `ehFrameBytes` is the size of the `.eh_frame` section as parsed by `ehcompile.Compile`. Either it returns this alongside the existing entries/classifications, or `store.go` reads it from the ELF before compile. The smaller change wins; pick during implementation.

**End-to-end wall time** is measured by the bench wrapping the constructor — no `OnSessionDone` hook needed.

**PID / binary counts** are already returned by `AttachAllProcesses` / `AttachAllMappings`. We surface them via a new `Profiler.AttachStats() (pidCount, binaryCount int)` method (and the same on `OffCPUProfiler`). The data is already in `TableStore`; this just exposes it.

**Why not functional options pattern.** The project uses functional options in `perfagent/` but `dwarfagent.NewProfiler` doesn't. Threading a single `hooks *Hooks` through ~3 functions is a smaller diff than retrofitting an options pattern, and we'd be the only consumer. If `dwarfagent` grows more knobs we can refactor then.

**Hook safety.** Hook callbacks must not panic; if they do, the call site recovers and logs at debug level. Hooks are observers, not gatekeepers — they cannot fail the operation.

## Component 2: Corpus benchmarks (extended `ehcompile_bench_test.go`)

Existing benchmarks: `BenchmarkCompile_Glibc`, `BenchmarkCompile_HelloX86`, `BenchmarkCompile_HelloArm64`. Extend with:

- `BenchmarkCompile_LargeRustRelease` — uses the `test/workloads/rust/cpu_bound` binary built stripped + frame-pointers off. Represents a "hostile" Rust binary where DWARF is the only option.
- `BenchmarkCompile_LibPython` — `libpython3.X.so` from the system, if present; skip with `t.Skipf` if not. Represents a heavy-runtime shared library.

For each, report `b.ReportMetric(float64(ehFrameBytes), "eh_frame_bytes/op")` and `b.ReportMetric(float64(len(entries)), "entries/op")` — gives `benchstat`-friendly per-iteration size context alongside ns/op.

**No caps required.** Run via `make bench-corpus`.

## Component 3: Scenario harness (`bench/cmd/scenario/`)

A standalone binary, not a Go test — the run loop is "set up fleet, time one cold construction, tear down" repeated N times. Doesn't fit `b.N`.

**CLI surface:**

| Flag | Default | Notes |
|------|---------|-------|
| `--scenario {pid-large \| system-wide-mixed}` | required | |
| `--processes N` | 30 | system-wide only |
| `--runs N` | 5 | iterations |
| `--drop-cache` | false | drops page cache between runs (root-only; we already have caps) |
| `--out PATH` | `./bench-{scenario}-{timestamp}.json` | |
| `--workloads-dir PATH` | auto-detect | locates `test/workloads/` |

**Default workload mix** for `system-wide-mixed --processes 30`: 10 Go, 10 Python, 5 Rust, 5 Node (ratios `1/3 : 1/3 : 1/6 : 1/6`).

**Scaling for non-default `--processes N`**: keep the same ratios, distribute via largest-remainder. Concretely: `go = floor(N/3)`, `python = floor(N/3)`, `rust = floor(N/6)`, `node = N - go - python - rust` (so the totals always sum exactly to `N`). Examples: `N=30 → {10,10,5,5}`, `N=100 → {33,33,16,18}`, `N=300 → {100,100,50,50}`.

**Cap check:** at startup, exit 0 with `BENCH_SKIPPED: missing CAP_PERFMON,CAP_BPF[,...]` if caps are missing. Lets CI invoke `make bench-scenarios` unconditionally — same pattern the integration tests already use.

**Run loop:**

1. Parse flags, cap check.
2. Spawn fleet via `bench/internal/fleet`. Wait until all PIDs are in `S` or `R` state via `/proc/<pid>/stat` (timeout 10s). Fail fast if any worker exits or fails to launch.
3. For `i = 1..runs`:
   - If `--drop-cache`: write `3` to `/proc/sys/vm/drop_caches`.
   - Construct `dwarfagent.NewProfilerWithHooks(...)` with hooks recording per-binary timings. Time the constructor call as `total_ms`.
   - Call `Profiler.AttachStats()` → record `pid_count`, `distinct_binary_count`.
   - Tear down profiler immediately (we measure startup, not steady state).
4. Tear down fleet (idempotent, SIGTERM with 1s grace then SIGKILL).
5. Write JSON.

**Cache state default is warm** because that's the realistic case: when an SRE runs `perf-agent -a` on a long-running host, the executables of running processes are already in page cache. `--drop-cache` is for measuring cold-start specifically.

**Caveat documented in `bench/README.md`:** `system-wide-mixed` exercises **PID scaling**, not **binary diversity** — distinct-binary count is bounded by the test workload set + their shared libs (~20–30). The doc's "40s on 500-process host" anecdote came from a real laptop with many distinct service binaries. The corpus layer covers per-binary cost; for real-world end-to-end numbers, the user runs `perf-agent -a` on their own host.

## Component 4: Fleet driver (`bench/internal/fleet/`)

```go
type Opts struct {
    Mix         map[string]int // {"go": 10, "python": 10, "rust": 5, "node": 5}
    WorkloadDir string
    StartupTimeout time.Duration
}

type Fleet struct { /* PIDs, processes, ... */ }

func Spawn(opts Opts) (*Fleet, error)
func (*Fleet) Wait(timeout time.Duration) error // wait for all PIDs S/R
func (*Fleet) PIDs() []int
func (*Fleet) Stop() error                      // idempotent: SIGTERM, 1s grace, SIGKILL
```

**Implementation notes:**
- Uses `os/exec`. Stdin connected to `/dev/null`; stdout/stderr captured to a buffer for postmortem on failure.
- Workloads are launched with their `cpu_bound` variant (consistent with `test/workloads/`'s primary use). If a language only ships `io_bound`, fall back to that. The choice is not load-bearing for the measurement — we time `newSession`, not steady-state CPU — but pinning to a single default keeps runs reproducible.
- `Stop` must not leak processes — verified by unit test (spawn, kill parent, confirm children reaped).
- No caps needed. Unit-tested.

## Component 5: Output schema (`bench/internal/schema/`)

Shared between `cmd/scenario` (writer) and `cmd/report` (reader). Schema-version-stamped so future readers can detect drift and reject incompatible versions cleanly.

```json
{
  "schema_version": 1,
  "scenario": "system-wide-mixed",
  "config": {
    "processes": 30,
    "runs": 5,
    "drop_cache": false,
    "workload_mix": {"go": 10, "python": 10, "rust": 5, "node": 5}
  },
  "system": {
    "kernel": "6.19.9-200.fc43.x86_64",
    "cpu_model": "AMD Ryzen ...",
    "ncpu": 16,
    "go_version": "go1.26.0",
    "perf_agent_commit": "b5ba18d9"
  },
  "started_at": "2026-04-25T19:30:00Z",
  "runs": [
    {
      "run_n": 1,
      "total_ms": 3214.7,
      "pid_count": 30,
      "distinct_binary_count": 24,
      "per_binary": [
        {"path": "/lib64/libc.so.6", "build_id": "abc...", "eh_frame_bytes": 31420, "compile_ms": 12.31}
      ]
    }
  ]
}
```

**Sort order:** `per_binary` is sorted descending by `compile_ms` in the writer so a casual reader sees hot binaries at the top without re-sorting.

## Component 6: Report tool (`bench/cmd/report/`)

```
report --in PATH...           one or more JSON files (also accepts a directory: globs *.json)
report --diff A.json B.json   benchstat-style comparison
report --format markdown|csv  default markdown
```

**Single-file output (markdown):**

- Summary header: scenario, config, system info.
- Per-run wall-time table with **p50 / p95 / max** across runs.
- "Top 10 binaries by compile time" table — uses **median** compile_ms per binary across runs (not first-run; first run includes any one-time costs). Deduped by build_id; if the same path appears with different build_ids across runs, that's surfaced.

**Diff output:** same tables but with `before → after (Δ%, ±noise)` columns. Noise estimate is the within-config stdev across runs. Format chosen so a PR comment can paste it directly.

**No HTML/charts in v1.** Markdown is sufficient for the gating decision.

**Why a custom tool, not benchstat:** benchstat operates on `go test -bench` text format. Our scenario data is structured JSON with per-binary detail benchstat can't represent. The corpus layer (which IS `go test -bench`) can use benchstat directly — independent.

## File layout

```
bench/
├── cmd/
│   ├── scenario/main.go          # scenario harness binary
│   └── report/main.go            # JSON → markdown aggregator
├── internal/
│   ├── fleet/
│   │   ├── fleet.go              # Spawn/Wait/Stop
│   │   └── fleet_test.go         # unit-tested, no caps
│   └── schema/
│       └── schema.go             # shared JSON types
└── README.md                     # how to run, what numbers mean, caveats

unwind/dwarfagent/
├── hooks.go                      # NEW: Hooks struct
├── agent.go                      # NewProfilerWithHooks added; existing ctor delegates with nil
├── offcpu.go                     # NewOffCPUProfilerWithHooks added; same
└── common.go                     # newSession threads hooks to TableStore

unwind/ehmaps/
└── store.go                      # AcquireBinary fires hook around ehcompile.Compile

unwind/ehcompile/
└── ehcompile_bench_test.go       # extended with LargeRustRelease + LibPython
```

## Makefile additions

```make
bench-corpus:
	GOTOOLCHAIN=auto go test -bench=. -benchmem -run=^$$ ./unwind/ehcompile/...

bench-scenarios:
	$(MAKE) build
	go build -o bench/cmd/scenario/scenario ./bench/cmd/scenario
	go build -o bench/cmd/report/report ./bench/cmd/report
	sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario
	./bench/cmd/scenario/scenario --scenario pid-large --runs 5 --out bench-pid-large.json
	./bench/cmd/scenario/scenario --scenario system-wide-mixed --processes 30 --runs 5 --out bench-system-wide.json
	./bench/cmd/report/report --in bench-pid-large.json bench-system-wide.json --format markdown > bench-report.md
```

The `setcap` step matches the project's existing pattern (setcap once, run without sudo). `bench-scenarios` requires one-time `sudo` to set caps; subsequent runs of the harness don't need it.

## Error handling

| Condition | Exit | Behavior |
|-----------|------|----------|
| Missing caps | 0 | Print `BENCH_SKIPPED: missing <caps>` and exit successfully — lets CI invoke unconditionally. |
| Fleet spawn fails | 2 | Identify failing workload (lang + index) + tail of its stderr. |
| Workload PID exits before measurement loop completes | 3 | Report PID, path, last stderr. |
| BPF program load fails (kernel mismatch, verifier reject, etc.) | 4 | Surface underlying error verbatim. |
| Hook callback panics | — | Recover, log at debug level. Hooks are observers, not gatekeepers. |

## Testing

- `bench/internal/fleet/fleet_test.go` — spawn/wait/stop semantics, leak verification (no caps).
- `bench/internal/schema/schema_test.go` — JSON marshal/unmarshal roundtrip, `schema_version` mismatch handling.
- `bench/cmd/report/main_test.go` — golden-file test: canned JSON in, expected markdown out. Covers single-file and `--diff` modes.
- Hook plumbing — extend an existing `dwarfagent` test (or add a focused one) that constructs a profiler with a hook and verifies `OnCompile` fires for at least one binary on the integration-test workloads. Caps-gated.

## What we are NOT doing in v1

YAGNI fence. Surfaced here so the implementation plan stays focused.

- Memory measurements (peak RSS, BPF map row counts).
- Real-host scenario.
- HTML/chart rendering.
- Continuous regression dashboard / CI gating on numbers.
- Benchmarking the `--unwind fp` path — its startup is ~0 by design.

## Open questions to resolve during implementation

- Does `ehcompile.Compile` already return `.eh_frame` size, or do we read it from the ELF in `store.go` before calling compile? Pick the smaller diff.
- For `pid-large`: does the existing `test/workloads/rust/cpu_bound` already build with frame-pointers off + stripped, or does it need a separate build target? Inspect `Makefile` and adjust.
- If runs prove noisy with `cpu_bound` workloads under load, revisit the fleet variant choice (see Component 4). Not expected to matter — the measurement window is the constructor call, not steady state — but flagged for visibility.
