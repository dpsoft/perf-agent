# Unwind-Auto Benchmark Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a repo-resident benchmark suite that measures `--unwind dwarf` (= `--unwind auto`) startup cost — corpus layer for per-binary `ehcompile.Compile` cost, scenario layer for end-to-end `newSession` cost — with per-binary breakdown so the right `--unwind auto` refinement (Option A1/A2/B from `docs/unwind-auto-refinement-design.md`) can be chosen on data.

**Architecture:** Two layers. Corpus = extended `go test -bench` in `unwind/ehcompile/ehcompile_bench_test.go`. Scenario = standalone harness binary in `bench/cmd/scenario/` that spawns a synthetic process fleet, constructs `dwarfagent.Profiler`/`OffCPUProfiler` with optional `Hooks`, times `newSession()`, writes JSON. A separate `bench/cmd/report/` aggregates JSON to markdown. Production code grows one new `dwarfagent.Hooks` struct + new ctor variants (`NewProfilerWithHooks`, `NewOffCPUProfilerWithHooks`) that thread an optional `OnCompile` callback through `TableStore.AcquireBinary`. Existing callers untouched; nil hooks → zero overhead.

**Tech Stack:** Go 1.26.0 (via `GOTOOLCHAIN=auto`), CGO + blazesym (existing build setup; CGO env vars come from the project's Makefile). Existing libraries: `cilium/ebpf`, `unwind/ehmaps`, `unwind/ehcompile`. New file layout: `bench/cmd/{scenario,report}/`, `bench/internal/{fleet,schema}/`. All builds are run via the project Makefile or with `GOTOOLCHAIN=auto go ...` directly.

**Companion spec:** `docs/superpowers/specs/2026-04-25-unwind-auto-benchmark-design.md`.

---

## Build environment recap (read once)

CLAUDE.md and the project Makefile establish these invariants. Any task that builds or tests must respect them:

- Go 1.26.0 is required by `go.mod`. The user's system has 1.25.8; **always prefix Go commands with `GOTOOLCHAIN=auto`** so the toolchain auto-fetches.
- CGO env vars for blazesym are required for any build/test that touches `dwarfagent`, `unwind/ehmaps`, `profile`, or any package that imports them. The Makefile sets these. Use the Makefile (`make build`, `make test-unit`) when possible; if running `go test` directly, prefix with the same env vars the Makefile uses.
- Pure unit tests for packages with no CGO-touching imports (`bench/internal/schema`, `bench/internal/fleet`, `bench/cmd/report`) run cleanly with just `GOTOOLCHAIN=auto go test ./...`. Use this when iterating on those packages — much faster than going through the Makefile.
- Caps for the scenario layer follow the project's `setcap` pattern (`feedback_setcap_over_sudo.md`). The scenario harness binary, once built, is `setcap`'d once and runs without sudo thereafter.

---

## File structure

**New files (created by this plan):**

```
bench/
├── cmd/
│   ├── scenario/
│   │   └── main.go                  # Task 13–14: scenario harness
│   └── report/
│       ├── main.go                  # Task 11–12: aggregator
│       └── main_test.go             # Task 11–12: golden-file tests
├── internal/
│   ├── fleet/
│   │   ├── fleet.go                 # Task 8: Spawn/Wait/Stop
│   │   └── fleet_test.go            # Task 8: unit tests, no caps
│   └── schema/
│       ├── schema.go                # Task 1: shared JSON types
│       └── schema_test.go           # Task 1: roundtrip + version-mismatch
└── README.md                        # Task 13: how to run, caveats

unwind/dwarfagent/
└── hooks.go                         # Task 3: Hooks struct + invoker

docs/superpowers/plans/
└── 2026-04-25-unwind-auto-benchmark.md   # this file
```

**Files modified by this plan:**

```
unwind/ehcompile/ehcompile.go             # Task 2: Compile returns ehFrameBytes
unwind/ehcompile/ehcompile_test.go        # Task 2: update 5 callers
unwind/ehcompile/ehcompile_bench_test.go  # Task 2 + 7: update callers, add new bench cases
unwind/ehmaps/store.go                    # Task 4: TableStore.OnCompile field + invocation
test/integration_test.go                  # Task 2: update 1 caller
unwind/dwarfagent/common.go               # Task 5: thread hooks to NewTableStore
unwind/dwarfagent/agent.go                # Task 5: NewProfilerWithHooks + AttachStats
unwind/dwarfagent/offcpu.go               # Task 5: NewOffCPUProfilerWithHooks + AttachStats
unwind/dwarfagent/agent_test.go           # Task 6: caps-gated hook integration test (or new file)
Makefile                                  # Task 13: bench-corpus + bench-scenarios
```

---

## Task 1: Shared schema package (`bench/internal/schema/`)

**Why first:** No dependencies. Pure data + JSON. Used by both Task 11 (report tool) and Task 13 (scenario harness).

**Files:**
- Create: `bench/internal/schema/schema.go`
- Test: `bench/internal/schema/schema_test.go`

- [ ] **Step 1: Write `schema.go`**

```go
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// SchemaVersion is bumped when the JSON layout changes incompatibly.
const SchemaVersion = 1

// Document is the top-level JSON object written per scenario run.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Scenario      string    `json:"scenario"`
	Config        Config    `json:"config"`
	System        System    `json:"system"`
	StartedAt     time.Time `json:"started_at"`
	Runs          []Run     `json:"runs"`
}

type Config struct {
	Processes   int            `json:"processes"`
	Runs        int            `json:"runs"`
	DropCache   bool           `json:"drop_cache"`
	WorkloadMix map[string]int `json:"workload_mix,omitempty"`
}

type System struct {
	Kernel          string `json:"kernel"`
	CPUModel        string `json:"cpu_model"`
	NCPU            int    `json:"ncpu"`
	GoVersion       string `json:"go_version"`
	PerfAgentCommit string `json:"perf_agent_commit"`
}

type Run struct {
	RunN                int      `json:"run_n"`
	TotalMs             float64  `json:"total_ms"`
	PIDCount            int      `json:"pid_count"`
	DistinctBinaryCount int      `json:"distinct_binary_count"`
	PerBinary           []Binary `json:"per_binary"`
}

type Binary struct {
	Path         string  `json:"path"`
	BuildID      string  `json:"build_id"`
	EhFrameBytes int     `json:"eh_frame_bytes"`
	CompileMs    float64 `json:"compile_ms"`
}

// SortPerBinary sorts each Run's PerBinary by CompileMs descending so a
// human reader sees hot binaries at the top.
func (d *Document) SortPerBinary() {
	for i := range d.Runs {
		sort.Slice(d.Runs[i].PerBinary, func(a, b int) bool {
			return d.Runs[i].PerBinary[a].CompileMs > d.Runs[i].PerBinary[b].CompileMs
		})
	}
}

// Write encodes d to w as indented JSON, stamping SchemaVersion and
// sorting per_binary descending by compile_ms.
func Write(w io.Writer, d *Document) error {
	d.SchemaVersion = SchemaVersion
	d.SortPerBinary()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// ErrSchemaMismatch is returned by Read when the input's schema_version
// does not match the build's SchemaVersion.
var ErrSchemaMismatch = errors.New("schema version mismatch")

// Read decodes a Document from r. Returns ErrSchemaMismatch if the
// schema_version field doesn't match this package's SchemaVersion.
func Read(r io.Reader) (*Document, error) {
	var d Document
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, err
	}
	if d.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrSchemaMismatch, d.SchemaVersion, SchemaVersion)
	}
	return &d, nil
}
```

- [ ] **Step 2: Write `schema_test.go`**

```go
package schema

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRoundtrip(t *testing.T) {
	in := &Document{
		Scenario:  "system-wide-mixed",
		Config:    Config{Processes: 30, Runs: 5, WorkloadMix: map[string]int{"go": 10}},
		System:    System{Kernel: "6.19", NCPU: 16, GoVersion: "go1.26.0"},
		StartedAt: time.Date(2026, 4, 25, 19, 30, 0, 0, time.UTC),
		Runs: []Run{
			{RunN: 1, TotalMs: 3214.7, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []Binary{
					{Path: "/lib/libc.so", BuildID: "abc", EhFrameBytes: 31420, CompileMs: 12.3},
					{Path: "/bin/foo", BuildID: "def", EhFrameBytes: 9000, CompileMs: 50.0},
				}},
		},
	}

	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if out.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", out.SchemaVersion, SchemaVersion)
	}
	if out.Scenario != in.Scenario {
		t.Errorf("Scenario = %q, want %q", out.Scenario, in.Scenario)
	}
	if len(out.Runs) != 1 || len(out.Runs[0].PerBinary) != 2 {
		t.Fatalf("Runs/PerBinary shape mismatch: %#v", out.Runs)
	}
	// Sort: highest compile_ms first.
	if out.Runs[0].PerBinary[0].Path != "/bin/foo" {
		t.Errorf("PerBinary[0].Path = %q, want /bin/foo (sorted desc by compile_ms)",
			out.Runs[0].PerBinary[0].Path)
	}
}

func TestSchemaMismatch(t *testing.T) {
	const wrong = `{"schema_version": 999, "scenario": "x"}`
	_, err := Read(strings.NewReader(wrong))
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
	}
}
```

- [ ] **Step 3: Run the tests**

```bash
GOTOOLCHAIN=auto go test ./bench/internal/schema/...
```

Expected output: `ok  	github.com/dpsoft/perf-agent/bench/internal/schema  0.0XXs` with both tests passing.

- [ ] **Step 4: Commit**

```bash
git add bench/internal/schema/
git commit -m "bench: add shared schema package for benchmark JSON output"
```

---

## Task 2: Extend `ehcompile.Compile` to return `.eh_frame` size

**Why now:** The hook payload includes `ehFrameBytes`. The cleanest source is from inside `Compile` itself — the function already reads the section. Changing the signature requires updating ~10 call sites in this repo (no external consumers — `unwind/ehcompile` is internal). One-shot diff, then it's done.

**Files:**
- Modify: `unwind/ehcompile/ehcompile.go:25-77`
- Modify: `unwind/ehcompile/ehcompile_test.go` (5 callers)
- Modify: `unwind/ehcompile/ehcompile_bench_test.go` (3 callers; this task just keeps them green — Task 7 adds new cases)
- Modify: `unwind/ehmaps/store.go:101`
- Modify: `test/integration_test.go:1046`

- [ ] **Step 1: Update `Compile` signature in `unwind/ehcompile/ehcompile.go`**

Change the function signature on line 25 from:

```go
func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, err error) {
```

to:

```go
// Compile reads the ELF at elfPath and produces flat CFI + Classification
// tables, plus the size in bytes of the ELF's .eh_frame section. Both
// slices are sorted by PCStart. Adjacent rows with identical rules are
// coalesced at emission time.
//
// ehFrameBytes is the raw .eh_frame section size before parsing — useful
// for cost analysis (per-byte compile rate) and observability hooks. It
// is reported even on parse errors after the section has been read; if
// the section is missing entirely (ErrNoEHFrame), ehFrameBytes is 0.
//
// The ELF's machine type (x86_64 vs aarch64) is auto-detected and the
// appropriate archInfo is used for register-number translation.
//
// Not safe for concurrent calls per instance; callers should serialize.
func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, ehFrameBytes int, err error) {
```

Update the function body. All four early returns become `return nil, nil, 0, err` (or 0 + the error). Capture `len(data)` into `ehFrameBytes` after `sec.Data()` succeeds. The successful return becomes `return allEntries, allClasses, ehFrameBytes, nil`. Concretely:

```go
func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, ehFrameBytes int, err error) {
	f, err := elf.Open(elfPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("open elf: %w", err)
	}
	defer func() { _ = f.Close() }()

	arch, err := archFromELFMachine(f.Machine)
	if err != nil {
		return nil, nil, 0, err
	}

	sec := f.Section(".eh_frame")
	if sec == nil {
		return nil, nil, 0, ErrNoEHFrame
	}
	data, err := sec.Data()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read .eh_frame: %w", err)
	}
	ehFrameBytes = len(data)
	sectionPos := sec.Addr

	var allEntries []CFIEntry
	var allClasses []Classification

	err = walkEHFrame(data, sectionPos, func(off uint64, c *cie, fd *fde) error {
		if fd == nil {
			return nil
		}
		interp := newInterpreter(fd.cie, arch)
		if err := interp.run(fd.initialLocation, fd.initialLocation, fd.cie.initialInstructions); err != nil {
			return fmt.Errorf("CIE init at PC 0x%x: %w", fd.initialLocation, err)
		}
		interp.lastEmittedPC = fd.initialLocation
		if err := interp.run(fd.initialLocation, fd.initialLocation+fd.addressRange, fd.instructions); err != nil {
			return fmt.Errorf("FDE at PC 0x%x: %w", fd.initialLocation, err)
		}
		allEntries = append(allEntries, interp.entries...)
		allClasses = append(allClasses, interp.classifications...)
		return nil
	})
	if err != nil {
		return nil, nil, ehFrameBytes, err
	}

	sort.Slice(allEntries, func(i, j int) bool { return allEntries[i].PCStart < allEntries[j].PCStart })
	sort.Slice(allClasses, func(i, j int) bool { return allClasses[i].PCStart < allClasses[j].PCStart })

	return allEntries, allClasses, ehFrameBytes, nil
}
```

- [ ] **Step 2: Update callers in `unwind/ehcompile/ehcompile_test.go`**

Five callers, all pre-Task-2 of shape `entries, classes, err := Compile(...)` or `_, _, err := Compile(...)`. Update each to add a third return value (`ehFrameBytes`), discarded with `_` unless the test wants to assert on it.

Concretely, for each call, change:
- Line 25: `entries, classes, err := Compile(elfPath)` → `entries, classes, _, err := Compile(elfPath)`
- Line 48: `_, _, err := Compile("/dev/null")` → `_, _, _, err := Compile("/dev/null")`
- Line 56: `entries, classes, err := Compile("/bin/true")` → `entries, classes, _, err := Compile("/bin/true")`
- Line 94: `entries, classes, err := Compile(path)` → `entries, classes, _, err := Compile(path)`
- Line 117: `entries, _, err := Compile(path)` → `entries, _, _, err := Compile(path)`

- [ ] **Step 3: Update callers in `unwind/ehcompile/ehcompile_bench_test.go`**

Three callers, all `_, _, err := Compile(...)`. Change each to `_, _, _, err := Compile(...)`:
- Line 15
- Line 28
- Line 41

(Task 7 will add new bench cases; this step just keeps the existing ones compiling.)

- [ ] **Step 4: Update caller in `unwind/ehmaps/store.go:101`**

Change:
```go
entries, classifications, err := ehcompile.Compile(binPath)
```
to:
```go
entries, classifications, _, err := ehcompile.Compile(binPath)
```

(Task 4 will replace the `_` with a captured variable when wiring the OnCompile hook.)

- [ ] **Step 5: Update caller in `test/integration_test.go:1046`**

Change:
```go
entries, classifications, err := ehcompile.Compile(binPath)
```
to:
```go
entries, classifications, _, err := ehcompile.Compile(binPath)
```

- [ ] **Step 6: Verify nothing else calls `Compile`**

```bash
GOTOOLCHAIN=auto grep -rn "ehcompile\.Compile\b\|^\s*\(_, _, err\|entries, classes, err\|entries, _, err\|entries, classifications, err\) := Compile(" --include='*.go'
```

(The Grep tool is preferred for this; the engineer running the plan should use Grep with `pattern: ehcompile\.Compile|\bCompile\(` and `glob: *.go` and verify only the call sites above appear.)

- [ ] **Step 7: Build + unit tests pass**

```bash
GOTOOLCHAIN=auto make test-unit
```

Expected: all packages pass (7+ packages, 0 failures). The integration test file builds even though it's not run (it's in a separate module under `test/`; `make test-unit` doesn't run it but `make build` for the main module wouldn't compile `test/` either — verify the integration module separately):

