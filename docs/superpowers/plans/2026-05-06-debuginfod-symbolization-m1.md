# Debuginfod-backed symbolization — M1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land M1 of the debuginfod-symbolization design — a `symbolize.Symbolizer` interface with two impls (`LocalSymbolizer` preserves today's behavior; `DebuginfodSymbolizer` adds debuginfod-backed off-box DWARF fetch via blazesym's `process_dispatch` hook). Migrate the three existing symbolize call sites to the interface and wire the chosen impl through `perfagent.Agent`. Behavior is unchanged unless `--debuginfod-url` is configured.

**Architecture:** Two new packages. `symbolize/` defines the interface, `Frame` value type, and `LocalSymbolizer` (wraps the upstream `github.com/libbpf/blazesym/go` binding — today's behavior). `symbolize/debuginfod/` holds the cgo-backed `Symbolizer` that drives blazesym's `process_dispatch` callback through a 4-case routing table (cached executable / local-resolution-possible / fetch `/debuginfo` / fetch `/executable`), plus a build-id-indexed on-disk cache with a SQLite index (`modernc.org/sqlite`, pure Go).

**Tech Stack:** Go 1.26, cgo (blazesym capi), `golang.org/x/sync/singleflight`, `modernc.org/sqlite` (pure-Go), `httptest` for fetch tests, docker-compose-based integration test from the PoC archive.

**Authoritative spec:** `docs/superpowers/specs/2026-05-06-debuginfod-symbolization-design.md`. Read it before starting.

**Out of scope (deferred to M2):** `FailClosed` per-call build-id override pass, `metrics.Exporter` wiring, BUILDING.md update, end-to-end stripped-binary integration test, multi-instance race tests.

---

## File structure

**New files:**

```
symbolize/
  symbolize.go            interface + Frame + FailureReason
  local.go                LocalSymbolizer (wraps upstream blazesym/go)
  toprof.go               ToProfFrames adapter for pprof.Frame
  symbolize_test.go
  local_test.go
  toprof_test.go

symbolize/debuginfod/
  doc.go                  package docstring
  symbolizer.go           Symbolizer struct + New + SymbolizeProcess + Close + Stats
  options.go              Options struct
  dispatcher.go           cgo bridge: goDispatchCb + install_dispatch + dispatch decision tree
  resolution.go           localResolutionPossible + hasDwarf + hasResolvableDebuglink + binaryReadable
  buildid.go              readBuildID(mapsFile, symbolicPath) (thin wrapper over procmap.ReadBuildID)
  fetcher.go              fetcher struct (HTTP) + URL fallback
  singleflight.go         fetcher+singleflight wrapper
  stats.go                atomicStats + Stats
  errors.go               package-level sentinel errors
  symbolizer_test.go
  dispatcher_test.go
  fetcher_test.go
  resolution_test.go
  buildid_test.go

symbolize/debuginfod/cache/
  cache.go                Cache struct (path layout, atomic write, LRU eviction trigger)
  index.go                Index interface + Entry type
  index_sqlite.go         sole Index implementation (modernc.org/sqlite)
  cache_test.go
  index_sqlite_test.go

test/debuginfod/
  docker-compose.yml      (lifted/adapted from PoC archive)
  upload.sh, test.sh      smoke helpers (lifted from PoC archive)
  sample/                 (PoC's hello.c + Makefile)
  README.md               how to run the test locally
```

**Modified files:**

```
unwind/procmap/buildid.go     → export ReadBuildID
unwind/procmap/resolver.go    → export Resolver.BuildID(path string) string
profile/profiler.go           → take symbolize.Symbolizer, drop blazeSymToFrames
offcpu/profiler.go            → take symbolize.Symbolizer, drop blazeSymToFrames
unwind/dwarfagent/symbolize.go → take symbolize.Symbolizer
unwind/dwarfagent/common.go   → session takes symbolize.Symbolizer instead of constructing one
profile/profiler.go           → NewProfiler signature gains Symbolizer
offcpu/profiler.go            → NewProfiler signature gains Symbolizer
unwind/dwarfagent/profiler.go → constructor gains Symbolizer
perfagent/options.go          → add WithDebuginfod options + Config fields
perfagent/agent.go            → chooseSymbolizer() + wire into profilers + close ordering
main.go                       → flag definitions for debuginfod
go.mod                        → add modernc.org/sqlite + golang.org/x/sync
Makefile                      → blazesym dispatcher header guard
```

---

## Pre-flight (one-time, must happen before Phase 1)

### Task 0: Verify blazesym checkout has `process_dispatch`

**Files:**
- Check: `/home/diego/github/blazesym/capi/include/blazesym.h`

- [ ] **Step 1: Bring the local blazesym checkout to a commit with the dispatcher**

The dispatcher landed in upstream commit `1f2d983 capi: Add callback for symbolizer` (and review fixup `8891e70`). The local checkout may be older.

```bash
cd /home/diego/github/blazesym
git fetch origin
# Get to a commit that contains both 1f2d983 and 8891e70
git checkout 8891e70
# Or pull main if upstream main now contains them:
# git pull origin main
```

- [ ] **Step 2: Confirm the header exports the dispatcher**

```bash
grep -c 'blaze_symbolizer_dispatch' /home/diego/github/blazesym/capi/include/blazesym.h
```
Expected: a number ≥ 1 (the type and references).

```bash
grep -c 'process_dispatch' /home/diego/github/blazesym/capi/include/blazesym.h
```
Expected: a number ≥ 1.

If either is `0`, repeat Step 1 with a more recent commit/branch.

- [ ] **Step 3: Rebuild libblazesym_c.a so cgo links against the dispatcher-aware library**

```bash
cd /home/diego/github/blazesym
cargo build --release -p blazesym-c
ls -la target/release/libblazesym_c.a
```
Expected: file exists, mtime is recent.

- [ ] **Step 4: Add Makefile guard so future builds catch a stale checkout**

Open `/home/diego/github/perf-agent/Makefile`. After the `LIBBLAZESYM_OBJ` definition (around line 12), add a target that fails fast on stale headers:

```make
.PHONY: blazesym-check
blazesym-check:
	@if ! grep -q 'process_dispatch' $(LIBBLAZESYM_INC)/blazesym.h; then \
		echo "*** blazesym header at $(LIBBLAZESYM_INC)/blazesym.h is too old"; \
		echo "*** missing process_dispatch — pull blazesym to a commit ≥ 8891e70"; \
		exit 1; \
	fi

build: blazesym-check $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a
```

(The `build:` line replaces the existing `build:` line; otherwise the rule won't run the guard.)

- [ ] **Step 5: Confirm `make build` still works**

```bash
cd /home/diego/github/perf-agent
make build
```
Expected: green build, perf-agent binary produced.

- [ ] **Step 6: Commit**

```bash
git add Makefile
git commit -m "build: guard blazesym header has process_dispatch dispatcher"
```

---

## Phase 1 — `symbolize.Symbolizer` interface + LocalSymbolizer

### Task 1: Create `symbolize.Symbolizer` interface + `Frame` + `FailureReason`

**Files:**
- Create: `symbolize/symbolize.go`
- Test: `symbolize/symbolize_test.go`

- [ ] **Step 1: Write the failing test**

`symbolize/symbolize_test.go`:

```go
package symbolize

import "testing"

func TestFrameZeroValue(t *testing.T) {
	var f Frame
	if f.Reason != FailureNone {
		t.Fatalf("zero Frame.Reason = %d, want %d", f.Reason, FailureNone)
	}
	if f.Name != "" {
		t.Fatalf("zero Frame.Name = %q, want empty", f.Name)
	}
}

func TestFailureReasonExhaustive(t *testing.T) {
	// Locks the iota order: changes here are deliberate API-shape changes.
	want := map[FailureReason]string{
		FailureNone:              "none",
		FailureUnmapped:          "unmapped",
		FailureInvalidFileOffset: "invalid_file_offset",
		FailureMissingComponent:  "missing_component",
		FailureMissingSymbols:    "missing_symbols",
		FailureUnknownAddress:    "unknown_address",
		FailureFetchError:        "fetch_error",
		FailureNoBuildID:         "no_build_id",
	}
	for r, name := range want {
		if r.String() != name {
			t.Fatalf("FailureReason(%d).String() = %q, want %q", r, r.String(), name)
		}
	}
}
```

- [ ] **Step 2: Run test, confirm it fails (package missing)**

```bash
go test ./symbolize/...
```
Expected: FAIL — package not found.

- [ ] **Step 3: Implement `symbolize/symbolize.go`**

```go
// Package symbolize provides perf-agent's address-to-frame resolution
// abstraction. Implementations live in this package (LocalSymbolizer)
// and in symbolize/debuginfod (off-box-fetch).
package symbolize

// Symbolizer resolves abs addresses in a process's address space to
// symbolic frames. Implementations are safe for concurrent use.
type Symbolizer interface {
	SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error)
	Close() error
}

// Frame is a single symbolized stack frame. Name is "" when resolution
// failed; Reason explains why. Inlined holds the inline-expansion chain
// in caller-most-to-callee order when the resolver supports it.
type Frame struct {
	Address uint64
	Name    string
	Module  string
	BuildID string
	File    string
	Line    int
	Column  int
	Offset  uint64
	Inlined []Frame
	Reason  FailureReason
}

// FailureReason describes why a Frame's Name is empty.
type FailureReason uint8

const (
	FailureNone FailureReason = iota
	FailureUnmapped
	FailureInvalidFileOffset
	FailureMissingComponent
	FailureMissingSymbols
	FailureUnknownAddress
	FailureFetchError
	FailureNoBuildID
)

func (r FailureReason) String() string {
	switch r {
	case FailureNone:
		return "none"
	case FailureUnmapped:
		return "unmapped"
	case FailureInvalidFileOffset:
		return "invalid_file_offset"
	case FailureMissingComponent:
		return "missing_component"
	case FailureMissingSymbols:
		return "missing_symbols"
	case FailureUnknownAddress:
		return "unknown_address"
	case FailureFetchError:
		return "fetch_error"
	case FailureNoBuildID:
		return "no_build_id"
	}
	return "unknown"
}
```

- [ ] **Step 4: Run test, confirm it passes**

```bash
go test ./symbolize/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/symbolize.go symbolize/symbolize_test.go
git commit -m "symbolize: add Symbolizer interface and Frame value type"
```

---

### Task 2: Implement `LocalSymbolizer` (wraps upstream blazesym/go)

**Files:**
- Create: `symbolize/local.go`
- Test: `symbolize/local_test.go`

- [ ] **Step 1: Write the failing test**

`symbolize/local_test.go`:

```go
package symbolize

import (
	"errors"
	"os"
	"testing"
)

func TestLocalSymbolizerSymbolizeSelf(t *testing.T) {
	if testing.Short() {
		t.Skip("uses /proc/self/maps")
	}
	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	defer s.Close()

	// main is a symbol in our own binary — its address is the runtime PC of any
	// stack frame inside it. We don't need to find it precisely; we just need an
	// address that's in our own process. Use the address of os.Getpid (a real
	// runtime function) — its address is in our binary's mapping.
	addr := uint64(getOsGetpidAddr())
	frames, err := s.SymbolizeProcess(uint32(os.Getpid()), []uint64{addr})
	if err != nil {
		t.Fatalf("SymbolizeProcess: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("got 0 frames, want ≥1")
	}
	if frames[0].Name == "" {
		t.Fatalf("frame Name empty (Reason=%s)", frames[0].Reason)
	}
}

func TestLocalSymbolizerCloseIdempotent(t *testing.T) {
	s, err := NewLocalSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalSymbolizer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not panic; either err or nil is acceptable.
	if err := s.Close(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: unexpected err %v", err)
	}
}
```

Add the helper at the bottom of `symbolize/local_test.go`:

```go
//go:noinline
func getOsGetpidAddr() uintptr {
	// Reflect on os.Getpid's PC. It's a real function we can guarantee is mapped.
	return reflect.ValueOf(os.Getpid).Pointer()
}
```

…and add `"reflect"` to the imports.

- [ ] **Step 2: Run test, confirm it fails (LocalSymbolizer not yet defined)**

```bash
go test ./symbolize/...
```
Expected: FAIL — undefined `NewLocalSymbolizer`, `ErrClosed`.

- [ ] **Step 3: Implement `symbolize/local.go`**

```go
package symbolize

import (
	"errors"
	"sync/atomic"

	blazesym "github.com/libbpf/blazesym/go"
)

// ErrClosed is returned from operations on a closed Symbolizer.
var ErrClosed = errors.New("symbolize: closed")

// LocalSymbolizer wraps blazesym's Process source with no off-box hooks —
// preserves perf-agent's pre-debuginfod behavior. Used when no debuginfod
// URL is configured.
type LocalSymbolizer struct {
	bz      *blazesym.Symbolizer
	closed  atomic.Bool
}

// NewLocalSymbolizer constructs a LocalSymbolizer with code-info and
// inlined-fns enabled (matches today's behavior at the three call sites).
func NewLocalSymbolizer() (*LocalSymbolizer, error) {
	bz, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		return nil, err
	}
	return &LocalSymbolizer{bz: bz}, nil
}

// SymbolizeProcess returns one Frame per IP. blazesym's Inlined chain is
// expanded into the Frame.Inlined slice in caller-most-to-callee order.
func (s *LocalSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}
	syms, err := s.bz.SymbolizeProcessAbsAddrs(
		ips,
		pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	if err != nil {
		return nil, err
	}
	out := make([]Frame, 0, len(syms))
	for i, sym := range syms {
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, fromBlazesymSym(sym, addr))
	}
	return out, nil
}

// Close releases the underlying blazesym Symbolizer. Idempotent.
func (s *LocalSymbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.bz.Close()
	return nil
}

// fromBlazesymSym translates one blazesym.Sym into a Frame, populating
// Inlined in caller-most-to-callee order. addr is the abs IP this frame
// was resolved from.
func fromBlazesymSym(s blazesym.Sym, addr uint64) Frame {
	f := Frame{
		Address: addr,
		Name:    s.Name,
		Module:  s.Module,
		Offset:  s.Offset,
	}
	if s.CodeInfo != nil {
		f.File = s.CodeInfo.File
		f.Line = s.CodeInfo.Line
		f.Column = s.CodeInfo.Column
	}
	for _, in := range s.Inlined {
		inFrame := Frame{
			Address: addr,
			Name:    in.Name,
			Module:  s.Module,
		}
		if in.CodeInfo != nil {
			inFrame.File = in.CodeInfo.File
			inFrame.Line = in.CodeInfo.Line
			inFrame.Column = in.CodeInfo.Column
		}
		f.Inlined = append(f.Inlined, inFrame)
	}
	return f
}
```

- [ ] **Step 4: Run tests with the cgo build env (LocalSymbolizer needs blazesym at link time)**

```bash
cd /home/diego/github/perf-agent
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./symbolize/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/local.go symbolize/local_test.go
git commit -m "symbolize: add LocalSymbolizer wrapping upstream blazesym/go"
```

---

### Task 3: Add `ToProfFrames` adapter for `pprof.Frame`

**Files:**
- Create: `symbolize/toprof.go`
- Test: `symbolize/toprof_test.go`

The dwarfagent code path produces `[]pprof.Frame` directly. This adapter avoids a per-call-site translation — keeps Sym→Frame translation in one place (`fromBlazesymSym`) and Frame→pprof.Frame translation in another (`ToProfFrames`).

- [ ] **Step 1: Write the failing test**

`symbolize/toprof_test.go`:

```go
package symbolize

import (
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/pprof"
)

func TestToProfFramesLeafFirst(t *testing.T) {
	in := []Frame{
		{
			Address: 0x401000,
			Name:    "outer",
			Module:  "/bin/x",
			File:    "x.c", Line: 100,
			Inlined: []Frame{
				{Address: 0x401000, Name: "caller_inline", Module: "/bin/x", File: "x.c", Line: 50},
				{Address: 0x401000, Name: "callee_inline", Module: "/bin/x", File: "x.c", Line: 60},
			},
		},
	}
	got := ToProfFrames(in)
	want := []pprof.Frame{
		// Inline chain expands leaf-first (reverse of caller→callee order).
		{Name: "callee_inline", Module: "/bin/x", File: "x.c", Line: 60, Address: 0x401000},
		{Name: "caller_inline", Module: "/bin/x", File: "x.c", Line: 50, Address: 0x401000},
		{Name: "outer", Module: "/bin/x", File: "x.c", Line: 100, Address: 0x401000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToProfFrames mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestToProfFramesEmpty(t *testing.T) {
	if got := ToProfFrames(nil); got != nil {
		t.Fatalf("ToProfFrames(nil) = %+v, want nil", got)
	}
}
```

- [ ] **Step 2: Run test, confirm it fails (`ToProfFrames` undefined)**

```bash
go test ./symbolize/... -run ToProfFrames
```
Expected: FAIL.

- [ ] **Step 3: Implement `symbolize/toprof.go`**

```go
package symbolize

import "github.com/dpsoft/perf-agent/pprof"

// ToProfFrames flattens a []Frame (each with optional Inlined chain) into
// a leaf-first []pprof.Frame for direct insertion into pprof builders.
//
// blazesym's Inlined is reported caller→callee; pprof wants leaf-first.
// Each frame in a chain shares the outer Frame's Address so pprof's
// Locations stay distinguishable when two PCs symbolize identically.
func ToProfFrames(frames []Frame) []pprof.Frame {
	if len(frames) == 0 {
		return nil
	}
	out := make([]pprof.Frame, 0, len(frames))
	for _, f := range frames {
		// Inlined: walk in reverse to get leaf-first.
		for i := len(f.Inlined) - 1; i >= 0; i-- {
			in := f.Inlined[i]
			out = append(out, pprof.Frame{
				Name:    in.Name,
				Module:  f.Module,
				File:    in.File,
				Line:    in.Line,
				Address: f.Address,
			})
		}
		out = append(out, pprof.Frame{
			Name:    f.Name,
			Module:  f.Module,
			File:    f.File,
			Line:    f.Line,
			Address: f.Address,
		})
	}
	return out
}
```

- [ ] **Step 4: Run test, confirm it passes**

```bash
go test ./symbolize/... -run ToProfFrames
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/toprof.go symbolize/toprof_test.go
git commit -m "symbolize: add ToProfFrames adapter for pprof.Frame"
```

---

## Phase 2 — Migrate the three symbolize call sites to the interface

### Task 4: Migrate `profile/profiler.go` to `symbolize.Symbolizer`

**Files:**
- Modify: `profile/profiler.go` (constructor signature, field type, drop `blazeSymToFrames`)

- [ ] **Step 1: Confirm existing tests still pass before touching anything**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./profile/...
```
Expected: PASS.

- [ ] **Step 2: Modify `profile/profiler.go`**

Replace the `symbolizer` field type:

```go
// before:
import blazesym "github.com/libbpf/blazesym/go"

type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	// ...
}

// after:
import "github.com/dpsoft/perf-agent/symbolize"

type Profiler struct {
	objs       *perfObjects
	symbolizer symbolize.Symbolizer
	// ...
}
```

Delete the entire `blazeSymToFrames` function (lines 42-68 in the current file).

Change `NewProfiler` to take a symbolizer and remove the internal `blazesym.NewSymbolizer` call:

```go
func NewProfiler(
	pid int, systemWide bool, cpus []uint, tags []string, sampleRate int,
	labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec,
	sym symbolize.Symbolizer,
) (*Profiler, error) {
	// ... existing setup until just before the symbolizer block ...

	// REMOVE the existing blazesym.NewSymbolizer block (around lines 122-130).
	// The Agent owns the symbolizer; we just use it.

	return &Profiler{
		objs:       objs,
		symbolizer: sym,
		resolver:   procmap.NewResolver(),
		perfSet:    perfSet,
		tags:       tags,
		sampleRate: sampleRate,
		labels:     labels,
		perfData:   perfData,
	}, nil
}
```

In `Close`, **remove** the `pr.symbolizer.Close()` call — the Agent owns the symbolizer's lifetime now:

```go
func (pr *Profiler) Close() {
	// pr.symbolizer.Close() removed — Agent owns it
	pr.resolver.Close()
	_ = pr.perfSet.Close()
	_ = pr.objs.Close()
}
```

In `Collect`, replace the `SymbolizeProcessAbsAddrs` block:

```go
// before:
symbols, err := pr.symbolizer.SymbolizeProcessAbsAddrs(
    ips, samplePid,
    blazesym.ProcessSourceWithPerfMap(true),
    blazesym.ProcessSourceWithDebugSyms(true),
)
if err != nil {
    log.Printf("Failed to symbolize: %v", err)
} else {
    for i, s := range symbols {
        if i >= len(ips) { break }
        for _, f := range blazeSymToFrames(s, ips[i]) {
            sb.append(f)
        }
    }
}

// after:
frames, err := pr.symbolizer.SymbolizeProcess(samplePid, ips)
if err != nil {
    log.Printf("Failed to symbolize: %v", err)
} else {
    for _, f := range symbolize.ToProfFrames(frames) {
        sb.append(f)
    }
}
```

- [ ] **Step 3: Update existing callers of `NewProfiler` to pass a symbolizer**

Find the caller(s) in `perfagent/`:

```bash
grep -rn "profile.NewProfiler" /home/diego/github/perf-agent/ --include="*.go"
```

Each call site gets a temporary `symbolize.NewLocalSymbolizer()` for now; Phase 7 will route the Agent-owned symbolizer through. Insert this just above the `profile.NewProfiler(...)` call site:

```go
sym, err := symbolize.NewLocalSymbolizer()
if err != nil {
    return fmt.Errorf("symbolizer: %w", err)
}
// pass sym as the new last arg to profile.NewProfiler
```

(Don't worry about the leak; Phase 7 replaces this with proper Agent ownership.)

- [ ] **Step 4: Run profile tests, confirm they still pass**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./profile/... ./perfagent/...
```
Expected: PASS — behavior is unchanged because `LocalSymbolizer` is a thin wrapper.

- [ ] **Step 5: Commit**

```bash
git add profile/profiler.go perfagent/agent.go
git commit -m "profile: take symbolize.Symbolizer interface instead of blazesym handle"
```

---

### Task 5: Migrate `offcpu/profiler.go` to `symbolize.Symbolizer`

**Files:**
- Modify: `offcpu/profiler.go`

Identical pattern to Task 4. The diff is structurally the same.

- [ ] **Step 1: Apply the same kind of edit as Task 4**

In `offcpu/profiler.go`:
- Replace `*blazesym.Symbolizer` with `symbolize.Symbolizer`.
- Remove the `blazeSymToFrames` function (lines 42-60 in the current file).
- Add `sym symbolize.Symbolizer` as the last parameter to `NewProfiler`.
- Remove the internal `blazesym.NewSymbolizer(...)` block (lines 97-105).
- Remove `pr.symbolizer.Close()` from `Close`.
- Replace the `SymbolizeProcessAbsAddrs` call in `Collect` with `pr.symbolizer.SymbolizeProcess(...)` followed by `symbolize.ToProfFrames(...)`.
- Imports: drop `blazesym "github.com/libbpf/blazesym/go"`, add `"github.com/dpsoft/perf-agent/symbolize"`.

- [ ] **Step 2: Update callers of `offcpu.NewProfiler`**

```bash
grep -rn "offcpu.NewProfiler" /home/diego/github/perf-agent/ --include="*.go"
```
Inject a `symbolize.NewLocalSymbolizer()` argument like Task 4 step 3. Phase 7 cleans up.

- [ ] **Step 3: Run tests**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./offcpu/... ./perfagent/...
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add offcpu/profiler.go perfagent/agent.go
git commit -m "offcpu: take symbolize.Symbolizer interface instead of blazesym handle"
```

---

### Task 6: Migrate `unwind/dwarfagent` to `symbolize.Symbolizer`

**Files:**
- Modify: `unwind/dwarfagent/symbolize.go`
- Modify: `unwind/dwarfagent/common.go` (the `session` struct)
- Modify: dwarfagent's `Profiler` and `OffCPUProfiler` constructors to accept the interface

- [ ] **Step 1: Rewrite `unwind/dwarfagent/symbolize.go`**

Replace the entire file with:

```go
package dwarfagent

import (
	"log"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

// symbolizePID resolves ips for pid and returns pprof frames in the
// same order as ips. Failed IPs contribute a single synthetic
// "[unknown]" frame carrying the original PC as Address.
func symbolizePID(sym symbolize.Symbolizer, pid uint32, ips []uint64) []pprof.Frame {
	if len(ips) == 0 {
		return nil
	}
	frames, err := sym.SymbolizeProcess(pid, ips)
	if err != nil || len(frames) == 0 {
		log.Printf("dwarfagent: symbolize: %v", err)
		out := make([]pprof.Frame, len(ips))
		for i := range out {
			out[i] = pprof.Frame{Name: "[unknown]", Address: ips[i]}
		}
		return out
	}
	return symbolize.ToProfFrames(frames)
}
```

The old `blazeSymToFrames` function is gone. The `blazesym` import is gone.

- [ ] **Step 2: Modify `unwind/dwarfagent/common.go`**

In the `session` struct, change:

```go
// before:
symbolizer *blazesym.Symbolizer
// after:
symbolizer symbolize.Symbolizer
```

Add to imports: `"github.com/dpsoft/perf-agent/symbolize"`. Remove `blazesym "github.com/libbpf/blazesym/go"`.

In `newSession(...)`, **remove** the entire `blazesym.NewSymbolizer(...)` block (lines 186-194):

```go
// REMOVE THIS BLOCK:
symbolizer, err := blazesym.NewSymbolizer(
    blazesym.SymbolizerWithCodeInfo(true),
    blazesym.SymbolizerWithInlinedFns(true),
)
if err != nil {
    _ = rd.Close()
    _ = watcher.Close()
    return nil, fmt.Errorf("create symbolizer: %w", err)
}
```

Add `sym symbolize.Symbolizer` as a new parameter to `newSession`. Use it to populate the field:

```go
return &session{
    // ... existing fields ...
    symbolizer:  sym,
    // ...
}, nil
```

In `session.close()`, **remove** the `s.symbolizer.Close()` line — Agent owns the symbolizer.

- [ ] **Step 3: Modify dwarfagent's Profiler / OffCPUProfiler constructors**

Find them:

```bash
grep -n "newSession" /home/diego/github/perf-agent/unwind/dwarfagent/*.go
```

Each `New*` constructor that calls `newSession` gains a `sym symbolize.Symbolizer` parameter and passes it through. Update the callers in `perfagent/agent.go` to inject `symbolize.NewLocalSymbolizer()` for now (Phase 7 fixes).

- [ ] **Step 4: Run tests**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./unwind/dwarfagent/... ./perfagent/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add unwind/dwarfagent/ perfagent/agent.go
git commit -m "dwarfagent: take symbolize.Symbolizer interface instead of blazesym handle"
```

---

## Phase 3 — Expose `procmap` helpers needed by debuginfod

### Task 7: Export `ReadBuildID` and `Resolver.BuildID`

**Files:**
- Modify: `unwind/procmap/buildid.go`
- Modify: `unwind/procmap/resolver.go`

The new package needs to read build-ids without parsing ELFs from scratch and without re-implementing the cache. Both already exist as package-private; expose them.

- [ ] **Step 1: Write the failing test**

`unwind/procmap/buildid_export_test.go`:

```go
package procmap_test

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestExportedReadBuildID(t *testing.T) {
	id, err := procmap.ReadBuildID("/bin/ls")
	if err != nil {
		t.Skipf("/bin/ls not available or unreadable: %v", err)
	}
	if id == "" {
		t.Skip("/bin/ls has no build-id (some distros)")
	}
	if len(id)%2 != 0 || len(id) < 8 {
		t.Fatalf("ReadBuildID returned suspicious value: %q", id)
	}
}

func TestExportedResolverBuildID(t *testing.T) {
	r := procmap.NewResolver()
	defer r.Close()
	got := r.BuildID("/bin/ls")
	// Identical to a freshly-read value (caches don't change semantics).
	want, err := procmap.ReadBuildID("/bin/ls")
	if err != nil {
		t.Skipf("/bin/ls not available: %v", err)
	}
	if got != want {
		t.Fatalf("Resolver.BuildID = %q; ReadBuildID = %q (mismatch)", got, want)
	}
}
```

- [ ] **Step 2: Run test, confirm it fails**

```bash
go test ./unwind/procmap/... -run Exported
```
Expected: FAIL — `ReadBuildID` / `BuildID` undefined.

- [ ] **Step 3: Export the helpers**

In `unwind/procmap/buildid.go`, rename `readBuildID` → `ReadBuildID`. Update all internal callers in the same file/package. Keep the doc comment.

In `unwind/procmap/resolver.go`, rename `buildIDFor` → keep, but add an exported `BuildID` method that delegates:

```go
// BuildID returns a cached hex build-id for path, reading the ELF on
// first call. Read failures produce an empty string (cached) — a
// missing build-id is not a failure.
func (r *Resolver) BuildID(path string) string {
	return r.buildIDFor(path)
}
```

Update internal callers:

```bash
grep -rn "readBuildID\|buildIDFor" /home/diego/github/perf-agent/unwind/procmap/
```
Inside-package calls become `ReadBuildID` / continue to use `buildIDFor` (or `BuildID`).

- [ ] **Step 4: Run tests**

```bash
go test ./unwind/procmap/...
```
Expected: PASS (existing + new tests).

- [ ] **Step 5: Commit**

```bash
git add unwind/procmap/buildid.go unwind/procmap/resolver.go unwind/procmap/buildid_export_test.go
git commit -m "procmap: export ReadBuildID and Resolver.BuildID for debuginfod reuse"
```

---

## Phase 4 — Cache + index (SQLite-only)

### Task 8: Cache file-layout helpers

**Files:**
- Create: `symbolize/debuginfod/cache/cache.go`
- Test: `symbolize/debuginfod/cache/cache_test.go`

Pure file ops. Path layout, atomic write. No DB, no index yet.

- [ ] **Step 1: Write the failing test**

`symbolize/debuginfod/cache/cache_test.go`:

```go
package cache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathDebuginfo(t *testing.T) {
	got := pathFor("aabbccddeeff", KindDebuginfo)
	want := filepath.Join(".build-id", "aa", "bbccddeeff.debug")
	if got != want {
		t.Fatalf("pathFor debuginfo = %q, want %q", got, want)
	}
}

func TestPathExecutable(t *testing.T) {
	got := pathFor("aabbccddeeff", KindExecutable)
	want := filepath.Join(".build-id", "aa", "bbccddeeff")
	if got != want {
		t.Fatalf("pathFor executable = %q, want %q", got, want)
	}
}

func TestPathTooShortBuildID(t *testing.T) {
	got := pathFor("a", KindDebuginfo)
	if got != "" {
		t.Fatalf("pathFor(short) = %q, want empty", got)
	}
}

func TestWriteAtomicCreatesFile(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	body := strings.NewReader("hello world")
	abs, err := c.WriteAtomic(KindDebuginfo, "deadbeef0011223344", body)
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("file contents = %q", got)
	}
	if !strings.HasSuffix(abs, "/.build-id/de/adbeef0011223344.debug") {
		t.Fatalf("path layout wrong: %q", abs)
	}
}

func TestWriteAtomicNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	// Create an unreadable parent so rename fails late
	bad := filepath.Join(dir, ".build-id", "de")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(bad, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(bad, 0o755) //nolint:errcheck
	_, err := c.WriteAtomic(KindDebuginfo, "deadbeef0011223344", strings.NewReader("x"))
	if err == nil {
		t.Skip("rename succeeded despite chmod (likely root)")
	}
	// No .debug file should exist.
	final := filepath.Join(bad, "adbeef0011223344.debug")
	if _, err := os.Stat(final); err == nil {
		t.Fatalf("partial file present at %q", final)
	}
	// And no leftover tmp file in dir.
	entries, _ := os.ReadDir(bad)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fetch-") {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
}

// Sanity: io.Copy produces the same bytes through WriteAtomic.
func TestWriteAtomicLargeBody(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	const N = 1 << 20
	want := bytes.Repeat([]byte("A"), N)
	abs, err := c.WriteAtomic(KindExecutable, "abcdef0123456789aa", bytes.NewReader(want))
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body mismatch (got %d bytes, want %d)", len(got), N)
	}
}
```

- [ ] **Step 2: Run test, confirm it fails**

```bash
go test ./symbolize/debuginfod/cache/...
```
Expected: FAIL — package doesn't exist.

- [ ] **Step 3: Implement `symbolize/debuginfod/cache/cache.go`**

```go
// Package cache stores debuginfod-fetched artifacts on disk under a
// .build-id/<NN>/<rest>{.debug,} layout that blazesym's debug_dirs walker
// recognizes natively.
package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Kind selects the artifact flavor. KindDebuginfo files are placed where
// blazesym's debug-link / build-id resolver finds them automatically.
// KindExecutable files are returned by the dispatcher to blazesym.
type Kind int

const (
	KindDebuginfo Kind = iota
	KindExecutable
)

// Cache wraps a directory containing the .build-id index. Concrete index
// (SQLite or JSON) is wired in via Cache.Index.
type Cache struct {
	Dir   string
	Index Index // optional during early-cache tests; required for production
}

// pathFor returns the path within Dir for (buildID, kind), or "" if buildID
// is too short to split into the standard <NN>/<rest> layout.
func pathFor(buildID string, kind Kind) string {
	if len(buildID) < 4 {
		return ""
	}
	rest := buildID[2:]
	prefix := buildID[:2]
	if kind == KindDebuginfo {
		rest += ".debug"
	}
	return filepath.Join(".build-id", prefix, rest)
}

// AbsPath returns the absolute path within Dir for (buildID, kind).
// Empty when buildID is invalid.
func (c *Cache) AbsPath(buildID string, kind Kind) string {
	rel := pathFor(buildID, kind)
	if rel == "" {
		return ""
	}
	return filepath.Join(c.Dir, rel)
}

// Has reports whether the artifact is on disk.
func (c *Cache) Has(buildID string, kind Kind) bool {
	abs := c.AbsPath(buildID, kind)
	if abs == "" {
		return false
	}
	_, err := os.Stat(abs)
	return err == nil
}

// WriteAtomic streams body to a tmp file in the same directory as the
// final destination, then renames into place. Returns the absolute final
// path on success.
func (c *Cache) WriteAtomic(kind Kind, buildID string, body io.Reader) (string, error) {
	rel := pathFor(buildID, kind)
	if rel == "" {
		return "", fmt.Errorf("invalid build-id %q", buildID)
	}
	abs := filepath.Join(c.Dir, rel)
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "fetch-*.tmp")
	if err != nil {
		return "", fmt.Errorf("createtemp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	defer func() {
		// On any error path, ensure the tmp file is removed.
		if err != nil {
			cleanup()
		}
	}()
	if _, err = io.Copy(tmp, body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("copy: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if err = os.Rename(tmpName, abs); err != nil {
		return "", fmt.Errorf("rename: %w", err)
	}
	return abs, nil
}

// ErrNoIndex is returned when an operation requires a configured Index.
var ErrNoIndex = errors.New("cache: no index configured")
```

- [ ] **Step 4: Run tests, confirm pass**

```bash
go test ./symbolize/debuginfod/cache/...
```
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/cache/cache.go symbolize/debuginfod/cache/cache_test.go
git commit -m "cache: add path layout helpers and atomic write"
```

---

### Task 9: `index` interface + `entry` type

**Files:**
- Create: `symbolize/debuginfod/cache/index.go`

- [ ] **Step 1: Implement `index.go` (interface only — no test, the test belongs to each impl)**

```go
package cache

import "time"

// Entry describes a cached artifact.
type Entry struct {
	BuildID    string
	Kind       Kind
	Size       int64
	LastAccess time.Time
}

// Index tracks cache entries for LRU eviction. Implementations must be
// safe for concurrent use by the cache.
type Index interface {
	// Touch records (or refreshes) an entry. Called on every cache write
	// and on every cache hit (so LastAccess reflects actual use).
	Touch(buildID string, kind Kind, size int64) error

	// TotalBytes returns the sum of recorded entry sizes.
	TotalBytes() (int64, error)

	// EvictTo deletes the LRU-oldest entries until TotalBytes ≤ maxBytes.
	// Returns the entries that were evicted (caller is responsible for
	// removing the corresponding files).
	EvictTo(maxBytes int64) ([]Entry, error)

	// Iter visits every entry. The callback returns false to stop early.
	// Used at startup to re-populate from disk.
	Iter(yield func(Entry) bool) error

	// Forget removes a single entry's row (used during file deletion).
	Forget(buildID string, kind Kind) error

	Close() error
}
```

- [ ] **Step 2: Build to confirm package compiles**

```bash
go build ./symbolize/debuginfod/cache/...
```
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add symbolize/debuginfod/cache/index.go
git commit -m "cache: define Index interface and Entry type"
```

---

### Task 10: SQLite index implementation

**Files:**
- Create: `symbolize/debuginfod/cache/index_sqlite.go`
- Test: `symbolize/debuginfod/cache/index_sqlite_test.go`
- Modify: `go.mod` (add `modernc.org/sqlite`)

- [ ] **Step 1: Add the dep**

```bash
go get modernc.org/sqlite
```

- [ ] **Step 2: Write the failing test**

`symbolize/debuginfod/cache/index_sqlite_test.go`:

```go
package cache

import (
	"path/filepath"
	"testing"
	"time"
)

func newSQLiteIdx(t *testing.T) Index {
	t.Helper()
	dir := t.TempDir()
	idx, err := NewSQLiteIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSQLiteIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestSQLiteIndexTouchAndTotal(t *testing.T) {
	idx := newSQLiteIdx(t)
	if err := idx.Touch("aabb", KindDebuginfo, 100); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := idx.Touch("ccdd", KindExecutable, 250); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	total, err := idx.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	if total != 350 {
		t.Fatalf("TotalBytes = %d, want 350", total)
	}
}

func TestSQLiteIndexTouchUpdatesAccess(t *testing.T) {
	idx := newSQLiteIdx(t)
	if err := idx.Touch("aabb", KindDebuginfo, 100); err != nil {
		t.Fatalf("first Touch: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := idx.Touch("aabb", KindDebuginfo, 100); err != nil {
		t.Fatalf("second Touch: %v", err)
	}
	var seen int
	if err := idx.Iter(func(e Entry) bool {
		seen++
		if e.BuildID != "aabb" {
			t.Fatalf("BuildID = %q", e.BuildID)
		}
		return true
	}); err != nil {
		t.Fatalf("Iter: %v", err)
	}
	if seen != 1 {
		t.Fatalf("Iter visited %d entries, want 1 (Touch must upsert)", seen)
	}
}

func TestSQLiteIndexEvictToOldestFirst(t *testing.T) {
	idx := newSQLiteIdx(t)
	mustTouch(t, idx, "first", KindDebuginfo, 100)
	time.Sleep(2 * time.Millisecond)
	mustTouch(t, idx, "second", KindDebuginfo, 100)
	time.Sleep(2 * time.Millisecond)
	mustTouch(t, idx, "third", KindDebuginfo, 100)

	evicted, err := idx.EvictTo(150)
	if err != nil {
		t.Fatalf("EvictTo: %v", err)
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %d entries, want 2", len(evicted))
	}
	if evicted[0].BuildID != "first" || evicted[1].BuildID != "second" {
		t.Fatalf("eviction order: %+v", evicted)
	}
	total, _ := idx.TotalBytes()
	if total != 100 {
		t.Fatalf("TotalBytes after evict = %d, want 100", total)
	}
}

func TestSQLiteIndexForget(t *testing.T) {
	idx := newSQLiteIdx(t)
	mustTouch(t, idx, "aa", KindDebuginfo, 50)
	if err := idx.Forget("aa", KindDebuginfo); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	total, _ := idx.TotalBytes()
	if total != 0 {
		t.Fatalf("TotalBytes after Forget = %d, want 0", total)
	}
}

func mustTouch(t *testing.T, idx Index, b string, k Kind, n int64) {
	t.Helper()
	if err := idx.Touch(b, k, n); err != nil {
		t.Fatalf("Touch(%s): %v", b, err)
	}
}
```

- [ ] **Step 3: Run, confirm it fails**

```bash
go test ./symbolize/debuginfod/cache/... -run SQLite
```
Expected: FAIL — `NewSQLiteIndex` undefined.

- [ ] **Step 4: Implement `index_sqlite.go`**

```go
package cache

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteIndex struct {
	db *sql.DB
}

// NewSQLiteIndex opens or creates a SQLite database at dbPath.
func NewSQLiteIndex(dbPath string) (Index, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS entries (
			build_id    TEXT NOT NULL,
			kind        INTEGER NOT NULL,
			size        INTEGER NOT NULL,
			last_access INTEGER NOT NULL,
			PRIMARY KEY (build_id, kind)
		);
		CREATE INDEX IF NOT EXISTS idx_last_access ON entries(last_access);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &sqliteIndex{db: db}, nil
}

func (s *sqliteIndex) Touch(buildID string, kind Kind, size int64) error {
	now := time.Now().UnixNano()
	_, err := s.db.Exec(`
		INSERT INTO entries (build_id, kind, size, last_access)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(build_id, kind) DO UPDATE SET
			size = excluded.size,
			last_access = excluded.last_access
	`, buildID, int(kind), size, now)
	return err
}

func (s *sqliteIndex) TotalBytes() (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM entries`).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (s *sqliteIndex) EvictTo(maxBytes int64) ([]Entry, error) {
	total, err := s.TotalBytes()
	if err != nil {
		return nil, err
	}
	if total <= maxBytes {
		return nil, nil
	}
	excess := total - maxBytes

	rows, err := s.db.Query(`SELECT build_id, kind, size, last_access FROM entries ORDER BY last_access ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var evicted []Entry
	var freed int64
	for rows.Next() && freed < excess {
		var e Entry
		var k int
		var ns int64
		if err := rows.Scan(&e.BuildID, &k, &e.Size, &ns); err != nil {
			return evicted, err
		}
		e.Kind = Kind(k)
		e.LastAccess = time.Unix(0, ns)
		evicted = append(evicted, e)
		freed += e.Size
	}
	if err := rows.Err(); err != nil {
		return evicted, err
	}
	rows.Close()

	for _, e := range evicted {
		if _, err := s.db.Exec(`DELETE FROM entries WHERE build_id=? AND kind=?`, e.BuildID, int(e.Kind)); err != nil {
			return evicted, err
		}
	}
	return evicted, nil
}

func (s *sqliteIndex) Iter(yield func(Entry) bool) error {
	rows, err := s.db.Query(`SELECT build_id, kind, size, last_access FROM entries`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var k int
		var ns int64
		if err := rows.Scan(&e.BuildID, &k, &e.Size, &ns); err != nil {
			return err
		}
		e.Kind = Kind(k)
		e.LastAccess = time.Unix(0, ns)
		if !yield(e) {
			return nil
		}
	}
	return rows.Err()
}

func (s *sqliteIndex) Forget(buildID string, kind Kind) error {
	_, err := s.db.Exec(`DELETE FROM entries WHERE build_id=? AND kind=?`, buildID, int(kind))
	return err
}

func (s *sqliteIndex) Close() error { return s.db.Close() }
```

- [ ] **Step 5: Run tests, confirm pass**

```bash
go test ./symbolize/debuginfod/cache/... -run SQLite
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum symbolize/debuginfod/cache/index_sqlite.go symbolize/debuginfod/cache/index_sqlite_test.go
git commit -m "cache: add SQLite-backed Index implementation (default build)"
```

---

### Task 11: REMOVED — JSON index dropped

The plan originally shipped a JSON `Index` implementation behind the
`-tags noindex_sqlite` build tag, so restrictive deployments could opt out
of `modernc.org/sqlite`. After review, the JSON impl was dropped: SQLite is
pure-Go (no cgo) and tiny in binary size; the `Index` interface stays so
cache-layout tests can substitute a fake; nothing in the agent's deployment
story actually requires a SQLite-free build today.

**No code changes.** Downstream task numbers (12, 13, ...) are preserved.

---

### Task 12: Wire `Cache` to use `Index` + LRU eviction + pre-warm

**Files:**
- Modify: `symbolize/debuginfod/cache/cache.go`
- Modify: `symbolize/debuginfod/cache/cache_test.go`

- [ ] **Step 1: Write the failing test (extend the existing file)**

Append to `cache_test.go`:

```go
import (
	// ... existing imports ...
	"strconv"
)

func newCacheWithIndex(t *testing.T) *Cache {
	t.Helper()
	dir := t.TempDir()
	idx, err := NewSQLiteIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSQLiteIndex: %v", err)
	}
	c := &Cache{Dir: dir, Index: idx, MaxBytes: 250}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCacheWriteRecordsInIndex(t *testing.T) {
	c := newCacheWithIndex(t)
	if _, err := c.WriteAtomic(KindDebuginfo, "aabbccddee0011", strings.NewReader("xyz")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	total, err := c.Index.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	if total != 3 {
		t.Fatalf("TotalBytes = %d, want 3", total)
	}
}

func TestCacheEvictionDeletesFiles(t *testing.T) {
	c := newCacheWithIndex(t) // MaxBytes = 250

	for i := 0; i < 5; i++ {
		buildID := strings.Repeat(strconv.Itoa(i), 16)
		body := strings.Repeat("X", 100)
		if _, err := c.WriteAtomic(KindDebuginfo, buildID, strings.NewReader(body)); err != nil {
			t.Fatalf("WriteAtomic: %v", err)
		}
	}

	if err := c.Evict(); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	total, _ := c.Index.TotalBytes()
	if total > 250 {
		t.Fatalf("TotalBytes after evict = %d, want ≤ 250", total)
	}

	// Confirm matching files are physically gone.
	var remainingFiles int
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".debug") {
			remainingFiles++
		}
		return nil
	})
	if int64(remainingFiles)*100 != total {
		t.Fatalf("file count %d * 100 != index total %d (drift)", remainingFiles, total)
	}
}

func TestCachePrewarmRebuildsIndex(t *testing.T) {
	dir := t.TempDir()
	idx, _ := NewSQLiteIndex(filepath.Join(dir, "index.db"))
	c := &Cache{Dir: dir, Index: idx, MaxBytes: 1024}
	if _, err := c.WriteAtomic(KindDebuginfo, "ddee0011223344", strings.NewReader("hello")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	_ = c.Close()

	// New cache with empty index — pre-warm should pick up the file from disk.
	idx2, _ := NewSQLiteIndex(filepath.Join(dir, "index2.db"))
	c2 := &Cache{Dir: dir, Index: idx2, MaxBytes: 1024}
	defer c2.Close()
	if err := c2.Prewarm(); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	total, _ := c2.Index.TotalBytes()
	if total != 5 {
		t.Fatalf("TotalBytes after Prewarm = %d, want 5", total)
	}
}
```

- [ ] **Step 2: Run tests, confirm new ones fail**

```bash
go test ./symbolize/debuginfod/cache/...
```
Expected: FAIL on the three new tests (`MaxBytes`, `Evict`, `Prewarm`, `Close` undefined).

- [ ] **Step 3: Extend `cache.go` with `MaxBytes`, `Evict`, `Prewarm`, `Close`**

Add to the `Cache` struct:

```go
type Cache struct {
	Dir      string
	Index    Index
	MaxBytes int64 // 0 means unbounded
}
```

Modify `WriteAtomic` to call `c.Index.Touch(...)` after a successful write (but before returning):

```go
// at the bottom of WriteAtomic, after successful os.Rename:
size, _ := fileSize(abs)
if c.Index != nil {
	if err := c.Index.Touch(buildID, kind, size); err != nil {
		// Best-effort: log; don't fail the fetch.
		// (No logger in this package; caller will log via Stats / errors.)
	}
}
return abs, nil
```

Helper:

```go
func fileSize(p string) (int64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
```

Add `Evict`:

```go
// Evict deletes LRU entries until total cache size ≤ MaxBytes. Safe to call
// at any time. No-op when Index is nil or MaxBytes ≤ 0.
func (c *Cache) Evict() error {
	if c.Index == nil || c.MaxBytes <= 0 {
		return nil
	}
	evicted, err := c.Index.EvictTo(c.MaxBytes)
	if err != nil {
		return err
	}
	for _, e := range evicted {
		abs := c.AbsPath(e.BuildID, e.Kind)
		if abs == "" {
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Try to keep the index in sync even if the file is gone.
		}
	}
	return nil
}
```

(Add imports: `"errors"`, `"io/fs"`.)

Add `Prewarm`:

```go
// Prewarm walks Dir and calls Index.Touch for every existing artifact.
// Recovers from index loss (e.g., crash); also adopts an inherited cache
// from a previous process.
func (c *Cache) Prewarm() error {
	if c.Index == nil {
		return ErrNoIndex
	}
	root := filepath.Join(c.Dir, ".build-id")
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		buildID, kind, ok := parsePath(c.Dir, path)
		if !ok {
			return nil
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil
		}
		return c.Index.Touch(buildID, kind, st.Size())
	})
}

// parsePath inverts pathFor for files under <dir>/.build-id/<NN>/<rest>{.debug,}.
// Returns ok=false for paths outside that layout.
func parsePath(dir, path string) (buildID string, kind Kind, ok bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 3 || parts[0] != ".build-id" {
		return
	}
	prefix := parts[1]
	rest := parts[2]
	if strings.HasSuffix(rest, ".debug") {
		kind = KindDebuginfo
		rest = strings.TrimSuffix(rest, ".debug")
	} else {
		kind = KindExecutable
	}
	return prefix + rest, kind, true
}
```

(Imports: `"strings"`, `"io/fs"`.)

Add `Close`:

```go
func (c *Cache) Close() error {
	if c.Index == nil {
		return nil
	}
	return c.Index.Close()
}
```

- [ ] **Step 4: Run tests, confirm pass**

```bash
go test ./symbolize/debuginfod/cache/...
```
Expected: PASS (8 tests).

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/cache/cache.go symbolize/debuginfod/cache/cache_test.go
git commit -m "cache: integrate Index for LRU eviction and prewarm"
```

---

## Phase 5 — HTTP fetcher

### Task 13: `fetcher` with URL fallback + status handling

**Files:**
- Create: `symbolize/debuginfod/fetcher.go`
- Test: `symbolize/debuginfod/fetcher_test.go`

- [ ] **Step 1: Write the failing test**

`symbolize/debuginfod/fetcher_test.go`:

```go
package debuginfod

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetchOnce200(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/buildid/aabb/debuginfo" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
	})
	f := newFetcher([]string{url}, http.DefaultClient)
	body, err := f.fetchURL(t.Context(), url+"/buildid/aabb/debuginfo")
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	defer body.Close()
	buf := make([]byte, 16)
	n, _ := body.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Fatalf("body = %q", buf[:n])
	}
}

func TestFetchOnceFallback404(t *testing.T) {
	first := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	second := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	f := newFetcher([]string{first, second}, http.DefaultClient)
	body, err := f.fetch(t.Context(), "debuginfo", "aabbcc")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer body.Close()
	buf := make([]byte, 16)
	n, _ := body.Read(buf)
	if string(buf[:n]) != "ok" {
		t.Fatalf("body = %q", buf[:n])
	}
}

func TestFetchAll404ReturnsErrNotFound(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	f := newFetcher([]string{url}, http.DefaultClient)
	if _, err := f.fetch(t.Context(), "debuginfo", "aabbcc"); err == nil || err.Error() != "debuginfod: not found" {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	})
	client := &http.Client{Timeout: 50 * time.Millisecond}
	f := newFetcher([]string{url}, client)
	if _, err := f.fetch(t.Context(), "debuginfo", "aabbcc"); err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestFetchTrimsTrailingSlash(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Asserts no double-slash like /buildid//debuginfo.
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("path has //: %q", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	f := newFetcher([]string{url + "/"}, http.DefaultClient) // trailing slash
	body, err := f.fetch(t.Context(), "debuginfo", "aabbcc")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	body.Close()
}
```

- [ ] **Step 2: Run, confirm fail**

```bash
go test ./symbolize/debuginfod/...
```
Expected: FAIL — `newFetcher` undefined.

- [ ] **Step 3: Implement `fetcher.go`**

```go
package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrNotFound indicates every configured debuginfod URL returned 404 for
// the requested build-id.
var ErrNotFound = errors.New("debuginfod: not found")

type fetcher struct {
	client *http.Client
	urls   []string // pre-trimmed of trailing "/"
}

func newFetcher(urls []string, client *http.Client) *fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	trimmed := make([]string, 0, len(urls))
	for _, u := range urls {
		trimmed = append(trimmed, strings.TrimRight(u, "/"))
	}
	return &fetcher{client: client, urls: trimmed}
}

// fetch tries each URL in order. Returns the response body on the first 200.
// 404 falls through to the next URL; non-200/404 records lastErr and
// continues. Caller is responsible for Close()ing the returned body.
func (f *fetcher) fetch(ctx context.Context, kind, buildID string) (io.ReadCloser, error) {
	var lastErr error
	for _, base := range f.urls {
		url := base + "/buildid/" + buildID + "/" + kind
		body, err := f.fetchURL(ctx, url)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		lastErr = err
	}
	if lastErr == nil {
		return nil, ErrNotFound
	}
	return nil, lastErr
}

// fetchURL returns the body on 200, ErrNotFound on 404, or a wrapped
// error on other statuses or transport failure.
func (f *fetcher) fetchURL(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, ErrNotFound
	default:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("debuginfod: %s returned %d", url, resp.StatusCode)
	}
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./symbolize/debuginfod/...
```
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/fetcher.go symbolize/debuginfod/fetcher_test.go
git commit -m "debuginfod: add HTTP fetcher with URL fallback and 404 handling"
```

---

### Task 14: Singleflight wrapper

**Files:**
- Create: `symbolize/debuginfod/singleflight.go`
- Test: append to `symbolize/debuginfod/fetcher_test.go`

- [ ] **Step 1: Add the dep**

```bash
go get golang.org/x/sync/singleflight
```

- [ ] **Step 2: Write the failing test**

Append to `symbolize/debuginfod/fetcher_test.go`:

```go
import (
	// ... existing imports ...
	"sync"
	"sync/atomic"
)

func TestSingleflightCollapsesConcurrentFetches(t *testing.T) {
	var calls atomic.Int32
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})

	dir := t.TempDir()
	c := &cacheBackend{
		baseDir: dir,
	}
	sf := newSingleflightFetcher(newFetcher([]string{url}, http.DefaultClient), c)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			path, err := sf.fetchAndStore(t.Context(), "debuginfo", "aabbccddeeff")
			if err != nil {
				t.Errorf("fetch: %v", err)
			}
			if path == "" {
				t.Errorf("empty path")
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("HTTP call count = %d, want 1 (singleflight failure)", got)
	}
}
```

`cacheBackend` is a tiny test stub for the singleflight test that exposes only what `fetchAndStore` needs. We need to coordinate types — see Step 3.

- [ ] **Step 3: Implement `singleflight.go` and the cacheBackend stub**

```go
package debuginfod

import (
	"context"
	"io"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"golang.org/x/sync/singleflight"
)

// storer is the slice of cache.Cache that the singleflight fetcher needs.
// The interface lets tests substitute a stub.
type storer interface {
	WriteAtomic(kind cache.Kind, buildID string, body io.Reader) (string, error)
}

type singleflightFetcher struct {
	upstream *fetcher
	cache    storer
	sf       singleflight.Group
}

func newSingleflightFetcher(upstream *fetcher, store storer) *singleflightFetcher {
	return &singleflightFetcher{upstream: upstream, cache: store}
}

// fetchAndStore collapses concurrent fetches keyed by (kind, buildID).
// On success the response body is streamed into the cache and the absolute
// final path is returned.
func (s *singleflightFetcher) fetchAndStore(ctx context.Context, kindStr, buildID string) (string, error) {
	key := kindStr + ":" + buildID
	res, err, _ := s.sf.Do(key, func() (any, error) {
		body, err := s.upstream.fetch(ctx, kindStr, buildID)
		if err != nil {
			return "", err
		}
		defer body.Close()
		var k cache.Kind
		switch kindStr {
		case "debuginfo":
			k = cache.KindDebuginfo
		case "executable":
			k = cache.KindExecutable
		}
		return s.cache.WriteAtomic(k, buildID, body)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}
```

For the test stub, add this small helper to `fetcher_test.go`:

```go
type cacheBackend struct {
	baseDir string
}

func (c *cacheBackend) WriteAtomic(kind cache.Kind, buildID string, body io.Reader) (string, error) {
	cc := &cache.Cache{Dir: c.baseDir}
	return cc.WriteAtomic(kind, buildID, body)
}
```

(Add `"io"` and the cache import to the test file.)

- [ ] **Step 4: Run tests**

```bash
go test ./symbolize/debuginfod/...
```
Expected: PASS — `calls = 1` from 20 goroutines.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum symbolize/debuginfod/singleflight.go symbolize/debuginfod/fetcher_test.go
git commit -m "debuginfod: collapse concurrent fetches via singleflight"
```

---

## Phase 6 — Dispatcher + cgo bridge

### Task 15: `Stats` struct + atomic counters

**Files:**
- Create: `symbolize/debuginfod/stats.go`
- Test: `symbolize/debuginfod/stats_test.go`

- [ ] **Step 1: Write failing test**

`symbolize/debuginfod/stats_test.go`:

```go
package debuginfod

import "testing"

func TestStatsAtomicAccumulates(t *testing.T) {
	var as atomicStats
	as.cacheHits.Add(3)
	as.cacheMisses.Add(2)
	as.fetch404s.Add(1)
	got := as.snapshot()
	if got.CacheHits != 3 {
		t.Fatalf("CacheHits = %d, want 3", got.CacheHits)
	}
	if got.CacheMisses != 2 {
		t.Fatalf("CacheMisses = %d, want 2", got.CacheMisses)
	}
	if got.Fetch404s != 1 {
		t.Fatalf("Fetch404s = %d, want 1", got.Fetch404s)
	}
}
```

- [ ] **Step 2: Run, confirm fail**

```bash
go test ./symbolize/debuginfod/... -run Stats
```
Expected: FAIL.

- [ ] **Step 3: Implement `stats.go`**

```go
package debuginfod

import "sync/atomic"

// Stats reports operational counters for a Symbolizer. Read via Stats().
type Stats struct {
	CacheHits, CacheMisses, CacheEvictions             uint64
	FetchSuccessDebuginfo, FetchSuccessExecutable      uint64
	Fetch404s, FetchErrors                             uint64
	FetchBytesTotal                                    uint64
	InFlightFetches                                    int64
	DispatcherCalls, DispatcherSkippedLocal            uint64
	DispatcherPanics                                   uint64
}

type atomicStats struct {
	cacheHits, cacheMisses, cacheEvictions             atomic.Uint64
	fetchSuccessDebuginfo, fetchSuccessExecutable      atomic.Uint64
	fetch404s, fetchErrors                             atomic.Uint64
	fetchBytesTotal                                    atomic.Uint64
	inFlightFetches                                    atomic.Int64
	dispatcherCalls, dispatcherSkippedLocal            atomic.Uint64
	dispatcherPanics                                   atomic.Uint64
}

func (a *atomicStats) snapshot() Stats {
	return Stats{
		CacheHits:              a.cacheHits.Load(),
		CacheMisses:            a.cacheMisses.Load(),
		CacheEvictions:         a.cacheEvictions.Load(),
		FetchSuccessDebuginfo:  a.fetchSuccessDebuginfo.Load(),
		FetchSuccessExecutable: a.fetchSuccessExecutable.Load(),
		Fetch404s:              a.fetch404s.Load(),
		FetchErrors:            a.fetchErrors.Load(),
		FetchBytesTotal:        a.fetchBytesTotal.Load(),
		InFlightFetches:        a.inFlightFetches.Load(),
		DispatcherCalls:        a.dispatcherCalls.Load(),
		DispatcherSkippedLocal: a.dispatcherSkippedLocal.Load(),
		DispatcherPanics:       a.dispatcherPanics.Load(),
	}
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
go test ./symbolize/debuginfod/... -run Stats
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/stats.go symbolize/debuginfod/stats_test.go
git commit -m "debuginfod: add Stats with atomic counters"
```

---

### Task 16: `localResolutionPossible` + ELF DWARF probes

**Files:**
- Create: `symbolize/debuginfod/resolution.go`
- Test: `symbolize/debuginfod/resolution_test.go`

- [ ] **Step 1: Write failing test**

`symbolize/debuginfod/resolution_test.go`:

```go
package debuginfod

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasDwarfTrue(t *testing.T) {
	// /usr/bin/grep is typically built with debug info on Debian-style systems;
	// fall back to skipping if we can't find a binary that has DWARF.
	for _, p := range []string{"/usr/bin/grep", "/usr/bin/ls", "/bin/ls"} {
		if _, err := os.Stat(p); err == nil {
			if hasDwarf(p) {
				return // PASS
			}
		}
	}
	t.Skip("no system binary with DWARF found")
}

func TestHasDwarfMissingFile(t *testing.T) {
	if hasDwarf("/nonexistent/path") {
		t.Fatalf("hasDwarf on missing path returned true")
	}
}

func TestBinaryReadable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !binaryReadable(good) {
		t.Fatalf("binaryReadable(%s) = false", good)
	}
	if binaryReadable(filepath.Join(dir, "missing")) {
		t.Fatalf("binaryReadable(missing) = true")
	}
}

func TestHasResolvableDebuglinkMissing(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "x")
	if err := os.WriteFile(bin, []byte("not an elf"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if hasResolvableDebuglink(bin, nil) {
		t.Fatalf("non-ELF file returned true")
	}
}
```

- [ ] **Step 2: Run, confirm fail**

```bash
go test ./symbolize/debuginfod/... -run Has\|Binary
```
Expected: FAIL.

- [ ] **Step 3: Implement `resolution.go`**

```go
package debuginfod

import (
	"debug/elf"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// hasDwarf reports whether the ELF at path has a non-empty .debug_info
// section (the cheapest "DWARF is present" signal).
func hasDwarf(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sec := f.Section(".debug_info")
	return sec != nil && sec.Size > 0
}

// binaryReadable reports whether path can be opened (no read of contents).
// Distinguishes "binary on disk" from "sidecar can't see peer's filesystem".
func binaryReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// hasResolvableDebuglink returns true when path's .gnu_debuglink section
// names a file that exists in standard search paths plus any caller-supplied
// extras (e.g., the debuginfod cache dir).
func hasResolvableDebuglink(path string, extraDirs []string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sec := f.Section(".gnu_debuglink")
	if sec == nil {
		return false
	}
	data, err := sec.Data()
	if err != nil {
		return false
	}
	// Layout: NUL-terminated filename, padded to 4 bytes, then crc32.
	end := 0
	for end < len(data) && data[end] != 0 {
		end++
	}
	if end == 0 {
		return false
	}
	name := string(data[:end])
	candidates := append([]string{
		filepath.Join(filepath.Dir(path), name),
		filepath.Join(filepath.Dir(path), ".debug", name),
		filepath.Join("/usr/lib/debug", filepath.Dir(path), name),
		filepath.Join("/usr/lib/debug", name),
	}, extraDirsExpand(extraDirs, name)...)
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
	}
	return false
}

func extraDirsExpand(dirs []string, name string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(d, name))
	}
	return out
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
go test ./symbolize/debuginfod/... -run Has\|Binary
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/resolution.go symbolize/debuginfod/resolution_test.go
git commit -m "debuginfod: add ELF probes for hasDwarf / binaryReadable / debuglink"
```

---

### Task 17: `readBuildID` for the dispatcher (mapsFile → symbolicPath fallback)

**Files:**
- Create: `symbolize/debuginfod/buildid.go`
- Test: `symbolize/debuginfod/buildid_test.go`

- [ ] **Step 1: Write failing test**

`symbolize/debuginfod/buildid_test.go`:

```go
package debuginfod

import "testing"

func TestReadBuildIDPrefersMapsFile(t *testing.T) {
	for _, p := range []string{"/usr/bin/grep", "/usr/bin/ls", "/bin/ls"} {
		id := readBuildID(p, "/this/path/should/not/be/read")
		if id != "" {
			return // PASS — read from mapsFile
		}
	}
	t.Skip("no system binary with build-id available")
}

func TestReadBuildIDFallsBackToSymbolicPath(t *testing.T) {
	for _, p := range []string{"/usr/bin/grep", "/usr/bin/ls", "/bin/ls"} {
		id := readBuildID("/nonexistent/path", p)
		if id != "" {
			return
		}
	}
	t.Skip("no system binary with build-id available")
}

func TestReadBuildIDNothingFound(t *testing.T) {
	if id := readBuildID("/nonexistent/a", "/nonexistent/b"); id != "" {
		t.Fatalf("readBuildID = %q", id)
	}
}
```

- [ ] **Step 2: Run, confirm fail**

```bash
go test ./symbolize/debuginfod/... -run ReadBuildID
```
Expected: FAIL.

- [ ] **Step 3: Implement `buildid.go`**

```go
package debuginfod

import "github.com/dpsoft/perf-agent/unwind/procmap"

// readBuildID returns the GNU build-id (lowercase hex) of the ELF at
// mapsFile, falling back to symbolicPath. Empty string when neither path
// resolves (or the ELF has no .note.gnu.build-id).
//
// mapsFile is the kernel-resolved /proc/<pid>/map_files/<va>-<va> symlink,
// present even when symbolicPath isn't reachable from the agent's
// filesystem. symbolicPath is the path string from /proc/<pid>/maps.
func readBuildID(mapsFile, symbolicPath string) string {
	if mapsFile != "" {
		if id, _ := procmap.ReadBuildID(mapsFile); id != "" {
			return id
		}
	}
	if symbolicPath != "" && symbolicPath != mapsFile {
		if id, _ := procmap.ReadBuildID(symbolicPath); id != "" {
			return id
		}
	}
	return ""
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
go test ./symbolize/debuginfod/... -run ReadBuildID
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/buildid.go symbolize/debuginfod/buildid_test.go
git commit -m "debuginfod: read build-id with mapsFile→symbolicPath fallback"
```

---

### Task 18: `Options` struct + `Symbolizer` skeleton (no dispatcher yet)

**Files:**
- Create: `symbolize/debuginfod/options.go`
- Create: `symbolize/debuginfod/symbolizer.go` (skeleton — `New`, `Close`, `Stats`)
- Create: `symbolize/debuginfod/errors.go`

This task lays out the construction path and lifecycle without yet wiring the dispatcher. The next task adds the dispatcher.

- [ ] **Step 1: Implement `errors.go`**

```go
package debuginfod

import "errors"

var (
	ErrClosed       = errors.New("debuginfod: closed")
	ErrNoURLs       = errors.New("debuginfod: no URLs configured")
	ErrInvalidOpts  = errors.New("debuginfod: invalid options")
)
```

- [ ] **Step 2: Implement `options.go`**

```go
package debuginfod

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

type Options struct {
	URLs           []string
	CacheDir       string
	CacheMaxBytes  int64
	FetchTimeout   time.Duration
	FailClosed     bool
	Resolver       *procmap.Resolver
	HTTPClient     *http.Client
	Logger         *slog.Logger
	Demangle       bool
	InlinedFns     bool
	CodeInfo       bool
}

// validate fills in defaults and returns ErrInvalidOpts when something is
// outright wrong.
func (o *Options) validate() error {
	if len(o.URLs) == 0 {
		return ErrNoURLs
	}
	if o.CacheDir == "" {
		o.CacheDir = "/tmp/perf-agent-debuginfod"
	}
	if o.CacheMaxBytes == 0 {
		o.CacheMaxBytes = 2 << 30 // 2 GiB
	}
	if o.FetchTimeout == 0 {
		o.FetchTimeout = 30 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.FetchTimeout}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(devNull{}, nil))
	}
	return nil
}

// devNull discards log output for the default logger.
type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }
```

- [ ] **Step 3: Implement `symbolizer.go` skeleton**

```go
package debuginfod

import (
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

// Symbolizer resolves abs addresses against a process while consulting a
// debuginfod-protocol server for missing debug info. Implements
// symbolize.Symbolizer.
type Symbolizer struct {
	opts     Options
	cache    *cache.Cache
	fetcher  *fetcher
	sf       *singleflightFetcher
	stats    atomicStats
	closed   atomic.Bool
	inflight sync.WaitGroup
}

// New constructs a Symbolizer from opts. opts.URLs must be non-empty.
func New(opts Options) (*Symbolizer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	idx, err := openIndex(filepath.Join(opts.CacheDir, indexFilename))
	if err != nil {
		return nil, err
	}
	c := &cache.Cache{
		Dir:      opts.CacheDir,
		Index:    idx,
		MaxBytes: opts.CacheMaxBytes,
	}
	if err := c.Prewarm(); err != nil {
		_ = c.Close()
		return nil, err
	}
	f := newFetcher(opts.URLs, opts.HTTPClient)
	sf := newSingleflightFetcher(f, c)

	s := &Symbolizer{
		opts:    opts,
		cache:   c,
		fetcher: f,
		sf:      sf,
	}
	// Note: the cgo blazesym handle is wired in Task 19. For now the
	// SymbolizeProcess method is a placeholder that returns an error so
	// callers don't accidentally use a half-built Symbolizer.
	return s, nil
}

// SymbolizeProcess is a placeholder until Task 19 wires the cgo dispatcher.
func (s *Symbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	return nil, ErrInvalidOpts // replaced in Task 19
}

// Close drains in-flight dispatcher invocations, frees blazesym, and closes
// the cache index. Idempotent.
func (s *Symbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.inflight.Wait()
	// blazesym free goes here in Task 19.
	if t := s.opts.HTTPClient.Transport; t != nil {
		if cit, ok := t.(interface{ CloseIdleConnections() }); ok {
			cit.CloseIdleConnections()
		}
	}
	return s.cache.Close()
}

// Stats returns a snapshot of operational counters.
func (s *Symbolizer) Stats() Stats { return s.stats.snapshot() }
```

- [ ] **Step 4: Implement `openIndex` helper**

Append to `symbolizer.go`:

```go
const indexFilename = "index.db"

// openIndex constructs the cache index. SQLite-only; the indirection is
// kept so future tests can inject a fake Index without changing this site.
func openIndex(path string) (cache.Index, error) {
	return cache.NewSQLiteIndex(path)
}
```

- [ ] **Step 5: Smoke test the construction path**

Create `symbolize/debuginfod/symbolizer_test.go`:

```go
package debuginfod

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewWithoutURLsErrors(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatalf("New with no URLs: expected error")
	}
}

func TestNewBasicLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	s, err := New(Options{
		URLs:     []string{srv.URL},
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent close
	if err := s.Close(); err != ErrClosed {
		t.Fatalf("second Close: %v, want ErrClosed", err)
	}
}
```

```bash
go test ./symbolize/debuginfod/... -run "NewWithout|NewBasic"
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add symbolize/debuginfod/options.go symbolize/debuginfod/symbolizer.go symbolize/debuginfod/errors.go symbolize/debuginfod/symbolizer_test.go
git commit -m "debuginfod: add Options, Symbolizer skeleton, lifecycle (no dispatcher yet)"
```

---

### Task 19: cgo bridge + dispatcher decision tree + SymbolizeProcess

**Files:**
- Create: `symbolize/debuginfod/dispatcher.go`
- Modify: `symbolize/debuginfod/symbolizer.go` (replace placeholder `SymbolizeProcess`, add cgo handle, wire dispatcher)
- Test: `symbolize/debuginfod/dispatcher_test.go`

This is the biggest task. The dispatcher is the entire point of this milestone.

- [ ] **Step 1: Write the unit test for `dispatchDecision` (the pure-Go logic)**

`symbolize/debuginfod/dispatcher_test.go`:

```go
package debuginfod

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// We test dispatchDecision (pure Go) by giving it a handcrafted Symbolizer
// state — no cgo involved.

func newTestSymbolizer(t *testing.T, urls []string) *Symbolizer {
	t.Helper()
	s, err := New(Options{
		URLs:     urls,
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDispatchDecisionNoBuildID(t *testing.T) {
	s := newTestSymbolizer(t, []string{"http://example.invalid"})
	got := s.dispatchDecision(t.Context(), "/dev/null", "")
	if got != "" {
		t.Fatalf("expected empty (NULL), got %q", got)
	}
}

func TestDispatchDecisionCachedExecutable(t *testing.T) {
	s := newTestSymbolizer(t, []string{"http://example.invalid"})
	const buildID = "aabbccddeeff0011"
	abs, err := s.cache.WriteAtomic(0 /*KindDebuginfo*/, buildID, strings.NewReader("ignored"))
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	_ = abs
	// Place an executable on disk
	exec := s.cache.AbsPath(buildID, 1 /*KindExecutable*/)
	if err := os.MkdirAll(filepath.Dir(exec), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(exec, []byte("exe"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Stub readBuildID by passing a path that would resolve through procmap.
	// The cleanest test: ensure a /usr/bin/* exists with a build-id, copy
	// to tmp, override with our cached buildID via setBuildIDForTest hook.
	t.Skip("This case is exercised in the integration test (requires real ELF with known build-id)")
}

func TestDispatchDecisionFetchOnSidecar(t *testing.T) {
	body := "FAKE_ELF"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/executable") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	s := newTestSymbolizer(t, []string{srv.URL})
	// We bypass readBuildID by calling the inner method directly with a
	// known build-id and a path that doesn't exist (binaryReadable=false),
	// which forces the case 4 branch.
	got := s.dispatchDecisionForTest(t.Context(), "/nonexistent/foo", "/nonexistent/foo", "deadbeef0011223344")
	if !strings.HasSuffix(got, "/.build-id/de/adbeef0011223344") {
		t.Fatalf("expected returned executable path, got %q", got)
	}
	// And the file is real:
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read returned path: %v", err)
	}
	if string(data) != body {
		t.Fatalf("file body = %q, want %q", data, body)
	}
}
```

- [ ] **Step 2: Run, confirm fail (`dispatchDecision`, `dispatchDecisionForTest` undefined)**

```bash
go test ./symbolize/debuginfod/... -run Dispatch
```
Expected: FAIL.

- [ ] **Step 3: Implement `dispatcher.go` (cgo + Go dispatch)**

```go
package debuginfod

/*
#cgo CFLAGS: -I${SRCDIR}/../../  // adjusted by Makefile; placeholder
#include <stdlib.h>
#include "blazesym.h"

extern char* goDispatchCb(char* maps_file, char* symbolic_path, void* ctx);
typedef char* (*blaze_dispatch_fn)(const char*, const char*, void*);

static void install_dispatch(blaze_symbolizer_opts* opts,
                             blaze_symbolizer_dispatch* slot,
                             void* ctx) {
    slot->dispatch_cb = (blaze_dispatch_fn)goDispatchCb;
    slot->ctx = ctx;
    opts->process_dispatch = slot;
}

static blaze_symbolizer_opts make_default_opts(_Bool code_info, _Bool inlined_fns, _Bool demangle) {
    blaze_symbolizer_opts opts;
    memset(&opts, 0, sizeof(opts));
    opts.type_size = sizeof(opts);
    opts.auto_reload = 1;
    opts.code_info = code_info;
    opts.inlined_fns = inlined_fns;
    opts.demangle = demangle;
    return opts;
}

static blaze_symbolize_src_process make_process_src(uint32_t pid) {
    blaze_symbolize_src_process src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
    src.pid = pid;
    src.debug_syms = 1;
    return src;
}

static const blaze_sym* sym_at(const blaze_syms* syms, size_t i) {
    return &syms->syms[i];
}
*/
import "C"

import (
	"context"
	"runtime/cgo"
	"unsafe"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

//export goDispatchCb
func goDispatchCb(mapsFile, symbolicPath *C.char, ctx unsafe.Pointer) (ret *C.char) {
	h := cgo.Handle(uintptr(ctx))
	s := h.Value().(*Symbolizer)

	s.inflight.Add(1)
	defer s.inflight.Done()
	defer func() {
		if r := recover(); r != nil {
			s.stats.dispatcherPanics.Add(1)
			ret = nil
		}
	}()
	return s.cgoDispatch(C.GoString(mapsFile), C.GoString(symbolicPath))
}

// cgoDispatch is the C-side entry. It calls the pure-Go decision function
// and returns the result as a libc-malloc'd char* (blazesym free()s it).
func (s *Symbolizer) cgoDispatch(mapsFile, symbolicPath string) *C.char {
	ctx := context.Background() // M2: derive from a SymbolizeProcess context
	if s.opts.FetchTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.FetchTimeout)
		defer cancel()
	}
	path := s.dispatchDecision(ctx, mapsFile, symbolicPath)
	if path == "" {
		return nil
	}
	return C.CString(path)
}

// dispatchDecision is the pure-Go four-case routing logic. Returns the
// override path for blazesym to use, or "" to mean "use blazesym default".
//
// See spec §"Dispatcher decision tree".
func (s *Symbolizer) dispatchDecision(ctx context.Context, mapsFile, symbolicPath string) string {
	s.stats.dispatcherCalls.Add(1)

	buildID := readBuildID(mapsFile, symbolicPath)
	if buildID == "" {
		return ""
	}

	// Case 1: cached executable from prior fetch
	if s.cache.Has(buildID, cache.KindExecutable) {
		s.stats.cacheHits.Add(1)
		return s.cache.AbsPath(buildID, cache.KindExecutable)
	}

	// Case 2: blazesym default would work
	if s.localResolutionPossible(symbolicPath, buildID) {
		s.stats.dispatcherSkippedLocal.Add(1)
		return ""
	}

	// Case 3: binary on disk, no DWARF locally → fetch /debuginfo
	if binaryReadable(symbolicPath) {
		s.stats.cacheMisses.Add(1)
		if _, err := s.sf.fetchAndStore(ctx, "debuginfo", buildID); err != nil {
			s.recordFetchErr(err)
		} else {
			s.stats.fetchSuccessDebuginfo.Add(1)
		}
		return ""
	}

	// Case 4: binary not on disk → fetch /executable
	s.stats.cacheMisses.Add(1)
	abs, err := s.sf.fetchAndStore(ctx, "executable", buildID)
	if err != nil {
		s.recordFetchErr(err)
		return ""
	}
	s.stats.fetchSuccessExecutable.Add(1)
	return abs
}

// dispatchDecisionForTest is a test-only entry that lets unit tests bypass
// readBuildID (which depends on the ELF having .note.gnu.build-id).
func (s *Symbolizer) dispatchDecisionForTest(ctx context.Context, mapsFile, symbolicPath, buildID string) string {
	s.stats.dispatcherCalls.Add(1)

	if s.cache.Has(buildID, cache.KindExecutable) {
		return s.cache.AbsPath(buildID, cache.KindExecutable)
	}
	if s.localResolutionPossible(symbolicPath, buildID) {
		return ""
	}
	if binaryReadable(symbolicPath) {
		_, _ = s.sf.fetchAndStore(ctx, "debuginfo", buildID)
		return ""
	}
	abs, err := s.sf.fetchAndStore(ctx, "executable", buildID)
	if err != nil {
		return ""
	}
	return abs
}

// localResolutionPossible is the cheap pre-check that lets the dispatcher
// return "" when blazesym's default would succeed.
func (s *Symbolizer) localResolutionPossible(path, buildID string) bool {
	if s.cache.Has(buildID, cache.KindDebuginfo) {
		return true
	}
	if hasDwarf(path) {
		return true
	}
	if hasResolvableDebuglink(path, []string{s.opts.CacheDir}) {
		return true
	}
	return false
}

func (s *Symbolizer) recordFetchErr(err error) {
	if err == nil {
		return
	}
	if err == ErrNotFound {
		s.stats.fetch404s.Add(1)
		return
	}
	s.stats.fetchErrors.Add(1)
}
```

- [ ] **Step 4: Wire the cgo handle into `New` / `Close` (replace placeholder SymbolizeProcess)**

Open `symbolize/debuginfod/symbolizer.go`. Add to the struct:

```go
type Symbolizer struct {
	// ... existing ...
	csym       *C.blaze_symbolizer
	dispatch   C.blaze_symbolizer_dispatch
	handle     cgo.Handle
}
```

(Add `import "C"` and `runtime/cgo`. The cgo preamble already lives in `dispatcher.go`; cgo is per-file but C types span the package via the same preamble being generated once per package.)

Actually wait — cgo preambles aren't shared across files. We must keep all C declarations in `dispatcher.go` and reference them only from there. So restructure: keep the cgo struct fields out of `Symbolizer` definition in `symbolizer.go`. Put the cgo-state struct inside `dispatcher.go`:

```go
// dispatcher.go (append):

type cgoState struct {
	csym     *C.blaze_symbolizer
	dispatch C.blaze_symbolizer_dispatch
	handle   cgo.Handle
}

func newCgoState(s *Symbolizer) (*cgoState, error) {
	st := &cgoState{}
	st.handle = cgo.NewHandle(s)

	copts := C.make_default_opts(
		C._Bool(s.opts.CodeInfo),
		C._Bool(s.opts.InlinedFns),
		C._Bool(s.opts.Demangle),
	)
	C.install_dispatch(&copts, &st.dispatch, unsafe.Pointer(uintptr(st.handle)))

	st.csym = C.blaze_symbolizer_new_opts(&copts)
	if st.csym == nil {
		st.handle.Delete()
		return nil, fmt.Errorf("blaze_symbolizer_new_opts returned NULL")
	}
	return st, nil
}

func (st *cgoState) close() {
	C.blaze_symbolizer_free(st.csym)
	st.handle.Delete()
}

// symbolizeProcess is the C-side symbolize call.
func (st *cgoState) symbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	src := C.make_process_src(C.uint32_t(pid))
	caddr := (*C.uint64_t)(unsafe.Pointer(&ips[0]))
	syms := C.blaze_symbolize_process_abs_addrs(st.csym, &src, caddr, C.size_t(len(ips)))
	if syms == nil {
		return nil, fmt.Errorf("blaze_symbolize_process_abs_addrs returned NULL")
	}
	defer C.blaze_syms_free(syms)

	out := make([]symbolize.Frame, 0, int(syms.cnt))
	for i := 0; i < int(syms.cnt); i++ {
		csym := C.sym_at(syms, C.size_t(i))
		out = append(out, frameFromCSym(csym, ips[i]))
	}
	return out, nil
}

