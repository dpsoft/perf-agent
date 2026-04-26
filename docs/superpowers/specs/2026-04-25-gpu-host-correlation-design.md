# GPU Host Correlation Plane — Draft Design Spec

**Status:** draft for review and extension.  
**Branch context:** `gpu-profiling-spec`  
**Predecessor work:** `2026-04-25-gpu-profiling-design.md`, `2026-04-25-gpu-live-event-source-design.md`, and the replay/live-stream GPU core already implemented on this branch.  
**Goal of this draft:** define the first real host-side correlation path so `perf-agent` can attribute GPU executions back to CPU launch behavior without forcing a premature commitment to one vendor-only collection flow.

## 1. Overview

The branch now has:

- a vendor-agnostic GPU event model
- timeline correlation
- JSON snapshot export
- synthetic-frame `pprof` projection
- replay and live NDJSON ingestion backends

What it still lacks is a real path to capture CPU launch attribution from live workloads.

This spec defines the host correlation plane that sits between:

- CPU-side launch activity on the host
- vendor-specific GPU execution backends
- the existing normalized `gpu` event model

## 2. Problem

Without host correlation, the current system can ingest GPU events, but it cannot reliably answer:

1. which CPU stack launched a given GPU kernel
2. whether launch latency or runtime serialization starved the GPU
3. which process/thread/container initiated the work
4. how vendor runtime correlation IDs map back to CPU behavior

This is the missing layer between “GPU events exist” and “full-stack GPU profiling is useful.”

## 3. Design Principles

### 3.1 Correlation-first

The host plane exists to preserve causal links:

`CPU thread -> launch site -> runtime correlation object -> GPU execution`

It is not just a metadata side channel.

### 3.2 Attach-late friendly

The ideal mode is that `perf-agent` can attach after the workload is already running.

That pushes the design toward host-side probes and away from solutions that only work when the process starts under a wrapper.

### 3.3 Vendor-agnostic contract, vendor-aware sources

The public host-correlation contract should be vendor-agnostic.

The actual source of host launch records may still be vendor-aware:

- `uprobes` on CUDA / HIP / Level Zero runtime entry points
- runtime callbacks exposed by vendor SDKs
- mixed paths that combine both

### 3.4 Canonical GPU events remain the manager boundary

The `gpu.Manager` and `gpu.EventSink` should keep operating on canonical GPU events.

If the host plane needs intermediate partial records, that should stay inside the host-correlation implementation rather than leak a second public event model into the core.

## 4. Goals

The first host correlation design should support:

- CPU launch stack capture
- PID/TID/process/tag attribution
- runtime correlation ID capture when available
- conversion into canonical `gpu.GPUKernelLaunch` events
- compatibility with later NVIDIA / AMD / Intel execution backends

## 5. Non-goals

This phase does not try to:

- solve GPU device-side sampling
- replace vendor runtime callbacks when they are clearly stronger than probes
- guarantee one universal probe list across all runtimes
- fully solve container/cgroup attribution edge cases up front
- redesign the existing `gpu` canonical model

## 6. Approaches Considered

### 6.1 Runtime-callback first

Use vendor runtime callbacks as the only host launch source.

Pros:

- strongest correlation IDs
- runtime already knows launch semantics
- lowest ambiguity for kernel name / stream / context metadata

Cons:

- often not attach-late friendly
- pushes the first usable path behind vendor SDK integration
- makes the host plane look vendor-specific too early

### 6.2 Uprobe-first

Use `uprobes` on runtime entry points as the only host source.

Pros:

- attach-late friendly
- matches the repo’s existing eBPF-oriented collection style
- works as an always-on host-side control plane

Cons:

- symbol/version fragility
- weaker metadata depending on runtime ABI
- some runtimes may not expose enough correlation context via probed arguments alone

### 6.3 Hybrid host plane

Define one host-source contract, then allow both:

- `uprobes` for attach-late host attribution
- runtime callbacks where a vendor backend can provide stronger correlation IDs

This is the recommended approach.

It keeps the core contract stable while letting the first real deployment use the strongest available source.

## 7. Recommendation

Build a hybrid host correlation plane with these rules:

1. the host plane has a vendor-agnostic source contract
2. source implementations may be `uprobes`, runtime callbacks, or mixed
3. the host plane emits canonical `gpu.GPUKernelLaunch` records into the existing manager boundary
4. attach-late capability is a design goal, but not every first backend must achieve it immediately

## 8. Proposed Architecture

### 8.1 Layers

