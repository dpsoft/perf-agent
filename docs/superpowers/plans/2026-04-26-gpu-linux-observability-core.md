# GPU Linux Observability Core Implementation Plan

> **For agentic workers:** REQUIRED: Use `superpowers:subagent-driven-development` if subagents are available, or `superpowers:executing-plans` otherwise. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first real Linux-first, DRM-aware GPU observability core behind the existing `gpu` contract. The result should collect normalized lifecycle telemetry with `eBPF`, expose capability-driven nullability instead of fake parity, and validate the base timeline on at least one open-driver stack before adding runtime `uprobes` or vendor SDK depth.

**Architecture:** This plan implements the base operating mode described in the main GPU profiling spec. The collector is an `eBPF`-centered backend under `gpu/backend/` that observes syscall and `ioctl` boundaries, scheduler interaction, device and process identity, and open-driver lifecycle signals where available. It emits normalized timeline records into the existing GPU manager/export/projection pipeline. Runtime `uprobes`, callbacks, and vendor SDK collectors remain follow-on layers.

**Tech Stack:** Go 1.26, `github.com/cilium/ebpf`, BPF CO-RE via the repo’s existing `bpf2go` flow, ring buffer delivery, Linux syscall and scheduler tracepoints, optional BTF-backed hooks or driver tracepoints when present, existing `gpu.Manager`, JSON/raw export, and existing `perfagent` lifecycle wiring.

**Reference spec:** [2026-04-25-gpu-profiling-design.md](/home/diego/github/perf-agent/.worktrees/gpu-profiling-spec/docs/superpowers/specs/2026-04-25-gpu-profiling-design.md:1)

---

## Scope Lock

This plan is intentionally narrower than a full vendor backend.

It includes:

- normalized lifecycle and boundary telemetry
- DRM-aware `ioctl` timing
- process, thread, and device identity
- scheduler and wait-path correlation where feasible
- open-driver validation first
- `perfagent` wiring only after the package-level collector works

It does **not** include:

- vendor SDK integration such as `CUPTI`, `ROCprofiler`, or Level Zero metrics
- GPU PMU counters or replay-based per-kernel metrics
- `gpu/host/uprobes`
- mixed CPU+GPU flame graphs from deep device samples
- multi-vendor parity claims beyond the capability model

The first public operating scope is intentionally narrow:

- `--pid` only
- one active workload first
- no system-wide `--all` mode for this backend in the first implementation
- a nonzero PID is required in backend config, `perfagent` validation, and CLI wiring

---

## File Structure

**New:**

- `bpf/gpu_linux_observe.bpf.c` — tracepoint- and optionally BTF-backed collection for DRM-aware `ioctl` timing and scheduler correlation.
- `gpu/backend/linuxdrm/gen.go` — `bpf2go` directives for the Linux observability core.
- `gpu/backend/linuxdrm/export.go` — thin loader wrapper around generated BPF objects.
- `gpu/backend/linuxdrm/config.go` — backend configuration, target scope, and capability flags.
- `gpu/backend/linuxdrm/collector.go` — source lifecycle, attach, ringbuf consume loop, async error propagation.
- `gpu/backend/linuxdrm/records.go` — raw BPF record decoding.
- `gpu/backend/linuxdrm/normalize.go` — translation from raw records into canonical GPU timeline events.
- `gpu/backend/linuxdrm/fdclass.go` — DRM/GPU file-descriptor classification and device identity helpers.
- `gpu/backend/linuxdrm/open_driver.go` — optional enrichment hooks for open-driver-specific fields when tracepoints are present.
- `gpu/backend/linuxdrm/collector_test.go`
- `gpu/backend/linuxdrm/records_test.go`
- `gpu/backend/linuxdrm/normalize_test.go`
- `gpu/backend/linuxdrm/fdclass_test.go`
- `gpu/backend/linuxdrm/collector_integration_test.go`
- `gpu/backend/linuxdrm/testdata/` — record fixtures and normalization cases.

**Modified:**

- `gpu/types.go` — extend the canonical schema to support lower-level lifecycle and boundary timeline events, or add the generic event envelope chosen by the spec.
- `gpu/exporter.go` — include the new timeline event family in JSON/raw export.
- `gpu/timeline.go` — preserve the lower-level event stream without over-collapsing it into launch/exec records too early.
- `gpu/timeline_test.go`
- `gpu/manager.go` — accept the Linux observability backend and preserve its events through snapshot/export.
- `gpu/codec/ndjson.go` — only if the chosen schema extends NDJSON in this milestone; otherwise leave unchanged deliberately.
- `gpu/backend/replay/replay.go` — only if the chosen schema extends replay compatibility in this milestone; otherwise leave unchanged deliberately.
- `gpu/backend/stream/stream.go` — only if the chosen schema extends stream compatibility in this milestone; otherwise leave unchanged deliberately.
- `perfagent/options.go` — Linux observability backend option(s), but only after package-level live smoke passes.
- `perfagent/agent.go` — backend lifecycle wiring after collector proof.
- `perfagent/agent_test.go`
- `main.go` — experimental CLI flags for the Linux observability backend after collector proof.
- `main_test.go`
- `README.md` — experimental usage notes after public wiring lands.

