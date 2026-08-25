# Task 8b — the `Snapshot` join, and `gpu_pc_attrib`

Branch `feat/pc-snapshot-join`, one commit on top of Task 8a
(`feat/pc-pending-module-index`, PR #80, itself one commit off `main`).

Scope: attach correlation-less (Tier B / `CONTINUOUS`) PC samples to the
executions they belong to, through the module, and decide the
`gpu_pc_attrib` value for every PC-bearing execution. **Task 9 emits the
label; this task only decides it** — `gpu/projection.go` is untouched.

Five files: `gpu/timeline.go`, `gpu/modulestore.go`, `gpu/joinhealth.go` and
the new `gpu/pcattrib.go`, plus tests in `gpu/pcjoin_test.go`,
`gpu/modulestore_test.go` and `gpu/conformance_test.go`.

---

## 1. The join order

`Snapshot`, per execution:

1. **Exact-correlation index first, unchanged.** The existing
   `t.pending[exec.Correlation]` pass runs exactly as it did. Tier A keeps
   taking it and nothing in that path was modified — no new branch, no new
   condition, no reordering. The only addition is an `exactServed []bool`
   recording which executions it served.
2. **Module-keyed join second, over what the first pass left unserved.**
   `joinPendingModuleLocked` walks the `pendingModule` groups, resolves each
   `{PID, CubinCRC, FunctionIndex}` to a device function name through
   `ModuleStore.FunctionName`, and hands the group to an execution of that
   kernel in the same process.

Both passes run inside one hold of `t.mu`, so nothing can be created or
evicted between them, which is what makes the group identity in §5 an
identity rather than an approximation.

An execution served by the exact index is **not** a candidate for the module
join. `TestExactPathKeepsFirstClaim` pins it: a correlation-less group for
the same kernel does not pile onto an exactly-joined execution, it waits for
one of its own.

**"In the horizon" means "in this snapshot."** The candidate set is the
executions this `Snapshot` call drained. There is deliberately no time
filter between a sample and an execution's `[StartNs, EndNs]`: CUPTI PC
records carry no timestamp of their own, so `GPUPCSample.TimeNs` is the
drain time the producer stamped, and filtering executions by it would be
filtering on the wrong clock. The sample-side horizon
(`PendingSampleHorizonNs`) is what bounds how stale a group may get, and it
is unchanged.

Where more than one execution qualifies, the group goes **whole** to the
earliest by `StartNs` — not the first in ring order, which is drain order and
not run order (`TestTierBAmbiguousGoesToTheEarliestExecution` emits them
late-first to prove the difference). Splitting a group across candidates
would manufacture a distribution the data does not contain and would make
each resulting execution look individually more certain than the group is.
The label carries the doubt instead.

---

## 2. The name-match rule, and what happens when it fails

Kernel identity comes from the module. `ModuleStore` holds
`functionIndex → function name`, read out of the cubin's `.symtab`; the
execution carries `KernelName` from `gpu_kernel_name_v1`. The join is a
**plain, exact string comparison** of those two, inside a map key that also
carries the PID:

```go
type execKernelKey struct {
	pid        uint32
	kernelName string
}
```

No demangling. No prefix or suffix trimming. No normalization of any kind.
Every relaxation of this comparison is a way to attach a sample to a kernel
that merely looks related, and the plan's rule is that identity comes from
the module and never from guessing on the name string.

**When the names do not match, the group stays pending.** It is not attached
to a neighbour, not attached to the only execution in the snapshot, not
attached to anything. It remains in `pendingModule`, is counted in
`PCJoin.GroupsNoExecution`, and is eligible for a later `Snapshot` — the
execution may simply not have arrived yet. If it never becomes joinable it
ages out on the shared horizon into
`Dropped.EvictedPendingModuleSamples`, which is where the loss is counted.

`TestTierBKernelNameMismatchStaysPending` uses a name differing by one
character, in the same process, on the same queue, at a time any windowed
heuristic would have accepted — everything a "close enough" match would have
taken. Relaxing the comparison to a prefix match fails it (mutation M2).

**The PID is a field of the key, not a check beside it.** Two processes
running the identical binary produce the identical cubin CRC and the
identical function index; only the pid separates them, and issue #52's
discipline says the refusal must be structural rather than a guard someone
can edit out. `TestTierBJoinStaysWithinTheProcess` pins it; deleting the pid
from either side of the key fails it (M3).

A group whose PID is **zero** — the producer named no process — is refused
outright and counted in `GroupsNoProcess`. Every such group from every
process shares one key, so attaching one to an execution would attribute one
process's GPU samples to another's call stack on nothing but a kernel name.

---

## 3. `gpu_pc_attrib`, and the de-overloading

New type in `gpu/pcattrib.go`, new field `ExecutionView.PCAttrib`. Empty
exactly when `PCSamples` is empty (there is nothing to describe); one of the
four otherwise.

| value | set when |
| --- | --- |
| `exact` | the sample carried a correlation value and joined through the correlation index |
| `kernel` | joined through the module, exactly one execution of that kernel in the snapshot |
| `kernel-ambiguous` | joined through the module, **more than one** execution of that kernel in the snapshot |
| `kernel-multidevice` | joined through the module in a process observed running kernels on more than one device |

Ambiguity is counted over **every** execution of that kernel in the
snapshot, not only the ones the join was allowed to use: "how many
invocations could these samples have come from" is a question about the
horizon, not about which of them the exact pass happened to leave free.

Precedence is `multidevice > ambiguous > kernel > exact` (`worsePCAttrib`),
so an execution served by two groups of differing quality reports the worse
of the two rather than whichever was processed last. Attribution quality is
only ever revised downward.

### The test that pins it

`TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous`. Two executions of one
kernel, one Tier B group. It asserts:

- the receiving execution carries `PCAttribKernelAmbiguous`;
- `view.Ambiguous` is **false** on every execution;
- `projectionLabels(view)` contains **no** `gpu_ambiguous` key — asserted
  through the projection, not only the struct field, because the struct
  field is not what a consumer reads;
- `JoinStats.AmbiguousHeuristicMatchCount` is **zero**.

`ExecutionView.Ambiguous` is still set in exactly one place, the heuristic
launch-join branch, and still counted only by
`AmbiguousHeuristicMatchCount`. Making PC ambiguity also set it (mutation
M5) fails this test.

`PCAttrib` is set in `Snapshot`'s view loop in a `switch` that reads
`exactServed[i]` and `len(view.PCSamples)` and nothing else — it never reads
or writes `view.Join`, `view.Heuristic` or `view.Ambiguous`, and none of
them reads it. An execution can be joined to its launch by vendor
correlation, exactly, and still carry inferred PC samples; that is precisely
the pair of facts one boolean could not carry.

`PCAttrib.MarshalJSON` refuses anything that is not one of the four,
**including the empty value**, matching `SrcStatus`, `ClockDomain` and
`GPUCapability`. The field is `omitempty`, so a view with no PC samples never
reaches that path. `PCAttribs()` returns the four in stable order (a copy) so
Task 9's switch can be tested for exhaustiveness against the enum.

### Multi-device detection

`gpu_pc_sample_batch_v1` carries no device id and one binary has one cubin
CRC on both devices, so the samples are indistinguishable on the wire. There
is no way to make the join right; there is only a way to stop it from
looking right.

Detection is on **executions**, which do carry a device id
(`Execution.DeviceID`, falling back to `Queue.Device.DeviceID`). `EmitExec`
feeds `Timeline.devicesByPID`, which holds per process the first device id
seen and whether a different one has since arrived — a decision, not an
inventory.

It is **cumulative, not per-snapshot**: the condition is a property of the
process, and per-snapshot detection would clear the mark on exactly the
snapshots where only one device happened to report, which is this project's
recurring failure shape. It is **bounded** at
`maxTrackedDeviceProcesses = 4096`, because a system-wide profile has no
a-priori bound on distinct pids. Past the cap a new process is not admitted
and is therefore treated as single-device — the right guess, and still a
guess, so the refusal is counted in `PCJoin.DeviceTrackingCapped` and raised
as a `joinhealth` anomaly rather than absorbed.
`TestDeviceTrackingCapIsCountedNotSilent` drives it past the bound.

An untracked process answers "not multi-device". Turning every unknown into
a multidevice mark would bury the real ones.

---

## 4. `ModuleStore.FunctionName(crc, functionIndex) (string, bool)`

Added with its tests, as Task 4's report asked.

```go
func (s *ModuleStore) FunctionName(crc uint64, functionIndex uint32) (string, bool)
```

**Why `Resolve` cannot answer this.** `Resolution` carries a function name
only under `SrcResolved`, which is the four-valued status doing its job — a
name paired with `no-lineinfo` would invite a caller to emit `gpu_src_func`
for a module with no source information. But the continuous-mode chain is
`cubin_crc → module → function → KERNEL`, and it needs the name whether or
not `-lineinfo` was passed: a kernel built without it still has a name,
still runs, and still owns its PC samples. Routing attribution through
`Resolve` would make it depend on a build flag it has nothing to do with.
`TestFunctionNameWorksWithoutLineInfo` asserts both halves — the name is
returned, and the same module's `Resolve` still answers `no-lineinfo` with
no location.

`ok` is false when the CRC is not held (never arrived, or evicted), when the
bytes did not parse, or when the index is not in the symbol table. A
**damaged `.debug_line` does not make it false**: the symbol table is intact
and the kernel's identity does not come from DWARF, so attribution survives
a line table this reader cannot read even though source resolution does not
(`TestFunctionNameSurvivesADamagedLineTable`). There is never a
nearest-index guess.

It refreshes LRU recency, for `Resolve`'s reason: a module whose functions
are being joined is a module in use, and letting it age out under a burst of
unrelated loads would silently stop attribution for a live kernel
(`TestFunctionNameRefreshesRecency`).

It increments **none** of the four `Resolve*` counters. Those partition calls
to `Resolve` exactly and that identity is the store's main self-check;
folding a second entry point into it would break the identity while looking
like better instrumentation. `TestFunctionNameDoesNotDisturbTheResolveIdentity`
makes ten `FunctionName` calls, asserts `ResolveTotal() == 0`, then makes
three `Resolve` calls and asserts the identity still holds. What
`FunctionName`'s failures cost is counted at the join, in
`GroupsUnresolvedName`.

Seven tests, including `TestFunctionNameBindsTheIndexToTheRightKernel` over
the two-kernel fixture (an accessor returning a neighbouring function would
still look healthy on a single-kernel module) and
`TestFunctionNameAfterEvictionIsRefusedNotStale` (the store's no-memo
guarantee: an answer must never outlive the bytes it came from).

---

## 5. The extended reconciliation

Every PC sample the sink accepted lands in exactly one of: attributed-exact,
attributed-kernel, still-pending (in either store), or evicted (from either
store). 8a had already extended `assertPCSampleLossesAccounted` to cover both
pending stores; this task built on that rather than adding a third path.

Three identities, all in `assertPCSampleLossesAccounted` (so they run for
**every** conformance scenario) and re-asserted by
`assertPCJoinIdentities` in the new tests:

```
accepted == AttributedPCSamples
          + PendingSamples       + EvictedPendingSamples
          + PendingModuleSamples + EvictedPendingModuleSamples     (8a's, unchanged)

AttributedPCSamples == PCJoin.AttributedExact + PCJoin.AttributedKernel

PCJoin.GroupsExamined() == PCJoin.GroupsJoined + PendingModuleGroups
```

The second closes a hole the first cannot see: a module join that attached
samples while forgetting to count them would still satisfy the first
identity (the samples left the pending store and arrived on an execution)
while reporting a per-tier split that was quietly wrong.

The third is the group-level partition. Because the whole join runs under one
lock hold, every group it examined was either consumed or is still in the
store, and the three not-joined reasons — `GroupsUnresolvedName`,
`GroupsNoExecution`, `GroupsNoProcess` — partition the remainder exactly. A
refusal path that returned without incrementing any counter is the easiest
and most invisible mistake available in that function, and this is what
catches it (mutation M6).

Plus one presence invariant, `assertPCAttribAccompaniesSamples`, run over
every conformance scenario: every view holding PC samples carries one of
`PCAttribs()`, every view holding none carries the empty value. `gpu_pc_attrib`
is emitted unconditionally for `gpu_join`'s reason — an absent label must
never be readable as "exact" by a consumer that does not know to check for
its absence — and `MarshalJSON` refusing the empty value only catches that at
the serialization boundary, which not every consumer crosses.

### `TestConformancePCSampleReconciliationCoversPendingAndEvicted` grew

The existing test now also asserts its continuous-mode terms are
structurally zero (every sample there carries a correlation), which is what
keeps the two halves of the identity from being read as each other. Its
non-dead counterpart is the new
**`TestConformancePCSampleReconciliationCoversAttributedByKernel`**, which
runs the full invariant suite over `drivePCSampleMixedFateContinuous` — a
scenario with all three new buckets non-zero in one run: two samples
attributed by kernel, one still pending in an unnameable group, two evicted
by the per-group cap. `newConformanceHarnessWithTimeline` was added so a
harness can be given a module store; without one, no group can be named and
the attributed-by-kernel bucket would be a structurally unreachable dead
term.

### Counters

`Snapshot.PCJoin PCJoinStats` — nine fields, each assertable, none of which
can read green when things are worst:

- `AttributedExact` / `AttributedKernel` — the split of `AttributedPCSamples`.
- `GroupsJoined` / `GroupsUnresolvedName` / `GroupsNoExecution` /
  `GroupsNoProcess` — the partition above.
- `AmbiguousAttributions` / `MultiDeviceAttributions` — counts of
  **executions** carrying those `gpu_pc_attrib` values (the unit the label
  rides on), computed after the walk so two groups landing on one execution
  count once. `AmbiguousAttributions` is emphatically **not**
  `AmbiguousHeuristicMatchCount` and must never be merged with it.
- `MultiDeviceProcesses` — cumulative, and `DeviceTrackingCapped` beside it
  so "we found none" and "we stopped looking" stay distinguishable.

`joinhealth` gained one summary refinement (the exact/by-kernel split,
printed only when both are actually in play) and five anomalies: PC
ambiguity, multi-device processes, the device-tracker cap, unnameable
groups, and groups naming no process. `GroupsNoExecution` deliberately does
**not** raise one — a group whose execution has not landed yet is the normal
state at a snapshot boundary, exactly as `UnmatchedLaunchCount` is, and
raising it would devalue the word for the counters that mean something.
`TestJoinHealthHealthyRunIsOneShortLine` is unchanged and passes.

One drive-by fix in `joinhealth.go`: 8a's correlation-less summary clause
rendered `"1 1 kernel group"` (a `%d %s` against `plural`, which already
carries the number) and pluralized "samples" for a count of one. Corrected;
it is one line and the output is asserted by the new tests.

---

## 6. Not touched

`gpu/projection.go` — Task 9 emits the labels. `LaunchCache`. The #52
heuristic guard and `findLaunchHeuristic`. The exact-correlation pending
path in `EmitPCSample` and `Snapshot`. `ExecutionView.Ambiguous`,
`JoinStats.AmbiguousHeuristicMatchCount`, and every existing `JoinStats`
field. `internal/cubin`. The ABI.

`TimelineConfig.Modules` is nil for both shipping drivers
(`cmd/gpu-stub-profile`, `cmd/gpu-cuda-profile`) and for every backend that
does not do PC sampling. Nil is supported and **accounted for, not skipped**:
with no store nothing can be named, so every group is counted in
`GroupsUnresolvedName` and left pending — the same accounting as "the cubin
never arrived", because for the profile it is the same fact. Skipping
silently would make a missing store look identical to a healthy run with no
PC samples (`TestTierBWithoutAModuleStoreLeavesEverythingPending`).

---

## 7. Verification

`CapEff: 0`, no GPU, no capabilities. From the worktree with the plan's build
environment.

```
go build ./...                                     ok
go vet ./...                                       ok
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1 ok (12 packages)
go test ./... -count=1                             ok (whole repo, no regressions)
go test ./gpu/ -race -count=4                      ok  20.5s
~/go/bin/golangci-lint run --timeout=5m            0 issues.
```

The plan's four offline checks for this task, and the test that is each one:

| the plan asks | test |
| --- | --- |
| a Tier B sample reaches its execution | `TestTierBSampleReachesItsExecution` |
| two executions of one kernel yield `kernel-ambiguous` **and leave `gpu_ambiguous` unset** | `TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous` |
| a sample whose module is unknown stays pending, is evicted, and is counted | `TestTierBUnknownModuleStaysPendingAndIsEvicted` |
| a multi-device process yields `kernel-multidevice` | `TestTierBMultiDeviceProcessIsMarked` |

New tests, `gpu/pcjoin_test.go` (20): the four above, plus
`TestTierBJoinIsConsumingLikeTheExactPath`,
`TestTierBAmbiguousGoesToTheEarliestExecution`,
`TestMultiDeviceOutranksAmbiguity`,
`TestSingleDeviceProcessIsNotMarkedMultiDevice`,
`TestDeviceTrackingCapIsCountedNotSilent`,
`TestTierBKernelNameMismatchStaysPending`,
`TestTierBJoinStaysWithinTheProcess`,
`TestTierBGroupWithNoProcessIsRefused`,
`TestTierBWithoutAModuleStoreLeavesEverythingPending`,
`TestTierBUnknownFunctionIndexStaysPending`,
`TestExactCorrelationJoinIsUnchangedAndLabelledExact`,
`TestExactPathKeepsFirstClaim`,
`TestTierBJoinAccountsForEveryGroup`,
`TestTierBReconciliationCoversAttributedAndPending`,
`TestPCAttribsAreExhaustiveAndStable`,
`TestPCAttribRefusesValuesNobodyDecided`. Plus seven in
`modulestore_test.go` and one new conformance test.

### Mutation checks — the tests bite

Each applied alone to the finished code, `go test ./gpu/ -count=1`:

| mutation | result |
| --- | --- |
| M1 — ambiguity never marked (`candidates > 1` branch deleted) | `TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous`, `TestTierBAmbiguousGoesToTheEarliestExecution` |
| M2 — name match relaxed to a prefix match | `TestTierBKernelNameMismatchStaysPending` |
| M3 — pid dropped from `execKernelKey` | `TestTierBJoinStaysWithinTheProcess`, `TestTierBJoinAccountsForEveryGroup` |
| M4 — multi-device never checked | `TestTierBMultiDeviceProcessIsMarked`, `TestMultiDeviceOutranksAmbiguity` |
| M5 — PC ambiguity also sets `ExecutionView.Ambiguous` (the overload this label exists to prevent) | `TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous` |
| M6 — a refusal path returns without its counter | 4 failures, including both group-identity assertions |
| M7 — the module join runs over exactly-served executions too | `TestExactPathKeepsFirstClaim` |

A test that cannot fail is not a test.

---

## 8. Cannot verify

**Nothing here has been run against a GPU, and no claim in this report is
about hardware.** This machine has `CapEff: 0` and no NVIDIA device; no BPF
program, no CUPTI call and no shim was executed. Every sample this join has
ever seen is synthetic, and every module it has ever resolved is one of Task
1's four `sm_86` / CUDA 13.3 fixtures.

The plan defers two things about this task to the RTX 3090, and both are
open:

- **What fraction of real PC samples attribute versus stay pending.**
  Unknown. `PCJoin.GroupsJoined` against `GroupsUnresolvedName +
  GroupsNoExecution`, and `AttributedKernel` against `PendingModuleSamples`,
  are the instruments; both are in every `Snapshot` and one is in
  `joinhealth`. The dominant risk is the name comparison in §2: `KernelName`
  arrives from `gpu_kernel_name_v1` (what CUPTI's activity record reports)
  and the function name comes from the cubin's `.symtab`. Whether those two
  spellings are byte-identical for C++ kernels — mangled on one side and not
  the other, or mangled differently — **is not established anywhere in this
  repository**. The fixtures are `extern "C"`, so they cannot settle it. If
  the spellings differ, this join attributes nothing on real workloads and
  the symptom is `GroupsNoExecution` climbing to match the group count, with
  every sample aging out. That is the honest failure and it is visible, but
  it is a failure, and it is the first thing to measure.
- **Whether the eviction horizon is long enough given the drain period.**
  Unknown. `PendingSampleHorizonNs` is shared with the correlation-keyed
  store and defaults to zero (disabled) — a caller must set it. Whether the
  gap between a PC sample's drain-stamped `TimeNs` and its execution's
  arrival fits inside any particular horizon at the shim's 100 ms drain
  period has not been measured. `Dropped.EvictedPendingModuleSamples` with
  a low `AttributedKernel` is the reading that says it is too short.

Also unestablished, carried forward from Tasks 4 and 8a and load-bearing
here:

- **That `functionIndex` is the cubin's `.symtab` index.** Task 6 measures
  it. This join groups and names on whatever the store keys on, so a
  negative answer changes `indexFunctions` and nothing in this file — but it
  changes whether the names this join compares are the right names at all.
- **That real Tier B samples arrive with a usable `cubin_crc`.** If `CRC` and
  `FunctionIndex` both arrive zero, every sample from a process lands in one
  group whose CRC no module matches, and every one of them counts as
  `GroupsUnresolvedName`.
- **That `pcOffset` is function-relative in the sense the line table is.**
  Not this task's business — it is Task 9's — but it is the other half of
  what makes a PC sample useful once it has reached its execution.

The device-tracking cap (4,096) is reasoned, not measured. Reaching it needs
thousands of concurrently profiled processes that each run GPU kernels; the
counter and the anomaly exist so that if it ever is reached, that is visible
rather than inferred from a suspiciously clean profile.
