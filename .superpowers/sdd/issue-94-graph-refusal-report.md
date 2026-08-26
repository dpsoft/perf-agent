# Issue #94 — Tier A now actually refuses where CUDA graph executions have been observed

Branch `fix/tier-a-refuses-graphs`, one commit on `origin/main` (`e09c073a`, the Task 13 gate
merge). Verified on this machine: `CapEff: 0`, **no GPU**. What that means for each claim below
is stated where the claim is made, not once at the bottom.

The scope is the **refusal**. Making graphs actually *work* under Tier A needs `gpu_exec_v2`
carrying `graphId` plus a one-launch-to-many-executions join shape — "a phase, not a field" —
and none of it is attempted here.

---

## What consumes `DropClassGraphExec` now

Before this commit: nothing. The consumer decoded `gpu_dropped_v1` into `batch.Drops`, the
batch fell through `applyBatch`'s `default:` arm and was counted as `Stats.Undecoded`, and no
counter, no `Snapshot` field, no `joinhealth` line and no gate existed anywhere downstream.
Tier A ran happily in a graph-using process and produced `gpu_join="exact"`,
`gpu_pc_attrib="exact"`, every counter green, N kernels billed to one call site.

The chain now, wire to profile:

| hop | where | what |
| --- | --- | --- |
| decode | `gpuprobe/consumer.go` — new `case kindDropped:` + `noteDropLocked` | one record per class, dispatched by class |
| count | `Stats.GraphExecutions` / `GraphExecReports` / `DropsDecoded` / `DropsUnconsumed` | arrival, on the producer side of the sink |
| normalize | `gpu.GPUGraphExecutions{Backend, PID, Count}` (`gpu/types.go`) | a first-class event, not a string-parsed attribute |
| transport | `EventSink.EmitGraphExecutions` (new method), `CountingSink` pass-through | **unconditionally admitted** — see below |
| latch | `Timeline.EmitGraphExecutions`, `graphExecsByPID` + `isGraphRefusedLocked` | per process, cumulative |
| mark | `ExecutionView.GraphRefused`, `PCAttribGraphRefused` | applied in `Snapshot` |
| count | `Snapshot.GraphExecutions`, `GraphExecProcesses`, `GraphExecUnscoped`, `GraphExecTrackingCapped`, `ExecutionsGraphRefused`, `PCJoinStats.GraphRefusedAttributions` | |
| disclose | `gpu/joinhealth.go` — one summary clause, two anomaly lines | |
| label | `gpu_graph_refused="true"`, `gpu_pc_attrib="graph-refused"` (`gpu/projection.go`) | |
| refuse to start | `PCSamplingRequest.GraphExecutionsObserved` → `ErrPCSamplingGraphExecutions` (`gpu/tier.go`) | |

### The one deliberate exception to this package's own rules

`CountingSink.EmitGraphExecutions` has **no admission control at all**: no capacity check, no
token bucket, no clock-domain check, no path on which it can be refused. Every other method
there is bounded, because every other event is a *measurement* and dropping one costs a slice
of the profile. This one is a *refusal*, and dropping it does not cost a slice of the profile —
it restores the entire defect the refusal exists to prevent, precisely under the load that
makes a bucket bite. A bounded refusal is not a refusal. It is also cheap enough that the bound
buys nothing: the producer emits deltas at most once per drain tick.

Consequently there is no `SinkStats` row for it (a row whose three drop counters can never move
is a row of zeros that trains the reader to skip the table). The arrival count lives in
`gpuprobe.Stats.GraphExecutions` and can be compared against `Snapshot.GraphExecutions`; the
privileged gate asserts them equal.

### `Stats.Undecoded` changed meaning, and assertion 12 with it

Adding a `case kindDropped:` arm means `Undecoded` is now zero for **every** kind the ABI
defines. That is a stronger contract than before — it now means only "a kind neither side of
the wire knows", i.e. ABI drift — and it required editing the privileged gate's assertion 12
from `Undecoded == 4` to `Undecoded == 0` plus an exact partition of the four class records.

Scope discipline on the other three classes: `pc-dropped-hw`, `pc-buffer-full` and
`pc-non-user-kernel` are **decoded and counted, not acted on**. Turning each into an
operator-visible number is the separate task the Task 13 report deferred; `DropsUnconsumed` is
what keeps them from being silent in the meantime, exactly as `Undecoded` did before. The
identity `DropsDecoded == GraphExecReports + DropsUnconsumed` is asserted, so a class that
reaches the decoder and then vanishes shows up as a shortfall.

