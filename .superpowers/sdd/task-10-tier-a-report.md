# Task 10 — Tier A: `KERNEL_SERIALIZED` PC sampling, duty-cycled, and its disclosure

Branch `feat/tier-a-serialized`, one commit on top of `origin/main` (`965e0a48`, the Task 6
merge). Tier A is **off by default**: `PERFAGENT_GPU_PC_SAMPLING` is unset, and unset means no
burst controller, no burst timer, no `cuptiPCSamplingStart`, no window on the wire and
`gpu_serialized="false"` unconditionally on every execution.

Every CUPTI API this task needs exists in CUDA 13.3 exactly as the plan describes:
`CUPTI_PC_SAMPLING_COLLECTION_MODE_KERNEL_SERIALIZED`,
`CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_ENABLE_START_STOP_CONTROL` with
`enableStartStopControlData.enableStartStopControl`, `cuptiPCSamplingStart`,
`cuptiPCSamplingStop`. The header also states the flush rule this task turns on:

> Flushing of GPU PC Sampling data is required at following point to maintain uniqueness of PCs:
> … If configuration option `ENABLE_START_STOP_CONTROL` is enabled, then after every range end
> i.e. `cuptiPCSamplingStop()`

Nothing was improvised around a missing API.

---

## The burst controller, and its convergence proof

`shim/core/burst.h` + `shim/core/burst_test.cc`. No CUPTI, no clock, no allocation, no threads:
the caller supplies `now_ns` and the running `(PC, stall)` pair count, so the whole thing runs
against a fake clock on a machine with no NVIDIA hardware.

**The loop is a pure function of (target rate, observed rate, elapsed):**

```
observed_rate = pairs / elapsed                       pairs per second
ratio         = observed_rate / target_rate           >1 means too many
cycle         = elapsed * ratio                       the cycle that would have hit target
raw_gap       = max(cycle - burst_ns, 0)
gap           = prev_gap + gain * (raw_gap - prev_gap)
gap           = clamp(gap, min_gap, max_gap)
```

`burst_next_gap_ns(cfg, prev_gap, pairs, elapsed)` is a free function and is tested directly,
independently of the state machine.

**Convergence.** The model, stated in the header rather than left implicit: pairs are produced
only while a burst is open, so pairs-per-burst does not depend on the gap. Under that model
`raw_gap` is a constant `P/target - burst` for a steady workload, and the damped update is a
geometric sequence with ratio `(1 - gain)`. Asserted three ways:

- the analytic fixed point — 100 pairs per 50 ms burst at a 100/s target wants a 1 s cycle, so
  `gap* = 950 ms`. The simulation lands on **949 ms** and an achieved **102.4 pairs/s** over 60
  cycles including the un-converged ramp;
- convergence from both directions — starting at the floor (450 ms) and at the ceiling (10 s),
  40 iterations later the two agree to within 1 ms and both sit in (900 ms, 1000 ms);
- the fixed point is interior to both clamps in that scenario, so the loop has to actually find
  it rather than being pinned by a bound.

**The duty ceiling is a hard bound, not a target.** `min_gap = burst * (1/max_duty - 1)` — 450 ms
for a 50 ms burst at 10% — and `burst_next_gap_ns` clamps to it on every path. The test does not
argue this, it **sweeps** it: eleven pair counts from 0 to `UINT64_MAX` crossed with six previous
gaps, asserting `gap >= min_gap` and `burst/(burst+gap) <= max_duty` on all 66. Degenerate
configuration lands on a clamp rather than on undefined behaviour: `target_rate = 0` (ratio +inf)
→ `max_gap`; `target_rate` NaN → floor; `max_duty = 0` → a positive floor; `elapsed = 0` → hold.
Every comparison is written `!(x > y)` rather than `x <= y` so NaN falls to the safe side.

**A zero observed rate does not drive the gap to zero.** It drives it to `min_gap` — 450 ms — and
stops: 100 iterations at zero pairs converge on exactly `burst_min_gap_ns(cfg)`, which is `> 0`.
An idle GPU therefore samples at the duty ceiling and no faster, which is the opposite of the
failure the assertion exists to exclude. A whole-run simulation of an idle workload achieves
duty **0.102** against a 0.10 ceiling — the 2% is the burst timer's 5 ms granularity on a 50 ms
burst, asserted with the granularity in it rather than fudged.