- `gpu/host/`
  - host-source contract
  - normalization of source records into canonical launch events
  - PID/TID/tag/process metadata capture
  - optional clock normalization helpers
- `gpu/backend/<vendor>/`
  - device execution records
  - optional runtime callback adapters when the backend exposes stronger host-side correlation
- `gpu/`
  - unchanged canonical manager, timeline, exporter, and `pprof` projection

### 8.2 Manager boundary

The manager should continue receiving:

- `GPUKernelLaunch`
- `GPUKernelExec`
- `GPUCounterSample`
- `GPUSample`

That means the host plane is responsible for converting whatever it captured into a real `GPUKernelLaunch` before handing it to the manager.

## 9. Source Contract

The public contract should describe “host launch sources,” not “eBPF collectors only.”

Candidate shape:

```go
type HostSource interface {
	ID() string
	Start(ctx context.Context, sink HostSink) error
	Stop(ctx context.Context) error
	Close() error
}

type HostSink interface {
	EmitLaunchRecord(LaunchRecord)
}
```

The host plane may then normalize `LaunchRecord` into `gpu.GPUKernelLaunch`.

This keeps partial or source-specific state out of `gpu.Manager`.

## 10. Internal Launch Record

The host plane will likely need a source-facing record that is not yet the public canonical GPU event.

Candidate internal type:

```go
type LaunchRecord struct {
	Backend       gpu.GPUBackendID
	PID           uint32
	TID           uint32
	TimeNs        uint64
	CPUStack      []pp.Frame
	KernelName    string
	QueueID       string
	ContextID     string
	CorrelationID string
	Tags          map[string]string
	Source        string
}
```

Why keep this internal:

- some sources may only know a subset of fields at first
- some sources may need to merge runtime callback metadata with host probe metadata
- the public manager API should not be widened unless we are forced to widen it

## 11. Correlation Flow

The intended flow is:

1. host source observes launch site
2. host source captures CPU stack, PID/TID, and host timestamp
3. host source captures runtime correlation metadata if available
4. host plane normalizes into `gpu.GPUKernelLaunch`
5. vendor backend emits `gpu.GPUKernelExec`
6. existing timeline joins launch and execution by correlation ID first, then weaker evidence if needed

## 12. Attach Model

### 12.1 First-class attach modes

The design should explicitly allow:

- attach-late host probes via `uprobes`
- start-under-wrapper or callback mode where a vendor runtime requires it

### 12.2 Practical rule

The first real backend should use the strongest available host correlation source, even if that is callback-based.

But the architecture should not assume callbacks are the only future source, because that would undermine always-on and attach-late goals.

## 13. Failure and Fidelity Model

Host correlation should degrade clearly rather than silently:

- if CPU stack capture fails, still emit launch metadata with an explicit missing-stack state
- if correlation ID is missing, mark the launch as reduced fidelity
- if queue/context fields are inferred heuristically, preserve that fact in debug/raw output

The main rule is that guessed attribution must not look equivalent to hard runtime-provided correlation.

## 14. Recommended Repo Shape

The next split should be small and ownership-focused:

```text
gpu/
  host/
    source.go        # host-source contract
    launch.go        # internal launch record + normalization helpers
    process.go       # pid/tid/tag/process metadata helpers
    clock.go         # timestamp normalization if needed

    uprobe/
      collector.go   # host-side eBPF/uprobes collector
```

Important boundary rule:

- runtime callback adapters that are tightly tied to a vendor backend can live under `gpu/backend/<vendor>/`
- vendor-neutral host-source contracts and normalization should live under `gpu/host/`

## 15. Phasing

### Phase 1

- write the host-plane spec and plan
- keep the canonical `gpu` manager boundary unchanged

### Phase 2

- implement `gpu/host/` source contract
- add a deterministic host-launch replay fixture path if useful for tests

### Phase 3

- implement the first real host source
- whichever source gives the strongest correlation for the first target backend

### Phase 4

- join host-launch records with a real vendor execution backend
- validate mixed CPU+GPU output end to end

## 16. Open Questions

1. Should `LaunchRecord` remain purely internal, or should it become a shared public type if more than one subsystem needs it?
2. Should the first real host source be `uprobes`, vendor callbacks, or a mixed source for the chosen backend?
3. Do we want host correlation to live under `gpu/host/`, or should the first implementation stay inside `gpu/backend/<vendor>/` until a second source exists?
4. Do we need explicit raw/debug export for host-launch records before they become canonical `GPUKernelLaunch` events?