```bash
cd test && GOTOOLCHAIN=auto CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go vet ./...
```

Expected: no errors.

- [ ] **Step 8: Commit**

```bash
git add unwind/ehcompile/ehcompile.go unwind/ehcompile/ehcompile_test.go unwind/ehcompile/ehcompile_bench_test.go unwind/ehmaps/store.go test/integration_test.go
git commit -m "ehcompile: return .eh_frame section size from Compile"
```

---

## Task 3: `dwarfagent.Hooks` struct (`unwind/dwarfagent/hooks.go`)

**Files:**
- Create: `unwind/dwarfagent/hooks.go`
- Test: extend `unwind/dwarfagent/common_test.go` (or create a new test file `unwind/dwarfagent/hooks_test.go`)

- [ ] **Step 1: Write `hooks.go`**

```go
package dwarfagent

import (
	"log"
	"time"
)

// Hooks is an optional observation surface for the dwarf-mode profilers
// (Profiler and OffCPUProfiler). All fields may be nil; the profiler
// nil-checks each before invoking. Hooks must not panic — if they do,
// the call site recovers and logs at debug level. Hooks are observers,
// not gatekeepers; they cannot fail or alter the operation.
type Hooks struct {
	// OnCompile fires after each successful CFI table compile in
	// TableStore.AcquireBinary. Path is the binary or shared library
	// path; buildID may be empty if the ELF lacks a NT_GNU_BUILD_ID
	// note. ehFrameBytes is the raw .eh_frame section size in bytes.
	// dur is the wall-clock duration of the ehcompile.Compile call.
	OnCompile func(path, buildID string, ehFrameBytes int, dur time.Duration)
}

// onCompileFunc returns a non-nil callback safe to invoke from anywhere.
// If h or h.OnCompile is nil, returns a no-op. The returned function
// recovers from panics inside the user-supplied OnCompile and logs them
// (observers must not break operations).
func (h *Hooks) onCompileFunc() func(path, buildID string, ehFrameBytes int, dur time.Duration) {
	if h == nil || h.OnCompile == nil {
		return func(string, string, int, time.Duration) {}
	}
	user := h.OnCompile
	return func(path, buildID string, ehFrameBytes int, dur time.Duration) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("dwarfagent: Hooks.OnCompile panic recovered: %v (binary=%s)", r, path)
			}
		}()
		user(path, buildID, ehFrameBytes, dur)
	}
}
```

- [ ] **Step 2: Write `hooks_test.go` (in `unwind/dwarfagent/`)**

```go
package dwarfagent

import (
	"testing"
	"time"
)

func TestHooksNilSafe(t *testing.T) {
	var h *Hooks
	cb := h.onCompileFunc()
	cb("p", "b", 0, 0) // must not panic on nil receiver
}

func TestHooksNilFieldSafe(t *testing.T) {
	h := &Hooks{}
	cb := h.onCompileFunc()
	cb("p", "b", 0, 0) // must not panic when OnCompile is nil
}

func TestHooksCallbackFires(t *testing.T) {
	var got struct {
		path    string
		buildID string
		bytes   int
		dur     time.Duration
		fired   bool
	}
	h := &Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			got.path, got.buildID, got.bytes, got.dur, got.fired =
				path, buildID, ehFrameBytes, dur, true
		},
	}
	h.onCompileFunc()("/bin/foo", "abc", 1234, 5*time.Millisecond)
	if !got.fired {
		t.Fatal("OnCompile did not fire")
	}
	if got.path != "/bin/foo" || got.buildID != "abc" || got.bytes != 1234 || got.dur != 5*time.Millisecond {
		t.Errorf("got = %+v", got)
	}
}

func TestHooksRecoversFromPanic(t *testing.T) {
	h := &Hooks{
		OnCompile: func(string, string, int, time.Duration) {
			panic("boom")
		},
	}
	// Must not propagate the panic.
	h.onCompileFunc()("p", "b", 0, 0)
}
```

