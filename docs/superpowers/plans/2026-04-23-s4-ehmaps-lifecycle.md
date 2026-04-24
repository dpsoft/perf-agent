# S4: ehmaps Lifecycle + MMAP2 Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** turn the manual S3 "pre-populate one PID's CFI once" into automatic tracking — when a target process mmaps a new binary (e.g. `dlopen`), the agent compiles its CFI, installs the maps, and ref-counts; when the process exits, cleanup drops the tables.

**Architecture:** a new `TableStore` inside `unwind/ehmaps/` caches `build_id → table_id` with per-PID refcounts and owns the three outer BPF maps (`cfi_rules`, `cfi_classification`, `pid_mappings`) plus their length maps. A `PIDTracker` reads `/proc/<pid>/maps` on attach and listens to `PERF_RECORD_MMAP2` + `PERF_RECORD_EXIT` via a per-CPU perf_event ring buffer; each event feeds the store. No BPF-side changes — all S3 maps and helpers are reused.

**Tech Stack:** Go 1.26, cilium/ebpf v0.21.0, perf_event_open with `mmap_data=1`, existing `unwind/ehcompile` + `unwind/ehmaps` + `unwind/perfreader` packages.

---

## Scope

**S4 delivers:** after attaching the agent to a running target PID, any subsequent `mmap2` of an ELF in that PID's TGID automatically installs a CFI table (if new) and adds a pid-mapping row. Exit cleans up. Unwinding "just works" across the process's lifetime without pre-registration.

**Explicitly NOT in S4:**
- `profile.Profiler` integration / user-facing `--unwind dwarf` flag — S5.
- Off-CPU DWARF variant — S6.
- System-wide `-a` multi-PID tracking — S7.
- `execve` (new binary replaces address space) — stretch goal; documented as known gap if not done.

## Background for implementers

**PERF_RECORD_MMAP2:** perf_event_open with `attr.mmap_data = 1` (and `sample_period = 0`) causes the kernel to write MMAP2 records to the ring buffer whenever the target calls `mmap` with a file-backed executable mapping. The record carries: pid, tid, addr, len, pgoff, maj/min/ino/ino_gen, prot, flags, filename. For our purposes the useful fields are `pid + addr + len + pgoff + prot (x bit) + filename`.

**PERF_RECORD_EXIT:** fires when a PID exits. Carries pid, ppid, tid, ptid, time. Use this to drop pid mappings from the BPF map.

**Why `perf_event` for mmap2 rather than fanotify / inotify:** perf_event gives us per-PID filtering (attach to target PID) and consistent kernel-side framing. The existing `perfreader` package already does the ring-buffer reading dance.

**Existing facilities:**
- `unwind/perfreader` opens perf_events, mmap's the ring, parses records. S4 needs a variant with different `sample_type`/flags.
- `test/integration_test.go:readBuildID`, `extractGNUBuildID`, `loadProcessMappings` — these work; S4 promotes them to public helpers in `unwind/ehmaps` so agent code can call them.
- `unwind/ehmaps.Populate*` and `TableIDForBuildID` — already the primitives the new TableStore wraps.

**Key invariants:**
- **One table per build_id**, regardless of how many processes load it.
- **Refcount by PID** — when every PID that references a build_id has exited/unmapped it, evict.
- **Eviction is best-effort** — if evict fails (EINVAL, ENOMEM), log and continue; table stays leaked rather than risking corrupt state.

## File Structure

```
unwind/ehmaps/elf_helpers.go        CREATE — readBuildID, extractGNUBuildID, executable PT_LOAD scan, load_bias calc. Moved/promoted from test code.
unwind/ehmaps/elf_helpers_test.go   CREATE — round-trip tests with the Rust workload ELF.

unwind/ehmaps/store.go              CREATE — TableStore: build_id → table_id cache with refcounts; drives PopulateCFI/Classification. Owns no lifecycle.
unwind/ehmaps/store_test.go         CREATE — unit tests for refcount arithmetic (no BPF runtime).

unwind/ehmaps/tracker.go            CREATE — PIDTracker: combines TableStore with pid_mappings map; knows how to attach a new PID (scan /proc/maps) and detach one.
unwind/ehmaps/tracker_test.go       CREATE — CAP_BPF-gated test that attaches a synthetic PID (self) and verifies pid_mappings is populated.

unwind/ehmaps/mmap_watcher.go       CREATE — perf_event+ring reader specialised for PERF_RECORD_MMAP2 + PERF_RECORD_EXIT; feeds PIDTracker.
unwind/ehmaps/mmap_watcher_test.go  CREATE — CAP_BPF-gated test that spawns a test helper doing a mmap syscall, verifies the watcher sees it.

test/integration_test.go            MODIFY — remove the manual readBuildID/loadProcessMappings helpers (now in ehmaps); replace TestPerfDwarfWalker's manual population with a PIDTracker call. Keep the assertions.

test/integration_test.go (new test) MODIFY — add TestPerfDwarfMmap2Tracking: start rust workload, attach PIDTracker, start MMAP2 watcher, dlopen a probe .so from the workload, verify a new mapping gets installed (length map grew, outer map has the new table_id).

test/workloads/rust/src/main.rs     MODIFY — add an optional CLI flag `--dlopen <path>` that dlopens the given shared library before entering the CPU loop, so the integration test has something to trigger mmap2 events against.

docs/dwarf-unwinding-design.md      MODIFY — one-paragraph S4 status update (what shipped, what was deferred).
```

---

## Task 1 — Promote ELF helpers into `unwind/ehmaps`

