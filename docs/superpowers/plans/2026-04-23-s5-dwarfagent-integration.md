# S5: dwarfagent + `--unwind dwarf` Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** make `perf-agent --pid N --unwind dwarf` end-to-end — loads the perf_dwarf BPF program, auto-tracks the target's mappings via S4's TableStore/PIDTracker/MmapWatcher, consumes the stack_events ringbuf, symbolizes via blazesym, and writes a pprof.pb.gz just like `--unwind fp` does today.

**Architecture:** a new `unwind/dwarfagent/` package wraps the S3 BPF program + S4 ehmaps lifecycle behind the same `Collect(io.Writer)` / `CollectAndWrite(path)` interface the existing `profile.Profiler` exposes. `perfagent.Config` gains an `Unwind` field ("fp" | "dwarf" | "auto"); the agent constructor dispatches. Per the design doc, `--unwind auto` still routes to the FP path during S5 — the default flip is S8.

**Tech Stack:** Go 1.26, cilium/ebpf v0.21.0 (ringbuf reader), blazesym (existing CGO binding), existing `unwind/ehmaps` + `unwind/ehcompile`, existing `pprof` package.

---

## Scope

**S5 delivers:** end users can run `perf-agent --profile --unwind dwarf --pid N` and get a pprof.pb.gz with DWARF-unwound, symbolized Rust/C++/libstd stacks. Existing `--unwind fp` (and the default) stays byte-for-byte identical.

