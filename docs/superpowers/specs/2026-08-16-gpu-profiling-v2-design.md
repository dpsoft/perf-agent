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
  in Phase 5 scoped to cubin disassembly, where it has a concrete driver.
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
| `gpu_pc_sample_batch_v1` | batch of PC samples: correlation ID, module ref, PC offset, stall index, count |
| `gpu_module_load_v1` | cubin identity (CRC), size, and bytes ref |
| `gpu_stall_reason_map_v1` | one-shot stall index → name table |
| `gpu_config_v1` | sampling factor, SM count, clock frequency |
| `gpu_dropped_v1` | producer-side drop counts by class |

Requirements:

- Every batch record carries a monotonically increasing sequence number per
  probe, so the consumer can detect gaps it did not observe.
- `gpu_stall_reason_map_v1`, `gpu_config_v1` and outstanding `gpu_module_load_v1`
  records must be **replayed when a consumer attaches late**. The shim tracks the
  USDT semaphore count to detect attachment.
- Field names avoid CUDA vocabulary where a neutral term exists (`queue` not
  `stream`) so ROCm can emit the same ABI.

Authoring the probes requires `systemtap-sdt-devel` (`sys/sdt.h`), which is not
currently installed on the lab box.

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
Producers convert before emitting. The shim establishes the conversion by sampling
`cuptiGetTimestamp` against `CLOCK_MONOTONIC` at start and periodically re-fitting
for drift. It must not anchor each record's end to "now" — the approach currently
prototyped in `examples/rocprofiler_sdk_preload_bridge.cpp` — which drifts under
queueing.

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

## 10. Correlation and joins

The join ladder from PR #10 `gpu/timeline.go` is kept: exact correlation ID first,
then queue + kernel-name + bounded-time heuristic, with `Heuristic` and
`Ambiguous` marked per view and rolled into `JoinStats`. A guessed join is never
presented as a vendor-provided one. NVIDIA will mostly take the exact path, but
PC sample batches arrive late and need the honesty machinery.

The storage under it is rewritten:

- Unbounded slices become a **bounded ring with time-based eviction**. The
  eviction horizon is what determines how late a PC sample batch can arrive and
  still be attributable, so it is a first-class tunable, not an implementation
  detail.
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

Dropped:

- demo scripts (14 files), `cmd/amd-sample-collector` (1,584 lines), examples
- the stdin input paths — `--gpu-stream-stdin`, `--gpu-amd-sample-stdin` — and
  with them the `stream` and `amdsample` backends. This removes the only ROCm
  execution-data path; AMD degrades to lifecycle plus HIP host launches until a
  ROCm shim emits the ABI. Accepted deliberately.
- the 53 checked-in replay goldens (§13)
- the experimental CLI flag surface, collapsed to a source selector and an output
  selector

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

**Phase 3 — USDT ABI and eBPF consumer.** Frozen ABI, consumer driven by a stub
emitter.
*Gate:* the stub drives the full pipeline to pprof on a machine with no GPU.

**Phase 4 — CUPTI shim, callback and activity only.** Launch correlation anchor,
activity records for real GPU intervals, clock correlation, token bucket.
*Gate:* on the RTX 3090, a CUDA workload yields exact-join kernels carrying real
CPU stacks.

**Phase 5 — PC sampling and cubin.** Adaptive windows, bounded launch cache for
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

Gaps:

- `sys/sdt.h` is missing — `systemtap-sdt-devel` is required to author USDT probes
  and therefore blocks Phase 3
- NVIDIA gates GPU performance counters to admin users by default
  (`NVreg_RestrictProfilingToAdminUsers`). Whether PC sampling is reachable
  without a modprobe option and reboot is **unverified** and should be checked
  before Phase 5 is scheduled.
- Phase 5 requires workloads compiled with `nvcc -lineinfo` for source mapping;
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
- **AMD regression window.** Between dropping the stdin paths and shipping a ROCm
  shim, AMD has no kernel-execution data. Accepted.
- **NVIDIA profiling restriction** could block Phase 5 entirely on some hosts.
  Unverified.

## 17. Open questions

1. Does a narrow CUPTI spike precede Phase 3, to inform the ABI before it freezes?
   The trade is ABI quality against the risk that a spike on unbounded buffers
   hides the failure mode Phase 2 exists to fix.
2. Does the ROCm shim adopt the ABI in this cycle, or does AMD stay degraded until
   a later one?
3. Sidecar or same-container as the shipping default. Sidecar plus shared volume
   is the design target; same-container is simpler but couples lifecycles.
