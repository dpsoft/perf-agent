<!-- examples/rust-pgo/README.md -->
# Rust AutoFDO PGO with perf-agent

A complete, runnable demonstration: build a Rust workload, capture a profile
with perf-agent, convert via Google's `create_llvm_prof` (autofdo), rebuild
with PGO and a stripped final binary, measure the speedup.

## Prerequisites

- Rust toolchain (`cargo --version` ≥ 1.70).
- `perf-agent` built and on PATH (or pass `AGENT=/path/to/perf-agent`).
  Required caps: `setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`.
- `create_llvm_prof` from <https://github.com/google/autofdo>. Build:
  ```bash
  git clone https://github.com/google/autofdo
  cd autofdo
  cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
  cmake --build build
  sudo cp build/create_llvm_prof /usr/local/bin/
  ```
- Optional: `hyperfine` (`cargo install hyperfine`) for nicer benchmark output;
  the script falls back to `/usr/bin/time` if absent.

## Run

```bash
cd examples/rust-pgo
./pgo-cycle.sh
```

Tune the workload size with `ITER=<n>` (default 200M iterations) or capture
duration with `DURATION=60s` (default 30s).

## What it does

1. `cargo build --release` with `-C debuginfo=2` — keeps debug info so
   `create_llvm_prof` can map sample IPs back to function names.
2. Benchmarks the baseline binary.
3. Runs the workload, attaches perf-agent for `$DURATION`, writes
   `train.perf.data`.
4. `create_llvm_prof --binary=… --profile=train.perf.data --out=train.prof`
   produces an LLVM sample-profile.
5. `cargo build --release` with `-C profile-use=train.prof -C strip=symbols` —
   PGO build, final binary stripped.
6. Benchmarks the optimised binary, prints the speedup.

Typical result: 5–15% speedup on this synthetic workload. Real production
workloads vary; the numbers are illustrative.

## Why this works

The dispatch loop hits the `Add` arm 99% of the time. Without PGO, the
compiler has no way to know which arm is hot, so the match prologue treats
all four arms equally. With AutoFDO, the converter records that `Add` was
overwhelmingly the leaf at runtime; LLVM moves it to fall-through, hoists the
guard out of the loop, and inlines the call. The remaining 1% of operations
take a slow path; the bulk of the time gets faster.
