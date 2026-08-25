# GPU V2 Phase 6 — PC Sampling, Stall Reasons and SASS→Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Instruction-level GPU profiling — a per-instruction stall-reason histogram, resolved to a CUDA source line, attributed to a kernel and (in one of the two tiers) to the CPU stack that launched it. Two collection tiers, both opt-in, neither on by default. Nothing inferred is presented as measured.

**Architecture:** The shim enables CUPTI PC sampling per context and drains it on the drain timer it already owns. PC records key on `cubin_crc`, so the cubin bytes must reach the agent — they travel out of band over a dedicated AF_UNIX channel built beside — and deliberately **not** shared with — the enrollment rendezvous in `gpuprobe/enroll.go`, because the ABI's `bytes_ptr` is a pointer in the *producer's* address space and reading it from the agent would need `CAP_SYS_PTRACE`. The agent parses the cubin as an ELF, reads its DWARF line table, and turns `(function, pcOffset)` into `(file, line)`. Everything the PC sample carries lands in pprof **sample labels**; frames stop at the kernel, exactly as spec §8 rules.

**Tech Stack:** Go 1.26 (`debug/elf`, `debug/dwarf` — no cgo, no CUDA toolkit on the agent side), `github.com/cilium/ebpf` v0.21, C++17 for the shim, CUPTI 13.3 (`cupti_pcsampling.h`), testify.

