# GPU Profiling — Draft Design Spec

**Status:** draft for review and extension.
**Branch context:** `gpu-profiling-spec`
**Predecessor work:** S3-S9 DWARF unwinding and pprof fidelity improvements.
**Goal of this draft:** define a first architecture for full-stack GPU profiling that fits the current perf-agent collector model without pretending that GPU hardware profiling can be implemented with eBPF alone.

## 1. Overview

This document defines the design for integrating GPU profiling into `perf-agent`.

The goal is to extend `perf-agent` into a unified CPU + GPU continuous profiling system that preserves the branch's existing CPU-side strengths while adding timeline-oriented GPU observability.

## 2. Design Principles

### 2.1 Timeline-first

GPU workloads are asynchronous and queue-based.

Profiling must therefore be timeline-oriented first, not stack-oriented first.

### 2.2 Correlation-first

The primary value is understanding how CPU behavior drives GPU execution.

That means launch attribution, queueing delays, runtime behavior, and device execution must join into one correlated model instead of being emitted as unrelated metric streams.

### 2.3 Continuous-first

The design should favor:

- low overhead
- production safety
- always-on viability

### 2.4 Vendor-backed instrumentation

Real full-stack GPU profiling requires vendor-backed device signals.

Representative backend families are:

- NVIDIA via CUPTI
- AMD via ROCprofiler-SDK
- Intel via Level Zero and related runtime/driver hooks

### 2.5 Canonical representation

The canonical internal representation is a normalized event stream.

All downstream outputs, including `pprof`, flame graphs, summaries, and timeline views, are derived projections.

### 2.6 Linux-first base layer

The first serious implementation should be Linux-first and DRM-aware.

That means the base product should prioritize:

- syscall and `ioctl` timing
- scheduler interaction
- context and queue lifecycle where exposed
- process and thread ownership
- runtime and driver boundary correlation

before promising universal access to vendor PMUs or deep device internals.

### 2.7 Honest nullability

“Vendor-agnostic” should mean stable semantics and graceful degradation.

If a device or stack cannot expose queue identity, power, context IDs, or per-kernel counters, the collector should emit those fields as unavailable with clear provenance rather than inventing approximate values.

## 3. Goals

At a product level, the design should support:

- a portable event-timeline profiler first
- GPU timelines covering kernels, memory copies, queues, and related execution intervals
- CPU to GPU correlation
- continuous profiling
- a unified output model

## 4. Non-goals

This design does not promise:

- an `eBPF-only` GPU profiler
- immediate vendor parity
- UI design in this spec
- forced `pprof` as the only output artifact

## 5. Problem

The current branch has strong CPU-side collection and profile emission:

- on-CPU profiling via `profile/` and `unwind/dwarfagent/`
- off-CPU profiling via `offcpu/` and `unwind/dwarfagent/`
- PMU metrics via `cpu/`
- pprof output with improving frame and mapping fidelity

That gives us good visibility into host CPU behavior, but no way to answer questions such as:

1. Which CPU launch stacks caused expensive GPU kernels?
2. Which GPU kernels were stalled, under-occupied, or starved?
3. Was GPU under-utilization caused by CPU launch latency, runtime serialization, queue starvation, or device-side bottlenecks?
4. Can one flame graph or timeline show CPU launch work and the GPU execution that followed?

The design tension is that GPU profiling has two layers:

- a **vendor-agnostic host layer**, where process identity, CPU stacks, scheduling, and launch-side attribution can be observed generically
- a **vendor-specific device layer**, where kernel execution, GPU PCs, stall reasons, counters, and runtime correlation live behind vendor APIs, drivers, and runtimes

An `eBPF-only` design cannot deliver serious GPU profiling on its own. It can provide host attribution and some driver-level observation, but not the vendor-only device signals needed for full-stack GPU flame graphs.

## 6. Goal

Build a GPU profiling architecture for `perf-agent` with these properties:

1. The first-level control plane is vendor-agnostic.
2. `eBPF` is the default host correlation plane.
3. Vendor-specific collection is loaded on demand through explicit backends.
4. The normalized event model can support both:
   - timeline correlation between CPU launches and GPU execution
   - future mixed CPU+GPU flame graphs when a backend can provide GPU sample attribution