func frameFromCSym(c *C.blaze_sym, addr uint64) symbolize.Frame {
	f := symbolize.Frame{Address: addr}
	if c.name == nil {
		f.Reason = symbolize.FailureUnknownAddress
		return f
	}
	f.Name = C.GoString(c.name)
	if c.module != nil {
		f.Module = C.GoString(c.module)
	}
	f.Offset = uint64(c.offset)
	if c.code_info.file != nil {
		f.File = C.GoString(c.code_info.file)
		f.Line = int(c.code_info.line)
	}
	for j := C.size_t(0); j < c.inlined_cnt; j++ {
		in := (*C.blaze_symbolize_inlined_fn)(unsafe.Pointer(uintptr(unsafe.Pointer(c.inlined)) + uintptr(j)*unsafe.Sizeof(*c.inlined)))
		inFrame := symbolize.Frame{Address: addr, Module: f.Module}
		if in.name != nil {
			inFrame.Name = C.GoString(in.name)
		}
		if in.code_info.file != nil {
			inFrame.File = C.GoString(in.code_info.file)
			inFrame.Line = int(in.code_info.line)
		}
		f.Inlined = append(f.Inlined, inFrame)
	}
	return f
}
```

Open `symbolize/debuginfod/symbolizer.go` and update:

```go
type Symbolizer struct {
	opts     Options
	cache    *cache.Cache
	fetcher  *fetcher
	sf       *singleflightFetcher
	cgo      *cgoState
	stats    atomicStats
	closed   atomic.Bool
	inflight sync.WaitGroup
}