- [ ] **Step 3: Run tests**

```bash
GOTOOLCHAIN=auto make test-unit
```

Expected: `unwind/dwarfagent` package passes including the four new tests. (The package's existing tests may or may not run via `make test-unit`; if `dwarfagent` requires CGO setup, run via `make` rather than raw `go test`.)

- [ ] **Step 4: Commit**

```bash
git add unwind/dwarfagent/hooks.go unwind/dwarfagent/hooks_test.go
git commit -m "dwarfagent: add Hooks struct with nil-safe OnCompile callback"
```

---

## Task 4: Wire `OnCompile` into `TableStore.AcquireBinary`

**Why now:** Task 3 defined the callback shape; Task 2 made the size available. Now `TableStore` has to fire the callback at the right place.

**Approach:** `TableStore` holds an `OnCompile` field of type `func(path, buildID string, ehFrameBytes int, dur time.Duration)`. The field is set via a method (avoids breaking `NewTableStore`'s positional signature — `dwarfagent` calls `NewTableStore(...)` then `store.SetOnCompile(...)`). In `AcquireBinary`, time the `ehcompile.Compile` call and invoke `s.OnCompile` after a successful compile (skip on error).

**Files:**
- Modify: `unwind/ehmaps/store.go`
- Test: `unwind/ehmaps/store_test.go` (existing file; if absent, create one and add to it later)

- [ ] **Step 1: Add `OnCompile` field + setter to `TableStore`**

In `unwind/ehmaps/store.go`, change the `TableStore` struct (around line 65) to add an `OnCompile` field. After `NewTableStore`, add a setter method. Concrete diff:

```go
// TableStore owns the BPF-side cfi_* outer maps and composes refcount
// tracking with actual map population. Wraps Populate{CFI,Classification} with refcounting so callers don't
// hand-manage table lifetimes.
type TableStore struct {
	CFIRules          *ebpf.Map
	CFILengths        *ebpf.Map
	CFIClassification *ebpf.Map
	CFIClassLengths   *ebpf.Map

	rc *RefcountTable

	// OnCompile, if non-nil, is invoked after each successful first-time
	// compile in AcquireBinary. Nil means no observation. Set via
	// SetOnCompile after construction.
	onCompile func(path, buildID string, ehFrameBytes int, dur time.Duration)
}
```

Add the setter directly below `NewTableStore`:

```go
// SetOnCompile installs an observer callback that fires after each
// successful CFI compile in AcquireBinary. Pass nil to disable.
// Not safe to call concurrently with AcquireBinary; set once at
// construction time.
func (s *TableStore) SetOnCompile(fn func(path, buildID string, ehFrameBytes int, dur time.Duration)) {
	s.onCompile = fn
}
```

- [ ] **Step 2: Time + invoke in `AcquireBinary`**

In `AcquireBinary` (around line 91), wrap the `ehcompile.Compile` call with timing and capture `ehFrameBytes`. Concrete diff — replace the existing `// First reference for this tableID — compile + install.` block (lines 100-105):

```go
	// First reference for this tableID — compile + install.
	t0 := time.Now()
	entries, classifications, ehFrameBytes, err := ehcompile.Compile(binPath)
	compileDur := time.Since(t0)
	if err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("ehcompile %s: %w", binPath, err)
	}
	if s.onCompile != nil {
		s.onCompile(binPath, buildID, ehFrameBytes, compileDur)
	}
```

Add `"time"` to the imports if it isn't already.

- [ ] **Step 3: Add a unit test that exercises the callback**

This test needs to call `AcquireBinary` against a real ELF. The simplest victim is `/bin/true` (used in existing tests) or one of the testdata ELFs in `unwind/ehcompile/testdata/`. We don't need real BPF maps — `AcquireBinary` calls `PopulateCFI` which writes into BPF maps, and that requires real maps. So either we:

(a) Skip `AcquireBinary` and unit-test a tiny helper extracted from the timing path. Cleaner but more refactor.

(b) Write the test as caps-gated, requiring the real BPF program. Heavier setup.

(c) Test the simpler "OnCompile is invoked once per first-time compile" via a focused integration in Task 6 (which already plans to exercise the hook end-to-end through dwarfagent).

**Choose (c).** This task has no new test of its own; the callback wiring is exercised by Task 6's caps-gated test. The hook plumbing's correctness is already covered by Task 3's nil-safety and panic-recovery tests; what remains is "fires at the right call site," which Task 6 verifies.

- [ ] **Step 4: Verify build still passes**

```bash
GOTOOLCHAIN=auto make test-unit
```

Expected: all packages still pass (no functional change yet — `onCompile` is nil for all current callers, including the existing `dwarfagent.newSession`). Task 5 will set it.

- [ ] **Step 5: Commit**

```bash
git add unwind/ehmaps/store.go
git commit -m "ehmaps: add OnCompile callback to TableStore for benchmark observation"
```

---

## Task 5: New `dwarfagent` ctor variants + `AttachStats`

**Goal:** `NewProfilerWithHooks` and `NewOffCPUProfilerWithHooks` exist; existing `NewProfiler` / `NewOffCPUProfiler` delegate with `nil`. `Profiler.AttachStats()` and `OffCPUProfiler.AttachStats()` expose the PID/binary counts already returned from `AttachAllProcesses` / `AttachAllMappings`.

**Files:**
- Modify: `unwind/dwarfagent/common.go` (`newSession` accepts hooks, plumbs to TableStore + records stats)
- Modify: `unwind/dwarfagent/agent.go` (NewProfilerWithHooks + AttachStats)
- Modify: `unwind/dwarfagent/offcpu.go` (NewOffCPUProfilerWithHooks + AttachStats)

- [ ] **Step 1: Update `session` struct + `newSession` signature**

In `unwind/dwarfagent/common.go`:

(a) Add fields to `session`:
```go
type session struct {
	// ... existing fields ...

	attachStats attachStats
}

type attachStats struct {
	pidCount    int
	binaryCount int
}
```

(b) Change `newSession` signature (line 83) to accept `hooks *Hooks`:
```go
func newSession(objs sessionObjs, pid int, systemWide bool, cpus []uint, tags []string, logPrefix string, hooks *Hooks) (*session, error) {
```

(c) Right after `store := ehmaps.NewTableStore(...)`, install the hook:
```go
	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	if hooks != nil && hooks.OnCompile != nil {
		store.SetOnCompile(hooks.onCompileFunc())
	}
```

(d) Capture the attach return values into `attachStats`. Replace the `if systemWide { ... } else { ... }` block (lines 95-109) with:
```go
	var stats attachStats
	if systemWide {
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			log.Printf("%s: AttachAllProcesses: %v (continuing; walker uses FP path for unattached binaries)", logPrefix, err)
		} else {
			log.Printf("%s: attached %d distinct binaries across %d PIDs", logPrefix, nTables, nPIDs)
			stats.pidCount = nPIDs
			stats.binaryCount = nTables
		}
	} else {
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			log.Printf("%s: AttachAllMappings(pid=%d): %v (continuing; walker uses FP path for unattached binaries)", logPrefix, pid, err)
		} else {
			log.Printf("%s: attached %d binaries from /proc/%d/maps", logPrefix, n, pid)
			stats.pidCount = 1
			stats.binaryCount = n
		}
	}
```

(e) Add `attachStats: stats,` to the returned `&session{...}` literal at the bottom of `newSession`.

- [ ] **Step 2: Update `unwind/dwarfagent/agent.go` — add `NewProfilerWithHooks` + `AttachStats`**

Just above `NewProfiler` (line 47), add:

```go
// NewProfilerWithHooks is the variant of NewProfiler that accepts an
// optional observation surface. Pass nil hooks for the same behavior
// as NewProfiler. Hooks are non-load-bearing — see Hooks docs.
func NewProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks) (*Profiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadPerfDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent", hooks)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	p := &Profiler{session: sess, sampleRate: sampleRate}
	if err := p.attachPerfEvents(objs.Program(), cpus, sampleRate); err != nil {
		_ = p.close()
		return nil, err
	}

	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateCPUSample)

	return p, nil
}
```

Then **shrink the existing `NewProfiler` to delegate** — replace its body (lines 47-79) with:

```go
// NewProfiler loads the perf_dwarf BPF program, wires ehmaps via
// newSession, opens per-CPU perf events at sampleRate Hz, attaches
// the BPF program to each, and starts the ringbuf reader + tracker
// goroutines.
//
// On error, every resource created is closed before returning.
// Callers should NOT call Close on a Profiler they received as (nil, err).
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	return NewProfilerWithHooks(pid, systemWide, cpus, tags, sampleRate, nil)
}
```

Add at the bottom of `agent.go`:

```go
// AttachStats returns the (pidCount, binaryCount) recorded by newSession's
// initial AttachAllProcesses/AttachAllMappings call. For per-PID profilers,
// pidCount is always 1. For system-wide, pidCount is the number of distinct
// PIDs successfully scanned. binaryCount is the number of distinct binaries
// (by build-id) compiled into the BPF maps.
//
// Returns (0, 0) if the initial attach failed (the agent still ran in
// FP-only mode for unattached binaries).
func (p *Profiler) AttachStats() (pidCount, binaryCount int) {
	return p.attachStats.pidCount, p.attachStats.binaryCount
}
```

- [ ] **Step 3: Update `unwind/dwarfagent/offcpu.go` — same pattern**

Above the existing `NewOffCPUProfiler` (line 30), add `NewOffCPUProfilerWithHooks`:

```go
// NewOffCPUProfilerWithHooks is the variant of NewOffCPUProfiler that
// accepts an optional observation surface. Pass nil hooks for the same
// behavior as NewOffCPUProfiler.
func NewOffCPUProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, hooks *Hooks) (*OffCPUProfiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadOffCPUDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent (offcpu)", hooks)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	tpLink, err := link.AttachTracing(link.TracingOptions{Program: objs.Program()})
	if err != nil {
		_ = sess.close()
		return nil, fmt.Errorf("attach tp_btf: %w", err)
	}

	p := &OffCPUProfiler{session: sess, link: tpLink}
	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateOffCPUSample)

	return p, nil
}
```

Then **shrink the existing `NewOffCPUProfiler`** (lines 30-63) to delegate:

```go
// NewOffCPUProfiler loads the offcpu_dwarf BPF program, wires ehmaps
// via newSession, attaches the tp_btf program via link.AttachTracing,
// and starts the ringbuf reader + tracker goroutines.
//
// On error, every resource created is closed before returning.
// Callers should NOT call Close on an OffCPUProfiler they received
// as (nil, err).
func NewOffCPUProfiler(pid int, systemWide bool, cpus []uint, tags []string) (*OffCPUProfiler, error) {
	return NewOffCPUProfilerWithHooks(pid, systemWide, cpus, tags, nil)
}
```

Add at the bottom of `offcpu.go`:

```go
// AttachStats returns the (pidCount, binaryCount) recorded by newSession's
// initial AttachAllProcesses/AttachAllMappings call. See
// (*Profiler).AttachStats for full semantics.
func (p *OffCPUProfiler) AttachStats() (pidCount, binaryCount int) {
	return p.attachStats.pidCount, p.attachStats.binaryCount
}
```

- [ ] **Step 4: Update internal `newSession` callers**

`unwind/dwarfagent/common_test.go` and any other in-package callers of `newSession` need `nil` appended as the new last argument. Concrete approach: grep first.

```bash
GOTOOLCHAIN=auto grep -rn "newSession(" --include='*.go' unwind/dwarfagent/
```

For each match where the call passes 6 args (pre-hooks signature), append `, nil` so it passes 7. Test files may have helpers that wrap `newSession`; update those too.

- [ ] **Step 5: Build + unit tests**

```bash
GOTOOLCHAIN=auto make test-unit
```

Expected: passes.

```bash
GOTOOLCHAIN=auto make build
```

Expected: produces `./perf-agent` binary, ~18MB.

- [ ] **Step 6: Commit**

```bash
git add unwind/dwarfagent/agent.go unwind/dwarfagent/offcpu.go unwind/dwarfagent/common.go unwind/dwarfagent/common_test.go
git commit -m "dwarfagent: add NewProfilerWithHooks/NewOffCPUProfilerWithHooks + AttachStats"
```

---

## Task 6: Caps-gated hook integration test

**Goal:** Verify `OnCompile` actually fires through the full chain (`NewProfilerWithHooks` → `newSession` → `TableStore.AcquireBinary` → `ehcompile.Compile` → callback). This is the only end-to-end test of the hook plumbing.

**Approach:** Use the existing `unwind/dwarfagent/agent_test.go` (or add a focused new file) with the cap-aware skip pattern from `feedback_cap_aware_test_gates.md`. Spawn a small workload, wait until it's running, construct a hooked profiler with `--pid <workload>`, assert `OnCompile` fired ≥1 time and that the recorded values look sane.

**Files:**
- Modify (or create): `unwind/dwarfagent/agent_test.go` — add `TestNewProfilerWithHooks_FiresOnCompile`

- [ ] **Step 1: Match the existing cap-skip pattern from `unwind/dwarfagent/agent_test.go`**

The project uses `kernel.org/pub/linux/libs/security/libcap/cap`. Existing pattern (from `agent_test.go:21-27`):

```go
import (
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

if os.Getuid() != 0 {
	caps := cap.GetProc()
	have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
	if !have {
		t.Skip("requires root or CAP_BPF")
	}
}
```

The new test uses the same gate. (Optionally factor this into a helper like `requireCaps(t)` in a new `unwind/dwarfagent/test_helpers_test.go`, but for v1 just inline it — only one new test calls it.)

- [ ] **Step 2: Add test**

```go
func TestNewProfilerWithHooks_FiresOnCompile(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}

	// Spawn a tiny workload we can attach to. /usr/bin/sleep is universal
	// enough; on systems without it, fall back to /bin/sleep.
	sleepPath := "/usr/bin/sleep"
	if _, err := os.Stat(sleepPath); err != nil {
		sleepPath = "/bin/sleep"
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for the process to be visible in /proc.
	pid := cmd.Process.Pid
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/maps", pid)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var (
		mu     sync.Mutex
		fires  int
		paths  []string
		bytes  int
		anyDur bool
	)
	hooks := &dwarfagent.Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			fires++
			paths = append(paths, path)
			bytes += ehFrameBytes
			if dur > 0 {
				anyDur = true
			}
		},
	}

	prof, err := dwarfagent.NewProfilerWithHooks(pid, false, []uint{0}, nil, 99, hooks)
	if err != nil {
		t.Fatalf("NewProfilerWithHooks: %v", err)
	}
	t.Cleanup(func() { _ = prof.Close() })

	mu.Lock()
	defer mu.Unlock()
	if fires == 0 {
		t.Fatalf("OnCompile never fired (expected ≥1 for sleep + its shared libs)")
	}
	if !anyDur {
		t.Errorf("all OnCompile durations were zero — timing wrap may not be in effect")
	}
	if bytes == 0 {
		t.Errorf("OnCompile total ehFrameBytes was 0 — section size not propagating")
	}
	t.Logf("OnCompile fired %d times across %d binaries, total .eh_frame bytes %d", fires, len(paths), bytes)
}
```

The imports the test needs (add to top of file if missing — note `agent_test.go` already uses `package dwarfagent_test`, so the import of `dwarfagent` is already present):

```go
import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)
```

- [ ] **Step 3: Build the test binary and setcap it**

```bash
GOTOOLCHAIN=auto CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test -c -o /tmp/dwarfagent.test ./unwind/dwarfagent/
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep /tmp/dwarfagent.test
```

- [ ] **Step 4: Run the new test**

```bash
/tmp/dwarfagent.test -test.v -test.run TestNewProfilerWithHooks_FiresOnCompile
```

Expected: PASS. Output includes the `t.Logf` line showing fires > 0.

If the test skips (caps not set), redo Step 3.

- [ ] **Step 5: Commit**

```bash
git add unwind/dwarfagent/agent_test.go
git commit -m "dwarfagent: add caps-gated test that NewProfilerWithHooks fires OnCompile"
```

---

## Task 7: Corpus benchmark extensions

**Goal:** Two new benchmark cases in `ehcompile_bench_test.go`:
1. `BenchmarkCompile_LargeRustRelease` — uses `test/workloads/rust/cpu_bound`'s release binary if present.
2. `BenchmarkCompile_LibPython` — uses the system's `libpython3.X.so` if present.

Both use `b.Skip` if the fixture isn't available.

**Files:**
- Modify: `unwind/ehcompile/ehcompile_bench_test.go`

- [ ] **Step 1: Inspect the existing bench file to follow its style**

Read `unwind/ehcompile/ehcompile_bench_test.go` end-to-end so the new benchmarks match the existing pattern (uses `testing.B`, calls `b.ResetTimer`, etc.).

- [ ] **Step 2: Append new benchmarks**

At the end of `ehcompile_bench_test.go`, append:

```go
func BenchmarkCompile_LargeRustRelease(b *testing.B) {
	// Locate the test workload binary built by `make test-workloads`.
	candidates := []string{
		"../../test/workloads/rust/cpu_bound/target/release/cpu_bound",
		"../../test/workloads/rust/cpu_bound",
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		b.Skipf("test/workloads/rust/cpu_bound release binary not found; run `make test-workloads`")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, _, ehFrameBytes, err := Compile(path)
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			b.ReportMetric(float64(ehFrameBytes), "eh_frame_bytes/op")
			b.ReportMetric(float64(len(entries)), "entries/op")
		}
	}
}

func BenchmarkCompile_LibPython(b *testing.B) {
	// Find any libpython3.X.so on the system. Glob across common locations.
	candidates := []string{}
	matches, _ := filepath.Glob("/lib/x86_64-linux-gnu/libpython3.*.so*")
	candidates = append(candidates, matches...)
	matches, _ = filepath.Glob("/lib64/libpython3.*.so*")
	candidates = append(candidates, matches...)
	matches, _ = filepath.Glob("/usr/lib64/libpython3.*.so*")
	candidates = append(candidates, matches...)

	var path string
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			// Skip symlinks pointing nowhere; resolve.
			if real, err := filepath.EvalSymlinks(c); err == nil {
				path = real
				break
			}
		}
	}
	if path == "" {
		b.Skip("no libpython3.X.so found in standard locations")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, _, ehFrameBytes, err := Compile(path)
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			b.ReportMetric(float64(ehFrameBytes), "eh_frame_bytes/op")
			b.ReportMetric(float64(len(entries)), "entries/op")
		}
	}
}
```

Add the imports if not already present:
```go
import (
	"os"
	"path/filepath"
	"testing"
)
```

- [ ] **Step 3: Run the benchmarks**

```bash
GOTOOLCHAIN=auto go test -bench=. -benchmem -run=^$ -count=1 ./unwind/ehcompile/...
```

Expected: existing benchmarks still pass; new ones either pass or skip cleanly (no panic, no hang).

- [ ] **Step 4: Commit**

```bash
git add unwind/ehcompile/ehcompile_bench_test.go
git commit -m "ehcompile: add LargeRustRelease and LibPython corpus benchmarks"
```

---

## Task 8: Fleet driver (`bench/internal/fleet/`)

**Goal:** A small package that spawns N test workload processes, waits until they're observably running, and tears them down cleanly. No caps required.

**Files:**
- Create: `bench/internal/fleet/fleet.go`
- Create: `bench/internal/fleet/fleet_test.go`

- [ ] **Step 1: Write `fleet.go`**

```go
// Package fleet spawns and manages a set of child processes used as a
// fixture for the perf-agent scenario benchmark. It is not safe for
// production use — error handling assumes a controlled test environment.
package fleet

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Opts configures Spawn. Mix maps language name to the count of
// processes to launch (e.g. {"go": 10, "python": 10, "rust": 5, "node": 5}).
// WorkloadDir points to the test/workloads root; Spawn looks up
// {WorkloadDir}/{lang}/cpu_bound (or io_bound as fallback) for each
// entry in Mix.
type Opts struct {
	Mix         map[string]int
	WorkloadDir string

	// StartupTimeout bounds the per-process wait for visibility in
	// /proc/<pid>/stat. 10s is a sensible default.
	StartupTimeout time.Duration
}

// Fleet is a running set of child processes. Stop is idempotent.
type Fleet struct {
	procs []*exec.Cmd

	mu      sync.Mutex
	stopped bool
}

// Spawn launches Mix processes and returns a Fleet. On any spawn failure,
// already-launched processes are killed and the error is returned.
//
// Per-language launch convention (matches Makefile `test-workloads`):
//   - go:     {dir}/go/cpu_bound (build artifact, executable)
//   - rust:   {dir}/rust/target/release/rust-workload (cargo build artifact)
//   - python: python3 {dir}/python/cpu_bound.py (interpreter required)
//   - node:   node {dir}/node/cpu_bound.js (interpreter required)
//
// Falls back to io_bound for go/python where cpu_bound is unavailable.
// Rust and Node only ship cpu_bound.
func Spawn(opts Opts) (*Fleet, error) {
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 10 * time.Second
	}
	if opts.WorkloadDir == "" {
		return nil, errors.New("fleet: WorkloadDir must be set")
	}

	f := &Fleet{}
	for lang, count := range opts.Mix {
		argv, err := commandFor(opts.WorkloadDir, lang)
		if err != nil {
			_ = f.Stop()
			return nil, fmt.Errorf("fleet: resolve %s workload: %w", lang, err)
		}
		for i := 0; i < count; i++ {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = nil
			cmd.Stdout = nil
			cmd.Stderr = nil
			// New process group so SIGKILL doesn't leak grandchildren.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				_ = f.Stop()
				return nil, fmt.Errorf("fleet: start %s[%d] (%v): %w", lang, i, argv, err)
			}
			f.procs = append(f.procs, cmd)
		}
	}
	return f, nil
}

// commandFor returns the argv to spawn one workload of the given lang,
// using the build artifacts produced by `make test-workloads`.
func commandFor(dir, lang string) ([]string, error) {
	switch lang {
	case "go":
		for _, variant := range []string{"cpu_bound", "io_bound"} {
			p := filepath.Join(dir, "go", variant)
			if isExecFile(p) {
				return []string{p}, nil
			}
		}
		return nil, fmt.Errorf("no go binary at %s/go/cpu_bound or io_bound (run `make test-workloads`?)", dir)
	case "rust":
		// Cargo.toml's [package].name is "rust-workload".
		p := filepath.Join(dir, "rust", "target", "release", "rust-workload")
		if isExecFile(p) {
			return []string{p}, nil
		}
		return nil, fmt.Errorf("no rust binary at %s (run `make test-workloads`?)", p)
	case "python":
		for _, variant := range []string{"cpu_bound.py", "io_bound.py"} {
			p := filepath.Join(dir, "python", variant)
			if isFile(p) {
				return []string{"python3", p}, nil
			}
		}
		return nil, fmt.Errorf("no python script at %s/python/cpu_bound.py or io_bound.py", dir)
	case "node":
		p := filepath.Join(dir, "node", "cpu_bound.js")
		if isFile(p) {
			return []string{"node", p}, nil
		}
		return nil, fmt.Errorf("no node script at %s", p)
	default:
		return nil, fmt.Errorf("unknown language %q (want go, rust, python, node)", lang)
	}
}

func isExecFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Wait blocks until every spawned process is visible in /proc/<pid>/stat
// (i.e. the kernel has it as a task), or timeout elapses, or any process
// has exited (which is treated as a fatal startup failure).
func (f *Fleet) Wait(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, cmd := range f.procs {
		pid := cmd.Process.Pid
		for {
			// Check exit first — if the process died on us, that's fatal.
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				return fmt.Errorf("fleet: pid=%d exited before reaching ready state", pid)
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("fleet: pid=%d not ready within %s", pid, timeout)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return nil
}

// PIDs returns a snapshot of currently-tracked process IDs.
func (f *Fleet) PIDs() []int {
	out := make([]int, 0, len(f.procs))
	for _, cmd := range f.procs {
		out = append(out, cmd.Process.Pid)
	}
	return out
}

// Stop sends SIGTERM to every process group, waits 1s, then SIGKILLs
// any still alive. Idempotent. Returns the first error encountered;
// always attempts every process.
func (f *Fleet) Stop() error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return nil
	}
	f.stopped = true
	f.mu.Unlock()

	// SIGTERM the whole group of each process.
	for _, cmd := range f.procs {
		if cmd.Process == nil {
			continue
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}

	// Wait up to 1s for graceful exit.
	done := make(chan struct{})
	go func() {
		for _, cmd := range f.procs {
			_ = cmd.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(1 * time.Second):
	}

	// SIGKILL anyone still alive.
	for _, cmd := range f.procs {
		if cmd.Process == nil {
			continue
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
	return nil
}
```

- [ ] **Step 2: Write `fleet_test.go`**

```go
package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeStubGoWorkload creates a temp directory with a fake "go" workload —
// a shell script that sleeps until killed. We test the binary path only;
// python/node would require having the interpreters available, which is
// covered by the integration smoke run in Task 11/12 rather than here.
func makeStubGoWorkload(t *testing.T) (string, map[string]int) {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "go")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(sub, "cpu_bound")
	body := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return dir, map[string]int{"go": 3}
}

func TestSpawnAndStop(t *testing.T) {
	dir, mix := makeStubGoWorkload(t)
	f, err := Spawn(Opts{Mix: mix, WorkloadDir: dir})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got, want := len(f.PIDs()), 3; got != want {
		t.Errorf("PIDs len = %d, want %d", got, want)
	}
	if err := f.Wait(2 * time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := f.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop is idempotent.
	if err := f.Stop(); err != nil {
		t.Fatalf("Stop (second call): %v", err)
	}
	// Confirm the processes are gone.
	for _, pid := range f.PIDs() {
		if _, err := os.Stat("/proc/" + intStr(pid)); err == nil {
			t.Errorf("pid %d still in /proc after Stop", pid)
		}
	}
}

func TestSpawnFailsOnMissingWorkload(t *testing.T) {
	dir := t.TempDir() // empty
	_, err := Spawn(Opts{Mix: map[string]int{"go": 1}, WorkloadDir: dir})
	if err == nil {
		t.Fatal("expected error for missing workload, got nil")
	}
	if !strings.Contains(err.Error(), "no go binary") {
		t.Errorf("error message = %q, want to mention missing go binary", err.Error())
	}
}

// intStr is strconv.Itoa-equivalent without importing strconv into the
// test (keeps the test file imports minimal).
func intStr(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
```

- [ ] **Step 3: Run tests**

```bash
GOTOOLCHAIN=auto go test ./bench/internal/fleet/...
```

Expected: both tests pass.

- [ ] **Step 4: Commit**

```bash
git add bench/internal/fleet/
git commit -m "bench: add fleet driver for spawning synthetic workload processes"
```

---

## Task 9: Report tool — single-file markdown (`bench/cmd/report/`)

**Goal:** A binary that consumes one JSON file and prints a markdown report.

**Files:**
- Create: `bench/cmd/report/main.go`
- Create: `bench/cmd/report/main_test.go`

- [ ] **Step 1: Write `main.go`**

```go
// Command report aggregates one or more bench/cmd/scenario JSON outputs
// into a markdown summary.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
)

func main() {
	var (
		inFlag     stringSlice
		diffArgs   stringSlice
		formatFlag = flag.String("format", "markdown", "output format: markdown | csv")
	)
	flag.Var(&inFlag, "in", "input JSON file (repeatable)")
	flag.Var(&diffArgs, "diff", "two JSON files to diff (use --diff a.json --diff b.json)")
	flag.Parse()

	if *formatFlag != "markdown" && *formatFlag != "csv" {
		fmt.Fprintln(os.Stderr, "format must be markdown or csv")
		os.Exit(2)
	}

	switch {
	case len(diffArgs) == 2:
		runDiff(os.Stdout, string(diffArgs[0]), string(diffArgs[1]), *formatFlag)
	case len(diffArgs) != 0:
		fmt.Fprintln(os.Stderr, "--diff requires exactly two arguments")
		os.Exit(2)
	case len(inFlag) > 0:
		runSummary(os.Stdout, inFlag, *formatFlag)
	default:
		fmt.Fprintln(os.Stderr, "usage: report --in PATH... | --diff A.json --diff B.json")
		os.Exit(2)
	}
}

type stringSlice []string

func (s *stringSlice) String() string         { return fmt.Sprint([]string(*s)) }
func (s *stringSlice) Set(v string) error     { *s = append(*s, v); return nil }

// runSummary writes a markdown report covering each input doc.
func runSummary(w io.Writer, paths []string, format string) {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", p, err)
			os.Exit(3)
		}
		doc, err := schema.Read(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", p, err)
			os.Exit(3)
		}
		writeSummary(w, doc, format)
		fmt.Fprintln(w)
	}
}

func writeSummary(w io.Writer, d *schema.Document, format string) {
	if format != "markdown" {
		fmt.Fprintf(os.Stderr, "csv output is not implemented in v1\n")
		os.Exit(4)
	}

	fmt.Fprintf(w, "# Scenario: `%s`\n\n", d.Scenario)
	fmt.Fprintf(w, "- **Started:** %s\n", d.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "- **Kernel:** %s · **CPU:** %s · **NCPU:** %d · **Go:** %s · **Commit:** %s\n",
		d.System.Kernel, d.System.CPUModel, d.System.NCPU, d.System.GoVersion, d.System.PerfAgentCommit)
	fmt.Fprintf(w, "- **Config:** processes=%d runs=%d drop_cache=%v\n\n",
		d.Config.Processes, d.Config.Runs, d.Config.DropCache)

	// Wall-time stats.
	totals := make([]float64, 0, len(d.Runs))
	for _, r := range d.Runs {
		totals = append(totals, r.TotalMs)
	}
	p50, p95, max := stats(totals)
	fmt.Fprintln(w, "## Wall time (newSession startup)\n")
	fmt.Fprintln(w, "| metric | value (ms) |")
	fmt.Fprintln(w, "|--------|-----------|")
	fmt.Fprintf(w, "| p50 | %.1f |\n", p50)
	fmt.Fprintf(w, "| p95 | %.1f |\n", p95)
	fmt.Fprintf(w, "| max | %.1f |\n", max)
	fmt.Fprintln(w)

	// Top binaries by median compile_ms.
	type agg struct {
		path    string
		buildID string
		bytes   int
		samples []float64
	}
	byKey := map[string]*agg{}
	for _, r := range d.Runs {
		for _, b := range r.PerBinary {
			key := b.BuildID + "|" + b.Path
			a, ok := byKey[key]
			if !ok {
				a = &agg{path: b.Path, buildID: b.BuildID, bytes: b.EhFrameBytes}
				byKey[key] = a
			}
			a.samples = append(a.samples, b.CompileMs)
		}
	}
	rows := make([]*agg, 0, len(byKey))
	for _, a := range byKey {
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool {
		return median(rows[i].samples) > median(rows[j].samples)
	})
	if n := 10; len(rows) > n {
		rows = rows[:n]
	}

	fmt.Fprintln(w, "## Top binaries by median compile_ms\n")
	fmt.Fprintln(w, "| binary | build_id | eh_frame_bytes | median compile_ms |")
	fmt.Fprintln(w, "|--------|----------|----------------|-------------------|")
	for _, a := range rows {
		fmt.Fprintf(w, "| `%s` | `%s` | %d | %.2f |\n", a.path, shortID(a.buildID), a.bytes, median(a.samples))
	}
}

// stats returns p50, p95, max of xs (sorts in place).
func stats(xs []float64) (p50, p95, max float64) {
	if len(xs) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(xs)
	max = xs[len(xs)-1]
	p50 = xs[len(xs)/2]
	idx := int(math.Ceil(0.95*float64(len(xs)))) - 1
	if idx < 0 {
		idx = 0
	}
	p95 = xs[idx]
	return
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	return cp[len(cp)/2]
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// runDiff is implemented in Task 10.
func runDiff(w io.Writer, beforePath, afterPath, format string) {
	fmt.Fprintln(os.Stderr, "diff mode not yet implemented (see Task 10)")
	os.Exit(5)
}
```

- [ ] **Step 2: Write `main_test.go` with a golden-file test**

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
)

func TestWriteSummary_Markdown(t *testing.T) {
	doc := &schema.Document{
		SchemaVersion: schema.SchemaVersion,
		Scenario:      "system-wide-mixed",
		Config:        schema.Config{Processes: 30, Runs: 3, WorkloadMix: map[string]int{"go": 10}},
		System: schema.System{
			Kernel: "6.19", CPUModel: "Test CPU", NCPU: 4,
			GoVersion: "go1.26.0", PerfAgentCommit: "deadbeef",
		},
		StartedAt: time.Date(2026, 4, 25, 19, 30, 0, 0, time.UTC),
		Runs: []schema.Run{
			{RunN: 1, TotalMs: 1000, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []schema.Binary{
					{Path: "/lib/libc.so", BuildID: "111111111111aaaaaaa", EhFrameBytes: 30000, CompileMs: 10},
					{Path: "/bin/foo", BuildID: "222222222222bbbbbbb", EhFrameBytes: 9000, CompileMs: 50},
				}},
			{RunN: 2, TotalMs: 1100, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []schema.Binary{
					{Path: "/lib/libc.so", BuildID: "111111111111aaaaaaa", EhFrameBytes: 30000, CompileMs: 11},
					{Path: "/bin/foo", BuildID: "222222222222bbbbbbb", EhFrameBytes: 9000, CompileMs: 55},
				}},
			{RunN: 3, TotalMs: 950, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []schema.Binary{
					{Path: "/lib/libc.so", BuildID: "111111111111aaaaaaa", EhFrameBytes: 30000, CompileMs: 9},
					{Path: "/bin/foo", BuildID: "222222222222bbbbbbb", EhFrameBytes: 9000, CompileMs: 48},
				}},
		},
	}

	var got bytes.Buffer
	writeSummary(&got, doc, "markdown")

	wantPath := filepath.Join("testdata", "summary.md")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(wantPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wantPath, got.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("missing golden file %s; regenerate with UPDATE_GOLDEN=1: %v", wantPath, err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("output diverges from golden\n--- got ---\n%s\n--- want ---\n%s", got.String(), string(want))
	}
	// Sanity: median ordering — /bin/foo's median (50) > /lib/libc.so's (10), so /bin/foo first.
	if !strings.Contains(got.String(), "| `/bin/foo`") {
		t.Errorf("missing /bin/foo row")
	}
}
```

- [ ] **Step 3: Generate the golden file**

```bash
mkdir -p bench/cmd/report/testdata
GOTOOLCHAIN=auto UPDATE_GOLDEN=1 go test ./bench/cmd/report/...
```

Expected: PASS, with `bench/cmd/report/testdata/summary.md` now created.

- [ ] **Step 4: Run without UPDATE_GOLDEN to confirm stability**

```bash
GOTOOLCHAIN=auto go test ./bench/cmd/report/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add bench/cmd/report/
git commit -m "bench: add report tool (single-file markdown summary)"
```

---

## Task 10: Report tool — diff mode

**Goal:** Implement `report --diff before.json --diff after.json` to print a side-by-side comparison.

**Files:**
- Modify: `bench/cmd/report/main.go` — replace the stub `runDiff`.
- Modify: `bench/cmd/report/main_test.go` — add `TestRunDiff_Markdown`.

- [ ] **Step 1: Replace the `runDiff` stub**

In `bench/cmd/report/main.go`, replace the stub at the bottom with:

```go
func runDiff(w io.Writer, beforePath, afterPath, format string) {
	if format != "markdown" {
		fmt.Fprintln(os.Stderr, "csv diff is not implemented in v1")
		os.Exit(4)
	}
	before, err := readDoc(beforePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(3)
	}
	after, err := readDoc(afterPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(3)
	}

	if before.Scenario != after.Scenario {
		fmt.Fprintf(os.Stderr, "warning: scenario differs (%q vs %q); diff may be misleading\n",
			before.Scenario, after.Scenario)
	}

	bTotals := totalsOf(before)
	aTotals := totalsOf(after)
	bP50, bP95, bMax := stats(append([]float64{}, bTotals...))
	aP50, aP95, aMax := stats(append([]float64{}, aTotals...))
	bStd := stddev(bTotals)
	aStd := stddev(aTotals)

	fmt.Fprintf(w, "# Diff: `%s` → `%s`\n\n", beforePath, afterPath)
	fmt.Fprintln(w, "## Wall time\n")
	fmt.Fprintln(w, "| metric | before (ms) | after (ms) | Δ% | noise (±ms, max stddev) |")
	fmt.Fprintln(w, "|--------|-------------|-----------|----|-------------------------|")
	fmt.Fprintf(w, "| p50 | %.1f | %.1f | %s | %.1f |\n",
		bP50, aP50, deltaPct(bP50, aP50), maxF(bStd, aStd))
	fmt.Fprintf(w, "| p95 | %.1f | %.1f | %s | %.1f |\n",
		bP95, aP95, deltaPct(bP95, aP95), maxF(bStd, aStd))
	fmt.Fprintf(w, "| max | %.1f | %.1f | %s | %.1f |\n",
		bMax, aMax, deltaPct(bMax, aMax), maxF(bStd, aStd))
}

func readDoc(path string) (*schema.Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return schema.Read(f)
}

func totalsOf(d *schema.Document) []float64 {
	out := make([]float64, len(d.Runs))
	for i, r := range d.Runs {
		out[i] = r.TotalMs
	}
	return out
}

func stddev(xs []float64) float64 {
	if len(xs) <= 1 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

func deltaPct(before, after float64) string {
	if before == 0 {
		return "n/a"
	}
	pct := (after - before) / before * 100
	sign := "+"
	if pct < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%.1f%%", sign, pct)
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
```

- [ ] **Step 2: Update `main_test.go` to test the diff path**

Append to `main_test.go`:

```go
func TestRunDiff_Markdown(t *testing.T) {
	mkDoc := func(scenario string, runs []float64) *schema.Document {
		d := &schema.Document{
			SchemaVersion: schema.SchemaVersion,
			Scenario:      scenario,
			Config:        schema.Config{Runs: len(runs)},
			System:        schema.System{Kernel: "x"},
			StartedAt:     time.Now(),
		}
		for i, ms := range runs {
			d.Runs = append(d.Runs, schema.Run{RunN: i + 1, TotalMs: ms})
		}
		return d
	}

	beforeF := writeTempJSON(t, mkDoc("system-wide-mixed", []float64{1000, 1100, 950}))
	afterF := writeTempJSON(t, mkDoc("system-wide-mixed", []float64{800, 850, 870}))

	var got bytes.Buffer
	runDiff(&got, beforeF, afterF, "markdown")

	out := got.String()
	for _, want := range []string{"# Diff:", "## Wall time", "| p50 |", "Δ%"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	// p50: before=1000, after=850, Δ = -15%.
	if !strings.Contains(out, "-15.0%") {
		t.Errorf("expected -15.0%% somewhere in p50 row; got:\n%s", out)
	}
}

func writeTempJSON(t *testing.T, d *schema.Document) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := schema.Write(f, d); err != nil {
		t.Fatal(err)
	}
	return p
}
```

- [ ] **Step 3: Run tests**

```bash
GOTOOLCHAIN=auto go test ./bench/cmd/report/...
```

Expected: both tests pass.

- [ ] **Step 4: Commit**

```bash
git add bench/cmd/report/
git commit -m "bench: report tool: add diff mode for before/after comparisons"
```

---

## Task 11: Scenario harness — `pid-large`

**Goal:** A harness binary that, given `--scenario pid-large`, spawns one stripped Rust release binary, constructs `dwarfagent.NewProfilerWithHooks(pid, false, ...)` with hooks recording per-binary timings, repeats N times, writes JSON. Plus the cap-skip pattern.

**Files:**
- Create: `bench/cmd/scenario/main.go`

- [ ] **Step 1: Write `main.go`**

```go
// Command scenario runs a perf-agent --unwind dwarf startup benchmark
// against a synthetic process fleet, recording per-binary CFI compile
// timings via dwarfagent.Hooks.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/bench/internal/fleet"
	"github.com/dpsoft/perf-agent/bench/internal/schema"
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

func main() {
	var (
		scenario      = flag.String("scenario", "", "pid-large | system-wide-mixed (required)")
		processes    = flag.Int("processes", 30, "fleet size for system-wide-mixed")
		runs         = flag.Int("runs", 5, "iterations per scenario")
		dropCache    = flag.Bool("drop-cache", false, "drop page cache between runs (root-only)")
		outPath      = flag.String("out", "", "output JSON path (default ./bench-{scenario}-{ts}.json)")
		workloadDir  = flag.String("workloads-dir", "", "test/workloads dir (default auto-detect)")
	)
	flag.Parse()

	if *scenario == "" {
		fmt.Fprintln(os.Stderr, "--scenario is required")
		os.Exit(2)
	}

	if !checkCaps() {
		fmt.Fprintln(os.Stdout, "BENCH_SKIPPED: missing required capabilities (CAP_PERFMON, CAP_BPF, CAP_SYS_ADMIN, CAP_SYS_PTRACE, CAP_CHECKPOINT_RESTORE)")
		os.Exit(0)
	}

	dir := *workloadDir
	if dir == "" {
		var err error
		dir, err = autodetectWorkloadDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "auto-detect workloads: %v\n", err)
			os.Exit(2)
		}
	}

	doc := &schema.Document{
		Scenario:  *scenario,
		StartedAt: time.Now().UTC(),
		Config: schema.Config{
			Processes: *processes,
			Runs:      *runs,
			DropCache: *dropCache,
		},
		System: gatherSystemInfo(),
	}

	switch *scenario {
	case "pid-large":
		runPIDLarge(doc, dir, *runs, *dropCache)
	case "system-wide-mixed":
		// Implemented in Task 12.
		fmt.Fprintln(os.Stderr, "system-wide-mixed not yet implemented (see Task 12)")
		os.Exit(5)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
		os.Exit(2)
	}

	out := *outPath
	if out == "" {
		out = fmt.Sprintf("bench-%s-%d.json", *scenario, time.Now().Unix())
	}
	f, err := os.Create(out)
	if err != nil {
		log.Fatalf("create %s: %v", out, err)
	}
	defer f.Close()
	if err := schema.Write(f, doc); err != nil {
		log.Fatalf("write %s: %v", out, err)
	}
	fmt.Fprintf(os.Stdout, "wrote %s\n", out)
}

// runPIDLarge spawns one Rust workload, attaches dwarfagent --pid,
// and records per-binary timings across N runs.
func runPIDLarge(doc *schema.Document, workloadDir string, runs int, dropCache bool) {
	doc.Config.WorkloadMix = map[string]int{"rust": 1}

	flt, err := fleet.Spawn(fleet.Opts{
		Mix:         map[string]int{"rust": 1},
		WorkloadDir: workloadDir,
	})
	if err != nil {
		log.Fatalf("spawn fleet: %v", err)
	}
	defer flt.Stop()

	if err := flt.Wait(10 * time.Second); err != nil {
		log.Fatalf("fleet wait: %v", err)
	}
	pids := flt.PIDs()
	if len(pids) != 1 {
		log.Fatalf("expected 1 PID, got %d", len(pids))
	}
	pid := pids[0]

	for i := 1; i <= runs; i++ {
		if dropCache {
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				log.Printf("drop_caches: %v (continuing — measurement is warm-cache for this run)", err)
			}
		}
		run := measureOnePID(pid, i)
		doc.Runs = append(doc.Runs, run)
	}
}

// measureOnePID times one NewProfilerWithHooks(pid=...) call and the
// per-binary breakdown collected via OnCompile.
func measureOnePID(pid, runN int) schema.Run {
	var (
		mu      sync.Mutex
		entries []schema.Binary
	)
	hooks := &dwarfagent.Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			entries = append(entries, schema.Binary{
				Path:         path,
				BuildID:      buildID,
				EhFrameBytes: ehFrameBytes,
				CompileMs:    float64(dur.Microseconds()) / 1000.0,
			})
		},
	}
	t0 := time.Now()
	prof, err := dwarfagent.NewProfilerWithHooks(pid, false, []uint{0}, nil, 99, hooks)
	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		log.Fatalf("NewProfilerWithHooks (run %d): %v", runN, err)
	}
	pidCount, binCount := prof.AttachStats()
	_ = prof.Close()

	mu.Lock()
	defer mu.Unlock()
	out := schema.Run{
		RunN:                runN,
		TotalMs:             totalMs,
		PIDCount:            pidCount,
		DistinctBinaryCount: binCount,
		PerBinary:           append([]schema.Binary(nil), entries...),
	}
	return out
}

