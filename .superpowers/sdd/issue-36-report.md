# Issue #36 — `LaunchCache` and `Timeline.pending` were PID-blind, and sampled stacks made that dangerous

Branch `fix/gpu-join-pid`, worktree `.worktrees/gpu-join-pid`, cut from `origin/main`
(`35d5d496`). One commit. Not pushed, no PR.

---

## 1. The design decision, and why

### 1.1 What was wrong

`gpu.LaunchCache.byCorr` and `gpu.Timeline.pending` are both `map[CorrelationID]…`,
and `CorrelationID` was `{Backend, Value}` — no process. The probes are attached with
`uprobe_multi` against the shim **file**, so every process that maps it feeds one
`Consumer` and one `Timeline`, and `Config.PID == 0` (system-wide) is the documented
default. Vendor correlation counters restart from a low value in every process
(CUPTI's `correlationId` is a process-wide counter; spec §6.3 finding 4), so two
profiled processes collide within the first handful of launches.

Since Phase 4a a launch carries a symbolized CPU stack resolved against
`/proc/<pid>/maps` of the process that produced it. So a mis-join did not merely swap
metadata — it attributed one process's measured GPU time to a call path in a
different address space, and reported it as `JoinExact`, i.e. vendor-provided truth.

The consumer's own side tables were fixed to `launchKey{pid, corr}` in `63e9f610`.
The timeline underneath them was not.

### 1.2 What I did

Put the process **inside** `CorrelationID`:

```go
type CorrelationID struct {
	Backend GPUBackendID `json:"backend"`
	PID     uint32       `json:"pid,omitempty"`
	Value   string       `json:"value"`
}
```

I implemented the issue's suggested shape rather than an alternative, and I agree with
its reasoning: the alternative — a `PID` field on `GPUKernelExec`/`GPUPCSample` plus a
`pid` argument on every join — puts the burden on each *join site* to remember, which
is exactly the failure this bug is. Inside the id, the compiler enforces it at every
*construction* site, and there is nothing left to remember at the four map lookups.
`GPUKernelExec` and `GPUPCSample` still carry no `PID` field of their own; their
correlation is where their process identity lives, and that is documented on both
types.

Consequences, all in the diff:

- **`LaunchCache` and `Timeline.pending` needed no code change at all.** Their keys are
  `CorrelationID`; widening the type widened the key. That is the whole point of the
  shape, and it is why the fix is small.
- **`Consumer.correlationOf` now takes the pid**: `correlationOf(pid uint32, v uint64)`.
  It is the single construction point on the producing side, and it cannot be called
  without naming a process. Both call sites pass `b.PID`, the batch header's — the
  process that fired the probe, already on the wire, already the field
  `LaunchContext.PID` is filled from.
- **`gpuprobe.launchKey` is deleted.** With the pid in the id, `launchKey{pid, corr}`
  was two copies of the same fact that could drift apart — a redundant key that
  silently misses when its halves disagree is a new failure mode, not defence in
  depth. `pendingStacks` and `deferredLaunches` now key on `gpu.CorrelationID`
  directly, and `attachSampledStackLocked` builds its key through the same
  `correlationOf` as the batched twin, so the two keys are equal by construction
  rather than by two hand-written literals agreeing. `launchKey`'s doc comment (which
  was the best statement of this hazard anywhere in the tree) is preserved, partly on
  `CorrelationID` and partly above `pendingStacks`.

### 1.3 The one genuinely new hazard, and the guard for it

Adding a field to `CorrelationID` breaks `correlation != CorrelationID{}` as the test
for *"did this record carry a correlation at all"*. That test is load-bearing:
`Timeline.Snapshot` uses it to route a correlation-less execution to the heuristic
join and an execution whose correlation merely **missed** the cache to unattributed
(review Critical 2, spec §13). A producer that knows the pid but got no vendor
correlation would now build a non-zero `CorrelationID` with an empty `Value` and be
read as carrying a real, joinable id.

