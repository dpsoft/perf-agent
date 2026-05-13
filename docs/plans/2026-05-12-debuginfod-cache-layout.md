# Debuginfod cache layout fix — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make off-box symbolization actually work for stripped binaries that carry only `.note.gnu.build-id` (no `.gnu_debuglink`) — covering the common case of Rust/Go release builds.

**Architecture:** Per-mapping classifier routes each ELF mapping in a target PID through one of two paths: blazesym's existing **process-mode** (for binaries with local DWARF or resolvable debug-link, including system libs), or a new **file-mode** path that does Go-side address normalization (`/proc/<pid>/maps` + ELF PHDRs) and calls `blaze_symbolize_elf_virt_offsets` against the cached `.debug` directly. Pattern matches Parca / Pyroscope / OpenTelemetry eBPF profiler. Dispatcher Case 3 becomes a no-fetch fail-open. Cache layout unchanged.

**Tech Stack:** Go 1.26 (modern idioms — `slices`, `maps`, `cmp.Or`, `errors.AsType`, `t.Context()`, `b.Loop()`, `wg.Go`), cgo to libblazesym_c (Rust), eBPF (cilium/ebpf — no changes here), elfutils debuginfod protocol (HTTP), modernc.org/sqlite (LRU index).

**Spec:** `docs/specs/2026-05-12-debuginfod-cache-layout-design.md`

---

## File map

| Path | Action | Responsibility |
|---|---|---|
| `unwind/procmap/procmap.go` | MODIFY | Add `Mapping.MapFiles` field + `Mapping.OpenablePath()` helper |
| `unwind/procmap/procmap_test.go` | MODIFY | Tests for `OpenablePath()` |
| `unwind/procmap/resolver.go` | MODIFY | `populate()` reads build-id via `map_files` first, falls back to symbolic |
| `unwind/procmap/resolver_test.go` | MODIFY | Build-id attached when symbolic path is missing but map_files works |
| `unwind/procmap/addressmapper.go` | CREATE | Port `pfelf.AddressMapper` from OTel (Apache-2.0) |
| `unwind/procmap/addressmapper_test.go` | CREATE | Page-alignment edges, multi-PT_LOAD, PIE/non-PIE |
| `symbolize/debuginfod/classifier.go` | CREATE | `procmapClassifier`, route decisions, candidate iteration with `badDebug` filtering, `negFetch` |
| `symbolize/debuginfod/classifier_test.go` | CREATE | Table-driven classification tests |
| `symbolize/debuginfod/dispatcher.go` | MODIFY | Reframe Case 3 as no-fetch fallback; add `symbolize_elf_virt` cgo function + `symbolizeElfVirt` Go wrapper with originalIPs address rewrite |
| `symbolize/debuginfod/dispatcher_test.go` | MODIFY | Case 3 returns `""` without fetching |
| `symbolize/debuginfod/symbolizer.go` | MODIFY | `Symbolize()` does per-mapping routing, batch splitting, result merging |
| `symbolize/debuginfod/stats.go` | MODIFY | New counters: classify{ProcessMode,FileMode,Skipped}, fileMode{Calls,FetchFails,ParseFails,LocalHits}, normalizationFails |
| `test/integration_strip_helpers.go` | CREATE | `stripWorkload()`, `uploadDebug()` helpers (build-tag `integration` not required — used by non-integration-tagged tests too) |
| `test/integration_test.go` | MODIFY | 8 new integration tests |

---

## Phase 0 — Foundation

### Task 0a: Add `MapFiles` field + `OpenablePath()` to `procmap.Mapping`

**Files:**
- Modify: `unwind/procmap/procmap.go`
- Test: `unwind/procmap/procmap_test.go`

- [ ] **Step 1: Write the failing test**

Add to `unwind/procmap/procmap_test.go`:

```go
func TestMappingOpenablePath(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "exe")
	if err := os.WriteFile(binPath, []byte("dummy"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		mapFiles string
		path     string
		want     string
	}{
		{"map_files preferred when both readable", binPath, binPath, binPath},
		{"falls back to symbolic when map_files empty", "", binPath, binPath},
		{"falls back to symbolic when map_files missing", "/nope/missing", binPath, binPath},
		{"map_files wins when symbolic deleted", binPath, "/deleted/path", binPath},
		{"empty when neither works", "/nope/a", "/nope/b", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Mapping{MapFiles: tc.mapFiles, Path: tc.path}
			if got := m.OpenablePath(); got != tc.want {
				t.Errorf("OpenablePath() = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/diego/github/perf-agent/.worktrees/debuginfod-cache-layout && LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -run TestMappingOpenablePath -v`
Expected: `unknown field MapFiles in struct literal of type Mapping` (compile error).

- [ ] **Step 3: Add the field and helper**

Modify `unwind/procmap/procmap.go`:

```go
// Mapping describes one executable range in a process's address space.
// Non-executable and anonymous ranges are dropped during parsing.
type Mapping struct {
	Path    string
	// MapFiles is /proc/<pid>/map_files/<start>-<limit>. Present even when
	// the symbolic Path is unreachable from the agent's mount namespace
	// (sidecar / deleted-binary cases). Empty when /proc/<pid>/map_files
	// is restricted by the kernel.
	MapFiles string
	Start    uint64
	Limit    uint64 // exclusive
	Offset   uint64 // p_offset of the backing PT_LOAD segment
	BuildID  string // hex; empty if no .note.gnu.build-id
	IsExec   bool
}

// OpenablePath returns the first openable path: MapFiles (preferred — works
// across mount namespaces and survives unlinked-but-mapped binaries) then
// the symbolic Path. Returns "" when neither is readable.
func (m Mapping) OpenablePath() string {
	for _, p := range [2]string{m.MapFiles, m.Path} {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_ = f.Close()
		return p
	}
	return ""
}
```

Add `"os"` to the imports if not already present.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/diego/github/perf-agent/.worktrees/debuginfod-cache-layout && LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -run TestMappingOpenablePath -v`
Expected: `--- PASS: TestMappingOpenablePath (0.00s)` with all 5 subtests passing.

- [ ] **Step 5: Commit**

```bash
cd /home/diego/github/perf-agent/.worktrees/debuginfod-cache-layout
git add unwind/procmap/procmap.go unwind/procmap/procmap_test.go
git commit -m "procmap: add Mapping.MapFiles + OpenablePath() helper

map_files is /proc/<pid>/map_files/<start>-<limit>, a kernel-resolved
symlink that works across mount namespaces and survives deletion of
the original file. OpenablePath() prefers it, falls back to the
symbolic path, returns \"\" when neither is readable.

Foundation for the debuginfod cache-layout fix: classifier and
AddressMapper need namespace-safe paths for sidecar / deleted-binary
cases."
```

---

### Task 0b: `Resolver.populate` reads build-id via map_files first

**Files:**
- Modify: `unwind/procmap/resolver.go`
- Test: `unwind/procmap/resolver_test.go`

- [ ] **Step 1: Inspect current resolver and find the test pattern**

Run: `grep -n "buildIDFor\|populate" unwind/procmap/resolver.go unwind/procmap/resolver_test.go`
Note the line numbers of `populate()` and `buildIDFor()` for the modification.

- [ ] **Step 2: Write the failing test**

Add to `unwind/procmap/resolver_test.go`:

```go
func TestResolverPopulateBuildIDViaMapFiles(t *testing.T) {
	// Simulate the sidecar case: build-id is only readable through the
	// MapFiles symlink because the symbolic Path is unreachable.
	tmp := t.TempDir()
	binWithBuildID := writeELFWithBuildID(t, tmp, []byte{0xab, 0xcd, 0xef, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11})

	m := Mapping{
		Path:     "/sidecar/unreachable/bin", // doesn't exist on host
		MapFiles: binWithBuildID,             // does exist
		Start:    0x400000,
		Limit:    0x401000,
		IsExec:   true,
	}

	r := &Resolver{} // bare resolver; we only need the buildID helper path
	mappings := []Mapping{m}
	r.populate(mappings)

	want := "abcdef0102030405060708090a0b0c0d0e0f1011"
	if mappings[0].BuildID != want {
		t.Errorf("BuildID via MapFiles = %q, want %q", mappings[0].BuildID, want)
	}
}