**Optional if needed for validation coverage:**

- `docs/superpowers/specs/2026-04-26-gpu-open-driver-validation.md` — only if validation rules need their own separate note during implementation.

---

## Design Boundaries Locked In

- **Linux-first before vendor-first:** the first real collector starts at syscall, `ioctl`, scheduler, and open-driver boundaries.
- **Truthful nullability:** unavailable queue IDs, context IDs, or metrics stay unavailable; they are not guessed.
- **Ringbuf canonical feed:** raw records flow through one ordered event stream into userspace normalization.
- **Explicit low-level event path:** the new lifecycle event family must traverse a defined `gpu` manager path. For this milestone, it is required in manager snapshot and JSON/raw export; extending NDJSON replay/stream compatibility is explicitly deferred unless the schema decision makes it cheap.
- **Stable file identity at event time:** normalization must not depend on best-effort `/proc/<pid>/fd/<n>` lookups after the fact. The collector must capture stable file identity at event time or maintain an explicit FD lifecycle tracker.
- **Open-driver validation first:** at least one transparent Linux stack must validate the base timeline before runtime `uprobes` or vendor SDK depth becomes the next milestone.
- **Public wiring delayed:** `perfagent`, CLI, and README changes happen only after the package-level collector passes live smoke.

---

## Testing Conventions

Use TDD for every userspace behavior and capability-gated tests for BPF attach and live consume.

Standard commands:

```bash
go test ./gpu/... -v
go test ./gpu/backend/linuxdrm -v
go test ./perfagent/... . -v
```

When touching generated BPF artifacts or public wiring, finish with:

```bash
make test-unit
```

Use the project-standard Go environment when invoking `go test` or `go run` for packages that pull existing `perfagent` dependencies:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
```

Follow the repo’s existing BPF build convention:

- generate `bpf2go` outputs explicitly
- keep generated artifacts checked in when the repo already does so for comparable collectors
- make any extra build prerequisite explicit in package docs or test skips

Capability-gated tests must:

- skip cleanly when `CAP_BPF`, `CAP_PERFMON`, or required tracepoint access is unavailable
- skip cleanly when the host does not expose the target open-driver surface
- never silently downgrade a “live collector” test to fixture-only behavior

---

## Chunk 1: Extend the Canonical Timeline Schema

### Task 1: Add lifecycle and boundary events to the canonical GPU model

**Files:**

- Modify: `gpu/types.go`
- Modify: `gpu/exporter.go`
- Modify: `gpu/timeline.go`
- Modify: `gpu/timeline_test.go`
- Modify: `gpu/manager.go`
- Optional modify: `gpu/codec/ndjson.go`
- Optional modify: `gpu/backend/replay/replay.go`
- Optional modify: `gpu/backend/stream/stream.go`

- [ ] **Step 1: Write the failing tests**

Add tests covering:

- export and round-trip of the new lifecycle/boundary event family
- preservation of event order across mixed launch/exec and lower-level records
- explicit nullability for missing queue/context/device fields

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./gpu -run 'TestTimeline|TestExporter' -v
```

Expected: FAIL because the canonical model cannot yet represent the new event family.

- [ ] **Step 3: Implement the minimal schema extension**

Add either:

- explicit typed lifecycle structs, or
- one generic normalized timeline event envelope

Requirements:

- keep high-level `GPUKernelLaunch`, `GPUKernelExec`, `GPUCounterSample`, and `GPUSample`
- add enough structure for syscall, `ioctl`, wait, context, queue, and memory boundary telemetry
- record provenance and confidence explicitly
- choose and lock one traversal path through the current contract
  - either add a new manager and sink method for lifecycle events, or
  - explicitly scope the new event family to manager snapshot and JSON/raw export only for this milestone
