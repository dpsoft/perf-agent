# Task 13 — The Phase 6 gate, GPU-free half (assertions 1–12)

Branch `feat/phase6-gate`, one commit, test-only. No file under `gpu/`, `shim/` or
`bpf/` changed; `git diff --stat` on `gpuprobe/gate_test.go` is 803 insertions and
**zero deletions**, so no existing gate assertion was weakened.

Verified on this machine: `CapEff: 0`, no GPU, no passwordless sudo. That shapes the
whole deliverable and is stated plainly below rather than glossed.

## Where the gate lives, and why it is in three places

| file | package | privilege | what |
| --- | --- | --- | --- |
| `gpu/gate_test.go` | `gpu` | none | `TestPhase6Gate` — assertions 1–9 and 10b |
| `gpuprobe/gate_compose_test.go` | `gpuprobe` | none | `TestPhase6GateConsumerHalf` — assertions 10, 10a, 11, 12 |
| `gpuprobe/gate_test.go` | `gpuprobe_test` | CAP_BPF + CAP_PERFMON + CAP_CHECKPOINT_RESTORE | `TestStubDrivesPCSamplingToPprofWithoutAGPU` — 1–5, 8, 9, 12 end to end |

The plan writes the gate as an extension of `TestStubDrivesThePipelineToPprofWithoutAGPU`.
Written only there, the gate **skips on every machine without capabilities**, including
this one — and a twelve-point gate that cannot run is the most expensive instance of the
failure this project has hit nineteen times. So every assertion that is a statement about
the `gpu` or `gpuprobe` package is *also* asserted where it needs no privilege, and the
privileged test adds what only a real producer can add: a CPU stack walked by BPF through
two `-fomit-frame-pointer` frames, real cubins crossing a real socket, 500 real executions.

The two unprivileged entry points **compose** rather than restate: they call the tests the
tasks already wrote, as sub-tests named for the gate assertion. Two assertions of one fact
drift apart; one assertion referenced from the gate does not. What composing buys is that
deleting or weakening any of them now fails *the gate*, by assertion number.

`TestStubDrivesPCSamplingToPprofWithoutAGPU` is a **second** privileged test rather than an
edit to the baseline one. The baseline asserts `require.Len(samples, len(snap.Executions))`,
which is true precisely because that run emits no PC samples; turning PC sampling on inside
it would have meant weakening that and several others.

---

## The twelve

"Reused" means the gate calls an existing test. "New" means no test asserted the property.
"Ran" means it executed on this machine, unprivileged. "Compiled" means it type-checks and
is wired into the privileged gate, which cannot run here.

### 1. Frames stop at the kernel

**Asserts.** Frames are exhaustively `<CPU stack> → [gpu:launch]|[gpu:launch unsampled] →
[gpu:kernel:<name>]`. No frame carries the PC, the stall reason, the source location or the
attribution quality.

**Catches.** Promoting any per-sample detail to a frame, in any spelling. At PC-sampling
rates one frame per PC destroys aggregation and fragments the kernel's own block, which is
the §8 ruling the whole label design rests on.

**Reused** — `TestProjectionAddsNoFrames`, `TestProjectionKeepsPCOutOfStackIdentity`.
**New end-to-end**: the privileged gate compares each sample's frames against the launch's
own `CPUStack` as a *whole slice*, so nothing may be inserted anywhere rather than merely
appended. Whole-slice comparison also avoids a false positive a substring scan would hit:
three of the stub's stall reasons are spelt `wait`, `barrier` and `membar`, and libc frame
names contain all three.

**Ran** (unprivileged half) / **compiled** (end-to-end half).
**Mutation-checked**: appending `[gpu:src:mutant]` in `projectionFrames` fails assertions
1, 2 and 3 of `TestPhase6Gate`.

### 2. A source line is reached from a CPU stack — the Phase 6 exit condition

