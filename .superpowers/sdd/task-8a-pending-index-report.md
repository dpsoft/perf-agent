# Task 8a — `GPUPCSample.FunctionIndex` and the second pending index

Branch `feat/pc-pending-module-index`, one commit off `origin/main` (`ccd0fe5a`).

Scope: get correlation-less (Tier B / `CONTINUOUS`) PC samples into a correctly-keyed
index. **The `Snapshot` join is deliberately not here** — that is Task 8b. Nothing
attaches out of the new store yet, and `AttributedPCSamples` is unchanged by this work.

---

## 1. The field

`gpu/types.go`, additive on the capability-gated `GPUPCSample`:

```go
FunctionIndex uint32 `json:"function_index,omitempty"`
```

CUPTI's per-module `functionIndex`. It has been on `gpu_pc_sample_batch_v1` since the
ABI froze (`internal/gpuabi/records.go:83`, decoded at `records.go:146`) and was simply
never carried through to the canonical type. Meaningless without `Module`, exactly as
`PCOffset` is: `(Module.CRC, FunctionIndex)` names a device function,
`(Module.CRC, FunctionIndex, PCOffset)` names an instruction.

**`gpu/types_test.go:44` checked, not assumed.** That test's forbidden-name list
(`pc`, `stall`, `module`, `cubin`, `sass`) is iterated over
`reflect.TypeOf(GPUKernelExec{})` — the lean shared execution type — and its second half
asserts positively that `PCOffset`/`StallReason`/`Module` *do* live on `GPUPCSample`.
Adding a field to `GPUPCSample` is what that test wants, not what it forbids;
`TestPCSamplingFieldsDoNotWidenExecution` passes unchanged.

**Not populated by the consumer in this commit.** `gpuprobe/consumer.go` has no
`kindPC` decode arm on `main` at all — `kindPC` lands in the `default:` arm and is
counted in `Stats.Undecoded`. Building that arm is Task 7's diff (in flight in
`.worktrees/consumer-decode`, which also populates `Module`). Adding a competing decode
path here would have been a real conflict for no benefit, so this commit supplies the
field and the index and leaves the wire→field assignment to Task 7's one-line addition
(`FunctionIndex: rec.FunctionIndex`).

## 2. The index

`gpu/timeline.go`. A second pending store, entered on `!p.Correlation.Present()` and
nothing else — the same gate `Snapshot` already uses to route a correlation-less
*execution* to the heuristic join.

| | correlation-keyed (`pending`) | module-keyed (`pendingModule`) |
|---|---|---|
| key | `CorrelationID` | `pendingModuleKey{Backend, PID, CubinCRC, FunctionIndex}` |
| cardinality cap | `pendingCap` = `LaunchCache.Capacity` (65,536) | `pendingModuleCap` = `MaxPendingModuleGroups`, default **4,096** |
| per-group sample cap | `pendingSampleCap` (4,096) | `pendingSampleCap` — shared |
| horizon | `pendingHorizonNs` | `pendingHorizonNs` — shared, per the plan |
| anchor | `pendingNewestNs` | `pendingNewestNs` — shared |
| order/liveness walk | `orderedFIFO[CorrelationID]` | `orderedFIFO[pendingModuleKey]` |
| eviction counter | `Dropped.EvictedPendingSamples` | `Dropped.EvictedPendingModuleSamples` |
| gauges | `PendingSamples` / `PendingCorrelations` | `PendingModuleSamples` / `PendingModuleGroups` |

**Cardinality cap is its own dial and deliberately not `LaunchCache.Capacity`,** which
every other store here reuses. That capacity tracks *launch volume*, a function of run
length. This store's cardinality is a function of the profiled *binary* — one group per
device function per process, tens to low hundreds for a real workload. Sizing it off
launch volume would reserve a 65,536-entry map to hold fifty.

**Horizon reuses `pendingHorizonNs` by value, and the anchor `pendingNewestNs` by
sharing.** One PC-sample stream, one clock domain, one anomalous-jump clamp
(`defaultMaxAdvanceNs`). The tiers are mutually exclusive (Task 11), so a Tier A stream
advancing the anchor past a Tier B group cannot arise in practice.

**`orderedFIFO` reused, as the plan requires** — no third eviction walk.
`isPendingModuleLiveLocked` mirrors `isPendingLiveLocked`, and the
absent-to-present-only sequence bump is the same: a group accumulates into one live
generation across many `EmitPCSample` calls, so re-stamping per append would orphan its
own live order position. That is exactly the deleted-then-reused hazard `LaunchCache`
and `Timeline.pending` each hit separately before `orderedFIFO` existed.