**One design correction the test caught.** The first draft read the pair count inside `poll()` at
stop time. CUPTI hands a burst's PC records over on the flush that *follows* the stop, so the
controller measured every burst as having produced nothing and sat at the duty floor forever —
converged-looking, and wrong. The cycle is now three calls (`poll → kStart`, `poll → kStop`,
`closed(pairs_total)` after the range-end drain), and the reason is written at the class. A caller
that forgets `closed()` degrades rather than breaks: the stop schedules the next burst at the
current gap, so the duty cycle keeps running and only the loop stops adapting.

---

## The three `gpu_serialized` values

`gpu/serialization.go` holds the evidence and answers the question; `gpu/timeline.go` calls it
once per execution at `Snapshot`; `gpu/projection.go` writes the label.

**The zero value is `"unknown"`.** `SerializationState` is an integer enum with
`SerializationUnknown = iota`. This is the strongest available form of "unknown must never
degrade to false": a field nobody set, a struct built by a test, a value lost in a copy and a code
path that forgot to classify all land on `"unknown"`, because `"false"` has to be written
deliberately. `TestSerializedLabelIsUnconditionalAndHasThreeValues` includes a view nobody
classified and asserts it renders `"unknown"`.

**The rule, in order:**

1. intersects a **closed, kernel-serialized** burst → `"true"`. Definite, and it outranks
   `"unknown"` — an execution that provably overlapped a burst is perturbed whatever else is
   unknown about the rest of its interval (`TestSerializationTrueOutranksUnknown`);
2. wholly inside a span the store holds an **unbroken** history for, touching no burst →
   `"false"`. This is the only branch in the file that returns `"false"`, and it needs positive
   evidence on both endpoints;
3. everything else → `"unknown"`.

**Coverage, and why it is not simply "the earliest to the latest window".** The store tracks a
`coverageStart` that moves **forward only**, on two events: a sequence gap (records were lost, so
nothing before the hole can be shown to be contiguous) and an eviction (the oldest burst left).
`coverageEnd` is the earliest **open** window's start if there is one, otherwise the end of the
last closed burst — deliberately *not* extending past it, because the next burst's open record
may simply not have been drained yet, so the interval after the last known window is not a proven
gap. Executions in that trailing interval read `"unknown"`; on a 60 s profile with a 100 ms drain
that is a small tail, and it is honest.

**`"unknown"` never becoming `"false"`, pinned.**
`TestSerializationFalseIsOnlyEverReachedFromPositiveEvidence` drives four window histories (none,
one closed burst, two closed bursts, a burst that never closed) against 324 execution intervals
each, and for **every** execution that came out `"false"` asserts containment in the covered span
and non-overlap with every burst. It also asserts the reverse for the two histories that prove no
gap at all: zero `"false"`s. That is the test the plan asks for, and it is a sweep rather than a
sample because the claim is universal.

Additionally pinned: no windows at all → all `"unknown"` and
`ExecutionsNotSerialized == 0`; outside the covered span → `"unknown"`; a sequence gap restarts
coverage so a previously-provable gap **degrades** to `"unknown"` while the bursts already held
still prove `"true"`; eviction moves answers `"false" → "unknown"` and the test asserts that
direction by classifying the same execution before and after; a window whose mode the producer
left unset is opaque (neither `"true"` nor `"false"`); an inverted window is refused; a PID past
the store's bound reads `"unknown"` rather than inheriting somebody else's history.

**In Tier B and with sampling off, `"false"` is correct and unconditional.**
`TimelineConfig.SerializedSampling` is the agent's own configuration — Task 11 owns the setting
that flips it — and when it is false the window store is not consulted at all. Windows that arrive
anyway (a leftover producer, a system-wide attach) are still ingested and counted; only the answer
is unconditional (`TestSerializationIgnoresWindowsWhenTierAWasNotSelected`).

---

## The open window, and the two-record protocol it required

**`end_ns == 0` is open, not zero-length.** Making that reachable took a protocol decision the
plan implies but does not spell out: a burst reaches the wire **twice**.

- the **open** record goes out the instant `cuptiPCSamplingStart` succeeds, with `end_ns = 0`;
- the **closed** record goes out on the stop, with the same `start_ns` and a real `end_ns`.

Emitting only on the stop would lose the entire burst on a hard exit, and the executions inside it
would then read `"false"` — "not perturbed" when the truth is "cannot tell", the one answer that
must never be reachable by accident. With the open record already delivered, a `SIGKILL` leaves a
window saying a burst was open from `start_ns` and never closed.