**Asserts.** One pprof sample carrying *both* a real CPU call path in its frames *and*
`gpu_src_status="resolved"` with `gpu_src_file`/`gpu_src_line` naming a real line of
`internal/cubin/testdata/single.cu`. The line number is checked against the `.cu` itself,
not against a constant, so a fixture rebuilt from edited source cannot leave it green.

**Catches.** A projection that resolves source labels but loses the launch's frames, or the
reverse; a join that reaches the execution but drops `PCSamples` so nothing projects; a
store keyed on something the sample does not carry; a resolution naming a line the table
does not contain.

**New.** Three tests each covered a segment and none the conjunction:
`TestTierBSampleReachesItsExecution` drives the join but its execution has no launch and
never projects; `TestProjectionEmitsAllFourSrcStatuses` reaches `resolved` from a hand-built
`ExecutionView` with no `Launch`; `TestProjectionAddsNoFrames` has the CPU stack but asserts
the *negative*. `TestGateASourceLineIsReachedFromACPUStack` drives a real `Timeline` so the
module join and the resolution both run for real.

**Ran** (through the `gpu` package) / **compiled** (end to end).
**This is the assertion with the largest gap between what the gate proves and what the
product does — see "Findings" below.**

### 3. No `-lineinfo`, no invention

**Asserts.** Every no-lineinfo sample carries `gpu_src_status="no-lineinfo"` and no
`gpu_src_file`/`_line`/`_func`; the kernel frame is unchanged, so the resolvable and
unresolvable populations still aggregate at the same kernel.

**Catches.** Synthesizing a nearest line or a function's first line. And, for the second
clause: any frame that varies with `gpu_src_status`, which would split one kernel's block
in two in a flame graph while every label assertion still passed.

**Reused** for the labels — `TestProjectionEmitsAllFourSrcStatuses`.
**New** for the aggregation clause — `TestGateResolvedAndNoLineinfoAggregateAtTheSameKernel`;
nothing asserted that the two populations share a stack.

**Ran** / **compiled** (end to end, generalized there to all three unresolvable statuses).
**Mutation-checked**: making `Resolve` fall back to `Resolve(fn, 0)` when the PC is
uncovered fails assertions 3 and 4.

### 4. All four `gpu_src_status` values reachable, each by the fixture that should produce it

**Asserts.** `resolved` from the `-lineinfo` fixture, `no-lineinfo` from the no-lineinfo
fixture, `no-module` from a CRC no cubin was stored under, `unmapped` from a PC past the
function — in the store *and* in the labels; the enum is exhaustive and its zero value is
not a status.

**Catches.** A fifth value; a silent default; the two "cannot resolve" statuses collapsing
into one, which is the difference between "recompile with `-lineinfo`" and "the compiler
emitted no line here".

**Reused** — `TestModuleStoreAllFourStatusesAreReachable`,
`TestProjectionEmitsAllFourSrcStatuses`, `TestSrcStatusesIsExhaustiveAndStable`,
`TestSrcStatusZeroValueIsNotAStatus`. **Ran** / **compiled** (end to end, with exact
per-status counts).

### 5. Reconciliation

**Asserts.** Every PC record accepted by the sink lands in exactly one of attributed-exact,
attributed-kernel, still-pending (in either of the two pending stores) or evicted (from
either), and the counters sum to what was emitted. Plus the group-level identity: every
pending module group is joined or left pending for exactly one *counted* reason.

**Catches.** A join that attaches samples without counting them (satisfies the sample
identity, reports a wrong per-tier split); a refusal path that returns without incrementing
any counter — the easiest and most invisible mistake available in that function.

**Reused** — `TestConformancePCSampleReconciliationCoversPendingAndEvicted`,
`...CoversAttributedByKernel`, `TestTierBSamplesReconcile`,
`TestTierBJoinAccountsForEveryGroup`. **New end-to-end**: the identity over 64 wire records
plus 44 injected ones, with `PendingModuleSamples == 64` as an equality (see finding 1).
**Ran** / **compiled**.

### 6. Tier B does not collapse

