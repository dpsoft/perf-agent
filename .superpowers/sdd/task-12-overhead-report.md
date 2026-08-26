# Task 12 — Overhead: what to measure, against what, and what sinks Tier A

Branch `feat/pc-sampling-overhead`, one commit on `origin/main` (`6f7cdf08`, the Task 11 merge).

**This task produced the instrument, not the verdict.** The implementer has `CapEff: 0` and no
GPU. Every number the plan asks for is outstanding and **the tier decision is not yet made**. What
is here is a `bench/` scenario built so that running it on the RTX 3090 yields a decision rather
than a discussion, plus the offline half of its own correctness: the parsers, the medians, the
ratio and all four pre-committed threshold clauses are unit-tested, and the skip path is
exercised.

No Go file under `gpu/` or `gpuprobe/` changed, and nothing under `shim/` changed except the new
`nvidia-concurrent` target in `shim/Makefile` and the new `.cu` it builds. Tier A, Tier B, tier
selection, the adapter and the ABI are untouched; no `.o` churn.

---

## The workload, and why it is a second one

`shim/nvidia/testdata/cuda_concurrent.cu`, new, built by `make -C shim nvidia-concurrent` with the
same `-O2 -g -lineinfo -arch=sm_86 -rdynamic` as `cuda_workload`.

**I added a second workload rather than extending `cuda_workload.cu`, and the reason is the
measurement rather than tidiness.** `cuda_workload.cu` is a two-kernel *serial* loop on the
default stream with a `usleep` between iterations; its kernels are 64k elements of one FMA each.
That shape is exactly right for what it exists for — it is the fixture the adapter, the join, the
kernel-name table and the `-lineinfo` source resolution are all proven against, and the hardware
half of the phase gate asserts against **its** source lines. Changing its shape would change what
those gates measure.

It is also the wrong instrument here, for the reason the plan states: a stream of trivial kernels
exaggerates per-launch costs and **understates serialization costs**, because serialization hurts
in proportion to the concurrency it destroys and a serial loop has none. A Tier A number measured
on it would be small for the same reason a stopped clock is right twice a day.

The new workload:

- **S streams** (default 4), created `cudaStreamNonBlocking` — spelled out rather than defaulted,
  because the default (blocking) flag makes every stream serialize against the legacy default
  stream and the workload would then have no concurrency at all, which is precisely the failure
  the file exists to avoid;
- **non-trivial kernels.** One kernel is `rounds` dependent FMAs up then `rounds` dependent FMAs
  down. The duration comes from the *dependency chain*, not from a large grid — deliberately, so
  the kernel occupies a fraction of the device (16 blocks × 256 threads by default on an 82-SM
  GA102) and several kernels genuinely co-reside. A big-grid kernel would saturate the device and
  leave nothing for a second stream to overlap with, quietly turning this back into the serial
  workload;
- **fixed work**, not fixed time: `iters × streams` kernels, whatever it takes;
- a **warm-up phase outside the timed region**, so module load, JIT, first-touch allocation and
  the adapter's own steady state are not charged to the measurement;
- a **device sync every `sync_every` iterations**, which is not housekeeping: it bounds queue
  depth and forces the pipeline to **refill** after every drain. Refill cost is exactly what makes
  Tier A's damage outlast its burst, which is what the cost-over-duty ratio exists to detect;
- an **exact** self-check. Every kernel is the identity on its buffer (`+1` × rounds then `−1` ×
  rounds, with `rounds ≤ 2^23` so every partial sum is exactly representable in float), so
  `max_abs_err` must be **exactly 0.0**, not "small". An arm that perturbed the computation fails
  the run instead of contributing a number.