So the predicate is now explicit:

```go
func (c CorrelationID) Present() bool { return c.Value != "" }
```

and **all four** sites that ask the presence question use it: `Timeline.Snapshot`,
`Consumer.admitLaunchLocked`, `projectionLabels` (`gpu/projection.go:214`, which had
been asking it inline as `Correlation.Value != ""` — correct, but the duplicated
literal this change exists to remove), and the conformance harness's
`assertHeuristicOnlyForCorrelationlessExecs`.
`TestCorrelationCarryingOnlyAProcessIsNotPresent` pins it, including end-to-end
through `Timeline`: an exec with a pid and no value must reach the heuristic path.

`correlationOf` deliberately still returns the **whole** zero value for wire value 0,
not one carrying only the pid, so the old `== CorrelationID{}` reading and the
`Present()` reading agree on exactly those records.

---

## 2. Pre-fix failure output of the regression tests

Written first, run against the unfixed keying.

**Reproduction recipe.** Both transcripts below are from a scratch worktree checked
out at `origin/main` (`35d5d496`) — genuine pre-fix code, not this branch with a
patch reverted:

```
git worktree add /tmp/prefix-check origin/main --detach
```

then copy in `gpu/crossprocess_test.go` and append the `execBatchWith` helper plus
`TestSameCorrelationInTwoProcessesJoinsInTheTimeline` to
`gpuprobe/consumer_test.go`. Two edits are needed to make the new tests compile
against a `CorrelationID` that has no `PID` field:

- `corrFor` drops the field (`_ = pid`) — which is not a contrivance, it is precisely
  what `main`'s type forces on every caller, and is the bug;
- `TestCorrelationCarryingOnlyAProcessIsNotPresent` is omitted, since it exercises
  `Present()`, an API this change introduces rather than a behaviour it repairs.

Everything else — test bodies, assertions, messages — is byte-identical to what is
committed here.

**An earlier draft of §2.2 quoted a run made by reverting `correlationOf` on this
branch. That transcript was wrong and has been replaced.** Because this branch also
deletes `gpuprobe.launchKey`, reverting `correlationOf` alone reverts the consumer's
side tables *as well as* the timeline's key, producing a state strictly worse than
`main` (both processes lose their stacks) rather than the pre-fix keying. The bug's
direction was right, but the output was not reproducible as written. What follows is
what `origin/main` actually prints.

### 2.1 `./gpu/` at `origin/main` — four failures, one pass

