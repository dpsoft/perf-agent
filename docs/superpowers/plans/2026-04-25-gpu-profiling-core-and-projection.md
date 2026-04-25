# GPU Profiling Core and Synthetic-Frame Projection Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the vendor-agnostic GPU profiling core, a deterministic replay backend, JSON raw-event export, and a mixed CPU+GPU `pprof` projection using synthetic GPU frames.

**Architecture:** This plan intentionally implements the contract-first portion of the GPU profiling spec without choosing a real vendor SDK yet. A new `gpu/` package owns normalized event types, timeline correlation, lifecycle management, export, and pprof projection. A `gpu/backend/replay/` backend replays fixture events into the core so the end-to-end path is testable now; real vendor backends plug into the same contract later.

**Implementation note:** The actual branch keeps `Backend` and `EventSink` in `gpu/types.go` instead of a separate `gpu/backend/backend.go` file. That avoids an import cycle once `gpu.Manager` depends on the backend contract and implementation packages like `gpu/backend/replay` depend on canonical `gpu` event types.

**Tech Stack:** Go 1.26, `encoding/json`, `context`, `errors`, `slices`, `maps`, `cmp`, `github.com/google/pprof/profile`, existing `pprof/` package, existing `perfagent` CLI and lifecycle wiring.

**Reference spec:** `docs/superpowers/specs/2026-04-25-gpu-profiling-design.md`

---

## File Structure

**New:**
- `gpu/types.go` — canonical GPU event types, capability constants, execution identity types.
- `gpu/types_test.go` — zero-value, JSON, and capability-model tests.
- `gpu/timeline.go` — event ingestion, correlation, and snapshot assembly.
- `gpu/timeline_test.go` — correlation and heuristic-join tests.
- `gpu/exporter.go` — JSON raw-event export and snapshot export helpers.
- `gpu/exporter_test.go` — export format tests.
- `gpu/pprof_projection.go` — `pprof.ProfileSample` projection with synthetic GPU frames.
- `gpu/pprof_projection_test.go` — mixed-stack projection tests.
- `gpu/manager.go` — backend lifecycle, fan-in, cancellation, and output orchestration.
- `gpu/manager_test.go` — manager lifecycle and error-propagation tests.
- `gpu/backend/replay/replay.go` — deterministic fixture-backed backend for Phase 1.
- `gpu/backend/replay/replay_test.go` — replay backend tests.
- `gpu/testdata/replay/flash_attn.json` — normalized event fixture representing one active workload.

**Modified:**
- `perfagent/options.go` — GPU replay / raw-output / pprof-output config options.
- `perfagent/agent.go` — GPU manager lifecycle integration.
- `perfagent/agent_test.go` — config and lifecycle tests for GPU mode.
- `main.go` — experimental CLI flags for replay-backed GPU profiling.
- `README.md` — short note documenting the experimental replay-driven GPU path.

**Not in scope for this plan:**
- Real vendor SDK integration (`gpu/backend/nvidia`, `gpu/backend/intel`, `gpu/backend/amd`)
- eBPF host launch collector
- system-wide multi-workload arbitration
- folded-stack export as a primary artifact

---

## Testing Conventions

All tasks in this plan use root-module unit tests only. No root privileges or vendor SDKs are required.

Standard commands:

```bash
go test ./gpu/... -v
go test ./perfagent/... -v
go test ./... -run 'TestGPU|TestReplay|TestProjection' -v
```

When touching the CLI or agent wiring, finish with:

```bash
make test-unit
```

Expected: PASS, with any existing CAP_BPF-dependent tests still skipped as they are today.

---

## Chunk 1: Core Types, Timeline, and Export

### Task 1: Canonical GPU types and capability model

**Files:**
- Create: `gpu/types.go`
- Create: `gpu/types_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/types_test.go` with:

```go
package gpu

import (
	"encoding/json"
	"testing"
)

func TestCapabilityConstantsStable(t *testing.T) {
	got := []GPUCapability{
		CapabilityLaunchTrace,
		CapabilityExecTimeline,
		CapabilityDeviceCounters,
		CapabilityPCSampling,
		CapabilityStallReasons,
		CapabilitySourceMap,
	}
	want := []GPUCapability{
		"launch-trace",
		"exec-timeline",
		"device-counters",
		"gpu-pc-sampling",
		"stall-reasons",
		"gpu-source-correlation",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cap[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestLaunchRoundTripJSON(t *testing.T) {
	in := GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "replay", Value: "corr-1"},
		KernelName:  "flash_attn_fwd",
		Launch: LaunchContext{PID: 42, TID: 43},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out GPUKernelLaunch
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.KernelName != in.KernelName || out.Correlation != in.Correlation {
		t.Fatalf("round-trip mismatch: %#v vs %#v", out, in)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/... -run 'TestCapabilityConstantsStable|TestLaunchRoundTripJSON' -v
```

Expected: FAIL with undefined GPU types and capabilities.

- [ ] **Step 3: Implement minimal types and backend contract**

Create `gpu/types.go` with the canonical exported structs from the spec, including:
- `GPUBackendID`
- `GPUCapability`
- `GPUDeviceRef`
- `GPUQueueRef`
- `GPUExecutionRef`
- `CorrelationID`
- `LaunchContext`
- `GPUKernelLaunch`
- `GPUKernelExec`
- `GPUCounterSample`
- `GPUSample`
- `Backend` and `EventSink` interfaces

Use `any` where needed, not `interface{}`.

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/... -run 'TestCapabilityConstantsStable|TestLaunchRoundTripJSON' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/types.go gpu/types_test.go
git commit -m "gpu: add canonical event types and backend contract"
```

### Task 2: Timeline correlation and join policy

**Files:**
- Create: `gpu/timeline.go`
- Create: `gpu/timeline_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/timeline_test.go` with:

```go
package gpu

import "testing"

func TestTimelineCorrelatesByCorrelationID(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "replay", Value: "corr-1"},
		KernelName:  "flash_attn_fwd",
		Launch: LaunchContext{PID: 101, TID: 202},
	})
	tl.RecordExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: "replay", Value: "corr-1"},
		KernelName:  "flash_attn_fwd",
		StartNs:     100,
		EndNs:       250,
	})
	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 {
		t.Fatalf("got %d executions", len(snapshot.Executions))
	}
	if snapshot.Executions[0].Launch == nil {
		t.Fatalf("expected correlated launch")
	}
}