*Contrast with Task 4.* `ModuleStore` deliberately did **not** reuse `orderedFIFO`,
because an LRU must move an entry to the front on every *read* and `orderedFIFO` has no
notion of a read. `pendingModule` is pure FIFO by arrival, like `pending`, so that
reason does not apply.

**One documented widening of the plan's key.** The plan writes
`{PID, CubinCRC, FunctionIndex}`; the implementation is
`{Backend, PID, CubinCRC, FunctionIndex}`. Nothing today emits PC samples from two
backends into one `Timeline` (ROCm does not advertise `CapabilityPCSampling`), so
`Backend` changes no behaviour — it is present for the same reason `CorrelationID`
carries it, and costs nothing to make impossible. Flagged here rather than absorbed
silently.

## 3. Cross-process refusal is structural

`PID` is a **field of the map key**, taken from `Correlation.PID` — which a
correlation-less sample still carries, since PID and Backend are context the producer
knows regardless (`CorrelationID.Present`'s doc comment). There is no cross-process
check anywhere in the new path, because there is nothing to check: two processes
running the identical binary produce the identical cubin CRC and the identical function
index, so a store keyed on the module alone would merge them, and a store keyed on the
module *plus a guard* would merge them the day someone edits the guard out. Issue #52's
discipline.

`TestTierBSamplesStayWithTheirProcess` pins it with the worst case — same CRC, same
function index, two PIDs. Deleting `PID` from `pendingModuleKeyFor` fails it (mutation
M2 below).

## 4. Pre-fix failure of the anti-collapse test

The deliverable test drives 10,000 Tier B samples over 50 distinct
`(crc, functionIndex)` pairs and asserts none are lost to `EvictedPendingSamples`.

**(a) Against pristine `main`** it does not compile — the key it needs cannot be
expressed:

```
# github.com/dpsoft/perf-agent/gpu [github.com/dpsoft/perf-agent/gpu.test]
gpu/pendingmodule_test.go:19:3: unknown field FunctionIndex in struct literal of type GPUPCSample
gpu/pendingmodule_test.go:65:30: snap.Dropped.EvictedPendingModuleSamples undefined (type TimelineDropStats has no field or method EvictedPendingModuleSamples)
gpu/pendingmodule_test.go:68:32: snap.PendingModuleSamples undefined (type Snapshot has no field or method PendingModuleSamples)
gpu/pendingmodule_test.go:70:37: snap.PendingModuleGroups undefined (type Snapshot has no field or method PendingModuleGroups)
gpu/pendingmodule_test.go:91:20: tl.pendingModule undefined (type *Timeline has no field or method pendingModule)
...
FAIL	github.com/dpsoft/perf-agent/gpu [build failed]
```

A compile failure is weak evidence, so:

**(b) The same drive expressed in `main`'s own types** (pair index riding in `PCOffset`,
which exists on `main`), run against unmodified `origin/main`:

```
--- FAIL: TestPreFixTierBCollapse (0.01s)
    zz_prefix_probe_test.go:34:
        	Error:      	Should be zero, but was 5904
        	Messages:   	10,000 Tier B samples over 50 distinct (crc, functionIndex) groups: none may be lost
    zz_prefix_probe_test.go:36:
        	Error:      	Not equal:
        	            	expected: 50
        	            	actual  : 1
        	Messages:   	the samples must be spread over one group per (crc, functionIndex) pair
FAIL
FAIL	github.com/dpsoft/perf-agent/gpu	0.010s
```

**50 groups collapsed to 1, and 5,904 of 10,000 samples evicted** — precisely
`10,000 − pendingSampleCap`. And the 4,096 that *survived* would have been matched by
nothing, because no execution carries an empty correlation value. That probe file was
deleted after the run; the shipped test is the typed version.

## 5. Mutation checks — the tests bite

Each mutation applied alone to the finished code, `go test ./gpu/ -run 'TestTierA|TestTierB'`:

| mutation | result |
|---|---|
| M1 — key ignores `FunctionIndex` | 3 failures, incl. "two device functions in one cubin must key apart" and 50 groups → 10 |
| M2 — key ignores `PID` | `TestTierBSamplesStayWithTheirProcess`: "identical (crc, functionIndex) from two processes must be two groups, not one" |
| M3 — module evictions charged to `EvictedPendingSamples` | 4 failures, incl. "a Tier B eviction must never be charged to the Tier A counter" |
| M4 — routing on `Correlation.Present()` removed | the pre-fix pathology returns exactly: "Should be zero, but was 5904" |

## 6. Counters, and what each reads when things are worst