func New(opts Options) (*Symbolizer, error) {
	// ... existing setup until cgo wiring ...

	s := &Symbolizer{ /* existing fields */ }

	st, err := newCgoState(s)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	s.cgo = st
	return s, nil
}

func (s *Symbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	return s.cgo.symbolizeProcess(pid, ips)
}

func (s *Symbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.inflight.Wait()
	if s.cgo != nil {
		s.cgo.close()
	}
	if t := s.opts.HTTPClient.Transport; t != nil {
		if cit, ok := t.(interface{ CloseIdleConnections() }); ok {
			cit.CloseIdleConnections()
		}
	}
	return s.cache.Close()
}
```

- [ ] **Step 5: Update CGO flags so cgo finds blazesym.h**

The existing perf-agent Makefile passes `-I /home/diego/github/blazesym/capi/include` via `CGO_CFLAGS`. The package-level cgo `CFLAGS` directive in `dispatcher.go` should reference an absolute path or rely on the env. Pragmatic: omit a `CFLAGS` directive and rely on the Makefile-supplied `CGO_CFLAGS`:

In `dispatcher.go`, replace:

```c
#cgo CFLAGS: -I${SRCDIR}/../../  // adjusted by Makefile; placeholder
```

with:

```c
// CGO_CFLAGS provided by the build (Makefile or test harness).
```

(So no `#cgo CFLAGS:` line — rely on the env. The existing `perfagent` package builds the same way.)

