# Task 6 — Tier B: `CONTINUOUS` PC sampling in the CUPTI adapter

Branch `feat/tier-b-continuous`, one commit on top of `origin/main` (`11357463`).

Every CUPTI API the plan names exists in CUDA 13.3 exactly as described — `cuptiPCSamplingEnable/Disable`,
`SetConfigurationAttribute`, `GetConfigurationAttribute`, `GetData`, `GetNumStallReasons`, `GetStallReasons`,
`CUpti_PCSamplingData` with `totalSamples`/`droppedSamples`/`nonUsrKernelsTotalSamples`/`hardwareBufferFull`,
and `CUpti_PCSamplingPCData` with `cubinCrc`/`pcOffset`/`functionIndex`/`functionName`/`correlationId`.
Nothing was improvised around a missing API.

**Tier B is off by default.** `PERFAGENT_GPU_PC_SAMPLING=1` turns it on. With it unset the adapter
allocates nothing, subscribes no extra domain, enables no extra activity kind and calls no PC-sampling
API. Tier A (`KERNEL_SERIALIZED`) and tier selection are untouched.

---

## One ABI gap the brief assumed away

The brief says to "add `classPCDroppedHW`, `classPCBufferFull`, `classPCNonUserKernel` and `classGraphExec`
to the drop-class enum". **There was no drop-class enum, and no way for `gpu_dropped_v1` to reach a
consumer at all.** The record has been in `shim/core/usdt_abi.h` since Phase 3 with no `KIND_*` in
`bpf/gpu_usdt.bpf.c`, no `cookieFor` entry, no probe in either producer and no decoder arm. So every class
this task defines would have been a counter that could not go non-zero — the exact shape of the twelve
past defects the constraints name.

Wiring it was therefore part of the task, not an extension of it:

- `GPU_DROP_CLASS_*` in `usdt_abi.h` (0 reserved for "unset", 1–4 the four classes), with offset asserts
  on `count`/`klass` and C11 coverage in `core/usdt_abi_test.c`.
- `KIND_DROPPED = 10`, `REC_DROPPED 16`, and `record_size`/`max_records` arms in `bpf/gpu_usdt.bpf.c`.
  `KIND_MAX` stays 16, so **no BPF map layout changed**; both `.o` files are regenerated.
- `kindDropped`, `cookieFor("gpu_dropped_v1")`, a `decodeBatch` arm and `batch.Drops` in `gpuprobe`.
- `gpuabi.DropClass*` constants and `DropClassName`, pinned against the C `#define`s by a test.

Normalizing drops into an operator-visible `Stats` counter is left to **Task 7**, which is where the plan
puts every other new kind's `applyBatch` arm; until then they land in `default:` and are counted as
`Stats.Undecoded`, exactly as `kindStallMap`/`kindConfig`/`kindSamplingWindow` do today. Nothing is silent
in the meantime.

`MAX_BATCHED_RECORD_BYTES`, `MAX_RECORD_BYTES`, every frozen layout and the enrollment/cubin paths are
untouched.

---

## The enable path

On `CUPTI_CBID_RESOURCE_CONTEXT_CREATED`, under one process-wide mutex:

1. `cuptiPCSamplingEnable(ctx)`. Enable **before** configure — this is the order NVIDIA's own continuous
   sampling sample uses, and the stall-reason query is keyed on a context CUPTI already knows is sampled.
2. `cuptiPCSamplingGetNumStallReasons` / `GetStallReasons`, **once per process**. Each entry becomes a
   `gpu_stall_reason_map_v1`, recorded into `ReplayLog` first and emitted second, so the replay copy
   exists even though no consumer is realistically attached at context-creation time.
3. Buffer setup: a client-owned `CUpti_PCSamplingData` whose `pPcData` is an array of `collectNumPcs`
   `CUpti_PCSamplingPCData`, each with its own `numStallReasons`-entry `stallReason` array.