The consumer's store supersedes one-way: a closed record replaces an open one with the same start,
an open record **never** replaces a closed one. Both delivery orders are tested
(`TestSerializationClosedWindowSupersedesItsOwnOpenRecord`), so a lossy transport cannot leave a
permanently-open window behind an ordinary duty cycle, and `Snapshot.SamplingWindowsOpen` stays a
real signal.

`TestSerializationOpenWindowMakesEverythingFromItsStartUnknownAndNeverFalse` sets up a completed
burst first — so the store has genuine coverage and a naive implementation would happily answer
`"false"` after it — then opens a burst that never closes, and sweeps 41 executions across the
whole timeline asserting both halves on every one at or after the open start: it **is**
`"unknown"`, and it is **not** `"false"`.

**`end_ns == 0` means a hard exit specifically.** `at_exit_handler` stops the burst timer first,
then `burst_shutdown()` closes the open window with the exit timestamp, then `on_finalize` runs.
On the CUPTI fatal-error path `on_finalize` calls `g_burst->shutdown()` (which takes only the
controller's own mutex, never `g_pc_mu`, which that path may fail to acquire) and deliberately
does **not** close the window — a CUPTI fatal error *is* the hard case.

One correction to the `Stats.SamplingWindowsOpen` doc that Task 7 wrote: it said "healthy: zero".
With the open record emitted at every burst start, a healthy Tier A run produces one per burst and
this wire-side counter tracks `SamplingWindowsDecoded / 2`. The anomaly is a window still open
*in the store* at snapshot time, which is `Snapshot.SamplingWindowsOpen`. Both doc comments now
say which is which.

---

## The CUDA-graph refusal

Structural, in the pure controller, so it is testable with no GPU. `BurstController::poll(now,
graphs_observed)` — once `graphs_observed` reads true, the controller latches `refused_`, returns
`kStop` if a burst is open (so the window closes honestly rather than being abandoned) and
**never returns `kStart` again**. It does not know how to become Tier B, which is the point: a
silent downgrade would leave the operator reading a Tier B profile while believing they asked for
Tier A.

Two moments in the adapter:

- **at enable time** — `pc_enable_ctx` refuses outright if `g_exec_from_graph != 0`, logs the
  reason at length and counts `g_ctx_enable_failed`. PC sampling is not enabled for that context
  at all;
- **on every burst tick** — a process can run for minutes before its first graph launch. When one
  arrives the open burst is cut short, `g_tier_a_graph_refused` increments once (not per poll) and
  a loud log line says bursts have stopped permanently. Executions already inside a window stay
  marked serialized, which is correct: they really were.

`burst_test.cc` covers both: a graph mid-burst (`kStop`, `last_stop_reason() == kGraph`, the 10 ms
of real perturbation still accounted in `burst_ns()`, then 1000 further polls all `kNone`), and a
graph observed before the first burst (`bursts() == 0`, `burst_ns() == 0`, `graph_refusals() == 1`
— refuses to *start*, distinct from stopping).

The condition also already rides the wire as `gpu_dropped_v1 / GPU_DROP_CLASS_GRAPH_EXEC`, emitted
before the tier gate in `on_tick` (Task 6).

---

## The sum identity

`ExecutionsSerialized + ExecutionsNotSerialized + ExecutionsSerializationUnknown ==
len(Snapshot.Executions)`, exactly.

It holds **by construction**: the three counters are incremented where the `ExecutionView` is
built, before any of the join loop's four `continue`s, so every execution is counted exactly once
on every path through `Snapshot`.

It is asserted in three places:

- `assertSumIdentity` runs in every one of the 17 tests in `gpu/serialization_test.go`;
- `assertSerializationOutcomesAccounted` is now part of `assertConformanceInvariants`, so it runs
  on **every** conformance scenario in the file, including the ones that emit no windows at all —
  and it additionally asserts the negative for the default harness (nothing serializes kernels
  there, so no execution may read `"true"` or `"unknown"`);
- `joinAnomalies` raises it at runtime, in the same place and with the same wording as the join
  outcomes' identity: *"serialization outcomes sum to N but the snapshot holds M executions — some
  execution carries no gpu_serialized disclosure at all"*.

The counters the task named, and where each lives:

| counter | side | where |
| --- | --- | --- |
| `SamplingBursts` | producer | `g_sampling_bursts`, adapter log `tier A bursts=` |
| `SamplingBurstNs` | producer | `g_sampling_burst_ns`, with `duty=` beside it |
| `SamplingWindowsReceived` | agent | `Snapshot.SamplingWindowsReceived` (cumulative) |
| `ExecutionsSerialized` | agent | `Snapshot`, per snapshot |
| `ExecutionsNotSerialized` | agent | `Snapshot`, per snapshot |
| `ExecutionsSerializationUnknown` | agent | `Snapshot`, per snapshot |

The producer/consumer split is deliberate: an agent-side `SamplingBursts` would equal
`SamplingWindowsReceived` by construction and could never disagree with it, which is not a
counter. Split across the two ends, the **gap between them is the loss**. Alongside them:
`Snapshot.SamplingWindowsHeld` / `SamplingWindowsOpen` (gauges),
`Dropped.EvictedSamplingWindows`, `SinkStats.SamplingWindows`, and on the shim
`g_windows_emitted`, `g_burst_start_failed`, `g_burst_stop_failed`, `g_tier_a_graph_refused`,
`PCDrainSchedule::range_end()`.

Every one of them can go non-zero from a test.

---

## What else changed, and what deliberately did not

**`shim/core/pcdrain.h`** gains `PCDrainReason::kRangeEnd` and its own counter. The range-end
flush is mandatory in this configuration exactly as the module-unload flush is in CONTINUOUS —
missing it does not lose data, it makes two instructions share a PC identity, silently. It also
marks the **shared** schedule's phase, so the 100 ms drain tick coalesces instead of repeating the
pull microseconds later. `pcdrain_test.cc` asserts five range-end drains inside one period, that
they are counted apart from unload and teardown drains, and that the phase moved.

**A second timer, and this is a deviation from the plan's wording.** The plan says "the existing
drain timer is its natural home". The flush is: it goes through the same `pc_drain_all` and the
same `PCDrainSchedule`. The burst *cycle* is not, and cannot be — the drain tick is 100 ms and a
50 ms burst cannot be expressed on it. Quantizing the burst to the drain period would silently
double the burst length and therefore the duty fraction, which is the one number this tier exists
to bound. So the burst rides a `Drainer` of its own at `burst_ns/5` (10 ms by default), doing an
atomic load and a compare when no transition is due. The reason is written at the declaration.

**Task 6's `on_tick` hazard was not reintroduced.** The gate at the bottom of `on_tick` was
`if (!g_pc_tier_b) return;` and is now `if (!g_pc_enabled) return;` — a rename of the same
variable, which is now set by `PERFAGENT_GPU_PC_SAMPLING != 0` and so covers both tiers. Nothing
was appended below it. The cubin `drain()` and the `classGraphExec` emission are still **above**
it, with their comments intact; `make -C shim nvidia` and the probe listing confirm all nine
pre-existing probes plus the new one.

**Nothing weakened.** Tier B's configuration is unchanged (`ENABLE_START_STOP_CONTROL` is still
deliberately off there, and the comment now says why in both directions). The cubin capture, the
`CubinView` guard (`make -C shim check-cubin-defer`: OK, all 5 deferrals still refuse to compile)
and the `MODULE_UNLOAD_STARTING` drain are untouched — `on_resource`'s switch still owns
`MODULE_UNLOAD_STARTING`, still calls `pc_drain_all` and still **blocks** on `g_pc_mu`.