- [ ] **Step 6: Run tests with cgo env**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./symbolize/debuginfod/... -run Dispatch
```
Expected: PASS (the sidecar-fetch test in step 1).

- [ ] **Step 7: Commit**

```bash
git add symbolize/debuginfod/dispatcher.go symbolize/debuginfod/symbolizer.go symbolize/debuginfod/dispatcher_test.go
git commit -m "debuginfod: wire blazesym process_dispatch via cgo, four-case decision tree"
```

---

## Phase 7 — Agent wiring

### Task 20: `chooseSymbolizer` factory + `Config` extensions

**Files:**
- Modify: `perfagent/options.go` (add config fields + WithDebuginfod options)
- Modify: `perfagent/agent.go` (add Symbolizer field + chooseSymbolizer + close ordering)

- [ ] **Step 1: Extend `Config` in `perfagent/options.go`**

After the `Labels` block, add:

```go
// DebuginfodURLs is the ordered list of debuginfod servers to consult for
// off-box DWARF/executable fetching. If empty (and DEBUGINFOD_URLS env is
// also empty), the agent uses the local symbolizer.
DebuginfodURLs []string

// SymbolCacheDir overrides the debuginfod cache directory. Default:
// /tmp/perf-agent-debuginfod.
SymbolCacheDir string

