# Issue #52 — the heuristic GPU join path was process-blind, guarded only by shim contract

Branch `fix/heuristic-process-guard`, worktree `.worktrees/heuristic-guard`, cut from
`origin/main` (`a044264b`). One commit. Not pushed, no PR.

---

## 1. The finding that decided the design

The issue says:

> It also cannot be fixed the way #36 fixed the exact path: the heuristic runs
> precisely when there is no correlation, and after #36 the correlation is the only
> place an execution's process identity lives. There is no pid to group by. Closing it
> needs a pid field on `GPUKernelExec` itself — which no current producer would write.

**That premise is wrong, and the tree already contained the proof.**

`CorrelationID` is `{Backend, PID, Value}`, and `Present()` is:

```go
func (c CorrelationID) Present() bool { return c.Value != "" }
```

`PID` and `Value` are independent. A record can name its process and carry no vendor
correlation. That is not a hypothetical shape — `gpu/crossprocess_test.go`'s
`TestCorrelationCarryingOnlyAProcessIsNotPresent` (added by #36) constructs exactly it,
`CorrelationID{Backend: BackendLinuxDRM, PID: 4242}`, and asserts it takes the
heuristic path. And `gpu/projection.go`'s `labelPID` already *reads* it, falling back
to `view.Exec.Correlation.PID` for an execution with no launch.

So the pid option 2 wanted on `GPUKernelExec` was already there, on the correlation,
where #36 put it. Option 2's semantics were available at option 1's cost, and that is
what I built.

## 2. Option chosen

**Option 2, realised without the event-type change #36 avoided.** The heuristic join is
process-qualified end to end:

- `candidateGroupKey` gains a `pid`, so a launch is only a candidate for an execution
  in the same process.
- `Timeline.Snapshot` refuses the heuristic outright when `exec.Correlation.PID == 0`.
  An execution that names neither a correlation nor a process cannot be shown to share
  one with any launch, so it is not joined at all.
- The launch's side of the pid is `launchProcessID(l)` — `LaunchContext.PID`, falling
  back to `Correlation.PID`. Two fields exist and producers populate them
  independently; `gpuprobe` sets both from the batch header.

A refused execution is **not dropped**. It degrades to unattributed: its measured GPU
time stays in the profile under `FrameLaunchUnsampled`, carrying no CPU stack, no
`pod_uid` and no `container_id`, and lands in `UnmatchedExecutionCount` like any other
miss. Two new counters say why.

A heuristic join that *does* happen is still labelled and counted as a guess —
`gpu_join="heuristic"`, `HeuristicExecutionJoinCount`, `Ambiguous` — unchanged. The
guard narrows *which* guesses are permitted; it never promotes one.

### What I rejected, and why

**Option 1 as written (reject or count-and-drop at the consumer boundary).** Rejected
in its dropping form. The parent asked directly whether count-and-drop is honest
enough; it is not. An execution is a *measurement* — a real kernel that really ran for
a real duration. Dropping it removes GPU time from the profile, which is a worse lie
than removing its stack: the profile would under-report total GPU time with nothing in
the output to say so. Count-and-degrade-to-unattributed is the honest form, and that is
what the timeline now does by construction. The degradation lives in `Timeline`, not in
`gpuprobe`, because `gpuprobe` has no way to express "emit this but do not guess at
it" — the join decision is the timeline's, so the guard belongs there. Option 1 also
only ever covers one producer; a guard in `Timeline` covers every producer, including
the ones that do not exist yet, which is the entire premise of the issue.

**Option 1's counting half I did keep**, in a narrower form — see §4.

**Option 3 (restrict candidates when `Config.PID != 0`).** Rejected as redundant and as
the wrong shape. Redundant: with the pid in the group key, single-process mode is
already the case where every candidate is in the exec's own process, so option 3's
safety falls out for free — and it falls out for *system-wide* profiling too, which
option 3 explicitly could not do. Wrong shape: `gpu/` has no access to the agent's mode
and `projection.go` documents a deliberate decision not to grow one ("This package has
no access to the agent's mode and should not grow one for this"). Plumbing `Config.PID`
into `TimelineConfig` to buy a subset of what the group key already buys is cost with
no return.

**Changing `gpuprobe.correlationOf` to preserve the pid on a zero wire correlation.**
Tempting — it would let a contract-violating `gpuprobe` exec still join correctly
within its own process instead of degrading. Rejected: `correlationOf` is shared with
the launch, PC-sample and sampled-stack paths, `Timeline.pending` is keyed on the whole
`CorrelationID`, and the function's doc comment justifies returning the whole zero
value so that the `== gpu.CorrelationID{}` reading and the `Present()` reading agree.
Changing it would alter pending-sample bucketing on the path #68 just stabilised, to
improve a case that must never occur. The safe direction is the one it already takes:
pid 0 → refused → unattributed.

**Did not touch the exact-join path.** `TestExactJoinsNeverEnterTheHeuristicCounters`
pins that an execution carrying a correlation never enters either new counter, whatever
the process layout — including #36's honest cross-process miss, which must stay
credited to the exact path and not to this guard.

## 3. Pre-change behaviour, measured

`CapEff: 0`, so no live run. The heuristic is pure Go; I ran the motivating case against
`origin/main` directly (stash, drop in a throwaway test, run, restore):

```
RESULT: pid 5353's execution joined pid 4242's launch; join="heuristic" heuristic=true
RESULT labels: gpu_join="heuristic" gpu_pid="4242" pod_uid="pod-a" container_id="c-pod-a"
RESULT frames: [a_work [gpu:launch] [gpu:kernel:hot_kernel]]
RESULT counters: heuristic=1 unmatched=0
```

One launch, from pid 4242 in pod-a. One correlation-less execution, from pid 5353. The
old code handed pid 5353's GPU time pid 4242's call stack and pod-a's `pod_uid` and
`container_id`. Note `gpu_pid="4242"`: `labelPID` prefers the launch's pid over the
execution's own, so the sample named the **wrong process as producer** even though the
execution's own correlation said 5353. The issue describes the stack and tags; the
producer label went with them.

After the change the same input yields no launch, `gpu_join="unmatched"`, no `pod_uid`,
no `container_id`, `gpu_pid="5353"`, and the GPU time intact.

The tree's own suite contained two live cross-process heuristic joins asserted as
correct, which is how thin "unreachable by convention" was:

- `TestTimelineHeuristicRespectsJoinWindow` — candidate "far" at PID 1, candidate
  "near" at PID 2, pid-less exec. It joined PID 2's launch.
- `TestProjectionEmitsGpuJoinHeuristicAndAmbiguous` — "older" at PID 1 tagged
  `pod_uid: guessed-pod`, "newer" at PID 2, pid-less exec. It joined PID 2's and was
  flagged ambiguous *against a different process's launch*.

Both are now single-process, so they test the window and the ambiguity rule rather than
smuggling a cross-process join through them.

## 4. The counter story

Eight defects on this project were counters reading green when things were worst, so
the rule for this change was: nothing about the guard may be invisible, and a guarded
path that starts firing must announce itself.

Three counters, at two independent boundaries.

**`gpu.JoinStats.CorrelationlessExecutionCount`** — every execution that entered the
heuristic path, joined or not. This is the "attempts to reach the now-guarded path"
counter the constraint asks for. It is zero on every backend shipping today (spec §6
makes a correlation mandatory) and a non-zero reading is a producer contract violation,
whoever the producer is. It deliberately overlaps `HeuristicExecutionJoinCount` and
`UnmatchedExecutionCount` and is *not* part of the three-outcome sum `joinAnomalies`
checks — documented on the field.

**`gpu.JoinStats.CrossProcessHeuristicBlockedCount`** — the load-bearing one. It counts
refusals *where a candidate actually qualified*: on a refusal, the pre-#52 grouping is
rebuilt (process dropped) and searched under the same window, and only a hit counts.
Every increment is one cross-container attribution that did not happen. Deliberately
not "every correlation-less miss" — that would read high for reasons unrelated to
processes, and "N cross-container attributions prevented" would stop meaning that.
`TestCorrelationlessExecutionIsCountedWithNoCandidateToRefuse` pins the separation, and
the two "rejects" tests assert it stays zero when causality or the queue/name filter is
what rejected the candidate, so the guard cannot take credit for other filters' work.

Keeping the old rule *executable* rather than deleting it is the mechanism that stops
the guarded path going dark. A refusal that produced no join and no counter is
indistinguishable from a workload that never had a candidate.

**`gpuprobe.Stats.ZeroCorrelationExecs`** — option 1's counting half, at the producer
boundary. The issue notes a contract-violating exec is "counted only in
`ZeroCorrelation`", but in practice that is invisible: `ZeroCorrelation` aggregates
every record kind, and a zero correlation is the documented *normal* case for PC
samples in continuous collection (spec §6.3 finding 3), so the aggregate is large on a
healthy run and a handful of bad executions vanishes inside it. The narrow counter can
be asserted zero where the aggregate cannot, and `gate_test.go` now does. Counted at
the `kindExec` site, not inside `correlationOf`, which is shared and cannot tell the
kinds apart.

**Surfaced, not just recorded.** #51's complaint is that these counters are never
printed. Both new `JoinStats` fields get a `JoinHealth` anomaly line — the one thing
both drivers do print — and `TestJoinHealthSurfacesTheHeuristicProcessGuard` asserts
the lines appear when the counters move and that a healthy snapshot stays one line.
`gpuprobe.Stats` is already printed whole via `%+v`.

I also corrected the existing heuristic anomaly line, which said "the CPU stack may be
another launch's". It now says "another launch's **from the same process**", which is
what is true after this change.

## 5. Unreachable versus merely unlikely

**Now unreachable by construction:** a heuristic join between an execution and a launch
that the timeline cannot prove share a process. Not "no producer does this" — the
lookup key makes the other process's launches unreachable, and the zero-pid case never
reaches a lookup at all. The pre-#52 grouping still exists, but only as an argument to
a counter; nothing found through it is ever attached to an `ExecutionView`.

**Still reachable, and intended:** a heuristic join *within* one process. It remains a
guess — same kernel name, same queue, most recent preceding launch inside the window —
and is still labelled `gpu_join="heuristic"` and counted. Ambiguity is now
same-process-only, which is a narrower and more meaningful signal than before.

**Merely unlikely, not closed:**

- A producer that leaves `Correlation.PID` zero on **both** the launch and the
  execution puts them both in process-group 0 — except that Snapshot refuses a zero-pid
  execution before any lookup, so they cannot join either. This is the one behaviour
  change that could cost a hypothetical future producer a join it used to get. It is
  deliberate: such a producer is claiming no process identity anywhere, which in
  system-wide mode is precisely the condition under which the guess crosses containers.
  Its remedy is one field, and `CrossProcessHeuristicBlockedCount` plus the JoinHealth
  line name that field explicitly.
- An execution whose process is known but whose own process's launch is missing, while
  another process holds a plausible one, is refused and counted — but the *converse*
  is not detectable: if a correlation-less execution's true producer were misreported
  by the backend, the guard would happily join it inside the wrong process. Nothing
  downstream of a producer can catch a producer lying about its own pid.
- `matched[match.launch.Correlation]` still keys `MatchedLaunchCount` on the launch's
  correlation, so heuristic matches against launches with a zero correlation collapse
  into one bucket. Pre-existing, unrelated to #52, untouched.
- **`launchProcessID` prefers `Launch.PID` over `Correlation.PID`, and the two could
  disagree.** A producer that populated them inconsistently — say `LaunchContext.PID`
  from the batch header but `Correlation.PID` from somewhere else — would file its
  launch under one pid and its execution (which has only `Correlation.PID`) under the
  other. The same-process join then becomes a *counted cross-process refusal*: correct
  data, refused, and `CrossProcessHeuristicBlockedCount` reading as though a
  container boundary had been defended when nothing of the sort happened. Theoretical
  today — `gpuprobe` sets both from the batch header, so they cannot diverge — but it
  is a real way for this guard to be wrong in the *safe* direction while making its own
  counter lie, and it is named on `launchProcessID`'s doc comment as well as here. A
  future producer must treat the two fields as one fact.
- **For `gpuprobe` the heuristic rung is now closed in both directions**, and anyone
  reading `HeuristicExecutionJoinCount` should know why it reads zero. The exec side
  can never reach the rung (spec §6 guarantees a correlation, and `gate_test.go` now
  asserts `ZeroCorrelationExecs == 0`); if it ever did, `correlationOf`'s whole-zero
  return means the guard refuses it anyway. So that counter can only move for a backend
  that does not exist yet. It reads zero because the path is shut, **not** because the
  guessing is going well — a distinction that matters if it is ever mistaken for live
  coverage of the heuristic. `CorrelationlessExecutionCount` is the counter to watch:
  it is what fires first if the path ever reopens.

## 6. Cannot verify

- **No hardware, no live run.** `CapEff: 0` on this box; the eBPF and probe paths were
  not exercised. Everything here is unit-level Go against `gpu/` and `gpuprobe/`'s
  wire-format decoder.
- **No end-to-end proof that a real correlation-less backend behaves as designed**,
  because there is no such backend: `BackendLinuxDRM` has no implementation and the
  ROCm adapter is unwritten. `heurExecIn`/`noCorrelation` model what one would emit;
  whether a real ROCm adapter populates `Correlation.PID` is a decision that has not
  been made yet. If it does not, it gets refusals and a counter telling it exactly what
  to set — which is the outcome I designed for, but I cannot demonstrate it against a
  producer that does not exist.
- **`CrossProcessHeuristicBlockedCount`'s "would have matched" claim is verified by
  construction, not by A/B run.** It re-runs `findLaunchHeuristic` against the pre-#52
  grouping through the same shared `groupLaunchesByTime` helper, so the two indexes
  cannot drift in their sort or bucketing rule — but it is the same *code*, not the
  same *binary*, as pre-change `main`. The §3 measurement against `origin/main` is the
  independent check on the one case that mattered.
- **Cost under a pathological all-refusal workload is reasoned, not measured.** The
  second index is built lazily, at most once per `Snapshot`, so the added cost is one
  `O(capacity)` group-and-sort on a snapshot that has at least one refusal, and nothing
  at all otherwise. `BenchmarkTimelineSnapshotAllMisses` exists and still runs, but I
  did not take before/after numbers — no correlation-less production workload exists to
  make them mean anything.

## 7. Verification run

```
go build ./... && go vet ./...                                  clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1              ok (11 packages)
go test ./gpu/ ./gpuprobe/ -race -count=4                       ok
~/go/bin/golangci-lint run --timeout=5m                         0 issues
gofmt -l gpu gpuprobe                                           clean
```

Seven existing tests failed on the first build of the guard, all of them
correlation-less executions being joined to a launch from a named process. Each was
updated to name the execution's process — which is what a real correlation-less backend
must do — and none had its assertion weakened. Two of them (§3) were cross-process
joins the suite had been asserting as correct.

I also re-checked that the two tests whose *purpose* the guard could have quietly
voided still do their job:
`TestTimelineHeuristicScalesWithSingleQueueSingleKernelName` performs 10,000 real
heuristic joins after the change, not the near-instant no-op it would have become had
the exec been left pid-less; and `BenchmarkTimelineSnapshotAllMisses` still drives the
full candidate search.

**That check is now in the suite, not in this report** (review Minor 1). The scaling
test discarded its `Snapshot()` and asserted only `elapsed < 5s`, and this change gave
it a brand-new way to go silently no-op: a guard that refuses every exec does *less*
work, finishes *faster*, and passes *more* comfortably — a performance assertion that
gets greener as the functionality disappears, which is precisely the green-when-worst
shape this project keeps finding. The snapshot is now captured and the work pinned
alongside the clock:

```go
require.Len(t, snap.Executions, misses)
assert.Equal(t, uint64(misses), js.CorrelationlessExecutionCount, …)
assert.Equal(t, uint64(misses), js.HeuristicExecutionJoinCount, …)
assert.Zero(t, js.UnmatchedExecutionCount, …)
assert.Zero(t, js.CrossProcessHeuristicBlockedCount, …)
```

Mutation-checked: forcing the process guard to refuse everything (`if execPID == 0` →
`if true`) fails the test on three assertions. The timing bound passed under that same
mutation, at 0.08s — the no-op is genuinely invisible to it, which is the whole point.

## 8. Files

| File | What changed |
| --- | --- |
| `gpu/timeline.go` | `candidateGroupKey` gains `pid`; `Snapshot` refuses a zero-pid heuristic and counts refusals; `anyProcessGroupKey` + `launchProcessID` + shared `groupLaunchesByTime` |
| `gpu/types.go` | `CorrelationlessExecutionCount`, `CrossProcessHeuristicBlockedCount`; `GPUKernelExec` doc corrected |
| `gpu/joinhealth.go` | Two new anomaly lines; existing heuristic line corrected |
| `gpu/crossprocess_test.go` | Six regression tests for #52 |
| `gpu/timeline_test.go`, `gpu/conformance_test.go`, `gpu/projection_test.go` | Correlation-less execs now name their process |
| `gpuprobe/consumer.go` | `Stats.ZeroCorrelationExecs`, counted at the `kindExec` site |
| `gpuprobe/consumer_test.go`, `gpuprobe/gate_test.go` | Counter tests; gate asserts it zero |
| `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` | §10 records that both join rungs are process-qualified |