**The loops survive the compiler, and that is checked rather than assumed.** `up` and `down` are
runtime kernel arguments and `#pragma unroll 1` blocks unroll-and-reassociate. Verified offline
with `cuobjdump -sass`: the SASS for `_Z20perfagent_conc_chainPfiiff` contains exactly **two
`FFMA`s, each inside its own backward branch** — two rolled loops of one dependent FMA. Nothing
was folded into a closed form, and `cuobjdump -elf` shows the `-lineinfo` `debug_line` sections
are present.

**The realism is measured, not claimed.** The harness computes, from the profile the adapter
produces:

```
concurrency  = Σ(exec EndNs − StartNs) / (max EndNs − min StartNs)
meanKernelUs = Σ(exec EndNs − StartNs) / count
```

and **fails the whole run** if the *baseline* arm's concurrency is below `--gpu-min-concurrency`
(1.5) or its mean kernel duration below `--gpu-min-kernel-us` (50). A workload that turned out to
be serial, or whose kernels turned out to be microseconds long, would otherwise produce a small,
green, meaningless Tier A cost — the benchmark-shaped instance of this project's standing defect.
Both spans are taken over the executions the profile *retained*, so ring eviction cannot skew the
ratio: numerator and denominator describe the same interval.

---

## The baseline, and what it is not

**The baseline arm is the shipping Phase 4 configuration with PC sampling off** — shim injected
via `CUDA_INJECTION64_PATH`, RUNTIME + RESOURCE callbacks, `CONCURRENT_KERNEL` activity, 100 ms
drain, launch sampling, and a `gpuprobe` consumer attached so the probe semaphores are armed and
the producer actually does its work. §9.1 already measured injection at 0.0% and the activity path
at −10.0%; those costs are paid whether or not PC sampling is on, so charging them to PC sampling
would be measuring the wrong thing twice.

An **uninjected** run is taken as well, once, *before* the arms. It is labelled `calibration` in
the output and in the JSON, it is never used as a baseline, and it exists for two reasons: it
warms the device clocks before the first arm, and it proves the fixed work is sized for the device
in front of it (the run fails, with the retuning advice, if it falls outside 10–120 s).

The consumer runs **without a symbolizer**, identically on every arm. Resolving a sampled launch's
stack is agent-side work that costs the workload nothing, and it cannot succeed anyway once the
workload has exited. The producer still samples launches and still captures stacks in BPF on every
arm, exactly as the shipping configuration does; only the agent-side resolution — a variance
source that cancels — is left out.

---

## The arms, and what each proves it ran

Five arms, **five interleaved runs each** (every arm once per round, not five of one then five of
the next, so thermal drift and boost state fall on all arms alike), medians, fixed work.