// SymbolCacheMaxBytes overrides the debuginfod cache size cap. Default: 2 GiB.
SymbolCacheMaxBytes int64

// SymbolFetchTimeout overrides per-artifact fetch timeout. Default: 30s.
SymbolFetchTimeout time.Duration

// SymbolFailClosed makes the agent refuse to symbolize a mapping whose
// debuginfod fetch failed (vs. fall back to local). Default: false.
// Note: M1 ships the option but FailClosed semantics are M2.
SymbolFailClosed bool
```

(Add `import "time"` if not present.)

Add functional options:

```go
// WithDebuginfodURL appends a debuginfod server URL. Repeatable.
func WithDebuginfodURL(url string) Option {
	return func(c *Config) {
		c.DebuginfodURLs = append(c.DebuginfodURLs, url)
	}
}

// WithSymbolCacheDir overrides the debuginfod cache directory.
func WithSymbolCacheDir(dir string) Option {
	return func(c *Config) { c.SymbolCacheDir = dir }
}

// WithSymbolCacheMaxBytes overrides the debuginfod cache cap.
func WithSymbolCacheMaxBytes(n int64) Option {
	return func(c *Config) { c.SymbolCacheMaxBytes = n }
}

// WithSymbolFetchTimeout overrides per-artifact fetch timeout.
func WithSymbolFetchTimeout(d time.Duration) Option {
	return func(c *Config) { c.SymbolFetchTimeout = d }
}