4. `cuptiPCSamplingSetConfigurationAttribute` with `COLLECTION_MODE = CONTINUOUS`, all stall reasons,
   `OUTPUT_DATA_FORMAT = PARSED`, the sampling data buffer, and the period / scratch / hardware-buffer
   sizes when configured. `ENABLE_START_STOP_CONTROL` is deliberately left off: it is Tier A's mechanism,
   and turning it on in CONTINUOUS mode changes when a flush is required for nothing in return.
5. **Per-attribute `attributeStatus` is checked, not just the call's return.** A configuration call can
   succeed with one attribute refused, and a silently unapplied `COLLECTION_MODE` would serialize every
   kernel in the process while the profile claimed it had not.

Any failure after step 1 disables the context again rather than leaving it half-applied, and increments
`g_ctx_enable_failed`.

If no period was configured it is **read back** with `cuptiPCSamplingGetConfigurationAttribute`, so
`gpu_config_v1` reports what the device is doing rather than what we asked for.

### `gpu_config_v1`

Emitted once, from the first drain tick after a context enabled, and recorded in `ReplayLog` for late
attach. `vendor = GPU_VENDOR_NVIDIA`. `sm_count` and `clock_hz` come from a `CUpti_ActivityDevice6`
record (`numMultiprocessors`, `coreClockRate` × 1000) — there is no `cuptiDeviceGetAttribute` for either
and the adapter does not link libcuda, so `CUPTI_ACTIVITY_KIND_DEVICE` is enabled under Tier B only. If
the device record has not arrived by the first tick the record still goes out with `sm_count = 0` and
`g_config_no_device` counts it, rather than being withheld for a field it does not need.

`sampling_factor` carries **`2^period` — SM cycles between PC samples**. It is not a scale factor and no
count is ever multiplied by it. The field name is a hazard inherited from the frozen record; the value
chosen is the one a reader can act on, and the exponent is in the log beside it.

---

## The four drop classes, and what each reads on a healthy run

| class | wire value | source | healthy value |
| --- | --- | --- | --- |
| `GPU_DROP_CLASS_PC_DROPPED_HW` | 1 | `CUpti_PCSamplingData.droppedSamples`, as a delta | **0**. Non-zero says lower the sampling frequency. |
| `GPU_DROP_CLASS_PC_BUFFER_FULL` | 2 | `hardwareBufferFull`, **and** the documented `CUPTI_ERROR_OUT_OF_MEMORY` return from `cuptiPCSamplingGetData` | **0**. Non-zero says raise `HARDWARE_BUFFER_SIZE` or drain more often. |
| `GPU_DROP_CLASS_PC_NON_USER_KERNEL` | 3 | `nonUsrKernelsTotalSamples`, as a delta | **not expected to be zero.** It is a completeness bound, not a fault. |
| `GPU_DROP_CLASS_GRAPH_EXEC` | 4 | `CUpti_ActivityKernel12.graphId != 0` | **0** for a process that launches no CUDA graphs. |

Both spellings of "hardware buffer full" feed class 2, because CUPTI reports it in-band on a successful
call *and* as an error return, and only one of the two is obvious from the header.

Counts are emitted as **deltas**, not running totals: CUPTI's counters are cumulative per context and a
consumer sums drop records.

`GPU_DROP_CLASS_GRAPH_EXEC` is emitted **before** the Tier B gate in `on_tick`. The condition it discloses
is a property of the launch→execution join, not of PC sampling, and it is exactly as invisible with PC
sampling off as with it on.

---

## The two structural omissions, and how the profile states them

`cupti_pcsampling.h` documents two omissions on `CUpti_PCSamplingData` that no counter can recover:

> "CUPTI does not provide PC records for non-user kernels."
> "CUPTI does not provide PC records for instructions for which all selected stall reason metrics counts are zero."

**The first has a size.** `nonUsrKernelsTotalSamples` rides out on `gpu_dropped_v1` under
`GPU_DROP_CLASS_PC_NON_USER_KERNEL` — a wire record with no ABI change, because the frozen
`{count u64, klass u8}` already accommodates it. A reader can therefore see how much of the device's
sampled time this mechanism structurally cannot attribute. It is filed as loss rather than as a metric
precisely so it cannot be mistaken for something the profile measured.

