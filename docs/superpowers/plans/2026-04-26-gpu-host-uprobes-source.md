# GPU Host Uprobes Source Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the first real live host launch source using `gpu/host/uprobes`, so `perf-agent` can capture CPU launch attribution from a running process and emit `host.LaunchRecord` values into the existing GPU correlation path.

**Architecture:** This plan keeps the public host contract unchanged and adds one concrete `gpu/host/uprobes` implementation behind it. The first slice is intentionally narrow: one target process, one runtime adapter, one or two launch entry-point symbols, userspace parsing into `host.LaunchRecord`, and `perfagent` wiring to run this live source alongside the existing GPU stream backend. Device-side vendor execution remains out of scope beyond the existing canonical GPU event path.

**Tech Stack:** Go 1.26, `github.com/cilium/ebpf`, existing BPF build flow (`go generate`, generated `_bpfel.o` objects), `link.OpenExecutable`, `ringbuf`, existing `gpu/host` contract, existing `perfagent` lifecycle.

**Reference spec:** `docs/superpowers/specs/2026-04-25-gpu-host-uprobes-design.md`

---

## File Structure

**New:**
- `gpu/host/uprobes/config.go` — target PID and runtime-adapter configuration.
- `gpu/host/uprobes/runtime.go` — internal runtime-adapter interface plus shared probe spec types.
- `gpu/host/uprobes/runtime_stub.go` — first minimal runtime adapter used by tests and initial wiring.
- `gpu/host/uprobes/records.go` — userspace record parsing and conversion into `host.LaunchRecord`.
- `gpu/host/uprobes/collector.go` — source lifecycle, attach/detach, ringbuf consume loop, and sink emission.
- `gpu/host/uprobes/collector_test.go` — config, lifecycle, and parsing-oriented tests that do not require root.
- `gpu/host/uprobes/runtime_test.go` — adapter symbol/decode tests.
- `gpu/host/uprobes/testdata/raw_launch.json` — raw-record fixture for userspace decode tests.

**Modified:**
- `perfagent/options.go` — host uprobes configuration option(s).
- `perfagent/agent.go` — build and start a host uprobes source.
- `perfagent/agent_test.go` — config validation tests for host source selection.
- `README.md` — short experimental host-`uprobes` note.

**Deferred to a follow-on plan:**
- actual eBPF program source under `bpf/` if the first slice proves we need kernel-side raw stack collection immediately
- full runtime-specific adapters for NVIDIA / AMD / Intel
- system-wide process tracking
- container/cgroup-specific metadata enrichment

---

## Scope Constraints

This plan is intentionally narrow.

The first implementation should:
- create the `gpu/host/uprobes` package and stable source API
- prove attach orchestration and userspace record decoding
- wire the source into `perfagent`

The first implementation should **not** attempt to solve:
- every runtime family
- every metadata field in `LaunchRecord`
- production-complete stack symbolization
- generalized probe discovery

If the initial implementation needs a stub runtime adapter and fixture-backed raw event path before a real BPF probe record exists, that is acceptable. The goal is a clean path to a real live source, not artificial completeness.

---

## Testing Conventions

All tasks in this plan use unit tests and local smoke commands first.

Standard commands:

```bash
go test ./gpu/host/uprobes -v
go test ./gpu/host/... ./gpu/... ./perfagent/... -v
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

If a task introduces an integration test that requires root or `CAP_BPF`, the test must detect that and skip cleanly when unavailable.

---

## Chunk 1: Runtime Adapter and Record Decoding

### Task 1: Define the internal runtime-adapter contract

**Files:**
- Create: `gpu/host/uprobes/runtime.go`
- Create: `gpu/host/uprobes/runtime_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/host/uprobes/runtime_test.go` with tests for:
- adapter ID stability
- adapter symbol list not being empty
- adapter decode rejecting malformed raw records

Use tests along these lines:

```go
package uprobes

import "testing"

