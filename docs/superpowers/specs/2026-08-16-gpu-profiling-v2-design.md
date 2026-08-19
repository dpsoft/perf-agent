# GPU Profiling V2 — Design Spec

Status: draft for review
Date: 2026-08-16
Supersedes: the GPU design work on branch `gpu-profiling-spec` (PR #10), which becomes reference material

## 1. Context

perf-agent is an eBPF CPU/off-CPU/PMU profiler. It owns what we will call the
**host semantic plane**: PID/TID, cgroup, pod and container identity, CPU stacks
at a launch site, scheduling and submission timing.

It has no **vendor semantic plane**. DRM/ioctl tracing can observe that a process
submits work to a GPU driver, but it cannot recover a kernel name, a stream, a
stall reason, or a source line. Those facts exist only inside vendor profiling
APIs — CUPTI for NVIDIA, ROCProfiler for AMD.

Neither plane substitutes for the other, and neither can be reconstructed from
the other. V2 exists to add the vendor plane and join it to the host plane that
already works.

PR #10 explored this and produced a vendor-neutral event model, a join ladder,
cgroup attribution, and a pprof projection — validated entirely against replay
fixtures. The model and the join logic are worth keeping. The storage layer, the
projection, the input paths, and the test strategy are not.

## 2. Goals

- Attribute GPU work to the CPU stack, process, container and pod that caused it.
- Support NVIDIA via CUPTI as the first vendor backend, with a transport that AMD
  can adopt unchanged.
- Stay viable as an always-on profiler: bounded memory, bounded overhead,
  accounted losses.
- Emit standard pprof. No new wire format.

## 3. Non-goals

- **Not** an OTLP-profiles emitter. OTLP profiles entered public alpha in March
  2026 and the Profiling SIG advises against relying on it. It round-trips
  losslessly with pprof, and Collector v0.148.0+ ships a pprof receiver, so the
  ecosystem on-ramp already exists without a format change. Revisit at beta.
- **Not** a general off-box symbolization redesign. Off-box symbolization enters
  in Phase 6 scoped to cubin disassembly, where it has a concrete driver.
- **Not** a replacement for vendor tracing tools. Nsight and rocprof remain the
  right tools for one-shot deep analysis.
- **Not** multi-vendor at launch. NVIDIA first; AMD converges onto the same ABI
  afterward.

## 4. Invariants

Three from the architecture discussion, held as constraints on every decision
below:

1. **The vendor SDK is the semantic source of truth.** Kernel identity, execution
   intervals, correlation IDs, stall reasons and source mapping come from CUPTI.
   We do not attempt to infer them from DRM or eBPF.
2. **USDT is transport, never the canonical model.** The probe ABI is
   perf-agent's, not CUPTI-shaped. Raw vendor structs never cross the boundary.
3. **perf-agent owns correlation, host attribution and representation.** The shim
   never unwinds a stack, never symbolizes, and never learns about pods.

Three amendments established during review:

4. **Batch every event class, not only PC samples.** A USDT probe with an eBPF
   consumer attached traps into the kernel on every fire. ParcaGPU's default
   token-bucket of 100 events/sec/thread exists because of probe cost, not data
   volume. Rate-limiting launches would discard correlation anchors that later PC
   sample batches need, so launches must be batched rather than dropped.
5. **GPU detail belongs in sample labels, not stack frames.** See §8.
6. **Clock correlation happens once in the shim, as a fit.** Not per-record.

## 5. Architecture

```
CUDA application
      │  CUDA_INJECTION64_PATH
      ▼
libperfagent-gpu.so
      │  CUPTI callback + activity APIs
      │  clock correlation, batching, token-bucket, cubin capture
      │
      │  vendor-neutral USDT ABI
      ▼
eBPF programs (uprobe/USDT attach)
      │  kernel-side filter; task identity free at probe time
      ▼
ring buffer → canonical GPU events
      │  bounded ring, correlation-ID index
      ▼
join with host plane  ── CPU stack, cgroup/pod/container (existing)
      ▼
pprof
   frames: CPU stack → [gpu:launch] → kernel name
   labels: stall, pc, queue, device, correlation, cgroup, pod, container
```

The shim is the only NVIDIA-aware component. Everything below the USDT boundary
is vendor-neutral.

## 6. The USDT ABI

Versioned, batch-oriented, and stable across CUPTI releases. Large payloads are
passed by pointer + length so that CUPTI struct layout changes do not break the
ABI (the consumer reads a perf-agent-defined record, not a CUPTI one).

| Probe | Carries |
|---|---|
| `gpu_launch_v1` | batch of launches: correlation ID, kernel name ref, queue/stream, context, host timestamp |
| `gpu_exec_v1` | batch of executions: correlation ID, device, queue, start/end in CPU-monotonic |
| `gpu_pc_sample_batch_v1` | batch of PC samples: module CRC, PC offset, function index, stall index, count, and a correlation ID only where the vendor supplies one |
| `gpu_module_load_v1` | cubin identity (CRC), size, and bytes ref |
| `gpu_stall_reason_map_v1` | one-shot stall index → name table |
| `gpu_config_v1` | sampling factor, SM count, clock frequency |
| `gpu_dropped_v1` | producer-side drop counts by class |

### 6.1 Shared runtime, thin adapters

Every vendor needs its own shim — CUPTI, rocprofiler-sdk and Level Zero are
different APIs and nothing below the process boundary can paper over that. The
cost is bounded by factoring aggressively:

```
shim/
  core/     shared: USDT ABI, batching, token bucket, sequence numbers,
            semaphore watch + late-attach replay, clock pairing with step
            detection, periodic drain timer
  nvidia/   CUPTI adapter            → libperfagent-gpu-nvidia.so
  amd/      rocprofiler-sdk adapter  → libperfagent-gpu-amd.so
```

Only five things are genuinely per-vendor: the injection mechanism
(`CUDA_INJECTION64_PATH` vs `LD_PRELOAD`/`HSA_TOOLS_LIB`), SDK registration,
event-kind mapping onto our record types, which timestamp call feeds the clock
fit, and the PC-sampling enable/disable API. Everything else lives in `core/`.

**`core/` is a static archive linked into each adapter, not a shared object.**
Each vendor therefore ships as exactly one self-contained `.so`. This matters
most for deployment: §11 puts the shim on an `emptyDir` mounted into both
containers, and a shared `core` would have to be findable at runtime via RPATH or
`LD_LIBRARY_PATH` inside the application's container — fragile, and secure-execution
mode ignores `LD_LIBRARY_PATH` outright. One file to mount, nothing to resolve.
It also removes any versioned interface between `core` and the adapters. The cost
is that a transport fix requires rebuilding every adapter, which is acceptable at
two of them.

Because the result is injected into someone else's address space, `core/` must be
built with hidden symbol visibility and the adapter must export only the entry
points its vendor SDK requires. Leaking `core` symbols into the application's
global namespace risks collisions with whatever else the process has loaded.

This is the one place the design deliberately diverges from ParcaGPU rather than
following it. Their probes are CUPTI-shaped (`cuda_correlation`, `cubin_loaded`),
so a second vendor means a second ABI and a second consumer. Vendor-neutrality
only pays for itself if `core/` is real; otherwise it is a tax with no benefit.

**NVIDIA ships first.** The risk that carries is that the ABI ossifies
CUPTI-shaped before a second consumer exists. The mitigation is a design-time
cross-check, not implementation work: the rocprofiler bridge on the
`gpu-profiling-spec` branch already shows what ROCm produces — its buffer-tracing
kinds and its `CODE_OBJECT_DEVICE_KERNEL_SYMBOL_REGISTER` callback map onto the
same four record types — so the ABI is reviewed against that known taxonomy
before it is frozen.

Requirements:

- Every batch record carries a monotonically increasing sequence number per
  probe, so the consumer can detect gaps it did not observe.
- `gpu_stall_reason_map_v1`, `gpu_config_v1` and outstanding `gpu_module_load_v1`
  records must be **replayed when a consumer attaches late**. The shim tracks the
  USDT semaphore count to detect attachment.
- Field names avoid CUDA vocabulary where a neutral term exists (`queue` not
  `stream`) so ROCm can emit the same ABI.
- **Every launch and execution carries a correlation ID, without exception.** A
  producer whose vendor API does not supply one must synthesise a unique value
  per launch; emitting the zero value is not permitted. A vendor id that wraps
  counts as "does not supply one" — see §6.3 finding 4.
- **The semaphore gates the producer, and costs nothing when nobody listens.**
  Measured with the real shim: with no consumer attached the semaphore reads 0,
  and 200 launches produced 200 skipped probes and zero emitted records. The shim
  does no formatting, no batching and no work beyond one load-and-branch per
  launch until a consumer attaches. This is what makes it safe to ship the shim
  in an application container that is not currently being profiled.
- `core/` **owns a drain timer**, and its period is a first-class tunable rather
  than a vendor default. Both vendors deliver events in buffers that are handed
  over when full, so on an idle GPU events sit undelivered for as long as it
  takes to fill one — measured at up to 15 s on CUPTI (§10). The vendor's own
  periodic-flush knob is not a substitute: CUPTI's flushes only *full* buffers.
- `core/` samples the (vendor clock, `CLOCK_MONOTONIC`) pair on that same timer
  and watches for discontinuity, because at least one vendor clock —
  `cuptiGetTimestamp` — is `CLOCK_REALTIME` and can be stepped (§7).

  This last requirement was discovered by building Phase 2 rather than by
  design, and it is the kind of constraint the phase existed to surface. The
  core indexes launches by `CorrelationID`, so every correlation-less launch
  collapses onto the single zero-value key: the cache retains one of them, the
  rest are counted as replacements, and the heuristic join — which after Phase 2
  serves correlation-less executions exclusively — sees at most one candidate.
  A DRM/lifecycle backend emitting correlation-less events would therefore
  attribute almost nothing, and `MatchedLaunchCount` would read 1 regardless of
  volume.

  A synthetic ID costs the producer a counter. Making the core key on something
  else would cost it the O(1) exact-join path that Phase 2's gate depends on.

Authoring the probes requires `systemtap-sdt-devel` (`sys/sdt.h`), or emitting
`.note.stapsdt` notes via inline asm to avoid the build dependency entirely.

**Both routes are verified working on the lab box.** `systemtap-sdt-devel`
(5.5-1.fc44) *is* installed — an earlier note in this spec claiming otherwise
was wrong. A probe built with `DTRACE_PROBE1` compiles and `readelf -n` shows a
valid `NT_STAPSDT` note carrying provider, name, location and argument
descriptor. The inline-asm route compiles equally cleanly with no systemtap
dependency and produces a note a consumer reads identically.

Two findings that bear on Phase 3's choice between them:

- The systemtap header emitted exactly one probe per call site. The hand-rolled
  inline-asm version emitted the same probe **twice** — once inlined into the
  caller and once standalone — because nothing stopped the compiler duplicating
  the call site. A hand-rolled macro must account for inlining; the header
  already does.
- **Semaphores are not automatic, and the shim must supply them itself.** A
  plain `DTRACE_PROBE1` reports `Semaphore: 0x0`. §6's replay-on-late-attach
  requirement depends on the semaphore count to detect when a consumer attaches,
  so the shim must:

  ```c
  #define _SDT_HAS_SEMAPHORES 1
  #include <sys/sdt.h>

  __extension__ unsigned short perfagent_gpu_launch_v1_semaphore
      __attribute__((unused)) __attribute__((section(".probes")));

  if (perfagent_gpu_launch_v1_semaphore) {   /* nobody listening: skip the work */
      STAP_PROBE1(perfagent, gpu_launch_v1, ptr);
  }
  ```

  Verified: the note then carries a real semaphore address rather than `0x0`,
  and the variable reads 0 with nothing attached, so an unobserved shim does no
  batching work at all.

  The consumer does **not** need the symbol to be exported — it reads the
  semaphore address out of the note and attaches with `ref_ctr_offset` pointing
  at it, letting the kernel maintain the count. That is the same mechanism the
  §11 sidecar deployment relies on, where the shim and agent share the file but
  not a symbol table.

  Getting this wrong is silent: the probe still fires, the shim still works, and
  late attachment simply never replays — which surfaces much later as
  unsymbolizable PC samples after an agent restart.

On that evidence the systemtap header is the better default, and the inline-asm
route stays a proven fallback rather than a hypothetical one — so the build
dependency is not load-bearing.

### 6.2 What the consumer side already gets, and what it must supply

Checked against the `cilium/ebpf` version this repo already depends on (v0.21.0),
so Phase 3 does not rediscover it:

**Already provided.** `link.UprobeOptions.RefCtrOffset` exists and is passed to
the kernel as `OffsetReferenceCount`, so the **kernel maintains the semaphore
count** — the consumer does not write to the shim's memory, and attach/detach
bookkeeping is not ours to get right. It is feature-gated on
`/sys/bus/event_source/devices/uprobe/format/ref_ctr_offset`, so a kernel
without it degrades to an always-firing probe rather than failing; the shim's
`if (semaphore)` guard simply always passes.

**Not provided.** The module contains no `.note.stapsdt` parsing at all.
Phase 3 must supply it: read the ELF note out of the shim `.so` to recover, per
probe, the location offset and the semaphore address, then hand those to
`Uprobe` as the offset and `RefCtrOffset`.

That parser is worth building early. It is pure userspace ELF reading — no
GPU, no CUPTI, no privileges, no dependence on the probe payloads — so it is
the one piece of Phase 3 that can be written and fully tested before the ABI
is frozen, and it is required no matter what shape the ABI takes.


### 6.3 Draft record layouts

Drafted against the ROCm bridge on branch `gpu-profiling-spec`
(`examples/rocprofiler_sdk_preload_bridge.cpp`) — real code, validated on the
development host's AMD hardware — rather than against CUPTI documentation.
**NVIDIA takes the identical approach**: the CUPTI adapter emits these same
records, and anywhere the two vendors differ is called out below rather than
papered over. That is the whole point of §6.1 — one ABI, thin adapters.

The layouts below already incorporate the CUPTI spike's findings (2026-08-19 —
see *What the spike settled*), so the fields that once carried a **[spike]** mark
now carry an answer. Everything else is grounded in a working producer.

