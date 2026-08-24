# Issue #50 — the launch sampler aliased against periodic launch patterns

Branch `fix/sampler-aliasing`. One commit. Not pushed, no PR.

## Cannot verify (read this first)

`CapEff: 0000000000000000` on this machine. `TestStubDrivesThePipelineToPprofWithoutAGPU`
**skipped**, and no CUDA run was possible. So:

- The end-to-end pipeline assertions in `gpuprobe/gate_test.go` — including the new
  both-kernels-get-stacks assertion and the changed sampled count — **have not been
  executed**. They compile and vet clean; that is all I can say about them.
- The RTX 3090 numbers below are **predictions derived before any run**, from the
  schedule itself. They are not measurements.

What *was* executed: the whole C++ shim suite (`make -C shim test`, plus `test-tsan`),
the new Go replica suite, `go build`, `go vet`, `go test ./gpu/ ./gpuprobe/
./internal/...`, and `golangci-lint` (0 issues). The stub binary was run standalone to
confirm its accounting line.

## The scheme chosen: jittered stride, deterministic given a seed

Option 1 from the issue, with the one addition that makes it keep everything the fixed
stride was chosen for.

After sampling launch `s`, the next sample is at `s + gap_at(s)`, where the gap is drawn
uniformly from the `2k+1` consecutive integers centred on `period` (`k = period/2`):

| period | gap band | mean gap |
|---:|---|---:|
| 1 | {1} | 1 (every launch) |
| 2 | [1, 3] | 2 |
| 3 | [2, 4] | 3 |
| 8 | [4, 12] | 8 |
| 64 | [32, 96] | 64 |

The set is symmetric about `period`, so the mean gap is **exactly** `period` and the
long-run rate is **exactly** `1/period` — the same rate the fixed stride had. Because the
band always spans both parities for any period ≥ 2, the phase cannot lock: over a
`c`-cycle every residue mod `c` is reached.

The gap is **not** read from a running PRNG state. It is a pure function
`gap_at(sample_point, period, seed)` — a splitmix64 finalizer of `seed ^ (seq * φ)`,
mapped into the band by Lemire's multiply-shift. So the schedule `0, s1, s2, …` is a
deterministic chain replayable from the seed alone.

The seed defaults to a fixed constant (`Sampler::kDefaultSeed = 0x9E3779B97F4A7C15`)
rather than being randomized per process. Reproducibility is why this sampler was
deterministic in the first place, and a fixed seed is what keeps the phase gate's
assertion an equality. `PERFAGENT_GPU_SAMPLE_SEED` (CUPTI adapter) buys per-process
variation for anyone who wants to average several runs; both the adapter and the stub now
print the seed they ran on, so any profile is auditable after the fact.

### Why not Bernoulli

Bernoulli is simpler, but the count over a run becomes a random variable with no fixed
value, which turns three exact gate assertions into tolerance bands and makes
`expected_sampled` an estimate. Jittered-with-a-seed gives the same phase-unlocking for
the same hot-path cost while every count stays exact. There was no reason to pay for
Bernoulli's simplicity with a weaker gate.

## The pre-fix failure output

The regression test was written first and run against the unfixed sampler. Verbatim:

```
  alternating 2-kernel at period 8: kernel_a=500 kernel_b=0
  FAIL: the sampler locked phase against a 2-cycle: one kernel of the pair received every stack and the other received none
prefix_sampler_test: /tmp/prefix_sampler_test.cc:76: int main(): Assertion `stacks[1] > 0' failed.
EXIT=134
```

With the asserts removed so the sweep could run to completion, the unfixed sampler locked
phase in **30** of the 33 (period, cycle) pairs tested:

```
  FAIL: period=2 locks phase against a 2-cycle: residue 1 never sampled
  FAIL: period=4 locks phase against a 2-cycle: residue 1 never sampled
  FAIL: period=6 locks phase against a 2-cycle: residue 1 never sampled
  FAIL: period=8 locks phase against a 2-cycle: residue 1 never sampled
  FAIL: period=10 locks phase against a 2-cycle: residue 1 never sampled
  FAIL: period=12 locks phase against a 2-cycle: residue 1 never sampled
  FAIL: period=3 locks phase against a 3-cycle: residue 1 never sampled
  FAIL: period=3 locks phase against a 3-cycle: residue 2 never sampled
  FAIL: period=6 locks phase against a 3-cycle: residue 1 never sampled
  FAIL: period=6 locks phase against a 3-cycle: residue 2 never sampled
  FAIL: period=9 locks phase against a 3-cycle: residue 1 never sampled
  FAIL: period=9 locks phase against a 3-cycle: residue 2 never sampled
  FAIL: period=12 locks phase against a 3-cycle: residue 1 never sampled
  FAIL: period=12 locks phase against a 3-cycle: residue 2 never sampled
  FAIL: period=2 locks phase against a 4-cycle: residue 1 never sampled
  FAIL: period=2 locks phase against a 4-cycle: residue 3 never sampled
  FAIL: period=4 locks phase against a 4-cycle: residue 1 never sampled
  FAIL: period=4 locks phase against a 4-cycle: residue 2 never sampled
  FAIL: period=4 locks phase against a 4-cycle: residue 3 never sampled
  FAIL: period=6 locks phase against a 4-cycle: residue 1 never sampled
  FAIL: period=6 locks phase against a 4-cycle: residue 3 never sampled
  FAIL: period=8 locks phase against a 4-cycle: residue 1 never sampled
  FAIL: period=8 locks phase against a 4-cycle: residue 2 never sampled
  FAIL: period=8 locks phase against a 4-cycle: residue 3 never sampled
  FAIL: period=10 locks phase against a 4-cycle: residue 1 never sampled
  FAIL: period=10 locks phase against a 4-cycle: residue 3 never sampled
  FAIL: period=12 locks phase against a 4-cycle: residue 1 never sampled
  FAIL: period=12 locks phase against a 4-cycle: residue 2 never sampled
  FAIL: period=12 locks phase against a 4-cycle: residue 3 never sampled
```

The rate block passed on the unfixed sampler (it always did — the rate was never the
defect), which is the point: a rate check alone would never have found this.

Post-fix, the same suite:

```
  alternating 2-kernel at period 8: kernel_a=255 kernel_b=250
  rate at period  2: sampled=100069 expected=100000.0 err=+0.069%
  rate at period  3: sampled=66561 expected=66666.7 err=-0.159%
  rate at period  4: sampled=49943 expected=50000.0 err=-0.114%
  rate at period  7: sampled=28596 expected=28571.4 err=+0.086%
  rate at period  8: sampled=25054 expected=25000.0 err=+0.216%
  rate at period 16: sampled=12517 expected=12500.0 err=+0.136%
  rate at period 64: sampled=3124 expected=3125.0 err=-0.032%
  schedule pin: 500 launches at period 8 -> 58 sampled
  schedule pin: 4000 launches at period 8 -> 505 sampled