| arm | env handed to the producer | duty |
| --- | --- | ---: |
| baseline | `PERFAGENT_GPU_PC_SAMPLING=off` | — |
| tier B continuous | `=continuous` | — |
| tier A 50 ms / 450 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=100`, `MAX_GAP_MS=450` | 10% |
| tier A 50 ms / 950 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=50`, `MAX_GAP_MS=950` | 5% |
| tier A 50 ms / 1950 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=25`, `MAX_GAP_MS=1950` | 2.5% |

**The gaps are pinned from both sides, and that is deliberate.** The burst controller clamps its
gap into `[min_gap, max_gap]` where `min_gap = burst × (1/max_duty − 1)` — so 450 / 950 / 1950 ms
after a 50 ms burst *are* 10% / 5% / 2.5%. Setting the ceiling gives the minimum; setting
`MAX_GAP_MS` to the same value gives the maximum; the interval collapses to a point and whatever
the closed loop computes, the gap it returns is the arm's gap. With only the ceiling set, a
workload producing a high pair rate would have the loop lengthen the gap, the arm would run at a
duty nobody asked for, and the ratio would be divided by a denominator that never happened. The
tier is written **explicitly on every arm including the off one**, so an exported
`PERFAGENT_GPU_PC_SAMPLING` in the operator's shell cannot turn the baseline into a serializing
arm and make every other arm look free.

**Each arm proves it ran in the mode it claims, from two ends that can disagree.** The producer's
own report line on stderr (`PERFAGENT_GPU_LOG=stderr`) and the consumer's counters are parsed
separately and both are recorded in the JSON, because an arm proved only by the consumer would
look identical whether the producer ran in the right mode and lost records or ran in the wrong
mode and had nothing to send.

Every clause below **fails the run** when it does not hold:

| arm | asserted |
| --- | --- |
| *all* | `ExecutionsSeen > 0` and the adapter printed a report line at all — an arm where the pipeline never ran satisfies every negative clause below perfectly and would be the cheapest arm in the table |
| baseline | producer `tier=off`; `PCSamplesDecoded == 0`; `SamplingWindowsDecoded == SamplingWindowsReceived == 0`; `bursts == 0`; `ExecutionsSerialized == 0` |
| tier B | producer `tier=continuous`; `pc_records > 0`; `PCSamplesDecoded > 0`; **zero** windows and **zero** bursts (a `CONTINUOUS` producer announcing a window would be claiming a perturbation it did not cause) |
| tier A | producer `tier=serialized`; `bursts ≥ --gpu-min-bursts` (4); `windows == 2N` or `2N−1`; `start_failed == stop_failed == 0`; `graph_execs == graph_refused == 0`; `PCSamplesDecoded > 0`; `SamplingWindowsReceived > 0`; `Snapshot.PCSampling == serialized`; and **`ExecutionsSerialized > 0`** |
| tier A | achieved duty inside `[configured/2, (burst+2·tick)/(burst+2·tick+gap)]` — the upper bound is *derived* from the burst timer's `burst/5` tick plus two ticks of scheduling slack, not a magic tolerance |
| cross-arm | a lower duty must open **strictly fewer** bursts than a higher one over the same fixed work |

The two load-bearing ones are the last two rows and the `ExecutionsSerialized > 0` clause. Bursts
that overlapped no kernel serialized nothing, so an arm with zero serialized executions measured
the cost of starting and stopping CUPTI and not the cost of serialization — and would report a
beautifully small number for it. And if the three Tier A arms all opened the same number of
bursts, the duty environment did not take, they are one arm under three names, and their three
different ratios are fiction.

On failure the run still writes its JSON — the arms are the diagnostic — but the `decision` object
stays at its zero value so nothing in the file can be read as a verdict, stderr says so in as many
words, and the exit code is 3.

---

## The threshold logic

`cost% = (arm median − baseline median) / baseline median × 100`, from the workload's own
fixed-work `elapsed_ms` (which excludes CUDA init, allocation and warm-up; the whole-process time
is recorded beside it, because a divergence between the two would mean the cost moved into
startup where the fixed-work number would hide it).

`cost ÷ duty` is computed against the **configured** duty, and that is a choice, not arithmetic.
It is the number the thresholds name ("Tier A at 10% duty"), and it is the conservative one: the
achieved duty can only overshoot the configured one (the burst timer's granularity rounds bursts
up, never down), so dividing by the configured value can only make the ratio **larger** —
pessimistic for Tier A, never flattering. The achieved duty is reported beside it and separately
asserted to be inside the derived bound.

The four clauses are evaluated **independently** and every one that fires is recorded, because
more than one can fire and the combination is itself information. The verdict is then resolved by
severity, stated explicitly rather than left to the order the code happens to be written in:

```
TIER_A_UNSHIPPABLE  >  TIER_A_DEEP_DIVE_ONLY  >  TIER_A_SHIPS_AT_A_SMALLER_DUTY  >  TIER_A_SHIPS_OPT_IN
```

| clause id | fires when | verdict it argues for |
| --- | --- | --- |
| `tier-b-cost-over-5pct` | tier B cost > 5% | `TIER_B_NOT_ALWAYS_ON` |
| `tier-a-10pct-duty-within-5pct-and-ratio-within-2` | headline duty within both bars | `TIER_A_SHIPS_OPT_IN` |
| `tier-a-cost-over-duty-above-2-at-every-duty` | ratio > 2 at every duty tested | `TIER_A_DEEP_DIVE_ONLY` |
| `tier-a-2.5pct-duty-cost-over-5pct` | lowest duty still over 5% | `TIER_A_UNSHIPPABLE` |

**Two verdicts the plan does not name, and why they exist rather than a silent pick:**

- `TIER_A_SHIPS_AT_A_SMALLER_DUTY` — the headline duty is over the wall-clock bar but a smaller
  one is within **both** bars. Duty-cycling works here; it just needs turning down. The harness
  names the largest qualifying duty. Promoting this to `SHIPS_OPT_IN` would claim a duty that was
  measured over budget; demoting it to `DEEP_DIVE_ONLY` would discard a tier that is demonstrably
  tunable.
- `TIER_A_INDETERMINATE` — nothing qualifies at any duty and neither harsh clause fired, which
  means cost does not fall with duty the way serialization says it must, i.e. the measurement is
  unsound. It prints *"do not read this as a pass"*. It is a named outcome for the same reason
  `gpu_serialized` has an `"unknown"` that must never degrade to `"false"`.

### A finding about the thresholds themselves

`cost ÷ duty > 2` is the same statement as `cost% > 200 × duty`. At **2.5% duty that is exactly
`cost% > 5%`** — the wall-clock bar. So on the plan's own three duties, clause 3 **strictly
implies** clause 4, and `TIER_A_DEEP_DIVE_ONLY` is unreachable: the unshippable verdict always
outranks it. The two clauses separate only below 2.5% duty.

This is a property of the thresholds, not of the hardware, and it is not something I resolved by
adjusting a threshold. The harness applies the clauses as written, and **prints the coincidence
whenever both fire**, so a controller reading a result does not conclude the deep-dive branch was
considered and rejected:

> `NOTE both harsh clauses fired, and at the lowest duty tested (2.50%) they are the SAME
> condition: cost/duty > 2.0 means cost > 5.00%, and the wall bar is 5.0%. They separate only
> below 2.50% duty, which this table does not test — so TIER_A_DEEP_DIVE_ONLY could not have been
> reached here whatever the numbers. Add a lower-duty arm if that distinction is wanted.`

The reassuring half is pinned too: on the plan's duties the decision is **total**. At 2.5% duty
"within the wall bar" and "within the ratio bar" are the same condition, so the lowest-duty arm
either qualifies for both — giving opt-in or a smaller duty — or fails both, giving unshippable.
`TestOnThePlansDutiesTheDecisionIsTotal` sweeps eight cost tables and asserts none of them reaches
`INDETERMINATE`.

Whether to add a 1%-duty arm to make clause 3 distinguishable is a decision for whoever runs this.
I did not add one: the plan's arm table is what was pre-committed, and quietly extending it would
be the same category of move as quietly moving a bar.

---

## The exact command for the RTX 3090

```bash
cd /home/diego/github/perf-agent            # on the branch feat/pc-sampling-overhead
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release