The asymmetry is written at `noteDropLocked`: the other three describe **loss**, which
under-reports a profile and is visible as a shortfall; the graph class describes a claim that
is **false while looking like the strongest answer available**, which cannot wait for the
general treatment because its whole purpose is to stop a tier.

---

## The refusal's shape and its loudness

### Two halves, because a graph can arrive at any time

**It never starts.** `PCSamplingRequest.Select` refuses `"serialized"` with
`ErrPCSamplingGraphExecutions`, resolving to `PCSamplingOff` — never to `"continuous"`. Off
means the producer's environment carries `PERFAGENT_GPU_PC_SAMPLING=off`, so the burst
controller is never constructed, no burst is ever opened and no kernel is ever serialized. The
check runs **before** the perturbation acknowledgement and no flag buys past it: an operator
who acknowledges the perturbation has agreed to pay a cost for exact attribution, and in a
graph-using process they would pay the cost and receive attribution that is exact-looking and
many-to-one, which is not the trade they agreed to.

The refusal is a refusal and not a downgrade because the plan says so and the reason is
load-bearing: a silent fall back to Tier B is *indistinguishable from Tier A working*.

**It is withdrawn mid-run.** A process's first graph launch can be minutes into a run that
began legitimately. Two mechanisms, neither of which is `Select`:

- the producer stops bursting. `BurstController::poll` latches on the first graph execution,
  returns `kStop` so an open burst closes honestly rather than being abandoned, and never
  returns `kStart` again (`shim/core/burst.h`, Task 10). **This already existed** and is
  untouched by this commit;
- the agent withdraws the claim. `Timeline` marks every execution of that process
  `GraphRefused`, turns `gpu_pc_attrib` from `"exact"` to `"graph-refused"`, counts both, and
  `JoinHealth` raises a standing anomaly.

### Loudness, in the exact words an operator sees

On the **summary line every run prints**, so a reader who stops after one line cannot come away
believing the attribution held:

```
gpu join: 3 executions, all exact; 3 launches, all matched; cache 3 live; pc samples 3
attributed, 0 pending; 512 executions from CUDA graphs — TIER A EXACT ATTRIBUTION WITHDRAWN on
3 executions; pc sampling serialized; serialization 3 true, 0 false, 0 unknown over 1 burst;
4 standing warning lines; 3 anomalies
```

And as an anomaly:

```
gpu join ANOMALY: 512 kernel executions launched from CUDA GRAPHS in 1 process, and Tier A
("serialized") PC sampling was selected — ONE graph launch fires ONE runtime callback for N
kernels and gpu_exec_v1 carries no graph id, so those executions share one correlation and one
CPU stack. Tier A's exact-launch attribution is FALSE here while still looking exact. It is
withdrawn rather than downgraded: 3 of 3 executions in this snapshot carry
gpu_graph_refused="true", and 3 executions had gpu_pc_attrib turned from "exact" to
"graph-refused". Their durations and sample weights are real measurements and are kept; only
the attribution claim is gone. The producer has also stopped opening bursts in that process.
Re-run with --gpu-pc-sampling=continuous, which joins through the module rather than through
the launch and is unaffected by graphs
```

It is raised **immediately after the serialization identity and before every other attribution
clause**, because it is the one condition on that list that makes the counters below it read
green while being wrong.

Both drivers already log `JoinHealthWith` line by line, so **no change to `cmd/` was needed**
for the mid-run refusal to reach an operator — which also kept this branch clear of
`.worktrees/wire-modulestore`.

### The counter is assertable in both directions

`Snapshot.GraphExecutions` and `gpuprobe.Stats.GraphExecutions` read **exactly zero** on any run
of a workload that launches no CUDA graph, and are non-zero the instant one is reported. Both
directions are asserted (`TestGraphCountersAreZeroOnAHealthyRunAndNonZeroImmediately`), the
zero half additionally runs on **every** conformance scenario in `gpu/conformance_test.go` via
the new `assertGraphRefusalAccounted`, and `TestHealthyTierBRunLeavesEveryNewLossCounterAtZero`
asserts the four new consumer counters at zero.

