# Task 7 — The consumer decodes PC samples, modules and the stall map

Branch `feat/consumer-decodes-pc`, one commit on top of `origin/main` at `4f8cfcaf`.

`gpuprobe/applyBatch` now has arms for all five PC-sampling kinds. `Stats.Undecoded`
reads zero for every one of them, and a test asserts it for all five at once.

Files: `gpuprobe/consumer.go`, `gpuprobe/stallnames.go` (new),
`gpuprobe/consumer_test.go`, `gpuprobe/stallnames_test.go` (new).

---

## The five arms

`decodeBatch` also grew arms for `kindModule` and `kindPC` — those two had a
cookie, a BPF `record_size` and a `KIND_*`, but no Go decode at all, so the
`batch` struct gained `Modules []gpuabi.ModuleLoad` and `PCSamples
[]gpuabi.PCSample` alongside the three Task 2 already decoded.

| kind | arm | what it produces |
| --- | --- | --- |
| `kindModule` (3) | `emitModuleLocked` | `gpu.GPUModule{Ref:{Backend, CRC}, SizeBytes, LoadedNs}` → `Sink.EmitModule` |
| `kindPC` (4) | `emitPCSampleLocked` | `gpu.GPUPCSample{Correlation, Module:{Backend, CRC}, ClockDomain, PCOffset, StallReason, Count}` → `Sink.EmitPCSample`, or held for its stall name |
| `kindStallMap` (7) | `learnStallNameLocked` | interns `index → name`, releases every PC sample waiting on that index |
| `kindSamplingWindow` (8) | `noteSamplingWindowLocked` | counts the burst, and counts separately whether it was left open |
| `kindConfig` (9) | `noteConfigLocked` | last-writer-wins gauges for `sampling_factor` / `sm_count` / `clock_hz`, plus a disagreement counter |

Each arm does `c.stats.Records++` per record, which is what `Undecoded` stopped
counting. The `default:` arm stays, and its doc comment now says what reaching
it means: no kind the ABI defines can land there any more, so a non-zero
`Undecoded` is the ABI having drifted — a `KIND_*` added on one side of the
wire and not the other.

### Three decisions inside the arms

**`ModuleLoad.BytesPtr` is decoded and deliberately dropped.** It is a pointer
into the *producer's* address space; following it needs `CAP_SYS_PTRACE`, which
this agent does not have. The bytes come over the cubin channel (`cubin.go`).
The record says *that* a module loaded, with its content hash and size, and
nothing more. `ModulesDecoded` and `CubinsReceived` disagreeing is the ordinary
Tier B failure — a module announced but unresolvable.

**`GPUPCSample.TimeNs` stays zero.** `gpu_pc_sample_batch_v1` carries no
timestamp and neither does the batch header. Stamping the arrival time would
present a consumer-side clock reading as a measurement of when the instruction
stalled. The consequence is stated rather than papered over: `Timeline`'s
pending horizon does not age these out by time, and bounding them is Task 8a's
second pending index, which keys on `{PID, CubinCRC, FunctionIndex}` and brings
its own eviction counter.

**`PCSample.FunctionIndex` is decoded off the wire and not carried onto
`GPUPCSample`.** The field does not exist on that type yet; adding it is Task 8a
by name ("neither Task 7 nor the old Task 8 added the field"). Flagged here so
it is a known gap rather than an unnoticed one.

---

## How the stall-name table mirrors `kernelnames.go`

`gpuprobe/stallnames.go` is `kernelnames.go` with the types swapped. Same
structure, same invariants, same reasons:

| `kernelnames.go` | `stallnames.go` |
| --- | --- |
| `kernelName{name, truncated}` / `.resolved()` | `stallName{name, truncated}` / `.resolved()` |
| `truncatedNameSuffix` | `truncatedStallSuffix` (the same rune, aliased, not a second marker) |
| `kernelNameTable` — map + FIFO `order` + `head` + `capacity`, `put` returns evictions, `compact()` | `stallNameTable`, identical |
| re-intern replaces in place without appending, so an id appears at most once and `len(order)-head == len(byID)` | identical, and pinned by `TestStallNameOrderTracksLiveEntriesOnly` |
| eviction by *first* insertion, so a replay does not renew a position | identical, `TestStallNameEvictionIsByFirstInsertion` |
| `pendingNames` — bounded FIFO, `push` returns the oldest for release, `takeByKernel`, `drain`, `compact` | `pendingStallSamples`, `takeByIndex`, otherwise identical |
| overflow **releases** the oldest rather than dropping it | identical |
| `learnKernelNameLocked` interns, counts, releases waiters | `learnStallNameLocked`, identical |
| `Flush` drains the queue, unnamed and counted | `Flush` also drains `unresolvedStall` |
| not internally synchronized; `Consumer` calls it under `c.mu` | identical |

### Where it deliberately does not mirror it

**1. No sentinel index.** `resolveKernelNameLocked` short-circuits on
`kernelID == 0` because the ABI defines zero as "no kernel". CUPTI's stall
reason indices are opaque vendor numbers and **0 is an ordinary one**, so
`resolveStallNameLocked` has no such branch. Treating it as absent would
silently blank one real stall reason on every device where it lands at index 0.
Pinned by `TestStallNameIndexZeroIsAnOrdinaryEntry` and by the `stallIndex: 0`
case in `TestPCSamplesResolveStallNamesFromTheMap`.

**2. No `waitsForNameLocked` gate.** A launch is held for a name only once the
producer has *demonstrated* that names exist (`sawKernelName`), because holding
one for a name that is never coming delays the join. A PC sample with an
unmapped index is held **unconditionally**. The gate would break exactly the
case the plan requires to work: the stall map is one-shot plus late-attach
replay, so the *first* PC batch of a run routinely precedes it, and a
"have we seen a map yet" gate would send precisely those samples out permanently
blank. The cost of dropping the gate is bounded and does not lose a record — a
producer that never maps its indices pays a fixed lag of
`PendingStallSampleCapacity` samples, because the queue releases its oldest on
every push. `TestPCSamplesFlowWhenNoStallMapEverArrives` drives 20 samples
through a queue of 4 and asserts all 20 arrive, in order, all counted.

**3. The pending entry holds a value, not pointers.** `unnamedEvent` holds
`*launch` / `*exec` to avoid paying for both structs; `unresolvedSample` has one
event type, so there is nothing to avoid.

**Capacities.** `defaultStallNameCapacity = 256` (a device's stall reasons are a
fixed enum — 38 on GA102 — so this is an order of magnitude of headroom);
`defaultPendingStallSampleCapacity = 4096`, sized like `Timeline`'s own
`pendingSampleCap` because the window it covers is the producer's 100 ms drain
interval at full PC rate. Both overridable via `Config.StallNameCapacity` /
`Config.PendingStallSampleCapacity`.

---

## Every new counter, healthy and worst

Loss and anomaly counters — **all assertable at zero on a healthy run**, and all
asserted at zero together in
`TestHealthyTierBRunLeavesEveryNewLossCounterAtZero`:

| counter | healthy | worst, and what it means |
| --- | --- | --- |
| `StallNamesMissing` | 0 | `== PCSamplesDecoded`: a profile full of PC samples with no stall reason at all — the entire point of PC sampling, silently absent if it were not counted. The samples are still delivered and their GPU time still measured. |
| `StallNamesEvicted` | 0 | non-zero: the bounded table is churning, so the stall labels in the profile describe whichever indices happened to be resident. |
| `StallNamesTruncated` | 0 | non-zero: the producer is cutting names, and two distinct reasons sharing a prefix would otherwise aggregate into one label value. The ABI's buffer is exactly CUPTI's `CUPTI_STALL_REASON_STRING_SIZE`, so a CUPTI producer cannot reach this. |
| `SamplingWindowsOpen` | 0 | non-zero: a burst's end is unknown (hard exit mid-burst — the shim's `atexit` closes it on the ordinary path), so an unbounded tail of executions is `serialized="unknown"`, never `"false"`. |
| `ConfigsDisagreed` | 0 | non-zero: `ConfigSamplingFactor` / `ConfigSMCount` / `ConfigClockHz` describe an arbitrary one of several producers and must not be used to scale anything. |
| `Undecoded` (existing, meaning tightened) | 0 | equal to the record count of every batch of a kind the consumer cannot read — the ABI has drifted. |