#### Conventions

- Every probe fires with `(ptr, count, seq)`: a pointer to an array of
  fixed-size records, how many, and a per-probe monotonic sequence number.
  Records are fixed-size so the eBPF consumer can stride them without parsing;
  variable-length data is interned (see kernel names below).
- All integers little-endian, naturally aligned, explicitly sized. No enums, no
  bitfields, no compiler-layout dependence — the consumer is a BPF program, not
  a C compiler.
- `correlation` is `u64`. ROCm supplies `record->correlation_id.internal`
  directly. CUPTI's `correlationId` is a `uint32` process-wide counter that wraps
  in hours under load, so the adapter does not merely zero-extend it: it prefixes
  a 32-bit epoch (§6.3, finding 4). §6's requirement that **every** launch and
  execution carry a non-zero correlation applies to both, and on the CUPTI side
  it is the adapter, not the vendor, that makes it true.
- All `*_ns` are CPU-monotonic. Conversion happens in the adapter, never in the
  core (§7).

#### The interning decision, and why the real code forced it

Kernel names are variable-length, high-cardinality, and repeat constantly. They
must not ride on every execution record.

The ROCm bridge answered this by accident of design: it subscribes to
`ROCPROFILER_CODE_OBJECT_DEVICE_KERNEL_SYMBOL_REGISTER`, which delivers
`kernel_id → kernel_name` once at registration, and its dispatch records then
carry only `dispatch_info.kernel_id`. So ROCm's natural shape is already an
interned id plus a separate mapping.