**Tier selection is not implemented.** `PERFAGENT_GPU_PC_SAMPLING` gained the value `2` for Tier
A, in the same variable as `1` — so the two tiers are mutually exclusive *by construction* rather
than by a check that can be forgotten, which is what Task 11 wants anyway. There is no runtime
switch and no CLI flag; `TimelineConfig.SerializedSampling` defaults false and nothing sets it
yet, so a merge of this branch changes nothing a shipping profiler does.

**`gpu_serialized` reaches `gpu/projection.go` in this task**, though the plan files the label set
under Task 9. A tri-state that never reaches the profile is exactly the "counter reading green"
shape the constraints warn about, and the addition is three lines plus a comment. Task 9's other
labels are untouched.

**`notYetFired` is now empty.** `TestEveryProducerProbeHasACookieAndViceVersa` failed on the first
run after the adapter started firing `gpu_sampling_window_v1` and named the fix in its own message,
exactly as it did for `gpu_module_load_v1` in Task 5. Every probe the ABI defines is now fired by a
producer.

**The stub emits windows.** `PERFAGENT_STUB_SAMPLING_WINDOWS=<n>` cuts the executions' span into
`2n` slices and makes every other one a burst, emitting the open-then-closed pair for each, so
roughly half the executions fall inside a window and half in a proven gap.
`PERFAGENT_STUB_SAMPLING_WINDOW_OPEN=1` leaves the last burst open — the hard-exit shape. With
both unset the stub's wire is byte-for-byte what it was.