5. Existing CPU/off-CPU/pprof work on this branch remains reusable rather than bypassed.
6. The system must be able to generate a flame graph shaped like the public AI Profiler examples: CPU launch stack, runtime/driver path, and GPU execution/sample context in one visual when backend capability allows it.

### 6.1 Non-functional requirements

The first design should explicitly optimize for the same properties that made CPU profiling broadly usable:

- **Near-zero overhead**
  - target an architecture that can plausibly stay below “always-on profiler” thresholds in production-like environments
  - avoid designs that require pervasive binary instrumentation in the steady state
- **No special relaunch path**
  - the ideal attach model is “profiler starts after workload is already running”
  - requiring users to rebuild or relaunch under a special wrapper should be treated as a fallback, not the default
- **Low setup burden**
  - avoid assumptions that developers have SSH access, control over start scripts, or the ability to modify deployment flows
- **Degradation by capability**
  - when a backend cannot provide GPU PC sampling or source attribution, the session should still provide useful launch and execution correlation
- **Truthful scope**
  - any backend-specific workaround should be isolated behind a capability boundary rather than leaked into the core API as though it were universal
- **Event stream as canonical truth**
  - raw normalized events should be the primary internal representation
  - pprof, summaries, and flame graphs should be derived projections
- **Subsampling support**
  - backends that emit high-volume GPU samples must be able to reduce collection volume in a controlled and explicit way
- **Single-active-workload-first viability**
  - the first serious implementation may target one active GPU workload per device/session before attempting broad multi-tenant attribution

## 7. Detailed Non-goals

- No promise of a universal `eBPF-only` GPU profiler.
- No requirement that all vendors expose the same fidelity on day one.
- No attempt in the first version to support every GPU runtime.
- No UI design in this spec; output is collector-side only.
- No commitment yet to whether mixed CPU+GPU output is emitted as:
  - pure pprof with conventions, or
  - pprof plus a sidecar event stream
- No kernel-driver reverse engineering as a baseline dependency.

## 8. Recommendation

Use a **layered architecture**:

- **Layer A: Core GPU profiling model**
  - vendor-agnostic event types, correlation IDs, capability negotiation, and output contract
- **Layer B: Linux-first observability core**
  - `eBPF` collection, DRM-aware syscall and `ioctl` timing, scheduler context, process/container metadata, optional driver tracepoints, and runtime-boundary correlation
- **Layer C: On-demand vendor backends**
  - NVIDIA via CUPTI
  - Intel via Level Zero and/or `iaprof`-style runtime/driver integration
  - AMD via ROCprofiler-SDK

This is the only architecture that is honest about current Linux GPU observability while still preserving a clean, mostly vendor-neutral product surface.

### 8.1 Meaning of “vendor-agnostic”

In this spec, “vendor-agnostic” applies to:

- the public backend contract
- the normalized event model
- the capability model
- the manager and projection pipeline

It does **not** require the collector internals to be identical across vendors.

So the design rule is:

- backend **contracts** must be vendor-agnostic
- backend **implementations** may be vendor-specific behind that contract

Reviewers should evaluate portability at the contract boundary, not by forcing NVIDIA, Intel, and AMD collection internals into one fake-uniform implementation strategy.

## 9. Why eBPF First, but Not eBPF Only?

`eBPF` should be first in the architecture, but not first as the only data source.

### 9.1 What eBPF does well

- capture CPU stacks at GPU API call sites or queue submission sites
- track PID/TID/cgroup/container/process identity
- correlate host scheduling and off-CPU delays around GPU submission
- ingest generic kernel events and some driver tracepoints
- provide a single always-on control plane across vendors

### 9.2 What eBPF does not solve

- GPU PC sampling
- stall reasons
- occupancy and issue-slot data
- runtime correlation objects that only vendor libraries expose
- per-kernel metrics that live behind CUPTI / Level Zero / ROCprofiler

### 9.3 Design implication

The architecture should treat eBPF as the **common attribution spine**, not as the complete GPU profiler.

### 9.4 Portable base scope

The base layer should normalize the parts of GPU observability that can be captured honestly across Linux stacks:

- device identity
  - PCI BDF, DRM node, driver name, and process ownership when available
- process and thread correlation
  - PID, TID, cgroup, `fd`, syscall path, runtime callsite, scheduler state