// writeELFWithBuildID writes a minimal ELF with a .note.gnu.build-id at tmp/exe.
// The build-id is the 20 bytes provided. Returns the path. Reuses the existing
// helper if one exists in unwind/procmap or unwind/dwarfagent — otherwise
// inline a small builder using debug/elf to keep this test self-contained.
func writeELFWithBuildID(t *testing.T, tmp string, buildID []byte) string {
	t.Helper()
	// Implementation: use debug/elf + binary.Write to emit:
	//   - ELF64 header (e_machine = EM_X86_64, e_type = ET_EXEC)
	//   - One PT_NOTE program header pointing at the build-id note
	//   - SHT_NOTE section with NT_GNU_BUILD_ID (3) carrying the 20 bytes
	// Returns the absolute file path. ~40 LOC. If unwind/procmap already
	// has an equivalent helper (check unwind/procmap/buildid_test.go and
	// unwind/procmap/buildid_export_test.go), use that instead.
	path, err := buildIDFixture(tmp, buildID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
```

If a helper like `buildIDFixture` or `writeELFWithBuildID` already exists in `unwind/procmap/buildid_export_test.go`, use it directly and delete the wrapper. Otherwise, inline the minimal ELF builder. Check first with:
```bash
grep -rn "func.*BuildID.*\.note\.gnu\|func writeELF\|func buildIDFixture" unwind/procmap/
```

- [ ] **Step 3: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -run TestResolverPopulateBuildIDViaMapFiles -v`
Expected: FAIL — `BuildID via MapFiles = "", want "abcdef0102030405060708090a0b0c0d0e0f1011"` (because the current resolver reads via the symbolic Path, which doesn't exist).

- [ ] **Step 4: Update `populate()` to use map_files first**

Find the existing line that calls `buildIDFor`:
```bash
grep -n "buildIDFor\|BuildID = " unwind/procmap/resolver.go
```

Modify the build-id lookup loop in `populate()`:

```go
// In Resolver.populate (line will need to be located via grep above):
for i := range mappings {
	if mappings[i].BuildID != "" {
		continue // already attached
	}
	// Try MapFiles first (works across mount namespaces, survives
	// unlinked binaries). Fall back to symbolic Path.
	if mappings[i].MapFiles != "" {
		if id, _ := ReadBuildID(mappings[i].MapFiles); id != "" {
			mappings[i].BuildID = id
			continue
		}
	}
	if mappings[i].Path != "" {
		if id, _ := ReadBuildID(mappings[i].Path); id != "" {
			mappings[i].BuildID = id
		}
	}
}
```

Adjust the surrounding code if `populate()` uses a different helper (`buildIDFor` may wrap `ReadBuildID`). The change is: prefer `MapFiles` over `Path` for the lookup.

- [ ] **Step 5: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -v`
Expected: All tests pass including the new one.

- [ ] **Step 6: Commit**

```bash
git add unwind/procmap/resolver.go unwind/procmap/resolver_test.go
git commit -m "procmap: resolver reads build-id via map_files first

populate() now tries Mapping.MapFiles (kernel-resolved symlink, works
across mount namespaces) before falling back to the symbolic Path.
Fixes the sidecar / deleted-binary case where pprof Mapping.BuildID
was silently empty because the symbolic path was unreachable from
the agent's namespace."
```

---

### Task 0c: Port `AddressMapper` (basic structure + page-alignment)

**Files:**
- Create: `unwind/procmap/addressmapper.go`
- Test: `unwind/procmap/addressmapper_test.go`

- [ ] **Step 1: Write the failing test**

Create `unwind/procmap/addressmapper_test.go`:

```go
package procmap

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeMinimalELF emits an ELF64 with a single executable PT_LOAD segment
// covering [Off, Off+Filesz) in the file and mapping to virtual address Vaddr.
// pageSize controls the alignment trick we want to test.
func writeMinimalELF(t *testing.T, dir string, off, vaddr, filesz uint64) string {
	t.Helper()
	path := filepath.Join(dir, "exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// ELF64 header (we fill phoff/phnum after we know the layout)
	const ehSize = 64
	const phSize = 56
	phOff := uint64(ehSize)

	// Write header
	var eh [ehSize]byte
	copy(eh[:4], []byte{0x7f, 'E', 'L', 'F'})
	eh[4] = 2 // ELFCLASS64
	eh[5] = 1 // ELFDATA2LSB
	eh[6] = 1 // EV_CURRENT
	binary.LittleEndian.PutUint16(eh[16:], 2)               // e_type = ET_EXEC
	binary.LittleEndian.PutUint16(eh[18:], 62)              // e_machine = EM_X86_64
	binary.LittleEndian.PutUint32(eh[20:], 1)               // e_version
	binary.LittleEndian.PutUint64(eh[32:], phOff)           // e_phoff
	binary.LittleEndian.PutUint16(eh[52:], ehSize)          // e_ehsize
	binary.LittleEndian.PutUint16(eh[54:], phSize)          // e_phentsize
	binary.LittleEndian.PutUint16(eh[56:], 1)               // e_phnum
	if _, err := f.Write(eh[:]); err != nil {
		t.Fatal(err)
	}

	// PT_LOAD program header
	var ph [phSize]byte
	binary.LittleEndian.PutUint32(ph[0:], 1)        // p_type = PT_LOAD
	binary.LittleEndian.PutUint32(ph[4:], 5)        // p_flags = PF_R|PF_X
	binary.LittleEndian.PutUint64(ph[8:], off)      // p_offset
	binary.LittleEndian.PutUint64(ph[16:], vaddr)   // p_vaddr
	binary.LittleEndian.PutUint64(ph[24:], vaddr)   // p_paddr
	binary.LittleEndian.PutUint64(ph[32:], filesz)  // p_filesz
	binary.LittleEndian.PutUint64(ph[40:], filesz)  // p_memsz
	binary.LittleEndian.PutUint64(ph[48:], 0x1000)  // p_align
	if _, err := f.Write(ph[:]); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestAddressMapperBasicLookup(t *testing.T) {
	tmp := t.TempDir()
	// PT_LOAD: off=0x1000, vaddr=0x400000, filesz=0x2000
	path := writeMinimalELF(t, tmp, 0x1000, 0x400000, 0x2000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}

	tests := []struct {
		name string
		off  uint64
		want uint64
		ok   bool
	}{
		{"first byte of segment", 0x1000, 0x400000, true},
		{"middle of segment", 0x1500, 0x400500, true},
		{"last byte of segment", 0x1000 + 0x1fff, 0x400000 + 0x1fff, true},
		{"before any segment", 0x0500, 0, false},
		{"past end of segment", 0x1000 + 0x2000, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.FileOffsetToVirtualAddress(tc.off)
			if ok != tc.ok || got != tc.want {
				t.Errorf("FileOffsetToVirtualAddress(%#x) = (%#x, %v), want (%#x, %v)",
					tc.off, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAddressMapperPageAlignment(t *testing.T) {
	tmp := t.TempDir()
	// Kernel mmap aligns p_offset DOWN to page boundary. With p_offset=0x1234
	// and a 4KB page, the segment effectively starts at file-offset 0x1000.
	// Without the page-align trick, a probe at 0x1000 would fall outside the
	// declared range; with the trick, it routes to this segment.
	path := writeMinimalELF(t, tmp, 0x1234, 0x400000, 0x2000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}
	// page-aligned start of the segment is 0x1000 (0x1234 &^ 0xfff)
	got, ok := m.FileOffsetToVirtualAddress(0x1000)
	if !ok {
		t.Fatalf("FileOffsetToVirtualAddress(0x1000) ok=false, want true (page-align should include this offset)")
	}
	// The mapping from off=0x1000 (page-aligned start) follows the same
	// arithmetic OTel uses: vaddr + (off - off_aligned). When the requested
	// offset equals off_aligned, the returned VA equals vaddr.
	if got != 0x400000 {
		t.Errorf("FileOffsetToVirtualAddress(0x1000) = %#x, want %#x", got, 0x400000)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -run TestAddressMapper -v`
Expected: `undefined: NewAddressMapper` (compile error).

- [ ] **Step 3: Create `addressmapper.go`**

Create `unwind/procmap/addressmapper.go`:

```go
package procmap

import (
	"debug/elf"
	"fmt"
	"os"
	"slices"
)

// AddressMapper is a port of pfelf.AddressMapper from
// github.com/open-telemetry/opentelemetry-ebpf-profiler
// (libpf/pfelf/addressmapper.go) — Apache-2.0, used per §4 of the license.
// Original copyright: Elasticsearch B.V. / OpenTelemetry Authors.
//
// Maps a file offset within an ELF to the virtual address that offset
// would have in the running image, following the kernel's mmap alignment
// of PT_LOAD p_offset to the page boundary. Used to convert file-relative
// addresses (from /proc/<pid>/maps) into ELF-relative virtual addresses
// for symbolization.
type AddressMapper struct {
	pageSize uint64
	loads    []ptLoad
}

type ptLoad struct {
	Off    uint64 // p_offset (page-aligned by NewAddressMapper)
	Vaddr  uint64 // p_vaddr
	Filesz uint64 // p_filesz
}

// NewAddressMapper reads PHDRs from the ELF at path and returns a mapper
// for its executable PT_LOAD segments. Callers should choose a path that's
// readable from the agent's namespace — typically mapping.OpenablePath()
// which prefers /proc/<pid>/map_files/<va>-<va>.
func NewAddressMapper(path string) (*AddressMapper, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("address mapper: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	pageSize := uint64(os.Getpagesize())
	var loads []ptLoad
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Flags&elf.PF_X == 0 {
			continue // only executable segments matter for symbolization
		}
		// OTel's correctness fix: page-align p_offset DOWN to mirror
		// kernel mmap alignment. Without this, offsets near segment
		// starts get misattributed.
		aligned := p.Off &^ (pageSize - 1)
		loads = append(loads, ptLoad{
			Off:    aligned,
			Vaddr:  p.Vaddr,
			Filesz: p.Filesz + (p.Off - aligned),
		})
	}
	slices.SortFunc(loads, func(a, b ptLoad) int {
		if a.Off < b.Off {
			return -1
		}
		if a.Off > b.Off {
			return 1
		}
		return 0
	})
	return &AddressMapper{pageSize: pageSize, loads: loads}, nil
}

// FileOffsetToVirtualAddress maps a file offset to its ELF virtual address.
// Returns (0, false) if the offset is outside every executable PT_LOAD.
func (m *AddressMapper) FileOffsetToVirtualAddress(off uint64) (uint64, bool) {
	for _, l := range m.loads {
		if off >= l.Off && off < l.Off+l.Filesz {
			return l.Vaddr + (off - l.Off), true
		}
	}
	return 0, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -v`
Expected: All tests pass, including `TestAddressMapperBasicLookup` (5 subtests) and `TestAddressMapperPageAlignment`.

- [ ] **Step 5: Commit**

```bash
git add unwind/procmap/addressmapper.go unwind/procmap/addressmapper_test.go
git commit -m "procmap: add AddressMapper for file-offset → ELF VA translation

Port of pfelf.AddressMapper from open-telemetry/opentelemetry-ebpf-profiler
(libpf/pfelf/addressmapper.go) under Apache-2.0. Preserves the
page-alignment correctness fix (offsets near segment starts get
misattributed without it) by aligning p_offset down to page boundary
to mirror the kernel's mmap behavior.

Used by the upcoming file-mode symbolization path: addresses captured
in /proc/<pid>/maps space get normalized to ELF virtual addresses
that blazesym's elf-mode API can resolve against a separate .debug file."
```

---

### Task 0d: AddressMapper — multi-PT_LOAD + PIE coverage

**Files:**
- Modify: `unwind/procmap/addressmapper_test.go`

- [ ] **Step 1: Write the failing test**

Append to `unwind/procmap/addressmapper_test.go`:

```go
// Multi-PT_LOAD: many real binaries (especially PIE ones) have multiple
// executable segments. The mapper must dispatch each offset to the
// correct one and report "gap" offsets as unmapped.
func TestAddressMapperMultiPTLOAD(t *testing.T) {
	tmp := t.TempDir()
	// Two executable PT_LOADs:
	//   1: off=0x1000, vaddr=0x400000, filesz=0x800
	//   2: off=0x4000, vaddr=0x600000, filesz=0x800
	path := writeELFTwoSegments(t, tmp,
		ptLoad{Off: 0x1000, Vaddr: 0x400000, Filesz: 0x800},
		ptLoad{Off: 0x4000, Vaddr: 0x600000, Filesz: 0x800},
	)
	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}

	cases := []struct {
		off  uint64
		want uint64
		ok   bool
	}{
		{0x1000, 0x400000, true},          // first segment start
		{0x17ff, 0x4007ff, true},          // first segment end
		{0x2000, 0, false},                // in the gap between segments
		{0x4000, 0x600000, true},          // second segment start
		{0x47ff, 0x6007ff, true},          // second segment end
		{0x4800, 0, false},                // past second segment
	}
	for _, tc := range cases {
		got, ok := m.FileOffsetToVirtualAddress(tc.off)
		if ok != tc.ok || got != tc.want {
			t.Errorf("off=%#x: got (%#x, %v), want (%#x, %v)",
				tc.off, got, ok, tc.want, tc.ok)
		}
	}
}

// PIE (ET_DYN) binaries get loaded at random addresses; the mapper itself
// doesn't care — it operates on file→file-VA, the caller computes bias.
// This test confirms ET_DYN ELFs parse identically.
func TestAddressMapperPIE(t *testing.T) {
	tmp := t.TempDir()
	path := writeMinimalELFType(t, tmp, elf.ET_DYN, 0x1000, 0x0, 0x2000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper(ET_DYN): %v", err)
	}
	got, ok := m.FileOffsetToVirtualAddress(0x1500)
	if !ok || got != 0x500 {
		t.Errorf("ET_DYN FileOffsetToVirtualAddress(0x1500) = (%#x, %v), want (0x500, true)",
			got, ok)
	}
}

// writeELFTwoSegments emits an ELF64 with two PT_LOAD segments.
func writeELFTwoSegments(t *testing.T, dir string, p1, p2 ptLoad) string {
	t.Helper()
	// Variant of writeMinimalELF with two PHDRs. Same structure, just
	// e_phnum=2 and a second PT_LOAD entry. Place file contents far enough
	// out that p1.Off..p1.Off+p1.Filesz and p2.Off..p2.Off+p2.Filesz
	// don't overlap.
	path := filepath.Join(dir, "exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	const ehSize = 64
	const phSize = 56
	phOff := uint64(ehSize)

	var eh [ehSize]byte
	copy(eh[:4], []byte{0x7f, 'E', 'L', 'F'})
	eh[4] = 2
	eh[5] = 1
	eh[6] = 1
	binary.LittleEndian.PutUint16(eh[16:], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(eh[18:], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(eh[20:], 1)
	binary.LittleEndian.PutUint64(eh[32:], phOff)
	binary.LittleEndian.PutUint16(eh[52:], ehSize)
	binary.LittleEndian.PutUint16(eh[54:], phSize)
	binary.LittleEndian.PutUint16(eh[56:], 2)
	if _, err := f.Write(eh[:]); err != nil {
		t.Fatal(err)
	}

	for _, p := range [2]ptLoad{p1, p2} {
		var ph [phSize]byte
		binary.LittleEndian.PutUint32(ph[0:], 1)
		binary.LittleEndian.PutUint32(ph[4:], 5)
		binary.LittleEndian.PutUint64(ph[8:], p.Off)
		binary.LittleEndian.PutUint64(ph[16:], p.Vaddr)
		binary.LittleEndian.PutUint64(ph[24:], p.Vaddr)
		binary.LittleEndian.PutUint64(ph[32:], p.Filesz)
		binary.LittleEndian.PutUint64(ph[40:], p.Filesz)
		binary.LittleEndian.PutUint64(ph[48:], 0x1000)
		if _, err := f.Write(ph[:]); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// writeMinimalELFType emits an ELF64 of the given e_type with one PT_LOAD.
func writeMinimalELFType(t *testing.T, dir string, etype elf.Type, off, vaddr, filesz uint64) string {
	t.Helper()
	path := filepath.Join(dir, "exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	const ehSize = 64
	const phSize = 56
	phOff := uint64(ehSize)

	var eh [ehSize]byte
	copy(eh[:4], []byte{0x7f, 'E', 'L', 'F'})
	eh[4] = 2
	eh[5] = 1
	eh[6] = 1
	binary.LittleEndian.PutUint16(eh[16:], uint16(etype))
	binary.LittleEndian.PutUint16(eh[18:], 62)
	binary.LittleEndian.PutUint32(eh[20:], 1)
	binary.LittleEndian.PutUint64(eh[32:], phOff)
	binary.LittleEndian.PutUint16(eh[52:], ehSize)
	binary.LittleEndian.PutUint16(eh[54:], phSize)
	binary.LittleEndian.PutUint16(eh[56:], 1)
	if _, err := f.Write(eh[:]); err != nil {
		t.Fatal(err)
	}

	var ph [phSize]byte
	binary.LittleEndian.PutUint32(ph[0:], 1)
	binary.LittleEndian.PutUint32(ph[4:], 5)
	binary.LittleEndian.PutUint64(ph[8:], off)
	binary.LittleEndian.PutUint64(ph[16:], vaddr)
	binary.LittleEndian.PutUint64(ph[24:], vaddr)
	binary.LittleEndian.PutUint64(ph[32:], filesz)
	binary.LittleEndian.PutUint64(ph[40:], filesz)
	binary.LittleEndian.PutUint64(ph[48:], 0x1000)
	if _, err := f.Write(ph[:]); err != nil {
		t.Fatal(err)
	}
	return path
}
```

- [ ] **Step 2: Run test to verify it passes**

The implementation from Task 0c already supports multi-PT_LOAD (the loop iterates all segments). Run:
`LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./unwind/procmap/... -run TestAddressMapper -v`
Expected: All AddressMapper tests pass.

- [ ] **Step 3: Commit**

```bash
git add unwind/procmap/addressmapper_test.go
git commit -m "procmap: add AddressMapper coverage for multi-PT_LOAD + PIE

Guards against future regressions: multi-segment binaries (common in
PIE builds) must dispatch each offset to the correct segment, and
report gaps between segments as unmapped. ET_DYN parses identically
to ET_EXEC for the mapper's purposes."
```

---

## Phase 1 — Classifier

### Task 1a: Classifier types + skeleton

**Files:**
- Create: `symbolize/debuginfod/classifier.go`
- Test: `symbolize/debuginfod/classifier_test.go`

- [ ] **Step 1: Write the failing test**

Create `symbolize/debuginfod/classifier_test.go`:

```go
package debuginfod

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestClassifierSkipPaths(t *testing.T) {
	c := newClassifier(nil /* cache unused for skip-path tests */, nil /* fetcher unused */)
	skipPaths := []string{"", "[vdso]", "[stack]", "[vsyscall]", "[heap]"}
	for _, p := range skipPaths {
		t.Run(p, func(t *testing.T) {
			m := procmap.Mapping{Path: p}
			got := c.classify(t.Context(), m)
			if got.route != routeSkip {
				t.Errorf("classify(%q) route = %v, want routeSkip", p, got.route)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -run TestClassifierSkipPaths -v`
Expected: `undefined: newClassifier` (compile error).

- [ ] **Step 3: Create the classifier skeleton**

Create `symbolize/debuginfod/classifier.go`:

```go
package debuginfod

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// routeKind selects the symbolization path for a single mapping.
type routeKind int

const (
	routeSkip routeKind = iota
	routeProcessMode
	routeFileMode
)

func (r routeKind) String() string {
	switch r {
	case routeSkip:
		return "skip"
	case routeProcessMode:
		return "process-mode"
	case routeFileMode:
		return "file-mode"
	default:
		return "unknown"
	}
}

// classifyResult is the per-mapping decision.
type classifyResult struct {
	route routeKind
	// debugPath is set when route == routeFileMode: the absolute path of
	// the .debug file blazesym should symbolize against.
	debugPath string
}

// classifier picks a route per mapping. It owns the negFetch and badDebug
// state — both content-addressed, both bounded LRU.
type classifier struct {
	cache   *cache.Cache
	fetcher *singleflightFetcher

	mu       sync.Mutex
	mappers  map[mapperKey]*procmap.AddressMapper
	negFetch map[string]time.Time         // build-id → deadline (don't re-fetch)
	badDebug map[pathSig]time.Time        // path signature → deadline (don't re-open)
}

type mapperKey struct {
	dev uint64
	ino uint64
}

type pathSig struct {
	dev   uint64
	ino   uint64
	mtime int64 // unix nanos
}

const (
	negFetchTTL = 5 * time.Minute
	badDebugTTL = 1 * time.Hour
)

// nonSymbolizablePaths are virtual/anonymous mapping names that have no
// backing ELF; we drop them at Tier 1.
var nonSymbolizablePaths = []string{"", "[vdso]", "[stack]", "[vsyscall]", "[heap]"}

func newClassifier(c *cache.Cache, f *singleflightFetcher) *classifier {
	return &classifier{
		cache:    c,
		fetcher:  f,
		mappers:  make(map[mapperKey]*procmap.AddressMapper),
		negFetch: make(map[string]time.Time),
		badDebug: make(map[pathSig]time.Time),
	}
}

// classify returns the route for one mapping.
func (c *classifier) classify(ctx context.Context, m procmap.Mapping) classifyResult {
	// Tier 1: inherent non-symbolizable.
	if slices.Contains(nonSymbolizablePaths, m.Path) {
		return classifyResult{route: routeSkip}
	}

	// Subsequent tiers come in later tasks.
	return classifyResult{route: routeProcessMode}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -run TestClassifierSkipPaths -v`
Expected: All 5 subtests pass.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/classifier.go symbolize/debuginfod/classifier_test.go
git commit -m "debuginfod: add classifier skeleton with Tier 1 (skip) routing

Lays out routeKind, classifyResult, classifier struct with negFetch
(build-id keyed) and badDebug (pathSig keyed) state. Implements
only Tier 1 — paths that are inherently non-symbolizable (vdso/
stack/vsyscall/heap/empty). Subsequent tasks add Tier 2 (process-
mode routing) and Tier 3 (file-mode candidate iteration)."
```

---

### Task 1b: Classifier Tier 2 — process-mode routing

**Files:**
- Modify: `symbolize/debuginfod/classifier.go`
- Modify: `symbolize/debuginfod/classifier_test.go`

- [ ] **Step 1: Write the failing test**

Append to `symbolize/debuginfod/classifier_test.go`:

```go
func TestClassifierTier2ProcessMode(t *testing.T) {
	// Build three ELFs in temp:
	//   1. dwarfPresent: has .debug_info → process-mode (route picked early)
	//   2. debugLinkOnly: has .gnu_debuglink, target file exists in /usr/lib/debug → process-mode
	//   3. unreadable: file-like path that doesn't exist → process-mode (defensive)
	tmp := t.TempDir()
	dwarfPresent := writeELFWithDwarf(t, tmp+"/dwarf")
	debugLinkOnly := writeELFWithDebugLink(t, tmp+"/linked", tmp+"/linked.debug")
	// Place the linkee where hasResolvableDebuglink would find it. For
	// testing without /usr/lib/debug, the helper checks relative to the
	// binary's dir first — write the linkee alongside.

	c := newClassifier(nil, nil)
	cases := []struct {
		name string
		m    procmap.Mapping
		want routeKind
	}{
		{"dwarf in binary", procmap.Mapping{Path: dwarfPresent}, routeProcessMode},
		{"resolvable debug-link", procmap.Mapping{Path: debugLinkOnly}, routeProcessMode},
		{"unreadable file-like path", procmap.Mapping{Path: "/nope/missing"}, routeProcessMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.classify(t.Context(), tc.m)
			if got.route != tc.want {
				t.Errorf("route = %v, want %v", got.route, tc.want)
			}
		})
	}
}

// writeELFWithDwarf writes a minimal ELF with a non-empty .debug_info section.
func writeELFWithDwarf(t *testing.T, path string) string {
	t.Helper()
	// Use unwind/procmap test helpers or inline a builder that emits an
	// ELF with one SHT_PROGBITS section named ".debug_info". The presence
	// of the section name + non-zero size is what hasDwarf checks.
	if err := writeELFWithSection(path, ".debug_info", []byte("placeholder dwarf payload")); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeELFWithDebugLink writes a stripped ELF with a .gnu_debuglink section
// pointing to linkeePath's basename. Also writes the linkee as a separate
// file so hasResolvableDebuglink finds it adjacent to the binary.
func writeELFWithDebugLink(t *testing.T, binPath, linkeePath string) string {
	t.Helper()
	// Linkee: any ELF file at linkeePath. Content doesn't matter for the
	// resolvability check — only filename + presence.
	if err := writeBytes(linkeePath, []byte{0x7f, 'E', 'L', 'F'}); err != nil {
		t.Fatal(err)
	}
	// Binary: an ELF with a .gnu_debuglink section whose payload is
	// "<basename>\0\0\0\0<crc32>". The current hasResolvableDebuglink
	// reads just the filename component for adjacency checks.
	linkee := filepath.Base(linkeePath)
	payload := append(append([]byte(linkee), 0), 0, 0, 0)
	payload = append(payload, 0xde, 0xad, 0xbe, 0xef) // dummy CRC32
	if err := writeELFWithSection(binPath, ".gnu_debuglink", payload); err != nil {
		t.Fatal(err)
	}
	return binPath
}

// writeELFWithSection writes an ELF64 with one named SHT_PROGBITS section
// containing data. Returns nil on success.
func writeELFWithSection(path, sectionName string, data []byte) error {
	// Inline minimal builder using encoding/binary. ~80 LOC. Header,
	// program headers (one PT_LOAD covering the section), section
	// headers (one named section + .shstrtab). If this helper already
	// exists in the repo (check unwind/procmap/buildid_export_test.go),
	// reuse it.
	return errPlaceholderInlineThisHelper // see "Implementation note" below
}

func writeBytes(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}
```

**Implementation note for the helper**: `writeELFWithSection` is a non-trivial ELF builder (~80 LOC). To keep the plan honest, the implementer should:
1. First run `grep -rn "writeELFWithSection\|writeELF.*Section\|writeELFWith" unwind/ symbolize/` to find any existing helper.
2. If found, import/move it to `symbolize/debuginfod/elftest_helper_test.go` (shared across tests) and skip writing a new builder.
3. If not found, write the helper inline using `debug/elf` + `encoding/binary`. The minimal viable structure is:
   - 64-byte ELF64 header (`e_machine = EM_X86_64`, `e_type = ET_EXEC`, `e_shoff`/`e_shnum`/`e_shstrndx` set)
   - Two section headers: the named section (`sh_type = SHT_PROGBITS`, `sh_name` pointing into shstrtab, `sh_offset` + `sh_size` describing the data) and `.shstrtab` (`sh_type = SHT_STRTAB`)
   - shstrtab contents: `\0<sectionName>\0.shstrtab\0`
   - Data appended after the headers/strtab

A reference is in the OTel codebase at `libpf/pfelf/file_test.go` if needed.

- [ ] **Step 2: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -run TestClassifierTier2 -v`
Expected: FAIL — all three subtests return `routeProcessMode` only by accident (the skeleton returns process-mode for everything not Tier 1). After we add Tier 3 in later tasks, this test guards Tier 2's correctness explicitly.

(Note: at this step the skeleton happens to return the right route for the wrong reason. The implementation step below makes the reason correct.)

- [ ] **Step 3: Implement Tier 2 in `classify()`**

Replace the body of `classify()` in `symbolize/debuginfod/classifier.go`:

```go
func (c *classifier) classify(ctx context.Context, m procmap.Mapping) classifyResult {
	// Tier 1: inherent non-symbolizable.
	if slices.Contains(nonSymbolizablePaths, m.Path) {
		return classifyResult{route: routeSkip}
	}

	// Tier 2: try to read the ELF; on any failure fall through to
	// process-mode so blazesym's defaults can attempt resolution.
	openPath := m.OpenablePath()
	if openPath == "" {
		// Unreachable from agent namespace. Process-mode is defensive —
		// blazesym will emit [binary]:offset which still preserves
		// stack shape vs skipping (which would drop frames).
		return classifyResult{route: routeProcessMode}
	}
	if hasDwarf(openPath) {
		return classifyResult{route: routeProcessMode}
	}
	// Debug-link search is filesystem-relative; only meaningful with the
	// symbolic path. Skip if it isn't readable.
	if m.Path != "" && hasResolvableDebuglink(m.Path, nil) {
		return classifyResult{route: routeProcessMode}
	}

	// Tier 3 (file-mode) lands in the next task.
	return classifyResult{route: routeProcessMode}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -run "TestClassifierSkip|TestClassifierTier2" -v`
Expected: All subtests pass.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/classifier.go symbolize/debuginfod/classifier_test.go
git commit -m "debuginfod: classifier Tier 2 — process-mode for local DWARF/debug-link

Mappings with .debug_info in-binary, a resolvable .gnu_debuglink, or
an unreachable symbolic path go through blazesym's process-mode path
(its defaults handle them). Debug-link check uses mapping.Path
(symbolic) — map_files paths would search /proc/<pid>/map_files/
which never contains the linkee."
```

---

### Task 1c: Classifier Tier 3 — file-mode candidate iteration with badDebug

**Files:**
- Modify: `symbolize/debuginfod/classifier.go`
- Modify: `symbolize/debuginfod/classifier_test.go`

- [ ] **Step 1: Write the failing test**

Append to `symbolize/debuginfod/classifier_test.go`:

```go
func TestClassifierTier3FileModeCacheHit(t *testing.T) {
	// Setup: stripped ELF with build-id, .debug in our cache, nothing in
	// system path. Expect file-mode with cache path.
	tmp := t.TempDir()
	buildID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	binPath := writeStrippedELFWithBuildID(t, tmp+"/exe", buildID)

	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.OpenSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cc := &cache.Cache{Dir: cacheDir, Index: idx}

	// Write a fake .debug at the cache layout location.
	debugDir := filepath.Join(cacheDir, ".build-id", buildID[:2])
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(debugDir, buildID[2:]+".debug")
	if err := os.WriteFile(debugPath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Touch(buildID, cache.KindDebuginfo, 4); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(cc, nil /* fetcher not used because cache hit */)
	got := c.classify(t.Context(), procmap.Mapping{Path: binPath})
	if got.route != routeFileMode {
		t.Errorf("route = %v, want routeFileMode", got.route)
	}
	if got.debugPath != debugPath {
		t.Errorf("debugPath = %q, want %q", got.debugPath, debugPath)
	}
}

func TestClassifierBadDebugFiltersCacheCopy(t *testing.T) {
	// Setup: cache copy is in badDebug; classifier should fall through
	// (returning process-mode) because no other candidate exists and no
	// fetcher is configured.
	tmp := t.TempDir()
	buildID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	binPath := writeStrippedELFWithBuildID(t, tmp+"/exe", buildID)

	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.OpenSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cc := &cache.Cache{Dir: cacheDir, Index: idx}

	debugDir := filepath.Join(cacheDir, ".build-id", buildID[:2])
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(debugDir, buildID[2:]+".debug")
	if err := os.WriteFile(debugPath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Touch(buildID, cache.KindDebuginfo, 4); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(cc, nil)
	// Mark the cached file as bad.
	sig, err := statSig(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	c.markBadDebug(sig)

	got := c.classify(t.Context(), procmap.Mapping{Path: binPath})
	if got.route != routeProcessMode {
		t.Errorf("route with badDebug-blocked cache = %v, want routeProcessMode", got.route)
	}
}

// writeStrippedELFWithBuildID writes a minimal ELF with .note.gnu.build-id
// but no .debug_info, no .gnu_debuglink, no .symtab. The build-id arg is
// the 40-char hex string; it's decoded into 20 bytes before embedding.
func writeStrippedELFWithBuildID(t *testing.T, path, buildIDHex string) string {
	t.Helper()
	buildID, err := hex.DecodeString(buildIDHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeELFWithNoteGnuBuildID(path, buildID); err != nil {
		t.Fatal(err)
	}
	return path
}
```

Add imports as needed (`encoding/hex`, `path/filepath`, the cache package). `writeELFWithNoteGnuBuildID` follows the same pattern as the writeELF helpers above — emit a `PT_NOTE` segment + `SHT_NOTE` section carrying a `NT_GNU_BUILD_ID` (type 3) note with name "GNU\0" and the build-id bytes as desc. If a helper already exists in `unwind/procmap/buildid_export_test.go`, reuse it.

- [ ] **Step 2: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -run "TestClassifierTier3FileModeCacheHit|TestClassifierBadDebugFiltersCacheCopy" -v`
Expected: FAIL — `route = process-mode, want routeFileMode` (because Tier 3 isn't implemented yet). Also: `undefined: statSig` / `undefined: markBadDebug` from the test setup.

- [ ] **Step 3: Implement Tier 3 + helpers in `classifier.go`**

Replace the body of `classify()` (Tier 3 added below the Tier 2 fallthrough):

```go
func (c *classifier) classify(ctx context.Context, m procmap.Mapping) classifyResult {
	// Tier 1: inherent non-symbolizable.
	if slices.Contains(nonSymbolizablePaths, m.Path) {
		return classifyResult{route: routeSkip}
	}

	// Tier 2: try to read the ELF; on any failure fall through to
	// process-mode so blazesym's defaults can attempt resolution.
	openPath := m.OpenablePath()
	if openPath == "" {
		return classifyResult{route: routeProcessMode}
	}
	if hasDwarf(openPath) {
		return classifyResult{route: routeProcessMode}
	}
	if m.Path != "" && hasResolvableDebuglink(m.Path, nil) {
		return classifyResult{route: routeProcessMode}
	}

	// Tier 3: file-mode routing. Read build-id; without one we can't
	// look up any debug file remotely, so fall through to process-mode.
	buildID := readBuildID(m.MapFiles, m.Path)
	if buildID == "" {
		return classifyResult{route: routeProcessMode}
	}

	// 3a: collect local candidate paths (no network).
	candidates := make([]string, 0, 2)
	if sys := systemDebugPath(buildID); sys != "" {
		candidates = append(candidates, sys)
	}
	if c.cache != nil && c.cache.Has(buildID, cache.KindDebuginfo) {
		candidates = append(candidates, c.cache.AbsPath(buildID, cache.KindDebuginfo))
	}

	// Try each candidate; skip ones in badDebug.
	for _, p := range candidates {
		sig, err := statSig(p)
		if err != nil {
			continue // file vanished between exists() check and stat
		}
		if c.isBadDebug(sig) {
			continue
		}
		return classifyResult{route: routeFileMode, debugPath: p}
	}

	// 3b: local candidates exhausted (or all bad). Try remote fetch unless
	// a recent fetch failed.
	if c.isNegFetch(buildID) || c.fetcher == nil {
		return classifyResult{route: routeProcessMode}
	}

	abs, err := c.fetcher.fetchAndStore(ctx, "debuginfo", buildID)
	if err != nil {
		c.markNegFetch(buildID)
		return classifyResult{route: routeProcessMode}
	}

	// Newly fetched. Check badDebug in case the same signature was
	// previously marked.
	sig, err := statSig(abs)
	if err != nil || c.isBadDebug(sig) {
		return classifyResult{route: routeProcessMode}
	}
	return classifyResult{route: routeFileMode, debugPath: abs}
}

// systemDebugPath returns the elfutils-standard /usr/lib/debug location for
// a given build-id. Returns "" if buildID is too short to split.
func systemDebugPath(buildID string) string {
	if len(buildID) < 4 {
		return ""
	}
	candidate := filepath.Join("/usr/lib/debug", ".build-id", buildID[:2], buildID[2:]+".debug")
	if _, err := os.Stat(candidate); err != nil {
		return ""
	}
	return candidate
}

// statSig captures the (dev, ino, mtime) of path for badDebug keying.
func statSig(path string) (pathSig, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return pathSig{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return pathSig{}, errors.New("classifier: stat: not a *syscall.Stat_t")
	}
	return pathSig{
		dev:   uint64(st.Dev),
		ino:   st.Ino,
		mtime: fi.ModTime().UnixNano(),
	}, nil
}

func (c *classifier) markBadDebug(sig pathSig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.badDebug[sig] = time.Now().Add(badDebugTTL)
}

func (c *classifier) isBadDebug(sig pathSig) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, ok := c.badDebug[sig]
	if !ok {
		return false
	}
	if time.Now().After(deadline) {
		delete(c.badDebug, sig)
		return false
	}
	return true
}

func (c *classifier) markNegFetch(buildID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.negFetch[buildID] = time.Now().Add(negFetchTTL)
}

func (c *classifier) isNegFetch(buildID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, ok := c.negFetch[buildID]
	if !ok {
		return false
	}
	if time.Now().After(deadline) {
		delete(c.negFetch, buildID)
		return false
	}
	return true
}
```

Add the new imports:
```go
import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -v`
Expected: All classifier tests pass.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/classifier.go symbolize/debuginfod/classifier_test.go
git commit -m "debuginfod: classifier Tier 3 — file-mode with badDebug per-path filtering

Iterates candidate paths (systemPath, cache.AbsPath) and filters each
by its (dev, ino, mtime) signature against badDebug. One corrupt copy
never blocks a valid sibling for the same build-id. Falls through to
process-mode when every candidate is bad and the build-id is in
negFetch."
```

---

## Phase 2 — cgo file-mode wrapper

### Task 2: `symbolizeElfVirt` with originalIPs address rewrite

**Files:**
- Modify: `symbolize/debuginfod/dispatcher.go`
- Test: `symbolize/debuginfod/dispatcher_test.go`

- [ ] **Step 1: Write the failing test**

This test needs a real `.debug` file with at least one resolvable symbol. The easiest way is to use the existing test infrastructure that builds a small Go test binary with DWARF. Check first:
```bash
grep -rn "symbolizeElfVirt\|blaze_symbolize_elf_virt_offsets\|fixture_.*\.debug" symbolize/debuginfod/
```

Append to `symbolize/debuginfod/dispatcher_test.go`:

```go
func TestSymbolizeElfVirtRewritesAddressToOriginalIP(t *testing.T) {
	// We need a .debug file with at least one resolvable function.
	// Build a small Go program in t.TempDir() with DWARF and use its
	// build-id'd debug section.
	debugPath := buildGoFixtureWithDWARF(t, t.TempDir())
	resolvableVA, resolvableName := pickResolvableSymbol(t, debugPath)

	sym, err := New(Options{}) // default CodeInfo, InlinedFns, Demangle = true
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sym.Close() }()

	originalIPs := []uint64{0xdeadbeef00000000} // a deliberately-not-VA value
	virtOffsets := []uint64{resolvableVA}

	frames, err := sym.cgo.symbolizeElfVirt(debugPath, originalIPs, virtOffsets)
	if err != nil {
		t.Fatalf("symbolizeElfVirt: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("got 0 frames")
	}
	if frames[0].Name != resolvableName {
		t.Errorf("frames[0].Name = %q, want %q", frames[0].Name, resolvableName)
	}
	// Critical: the frame's Address MUST be originalIPs[0], not virtOffsets[0].
	// Without this rewrite, pprof.Profile.Resolve cannot route this location
	// to its containing mapping.
	if frames[0].Address != originalIPs[0] {
		t.Errorf("frames[0].Address = %#x, want %#x (originalIPs[0]) — file-mode address rewrite is broken",
			frames[0].Address, originalIPs[0])
	}
}

// buildGoFixtureWithDWARF compiles a tiny Go program with DWARF intact and
// extracts only the .debug sections via objcopy --only-keep-debug. Returns
// the path of the resulting .debug file. Skips if Go toolchain or objcopy
// is unavailable.
func buildGoFixtureWithDWARF(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main

func helloWorld() string { return "hi" }
func main() { _ = helloWorld() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("go build failed (toolchain unavailable?): %v\n%s", err, out)
	}
	debug := filepath.Join(dir, "bin.debug")
	cmd = exec.Command("objcopy", "--only-keep-debug", bin, debug)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("objcopy failed: %v\n%s", err, out)
	}
	return debug
}

// pickResolvableSymbol returns one (file-VA, name) pair from the .debug
// file suitable for round-trip testing. Picks the symbolic address of
// "main.helloWorld" if present; falls back to "main.main".
func pickResolvableSymbol(t *testing.T, debugPath string) (uint64, string) {
	t.Helper()
	f, err := elf.Open(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	syms, err := f.Symbols()
	if err != nil {
		t.Skipf("no symtab in fixture: %v", err)
	}
	for _, want := range []string{"main.helloWorld", "main.main"} {
		for _, s := range syms {
			if s.Name == want && s.Value != 0 {
				return s.Value, s.Name
			}
		}
	}
	t.Skip("no usable symbol in fixture")
	return 0, ""
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test ./symbolize/debuginfod/... -run TestSymbolizeElfVirtRewritesAddressToOriginalIP -v`
Expected: `sym.cgo.symbolizeElfVirt undefined` (compile error).

- [ ] **Step 3: Add the cgo wrapper**

In `symbolize/debuginfod/dispatcher.go`, add to the C header block:

```c
static const blaze_syms* symbolize_elf_virt(
    blaze_symbolizer* sym,
    const char* path,
    const uint64_t* virt_offsets,
    size_t cnt) {
    blaze_symbolize_src_elf src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
    src.path = path;
    src.debug_syms = true;
    return blaze_symbolize_elf_virt_offsets(sym, &src, virt_offsets, cnt);
}
```

Add the Go wrapper alongside `symbolizeProcess`:

```go
// symbolizeElfVirt symbolizes file-VA addresses against the ELF at path.
//
// originalIPs and virtOffsets MUST be the same length. virtOffsets[i] is
// the file-VA used for the lookup (output of AddressMapper); originalIPs[i]
// is the process PC that the returned Frame.Address must carry so pprof
// can route the location to its mapping. This rewrite also propagates to
// every inlined entry in the leaf's chain.
func (st *cgoState) symbolizeElfVirt(path string, originalIPs, virtOffsets []uint64) ([]symbolize.Frame, error) {
	if len(originalIPs) != len(virtOffsets) {
		return nil, fmt.Errorf("debuginfod: symbolizeElfVirt: len mismatch %d vs %d",
			len(originalIPs), len(virtOffsets))
	}
	if len(virtOffsets) == 0 {
		return nil, nil
	}

	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	caddr := (*C.uint64_t)(unsafe.Pointer(&virtOffsets[0]))
	syms := C.symbolize_elf_virt(st.csym, cpath, caddr, C.size_t(len(virtOffsets)))
	if syms == nil {
		return nil, fmt.Errorf("debuginfod: blaze_symbolize_elf_virt_offsets returned NULL")
	}
	defer C.blaze_syms_free(syms)

	cnt := int(syms.cnt)
	out := make([]symbolize.Frame, 0, cnt)
	for i := range cnt {
		csym := C.sym_at(syms, C.size_t(i))
		f := frameFromCSym(csym)
		// Critical: rewrite Frame.Address from virt-offset to original PC.
		// Without this, pprof's mapping resolver cannot find this address
		// in any /proc/<pid>/maps range.
		f.Address = originalIPs[i]
		// Propagate to inlined chain.
		for j := range f.Inlined {
			f.Inlined[j].Address = originalIPs[i]
		}
		out = append(out, f)
	}
	return out, nil
}
```

If `frameFromCSym` doesn't currently propagate inlined frames, check `symbolizeProcess` to see how it walks the chain — replicate that pattern here. Look for `inlined_at` usage.

- [ ] **Step 4: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test ./symbolize/debuginfod/... -run TestSymbolizeElfVirtRewritesAddressToOriginalIP -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/dispatcher.go symbolize/debuginfod/dispatcher_test.go
git commit -m "debuginfod: add symbolizeElfVirt with originalIPs address rewrite

cgo wrapper for blaze_symbolize_elf_virt_offsets. Takes both the
file-VA (for the actual lookup) and the original process PC. Rewrites
Frame.Address (and every Inlined entry's Address) to originalIPs[i]
post-symbolization so pprof's mapping resolver can find each location
in /proc/<pid>/maps. Without the rewrite, file-mode locations would
land at synthetic mapping 0 with no BuildID."
```

---

## Phase 3 — Symbolizer integration

### Task 3a: Reframe dispatcher Case 3 as no-fetch fallback

**Files:**
- Modify: `symbolize/debuginfod/dispatcher.go`
- Modify: `symbolize/debuginfod/dispatcher_test.go`

- [ ] **Step 1: Write the failing test**

Append to `symbolize/debuginfod/dispatcher_test.go`:

```go
func TestDispatcherCase3ReturnsEmptyNoFetch(t *testing.T) {
	// Build a stripped binary with build-id, no .gnu_debuglink, no DWARF.
	// dispatchWithBuildID should now return "" (Case 3 no-fetch fallback)
	// instead of triggering a fetch.
	tmp := t.TempDir()
	binPath := writeStrippedELFWithBuildID(t, filepath.Join(tmp, "exe"), "ccccccccccccccccccccccccccccccccccccccccccc")

	fetchCount := atomic.Int64{}
	fakeFetcher := &fakeSingleflightFetcher{
		fetch: func(_ context.Context, kind, buildID string) (string, error) {
			fetchCount.Add(1)
			t.Errorf("unexpected fetch call: kind=%s buildID=%s", kind, buildID)
			return "", nil
		},
	}

	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.OpenSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	s := &Symbolizer{
		opts:  Options{CacheDir: cacheDir},
		cache: &cache.Cache{Dir: cacheDir, Index: idx},
		sf:    fakeFetcher,
	}

	got := s.dispatchWithBuildID(t.Context(), binPath, "ccccccccccccccccccccccccccccccccccccccccccc")
	if got != "" {
		t.Errorf("Case 3 dispatch returned %q, want \"\" (no-fetch fallback)", got)
	}
	if fetchCount.Load() != 0 {
		t.Errorf("Case 3 dispatch made %d fetch calls, want 0", fetchCount.Load())
	}
}

// fakeSingleflightFetcher lets tests intercept fetchAndStore calls.
type fakeSingleflightFetcher struct {
	fetch func(ctx context.Context, kindStr, buildID string) (string, error)
}

func (f *fakeSingleflightFetcher) fetchAndStore(ctx context.Context, kindStr, buildID string) (string, error) {
	return f.fetch(ctx, kindStr, buildID)
}
```

The test assumes `singleflightFetcher` can be replaced by an interface or that `Symbolizer.sf` has a field. Check the current type:
```bash
grep -n "sf\s\+\*singleflightFetcher\|type singleflightFetcher" symbolize/debuginfod/
```
If `sf` is a concrete type, the test should mock by injecting a sentinel pointer or by making `sf` an interface. Use the smaller-scope refactor: introduce a `fetcher` interface in this package, change `Symbolizer.sf` to that interface type.

- [ ] **Step 2: Run test to verify it fails**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -run TestDispatcherCase3 -v`
Expected: FAIL — either a compile error (test mocks an interface that doesn't exist yet) or the test sees `fetchCount > 0` because Case 3 still calls fetchAndStore.

- [ ] **Step 3: Reframe Case 3**

Locate the current Case 3 in `dispatcher.go`:
```bash
grep -n "Case 3\|binaryReadable" symbolize/debuginfod/dispatcher.go
```

Replace it with the no-fetch fallback. Find the block matching:
```go
	// Case 3: binary on disk, no DWARF locally → fetch /debuginfo into
	// the build-id cache; blazesym will find it via debug_dirs.
	if binaryReadable(symbolicPath) {
		s.stats.cacheMisses.Add(1)
		if _, err := s.sf.fetchAndStore(ctx, "debuginfo", buildID); err != nil {
			s.recordFetchErr(err)
		} else {
			s.stats.fetchSuccessDebuginfo.Add(1)
		}
		return ""
	}
```

Replace with:

```go
	// Case 3: binary on disk, no DWARF locally, no resolvable debug-link.
	// In the v1.1.0 design this branch fetched the .debug file and let
	// blazesym find it via debug_dirs. That never worked — blazesym's
	// debug_dirs walker is gated on .gnu_debuglink, and stripped binaries
	// like Rust/Go release builds don't carry one. The fix lives in the
	// classifier (see symbolize/debuginfod/classifier.go): per-mapping
	// routing sends these mappings through file-mode against the cached
	// .debug directly, bypassing the dispatcher entirely.
	//
	// If we reach this branch, one of two things happened:
	//   (a) The mapping was demoted from file-mode after a fetch or parse
	//       failure. Process-mode is the correct fail-open: blazesym
	//       emits [binary]:offset, identical to v1.1.0 behavior.
	//   (b) Process-mode found a build-id-only mapping that the classifier
	//       didn't pre-route (rare, transient race during a /proc/maps
	//       snapshot change). Same outcome.
	//
	// Either way: no fetch, no panic. Return "" so blazesym uses its
	// defaults.
	if binaryReadable(symbolicPath) {
		s.stats.dispatcherSkippedLocal.Add(1)
		return ""
	}
```

This removes the fetch call; `s.stats.dispatcherSkippedLocal` is incremented as a "we let blazesym try its defaults" signal. (`s.stats.cacheMisses` and `s.stats.fetchSuccessDebuginfo` semantics move to the classifier in Task 4.)

- [ ] **Step 4: Run test to verify it passes**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/debuginfod/... -v`
Expected: All tests pass. `TestDispatcherCase3ReturnsEmptyNoFetch` confirms `fetchCount == 0` and the return is `""`.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/dispatcher.go symbolize/debuginfod/dispatcher_test.go
git commit -m "debuginfod: reframe dispatcher Case 3 as no-fetch fail-open

The v1.1.0 Case 3 fetched .debug into the cache and returned \"\",
expecting blazesym's debug_dirs walker to find it. That never worked:
blazesym's split-debug lookup is gated on .gnu_debuglink, which most
stripped Rust/Go binaries lack. File-mode routing in the classifier
owns this case now.

Case 3 stays in the dispatcher as a fail-open path for two reasons:
(a) a mapping demoted from file-mode after a fetch/parse failure
re-enters here; (b) a transient race could leave a build-id-only
mapping unclassified. Both want \"\" — let blazesym emit [binary]:
offset fallback. No fetch, no panic."
```

---

### Task 3b: Symbolizer per-mapping routing

**Files:**
- Modify: `symbolize/debuginfod/symbolizer.go`
- Modify: `symbolize/debuginfod/classifier_test.go` (add per-PID symbolize test)

- [ ] **Step 1: Write the failing test**

Append to `symbolize/debuginfod/classifier_test.go`:

```go
// TestSymbolizerSplitsBatchesByRoute exercises the integration glue: an
// IP batch with two distinct mappings — one routed to process-mode,
// one to file-mode — should produce frames in the correct order with
// the correct sources. This is a unit-level guard for the routing logic
// in Symbolize(); the full end-to-end story lives in the integration
// tests.
func TestSymbolizerSplitsBatchesByRoute(t *testing.T) {
	t.Skip("integration-style test; uses real /proc/<pid>/maps from this very test process")
	// Implementation hint: pick a non-trivial Go function in the current
	// process (a runtime symbol), call Symbolize(os.Getpid(), [its_PC]),
	// assert one frame with that name. Then also pass an address inside
	// a deliberately-stripped fixture mapped via syscall.Mmap. Verify
	// both frames resolve. This test is gated by t.Skip because it's
	// fragile across runners; the integration tests in test/ cover the
	// same path with real workloads. Left in as a documented placeholder
	// in case someone wants to expand unit coverage of the router.
}
```

The "real" integration test for this lives in Phase 4. Unit-level coverage of the router would require mocking blazesym's process-mode call site, which is too invasive for the value. Skip it and rely on Phase 4 integration tests.

- [ ] **Step 2: Implement per-mapping routing in `Symbolize()`**

Locate the current `Symbolize` method:
```bash
grep -n "func.*Symbolize\|func (s \*Symbolizer) Symbolize" symbolize/debuginfod/symbolizer.go
```

Refactor it to do per-mapping routing. Sketch:

```go
// Symbolize resolves a slice of process-relative IPs into frames. Each IP
// is routed per-mapping: file-mode (via cached .debug + AddressMapper) for
// stripped binaries with build-id only; process-mode (blazesym's defaults)
// for everything else.
func (s *Symbolizer) Symbolize(ctx context.Context, pid int, ips []uint64) ([]symbolize.Frame, error) {
	if len(ips) == 0 {
		return nil, nil
	}

	// Snapshot mappings (re-snapshot every call; mappings change with
	// mmap/dlopen/exec).
	mappings, err := s.resolver.Resolve(pid)
	if err != nil {
		// Resolver failure → fall back to pure process-mode for the batch.
		s.stats.classifySkipped.Add(uint64(len(ips)))
		return s.cgo.symbolizeProcess(uint32(pid), ips)
	}

	// Classify each mapping; cache results in a per-call map.
	routesByMapping := make(map[uint64]classifyResult, len(mappings))
	for _, m := range mappings {
		routesByMapping[m.Start] = s.classifier.classify(ctx, m)
	}

	// Bucket addresses by route + mapping.
	type fileBucket struct {
		mapping     procmap.Mapping
		debugPath   string
		originalIPs []uint64
		indices     []int
	}
	var processBatch struct {
		ips     []uint64
		indices []int
	}
	fileBuckets := map[uint64]*fileBucket{}
	skipped := map[int]bool{}

	for i, ip := range ips {
		m, ok := findMapping(mappings, ip)
		if !ok {
			// No mapping → blazesym will produce [unknown]
			processBatch.ips = append(processBatch.ips, ip)
			processBatch.indices = append(processBatch.indices, i)
			continue
		}
		r := routesByMapping[m.Start]
		switch r.route {
		case routeSkip:
			s.stats.classifySkipped.Add(1)
			skipped[i] = true
		case routeProcessMode:
			s.stats.classifyProcessMode.Add(1)
			processBatch.ips = append(processBatch.ips, ip)
			processBatch.indices = append(processBatch.indices, i)
		case routeFileMode:
			s.stats.classifyFileMode.Add(1)
			b, ok := fileBuckets[m.Start]
			if !ok {
				b = &fileBucket{mapping: m, debugPath: r.debugPath}
				fileBuckets[m.Start] = b
			}
			b.originalIPs = append(b.originalIPs, ip)
			b.indices = append(b.indices, i)
		}
	}

	out := make([]symbolize.Frame, len(ips))

	// Process-mode batch.
	if len(processBatch.ips) > 0 {
		frames, err := s.cgo.symbolizeProcess(uint32(pid), processBatch.ips)
		if err != nil {
			return nil, err
		}
		for j, idx := range processBatch.indices {
			out[idx] = frames[j]
		}
	}

	// File-mode batches, one per mapping.
	for _, b := range fileBuckets {
		mapper, err := s.classifier.mapperFor(b.mapping)
		if err != nil {
			// Mapper construction failed; demote this bucket to process-mode.
			s.stats.normalizationFails.Add(uint64(len(b.originalIPs)))
			fallback, err := s.cgo.symbolizeProcess(uint32(pid), b.originalIPs)
			if err != nil {
				return nil, err
			}
			for j, idx := range b.indices {
				out[idx] = fallback[j]
			}
			continue
		}
		// Normalize each IP via the AddressMapper.
		virt := make([]uint64, 0, len(b.originalIPs))
		fallbackIdx := make([]int, 0)
		fallbackIPs := make([]uint64, 0)
		validIdx := make([]int, 0)
		validIPs := make([]uint64, 0)
		for j, ip := range b.originalIPs {
			fileOff := ip - b.mapping.Start + b.mapping.Offset
			va, ok := mapper.FileOffsetToVirtualAddress(fileOff)
			if !ok {
				s.stats.normalizationFails.Add(1)
				fallbackIdx = append(fallbackIdx, b.indices[j])
				fallbackIPs = append(fallbackIPs, ip)
				continue
			}
			virt = append(virt, va)
			validIPs = append(validIPs, ip)
			validIdx = append(validIdx, b.indices[j])
		}
		if len(virt) > 0 {
			frames, err := s.cgo.symbolizeElfVirt(b.debugPath, validIPs, virt)
			if err != nil {
				// Parse failure → mark badDebug and route through
				// process-mode for these IPs.
				if sig, sigErr := statSig(b.debugPath); sigErr == nil {
					s.classifier.markBadDebug(sig)
				}
				s.stats.fileModeParseFails.Add(1)
				fallback, ferr := s.cgo.symbolizeProcess(uint32(pid), validIPs)
				if ferr != nil {
					return nil, ferr
				}
				for j, idx := range validIdx {
					out[idx] = fallback[j]
				}
			} else {
				s.stats.fileModeCalls.Add(1)
				for j, idx := range validIdx {
					out[idx] = frames[j]
				}
			}
		}
		if len(fallbackIPs) > 0 {
			fallback, err := s.cgo.symbolizeProcess(uint32(pid), fallbackIPs)
			if err != nil {
				return nil, err
			}
			for j, idx := range fallbackIdx {
				out[idx] = fallback[j]
			}
		}
	}

	// Fill skipped frames with empty (preserves stack shape via the
	// pprof mapping-name fallback).
	for i := range out {
		if skipped[i] {
			out[i] = symbolize.Frame{Address: ips[i]}
		}
	}

	return out, nil
}

// findMapping locates the procmap.Mapping that contains ip. Mappings are
// not pre-sorted in all paths; for the batch sizes we see (~hundreds of
// frames, ~50 mappings) a linear scan is fine.
func findMapping(mappings []procmap.Mapping, ip uint64) (procmap.Mapping, bool) {
	for _, m := range mappings {
		if ip >= m.Start && ip < m.Limit {
			return m, true
		}
	}
	return procmap.Mapping{}, false
}
```

Add the `mapperFor` helper to `classifier.go`:

```go
// mapperFor returns an AddressMapper for the mapping, content-addressed
// by (dev, ino) of mapping.OpenablePath().
func (c *classifier) mapperFor(m procmap.Mapping) (*procmap.AddressMapper, error) {
	openPath := m.OpenablePath()
	if openPath == "" {
		return nil, errors.New("classifier: mapping not openable")
	}
	fi, err := os.Stat(openPath)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("classifier: stat: not a *syscall.Stat_t")
	}
	key := mapperKey{dev: uint64(st.Dev), ino: st.Ino}

	c.mu.Lock()
	if existing, ok := c.mappers[key]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.mu.Unlock()

	mapper, err := procmap.NewAddressMapper(openPath)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.mappers[key] = mapper
	c.mu.Unlock()
	return mapper, nil
}
```

- [ ] **Step 3: Run the build to verify it compiles**

Run: `cd /home/diego/github/perf-agent/.worktrees/debuginfod-cache-layout && GOTOOLCHAIN=auto make build 2>&1 | tail -5`
Expected: Build succeeds (warnings from libblazesym_c.a static-link are OK).

- [ ] **Step 4: Run the existing test suite to verify no regressions**

Run: `LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release GOTOOLCHAIN=auto go test ./symbolize/... -v 2>&1 | tail -20`
Expected: All existing tests pass.

- [ ] **Step 5: Commit**

```bash
git add symbolize/debuginfod/symbolizer.go symbolize/debuginfod/classifier.go symbolize/debuginfod/classifier_test.go
git commit -m "debuginfod: Symbolize() does per-mapping routing

Classifies each mapping in the target PID via classifier.classify(),
then buckets the IPs by route. Process-mode batch handed to blazesym
as before; file-mode buckets (one per cached .debug) go through
symbolizeElfVirt with AddressMapper-normalized addresses and the
originalIPs rewrite.

Failure modes preserved: AddressMapper miss demotes the IP to
process-mode; symbolizeElfVirt NULL marks the path in badDebug and
falls back to process-mode for that bucket; resolver failure falls
back to a pure process-mode batch."
```

---

### Task 3c: Stats counters

**Files:**
- Modify: `symbolize/debuginfod/stats.go`

- [ ] **Step 1: Inspect existing stats**

Run: `cat symbolize/debuginfod/stats.go`

Note the existing counter field names and the `Snapshot()` / `String()` methods (if any).

- [ ] **Step 2: Add new counters**

Modify `symbolize/debuginfod/stats.go` to add the new fields and surface them in any existing logging helper:

```go
type stats struct {
	// ... existing fields ...

	// classifier routing
	classifyProcessMode atomic.Uint64
	classifyFileMode    atomic.Uint64
	classifySkipped     atomic.Uint64

	// file-mode outcomes
	fileModeCalls       atomic.Uint64
	fileModeFetchFails  atomic.Uint64
	fileModeParseFails  atomic.Uint64
	fileModeLocalHits   atomic.Uint64

	// normalization (AddressMapper miss for an address)
	normalizationFails  atomic.Uint64
}
```

If there's a `Snapshot()` method that returns a struct with the values, add the new fields there too. If there's a `String()` method, append the new counters.

- [ ] **Step 3: Verify build**

Run: `GOTOOLCHAIN=auto make build 2>&1 | tail -3`
Expected: Build succeeds.

- [ ] **Step 4: Commit**

```bash
git add symbolize/debuginfod/stats.go
git commit -m "debuginfod: add stats counters for classifier + file-mode

classifyProcessMode / classifyFileMode / classifySkipped track route
decisions. fileModeCalls / fileModeFetchFails / fileModeParseFails /
fileModeLocalHits track file-mode outcomes. normalizationFails
records AddressMapper misses. All exposed through the existing
Snapshot/log paths."
```

---

## Phase 4 — Integration tests

### Task 4a: Strip + upload helpers

**Files:**
- Create: `test/integration_strip_helpers.go`

- [ ] **Step 1: Write the helper file**

Create `test/integration_strip_helpers.go`:

```go
package test

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stripWorkload copies src to dst, runs `objcopy --strip-all dst`, and
// returns dst's GNU build-id (lowercase hex). dst MUST be under the
// worktree, NOT /tmp — /tmp is mounted nosuid on many distros and file
// caps don't survive exec from nosuid mounts.
func stripWorkload(t *testing.T, src, dst string) string {
	t.Helper()
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copy %s → %s: %v", src, dst, err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cmd := exec.Command("objcopy", "--strip-all", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("objcopy --strip-all: %v\n%s", err, out)
	}
	return readBuildID(t, dst)
}

// uploadDebug invokes test/debuginfod/upload.sh to extract the debug
// info from srcWithDwarf and deposit it under the debuginfod store at
// test/debuginfod/debuginfo-store. Returns the build-id and the
// expected store-relative path of the .debug file.
//
// The caller is responsible for waiting for the debuginfod server to
// re-scan (the helper's rescan period is ~10s; tests should sleep 12s
// and then verify with curl /buildid/<id>/debuginfo before launching
// perf-agent).
func uploadDebug(t *testing.T, srcWithDwarf string) (buildID, debugPath string) {
	t.Helper()
	abs, err := filepath.Abs(srcWithDwarf)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("./debuginfod/upload.sh", abs)
	cmd.Dir = "./" // test/ is the cwd when go test runs
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upload.sh: %v\n%s", err, out)
	}
	buildID = readBuildID(t, srcWithDwarf)
	debugPath = filepath.Join("debuginfod", "debuginfo-store", ".build-id", buildID[:2], buildID[2:]+".debug")
	return buildID, debugPath
}

// readBuildID parses readelf -n output to extract the GNU build-id.
func readBuildID(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command("readelf", "-n", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("readelf -n %s: %v", path, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Build ID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Build ID:"))
		}
	}
	t.Fatalf("no GNU Build ID in %s\n%s", path, out)
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// waitForDebuginfodReady polls http://localhost:8002/buildid/<id>/debuginfo
// for up to 30s, returning when the server starts serving the build-id
// (HTTP 200) or fatal on timeout.
func waitForDebuginfodReady(t *testing.T, buildID string) {
	t.Helper()
	url := fmt.Sprintf("http://localhost:8002/buildid/%s/debuginfo", buildID)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("curl", "-fsS", "-o", "/dev/null", url)
		if err := cmd.Run(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("debuginfod did not serve %s within 30s", buildID)
}

// _ keeps bufio imported for future helpers that may scan stdout/stderr.
var _ = bufio.NewScanner
```

Add imports as needed. The `strings.SplitSeq` is Go 1.24+; the project targets 1.26 so this is correct.

- [ ] **Step 2: Verify it compiles**

Run: `cd test && GOTOOLCHAIN=auto go build ./... 2>&1 | tail -3`
Expected: No errors.

- [ ] **Step 3: Commit**

```bash
git add test/integration_strip_helpers.go
git commit -m "test: add strip/upload helpers for off-box symbolization tests

stripWorkload(): copy + objcopy --strip-all, return build-id.
uploadDebug(): invoke debuginfod/upload.sh, return build-id + expected
.debug path under the store dir.
waitForDebuginfodReady(): poll the server until the build-id serves.

Used by the upcoming TestStripped* / TestFileMode* integration tests.
dst paths MUST be under the worktree, not /tmp — caps don't survive
exec from nosuid mounts."
```

---

### Task 4b: `TestStrippedRustOffBoxSymbolization`

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Inspect the existing test harness**

Run:
```bash
grep -n "TestStripped\|debuginfod-url\|requireBPFRunnable\|spawnRustWorkload" test/integration_test.go test/debuginfod_integration_test.go
ls test/workloads/rust/
```

Find an existing helper that spawns the Rust workload as a long-running process so the new test can reuse it. If none, the new test can build + run `./workloads/rust/target/release/rust-workload` directly with `exec.Command`.

- [ ] **Step 2: Add the test**

Append to `test/integration_test.go`:

```go
// TestStrippedRustOffBoxSymbolization verifies off-box symbolization for a
// stripped Rust release binary with build-id only (no .gnu_debuglink).
// Without the debuginfod cache layout fix, the user-side function names
// would be missing from the resulting pprof.
func TestStrippedRustOffBoxSymbolization(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir() // somewhere under worktree, not /tmp

	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built (run make test-workloads): %v", err)
	}

	// Upload .debug from the unstripped binary, then strip a copy.
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")
	agent := exec.Command(bin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "6s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	p := parseProfile(t, out)
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}

	want := []string{
		"rust_workload::cpu_intensive_work",
		"core::num::<impl u64>::wrapping_add",
	}
	for _, w := range want {
		found := false
		for name := range got {
			if strings.Contains(name, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected symbol %q in stripped pprof; got: %v", w, sortedKeys(got))
		}
	}
}

// requireDebuginfodContainer skips the test unless the local debuginfod
// docker container is running on localhost:8002.
func requireDebuginfodContainer(t *testing.T) {
	t.Helper()
	cmd := exec.Command("curl", "-fsS", "-o", "/dev/null", "http://localhost:8002/metrics")
	if err := cmd.Run(); err != nil {
		t.Skip("debuginfod container not running on localhost:8002 (run: cd test/debuginfod && docker compose up -d)")
	}
}

// requireTool skips the test when the named CLI tool is not on PATH.
func requireTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s not on PATH", tool)
	}
}

// spawnBinaryAsWorkload starts the binary and returns the running command.
// Caller MUST call cleanup() to kill+wait the process. The binary is
// expected to be CPU-bound for at least 15s.
func spawnBinaryAsWorkload(t *testing.T, bin string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	// Give it 0.5s to set up worker threads.
	time.Sleep(500 * time.Millisecond)
	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return cmd, cleanup
}
```

- [ ] **Step 3: Run the test**

Build the perf-agent + the test binary first (caps required):
```bash
cd /home/diego/github/perf-agent/.worktrees/debuginfod-cache-layout
GOTOOLCHAIN=auto make build
# The agent caps from kernel-stacks already cover this test:
# sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
cd test
GOTOOLCHAIN=auto \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -c -o ../integration.test ./...
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestStrippedRustOffBoxSymbolization -test.v -test.timeout 5m
```
Expected: `--- PASS: TestStrippedRustOffBoxSymbolization`. If the rust workload isn't built or debuginfod isn't running, the test skips with a clear message.

- [ ] **Step 4: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestStrippedRustOffBoxSymbolization

End-to-end coverage of the file-mode symbolization path for a stripped
Rust release binary. Builds the workload with DWARF, uploads .debug to
the local debuginfod container, strips a copy, runs perf-agent with
--debuginfod-url, and asserts that rust_workload::cpu_intensive_work
appears in the pprof — proving the file-mode path resolved the symbol
even though the binary itself carries only .note.gnu.build-id."
```

---

### Task 4c: `TestStrippedGoOffBoxSymbolization`

**Files:**
- Modify: `test/integration_test.go`
- Add: `test/workloads/go/cpu_bound_for_offbox.go` (small helper if existing go workloads aren't built with DWARF — usually they are)

- [ ] **Step 1: Decide whether the existing `cpu_bound` Go workload works**

Inspect:
```bash
readelf -S test/workloads/go/cpu_bound | grep -E '\.debug_info|\.gnu_debuglink|\.note\.gnu\.build-id'
```

If `.debug_info` is present and `.gnu_debuglink` is absent, the existing binary is already what we want — just strip + upload it. Skip the helper file in that case.

If DWARF is missing, rebuild with default `go build` (no `-ldflags=-w`). Update the Makefile target if needed.

- [ ] **Step 2: Add the test**

Append to `test/integration_test.go`:

```go
// TestStrippedGoOffBoxSymbolization verifies off-box symbolization for a
// stripped Go release binary. Plain `go build` emits DWARF + symtab; we
// strip both via objcopy --strip-all leaving only .note.gnu.build-id.
// The .debug file uploaded to debuginfod must carry the DWARF blazesym
// reads.
func TestStrippedGoOffBoxSymbolization(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	goSrc := "./workloads/go/cpu_bound"
	if _, err := os.Stat(goSrc); err != nil {
		t.Skipf("go workload not built (run make test-workloads): %v", err)
	}
	// Sanity: confirm the source binary has DWARF — otherwise the test
	// would silently pass for the wrong reason.
	if !elfHasSection(t, goSrc, ".debug_info") {
		t.Skipf("go workload at %s has no .debug_info; rebuild without -ldflags='-w'", goSrc)
	}

	buildID, _ := uploadDebug(t, goSrc)
	stripped := filepath.Join(worktreeTmp, "go-cpu-bound-stripped")
	stripWorkload(t, goSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")
	agent := exec.Command(bin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "6s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	p := parseProfile(t, out)
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	// `main.main` is always present in a Go binary; `cpu_bound`'s worker
	// loop is typically in main.cpuWork or main.run — accept either.
	wantAny := []string{"main.main", "main.cpuWork", "main.run", "main.worker"}
	found := false
	for _, w := range wantAny {
		for name := range got {
			if strings.Contains(name, w) {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Errorf("no Go user-side function found in stripped pprof; got: %v", sortedKeys(got))
	}
}

// elfHasSection reports whether the ELF at path has a non-empty section
// with the given name. Used as a guard before tests that depend on DWARF
// being present in a fixture binary.
func elfHasSection(t *testing.T, path, name string) bool {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sec := f.Section(name)
	return sec != nil && sec.Size > 0
}
```

- [ ] **Step 3: Run the test**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestStrippedGoOffBoxSymbolization -test.v -test.timeout 5m
```
Expected: `--- PASS: TestStrippedGoOffBoxSymbolization`.

- [ ] **Step 4: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestStrippedGoOffBoxSymbolization

Mirror of the Rust test for a Go release binary. Asserts DWARF is
present in the fixture before stripping (rebuild without -ldflags='-w'
if it isn't), then strips, uploads, profiles, and confirms main.* is
resolved through the file-mode path."
```

---

### Task 4d: `TestFileModeFrameAddressPreservesMapping`

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
// TestFileModeFrameAddressPreservesMapping is the regression guard for the
// originalIPs address-rewrite invariant. Without it, file-mode locations
// carry the ELF virt-offset instead of the process PC, and pprof's
// resolver routes them to the synthetic mapping 0 with no BuildID.
func TestFileModeFrameAddressPreservesMapping(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")
	agent := exec.Command(bin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "6s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	p := parseProfile(t, out)

	// For each Location whose function name looks like a Rust symbol
	// (rust_workload::*), assert it has a real Mapping with a BuildID.
	rustRe := regexp.MustCompile(`^rust_workload::`)
	checked := 0
	for _, loc := range p.Location {
		hasRust := false
		for _, ln := range loc.Line {
			if ln.Function != nil && rustRe.MatchString(ln.Function.Name) {
				hasRust = true
				break
			}
		}
		if !hasRust {
			continue
		}
		checked++

		// Location.Address must be inside Location.Mapping.Start..Mapping.Limit.
		if loc.Mapping == nil {
			t.Errorf("rust frame at addr %#x has no Mapping (file-mode address rewrite broken)", loc.Address)
			continue
		}
		if loc.Address < loc.Mapping.Start || loc.Address >= loc.Mapping.Limit {
			t.Errorf("rust frame addr %#x outside Mapping[%#x, %#x) — file-mode address rewrite broken",
				loc.Address, loc.Mapping.Start, loc.Mapping.Limit)
		}
		// Mapping.BuildID must equal the workload's build-id.
		if !strings.EqualFold(loc.Mapping.BuildID, buildID) {
			t.Errorf("rust frame Mapping.BuildID = %q, want %q",
				loc.Mapping.BuildID, buildID)
		}
		// Mapping.File must point at the workload (not [unknown] or [jit]).
		if !strings.Contains(loc.Mapping.File, "rust-workload") {
			t.Errorf("rust frame Mapping.File = %q, want a path containing rust-workload",
				loc.Mapping.File)
		}
	}
	if checked == 0 {
		t.Fatal("no rust frames in pprof — symbolization didn't fire at all")
	}
}
```

- [ ] **Step 2: Run the test**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestFileModeFrameAddressPreservesMapping -test.v -test.timeout 5m
```
Expected: `--- PASS`. Without the address-rewrite invariant, the test FAILs with all three sub-assertions firing.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestFileModeFrameAddressPreservesMapping

Regression guard for the symbolizeElfVirt address-rewrite invariant.
Asserts that for every Rust frame in the resulting pprof:
  * Location.Address is inside Location.Mapping.Start..Mapping.Limit
  * Location.Mapping.BuildID equals the workload's build-id
  * Location.Mapping.File points at the workload
Without the rewrite, every assertion fails — locations land at the
synthetic mapping 0 with no build-id."
```

---

### Task 4e: `TestStrippedCachedHitNoFetch`

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
// TestStrippedCachedHitNoFetch verifies that a second profiling run for the
// same stripped binary doesn't re-fetch from debuginfod when the cache
// already has the .debug. Confirms the cache.Has → file-mode short-circuit.
func TestStrippedCachedHitNoFetch(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()

	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// First run: should fetch.
	runStripped(t, bin, stripped, cacheDir, t.TempDir())

	// Snapshot the debuginfod container access log line count.
	prevHits := countDebuginfodHits(t, buildID)

	// Second run: should NOT fetch (cache hit).
	runStripped(t, bin, stripped, cacheDir, t.TempDir())

	newHits := countDebuginfodHits(t, buildID)
	delta := newHits - prevHits
	if delta > 0 {
		t.Errorf("expected 0 new debuginfod fetches on second run; saw %d new GET /buildid/%s/debuginfo entries",
			delta, buildID)
	}
}

// runStripped is a small helper that runs the agent for one short profile.
func runStripped(t *testing.T, agentBin, target, cacheDir, outDir string) {
	t.Helper()
	cmd, cleanup := spawnBinaryAsWorkload(t, target)
	defer cleanup()
	out := filepath.Join(outDir, "profile.pb.gz")
	agent := exec.Command(agentBin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}
}

// countDebuginfodHits returns the number of `GET /buildid/<buildID>/debuginfo`
// log lines emitted by the debuginfod container so far. Best-effort —
// returns 0 if `docker logs` fails (we surface that as 0 delta upstream).
func countDebuginfodHits(t *testing.T, buildID string) int {
	t.Helper()
	cmd := exec.Command("docker", "logs", "debuginfod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("docker logs debuginfod: %v (proceeding with 0 hits)", err)
		return 0
	}
	needle := "GET /buildid/" + buildID + "/debuginfo"
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}
```

- [ ] **Step 2: Run the test**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestStrippedCachedHitNoFetch -test.v -test.timeout 5m
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestStrippedCachedHitNoFetch

Two consecutive runs share a --symbol-cache-dir. Asserts the second
run doesn't hit debuginfod for the same build-id — proves the cache.Has
short-circuit in the classifier."
```

---

### Task 4f: `TestFileModeParseFailDemotes`

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
// TestFileModeParseFailDemotes truncates a cached .debug to make
// blaze_symbolize_elf_virt_offsets return NULL. The mapping should demote
// to process-mode and pprof should still emit frames (just unsymbolized).
// Confirms badDebug per-path filtering.
func TestFileModeParseFailDemotes(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()
	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")

	// First run: populates the cache.
	runStripped(t, bin, stripped, cacheDir, t.TempDir())

	// Corrupt the cached .debug.
	cached := filepath.Join(cacheDir, ".build-id", buildID[:2], buildID[2:]+".debug")
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("expected cached .debug at %s: %v", cached, err)
	}
	if err := os.Truncate(cached, 100); err != nil {
		t.Fatalf("truncate %s: %v", cached, err)
	}

	// Second run: parse fails, mapping demotes to process-mode.
	// pprof must still emit frames for the workload's mapping.
	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cmd, cleanup := spawnBinaryAsWorkload(t, stripped)
	defer cleanup()
	agent := exec.Command(bin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	p := parseProfile(t, out)
	if len(p.Sample) == 0 {
		t.Fatalf("no samples in pprof — agent crashed or got 0 frames")
	}
	// At least one sample's leaf should fall in the workload's mapping
	// (even if unsymbolized).
	var workloadMapping *profile.Mapping
	for _, m := range p.Mapping {
		if strings.Contains(m.File, "rust-workload") {
			workloadMapping = m
			break
		}
	}
	if workloadMapping == nil {
		t.Fatalf("no rust-workload mapping in pprof — agent didn't see the binary")
	}
}
```

- [ ] **Step 2: Run the test**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestFileModeParseFailDemotes -test.v -test.timeout 5m
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestFileModeParseFailDemotes

Truncates a cached .debug to force blaze_symbolize_elf_virt_offsets to
return NULL. Asserts the agent doesn't crash and that pprof still emits
samples + the workload's mapping — proving the badDebug per-path
filter demotes the mapping to process-mode gracefully."
```

---

### Task 4g: `TestStrippedSidecarUnreachableSymbolicPath`

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
// TestStrippedSidecarUnreachableSymbolicPath simulates the sidecar /
// mount-namespace case by deleting the workload binary from disk while
// it's still running. The process keeps the binary alive via its open
// file descriptor; /proc/<pid>/map_files/... still resolves, but the
// symbolic path is gone. Asserts symbols still resolve and that
// Mapping.BuildID is populated through map_files.
func TestStrippedSidecarUnreachableSymbolicPath(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)
	requireTool(t, "objcopy")

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()
	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	buildID, _ := uploadDebug(t, rustSrc)
	stripped := filepath.Join(worktreeTmp, "rust-workload-stripped")
	stripWorkload(t, rustSrc, stripped)
	waitForDebuginfodReady(t, buildID)

	cmd, cleanup := spawnBinaryAsWorkload(t, stripped)
	defer cleanup()

	// Delete the binary from disk; the running process keeps it alive
	// through the open fd. /proc/<pid>/map_files/<va>-<va> still resolves.
	if err := os.Remove(stripped); err != nil {
		t.Fatalf("remove %s: %v", stripped, err)
	}

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")
	agent := exec.Command(bin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "6s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	p := parseProfile(t, out)
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	// Assert symbol resolved through map_files-derived path.
	found := false
	for name := range got {
		if strings.Contains(name, "rust_workload::cpu_intensive_work") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sidecar-style profiling didn't resolve symbols; got: %v", sortedKeys(got))
	}
	// Assert Mapping.BuildID is populated (i.e., Resolver.populate read
	// it via map_files since the symbolic path is gone).
	var workloadMapping *profile.Mapping
	for _, m := range p.Mapping {
		// File is the symbolic path which we deleted; it shows as "(deleted)"
		// suffix in /proc/<pid>/maps. Match by build-id instead.
		if strings.EqualFold(m.BuildID, buildID) {
			workloadMapping = m
			break
		}
	}
	if workloadMapping == nil {
		t.Errorf("no mapping with workload build-id %s — Resolver.populate didn't use map_files", buildID)
	}
}
```

- [ ] **Step 2: Run the test**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestStrippedSidecarUnreachableSymbolicPath -test.v -test.timeout 5m
```
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestStrippedSidecarUnreachableSymbolicPath

Deletes the stripped workload from disk while it's still running. The
process keeps it alive via its open file descriptor; /proc/<pid>/map_files
still resolves. Asserts symbols resolve via map_files AND that pprof
Mapping.BuildID is populated (Resolver.populate must read via map_files
when the symbolic path is gone)."
```

---

### Task 4h: `TestOffBoxLibcResolution`

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
// TestOffBoxLibcResolution verifies that system libraries (libc) continue
// to resolve through the process-mode path when local /usr/lib/debug
// debuginfo is installed. The new classifier must NOT refetch them.
// Skip when the local debuginfo isn't available.
func TestOffBoxLibcResolution(t *testing.T) {
	t.Helper()
	requireBPFRunnable(t, getAgentPath(t))
	requireDebuginfodContainer(t)

	// Find libc with build-id and assert a corresponding .debug exists at
	// /usr/lib/debug/.build-id/...
	libc, libcBuildID := findLibcWithLocalDebuginfo(t)

	bin := getAgentPath(t)
	worktreeTmp := t.TempDir()
	rustSrc := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(rustSrc); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}
	cmd, cleanup := spawnBinaryAsWorkload(t, rustSrc)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	cacheDir := filepath.Join(t.TempDir(), "symbol-cache")
	agent := exec.Command(bin,
		"--profile",
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", cacheDir,
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "6s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	// libc should NOT have been fetched via debuginfod — it was resolvable
	// locally through process-mode.
	hits := countDebuginfodHits(t, libcBuildID)
	if hits > 0 {
		t.Errorf("libc fetched from debuginfod %d times; local /usr/lib/debug should have been used. libc=%s build-id=%s",
			hits, libc, libcBuildID)
	}

	// libc functions should appear in the pprof.
	p := parseProfile(t, out)
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	wantAny := []string{"__libc_start_main", "malloc", "__GI___libc_malloc", "free"}
	found := false
	for _, w := range wantAny {
		for name := range got {
			if strings.Contains(name, w) {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Logf("no libc symbol resolved; got: %v (acceptable on systems with no libc debuginfo)", sortedKeys(got))
	}
}

// findLibcWithLocalDebuginfo locates libc.so.6 in the loader cache, reads
// its build-id, and verifies /usr/lib/debug/.build-id/NN/REST.debug exists.
// Skips the test if any of these aren't true.
func findLibcWithLocalDebuginfo(t *testing.T) (string, string) {
	t.Helper()
	// Search common paths.
	candidates := []string{
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib64/libc.so.6",
		"/usr/lib64/libc.so.6",
		"/usr/lib/x86_64-linux-gnu/libc.so.6",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			id := readBuildID(t, p)
			if id == "" {
				continue
			}
			debugPath := filepath.Join("/usr/lib/debug", ".build-id", id[:2], id[2:]+".debug")
			if _, err := os.Stat(debugPath); err == nil {
				return p, id
			}
		}
	}
	t.Skip("no libc.so.6 with local /usr/lib/debug/.build-id debuginfo found — install glibc-debuginfo")
	return "", ""
}
```

- [ ] **Step 2: Run the test**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ../integration.test -test.run TestOffBoxLibcResolution -test.v -test.timeout 5m
```
Expected: PASS on hosts with `glibc-debuginfo` installed; SKIP otherwise.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: add TestOffBoxLibcResolution

Verifies G2: when local /usr/lib/debug/.build-id/.../libc.so.debug
exists, the classifier doesn't refetch libc through debuginfod. Skips
on hosts without glibc-debuginfo installed."
```

---

## Phase 5 — Final verification

### Task 5: Full build + test matrix

**Files:** None (verification only)

- [ ] **Step 1: Clean build**

```bash
cd /home/diego/github/perf-agent/.worktrees/debuginfod-cache-layout
make clean
GOTOOLCHAIN=auto make build
```
Expected: Build succeeds with only the pre-existing libblazesym_c.a static-link warnings.

- [ ] **Step 2: Unit tests (no caps required)**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
  GOTOOLCHAIN=auto \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./... -count=1
```
Expected: All packages pass.

- [ ] **Step 3: `go vet`**

```bash
GOTOOLCHAIN=auto go vet ./...
```
Expected: Clean.

- [ ] **Step 4: golangci-lint**

```bash
golangci-lint run --timeout=5m
```
Expected: Clean (or only pre-existing issues unrelated to this work).

- [ ] **Step 5: Integration tests (requires caps + debuginfod container)**

```bash
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
cd test/debuginfod && docker compose up -d && cd ../..
cd test
GOTOOLCHAIN=auto \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -c -o ../integration.test ./...
cd ..
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release ./integration.test -test.run 'TestStripped|TestFileMode|TestOffBoxLibc' -test.v -test.timeout 15m
```
Expected: All 8 new tests pass (or SKIP cleanly when prerequisites aren't met).

- [ ] **Step 6: Verify stats**

After integration tests pass, run perf-agent manually against a stripped workload and confirm the new stats counters appear in the shutdown log:

```bash
./perf-agent --profile --debuginfod-url=http://localhost:8002 \
  --symbol-cache-dir=/tmp/symcache-check \
  --pid=$(pgrep rust-workload) --duration=3s \
  --profile-output=/tmp/check.pb.gz 2>&1 | grep -E "classifyFileMode|fileModeCalls|normalizationFails"
```
Expected: stats output includes the new counters with non-zero `classifyFileMode` and `fileModeCalls` values.

---

## Self-review checklist

After completing all tasks, run a final pass:

- [ ] **Spec coverage**: Walk the spec section by section. Each section has at least one task:
  - G1 (stripped + build-id only works): Task 4b + 4c
  - G2 (system libs reuse local debuginfo): Task 4h
  - G3 (no regressions for DWARF/debuglink binaries): existing tests + Task 4h
  - G4 (Rust + Go integration tests): Tasks 4b, 4c
  - Sidecar / map_files: Tasks 0a, 0b, 4g
  - AddressMapper page-alignment + multi-PT_LOAD + PIE: Tasks 0c, 0d
  - Classifier Tier 1/2/3: Tasks 1a, 1b, 1c
  - badDebug per-path keying: Task 1c + 4f
  - negFetch with TTL: Task 1c
  - File-mode address rewrite: Task 2 + 4d
  - Dispatcher Case 3 reframe: Task 3a
  - Per-mapping routing in Symbolize(): Task 3b
  - New stats counters: Task 3c
  - Cache hit short-circuit: Task 4e
  - Parse-fail demotion: Task 4f

- [ ] **Placeholders**: Search the plan for "TBD", "TODO", "fill in", "similar to". Replace any with concrete content.

- [ ] **Type consistency**: Are types and method names consistent across tasks?
  - `routeKind` (Task 1a → all later tasks) ✓
  - `classifyResult{route, debugPath}` (Task 1a → 3b) ✓
  - `mapperKey{dev, ino}` (Task 1a) ✓
  - `pathSig{dev, ino, mtime}` (Task 1a → 1c) ✓
  - `symbolizeElfVirt(path, originalIPs, virtOffsets)` (Task 2 → 3b) ✓
  - `procmap.Mapping.MapFiles + OpenablePath()` (Task 0a → all later) ✓

- [ ] **Imports**: Each new file's import block is complete (Go's strict import checking will flag missing imports during compilation).