The one counter whose healthy reading is **not** zero, and the reason it exists:

| counter | healthy | worst |
| --- | --- | --- |
| `PCSamplesWithoutCorrelation` | **Tier B: `== PCSamplesDecoded`.** Tier A: 0. | non-zero **in Tier A**, where CUPTI populates `correlationId` on every record (the spike measured 1,828 of 1,828). One such sample breaks Tier A's whole claim — that a PC sample joins to a launch exactly — and every one of them can then only be attributed by inference. |

This is the defect the task names, in miniature. `ZeroCorrelation` aggregates two
populations whose healthy values are **opposites**: a PC sample's zero is normal
and dominant in Tier B, an execution's zero is a shim contract violation. With PC
samples dominating the traffic, a Tier A violation would be invisible inside the
aggregate — the same shape as issue #52, one population masking another. The two
subsets are now kept apart: `ZeroCorrelationExecs` (existing) and
`PCSamplesWithoutCorrelation` (new). `ZeroCorrelation` keeps counting both, and
its doc comment now says it must never be read alone.
`TestTierBZeroCorrelationIsCountedApartFromTheAggregate` asserts a PC sample's
zero does not move `ZeroCorrelationExecs`;
`TestTierAPCSampleCarriesTheProcessQualifiedCorrelation` asserts the opposite
condition.

Volume counters — the answer to "did this kind arrive at all", which no existing
counter gives because `Records` aggregates every kind:

| counter | healthy | worst |
| --- | --- | --- |
| `PCSamplesDecoded` | non-zero with a tier enabled | **0 with a tier enabled**: the shim never drained a PC buffer. A total, silent absence that looks exactly like an idle GPU. |
| `ModulesDecoded` | one per distinct module loaded | 0 while executions flow: no module ever reached the agent, so nothing Tier B can attribute through. |
| `StallNamesLearned` | ≥ one full table per producer (38 on GA102) | 0 while PC samples flow → `StallNamesMissing == PCSamplesDecoded`. |
| `SamplingWindowsDecoded` | many in Tier A, ≤1 in Tier B | **0 in Tier A**: nothing says which executions ran perturbed. |
| `ConfigsDecoded` | ≥ one per producer | 0: the three gauges are unset and the sampling period behind every PC sample is unknown. |

Gauges: `PendingStallSamples`, `KnownStallNames`, `ConfigSamplingFactor`,
`ConfigSMCount`, `ConfigClockHz`.

**`SamplingWindowsDecoded` is deliberately the whole of what the consumer does
with a window.** The serialization disclosure that consumes the content is
Task 10; building half of it here would leave a store nothing reads. The counter
makes the discard *sized and visible*, which is the standing rule — it is not a
claim the windows have been used, and the doc comment says so.

---

## `""`, never `"stall#17"`

`GPUPCSample.StallReason` is a string, and an unresolved index becomes the empty
string. The index is the **vendor's** — not stable across devices or driver
versions — so rendering it would put an unstable internal number into a label
value that consumers aggregate on, and would merge two different stall reasons
from two driver versions into one bucket. `TestUnmappedStallIndexYieldsEmptyStringAndIsCounted`
asserts the empty string, asserts the rendered index does not appear, asserts
the sample itself is delivered intact, and asserts `StallNamesMissing == 1`.

## The pending-map test

`TestPCBatchBeforeItsStallMapStillResolves` is the one the task calls for. It
applies a PC batch with index 17 **before** any stall map, asserts nothing
reached the sink and `PendingStallSamples == 2`, then applies the map and
asserts both samples arrive with `"long_scoreboard"`, oldest first, with
`StallNamesMissing == 0` — a sample that resolved late is not a missing name.

