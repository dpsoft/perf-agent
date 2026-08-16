<!-- examples/cpp-pgo/README.md -->
# C++ AutoFDO PGO with perf-agent

Same shape as `examples/rust-pgo` but using clang's
`-fprofile-sample-use=` flag. Useful for comparing PGO impact between
languages on the same algorithmic workload.

## Prerequisites

- clang ≥ 12 (any modern release supports `-fprofile-sample-use`).
- `perf-agent` built and on PATH (caps as documented in the repo README).
- `create_llvm_prof` from <https://github.com/google/autofdo>.
- Optional: `hyperfine`.

## Run

```bash
cd examples/cpp-pgo
./pgo-cycle.sh
```

`ITER` and `DURATION` env vars work the same as in the Rust example.

## What it does

1. Builds `workload-baseline` with `-O2 -g`.
2. Benchmarks it.
3. Captures a profile with perf-agent → `train.perf.data`.
4. Converts via `create_llvm_prof` → `train.prof`.
5. Builds `workload-pgo` with `-fprofile-sample-use=train.prof`.
6. Strips the optimised binary.
7. Benchmarks and prints the speedup.

The same dispatch-loop trick used in the Rust example: 99% of operations
hit a single match arm, AutoFDO pulls that arm to fall-through.
