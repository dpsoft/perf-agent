# GPU Live Event Source — Draft Design Spec

**Status:** draft for review and extension.
**Branch context:** `gpu-profiling-spec`
**Predecessor work:** `2026-04-25-gpu-profiling-design.md`, `2026-04-25-gpu-profiling-core-and-projection.md`, replay-backed GPU core implementation on this branch.
**Goal of this draft:** define the first live event source that feeds the existing vendor-agnostic `gpu` contract without forcing an immediate commitment to a concrete vendor SDK inside `perf-agent`.

## 1. Problem

The current branch now has:

- a vendor-agnostic `gpu` event model
- timeline correlation and JSON export
- synthetic-frame `pprof` projection
- a deterministic replay backend used for test and contract validation

What it does **not** have yet is a live event source. That means:

1. the `gpu` manager only processes fixture data, not streaming runtime events
2. no external collector can attach to a running workload and feed `perf-agent` in real time
3. the branch still lacks the transition point from “spec + replay” to “live ingestion”

We need a first real source that:

- exercises the current `gpu` contract under live conditions
- stays vendor-agnostic at the `perf-agent` boundary
- does not force us to embed a vendor SDK into the first live implementation step

## 2. Goal

Build the first **live** source for the `gpu` contract with these properties:

1. `perf-agent` accepts normalized GPU events from a streaming source, not just a replay file
2. the source contract is vendor-agnostic
3. the transport is simple enough to debug by hand
4. the source can be used by future vendor-specific helpers without changing the `gpu` core
5. the same live path can drive:
   - raw JSON snapshot export
   - mixed CPU+GPU synthetic-frame `pprof`

## 3. Non-goals

- No vendor SDK integration inside this phase
- No claim that the stream source itself is sufficient for full GPU profiling
- No requirement yet to solve host CPU stack capture in this phase
- No attempt to standardize a cross-process binary protocol up front
- No system-wide multi-producer aggregation protocol yet

## 4. Recommendation

The next source should be a **live NDJSON event stream backend**.

Concretely:

- `perf-agent` opens a local stream input
- a producer writes normalized event objects one per line
- `perf-agent` decodes them into canonical `gpu` types and feeds the existing manager

This is a better immediate step than embedding a real vendor runtime because:

- it validates the live ingestion path now
- it preserves the vendor-agnostic contract
- it decouples source transport work from vendor-collector work
- it gives future NVIDIA / Intel / AMD helpers a stable ingestion target

## 5. Why NDJSON First?

### 5.1 Advantages

- line-oriented and easy to inspect with standard tools
- straightforward to stream incrementally
- resilient for debugging partial failures
- simple to generate from Go, Python, Rust, or shell-adjacent helpers
- keeps the initial protocol transparent

### 5.2 Why not protobuf first?

protobuf may become the better long-term wire format, but it is premature for the first live source because:

- it makes manual debugging harder
- it introduces schema and versioning complexity before we have enough producer experience
- it obscures transport-level bugs that are easier to see in NDJSON

## 6. Source Boundary

The transport backend should be live, but still treat the `gpu` package as the source of truth.

### 6.1 Contract rule

- producers emit normalized event records
- `perf-agent` validates and ingests them
- `perf-agent` does **not** try to infer vendor-specific meanings beyond the normalized fields it already understands

### 6.2 Architectural consequence

This phase is a bridge layer:

- **below** the bridge: future vendor-specific collectors or host-side helpers
- **above** the bridge: current `gpu` timeline, export, and projection code

## 7. Transport Model

### 7.1 Initial transport

Use a **single local file descriptor or FIFO-like stream** as the first transport.

Possible concrete forms:

1. stdin
2. named pipe path
3. Unix domain socket stream

### 7.2 Recommended order

For the first implementation:

1. file-backed stream or stdin for simplest bring-up
2. Unix socket only if attach-late streaming or producer lifecycle requires it

This keeps the source simple while preserving the option to move to a longer-lived producer/consumer arrangement.

## 8. Event Framing

Each line is one JSON object with a required kind discriminator.

Example:

```json
{"kind":"launch","correlation":{"backend":"bridge","value":"corr-1"},"kernel_name":"flash_attn_fwd","time_ns":100}
{"kind":"exec","correlation":{"backend":"bridge","value":"corr-1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}
{"kind":"sample","correlation":{"backend":"bridge","value":"corr-1"},"kernel_name":"flash_attn_fwd","stall_reason":"memory_throttle","weight":7}
```

