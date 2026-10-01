<!-- examples/rust-pgo/README.md -->
# Rust AutoFDO PGO with perf-agent

A complete, runnable demonstration: build a Rust workload, capture a profile
with perf-agent, convert via Google's `create_llvm_prof` (autofdo), rebuild
with PGO and a stripped final binary, measure the speedup.

## Prerequisites

- Rust toolchain (`cargo --version` ≥ 1.70).
- `perf-agent` built and on PATH (or pass `AGENT=/path/to/perf-agent`).
  Required caps: `setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep`.
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
4. `create_llvm_prof --binary=… --profile=train.perf.data --out=train.prof
   --use_lbr=false` produces an LLVM sample-profile. `--use_lbr=false` is
   required because perf-agent samples cycles without branch records;
   without the flag, AutoFDO produces an empty profile.
5. `cargo build --release` with
   `-Cllvm-args=-sample-profile-file=train.prof -C strip=symbols` —
   PGO build, final binary stripped. Stable rustc has no high-level
   AutoFDO flag (`-C profile-use` is for *instrumented* PGO and rejects
   sample profiles with "bad magic"), so we drop to the underlying LLVM
   option directly.
6. Benchmarks the optimised binary, prints the speedup.

**Measured on rustc 1.97.1: no improvement.** Two runs on an otherwise
idle machine gave 1.00x (-0.0% and -0.4%), which is noise. The pipeline
itself is healthy — `create_llvm_prof` converts the profile, every sample
maps, the `.prof` carries real line-level counts, and LLVM raises no
`-pgo-warn-missing-function` warning — so the profile is being applied
and simply does not move this workload.

The companion [C++ demo](../cpp-pgo/) on the same shape of workload
reaches **1.23x (19%)**, measured the same way. clang's
`-fprofile-sample-use` integrates the profile into the full optimisation
pipeline (inlining, branch layout, register allocation), whereas rustc
only feeds it to LLVM at the codegen pass, and on stable there is no
high-level AutoFDO flag at all.

So treat this example as a demonstration that perf-agent's output drives
the AutoFDO toolchain for Rust, not as a benchmark showing Rust gains
from it. Whether a workload gains is a property of the workload and the
rustc version; see
[#168](https://github.com/dpsoft/perf-agent/issues/168).

## Why this works

The dispatch loop hits the `Add` arm 99% of the time. Without PGO, the
compiler has no way to know which arm is hot, so the match prologue treats
all four arms equally. With AutoFDO, the converter records that `Add` was
overwhelmingly the leaf at runtime; LLVM moves it to fall-through, hoists the
guard out of the loop, and inlines the call. The remaining 1% of operations
take a slow path; the bulk of the time gets faster.