The ABI adopts that shape, and CUPTI synthesises it: hash the function name to
a `kernel_id` and emit the mapping once on first sight. This costs the CUPTI
adapter a small map and saves every execution record from carrying a string.

#### Probes

| Probe | Record fields |
|---|---|
| `gpu_launch_v1` | `correlation u64`, `kernel_id u64`, `queue_id u64`, `context_id u64`, `time_ns u64`, `tid u32`, `_pad u32` |
| `gpu_exec_v1` | `correlation u64`, `kernel_id u64`, `queue_id u64`, `device_id u64`, `start_ns u64`, `end_ns u64` |
| `gpu_kernel_name_v1` | `kernel_id u64`, `name_len u16`, `name[]` — the interning table; replayed on late attach |
| `gpu_pc_sample_batch_v1` | `cubin_crc u64`, `correlation u64` (0 = unknown), `pc_offset u64`, `function_index u32`, `stall_index u32`, `count u32`, `_pad u32` |
| `gpu_module_load_v1` | `cubin_crc u64`, `module_id u64`, `size_bytes u64`, `load_ns u64`, `bytes_ptr u64` |
| `gpu_stall_reason_map_v1` | `index u32`, `name_len u16`, `name[]` — one-shot, replayed on late attach |
| `gpu_config_v1` | `sampling_factor u32`, `sm_count u32`, `clock_hz u64`, `vendor u8` |
| `gpu_dropped_v1` | `class u8`, `count u64` — producer-side loss, per §9 and the §7 sink contract |