All three are zero on a healthy run and each of my tests asserts them so.

- **`Dropped.EvictedPendingModuleSamples`** — correlation-less samples destroyed before
  anything could use them. Non-zero means one of exactly three things, and the
  `joinhealth` line names all three: the group index is too small for the workload's
  distinct device functions (`MaxPendingModuleGroups`), the horizon is shorter than the
  gap between a sample and its execution, or one device function's samples outran
  `MaxPendingSamplesPerCorrelation` because its execution never arrived. At its worst it
  reads as a large fraction of everything the producer emitted, and the stall detail on
  those kernels is simply absent from the profile. **Kept separate from
  `EvictedPendingSamples` on purpose** — a non-zero `EvictedPendingSamples` says
  executions are not arriving for the correlations their samples carry, which is a
  different problem with a different fix, and one counter for both would make the two
  indistinguishable at exactly the moment they matter.
- **`PendingModuleSamples` / `PendingModuleGroups`** — gauges, not deltas. Until Task 8b
  lands they rise monotonically to their bounds while `AttributedPCSamples` stays flat,
  which is the honest reading of "collected, correctly grouped, not yet attributed".
  After 8b, a persistently high `PendingModuleSamples` with a low attribution rate means
  the join is not finding executions for these kernels.

**Reconciliation identity extended** (`assertPCSampleLossesAccounted`,
`gpu/conformance_test.go`): `accepted == attributed + pending + evicted +
pendingModule + evictedPendingModule`. Without the new terms every continuous-mode
sample would read as unaccounted-for the moment such a scenario exists.
`TestTierBSamplesReconcile` forces all three module-store terms non-zero in one run —
per-group cap, cardinality cap and horizon all firing — so they are not dead terms.

**`joinhealth`** gained one summary clause (present only when the store is non-empty)
and one anomaly line. Both are absent on a healthy run; `TestJoinHealthHealthyRunIsOneShortLine`
is unchanged and passes.

## 7. Verification

`CapEff: 0`, no GPU, no capabilities. All from the worktree with the plan's build env.

```
go build ./...                                     ok
go vet ./...                                       ok
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1 all ok (12 packages)
go test ./gpu/ -race -count=4                      ok  19.4s
~/go/bin/golangci-lint run --timeout=5m            0 issues.
```

New tests, all in `gpu/pendingmodule_test.go`:

- `TestTierBPCSamplesDoNotCollapseOntoOneKey` — the deliverable regression.
- `TestTierBHorizonEvictionCountsSeparately`
- `TestTierBCardinalityEvictionCountsSeparately`
- `TestTierBPerGroupSampleCapCountsSeparately`
- `TestTierBSamplesStayWithTheirProcess`
- `TestTierBDistinctFunctionIndexSplitsGroups`
- `TestTierBSamplesReconcile`
- `TestTierAPCSamplesStillUseTheCorrelationIndex` — non-regression: a correlation-bearing
  sample never enters the new store, still joins exactly, and carries `FunctionIndex`
  through the exact path unchanged.

## 8. Cannot verify

**I cannot verify anything about real hardware, and nothing here was run against a GPU.**
The plan states for this task: *"Must be measured on the RTX 3090 afterwards: nothing."*
That is accurate — every claim above is about in-process data structures driven by
synthetic samples, and all of it is exercised by the tests. What is *not* established by
this commit, and must not be read into it:

- **That `functionIndex` is the cubin's `.symtab` index.** Task 6 measures it. If it is
  not, the plan's pre-approved fallback is `gpu_pc_sample_batch_v2` with an appended
  `kernel_id`. The index built here is unaffected either way — it groups on whatever
  `functionIndex` is, and only Task 8b's *resolution* of that index to a name depends on
  the answer.
- **That real Tier B samples arrive with a usable `cubin_crc` at all.** No consumer decode
  arm exists yet (Task 7), so no `GPUPCSample` in this repository has ever been populated
  from a wire record. If `Module.CRC` and `FunctionIndex` both arrive zero, every sample
  from a process lands in one group — honest, since they would genuinely be
  indistinguishable, but no better than today.
- **That the shared `pendingHorizonNs` is long enough** for the gap between a PC sample
  and its execution at the shim's 100 ms drain period. Task 8b's hardware note owns this.
- **That the 4,096-group default is right.** It is reasoned from "one group per device
  function per process", not measured. Worst case with the defaults is 4,096 groups ×
  4,096 samples of `GPUPCSample`, reachable only by a producer emitting thousands of
  distinct device functions whose executions never arrive — which is itself the
  condition `EvictedPendingModuleSamples` exists to report.