// autodetectWorkloadDir walks up from CWD looking for test/workloads/.
func autodetectWorkloadDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cur := wd
	for {
		cand := filepath.Join(cur, "test", "workloads")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("test/workloads not found above %s", wd)
		}
		cur = parent
	}
}

// checkCaps returns true if the binary has the full set of capabilities
// the perf-agent BPF programs need. Mirrors `perfagent/agent.go`'s set
// (cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE).
func checkCaps() bool {
	if os.Geteuid() == 0 {
		return true
	}
	caps := cap.GetProc()
	for _, c := range []cap.Value{cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE} {
		have, err := caps.GetFlag(cap.Permitted, c)
		if err != nil || !have {
			return false
		}
	}
	return true
}

func gatherSystemInfo() schema.System {
	out := schema.System{
		NCPU:      runtime.NumCPU(),
		GoVersion: runtime.Version(),
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		out.Kernel = strings.TrimSpace(string(data))
	}
	if cmd := exec.Command("git", "rev-parse", "--short", "HEAD"); cmd != nil {
		if b, err := cmd.Output(); err == nil {
			out.PerfAgentCommit = strings.TrimSpace(string(b))
		}
	}
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					out.CPUModel = strings.TrimSpace(line[i+1:])
				}
				break
			}
		}
	}
	return out
}
```

- [ ] **Step 2: Build the harness**

```bash
GOTOOLCHAIN=auto CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build -o bench/cmd/scenario/scenario ./bench/cmd/scenario
```

Expected: produces `bench/cmd/scenario/scenario` binary.

- [ ] **Step 3: Set caps + smoke-run pid-large**

```bash
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario
make test-workloads
./bench/cmd/scenario/scenario --scenario pid-large --runs 2 --out /tmp/pid-large.json
```

Expected: prints `wrote /tmp/pid-large.json`. The JSON has 2 runs with non-zero `total_ms`, non-empty `per_binary`, `pid_count = 1`, `distinct_binary_count > 0`.

- [ ] **Step 4: Verify report tool consumes it**

```bash
GOTOOLCHAIN=auto go build -o bench/cmd/report/report ./bench/cmd/report
./bench/cmd/report/report --in /tmp/pid-large.json
```

Expected: a markdown summary with wall-time table and top-binaries table.

- [ ] **Step 5: Commit**

```bash
git add bench/cmd/scenario/main.go
git commit -m "bench: scenario harness — pid-large path"
```

---

## Task 12: Scenario harness — `system-wide-mixed`

**Files:**
- Modify: `bench/cmd/scenario/main.go` — replace the `system-wide-mixed not yet implemented` stub.

- [ ] **Step 1: Replace the stub with `runSystemWide`**

In `main()`, replace the `case "system-wide-mixed":` branch with `runSystemWideMixed(doc, dir, *processes, *runs, *dropCache)`. Add the function:

```go
// runSystemWideMixed spawns a fleet matching the proportional mix and
// times newSession in system-wide mode for each run.
func runSystemWideMixed(doc *schema.Document, workloadDir string, processes, runs int, dropCache bool) {
	mix := computeMix(processes)
	doc.Config.WorkloadMix = mix

	flt, err := fleet.Spawn(fleet.Opts{Mix: mix, WorkloadDir: workloadDir})
	if err != nil {
		log.Fatalf("spawn fleet: %v", err)
	}
	defer flt.Stop()

	if err := flt.Wait(10 * time.Second); err != nil {
		log.Fatalf("fleet wait: %v", err)
	}

	for i := 1; i <= runs; i++ {
		if dropCache {
			if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
				log.Printf("drop_caches: %v", err)
			}
		}
		run := measureSystemWide(i)
		doc.Runs = append(doc.Runs, run)
	}
}