**No ABI change and no BPF change.** `gpu_sampling_window_v1`, `KIND_SAMPLING_WINDOW = 8` and
`REC_SAMPLING_WINDOW` have been in place since Task 2; this task is the first producer.
`git status` shows no `.o` churn.

---

## Verification actually run

```
make -C shim                      exit 0
make -C shim test                 exit 0  (burst_test OK, pcdrain_test OK, check-cubin-defer OK,
                                           cubinqueue_test OK, probe_order_test OK, usdt_abi_test OK)
make -C shim check-fpless         OK
make -C shim check-cubin-defer    OK - the compliant capture compiles, all 5 deferrals do not
make -C shim nvidia               exit 0 (CUDA 13.3, real libcupti)
go build ./... && go vet ./...    clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1     all pass
go test ./gpu/ ./gpuprobe/ -race -count=4              ok gpu 19.7s, gpuprobe 18.8s
~/go/bin/golangci-lint run --timeout=5m                0 issues
```

Ten probes in the built adapter, one exported symbol:

```
$ readelf -n shim/libperfagent-gpu-nvidia.so | grep -o 'gpu_[a-z_0-9]*' | sort -u
gpu_config_v1  gpu_dropped_v1  gpu_exec_v1  gpu_kernel_name_v1  gpu_launch_sampled_v1
gpu_launch_v1  gpu_module_load_v1  gpu_pc_sample_batch_v1  gpu_sampling_window_v1
gpu_stall_reason_map_v1
$ nm -D --defined-only shim/libperfagent-gpu-nvidia.so
0000000000004c40 T InitializeInjection
```

New tests:

- `shim/core/burst_test.cc` — the duty ceiling swept across the whole observed-rate range,
  convergence to the analytic fixed point from both directions, the zero-rate floor, degenerate
  configuration, the start/stop state machine over 2,000 ticks, the graph refusal in both its
  shapes, and shutdown closing an open burst.
- `shim/core/pcdrain_test.cc` — the range-end drain: mandatory, counted apart, resets the phase.
- `gpu/serialization_test.go` — 17 tests; the three values, the open-window sweep, the supersede
  protocol in both delivery orders, the sequence-gap restart, cross-process isolation, eviction's
  direction, degenerate records, and the universal "false only from positive evidence" sweep.
- `gpu/projection_test.go` — the label is unconditional, has three values, degrades to `"unknown"`
  when unclassified, reaches every PC-derived sample, and beats a forged `Tags` entry.
- `gpu/conformance_test.go` — the sum identity on every scenario.
- `gpu/sink_test.go` — a window draws on the anchor budget, not the data one, so exec volume
  cannot starve the disclosure out of the sink.
- `gpu/joinhealth_test.go` — the three outcomes in the summary and the three Tier A anomalies.
- `gpuprobe/consumer_test.go` — the window reaches the sink with its process, the sequence gap
  rides the first record of its batch and no other, sequences are per-process, and a refused
  window is counted.

---

## Cannot verify — every item needs the RTX 3090

Nothing below has been executed. `CapEff: 0`, no GPU on this machine, and **no `CUpti_` code path
added by this commit has ever run**.

**The plan's four:**

1. That `correlationId` is non-zero on ≥99% of PC records in `KERNEL_SERIALIZED` mode (the spike
   says 1,828/1,828). `Stats.PCSamplesWithoutCorrelation` is the counter that will say; a single
   non-zero there breaks Tier A's whole claim.
2. That windows actually bracket the executions that ran in them — i.e. that the mono-clock
   timestamps this adapter stamps around `cuptiPCSamplingStart`/`Stop` and the converted activity
   timestamps on `gpu_exec_v1` land in the same domain closely enough for the intersection to
   mean what it says. **This is the single most load-bearing unverified item in the task**: the
   whole disclosure is an interval intersection, and a systematic skew between the two clocks
   would mis-mark executions in a way no counter here can detect.