- context and queue lifecycle where exposed
  - create, destroy, submit, wait, signal, timeout, reset
- latency intervals
  - API-call latency, `ioctl` latency, submission-to-completion when timestamps exist
- memory boundary events
  - alloc, map, unmap, migrate, fault, and address-space association when exposed
- coarse operational samples
  - utilization, power, clocks, VRAM, or engine state only when a stack exposes them cleanly

The base layer should not promise portable access to:

- GPU PMU counters
- occupancy or issue-slot metrics
- warp, wavefront, or EU internals
- precise queue residency on proprietary stacks
- replay-based per-kernel profiling modes

Those belong to on-demand vendor adapters.

## 10. Proposed Scope Split

### 10.1 Vendor-agnostic core

The core owns:

- feature and capability negotiation
- session lifecycle
- normalized event schema
- timestamp normalization
- CPU stack capture and symbolization
- cross-source correlation
- profile and timeline assembly

### 10.2 Vendor-specific backends

Backends own:

- runtime interception required by that vendor stack
- device execution records
- GPU hardware counters
- GPU PC or stall samples if available
- device/stream/queue metadata
- vendor-specific correlation IDs

## 11. Capability Model

Backends should declare capabilities rather than pretending every backend is equivalent.

```go
type GPUCapability string

const (
    CapabilityLaunchTrace    GPUCapability = "launch-trace"
    CapabilityExecTimeline   GPUCapability = "exec-timeline"
    CapabilityDeviceCounters GPUCapability = "device-counters"
    CapabilityPCSampling     GPUCapability = "gpu-pc-sampling"
    CapabilityStallReasons   GPUCapability = "stall-reasons"
    CapabilitySourceMap      GPUCapability = "gpu-source-correlation"
)
```

Examples:

- a basic backend may only support `launch-trace`, `exec-timeline`, and `device-counters`
- a deeper backend may add `gpu-pc-sampling` and `stall-reasons`

This lets the product be vendor-agnostic at the contract level while remaining honest about backend-specific depth.

## 12. Normalized Data Model

The core event model should be explicit and append-only.

### 12.1 Session-level types

```go
type GPUBackendID string

type GPUDeviceRef struct {
    Backend  GPUBackendID
    DeviceID string
    Name     string
}

type GPUQueueRef struct {
    Backend GPUBackendID
    Device  GPUDeviceRef
    QueueID string
}
```

### 12.2 Correlation types

```go
type CorrelationID struct {
    Backend GPUBackendID
    Value   string
}

type LaunchContext struct {
    PID      uint32
    TID      uint32
    TimeNs   uint64
    CPUStack []pprof.Frame
    Tags     map[string]string
}
```

### 12.3 Normalized events

```go
type GPUEventKind uint8

const (
    GPUEventLaunch GPUEventKind = iota + 1
    GPUEventExec
    GPUEventCounter
    GPUEventSample
)

type GPUKernelLaunch struct {
    Correlation CorrelationID
    Queue       GPUQueueRef
    KernelName  string
    TimeNs      uint64
    Launch      LaunchContext
}

type GPUKernelExec struct {
    Correlation CorrelationID
    Queue       GPUQueueRef
    KernelName  string
    StartNs     uint64
    EndNs       uint64
}

type GPUCounterSample struct {
    Device   GPUDeviceRef
    TimeNs   uint64
    Name     string
    Value    float64
    Unit     string
}

type GPUSample struct {
    Correlation CorrelationID
    Device      GPUDeviceRef
    TimeNs      uint64
    KernelName  string

    // Optional, backend-dependent fields
    PC          uint64
    Function    string
    File        string
    Line        uint32
    StallReason string
    Weight      uint64
}
```

### 12.3.1 Base observability event families

The current branch already has useful canonical types for launch, execution, counters, and samples.

That is enough for replay, projection, and early backend work, but it is too narrow for the Linux-first observability core. The base layer also needs lifecycle and boundary telemetry such as:

- runtime/API enter and exit
- syscall and `ioctl` enter and exit
- submit and wait intervals
- context and queue create and destroy
- memory map, unmap, migrate, and fault
- reset, timeout, and lost-device signals

So the event model should evolve to support two layers:

1. high-level execution records such as `GPUKernelLaunch`, `GPUKernelExec`, `GPUCounterSample`, and `GPUSample`
2. lower-level timeline events for lifecycle and boundary observation

That lower-level layer may be represented as:

- additional typed structs, or
- a generic normalized timeline event envelope

The key requirement is that the Linux-first base can emit truthful lifecycle telemetry without pretending every event is already a kernel launch or execution interval.

### 12.4 Execution identity and context requirements

The core model needs a stronger notion of GPU execution identity than just `(device, queue, kernel_name, time)`.

Backends should provide, when possible:

```go
type GPUExecutionRef struct {
    Backend   GPUBackendID
    DeviceID  string
    QueueID   string
    ContextID string
    ExecID    string
}
```

And normalized execution records should carry it:

```go
type GPUKernelExec struct {
    Execution   GPUExecutionRef
    Correlation CorrelationID
    Queue       GPUQueueRef
    KernelName  string
    StartNs     uint64
    EndNs       uint64
}
```

This requirement exists because some GPU sampling surfaces are not globally unique by virtual address alone. If a backend cannot reliably distinguish execution contexts, the core must treat that as a reduced-fidelity mode rather than silently over-joining unrelated samples.

## 13. Correlation Model

The main semantic unit is:

`CPU launch stack -> backend correlation ID -> GPU kernel execution -> optional GPU samples/counters`

### 13.1 Cross-layer correlation stack

Conceptually the profiler is joining evidence across layers:

```text
CPU application stack
-> runtime/API launch site
-> user-mode driver / runtime correlation object
-> kernel driver / queue submission
-> GPU execution interval
-> optional GPU hardware sample or stall
```

The design should preserve those boundaries in the raw event stream even if later projections collapse them into one flame graph or summary.

This gives three useful output levels:

1. **Launch timeline**
   - “this CPU stack launched this GPU kernel”
2. **Execution correlation**
   - “this launched kernel actually ran here on device/queue X”
3. **Full-stack attribution**
   - “this GPU sample or stall belongs to that execution, which came from this CPU stack”

If a backend cannot provide GPU samples, the first two levels still work.

### 13.2 Join policy

Correlation should be performed from strongest to weakest evidence:

1. explicit backend correlation ID
2. explicit execution/context identity
3. queue/device identity plus bounded time window
4. heuristic fallback

Heuristic joins must be marked as such in the normalized stream or debug output. The core should not present a guessed join as equivalent to a hard runtime-provided correlation.

## 14. Architecture in This Repo

The current code already suggests useful boundaries.

### 14.1 Reusable parts

- [perfagent/agent.go](/home/diego/github/perf-agent/perfagent/agent.go:1)
  - central lifecycle and feature dispatch
- [unwind/dwarfagent/common.go](/home/diego/github/perf-agent/unwind/dwarfagent/common.go:1)
  - strong pattern for raw-event ingestion, aggregation, symbolization, and profile assembly
- [pprof/pprof.go](/home/diego/github/perf-agent/pprof/pprof.go:1)
  - profile builder and frame model
- [metrics/types.go](/home/diego/github/perf-agent/metrics/types.go:1)
  - precedent for non-pprof snapshots and exporters

### 14.2 Recommended repo organization

```text
cpu/
  ...                 # existing CPU profiling and PMU paths

gpu/
  types.go            # canonical GPU event model and capabilities
  manager.go          # backend lifecycle and fan-in
  timeline.go         # event assembly and correlation
  exporter.go         # JSON/raw snapshot export
  pprof_projection.go # synthetic-frame pprof projection

  backend/
    replay/
      replay.go
    stream/
      stream.go
    nvidia/
      cupti_collector.go
    amd/
      rocprofiler_collector.go
    intel/
      levelzero_collector.go

  correlation/
    pid_tid.go
    stream_context.go
    clock_sync.go

  codec/
    ndjson.go

pprof/
  ...                 # existing shared pprof builder/projection support

export/
  perfetto/
  otlp/
```

This layout keeps ownership clear:

- canonical GPU model and projections stay in `gpu/`
- vendor implementations live under `gpu/backend/`
- stream/file codecs stay under `gpu/codec/`
- correlation helpers only split out once they justify dedicated packages
- `pprof` remains a shared repo-level concern rather than a GPU-only exporter package