// computeMix distributes N processes across {go, python, rust, node}
// using ratios 1/3 : 1/3 : 1/6 : 1/6 with largest-remainder rounding so
// totals always equal N.
func computeMix(n int) map[string]int {
	gp := n / 3
	pp := n / 3
	rp := n / 6
	np := n - gp - pp - rp
	return map[string]int{"go": gp, "python": pp, "rust": rp, "node": np}
}

// measureSystemWide times one NewProfilerWithHooks in systemWide=true mode.
func measureSystemWide(runN int) schema.Run {
	var (
		mu      sync.Mutex
		entries []schema.Binary
	)
	hooks := &dwarfagent.Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			entries = append(entries, schema.Binary{
				Path:         path,
				BuildID:      buildID,
				EhFrameBytes: ehFrameBytes,
				CompileMs:    float64(dur.Microseconds()) / 1000.0,
			})
		},
	}
	cpus := allCPUs()
	t0 := time.Now()
	prof, err := dwarfagent.NewProfilerWithHooks(0, true, cpus, nil, 99, hooks)
	totalMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		log.Fatalf("NewProfilerWithHooks (run %d, system-wide): %v", runN, err)
	}
	pidCount, binCount := prof.AttachStats()
	_ = prof.Close()

	mu.Lock()
	defer mu.Unlock()
	return schema.Run{
		RunN:                runN,
		TotalMs:             totalMs,
		PIDCount:            pidCount,
		DistinctBinaryCount: binCount,
		PerBinary:           append([]schema.Binary(nil), entries...),
	}
}