`TierAGraphRefused()` is a **method, not a field**. A stored bool that a copy, a hand-built
`Snapshot` or a forgotten assignment could leave false while `GraphExecutions` was non-zero
would be a silent downgrade wearing the shape of the refusal.

### Scope, and the direction it fails in

The mark is **per process**, because PC-sampling collection mode is a property of the profiled
process — the burst controller lives inside it — so one graph-using process in a system-wide
profile must not withdraw the exact attribution of every other one.

Two ways that scope is lost, and both **widen** rather than narrow: a report naming no process
(`PID 0`), and the per-process tracker being full. That is the **opposite** resolution to the
multi-device tracker's cap, deliberately: there, absence of evidence for a second device is not
evidence of one, so an untracked process is treated as single-device. Here the evidence already
exists and only its scope is missing, so discarding it would throw away a proven refusal. Both
causes are counted (`GraphExecUnscoped`, `GraphExecTrackingCapped`) and raise their own
`joinhealth` line, so an operator looking at a profile where *everything* is marked can tell
"they all used graphs" from "we stopped being able to say which one did".

---

## What happens to samples from the pre-refusal window, and why

**They are KEPT, and MARKED.** Not discarded, and not left unmarked.

The window is real and was measured by Task 10 (its item 13): `g_exec_from_graph` is set from
`CUpti_ActivityKernel12.graphId` on the **activity** path, which reaches the producer's drain
tick — up to 100 ms, and one or two bursts, after the first graph kernel actually ran. So
executions **arrive before the report that condemns them**.

The three options are not equally honest:

- **Discarding them** is the silent-downgrade shape wearing different clothes. A profile with
  the graph process's GPU time removed is indistinguishable from a profile of a process that
  did no GPU work. Worse, it would destroy the `gpu_serialized="true"` disclosure for kernels
  that really were serialized: the profiler perturbed that workload, and deleting the evidence
  does not un-perturb it, it only hides that the damage was done.
- **Keeping them unmarked** is the defect the issue is about.
- **Keeping them marked** keeps every measurement and takes away exactly one thing: the claim
  about *which launch* they belong to.

So the durations, the sample weights, the stall reasons, the source labels and the whole
serialization disclosure survive untouched, and `gpu_pc_attrib` stops saying `"exact"`.
`TestGraphRefusalKeepsTheMeasurementsItRefusesToAttribute` asserts each of those, including
that the sample keeps its full share of the execution's duration.

### The mark is retroactive within a snapshot, and not across one

The condition is a property of the process, so it is applied at `Snapshot`, not at ingest:
**every execution the Timeline still holds is marked however late the report came**. Both
drivers in this tree snapshot exactly once, at the end of the run, so on those drivers **no
execution escapes the mark at all** — the pre-refusal window costs nothing.

A driver that snapshots *periodically* is different: a snapshot already emitted before the first
report carries executions this one would have refused. That residue is real, it is unmeasured on
hardware, and it is **reported rather than left to be discovered** — it has its own `joinhealth`
line, quoted here in full because it is the honest statement of the remaining gap:

```
gpu join ANOMALY: the graph refusal is RETROACTIVE within a snapshot but not across one — the
producer learns an execution came from a graph on its activity drain, up to a drain interval
after the kernel ran, so every execution this Timeline still held is marked above, but any
snapshot already EMITTED before the first graph report arrived carries executions labelled
gpu_pc_attrib="exact" that this one would have refused. A run that snapshots once, at the end,
has no such executions
```

Closing that residue would need the mark to travel with already-emitted pprof samples, which it
cannot; the alternative is for a periodic driver to hold its output until the run ends, which is
a driver decision and not this one.

---

## Tier B is untouched, and the asymmetry is now in the code

Tier B joins a PC sample through the **module** — cubin CRC and function index to a device
function name — and never through the launch, so a graph-launched kernel resolves to its own
kernel exactly as any other does. Nothing about Tier B is false in a graph-using process, and
marking it would be a false alarm that devalues the real one.

That is enforced in four places rather than assumed:

- `Timeline.isGraphRefusedLocked` returns false unless the tier is `PCSamplingSerialized`, and
  the tier check lives **there** rather than at the call sites so a future caller cannot forget
  it;
- `Snapshot` replaces **`PCAttribExact` and only `PCAttribExact`**. `kernel`,
  `kernel-ambiguous` and `kernel-multidevice` are left exactly as they are;