- if NDJSON, replay, and stream are not extended now, state that decision in code comments and tests so the limitation is deliberate rather than accidental

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu -run 'TestTimeline|TestExporter' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/types.go gpu/exporter.go gpu/timeline.go gpu/timeline_test.go gpu/manager.go
git commit -m "gpu: extend canonical timeline event model"
```

---

## Chunk 2: Add the Linux DRM Observability Backend Skeleton

### Task 2: Define backend config, capabilities, and lifecycle

**Files:**

- Create: `gpu/backend/linuxdrm/config.go`
- Create: `gpu/backend/linuxdrm/collector.go`
- Create: `gpu/backend/linuxdrm/collector_test.go`

- [ ] **Step 1: Write the failing tests**

Cover:

- backend ID stability
- declared capabilities for the base mode
- constructor validation
- async error propagation contract on `Stop`

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestCollector|TestConfig' -v
```

Expected: FAIL with undefined backend pieces.

- [ ] **Step 3: Implement the minimal backend shell**

Requirements:

- backend ID is stable and Linux-specific
- capabilities reflect the base mode honestly
- `Start`/`Stop`/`Close` follow the async error pattern already used by `gpu/backend/stream`
- no public `perfagent` or CLI wiring yet

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestCollector|TestConfig' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/backend/linuxdrm/config.go gpu/backend/linuxdrm/collector.go gpu/backend/linuxdrm/collector_test.go
git commit -m "gpu: add linux drm backend skeleton"
```

---

## Chunk 3: Collect DRM-Aware `ioctl` Timing

### Task 3: Add the first real BPF path for GPU-boundary `ioctl` timing

**Files:**

- Create: `bpf/gpu_linux_observe.bpf.c`
- Create: `gpu/backend/linuxdrm/gen.go`
- Create: `gpu/backend/linuxdrm/export.go`
- Create: `gpu/backend/linuxdrm/records.go`
- Create: `gpu/backend/linuxdrm/records_test.go`

- [ ] **Step 1: Write the failing tests**

Cover:

- raw record decoding for enter/exit `ioctl` pairs
- preservation of PID/TID, `fd`, command, return code, and duration
- rejection of malformed or truncated ringbuf records

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestDecode|TestRecord' -v
```

Expected: FAIL because no BPF record shape exists yet.

- [ ] **Step 3: Implement the first BPF path**

Requirements:

- attach to `sys_enter_ioctl` and `sys_exit_ioctl`
- track inflight calls by `TID`
- emit ordered ringbuf records with timestamp, PID, TID, command, duration, return code, and stable file identity captured at event time
- do not rely on deferred `/proc/<pid>/fd/<n>` lookups as the only source of device identity
- if stable file identity cannot be captured from tracepoints alone, add the necessary BTF-backed or kprobe-assisted path now, or add an explicit FD-lifecycle tracker in this same chunk
- keep the BPF payload minimal and push enrichment to userspace

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestDecode|TestRecord' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add bpf/gpu_linux_observe.bpf.c gpu/backend/linuxdrm/gen.go gpu/backend/linuxdrm/export.go gpu/backend/linuxdrm/records.go gpu/backend/linuxdrm/records_test.go
git commit -m "gpu: add linux drm ioctl timing path"
```

---

## Chunk 4: Classify GPU FDs and Normalize Timeline Events

### Task 4: Turn raw records into truthful GPU timeline events

**Files:**

- Create: `gpu/backend/linuxdrm/fdclass.go`
- Create: `gpu/backend/linuxdrm/fdclass_test.go`
- Create: `gpu/backend/linuxdrm/normalize.go`
- Create: `gpu/backend/linuxdrm/normalize_test.go`

- [ ] **Step 1: Write the failing tests**

Cover:

- identifying DRM render/display nodes from stable file identity captured at event time
- mapping raw `ioctl` events into normalized timeline events with provenance
- preserving explicit “unknown” or “unavailable” fields instead of fabricating queue/context IDs

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestFD|TestNormalize' -v
```

Expected: FAIL because the classifier and normalizer do not exist.

- [ ] **Step 3: Implement minimal classification and normalization**

Requirements:

- classify likely GPU/DRM file identities conservatively
- map the record stream into the extended canonical event model
- annotate events with source, driver, confidence, and explicit nullability
- avoid driver-specific decoding in the generic path unless the source proves it

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestFD|TestNormalize' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/backend/linuxdrm/fdclass.go gpu/backend/linuxdrm/fdclass_test.go gpu/backend/linuxdrm/normalize.go gpu/backend/linuxdrm/normalize_test.go
git commit -m "gpu: normalize linux drm boundary events"
```

---

## Chunk 5: Add Scheduler and Open-Driver Enrichment

### Task 5: Enrich the base timeline without making false portability claims

**Files:**

- Modify: `bpf/gpu_linux_observe.bpf.c`
- Create: `gpu/backend/linuxdrm/open_driver.go`
- Modify: `gpu/backend/linuxdrm/collector.go`
- Modify: `gpu/backend/linuxdrm/collector_integration_test.go`

- [ ] **Step 1: Write the failing tests**

Cover:

- scheduler correlation around threads performing GPU-boundary `ioctl`s
- open-driver enrichment remaining optional and capability-driven
- collector behavior when the host lacks the driver-specific tracepoints or BTF hooks

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestIntegration|TestOpenDriver' -v
```