The line protocol is append-only:

- unknown fields are ignored
- malformed lines are surfaced as ingest errors
- unknown kinds are rejected clearly

## 9. Ingestion Semantics

### 9.1 Source lifecycle

The live backend should:

- start reading immediately on `Start`
- decode records incrementally
- emit decoded events into the existing manager sink
- stop cleanly on EOF, cancellation, or unrecoverable decode error

### 9.2 Failure model

We need two classes of failure:

1. **transport failure**
   - stream closed unexpectedly
   - read error
2. **event failure**
   - malformed JSON
   - invalid kind
   - semantically invalid normalized event

Transport failures end the source.
Event failures should be explicit; whether they are fatal or skippable remains a policy decision for implementation.

## 10. Validation Rules

The live backend should validate enough to preserve the integrity of the existing correlation model:

- `kind` must be present
- launch/exec/sample payload must match the selected kind
- correlation IDs, queue IDs, and kernel names should be preserved exactly when present
- timestamp fields should remain in nanoseconds

Validation should be strict enough to prevent silent garbage, but not so strict that it blocks future vendor-specific extensions.

## 11. Integration with Existing GPU Core

The current branch already has the needed upper layers:

- `gpu/types.go`
- `gpu/timeline.go`
- `gpu/manager.go`
- `gpu/exporter.go`
- `gpu/pprof_projection.go`

The new live source should plug in beneath them as another `gpu.Backend` implementation, alongside replay.

## 12. Proposed Repo Shape

```text
gpu/backend/replay/
  replay.go

gpu/backend/stream/
  stream.go          # NDJSON decode loop
  stream_test.go     # line protocol, EOF, invalid event cases
```

Possible helper package if decode logic grows:

```text
gpu/codec/
  ndjson.go          # decode one normalized event per line
  ndjson_test.go
```

## 13. Output Expectations

The live backend does not change the upper outputs.

It should still drive:

- JSON snapshot output
- synthetic-frame `pprof`

So the expected user path becomes:

1. source emits live normalized events
2. `perf-agent` ingests and correlates them
3. `perf-agent` exports raw JSON snapshot and mixed `pprof`

## 14. Scope Decision

This phase should remain **single-producer, single-active-workload first**.

That means:

- one stream source at a time
- no multi-producer arbitration yet
- no attempt to merge multiple concurrent helpers into one session

## 15. Risks

### 15.1 Overfitting the stream schema

If we harden the line schema too early, future vendor helpers may need awkward workarounds.

### 15.2 Under-validating malformed events

If the stream accepts bad records too easily, the timeline and pprof projection become misleading.

### 15.3 Transport churn

We may learn that stdin is too limiting and sockets are necessary. The implementation should keep the decode loop separate from the transport plumbing so this is cheap to swap.

### 15.4 False sense of “real backend”

This phase gives us a real live source, but not yet a real vendor runtime integration. The docs should stay honest about that.

## 16. Initial Recommendation

Implement the next phase as:

1. NDJSON live stream backend
2. strict enough event validation to protect the current correlation model
3. reuse existing manager/timeline/pprof/export layers unchanged where possible
4. keep replay as the deterministic test backend beside the live source

## 17. Open Questions / Decision Prompts

### 17.1 First transport

Question:
- should the first live source read from stdin, a named pipe path, or a Unix socket?

Current recommendation:
- start with the simplest file-descriptor-backed stream the CLI can expose cleanly

### 17.2 Event failure policy

Question:
- should malformed event lines fail the session, or be counted and skipped?

Current recommendation:
- fail fast at first, then relax only if a real producer proves this is too strict

### 17.3 Producer ownership

Question:
- should `perf-agent` launch the producer process, or should it only consume an already-opened source?

Current recommendation:
- consume only, do not own producer lifecycle yet

## 18. Review Focus

This draft should be reviewed for:

- whether NDJSON is the right first live transport
- whether the source boundary stays vendor-agnostic enough
- whether the transport phase is scoped narrowly enough to avoid redoing the core
- whether validation is strict enough to protect the current timeline and pprof outputs