**Asserts.** 10,000 samples across 50 distinct `(crc, functionIndex)` pairs attribute with
no loss to `EvictedPendingSamples`.

**Catches.** Task 8a's pathology returning — a Tier B sample keyed on its (empty)
correlation value, collapsing a whole process onto `{backend, pid, ""}` and evicting
everything past `pendingSampleCap`.

**Reused** — `TestTierBPCSamplesDoNotCollapseOntoOneKey`,
`TestTierBDistinctFunctionIndexSplitsGroups`. **Ran.** Not reachable end to end: the stub
emits 64 records, not 10,000, and none of them attributes at all.

### 7. Ambiguity is marked in its own label

**Asserts.** Two executions of one kernel in the horizon with one Tier B batch give
`gpu_pc_attrib="kernel-ambiguous"` **and leave `gpu_ambiguous` unset**.

**Catches.** Reusing `ExecutionView.Ambiguous` for PC ambiguity, which would put
`gpu_join="exact" gpu_ambiguous="true"` on one sample — two unrelated facts on one boolean,
and `AmbiguousHeuristicMatchCount` no longer meaning what its name says.

**Reused** — `TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous`,
`TestProjectionKernelAmbiguousNeverCoincidesWithGpuAmbiguous`. **Ran.** Not reachable end to
end: the stub's PC records cannot join through the module at all (finding 1).

### 8. Tier A disclosure

**Asserts.** Inside a window `"true"`, in a proven gap `"false"`; Tier A selected with no
window arriving gives `"unknown"` on every execution and `"false"` on none; the three
outcomes partition the executions exactly.

**Catches.** The one answer that must never be reachable by accident — a profile reporting
"not perturbed" when it means "cannot tell".

**Reused** — `TestSerializationMarksExecutionsOverlappingABurst`,
`TestSerializationIsFalseInAProvenGap`, `TestSerializationIsUnknownWhenNoWindowsArrived`,
`TestSerializationFalseIsOnlyEverReachedFromPositiveEvidence`,
`TestSerializedLabelIsUnconditionalAndHasThreeValues`.
**New end-to-end**: real windows from the producer bracket real executions, and the
"no windows" half is driven by re-emitting *this run's own* executions into a second Tier A
`Timeline` that never sees a window — same population, only the evidence differs.
**Ran** / **compiled**.

### 9. Cardinality cap

**Asserts.** Past the ceiling `gpu_pc` is suppressed and nothing else — `gpu_stall`,
`gpu_src_*` and `gpu_pc_attrib` survive, the sample keeps its full share of the execution's
duration — with `ProjectionPCLabelsSuppressed` **equal to the suppressions actually visible
in the output**, and the loss surfaced in `joinhealth`.

**Catches.** A cap that drops the coarser, more actionable labels instead of the numerous
one; a suppression that is silent, since a profile that lost its PC labels looks identical
to one that never had any.

**Reused** — `TestProjectionCapSuppressesGpuPCAndOnlyGpuPC`,
`TestProjectionCapIsSurfacedInJoinHealth`. **New end-to-end**: 40 distinct injected offsets
against a ceiling of 8, with the counter tied to the observed output rather than to a
hard-coded number. **Ran** / **compiled**.

### 10. The cubin channel cannot touch enrolment

**Asserts.** The isolation test runs *as part of the gate*, in **both** orders including
offers flooded *ahead of* an enrolment with the cubin listener's accept loop deliberately
wedged: `CubinsThrottled` non-zero, `UnwindEnrollThrottled` unchanged, the enrolment still
confirmed. Plus: the two admission buckets are separate objects with different numbers, the
two addresses are siblings and not one socket, and the enrolment handler performs **no read**
on the producer's connection — asserted behaviourally and structurally.

**Catches.** Moving cubin traffic onto the enrolment listener, its goroutine, its accept
loop or its bucket. Any of those restores issue #49's ~38 % stack loss on a module-heavy
workload, silently, with only `UnwindEnrollThrottled` moving.