func allCPUs() []uint {
	out := make([]uint, runtime.NumCPU())
	for i := range out {
		out[i] = uint(i)
	}
	return out
}
```

- [ ] **Step 2: Build + smoke-run**

```bash
GOTOOLCHAIN=auto CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build -o bench/cmd/scenario/scenario ./bench/cmd/scenario
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario
./bench/cmd/scenario/scenario --scenario system-wide-mixed --processes 8 --runs 2 --out /tmp/system-wide.json
```

Expected: `wrote /tmp/system-wide.json`. JSON has 2 runs; `per_binary` is populated with binaries from the fleet workloads + their shared libs + already-running system processes.

- [ ] **Step 3: Confirm the report consumes it**

```bash
./bench/cmd/report/report --in /tmp/system-wide.json
```

Expected: markdown summary including the workload mix breakdown.

- [ ] **Step 4: Commit**

```bash
git add bench/cmd/scenario/main.go
git commit -m "bench: scenario harness — system-wide-mixed path"
```

---

## Task 13: Makefile + README

**Files:**
- Modify: `Makefile`
- Create: `bench/README.md`

- [ ] **Step 1: Append targets to `Makefile`**

Append at the end:

```make
.PHONY: bench-corpus bench-scenarios bench-build

bench-corpus:
	GOTOOLCHAIN=auto go test -bench=. -benchmem -run=^$$ ./unwind/ehcompile/...