**Explicitly NOT in S5:**
- Off-CPU DWARF variant — S6.
- System-wide `-a --unwind dwarf` — S7 (dwarfagent is `--pid N` only in this plan).
- Default flip of `--unwind auto` — S8.
- Per-CPU MmapWatcher upgrade (S4's per-TID limitation persists; dlopen-from-worker-thread still missed — acceptable for most real workloads).

## Background for implementers

**What's already in the repo:**
- `profile.LoadPerfDwarfForTest()` loads the `perf_dwarf` BPF program and returns a `PerfDwarfForTest` handle with accessors for every map we need. Despite the "ForTest" suffix it's production-shaped; S5 promotes it to `LoadPerfDwarf` / `PerfDwarf`.
- `ehmaps.TableStore`, `ehmaps.PIDTracker`, `ehmaps.MmapWatcher` — the S4 building blocks.
- `ehmaps.ReadBuildID` and `ehmaps.LoadProcessMappings` — ELF + /proc/maps helpers.
- `pprof.NewProfileBuilders` + `pprof.Frame` — existing pprof output plumbing.
- `profile.Profiler.Collect(w io.Writer)` at `profile/profiler.go:159` — the reference for ringbuf-consumer-to-pprof flow (though Profiler uses a counts map; dwarfagent uses ringbuf).
- blazesym is wrapped via `github.com/libbpf/blazesym/go` — Profiler's `blazeSymToFrames` helper at `profile/profiler.go:53` handles inline expansion.

**Sample record layout** (bpf/unwind_common.h): fixed 1032 bytes per record = 32-byte header + 127 × u64 PCs. Header fields (little-endian):
- `[0:4]` pid (u32), `[4:8]` tid (u32), `[8:16]` time_ns (u64), `[16:24]` value (u64), `[24]` mode (u8), `[25]` n_pcs (u8), `[26]` walker_flags (u8), `[27:32]` padding.

**Initial attach strategy:** on startup, `dwarfagent` walks `/proc/<pid>/maps`, finds each unique file-backed executable path, calls `tracker.Attach(pid, path)` per unique binary. That covers the main binary + every dependent `.so` present at agent start. Subsequent dlopens are picked up by `MmapWatcher.Run`.

**Why `ehmaps.Attach` once per binary is enough:** `LoadProcessMappings` inside `Attach` filters by basename match against the passed path — each call finds only that binary's mappings. Cumulative calls build up the full pid_mappings array. This is exactly what the S3 integration test `TestPerfDwarfWalker` does for one binary; S5 extends to all binaries in the address space.

**perf_event attachment:** S3's pattern — `pid=-1, cpu=N` per CPU, BPF-side `pids` map filters to the target TGID (via `objs.AddPID`). Keep this shape.

## File Structure

```
profile/dwarf_export.go                    MODIFY — rename LoadPerfDwarfForTest → LoadPerfDwarf, rename PerfDwarfForTest → PerfDwarf (keep old names as deprecated aliases for one commit to simplify migration, then remove).

unwind/ehmaps/tracker.go                   MODIFY — add AttachAllMappings(tracker, pid) helper that scans /proc/<pid>/maps and Attaches every unique file-backed executable path.
unwind/ehmaps/tracker_test.go              MODIFY — add TestAttachAllMappings against self (self's address space has multiple shared libs; verify >1 table_id ends up in cfi_lengths).

unwind/dwarfagent/agent.go                 CREATE — Profiler type: loads BPF, wires ehmaps lifecycle, opens per-CPU perf events, consumes ringbuf, holds accumulated samples.
unwind/dwarfagent/symbolize.go             CREATE — batch symbolization via blazesym (lifted from profile/profiler.go's blazeSymToFrames + the per-PID symbolize call).
unwind/dwarfagent/sample.go                CREATE — sample_record byte-layout parser; aggregator that groups identical stacks.
unwind/dwarfagent/agent_test.go            CREATE — CAP_BPF-gated test: start rust workload → NewProfiler → sleep → Collect → verify pprof.pb.gz has at least some samples + at least one frame named "cpu_intensive_work".

perfagent/options.go                       MODIFY — add `Unwind string` field (values: "fp", "dwarf", "auto", empty="") + WithUnwind option.
perfagent/agent.go                         MODIFY — CPU profiler construction dispatches on Config.Unwind: "dwarf" → dwarfagent.NewProfiler; else → profile.NewProfiler. Introduces a small local `cpuProfiler` interface with Collect/CollectAndWrite/Close methods so a.cpuProfiler can be either implementation.
perfagent/options_test.go                  MODIFY (if tests exist) — or add unit tests for WithUnwind.

main.go                                    MODIFY — add --unwind flag, plumb through WithUnwind.

test/integration_test.go                   MODIFY — add TestPerfAgentDwarfUnwind: runs the full perf-agent binary with --unwind dwarf --pid N against the rust workload, parses the pprof.pb.gz, asserts at least one function named "cpu_intensive_work".

docs/dwarf-unwinding-design.md             MODIFY — update S5 row to ✅ with plan reference.
```

---

## Task 1 — Promote `PerfDwarfForTest` to `PerfDwarf`

**Goal:** rename without semantic change. This is a pure refactor that keeps `cmd/perf-dwarf-test` and the S3 integration test working.

**Files:**
- Modify: `profile/dwarf_export.go`
- Modify: `cmd/perf-dwarf-test/main.go` (call-site rename)
- Modify: `test/integration_test.go` (call-site rename)

- [ ] **Step 1.1: Rename the type and constructor in `profile/dwarf_export.go`**

At the top of the file, replace the `PerfDwarfForTest` struct and `LoadPerfDwarfForTest` function with:

```go
// PerfDwarf is a thin wrapper around the generated perf_dwarf BPF objects.
// Construct with LoadPerfDwarf; always Close() when done.
type PerfDwarf struct {
	objs perf_dwarfObjects
}

// LoadPerfDwarf loads the BPF program and returns a handle. Caller must
// Close(). The program isn't attached to any perf event yet — the caller
// opens perf_event_open fds and attaches separately (see
// unwind/dwarfagent for the full wiring).
func LoadPerfDwarf() (*PerfDwarf, error) {
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true,
		cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE); err != nil {
		return nil, fmt.Errorf("set capabilities: %w", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	spec, err := loadPerf_dwarf()
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf spec: %w", err)
	}
	if err := spec.Variables["system_wide"].Set(false); err != nil {
		return nil, fmt.Errorf("set system_wide: %w", err)
	}
	p := &PerfDwarf{}
	if err := spec.LoadAndAssign(&p.objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign: %w", err)
	}
	return p, nil
}
```

Replace `(p *PerfDwarfForTest)` with `(p *PerfDwarf)` on every method in the file (Program, RingbufMap, SetSystemWide, AddPID, Close, CFIRulesMap, CFILengthsMap, CFIClassificationMap, CFIClassificationLengthsMap, PIDMappingsMap, PIDMappingLengthsMap).

- [ ] **Step 1.2: Update call sites**

In `cmd/perf-dwarf-test/main.go`:

```go
// before: profile.LoadPerfDwarfForTest()  → profile.LoadPerfDwarf()
// before: *profile.PerfDwarfForTest        → *profile.PerfDwarf
// before: type loadedObjects = profile.PerfDwarfForTest → profile.PerfDwarf
```

In `test/integration_test.go`:

```go
// before: perfprofile.LoadPerfDwarfForTest() → perfprofile.LoadPerfDwarf()
```

- [ ] **Step 1.3: Verify compile and tests**

```
GOTOOLCHAIN=go1.26.0 go vet ./...
GOTOOLCHAIN=go1.26.0 make test-unit
```

Expected: both succeed. `TestPerfDwarfLoads` should still compile (it's in `profile/perf_dwarf_test.go` — update its call too if it references the old name).

Check for any other references:

```
grep -rn "PerfDwarfForTest\|LoadPerfDwarfForTest" .
```

Expected: no matches after the rename.

- [ ] **Step 1.4: Commit**

```
git add profile/dwarf_export.go profile/perf_dwarf_test.go cmd/perf-dwarf-test/main.go test/integration_test.go
git commit -m "S5: rename PerfDwarfForTest → PerfDwarf (no behavior change)"
```

---

## Task 2 — `ehmaps.AttachAllMappings`: initial /proc/maps walk

**Goal:** one call that attaches a PID's main binary + every dependent shared library visible at call time. Subsequent dlopens are handled by `MmapWatcher.Run` (unchanged from S4).

**Files:**
- Modify: `unwind/ehmaps/tracker.go`
- Modify: `unwind/ehmaps/tracker_test.go`

- [ ] **Step 2.1: Add failing test**

Append to `unwind/ehmaps/tracker_test.go`:

```go
// TestAttachAllMappings attaches to the test process itself (which is
// multi-binary — the Go test harness + blazesym.so + libc + ld.so +
// libpthread + etc.) and verifies that more than one cfi_lengths entry
// gets installed (i.e. AttachAllMappings found several unique binaries
// in /proc/self/maps and Attach'd each).
func TestAttachAllMappings(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}
	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := ehmaps.NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := ehmaps.NewPIDTracker(store, pidMaps, pidMapLen)

	n, err := ehmaps.AttachAllMappings(tracker, uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("AttachAllMappings: %v", err)
	}
	if n < 2 {
		t.Fatalf("AttachAllMappings installed %d tables, want >= 2 (main + at least one .so)", n)
	}

	installed := 0
	it := cfiLen.Iterate()
	var tid uint64
	var cnt uint32
	for it.Next(&tid, &cnt) {
		installed++
	}
	if installed != n {
		t.Fatalf("cfi_lengths has %d entries, AttachAllMappings claimed %d", installed, n)
	}
}
```

- [ ] **Step 2.2: Run, verify fail**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: ehmaps.AttachAllMappings`.

- [ ] **Step 2.3: Implement AttachAllMappings**

Append to `unwind/ehmaps/tracker.go`:

```go
// AttachAllMappings walks /proc/<pid>/maps, finds every file-backed
// executable mapping, and calls tracker.Attach once per unique binary
// path. Returns the count of distinct binaries attached.
//
// Call this once at agent startup to cover the main binary plus every
// shared library present at that moment. Subsequent mmaps (dlopen,
// runtime-loaded plugins) are handled by MmapWatcher driving
// PIDTracker.Run.
//
// Attach failures for individual binaries are logged and skipped
// rather than fatal — a process may have exotic mappings
// (ehcompile-rejectable formats, ELFs without .eh_frame) that we
// shouldn't fail the whole attach on. The first binary's failure
// is returned so callers can notice setup problems; subsequent ones
// are logged at Debug.
func AttachAllMappings(t *PIDTracker, pid uint32) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/maps: %w", pid, err)
	}
	seen := map[string]struct{}{}
	var firstErr error
	n := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		path := fields[5]
		if path == "" || strings.HasPrefix(path, "[") || strings.HasPrefix(path, "//anon") {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := t.Attach(pid, path); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("attach %s: %w", path, err)
			} else {
				slog.Debug("ehmaps: AttachAllMappings: skip", "path", path, "err", err)
			}
			continue
		}
		n++
	}
	if n == 0 && firstErr != nil {
		return 0, firstErr
	}
	return n, nil
}
```

The imports you need (`os`, `strings`, `fmt`, `slog`) should already be in tracker.go from S4 Task 5. If `fmt` is missing, add it.

- [ ] **Step 2.4: Run test, verify pass unprivileged**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: compile passes; `TestAttachAllMappings` SKIPs without CAP_BPF.

- [ ] **Step 2.5: DO NOT run capped binary yourself**

Controller handles setcap + runtime verification.

- [ ] **Step 2.6: Commit**

```
git add unwind/ehmaps/tracker.go unwind/ehmaps/tracker_test.go
git commit -m "S5: ehmaps.AttachAllMappings for initial /proc/maps walk"
```

---

## Task 3 — dwarfagent package skeleton + sample parser

**Goal:** package boundary + the byte-level sample_record parser. No BPF yet; just a pure-Go type that can take 1032 raw bytes and return `(pid, nPCs, walkerFlags, pcs[])`.

**Files:**
- Create: `unwind/dwarfagent/sample.go`
- Create: `unwind/dwarfagent/sample_test.go`

- [ ] **Step 3.1: Write failing test**

Create `unwind/dwarfagent/sample_test.go`:

```go
package dwarfagent