**Reused** — `TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment`,
`TestTheCubinAdmissionBucketIsItsOwn`,
`TestTheCubinAddressIsASiblingOfTheRendezvousAndNotTheSameSocket`,
`TestAnEnrolmentCompletesWithNoReadOnThatConnection`. **Ran.** The privileged gate adds the
live form: real cubin traffic crossing while a real enrolment happens, with
`UnwindEnrollThrottled == 0` and `UnwindEnrollConfirmed == 1`.
**Mutation-checked**: sizing the cubin per-uid bucket at `enrollUIDBurst` fails
`assertion-10-the-buckets-are-separate-objects`.

### 10a. Seals are enforced

**Asserts.** A memfd missing each required seal *in turn* is rejected, counted in
`CubinsRejectedUnsealed`, and **never mapped**; a descriptor that is not a sealed memfd at
all (a pipe, an unsealed tmpfs file) is refused; the required set is spelt out.

**Catches.** Dropping a seal — without `F_SEAL_SHRINK` a peer can `ftruncate` under our
`mmap` and SIGBUS the agent; without `F_SEAL_WRITE` the ELF mutates under the parser
mid-parse — and, worse, any fallback that maps it anyway.

**Reused** — `TestEachRequiredSealMissingInTurnIsRejectedAndNeverMapped`,
`TestADescriptorThatIsNotASealedMemfdIsRefused`, `TestSealNamesAreSpeltOut`. **Ran.**

### 10b. Out-of-scope conditions refuse rather than guess

Three clauses, and they are not in the same state.

- **Two device ids give `gpu_pc_attrib="kernel-multidevice"`** — **reused**
  (`TestTierBMultiDeviceProcessIsMarked`, `TestMultiDeviceOutranksAmbiguity`), **ran**.
- **A window with `end_ns == 0` gives `"unknown"` on everything after it and `"false"` on
  nothing** — **reused**
  (`TestSerializationOpenWindowMakesEverythingFromItsStartUnknownAndNeverFalse`), **ran**;
  the privileged gate also drives it over this run's own executions.
- **A graph execution makes Tier A refuse to start** — **NOT IMPLEMENTED BY THE PRODUCT.**
  See finding 3. Pinned, not asserted, by
  `TestGateGraphExecutionRefusalIsNotAssertableYet`, which **ran**.

### 11. `getcap` on the gate binary shows no `cap_sys_admin`

**Asserts.** From two independent directions, because either alone is escapable: the file
capabilities of the test binary itself (via `cap.GetFile`, the same
`security.capability` xattr `getcap` prints — no external tool needed, so it cannot degrade
to a skip on a container image without `getcap`), and this process's own Permitted set. A
separate test runs `getcap(8)` when present and requires it to agree. Vacuous under root, so
the process half is skipped there and said so, while the file half still runs.

**Catches.** The two quiet one-liners that acquire the requirement: attaching with
`link.Uprobe` instead of `link.UprobeMulti` (the `perf_uprobe` PMU needs CAP_SYS_ADMIN, the
BPF link does not), and reading the producer's address space to follow
`gpu_module_load_v1.bytes_ptr` (needs CAP_SYS_PTRACE) — both of which work fine on a
developer box that runs tests as root and say nothing about why.

**New.** No test asserted it anywhere; the standing Phase 1 assertion existed only as prose.
Composed alongside `TestEmbeddedProgramIsUprobeMulti`, which is *why* no CAP_SYS_ADMIN is
needed. **Ran.**

### 12. `Stats.Undecoded` is zero for every kind this phase decodes

**Asserts.** All five PC-sampling kinds (module, PC, stall map, sampling window, config)
have an `applyBatch` arm and are counted in `Records`, not `Undecoded`; a healthy Tier B run
leaves every new loss counter at zero; an unknown kind is still counted rather than dropped;
`kindMax` matches the embedded BPF object's `dropped` map; every cookie has a sized kind.

**Catches.** A `KIND_*` added on one side of the wire and not the other — silent loss unless
counted — and, in the other direction, a decode arm removed, which would put a kind back
into the default arm while every other counter still read healthy.