make -C shim nvidia nvidia-concurrent
make bench-build
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario

make bench-gpu-pc-overhead
```

Equivalently, by hand:

```bash
./bench/cmd/scenario/scenario --scenario gpu-pc-overhead --runs 5 \
    --out bench-gpu-pc-overhead.json
```

`cap_sys_admin` is **not** in that set and must not be added: the capability constraint is
`cap_bpf,cap_perfmon,cap_checkpoint_restore`, and `getcap` on the binary showing no
`cap_sys_admin` is the standing Phase 1 assertion. Do not put the binary in `/tmp` — that mount is
`nosuid` and file caps do not survive exec.

**Expect roughly 26 fixed-work runs** (one calibration + 5 arms × 5 rounds). At the default sizing
each is ~20–30 s plus attach and drain, so budget 15–25 minutes.

**If the calibration pass says the work is mis-sized**, retune and re-run — the failure message
names the flags. `--gpu-rounds` sets kernel duration (the dependency chain length);
`--gpu-iters` sets how many. If the baseline arm fails the concurrency floor, raise
`--gpu-streams` or lower `--gpu-blocks` so each kernel occupies less of the device and more of
them co-reside.

**Reading the result:** the table, the ratios, the `THRESHOLD FIRED` lines and the two `DECISION`
lines are printed to stdout and the whole thing is in the JSON under `gpu_pc_overhead`. Exit 0
means the measurement completed — *whatever the verdict*; an honest `TIER_A_UNSHIPPABLE` is a
successful run of this benchmark. Exit 3 means an arm could not prove what it measured, and then
the numbers are **not** a tier decision.

When the numbers exist, record them in
`docs/superpowers/plans/2026-08-25-gpu-pc-sampling.md` under Task 12 and apply the Tier A verdict
to Task 11's defaults, per the phase gate.

---

## Verification actually run

```
go build ./... && go vet ./...                          clean
go test ./... -count=1                                  all pass
~/go/bin/golangci-lint run --timeout=5m                 0 issues
make -C shim nvidia-concurrent                          exit 0 (nvcc 13.3, sm_86)
./bench/cmd/scenario/scenario --scenario gpu-pc-overhead
    BENCH_SKIPPED: missing required capabilities (CAP_BPF, CAP_PERFMON,
    CAP_CHECKPOINT_RESTORE); run: sudo setcap ...
    exit 0, no output file written