Other tests added: the five-kind `Undecoded == 0` table
(`TestTheFivePCSamplingKindsAreDecodedNotCountedUndecoded`), module
normalization and `bytes_ptr` non-following, ordinary map-then-samples order
with index 0 as a real index, no-map-ever, truncation marking, table eviction,
replay-does-not-evict-itself, Tier A / Tier B correlation, open vs closed
sampling window, inverted window refused at the boundary, config decode and
disagreement, sequence gaps on the new kinds (including that a different pid
and a different kind are independent streams), sink rejection counted, and the
all-counters-zero healthy run. Plus six table/FIFO tests in
`stallnames_test.go` mirroring `kernelnames_test.go`.

Two existing tests were flipped rather than deleted:
`TestNewKindsAreCarriedUndecodedAndCounted` became
`TestTheFivePCSamplingKindsAreDecodedNotCountedUndecoded`, and
`TestUndecodedKindsAreCountedNotDropped` now drives kind 15 — below `kindMax`,
above every kind either side defines — so it still proves `Undecoded` counts
rather than drops.

---

## One discrepancy with the plan's prose, resolved in favour of the code

The task says `Correlation` "gets the PID from the batch header in both tiers,
with the value only when non-zero — which is already what `correlationOf` does".
Those two clauses disagree. `correlationOf` returns the **whole zero
`CorrelationID`** on a wire zero, not one carrying only the pid, and its doc
comment gives the reason at length: so that the older `== gpu.CorrelationID{}`
reading and the `Present()` reading agree on these records.

I took the operative half of the instruction — use `correlationOf`, do not
invent a second rule — so a Tier B sample's correlation is
`gpu.CorrelationID{}` and a Tier A sample's is
`{Backend, PID: <batch header>, Value: "<n>"}`. Nothing needs the pid on a
Tier B correlation: Task 8a's index carries it in the key itself
(`{PID, CubinCRC, FunctionIndex}`), which is issue #52's discipline —
cross-process refusal is structural, not a check someone can forget.

---

## Verification

Run in the worktree with the plan's build environment.

```
go build ./...                                   ok
go vet ./...                                     ok
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1
                                                 all ok
go test ./gpuprobe/ -race -count=4               ok (15.8s)
make -C shim && make -C shim test                ok
~/go/bin/golangci-lint run --timeout=5m          0 issues
```

The four things the plan asks this task to prove offline, and where:

- `Undecoded == 0` for all five kinds — `TestTheFivePCSamplingKindsAreDecodedNotCountedUndecoded`.
- a PC batch arriving before its stall map still resolves once the map arrives — `TestPCBatchBeforeItsStallMapStillResolves`.
- a stall index with no map entry yields `""` and a non-zero `StallNamesMissing` — `TestUnmappedStallIndexYieldsEmptyStringAndIsCounted`.
- sequence-gap accounting works on the new kinds — `TestSequenceGapsAreCountedOnTheNewKinds`.

## Cannot verify

`CapEff: 0`, no GPU on this machine. Everything above is decode and accounting
against synthetic wire batches built in the test file; **no real
`gpu_pc_sample_batch_v1`, `gpu_stall_reason_map_v1`, `gpu_config_v1`,
`gpu_sampling_window_v1` or `gpu_module_load_v1` record produced by CUPTI has
been through this code.** In particular, unverified here:

- that a real device's stall indices and names decode as this assumes — Task 6 measures whether 38 stall reasons arrive on GA102 and what they are called;
- that Tier A really populates `correlationId` on every PC record, which is what makes `PCSamplesWithoutCorrelation == 0` a meaningful Tier A assertion;
- that `pcOffset` means what the line table will need it to mean, and whether `functionIndex` is the cubin `.symtab` index — the finding-2 question, and the trigger for `gpu_pc_sample_batch_v2`;
- the real PC-record rate, which is what decides whether `defaultPendingStallSampleCapacity = 4096` is generous or tight in the window before the stall map replays.

Per the plan, this task's line is: **must be measured on the RTX 3090
afterwards — nothing.** The unverified items above all belong to Task 6's
hardware list, not to this one.

## Out of scope, as instructed

No `gpu.ModuleStore` wiring (Task 4, PR #77, unmerged). No timeline join, no
`gpu_pc_attrib`, no source-line resolution (Tasks 8a/8b/9). No change to the
enrollment or cubin transport paths, no change to any frozen record layout, no
change to `MAX_BATCHED_RECORD_BYTES`, no change to `bpf/`.