#### Where the two vendors genuinely differ

Three asymmetries the bridge made visible. None is fatal; all need a decision
before freeze.

1. **Module identity is not the same concept.** CUPTI's `cubin_loaded` delivers
   raw binary bytes keyed by CRC — a real object to disassemble. ROCm's
   `CODE_OBJECT_DEVICE_KERNEL_SYMBOL_REGISTER` delivers a *symbol registration*,
   `kernel_id → name`, with no bytes. So `gpu_module_load_v1` is CUPTI-shaped
   and ROCm will not populate `bytes_ptr`.

   This is fine — PC sampling is capability-gated (§7), and a backend that
   cannot supply module bytes simply does not advertise `CapabilityPCSampling`.
   But it means `gpu_module_load_v1` and `gpu_kernel_name_v1` are **separate
   probes on purpose**: ROCm emits only the latter, CUPTI emits both.

2. **Queue and device identity is thinner on the ROCm side than expected.** The
   bridge reads `dispatch_info.dispatch_id` and `kernel_id` but never extracts
   agent or queue identity. Either rocprofiler exposes it elsewhere and the
   bridge simply did not need it, or it requires a separate subscription. Until
   that is settled, `queue_id` and `device_id` are **optional (zero means
   unknown)** rather than required — which the core already tolerates, since the
   heuristic join keys on `(queue, kernel name)` and a zero queue degrades to a
   single group.

3. **CUPTI has a launch/exec split ROCm expresses differently.** ROCm's
   `HIP_RUNTIME_API` buffer records are the launch side and `KERNEL_DISPATCH`
   the execution side — a clean match. CUPTI's callback API supplies the launch
   anchor and the activity API the execution, which is the same split arriving
   through two different mechanisms. No ABI consequence; noted so the CUPTI
   adapter is not tempted to invent a third record type. The spike confirmed that
   the activity record carries everything `gpu_exec_v1` needs — `start`, `end`,
   `correlationId`, `deviceId`, `streamId` and the kernel name — in a single
   `CUpti_ActivityKernel12`.

#### What the spike settled

Ran 2026-08-19 against the lab RTX 3090 (GA102, sm_86), driver 610.57.04, CUDA
13.3, `RmProfilingAdminOnly: 0`. Four throwaway programs; the code is discarded,
the findings below are the output. Two of the four answers changed the ABI.

**1. Device and queue identity: the execution record supplies both directly.**

`CUpti_ActivityKernel12` — the current revision in CUDA 13.3 — carries
`deviceId`, `contextId`, `streamId`, `correlationId`, `gridId`, `channelID` and
`name`. Nothing has to be tracked from callbacks to populate `gpu_exec_v1`.

The *launch* side is what cannot be trusted. A launch callback can ask
`cuptiGetStreamIdEx(ctx, stream, perThreadDefaultStream, &id)`, but for the
**default stream** the answer depends on that flag: in one process
`perThreadDefaultStream=0` returned 7 and `=1` returned 13, while the execution
records for those same default-stream launches reported 7. The flag mirrors
whether the application was compiled `--default-stream per-thread`, which the
adapter cannot observe from inside the process. Explicit streams agree under
either flag.

So `gpu_launch_v1.queue_id` stays **optional (zero means unknown)** and the
adapter must not guess it; `gpu_exec_v1` is the authoritative source of queue and
device. The optionality was already conceded for ROCm above; CUPTI needs the same
allowance for an unrelated reason.

**2. The cubin behind `bytes_ptr` must be copied inside the load callback.**

`CUPTI_CBID_RESOURCE_MODULE_LOADED` delivers `{moduleId u32, cubinSize, pCubin}`
pointing at an ELF image. Measured lifetime of that buffer:

| when | result |
|---|---|
| while the module is loaded, including late in process life | readable, contents identical |
| immediately after `cuModuleUnload` | readable, **contents changed** |
| after 64 MB of heap churn | readable, contents changed |

That is the dangerous shape: the buffer is not unmapped, so a late reader gets
silently wrong bytes rather than a fault. The adapter must copy during the load
callback; `MODULE_UNLOAD_STARTING` — which arrives with the same `moduleId` and
the same pointer — is the deadline. CUDA's lazy module loading puts loads and
unloads at arbitrary points in a long-running process, so "copy later" has no
safe definition. `bytes_ptr` on the wire therefore points at an adapter-owned
copy, never at CUPTI's buffer.

`moduleId` is a small per-process sequential `u32` (22, then 23 in one run) — not
a hash, not stable across processes, and not the identity PC samples use. Which
leads to the next finding.

**3. PC samples carry no correlation in the mode we ship; they key on `cubinCrc`.**

The same workload under both collection modes:

| mode | samples | PC records | `correlationId` |
|---|---|---|---|
| `CONTINUOUS` | 103,440 | 352 | 0 on every record |
| `KERNEL_SERIALIZED` | 103,515 | 1,828 | non-zero on all 1,828, every one matching a launch callback's id |

`cupti_pcsampling.h` states it and the hardware agrees: the field is "only valid
for serialized mode of pc sampling collection. For continous mode of collection
the correlationId will be set to 0." Serialized mode serializes kernel execution,
which §2 rules out for continuous production profiling — so **the configuration
we ship is the one where PC samples have no correlation at all.**

What the records do carry: `cubinCrc u64`, `pcOffset u64`, `functionIndex u32`,
`functionName`, and a variable-length array of (stall index, sample count) pairs
drawn from the 38 stall reasons this device exposes. Attribution is therefore
`cubinCrc` → module → `functionIndex`/`pcOffset` → symbol, reaching the kernel
through the module and function rather than through a launch.