bench-build:
	GOTOOLCHAIN=auto CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" go build -o bench/cmd/scenario/scenario ./bench/cmd/scenario
	GOTOOLCHAIN=auto go build -o bench/cmd/report/report ./bench/cmd/report

bench-scenarios: bench-build test-workloads
	@if ! getcap ./bench/cmd/scenario/scenario | grep -q cap_perfmon; then \
		echo "*** scenario binary missing caps; run: sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario"; \
		exit 1; \
	fi
	./bench/cmd/scenario/scenario --scenario pid-large --runs 5 --out bench-pid-large.json
	./bench/cmd/scenario/scenario --scenario system-wide-mixed --processes 30 --runs 5 --out bench-system-wide-mixed.json
	./bench/cmd/report/report --in bench-pid-large.json bench-system-wide-mixed.json > bench-report.md
	@echo "report written to bench-report.md"
```

(The `CGO_CFLAGS` / `CGO_LDFLAGS` references should match the existing variable names in the Makefile — the implementer should grep `Makefile` first to confirm the variable names and match them.)

- [ ] **Step 2: Write `bench/README.md`**

```markdown
# perf-agent benchmark suite

Two-layer benchmark for `--unwind dwarf` startup cost. Companion to
`docs/superpowers/specs/2026-04-25-unwind-auto-benchmark-design.md`.