**The second has no size at all.** An instruction that never stalled for any selected reason simply is not
in the data. There is no counter, in CUPTI or here, that could produce its magnitude. It is stated in
prose in `usdt_abi.h`, in the adapter's header comment, in `gpuabi.DropClassPCNonUserKernel`'s doc comment,
and here — and nothing in the pipeline presents the remainder as complete.

### `totalSamples` is not loss, and the identity is not a check

`totalSamples` counts every sample the hardware took, including the ones that became records. It has no
wire field, so the relation

```
emitted_counts + droppedSamples + nonUsrKernelsTotalSamples  <=  totalSamples
```

is checkable only in this process's log. It is an **inequality**, not an equation: the gap is the
all-zero-stall instructions, which are unobservable. The adapter logs both sides with the words
"log only, not a check" attached. **It is stated as a limitation and claimed as a check nowhere.**
Closing it would need `gpu_config_v2` and is not worth a version bump for a diagnostic.

---

## The module-unload drain

`CUPTI_CBID_RESOURCE_MODULE_UNLOAD_STARTING` calls `cuptiPCSamplingGetData` on every tracked context
before returning. `cupti_pcsampling.h` requires this after every module load-unload-load in CONTINUOUS
mode to keep PCs unique; missing it does not lose data, it makes two different instructions share a PC
identity — silently, with nothing counted anywhere. It is the worst failure available here, so the unload
path **blocks** on the PC lock rather than trying it. A skipped flush is exactly what must not happen.

The scheduling around it is `shim/core/pcdrain.h` — `PCDrainSchedule`, a clock-parameterised class with no
CUPTI state, so `core/pcdrain_test.cc` proves the rules against a fake clock with no GPU:

- the first tick always drains;
- a tick inside the period does not, and counts `coalesced`;
- a forced (unload) drain always happens, whatever the phase — five unloads inside one period produce five
  drains;
- **a forced drain resets the phase**, so a module-churning process does not drain twice in quick
  succession forever;
- teardown drains are counted apart from unload drains;
- `period_ns == 0` degrades to "every tick", never to "never";
- under a two-thread race between the unload path and the tick path, `periodic + coalesced` equals the
  number of ticks exactly.

---

## The finalize handler

There was none in `shim/` before this. `on_finalize()` disables PC sampling on every tracked context
before anything else and counts `g_finalize_seen`. It is reached from two places:

- the `CUPTI_CB_DOMAIN_STATE` / `CUPTI_CBID_STATE_FATAL_ERROR` callback — the only notification CUPTI gives
  before invoking `cuptiFinalize()` itself, and precisely when an enabled PC-sampling session would
  otherwise have the rug pulled from under it. The domain is subscribed under Tier B only;
- `at_exit_handler`, ahead of `cuptiActivityFlushAll`.

Each context is **drained and then disabled**, in that order: `cuptiPCSamplingDisable` is what joins
CUPTI's worker threads and copies the last records into our buffer, discarding whatever does not fit, so
anything not pulled first is gone.

It takes the PC lock with **`try_lock`**, and this is the one place that is right. The fatal-error callback
can arrive on the thread already inside a CUPTI call this adapter made while holding that lock; a blocking
acquire would deadlock somebody else's process at the worst possible moment. Skipping the teardown loses
the tail of the profile, deadlocking loses the application. The skip counts `g_finalize_contended`, which
must be 0 on a healthy run.

---

## Per-context tracking

Enable, configure, drain and disable are per `CUcontext`, never per process. Contexts are tracked in a
list; `CUPTI_CBID_RESOURCE_CONTEXT_DESTROY_STARTING` drains, disables and removes the matching one.
Counters:

| counter | healthy |
| --- | --- |
| `ctx_seen` | contexts created |
| `ctx_enabled` | == `ctx_seen` |
| `ctx_enable_failed` | **0** — a context that fails to enable is otherwise a silent hole in coverage |
| `ctx_destroyed` | contexts destroyed |
| `ctx_disable_failed` | **0** |

Also assertable at zero: `getdata_failed`, `drain_rounds_capped`, `pc_batch_dropped`, `finalize_contended`,
`multi_device`. Also reported: `pc_records`, `pcs`, `zero_stall_pairs`, `getdata`, the four drain-schedule
counters, `module_unload_drains`, `finalize_seen`, `config_emitted`, `config_no_device`.

`CUpti_ResourceData` carries no `contextUid`, so the log names the `CUcontext` pointer rather than
inventing a zero uid that no other record shares.

---

## The multi-GPU guard

Distinct `device_id`s are tracked from `gpu_exec_v1`'s source, the kernel activity record — the only place
a device id exists, since `gpu_pc_sample_batch_v1` has no field for one. Two devices running the same
binary produce the **same** `cubin_crc`, so their PC samples are not separable on the wire at all. On the
second distinct device the shim logs a warning naming exactly that, and increments `g_multi_device`; the
agent's `MultiDeviceProcesses` and Task 9's `gpu_pc_attrib="kernel-multidevice"` are the consumer half.
For this phase a two-GPU process is a configuration mistake, not a condition to tolerate.

The check runs on every kernel activity record, on the CUPTI worker thread, whether or not Tier B is on, so
the common single-device case is one relaxed atomic load and a compare — no lock.

---

## One record per (PC, stall reason) pair

`CUpti_PCSamplingPCData` carries a variable-length stall array and `gpu_pc_sample_batch_v1` cannot, so the
array is flattened: one 40-byte record per pair, which §6.3 names as the price of the fixed-size record
rule. Pairs with `samples == 0` are **not** emitted — they carry no information and would multiply wire
volume by the number of stall reasons the device has — and they are counted in `zero_stall_pairs` so the
suppression is visible.

`correlation` is written straight from `pc.correlationId`, which the ABI says is zero in CONTINUOUS mode.
It is not forced to zero here: if a driver ever supplies one, the consumer sees the truth rather than our
assumption.

The drain loop pulls while `remainingNumPcs` is non-zero, bounded at 64 rounds, and counts
`drain_rounds_capped` if it hits the bound rather than spinning inside somebody else's process.

---

## Verification actually run

```
make -C shim && make -C shim test && make -C shim check-fpless && make -C shim nvidia   # all pass
go build ./... && go vet ./...                                                          # clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1                                      # all pass
go test ./gpuprobe/ -race -count=4                                                      # pass
~/go/bin/golangci-lint run --timeout=5m                                                 # 0 issues
```

The adapter builds against real CUDA 13.3 headers and links real `libcupti`, and still exports exactly one
dynamic symbol (`InitializeInjection`).

**New tests:**

- `shim/core/pcdrain_test.cc` — the drain schedule against a fake clock, including the two-thread race.
- `shim/core/drain_test.cc` — stall-map replay: interning by index, replay on the attach edge only,
  ordered config → stall map → modules.
- `shim/core/usdt_abi_test.c` — `gpu_dropped_v1` byte layout and the five class values, in C11.
- `internal/gpuabi` — `DecodeDropped` layout and short-buffer, class distinctness, `DropClassName`
  including an unknown class, and the Go↔C `#define` pin for all five classes plus `GPU_VENDOR_NVIDIA`.
- `gpuprobe` — `gpu_dropped_v1` has a kind, a cookie and a decode arm; an overlong dropped batch errors;
  **every probe a producer fires has a cookie and vice versa** (with the two not-yet-fired probes named
  and attributed to Tasks 5 and 10, so the list must shrink to empty); **every emitter's pinned wire size
  matches the BPF program's `REC_*`**.