`cuptiGetCubinCrc()` over the `MODULE_LOADED` bytes reproduces exactly the CRC
the PC records carry, and that identity is the only thing joining the two. Hence
`cubin_crc` is now the first field of `gpu_module_load_v1`, and
`gpu_pc_sample_batch_v1` keys on `cubin_crc` rather than `module_id`, keeping
`correlation` only as an optional (zero = unknown) field against a future
serialized mode. The canonical model was already right here: `ModuleRef` in
`gpu/types.go` keys on CRC, not on a module id.

The stall array is the one place the fixed-size record rule costs something: the
adapter emits one record per (PC, stall reason) pair rather than one per PC.

**4. `correlationId` is a process-wide counter, and it wraps.**

It starts at 1 and increments once per *outermost* traced API call — not per
launch. Observed directly: `cudaMalloc`=1, `cudaStreamCreate`=2,3,
`cudaLaunchKernel`=4, `cudaDeviceSynchronize`=5, and so on; 54 launches inside a
65-call mix consumed 65 ids. Nested driver calls share the runtime call's id, but
only while the DRIVER domain is unsubscribed: 500 launches consumed 1.004 ids
each under RUNTIME alone and 2.242 each under RUNTIME+DRIVER. **The adapter
subscribes RUNTIME and RESOURCE only** — adding DRIVER halves the time to wrap
and buys nothing this ABI reads.

Rate: 200,000 empty launches on one stream in 0.369 s = 542,515 launches/s, so
2^32 ids exhaust in ~7,900 s ≈ **2.2 hours** at that synthetic ceiling. Even at a
realistic 50k launches/s the wrap lands inside a day — well inside the lifetime
of a profiled service.

§6 requires a unique non-zero correlation on every launch and execution, and the
wire field is already `u64`. The adapter closes the gap: hold a 32-bit epoch,
increment it when an observed id drops below its predecessor by more than a guard
band, and emit `epoch << 32 | correlationId`. Uniqueness is a producer
obligation, not something CUPTI provides.

## 7. Canonical event model

Carried from PR #10 `gpu/types.go`, with changes:

- `Backend` / `EventSink` / capability advertisement: kept as-is. This contract is
  the most valuable artifact of PR #10 and a CUPTI producer fits it unchanged.
- `GPUSample` is **split**. A lean shared type stays; a new capability-gated
  `GPUPCSample` carries PC offset, module identity, stall index and count. This
  prevents CUPTI's richness from either taxing every backend or being truncated at
  the boundary — the failure mode the architecture discussion identified as
  "the canonical model becomes the limiting factor once you normalize."
- `GPUModule` is added as symbolization metadata on its own lifecycle. It is not
  an event in the profile.
- `EventSink` methods gain an error return, so a producer can be told to slow
  down and losses can be accounted. Today they return nothing.

Clock domain: the existing `ClockDomain` type models cpu-monotonic, synced and
gpu-device, but `ValidateSupportedClockDomain` rejects the latter two. That stays.
Producers convert before emitting. A producer must not anchor each record's end to
"now" — the approach currently prototyped in
`examples/rocprofiler_sdk_preload_bridge.cpp` — which drifts under queueing.

**`cuptiGetTimestamp` is `CLOCK_REALTIME`**, measured rather than assumed: across
2,000 trials it landed inside a `CLOCK_REALTIME` bracket 2,000 times and inside a
`CLOCK_MONOTONIC` bracket zero times, and it sits exactly 37 s from `CLOCK_TAI` —
the current TAI−UTC offset. Activity record `start`/`end` share that domain.

perf-agent's own samples are `bpf_ktime_get_ns()`, which is `CLOCK_MONOTONIC`
(`unwind/dwarfagent/miss_drainer.go` already documents this). So the conversion is
not a fit against a drifting device clock, as this spec previously assumed; it is
the realtime-to-monotonic offset sampled as a pair,
`mono = cupti_ts - (realtime - monotonic)`.

The hazard changes shape with it. `CLOCK_REALTIME` is slewed by NTP and can be
**stepped** — by NTP, by an administrator, by a container host's clock sync. Slew
is harmless at profiling resolution: the pair offset moved under 15 µs over 20 s
on an NTP-disciplined box. A step is not harmless. It shifts every subsequent GPU
timestamp relative to the CPU profile at once and can place an execution before
its own launch. The shim therefore re-samples the pair periodically and watches
for a *discontinuity* rather than fitting a smooth drift model; on a detected step
it re-anchors and marks the affected window instead of silently emitting
mis-joined records.

## 8. Output representation

PR #10 encodes all GPU context as synthetic stack frames. Frames are stack
identity, so `[gpu:pc:0x1a40]` as a frame produces one flame-graph leaf per
sampled instruction. With PC sampling at ~100 samples/sec this destroys
aggregation.

The split:

- **Frames** — what should nest in the flame graph: real CPU stack, then
  `[gpu:launch]`, then the kernel name.
- **Labels** — what must not fragment aggregation: stall reason, PC, queue,
  device, correlation ID, cgroup, pod UID, container ID.

This needs a small change in the existing pprof builder. `pprof/pprof.go` exposes
`BuildersOptions.Labels`, which is builder-level and constant — every sample in a
profile receives the same map. `ProfileSample`, which callers emit, has no
`Labels` field. Adding one and merging it with the builder map where labels are
applied is the whole change. `google/pprof`'s `Sample.Label` is already wired up.

## 9. Overhead control

- **Token-bucket rate limit** on callback-path probes, configurable, defaulting
  conservatively.