import (
	"encoding/binary"
	"testing"
)

func TestParseSampleHeader(t *testing.T) {
	const sampleSize = 32 + 127*8
	buf := make([]byte, sampleSize)
	binary.LittleEndian.PutUint32(buf[0:4], 0x1234)       // pid
	binary.LittleEndian.PutUint32(buf[4:8], 0x5678)       // tid
	binary.LittleEndian.PutUint64(buf[8:16], 0x9abc_def0) // time_ns
	binary.LittleEndian.PutUint64(buf[16:24], 1)          // value
	buf[24] = 1                                           // mode = MODE_FP_LESS
	buf[25] = 3                                           // n_pcs = 3
	buf[26] = 0x2                                         // walker_flags = DWARF_USED
	binary.LittleEndian.PutUint64(buf[32:40], 0xaaaa)
	binary.LittleEndian.PutUint64(buf[40:48], 0xbbbb)
	binary.LittleEndian.PutUint64(buf[48:56], 0xcccc)

	s, err := parseSample(buf)
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if s.PID != 0x1234 {
		t.Errorf("PID = %#x, want 0x1234", s.PID)
	}
	if s.Mode != 1 {
		t.Errorf("Mode = %d, want 1", s.Mode)
	}
	if len(s.PCs) != 3 {
		t.Fatalf("len(PCs) = %d, want 3", len(s.PCs))
	}
	if s.PCs[0] != 0xaaaa || s.PCs[1] != 0xbbbb || s.PCs[2] != 0xcccc {
		t.Errorf("PCs = %v, want [0xaaaa 0xbbbb 0xcccc]", s.PCs)
	}
	if s.WalkerFlags != 0x2 {
		t.Errorf("WalkerFlags = %#x, want 0x2", s.WalkerFlags)
	}
}