**Reused** — `TestTheFivePCSamplingKindsAreDecodedNotCountedUndecoded`,
`TestHealthyTierBRunLeavesEveryNewLossCounterAtZero`,
`TestUndecodedKindsAreCountedNotDropped`, `TestEmbeddedProgramIsUprobeMulti`,
`TestBPFSizesEveryKindCookieForInstalls`. **Ran.**

**End to end this is `Undecoded == 4`, not zero, and that is correct.** `gpu_dropped_v1` is
*not* a kind this phase decodes: it is decoded into `batch.Drops` and carried, and
normalizing a drop class into an operator-visible number is the consumer task after Task 7
(see `Stats.Undecoded`'s own comment). The stub emits exactly one record per drop class when
PC sampling is on, so the gate asserts the equality — a fifth undecoded record means a kind
arrived that nothing on this side knows about. **Compiled.**

---

## Findings — three product gaps the gate surfaced

All three are **reported, not silently worked around**, and each is pinned by a passing test
that **fails the moment the gap is closed**, so the gate is updated rather than left
claiming an assertion it never made. That is the shape issue #44 used and #45 inverted.

### Finding 1 — the stub's PC records cannot attribute to anything, in either tier

`shim/stub/stub.cc` emits real module loads carrying the real checked-in cubins, and real PC
records. They are unrelated:

- the PC records' `cubin_crc` is one of two compile-time constants,
  `kStubCubinCRC = {0xC0FFEE01, 0xC0FFEE02}`, while the modules the same run delivers are
  keyed by a content hash of the fixture bytes — measured on this machine,
  `0x9d57accad01046eb` for `single_lineinfo.cubin`. No cubin is ever stored under a
  `0xC0FFEE0n` key;
- their `correlation` is 0 in every tier — correct, that is what CONTINUOUS collection
  produces — so the exact-correlation path is unavailable to them;
- Tier B attribution runs `crc → module → function name → the execution's KernelName`, and
  the stub's kernel names are `kernel_1111`/`kernel_2222` while the fixtures' only function
  is the CUDA kernel they were compiled from (`addOne`). No name can match.

So neither join path can fire, and the stub cannot drive assertions 2, 3, 4, 7 or 9. The
pipeline is not at fault — it correctly counts all 64 as pending, which the gate asserts as
an exact equality, and `TestTierBKernelNameMismatchStaysPending` already pins that samples
stay pending rather than being attached to a plausible neighbour.

**Consequence for the gate.** `TestStubDrivesPCSamplingToPprofWithoutAGPU` injects 44 PC
samples of its own at `Timeline.EmitPCSample` — the same entry point the consumer calls — on
correlations that a wire-delivered, stack-carrying launch is known to occupy (replayed
exactly by `gpuabi.SampleSchedule`, which is pinned against the shim's own sampler). What is
injected is the *record*; everything downstream is product code — the join, the store's
four-valued resolution, the label set, the cardinality budget. What the injection skips is
the consumer's `KIND_PC` decode arm, and that is asserted separately and exactly by the 64
wire records.

**Fix** (a small change to `shim/stub/stub.cc`, out of scope on a test branch): record the
CRC each capture computed and use it on the PC records; name the kernels after the fixtures'
own functions. **Pinned by** `TestGateTheStubsPCRecordsCannotAttributeToAnything`
(unprivileged, ran).

### Finding 2 — the cubin transport does not feed `gpu.ModuleStore`, so the product cannot resolve a source line at all

Task 3 built the channel; Task 4 built the store. Nothing connects them:

- `Attach` calls `newCubinListener(cfg, nil)`, and a nil sink becomes `memCubinStore` — a
  bounded CRC→bytes map with no line table, no LRU and no `Resolve`. Its own comment says
  "Task 4 replaces it";
- `gpuprobe.Config` has no field by which a caller could supply one, and `gpu.ModuleStore`
  does not satisfy `cubinSink` (`Put`/`HasCubin` versus `PutCubin`/`HasCubin`) even if it
  had;
- `cmd/gpu-cuda-profile` builds neither: `gpu.NewTimeline(gpu.TimelineConfig{PCSampling: tier})`
  with no `Modules`, and `gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{})`.

**On hardware today every cubin is received, sealed, verified, identity-checked, stored —
and then never read, and every PC sample in a real profile reads
`gpu_src_status="no-module"`.** Gate assertion 2, the Phase 6 exit condition, is therefore
not satisfiable by the product as shipped. This is one hop, with both ends built and tested;
it is a wiring task, not a design gap. It is also, per `ProjectionConfig.Modules`' own
comment, *designed* to be visible rather than silent — "no-module" on every sample points
straight at the missing store.

**Consequence for the gate.** The privileged gate builds the `ModuleStore` itself and keys
it on the CRCs the **producer** declared (parsed from the producer's own report and
independently recomputed in Go over the checked-in fixture bytes), not on numbers the test
invented — so the identity the store keys on is the identity that went on the wire. It also
asserts that the decoded `gpu_module_load_v1` records name the same `(crc, size)` pairs.
That is the offline half of hardware assertion 13.

**Pinned by** `TestGateTheCubinTransportDoesNotYetFeedTheModuleStore` (unprivileged, ran).

### Finding 3 — Tier A does not refuse to start where CUDA graphs have been observed

The plan's Task 10 and its out-of-scope section require: *"Tier A refuses to start in a
process where graph executions have been observed … The refusal is loud and counted, not a
silent downgrade to Tier B."*

The wire signal exists — `internal/gpuabi.DropClassGraphExec` decodes and spells itself
`graph-exec`, and the stub emits one such record so the class is reachable from a test. The
refusal does not. **Nothing in `gpu/`, `gpuprobe/` or `cmd/` consumes that drop class**: the
consumer decodes `gpu_dropped_v1` into `batch.Drops` and stops (finding above), so there is
no counter, nothing on the `Snapshot`, no `joinhealth` anomaly, and no input by which
`PCSamplingRequest` could be told. Tier A starts happily in a graph-using process and
produces confident, exact-*looking* attribution of N kernels to one call site — which is the
plan's own finding 4, and the condition it says must be visible rather than merely out of
scope. Graph launches are the norm in inference serving.

What *is* true today, and is asserted: the Tier A acknowledgement refusal an operator must
read before the tier can run names CUDA graphs, and so does the standing warning.

**Pinned by** `TestGateGraphExecutionRefusalIsNotAssertableYet` (unprivileged, ran), which
uses reflection over `Snapshot`, `TimelineDropStats`, `PCJoinStats`, `TimelineConfig` and
`PCSamplingRequest` and fails the moment any of them grows a field naming graph executions.

---

## What ran and what only compiled

**Ran, unprivileged, on this machine** — `go test ./gpu/ ./gpuprobe/ ./internal/... -count=1`
and `-race -count=4`, both green:

- assertions **1, 3, 4, 5, 6, 7, 8, 9, 10b** (two of three clauses) via `TestPhase6Gate`;
- assertion **2** via `TestGateASourceLineIsReachedFromACPUStack`, through a real `Timeline`
  and a real `ModuleStore` over the checked-in `-lineinfo` fixture;
- assertions **10, 10a, 11, 12** via `TestPhase6GateConsumerHalf`;
- all three outstanding-gap pins.

**Compiled only** — `TestStubDrivesPCSamplingToPprofWithoutAGPU`. `CapEff: 0` and no
passwordless sudo on this machine (`sudo -n true` → "a password is required"), so it skips
with the message that names the setcap line. It type-checks, vets and lints clean, and every
number in it is derived rather than guessed:

- 58 sampled launches from `gpuabi.SampleSchedule(500, 8, DefaultSampleSeed)`, the same
  constant the baseline gate uses;
- 64 PC records, 2 cubins, 8 stall reasons, 4 drop records, 4 bursts — all read out of
  `shim/stub/stub.cc` and confirmed by **running the producer standalone** with the exact
  environment the gate sets. That run printed `pc_sampling=serialized`,
  `pc_samples=64 stall_reasons=8 cubins=2 functions=4 drop_classes=4`, and captured both
  fixtures with the CRCs above;
- the FNV-1a CRC replica in the test was checked against the producer's own output for both
  fixtures and matches exactly.

What has *not* been executed is the attach, the walk, the symbolization and the assertions
that read `Stats` after a live run. Those need `cap_bpf,cap_perfmon,cap_checkpoint_restore`.

**Mutation checks** (each applied, run, and reverted):

| mutation | gate result |
| --- | --- |
| `projectionFrames` appends `[gpu:src:mutant]` | assertions 1, 2, 3 FAIL |
| `ModuleStore.Resolve` falls back to `Resolve(fn, 0)` for an uncovered PC | assertions 3, 4 FAIL |
| cubin per-uid admission bucket sized at `enrollUIDBurst` | assertion 10 FAIL |

---

## Outstanding: assertions 13–16 (RTX 3090)

None of these is attempted here; all four need the hardware.

13. **`cuptiGetCubinCrc()` over the received copy equals the PC records' `cubinCrc`.** The
    gate proves the *shape* of this offline — one number identifies one set of bytes and the
    same number reaches both ends — against the stub's FNV-1a stand-in. CUPTI's polynomial
    is unpublished and there is no CUDA toolkit on the agent path, so the real equality is
    hardware-only.
14. **Tier B: a flame graph reaching a real line of `cuda_workload.cu` from a CPU stack,
    through labels** — the Phase 6 exit condition on real hardware. Blocked additionally by
    finding 2: the transport→store hop must be wired before this can pass on any machine.
15. **Tier A: `correlationId` non-zero on ≥ 99 % of PC records; `gpu_pc_attrib="exact"` on
    the resulting samples; windows bracketing the executions that ran in them.**
16. **Overhead within the Task 12 thresholds, or the tier decision they dictate.** Task 12
    is active on `.worktrees/pc-overhead` and its numbers are not in the plan file yet.

## The plan's own "cannot verify without hardware" list, restated

Beyond assertions 13–16, and unchanged by this task:

- whether `functionIndex` **is** the cubin's `.symtab` index — the finding-2 question of the
  plan, and the trigger for `gpu_pc_sample_batch_v2`. Every test in this tree, including the
  gate's, reads the index out of the fixture and asserts only that the store and the sample
  use the same one consistently;
- whether `pcOffset` is function-relative in the sense the line table is;
- every rate and buffer-sizing number (the spike's 352 records for ~103 k samples drives all
  of them);
- hardware-buffer overflow behaviour — `droppedSamples` / `hardwareBufferFull` under a
  saturating workload;
- whether the collection mode can change between `Stop` and `Start` without a full
  `Disable`/`Enable` — undocumented, and it decides whether a future tier switch is possible
  at all;
- whether per-context enable holds when contexts are created lazily on other threads;
- whether the `MODULE_UNLOAD_STARTING` drain preserves PC uniqueness across a
  load–unload–load cycle;
- whether a `cuptiFinalize` handler runs at all, and whether disabling per context inside it
  errors.

**Not verifiable at all, on hardware or otherwise:** MPS, and cross-process contention for
the per-device PC-sampling hardware. A process cannot observe either from inside itself.
Stated so a quiet profile is not read as a correct one.

## Verification run

```
make -C shim && make -C shim test && make -C shim check-fpless \
  && make -C shim check-cubin-defer && make -C shim nvidia     # all OK
go build ./... && go vet ./...                                  # clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1              # ok
go test ./gpu/ ./gpuprobe/ -race -count=4                       # ok
~/go/bin/golangci-lint run --timeout=5m                         # 0 issues
```

`git diff --stat` for the one modified file: `gpuprobe/gate_test.go`, 803 insertions,
0 deletions.