```

The skip is clean: exit 0, one `BENCH_SKIPPED` line, and **no JSON file** — a partial file with no
numbers in it is the thing most likely to be mistaken for a result. The other scenarios' skip
behaviour is unchanged (`--scenario pid-large` still reports the larger capability set).

The capability message names `CAP_BPF, CAP_PERFMON, CAP_CHECKPOINT_RESTORE` and deliberately not
`CAP_SYS_ADMIN`: this scenario needs gpuprobe's own set, and checking for the larger one that
`bench/cmd/scenario`'s other scenarios need would skip on a correctly-capped machine — a skip for
the wrong reason being indistinguishable from a skip for the right one.

Static checks on the workload, offline:

```
cuobjdump -sass  → 2 FFMA, each inside its own backward branch (two rolled loops
                   of one dependent FMA; nothing folded, nothing unrolled)
cuobjdump -elf   → debug_line sections present (-lineinfo took)
argument validation: unknown flag, --rounds past 2^23, and a zero dimension all
                   exit 2 with the reason, before any CUDA call
```

New tests, all offline, all in `bench/cmd/scenario/gpupc_test.go` (45 cases):

- **the four clauses**, each fired and each *not* fired, including exactly-at-the-bar (the plan
  says `> 5%`, so 5.0 does not fire — a strict-versus-non-strict slip is the classic way a
  pre-committed threshold quietly becomes a different one);
- the every-duty ratio clause is **universal, not headline** — one arm inside the bar stops it;
- the harsher verdict wins when clauses co-fire, and the coincidence is reported;
- the decision is total on the plan's duties;
- the recorded bars are the committed ones;
- **the medians and the ratio**: cost against the baseline, ratio against the configured duty, the
  baseline not a cost against itself, and `medianFloat` not reordering its input;
- **the parsers**: a Tier A report, a Tier B report, an off report, and — the one that matters —
  a producer that printed nothing leaves the tier **empty**, never `"off"`, because "the adapter
  was loaded with sampling off" and "the adapter was never loaded" must not look the same;
- **the arm assertions**, each shown red: an arm that measured nothing, a baseline that was
  sampling, a Tier A arm that serialized nothing, one with too few bursts, one whose windows do
  not reconcile with its bursts, one at the wrong duty (and one legitimately overshooting, which
  must pass), one in a CUDA-graph process, a Tier B arm that emitted a window, a Tier B arm that
  sampled nothing;
- **the workload guards**: a serial baseline and a trivial-kernel baseline both refused, a
  realistic one accepted; three Tier A arms with equal burst counts refused;
- **the concurrency measurement**: fully overlapping kernels → 4, serial kernels → 1, an empty
  snapshot → 0 rather than NaN (NaN would sail straight past the floor comparison), degenerate
  intervals ignored;
- **the arm table itself**: the duties are the plan's, and each gap equals the adapter's own
  `burst × (1/max_duty − 1)` — if either side ever changes, this fails;
- **the environment**: every arm names its tier explicitly, the Tier A arms pin the gap from both
  sides, and the two non-Tier-A arms carry no burst environment at all;
- **all four skip branches**, reachable via an injected-predicate variant, since only the first
  can ever fire on this machine.

---

## Cannot verify — every number is outstanding

`CapEff: 0`, no GPU on this machine. **No arm of this benchmark has ever been executed.** No CUDA
kernel in `cuda_concurrent.cu` has ever run; it has been compiled and disassembled, nothing more.

**The tier decision is not yet made, and no part of it may be made from this file.**

Outstanding, all of it:

1. **Every number in the arm table.** Baseline wall-clock, Tier B's cost, Tier A's cost at 10%, 5%
   and 2.5% duty, every achieved kernel throughput, every cost ÷ duty ratio. None exists.
2. **Which threshold fires, and therefore whether Tier A ships at all**, and whether Tier B
   remains a candidate for always-on.
3. **Whether the workload is actually concurrent on a 3090.** The design argues 16 blocks × 256
   threads leaves room for four kernels to co-reside on 82 SMs, and the SASS confirms the
   dependency chain is real, but the achieved concurrency is a hardware measurement. The harness
   asserts a floor rather than assuming it, so a negative answer is a loud failure with retuning
   advice and not a quiet underestimate — but the answer is unknown.
4. **Whether the default sizing lands in the 10–120 s window** on a 3090. `--gpu-rounds 64000`
   was estimated from a dependency-chain latency guess, not measured. The calibration pass exists
   because this is unknown.
5. **Whether pinning `MAX_DUTY_PERMILLE` and `MAX_GAP_MS` to the same gap really pins the burst
   controller.** It follows from `burst_next_gap_ns`'s clamp read as source, and
   `core/burst_test.cc` proves the clamp against a fake clock, but this exact combination has
   never been given to the adapter on hardware. The achieved-duty assertion is what would catch
   it.
6. **Whether the adapter's report line parses as expected on a real run.** The parser was written
   against the `logf` format strings in `cupti_adapter.cc` and tested against hand-written
   fixtures reproducing them. A format drift would show up as an empty producer tier, which is a
   failing assertion rather than a silent zero — but it has not been read off a real process.
7. **Whether 25 sequential `gpuprobe.Attach`/`Close` cycles are clean** — the enrollment
   rendezvous binding and unbinding twenty-five times in one process has not been exercised.
8. **Whether the baseline arm is stable enough for a 5% bar to be meaningful.** The spread across
   the five runs is recorded per arm for exactly this reason: a median whose spread is larger than
   the effect is not a measurement. Whether that holds on the lab machine is unknown.
9. Everything Tasks 6, 10 and 11 could not verify remains unverified and is unchanged by this
   task. In particular Task 10's item 2 — whether the sampling windows and the converted activity
   timestamps really land in the same clock domain closely enough for the intersection to mean
   what it says — is load-bearing *here* as well: the `ExecutionsSerialized > 0` assertion and the
   serialized/not-serialized split in the arm evidence both rest on it.

Not verifiable at all, on hardware or otherwise: MPS and cross-process contention for the sampling
hardware, which would perturb any of these arms invisibly.