```
=== RUN   TestLaunchCacheKeepsTwoProcessesSameCorrelationApart
    crossprocess_test.go:86: Not equal: expected: 2   actual: 1
        	Messages:   	two processes, two launches: one must not overwrite the other
--- FAIL: TestLaunchCacheKeepsTwoProcessesSameCorrelationApart (0.00s)
=== RUN   TestTimelineJoinsExecutionToItsOwnProcessLaunch
    crossprocess_test.go:123: Not equal: expected: 0x1092   actual: 0x14e9
        	Messages:   	pid 4242's execution joined a launch from pid 5353
    crossprocess_test.go:125: Not equal: expected: []string{"a_work"}   actual: []string{"b_work"}
        	Messages:   	pid 4242's GPU time must be attributed to the call path pid 4242 ran
    crossprocess_test.go:132: Not equal: expected: 0x2   actual: 0x1
        	Messages:   	two distinct launches were matched, not one launch matched twice
    crossprocess_test.go:134: Should be zero, but was 1
--- FAIL: TestTimelineJoinsExecutionToItsOwnProcessLaunch (0.01s)
=== RUN   TestTimelinePendingPCSamplesStayWithTheirProcess
    crossprocess_test.go:158: "[{{cupti 7} { 0} unknown-clock-domain-0 25 2730  1} {{cupti 7} { 0} unknown-clock-domain-0 26 3003  1}]" should have 1 item(s), but has 2
        	Messages:   	the execution starting at 20 must get exactly its own process's sample
--- FAIL: TestTimelinePendingPCSamplesStayWithTheirProcess (0.00s)
=== RUN   TestSingleProcessJoinsAreUnaffected
--- PASS: TestSingleProcessJoinsAreUnaffected (0.00s)
=== RUN   TestExecutionWithNoLaunchInItsOwnProcessIsUnattributed
    crossprocess_test.go:217: Expected nil, but got: &gpu.GPUKernelLaunch{
        	  Correlation:gpu.CorrelationID{Backend:"cupti", Value:"7"}, KernelName:"hot_kernel", TimeNs:0xa,
        	  Launch:gpu.LaunchContext{PID:0x1092, TID:0x1092, TimeNs:0xa,
        	    CPUStack:[]pprof.Frame{pprof.Frame{Name:"a_work", …}}}}
        	Messages:   	pid 5353's execution must not borrow pid 4242's launch, or its call stack
    crossprocess_test.go:221: Not equal: expected: 0x1   actual: 0x0
        	Messages:   	the miss must be counted, not absorbed silently
    crossprocess_test.go:223: Should be zero, but was 1
        	Messages:   	a cross-process match must never be reported as vendor-provided truth
--- FAIL: TestExecutionWithNoLaunchInItsOwnProcessIsUnattributed (0.02s)
FAIL
FAIL	github.com/dpsoft/perf-agent/gpu	0.042s
```

`0x1092` is 4242 and `0x14e9` is 5353. In the second test both executions joined pid
5353's launch and took pid 5353's stack, because pid 5353's `Put` had *replaced* pid
4242's entry. The third is the same collision in `Timeline.pending`: both samples
landed in one bucket and were handed wholesale to whichever execution drained first.

The fourth is the sharpest of the four, and the earlier draft of this report
undercounted the evidence by omitting it. With **only pid 4242 having launched at
all**, pid 5353's execution was handed pid 4242's launch object — including
`CPUStack:[{Name:"a_work"}]`, a symbol resolved against `/proc/4242/maps` — and it was
reported as `ExactExecutionJoinCount == 1` with `UnmatchedExecutionCount == 0`. Every
honesty signal read clean while the output was fabricated.

`TestSingleProcessJoinsAreUnaffected` passing here is the point of it: it is the
control, and the fix must not move it (§4).

### 2.2 `./gpuprobe/` at `origin/main` — the end-to-end run

`TestSameCorrelationInTwoProcessesJoinsInTheTimeline` drives two pids' sampled, launch
and exec batches through a real `Consumer` into a real `gpu.Timeline`:

```
--- FAIL: TestSameCorrelationInTwoProcessesJoinsInTheTimeline (0.01s)
    consumer_test.go:2568: Not equal: expected: 0x1092   actual: 0x14e9
        	Messages:   	pid 4242's execution joined a launch from pid 5353
    consumer_test.go:2570: Not equal: expected: []string{"fn_1000"}   actual: []string{"fn_2000"}
        	Messages:   	pid 4242's GPU time must carry the call path captured in pid 4242, not the other process's
    consumer_test.go:2574: Not equal: expected: 0x2   actual: 0x1
        	Messages:   	two distinct launches were matched, not one matched twice
    consumer_test.go:2577: Should be zero, but was 1
FAIL
FAIL	github.com/dpsoft/perf-agent/gpuprobe	0.010s
```

This is the clearest statement of the defect, and worth reading closely for **what
does not fail**. The assertion on pid 5353's stack passes: `main`'s consumer-side
`launchKey{pid, corr}` (commit `63e9f610`) did its job, so pid 5353's launch reached
the sink carrying its own `fn_2000`. The corruption is entirely in the layer below —
pid 4242's execution was shown running `fn_2000`, a symbol from pid 5353's address
space, because the timeline's `LaunchCache` had already discarded pid 4242's launch as
a "replacement". That is exactly the issue's framing: the same bug one layer down,
with the layer above already fixed and unable to help.