func TestParseSampleTruncatedHeader(t *testing.T) {
	buf := make([]byte, 16) // smaller than 32-byte header
	if _, err := parseSample(buf); err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestParseSampleNPCsClamped(t *testing.T) {
	const sampleSize = 32 + 127*8
	buf := make([]byte, sampleSize)
	binary.LittleEndian.PutUint32(buf[0:4], 42)
	buf[25] = 200 // n_pcs > 127, should clamp
	s, err := parseSample(buf)
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if len(s.PCs) != 127 {
		t.Errorf("clamped len(PCs) = %d, want 127", len(s.PCs))
	}
}
```

- [ ] **Step 3.2: Run, verify fail**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/dwarfagent/`

Expected: `no Go files in .../unwind/dwarfagent` — package doesn't exist yet.

- [ ] **Step 3.3: Implement the parser**

Create `unwind/dwarfagent/sample.go`:

```go
// Package dwarfagent wires the S3 perf_dwarf BPF program, the S4
// ehmaps lifecycle (TableStore / PIDTracker / MmapWatcher), and pprof
// output into a single Profiler with the same Collect/CollectAndWrite
// shape as profile.Profiler. The user-visible entry point is
// `perf-agent --profile --unwind dwarf --pid N`, which in
// perfagent.Start() dispatches to dwarfagent.NewProfiler instead of
// profile.NewProfiler.
package dwarfagent

import (
	"encoding/binary"
	"fmt"
)

// MaxFrames matches bpf/unwind_common.h's MAX_FRAMES (127).
const MaxFrames = 127

// SampleHeaderBytes matches the struct sample_header in
// bpf/unwind_common.h (32 bytes including padding).
const SampleHeaderBytes = 32

// SampleRecordBytes is the full record size: header + MaxFrames × u64.
const SampleRecordBytes = SampleHeaderBytes + MaxFrames*8

// Sample is the userspace parse of one ringbuf stack_events record.
type Sample struct {
	PID         uint32
	TID         uint32
	TimeNs      uint64
	Value       uint64
	Mode        uint8
	WalkerFlags uint8
	PCs         []uint64
}

// parseSample decodes one stack_events record. nPCs is clamped to
// MaxFrames. Returns an error if buf is smaller than the 32-byte
// header or can't contain the PCs the header claims.
func parseSample(buf []byte) (Sample, error) {
	if len(buf) < SampleHeaderBytes {
		return Sample{}, fmt.Errorf("sample truncated: %d bytes, need >= %d", len(buf), SampleHeaderBytes)
	}
	s := Sample{
		PID:         binary.LittleEndian.Uint32(buf[0:4]),
		TID:         binary.LittleEndian.Uint32(buf[4:8]),
		TimeNs:      binary.LittleEndian.Uint64(buf[8:16]),
		Value:       binary.LittleEndian.Uint64(buf[16:24]),
		Mode:        buf[24],
		WalkerFlags: buf[26],
	}
	nPCs := int(buf[25])
	if nPCs > MaxFrames {
		nPCs = MaxFrames
	}
	pcEnd := SampleHeaderBytes + nPCs*8
	if pcEnd > len(buf) {
		// Ringbuf records are fixed-size 1032 bytes; a truncated
		// buffer is a kernel-side bug. Clamp to what we have.
		nPCs = (len(buf) - SampleHeaderBytes) / 8
		pcEnd = SampleHeaderBytes + nPCs*8
	}
	s.PCs = make([]uint64, nPCs)
	for i := range nPCs {
		off := SampleHeaderBytes + i*8
		s.PCs[i] = binary.LittleEndian.Uint64(buf[off : off+8])
	}
	return s, nil
}
```

- [ ] **Step 3.4: Run test, verify pass**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/dwarfagent/`

Expected: 3 tests PASS.

- [ ] **Step 3.5: Commit**

```
git add unwind/dwarfagent/sample.go unwind/dwarfagent/sample_test.go
git commit -m "S5: dwarfagent package — sample_record parser"
```

---

## Task 4 — Symbolization helper

**Goal:** a helper that takes `(pid, pcs)` and returns `[]pprof.Frame`, using blazesym with inline expansion. This is lifted from `profile/profiler.go:53,217-231` but lives in a standalone file so dwarfagent doesn't depend on the profile package.

**Files:**
- Create: `unwind/dwarfagent/symbolize.go`

- [ ] **Step 4.1: Implement**

Create `unwind/dwarfagent/symbolize.go`:

```go
package dwarfagent

import (
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
)

// blazeSymToFrames converts a blazesym.Sym (one address's resolution,
// including any inlined frames) into one or more pprof.Frames in
// caller-to-innermost order. Lifted verbatim from profile.Profiler's
// version to keep the two walkers byte-compatible at the frame layer.
//
// blazesym reports Inlined in outer→inner order; we walk it in
// reverse so the pprof stack has the innermost frame first.
func blazeSymToFrames(s blazesym.Sym) []pprof.Frame {
	frames := make([]pprof.Frame, 0, 1+len(s.Inlined))
	// Outermost: the direct (non-inlined) frame for this PC.
	frames = append(frames, pprof.Frame{
		Name:   s.Name,
		File:   s.CodeInfo.File,
		Line:   uint32(s.CodeInfo.Line),
		Module: s.Module,
	})
	// Inlined entries come back outer→inner; reverse so our output is
	// outermost-last (matching the innermost-first pprof convention).
	for i := len(s.Inlined) - 1; i >= 0; i-- {
		f := s.Inlined[i]
		frames = append(frames, pprof.Frame{
			Name:   f.Name,
			File:   f.CodeInfo.File,
			Line:   uint32(f.CodeInfo.Line),
			Module: s.Module,
		})
	}
	return frames
}

// symbolizePID resolves a slice of absolute user-space addresses for
// one PID and returns the corresponding pprof frames in the same order
// as ips. Missing IPs (unresolved by blazesym) contribute a single
// synthetic "[unknown]" frame each so chain depths stay meaningful.
func symbolizePID(sym *blazesym.Symbolizer, pid uint32, ips []uint64) []pprof.Frame {
	if len(ips) == 0 {
		return nil
	}
	syms, err := sym.SymbolizeProcessAbsAddrs(
		ips,
		pid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	if err != nil || len(syms) == 0 {
		// Blanket fallback: emit one [unknown] per IP.
		out := make([]pprof.Frame, len(ips))
		for i := range out {
			out[i] = pprof.FrameFromName("[unknown]")
		}
		return out
	}
	var out []pprof.Frame
	for _, s := range syms {
		out = append(out, blazeSymToFrames(s)...)
	}
	return out
}
```

- [ ] **Step 4.2: Compile check**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./unwind/dwarfagent/
```

Expected: success (CGO flags are needed because blazesym is a CGO dep).

- [ ] **Step 4.3: Commit**

```
git add unwind/dwarfagent/symbolize.go
git commit -m "S5: dwarfagent symbolization helper (blazesym + inline expansion)"
```

---

## Task 5 — dwarfagent Profiler type

**Goal:** the main event. `NewProfiler` → loads BPF, wires ehmaps, opens perf events, starts the ringbuf consumer. `Collect(w)` → symbolize accumulated samples, write pprof. `CollectAndWrite(path)` / `Close()`.

**Files:**
- Create: `unwind/dwarfagent/agent.go`

- [ ] **Step 5.1: Implement Profiler**

Create `unwind/dwarfagent/agent.go`:

```go
package dwarfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// Profiler is the DWARF-capable CPU profiler. It has the same
// public shape as profile.Profiler — Collect, CollectAndWrite, Close —
// so perfagent.Agent can swap between the two on --unwind.
type Profiler struct {
	pid        int
	sampleRate int
	tags       []string

	objs       *profile.PerfDwarf
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    *ehmaps.MmapWatcher
	perfFDs    []int
	perfLinks  []link.Link
	ringReader *ringbuf.Reader

	symbolizer *blazesym.Symbolizer

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64
	stacks  map[sampleKey][]uint64 // stashed PC chain per key — lazy-init in stash()
}

// sampleKey is "(pid, stack hash)" — we dedupe identical stacks
// userspace-side to avoid re-symbolizing the same N-PC chain N times.
// The hash collides at the theoretical FNV rate (not cryptographic);
// collisions conflate counts but don't miss samples.
type sampleKey struct {
	pid  uint32
	hash uint64
}

// NewProfiler loads the BPF program, walks /proc/<pid>/maps to prime
// the ehmaps lifecycle, opens per-CPU perf events at sampleRate Hz,
// and starts the ringbuf reader goroutine.
func NewProfiler(pid int, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 (system-wide is S7 scope)")
	}
	objs, err := profile.LoadPerfDwarf()
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf: %w", err)
	}
	if err := objs.AddPID(uint32(pid)); err != nil {
		objs.Close()
		return nil, fmt.Errorf("add pid to filter: %w", err)
	}

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())

	nAttached, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("attach initial mappings: %w", err)
	}
	log.Printf("dwarfagent: attached %d binaries from /proc/%d/maps", nAttached, pid)

	watcher, err := ehmaps.NewMmapWatcher(uint32(pid))
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("mmap watcher: %w", err)
	}

	// Per-CPU perf events. pid=-1 + BPF-side pids filter = same pattern
	// as profile.Profiler in --unwind fp.
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: uint64(sampleRate),
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	perfFDs := make([]int, 0, len(cpus))
	perfLinks := make([]link.Link, 0, len(cpus))
	cleanupPerf := func() {
		for _, l := range perfLinks {
			_ = l.Close()
		}
		for _, fd := range perfFDs {
			_ = unix.Close(fd)
		}
	}
	for _, cpu := range cpus {
		fd, err := unix.PerfEventOpen(attr, -1, int(cpu), -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			cleanupPerf()
			watcher.Close()
			objs.Close()
			return nil, fmt.Errorf("perf_event_open cpu=%d: %w", cpu, err)
		}
		perfFDs = append(perfFDs, fd)
		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target:  fd,
			Program: objs.Program(),
			Attach:  ebpf.AttachPerfEvent,
		})
		if err != nil {
			cleanupPerf()
			watcher.Close()
			objs.Close()
			return nil, fmt.Errorf("attach perf event cpu=%d: %w", cpu, err)
		}
		perfLinks = append(perfLinks, rl)
		if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
			cleanupPerf()
			watcher.Close()
			objs.Close()
			return nil, fmt.Errorf("enable perf event cpu=%d: %w", cpu, err)
		}
	}
	if len(perfFDs) == 0 {
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("no perf events attached — pid %d may have exited", pid)
	}

	rd, err := ringbuf.NewReader(objs.RingbufMap())
	if err != nil {
		cleanupPerf()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("ringbuf reader: %w", err)
	}

	symbolizer, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		rd.Close()
		cleanupPerf()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	p := &Profiler{
		pid:        pid,
		sampleRate: sampleRate,
		tags:       tags,
		objs:       objs,
		store:      store,
		tracker:    tracker,
		watcher:    watcher,
		perfFDs:    perfFDs,
		perfLinks:  perfLinks,
		ringReader: rd,
		symbolizer: symbolizer,
		stop:       make(chan struct{}),
		samples:    map[sampleKey]uint64{},
	}

	// PIDTracker.Run consumes mmap events.
	p.trackerWG.Add(1)
	go func() {
		defer p.trackerWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-p.stop
			cancel()
		}()
		p.tracker.Run(ctx, p.watcher)
	}()

	// Ringbuf consumer.
	p.readerWG.Add(1)
	go p.consume()

	return p, nil
}

// consume is the ringbuf reader goroutine. Stops when p.stop fires
// (Close closes it) or when the reader returns ErrClosed.
func (p *Profiler) consume() {
	defer p.readerWG.Done()
	for {
		select {
		case <-p.stop:
			return
		default:
		}
		p.ringReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		rec, err := p.ringReader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			log.Printf("dwarfagent: ringbuf read: %v", err)
			return
		}
		s, err := parseSample(rec.RawSample)
		if err != nil {
			log.Printf("dwarfagent: parseSample: %v", err)
			continue
		}
		if len(s.PCs) == 0 {
			continue
		}
		key := sampleKey{pid: s.PID, hash: hashPCs(s.PCs)}
		p.mu.Lock()
		p.samples[key]++
		// Stash a copy of the first stack for this key so Collect
		// can symbolize without re-reading the ring. We cache into
		// a parallel map keyed the same way via stash().
		p.stash(key, s.PCs)
		p.mu.Unlock()
	}
}

// stash stores the PC chain for a given key if not already stashed.
// Called under p.mu.
func (p *Profiler) stash(key sampleKey, pcs []uint64) {
	if p.stacks == nil {
		p.stacks = map[sampleKey][]uint64{}
	}
	if _, have := p.stacks[key]; !have {
		p.stacks[key] = append([]uint64(nil), pcs...)
	}
}

// hashPCs: FNV-1a over the PC chain. Stable, fast, collision-rare.
func hashPCs(pcs []uint64) uint64 {
	const (
		offset uint64 = 0xcbf29ce484222325
		prime  uint64 = 0x100000001b3
	)
	h := offset
	for _, pc := range pcs {
		// Byte-wise FNV over the u64.
		for shift := uint(0); shift < 64; shift += 8 {
			h ^= (pc >> shift) & 0xff
			h *= prime
		}
	}
	return h
}

// Collect drains accumulated samples, symbolizes them, and writes a
// gzipped pprof to w. Does NOT clear accumulated state — follow with
// Close to release BPF resources.
func (p *Profiler) Collect(w io.Writer) error {
	p.mu.Lock()
	samples := make(map[sampleKey]uint64, len(p.samples))
	stacks := make(map[sampleKey][]uint64, len(p.stacks))
	for k, v := range p.samples {
		samples[k] = v
	}
	for k, v := range p.stacks {
		stacks[k] = v
	}
	p.mu.Unlock()

	if len(samples) == 0 {
		log.Println("dwarfagent: no samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    int64(p.sampleRate),
		PerPIDProfile: false,
		Comments:      p.tags,
	})

	for key, count := range samples {
		pcs := stacks[key]
		frames := symbolizePID(p.symbolizer, key.pid, pcs)
		sample := pprof.ProfileSample{
			Pid:         key.pid,
			SampleType:  pprof.SampleTypeCpu,
			Aggregation: pprof.SampleAggregated,
			Stack:       frames,
			Value:       count,
		}
		builders.AddSample(&sample)
	}

	for _, b := range builders.Builders {
		if _, err := b.Write(w); err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break // single non-per-PID profile
	}
	return nil
}

// CollectAndWrite is a convenience wrapper for file output.
func (p *Profiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close stops all goroutines and releases BPF / perf / symbolizer
// resources. Idempotent at the channel level (closing a closed stop
// panics, so we guard).
func (p *Profiler) Close() error {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.readerWG.Wait()
	p.ringReader.Close()
	p.watcher.Close()
	p.trackerWG.Wait()
	for _, l := range p.perfLinks {
		_ = l.Close()
	}
	for _, fd := range p.perfFDs {
		_ = unix.Close(fd)
	}
	if p.symbolizer != nil {
		p.symbolizer.Close()
	}
	return p.objs.Close()
}
```

(The `stacks` field is declared on the Profiler struct above — no separate file is needed.)

- [ ] **Step 5.2: Verify compile**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./unwind/dwarfagent/
```

Expected: success.

- [ ] **Step 5.3: Commit**

```
git add unwind/dwarfagent/agent.go
git commit -m "S5: dwarfagent.Profiler — BPF load + ehmaps lifecycle + ringbuf consumer"
```

---

## Task 6 — dwarfagent end-to-end test

**Goal:** CAP-gated test: start rust workload → NewProfiler → sleep → Collect → parse pprof → assert at least one function named `cpu_intensive_work`.

**Files:**
- Create: `unwind/dwarfagent/agent_test.go`

- [ ] **Step 6.1: Write the test**

Create `unwind/dwarfagent/agent_test.go`:

```go
package dwarfagent_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

// TestProfilerEndToEnd runs the full dwarfagent stack against the
// rust-workload and asserts that the resulting pprof contains at
// least one sample naming cpu_intensive_work.
func TestProfilerEndToEnd(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	binPath := "../../test/workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "10", "2")
	if err := workload.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(1 * time.Second)

	// Online CPUs — use runtime.NumCPU as a simple stand-in.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_ = ctx // not plumbed through NewProfiler yet; cancellation is via Close.

	cpus := make([]uint, 0)
	for i := 0; i < numOnlineCPUs(); i++ {
		cpus = append(cpus, uint(i))
	}

	p, err := dwarfagent.NewProfiler(workload.Process.Pid, cpus, nil, 99)
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	// Sample for 3s.
	time.Sleep(3 * time.Second)

	var buf bytes.Buffer
	if err := p.Collect(&buf); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Logf("Close (non-fatal): %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("Collect produced 0 bytes")
	}
	prof, err := profile.Parse(&buf)
	if err != nil {
		t.Fatalf("parse pprof: %v", err)
	}
	if len(prof.Sample) == 0 {
		t.Fatal("pprof has no samples")
	}
	hit := false
	for _, fn := range prof.Function {
		if strings.Contains(fn.Name, "cpu_intensive_work") {
			hit = true
			break
		}
	}
	if !hit {
		names := make([]string, 0, min(10, len(prof.Function)))
		for i, fn := range prof.Function {
			if i >= 10 {
				break
			}
			names = append(names, fn.Name)
		}
		t.Fatalf("no function named *cpu_intensive_work* in pprof; first few: %v", names)
	}
}

func numOnlineCPUs() int {
	// Reads /sys/devices/system/cpu/online or falls back to NumCPU.
	// For a CAP-gated test, keeping this simple is fine.
	data, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		// runtime.NumCPU() in stdlib
		return 1
	}
	// "0-15" or "0-3,6-7" — for tests, just count distinct CPUs.
	count := 0
	for _, part := range strings.Split(strings.TrimSpace(string(data)), ",") {
		if hy := strings.Index(part, "-"); hy >= 0 {
			a := part[:hy]
			b := part[hy+1:]
			var ai, bi int
			for _, c := range a {
				ai = ai*10 + int(c-'0')
			}
			for _, c := range b {
				bi = bi*10 + int(c-'0')
			}
			count += bi - ai + 1
		} else {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}
```

- [ ] **Step 6.2: Verify compile**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test -c -o /home/diego/bin/dwarfagent.test ./unwind/dwarfagent/
```

Expected: test binary built. DO NOT run it yourself — controller will setcap + run.

- [ ] **Step 6.3: Commit**

```
git add unwind/dwarfagent/agent_test.go
git commit -m "S5: dwarfagent end-to-end test against rust workload"
```

---

## Task 7 — perfagent config + agent dispatch

**Goal:** thread `--unwind` through Config → Agent.Start, dispatch on it.

**Files:**
- Modify: `perfagent/options.go`
- Modify: `perfagent/agent.go`

- [ ] **Step 7.1: Add Unwind field + WithUnwind option**

In `perfagent/options.go`, add to the `Config` struct (e.g. just before `CPUs`):

```go
	// Unwind selects the stack-unwinding strategy for --profile and
	// --offcpu modes. Valid values: "fp" (frame pointer), "dwarf"
	// (DWARF CFI), "auto" (currently routes to fp; see S8 in the
	// design doc). Empty string defaults to "fp".
	Unwind string
```

Append at the end of the file:

```go
// WithUnwind selects the stack-unwinding strategy. See Config.Unwind.
func WithUnwind(mode string) Option {
	return func(c *Config) {
		c.Unwind = mode
	}
}
```

- [ ] **Step 7.2: Dispatch in Agent.Start**

In `perfagent/agent.go`, find the section that constructs the CPU profiler:

```go
if a.config.EnableCPUProfile {
    profiler, err := profile.NewProfiler(
        a.config.PID,
        a.config.SystemWide,
        cpus,
        a.config.Tags,
        a.config.SampleRate,
    )
    ...
}
```

Replace with a dispatch:

```go
if a.config.EnableCPUProfile {
    switch a.config.Unwind {
    case "dwarf":
        if a.config.SystemWide {
            return fmt.Errorf("--unwind dwarf does not support system-wide mode yet (S7)")
        }
        p, err := dwarfagent.NewProfiler(
            a.config.PID,
            cpus,
            a.config.Tags,
            a.config.SampleRate,
        )
        if err != nil {
            return fmt.Errorf("create DWARF CPU profiler: %w", err)
        }
        a.cpuProfiler = p
        log.Printf("CPU profiler enabled (PID: %d, %d Hz, DWARF)", a.config.PID, a.config.SampleRate)
    default:
        p, err := profile.NewProfiler(
            a.config.PID,
            a.config.SystemWide,
            cpus,
            a.config.Tags,
            a.config.SampleRate,
        )
        if err != nil {
            return fmt.Errorf("create CPU profiler: %w", err)
        }
        a.cpuProfiler = p
        if a.config.SystemWide {
            log.Printf("CPU profiler enabled (system-wide, %d Hz)", a.config.SampleRate)
        } else {
            log.Printf("CPU profiler enabled (PID: %d, %d Hz)", a.config.PID, a.config.SampleRate)
        }
    }
}
```

The `a.cpuProfiler` field type needs to be an interface so both `*profile.Profiler` and `*dwarfagent.Profiler` fit. Find the Agent struct declaration (top of `agent.go`) and change:

```go
cpuProfiler *profile.Profiler
```

to:

```go
cpuProfiler cpuProfiler
```

and add this interface at package scope in `agent.go`:

```go
// cpuProfiler is the narrow shape both profile.Profiler and
// dwarfagent.Profiler satisfy, letting Agent dispatch on --unwind.
type cpuProfiler interface {
	Collect(w io.Writer) error
	CollectAndWrite(path string) error
	Close()
}
```

NOTE: `profile.Profiler.Close()` returns nothing; check its signature — if it returns `error`, update the interface accordingly. Same for `dwarfagent.Profiler.Close() error`. Match to whichever is currently declared. If the two differ, reconcile (this plan declares `dwarfagent.Profiler.Close() error`; if `profile.Profiler.Close()` returns nothing, wrap with `CloseFunc` or change dwarfagent's Close to `func (p *Profiler) Close()` that swallows the error).

The imports in `agent.go` need `"github.com/dpsoft/perf-agent/unwind/dwarfagent"` and `"io"`.

- [ ] **Step 7.3: Verify build**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./...
```

Expected: success.

- [ ] **Step 7.4: Commit**

```
git add perfagent/options.go perfagent/agent.go
git commit -m "S5: perfagent dispatches on Config.Unwind"
```

---

## Task 8 — main.go --unwind flag

**Goal:** expose `--unwind` on the CLI.

**Files:**
- Modify: `main.go`

- [ ] **Step 8.1: Add the flag**

In `main.go`'s var block (near the other flags), add:

```go
flagUnwind = flag.String("unwind", "fp", "Stack unwinding strategy: fp | dwarf | auto")
```

In `buildOptions()`, after the other `opts = append(...)` calls, add:

```go
	// Unwinding strategy.
	if *flagUnwind != "" {
		opts = append(opts, perfagent.WithUnwind(*flagUnwind))
	}
```

- [ ] **Step 8.2: Smoke-test parsing**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build -o /tmp/perf-agent .
/tmp/perf-agent --help 2>&1 | grep unwind
```

Expected: the `--unwind` flag appears in help output with `(default "fp")`.

- [ ] **Step 8.3: Commit**

```
git add main.go
git commit -m "S5: --unwind flag wired through main.go"
```

---

## Task 9 — Integration test: perf-agent --unwind dwarf

**Goal:** one integration test that runs the full binary with `--unwind dwarf` and verifies the pprof output.

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 9.1: Append the test**

```go
// TestPerfAgentDwarfUnwind runs the full perf-agent binary end-to-end
// with --unwind dwarf against the rust workload, then parses the
// resulting pprof.pb.gz and asserts cpu_intensive_work shows up as
// a symbolized function name.
func TestPerfAgentDwarfUnwind(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "profile-dwarf.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--profile",
		"--profile-output", outputFile,
		"--unwind", "dwarf",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)

	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "profile should have samples")

	hit := false
	for _, fn := range prof.Function {
		if strings.Contains(fn.Name, "cpu_intensive_work") {
			hit = true
			break
		}
	}
	if !hit {
		names := make([]string, 0, min(10, len(prof.Function)))
		for i, fn := range prof.Function {
			if i >= 10 {
				break
			}
			names = append(names, fn.Name)
		}
		t.Fatalf("no function named *cpu_intensive_work* in pprof; first few: %v", names)
	}
}
```

If `fmt` is not already imported in the file, add it (should be).

- [ ] **Step 9.2: Build capped integration binary + controller-run**

The controller handles:

```
cd test && GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/integration.test .
# setcap
# /home/diego/bin/integration.test -test.v -test.run TestPerfAgentDwarfUnwind
```

Expected: PASS, cpu_intensive_work shows up in the symbolized pprof.

- [ ] **Step 9.3: Commit**

```
git add test/integration_test.go
git commit -m "S5: TestPerfAgentDwarfUnwind — end-to-end --unwind dwarf against rust workload"
```

---

## Task 10 — Sanity matrix + design doc update

**Goal:** final check; mark S5 complete in design doc.

- [ ] **Step 10.1: Full test-unit**

`GOTOOLCHAIN=go1.26.0 make test-unit`

Expected: all PASS, CAP-gated tests skip.

- [ ] **Step 10.2: Capped test matrix**

```
/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads
/home/diego/bin/ehmaps.test -test.v
/home/diego/bin/dwarfagent.test -test.v -test.run TestProfilerEndToEnd
cd test && /home/diego/bin/integration.test -test.v -test.run "TestPerfDwarf|TestPerfAgentDwarf"
```

Expected: all PASS.

- [ ] **Step 10.3: Update design doc**

In `docs/dwarf-unwinding-design.md`, update the S5 row:

```
| S5 ✅  | `unwind/dwarfagent/` + `profile/` integration              | 2-3d    | End user runs `perf-agent --pid N --unwind dwarf` and gets a pprof profile. `--unwind auto` still routes to FP programs at this stage. **Shipped**: see `docs/superpowers/plans/2026-04-23-s5-dwarfagent-integration.md`. |
```

- [ ] **Step 10.4: Commit**

```
git add docs/dwarf-unwinding-design.md docs/superpowers/plans/2026-04-23-s5-dwarfagent-integration.md
git commit -m "S5: design doc status update + preserve implementation plan"
```

---

## Success criteria recap

From `docs/dwarf-unwinding-design.md` §Execution plan:

> **S5: `unwind/dwarfagent/` + `profile/` integration** — End user runs `perf-agent --pid N --unwind dwarf` and gets a pprof profile. `--unwind auto` still routes to FP programs at this stage.

Satisfied by:
- `TestPerfAgentDwarfUnwind` — invokes the real `perf-agent` binary with `--unwind dwarf --pid N`, parses the pprof output, asserts `cpu_intensive_work` symbol appears.
- `TestProfilerEndToEnd` — direct-library test validating dwarfagent.Profiler on its own.
- `--unwind auto` (or unspecified / `--unwind fp`) continues to go through `profile.Profiler` unchanged.

## Open risks

1. **Blazesym symbolization speed for Rust**. The rust-workload binary is ~5 MB with debug symbols; blazesym's first SymbolizeProcessAbsAddrs call can take hundreds of ms while it loads the symtab. Tests using 5s sample windows have some margin, but tight CI environments may need 10s. Bumping the sample window is the simplest fix if we see flakes.
2. **/proc/<pid>/maps churn during AttachAllMappings**. If the target dlopens a library mid-scan, we may miss it (scan only sees snapshot) but MmapWatcher picks it up later. Worst case: a short window where the new library's frames are unsymbolized. Acceptable for S5.
3. **Per-TID watcher limitation from S4**. If the target's main thread never dlopens anything (all dlopens happen from worker threads), new-library coverage depends on the initial scan only. Documented in the S4 doc row; S7 upgrades to per-CPU watchers to close this gap.
4. **`cpuProfiler` interface mismatch**. If `profile.Profiler.Close()` and `dwarfagent.Profiler.Close()` have different signatures, Task 7's interface declaration needs reconciling. Flagged inline but worth double-checking during implementation.
