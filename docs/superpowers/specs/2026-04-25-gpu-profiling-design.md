# GPU Profiling — Draft Design Spec

**Status:** draft for review and extension.
**Branch context:** `feat/dwarf-unwinding`
**Predecessor work:** S3-S9 DWARF unwinding and pprof fidelity improvements.
**Goal of this draft:** define a first architecture for full-stack GPU profiling that fits the current perf-agent collector model without pretending that GPU hardware profiling can be implemented with eBPF alone.

## 1. Problem

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

## 2. Goal

Build a GPU profiling architecture for `perf-agent` with these properties:

1. The first-level control plane is vendor-agnostic.
2. `eBPF` is the default host correlation plane.
3. Vendor-specific collection is loaded on demand through explicit backends.
4. The normalized event model can support both:
   - timeline correlation between CPU launches and GPU execution
   - future mixed CPU+GPU flame graphs when a backend can provide GPU sample attribution
5. Existing CPU/off-CPU/pprof work on this branch remains reusable rather than bypassed.
6. The system must be able to generate a flame graph shaped like the public AI Profiler examples: CPU launch stack, runtime/driver path, and GPU execution/sample context in one visual when backend capability allows it.

### 2.1 Non-functional requirements

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

## 3. Non-goals

- No promise of a universal `eBPF-only` GPU profiler.
- No requirement that all vendors expose the same fidelity on day one.
- No attempt in the first version to support every GPU runtime.
- No UI design in this spec; output is collector-side only.
- No commitment yet to whether mixed CPU+GPU output is emitted as:
  - pure pprof with conventions, or
  - pprof plus a sidecar event stream
- No kernel-driver reverse engineering as a baseline dependency.

## 4. Recommendation

Use a **layered architecture**:

- **Layer A: Core GPU profiling model**
  - vendor-agnostic event types, correlation IDs, capability negotiation, and output contract
- **Layer B: eBPF host correlation plane**
  - CPU stacks, process/container metadata, scheduler context, optional driver/runtime tracepoints, and launch-site attribution
- **Layer C: On-demand vendor backends**
  - NVIDIA via CUPTI
  - Intel via Level Zero and/or `iaprof`-style runtime/driver integration
  - AMD via ROCprofiler-SDK

This is the only architecture that is honest about current Linux GPU observability while still preserving a clean, mostly vendor-neutral product surface.

### 4.1 Meaning of “vendor-agnostic”

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

## 5. Why Not eBPF First Alone?

`eBPF` should be first in the architecture, but not first as the only data source.

### 5.1 What eBPF does well

- capture CPU stacks at GPU API call sites or queue submission sites
- track PID/TID/cgroup/container/process identity
- correlate host scheduling and off-CPU delays around GPU submission
- ingest generic kernel events and some driver tracepoints
- provide a single always-on control plane across vendors

### 5.2 What eBPF does not solve

- GPU PC sampling
- stall reasons
- occupancy and issue-slot data
- runtime correlation objects that only vendor libraries expose
- per-kernel metrics that live behind CUPTI / Level Zero / ROCprofiler

### 5.3 Design implication

The architecture should treat eBPF as the **common attribution spine**, not as the complete GPU profiler.

## 6. Proposed Scope Split

### 6.1 Vendor-agnostic core

The core owns:

- feature and capability negotiation
- session lifecycle
- normalized event schema
- timestamp normalization
- CPU stack capture and symbolization
- cross-source correlation
- profile and timeline assembly

### 6.2 Vendor-specific backends

Backends own:

- runtime interception required by that vendor stack
- device execution records
- GPU hardware counters
- GPU PC or stall samples if available
- device/stream/queue metadata
- vendor-specific correlation IDs

## 7. Capability Model

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

## 8. Normalized Data Model

The core event model should be explicit and append-only.

### 8.1 Session-level types

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

### 8.2 Correlation types

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

### 8.3 Normalized events

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

### 8.4 Execution identity and context requirements

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

## 9. Correlation Model

The main semantic unit is:

`CPU launch stack -> backend correlation ID -> GPU kernel execution -> optional GPU samples/counters`

### 9.1 Cross-layer correlation stack

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

### 9.2 Join policy

Correlation should be performed from strongest to weakest evidence:

1. explicit backend correlation ID
2. explicit execution/context identity
3. queue/device identity plus bounded time window
4. heuristic fallback

Heuristic joins must be marked as such in the normalized stream or debug output. The core should not present a guessed join as equivalent to a hard runtime-provided correlation.

## 10. Architecture in This Repo

The current code already suggests useful boundaries.

### 10.1 Reusable parts

- [perfagent/agent.go](/home/diego/github/perf-agent/perfagent/agent.go:1)
  - central lifecycle and feature dispatch
- [unwind/dwarfagent/common.go](/home/diego/github/perf-agent/unwind/dwarfagent/common.go:1)
  - strong pattern for raw-event ingestion, aggregation, symbolization, and profile assembly
- [pprof/pprof.go](/home/diego/github/perf-agent/pprof/pprof.go:1)
  - profile builder and frame model