### 2.3 The full set of new tests

| test | file | what it pins |
|---|---|---|
| `TestLaunchCacheKeepsTwoProcessesSameCorrelationApart` | `gpu/crossprocess_test.go` | the storage half: two entries, `Replaced == 0`, each `Get` returns its own process's launch |
| `TestTimelineJoinsExecutionToItsOwnProcessLaunch` | `gpu/crossprocess_test.go` | the join half: both exact, each carries its own process's stack, `MatchedLaunchCount == 2` |
| `TestTimelinePendingPCSamplesStayWithTheirProcess` | `gpu/crossprocess_test.go` | `Timeline.pending`: each execution gets exactly its own process's PC sample |
| `TestExecutionWithNoLaunchInItsOwnProcessIsUnattributed` | `gpu/crossprocess_test.go` | the honest miss: a cross-process value now misses, degrades to unattributed, and is **counted** — never repaired by the heuristic |
| `TestCorrelationCarryingOnlyAProcessIsNotPresent` | `gpu/crossprocess_test.go` | `Present()`, incl. through `Timeline`: pid + empty value ⇒ heuristic path, marked as a guess |
| `TestSingleProcessJoinsAreUnaffected` | `gpu/crossprocess_test.go` | non-regression, see §4 |
| `TestSameCorrelationInTwoProcessesJoinsInTheTimeline` | `gpuprobe/consumer_test.go` | consumer → timeline end to end |

All are pure unit tests: no privileges, no BPF attach, no GPU.

### 2.4 Overlap with `gpuprobe/sampledstacks_test.go` — asked for explicitly

**There is no overlap, and the file's own name is slightly misleading about where its
cross-process coverage lives.** `sampledstacks_test.go` holds three tests, all about
the `pendingStacks` order-slice bound (`TestParkedStackOrderStaysBounded…`,
`TestOrderReclamationPreservesEvictionOrder`); none of them involves two processes.
The consumer-side cross-process tests the issue refers to are in
**`gpuprobe/consumer_test.go`**: `TestSameCorrelationInTwoProcessesKeepsItsOwnStack`
(:1684) and `TestHeldLaunchDoesNotTakeAnotherProcessesStack` (:1716).

Those two are the model I followed, and their coverage stops one layer above this bug:
they assert what the **`Consumer` emitted to its sink** (each launch keeps its own
stack), and both already passed on `main`. Neither builds a `Timeline`, so neither can
see a launch that survives the consumer intact and is then handed to the wrong
execution downstream. They still earn their place — they cover `pendingStacks`/
`deferredLaunches`, which this change rekeyed — and they pass unchanged, which is
useful evidence that collapsing `launchKey` into `gpu.CorrelationID` preserved their
behaviour exactly. The new end-to-end test is deliberately the same scenario carried
one layer further, into the join.

---

## 3. Serialization — every site and its fate

I searched the whole tree for `json.Marshal`/`Unmarshal`/`NewEncoder`/`NewDecoder`
and for every importer of `github.com/dpsoft/perf-agent/gpu`.

| site | what it serializes | fate |
|---|---|---|
| `gpu/types.go` `GPUCapability.MarshalJSON` / `UnmarshalJSON` | a capability name string | untouched. Does not involve `CorrelationID`. |
| `gpu/types.go` `ClockDomain.MarshalJSON` / `UnmarshalJSON` | a clock-domain name string | untouched. Same. |
| `bench/internal/schema/schema.go` | the benchmark `Document` (compile timings) | untouched. Does not import `gpu`. |
| `unwind/ehcompile/ehcompile_test.go` | a CFI-row golden fixture | untouched. Does not import `gpu`. |

