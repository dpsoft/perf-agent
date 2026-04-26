# perf-agent benchmark suite

Two-layer benchmark for `--unwind dwarf` startup cost. Companion to
`docs/superpowers/specs/2026-04-25-unwind-auto-benchmark-design.md`.

## Layers

- **Corpus** (`unwind/ehcompile/ehcompile_bench_test.go`). Per-binary
  `ehcompile.Compile` cost via `go test -bench`. No caps needed,
  `benchstat`-friendly. Run via `make bench-corpus`.
- **Scenario** (`bench/cmd/scenario/`). End-to-end `dwarfagent.newSession()`
  cost on a synthetic process fleet. Caps required.
  Run via `make bench-scenarios` (one-time `sudo setcap` on the binary).

## Scenarios

- `pid-large` — one Rust release binary, attached via `--pid`. Measures
  per-mapping compile cost for a single process.
- `system-wide-mixed` — N processes across Go/Python/Rust/Node from
  `test/workloads/`, attached via `-a`. Measures `/proc/*` walk +
  per-PID maps parse + per-distinct-binary compile.

## First-time setup

```bash
make bench-build       # builds scenario + report binaries
make test-workloads    # builds the workload fixtures (also a bench-scenarios prereq)
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario
```

The setcap is one-time per binary build. Don't put the binary in `/tmp` —
that mount has `nosuid`, which strips file capabilities at exec time.

## Caveat

`system-wide-mixed` exercises **PID scaling**, not **binary diversity** —
distinct-binary count is bounded by the test workload set + their shared
libs (~20–30). The "40s on 500-process host" anecdote in the
unwind-auto-refinement doc came from a real laptop with many distinct
service binaries. The corpus layer covers per-binary cost; for
real-world end-to-end numbers, run `perf-agent -a` on your host
directly.

## Output

Each scenario run writes `bench-<scenario>-<timestamp>.json` (or
the path you pass via `--out`). The schema is in `bench/internal/schema/`.
The aggregator (`bench/cmd/report/`) reads JSON and produces markdown.

```bash
./bench/cmd/report/report --in bench-pid-large.json bench-system-wide-mixed.json
./bench/cmd/report/report --diff before.json after.json
```

## Flags

`bench/cmd/scenario`:
- `--scenario pid-large | system-wide-mixed` (required)
- `--processes N` (default 30) — fleet size for system-wide
- `--runs N` (default 5) — iterations
- `--drop-cache` (default off) — drop page cache between runs (warm-cache by default)
- `--out PATH` — JSON output path
- `--workloads-dir PATH` — auto-detected if not set

`bench/cmd/report`:
- `--in PATH` (repeatable) — summary mode
- `--diff A.json --diff B.json` — diff mode
- `--format markdown|csv` (default markdown; csv not yet implemented)