## Layers

- **Corpus** (`unwind/ehcompile/ehcompile_bench_test.go`). Per-binary
  `ehcompile.Compile` cost via `go test -bench`. No caps needed,
  `benchstat`-friendly. Run via `make bench-corpus`.
- **Scenario** (`bench/cmd/scenario/`). End-to-end `dwarfagent.newSession()`
  cost on a synthetic process fleet. Caps required.
  Run via `make bench-scenarios` (one-time `sudo setcap` on the binary).

## Scenarios

- `pid-large` — one Rust release binary, attached via `--pid`. Measures
  per-mapping compile cost for a single process.
- `system-wide-mixed` — N processes across Go/Python/Rust/Node from
  `test/workloads/`, attached via `-a`. Measures `/proc/*` walk +
  per-PID maps parse + per-distinct-binary compile.

## Caveat

`system-wide-mixed` exercises **PID scaling**, not **binary diversity** —
distinct-binary count is bounded by the test workload set + their shared
libs (~20–30). The "40s on 500-process host" anecdote in the
unwind-auto-refinement doc came from a real laptop with many distinct
service binaries. The corpus layer covers per-binary cost; for
real-world end-to-end numbers, run `perf-agent -a` on your host
directly.

## Output

Each scenario run writes `bench-<scenario>-<timestamp>.json`. The schema
is in `bench/internal/schema/`. The aggregator (`bench/cmd/report/`)
reads JSON and produces markdown.

```bash
./bench/cmd/report/report --in bench-pid-large.json bench-system-wide-mixed.json
./bench/cmd/report/report --diff before.json after.json
```
```

- [ ] **Step 3: Run `make bench-corpus` end-to-end**

```bash
make bench-corpus 2>&1 | tail -20
```

Expected: passes, prints benchmark output.

- [ ] **Step 4: Run `make bench-scenarios` end-to-end**

```bash
make bench-scenarios 2>&1 | tail -20
```

Expected (assuming caps already set): produces both JSON files + `bench-report.md`. If caps not set, make target prints the setcap command and exits non-zero — that's the expected friendly failure mode.

- [ ] **Step 5: Commit**

```bash
git add Makefile bench/README.md
git commit -m "bench: add Makefile targets (bench-corpus, bench-scenarios) + README"
```

---

## Self-review

Run before declaring the plan done:

**1. Spec coverage:**
- Component 1 (Hooks plumbing) → Tasks 2, 3, 4, 5, 6 ✓
- Component 2 (Corpus benchmarks) → Task 7 ✓
- Component 3 (Scenario harness) → Tasks 11, 12 ✓
- Component 4 (Fleet driver) → Task 8 ✓
- Component 5 (Output schema) → Task 1 ✓
- Component 6 (Report tool) → Tasks 9, 10 ✓
- Makefile + README → Task 13 ✓

**2. Placeholders:** None. `runDiff` is a deliberate stub at end of Task 9, replaced in Task 10. The cap-detection helper in Task 11 has fallback behavior fully spelled out.

**3. Type consistency:** `dwarfagent.Hooks.OnCompile` signature `func(path, buildID string, ehFrameBytes int, dur time.Duration)` is identical across Tasks 3, 4, 5, 6, 11, 12. `schema.Binary` fields `Path/BuildID/EhFrameBytes/CompileMs` consistent across Tasks 1, 9, 10, 11, 12. `Profiler.AttachStats()` returns `(pidCount, binaryCount int)` consistently in Tasks 5, 11, 12.

**4. Open questions from spec:**
- "Does `ehcompile.Compile` already return `.eh_frame` size, or do we read it from the ELF in `store.go` before calling compile?" → **Resolved in Task 2**: extend `Compile`'s signature.
- "For `pid-large`: does the existing `test/workloads/rust/cpu_bound` already build with frame-pointers off + stripped?" → The actual binary is `test/workloads/rust/target/release/rust-workload` per `Cargo.toml`. It builds with `debug = true, strip = false` — `.eh_frame` is fully populated, which is precisely what we want for measuring DWARF compile cost. The "stripped + FP off" framing in the spec was stricter than needed; the binary as-shipped is a representative DWARF-rich Rust release artifact. No change required.
- "If runs prove noisy with `cpu_bound`..." → Plan uses the existing workloads (go's `cpu_bound`, python's `cpu_bound.py`, node's `cpu_bound.js`, rust's `rust-workload`). Revisit only if numbers are noisy.