**No code anywhere in the repository marshals or unmarshals `CorrelationID`,
`GPUKernelLaunch`, `GPUKernelExec`, `GPUPCSample` or `Snapshot`.** The `json:` tags on
those types are a declared contract for a serialized snapshot that nothing produces or
consumes yet. There are no JSON fixtures in the tree (`find -name '*.json'` returns
only `.claude/settings.json`), and no replay backend exists — `BackendReplay` is a
declared constant with no implementation.

The `Snapshot` *fields* are likewise unchanged in shape — no field added, removed or
retyped — though two of them will read differently on a multi-process run now that
launches no longer collide into one entry: `LaunchCacheStats.Replaced` falls and
`EvictedCapacity` rises. See §5.2.

**So no wire or on-disk format changes.** For completeness, if that contract is
exercised later: `pid` is added as `"pid,omitempty"`, which is additive in both
directions — a pre-change document decodes with `PID == 0`, and a post-change document
read by a pre-change decoder ignores the unknown key. A document written before this
change and replayed after it would join as it always did, because all its correlations
share `PID == 0`.

Two adjacent boundaries I checked and did **not** change:

- **The USDT wire ABI** (`internal/gpuabi`, `shim/core/usdt_abi.h`, the `.bpf.c`
  producer) is untouched. The pid is not a new field on the wire: it already travels
  in the batch header (`batch_hdr` offset 16) and was already being read into
  `LaunchContext.PID`. This change only stops discarding it on the execution and
  correlation path. No `make generate`, no bytecode change.
- **The pprof label `gpu_correlation`** still formats as `backend:value`, with no pid.
  Its emit condition now goes through `Correlation.Present()` rather than an inline
  `Value != ""`, which is the same predicate and the same output.
  Adding the pid there would change profile output for a diagnostic label, which is
  outside this issue. The consequence is worth recording: in system-wide mode two
  processes' executions can still show the *same* `gpu_correlation` string. That is an
  ambiguous label, not a misattribution — each sample now carries its own process's
  stack and tags — but if `gpu_correlation` is ever used to group across a
  system-wide profile it will over-group, and the fix would be to add the pid to the
  label (or a `gpu_pid` label) in its own change.

---

## 4. Single-process non-regression evidence

Asked for as proof, not assumption. Three independent pieces:

1. **`TestSingleProcessJoinsAreUnaffected`** (`gpu/crossprocess_test.go`) drives 200
   launches and 200 executions through one pid and asserts every one joins exactly,
   pairs with its *own* correlation value and its own stack, and that
   `UnmatchedExecutionCount`, `HeuristicExecutionJoinCount`, `UnmatchedLaunchCount`
   and `LaunchCache.Replaced` are all zero. **It passes both before and after the
   change** — see the `--- PASS` line in §2.1, in the same run where the other three
   failed. That is the point: it is the control, and the fix must not move it.
2. **The argument it encodes.** With `Config.PID != 0` only one process's probes are
   attached, so every correlation reaching the cache carries the same constant `PID`.
   A map keyed on `{Backend, PID, Value}` with `PID` constant partitions identically
   to one keyed on `{Backend, Value}` — same hits, same misses, same eviction order
   (order is by `orderedFIFO` insertion sequence, which the key type does not affect).
3. **The pre-existing suite is itself a single-process corpus.** Every test in
   `gpu/timeline_test.go`, `gpu/launchcache_test.go`, `gpu/projection_test.go` and
   `gpu/conformance_test.go` builds correlations with no pid, i.e. `PID == 0`
   throughout — the degenerate single-process case. All pass unchanged, including the
   conformance harness's five join invariants and its loss-accounting reconciliation.
   The only pre-existing assertions I edited are the two that named the *old shape*
   rather than the behaviour (`assertHeuristicOnlyForCorrelationlessExecs`'s
   `== CorrelationID{}` → `Present()`, and two `TestApplyBatchNormalizes*` correlation
   assertions, both **strengthened** to require the batch pid rather than relaxed).

