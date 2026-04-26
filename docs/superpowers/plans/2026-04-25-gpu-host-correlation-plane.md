# GPU Host Correlation Plane Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the first host-correlation path so `perf-agent` can ingest CPU-side GPU launch attribution and convert it into canonical `gpu.GPUKernelLaunch` events alongside existing GPU execution backends.

**Architecture:** This plan intentionally stops short of a real `uprobes` or vendor-callback implementation. It first adds a vendor-agnostic `gpu/host/` contract, normalization logic from source-facing launch records into canonical GPU launch events, and a deterministic replay host source for tests. Then it wires `perfagent` to run host sources and GPU backends together so the branch can prove end-to-end CPU-launch-to-GPU-exec correlation before choosing a real host instrumentation source.

**Tech Stack:** Go 1.26, `encoding/json`, `context`, `errors`, `slices`, `maps`, existing `gpu` core, existing `pprof` frame model, existing `perfagent` lifecycle wiring.

**Reference spec:** `docs/superpowers/specs/2026-04-25-gpu-host-correlation-design.md`

---

## File Structure

**New:**
- `gpu/host/source.go` — vendor-agnostic host-source and host-sink contracts.
- `gpu/host/launch.go` — source-facing `LaunchRecord` and normalization into canonical `gpu.GPUKernelLaunch`.
- `gpu/host/launch_test.go` — normalization and validation tests.
- `gpu/host/replay/replay.go` — deterministic fixture-backed host-launch source.
- `gpu/host/replay/replay_test.go` — replay source lifecycle and emit tests.
- `gpu/testdata/host/replay/flash_attn_launches.json` — launch fixture with CPU stack and correlation metadata.

**Modified:**
- `perfagent/options.go` — host replay input option(s).
- `perfagent/agent.go` — start/stop lifecycle for host sources plus GPU backends.
- `perfagent/agent_test.go` — host+GPU integration tests.
- `README.md` — short experimental host-correlation replay note.

**Not in scope for this plan:**
- real `uprobes` collector
- vendor runtime callback integration
- GPU device-side backend changes beyond consuming canonical launch events
- changing `gpu.Manager`’s canonical `EventSink` boundary

---

## Testing Conventions

All tasks in this plan use unit tests and local smoke commands only.

Standard commands:

```bash
go test ./gpu/host/... -v
go test ./gpu/... ./perfagent/... -v
```

When touching `perfagent` wiring, finish with:

```bash
make test-unit
```

Use the project-standard CGO environment when invoking `go test` or `go run` on packages that pull in existing `perfagent` dependencies:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
```

---

## Chunk 1: Host Contract and Launch Normalization

### Task 1: Host source contract and `LaunchRecord` normalization

**Files:**
- Create: `gpu/host/source.go`
- Create: `gpu/host/launch.go`
- Create: `gpu/host/launch_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/host/launch_test.go` with tests for:
- normalizing a complete `LaunchRecord` into `gpu.GPUKernelLaunch`
- preserving CPU stack and tags
- rejecting a record with missing backend/correlation identity

Use tests along these lines:

```go
package host