- [metrics/types.go](/home/diego/github/perf-agent/metrics/types.go:1)
  - precedent for non-pprof snapshots and exporters

### 10.2 Proposed new packages

```text
gpu/
  manager.go          # backend selection, lifecycle, capability registry
  types.go            # normalized GPU event model and capabilities
  timeline.go         # event assembly and correlation
  exporter.go         # raw/timeline export interfaces

gpu/hostebpf/
  collector.go        # CPU-side launch attribution via eBPF/uprobes/tracepoints
  events.go           # host correlation events
gpu/backend/nvidia/
  cupti.go            # CUPTI-based tracing / sampling backend

gpu/backend/intel/
  levelzero.go        # Level Zero or iaprof-style backend

gpu/backend/amd/
  rocprofiler.go      # ROCprofiler-SDK backend
```

### 10.3 Backend interface

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

### 10.4 Go API design principles

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
    backends []backend.Backend
    sink     EventSink
}

func NewManager(backends []backend.Backend, sink EventSink) *Manager
func (m *Manager) Start(ctx context.Context) error
func (m *Manager) Stop(ctx context.Context) error
func (m *Manager) Close() error
```

This keeps the public surface simple while allowing the implementation to evolve internally.

### 10.5 Modern Go implementation notes

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

## 11. Host Correlation Plane

The host plane should be independent of any single vendor backend.

### 11.1 Responsibilities

- observe target processes and threads
- capture CPU launch stacks
- capture host timing around runtime calls
- attach process metadata and existing tags
- feed correlation records into the GPU manager

### 11.2 Collection options

Ordered by realism:

1. `uprobes` on vendor runtime entry points
2. runtime-specific callbacks if the backend already exposes them
3. kernel tracepoints / driver events where useful

For the first version, the host plane may need backend assistance to know which runtime functions to probe. That is acceptable. “Vendor-agnostic core” does not mean “zero vendor knowledge at the probe list.”

## 12. Output Model

We should explicitly separate **profile output** from **timeline output**.

### 12.1 Timeline output

The first output should be a normalized event stream that can drive:

- CLI summaries
- JSON export
- future UI integrations
- offline debugging and replay

This is safer than forcing everything into pprof immediately.

### 12.2 pprof output

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

### 12.2.1 Flame graph requirement

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

### 12.2.2 Candidate: `pprof` with synthetic GPU frames

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

### 12.3 Symbolization contract

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

## 13. Backend Strategy

### 13.0 `iaprof` lessons

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

### 13.1 NVIDIA

Expected source surfaces:

- CUPTI Activity API for launches and execution records
- CUPTI Callback API for runtime attribution
- CUPTI PC sampling for deeper GPU-side attribution when available

Likely maturity:

- strongest short-term path for launch + execution correlation
- deeper GPU flame graphs possible, but backend complexity is high

### 13.2 Intel

Expected source surfaces:

- Level Zero tracing and metrics
- optional `iaprof`-style runtime and eBPF integration patterns

Likely maturity:

- attractive because some of the public full-stack design is visible
- likely less broadly deployable than NVIDIA in many environments

### 13.3 AMD

Expected source surfaces:

- ROCprofiler-SDK tracing, counters, and sampling where supported

Likely maturity:

- viable long-term backend
- not the recommended first implementation unless AMD is the target environment

## 14. Phasing

### Phase 0: draft and architecture validation

- settle the normalized model
- settle the backend capability model
- decide first backend
- decide timeline export format
- decide whether the first target is explicitly “single active workload” rather than general system-wide GPU attribution

### Phase 1: vendor-agnostic core + one backend with launch/execution correlation

Success criteria:

- capture CPU launch stack
- capture corresponding GPU kernel execution interval
- export normalized timeline
- provide one CPU-launch-weighted pprof projection
- work reliably for a single active GPU workload even if broader multi-process fairness and attribution are deferred

This phase does **not** require GPU PC sampling yet.

### Phase 2: device counters

Success criteria:

- attach device utilization / memory / power / engine metrics to the same timeline
- correlate starvation vs saturation

### Phase 3: full-stack GPU sample attribution

Success criteria:

- ingest backend GPU samples
- join them to executions and launch stacks
- emit mixed CPU+GPU flame graph data
- produce at least one folded or pprof-derived flame graph artifact comparable in shape to the PDF examples
- expose an explicit backend or session-level subsampling control when raw sample volume is too high for practical continuous collection

## 15. Risks

### 15.1 Timebase alignment

Different sources report time differently. CPU `ktime`, runtime timestamps, and device timestamps may need calibration and drift handling.

### 15.2 Runtime interception fragility

Hooking launch APIs may vary by runtime version, loader behavior, and static vs dynamic linking.

### 15.3 Incomplete backend parity

The capability model helps, but users may still expect identical behavior across vendors.

### 15.4 pprof mismatch

pprof is CPU-centric. Mixed CPU+GPU representations may need conventions that are useful but not “standard.”

### 15.5 Over-aggregation

If we aggregate too early, we can lose the exact correlation chain needed for debugging. Raw normalized events should be retained until projection time.

### 15.6 GPU context ambiguity

Some backends may expose GPU PCs or virtual addresses without a sufficiently strong execution or context identifier. In those cases, the same address may be reused across processes or contexts, making naive joins incorrect.

The architecture must support:

- explicit “ambiguous sample” accounting
- backend-specific software workarounds where available
- clear degradation to launch/execution correlation when sample-to-context joins are not trustworthy

### 15.7 Proof-of-concept brittleness

The public Intel AI profiler material is useful because it proves the model is possible, but it also shows that early full-stack GPU profiling is brittle. Driver/runtime/compiler cooperation may be required for robustness, symbolization, and context identity.

This means:

- the core should expect backend-specific exceptions and escape hatches
- the first shipped backend should be narrow and honest
- the design should avoid hard-coding one vendor's assumptions into the vendor-agnostic event model

## 16. Initial Recommendation

Implement the architecture as:

1. vendor-agnostic normalized core
2. eBPF-centered host correlation plane
3. one vendor backend that provides launch and execution correlation first
4. pprof as a derived output, not the canonical raw format

This gives us a serious path to Option 2 without claiming a false vendor-neutral device sampler.

## 17. Open Questions / Decision Prompts

These are the main review decisions the draft still needs. The current branch now has a working default for each so planning can proceed.

### 17.1 First backend

Question:
- Which backend should be first: NVIDIA, Intel, or a narrower internal target?

Current recommendation:
- prefer the backend that matches the first real deployment environment

Working default:
- keep the public contract vendor-agnostic
- defer naming a single first vendor implementation in the spec until the first real target environment is chosen

### 17.2 First host correlation mechanism

Question:
- Should the first host correlation path rely on runtime callbacks only, or also add `uprobes` immediately?

Current recommendation:
- start with whichever source gives the strongest correlation IDs for the chosen backend
- add `uprobes` early if callbacks alone are not enough for attach-late workflows

Working default:
- start with the strongest correlation source available for the chosen backend
- add `uprobes` when callback-only collection is insufficient for attach-late workflows

### 17.3 Raw timeline export format

Question:
- Should raw timeline export be JSON first, protobuf first, or internal-only at the start?

Current recommendation:
- start with JSON if early inspection and debugging matter more than schema rigidity
- move to protobuf only if volume, compatibility, or external ingestion pressure justifies it

Working default:
- JSON first

### 17.4 Mixed stack representation

Question:
- Should mixed CPU+GPU stacks live in the existing `pprof.Frame` model, or in a new GPU-aware profile model with a pprof exporter on top?

Current recommendation:
- keep the normalized event stream as the source of truth
- defer a new profile model unless the pprof projection becomes too lossy

Working default:
- keep the normalized event stream as the source of truth
- use the existing `pprof.Frame`-based projection first

### 17.5 Initial scope width

Question:
- Do we want Phase 1 to target a single process, or system-wide multi-process GPU attribution from the start?

Current recommendation:
- optimize Phase 1 for one active workload first
- treat broader system-wide fairness and attribution as follow-on work

Working default:
- single active workload first

### 17.6 Subsampling contract

Question:
- Should subsampling be a universal backend capability in the core API, or an optional backend-specific control surfaced only when needed?

Current recommendation:
- define subsampling as an optional but standardizable backend capability
- avoid requiring every backend to implement it on day one

Working default:
- subsampling is part of the backend capability contract
- individual backends may omit it initially

### 17.7 Canonical flame-graph artifact

Question:
- Should the canonical flame-graph artifact be `pprof` with synthetic GPU frames, or should folded-stack export be first-class from the start as well?

Current recommendation:
- lean toward `pprof` with synthetic GPU frames as the first-class artifact
- keep folded-stack export available as a compatibility or debugging output if implementation cost stays low

Working default:
- `pprof` with synthetic GPU frames is the primary flame-graph artifact
- folded-stack export remains optional

## 18. Test Plan

### 18.1 Unit

- normalized event schema validation
- correlation join logic
- timestamp conversion helpers
- backend capability negotiation
- synthetic GPU frame naming and ordering rules
- event snapshot copy semantics and zero-value behavior

### 18.2 Integration

- known GPU workload launches kernel A from CPU stack X
- exported timeline links launch X to execution A
- optional counters appear on the same device/time axis
- ambiguous or heuristic joins are surfaced distinctly from hard joins
- pprof projection preserves CPU launch frames and appends synthetic GPU frames in the expected order

### 18.3 End-to-end

- run workload under `perf-agent`
- collect CPU profile, GPU timeline, and any backend counters
- verify the system can explain:
  - which CPU code launched GPU work
  - when the GPU ran
  - whether the GPU was idle, saturated, or stalled
  - whether the reported correlations are hard IDs or heuristic joins

## 19. Review Focus

This draft should be reviewed for:

- whether the core/backend split is the right abstraction boundary
- whether the normalized event model is sufficient
- whether pprof should remain central or become secondary for GPU work
- whether the first backend choice changes the shape of the core too much
- whether execution/context identity is modeled strongly enough to support trustworthy GPU sample joins
