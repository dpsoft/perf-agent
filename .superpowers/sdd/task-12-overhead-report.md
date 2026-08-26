# Task 12 — Overhead: what to measure, against what, and what sinks Tier A

Branch `feat/pc-sampling-overhead`, one commit on `origin/main` (`6f7cdf08`, the Task 11 merge).

> **Amended on branch `feat/overhead-1pct-arm`.** A sixth arm — Tier A at 50 ms / 4950 ms, 1%
> duty — was added so that `TIER_A_DEEP_DIVE_ONLY` becomes reachable; on the original five arms it
> could not fire without `TIER_A_UNSHIPPABLE` outranking it. **The thresholds are untouched.** What
> changed besides the arm is the **run length**, which is now derived from the arm table rather
> than guessed, and the duty at which the plan's fourth clause is evaluated. Both are set out
> below and in **"The 1%-duty arm"** at the end. Still no number has been measured.

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
in front of it (the run fails, with the retuning advice, if it falls outside 25–120 s — the
lower bound is derived from the lowest-duty arm's burst cycle, not a constant).

The consumer runs **without a symbolizer**, identically on every arm. Resolving a sampled launch's
stack is agent-side work that costs the workload nothing, and it cannot succeed anyway once the
workload has exited. The producer still samples launches and still captures stacks in BPF on every
arm, exactly as the shipping configuration does; only the agent-side resolution — a variance
source that cancels — is left out.

---

## The arms, and what each proves it ran

Six arms, **five interleaved runs each** (every arm once per round, not five of one then five of
the next, so thermal drift and boost state fall on all arms alike), medians, fixed work.

| arm | env handed to the producer | duty |
| --- | --- | ---: |
| baseline | `PERFAGENT_GPU_PC_SAMPLING=off` | — |
| tier B continuous | `=continuous` | — |
| tier A 50 ms / 450 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=100`, `MAX_GAP_MS=450` | 10% |
| tier A 50 ms / 950 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=50`, `MAX_GAP_MS=950` | 5% |
| tier A 50 ms / 1950 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=25`, `MAX_GAP_MS=1950` | 2.5% |
| tier A 50 ms / 4950 ms | `=serialized`, `BURST_MS=50`, `MAX_DUTY_PERMILLE=10`, `MAX_GAP_MS=4950` | 1% |

**The gaps are pinned from both sides, and that is deliberate.** The burst controller clamps its
gap into `[min_gap, max_gap]` where `min_gap = burst × (1/max_duty − 1)` — so 450 / 950 / 1950 /
4950 ms after a 50 ms burst *are* 10% / 5% / 2.5% / 1%. Setting the ceiling gives the minimum;
setting `MAX_GAP_MS` to the same value gives the maximum; the interval collapses to a point and
whatever the closed loop computes, the gap it returns is the arm's gap. The 1% arm is pinned by
the same two knobs and the same tests: `TestTierAArmsPinTheGapFromBothSides` now sweeps
`{100, 50, 25, 10}` per mille, and `TestTheArmTableIsThePlansAndItsDutiesAreExact` re-derives
every gap from `burst × (1/duty − 1)`, so neither side can drift without the other noticing. With only the ceiling set, a
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
| cross-arm | a lower duty must open **strictly fewer** bursts than a higher one over the same fixed work — the check walks the whole table in descending duty, so the 1% arm is held to it on the same terms as the rest |

The two load-bearing ones are the last two rows and the `ExecutionsSerialized > 0` clause. Bursts
that overlapped no kernel serialized nothing, so an arm with zero serialized executions measured
the cost of starting and stopping CUPTI and not the cost of serialization — and would report a
beautifully small number for it. And if the Tier A arms all opened the same number of bursts, the
duty environment did not take, they are one arm under several names, and their different ratios
are fiction.

**The 1% arm is the one most exposed to both of those, which is why nothing about it is relaxed.**
It opens the fewest bursts of any arm, so it is the arm most likely to fall under the
`--gpu-min-bursts` floor; and its bursts overlap the least kernel time, so it is the arm most
likely to serialize nothing at all. Both of those failures produce a *small* cost number — the
flattering direction — and both fail the run instead of contributing one. The run-length floor
below exists precisely so the first of them is prevented rather than merely detected.

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
| `tier-a-lowest-duty-cost-over-5pct` | lowest duty tested still over 5% | `TIER_A_UNSHIPPABLE` |

The fourth identifier used to read `tier-a-2.5pct-duty-cost-over-5pct`, because 2.5% was the
lowest duty on the table. It is now named for what it evaluates rather than for a duty that is no
longer the lowest — see **"Where the plan's fourth clause is evaluated"** below, which is a real
loosening and is disclosed in the output rather than absorbed.

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

### The finding about the thresholds themselves, and what was done about it

`cost ÷ duty > 2` is the same statement as `cost% > 200 × duty`. At **2.5% duty that is exactly
`cost% > 5%`** — the wall-clock bar. So on the plan's own three duties, clause 3 **strictly
implies** clause 4, and `TIER_A_DEEP_DIVE_ONLY` is unreachable: the unshippable verdict always
outranks it. The two clauses separate only below 2.5% duty.

This is a property of the thresholds, not of the hardware, and it was not resolved by adjusting a
threshold. **It was resolved by adding a duty below the coincidence point** — see "The 1%-duty
arm" below. On the original five arms the harness applied the clauses as written and **printed the
coincidence whenever both fired**, so a controller reading a result would not conclude the
deep-dive branch had been considered and rejected:

> `NOTE both harsh clauses fired, and at the lowest duty tested (2.50%) they are the SAME
> condition: cost/duty > 2.0 means cost > 5.00%, and the wall bar is 5.0%. They separate only
> below 2.50% duty, which this table does not test — so TIER_A_DEEP_DIVE_ONLY could not have been
> reached here whatever the numbers. Add a lower-duty arm if that distinction is wanted.`

That note still exists and is still reachable — if the table is ever trimmed back to 2.5%, the
coincidence returns and the reader is told. With the 1% arm present the harness prints the other
half instead, and it is a different statement rather than a milder one:

> `NOTE both harsh clauses fired and at the lowest duty tested (1.00%) they are DIFFERENT
> conditions — cost/duty > 2.0 means cost > 2.00% there, well inside the 5.0% wall bar. This table
> separates them below 2.50% duty, so TIER_A_DEEP_DIVE_ONLY was reachable and was not reached: the
> lowest duty is over the wall bar on its own.`

The reassuring half was pinned too: on the plan's **original three** duties the decision is
**total**. At 2.5% duty "within the wall bar" and "within the ratio bar" are the same condition, so
the lowest-duty arm either qualifies for both — giving opt-in or a smaller duty — or fails both,
giving unshippable. `TestOnThePlansDutiesTheDecisionIsTotal` sweeps eight cost tables and asserts
none of them reaches `INDETERMINATE`.

**That totality is the same fact as the unreachability, and the 1% arm trades it away
deliberately.** Below the coincidence point the two bars separate, so a cost table can now fail
both without either harsh clause firing, and `TIER_A_INDETERMINATE` becomes reachable on the
harness's own duties. That is not a regression: `INDETERMINATE` says *cost did not fall with duty
the way serialization says it must, so re-run before deciding anything; do not read this as a
pass*, which is the correct thing to say about such a table.
`TestOnTheHarnessDutiesIndeterminateIsReachableAndIsNotAPass` pins it, and the original
three-duty totality test is kept unchanged beside it.

---

## The 1%-duty arm

**Tier A at 50 ms burst / 4950 ms gap.** `min_gap = 50 × (1/0.01 − 1) = 4950 ms`, so
`MAX_DUTY_PERMILLE=10` and `MAX_GAP_MS=4950` clamp the burst controller's interval to a single
point, exactly as the other three Tier A arms do.

### Why it exists

`cost ÷ duty > 2` is the same statement as `cost% > 200 × duty`, so the deep-dive clause and the
wall-clock clause are the *identical condition* at `duty = 5 / (100 × 2) = 2.5%` — the lowest duty
the original table tested. Above that duty the ratio clause strictly implies the wall clause and
the harsher verdict outranks it; `TIER_A_DEEP_DIVE_ONLY` could not fire alone whatever the numbers
were. The two verdicts mean materially different things — *"the tier does not ship"* versus *"the
tier works, but only as a deliberate tool"* — so a harness that could only ever produce the first
was applying three clauses, not four.

At 1% duty, `ratio > 2` means `cost > 2%`, comfortably inside the 5% bar. A tier that is
consistently inefficient but genuinely cheap at low duty now lands in deep-dive instead of being
killed.

Worked example, and the test that pins it
(`TestTheOnePercentArmMakesDeepDiveOnlyReachable`): costs of 22% / 11% / 5.5% / 3% at
10% / 5% / 2.5% / 1% give ratios 2.2 / 2.2 / 2.2 / 3.0 — every one above the bar, so the ratio
clause fires — while the lowest duty costs 3%, inside the wall bar, so the unshippable clause does
not. Verdict: `TIER_A_DEEP_DIVE_ONLY`. The same test feeds the same three costs to the original
three duties and asserts they still land on `TIER_A_UNSHIPPABLE`, so the finding is recorded
rather than erased.

### Where the plan's fourth clause is evaluated — a real loosening, disclosed

The plan words its fourth clause *"Tier A at 2.5% duty > 5% wall-clock"*, and gives as its reason
that *"duty-cycling has no remaining lever"*. The lever it names is the lowest duty on the table.
With a 1% arm present that is no longer 2.5%, so the clause is evaluated at 1% — which is what its
own reason asks for, and which is what the worked example above depends on.

**This makes the clause strictly weaker than its literal wording.** A table where the 2.5% arm is
over the bar and the 1% arm is not now escapes `TIER_A_UNSHIPPABLE`, where before it could not.
That is exactly the kind of change that must not happen quietly, so it does not:

- the clause identifier is now `tier-a-lowest-duty-cost-over-5pct`, naming what it evaluates
  rather than a duty that is not the one being tested;
- whenever the difference changes the answer — the 2.5% arm over the bar, the lowest arm not — the
  harness prints, above the verdict:

  > `NOTE the plan words its fourth clause as "Tier A at 2.5% duty > 5.0%", and the tier A 2.5%
  > duty arm DOES cost +5.50%. It is evaluated at the lowest duty tested (tier A 1% duty, +3.00%)
  > because the clause's own reason is that duty-cycling has no remaining lever, and 1.0% duty is
  > a remaining lever. Read this result as "unshippable does not fire at 1.0% duty", NOT as "2.5%
  > duty is within budget".`

- `TestThePlansLiteralFourthClauseIsReportedWhenItWouldHaveFired` pins the note, and
  `TestNoLiteralClauseNoteWhenThe25PercentArmIsWithinTheBar` pins that it stays silent when there
  is nothing to disclose — a note that fires unconditionally is a note nobody reads.

### What it costs: run length, and that is the whole cost

**A 1% duty at a 50 ms burst is a 4950 ms gap, so one burst opens every 5 seconds.** Every Tier A
arm must open at least `--gpu-min-bursts` (4) of them or its duty fraction means nothing, and the
2.5% arm needed only 8 s of fixed work for that. The 1% arm needs **20 s**, and the harness demands
**25 s** — one extra cycle, because the count is of *completed* cycles, the first burst does not
open at `t=0`, and a floor met only exactly would turn ordinary jitter into a failed run of the
whole benchmark after twenty minutes of GPU time.

That floor is **derived from the arm table**, not written down: `gpuPCMinFixedWorkSec` takes the
longest burst+gap cycle among the Tier A arms and multiplies by `MinBursts + 1`. Trimming or
adding an arm moves it automatically, and `TestTheFixedWorkFloorIsDerivedFromTheLowestDutyArm`
pins both the cycle and the product. `--gpu-min-calibration-sec` is now a floor that is *raised*
to the derived value and never lowered below it.

Two things make the check conservative in the right direction rather than the flattering one. It
is applied to the **uninjected calibration run**, which is the fastest run the benchmark takes —
every arm is slower and therefore opens *more* bursts. And it is applied to the workload's
fixed-work `elapsed_ms`, while the adapter's burst timer runs for the whole process, including
CUDA init, warm-up and teardown.

It is also checked **before** anything runs. The harness prints the derived floor and the run
count up front, and a configuration where the floor exceeds `--gpu-max-calibration-sec` is refused
immediately rather than discovered after the GPU time is spent
(`TestAnImpossibleFixedWorkWindowIsRefusedBeforeAnythingRuns`).

### The consequence for the operator, stated plainly

**The run gets materially longer, in two ways at once: five more fixed-work runs, and each run has
to be about half as long again.**

- **26 → 31 fixed-work runs** (1 calibration + 6 arms × 5 rounds).
- **The fixed work must now take 25–120 s uninjected, not 10–120 s.** The old default of
  `--gpu-iters 20000` was *estimated* at ~20–30 s, which straddles the new 25 s floor, so the
  default is now `--gpu-iters 30000` — estimated at ~30–45 s. Both figures are estimates from a
  dependency-chain latency guess, not measurements; the calibration pass is what turns a wrong
  guess into a named retuning instruction, and it now names the `--gpu-iters` value to try.
- **Budget 25–40 minutes** rather than the 15–25 the original five-arm table quoted. If that is
  too long, `--gpu-min-bursts 4` and the 1% arm are the two knobs — but lowering the burst floor
  buys time by making the lowest arm's duty mean less, which is the trade the floor exists to
  refuse by default.

---

## The exact command for the RTX 3090

```bash
cd /home/diego/github/perf-agent            # on the branch feat/overhead-1pct-arm
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

**Expect 31 fixed-work runs** (one calibration + 6 arms × 5 rounds). The harness prints that count
and the derived fixed-work floor before it runs anything:

```
gpu-pc-overhead: 6 arms x 5 interleaved runs + 1 calibration = 31 fixed-work runs; the fixed
work must take 25-120 s (the 25 s floor is derived: the lowest-duty arm's 5.0 s cycle x 4
bursts + one cycle of slack)
```

At the default sizing (`--gpu-iters 30000`) each run is an estimated ~30–45 s plus attach and
drain, so **budget 25–40 minutes** — up from the 15–25 the five-arm table quoted, because there
are five more runs *and* each must be about half as long again for the 1% arm to open its bursts.
That is a real cost and the operator should know it before starting.

**If the calibration pass says the work is mis-sized**, retune and re-run — the failure message
names the flags and now also names an `--gpu-iters` value to try, computed from the time the
calibration run actually took. `--gpu-rounds` sets kernel duration (the dependency chain length);
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

Re-run unchanged on `feat/overhead-1pct-arm` after the 1% arm: build, vet, `go test ./... -count=1`
and `golangci-lint` all clean, and the capability-less skip is still one `BENCH_SKIPPED` line,
exit 0, **no JSON file written**. The other scenarios' skip behaviour is untouched.

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

New tests, all offline, all in `bench/cmd/scenario/gpupc_test.go` (**62 cases**, up from 45; the
1%-duty arm added 17 rather than starting a parallel set):

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

Added with the 1% arm:

- **the arm exists and is 50 ms / 4950 ms**, found by its duty rather than by its index so a
  reordering of the table cannot make the test examine a different arm;
- **deep-dive is reachable** — the worked example above — *and* the same costs on the original
  three duties still land on unshippable;
- **all four clauses and all four Tier A verdicts fire** on the duties the harness actually runs,
  asserted as a property rather than inferred from one table;
- **`TIER_A_INDETERMINATE` is reachable** on the new table and still says "do not read this as a
  pass";
- **both halves of the coincidence note**: DIFFERENT below 2.5% duty, SAME at or above it;
- **the plan's literal fourth clause is disclosed** when the evaluation point changes the answer,
  and silent when it does not;
- **the run-length floor** is derived from the table, and an impossible fixed-work window is
  refused before anything runs;
- **the 1% arm proves its own mode**, on the same terms as every other arm and each shown red:
  too few bursts, nothing serialized, windows that do not reconcile, a duty that overshoots the
  derived ceiling (and one that legitimately overshoots, which must pass), a duty far *below* the
  configured one, a graph process, and a failed `cuptiPCSamplingStart`;
- **the cross-arm burst check extends to four arms**: 1% must open strictly fewer bursts than
  2.5%, and a 1% arm that opened as many is refused.

---

## Cannot verify — every number is outstanding

`CapEff: 0`, no GPU on this machine. **No arm of this benchmark has ever been executed.** No CUDA
kernel in `cuda_concurrent.cu` has ever run; it has been compiled and disassembled, nothing more.

**The tier decision is not yet made, and no part of it may be made from this file.**

Outstanding, all of it:

1. **Every number in the arm table.** Baseline wall-clock, Tier B's cost, Tier A's cost at 10%,
   5%, 2.5% **and 1%** duty, every achieved kernel throughput, every cost ÷ duty ratio. None
   exists. **The 1% arm has never run either** — adding it made a verdict reachable, not measured.
2. **Which threshold fires, and therefore whether Tier A ships at all**, and whether Tier B
   remains a candidate for always-on.
3. **Whether the workload is actually concurrent on a 3090.** The design argues 16 blocks × 256
   threads leaves room for four kernels to co-reside on 82 SMs, and the SASS confirms the
   dependency chain is real, but the achieved concurrency is a hardware measurement. The harness
   asserts a floor rather than assuming it, so a negative answer is a loud failure with retuning
   advice and not a quiet underestimate — but the answer is unknown.
4. **Whether the default sizing lands in the 25–120 s window** on a 3090. `--gpu-rounds 64000`
   was estimated from a dependency-chain latency guess, not measured. The calibration pass exists
   because this is unknown.
5. **Whether pinning `MAX_DUTY_PERMILLE` and `MAX_GAP_MS` to the same gap really pins the burst
   controller.** It follows from `burst_next_gap_ns`'s clamp read as source, and
   `core/burst_test.cc` proves the clamp against a fake clock, but this exact combination has
   never been given to the adapter on hardware. The achieved-duty assertion is what would catch
   it. **This is sharper for the 1% arm than for the other three**: `MAX_DUTY_PERMILLE=10` makes
   `burst_min_gap_ns` compute `50 ms × 99 = 4950 ms`, which is *equal to* rather than below
   `max_gap_ns`, so the clamp returns `max_gap_ns` by its own `g >= max_gap_ns` branch. The value
   is identical either way, and `TestTierAArmsPinTheGapFromBothSides` pins the environment — but
   which branch the adapter takes has never been observed on hardware.
5a. **Whether 25 s of fixed work really opens four bursts at a 5 s cycle.** The floor is derived
   from arithmetic and is conservative twice over (checked against the fastest, uninjected run,
   and against `elapsed_ms` rather than the longer process lifetime), but the burst controller's
   start-up latency on a real process is unmeasured. A shortfall is a **loud failure** —
   `bursts >= --gpu-min-bursts` — and not a quiet arm running at a duty nobody configured.
5b. **Whether the default `--gpu-iters 30000` lands inside the new 25–120 s window on a 3090.**
   It is the old guess scaled by 1.5 and is no more measured than the old one was. The
   calibration pass is what catches it, and it now names the `--gpu-iters` value to try.
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
