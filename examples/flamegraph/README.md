<!-- examples/flamegraph/README.md -->
# perf-agent → FlameGraph

Capture a profile with perf-agent and render a Brendan Gregg-style flame
graph. Demonstrates that perf-agent's perf.data output feeds the canonical
flame-graph tooling unchanged.

## Prerequisites

- `perf-agent` built and on PATH, with caps set
  (`setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`).
- `perf` binary on PATH (for `perf script`). Most distributions ship it
  in the `linux-tools` / `perf` / `linux-perf` package.
- Brendan Gregg's FlameGraph scripts:
  ```bash
  git clone https://github.com/brendangregg/FlameGraph
  export FLAMEGRAPH_DIR=$(pwd)/FlameGraph
  ```
  Or copy `stackcollapse-perf.pl` and `flamegraph.pl` into a directory on
  your PATH.

## Run

```bash
# Capture against any running process for 30 seconds:
./capture.sh $(pgrep my-app) 30s

# Or specify a PID directly:
./capture.sh 12345 60s
```

Output: `flame.svg` in the current directory. Open it in any browser —
flame graphs are interactive (click to zoom, search by symbol name).

## What it does

1. `perf-agent --profile --pid <PID> --duration <D> --perf-data-output capture.perf.data`
2. `perf script -i capture.perf.data` → text per-sample dump.
3. `stackcollapse-perf.pl` → "folded" stack format (one stack per line, with sample count).
4. `flamegraph.pl` → SVG.

The pipeline is the same one you'd use with `perf record`. perf-agent slots
in as the capture step; the rest of the chain is unchanged.

## Notes on accuracy

perf-agent samples at 99 Hz with software cpu-clock by default (or hardware
cycles if available — see the agent's INFO log on startup). For flame graphs
that's plenty — the goal is identifying *which functions* dominate, not
precise cycle attribution. If you need higher fidelity, increase the sample
rate (`--sample-rate 999`) or capture for longer (`DURATION=120s`).