- `PCAttribGraphRefused` is deliberately **not** reachable through `worsePCAttrib`. It sits last
  in `pcAttribs` so `MarshalJSON` accepts it and `PCAttribs()` stays exhaustive, and its doc
  comment says no path ranks its way there. Implementing the refusal with `worsePCAttrib` is a
  mutation the gate catches;
- `joinhealth` raises the anomaly only under Tier A. The count still rides on the `Snapshot` in
  every tier — ingest is never gated, only the consequence — and the summary line says
  *"(this tier joins through the module, not the launch, and is unaffected)"* once, so an
  operator learns not to look for the refusal there.

`TestGraphExecutionsDoNotWeakenTierB` drives Tier B and off; 
`TestGraphRefusalLeavesModuleKeyedAttributionAloneThroughTheJoin` drives a **module-keyed join
under Tier A with the refusal armed** and asserts the attribution survives — that second test
exists because the first one, which builds views by hand, does not exercise `Snapshot` and
therefore cannot catch a `Snapshot` that marks too widely.

The cubin capture, the `CubinView` guard (`make -C shim check-cubin-defer`: OK, all 5 deferrals
still refuse to compile), the `MODULE_UNLOAD_STARTING` drain and tier selection are unchanged.
**No file under `shim/` or `bpf/` changed**; no `.o` churn.

---

## The gate assertion's conversion

`TestGateGraphExecutionRefusalIsNotAssertableYet` was the #44 idiom: a passing test that pinned
the gap by reflecting over `Snapshot`, `TimelineDropStats`, `PCJoinStats`, `TimelineConfig` and
`PCSamplingRequest` and failing the moment any of them grew a field naming graph executions.

It failed, by name, in the gate's own file, on the first run after `Snapshot` grew
`GraphExecutions`. It is **deleted and replaced** by `TestGateGraphExecutionRefusalIsReal`,
which composes rather than restates — the eight behaviours live in `gpu/graphrefusal_test.go`
beside the code, and deleting or weakening any of them now fails *the gate*, by assertion
number:

```
TestPhase6Gate/assertion-10b-graph-execution-makes-tier-a-refuse/
    refuses-to-start
    no-acknowledgement-buys-past-it
    withdraws-exact-mid-run
    is-loud-and-counted
    counters-are-zero-when-healthy
    keeps-the-measurements
    does-not-weaken-tier-b
    does-not-touch-module-keyed-attribution
    does-not-touch-module-keyed-attribution-through-the-join
```

It also keeps the half that was always true: the acknowledgement refusal and the standing
warning must go on naming CUDA graphs, since those are the only places the limitation reaches
an operator who never hits the condition.

The gate's assertion 10b comment no longer says the first clause is asserted "at the level the
product implements it". It is asserted at the level the plan specified.

**The privileged end-to-end gate now drives the refusal for real.** The stub emits
`{count: 2, GPU_DROP_CLASS_GRAPH_EXEC}` whenever PC sampling is on, and
`TestStubDrivesPCSamplingToPprofWithoutAGPU` runs Tier A, so on that run the class crosses a
real uprobe, is decoded by the consumer's own arm, is scoped to the producer's pid from the
batch header and reaches the Timeline through the ordinary sink. It asserts
`Stats.GraphExecutions == 2`, `Stats.GraphExecutions == Snapshot.GraphExecutions` (loss between
the wire and the join), one graph process, nothing unscoped, every execution marked, **no
sample left claiming `gpu_pc_attrib="exact"`**, `gpu_graph_refused="true"` on every projected
sample, and the two `joinhealth` strings. That test **compiles, vets and lints** here and
**cannot run** — see below.

Assertion 12's `Undecoded == 4` became `Undecoded == 0` plus the exact four-record partition,
for the reason given above.

### Mutation checks (each applied, run, reverted)

| mutation | result |
| --- | --- |
| `isGraphRefusedLocked` always returns false | 4 gate sub-assertions FAIL |
| `Select` returns `PCSamplingContinuous` instead of refusing (the silent downgrade) | 2 gate sub-assertions FAIL |
| `Snapshot` guard relaxed from `== PCAttribExact` to `!= ""` (ranks over Tier B) | `does-not-touch-module-keyed-attribution-through-the-join` FAILS |
| the `joinhealth` anomaly suppressed | 2 gate sub-assertions FAIL |
| `noteDropLocked` treats every class as unconsumed | 3 consumer tests FAIL |