- **Batching** on every probe class (§4.4).
- **Adaptive PC sampling**: enable in short windows (~50 ms), then disable, with
  the gap tuned by a controller to hold a target samples/sec. Continuous PC
  sampling in kernel-serialized mode is not production-viable.
- **Drop accounting** end to end: producer drops via `gpu_dropped_v1`, ring-buffer
  drops at the eBPF boundary, and eviction counts from the bounded ring. All
  surfaced; none silent.

### 9.1 What the injected shim actually costs

Measured on the lab 3090 (2026-08-19) against a CUDA process driving a saturating
~393k launches/s — an unrealistic ceiling, chosen so the per-launch cost is
visible at all. Five interleaved 6-second runs per mode, medians:

| shim mode | launches/s | vs baseline |
|---|---:|---:|
| no injection | 393,325 | — |
| injected, CUPTI untouched | 393,492 | 0.0% |
| callbacks (RUNTIME + RESOURCE) | 391,408 | −0.5% |
| activity (CONCURRENT_KERNEL) | 354,158 | −10.0% |
| both | 353,825 | −10.0% |

Injection itself is free, and the callback path — the launch anchor Phase 4 is
built on — is nearly free. The entire cost sits in the activity path: ~0.28 µs per
launch (2.54 µs → 2.82 µs). That is a per-launch constant to budget against, not a
percentage; at a realistic 10–50k launches/s it is well under 1%.

Which inverts where the pressure lies: the token bucket protects the callback path,
but the thing worth being adaptive about is the activity path.

## 10. Correlation and joins

The join ladder from PR #10 `gpu/timeline.go` is kept: exact correlation ID first,
then queue + kernel-name + bounded-time heuristic, with `Heuristic` and
`Ambiguous` marked per view and rolled into `JoinStats`. A guessed join is never
presented as a vendor-provided one. NVIDIA will mostly take the exact path, but
PC sample batches arrive late and need the honesty machinery.

PC samples are a special case the spike forced (§6.3, finding 3): in continuous
mode they carry **no correlation ID**, so the exact-correlation rung is not
merely unlikely for them, it is unavailable. They enter through the module
instead — `cubin_crc` to a loaded module, `function_index`/`pc_offset` to a
symbol — and are attributed to executions of that kernel within the eviction
horizon. Where that window holds more than one execution of the same kernel, the
attribution is `Ambiguous` and is marked, not guessed away.

The storage under it is rewritten:

- Unbounded slices become a **bounded ring with time-based eviction**. The
  eviction horizon is what determines how late a PC sample batch can arrive and
  still be attributable, so it is a first-class tunable, not an implementation
  detail. It now has a measured floor — see below.

**How late records actually are.** Activity records do not arrive when a kernel
ends; they arrive when their buffer is delivered, and CUPTI delivers a buffer only
once it is full. Measured delivery lag after kernel end:

| workload | shim flush | p50 | p99 | max |
|---|---|---:|---:|---:|
| ~393k launches/s | none | 33 ms | 112 ms | 119 ms |
| ~25 launches/s | none | 7.6 s | 15.0 s | 15.0 s |
| ~25 launches/s | own `cuptiActivityFlushAll` every 100 ms | 91 ms | 101 ms | 101 ms |

The idle case is the dangerous one, and it is the counterintuitive direction: a
*quiet* GPU delivers records later, because a 4 MB buffer holding five records
takes an age to fill. In the 25 launches/s run with no flush, nothing arrived at
all until process exit — one buffer, 375 records, up to 15 s stale.

`cuptiActivityFlushPeriod()` does not solve it. It returns success and changes
nothing, because by its own contract it "can return only those activity buffers
which are full". The shim must own a flush timer calling `cuptiActivityFlushAll()`;
at 100 ms that bounded delivery to ~100 ms at both extremes and cost nothing
measurable at the 393k launches/s ceiling.

So the eviction horizon is not a guess: it is the shim's flush period plus transit,
and the flush period is the knob that sets it. A horizon shorter than the flush
period drops every exec record's join, silently, and only on idle workloads.
- Linear scans become a **correlation-ID index**. Today `Snapshot()` scans all
  launches for every execution and every event, and all samples for every
  execution — quadratic, and invisible at fixture scale.
- Ingestion is single-owner or mutex-guarded. Today `Timeline` and `Manager` have
  no synchronization and are safe only by the accident that emitters touch
  different slices and `Snapshot()` runs after both `Stop()` calls join.

## 11. Deployment

perf-agent runs per-pod rather than as a DaemonSet, which changes the transport
question materially. Parca does not solve the sidecar problem; it avoids it by
running as a node DaemonSet with `hostPID` and `privileged`.

Target model: **sidecar plus a shared volume.**

- The shim ships on an `emptyDir` mounted into both the application container and
  the perf-agent container at the same path. The app sets
  `CUDA_INJECTION64_PATH` to it; the agent opens the identical file in its own
  mount namespace. Same inode — which is what uprobe attachment actually keys on
  — with no `/proc/<pid>/root` traversal and no overlay-inode instability.

  The injection half of this is confirmed on hardware (2026-08-19): a shim
  exporting `InitializeInjection` was loaded into an ordinary CUDA process that
  links no CUPTI and knows nothing about profiling, subscribed successfully, and
  captured 6.67 M launches, their execution records and the module load. Injection
  cost nothing measurable on its own (§9.1).

  So is the inode claim this bullet rests on. In the victim's `/proc/<pid>/maps`
  the injected shim appears as four file-backed mappings — `r-xp` among them —
  all carrying the same inode that `stat` reports for the file on disk. That
  inode is what uprobe attachment keys on, so the shared-volume scheme has no
  remaining unknown on the injection side; what is untested is only the agent
  opening the same inode from a *different mount namespace*.