// WithSymbolFailClosed enables fail-closed behavior on debuginfod errors.
func WithSymbolFailClosed() Option {
	return func(c *Config) { c.SymbolFailClosed = true }
}
```

- [ ] **Step 2: Add `Symbolizer` field + `chooseSymbolizer` to `perfagent/agent.go`**

In the `Agent` struct (around line 71), add:

```go
type Agent struct {
	config *Config

	// Symbolizer is the agent-owned shared symbol resolver. Selected at
	// Start() time based on whether DebuginfodURLs is non-empty.
	symbolizer symbolize.Symbolizer

	// ... existing fields ...
}
```

(Add `import "github.com/dpsoft/perf-agent/symbolize"` and `"github.com/dpsoft/perf-agent/symbolize/debuginfod"`, and `"cmp"`, `"strings"`, `"os"` if not already imported. `slog` is already there.)

Add the factory function (top-level, anywhere in the file):

```go
func chooseSymbolizer(cfg *Config, res *procmap.Resolver, logger *slog.Logger) (symbolize.Symbolizer, error) {
	urls := cfg.DebuginfodURLs
	if len(urls) == 0 {
		for u := range strings.FieldsSeq(os.Getenv("DEBUGINFOD_URLS")) {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return symbolize.NewLocalSymbolizer()
	}
	cacheDir := cmp.Or(cfg.SymbolCacheDir, "/tmp/perf-agent-debuginfod")
	cacheMax := cmp.Or(cfg.SymbolCacheMaxBytes, int64(2<<30))
	timeout := cmp.Or(cfg.SymbolFetchTimeout, 30*time.Second)
	return debuginfod.New(debuginfod.Options{
		URLs:          urls,
		CacheDir:      cacheDir,
		CacheMaxBytes: cacheMax,
		FetchTimeout:  timeout,
		FailClosed:    cfg.SymbolFailClosed,
		Resolver:      res,
		Logger:        logger,
		Demangle:      true,
		InlinedFns:    true,
		CodeInfo:      true,
	})
}
```

(Add `import "github.com/dpsoft/perf-agent/unwind/procmap"` if not present.)

- [ ] **Step 3: Replace per-profiler `symbolize.NewLocalSymbolizer()` with the agent-owned one**

Find every place Phase 1-2 inserted a stopgap `symbolize.NewLocalSymbolizer()` call:

```bash
grep -rn "symbolize.NewLocalSymbolizer" /home/diego/github/perf-agent/perfagent/
```

Each call site already has access to the `Agent` (`a`). Replace the stopgap with `a.symbolizer`. The Agent's `Start` (or wherever profilers are constructed) must construct the symbolizer first:

```go
// In Agent.Start (or whatever method spins up profilers):
sym, err := chooseSymbolizer(a.config, /* resolver: */ procmap.NewResolver(), slog.Default())
if err != nil {
	return fmt.Errorf("symbolizer: %w", err)
}
a.symbolizer = sym

// Then construct profilers with a.symbolizer as the symbolize.Symbolizer arg.
```

The exact line numbers depend on the existing Start implementation; grep the file structure to find them:

```bash
grep -n "profile.NewProfiler\|offcpu.NewProfiler\|dwarfagent.New" /home/diego/github/perf-agent/perfagent/*.go
```

- [ ] **Step 4: Update `Agent.Close` to close the symbolizer last**

In whatever `Close`/`Stop` method `perfagent.Agent` exposes, add the symbolizer close after profilers but before any cleanup that might rely on it:

```go
func (a *Agent) Close() error {
	var errs []error
	// Close profilers first (they may have called SymbolizeProcess).
	if a.cpuProfiler != nil { a.cpuProfiler.Close() }
	if a.offcpuProfiler != nil { a.offcpuProfiler.Close() }
	// ... (matches existing structure) ...

	if a.symbolizer != nil {
		errs = append(errs, a.symbolizer.Close())
	}
	return errors.Join(errs...)
}
```

(Adjust to whatever the existing Close shape is — the spec says "errors.Join close calls", but the actual existing Agent code may use a different idiom.)

- [ ] **Step 5: Run all tests**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./perfagent/... ./profile/... ./offcpu/... ./unwind/... ./symbolize/...
```
Expected: PASS — behavior is identical when `DebuginfodURLs` is empty (LocalSymbolizer is used).

- [ ] **Step 6: Commit**

```bash
git add perfagent/options.go perfagent/agent.go
git commit -m "perfagent: own symbolize.Symbolizer; chooseSymbolizer picks Local vs Debuginfod"
```

---

## Phase 8 — CLI flags

### Task 21: Add `--debuginfod-url` and friends to `main.go`

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Define flag types and variables**

Open `/home/diego/github/perf-agent/main.go`. After the `tagFlags` definition, add a `urlFlags` type and the var declarations:

```go
// urlFlags collects multiple --debuginfod-url arguments.
type urlFlags []string

func (u *urlFlags) String() string { return strings.Join(*u, ",") }
func (u *urlFlags) Set(v string) error {
	if v == "" {
		return errors.New("debuginfod URL must not be empty")
	}
	*u = append(*u, v)
	return nil
}

var (
	flagDebuginfodURLs urlFlags
	flagSymbolCacheDir = flag.String("symbol-cache-dir", "",
		"Directory for debuginfod-fetched artifacts. Default: /tmp/perf-agent-debuginfod.")
	flagSymbolCacheMax = flag.Int64("symbol-cache-max", 0,
		"Maximum size of the debuginfod cache in bytes. Default: 2 GiB.")
	flagSymbolFetchTimeout = flag.Duration("symbol-fetch-timeout", 0,
		"Per-artifact debuginfod fetch timeout. Default: 30s.")
	flagSymbolFailClosed = flag.Bool("symbol-fail-closed", false,
		"Refuse to symbolize a mapping whose debuginfod fetch failed (no fallback to local).")
)
```

(Add `"errors"` to imports if not already there.)

- [ ] **Step 2: Register the repeatable flag in `init`**

Append to the existing `init()`:

```go
flag.Var(&flagDebuginfodURLs, "debuginfod-url",
	"Add a debuginfod server URL (repeatable). Falls back to DEBUGINFOD_URLS env var.")
```

- [ ] **Step 3: Wire flags into `buildOptions`**

Find `buildOptions()` (around line 116). After the Tags handling, add:

```go
for _, u := range flagDebuginfodURLs {
	opts = append(opts, perfagent.WithDebuginfodURL(u))
}
if *flagSymbolCacheDir != "" {
	opts = append(opts, perfagent.WithSymbolCacheDir(*flagSymbolCacheDir))
}
if *flagSymbolCacheMax > 0 {
	opts = append(opts, perfagent.WithSymbolCacheMaxBytes(*flagSymbolCacheMax))
}
if *flagSymbolFetchTimeout > 0 {
	opts = append(opts, perfagent.WithSymbolFetchTimeout(*flagSymbolFetchTimeout))
}
if *flagSymbolFailClosed {
	opts = append(opts, perfagent.WithSymbolFailClosed())
}
```

- [ ] **Step 4: Verify build**

```bash
make build
```
Expected: clean build.

```bash
./perf-agent -h 2>&1 | grep -i debuginfod
```
Expected: shows `--debuginfod-url`, `--symbol-cache-dir`, etc.

- [ ] **Step 5: Smoke test — feature off (default) preserves today's behavior**

```bash
sudo ./perf-agent --profile --pid $(pidof bash | awk '{print $1}') --duration 2s
```
Expected: profile.pb.gz produced, `LocalSymbolizer` path. (Skip if no bash available; the point is the binary still runs.)

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "main: add --debuginfod-url / --symbol-cache-* CLI flags"
```

---

## Phase 9 — Integration test (PoC docker-compose adapted)

### Task 22: Lift the PoC's docker-compose into `test/debuginfod/`

**Files:**
- Create: `test/debuginfod/docker-compose.yml`
- Create: `test/debuginfod/upload.sh`
- Create: `test/debuginfod/test.sh`
- Create: `test/debuginfod/sample/{hello.c,Makefile}`
- Create: `test/debuginfod/README.md`

The user's archive at `/tmp/blazedebuginfod/` already contains all of these. The task is to copy them in, adapt URL/path references, and document.

- [ ] **Step 1: Create the directory and copy artifacts**

```bash
mkdir -p /home/diego/github/perf-agent/test/debuginfod/sample
cp /tmp/blazedebuginfod/docker-compose.yml /home/diego/github/perf-agent/test/debuginfod/
cp /tmp/blazedebuginfod/upload.sh /home/diego/github/perf-agent/test/debuginfod/
cp /tmp/blazedebuginfod/test.sh /home/diego/github/perf-agent/test/debuginfod/
cp /tmp/blazedebuginfod/sample/hello.c /home/diego/github/perf-agent/test/debuginfod/sample/
cp /tmp/blazedebuginfod/sample/Makefile /home/diego/github/perf-agent/test/debuginfod/sample/
chmod +x /home/diego/github/perf-agent/test/debuginfod/upload.sh
chmod +x /home/diego/github/perf-agent/test/debuginfod/test.sh
```

- [ ] **Step 2: Strip the PoC's client/ service from docker-compose.yml**

We don't need the PoC's Go client; perf-agent's integration test will replace it. Open `test/debuginfod/docker-compose.yml` and remove the `client:` service definition and its `profiles: [client]` references. Keep only the `debuginfod:` service.

If the resulting compose has unused volumes, remove them too. Goal: a single-service compose that starts a debuginfod server on port 8002.

- [ ] **Step 3: Write the README**

`test/debuginfod/README.md`:

```markdown
# debuginfod integration test fixture

A minimal `debuginfod` server in Docker plus a sample C binary with separable
debug info, used by perf-agent's debuginfod integration tests.

## Layout

- `docker-compose.yml` — single-service `debuginfod` container on port 8002
- `sample/hello.c` + `sample/Makefile` — fixture binary built with
  `--build-id` and split debug info
- `upload.sh` — extracts `.debug` from the binary into the server's
  `.build-id` index
- `test.sh` — quick smoke test: GET /buildid/<X>/debuginfo and check sections

## Use

```bash
cd test/debuginfod
docker compose up -d debuginfod
# Build the sample binary (Linux ELF — use a Linux container if on macOS):
make -C sample
./upload.sh sample/hello
./test.sh sample/hello
```

The integration test in `test/integration_test.go` (`TestSymbolizeViaDebuginfod`)
boots the server, waits for readiness, and runs the agent against the sample
binary with `--debuginfod-url=http://localhost:8002`.

## Cleanup

```bash
docker compose down -v
rm -rf debuginfo-store/.build-id
```
```

- [ ] **Step 4: Smoke test the fixture builds (manual verification)**

If the developer is on Linux:

```bash
cd /home/diego/github/perf-agent/test/debuginfod
make -C sample
file sample/hello   # Confirm ELF
docker compose up -d debuginfod
./upload.sh sample/hello
./test.sh sample/hello
docker compose down -v
```
Expected: `test.sh` reports DWARF sections.

- [ ] **Step 5: Commit**

```bash
git add test/debuginfod/
git commit -m "test/debuginfod: lift PoC docker-compose fixture for integration tests"
```

---

### Task 23: Integration test — agent symbolizes via debuginfod

**Files:**
- Create: `test/debuginfod_integration_test.go`

This test lives in perf-agent's existing `test/` module. It boots the docker-compose, builds the sample binary, runs perf-agent against it pointed at the local debuginfod, and asserts function names appear in the produced pprof.

- [ ] **Step 1: Write the test**

`test/debuginfod_integration_test.go`:

```go
//go:build integration

package test

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	pprofpb "github.com/google/pprof/profile"
)

func TestSymbolizeViaDebuginfod(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if !hasDocker(t) {
		t.Skip("docker not available")
	}

	fixtureDir := absFixtureDir(t)
	startCompose(t, fixtureDir)
	t.Cleanup(func() { stopCompose(t, fixtureDir) })

	if err := runMake(fixtureDir+"/sample", "all"); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if err := runScript(fixtureDir+"/upload.sh", fixtureDir+"/sample/hello"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	waitForServer(t, "http://localhost:8002", 30*time.Second)

	// Spawn the fixture binary, profile it, assert symbols.
	cmd := exec.Command(fixtureDir + "/sample/hello")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hello: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	bin := agentBinary(t)
	agent := exec.Command(bin,
		"--profile",
		"--pid", itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", out,
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", t.TempDir(),
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read inflated: %v", err)
	}
	var p pprofpb.Profile
	if err := proto.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal pprof: %v", err)
	}

	// Expect to see at least one of: deep_function, middle_function, main
	wantAny := []string{"deep_function", "middle_function", "main"}
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	var matched bool
	for _, w := range wantAny {
		if got[w] {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("no expected fixture function in profile; have: %+v", got)
	}
}

// helpers (compose lifecycle, agent binary discovery, etc.)
func hasDocker(t *testing.T) bool { /* ... */ }
func absFixtureDir(t *testing.T) string { /* ... */ }
func startCompose(t *testing.T, dir string) { /* ... */ }
func stopCompose(t *testing.T, dir string) { /* ... */ }
func runMake(dir, target string) error { /* ... */ }
func runScript(path string, args ...string) error { /* ... */ }
func waitForServer(t *testing.T, base string, deadline time.Duration) { /* ... */ }
func agentBinary(t *testing.T) string { /* ... */ }
func itoa(n int) string { /* ... */ }
```

Implement the helpers using the patterns from the existing `test/integration_test.go` (which the test module already has). Keep `hasDocker` simple:

```go
func hasDocker(t *testing.T) bool {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		return false
	}
	return true
}

func absFixtureDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return filepath.Join(wd, "debuginfod")
}

func startCompose(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", dir+"/docker-compose.yml", "up", "-d", "debuginfod")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("compose up: %v", err)
	}
}

func stopCompose(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", dir+"/docker-compose.yml", "down", "-v")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func runMake(dir, target string) error {
	cmd := exec.Command("make", "-C", dir, target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runScript(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForServer(t *testing.T, base string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/metrics", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("debuginfod server at %s did not become ready in %v", base, deadline)
}

func agentBinary(t *testing.T) string {
	t.Helper()
	// Existing test infra builds perf-agent into a known path. Match it.
	return os.Getenv("PERF_AGENT_BIN")
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
```

(Adjust imports: `"context"`, `"io"`, `"net/http"`, `"strconv"`.)

- [ ] **Step 2: Add an `integration` build tag handler in `test/run_tests.sh`**

Inspect the existing script:

```bash
cat /home/diego/github/perf-agent/test/run_tests.sh
```

Confirm or add: a section that runs `go test -tags integration -run TestSymbolizeViaDebuginfod ./...`. The exact change depends on the script's structure; if uncertain, leave the test runnable directly:

```bash
cd test
go test -tags integration -run TestSymbolizeViaDebuginfod -v ./...
```

- [ ] **Step 3: Run the integration test**

```bash
cd /home/diego/github/perf-agent
make build  # produces ./perf-agent
PERF_AGENT_BIN="$PWD/perf-agent" \
  LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -tags integration -run TestSymbolizeViaDebuginfod -v ./test/...
```
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add test/debuginfod_integration_test.go test/run_tests.sh
git commit -m "test: integration test exercising debuginfod symbolize end-to-end"
```

---

## Final verification

### Task 24: Run the entire test matrix and the docker fixture

- [ ] **Step 1: Default-build unit tests**

```bash
LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./...
```
Expected: PASS.

- [ ] **Step 2: Lint**

```bash
golangci-lint run --timeout=5m ./symbolize/...
```
Expected: clean.

- [ ] **Step 3: Integration test**

```bash
cd /home/diego/github/perf-agent
make build
PERF_AGENT_BIN="$PWD/perf-agent" \
  LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -tags integration -run TestSymbolizeViaDebuginfod -v ./test/...
```
Expected: PASS.

- [ ] **Step 4: Confirm zero behavior change for non-debuginfod users**

```bash
# Existing integration tests, no debuginfod URL configured — agent uses LocalSymbolizer.
sudo make test-integration
```
Expected: PASS (same as before this work).

- [ ] **Step 5: Commit any final touch-ups (lint, test-runner integration)**

If the lint/test runner needed adjustments:

```bash
git add -p   # review carefully
git commit -m "test: housekeeping for debuginfod symbolize M1"
```

---

## End of M1

After this plan completes:

- `symbolize.Symbolizer` is the abstraction every profiler depends on.
- `LocalSymbolizer` preserves today's behavior bit-for-bit.
- `DebuginfodSymbolizer` is opt-in via `--debuginfod-url`, hybrid-routes per mapping, caches under `.build-id/...` with a SQLite index (`modernc.org/sqlite`).
- All three call sites use the interface; the Agent owns the symbolizer.
- An integration test boots the PoC's docker-compose and asserts end-to-end symbolization.

**Deferred to M2** (separate plan): `FailClosed` per-call build-id override, `metrics.Exporter` wiring, BUILDING.md update, multi-instance race tests, end-to-end stripped-binary test, exponential backoff on 5xx.
