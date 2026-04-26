# GPU Host Uprobes Source — Draft Design Spec

**Status:** draft for review and extension.  
**Branch context:** `gpu-profiling-spec`  
**Predecessor work:** `2026-04-25-gpu-host-correlation-design.md`, host replay correlation implementation on this branch, and the existing GPU core / stream backend.  
**Goal of this draft:** define the first real attach-late host launch source using `uprobes`, so `perf-agent` can capture live CPU launch attribution without requiring a start-under-wrapper runtime callback path.

## 1. Overview

The branch now has:

- canonical GPU event types in `gpu/`
- a live GPU NDJSON execution stream
- a host replay source that proves CPU launch stacks can flow into mixed CPU+GPU output

What it still lacks is a live host launch collector.

This spec defines the first real host source as a `gpu/host/uprobes` implementation that captures runtime entry-point launches and turns them into `host.LaunchRecord` values.

## 2. Problem

Replay proved the architecture, but it does not collect anything from a running workload.

Without a live host source, the branch still cannot:

1. attach to a process after it starts
2. capture real CPU launch stacks at runtime entry points
3. observe launch timing without a vendor SDK callback path
4. validate the host-correlation contract under real collection conditions

## 3. Why Uprobes Next

`uprobes` are the best next step because they align with the main design constraints:

- attach-late friendly
- production-oriented
- always-on plausible
- compatible with the repo’s existing eBPF collection style

They are not perfect:

- symbol and ABI drift are real
- they may expose weaker correlation metadata than vendor callbacks
- some runtimes may need backend assistance to map arguments into queue/context/correlation fields

But they are still the best first real host source because they validate the live attach model and the CPU-side collection path before vendor callback integration.

## 4. Goals

The first `gpu/host/uprobes` implementation should support:

- attaching to one or more runtime symbols in a target process
- capturing PID/TID and a host timestamp
- capturing a CPU stack at the launch site
- extracting enough launch metadata to produce a `host.LaunchRecord`
- feeding the existing host sink / launch normalization path

## 5. Non-goals

This phase does not try to:

- solve every vendor runtime at once
- guarantee one universal probe ABI across CUDA / HIP / Level Zero
- collect device-side execution or samples
- replace vendor callbacks for backends that can provide stronger runtime correlation
- solve system-wide multi-runtime probe discovery in the first pass

## 6. Recommended Scope

The first real `uprobes` source should be intentionally narrow:

1. one target process first
2. one runtime family first
3. a small probe set around launch entry points
4. enough metadata for useful launch attribution, even if queue/context fidelity is incomplete

The main success condition is:

- a live process launches GPU work
- `perf-agent` captures a CPU launch stack
- the launch record joins to a GPU execution path through the existing correlation model

## 7. Approaches Considered

### 7.1 Generic “probe everything” layer first

Build a runtime-agnostic probe resolver before collecting anything real.

Pros:

- clean abstraction on paper
- fewer backend-specific conditionals later

Cons:

- too much architecture before evidence
- likely wrong abstraction for vendor-specific ABIs
- delays the first real live source

### 7.2 One runtime, hard-coded probe list

Start with one runtime family and a minimal symbol list.

Pros:

- fastest real validation
- easiest way to learn what metadata is actually available

Cons:

- less reusable at first
- can accidentally leak one runtime’s assumptions into the general host source

### 7.3 Narrow runtime adapter behind a generic host contract

Keep the public `gpu/host/` contract stable, but allow `gpu/host/uprobes` to start with one runtime adapter internally.

This is the recommended approach.

It keeps the architecture honest without blocking the first live collector on premature generalization.

## 8. Proposed Architecture

### 8.1 Public boundary

The public boundary stays:

- `gpu/host/source.go`
- `host.LaunchRecord`
- `host.HostSink`

`gpu/host/uprobes` is only one implementation of that source contract.

### 8.2 Internal layout

Recommended shape:

```text
gpu/host/uprobes/
  collector.go      # source lifecycle and eBPF attach orchestration
  config.go         # target process / symbol config
  records.go        # userspace parsing into host.LaunchRecord
  runtime.go        # runtime adapter interface
  runtime_<vendor>.go
```

The runtime-specific parsing stays internal to `gpu/host/uprobes`, not in the public host package.

## 9. Runtime Adapter Model

The first `uprobes` collector should not pretend every runtime ABI is the same.

Candidate internal adapter shape:

```go
type runtimeAdapter interface {
	ID() string
	Symbols() []probeSpec
	DecodeLaunch(rawRecord) (host.LaunchRecord, error)
}
```

Where:

- `Symbols()` declares which entry points to attach
- `DecodeLaunch()` maps captured arguments and metadata into a normalized launch record

This keeps runtime-specific logic contained while preserving a stable source contract.

## 10. Probe Semantics

The first collector should target launch-oriented entry points only.

Examples of data to capture at probe time:

- PID / TID
- timestamp
- userspace stack ID or raw stack
- selected runtime arguments
- process tags already known to `perf-agent`

The collector should avoid broad API coverage initially. One reliable launch path is more valuable than a wide but noisy surface.

## 11. Stack Capture

The defining feature of this source is CPU launch attribution, so stack capture is not optional in the design.

The first implementation should:

- capture a userspace stack at probe time
- symbolize later in userspace using the branch’s existing symbolization path where possible

Design rule:

- kernel-side collection should capture enough raw stack identity to defer expensive symbol work into userspace

This matches the repo’s existing profiling architecture better than doing heavy symbol handling in the BPF path.

## 12. Output to Host Layer

The `uprobes` collector should emit `host.LaunchRecord`, not canonical `gpu.GPUKernelLaunch`.

That keeps:

- raw live probe output
- runtime-specific decoding
- host-layer normalization

as separate responsibilities.

The existing `host.NewLaunchSink(...)` bridge can then normalize and emit canonical launch events into `gpu.Manager`.

## 13. Failure and Fidelity

The first real source should be explicit about reduced-fidelity modes:

- missing stack: still emit launch metadata, but mark stack absent
- missing correlation ID: emit reduced-fidelity launch record only if the runtime adapter says heuristic join is acceptable
- decode failure: count and surface as source errors or debug stats, do not silently fabricate fields
- symbol not found / probe attach failure: fail the source clearly rather than partially pretending success

## 14. Attach Model

### 14.1 First version

The first version should prefer:

- attach to an already-running process by PID
- explicit runtime adapter selection or backend-assisted adapter selection

### 14.2 Later versions

Follow-on work can expand to:

- system-wide process tracking
- automatic runtime detection
- multi-process attach
- coordinated host+device backend startup

## 15. Repo Integration

The first `uprobes` source should integrate with current branch boundaries:

- `perfagent` creates a host source
- host source emits `LaunchRecord`
- host sink normalizes into canonical launch events
- `gpu.Manager` stays unchanged at its boundary

That means this phase should not redesign:

- `gpu.Manager`
- `gpu.EventSink`
- `gpu.Timeline`
- `gpu` projection/export code

## 16. Recommended First Milestone

A realistic first milestone is:

1. one runtime adapter
2. one or two launch symbols
3. one target process
4. live launch stack capture
5. launch record visible in raw JSON and mixed `pprof`

That is enough to validate the real attach-late host source without overcommitting the architecture.

## 17. Open Questions

1. Which runtime family should be the first adapter target?
2. Should adapter selection live in `gpu/host/uprobes` config, or be chosen by the future vendor backend?
3. Do we want raw debug export for undecoded probe records during bring-up?
4. Should the first version collect only launch entry probes, or also return/submit probes if needed for stronger correlation?