The design should avoid generic top-level packages such as `model/` when the ownership is really GPU-specific. Putting the canonical event schema in `gpu/types.go` keeps the contract close to the subsystem that owns it and reduces the chance of vague cross-package dependencies.

### 14.3 Backend interface

```go
type Backend interface {
    ID() GPUBackendID
    Capabilities() []GPUCapability
    Start(ctx context.Context, sink EventSink) error
    Stop(ctx context.Context) error
    Close() error
}

type EventSink interface {
    EmitLaunch(GPUKernelLaunch)
    EmitExec(GPUKernelExec)
    EmitCounter(GPUCounterSample)
    EmitSample(GPUSample)
}
```

### 14.4 Go API design principles

The GPU packages should follow the same broad design discipline as the rest of this repo:

- keep interfaces narrow and behavioral
- keep ownership of concurrency at the manager/session boundary
- keep normalized event types in one package so backends do not redefine the contract
- prefer explicit capability checks over type assertions against concrete backends
- keep the raw event stream append-only until projection time

Concretely:

- `gpu/types.go` should own the canonical exported event structs
- `gpu/types.go` should also own the narrow backend and event-sink contracts, so implementation packages can depend on `gpu` without creating an import cycle through a separate contract package
- backend packages should translate vendor-native records into core types as early as possible
- `gpu/manager` should own lifecycle, fan-in, and projection wiring

Candidate manager shape:

```go
type Manager struct {
    backends []Backend
    sink     EventSink
}

func NewManager(backends []Backend, sink EventSink) *Manager
func (m *Manager) Start(ctx context.Context) error
func (m *Manager) Stop(ctx context.Context) error
func (m *Manager) Close() error
```

This keeps the public surface simple while allowing the implementation to evolve internally.

### 14.5 Modern Go implementation notes

When implementation starts, the GPU packages should prefer modern Go 1.26-era standard library patterns over custom helpers or legacy idioms.

Use these defaults unless repo-local constraints argue otherwise:

- `slices`, `maps`, and `cmp` for common collection operations instead of bespoke loops where readability improves
- `errors.Is` and `errors.Join` for error inspection and aggregation
- `context.WithCancelCause`, `context.WithTimeoutCause`, and `context.Cause` when manager or backend shutdown needs to preserve failure reason
- `sync.OnceFunc` or `sync.OnceValue` for lazy backend/global initialization instead of open-coded `sync.Once` wrappers
- `atomic.Bool`, `atomic.Int64`, and `atomic.Pointer[T]` for shared backend state rather than untyped atomic patterns
- `clear`, `min`, and `max` where they simplify intent
- `slices.Clone` and `maps.Clone` when raw event snapshots need defensive copies

Avoid:

- custom slice or map utility helpers when stdlib packages already cover the operation
- lossy cancellation where the cause of shutdown matters for backend diagnosis
- broad interfaces that exist only to make testing easier

For tests:

- use `t.Context()` when test code needs a context
- prefer table-driven tests around normalized event joins and projection rules
- keep backend-specific fixtures at the package boundary so core tests do not depend on vendor SDKs

## 15. Linux Observability and Host Correlation Plane

The host plane should be independent of any single vendor backend.

### 15.1 Responsibilities

- observe target processes and threads
- capture CPU launch stacks
- capture host timing around runtime calls
- attach process metadata and existing tags
- feed correlation records into the GPU manager

### 15.2 Collection options

Ordered by realism:

1. syscall and `ioctl` tracepoints plus scheduler correlation
2. open-driver tracepoints or BTF-backed hooks where available
3. `uprobes` on runtime entry points when kernel-boundary telemetry is not enough
4. runtime-specific callbacks if a backend already exposes stronger correlation IDs

For the first version, the host plane should prefer the Linux/DRM boundary first and add runtime-specific probes only when they materially improve correlation. “Vendor-agnostic core” does not mean “zero vendor knowledge,” but it also does not mean the first milestone should start at proprietary userspace entry points.

## 16. Output Model

We should explicitly separate **profile output** from **timeline output**.

### 16.1 Timeline output

The first output should be a normalized event stream that can drive:

- CLI summaries
- JSON export
- future UI integrations
- offline debugging and replay

This is safer than forcing everything into pprof immediately.

### 16.2 pprof output