func TestTimelineMarksHeuristicJoin(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Queue:      GPUQueueRef{Backend: "replay", QueueID: "q0"},
		KernelName: "flash_attn_fwd",
		TimeNs:     100,
	})
	tl.RecordExec(GPUKernelExec{
		Queue:      GPUQueueRef{Backend: "replay", QueueID: "q0"},
		KernelName: "flash_attn_fwd",
		StartNs:    120,
		EndNs:      200,
	})
	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 || !snapshot.Executions[0].Heuristic {
		t.Fatalf("expected heuristic join: %#v", snapshot.Executions)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/... -run 'TestTimelineCorrelatesByCorrelationID|TestTimelineMarksHeuristicJoin' -v
```

Expected: FAIL with undefined timeline symbols.

- [ ] **Step 3: Implement minimal timeline logic**

Create `gpu/timeline.go` with:
- `Timeline` type
- `NewTimeline() *Timeline`
- `RecordLaunch`, `RecordExec`, `RecordCounter`, `RecordSample`
- `Snapshot() Snapshot`
- `Snapshot` / `ExecutionView` types carrying `Launch *GPUKernelLaunch`, `Exec GPUKernelExec`, `Samples []GPUSample`, `Heuristic bool`

Use the documented join order:
1. correlation ID
2. execution/context identity
3. queue/device plus bounded time window
4. heuristic flag set when step 3 is used

Prefer `maps.Clone` / `slices.Clone` where defensive copies are needed.

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/... -run 'TestTimelineCorrelatesByCorrelationID|TestTimelineMarksHeuristicJoin' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/timeline.go gpu/timeline_test.go
git commit -m "gpu: add timeline correlation and join policy"
```

### Task 3: JSON raw-event export

**Files:**
- Create: `gpu/exporter.go`
- Create: `gpu/exporter_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/exporter_test.go` with:

```go
package gpu

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteJSONSnapshot(t *testing.T) {
	var buf bytes.Buffer
	snap := Snapshot{
		Executions: []ExecutionView{
			{Exec: GPUKernelExec{KernelName: "flash_attn_fwd", StartNs: 1, EndNs: 2}},
		},
	}
	if err := WriteJSONSnapshot(&buf, snap); err != nil {
		t.Fatalf("WriteJSONSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), "flash_attn_fwd") {
		t.Fatalf("missing kernel name in %q", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/... -run TestWriteJSONSnapshot -v
```

Expected: FAIL with undefined `WriteJSONSnapshot`.

- [ ] **Step 3: Implement exporter helpers**

Create `gpu/exporter.go` with:
- `WriteJSONSnapshot(w io.Writer, snap Snapshot) error`
- optional `WriteJSONEvents(w io.Writer, events []any) error` if needed by replay fixtures

Use `json.Encoder` and preserve deterministic field order via struct fields rather than manual map assembly.

- [ ] **Step 4: Run test to verify it passes**

Run:

```bash
go test ./gpu/... -run TestWriteJSONSnapshot -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/exporter.go gpu/exporter_test.go
git commit -m "gpu: add json snapshot export"
```

## Chunk 2: Projection, Replay Backend, and Lifecycle

### Task 4: Mixed CPU+GPU `pprof` projection

**Files:**
- Create: `gpu/pprof_projection.go`
- Create: `gpu/pprof_projection_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/pprof_projection_test.go` with:

```go
package gpu

import (
	"testing"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestProjectionAppendsSyntheticGPUFrames(t *testing.T) {
	snap := Snapshot{
		Executions: []ExecutionView{
			{
				Launch: &GPUKernelLaunch{
					Queue:      GPUQueueRef{Backend: "replay", QueueID: "q7"},
					KernelName: "flash_attn_fwd",
					Launch: LaunchContext{
						PID: 1,
						CPUStack: []pp.Frame{
							pp.FrameFromName("train_step"),
							pp.FrameFromName("cudaLaunchKernel"),
						},
					},
				},
				Exec: GPUKernelExec{
					Queue:      GPUQueueRef{Backend: "replay", QueueID: "q7"},
					KernelName: "flash_attn_fwd",
					StartNs:    10,
					EndNs:      50,
				},
				Samples: []GPUSample{{StallReason: "memory_throttle", Weight: 7}},
			},
		},
	}
	samples := ProjectExecutionSamples(snap)
	if len(samples) != 1 {
		t.Fatalf("got %d samples", len(samples))
	}
	got := samples[0].Stack
	wantNames := []string{
		"train_step",
		"cudaLaunchKernel",
		"[gpu:launch]",
		"[gpu:queue:q7]",
		"[gpu:kernel:flash_attn_fwd]",
		"[gpu:stall:memory_throttle]",
	}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d frames, want %d", len(got), len(wantNames))
	}
	for i, want := range wantNames {
		if got[i].Name != want {
			t.Fatalf("frame %d = %q want %q", i, got[i].Name, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/... -run TestProjectionAppendsSyntheticGPUFrames -v
```

Expected: FAIL with undefined projection symbols.

- [ ] **Step 3: Implement minimal projection**

Create `gpu/pprof_projection.go` with:
- `ProjectExecutionSamples(snap Snapshot) []pprof.ProfileSample`
- helper(s) that append synthetic GPU frames after the launch CPU stack

Use:
- sample value = GPU sample `Weight` when present
- fallback value = `Exec.EndNs - Exec.StartNs` for execution-weighted samples

- [ ] **Step 4: Run test to verify it passes**

Run:

```bash
go test ./gpu/... -run TestProjectionAppendsSyntheticGPUFrames -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/pprof_projection.go gpu/pprof_projection_test.go
git commit -m "gpu: project correlated events into synthetic pprof stacks"
```

### Task 5: Replay backend for deterministic end-to-end testing

**Files:**
- Create: `gpu/backend/replay/replay.go`
- Create: `gpu/backend/replay/replay_test.go`
- Create: `gpu/testdata/replay/flash_attn.json`

- [ ] **Step 1: Create replay fixture**

Create `gpu/testdata/replay/flash_attn.json` as a JSON array containing:
- one launch event
- one exec event
- one sample event

All with matching correlation and queue metadata.

- [ ] **Step 2: Write the failing tests**

Create `gpu/backend/replay/replay_test.go` with:

```go
package replay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type sink struct{ launches int; execs int; samples int }
func (s *sink) EmitLaunch(gpu.GPUKernelLaunch)       { s.launches++ }
func (s *sink) EmitExec(gpu.GPUKernelExec)           { s.execs++ }
func (s *sink) EmitCounter(gpu.GPUCounterSample)     {}
func (s *sink) EmitSample(gpu.GPUSample)             { s.samples++ }

func TestReplayBackendEmitsFixture(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "replay", "flash_attn.json")
	b, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var s sink
	if err := b.Start(context.Background(), &s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.launches != 1 || s.execs != 1 || s.samples != 1 {
		t.Fatalf("counts: %+v", s)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run:

```bash
go test ./gpu/backend/replay -run TestReplayBackendEmitsFixture -v
```

Expected: FAIL with undefined replay backend symbols.

- [ ] **Step 4: Implement replay backend**

Create `gpu/backend/replay/replay.go` with:
- `type Backend struct { path string }`
- `func New(path string) (*Backend, error)`
- `ID`, `Capabilities`, `Start`, `Stop`, `Close`

`Start` should:
- read the fixture
- decode the JSON array
- emit events in order to the sink

Keep it synchronous and deterministic for Phase 1.

- [ ] **Step 5: Run test to verify it passes**

Run:

```bash
go test ./gpu/backend/replay -run TestReplayBackendEmitsFixture -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add gpu/backend/replay/replay.go gpu/backend/replay/replay_test.go gpu/testdata/replay/flash_attn.json
git commit -m "gpu: add replay backend for deterministic event ingestion"
```

### Task 6: Manager lifecycle and failure propagation

**Files:**
- Create: `gpu/manager.go`
- Create: `gpu/manager_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/manager_test.go` with:

```go
package gpu

import (
	"context"
	"errors"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/backend"
)

type fakeBackend struct{ startErr error }

func (f fakeBackend) ID() GPUBackendID                     { return "fake" }
func (f fakeBackend) Capabilities() []GPUCapability       { return nil }
func (f fakeBackend) Start(context.Context, backend.EventSink) error { return f.startErr }
func (f fakeBackend) Stop(context.Context) error          { return nil }
func (f fakeBackend) Close() error                        { return nil }

func TestManagerStartPropagatesCause(t *testing.T) {
	want := errors.New("boom")
	m := NewManager([]backend.Backend{fakeBackend{startErr: want}}, nil)
	err := m.Start(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Start error = %v, want %v", err, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/... -run TestManagerStartPropagatesCause -v
```

Expected: FAIL with undefined manager symbols.

- [ ] **Step 3: Implement manager**

Create `gpu/manager.go` with:
- `Manager` type
- `NewManager`
- `Start`, `Stop`, `Close`
- internal `Timeline` ownership
- optional writers for raw JSON and projected pprof

Use `context.WithCancelCause` so the first backend failure is preserved.

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/... -run TestManagerStartPropagatesCause -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/manager.go gpu/manager_test.go
git commit -m "gpu: add manager lifecycle and error propagation"
```

## Chunk 3: perfagent and CLI Wiring

### Task 7: Wire replay-backed GPU mode into `perfagent`

**Files:**
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`
- Modify: `perfagent/agent_test.go`

- [ ] **Step 1: Write the failing tests**

Add a unit test to `perfagent/agent_test.go` that:
- builds an agent with GPU replay input plus GPU raw/pprof outputs
- verifies config validation and `Start`/`Stop` complete without enabling CPU/offCPU/PMU

Sketch:

```go
func TestAgentGPUReplayMode(t *testing.T) {
	agent, err := New(
		WithGPUReplayInput("gpu/testdata/replay/flash_attn.json"),
		WithGPURawOutput(io.Discard),
		WithGPUProfileOutput(io.Discard),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()
	if err := agent.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := agent.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./perfagent/... -run TestAgentGPUReplayMode -v
```

Expected: FAIL with missing GPU config options.

- [ ] **Step 3: Implement option and agent wiring**

Add to `perfagent/options.go`:
- GPU replay input path
- GPU raw output writer/path
- GPU pprof output writer/path
- `WithGPUReplayInput`, `WithGPURawOutput`, `WithGPUProfileOutput`

Add to `perfagent/agent.go`:
- `gpuManager *gpu.Manager`
- validation that allows “GPU replay only” mode
- `Start` creates replay backend + manager
- `Stop` emits JSON raw output and projected pprof output when configured

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./perfagent/... -run TestAgentGPUReplayMode -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go perfagent/agent.go perfagent/agent_test.go
git commit -m "perfagent: wire replay-backed gpu manager"
```

### Task 8: Add experimental CLI path and docs

**Files:**
- Modify: `main.go`
- Modify: `README.md`

- [ ] **Step 1: Write the failing CLI-level test or smoke command**

If the repo has no CLI test harness, use a smoke command as the red-green check:

```bash
go run . \
  --gpu-replay-input gpu/testdata/replay/flash_attn.json \
  --gpu-raw-output /tmp/gpu-raw.json \
  --gpu-profile-output /tmp/gpu.pb.gz \
  --duration 1ms
```

Expected initially: FAIL with unknown flags.

- [ ] **Step 2: Implement CLI flags**

Add to `main.go`:
- `--gpu-replay-input`
- `--gpu-raw-output`
- `--gpu-profile-output`

Wire them into `buildOptions()` using the new `perfagent` options.

- [ ] **Step 3: Update README**

Add a short “Experimental GPU replay pipeline” section showing:

```bash
go run . \
  --gpu-replay-input gpu/testdata/replay/flash_attn.json \
  --gpu-raw-output /tmp/gpu-raw.json \
  --gpu-profile-output /tmp/gpu.pb.gz \
  --duration 1ms
go tool pprof /tmp/gpu.pb.gz
```

State clearly that this is:
- vendor-agnostic core validation
- not yet a real vendor backend
- expected to produce a mixed CPU+GPU synthetic-frame profile

- [ ] **Step 4: Run verification**

Run:

```bash
go test ./gpu/... ./perfagent/... -v
make test-unit
go run . \
  --gpu-replay-input gpu/testdata/replay/flash_attn.json \
  --gpu-raw-output /tmp/gpu-raw.json \
  --gpu-profile-output /tmp/gpu.pb.gz \
  --duration 1ms
```

Expected:
- tests PASS
- command exits successfully
- `/tmp/gpu-raw.json` and `/tmp/gpu.pb.gz` exist

- [ ] **Step 5: Commit**

```bash
git add main.go README.md
git commit -m "cli: add experimental gpu replay mode"
```

---

## Plan Notes

- This plan deliberately stops short of a real vendor backend because the spec keeps the first concrete vendor target open.
- The replay backend is not throwaway work; it becomes the deterministic fixture harness for future NVIDIA / Intel / AMD backends.
- Once this plan lands, the next plan should target:
  - first real backend selection
  - attach-late host correlation source
  - real launch/execution event ingestion

## Review Focus

When reviewing this plan, focus on:

- whether the replay backend is the right Phase 1 vehicle for exercising the vendor-agnostic contract
- whether `pprof` synthetic GPU frames are being validated early enough
- whether the `perfagent` integration is scoped correctly for an experimental path
- whether any task still assumes a concrete vendor choice too early

---

Plan complete and saved to `docs/superpowers/plans/2026-04-25-gpu-profiling-core-and-projection.md`. Ready to execute?