---

## 5. Metric integrity and capacity — what these counters read when joins fail

### 5.1 Join counters

The project's stated pattern (six defects that were counters reading green exactly
when things were worst) applies squarely here, so:

**This change adds no counter and removes none.** It changes *which* counter a
cross-process pair lands in, and that is the substance of the fix.

- **Before:** a cross-process pair was an `ExactExecutionJoinCount` — reported as
  vendor-provided truth. §2.1 shows this: with two executions and two launches, the
  pre-fix run reported `ExactExecutionJoinCount == 2` and both were wrong.
  `HeuristicExecutionJoinCount` was 0 and `AmbiguousHeuristicMatchCount` was 0, so
  every "is the join honest" signal read clean.
- **After:** an execution whose correlation exists only in another process misses the
  cache and lands in `UnmatchedExecutionCount`, and the view carries
  `gpu_join="unmatched"` with `Launch == nil`.
  `TestExecutionWithNoLaunchInItsOwnProcessIsUnattributed` asserts exactly that,
  including that `ExactExecutionJoinCount` stays 0 and the heuristic does not run.

The one signal that *was* available pre-fix is worth naming, because it is why this
went unnoticed: `MatchedLaunchCount` counts **distinct** launches matched, so the
pre-fix run reported `MatchedLaunchCount == 1` and `UnmatchedLaunchCount == 1` for two
executions. That is a real signal — but it is indistinguishable from the ordinary,
benign case of a launch whose execution simply had not arrived in this snapshot
window, which happens constantly. It could not have raised an alarm on its own. The
new tests assert `MatchedLaunchCount == 2` precisely so that signal is pinned.

There is no counter for "this execution missed because its correlation belongs to
another process", and I did not add one: the cache cannot distinguish that from "the
launch aged out" without retaining evicted keys, so such a counter could only be
guessed at — the exact anti-pattern the note warns about.

### 5.2 Capacity — a real consequence of this change

**This change makes the bounded stores fill faster in system-wide mode, and the report
should say so plainly.**

Both bounds are global and unscaled: `LaunchCache` defaults to 65536 entries
(`defaultLaunchCacheCapacity`, `gpu/launchcache.go:46`), and `Timeline.pendingCap`
reuses that same normalized capacity for the pending-PC-sample map's cardinality.
Neither is per-process, and neither is derived from how many processes are being
profiled.

Pre-fix, two processes' colliding launches **collapsed into one entry** — the second
`Put` overwrote the first and was tallied as `Replaced`. Post-fix they correctly
occupy two. So an N-process run now reaches capacity roughly N× sooner than it did,
and evicts entries that previously survived. Concretely: the storage that used to look
adequate was partly an illusion created by the very collision this change fixes.

Three reasons this is a consequence to record rather than a regression to fix:

1. **What survived before was the wrong entry.** A cache that fits more launches by
   silently discarding one process's launch in favour of another's is not "holding
   more", it is holding the wrong things and mis-joining them. Correct eviction under
   pressure is strictly better than incorrect retention.
2. **Nothing is silent.** Every eviction path is already counted and reaches the
   serialized `Snapshot`: `LaunchCacheStats.EvictedCapacity` and `EvictedHorizon` for
   launches, `TimelineDropStats.EvictedPendingSamples` for pending samples. The
   pressure shows up as a rising eviction count, not as a mystery drop in attribution.
   The counter that *falls* is `Replaced`, which pre-fix was quietly absorbing
   cross-process collisions and reading as benign correlation reuse.
3. **An evicted launch degrades honestly.** Its execution misses the cache and lands in
   `UnmatchedExecutionCount` with `Launch == nil` and `gpu_join="unmatched"` — the
   review Critical 2 path, which is exactly the behaviour
   `TestTimelineDegradesWhenLaunchEvicted` and
   `TestConformanceEvictedLaunchDegradesToUnattributed` already pin.

