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
- `gpu-pc-overhead` — the marginal cost of GPU PC sampling, and the
  pre-committed thresholds that turn it into a decision. See below.

## `gpu-pc-overhead`

Plan Task 12. Measures the **marginal** cost of PC sampling: the baseline arm
is the shipping Phase 4 configuration (shim injected, RUNTIME + RESOURCE
callbacks, `CONCURRENT_KERNEL` activity, 100 ms drain, consumer attached) with
PC sampling **off**, not an uninjected run. Spec §9.1 already measured
injection and the activity path, and those costs are paid either way.

Five arms, five interleaved runs each, medians, fixed work rather than fixed
time:

| arm | duty |
| --- | --- |
| baseline (PC sampling off) | — |
| Tier B continuous | — |
| Tier A 50 ms / 450 ms | 10% |
| Tier A 50 ms / 950 ms | 5% |
| Tier A 50 ms / 1950 ms | 2.5% |

The workload is `shim/nvidia/testdata/cuda_concurrent.cu`: several streams,
non-trivial kernel durations, genuine overlap. It is a **second** workload, not
a change to `cuda_workload.cu` — the serial fixture the adapter and the phase
gate are proven against gives serialization nothing to destroy, so measuring
Tier A on it would understate its cost. The harness measures the achieved
concurrency out of the profile and **fails** if the baseline arm is near
serial or its kernels are microseconds long.

Every arm proves it ran in the mode it claims, from both ends independently —
the adapter's own report line on stderr and the consumer's counters — and a
mismatch fails the run rather than contributing a number. Cross-arm, a lower
duty must open strictly fewer bursts, or the three Tier A arms are one arm
under three names.

```bash
make -C shim nvidia nvidia-concurrent
make bench-build
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario
make bench-gpu-pc-overhead
```

Exit codes: `0` when the measurement completed (whatever the verdict), `3`
when an arm could not prove what it measured.

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
- `--scenario pid-large | system-wide-mixed | self | gpu-pc-overhead` (required)
- `--processes N` (default 30) — fleet size for system-wide
- `--runs N` (default 5) — iterations
- `--drop-cache` (default off) — drop page cache between runs (warm-cache by default)
- `--out PATH` — JSON output path
- `--workloads-dir PATH` — auto-detected if not set

`gpu-pc-overhead` only:
- `--gpu-shim PATH` / `--gpu-workload PATH` — the adapter and the concurrent workload
- `--gpu-iters N` / `--gpu-rounds N` — the fixed work; the calibration pass says
  when they need retuning for the device in front of you
- `--gpu-streams N` (4) / `--gpu-blocks N` (16) / `--gpu-threads N` (256) —
  the concurrency; fewer blocks means more kernels co-reside
- `--gpu-sync-every N` (4) — device sync cadence; forces concurrency to refill
- `--gpu-min-concurrency` (1.5) / `--gpu-min-kernel-us` (50) — the guards that
  refuse to report numbers from a microbenchmark
- `--gpu-min-bursts` (4) — the floor on bursts per Tier A arm
- `--gpu-min-calibration-sec` (10) / `--gpu-max-calibration-sec` (120)

`bench/cmd/report`:
- `--in PATH` (repeatable) — summary mode
- `--diff A.json --diff B.json` — diff mode
- `--format markdown|csv` (default markdown; csv not yet implemented)