func TestStubRuntimeAdapterSymbols(t *testing.T) {
	adapter := newStubRuntimeAdapter()
	if adapter.ID() == "" {
		t.Fatal("expected adapter ID")
	}
	if len(adapter.Symbols()) == 0 {
		t.Fatal("expected at least one symbol")
	}
}

func TestStubRuntimeAdapterRejectsMalformedRecord(t *testing.T) {
	adapter := newStubRuntimeAdapter()
	_, err := adapter.DecodeLaunch(rawRecord{})
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host/uprobes -run 'TestStubRuntimeAdapterSymbols|TestStubRuntimeAdapterRejectsMalformedRecord' -v
```

Expected: FAIL with undefined adapter and raw-record symbols.

- [ ] **Step 3: Implement the minimal runtime contract**

Create `gpu/host/uprobes/runtime.go` with:
- `type probeSpec struct`
- `type rawRecord struct`
- `type runtimeAdapter interface`

Create `gpu/host/uprobes/runtime_stub.go` with:
- `type stubRuntimeAdapter struct`
- `func newStubRuntimeAdapter() runtimeAdapter`
- `ID()`, `Symbols()`, and `DecodeLaunch(rawRecord)` implementations

Keep the stub adapter narrow:
- one or two launch symbols
- enough fields to emit a valid `host.LaunchRecord`
- no attempt to represent a real vendor ABI yet

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host/uprobes -run 'TestStubRuntimeAdapterSymbols|TestStubRuntimeAdapterRejectsMalformedRecord' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/host/uprobes/runtime.go gpu/host/uprobes/runtime_stub.go gpu/host/uprobes/runtime_test.go
git commit -m "gpu: add host uprobes runtime adapter contract"
```

### Task 2: Parse raw uprobe records into `host.LaunchRecord`

**Files:**
- Create: `gpu/host/uprobes/records.go`
- Modify: `gpu/host/uprobes/runtime_test.go`
- Create: `gpu/host/uprobes/testdata/raw_launch.json`

- [ ] **Step 1: Write the failing tests**

Add a test that decodes a raw record fixture into a normalized `host.LaunchRecord` through the stub adapter:

```go
func TestDecodeRawRecordIntoLaunchRecord(t *testing.T) {
	rec, err := decodeRawRecordFile("testdata/raw_launch.json")
	if err != nil {
		t.Fatalf("decodeRawRecordFile: %v", err)
	}
	launch, err := newStubRuntimeAdapter().DecodeLaunch(rec)
	if err != nil {
		t.Fatalf("DecodeLaunch: %v", err)
	}
	if launch.KernelName == "" {
		t.Fatal("expected kernel name")
	}
	if launch.CorrelationID == "" {
		t.Fatal("expected correlation ID")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host/uprobes -run TestDecodeRawRecordIntoLaunchRecord -v
```

Expected: FAIL with missing decode helper / record parsing.

- [ ] **Step 3: Implement minimal record parsing**

Create `gpu/host/uprobes/records.go` with:
- a file-local helper to decode raw-record fixtures
- strict field validation for the stub adapter path

Create `gpu/host/uprobes/testdata/raw_launch.json` with:
- pid/tid
- timestamp
- kernel name
- queue or stream identity
- correlation ID
- a minimal stack representation if required by the stub path

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host/uprobes -run TestDecodeRawRecordIntoLaunchRecord -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/host/uprobes/records.go gpu/host/uprobes/runtime_test.go gpu/host/uprobes/testdata/raw_launch.json
git commit -m "gpu: decode host uprobes raw records"
```

## Chunk 2: Collector Lifecycle and HostSource Implementation

### Task 3: Add source config and lifecycle shape

**Files:**
- Create: `gpu/host/uprobes/config.go`
- Create: `gpu/host/uprobes/collector.go`
- Create: `gpu/host/uprobes/collector_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/host/uprobes/collector_test.go` with tests for:
- rejecting an empty PID
- rejecting a nil adapter
- constructing a valid source config

Use tests like:

```go
package uprobes

import "testing"

func TestNewRejectsEmptyPID(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNewAcceptsMinimalConfig(t *testing.T) {
	src, err := New(Config{PID: 123, Adapter: newStubRuntimeAdapter()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if src == nil {
		t.Fatal("expected source")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host/uprobes -run 'TestNewRejectsEmptyPID|TestNewAcceptsMinimalConfig' -v
```

Expected: FAIL with missing `Config` / `New`.

- [ ] **Step 3: Implement minimal config and source constructor**

Create `gpu/host/uprobes/config.go` with:
- `type Config struct`
  - `PID int`
  - `Adapter runtimeAdapter`
  - optional paths or symbol overrides only if required immediately

Create `gpu/host/uprobes/collector.go` with:
- `type Source struct`
- `func New(Config) (*Source, error)`
- `func (s *Source) ID() string`

Do not implement real attach logic yet. Only validate config and create the source.

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host/uprobes -run 'TestNewRejectsEmptyPID|TestNewAcceptsMinimalConfig' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/host/uprobes/config.go gpu/host/uprobes/collector.go gpu/host/uprobes/collector_test.go
git commit -m "gpu: add host uprobes source config"
```

### Task 4: Implement a fixture-backed consume loop behind the `HostSource` contract

**Files:**
- Modify: `gpu/host/uprobes/collector.go`
- Modify: `gpu/host/uprobes/collector_test.go`

- [ ] **Step 1: Write the failing tests**

Add a test that starts the source, consumes one decoded raw record through the adapter, and emits one `host.LaunchRecord` to a capture sink.

Do this without a real BPF attach yet by injecting a fixture-backed raw-record provider.

Example:

```go
func TestSourceStartEmitsLaunchRecord(t *testing.T) {
	src, err := New(Config{
		PID:     123,
		Adapter: newStubRuntimeAdapter(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src.setTestRawRecords([]rawRecord{
		{
			PID:           123,
			TID:           124,
			TimeNs:        100,
			KernelName:    "flash_attn_fwd",
			CorrelationID: "c1",
		},
	})

	var sink captureSink
	if err := src.Start(t.Context(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sink.records) != 1 {
		t.Fatalf("records=%d", len(sink.records))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/host/uprobes -run TestSourceStartEmitsLaunchRecord -v
```

Expected: FAIL with missing consume-loop or test-injection support.

- [ ] **Step 3: Implement minimal start/stop/close behavior**

Update `collector.go` so:
- `Start` accepts a `host.HostSink`
- the source consumes injected raw records in tests
- decoded records are emitted as `host.LaunchRecord`
- `Stop` and `Close` are implemented and idempotent

This stage still avoids a real probe attach. It validates the source lifecycle and emission path first.

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/host/uprobes -run TestSourceStartEmitsLaunchRecord -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/host/uprobes/collector.go gpu/host/uprobes/collector_test.go
git commit -m "gpu: emit launch records from host uprobes source"
```

## Chunk 3: Perfagent Wiring

### Task 5: Add host-`uprobes` config options and validation

**Files:**
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`
- Modify: `perfagent/agent_test.go`

- [ ] **Step 1: Write the failing tests**

Add tests for:
- valid host-`uprobes` source plus GPU stream mode
- rejecting multiple host source options if replay and `uprobes` are both configured

Example config test shape:

```go
func TestConfigRejectsMultipleHostSources(t *testing.T) {
	_, err := New(
		WithGPUHostReplayInput("fixture.json"),
		WithGPUHostUprobesPID(123),
		WithGPUStreamInput(strings.NewReader("")),
	)
	if err == nil {
		t.Fatal("expected error")
	}
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
go test ./perfagent -run 'TestConfigValidation|TestConfigRejectsMultipleHostSources' -v
```

Expected: FAIL with missing host-`uprobes` options or validation.

- [ ] **Step 3: Implement minimal perfagent options**

Add to `perfagent/options.go`:
- `GPUHostUprobesPID int`
- `func WithGPUHostUprobesPID(pid int) Option`

Update `perfagent/agent.go`:
- include the new host source in `hostSourceCount()`
- reject conflicts with host replay
- create a `gpu/host/uprobes` source when configured

For the first version, use the stub runtime adapter internally. Do not solve runtime selection yet.

- [ ] **Step 4: Run targeted tests to verify they pass**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test ./perfagent -run 'TestConfigValidation|TestConfigRejectsMultipleHostSources' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go perfagent/agent.go perfagent/agent_test.go
git commit -m "perfagent: add host uprobes source options"
```

### Task 6: Add CLI option and root-module config test

**Files:**
- Modify: `main.go`
- Modify: `main_test.go`

- [ ] **Step 1: Write the failing test**

Add a root-level test similar to the current GPU stream tests:

```go
func TestBuildOptionsGPUHostUprobesPlusStreamMode(t *testing.T) {
	prevPID := *flagGPUHostUprobesPID
	...
	*flagGPUHostUprobesPID = 123
	*flagGPUStreamStdin = true

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
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
go test . -run TestBuildOptionsGPUHostUprobesPlusStreamMode -v
```

Expected: FAIL with missing CLI flag plumbing.

- [ ] **Step 3: Implement minimal CLI flag support**

Add to `main.go`:
- `--gpu-host-uprobes-pid`

Update `buildOptions()` to:
- create the `perfagent.WithGPUHostUprobesPID(...)` option
- allow host-`uprobes` plus GPU stream mode without requiring `--pid` or `--all` for the main CPU profiler path

- [ ] **Step 4: Run test to verify it passes**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test . -run TestBuildOptionsGPUHostUprobesPlusStreamMode -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "main: add host uprobes CLI flag"
```

## Chunk 4: Documentation and Verification

### Task 7: Document the experimental host-`uprobes` path

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add or extend the relevant test first**

If needed, extend one `perfagent` test so it asserts a host-`uprobes`-configured source can start in fixture-backed mode and produce a non-empty launch record path.

- [ ] **Step 2: Run that targeted test**

Run one of:

```bash
go test ./gpu/host/uprobes -v
```

or

```bash
go test ./perfagent -run 'TestAgent.*HostUprobes' -v
```

Expected: PASS before doc edits.

- [ ] **Step 3: Update README**

Document:
- the experimental `--gpu-host-uprobes-pid` option
- that the first implementation is still narrow and runtime-adapter-limited
- that this is the first real attach-late host source, not a full vendor backend

- [ ] **Step 4: Run broad verification**

Run:

```bash
export GOCACHE=/tmp/perf-agent-gocache
export GOMODCACHE=/tmp/perf-agent-gomodcache
export GOTOOLCHAIN=auto
export LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH"
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
go test ./gpu/host/uprobes ./gpu/host/... ./gpu/... ./perfagent/... . -v
make test-unit
```

Expected:
- PASS
- CAP_BPF-dependent tests may skip
- `make test-unit` may regenerate tracked `profile/*_bpfel.o` artifacts; clean only that generated drift before finalizing

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document host uprobes path"
```

## Chunk 5: Real Probe Bring-Up Gate

### Task 8: Decide whether to add the first real BPF attach in this branch or a follow-on plan

**Files:**
- Potentially create later:
  - `bpf/gpu_host_uprobes.bpf.c`
  - generated `.o` and `.go` files once attach semantics are clear

- [ ] **Step 1: Review the completed fixture-backed source**

Confirm:
- source contract is stable
- runtime adapter boundary is stable
- `perfagent` wiring is stable

- [ ] **Step 2: Decide if a real BPF attach is now low risk**

If yes:
- write a dedicated follow-on plan for the first real attach

If no:
- stop here and keep this branch as the userspace/source-API checkpoint

- [ ] **Step 3: Do not guess**

If actual BPF attach requirements are still unclear:
- do not bundle them into this plan
- create a follow-on spec/plan after validating the narrow source design
