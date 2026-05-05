# GPU Live Event Source Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the first live GPU event source by implementing an NDJSON stream backend that feeds the existing `gpu` contract, JSON snapshot export, and synthetic-frame `pprof` projection.

**Architecture:** The current branch already has the `gpu` core, replay backend, timeline correlation, JSON snapshot output, and synthetic-frame `pprof` projection. This plan adds a new `gpu/backend/stream/` implementation that reads normalized NDJSON records from a live reader, decodes and validates them incrementally, emits canonical `gpu` events into the existing manager, and wires an experimental CLI path for stream input. Replay remains the deterministic fixture backend for tests.

**Tech Stack:** Go 1.26, `bufio`, `encoding/json`, `context`, `errors`, `slices`, `maps`, existing `gpu` package, existing `perfagent` and CLI wiring.

**Reference spec:** `docs/superpowers/specs/2026-04-25-gpu-live-event-source-design.md`

---

## File Structure

**New:**
- `gpu/backend/stream/stream.go` — live NDJSON backend using an `io.Reader`.
- `gpu/backend/stream/stream_test.go` — line protocol, EOF, malformed-event, and kind-dispatch tests.
- `gpu/codec/ndjson.go` — decode one normalized event per line into canonical `gpu` values.
- `gpu/codec/ndjson_test.go` — decode and validation tests.

**Modified:**
- `gpu/manager.go` — optional snapshot access or lifecycle helpers only if the stream backend needs them.
- `perfagent/options.go` — live stream input option(s).
- `perfagent/agent.go` — manager wiring for live stream mode.
- `perfagent/agent_test.go` — live stream mode tests.
- `main.go` — experimental stream flags.
- `README.md` — short “experimental live GPU stream” section.

**Not in scope for this plan:**
- vendor SDK integration
- CPU launch stack capture from real host instrumentation
- Unix socket transport if stdin / file-backed stream is sufficient
- multi-producer aggregation

---

## Testing Conventions

All tasks in this plan use unit tests and local smoke commands only.

Standard commands:

```bash
go test ./gpu/backend/stream ./gpu/codec -v
go test ./gpu/... ./perfagent/... -v
```

When touching the CLI or agent wiring, finish with:

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

## Chunk 1: NDJSON Decode and Validation

### Task 1: NDJSON codec for one event per line

**Files:**
- Create: `gpu/codec/ndjson.go`
- Create: `gpu/codec/ndjson_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/codec/ndjson_test.go` with tests for:
- decoding a valid `launch` line
- decoding a valid `exec` line
- decoding a valid `sample` line
- rejecting malformed JSON
- rejecting unknown `kind`

Use a small union-style result, for example:

```go
func TestDecodeLaunchLine(t *testing.T) {
	line := []byte(`{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":100}`)
	ev, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if ev.Kind != KindLaunch {
		t.Fatalf("kind=%v", ev.Kind)
	}
	if ev.Launch.KernelName != "flash_attn_fwd" {
		t.Fatalf("kernel=%q", ev.Launch.KernelName)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/codec -run 'TestDecodeLaunchLine|TestDecodeExecLine|TestDecodeSampleLine|TestDecodeRejectsMalformedJSON|TestDecodeRejectsUnknownKind' -v
```

Expected: FAIL with undefined decode symbols.

- [ ] **Step 3: Implement minimal codec**

Create `gpu/codec/ndjson.go` with:
- a small `DecodedEvent` struct
- `KindLaunch`, `KindExec`, `KindCounter`, `KindSample` constants
- `DecodeLine(line []byte) (DecodedEvent, error)`

Validation rules:
- `kind` required
- per-kind payload must decode into the matching canonical `gpu` struct
- unknown kind is an error

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/codec -run 'TestDecodeLaunchLine|TestDecodeExecLine|TestDecodeSampleLine|TestDecodeRejectsMalformedJSON|TestDecodeRejectsUnknownKind' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/codec/ndjson.go gpu/codec/ndjson_test.go
git commit -m "gpu: add ndjson event codec"
```

### Task 2: Validation for required fields and nanosecond timestamps

**Files:**
- Modify: `gpu/codec/ndjson.go`
- Modify: `gpu/codec/ndjson_test.go`

- [ ] **Step 1: Write the failing tests**

Add tests for:
- missing `kind`
- missing required payload for selected kind
- malformed timestamp field type

Example:

```go
func TestDecodeRejectsMissingKind(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kernel_name":"flash_attn_fwd"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/codec -run 'TestDecodeRejectsMissingKind|TestDecodeRejectsMissingKindPayload|TestDecodeRejectsBadTimestampType' -v
```

Expected: FAIL.

- [ ] **Step 3: Implement minimal validation**

Tighten `DecodeLine` so it:
- rejects empty or missing `kind`
- rejects kind/payload mismatches
- preserves timestamp fields as `uint64`-backed nanosecond values only

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/codec -run 'TestDecodeRejectsMissingKind|TestDecodeRejectsMissingKindPayload|TestDecodeRejectsBadTimestampType' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/codec/ndjson.go gpu/codec/ndjson_test.go
git commit -m "gpu: validate ndjson event payloads"
```

## Chunk 2: Live Stream Backend

### Task 3: Backend reads NDJSON events from a live reader

**Files:**
- Create: `gpu/backend/stream/stream.go`
- Create: `gpu/backend/stream/stream_test.go`

- [ ] **Step 1: Write the failing tests**

Create `gpu/backend/stream/stream_test.go` with tests for:
- one launch/exec/sample triplet from a `strings.Reader`
- clean EOF handling

Example:

```go
func TestStreamBackendEmitsEventsFromReader(t *testing.T) {
	src := strings.NewReader(
		"{\"kind\":\"launch\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":100}\n" +
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n" +
			"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"stall_reason\":\"memory_throttle\",\"weight\":7}\n",
	)
	b := New(src)
	var s sink
	if err := b.Start(context.Background(), &s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.launches != 1 || s.execs != 1 || s.samples != 1 {
		t.Fatalf("counts: %+v", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/backend/stream -run 'TestStreamBackendEmitsEventsFromReader|TestStreamBackendEOF' -v
```

Expected: FAIL with undefined stream backend symbols.

- [ ] **Step 3: Implement minimal stream backend**

Create `gpu/backend/stream/stream.go` with:
- `type Backend struct { r io.Reader }`
- `func New(r io.Reader) *Backend`
- `ID`, `Capabilities`, `Start`, `Stop`, `Close`

`Start` should:
- scan line by line
- call `codec.DecodeLine`
- dispatch to the sink

Use a `bufio.Scanner` first; only revisit framing if line size proves limiting.

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./gpu/backend/stream -run 'TestStreamBackendEmitsEventsFromReader|TestStreamBackendEOF' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/backend/stream/stream.go gpu/backend/stream/stream_test.go
git commit -m "gpu: add live ndjson stream backend"
```

### Task 4: Stream backend failure behavior

**Files:**
- Modify: `gpu/backend/stream/stream.go`
- Modify: `gpu/backend/stream/stream_test.go`

- [ ] **Step 1: Write the failing tests**

Add tests for:
- malformed JSON line returns error
- unknown kind returns error

Example:

```go
func TestStreamBackendFailsOnMalformedLine(t *testing.T) {
	src := strings.NewReader("{not-json}\n")
	b := New(src)
	var s sink
	if err := b.Start(context.Background(), &s); err == nil {
		t.Fatal("expected error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./gpu/backend/stream -run 'TestStreamBackendFailsOnMalformedLine|TestStreamBackendFailsOnUnknownKind' -v
```

Expected: FAIL.

- [ ] **Step 3: Implement failure policy**

For the first implementation:
- fail fast on malformed event lines
- return the decode/validation error directly

- [ ] **Step 4: Run test to verify it passes**

Run:

```bash
go test ./gpu/backend/stream -run 'TestStreamBackendFailsOnMalformedLine|TestStreamBackendFailsOnUnknownKind' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gpu/backend/stream/stream.go gpu/backend/stream/stream_test.go
git commit -m "gpu: fail fast on malformed stream events"
```

## Chunk 3: perfagent and CLI Wiring

### Task 5: Wire stream backend into `perfagent`

**Files:**
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`
- Modify: `perfagent/agent_test.go`

- [ ] **Step 1: Write the failing tests**

Add a test to `perfagent/agent_test.go` for live stream mode using an in-memory reader.

Example shape:

```go
func TestAgentGPUStreamMode(t *testing.T) {
	src := strings.NewReader("{\"kind\":\"launch\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":100}\n")
	agent, err := New(
		WithGPUStreamInput(src),
		WithGPURawOutput(io.Discard),
		WithGPUProfileOutput(io.Discard),
	)
	require.NoError(t, err)
	require.NoError(t, agent.Start(t.Context()))
	require.NoError(t, agent.Stop(t.Context()))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./perfagent/... -run TestAgentGPUStreamMode -v
```

Expected: FAIL with missing stream options or wiring.

- [ ] **Step 3: Implement option and agent wiring**

Add to `perfagent/options.go`:
- `GPUStreamInput io.Reader`
- `WithGPUStreamInput(r io.Reader)`

Add to `perfagent/agent.go`:
- validation that allows stream-only GPU mode
- startup path that selects stream backend when configured
- existing JSON snapshot / synthetic-frame `pprof` output path reused unchanged

- [ ] **Step 4: Run tests to verify they pass**

Run:

```bash
go test ./perfagent/... -run TestAgentGPUStreamMode -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go perfagent/agent.go perfagent/agent_test.go
git commit -m "perfagent: wire gpu live stream mode"
```

### Task 6: Experimental CLI path for NDJSON stream input

**Files:**
- Modify: `main.go`
- Modify: `README.md`

- [ ] **Step 1: Write the failing smoke command**

Use:

```bash
printf '%s\n' \
  '{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":100}' \
  '{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}' \
| go run . --gpu-stream-stdin --gpu-raw-output /tmp/gpu-stream.json --gpu-profile-output /tmp/gpu-stream.pb.gz --duration 1ms
```

Expected initially: FAIL with unknown flag.

- [ ] **Step 2: Implement CLI flag**

Add to `main.go`:
- `--gpu-stream-stdin`

When set:
- use `os.Stdin` as the stream input
- allow the same replay-style JSON and profile outputs

- [ ] **Step 3: Update README**

Add a short “Experimental GPU live stream” section with:

```bash
printf '%s\n' \
  '{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":100}' \
  '{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}' \
| go run . --gpu-stream-stdin --gpu-raw-output /tmp/gpu-stream.json --gpu-profile-output /tmp/gpu-stream.pb.gz --duration 1ms
```

Be explicit that this is:
- a live contract-ingestion path
- still not a real vendor runtime backend

- [ ] **Step 4: Run verification**

Run:

```bash
go test ./gpu/backend/stream ./gpu/codec ./gpu/... ./perfagent/... -v
make test-unit
printf '%s\n' \
  '{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":100}' \
  '{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}' \
| go run . --gpu-stream-stdin --gpu-raw-output /tmp/gpu-stream.json --gpu-profile-output /tmp/gpu-stream.pb.gz --duration 1ms
```

Expected:
- tests PASS
- command exits successfully
- `/tmp/gpu-stream.json` and `/tmp/gpu-stream.pb.gz` exist

- [ ] **Step 5: Commit**

```bash
git add main.go README.md
git commit -m "cli: add experimental gpu live stream mode"
```

---

## Plan Notes

- This phase deliberately avoids real vendor SDKs; the stream source is the bridge from replay to live ingestion.
- Replay stays important after this phase because it remains the deterministic backend for regression tests.
- If stdin proves too limiting, the next plan can swap transport plumbing to named pipes or Unix sockets without reworking the `gpu` core.

## Review Focus

When reviewing this plan, focus on:

- whether NDJSON-on-a-live-reader is the right first transport
- whether the codec and backend split is clean enough
- whether `perfagent` stream mode stays honest about scope
- whether the plan preserves replay as the deterministic test path

---

Plan complete and saved to `docs/superpowers/plans/2026-04-25-gpu-live-event-source.md`. Ready to execute?