- PID visibility is still required, but for perf-agent's **existing** function,
  not the GPU path: `unwind/procmap` reads `/proc/<pid>/maps`,
  `/proc/<pid>/map_files/...` and `/proc/<pid>/comm` to unwind and symbolize.
  `internal/nspid` already translates namespace-local PIDs to host PIDs, and
  `internal/k8slabels` already derives identity from `/proc` and downward-API env
  with no Kubernetes API, kubelet, or CRI socket.

**Capability reduction is a prerequisite, not a nicety.** perf-agent currently
requests `CAP_SYS_ADMIN` in `perfagent/agent.go` and every documented `setcap`
line includes it. On any kernel ≥ 5.9 it is redundant:

- `CAP_PERFMON` (5.8) covers `perf_event_open`, including `pid=-1` system-wide.
- `CAP_CHECKPOINT_RESTORE` (5.9) covers `/proc/<pid>/map_files`, which is what
  `perfagent/agent.go` already documents while `unwind/ehmaps/openable.go` still
  attributes to `CAP_SYS_ADMIN`.

`CAP_SYS_ADMIN` is near-root and is what gets a per-pod agent rejected. Dropping
it is the difference between "needs a privileged pod" and "needs four narrow
capabilities."

## 12. What we carry, what we drop

Carried from PR #10 as **contracts, revised — not ported verbatim**:

- `gpu/types.go` — the backend/sink contract and capability model, with §7 changes
- the join ladder logic from `gpu/timeline.go`
- `gpu/backend/linuxdrm` and `linuxkfd` lifecycle observation, as a complement to
  the vendor plane rather than a substitute
- cgroup attribution, **folded into the existing `internal/k8slabels`** rather
  than carried. There are currently three cgroup parsers in the tree:
  `gpu/cgroupmeta/cgroupmeta.go` and `gpu/backend/linuxdrm/cgroup.go` are verbatim
  duplicates of each other, and `internal/k8slabels/cgroup_parse.go` predates both
  and handles cgroupfs and systemd driver conventions better.

Carried as the **template for the vendor adapter**, not as shipping code:

- `examples/rocprofiler_sdk_preload_bridge.cpp` (837 lines) is already the shim
  architecture this design calls for. It registers via `rocprofiler_configure`,
  uses buffer tracing for execution records, and hooks
  `CODE_OBJECT_DEVICE_KERNEL_SYMBOL_REGISTER` — the AMD analogue of cubin module
  load. The only part that does not survive is its emit path: four
  `write_line_locked` call sites that become USDT probe fires. It informs the ABI
  review in §6.1 and becomes the second adapter in Phase 5.

Dropped:

- demo scripts (14 files), `cmd/amd-sample-collector` (1,584 lines), examples.
  The collector exists only because there was no in-process transport; the USDT
  ABI removes its reason to exist.
- the 53 checked-in replay goldens (§13)
- the experimental CLI flag surface, collapsed to a source selector and an output
  selector

Dropped on a delay:

- the stdin input paths — `--gpu-stream-stdin`, `--gpu-amd-sample-stdin` — and the
  `stream` and `amdsample` backends. These are the only ROCm execution-data path,
  and with NVIDIA shipping first their removal would leave AMD degraded to
  lifecycle plus HIP host launches for a full cycle. They are therefore kept alive
  but **deprecated and undocumented** through Phases 3–4, and deleted in Phase 5
  once the ROCm adapter replaces them. Deletion is cleanup, not a prerequisite, so
  this costs nothing and removes the regression window entirely.

`gpu/backend/replay` and `gpu/host/replay` are kept but demoted from CLI flags to
test-only, to drive the conformance suite deterministically.

## 13. Testing

Replace goldens with a **backend-agnostic conformance suite**. The 53 checked-in
goldens each encode the current JSON shape of the normalized model; adding a field
regenerates all of them and produces an unreviewable diff, which pressures the
model to stop evolving — exactly what this work requires it to do. Two of them
already fail host-dependently by resolving cgroup ID 1000 against the real host,
which is proof they capture environment as contract.

The suite asserts invariants that any producer must satisfy:

- an exact join is never reported as heuristic, and heuristic joins are always
  marked
- emitted timestamps are monotonic within the declared clock domain
- dropped events are counted at every stage, never silently lost
- eviction from the bounded ring is accounted and bounded
- a sample whose launch has been evicted degrades to unattributed rather than
  mis-attributed

Replay, HIP and CUPTI producers all run the same suite. Five existing test files
read `gpu/testdata` and are rewritten as part of this work.

## 14. Phasing

Each phase has an exit gate. No phase begins before its predecessor's gate passes.

This spec describes a program, not a single unit of work. **The first
implementation plan covers Phases 1 and 2 only** — the enablers and the
continuous core. Phases 3–5 are specified here to establish direction and to
constrain Phase 2's interfaces, but each gets its own plan once its predecessor's
gate has passed and its open questions (§17) are resolved.

**Phase 1 — Enablers on `main`.** `ProfileSample.Labels`; drop `cap.SYS_ADMIN`
from the capability set and fix the two stale comments and the documented `setcap`
lines. Two small independent PRs.
*Gate:* a setcap'd binary without `cap_sys_admin` completes both a `--pid` and an
`-a` profile run.

**Phase 2 — Core made continuous.** Bounded ring, correlation-ID index, sink
backpressure and drop counters, guarded ingestion. Port types and join ladder;
rewrite storage and projection. Conformance suite replaces goldens.
*Gate:* a synthetic high-rate replay holds flat memory and sub-second snapshot.

**Phase 3 — USDT ABI, shared shim core, eBPF consumer.** The `core/` runtime of
§6.1 and the consumer, driven by a stub emitter. Before the ABI is frozen it is
reviewed against the rocprofiler bridge's known event taxonomy (§6.1), on paper.
*Gate:* the stub drives the full pipeline to pprof on a machine with no GPU, and
the ABI review has been done.