**The stub** (`PERFAGENT_STUB_PC_SAMPLES=<n>`, default 0) emits synthetic PC batches across two cubin CRCs
and four function indices, the eight-entry stall map, a config record and one drop record per class,
replaying the map and config through the same `ReplayLog` the adapter uses. **PC samples are emitted
before the stall map on purpose** — a stall index is unresolvable until the map arrives, and Task 7's
consumer has to hold those samples rather than render `stall#17` or drop them. All eight probes are
present in `readelf -n` output for both producers. With the env unset the stub's wire is byte-for-byte
what it was, so `probe_order_test` and the existing gates are unperturbed.

---

## Cannot verify — every item needs the RTX 3090

Nothing below was executed. `CapEff: 0`, no GPU on this machine.

**From the plan's own list:**

1. That 38 stall reasons arrive on GA102, and their names.
2. The actual PC-record rate for the test workload (the spike saw 352 records in CONTINUOUS for ~103k
   samples; that ratio is what `kPCDefaultCollectNumPcs = 2048` is sized against).
3. **Whether `functionIndex` is the cubin `.symtab` index** — the finding-2 question and the trigger for
   `gpu_pc_sample_batch_v2`. The adapter writes it through verbatim and does not interpret it.
4. **Whether `pcOffset` is function-relative in the same sense the line table is** — if it is module- or
   section-relative instead, every `Resolve` in Task 4 is off by a function base.
5. `droppedSamples` / `hardwareBufferFull` behaviour under a saturating workload.
6. That the `MODULE_UNLOAD_STARTING` drain actually preserves PC uniqueness across a load-unload-load
   cycle.
7. That the `cuptiFinalize` handler runs at all, and that disabling per context inside it does not error.
8. Tier B overhead (Task 12).

**Discovered while implementing, and added to that list:**

9. **The enable-then-configure order.** `cupti_pcsampling.h` does not state an ordering. This follows
   NVIDIA's sample; if `cuptiPCSamplingSetConfigurationAttribute` must precede `cuptiPCSamplingEnable`,
   every context reports `ctx_enable_failed` — loudly, but on hardware only.
10. **Whether the stall-reason query works on a just-enabled context.** Both queries take a `CUcontext` and
    are made after the enable. A failure disables and counts, but which side of the enable they belong on
    is unmeasured.
11. **Whether `CUPTI_ACTIVITY_KIND_DEVICE` delivers before the first drain tick.** If it does not,
    `gpu_config_v1` ships with `sm_count = 0` and `config_no_device = 1`. Honest, but the value is wanted.
12. **Whether `coreClockRate` is kHz on this device.** Assumed from CUPTI's documentation and multiplied
    by 1000 into `clock_hz`.
13. **Whether `cuptiPCSamplingGetData` can re-enter our resource callback.** The drain holds the PC mutex
    across the call; a re-entrant `MODULE_UNLOAD_STARTING` would self-deadlock. `on_finalize` is defended
    with `try_lock`; the drain path is not, because a missed unload flush is worse than the risk.
14. **Whether `remainingNumPcs` ever exceeds 64 rounds** — i.e. whether `drain_rounds_capped` can move on a
    real workload, and whether `collectNumPcs` should be larger.
15. **Whether the default sampling period read back from CUPTI is inside 5..31**, and therefore whether
    `sampling_factor = 1u << period` is ever emitted as 0.
16. **`nonUsrKernelsTotalSamples` magnitude relative to `totalSamples`** — the size of the first structural
    omission, which is the number that decides whether the completeness bound is a footnote or a headline.
17. **Whether a `CUcontext` can be created before `InitializeInjection` returns**, i.e. whether
    `CONTEXT_CREATED` can fire before `g_pc_tier_b` and the buffers are set up. The env read and every
    allocation happen before `cuptiSubscribe`, which is the earliest a resource callback can reach us, so
    this is believed safe by construction and unverified in practice.
18. **Whether `Batch<gpu_pc_sample_batch_v1, 32>` is the right batch size** under a real record rate, and
    whether flushing inside the PC lock costs anything measurable.