3. **Whether the collection mode can be changed between `Stop` and `Start` without a full
   `Disable`/`Enable`.** Undocumented in `cupti_pcsampling.h`, and it decides whether a runtime
   tier switch is possible at all. Nothing in this commit depends on the answer — the mode is set
   once, at context creation — but Task 11's "switching tiers mid-run is out of scope, gated on
   this" cannot be resolved without it.
4. Tier A's overhead (Task 12), which decides whether this tier ships.

**Discovered while implementing, and added to that list:**

5. **Whether `ENABLE_START_STOP_CONTROL` is accepted alongside `KERNEL_SERIALIZED` in one
   `cuptiPCSamplingSetConfigurationAttribute` call.** Task 6's per-attribute `attributeStatus`
   check will catch a refusal loudly (the context disables and `ctx_enable_failed` moves), but
   which attributes may be combined is not stated in the header.
6. **Whether `cuptiPCSamplingStart` may be called on a context that has never sampled, and
   whether `Stop` on an already-stopped context errors.** The adapter starts and stops every
   tracked context on each burst; a context created mid-burst is started on the *next* one, so it
   misses part of a window it is nonetheless covered by. `g_burst_start_failed` /
   `g_burst_stop_failed` count, and must be 0.
7. **Whether the range-end `cuptiPCSamplingGetData` actually returns the burst's records
   synchronously.** The closed-loop controller's yield measurement assumes it does. If CUPTI
   defers them to a later flush, the loop sees a lagged pair count — it still converges (the lag
   is a constant offset for a steady workload) but the transient after a workload change is one
   cycle longer than modelled. Nothing breaks; the settling time is wrong.
8. **The real pairs-per-burst figure**, which decides whether the fixed point is interior to the
   clamps at all. The spike saw 352 PC records for ~103k samples in CONTINUOUS over an unknown
   interval. If a 50 ms serialized burst yields thousands of pairs, `max_gap_ns` (10 s) binds and
   the achieved rate stays above the 100/s target — visible in the adapter's `gap_ns=` reaching
   10000 ms, and the remedy is a longer `max_gap` or a shorter burst.
9. **Whether serialization visibly inflates `gpu_exec_v1` durations**, i.e. whether the
   disclosure is measuring something real. The obvious check on hardware: the same workload in
   Tier A, comparing the duration distribution of `gpu_serialized="true"` executions against
   `"false"` ones **in the same profile**. If they do not separate, either the duty cycle is not
   doing what it says or the window intersection is mis-aligned (item 2).
10. **Whether `g_ctx_enabled` is a sufficient guard on the burst timer starting first.** The
    timer starts at the end of `InitializeInjection`, before any `CONTEXT_CREATED` callback can
    have fired, so `on_burst_tick` refuses to open a burst until at least one context has enabled
    — otherwise it would announce a window covering executions nothing was sampling. Whether a
    context can *fail* to enable after that (leaving `ctx_enabled` non-zero with no live context)
    is unmeasured; if it can, a window would be announced with nothing behind it, over-stating
    perturbation for one burst. Safe direction, but wrong.
11. **Whether `cuptiPCSamplingStop` can re-enter our resource callback**, the same hazard Task 6
    listed for `GetData`. `burst_close` holds `g_pc_mu` across `Stop` and then across the drain;
    a re-entrant `MODULE_UNLOAD_STARTING` on the same thread would self-deadlock.
12. **The burst timer's real granularity under load.** The 5 ms figure in the test is a fake
    clock; a 10 ms `sleep_for` on a loaded machine can overshoot, and every millisecond of
    overshoot is duty the ceiling did not authorise. `duty=` in the adapter's report is the
    measurement, and it is reported precisely because `max_duty` bounds what the loop may *ask*
    for and not what the OS delivers.
13. **Whether `g_exec_from_graph` moves early enough for the refusal to be useful.** It is set
    from `CUpti_ActivityKernel12.graphId` on the activity path, which arrives on the drain tick —
    up to 100 ms, and one or two bursts, after the first graph kernel actually ran. Those bursts'
    windows are emitted and their executions are marked, so nothing is silent; but Tier A does run
    briefly in a graph-using process before refusing, and the size of that window is unmeasured.