pprof remains useful for mixed-stack aggregation, but it should be treated as a derived view.

Two likely pprof products:

1. **CPU-launch profile**
   - aggregate by CPU launch stack and weight by GPU time, sample count, or stall weight
2. **Mixed full-stack profile**
   - CPU launch frames followed by synthetic delimiter frames and then GPU frames

Example synthetic stack:

```text
app::train_step
cudaLaunchKernel
[gpu-launch]
my_kernel
[gpu]smsp__pcsamp_warp_stall
```

This is intentionally provisional. The normalized event stream is the primary truth; pprof is a downstream projection.

### 16.2.1 Flame graph requirement

The architecture is not complete unless it can project the normalized stream into a flame graph resembling the public AI Profiler examples:

- process / program identity
- CPU user stack
- optional CPU kernel stack
- runtime / API launch frame(s)
- optional driver / submission delimiter frames
- GPU kernel identity
- optional GPU function / source context
- optional GPU instruction, stall, or sample reason

That output may be implemented as:

1. pprof with agreed synthetic frame conventions
2. folded-stack text for `flamegraph.pl`
3. both

But the requirement is the same: one visual stack that answers “which CPU code caused which GPU work, and why was that GPU work expensive?”

### 16.2.2 Candidate: `pprof` with synthetic GPU frames

One likely projection is to reuse the existing pprof pipeline and encode GPU-side context as synthetic frames appended after the real CPU launch stack.

Example shape:

```text
app::train_step
model::forward
cudaLaunchKernel
[gpu:launch]
[gpu:device:0]
[gpu:queue:7]
[gpu:kernel:flash_attn_fwd]
[gpu:stall:memory_throttle]
[gpu:pc:0x1a40]
```

Why this is attractive:

- it reuses the existing pprof builder and profile-writing path
- it preserves the real CPU stack at the top of the flame graph
- it can produce a mixed CPU+GPU visual without requiring a custom profile format first

Why it is still an open question:

- synthetic GPU frames are a convention, not a native pprof concept
- naming stability matters because it directly affects aggregation behavior
- some projections may still need folded-stack export for compatibility with existing flame graph tooling

If this route is chosen, the ordering rule should be:

1. real CPU user frames
2. optional CPU runtime or kernel frames
3. one or more GPU boundary markers
4. GPU execution identity
5. optional GPU function/source/stall/PC detail

And the naming rule should be explicit and machine-stable, for example:

```text
[gpu:launch]
[gpu:device:<id>]
[gpu:queue:<id>]
[gpu:kernel:<name>]
[gpu:function:<name>]
[gpu:stall:<reason>]
[gpu:pc:<hex>]
```

This section is intentionally a candidate design, not a final commitment.

### 16.3 Symbolization contract

Full-stack flame graphs eventually depend on more than collection:

- GPU kernels need stable symbol names
- source file and line mappings need to be obtainable with low overhead
- inlined call information needs to survive compiler lowering where feasible
- the backend needs enough module/context identity to disambiguate reused virtual addresses

So the core architecture should assume three symbolization tiers:

1. **Tier 1**
   - kernel name only
2. **Tier 2**
   - kernel name plus source file and line
3. **Tier 3**
   - source-level GPU call stack with inlining support

The first implementation should be successful even if the first backend initially only reaches Tier 1 or Tier 2.

## 17. Backend Strategy

### 17.1 `iaprof` lessons

`iaprof` is interesting here less as a reusable implementation and more as a proof of shape:

- it validates the target join model:
  - GPU hardware sample or stall
  - GPU kernel execution context
  - CPU launch stack
- it demonstrates that a useful first system can center on one active workload rather than full multi-tenant continuous profiling
- it uses a raw event stream as the primary emitted artifact, with downstream tooling responsible for flame graphs and heatmaps
- it exposes explicit subsampling controls for high-volume GPU sampling paths
- it is also a warning that real full-stack GPU profiling currently depends on vendor-specific runtime, driver, and hardware integration

This spec adopts the architectural lessons without inheriting Intel-specific assumptions as universal design constraints.

### 17.1.1 Linux-first base before vendor depth

Before the first vendor-deep backend, the product should have a credible Linux-first observability core:

- `eBPF` and ringbuf-based event collection
- DRM-aware syscall and `ioctl` timing
- scheduler and wait-path correlation
- open-driver validation on the most transparent stacks available

That base is the default operating mode.

Vendor SDKs, PMUs, and replay-style metrics are follow-on capability layers, not the starting point of the product.

### 17.2 NVIDIA

Expected source surfaces:

- CUPTI Activity API for launches and execution records
- CUPTI Callback API for runtime attribution
- CUPTI PC sampling for deeper GPU-side attribution when available

Likely maturity:

- strongest short-term path for launch + execution correlation
- deeper GPU flame graphs possible, but backend complexity is high

### 17.3 Intel

Expected source surfaces:

- Level Zero tracing and metrics
- optional `iaprof`-style runtime and eBPF integration patterns

Likely maturity:

- attractive because some of the public full-stack design is visible
- likely less broadly deployable than NVIDIA in many environments

### 17.4 AMD

Expected source surfaces:

- ROCprofiler-SDK tracing, counters, and sampling where supported

Likely maturity:

- viable long-term backend
- not the recommended first implementation unless AMD is the target environment

## 18. Phasing

### Phase 0: draft and architecture validation

- settle the normalized model
- settle the backend capability model
- decide first backend
- decide timeline export format
- decide whether the first target is explicitly “single active workload” rather than general system-wide GPU attribution

### Phase 1: vendor-agnostic core + Linux-first observability core

Success criteria:

- capture DRM-aware syscall and `ioctl` lifecycle events
- capture scheduler and wait-path correlation around GPU activity
- capture process, thread, and device identity with explicit provenance
- export normalized timeline
- validate the normalized base on at least one open-driver stack
- work reliably for a single active GPU workload even if broader multi-process fairness and attribution are deferred

This phase does **not** require GPU PC sampling, vendor PMUs, or proprietary runtime callbacks yet.

### Phase 2: host launch attribution and coarse metrics

Success criteria:

- attach CPU launch stacks or runtime-boundary attribution to the same timeline when available
- attach device utilization / memory / power / engine metrics to the same timeline
- correlate starvation vs saturation

### Phase 3: backend execution correlation

Success criteria:

- ingest vendor or open-driver execution intervals
- join them to the base timeline and host launch attribution
- provide one CPU-launch-weighted `pprof` projection with synthetic GPU frames where the data is strong enough

### Phase 4: full-stack GPU sample attribution

Success criteria:

- ingest backend GPU samples
- join them to executions and launch stacks
- emit mixed CPU+GPU flame graph data
- produce at least one folded or pprof-derived flame graph artifact comparable in shape to the PDF examples
- expose an explicit backend or session-level subsampling control when raw sample volume is too high for practical continuous collection

## 19. Risks

### 19.1 Timebase alignment

Different sources report time differently. CPU `ktime`, runtime timestamps, and device timestamps may need calibration and drift handling.

### 19.2 Runtime interception fragility

Hooking launch APIs may vary by runtime version, loader behavior, and static vs dynamic linking.

### 19.3 Incomplete backend parity

The capability model helps, but users may still expect identical behavior across vendors.

### 19.4 pprof mismatch

pprof is CPU-centric. Mixed CPU+GPU representations may need conventions that are useful but not “standard.”

### 19.5 Over-aggregation

If we aggregate too early, we can lose the exact correlation chain needed for debugging. Raw normalized events should be retained until projection time.

### 19.6 GPU context ambiguity

Some backends may expose GPU PCs or virtual addresses without a sufficiently strong execution or context identifier. In those cases, the same address may be reused across processes or contexts, making naive joins incorrect.

The architecture must support:

- explicit “ambiguous sample” accounting
- backend-specific software workarounds where available
- clear degradation to launch/execution correlation when sample-to-context joins are not trustworthy

### 19.7 Proof-of-concept brittleness

The public Intel AI profiler material is useful because it proves the model is possible, but it also shows that early full-stack GPU profiling is brittle. Driver/runtime/compiler cooperation may be required for robustness, symbolization, and context identity.

This means:

- the core should expect backend-specific exceptions and escape hatches
- the first shipped backend should be narrow and honest
- the design should avoid hard-coding one vendor's assumptions into the vendor-agnostic event model

## 20. Initial Recommendation

Implement the architecture as:

1. vendor-agnostic normalized core
2. Linux-first `eBPF` observability core
3. host launch attribution after the base timeline is trustworthy
4. one vendor or open-driver execution backend that provides launch and execution correlation
5. pprof as a derived output, not the canonical raw format

This gives us a serious path to Option 2 without claiming a false vendor-neutral device sampler.

## 21. Open Questions / Decision Prompts

These are the main review decisions the draft still needs. The current branch now has a working default for each so planning can proceed.

### 21.1 First backend

Question:
- Which backend should be first: NVIDIA, Intel, or a narrower internal target?

Current recommendation:
- prefer the backend that matches the first real deployment environment

Working default:
- keep the public contract vendor-agnostic
- defer naming a single first vendor implementation in the spec until the first real target environment is chosen

### 21.2 First host correlation mechanism

Question:
- Should the first real host-side source start at the Linux/DRM boundary, or start directly with runtime `uprobes`/callbacks?

Current recommendation:
- start with syscall, `ioctl`, scheduler, and open-driver telemetry
- add runtime probes only when the Linux-first base is insufficient for useful correlation

Working default:
- start with the Linux-first observability core
- treat `uprobes` and callbacks as follow-on correlation sources, not the first milestone

### 21.3 Raw timeline export format

Question:
- Should raw timeline export be JSON first, protobuf first, or internal-only at the start?

Current recommendation:
- start with JSON if early inspection and debugging matter more than schema rigidity
- move to protobuf only if volume, compatibility, or external ingestion pressure justifies it

Working default:
- JSON first

### 21.4 Mixed stack representation

Question:
- Should mixed CPU+GPU stacks live in the existing `pprof.Frame` model, or in a new GPU-aware profile model with a pprof exporter on top?

Current recommendation:
- keep the normalized event stream as the source of truth
- defer a new profile model unless the pprof projection becomes too lossy

Working default:
- keep the normalized event stream as the source of truth
- use the existing `pprof.Frame`-based projection first

### 21.5 Initial scope width

Question:
- Do we want Phase 1 to target a single process, or system-wide multi-process GPU attribution from the start?

Current recommendation:
- optimize Phase 1 for one active workload first
- treat broader system-wide fairness and attribution as follow-on work

Working default:
- single active workload first

### 21.6 Subsampling contract

Question:
- Should subsampling be a universal backend capability in the core API, or an optional backend-specific control surfaced only when needed?

Current recommendation:
- define subsampling as an optional but standardizable backend capability
- avoid requiring every backend to implement it on day one

Working default:
- subsampling is part of the backend capability contract
- individual backends may omit it initially

### 21.7 Canonical flame-graph artifact

Question:
- Should the canonical flame-graph artifact be `pprof` with synthetic GPU frames, or should folded-stack export be first-class from the start as well?

Current recommendation:
- lean toward `pprof` with synthetic GPU frames as the first-class artifact
- keep folded-stack export available as a compatibility or debugging output if implementation cost stays low

Working default:
- `pprof` with synthetic GPU frames is the primary flame-graph artifact
- folded-stack export remains optional

## 22. Test Plan

### 22.1 Unit

- normalized event schema validation
- correlation join logic
- timestamp conversion helpers
- backend capability negotiation
- synthetic GPU frame naming and ordering rules
- event snapshot copy semantics and zero-value behavior

### 22.2 Integration

- known GPU workload launches kernel A from CPU stack X
- exported timeline links launch X to execution A
- optional counters appear on the same device/time axis
- ambiguous or heuristic joins are surfaced distinctly from hard joins
- pprof projection preserves CPU launch frames and appends synthetic GPU frames in the expected order

### 22.3 End-to-end

- run workload under `perf-agent`
- collect CPU profile, GPU timeline, and any backend counters
- verify the system can explain:
  - which CPU code launched GPU work
  - when the GPU ran
  - whether the GPU was idle, saturated, or stalled
  - whether the reported correlations are hard IDs or heuristic joins

## 23. Review Focus

This draft should be reviewed for:

- whether the core/backend split is the right abstraction boundary
- whether the normalized event model is sufficient
- whether pprof should remain central or become secondary for GPU work
- whether the first backend choice changes the shape of the core too much
- whether execution/context identity is modeled strongly enough to support trustworthy GPU sample joins