**Phase 4 — CUPTI adapter: callback and activity only. NVIDIA ships.** Launch
correlation anchor, activity records for real GPU intervals, clock correlation,
token bucket. This is the milestone the program exists for.
*Gate:* on the RTX 3090, a CUDA workload yields exact-join kernels carrying real
CPU stacks.

**Phase 5 — ROCm adapter.** Port the existing bridge's emit path onto the ABI.
Small, because the bridge is written and validated. Its real function is to prove
`core/` is not secretly CUPTI-shaped — a split with one consumer cannot be
trusted. Deletes the deprecated stdin paths and the `stream`/`amdsample` backends.
*Gate:* AMD produces exact-join kernel executions through the same consumer and
the same canonical events as NVIDIA, with no NVIDIA-specific code in `core/`.

**Phase 6 — PC sampling and cubin.** Adaptive windows, bounded launch cache for
late batches, stall-reason and config replay on late attach, cubin capture by CRC,
and cubin disassembly to address→source. Off-box symbolization enters here. Ships
disabled by default.
*Gate:* measured overhead on a real workload, not a microbenchmark; and a flame
graph that reaches a CUDA source line from a CPU stack.

## 15. Lab readiness

Verified on the development host:

- RTX 3090 (GA102, sm_86) — Ampere, so CUPTI PC sampling is supported (Volta+)
- CUDA 13.3 with CUPTI including `cupti_pcsampling.h`, plus `nvcc` and `nsys`
- the card is deliberately idle/unbound; `gpu-lab on|off|status` manages it

Setup steps, none of them obstacles:

- `sys/sdt.h` is not installed (`systemtap-sdt-devel`), and it turns out not to be
  needed. **Decided on evidence 2026-08-19: the shim emits `.note.stapsdt` via
  inline asm and carries no systemtap build dependency.** A hand-written macro
  produced a note that `readelf -n` reads back correctly (provider, name, base,
  semaphore, `8@%rax 8@%rdx 8@%rcx`), and `internal/usdt.ParseFile` — the parser
  merged in PR #32 — recovered it from the real shim `.so`, resolving the
  semaphore's link-time address to a file offset. That is the parser's first
  exercise against a producer rather than a fixture.

  The argument descriptor came back as whatever registers the compiler happened
  to choose, which is the concrete reason `usdt.Probe.Args` is stored verbatim
  and parsed by the consumer rather than assumed by the ABI.
- `NVreg_RestrictProfilingToAdminUsers=0` is needed for non-root access to the
  CUPTI profiling APIs. On the lab box this is already set — `/proc/driver/nvidia/params`
  reports `RmProfilingAdminOnly: 0`, and the 2026-08-19 spike ran PC sampling as
  an unprivileged user against driver 610.57.04 to confirm it. Elsewhere it is a
  modprobe option and a reboot. In
  production it is a **node prerequisite in the same class as installing the
  driver**, and it matters because the shim runs as the application's user, which
  in a container is usually not root.
- Phase 6 source mapping requires workloads compiled with `nvcc -lineinfo`;
  without it, degrade to PC-offset frames.

## 16. Risks

- **ABI churn.** Freezing the USDT ABI in Phase 3 before CUPTI experience in
  Phase 4 risks a bad shape. Mitigated by pointer-passed records and version
  suffixes; a narrow throwaway spike against CUPTI before Phase 3 freezes the ABI
  would reduce this further and is worth considering.
- **Probe overhead at high launch rates.** Batching is the mitigation; if it is
  insufficient, the fallback is sampling launches while preserving correlation
  anchors for sampled kernels only.
- **Late attach.** Cubin loads, stall maps and device config occur before the
  consumer attaches. Mitigated by semaphore-count detection and replay; incomplete
  replay produces unsymbolizable PC samples rather than wrong ones.
- **The ABI ossifies CUPTI-shaped.** NVIDIA ships first and is the only consumer
  until Phase 5, which is exactly how ParcaGPU's probe surface ended up
  vendor-specific. Mitigated by the paper review in §6.1 against the rocprofiler
  bridge's known taxonomy, and by Phase 5's gate being explicitly "no
  NVIDIA-specific code in `core/`" rather than merely "AMD works". If Phase 5
  requires ABI changes, that is the design working as intended, not a failure —
  which is why the version suffixes exist.

## 17. Open questions

1. Does a narrow CUPTI spike precede Phase 3, to inform the ABI before it freezes?
   The trade is ABI quality against the risk that a spike on unbounded buffers
   hides the failure mode Phase 2 exists to fix. Partly answered by §6.1: the
   rocprofiler bridge already supplies a second real taxonomy to design against,
   which is most of what a spike would have bought.

   **Phase 2 shifted this toward yes.** Twelve defects in that phase came from
   specifying behaviour against a document rather than against something real,
   and the ABI is the most expensive artifact in the program to get wrong —
   version suffixes make it survivable, not free. The mitigation for the
   original objection is that a spike's output should be *knowledge about CUPTI
   record shapes*, with the code discarded; it does not need to run against the
   Phase 2 core at all, so it cannot be misled by buffer behaviour.

   **Settled 2026-08-19: yes, and it ran.** The findings are in §6.3 *What the
   spike settled*. It repaid itself — two of its four answers changed the ABI
   (`gpu_pc_sample_batch_v1` cannot key on correlation; `gpu_module_load_v1`
   needs `cubin_crc`), and a third turned a correctness assumption into a
   producer obligation (correlation IDs wrap in hours, not never). The code was
   discarded as planned; only the knowledge was kept.
2. Sidecar or same-container as the shipping default. Sidecar plus shared volume
   is the design target; same-container is simpler but couples lifecycles.