import (
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestNormalizeLaunchRecord(t *testing.T) {
	rec := LaunchRecord{
		Backend:       "stream",
		PID:           123,
		TID:           456,
		TimeNs:        100,
		KernelName:    "flash_attn_fwd",
		QueueID:       "q7",
		CorrelationID: "c1",
		CPUStack: []pp.Frame{
			pp.FrameFromName("train_step"),
			pp.FrameFromName("cudaLaunchKernel"),
		},
		Tags: map[string]string{"env": "test"},
	}

	launch, err := NormalizeLaunch(rec)
	if err != nil {
		t.Fatalf("NormalizeLaunch: %v", err)
	}
	if launch.Correlation != (gpu.CorrelationID{Backend: "stream", Value: "c1"}) {
		t.Fatalf("correlation=%+v", launch.Correlation)
	}
	if got := len(launch.Launch.CPUStack); got != 2 {
		t.Fatalf("cpu stack len=%d", got)
	}
}

func TestNormalizeLaunchRejectsMissingCorrelation(t *testing.T) {
	_, err := NormalizeLaunch(LaunchRecord{Backend: "stream", KernelName: "flash_attn_fwd"})
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host -run 'TestNormalizeLaunchRecord|TestNormalizeLaunchRejectsMissingCorrelation' -v
```

Expected: FAIL with undefined `LaunchRecord` / `NormalizeLaunch`.

- [ ] **Step 3: Implement minimal host contract and normalization**

Create `gpu/host/source.go` with:
- `type HostSource interface`
- `type HostSink interface`

Create `gpu/host/launch.go` with:
- `type LaunchRecord struct`
- `func NormalizeLaunch(LaunchRecord) (gpu.GPUKernelLaunch, error)`

Keep these rules:
- `LaunchRecord` remains host-package-owned, not part of the public `gpu` core contract
- normalization must preserve CPU stack and tags with defensive copies
- `Backend`, `CorrelationID`, and `KernelName` are required for the first version

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host -run 'TestNormalizeLaunchRecord|TestNormalizeLaunchRejectsMissingCorrelation' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/host/source.go gpu/host/launch.go gpu/host/launch_test.go
git commit -m "gpu: add host launch normalization"
```

### Task 2: Adapter from host sink to canonical GPU event sink

**Files:**
- Modify: `gpu/host/launch.go`
- Modify: `gpu/host/launch_test.go`

- [ ] **Step 1: Write the failing tests**

Add a test that adapts a `HostSink` to an existing `gpu.EventSink` and emits a canonical launch:

```go
func TestLaunchSinkEmitsCanonicalLaunch(t *testing.T) {
	var sink captureEventSink
	hostSink := NewLaunchSink(&sink)

	err := hostSink.EmitLaunchRecord(LaunchRecord{
		Backend:       "stream",
		KernelName:    "flash_attn_fwd",
		CorrelationID: "c1",
	})
	if err != nil {
		t.Fatalf("EmitLaunchRecord: %v", err)
	}
	if len(sink.launches) != 1 {
		t.Fatalf("launches=%d", len(sink.launches))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host -run TestLaunchSinkEmitsCanonicalLaunch -v
```

Expected: FAIL with missing adapter symbols.

- [ ] **Step 3: Implement minimal adapter**

Add:
- `type launchSink struct`
- `func NewLaunchSink(gpu.EventSink) HostSink`
- `func (s *launchSink) EmitLaunchRecord(LaunchRecord) error`

The adapter should:
- normalize the record
- emit exactly one canonical `gpu.GPUKernelLaunch`
- return normalization errors directly

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host -run TestLaunchSinkEmitsCanonicalLaunch -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/host/launch.go gpu/host/launch_test.go
git commit -m "gpu: adapt host launch records into canonical events"
```

## Chunk 2: Deterministic Host Replay Source

### Task 3: Replay host source emits `LaunchRecord` values from fixture data

**Files:**
- Create: `gpu/host/replay/replay.go`
- Create: `gpu/host/replay/replay_test.go`
- Create: `gpu/testdata/host/replay/flash_attn_launches.json`

- [ ] **Step 1: Write the failing tests**

Create `gpu/host/replay/replay_test.go` with tests for:
- reading a launch fixture and emitting one or more `LaunchRecord` values
- rejecting unknown or malformed fixture data

Use tests shaped like:

```go
package replay

import (
	"path/filepath"
	"testing"
)

func TestReplaySourceEmitsLaunchRecords(t *testing.T) {
	src, err := New(filepath.Join("..", "..", "testdata", "host", "replay", "flash_attn_launches.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var sink captureSink
	if err := src.Start(t.Context(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(sink.records); got == 0 {
		t.Fatal("expected records")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host/replay -run 'TestReplaySourceEmitsLaunchRecords|TestReplaySourceRejectsMalformedFixture' -v
```

Expected: FAIL with undefined replay source symbols.

- [ ] **Step 3: Implement minimal replay source**

Create:
- `type Source struct { path string }`
- `func New(path string) (*Source, error)`
- `func (s *Source) Start(ctx context.Context, sink host.HostSink) error`
- `func (s *Source) Stop(ctx context.Context) error`
- `func (s *Source) Close() error`

Fixture format should be a JSON array of `LaunchRecord`-shaped objects.

Keep this simple:
- read whole file
- decode array
- emit each record in order

- [ ] **Step 4: Add minimal fixture**

Create `gpu/testdata/host/replay/flash_attn_launches.json` with at least one launch containing:
- backend
- pid/tid
- kernel name
- queue ID
- correlation ID
- CPU stack entries matching a realistic launch path

- [ ] **Step 5: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host/replay -run 'TestReplaySourceEmitsLaunchRecords|TestReplaySourceRejectsMalformedFixture' -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add gpu/host/replay/replay.go gpu/host/replay/replay_test.go gpu/testdata/host/replay/flash_attn_launches.json
git commit -m "gpu: add host replay source"
```

## Chunk 3: `perfagent` Wiring and End-to-End Correlation

### Task 4: Add host-source options and lifecycle wiring

**Files:**
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`
- Modify: `perfagent/agent_test.go`

- [ ] **Step 1: Write the failing tests**

Add tests for:
- valid host replay mode combined with GPU stream mode
- host replay launch attribution producing non-empty synthetic-frame output when paired with a GPU exec/sample stream
- rejecting conflicting host source options if more than one is introduced

Example integration test shape:

```go
func TestAgentHostReplayPlusGPUStreamMode(t *testing.T) {
	var raw bytes.Buffer
	var profile bytes.Buffer

	agent, err := New(
		WithGPUHostReplayInput(filepath.Join("..", "gpu", "testdata", "host", "replay", "flash_attn_launches.json")),
		WithGPUStreamInput(strings.NewReader(
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n" +
				"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
		)),
		WithGPURawOutput(&raw),
		WithGPUProfileOutput(&profile),
	)
	require.NoError(t, err)

	ctx := t.Context()
	require.NoError(t, agent.Start(ctx))
	require.NoError(t, agent.Stop(ctx))
	assert.Contains(t, raw.String(), "flash_attn_fwd")
	assert.NotZero(t, profile.Len())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test ./perfagent -run TestAgentHostReplayPlusGPUStreamMode -v
```

Expected: FAIL with missing host replay options/wiring.

- [ ] **Step 3: Implement minimal host-source wiring**

Add to `perfagent/options.go`:
- `GPUHostReplayInput string`
- `func WithGPUHostReplayInput(path string) Option`

Update `perfagent/agent.go`:
- track zero or one host source for this phase
- build a `host.NewLaunchSink(a.gpuManager)` adapter
- start the host source before or alongside the GPU backend
- stop host source before snapshot/export so launch events are fully ingested

Keep the first version narrow:
- only host replay source is supported
- source conflicts should be rejected explicitly

- [ ] **Step 4: Run targeted tests to verify they pass**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test ./perfagent -run 'TestConfigValidation|TestAgentHostReplayPlusGPUStreamMode' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go perfagent/agent.go perfagent/agent_test.go
git commit -m "perfagent: wire host replay correlation source"
```

### Task 5: Document the host replay path and run full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Write the failing doc-oriented test or assertion**

There is no dedicated doc test here. Instead, add or extend the integration test from Task 4 so it asserts the final JSON snapshot contains launch metadata, not only execution/sample records.

- [ ] **Step 2: Run that test to verify the current behavior**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test ./perfagent -run TestAgentHostReplayPlusGPUStreamMode -v
```

Expected: PASS before doc updates.

- [ ] **Step 3: Update README**

Document an experimental path showing:
- host replay input
- live GPU stream input
- raw JSON export
- synthetic-frame `pprof` output

Keep the note explicit that this is still a contract-validation path, not a real `uprobes` collector or vendor callback backend yet.

- [ ] **Step 4: Run broad verification**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test ./gpu/host/... ./gpu/... ./perfagent/... . -v
make test-unit
```

Expected:
- PASS
- existing CAP_BPF-dependent tests may still skip
- `make test-unit` may regenerate tracked `profile/*_bpfel.o` files; clean only that generated drift before finalizing

- [ ] **Step 5: Optional smoke command**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/flash_attn_launches.json \
  --gpu-stream-stdin \
  --gpu-raw-output /tmp/gpu-host-raw.json \
  --gpu-profile-output /tmp/gpu-host.pb.gz \
  --duration 1ms <<'EOF'
{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}
{"kind":"sample","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":150,"stall_reason":"memory_throttle","weight":7}
EOF
```

Expected:
- successful run
- raw JSON includes launch, exec, and sample linkage
- pprof output is non-empty and includes CPU launch frames from the host replay fixture

- [ ] **Step 6: Commit**

```bash
git add README.md perfagent/agent_test.go
git commit -m "docs: document host replay correlation path"
```