---

## Cannot verify

**Everything below has not been executed. `CapEff: 0`, no GPU, no passwordless sudo, and no
`CUpti_` code path in this repository has ever run on this machine.**

1. **The size of the pre-refusal window on real hardware — the item this issue explicitly leaves
   outstanding for the RTX 3090.** Task 10 item 13 established the mechanism (the activity path,
   drained on a ≤100 ms tick) but not the magnitude: how many kernels, how many bursts and how
   much wall time elapse between a graph-using process's first graph kernel and the agent's
   first `gpu_dropped_v1 / GRAPH_EXEC` record. On this tree the answer is "it costs nothing for
   a driver that snapshots once", which is both drivers here — but that is an argument, not a
   measurement, and it says nothing about a periodic driver. **The measurement to take:** run
   `cmd/gpu-cuda-profile` under Tier A against a graph-using workload, and compare
   `Snapshot.GraphExecutions` against the count of executions whose `StartNs` precedes the first
   graph report's arrival. Nothing here can produce that number.
2. **`TestStubDrivesPCSamplingToPprofWithoutAGPU`** — compiles, vets and lints; skips here for
   want of `cap_bpf,cap_perfmon,cap_checkpoint_restore`. The new end-to-end graph assertions in
   it have never executed. What *has* executed, unprivileged, is every product hop they cover
   except the BPF attach and the ringbuf: the decode arm (`gpuprobe/consumer_test.go`, against
   synthetic `kindDropped` wire bytes built to the same ABI), the sink, the Timeline latch, the
   `Snapshot` marking, the labels and `joinhealth`.
3. **Whether the CUPTI adapter's `g_graph_exec_reported` delta actually reaches the wire at the
   rate assumed.** `CountingSink.EmitGraphExecutions` is unbounded partly on the argument that
   the volume is a handful of records per second per process; an adapter that emitted per
   execution would make that argument false. The code says it emits deltas
   (`shim/nvidia/cupti_adapter.cc`), and no run has confirmed it.
4. **Whether `CUpti_ActivityKernel12.graphId` is non-zero for every graph-launched execution**,
   or only for some — e.g. whether it survives graph instantiation and update. A partial signal
   would still arm the refusal (one report is enough) but would make `GraphExecutions` an
   undercount rather than a count.
5. **Whether the producer's burst controller and this agent's withdrawal agree on hardware.**
   Both refuse independently and neither reads the other; a disagreement (the producer still
   bursting while the agent has withdrawn, or the reverse) would show up as
   `ExecutionsSerialized > 0` alongside `ExecutionsGraphRefused > 0` for windows that opened
   *after* the first report. Nothing here can produce that combination.

**Not verifiable at all:** whether a workload's graph usage is representative. Inference serving
uses graphs pervasively, which is precisely why the plan's risk section says this refusal
shrinks Tier A's addressable market to nearly nothing in the workloads that most want it. That
is a product fact this commit makes *visible*; it does not change it.

### Outstanding, and named rather than hidden

`PCSamplingRequest.GraphExecutionsObserved` **has no in-tree writer**. No driver here can know,
on a first run against an unknown process, that it uses graphs — the knowledge arrives only
after the run that discovers it. The field is kept for the same reason `TimelineConfig.Modules`
is (finding 2 of the Task 13 report): a known, named hop with both ends built, awaiting a caller
that has the information — a supervisor, a second profiling round, or an operator acting on the
previous run's anomaly, for whom `Snapshot.GraphExecutions > 0` is the value to pass. Its doc
comment says exactly this. The mid-run half needs no such caller and is fully live.

---

## Verification actually run

```
make -C shim                    OK
make -C shim test               OK
make -C shim check-fpless       OK
make -C shim check-cubin-defer  OK - all 5 deferrals still refuse to compile
make -C shim nvidia             OK (CUDA 13.3, real libcupti)
go build ./... && go vet ./...  clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1     all pass
go test ./gpu/ ./gpuprobe/ -race -count=4              ok gpu 23.0s, gpuprobe 19.6s
~/go/bin/golangci-lint run --timeout=5m                0 issues
```

`git diff --stat`: 17 files changed plus the new `gpu/graphrefusal_test.go`. No file under
`shim/`, `bpf/` or `cmd/` changed.