**Spec:** `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` — §2 (serialization ruled out for continuous production profiling), §4 (invariants; honesty), §6.3 (the record layouts and the CUPTI spike's four findings), §7 (clock domain), §8 (frames versus labels), §9 (overhead control), §10 (correlation, joins, and the eviction horizon), §14 Phase 6, §15 (`-lineinfo` prerequisite), §16 (risks).

**Predecessors:** Phases 1–4b, all merged. `origin/main` at `1dfa5f55`.

---

## Global Constraints

- Go 1.26.0+. CGO is required for the agent (blazesym); export the block in "Build environment" before any `go` command. The **cubin reader added by this plan is pure Go and must stay cgo-free** — the agent runs in containers that have no CUDA toolkit.
- **Linux 6.6+ at runtime.** Attach stays `link.UprobeMulti`. Never `link.Uprobe` / the `perf_uprobe` PMU.
- **The capability set does not grow.** `cap_bpf,cap_perfmon,cap_checkpoint_restore`. In particular **nothing in this plan may read another process's memory** — no `/proc/<pid>/mem`, no `process_vm_readv`. Both need `CAP_SYS_PTRACE`. This is the constraint that shapes Task 3 and it is not negotiable for a per-pod agent.
- Records stay fixed-size, little-endian, naturally aligned. Existing layouts are frozen: Launch 48, Exec 48, ModuleLoad 40, PCSample 40, Config 24, Dropped 16, LaunchSampled 56, KernelName 272. **New records are new probes, never mutations of old ones.**
- Probe arguments stay pinned to `rdi/rsi/rdx`; the descriptor is `8@%rdi 8@%rsi 8@%rdx`.
- All `*_ns` on the wire are CPU-monotonic. Conversion happens in the adapter, never in `core/` (§7).
- **No loss is ever silent, and no inference is presented as a measurement.** Every lossy or inferred step in this plan gets a counter, and every counter is assertable from a test. Nine defects on this project have been counters reading green exactly when things were worst; a counter that cannot go non-zero in a test is not a counter.
- The shim does no work when no consumer is attached; every emit is gated on its probe's semaphore.
- Do not commit `CLAUDE.md`. No `Co-Authored-By` lines.
- Commit with `git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit`.

## Build environment

```bash
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release
```

Run `go test` and `go generate` directly. Do not run `make test-unit`.

Privileged tests need `cap_bpf,cap_perfmon,cap_checkpoint_restore`. Build such binaries **outside `/tmp`** (it is `nosuid`) and link blazesym statically:

```bash
go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test \
  -ldflags '-linkmode external -extldflags "-Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"'
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep /home/diego/gpuprobe.test
```

**The implementer has `CapEff: 0`.** Every task below states what it proves without a GPU and without capabilities, and Tasks 1–4, 7, 8a, 8b, 9 and 11 are fully verifiable that way. Tasks 5, 6, 10, 12 and half of 13 are not; they say so. Fourteen units of work in dependency order.

---

## The two tiers

Both are opt-in, both default off, and they are **mutually exclusive** (Task 11).

**Tier A — `KERNEL_SERIALIZED`, duty-cycled.** CUPTI populates `correlationId` on every PC record in this mode. The spike measured 1,828 of 1,828, each matching a launch callback's id, so a PC sample joins to a launch — and therefore to a CPU stack — *exactly*. The cost is that kernels serialize while sampling is enabled, which §2 rules out for continuous production profiling. Bound it by duty-cycling: `cuptiPCSamplingStart()` for a ~50 ms burst, `cuptiPCSamplingStop()`, repeat, with the gap tuned to a target samples/sec (Parca ships ~50 ms bursts targeting ~100 PC/stall pairs per second). Every kernel that ran inside a burst was perturbed, and the profile must say which ones.

**Tier B — `CONTINUOUS`.** No serialization, so it is the only tier that is a candidate for always-on. `correlationId` is 0 on every record, so the exact rung is not merely unlikely, it is unavailable. Attribution runs `cubin_crc → module → function → symbol`, reaching the *kernel*. A kernel launched from one call site attributes exactly; a kernel launched from several is an inference and is marked.

Neither tier changes anything about executions: `gpu_exec_v1` is unaffected, so measured GPU *time* is exactly as measured as it is today in both tiers. What the tiers differ on is what a *PC sample* can be attributed to.

---

## Output representation — settled, not open

Spec §8 rules PC offset and stall reason into **labels**, not frames, because at PC-sampling rates one frame per PC destroys aggregation and fragments the kernel's own block. That ruling stands. Frames are exhaustively:

```
<real CPU stack> → [gpu:launch] → [gpu:kernel:<name>]
```

or, for an execution whose launch carried no sampled stack, `[gpu:launch unsampled] → [gpu:kernel:<name>]`. **This plan adds no frame.** Task 13's gate asserts that negatively.

Parca independently made the same call: their backend disassembles cubins to turn a PC offset back into a function, file and line, and the result rides as metadata rather than as a stack layer. Brendan Gregg's AI Flame Graphs and Intel's `iaprof` go the other way, promoting both a source layer and an instruction layer to frames. We are deliberately taking the first. Recorded here as corroboration, not as a decision to revisit: the labels this plan emits carry file, line and function, so a consumer that wants the layered visual can build it from a profile we produce. Nothing is foreclosed.

Source resolution is **fully in scope** — it is Tasks 1, 3, 4 and 9. Only its destination changed.

### Label cardinality, and what needs bounding

pprof stores one string-table entry per distinct label key and per distinct label value; a sample stores index pairs. The question is which of these grow with run length.

| label | distinct values | grows with |
|---|---|---|
| `gpu_stall` | 38 on GA102 — a device-fixed enum | nothing |
| `gpu_pc` | one per distinct *sampled instruction* | the **static** size of the hot cubins; saturates |
| `gpu_src_status` | 4 | nothing |
| `gpu_src_file` | one per distinct source file | source tree, tens |
| `gpu_src_line` | one per distinct `(file, line)` | strictly ≤ `gpu_pc` |
| `gpu_src_func` | one per distinct device function | tens to hundreds |
| `gpu_pc_attrib` | 3 | nothing |
| `gpu_serialized` | 3 | nothing |
| `gpu_correlation` *(already shipping)* | one per execution | **run length — unbounded** |

The load-bearing point: **every label this phase adds is bounded by the profiled binary, not by the length of the run.** `gpu_pc` saturates — once every hot instruction has been sampled at least once, a ten-minute run adds no new values that a one-minute run did not. `gpu_src_*` is strictly coarser than `gpu_pc`. The already-shipping `gpu_correlation` is the only unbounded label in the profile (4,000 distinct values in 4,000 samples on the real Phase 4 profile), and this phase does not make it worse. So the profile's cardinality *regime* is unchanged by this work.

The collapse ratio is real and measurable offline. A trivial `-lineinfo` cubin built for sm_86 during planning: 384 bytes of SASS = 24 instructions = up to 24 distinct PCs, collapsing to **5 distinct source lines**. Kernels with unrolled loops collapse much harder, because every unrolled copy of a loop body carries the same source line.

Size estimate for the pathological end: 20,000 distinct PCs × (~20 bytes per hex string plus protobuf overhead) ≈ 400 KB pre-gzip, ~140 KB after (hex compresses well). Tolerable, and it saturates.

**What still needs bounding** is a JIT- or template-explosion workload that loads thousands of distinct cubins. Two bounds, both counted:

- the module store is bounded and LRU (Task 4), counting `EvictedModules`;
- the projection caps distinct `gpu_pc` values per profile (Task 9). Past the cap `gpu_pc` is **dropped and counted** in `ProjectionPCLabelsSuppressed`, while `gpu_stall` and `gpu_src_*` survive — they are coarser and more actionable, so the label that is dropped first is the one that is least useful and most numerous.

### When the cubin carried no `-lineinfo`

An absent label reads as "not sampled". An explicit one reads as "sampled, unresolvable". Those are different facts and the profile must distinguish them, so **`gpu_src_status` is set unconditionally on every PC-derived sample**, exactly as `gpu_join` is:

> `gpu_join` is set unconditionally (not `omitempty`, no "only if non-default" branch) … an ABSENT label must never be readable as "exact" by a consumer that doesn't know to check for its absence. — `gpu/projection.go`

Four values, mutually exclusive and exhaustive:

| `gpu_src_status` | means | `gpu_src_file` / `_line` / `_func` |
| --- | --- | --- |
| `resolved` | the cubin's line table covers this `pcOffset` | present |
| `no-lineinfo` | the cubin is in hand and has **no** `.debug_line` — built without `-lineinfo` | absent |
| `no-module` | the cubin never reached the agent (loaded before attach and replay missed it, or evicted) | absent |
| `unmapped` | the cubin has a line table, but nothing covers this `pcOffset` | absent |

`no-lineinfo` is a property of the module and `unmapped` is a property of the PC; keeping them apart is the difference between "recompile with `-lineinfo`" and "the compiler emitted no line for this instruction", which are different actions for the reader. Never synthesize a line — no nearest-line search, no "the kernel's first line".

---

## What already exists — do not rewrite

Several things the brief might suggest building are already built. Read them before writing anything.

- **`gpu/projection.go` already projects PC samples.** `ProjectExecutions` emits one pprof sample per `GPUPCSample`, already sets `gpu_pc` and `gpu_stall`, already clones the common label map so per-sample labels cannot be forged by producer-supplied `Tags`, and already splits the execution's *duration* across its PC samples proportionally by `Count` with a 128-bit intermediate (`distributeExecutionWeight`). Task 9 extends this; it does not replace it.
- **`gpu/types.go` has `GPUPCSample` and `ModuleRef`.** `ModuleRef` already keys on CRC, not on a module id. `GPUPCSample.StallReason` is a **string**, so the `index → name` resolution happens before the canonical type, in the consumer.
- **`bpf/gpu_usdt.bpf.c` already carries `KIND_MODULE` and `KIND_PC`** with correct record sizes (40 each) and correct `BATCH_CAP`s. `gpuprobe.cookieFor` already maps `gpu_module_load_v1` and `gpu_pc_sample_batch_v1` to those kinds and attaches their probes. The transport is done; the two kinds land in `applyBatch`'s `default:` arm and are counted in `Stats.Undecoded`.
- **`gpu/timeline.go` has a bounded, horizon-evicted pending PC-sample store**, `AttributedPCSamples` / `PendingSamples` / `PendingCorrelations` gauges, and `Dropped.EvictedPendingSamples`, all reconcilable. `gpu/conformance_test.go` already has `TestConformancePCSampleReconciliationCoversPendingAndEvicted`.
- **`gpuprobe/enroll.go` + `shim/core/enroll.h`** are a working, PID-verified, rate-limited AF_UNIX rendezvous between shim and agent, addressed by the shim's own dev:inode so neither end needs a path or an env var. Task 3 extends this rather than inventing a channel.
- **`shim/nvidia/cupti_adapter.cc` already subscribes `CUPTI_CB_DOMAIN_RESOURCE`** and histograms every resource cbid it sees in `g_resource_events[]`. Module-load and context-created callbacks already arrive; nothing reads them yet.
- **`shim/core/drain.h`** already owns the 100 ms drain timer that `on_tick` uses for `cuptiActivityFlushAll`, clock resampling and late-attach replay. PC-sample draining hangs off the same timer.
- **`shim/nvidia/testdata/cuda_workload.cu` is already built with `-lineinfo -g`** (see `shim/Makefile`), so the hardware gate has a source-mapped workload without any change.
- **`gpu/joinhealth.go`** already renders join counters and anomalies for the operator, including a PC-sample clause.

## Scope boundary

In scope: cubin capture and transport, cubin line-table parsing, PC-sample and stall-map decode, Tier B module-granularity attribution, Tier A duty-cycling and its serialization disclosure, the label set, tier selection, the overhead measurement, and the gate.

Out of scope: off-box symbolization as a *service* (the reader here is in-process and pure Go); ROCm (`gpu_pc_sample_batch_v1` is CUPTI-shaped by §6.3's first asymmetry and ROCm does not advertise `CapabilityPCSampling`); any change to how executions are measured or joined to launches.

---

## Findings that change the brief

Three things the brief assumed, which the code does not support. Report these; do not silently plan around them.

**1. `gpu_stall_reason_map_v1` is not frozen — it does not exist.** It is named in spec §6 and §6.3's probe table, but there is no struct in `shim/core/usdt_abi.h`, no mirror in `internal/gpuabi`, no `KIND_*` in `bpf/gpu_usdt.bpf.c`, and no `cookieFor` entry. It has never been emitted and never been decoded. Adding it is therefore *new* ABI, not implementation of frozen ABI — which is fine (a `_v1` probe that has never fired binds nobody) but it is a different kind of change and Task 2 treats it as one. `gpu_config_v1` is in the same state but half-way: the struct is in the header and `SizeConfig = 24` is in Go, but there is no `Config` type, no `DecodeConfig`, no KIND and no cookie.

**2. `gpu_pc_sample_batch_v1` cannot name a kernel, and the cubin is the missing table.** The record carries `cubin_crc`, `pc_offset`, `function_index` and `stall_index` — and no `kernel_id`. `gpu_kernel_name_v1` interns `kernel_id → name` where `kernel_id` is an FNV hash of the mangled name, which `function_index` (a per-module `uint32`) is not. So nothing on the wire maps `(cubin_crc, function_index)` to anything nameable. CUPTI's own `CUpti_PCSamplingPCData` carries `functionName` beside `functionIndex`; the ABI dropped it.

The consequence is that **Tier B's entire attribution chain runs through the cubin bytes**, not just its source lines. Without the cubin, a Tier B PC sample is `(crc, index, offset, stall)` and there is nothing to turn any of it into a kernel name — the profile would be honest and useless. This is the biggest risk in the plan and Task 3 is where it is paid down.

`functionIndex` is documented as "the function's unique symbol index in the module". If that is the cubin's `.symtab` index, the cubin resolves it and no ABI change is needed. **Whether it actually is cannot be determined without hardware.** Task 6 measures it; Task 2 pre-specifies the fallback so a negative result is a small change rather than a redesign:

> **Fallback, pre-approved:** if `functionIndex` is not the `.symtab` index, add `gpu_pc_sample_batch_v2` with `kernel_id u64` appended (48 bytes — the same size as launch and exec, so it fits `MAX_BATCHED_RECORD_BYTES` with no reservation change) and have the adapter hash `functionName` through the same `hash_name()` it already uses for launches and executions. `_v1` stays on the wire unchanged. This is the version-suffix mechanism working as designed (§16), not a failure.

**3. `bytes_ptr` is unreadable from the agent under the required capability set.** `gpu_module_load_v1.bytes_ptr` is a pointer into the *producer's* address space. Reading it needs `/proc/<pid>/mem` or `process_vm_readv`, both of which need `CAP_SYS_PTRACE` — which the constraints forbid. Reading it in BPF is not an alternative either: the ringbuf reservation is a compile-time 3,072-byte payload and cubins run from a few KB to hundreds of KB, so a cubin would need ~170 ordered, reassembled batches per module and would blow the uprobe's instruction budget. `bytes_ptr` is therefore **not a transport**; it is at most a debugging aid. Task 3 moves the bytes over a channel of their own instead. No ABI change — `gpu_module_load_v1` stays exactly as frozen and keeps announcing *that* a module loaded, with its CRC and size; only the bytes take a different road.

**4. `gpu_exec_v1` cannot say a kernel came from a CUDA graph, and that silently degrades Tier A.** A graph launch fires **one** runtime callback for the whole graph, so `gpu_launch_v1` gets one record where N kernels ran. The activity records still arrive per node, and `CUpti_ActivityKernel12` carries `graphId` and `graphNodeId` — but `gpu_exec_v1` is frozen at 48 bytes with no field for either. So N executions arrive sharing one correlation, the exact-correlation join becomes one-to-many, and in Tier A a PC sample's correlation names the *graph launch* rather than the kernel that stalled.

Nothing in the current pipeline detects this. It does not produce an error, a zero correlation, or a heuristic fallback — it produces confident, exact-looking attribution of many kernels to one call site. Graph launches are the norm in inference serving, so this is not an edge case. See "Conditions this phase does not handle" for how the plan refuses rather than guesses, and why the fix (`gpu_exec_v2` with `graph_id`/`graph_node_id`) is deliberately not taken here.

---

## Conditions this phase does not handle

Each of these is named, given a detection rule and a refusal, and left out of scope. A condition that is out of scope must still be *visible* — the failure this list exists to prevent is confident-looking attribution under a condition nobody checked for.

**CUDA graphs — the significant one.** Per finding 4, a graph launch produces one launch record for N executions, and neither the launch nor the exec record can say so. **Detection:** the adapter can see `CUpti_ActivityKernel12.graphId != 0` on an execution even though it cannot put it on the wire. **Refusal:** the adapter counts graph-launched executions in `g_exec_from_graph` and, on the first one, emits a `gpu_dropped_v1` record under a `classGraphExec` class; the agent surfaces this in `joinhealth` as a standing anomaly, and **Tier A refuses to start** in a process where graph executions have been observed, because Tier A's whole claim is exact launch attribution and a graph makes that claim false. Tier B is unaffected — its attribution runs through the module, not the launch, so a graph-launched kernel resolves to its own kernel exactly as any other does. This is the cleanest reason Tier B is the tier that ships toward always-on. The real fix is `gpu_exec_v2` with `graph_id` and `graph_node_id`; it is a whole phase's worth of join work (one launch to many executions is a new join shape, not a new field) and is deliberately not started here.

**Multiple GPUs.** PC sampling is enabled per `CUcontext`, and `gpu_pc_sample_batch_v1` carries no `device_id`. Two devices running the same binary produce the same `cubin_crc`, so their samples are **indistinguishable on the wire**. **Detection:** more than one distinct `device_id` across `gpu_exec_v1` records from one process. **Refusal:** the agent counts `MultiDeviceProcesses` and marks every PC-derived sample from such a process `gpu_pc_attrib="kernel-multidevice"`, which Task 9 renders and which no consumer can mistake for an exact answer. PC sampling is single-GPU in this phase.

**`cuptiFinalize`.** There is no handler for it anywhere in `shim/` — the adapter's only teardown path is the `atexit` handler at `cupti_adapter.cc:599`. Calling `cuptiFinalize` while PC sampling is enabled is undefined for us today. Task 6 registers a finalize handler that disables sampling per context before anything else runs, and counts `g_finalize_seen`. Untestable without hardware; listed so it is a known gap rather than an unknown one.

**The target exits mid-burst.** `cuptiPCSamplingStop` never runs and the burst's `gpu_sampling_window_v1` has no end. Encoded rather than lost: **`end_ns == 0` means "open when the producer stopped reporting"**, and every execution at or after that window's `start_ns` is `gpu_serialized="unknown"` — never `"false"`. The `atexit` handler closes the open window with the exit timestamp on the ordinary path, so `end_ns == 0` means a hard exit specifically. Task 10 tests both.

**A cubin unloads mid-burst.** Resolution is unaffected: we hold our own copy (Task 5), and `cubin_crc` is content-addressed, so a "reused" CRC is by definition the same bytes. But CUPTI requires a PC-data flush after every module load-unload-load in `CONTINUOUS` mode to keep PCs unique — so Task 6 drains on `MODULE_UNLOAD_STARTING`, not only on the timer. Missing that flush corrupts PC identity silently, which is the worst available failure mode.

**MPS, and two profilers on one device.** The PC sampling hardware is per-device and contention between processes is not something a process can observe from inside. Out of scope, undetectable from our side, and stated here so nobody reads a quiet profile as a correct one.

---

## Tasks

### Task 1: `internal/cubin` — read a cubin's line table, in pure Go

**Changes:** new package `internal/cubin`. Parses a cubin (an ELF) into: the `FUNC` symbols and their per-function `.text.<mangled>` sections; and a line table mapping a function-relative `pcOffset` to `(file, line)`. Exposes `Parse([]byte) (*Cubin, error)`, `(*Cubin).Functions() []Function`, `(*Cubin).Resolve(fn, pcOffset) (file string, line uint32, ok bool)`, and `(*Cubin).HasLineInfo() bool`.

**The mechanism, already proven during planning.** A cubin built `nvcc -arch=sm_86 -lineinfo -cubin` contains `.debug_line` in **standard DWARF v2** and no `.debug_info`. Go's `debug/dwarf` refuses to construct without `.debug_info`, but it will accept a 20-byte synthetic one:

```go
// abbrev: code 1, DW_TAG_compile_unit(0x11), no children,
//         DW_AT_stmt_list(0x10) DW_FORM_data4(0x06), terminators.
abbrev := []byte{0x01, 0x11, 0x00, 0x10, 0x06, 0x00, 0x00, 0x00}
// CU: version 2, abbrev_off 0, addr_size 8, abbrev code 1, stmt_list 0.
body := []byte{2, 0, 0, 0, 0, 0, 8, 0x01, 0, 0, 0, 0}
```

and then `dwarf.New(abbrev, nil, nil, info, lineSection, nil, nil, nil)` → `d.Reader().Next()` → `d.LineReader(cu)` walks the table. Verified during planning against a real sm_86 cubin: it yielded `pcOffset=0x0 line=1`, `0x10 line=2`, `0x40 line=3`, `0x60 line=4`, `0x90 line=6`, `0xa0 line=4`, `0xb0 line=6`, `0xe0 line=8`, end-sequence at `0x180`, with full source paths. The end-sequence address `0x180` equals the function symbol's size (384), which confirms the table's addresses are **function-relative**, which is what `pcOffset` is.

No cgo. No `nvdisasm`, no `cuobjdump`, no libcupti. The agent stays CUDA-free.

**`-lineinfo` detection** is the presence of a `.debug_line` section. Verified: the same source built without `-lineinfo` has `.debug_frame` and no `.debug_line`.

**Deliberately not done here:** SASS decoding. We never need to *disassemble* — the line table's addresses are what we look up against, so instruction decoding buys nothing this plan uses.

**Watch for:** `.rel.debug_line` exists and carries one relocation per function against the function's own symbol. Since we resolve *function-relative* offsets, the relocation is the identity for our purposes — but assert that reading, do not assume it: a cubin with several functions must be checked to see whether its `.debug_line` holds one sequence per function (each starting at 0) or one relocated sequence. **Fixture with two kernels is required.**

**Verification without a GPU or capabilities — complete.**
- Fixtures checked in under `internal/cubin/testdata/`, built by a documented `nvcc -cubin` line. The CUDA **toolkit** is needed to regenerate them; the **GPU is not**, and the tests read the committed bytes.
- `single_lineinfo.cubin`, `single_nolineinfo.cubin`, `two_kernels_lineinfo.cubin`, `unrolled_lineinfo.cubin`.
- Assert: exact `(pcOffset → line)` pairs for the single-kernel fixture; `HasLineInfo() == false` and every `Resolve` returning `ok == false` for the no-lineinfo one; both kernels resolving independently in the two-kernel one; and, for `unrolled_lineinfo.cubin`, that distinct PCs outnumber distinct lines by ≥ 4× — the collapse claim in "Label cardinality" made assertable rather than asserted.
- Fuzz `Parse` against truncated and byte-flipped fixtures: it must error, never panic and never allocate unboundedly.

**Must be measured on the RTX 3090 afterwards:** nothing. This task is complete offline.

---

### Task 2: ABI additions — the stall-reason map and the sampling window

**Changes:** two new probes. Neither mutates a frozen record.

```c
// index -> name for the device's stall reasons. One-shot, replayed on late
// attach. Fixed-size by the ABI's rules; name_len is authoritative.
#define GPU_STALL_NAME_MAX 128          // CUPTI_STALL_REASON_STRING_SIZE
struct gpu_stall_reason_map_v1 {
    uint32_t index;
    uint16_t name_len;
    uint8_t  truncated;
    uint8_t  _pad;
    char     name[GPU_STALL_NAME_MAX];
};                                       // 136 bytes

// One PC-sampling burst. Tier A only; this is how the profile knows which
// executions ran serialized. mode mirrors the tier.
struct gpu_sampling_window_v1 {
    uint64_t start_ns;
    uint64_t end_ns;
    uint8_t  mode;                       // 1 = continuous, 2 = kernel-serialized
    uint8_t  _pad[7];
};                                       // 24 bytes
```

Plus: `KIND_STALL_MAP = 7`, `KIND_SAMPLING_WINDOW = 8`, and **`KIND_MAX` 8 → 16**, which resizes the BPF `dropped` and `stacks_missing` arrays. `kindMax` in `gpuprobe/consumer.go` moves in the *same commit* — it mirrors `KIND_MAX` and a mismatch silently mis-sizes the drop accounting. `record_size()` and `max_records()` gain arms (136 → `BATCH_CAP` 22; 24 → capped at 64). `cookieFor` gains both names. `decodeBatch` gains both arms. `internal/gpuabi` gains `StallReason`/`SamplingWindow` types, `SizeStallReason`/`SizeSamplingWindow`, and decoders.

**While here**, finish `gpu_config_v1`: it has a struct and a `SizeConfig` constant but no Go type, decoder, KIND or cookie. Add `KIND_CONFIG = 9`, the decoder and the cookie so `sampling_factor`/`sm_count`/`clock_hz` stop being unreachable. Behaviour comes in Task 6.

**Watch for:** `MAX_RECORD_BYTES` is 272 and `PAYLOAD_BYTES` is 3,072, so 136 fits with room. The existing `_Static_assert(MAX_RECORD_BYTES <= PAYLOAD_BYTES)` still holds. Do **not** raise `MAX_BATCHED_RECORD_BYTES` — that would enlarge every launch batch's reservation to serve a probe that fires a handful of times per process.

**Verification without a GPU or capabilities — complete.**
- `make -C shim test` — the C++ `GPU_STATIC_ASSERT`s and the C11 `core/usdt_abi_test.c` both cover the new sizes and offsets.
- A Go test pinning `gpuabi.Size*` against the C sizes, in the shape the existing size tests use.
- A Go test pinning `kindMax` against the embedded BPF object's actual `dropped` map `max_entries`, so the two can never drift silently. **This test must exist before `KIND_MAX` is touched**, not after.
- Decoder round-trip tests including a `name_len > GPU_STALL_NAME_MAX` record, which must error rather than slice out of range.

**Must be measured on the RTX 3090 afterwards:** nothing structural. Task 6 confirms 38 stall reasons arrive on GA102.

---

### Task 3: Cubin transport — a channel of its own, never the enrollment socket

**This is the riskiest task and everything Tier B does sits behind it. Read finding 3 first, then read `gpuprobe/enroll.go` and `shim/core/enroll.h` end to end before writing anything.**

**The enrollment socket cannot carry this, and the reason is issue #49 again.** `enroll.h` states the protocol as "The producer sends nothing", and `handle()` implements exactly that: creds → admit → uid/pid checks → `procMapsHaveInode` → `reg.enroll` → one status byte. It **never reads**. A cubin offer must send a header, so a shared socket would force the agent to read in order to discriminate offer from enrollment — and on a genuine *enrollment* connection that read blocks until the producer's 2 s budget expires and it closes. **Every rendezvous becomes a 2 s stall ending in `kEnrollError`**, which is precisely the failure that took three live runs and two wrong diagnoses to find. Three further hazards on the same socket:

- `serve()` is serial **at `Accept`**, not merely at `handle`. Short-lived offer connections are accepted by the same loop, so a queue of offers — or one 2 MB stream — is served FIFO *ahead of* a genuine producer whose `connect()` already landed in the backlog. The dangerous direction is offers queued **ahead** of enrollment, not behind it.
- Admission is charged per connection against `enrollUIDBurst = 32`, refilling 32/s. A JIT- or template-heavy workload loading more than 32 modules/s — the exact case this plan's own bounding section names — drains the bucket and then **refuses genuine enrollments from that uid**, silently restoring the ~38% stack loss issue #49 measured, with only `UnwindEnrollThrottled` moving.
- It would put a `connect()` and an up-to-2 MB write on the application's `cuModuleLoad` path, contradicting this plan's own rule that the application is never stalled for a profiler.

**So: a second channel, structurally separate.** Roughly 30 lines, reusing the parts that already work:

- a second abstract address, `@perfagent-gpu-cubin.v1.<maj>.<min>.<ino>`, derived from the shim's dev:inode by the **same** `enrollAddressFor` shape at `enroll.go:246` — so it inherits the btrfs `stat`-vs-`maps` fix rather than reintroducing that bug;
- its **own** listener, its **own** goroutine, and its **own** `enrollAdmission` bucket, so cubin traffic cannot spend an enrollment token or an enrollment `Accept` slot. This is what makes every hazard above **structurally impossible rather than test-defended**;
- `enrollPeerCreds` and `procMapsHaveInode` reused **verbatim** — a cubin offer is authenticated exactly as an enrollment is, and a peer that does not map the shim inode is refused.

**Payload: a sealed `memfd`, passed by `SCM_RIGHTS`.** The agent `mmap`s it and never copies through the socket, so the shim never blocks on the agent's read rate. **The seals are load-bearing and must be named, applied and verified:**

```
F_SEAL_SEAL | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_WRITE
```

- without `F_SEAL_SHRINK`, a peer can `ftruncate` under the agent's `mmap` and **SIGBUS the agent**;
- without `F_SEAL_WRITE`, the ELF mutates under the parser mid-parse;
- without `F_SEAL_GROW`/`F_SEAL_SEAL`, neither of the above stays true.

The agent **verifies with `fcntl(F_GET_SEALS)` before mapping**, and a missing seal is a **counted rejection**. There is no fallback that reads it anyway — falling back is how a defended path becomes an undefended one.

**Bounds and counters, all assertable:**

| counter | side | meaning |
| --- | --- | --- |
| `CubinsOffered` | shim | modules the shim tried to hand over |
| `CubinsSendFailed` | shim | refused, timed out, or short-written |
| `CubinsReceived` | agent | accepted and stored |
| `CubinsRejectedTooLarge` | agent | over the per-cubin ceiling |
| `CubinsRejectedMalformed` | agent | bad header, bad magic, size mismatch |
| `CubinsRejectedUnsealed` | agent | `F_GET_SEALS` missing a required seal |
| `CubinsRejectedUnauthorized` | agent | peer does not map the shim inode |
| `CubinsThrottled` | agent | refused by the **cubin** admission bucket |
| `CubinBytesReceived` | agent | total, so a ceiling can be reasoned about |

`CubinsThrottled` is the counter for review finding 4's first hole. It exists because the buckets are separate: a throttled cubin offer costs a module's source resolution and **cannot** cost an enrollment, and the test below asserts that rather than assuming it.

Per-cubin and total-bytes ceilings, both configurable, both counted when they bite. An oversized cubin is rejected **whole**, never truncated — a truncated cubin parses into a *wrong* line table, the one failure worse than no line table.

**Verification without a GPU or capabilities — complete.** `gpuprobe/enroll_test.go` already drives a fake producer over an abstract socket with no GPU and no privileges; the new listener gets the same treatment.
- a 5 KB and a 2 MB cubin arrive byte-identical (compare against the fixture and its CRC);
- an offer 1 byte over the ceiling is rejected, `CubinsRejectedTooLarge == 1`, nothing stored;
- a declared size disagreeing with the payload is rejected and counted; no partial cubin is stored;
- a memfd missing **each** required seal in turn is rejected, `CubinsRejectedUnsealed` exact, and the agent never maps it;
- a peer not mapping the shim inode is refused and counted;
- an offer for an already-stored CRC is a counted no-op, not a re-parse;
- **the isolation test, which is the point of this task:** flood the cubin listener with offers until `CubinsThrottled` is non-zero, then perform a normal enrollment and assert it succeeds with `UnwindEnrollThrottled` unchanged. Run it in **both** orders — offers queued *ahead of* enrollment as well as behind it — because the ahead direction is the one a shared socket fails and the behind direction is the easy one;
- an enrollment on the enrollment socket still completes without any read on that connection (a regression test that nobody adds a discriminating read later).

**Must be measured on the RTX 3090 afterwards:** that a real `MODULE_LOADED` cubin survives byte-identical, and that `cuptiGetCubinCrc()` over the received bytes equals the `cubinCrc` the PC records carry.

---

### Task 4: `gpu.ModuleStore` — bounded, CRC-keyed, line-table-backed

**Changes:** a store mapping `cubin_crc → (bytes, *cubin.Cubin, functionIndex→name table)`, bounded and LRU, living beside `Timeline`. `EmitModule` already exists and pushes `GPUModule` into a bounded ring; that ring records *that* a module loaded. This store holds what a module *is*.

API: `Put(crc uint64, bytes []byte) error`, `Resolve(crc uint64, functionIndex uint32, pcOffset uint64) Resolution` where `Resolution` carries `{Status, Function, File, Line}` and `Status` is exactly the four-valued `gpu_src_status` enum from the representation section. **The store is the single place that enum is decided**, so no caller can invent a fifth answer or a silent default.

Counters: `ModulesStored`, `ModulesEvicted`, `ModulesWithoutLineInfo`, `ModulesUnparseable`, `ResolveResolved`, `ResolveNoModule`, `ResolveNoLineInfo`, `ResolveUnmapped`. The four `Resolve*` counters must sum to every `Resolve` call — that identity is a test.

**Watch for:** `ModulesUnparseable` is not the same as `ModulesWithoutLineInfo`. A cubin that fails `cubin.Parse` outright (corrupt, truncated, a format we do not know) resolves as `no-module`, because we hold bytes we cannot use — which for the reader is the same actionable fact as holding nothing. Keeping the counters separate is what makes a transport bug distinguishable from a build-flag choice.

**Verification without a GPU or capabilities — complete.** Table-driven tests over the Task 1 fixtures: all four statuses reachable, the sum identity, LRU eviction under a small bound with `ModulesEvicted` exact, and a resolve after eviction returning `no-module` rather than a stale answer.

**Must be measured on the RTX 3090 afterwards:** nothing.

---

### Task 5: The adapter captures cubins

**Changes:** `shim/nvidia/cupti_adapter.cc` reads the RESOURCE callbacks it currently only histograms.

On `CUPTI_CBID_RESOURCE_MODULE_LOADED`: **copy the bytes immediately, inside the callback.** §6.3 finding 2 measured the hazard precisely — after `cuModuleUnload` the buffer is still mapped and readable but its *contents have changed*, so a late reader gets silently wrong bytes rather than a fault, and CUDA's lazy module loading puts loads and unloads at arbitrary points in a long-running process. There is no safe definition of "copy later". Then `cuptiGetCubinCrc()` over the copy, and fire `gpu_module_load_v1` with `bytes_ptr` pointing at the adapter-owned copy (it stays in the ABI, and it stays accurate; it is simply not how the agent gets the bytes).

**The copy happens in the callback. The offer does not.** A `connect()` plus an up-to-2 MB handover on the application's `cuModuleLoad` path would stall the application for the profiler's benefit, which this plan forbids. The copy is enqueued to a bounded queue and sent from the **drain thread** that Task 6 already runs. Enqueue is a `memcpy` and a mutexed push; a full queue drops the *offer* (not the copy) and counts `g_cubin_queue_full`, so a module simply goes unresolvable rather than the application going slow. The two halves have different deadlines for different reasons — the copy is deadline-bound by CUPTI's buffer lifetime, the send is deadline-bound by nothing at all — and collapsing them into one call site is how the application ends up paying.

**Structural assertion, not a comment:** no deferral may sit between the callback entry and the `memcpy`. Enforce it by having the capture function take CUPTI's buffer as a non-owning `span`-like view whose only consumer is the copy, so the *pointer* cannot escape into the queue and deferring the copy does not compile. What is enqueued is the owned copy, which is safe to hold indefinitely.

Also: intern the module in a small adapter-side set so a re-load of the same CRC does not re-offer, and count `g_module_reload_skipped`. Counters: `g_modules_captured`, `g_module_reload_skipped`, `g_cubin_queue_full`, `g_cubin_send_failed`.

**Verification without a GPU or capabilities — partial.** The **stub** (`shim/stub/stub.cc`) gains a fake module-load path that reads a checked-in cubin fixture from disk, offers it over the Task 3 cubin channel and fires `gpu_module_load_v1`. That drives Tasks 3, 4, 7 and 9 end to end with no GPU, and is what the Task 13 gate uses. The adapter itself only gets a compile check and the escape-analysis assertion.

**Must be measured on the RTX 3090 afterwards:**
- that `MODULE_LOADED` fires at all for the test workload, and how many times (lazy loading means it may be later than expected);
- that a `cuModuleUnload` after capture leaves our copy's re-computed CRC unchanged — the direct test of finding 2;
- that `cuptiGetCubinCrc()` over the copy matches the PC records' `cubinCrc`.

---

### Task 6: Tier B — `CONTINUOUS` PC sampling in the adapter

**Changes:** on `CUPTI_CBID_RESOURCE_CONTEXT_CREATED`, configure and enable PC sampling for that context: collection mode `CONTINUOUS`, sampling period from config, all stall reasons. Query `cuptiPCSamplingGetNumStallReasons` / `GetStallReasons` once and emit `gpu_stall_reason_map_v1` (replayed on late attach through the existing `ReplayLog`, exactly as `gpu_kernel_name_v1` already is). Emit `gpu_config_v1` with the sampling period and SM count. On the existing 100 ms drain tick, call `cuptiPCSamplingGetData` and emit `gpu_pc_sample_batch_v1` — **one record per `(PC, stall reason)` pair**, which §6.3 names as the price of the fixed-size record rule.

**Honesty plumbing, which is the substance of this task.** `CUpti_PCSamplingData` reports `droppedSamples` (hardware backpressure) and `hardwareBufferFull`. Both feed `gpu_dropped_v1` under their own classes. CUPTI additionally documents two structural omissions that no counter can recover and that the profile must state rather than imply: it provides no PC records for non-user kernels, and none for instructions whose selected stall-reason counts are all zero. `nonUsrKernelsTotalSamples` is reported and must be surfaced, because it is the size of the first omission.

`nonUsrKernelsTotalSamples` is the size of the first omission and it needs a wire record, which review finding 4 correctly noted it did not have. It gets one **without an ABI change**: it is loss, so it rides `gpu_dropped_v1` under its own class, alongside `droppedSamples` and `hardwareBufferFull` under theirs. Add `classPCDroppedHW`, `classPCBufferFull`, `classPCNonUserKernel` and `classGraphExec` (the last from the out-of-scope section) to the drop-class enum, which the frozen `{count u64, klass u8}` record already accommodates.

`totalSamples` is **not** loss and has no wire field, so the identity "`totalSamples` equals the sum of per-record counts plus the losses above" is checkable only in the shim's log, not by the agent. Stated as a limitation rather than claimed as a check: closing it would need `gpu_config_v2`, which is not worth a version bump for a diagnostic.

**Drain on module unload, not only on the timer.** `cupti_pcsampling.h` requires a PC-data flush after every module load-unload-load in `CONTINUOUS` mode to keep PCs unique. So `CUPTI_CBID_RESOURCE_MODULE_UNLOAD_STARTING` calls `cuptiPCSamplingGetData` before returning. Missing this corrupts PC identity **silently**, which is the worst failure mode available here.

**A `cuptiFinalize` handler**, per the out-of-scope section: disable PC sampling on every tracked context before anything else, and count `g_finalize_seen`. There is no such handler in `shim/` today.

**The multi-GPU guard.** Track distinct `device_id`s seen on `gpu_exec_v1`. The agent counts `MultiDeviceProcesses` and Task 9 marks the affected samples; the shim additionally logs it, because a two-GPU process is a configuration mistake for this phase rather than a runtime condition to tolerate.

**Per-context, not per-process.** Enable/disable is per `CUcontext`. Track them, disable each on `CONTEXT_DESTROY_STARTING`, and count contexts seen, enabled and failed-to-enable — a context that fails to enable is a silent hole in coverage otherwise.

**Verification without a GPU or capabilities — partial.** The stub emits synthetic PC batches, a stall map and a config record, which proves Task 7's decode and Task 9's labels. The drain-tick scheduling logic is testable in `shim/core` against a fake clock.

**Must be measured on the RTX 3090 afterwards — most of this task:**
- that 38 stall reasons arrive on GA102 and their names;
- the actual PC-record rate for the test workload (the spike saw 352 records in `CONTINUOUS` for ~103k samples; that ratio drives every buffer size here);
- whether `functionIndex` is the cubin `.symtab` index — **the finding-2 question**, and the trigger for `gpu_pc_sample_batch_v2`;
- whether `pcOffset` is function-relative in the same sense the line table is;
- `droppedSamples` / `hardwareBufferFull` behaviour under a saturating workload;
- that the `MODULE_UNLOAD_STARTING` drain actually preserves PC uniqueness across a load-unload-load cycle;
- that the `cuptiFinalize` handler runs at all, and that disabling per context inside it does not error;
- Tier B overhead (Task 12).

---

### Task 7: The consumer decodes PC samples, modules and the stall map

**Changes:** `gpuprobe/consumer.go` grows `applyBatch` arms for `kindPC`, `kindModule`, `kindStallMap`, `kindSamplingWindow` and `kindConfig`. These kinds currently fall into `default:` and are counted as `Stats.Undecoded`; that counter must go to zero for them, and the test asserts it does.

`stall_index → name` resolves from a bounded table fed by the stall map, mirroring `kernelnames.go` exactly — including its pending-event handling, because a PC batch can arrive before the map does. Reuse `pendingNames`' shape; do not invent a second one. `GPUPCSample.StallReason` is a string, so an unresolved index becomes `""` and increments `Stats.StallNamesMissing` rather than being rendered as `"stall#17"`, which would leak an unstable internal index into a label value.

`GPUPCSample.Module` gets `{Backend, CRC}`. `Correlation` gets the PID from the batch header in both tiers and the value only when non-zero — which is already what `correlationOf` does, and already what `Stats.ZeroCorrelation` counts. In Tier B **every** PC record has a zero correlation, so that counter stops being an anomaly signal for this population; add `Stats.PCSamplesWithoutCorrelation` separately so Tier A can be checked for the *opposite* condition (any zero there is a contract violation, exactly as `ZeroCorrelationExecs` is for executions).

**Verification without a GPU or capabilities — complete.** Stub-driven, in the existing `gpuprobe` unit tests plus the gate. Assert: `Undecoded == 0` for these kinds; a PC batch preceding its stall map still resolves once the map arrives; a stall index with no map entry yields `""` and a non-zero `StallNamesMissing`; sequence-gap accounting works on the new kinds.

**Must be measured on the RTX 3090 afterwards:** nothing.

---

### Task 8a: `GPUPCSample.FunctionIndex` and the second pending index

**The key does not exist yet.** `gpu/types.go` gives `GPUPCSample` a `Module ModuleRef` and a `PCOffset`, and **no `FunctionIndex`** — so the Tier B key `{PID, CubinCRC, FunctionIndex}` cannot be built against today's types, and neither Task 7 nor the old Task 8 added the field. Add it: `FunctionIndex uint32` on `GPUPCSample`, populated by the consumer from the wire record (which has carried it since the ABI froze). Check it against `gpu/types_test.go:44` — that test's forbidden-name list guards the lean shared `GPUSample`, not the capability-gated `GPUPCSample`, so this is additive and does not trip it.

**The problem the index solves.** `Timeline.pending` keys on the whole `CorrelationID`, and `Snapshot` attaches samples only via `t.pending[exec.Correlation]`. A Tier B sample has `Correlation.Value == ""` and `Correlation.PID` set, so **every Tier B sample from one process collapses onto the single key `{Backend, PID, ""}`**. `pendingSampleCap` (4,096) then bounds an entire process's PC samples to one entry and evicts the rest into `EvictedPendingSamples`, and `Snapshot` matches none of them because no execution carries an empty correlation value. This is exactly the pathology spec §6.1 describes for correlation-less launches.

**The change.** A second pending index, used only for samples with no correlation value, keyed on `{PID, CubinCRC, FunctionIndex}`. Its own cardinality cap, its own horizon (reusing `pendingHorizonNs`), and its own eviction counter `EvictedPendingModuleSamples`, kept separate from `EvictedPendingSamples` so a Tier B eviction storm is distinguishable from a Tier A one. `orderedFIFO` already exists and already handles the generation/liveness problem correctly — reuse it rather than writing a third eviction walk. Cross-process is refused structurally, because the PID is in the key rather than in a check that can be forgotten (issue #52's discipline).

**Verification without a GPU or capabilities — complete.** The anti-collapse regression test is the deliverable: drive 10,000 Tier B samples across 50 distinct `(crc, functionIndex)` pairs and assert none are lost to `EvictedPendingSamples`. **It must fail against today's code**; a test that passes before the fix is testing nothing. Plus: horizon eviction counts into `EvictedPendingModuleSamples` and not into `EvictedPendingSamples`; a sample whose PID differs never lands in another process's group.

**Must be measured on the RTX 3090 afterwards:** nothing.

---

### Task 8b: The `Snapshot` join, and `gpu_pc_attrib`

**Changes:** at `Snapshot`, for each execution: try the exact-correlation index first (unchanged — Tier A keeps taking it); on a miss, resolve the execution's kernel to a `{PID, CubinCRC, FunctionIndex}` through `ModuleStore` and take that group's samples.

The kernel identity comes from the **module**, never from guessing on the kernel-name string. `ModuleStore.Resolve` gives `functionIndex → function name`; the execution carries `KernelName` from `gpu_kernel_name_v1`. Matching those two names is what links a PC group to an execution, and where they do not match the samples **stay pending** rather than being attached to a plausible neighbour.

**`gpu_pc_attrib` is a new label with its own values, and it must not reuse `ExecutionView.Ambiguous`.** That flag is set in exactly one place — the heuristic *launch*-join branch at `gpu/timeline.go:661` — and is counted as `AmbiguousHeuristicMatchCount`. Reusing it for PC ambiguity would emit `gpu_join="exact" gpu_ambiguous="true"` on the same sample: two unrelated facts on one boolean, and a counter that no longer means what its name says. That is precisely the ambiguity this plan forbids elsewhere. So PC-attribution quality lives entirely in its own label:

| `gpu_pc_attrib` | means |
| --- | --- |
| `exact` | the sample carried a correlation and joined through it (Tier A) |
| `kernel` | joined through the module, and exactly one execution of that kernel was in the horizon |
| `kernel-ambiguous` | joined through the module, and **more than one** execution of that kernel was in the horizon — an inference, marked |
| `kernel-multidevice` | joined through the module in a process that used more than one device, where `cubin_crc` cannot distinguish them (out-of-scope section) |

`ExecutionView.Ambiguous` and `gpu_ambiguous` keep their current meaning untouched, and §10's requirement — *"Where that window holds more than one execution of the same kernel, the attribution is Ambiguous and is marked, not guessed away"* — is satisfied by `kernel-ambiguous` carrying it explicitly.

**Reconciliation identity, extended:** every PC sample ingested lands in exactly one of attributed-exact, attributed-kernel (any `kernel*` value), still-pending, or evicted. `TestConformancePCSampleReconciliationCoversPendingAndEvicted` grows the new buckets.

**Verification without a GPU or capabilities — complete.** A Tier B sample reaches its execution; two executions of one kernel in the horizon yield `kernel-ambiguous` **and leave `gpu_ambiguous` unset**, which is the assertion that pins the de-overloading; a sample whose module is unknown stays pending, is evicted, and is counted; a multi-device process yields `kernel-multidevice`.

**Must be measured on the RTX 3090 afterwards:** what fraction of real PC samples attribute versus stay pending, and whether the eviction horizon is long enough given the drain period.

---

### Task 9: Projection — the label set

**Changes:** `gpu/projection.go`. `gpu_pc` and `gpu_stall` are already emitted; keep them exactly as they are. Add, on every PC-derived sample:

- `gpu_src_status` — unconditional, four values, from `ModuleStore.Resolution.Status`;
- `gpu_src_file`, `gpu_src_line`, `gpu_src_func` — only when `resolved`;
- `gpu_pc_attrib` — unconditional, one of `exact` / `kernel` / `kernel-ambiguous` / `kernel-multidevice`, from Task 8b;
- `gpu_serialized` — unconditional on **every** execution (not only PC-bearing ones), `"true"` / `"false"` / `"unknown"` from Task 10's windows.

**`gpu_serialized` reaches only the GPU samples, and the plan says so rather than implying otherwise.** `ProjectExecutions` is the GPU projection; the on-CPU and off-CPU profilers are a wholly separate path with no window awareness, so **CPU and off-CPU samples taken during a Tier A burst are unlabelled and distorted.** The distortion is not hypothetical and not small: serialization inflates precisely the synchronization wait that off-CPU profiling exists to measure, so a Tier A run makes `cudaDeviceSynchronize`-shaped off-CPU time look worse than it is, with nothing in that profile saying why. Carrying windows into the CPU path is a cross-cutting change to a profiler that currently knows nothing about GPUs, and it is **out of scope for this phase**. What is in scope is refusing to let it be a surprise: Task 11's standing operator warning names it explicitly, and `joinhealth` repeats it whenever any window was recorded. A limitation an operator is told about is a limitation; one they discover from a misleading profile is a defect.

All of them are set **after** the `Tags` copy, so a producer-supplied tag named `gpu_src_file` cannot forge one — the reserved-name discipline the file already documents at length.

`gpu_src_file` carries the **basename**; the directory goes in nothing at all. Full build-host paths vary per build, blow up the string table for no reader benefit, and leak build-environment layout into a profile that may be shared. The basename plus `gpu_src_func` is enough to locate a line in the repository it came from.

**The `gpu_pc` cap.** Track distinct `gpu_pc` values within one `ProjectExecutions` call. Past a configurable ceiling, stop emitting `gpu_pc` and count every suppression in `ProjectionPCLabelsSuppressed`. `gpu_stall` and `gpu_src_*` are unaffected — they are coarser and more actionable, so the label dropped under pressure is the numerous one, not the useful one. Suppression must be visible in `joinhealth` output, because a profile that silently lost its PC labels looks identical to one that never had them.

**Frames do not change.** `projectionFrames` gains nothing.

**Verification without a GPU or capabilities — complete.** `gpu/projection_test.go`. Assert every label above including all four `gpu_src_status` values, all four `gpu_pc_attrib` values and all three `gpu_serialized` values; that `kernel-ambiguous` never coincides with `gpu_ambiguous="true"`; that a `Tags` entry named `gpu_src_file` loses to the derived value; that the cap suppresses `gpu_pc` and only `gpu_pc`, with an exact `ProjectionPCLabelsSuppressed`; and — negatively — that no frame name contains `gpu:pc`, `gpu:src`, or any stall-reason string.

**Must be measured on the RTX 3090 afterwards:** the real distinct-value counts per label on a genuine profile, to confirm the cardinality table above and to set the `gpu_pc` ceiling to a number rather than a guess.

---

### Task 10: Tier A — `KERNEL_SERIALIZED`, duty-cycled, and its disclosure

**Changes:** the adapter configures `CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_COLLECTION_MODE = KERNEL_SERIALIZED` and `ENABLE_START_STOP_CONTROL = 1`, then duty-cycles with `cuptiPCSamplingStart()` / `cuptiPCSamplingStop()`. **Start/Stop, not Enable/Disable** — the latter tears down and rebuilds the configuration each burst and is not what the start/stop control exists for. CUPTI additionally requires a flush after every range end in this configuration, which the existing drain timer is the natural home for.

A burst controller in `shim/core/`: burst length (default ~50 ms), and a gap tuned by a closed loop to hold a target PC/stall pairs per second (Parca's default target is ~100/s). It must be a **pure function** of `(target rate, observed rate, elapsed)` so it can be unit-tested against a fake clock with no GPU.

**Disclosure — the honesty obligation for this tier.** Every burst emits `gpu_sampling_window_v1{start_ns, end_ns, mode}`. Every kernel that executed while a burst was open ran serialized, sampled or not, so the window — not the set of sampled kernels — is the honest unit. In the Timeline, an execution whose `[StartNs, EndNs]` intersects any window is marked serialized.

Three values, and the third is the one that matters:

- `gpu_serialized="true"` — this execution overlapped a burst; **its duration is perturbed by the measurement**;
- `gpu_serialized="false"` — Tier A was running and no window overlapped this execution;
- `gpu_serialized="unknown"` — Tier A was selected but no window records arrived (dropped batch, late attach, sequence gap). **It must never degrade to `"false"`.** A profile that says "not perturbed" when it means "cannot tell" is precisely the failure §4 forbids, and it is the exact shape of the `gpu_join` precedent.

In Tier B and with sampling off, `gpu_serialized="false"` is correct and unconditional — nothing was ever serialized.

**A window with `end_ns == 0` is open, not zero-length.** If the target exits mid-burst, `cuptiPCSamplingStop` never runs and the burst never closes. The `atexit` handler stops sampling and closes the open window with the exit timestamp on the ordinary path; `end_ns == 0` therefore means a hard exit specifically, and every execution at or after that window's `start_ns` is `"unknown"`. Treating an unterminated window as zero-length would mark a whole perturbed tail `"false"`, which is the one answer that must never be reachable by accident.

**Tier A refuses to start where CUDA graphs have been observed**, per the out-of-scope section: Tier A's entire claim is exact launch attribution, and a graph launch makes that claim false while still looking exact. The refusal is loud and counted, not a silent downgrade to Tier B.

**Counters:** `SamplingBursts`, `SamplingBurstNs` (total), `SamplingWindowsReceived`, `ExecutionsSerialized`, `ExecutionsNotSerialized`, `ExecutionsSerializationUnknown`. The last three must sum to the executions in the snapshot — the same sum-identity discipline `gpu_join`'s three outcomes already carry.

**Verification without a GPU or capabilities — partial.** The burst controller is a pure unit test against a fake clock: assert it converges to the target rate, that it never exceeds a maximum duty fraction whatever the observed rate does, and that a zero observed rate does not drive the gap to zero. The window→execution intersection and all three `gpu_serialized` values are ordinary `gpu` package tests driven by the stub, including: windows *missing* entirely → `"unknown"`; a window with `end_ns == 0` → every execution from `start_ns` onward `"unknown"` and **never** `"false"`; and a stub reporting a graph execution → Tier A refuses to start.

**Must be measured on the RTX 3090 afterwards:**
- that `correlationId` is non-zero on ≥99% of PC records in this mode (the spike says 1,828/1,828);
- that windows actually bracket the executions that ran in them;
- whether the collection mode can be changed between `Stop` and `Start` without a full `Disable`/`Enable` — **undocumented, and it decides whether a future tier-switch is possible at all**;
- the overhead (Task 12), which decides whether this tier ships.

---

### Task 11: Tier selection

**Changes:** one setting, three values: `off` (default), `continuous`, `serialized`. Surfaced as `PERFAGENT_GPU_PC_SAMPLING` to the shim and as a CLI flag on `cmd/gpu-cuda-profile`.

**They cannot run simultaneously, and the plan should say why rather than just forbidding it.** `COLLECTION_MODE` is a single per-`CUcontext` CUPTI attribute. A process could in principle set different modes on different contexts, but which context a given kernel lands on is the application's choice, not the profiler's — so a "both" mode would produce a profile whose attribution quality varied by an axis the operator cannot see or control. That is worse than either tier alone. Selection is therefore process-wide and exclusive: an attempt to set both is a **startup error**, logged loudly, never a silent pick.

Switching tiers mid-run is out of scope for this phase and is gated on the Task 10 hardware question about `Stop`/`Start`.

Because Tier A perturbs the workload, `serialized` must also be refused unless explicitly acknowledged — the same shape as any destructive flag. Ship it as `serialized` requiring the operator to have set it deliberately, and have `joinhealth` print a standing warning for the whole run rather than once at startup. **The warning must name all three perturbations**, because an operator who is told only about the first will misread the other two: (1) GPU kernel durations inside a burst are inflated by serialization and are marked `gpu_serialized="true"`; (2) **CPU and off-CPU samples during a burst are distorted and carry no marking at all** — off-CPU synchronization waits especially (Task 9); and (3) Tier A is unavailable where CUDA graphs are in use.

**Verification without a GPU or capabilities — complete.** Config validation unit tests: the three values, the both-set error, the unknown-value error, and that `off` results in no PC-sampling calls at all (assert via the stub that no PC probe ever fires).

**Must be measured on the RTX 3090 afterwards:** nothing beyond what Tasks 6 and 10 already cover.

---

### Task 12: Overhead — what to measure, against what, and what sinks Tier A

**Not a code task in the usual sense**; it produces a `bench/` scenario and a number that decides whether Tier A ships.

**The baseline is the shipping Phase 4 configuration**, not "no injection". §9.1 already measured injection at 0.0% and the activity path at −10.0% at a 393k launches/s ceiling; those costs are paid whether or not PC sampling is on. The question here is strictly the **marginal** cost of PC sampling, so the baseline is: shim injected, RUNTIME + RESOURCE callbacks, `CONCURRENT_KERNEL` activity, drain timer at 100 ms, **PC sampling disabled**.

**Workload:** a realistic one, per the Phase 6 gate's own wording — *"measured overhead on a real workload, not a microbenchmark."* The saturating 393k launches/s ceiling is the wrong instrument here: it exaggerates per-launch costs and *understates* serialization costs, because serialization hurts in proportion to the concurrency it destroys and a stream of trivial kernels has little. Use a workload with genuine kernel concurrency across several streams and non-trivial kernel durations.

**Method:** §9.1's, unchanged — five interleaved runs per mode, medians, fixed work rather than fixed time. Report wall-clock for the fixed work; also report achieved kernel throughput.

**Arms:**

| arm | expectation |
| --- | --- |
| baseline (PC sampling off) | reference |
| Tier B continuous | small; it does not serialize |
| Tier A, 50 ms burst / 450 ms gap (10% duty) | the headline number |
| Tier A, 50 ms / 950 ms (5%) | |
| Tier A, 50 ms / 1950 ms (2.5%) | |

**The number that matters is cost ÷ duty fraction, not cost alone.** Serialization's damage does not stop when the burst does — concurrency has to refill afterwards — so if the ratio is near 1, duty-cycling works and the tier is tunable to any budget. If the ratio is much greater than 1, duty-cycling is not buying what it appears to and a smaller duty will not rescue it.

**Pre-committed thresholds, so the result is a decision and not a discussion:**

- **Tier B > 5% wall-clock on a realistic workload → Tier B does not ship as always-on**; it becomes an explicitly-enabled mode like Tier A.
- **Tier A at 10% duty ≤ 5% wall-clock, and cost ÷ duty ≤ 2 → ships as an opt-in tier**, as planned.
- **cost ÷ duty > 2 at every duty tested → Tier A ships only as a deliberate deep-dive mode**, with the operator warning of Task 11 and no suggestion that it is suitable for continuous use.
- **Tier A at 2.5% duty > 5% wall-clock → Tier A is unshippable in this phase.** Serialization would then be costing more than the sampling window can explain, and duty-cycling has no remaining lever.

Record the numbers in this file when they exist. A threshold decided after seeing the data is not a threshold.

**Verification without a GPU or capabilities — partial.** The scenario harness builds and reports `BENCH_SKIPPED` without caps or GPU, in the shape `bench/cmd/scenario/main.go` already uses. That is all that can be proven offline.

**Must be measured on the RTX 3090 afterwards:** every number above. **This task cannot be completed without hardware, and no part of the tier decision may be made without it.**

---

### Task 13: The phase gate

**GPU-free half** — extends `TestStubDrivesThePipelineToPprofWithoutAGPU`. The stub emits module loads carrying real checked-in cubins, a stall map, PC batches, launches and executions. Assertions:

1. **Frames stop at the kernel.** No frame name in the profile contains `gpu:pc`, `gpu:src`, or any stall-reason string. Frames are exactly the CPU stack, `[gpu:launch]` or `[gpu:launch unsampled]`, and `[gpu:kernel:<name>]`. *This is the §8 pin and it is the first assertion for a reason.*
2. **A source line is reached from a CPU stack.** With the `-lineinfo` fixture, at least one sample carries a real CPU stack in its frames **and** `gpu_src_status="resolved"` with a `gpu_src_file`/`gpu_src_line` naming a real line of the fixture's source. This is the Phase 6 gate's own wording, satisfied through labels.
3. **No `-lineinfo`, no invention.** With the no-lineinfo fixture, every PC sample carries `gpu_src_status="no-lineinfo"` and **no** `gpu_src_file`/`gpu_src_line`/`gpu_src_func`. The kernel frame is unchanged, so the two populations still aggregate together at the kernel level.
4. **All four `gpu_src_status` values are reachable** from the stub, and each is reached by the fixture that should produce it.
5. **Reconciliation.** Every PC record the stub emitted lands in exactly one of attributed-exact, attributed-kernel, pending, or evicted, and the counters sum to what was emitted.
6. **Tier B does not collapse.** 10,000 Tier B samples across 50 distinct `(crc, functionIndex)` pairs attribute without loss to `EvictedPendingSamples`.
7. **Ambiguity is marked, in its own label.** Two executions of one kernel inside the horizon with one Tier B batch produce `gpu_pc_attrib="kernel-ambiguous"` — **and leave `gpu_ambiguous` unset**, since that flag means a heuristic *launch* join and nothing else.
8. **Tier A disclosure.** With windows: executions inside a window carry `gpu_serialized="true"`, outside `"false"`. Without windows but with Tier A selected: `"unknown"` on every execution and **never** `"false"`.
9. **Cardinality cap.** More distinct PCs than the ceiling suppresses `gpu_pc` and nothing else, with an exact `ProjectionPCLabelsSuppressed`.
10. **The cubin channel cannot touch enrollment.** Task 3's isolation test runs as part of the gate, in **both** orders: offers flooded *ahead of* an enrollment as well as behind it. `CubinsThrottled` non-zero while `UnwindEnrollThrottled` is unchanged, and the enrollment still succeeds. The enrollment socket is asserted to perform no read on the producer's connection.
10a. **Seals are enforced.** A memfd missing any required seal is rejected, counted in `CubinsRejectedUnsealed`, and never mapped.
10b. **Out-of-scope conditions refuse rather than guess.** A stub reporting a graph execution makes Tier A refuse to start; a stub reporting two device ids yields `gpu_pc_attrib="kernel-multidevice"`; a window with `end_ns == 0` yields `gpu_serialized="unknown"` on everything after it and `"false"` on nothing.
11. **`getcap` on the gate binary shows no `cap_sys_admin`** — the standing Phase 1 assertion.
12. `Stats.Undecoded` is zero for every kind this phase decodes.

**Hardware half** (RTX 3090, `shim/nvidia/testdata/cuda_workload`, already built `-lineinfo -g`):

13. `cuptiGetCubinCrc()` over the received copy equals the PC records' `cubinCrc`.
14. Tier B: a flame graph reaching a real line of `cuda_workload.cu` from a CPU stack, through labels — the Phase 6 exit condition.
15. Tier A: `correlationId` non-zero on ≥99% of PC records; `gpu_pc_attrib="exact"` on the resulting samples; windows bracket the executions that ran in them.
16. Overhead within the Task 12 thresholds, or the tier decision they dictate.

**What cannot be verified without hardware — stated plainly.** Assertions 13–16. Beyond the gate: whether `functionIndex` is the cubin `.symtab` index (Task 6, and the trigger for `gpu_pc_sample_batch_v2`); whether `pcOffset` is function-relative in the sense the line table is; every rate and buffer-sizing number; hardware-buffer overflow behaviour; whether the collection mode can change between `Stop` and `Start`; whether per-context enable holds when contexts are created lazily on other threads; whether the `MODULE_UNLOAD_STARTING` drain preserves PC uniqueness; and whether a `cuptiFinalize` handler runs at all. MPS and cross-process contention for the sampling hardware are **not verifiable at all** from inside a profiled process, on hardware or otherwise.

---

## Phase gate

The gate passes when: the GPU-free half (1–12) is green on a machine with no GPU, and the hardware half (13–16) is green on the RTX 3090 **with the Task 12 numbers recorded in this file** and the tier decision they imply applied to Task 11's defaults.

## Risks

**The biggest one: cubin transport is load-bearing for attribution, not just for source lines.** The frozen ABI gives a Tier B PC sample `(cubin_crc, function_index, pc_offset, stall_index)` and nothing that turns any of it into a kernel name — `gpu_pc_sample_batch_v1` carries no `kernel_id`, and `function_index` is not one. The cubin *is* the missing mapping table. And the ABI's own answer for getting it, `bytes_ptr`, is a pointer in the producer's address space that the agent cannot read without `CAP_SYS_PTRACE`, which the capability constraint forbids. So everything Tier B exists to produce sits behind Task 3, a channel that does not exist yet, built on top of a socket whose first duty is a startup rendezvous that must not be delayed. If Task 3 fails or has to be descoped, Tier B produces stall histograms attributable to nothing — honest and useless — and the phase has no product.

Second: `functionIndex`'s meaning is unverified and only hardware settles it. The `_v2` fallback is pre-specified so a negative answer is a small change, but it is a change to a record the plan otherwise treats as frozen.

Third: **CUDA graphs quietly shrink Tier A's addressable market to nearly nothing in the workloads that most want it.** A graph launch is one callback for N kernels, `gpu_exec_v1` cannot carry `graphId`, and the resulting many-to-one attribution looks exact. Graphs are the norm in inference serving. The plan's answer is to detect and refuse rather than guess, which is correct but is a refusal, not a capability — so Tier A ships useful for eager-mode training and CUDA-C workloads and unavailable for much of inference. If that is unacceptable, the work is `gpu_exec_v2` plus a one-launch-to-many-executions join shape, which is a phase, not a field.

Fourth: Tier A's overhead may not be rescuable by duty-cycling, because serialization's cost outlasts the burst. Task 12's cost-÷-duty ratio is the instrument for detecting that, and the thresholds are committed in advance so the answer is not negotiated after the fact.

Fifth, and smaller than it looks: Tier A distorts the CPU and off-CPU profiles taken alongside it, and this phase marks only the GPU side. Mitigated by disclosure (Tasks 9 and 11) rather than by correction, which is honest but leaves an operator holding two profiles of unequal trustworthiness with only one of them labelled.