**There is no per-process fairness, and that is the part worth watching on a real
multi-process run.** Eviction is global FIFO by insertion sequence, so a busy process
can push a quiet process's launches out of the cache before their executions arrive.
The quiet process then loses attribution it would have kept if it were profiled alone.
This is not new — the FIFO was always global — but this change is what makes multiple
processes actually *occupy* the cache simultaneously, so it is the change that makes
the unfairness reachable. Sizing `LaunchCacheConfig.Capacity` for the expected process
count, or making eviction process-aware, would be the remedy; both are design
decisions well outside a keying bugfix, and neither is needed until a real
multi-process run shows `EvictedCapacity` climbing.

---

## 6. Known residual, deliberately not fixed

**The heuristic join is still process-blind, and cannot be fixed here.**
`candidateGroupKey` groups launches by `(queue, kernel name)` with no pid. It cannot
carry one: the heuristic runs only for an execution that supplied **no correlation at
all**, and the correlation is now the only place an execution's process identity
lives — so a correlation-less execution has no pid to group by. Closing it needs a
process field on `GPUKernelExec` itself, which no producer in the tree would populate:
the one shipping backend (`gpuprobe`) supplies a correlation on every launch and
execution, as spec §6 requires without exception, so nothing reaches that path today,
and `BackendLinuxDRM` has no implementation. A field nobody writes is the other half
of the metric-integrity anti-pattern, so I documented the gap at
`candidateGroupKey` rather than adding one.

Out of scope and untouched, as instructed: **#49** (CFI tables not yet registered),
**#42** (CI flakiness).

---

## 7. Verification run

Re-run in full after the review amendments (including the `projection.go` change):

```
go build ./...                                                    ok
go vet ./...                                                      ok
go test ./gpu/ ./gpuprobe/ ./internal/... ./unwind/... -count=1    all ok
go test ./gpu/ ./gpuprobe/ -race -count=1                          ok  (gpu 5.370s, gpuprobe 1.193s)
~/go/bin/golangci-lint run --timeout=5m                            0 issues.
```

Plus the pre-fix runs of §2.1 and §2.2, against a scratch worktree at `origin/main`.

---

## 8. Cannot verify

- **Anything requiring privilege.** `CapEff: 0` in this environment: no BPF program
  load, no `uprobe_multi` attach, no ringbuf. `gpuprobe/gate_test.go` and
  `gpuprobe/attach_test.go`'s attach paths skip on the capability gate; they compile
  and their non-privileged assertions pass, but the attach itself never ran here.
- **The live two-process RTX 3090 run.** The scenario this issue is really about —
  two real CUDA processes mapping the shim, each with its own correlation sequence,
  producing a profile in which each process's GPU time sits under its own call paths —
  has only been exercised against synthetic batches. The end-to-end test in §2.2 goes
  through the real `Consumer.applyBatch` and the real `Timeline`, but its input is
  hand-built wire bytes, not a shim. The one thing I would specifically watch on that
  run: `JoinStats.UnmatchedExecutionCount` should stay at its pre-change level. If it
  *rises* on a single-process run, something is populating the launch's and the
  execution's correlation pid from different sources; §4 argues it cannot, but that
  is an argument, not a measurement.
- **PC-sample behaviour with real CUPTI.** `Timeline.pending` is now process-keyed,
  but the shipping collection mode supplies **no** correlation on PC samples (spec
  §6.3 finding 3), so on the real hardware every PC sample still lands on the
  not-`Present()` key regardless of process. The PC-sample test in §2.3 exercises the
  correlated path, which today only a hypothetical kernel-serialized collector would
  produce. This change does not make PC-sample attribution worse, and does not make it
  work either.
- **Nothing was skipped or weakened to get a green local run.** No assertion was
  relaxed; the two pre-existing assertions I edited were both strengthened (§4, item 3).