sampler_test OK
```

## Did `sample_period`'s meaning change?

**Yes, slightly, and it needs stating plainly.** It used to be an *exact* stride: a
consumer could assert `launch_seq % sample_period == 0`. It is now the *mean* stride.

What did **not** change:

- The wire ABI. `gpu_launch_sampled_v1` is byte-for-byte identical; `sample_period` is
  still the `uint32` at offset 44 and is still never 0. No consumer code needed changing,
  and none was changed on the decode path.
- The **rate** it names. Gaps are drawn from a set symmetric about `period`, so the
  long-run rate is exactly `1/sample_period`, as before.
- It is still **not a scale factor**. Confirmed below.

The two comments that stated the old meaning are updated to state the new one:
`shim/core/usdt_abi.h` (the field) and `gpu.LaunchContext.SamplePeriod` (the Go type).

### What replaces `launch_seq % period == 0` as the consumer's check

Two things, and between them the verifiability is stronger than before, not weaker:

1. **Full replay.** `internal/gpuabi.SampleSchedule` / `SampledCount` are a Go replica of
   `Sampler::gap_at()`. Given `(seed, period)` a consumer reproduces the exact set of
   sampled ordinals. The two implementations are pinned to a shared vector —
   `shim/core/sampler_test.cc`'s "schedule pin" block and
   `TestTheGoReplicaMatchesTheShimSampler` assert the same ten gaps, the same first ten
   sample points and the same two counts — so they cannot drift apart quietly.
2. **A seedless invariant.** Consecutive sampled `launch_seq` values from one process must
   differ by a gap inside `[period - period/2, period + period/2]`. That catches a broken
   producer that the modulo check could not: a sampler that stopped, doubled up, or ran at
   the wrong rate.

## Durations are never scaled — confirmed

Unchanged and re-checked end to end:

- `Sampler::should_sample()` returns a bool and touches no timestamp. Its only callers
  (`shim/stub/stub.cc`, `shim/nvidia/cupti_adapter.cc`) use it solely to decide whether to
  fire `gpu_launch_sampled_v1`. `gpu_launch_v1` and `gpu_exec_v1` are emitted for **every**
  launch and execution regardless.
- `gpu/projection.go` still puts `sample_period` in a label only
  (`gpu_sample_period`) and multiplies nothing by it; its comment explaining why scaling
  would turn a measurement into an estimate is untouched.
- The gate's conservation assertions — sample values summing to exactly `totalExecNs`, and
  no sample exceeding its own execution's duration — are untouched (unrunnable here, but
  unmodified).

## The gate-assertion decision

**No assertion was widened. Every exact count stayed exact.** That was the deciding
constraint and it drove the choice of a seeded, pure-function schedule over Bernoulli.

`gpuprobe/gate_test.go` changes, all localised (expect a trivial rebase under #58):

- `wantSampled` 63 → **58**, and the two other hardcoded `63`s with it. 63 was
  `ceil(500/8)`; 58 is the exact length of the default schedule over 500 launches. Still an
  equality, still a magic number with its derivation in the comment, and now cross-checked
  three ways: the stub's own `sampled=` line, the consumer's `Stats.SampledLaunches`, and
  the pinned replica.
- **One assertion added**, not relaxed: the stub alternates two kernel ids one per launch,
  so its launch stream is exactly the 2-cycle this bug aliased against. The gate now
  collects the kernel name of every stack-carrying launch and requires more than one
  distinct name. Under the old sampler this would have **failed** — every sampled ordinal
  was even, so `kernel_1111` took all 63 stacks and `kernel_2222` took none, in a run that
  passed every other assertion in the file. That is the issue reproduced at the pipeline
  level rather than the unit level.
- **One assertion added**: the stub's stderr must carry `seed=0x9e3779b97f4a7c15`. Without
  it the exact 58 above would describe a schedule the run did not use, and a seed change
  would show up as a confusing count mismatch instead of a named cause.

`cmd/gpu-cuda-profile`'s `expected_sampled` stays exact too: it is now
`gpuabi.SampledCount(launches, period, DefaultSampleSeed)` rather than
`ceil(launches/period)`. `cmd/gpu-stub-profile` had the same `2000/8` rounding and would
have parked on its 30 s deadline waiting for a count that is never reached; it uses the
replica now as well.

## Cost — measured, not argued

The header comment initially claimed the new path was *cheaper* (it drops a 64-bit
division). I benchmarked it and that was **wrong**; the claim in the code is now the
measurement. Single-threaded, `-O2`, period 8, 2×10⁸ calls, three runs:

```
old %period  4.377 / 4.332 / 4.319 ns/launch
new jittered 4.887 / 4.818 / 4.881 ns/launch
```

**+0.5 ns/launch, +12%.** Both are dominated by the atomic increment; the extra acquire
load costs a little more than the division saves. At the measured 393k launches/s ceiling
that is 0.0002 s/s — 0.02% of one core, against the 1–2 µs uprobe trap each *sampled*
launch already pays. No syscall, no lock, no allocation, and `gap_at()` runs only on the
launches actually sampled.

## What determinism survives concurrency (and what does not)

Worth being explicit, because it is not obvious and the gate rests on it.

`next_` walks the chain `0, s1, s2, …` whatever the thread interleaving, because each
successor is derived from the value being replaced, not from how many threads raced for
it. So the sampled **count** over L launches is exactly the number of chain points below
L, thread-independently — asserted directly: an 8-trial 4-thread test that must land on
the same count as a single-threaded run of the same length.

What *can* shift under concurrent launches is **which** launch carries each stack: a
thread whose ordinal is past a chain point claims that point, so a sampled `launch_seq`
can be later than its chain point (never earlier). For a single-threaded launch stream —
the stub, the CUDA validation workload, most real submission loops — the two coincide
exactly, and only there is the gap-band invariant exact rather than approximate. Nothing
in the accounting depends on it either way. Documented in `shim/core/sampler.h` and in the
replica's package comment.

`make -C shim test-tsan` is clean on the new CAS loop.

## Prediction for the live RTX 3090 run

Derived from the schedule, before any run. `cmd/gpu-cuda-profile` defaults: `iters=2000`
→ **4000 launches**, `period=8`, default seed. The workload's loop is
`launch_axpy(); launch_scale();`, and the adapter's `on_launch` fires only on kernel-launch
cbids with no warm-up kernel, so **even ordinal = `perfagent_axpy`, odd = `perfagent_scale`**.

Sampler level — exact, not approximate:

| quantity | prediction |
|---|---:|
| adapter `sampled=` (and `expected_sampled=`) | **505** |
| of which even ordinal → `perfagent_axpy` | **255** |
| of which odd ordinal → `perfagent_scale` | **250** |
| adapter line | `sample_period=8 sample_seed=0x9e3779b97f4a7c15` |

Profile level: the run that produced `gpu-cuda-45.pb.gz` yielded 257 stack-carrying
launches out of a nominal 500 sampled — roughly half — for reasons outside this change
(walk/symbolize/eviction attrition), and I have no way to predict that yield here. So the
robust prediction is the **split**, which is what issue #50 is about:

- `perfagent_scale` must have a **non-zero** stack count. Under the old sampler it was
  exactly 0, permanently.
- The two kernels' stack counts should sit near **50.5% / 49.5%** (255:250). If the run
  reproduces the same ~257 total stack yield, expect roughly **130 axpy / 127 scale**.
- Anything near 100/0 in either direction means the fix did not take.
- GPU-time totals and the join and conservation invariants should be **unchanged** — this
  change touches attribution only.

Re-derive for any other `-iters` with
`gpuabi.SampledCount(iters*2, period, gpuabi.DefaultSampleSeed)`, or by splitting
`gpuabi.SampleSchedule(...)` on ordinal parity.

## Files changed

| file | change |
|---|---|
| `shim/core/sampler.h` | the jittered schedule, `gap_at`, the seed, the CAS claim loop; header rewritten |
| `shim/core/sampler_test.cc` | the regression test, the phase sweep, rate, determinism, seed, gap band, thread-independence, schedule pin |
| `internal/gpuabi/sampler.go` | new: the Go replica of the schedule |
| `internal/gpuabi/sampler_test.go` | new: the shared pin plus the consumer-side properties |
| `shim/core/usdt_abi.h` | `sample_period` is the mean stride (comment only; layout untouched) |
| `shim/stub/stub.cc` | reports `seed=` alongside `period=` |
| `shim/nvidia/cupti_adapter.cc` | `PERFAGENT_GPU_SAMPLE_SEED`, logs the seed |
| `gpu/types.go` | `SamplePeriod` doc |
| `gpuprobe/gate_test.go` | 63 → 58; both-kernels assertion; seed assertion |
| `cmd/gpu-cuda-profile/main.go` | exact `expected_sampled` from the replica; flag validation |
| `cmd/gpu-stub-profile/main.go` | same, and it would otherwise have hung on its deadline |

## Verification run here

```
make -C shim && make -C shim test        # batch/clock/drain/usdt_abi/sampler/probe_args all OK
make -C shim test-tsan                   # clean
make -C shim nvidia                      # the CUPTI adapter still builds
make -C shim check-fpless                # OK
go build ./... && go vet ./...           # clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1   # all ok
golangci-lint run --timeout=5m           # 0 issues
./shim/perfagent-gpu-stub 500 0 8 0
  -> stub: launches=500 observed=500 sampled=58 period=8 seed=0x9e3779b97f4a7c15 ...
```

`TestStubDrivesThePipelineToPprofWithoutAGPU`: **SKIP** (CapEff 0). No CUDA run.