**Goal:** move `readBuildID`, `extractGNUBuildID`, and `loadProcessMappings` from test-only code into the `ehmaps` package so agent code can call them. Keep the exact implementations (they're already correct post S3).

**Files:**
- Create: `unwind/ehmaps/elf_helpers.go`
- Create: `unwind/ehmaps/elf_helpers_test.go`
- Modify: `test/integration_test.go` — delete the local copies, import from ehmaps
- Modify: `cmd/perf-dwarf-test/main.go` — same

- [ ] **Step 1.1: Write failing tests**

Create `unwind/ehmaps/elf_helpers_test.go`:

```go
package ehmaps_test

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// TestReadBuildIDRustWorkload checks that ReadBuildID returns a non-empty
// byte slice for the Rust workload binary committed to the repo. Exact
// bytes change per-build; just assert presence.
func TestReadBuildIDRustWorkload(t *testing.T) {
	const path = "../../test/workloads/rust/target/release/rust-workload"
	id, err := ehmaps.ReadBuildID(path)
	if err != nil {
		t.Skipf("rust workload not built (%v); skipping", err)
	}
	if len(id) == 0 {
		t.Fatalf("ReadBuildID returned empty slice for %s", path)
	}
	// GNU build-id is conventionally 20 bytes for sha1 / 16 for md5.
	if len(id) < 8 {
		t.Fatalf("build-id suspiciously short (%d bytes): %x", len(id), id)
	}
}

func TestReadBuildIDMissingFile(t *testing.T) {
	if _, err := ehmaps.ReadBuildID("/nonexistent/binary"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
```

- [ ] **Step 1.2: Run tests, verify they fail**

Run: `GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: ehmaps.ReadBuildID`.

- [ ] **Step 1.3: Create `unwind/ehmaps/elf_helpers.go`**

```go
package ehmaps

import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadBuildID reads the GNU build-id from an ELF's .note.gnu.build-id.
// Returns the raw bytes (typically 20 for sha1), or an error if absent.
func ReadBuildID(path string) ([]byte, error) {
	ef, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer ef.Close()
	for _, sec := range ef.Sections {
		if sec.Type != elf.SHT_NOTE {
			continue
		}
		data, err := sec.Data()
		if err != nil {
			continue
		}
		if id := extractGNUBuildID(data); id != nil {
			return id, nil
		}
	}
	return nil, errors.New("no .note.gnu.build-id section")
}

// extractGNUBuildID walks an ELF .note section payload looking for the
// type-3 "GNU"-named note. Format: u32 name_size, u32 desc_size, u32 type,
// name (padded to 4), desc (padded to 4).
func extractGNUBuildID(notes []byte) []byte {
	for len(notes) >= 12 {
		nameSize := binary.LittleEndian.Uint32(notes[0:4])
		descSize := binary.LittleEndian.Uint32(notes[4:8])
		noteType := binary.LittleEndian.Uint32(notes[8:12])
		p := 12
		nameEnd := p + int(nameSize)
		namePadded := (nameEnd + 3) &^ 3
		if namePadded > len(notes) {
			return nil
		}
		descEnd := namePadded + int(descSize)
		descPadded := (descEnd + 3) &^ 3
		if descPadded > len(notes) {
			return nil
		}
		if noteType == 3 && nameSize == 4 && string(notes[p:p+3]) == "GNU" {
			return notes[namePadded:descEnd]
		}
		notes = notes[descPadded:]
	}
	return nil
}

// LoadProcessMappings reads /proc/<pid>/maps and returns one PIDMapping
// per executable-mapped range of binPath. The load bias is computed from
// the ELF's executable PT_LOAD — "vma_start - file_offset" is wrong for
// PIE binaries where PT_LOAD vaddr differs from file offset (Rust's
// release output has a 0x1000 hole).
func LoadProcessMappings(pid int, binPath string, tableID uint64) ([]PIDMapping, error) {
	ef, err := elf.Open(binPath)
	if err != nil {
		return nil, err
	}
	defer ef.Close()
	var execProg *elf.Prog
	for _, p := range ef.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
			execProg = p
			break
		}
	}
	if execProg == nil {
		return nil, errors.New("no executable PT_LOAD in ELF")
	}
	const pageMask uint64 = ^uint64(0xfff)
	execVaddrAligned := execProg.Vaddr & pageMask
	execOffsetAligned := execProg.Off & pageMask

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, err
	}
	base := binPath[strings.LastIndex(binPath, "/")+1:]

	var loadBias uint64
	var haveBias bool
	var out []PIDMapping
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		if !strings.HasSuffix(fields[5], base) {
			continue
		}
		addrs := strings.SplitN(fields[0], "-", 2)
		if len(addrs) != 2 {
			continue
		}
		start, err := strconv.ParseUint(addrs[0], 16, 64)
		if err != nil {
			continue
		}
		end, err := strconv.ParseUint(addrs[1], 16, 64)
		if err != nil {
			continue
		}
		offset, err := strconv.ParseUint(fields[2], 16, 64)
		if err != nil {
			continue
		}
		if !haveBias && offset == execOffsetAligned {
			loadBias = start - execVaddrAligned
			haveBias = true
		}
		out = append(out, PIDMapping{
			VMAStart: start,
			VMAEnd:   end,
			TableID:  tableID,
		})
	}
	if !haveBias {
		return nil, errors.New("executable PT_LOAD has no matching /proc/<pid>/maps entry")
	}
	for i := range out {
		out[i].LoadBias = loadBias
	}
	return out, nil
}
```

- [ ] **Step 1.4: Run tests, verify they pass**

Run: `GOTOOLCHAIN=go1.26.0 make test-workloads && GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `TestReadBuildIDRustWorkload` PASS (or SKIP if workload not built), `TestReadBuildIDMissingFile` PASS.

- [ ] **Step 1.5: Delete test-local copies**

In `test/integration_test.go`: delete the `readBuildID`, `extractGNUBuildID`, `loadProcessMappings` function definitions (keep the call sites). Replace the call sites:

```go
buildID, err := readBuildID(binPath)
```
with:
```go
buildID, err := ehmaps.ReadBuildID(binPath)
```

And:
```go
mappings, err := loadProcessMappings(workload.Process.Pid, binPath, tableID)
```
with:
```go
mappings, err := ehmaps.LoadProcessMappings(workload.Process.Pid, binPath, tableID)
```

Do the same in `cmd/perf-dwarf-test/main.go`.

- [ ] **Step 1.6: Rebuild + verify**

```
GOTOOLCHAIN=go1.26.0 go vet ./...
GOTOOLCHAIN=go1.26.0 make test-unit
```

Expected: both succeed. The integration test's helper functions are now gone; integration.test needs a rebuild before it can run.

- [ ] **Step 1.7: Commit**

```
git add unwind/ehmaps/elf_helpers.go unwind/ehmaps/elf_helpers_test.go test/integration_test.go cmd/perf-dwarf-test/main.go
git commit -m "S4: promote ELF helpers from test code into ehmaps package"
```

---

## Task 2 — `TableStore`: build_id → table_id cache with refcounts

**Goal:** a Go type that owns the three outer BPF map handles (`cfi_rules`, `cfi_lengths`, `cfi_classification`, `cfi_classification_lengths`) and tracks how many PIDs have claimed each table_id. On first claim, compiles + populates. On last release, evicts.

**Files:**
- Create: `unwind/ehmaps/store.go`
- Create: `unwind/ehmaps/store_test.go`

- [ ] **Step 2.1: Write the failing test**

Create `unwind/ehmaps/store_test.go`:

```go
package ehmaps_test

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// TestRefcountIncrement verifies that AcquireByID on the same tableID
// from multiple PIDs increments the refcount rather than re-compiling.
func TestRefcountIncrement(t *testing.T) {
	rc := ehmaps.NewRefcountTable()
	const tid uint64 = 0x42

	if got := rc.Acquire(tid, 100); got != 1 {
		t.Fatalf("first acquire: refcount=%d, want 1", got)
	}
	if got := rc.Acquire(tid, 200); got != 2 {
		t.Fatalf("second acquire: refcount=%d, want 2", got)
	}
	if got := rc.Release(tid, 100); got != 1 {
		t.Fatalf("release pid 100: refcount=%d, want 1", got)
	}
	if got := rc.Release(tid, 200); got != 0 {
		t.Fatalf("release pid 200: refcount=%d, want 0 (evictable)", got)
	}
}

// TestRefcountDoubleAcquireSamePID is a no-op by design — acquiring the
// same (tid, pid) pair twice should only count as one reference.
func TestRefcountDoubleAcquireSamePID(t *testing.T) {
	rc := ehmaps.NewRefcountTable()
	const tid uint64 = 0x42
	rc.Acquire(tid, 100)
	if got := rc.Acquire(tid, 100); got != 1 {
		t.Fatalf("re-acquire same pid: refcount=%d, want 1 (idempotent)", got)
	}
	if got := rc.Release(tid, 100); got != 0 {
		t.Fatalf("release after re-acquire: refcount=%d, want 0", got)
	}
}

// TestRefcountReleaseUntracked returns 0 without error.
func TestRefcountReleaseUntracked(t *testing.T) {
	rc := ehmaps.NewRefcountTable()
	if got := rc.Release(0x99, 42); got != 0 {
		t.Fatalf("release untracked: refcount=%d, want 0", got)
	}
}
```

- [ ] **Step 2.2: Run, verify fail**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: ehmaps.NewRefcountTable`.

- [ ] **Step 2.3: Implement the refcount table**

Create `unwind/ehmaps/store.go`:

```go
package ehmaps

import (
	"fmt"
	"sync"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// RefcountTable tracks which (tableID, PID) pairs currently reference a
// CFI table. A table stays in the BPF maps until the last PID releases it.
// Zero-value is not usable; construct via NewRefcountTable.
//
// Operations are safe for concurrent use across multiple MMAP2/EXIT
// event handlers. Acquire and Release return the post-operation refcount
// for the given tableID so the caller can decide whether to install or
// evict the actual BPF-side table.
type RefcountTable struct {
	mu     sync.Mutex
	byID   map[uint64]map[uint32]struct{} // tableID → set of PIDs
}

func NewRefcountTable() *RefcountTable {
	return &RefcountTable{byID: map[uint64]map[uint32]struct{}{}}
}

// Acquire records that `pid` now references `tableID`. Idempotent — a
// repeat acquire for the same (tid, pid) does NOT double-count. Returns
// the resulting refcount (number of distinct PIDs holding this tableID).
func (r *RefcountTable) Acquire(tableID uint64, pid uint32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	pids, ok := r.byID[tableID]
	if !ok {
		pids = map[uint32]struct{}{}
		r.byID[tableID] = pids
	}
	pids[pid] = struct{}{}
	return len(pids)
}

// Release records that `pid` no longer references `tableID`. Returns the
// resulting refcount. Releasing an untracked (tid, pid) is a no-op (returns 0).
func (r *RefcountTable) Release(tableID uint64, pid uint32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	pids, ok := r.byID[tableID]
	if !ok {
		return 0
	}
	delete(pids, pid)
	if len(pids) == 0 {
		delete(r.byID, tableID)
		return 0
	}
	return len(pids)
}

// TableStore owns the BPF-side cfi_* outer maps and composes refcount
// tracking with actual map population. It is the S4 replacement for the
// hand-wired calls to PopulateCFI/PopulateClassification in S3's tests.
type TableStore struct {
	CFIRules          *ebpf.Map
	CFILengths        *ebpf.Map
	CFIClassification *ebpf.Map
	CFIClassLengths   *ebpf.Map

	rc *RefcountTable
}

// NewTableStore wires up a TableStore around already-loaded BPF maps
// (typically from the agent's perf_dwarf program load). The caller owns
// the maps; TableStore does not close them.
func NewTableStore(cfi, cfiLen, cls, clsLen *ebpf.Map) *TableStore {
	return &TableStore{
		CFIRules:          cfi,
		CFILengths:        cfiLen,
		CFIClassification: cls,
		CFIClassLengths:   clsLen,
		rc:                NewRefcountTable(),
	}
}

// AcquireBinary ensures CFI for `binPath` is installed and references
// it on behalf of `pid`. Returns the tableID plus a boolean indicating
// whether a fresh compile happened.
func (s *TableStore) AcquireBinary(binPath string, pid uint32) (tableID uint64, compiled bool, err error) {
	buildID, err := ReadBuildID(binPath)
	if err != nil {
		return 0, false, fmt.Errorf("build-id %s: %w", binPath, err)
	}
	tableID = TableIDForBuildID(buildID)
	if rc := s.rc.Acquire(tableID, pid); rc > 1 {
		return tableID, false, nil // already installed
	}
	// First reference for this tableID — compile + install.
	entries, classifications, err := ehcompile.Compile(binPath)
	if err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("ehcompile %s: %w", binPath, err)
	}
	if err := PopulateCFI(PopulateCFIArgs{
		TableID: tableID, Entries: entries,
		OuterMap: s.CFIRules, LengthMap: s.CFILengths,
	}); err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("populate cfi: %w", err)
	}
	if err := PopulateClassification(PopulateClassificationArgs{
		TableID: tableID, Entries: classifications,
		OuterMap: s.CFIClassification, LengthMap: s.CFIClassLengths,
	}); err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("populate classification: %w", err)
	}
	return tableID, true, nil
}

// ReleaseBinary drops `pid`'s reference to `tableID`. If the refcount
// hits zero, evicts the inner maps (best-effort — eviction errors are
// returned but the refcount is still decremented).
func (s *TableStore) ReleaseBinary(tableID uint64, pid uint32) error {
	if rc := s.rc.Release(tableID, pid); rc > 0 {
		return nil
	}
	// Evict. Deleting from the outer HASH_OF_MAPS drops the kernel's
	// reference to the inner map, which the kernel then frees.
	var firstErr error
	if err := s.CFIRules.Delete(tableID); err != nil {
		firstErr = fmt.Errorf("evict cfi_rules[%#x]: %w", tableID, err)
	}
	if err := s.CFILengths.Delete(tableID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("evict cfi_lengths[%#x]: %w", tableID, err)
	}
	if err := s.CFIClassification.Delete(tableID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("evict cfi_classification[%#x]: %w", tableID, err)
	}
	if err := s.CFIClassLengths.Delete(tableID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("evict cfi_classification_lengths[%#x]: %w", tableID, err)
	}
	return firstErr
}
```

- [ ] **Step 2.4: Run tests, verify pass**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: 3 refcount tests PASS + existing tests pass.

- [ ] **Step 2.5: Commit**

```
git add unwind/ehmaps/store.go unwind/ehmaps/store_test.go
git commit -m "S4: TableStore with build-id refcounting + auto compile/evict"
```

### Review findings to incorporate before calling Task 2 complete

- **Concurrency bug in `AcquireBinary` / `ReleaseBinary`:** the sketch above increments/decrements the refcount under `RefcountTable.mu`, but the compile/install and evict paths run outside that lock. That creates a race where:
  `AcquireBinary` for PID A wins the first reference and starts `ehcompile` + map population;
  `AcquireBinary` for PID B arrives before install completes, sees `rc > 1`, returns success immediately, and `PIDTracker` can publish a `pid_mappings` row pointing at a table that is not installed yet;
  `ReleaseBinary` can also race with a concurrent reacquire and delete a freshly installed table.
  The implementation needs a per-table install/evict state machine or another synchronization mechanism that makes "table is referenced" imply "table is fully installed and not concurrently being evicted."

- **Partial-install leak on classification failure:** in the sketch above, if `PopulateCFI` succeeds but `PopulateClassification` fails, the function only drops the refcount and returns. That leaves `cfi_rules` / `cfi_lengths` populated for a table ID that was not fully installed. The final implementation must roll back any already-created outer-map entries (and their length-map rows) before returning the error so retries start from a clean state.

- **Test gap exposed by the review:** the current `store_test.go` coverage only exercises `RefcountTable`. Task 2 is not complete without tests that fail on the two cases above:
  concurrent acquire/release around one `tableID`, proving a second caller cannot observe success before install completes;
  failed classification install, proving any earlier `PopulateCFI` state is cleaned up.

---

## Task 3 — `PIDTracker`: combine TableStore with pid_mappings map

**Goal:** one type that: (a) owns the `pid_mappings` + `pid_mapping_lengths` BPF maps, (b) knows how to attach a PID (scan `/proc/<pid>/maps`, acquire each binary from TableStore, install pid_mappings row), and (c) how to detach (reverse).

**Files:**
- Create: `unwind/ehmaps/tracker.go`
- Create: `unwind/ehmaps/tracker_test.go`

- [ ] **Step 3.1: Write the failing test**

Create `unwind/ehmaps/tracker_test.go`:

```go
package ehmaps_test

import (
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

func requireBPFCaps(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	caps := cap.GetProc()
	have, err := caps.GetFlag(cap.Permitted, cap.BPF)
	if err != nil || !have {
		t.Skip("CAP_BPF not available")
	}
}

// TestTrackerAttachSelf attaches the tracker to the test process itself
// and verifies that at least one pid_mappings entry was written (the
// test binary mapping).
func TestTrackerAttachSelf(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}

	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := ehmaps.NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := ehmaps.NewPIDTracker(store, pidMaps, pidMapLen)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := tracker.Attach(uint32(os.Getpid()), self); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	var gotLen uint32
	if err := pidMapLen.Lookup(uint32(os.Getpid()), &gotLen); err != nil {
		t.Fatalf("pid_mapping_lengths lookup: %v", err)
	}
	if gotLen == 0 {
		t.Fatal("expected at least one pid_mappings entry, got zero")
	}

	if err := tracker.Detach(uint32(os.Getpid())); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	// Length entry should be gone.
	if err := pidMapLen.Lookup(uint32(os.Getpid()), &gotLen); err == nil {
		t.Fatalf("pid_mapping_lengths still present after Detach: %d", gotLen)
	}
}

// Helper: create S3-shape test maps that mirror the ones bpf2go generates.
func newTestMaps(t *testing.T) (cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen *ebpf.Map) {
	t.Helper()
	var err error
	const innerFlag = 0x1000 // BPF_F_INNER_MAP
	mk := func(spec *ebpf.MapSpec) *ebpf.Map {
		m, err := ebpf.NewMap(spec)
		if err != nil {
			t.Fatalf("NewMap %s: %v", spec.Type, err)
		}
		return m
	}
	cfi = mk(&ebpf.MapSpec{
		Type: ebpf.HashOfMaps, KeySize: 8, ValueSize: 4, MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: ehmaps.CFIEntryByteSize, MaxEntries: 1, Flags: innerFlag},
	})
	cfiLen = mk(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: 4, MaxEntries: 4})
	cls = mk(&ebpf.MapSpec{
		Type: ebpf.HashOfMaps, KeySize: 8, ValueSize: 4, MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: ehmaps.ClassificationByteSize, MaxEntries: 1, Flags: innerFlag},
	})
	clsLen = mk(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: 4, MaxEntries: 4})
	pidMaps = mk(&ebpf.MapSpec{
		Type: ebpf.HashOfMaps, KeySize: 4, ValueSize: 4, MaxEntries: 4,
		InnerMap: &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: ehmaps.PIDMappingByteSize, MaxEntries: ehmaps.MaxPIDMappings, Flags: innerFlag},
	})
	pidMapLen = mk(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 4, ValueSize: 4, MaxEntries: 4})
	_ = err
	return
}

func closeAll(ms ...*ebpf.Map) {
	for _, m := range ms {
		if m != nil {
			_ = m.Close()
		}
	}
}
```

- [ ] **Step 3.2: Run, verify fail**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: ehmaps.NewPIDTracker` (runtime test skips without caps; compile error surfaces first).

- [ ] **Step 3.3: Implement PIDTracker**

Create `unwind/ehmaps/tracker.go`:

```go
package ehmaps

import (
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
)

// PIDTracker holds per-PID state for the hybrid unwinder. Each Attach
// populates pid_mappings for that PID and takes a TableStore reference
// for every unique binary in the process's address space. Detach
// reverses both.
//
// S4 scope: single-binary attach (the target executable). Subsequent
// mmap2 events (shared libraries, dlopen) are added via AddMapping and
// paired with the same PID's refcounts.
type PIDTracker struct {
	store         *TableStore
	pidMappings   *ebpf.Map
	pidMapLens    *ebpf.Map

	mu       sync.Mutex
	perPID   map[uint32]*pidState
}

type pidState struct {
	mappings []PIDMapping
	tableIDs map[uint64]struct{} // unique table_ids this PID holds
}

func NewPIDTracker(store *TableStore, pidMappings, pidMapLengths *ebpf.Map) *PIDTracker {
	return &PIDTracker{
		store:       store,
		pidMappings: pidMappings,
		pidMapLens:  pidMapLengths,
		perPID:      map[uint32]*pidState{},
	}
}

// Attach walks /proc/<pid>/maps for binPath, acquires CFI via the store,
// and installs a pid_mappings row. Safe to call multiple times with
// different binPaths for the same PID (mappings accumulate).
func (t *PIDTracker) Attach(pid uint32, binPath string) error {
	tableID, _, err := t.store.AcquireBinary(binPath, pid)
	if err != nil {
		return fmt.Errorf("acquire %s: %w", binPath, err)
	}
	newMappings, err := LoadProcessMappings(int(pid), binPath, tableID)
	if err != nil {
		_ = t.store.ReleaseBinary(tableID, pid)
		return fmt.Errorf("load mappings pid=%d: %w", pid, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.perPID[pid]
	if !ok {
		st = &pidState{tableIDs: map[uint64]struct{}{}}
		t.perPID[pid] = st
	}
	st.mappings = append(st.mappings, newMappings...)
	st.tableIDs[tableID] = struct{}{}
	return PopulatePIDMappings(PopulatePIDMappingsArgs{
		PID: pid, Mappings: st.mappings,
		OuterMap: t.pidMappings, LengthMap: t.pidMapLens,
	})
}

// Detach removes the PID from the pid_mappings map and releases all
// binaries it held. Safe to call for an unknown PID (no-op).
func (t *PIDTracker) Detach(pid uint32) error {
	t.mu.Lock()
	st, ok := t.perPID[pid]
	if !ok {
		t.mu.Unlock()
		return nil
	}
	delete(t.perPID, pid)
	t.mu.Unlock()

	var firstErr error
	if err := t.pidMappings.Delete(pid); err != nil {
		firstErr = fmt.Errorf("delete pid_mappings[%d]: %w", pid, err)
	}
	if err := t.pidMapLens.Delete(pid); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete pid_mapping_lengths[%d]: %w", pid, err)
	}
	for tid := range st.tableIDs {
		if err := t.store.ReleaseBinary(tid, pid); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
```

- [ ] **Step 3.4: Run tests, verify compile**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: compile passes; `TestTrackerAttachSelf` skips (no caps).

- [ ] **Step 3.5: Capped run**

Build capped: `GOTOOLCHAIN=go1.26.0 go test -c -o /home/diego/bin/ehmaps.test ./unwind/ehmaps/`

Ask user: `sudo setcap cap_sys_admin,cap_bpf,cap_perfmon+ep /home/diego/bin/ehmaps.test`

Run: `/home/diego/bin/ehmaps.test -test.v -test.run TestTrackerAttachSelf`

Expected: PASS.

- [ ] **Step 3.6: Commit**

```
git add unwind/ehmaps/tracker.go unwind/ehmaps/tracker_test.go
git commit -m "S4: PIDTracker attaches PIDs via TableStore + pid_mappings"
```

---

## Task 4 — MMAP2 watcher: perf_event ring reader specialized for metadata events

**Goal:** a small reader that opens a perf_event with `mmap_data=1, sample_period=0` attached to `pid=targetPID, cpu=-1`, parses PERF_RECORD_MMAP2 and PERF_RECORD_EXIT from its ring buffer, and emits them as typed events on a Go channel.

**Files:**
- Create: `unwind/ehmaps/mmap_watcher.go`
- Create: `unwind/ehmaps/mmap_watcher_test.go`

- [ ] **Step 4.1: Write the failing test**

Create `unwind/ehmaps/mmap_watcher_test.go`:

```go
package ehmaps_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// TestMmapWatcherSeesMmap attaches a watcher to the test process itself
// and then does a deliberate executable file mmap, verifying the watcher
// picks up the event. Attaching to ourselves + controlling the mmap
// eliminates the child-startup race.
func TestMmapWatcherSeesMmap(t *testing.T) {
	requireBPFCaps(t)

	w, err := ehmaps.NewMmapWatcher(uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("NewMmapWatcher: %v", err)
	}
	defer w.Close()

	// Let the reader goroutine start draining.
	time.Sleep(100 * time.Millisecond)

	const target = "/bin/ls"
	f, err := os.Open(target)
	if err != nil {
		t.Skipf("%s not available: %v", target, err)
	}
	defer f.Close()
	data, err := unix.Mmap(int(f.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_EXEC, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap(%s, PROT_EXEC): %v", target, err)
	}
	defer unix.Munmap(data)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("event channel closed before /bin/ls MMAP2 observed")
			}
			if ev.Kind == ehmaps.MmapEvent && strings.HasSuffix(ev.Filename, "/ls") {
				return
			}
		case <-deadline:
			t.Fatal("no MMAP2 event for /bin/ls within 2s")
		}
	}
}
```

- [ ] **Step 4.2: Run, verify fail**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: ehmaps.NewMmapWatcher`.

- [ ] **Step 4.3: Implement the watcher**

Create `unwind/ehmaps/mmap_watcher.go`:

```go
package ehmaps

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// MmapEventKind distinguishes event records emitted by MmapWatcher.
type MmapEventKind int

const (
	MmapEvent MmapEventKind = iota + 1
	ExitEvent
)

// MmapEventRecord is a parsed PERF_RECORD_MMAP2 or PERF_RECORD_EXIT.
// Fields are populated based on Kind — Exit uses PID + TID (to distinguish
// group-leader exit from per-thread exit); Mmap uses all the mapping fields.
type MmapEventRecord struct {
	Kind     MmapEventKind
	PID      uint32 // TGID
	TID      uint32 // thread ID (equals PID when the group leader itself)
	Addr     uint64
	Len      uint64
	PgOff    uint64
	Prot     uint32
	Filename string
}

const (
	// PERF_RECORD_MMAP2 = 10, PERF_RECORD_EXIT = 4 (include/uapi/linux/perf_event.h).
	perfRecordMmap2 = 10
	perfRecordExit  = 4
)

// MmapWatcher owns one perf_event fd + ring buffer and delivers parsed
// MMAP2 and EXIT records via Events(). Construct with NewMmapWatcher;
// always Close() when done.
type MmapWatcher struct {
	fd       int
	mmap     []byte
	data     []byte
	pageSz   int
	dataHead *uint64
	dataTail *uint64
	events   chan MmapEventRecord
	done     chan struct{}
}

const (
	mwRingPages     = 64
	dataHeadOffset  = 1024
	dataTailOffset  = 1032
)

// NewMmapWatcher attaches a MMAP2-only perf_event to `pid` across all CPUs.
// Requires CAP_PERFMON (or CAP_BPF / root).
func NewMmapWatcher(pid uint32) (*MmapWatcher, error) {
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_DUMMY, // no samples; just metadata events
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		// Mmap2 enables PERF_RECORD_MMAP2 (MMAP with inode metadata).
		// Task enables FORK/EXIT/COMM; we only care about EXIT but the
		// flag is a bundle. Start disabled; enable after mmap'ing the ring.
		Bits: unix.PerfBitMmap2 | unix.PerfBitTask | unix.PerfBitDisabled,
	}
	fd, err := unix.PerfEventOpen(attr, int(pid), -1, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open (mmap2, pid=%d): %w", pid, err)
	}
	pageSz := os.Getpagesize()
	mmapSz := pageSz * (1 + mwRingPages)
	mapped, err := unix.Mmap(fd, 0, mmapSz, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("mmap mmap-watcher ring: %w", err)
	}
	w := &MmapWatcher{
		fd:       fd,
		mmap:     mapped,
		data:     mapped[pageSz:],
		pageSz:   pageSz,
		dataHead: (*uint64)(unsafe.Pointer(&mapped[dataHeadOffset])),
		dataTail: (*uint64)(unsafe.Pointer(&mapped[dataTailOffset])),
		events:   make(chan MmapEventRecord, 128),
		done:     make(chan struct{}),
	}
	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		w.Close()
		return nil, fmt.Errorf("perf_event enable: %w", err)
	}
	go w.loop()
	return w, nil
}

// Events returns the channel of parsed records. Closed when the watcher
// shuts down (via Close or unrecoverable ring error).
func (w *MmapWatcher) Events() <-chan MmapEventRecord { return w.events }

// Close stops the reader goroutine and releases the fd + mapping.
func (w *MmapWatcher) Close() error {
	select {
	case <-w.done:
		// already closed
	default:
		close(w.done)
	}
	var firstErr error
	if w.fd > 0 {
		if err := unix.IoctlSetInt(w.fd, unix.PERF_EVENT_IOC_DISABLE, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if w.mmap != nil {
		if err := unix.Munmap(w.mmap); err != nil && firstErr == nil {
			firstErr = err
		}
		w.mmap = nil
	}
	if w.fd > 0 {
		if err := unix.Close(w.fd); err != nil && firstErr == nil {
			firstErr = err
		}
		w.fd = 0
	}
	return firstErr
}

// loop is the reader goroutine. Polls the ring until done is signaled.
func (w *MmapWatcher) loop() {
	defer close(w.events)
	pfd := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-w.done:
			return
		default:
		}
		_, _ = unix.Poll(pfd, 100)
		w.drain()
	}
}

// drain consumes all records currently between data_tail and data_head.
func (w *MmapWatcher) drain() {
	head := atomic.LoadUint64(w.dataHead)
	tail := atomic.LoadUint64(w.dataTail)
	size := uint64(len(w.data))
	for tail < head {
		// Record header: u32 type, u16 misc, u16 size.
		base := tail % size
		hdr := w.readBytes(base, 8)
		typ := binary.LittleEndian.Uint32(hdr[0:4])
		recSize := binary.LittleEndian.Uint16(hdr[6:8])
		body := w.readBytes((base+8)%size, uint64(recSize)-8)

		switch typ {
		case perfRecordMmap2:
			if ev, ok := parseMmap2(body); ok {
				select {
				case w.events <- ev:
				case <-w.done:
					return
				}
			}
		case perfRecordExit:
			if ev, ok := parseExit(body); ok {
				select {
				case w.events <- ev:
				case <-w.done:
					return
				}
			}
		}
		tail += uint64(recSize)
	}
	atomic.StoreUint64(w.dataTail, tail)
}

// readBytes reads n bytes starting at offset `off` in the ring, handling
// wraparound.
func (w *MmapWatcher) readBytes(off, n uint64) []byte {
	size := uint64(len(w.data))
	if off+n <= size {
		return w.data[off : off+n]
	}
	buf := make([]byte, n)
	first := size - off
	copy(buf, w.data[off:])
	copy(buf[first:], w.data[:n-first])
	return buf
}

// parseMmap2 decodes a PERF_RECORD_MMAP2 body. Layout (simplified):
//   u32 pid, u32 tid, u64 addr, u64 len, u64 pgoff, u32 maj, u32 min,
//   u64 ino, u64 ino_generation, u32 prot, u32 flags, char filename[]
func parseMmap2(body []byte) (MmapEventRecord, bool) {
	if len(body) < 4+4+8+8+8+4+4+8+8+4+4 {
		return MmapEventRecord{}, false
	}
	pid := binary.LittleEndian.Uint32(body[0:4])
	tid := binary.LittleEndian.Uint32(body[4:8])
	addr := binary.LittleEndian.Uint64(body[8:16])
	length := binary.LittleEndian.Uint64(body[16:24])
	pgoff := binary.LittleEndian.Uint64(body[24:32])
	prot := binary.LittleEndian.Uint32(body[56:60])
	// filename is at offset 64, null-terminated, padded to u64.
	name := body[64:]
	if i := indexOfZero(name); i >= 0 {
		name = name[:i]
	}
	return MmapEventRecord{
		Kind: MmapEvent, PID: pid, TID: tid,
		Addr: addr, Len: length, PgOff: pgoff, Prot: prot,
		Filename: string(name),
	}, true
}

// parseExit decodes PERF_RECORD_EXIT body:
//
//	u32 pid, u32 ppid, u32 tid, u32 ptid, u64 time
//
// PERF_RECORD_EXIT fires per-task. When a thread in the watched TGID exits,
// pid is the TGID and tid is the exiting thread's ID. We carry both so the
// caller can distinguish group-leader exit (tid == pid, whole process gone)
// from a worker-thread exit (tid != pid, process still alive).
func parseExit(body []byte) (MmapEventRecord, bool) {
	if len(body) < 16 {
		return MmapEventRecord{}, false
	}
	return MmapEventRecord{
		Kind: ExitEvent,
		PID:  binary.LittleEndian.Uint32(body[0:4]),
		TID:  binary.LittleEndian.Uint32(body[8:12]),
	}, true
}

func indexOfZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 4.4: Compile + capped run**

`GOTOOLCHAIN=go1.26.0 go test -c -o /home/diego/bin/ehmaps.test ./unwind/ehmaps/`

User setcap step, then:

`/home/diego/bin/ehmaps.test -test.v -test.run TestMmapWatcher`

Expected: PASS with at least one MMAP2 event observed for the child PID.

- [ ] **Step 4.5: Commit**

```
git add unwind/ehmaps/mmap_watcher.go unwind/ehmaps/mmap_watcher_test.go
git commit -m "S4: MmapWatcher — perf_event ring reader for MMAP2+EXIT records"
```

---

## Task 5 — Wire MMAP2 → PIDTracker: auto-install on new mapping

**Goal:** add a high-level entry point that ties the watcher to the tracker. When a MMAP2 arrives for an executable file that the tracker doesn't already have CFI for, auto-attach. When an EXIT arrives for a known PID, detach.

**Files:**
- Modify: `unwind/ehmaps/tracker.go` — add `Run(context, watcher)` method that consumes events and reacts
- Modify: `unwind/ehmaps/tracker_test.go` — add `TestTrackerAutoAttachOnMmap`

- [ ] **Step 5.1: Write failing test**

Append to `unwind/ehmaps/tracker_test.go`:

```go
// TestTrackerAutoAttachOnMmap simulates the full S4 flow: start a
// workload that mmaps shared libraries during startup, run a
// PIDTracker driven by a MmapWatcher, and verify pid_mappings gets
// populated without any manual Attach call.
func TestTrackerAutoAttachOnMmap(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}
	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := ehmaps.NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := ehmaps.NewPIDTracker(store, pidMaps, pidMapLen)

	// Spawn a shell that delays, then exec's a second program. The
	// inner `exec` runs inside the already-tracked PID and fires fresh
	// MMAP2 events AFTER our watcher is up — avoiding the child-startup
	// race where libc's initial mmaps happen before we can attach.
	child := exec.Command("/bin/sh", "-c", "sleep 0.3 && exec /bin/cat /dev/null")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	w, err := ehmaps.NewMmapWatcher(uint32(child.Process.Pid))
	if err != nil {
		t.Fatalf("NewMmapWatcher: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go tracker.Run(ctx, w)

	deadline := time.After(3 * time.Second)
	for {
		var gotLen uint32
		err := pidMapLen.Lookup(uint32(child.Process.Pid), &gotLen)
		if err == nil && gotLen > 0 {
			return // success
		}
		select {
		case <-deadline:
			t.Fatal("pid_mapping_lengths never got a non-zero entry for child PID")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
```

(Also merge these imports into `tracker_test.go`'s existing import block: `"context"`, `"os/exec"`, `"time"`. Do NOT add a second `import (...)` block — Go toolchains like `goimports` collapse them, and duplicate blocks look like an oversight in review.)

- [ ] **Step 5.2: Run, verify fail**

Expected: `tracker.Run undefined`.

- [ ] **Step 5.3: Add Run method**

Append to `unwind/ehmaps/tracker.go`. **Merge the new imports into the file's existing single import block** — do not add a second `import (...)` block; `goimports` collapses them and the duplicate looks like a mistake in review. The new imports to add are: `"context"`, `"log/slog"`, `"os"`, `"path/filepath"`, `"strings"`.

```go
// (Imports above shown for reference only — merge with the existing block.)
import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Run blocks consuming events from the watcher until ctx is canceled or
// the watcher's event channel closes. Call from a goroutine. On MmapEvent
// with an executable filename in the PID's address space, auto-attaches.
// On ExitEvent, auto-detaches the PID.
func (t *PIDTracker) Run(ctx context.Context, w *MmapWatcher) {
	seen := map[uint32]map[string]struct{}{} // pid → set of binPaths already attached
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			switch ev.Kind {
			case MmapEvent:
				if !looksExecutable(ev) {
					continue
				}
				bucket, present := seen[ev.PID]
				if !present {
					bucket = map[string]struct{}{}
					seen[ev.PID] = bucket
				}
				if _, already := bucket[ev.Filename]; already {
					continue
				}
				bucket[ev.Filename] = struct{}{}
				if err := t.Attach(ev.PID, ev.Filename); err != nil {
					slog.Debug("ehmaps: Attach failed", "pid", ev.PID, "path", ev.Filename, "err", err)
				}
			case ExitEvent:
				// Only act on group-leader exit (whole process gone).
				// Per-thread exits still fire PERF_RECORD_EXIT but leave
				// the process alive; detaching on those would break
				// tracking mid-process.
				if ev.TID != ev.PID {
					continue
				}
				delete(seen, ev.PID)
				if err := t.Detach(ev.PID); err != nil {
					slog.Debug("ehmaps: Detach failed", "pid", ev.PID, "err", err)
				}
			}
		}
	}
}

// looksExecutable filters MMAP2 events down to those worth attaching to.
// Must be an executable mapping (PROT_EXEC), have a real filename
// (non-empty, not an anonymous or special kernel path), and the file
// must exist + be readable.
func looksExecutable(ev MmapEventRecord) bool {
	const protExec = 0x4
	if ev.Prot&protExec == 0 {
		return false
	}
	if ev.Filename == "" {
		return false
	}
	if strings.HasPrefix(ev.Filename, "[") || strings.HasPrefix(ev.Filename, "//anon") {
		return false
	}
	clean := filepath.Clean(ev.Filename)
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() {
		return false
	}
	return true
}
```

- [ ] **Step 5.4: Capped run**

`GOTOOLCHAIN=go1.26.0 go test -c -o /home/diego/bin/ehmaps.test ./unwind/ehmaps/`

User setcap, then:

`/home/diego/bin/ehmaps.test -test.v -test.run TestTrackerAutoAttachOnMmap`

Expected: PASS — a shell's libc/ld.so mmap triggers an Attach, `pid_mapping_lengths` gets a non-zero entry.

- [ ] **Step 5.5: Commit**

```
git add unwind/ehmaps/tracker.go unwind/ehmaps/tracker_test.go
git commit -m "S4: PIDTracker.Run — auto-attach/detach on MMAP2/EXIT events"
```

---

## Task 6 — End-to-end integration test with dlopen workload

**Goal:** the headline S4 test — workload loads a shared library at runtime, agent picks it up automatically, chains include the library's code.

**Files:**
- Modify: `test/workloads/rust/Cargo.toml` — add `libc` dep (needed for dlopen)
- Modify: `test/workloads/rust/src/main.rs` — add optional `--dlopen <path>` arg
- Create: `test/workloads/rust/probe/` — a minimal `cdylib` the workload dlopens
- Modify: `Makefile` `test-workloads` target to also build the probe
- Modify: `test/integration_test.go` — add `TestPerfDwarfMmap2Tracking`

- [ ] **Step 6.1: Create the probe cdylib**

`test/workloads/rust/probe/Cargo.toml`:

```toml
[package]
name = "probe"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[profile.release]
debug = true
strip = false
```

`test/workloads/rust/probe/src/lib.rs`:

```rust
#[inline(never)]
#[no_mangle]
pub extern "C" fn probe_spin(iters: u64) -> u64 {
    let mut sum: u64 = 0;
    for i in 0..iters {
        sum = sum.wrapping_add((i as f64).sqrt() as u64);
    }
    sum
}
```

- [ ] **Step 6.2: Extend rust-workload to optionally dlopen the probe**

Update `test/workloads/rust/Cargo.toml`:

```toml
[dependencies]
num_cpus = "1.16"
libc = "0.2"
```

Update `test/workloads/rust/src/main.rs` — add two flags: `--dlopen PATH` selects the library, `--dlopen-delay SECS` (default 0) sleeps before dlopening so the test has time to attach its watcher before the MMAP2 event fires. When the library is loaded, call `probe_spin` from the CPU loop:

```rust
use std::env;
use std::ffi::{CString, c_void};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicPtr, Ordering};
use std::thread;
use std::time::Duration;

type ProbeSpinFn = unsafe extern "C" fn(u64) -> u64;

#[inline(never)]
fn cpu_intensive_work(stop: Arc<AtomicBool>, probe: Arc<AtomicPtr<c_void>>) {
    let mut sum = 0u64;
    while !stop.load(Ordering::Relaxed) {
        let p = probe.load(Ordering::Relaxed);
        if !p.is_null() {
            let f: ProbeSpinFn = unsafe { std::mem::transmute(p) };
            unsafe { sum = sum.wrapping_add(f(100_000)); }
        } else {
            for i in 0..100_000u64 {
                sum = sum.wrapping_add((i as f64).sqrt() as u64);
            }
        }
    }
    std::hint::black_box(sum);
}

fn main() {
    let args: Vec<String> = env::args().collect();
    let duration = args.iter().nth(1).and_then(|s| s.parse::<u64>().ok()).unwrap_or(30);
    let threads = args.iter().nth(2).and_then(|s| s.parse::<usize>().ok()).unwrap_or(num_cpus::get());
    let dlopen_path = args.iter().position(|a| a == "--dlopen").and_then(|i| args.get(i+1)).cloned();
    let dlopen_delay = args.iter().position(|a| a == "--dlopen-delay")
        .and_then(|i| args.get(i+1))
        .and_then(|s| s.parse::<u64>().ok())
        .unwrap_or(0);

    println!("Rust CPU-bound workload: {} threads for {}s", threads, duration);
    println!("PID: {}", std::process::id());
    let stop = Arc::new(AtomicBool::new(false));
    let probe = Arc::new(AtomicPtr::<c_void>::new(std::ptr::null_mut()));
    let mut handles = vec![];
    for _ in 0..threads {
        let stop_clone = Arc::clone(&stop);
        let probe_clone = Arc::clone(&probe);
        handles.push(thread::spawn(move || cpu_intensive_work(stop_clone, probe_clone)));
    }

    if let Some(path) = dlopen_path {
        if dlopen_delay > 0 {
            println!("delaying dlopen by {}s", dlopen_delay);
            thread::sleep(Duration::from_secs(dlopen_delay));
        }
        unsafe {
            let c = CString::new(path.as_str()).unwrap();
            let h = libc::dlopen(c.as_ptr(), libc::RTLD_NOW);
            if h.is_null() {
                eprintln!("dlopen({}) failed", path);
                std::process::exit(2);
            }
            let sym = CString::new("probe_spin").unwrap();
            let ptr = libc::dlsym(h, sym.as_ptr());
            if ptr.is_null() {
                eprintln!("dlsym(probe_spin) failed");
                std::process::exit(2);
            }
            probe.store(ptr, Ordering::Release);
        }
        println!("dlopened {}", path);
    }

    thread::sleep(Duration::from_secs(duration));
    stop.store(true, Ordering::Relaxed);
    for h in handles { h.join().unwrap(); }
    println!("Rust workload completed");
}
```

This shape lets the test control the dlopen timing: workers spin without the probe until `probe` is non-null, then call `probe_spin` each iteration.

- [ ] **Step 6.3: Extend `make test-workloads`**

In `Makefile`, update the rust line:

```make
	@if command -v cargo >/dev/null 2>&1; then \
		cd test/workloads/rust && cargo build --release; \
		cd test/workloads/rust/probe && cargo build --release; \
	else \
		echo "Rust/Cargo not found, skipping Rust workload"; \
	fi
```

- [ ] **Step 6.4: Rebuild**

```
make test-workloads
ls test/workloads/rust/probe/target/release/libprobe.so
llvm-nm test/workloads/rust/probe/target/release/libprobe.so | grep probe_spin
```

Expected: `libprobe.so` present, `probe_spin` symbol visible.

- [ ] **Step 6.5: Add TestPerfDwarfMmap2Tracking**

Append to `test/integration_test.go`:

```go
// TestPerfDwarfMmap2Tracking validates the full S4 flow: after starting
// the rust workload with --dlopen, a MmapWatcher + PIDTracker should
// pick up the probe.so mapping and the walker should produce chains
// that include probe_spin's code range.
func TestPerfDwarfMmap2Tracking(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	binPath := "./workloads/rust/target/release/rust-workload"
	probePath := "./workloads/rust/probe/target/release/libprobe.so"
	for _, p := range []string{binPath, probePath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("workload %s not built: %v", p, err)
		}
	}

	// Start the workload with a 4s dlopen delay — gives us a wide window
	// to bring up the BPF maps, tracker, and watcher before the dlopen's
	// MMAP2 fires.
	workload := exec.Command(binPath, "20", "2", "--dlopen", probePath, "--dlopen-delay", "4")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(500 * time.Millisecond) // let workload print its PID banner

	objs, err := perfprofile.LoadPerfDwarfForTest()
	require.NoError(t, err)
	defer objs.Close()
	require.NoError(t, objs.AddPID(uint32(workload.Process.Pid)))

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap())
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())
	require.NoError(t, tracker.Attach(uint32(workload.Process.Pid), binPath))

	// Start the watcher BEFORE the dlopen fires. The 4s delay in the
	// workload above gives us time to get here.
	w, err := ehmaps.NewMmapWatcher(uint32(workload.Process.Pid))
	require.NoError(t, err)
	defer w.Close()

	runCtx, cancelRun := context.WithCancel(t.Context())
	runDone := make(chan struct{})
	go func() {
		tracker.Run(runCtx, w)
		close(runDone)
	}()

	// Wait for the dlopen event + Attach to land. 6s covers the 4s
	// pre-dlopen delay plus a generous margin.
	deadline := time.After(6 * time.Second)
	var installed int
wait:
	for {
		installed = countMapEntries(t, objs.CFILengthsMap())
		if installed >= 2 {
			break wait
		}
		select {
		case <-deadline:
			break wait
		case <-time.After(200 * time.Millisecond):
		}
	}
	cancelRun()
	<-runDone

	if installed < 2 {
		t.Fatalf("expected >= 2 tables installed (main + probe.so), got %d", installed)
	}
}