Expected: FAIL because the enrichment path does not exist yet.

- [ ] **Step 3: Implement the minimal enrichment path**

Requirements:

- add scheduler correlation that helps explain wait or submission stalls
- probe for open-driver enrichment opportunistically
- treat missing open-driver hooks as capability loss, not startup failure

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/backend/linuxdrm -run 'TestIntegration|TestOpenDriver' -v
```

Expected: PASS or clean SKIP when host capability is missing.

- [ ] **Step 5: Commit**

```bash
git add bpf/gpu_linux_observe.bpf.c gpu/backend/linuxdrm/open_driver.go gpu/backend/linuxdrm/collector.go gpu/backend/linuxdrm/collector_integration_test.go
git commit -m "gpu: add linux drm scheduler and open-driver enrichment"
```

---

## Chunk 6: Public Wiring and Validation

### Task 6: Wire the backend through `perfagent` after collector proof

**Files:**

- Modify: `gpu/manager.go`
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`
- Modify: `perfagent/agent_test.go`
- Modify: `main.go`
- Modify: `main_test.go`
- Modify: `README.md`

- [ ] **Step 1: Write the failing tests**

Cover:

- config validation for enabling the Linux observability backend
- rejection when the backend is enabled without a PID
- rejection when the backend is combined with `--all`
- manager lifecycle with the new backend
- raw timeline export containing Linux boundary events
- clean disablement when flags are unset

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./perfagent/... . -run 'TestGPU|TestMain' -v
```

Expected: FAIL because the backend is not wired publicly yet.

- [ ] **Step 3: Implement the minimal public wiring**

Requirements:

- keep the backend experimental
- keep the first public scope `--pid` only
- require a nonzero PID in backend config, `perfagent`, and CLI validation
- reject `--all` for this backend in the first implementation
- do not imply system-wide vendor parity
- make raw timeline export the primary public artifact for this milestone
- document privilege and kernel-surface expectations honestly

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./perfagent/... . -run 'TestGPU|TestMain' -v
make test-unit
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/manager.go perfagent/options.go perfagent/agent.go perfagent/agent_test.go main.go main_test.go README.md
git commit -m "perfagent: add linux gpu observability backend"
```

---

## Chunk 7: Open-Driver Validation Smoke

### Task 7: Prove the base collector on a transparent Linux stack

**Files:**

- Modify: `gpu/backend/linuxdrm/collector_integration_test.go`
- Optional add: `gpu/backend/linuxdrm/testdata/validation/*.json`

- [ ] **Step 1: Keep fixture-backed checks separate from live proof**

Fixture-backed tests may validate:

- record decoding
- normalization
- JSON export shape

They do **not** count as proof of live attach or file-identity capture.

- [ ] **Step 2: Add a capability-gated live smoke**

The smoke must:

- start the collector
- observe a real GPU-boundary workload on an open-driver host
- emit normalized lifecycle events
- confirm ordering and provenance in the JSON/raw output

- [ ] **Step 3: Run the smoke**

Run the appropriate package-level or repo-level smoke command for the available host.

Expected:

- PASS on a capable open-driver environment
- clean SKIP with explicit reason otherwise

- [ ] **Step 4: Document what was proven**

Update the README note or plan comments with:

- kernel and capability prerequisites
- which driver stack was used
- which event families were verified

- [ ] **Step 5: Commit**

```bash
git add gpu/backend/linuxdrm/collector_integration_test.go README.md
git commit -m "gpu: validate linux drm observability core"
```

---

## Final Verification

- [ ] Run:

```bash
go test ./gpu/... ./perfagent/... . -v
make test-unit
```

- [ ] Run one live smoke on a capable Linux host if available.

- [ ] Verify the worktree is clean except for intentionally untracked files.

- [ ] Commit any final docs alignment updates.

---

## Deferred Follow-ons

These are explicitly deferred until this plan is complete:

- `gpu/host/uprobes`
- vendor runtime callbacks
- `CUPTI` / `ROCprofiler` / Level Zero execution or metric backends
- deep mixed CPU+GPU flame graphs from device samples
- protobuf or parquet raw export
