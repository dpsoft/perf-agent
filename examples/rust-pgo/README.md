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
   `-Zprofile-sample-use=train.prof -C strip=symbols` —
   PGO build, final binary stripped. Stable rustc has no high-level
   AutoFDO flag (`-C profile-use` is for *instrumented* PGO and rejects
   sample profiles with "bad magic"), so this uses `-Z profile-sample-use`,
   which needs nightly (or `RUSTC_BOOTSTRAP=1` on stable).
6. Benchmarks the optimised binary, prints the speedup.

**Measured on rustc 1.97.1: 1.50x (33%).**

```
baseline  33.60s   (33.60 / 33.70 / 33.70)
pgo       22.42s   (22.42 / 22.66 / 23.01)
          1.50x faster (33.3% improvement)
```

ITER=20000000000, three runs each, minimum reported.

This README previously said "no improvement", on the strength of two runs
that gave 1.00x. That number was real and the explanation was wrong: the
script passed `-Cllvm-args=-sample-profile-file`, reaching for LLVM's raw
`cl::opt`, and rustc builds its sample-profile pipeline from its own
`PGOOptions` — so the option was parsed and then ignored. No profile was
ever applied.

The old text reasoned that LLVM raised no `-pgo-warn-missing-function`
warning, *therefore* the profile was being applied. That inference is
backwards. A nonexistent profile path produces no warning either, and
exits 0:

```
real profile      -> 296d0c258183ec74
bogus path        -> 296d0c258183ec74
no profile at all -> 296d0c258183ec74
```

Three byte-identical binaries. With `-Zprofile-sample-use` the same
`train.prof` gives a different binary, reproducibly, and the 33% above.

Step 5 now fails the cycle if the control and PGO builds come out
identical, so a silently-ignored flag cannot be reported as a 0% PGO
result again.

The companion [C++ demo](../cpp-pgo/) reaches **1.23x (19%)** on the same
shape of workload. The Rust number being *higher* is not a claim about
rustc beating clang — the two examples' workloads and iteration counts
differ, and neither is a benchmark. Both exist to show that perf-agent's
output drives the AutoFDO toolchain.

See [#168](https://github.com/dpsoft/perf-agent/issues/168).

## Why this works

The dispatch loop hits the `Add` arm 99% of the time. Without PGO, the
compiler has no way to know which arm is hot, so the match prologue treats
all four arms equally. With AutoFDO, the converter records that `Add` was
overwhelmingly the leaf at runtime; LLVM moves it to fall-through, hoists the
guard out of the loop, and inlines the call. The remaining 1% of operations
take a slow path; the bulk of the time gets faster.