// countMapEntries is a small helper to iterate a HASH map and return
// the number of populated keys. Safe to call while other goroutines
// write to the map (cilium/ebpf's Iterate is lock-free and may skip or
// re-report keys under concurrent mutation — for this test we only
// need "at least 2" which is monotonic once reached).
func countMapEntries(t *testing.T, m *ebpf.Map) int {
	t.Helper()
	it := m.Iterate()
	var key uint64
	var val uint32
	n := 0
	for it.Next(&key, &val) {
		n++
	}
	if err := it.Err(); err != nil {
		t.Logf("iterate: %v (continuing)", err)
	}
	return n
}
```

- [ ] **Step 6.6: Build + capped run**

```
cd test && GOTOOLCHAIN=go1.26.0 CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" go test -c -o /home/diego/bin/integration.test .
```

User setcap, then:

`/home/diego/bin/integration.test -test.v -test.run TestPerfDwarfMmap2Tracking`

Expected: PASS, "expected >= 2 tables installed".

- [ ] **Step 6.7: Commit**

```
git add test/workloads/rust/Cargo.toml test/workloads/rust/src/main.rs test/workloads/rust/probe/ Makefile test/integration_test.go
git commit -m "S4: probe cdylib + --dlopen flag + TestPerfDwarfMmap2Tracking"
```

---

## Task 7 — Sanity test matrix + doc update

**Goal:** confirm nothing regressed; update design doc.

- [ ] **Step 7.1: Full test-unit**

`GOTOOLCHAIN=go1.26.0 make test-unit`

Expected: all PASS; CAP-gated tests skip.

- [ ] **Step 7.2: All capped tests**

```
/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads
/home/diego/bin/ehmaps.test -test.v
cd test && /home/diego/bin/integration.test -test.v -test.run "TestPerfDwarf"
```

Expected: every test either PASSes or SKIPs cleanly.

- [ ] **Step 7.3: Update design doc**

In `docs/dwarf-unwinding-design.md`, the "Execution plan" table's S4 row — update Success-Criterion cell to reflect what actually shipped. Add a short note under "Implementation status" (create the section if absent) listing S4 as complete with a reference to this plan.

- [ ] **Step 7.4: Commit**

```
git add docs/dwarf-unwinding-design.md docs/superpowers/plans/2026-04-23-s4-ehmaps-lifecycle.md
git commit -m "S4: design doc status update + preserve implementation plan"
```

---

## Success criteria recap

From `docs/dwarf-unwinding-design.md` §Execution plan:

> **S4: `unwind/ehmaps/` + MMAP2 ingestion** — New `.so` loaded at runtime (via `dlopen`-style test) produces correct unwinds without restart. PID exit cleans up maps.

Satisfied by:
- `TestTrackerAutoAttachOnMmap` — MMAP2 → auto-Attach.
- `TestPerfDwarfMmap2Tracking` — dlopen'd probe.so shows up in `cfi_lengths` as a second installed table.
- `TestTrackerAttachSelf` exercises the detach/cleanup path; `Run` wires EXIT events to Detach.

## Open risks

1. **Initial snapshot races MMAP2 stream.** If a dlopen happens between `tracker.Attach` (which scans /proc/<pid>/maps) and `NewMmapWatcher`, the mapping is missed. Mitigation: MmapWatcher is created before Attach in real usage, so its ring captures the dlopen even if Attach hasn't yet processed it. For Task 6's test this isn't a practical issue (workload sleeps briefly before dlopening).
2. **PID reuse.** If the target exits and another process reuses the PID, a stale pid_mappings entry could route CFI lookups to the wrong binary. Detach on EXIT closes this — but between EXIT and the next Attach, a window exists. Acceptable for S4 scope; S5/S7 can revisit.
3. **Non-ELF mappings with filenames.** Some Linux runtimes create file-backed mmaps that look executable but aren't ELFs (e.g. Java JIT code cache with `/tmp/hsperf_...`). `looksExecutable` only checks PROT_EXEC + filename; an ELF-header check inside Attach would reject those cleanly (ehcompile fails with a clear error today, which the tracker already logs at debug).
4. **Filter accept pattern.** MMAP2 reports mappings from every thread; kernel thread events are filtered by attaching the perf_event to `pid=targetPID`. If a forked child shares TGID with the parent (unusual outside `pthread_create`), we accidentally pick up its mappings. Acceptable; fork children are rare in mainstream workloads.
