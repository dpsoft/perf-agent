# S9-Narrow: pprof Address + Mapping Fidelity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve each sample's absolute PC and produce a real per-binary `pprof.Mapping` table (path + start/limit/offset + build-id) so downstream consumers like `llvm-profdata` can attribute samples to ELF file offsets.

**Architecture:** A new `unwind/procmap/` package lazily parses `/proc/<pid>/maps` and reads `.note.gnu.build-id`, caching per-PID. Both the FP profiler and the DWARF profiler pass a shared `Resolver` to the pprof builder. `pprof.Frame` gains `Address` plus mapping metadata fields; `pprof.ProfileBuilder.addLocation` uses an address-keyed primary path with a name-based fallback. The DWARF session tees MMAP2/EXIT events from the existing `MmapWatcher` into `Resolver.InvalidateAddr` / `Resolver.Invalidate` for live accuracy; the FP path accepts snapshot-at-Collect-time semantics because its samples carry no timestamps.

**Tech Stack:** Go 1.26, `debug/elf`, `github.com/google/pprof/profile`, cilium/ebpf, blazesym (CGO).

**Reference spec:** `docs/superpowers/specs/2026-04-24-s9-pprof-fidelity-design.md`

---

## File Structure

**New:**
- `unwind/procmap/procmap.go` — public API (`Resolver`, `Mapping`, `Option`, constructors).
- `unwind/procmap/parse.go` — `/proc/<pid>/maps` parser.
- `unwind/procmap/buildid.go` — ELF `.note.gnu.build-id` reader.
- `unwind/procmap/resolver.go` — `Resolver` type with lazy cache, Lookup, Invalidate, InvalidateAddr.
- `unwind/procmap/procmap_test.go` — all unit tests.
- `unwind/procmap/testdata/proc/<pid>/maps` — fake /proc fixtures.
- `unwind/procmap/testdata/fixture.elf` — ELF fixture with known build-id.

**Modified:**
- `pprof/pprof.go` — `Frame` fields, intern keys, `BuildersOptions.Resolver`, `addLocation` branches, `addMapping`.
- `pprof/pprof_test.go` — new unit tests.
- `profile/profiler.go` — `blazeSymToFrames(s, addr)`, resolver wiring.
- `unwind/dwarfagent/symbolize.go` — `blazeSymToFrames(s, addr)`, `symbolizePID` passes IPs.
- `unwind/dwarfagent/common.go` — `session.resolver`, invalidation observer wired into `runTracker`.
- `unwind/ehmaps/tracker.go` — `PIDTracker.Run` gains variadic observer callbacks.
- `test/integration_test.go` — pprof fidelity assertions added to `TestProfileMode` and `TestPerfAgentSystemWideDwarfProfile`.

**Deleted (cleanup tasks):**
- `cmd/perf-dwarf-test/`, `cmd/perfreader-test/`, `cmd/test_blazesym/`.
- `SX` stage-marker comments across ~10 files (see Task 13).

---

## Testing Conventions

Unit tests for `unwind/procmap/`, `pprof/`, `profile/`, `unwind/dwarfagent/` run without root via the standard Go module at the repo root.

Integration tests live in `test/` (separate Go module with `replace` directive). They need root and CGO — this plan uses the project's standard invocation:

```bash
cd test && sudo -E \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -v -run <TestName> ./...
```

Per the project preference, prefer `setcap` on the `perf-agent` binary over sudo for repeat test runs:

```bash
sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
```

---

## Task 1: `unwind/procmap` — `/proc/<pid>/maps` parser

**Files:**
- Create: `unwind/procmap/procmap.go`
- Create: `unwind/procmap/parse.go`
- Create: `unwind/procmap/procmap_test.go`
- Create: `unwind/procmap/testdata/proc/4242/maps`

**Context:** Parse a single `/proc/<pid>/maps` file into `[]Mapping` sorted by `Start`. Only keep executable file-backed mappings (we don't resolve PCs into anonymous or data mappings). The format is:

```
addr_start-addr_end perms offset dev inode pathname
7f1a2b3c4000-7f1a2b3c6000 r-xp 00001000 fd:01 1234567 /usr/bin/target
```

Perms: 4 chars like `r-xp`, `r--p`. Exec = perms[2] == 'x'. Offset is hex.

- [ ] **Step 1: Create the testdata fixture**

```bash
mkdir -p unwind/procmap/testdata/proc/4242
cat > unwind/procmap/testdata/proc/4242/maps <<'EOF'
00400000-00420000 r-xp 00001000 fd:01 1234567                            /usr/bin/target
00420000-00421000 r--p 00021000 fd:01 1234567                            /usr/bin/target
7f0000001000-7f0000100000 r-xp 00002000 fd:01 7654321                    /lib/x86_64-linux-gnu/libc.so.6
7f0000200000-7f0000201000 rw-p 00000000 00:00 0                          [heap]
7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0                          [stack]
EOF
```

- [ ] **Step 2: Write failing test**

Create `unwind/procmap/procmap_test.go`:

```go
package procmap

import (
	"path/filepath"
	"testing"
)

func TestParseMapsFile(t *testing.T) {
	path := filepath.Join("testdata", "proc", "4242", "maps")
	got, err := parseMapsFile(path)
	if err != nil {
		t.Fatalf("parseMapsFile: %v", err)
	}

	want := []Mapping{
		{Path: "/usr/bin/target", Start: 0x00400000, Limit: 0x00420000, Offset: 0x1000, IsExec: true},
		{Path: "/lib/x86_64-linux-gnu/libc.so.6", Start: 0x7f0000001000, Limit: 0x7f0000100000, Offset: 0x2000, IsExec: true},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d mappings, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mapping %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

```bash
go test ./unwind/procmap/ -run TestParseMapsFile -v
```

Expected: FAIL with "undefined: parseMapsFile" or "undefined: Mapping".

- [ ] **Step 4: Create `unwind/procmap/procmap.go`**

```go
// Package procmap resolves addresses into per-binary mapping identity
// (path, start/limit, file offset, build-id) by parsing /proc/<pid>/maps
// and ELF .note.gnu.build-id sections. Results feed pprof.Mapping so
// downstream tools can round-trip samples back to ELF file offsets.
package procmap

// Mapping describes one executable range in a process's address space.
// Non-executable and anonymous ranges are dropped during parsing.
type Mapping struct {
	Path    string
	Start   uint64
	Limit   uint64 // exclusive
	Offset  uint64 // p_offset of the backing PT_LOAD segment
	BuildID string // hex; empty if no .note.gnu.build-id
	IsExec  bool
}
```

- [ ] **Step 5: Implement `unwind/procmap/parse.go`**

```go
package procmap

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// parseMapsFile reads a /proc/<pid>/maps file and returns executable
// file-backed mappings sorted by Start. Non-executable ranges,
// anonymous mappings, and special pseudo-files ([heap], [stack],
// [vdso], [vvar], [vsyscall]) are skipped — PCs inside them have no
// meaningful ELF identity.
func parseMapsFile(path string) ([]Mapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Mapping
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		m, ok, err := parseMapsLine(line)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", line, err)
		}
		if ok {
			out = append(out, m)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

// parseMapsLine parses one line of /proc/<pid>/maps. Returns ok=false
// for non-executable, anonymous, or pseudo-file lines (caller should
// skip them without emitting a Mapping). Returns an error only for
// malformed lines.
func parseMapsLine(line string) (Mapping, bool, error) {
	// addr_range perms offset dev inode pathname
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Mapping{}, false, fmt.Errorf("too few fields: %d", len(fields))
	}

	perms := fields[1]
	if len(perms) < 3 || perms[2] != 'x' {
		return Mapping{}, false, nil
	}

	var path string
	if len(fields) >= 6 {
		path = fields[5]
	}
	if path == "" || strings.HasPrefix(path, "[") {
		return Mapping{}, false, nil
	}

	dash := strings.IndexByte(fields[0], '-')
	if dash < 0 {
		return Mapping{}, false, fmt.Errorf("no dash in range %q", fields[0])
	}
	start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("start: %w", err)
	}
	limit, err := strconv.ParseUint(fields[0][dash+1:], 16, 64)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("limit: %w", err)
	}
	off, err := strconv.ParseUint(fields[2], 16, 64)
	if err != nil {
		return Mapping{}, false, fmt.Errorf("offset: %w", err)
	}

	return Mapping{Path: path, Start: start, Limit: limit, Offset: off, IsExec: true}, true, nil
}
```

- [ ] **Step 6: Run test to verify it passes**

```bash
go test ./unwind/procmap/ -run TestParseMapsFile -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add unwind/procmap/procmap.go unwind/procmap/parse.go unwind/procmap/procmap_test.go unwind/procmap/testdata
git commit -m "procmap: parse /proc/<pid>/maps into sorted executable mappings"
```

---

## Task 2: `unwind/procmap` — ELF build-id reader

**Files:**
- Create: `unwind/procmap/buildid.go`
- Modify: `unwind/procmap/procmap_test.go`

**Context:** GNU build-ids are stored in the ELF `.note.gnu.build-id` section (type `NT_GNU_BUILD_ID` = 3, name `"GNU\0"`, desc is the raw ID bytes). `debug/elf` exposes sections; we parse the note payload ourselves. Most host binaries (coreutils, libc) have build-ids so `/bin/ls` is a reliable fixture.

- [ ] **Step 1: Write failing test**

Append to `unwind/procmap/procmap_test.go`:

```go
func TestReadBuildID(t *testing.T) {
	// /bin/ls on any modern distro has a GNU build-id. We don't assert
	// the exact value (it varies) — only that it parses to a non-empty
	// lowercase hex string.
	id, err := readBuildID("/bin/ls")
	if err != nil {
		t.Fatalf("readBuildID(/bin/ls): %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty build-id, got empty")
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("build-id %q contains non-hex char %q", id, r)
		}
	}
}

func TestReadBuildIDMissing(t *testing.T) {
	id, err := readBuildID("/nonexistent/path/to/nothing")
	if err == nil {
		t.Fatalf("expected error, got id=%q", id)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./unwind/procmap/ -run TestReadBuildID -v
```

Expected: FAIL with "undefined: readBuildID".

- [ ] **Step 3: Implement `unwind/procmap/buildid.go`**

```go
package procmap

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

const (
	nt_GNU_BUILD_ID = 3
	gnu_name        = "GNU\x00"
)

// readBuildID returns the GNU build-id of the ELF at path as a
// lowercase hex string. Returns an empty string (with nil error) when
// the ELF is valid but has no .note.gnu.build-id note, and an error
// when the file can't be opened or isn't ELF.
func readBuildID(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sec := f.Section(".note.gnu.build-id")
	if sec == nil {
		return "", nil
	}

	data, err := sec.Data()
	if err != nil {
		return "", fmt.Errorf("read section: %w", err)
	}

	id, err := parseBuildIDNote(data, f.ByteOrder)
	if err != nil {
		return "", err
	}
	return id, nil
}

// parseBuildIDNote walks the ELF note records in data and returns the
// desc of the first NT_GNU_BUILD_ID note whose name is "GNU\0".
// Empty string with nil error means no matching note was found.
func parseBuildIDNote(data []byte, bo binary.ByteOrder) (string, error) {
	for len(data) > 0 {
		if len(data) < 12 {
			return "", fmt.Errorf("note header truncated")
		}
		namesz := bo.Uint32(data[0:4])
		descsz := bo.Uint32(data[4:8])
		typ := bo.Uint32(data[8:12])
		data = data[12:]

		nameEnd := int(alignUp(namesz, 4))
		if nameEnd > len(data) {
			return "", fmt.Errorf("note name truncated")
		}
		name := data[:namesz]
		data = data[nameEnd:]

		descEnd := int(alignUp(descsz, 4))
		if descEnd > len(data) {
			return "", fmt.Errorf("note desc truncated")
		}
		desc := data[:descsz]
		data = data[descEnd:]

		if typ == nt_GNU_BUILD_ID && bytes.Equal(name, []byte(gnu_name)) {
			return hex.EncodeToString(desc), nil
		}
	}
	return "", nil
}

func alignUp(n, a uint32) uint32 { return (n + a - 1) &^ (a - 1) }

// Keep the unused io import tolerant — debug/elf indirectly uses it.
var _ = io.EOF
```

- [ ] **Step 4: Run to verify it passes**

```bash
go test ./unwind/procmap/ -run TestReadBuildID -v
```

Expected: PASS (both subtests).

- [ ] **Step 5: Remove the placeholder io import if not needed**

Check if `io` is actually referenced; if not, remove both the import and `var _ = io.EOF`. Run `go vet ./unwind/procmap/` — no errors.

- [ ] **Step 6: Commit**

```bash
git add unwind/procmap/buildid.go unwind/procmap/procmap_test.go
git commit -m "procmap: read GNU build-id from ELF .note.gnu.build-id"
```

---

## Task 3: `unwind/procmap` — `Resolver` with lazy cache, `Lookup`, `Invalidate`

**Files:**
- Create: `unwind/procmap/resolver.go`
- Modify: `unwind/procmap/procmap.go` (add `Option`, `NewResolver`)
- Modify: `unwind/procmap/procmap_test.go`
- Create: `unwind/procmap/testdata/proc/5555/maps`

**Context:** `Resolver` lazily populates per-PID on first `Lookup`. Uses `sync.Once` per entry so concurrent Lookups for the same PID parse once. Build-ids are cached globally (`sync.Map` keyed by path) because the same binary often appears in many PIDs. `procRoot` defaulting to `/proc` but overridable via `WithProcRoot` for tests.

- [ ] **Step 1: Create a second fake /proc entry for Invalidate tests**

```bash
mkdir -p unwind/procmap/testdata/proc/5555
cat > unwind/procmap/testdata/proc/5555/maps <<'EOF'
00500000-00520000 r-xp 00000000 fd:01 9999999                            /usr/bin/other
EOF
```

- [ ] **Step 2: Write failing tests for Resolver**

Append to `unwind/procmap/procmap_test.go`:

```go
func TestResolverLookupHitMiss(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	m, ok := r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("expected lookup hit in /usr/bin/target range")
	}
	if m.Path != "/usr/bin/target" {
		t.Errorf("got Path=%q, want /usr/bin/target", m.Path)
	}

	_, ok = r.Lookup(4242, 0xdeadbeef)
	if ok {
		t.Fatal("expected lookup miss outside any mapping")
	}
}

func TestResolverMissingPID(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, ok := r.Lookup(9999999, 0x00401234)
	if ok {
		t.Fatal("expected miss for non-existent PID")
	}
	// Second call should hit the cached empty entry, not re-read /proc.
	_, ok = r.Lookup(9999999, 0x00401234)
	if ok {
		t.Fatal("second lookup should also miss")
	}
}

func TestResolverInvalidate(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, ok := r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("first lookup should hit")
	}
	r.Invalidate(4242)
	// Still hits because the fixture file is unchanged, but the path
	// re-populated. No public way to observe re-parse directly without
	// wiring an observer; this test just ensures Invalidate doesn't
	// panic and Lookup keeps working afterward.
	_, ok = r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("lookup after Invalidate should still hit")
	}
}

func TestResolverConcurrentLookup(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	const N = 32
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, ok := r.Lookup(4242, 0x00401234)
			if !ok {
				errs <- fmt.Errorf("lookup miss")
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
```

Also add `"fmt"` to the imports if not already there.

- [ ] **Step 3: Run to verify failures**

```bash
go test ./unwind/procmap/ -run TestResolver -v
```

Expected: FAIL with "undefined: NewResolver" / "undefined: WithProcRoot".

- [ ] **Step 4: Add options to `procmap.go`**

Append to `unwind/procmap/procmap.go`:

```go
// Option configures a Resolver.
type Option func(*resolverConfig)

type resolverConfig struct {
	procRoot string
}

// WithProcRoot overrides the filesystem root used to resolve /proc
// paths. Defaults to "/proc". Intended for unit tests with fake
// per-PID fixtures.
func WithProcRoot(path string) Option {
	return func(c *resolverConfig) { c.procRoot = path }
}
```

- [ ] **Step 5: Implement `unwind/procmap/resolver.go`**

```go
package procmap

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
)

// Resolver caches per-PID /proc/<pid>/maps snapshots and per-path
// build-ids. Safe for concurrent use. Populates lazily on first
// Lookup for a PID.
type Resolver struct {
	procRoot string

	mu    sync.RWMutex
	cache map[uint32]*pidEntry

	buildIDs sync.Map // path string → build-id hex string
}

type pidEntry struct {
	once     sync.Once
	err      error
	mappings []Mapping // sorted by Start; binary-searched on Lookup
}

// NewResolver returns a Resolver ready for concurrent use.
func NewResolver(opts ...Option) *Resolver {
	cfg := resolverConfig{procRoot: "/proc"}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Resolver{
		procRoot: cfg.procRoot,
		cache:    map[uint32]*pidEntry{},
	}
}

// Lookup returns the Mapping containing addr in pid's address space.
// Returns ok=false when pid has no cached (or resolvable) mappings,
// or when addr falls outside every known executable range.
func (r *Resolver) Lookup(pid uint32, addr uint64) (Mapping, bool) {
	entry := r.entryFor(pid)
	entry.once.Do(func() { r.populate(entry, pid) })
	if entry.err != nil || len(entry.mappings) == 0 {
		return Mapping{}, false
	}

	// Binary search for the largest Start <= addr.
	idx := sort.Search(len(entry.mappings), func(i int) bool {
		return entry.mappings[i].Start > addr
	}) - 1
	if idx < 0 {
		return Mapping{}, false
	}
	m := entry.mappings[idx]
	if addr >= m.Limit {
		return Mapping{}, false
	}
	return m, true
}

// Invalidate drops any cached state for pid. The next Lookup
// re-parses /proc/<pid>/maps. Call on process exit or when the
// agent learns of whole-process churn (e.g., exec).
func (r *Resolver) Invalidate(pid uint32) {
	r.mu.Lock()
	delete(r.cache, pid)
	r.mu.Unlock()
}

// InvalidateAddr invalidates pid's cache only if addr falls outside
// every currently cached mapping — i.e., a new mmap extended the
// process's address space. Cheap no-op otherwise.
func (r *Resolver) InvalidateAddr(pid uint32, addr uint64) {
	if _, ok := r.Lookup(pid, addr); ok {
		return
	}
	r.Invalidate(pid)
}

// Close releases internal state. Currently a no-op — Resolver holds
// no OS handles — but exposed for symmetry with other lifecycle-bearing
// types and future extension.
func (r *Resolver) Close() {
	r.mu.Lock()
	r.cache = map[uint32]*pidEntry{}
	r.mu.Unlock()
}

// entryFor returns the per-PID entry, creating it under the write
// lock if absent. The caller runs the entry's sync.Once to do the
// actual /proc parse; this method is purely the intern step.
func (r *Resolver) entryFor(pid uint32) *pidEntry {
	r.mu.RLock()
	e, ok := r.cache[pid]
	r.mu.RUnlock()
	if ok {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok = r.cache[pid]; ok {
		return e
	}
	e = &pidEntry{}
	r.cache[pid] = e
	return e
}

// populate reads /proc/<pid>/maps, fills entry.mappings, and attaches
// build-ids. Missing PIDs are cached as empty (entry.err==nil,
// entry.mappings==nil) so subsequent Lookups fast-fail.
func (r *Resolver) populate(entry *pidEntry, pid uint32) {
	path := filepath.Join(r.procRoot, fmt.Sprint(pid), "maps")
	mappings, err := parseMapsFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// PID gone — cache empty, no error surfaced to caller.
			return
		}
		entry.err = err
		return
	}

	for i := range mappings {
		mappings[i].BuildID = r.buildIDFor(mappings[i].Path)
	}
	entry.mappings = mappings
}

// buildIDFor returns a cached hex build-id for path, reading the ELF
// on first call. Read failures produce an empty string (cached) —
// a missing build-id is not a Lookup failure.
func (r *Resolver) buildIDFor(path string) string {
	if v, ok := r.buildIDs.Load(path); ok {
		return v.(string)
	}
	id, _ := readBuildID(path)
	actual, _ := r.buildIDs.LoadOrStore(path, id)
	return actual.(string)
}
```

- [ ] **Step 6: Run to verify all Resolver tests pass**

```bash
go test ./unwind/procmap/ -v
```

Expected: all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add unwind/procmap/procmap.go unwind/procmap/resolver.go unwind/procmap/procmap_test.go unwind/procmap/testdata/proc/5555
git commit -m "procmap: Resolver with lazy per-PID cache, Lookup, Invalidate"
```

---

## Task 4: `unwind/procmap` — `InvalidateAddr` observable re-parse

**Files:**
- Modify: `unwind/procmap/resolver.go`
- Modify: `unwind/procmap/procmap_test.go`
- Create: `unwind/procmap/testdata/proc/7777/maps`

**Context:** Task 3 added `InvalidateAddr` but our test can't observe the re-parse. We add a tiny internal counter so tests can verify that `InvalidateAddr(pid, addr_in_known_range)` is a no-op while `InvalidateAddr(pid, addr_outside_all_ranges)` triggers a re-parse.

- [ ] **Step 1: Create fixture**

```bash
mkdir -p unwind/procmap/testdata/proc/7777
cat > unwind/procmap/testdata/proc/7777/maps <<'EOF'
00700000-00720000 r-xp 00000000 fd:01 1111111                            /usr/bin/seven
EOF
```

- [ ] **Step 2: Write failing test**

Append to `unwind/procmap/procmap_test.go`:

```go
func TestResolverInvalidateAddrNoOpInRange(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, _ = r.Lookup(7777, 0x00701000) // populate
	before := r.populateCountForTest(7777)

	r.InvalidateAddr(7777, 0x00701000) // in-range → no-op
	after := r.populateCountForTest(7777)

	if after != before {
		t.Fatalf("populate count changed %d → %d after in-range InvalidateAddr", before, after)
	}
}

func TestResolverInvalidateAddrOutOfRangeForcesReparse(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, _ = r.Lookup(7777, 0x00701000) // populate
	before := r.populateCountForTest(7777)

	r.InvalidateAddr(7777, 0xdeadbeef) // out-of-range → evict
	_, _ = r.Lookup(7777, 0x00701000)  // re-populate
	after := r.populateCountForTest(7777)

	if after != before+1 {
		t.Fatalf("expected 1 re-populate, got %d → %d", before, after)
	}
}
```

- [ ] **Step 3: Run to verify failure**

```bash
go test ./unwind/procmap/ -run TestResolverInvalidateAddr -v
```

Expected: FAIL with "undefined: populateCountForTest".

- [ ] **Step 4: Add test-only counter to `resolver.go`**

In `unwind/procmap/resolver.go`, add a counter map and increment it at the end of `populate`:

```go
// Add field to Resolver struct:
//   populateCounts sync.Map // for tests; uint32(pid) -> *int64

// Add to NewResolver: (populateCounts is zero-value ready; no init needed)

// At the end of populate(), after entry.mappings is set:
func (r *Resolver) populate(entry *pidEntry, pid uint32) {
	// ... existing body ...
	r.bumpPopulateCount(pid)
}

// New helpers:
func (r *Resolver) bumpPopulateCount(pid uint32) {
	v, _ := r.populateCounts.LoadOrStore(pid, new(int64))
	*v.(*int64)++
}

// populateCountForTest returns the number of times populate ran for
// pid. Exported for tests in the same package only (unexported name
// — no external callers).
func (r *Resolver) populateCountForTest(pid uint32) int64 {
	v, ok := r.populateCounts.Load(pid)
	if !ok {
		return 0
	}
	return *v.(*int64)
}
```

Wire `populateCounts sync.Map` into the Resolver struct and call `r.bumpPopulateCount(pid)` as the last line of `populate`.

- [ ] **Step 5: Run to verify passes**

```bash
go test ./unwind/procmap/ -v
```

Expected: all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add unwind/procmap/resolver.go unwind/procmap/procmap_test.go unwind/procmap/testdata/proc/7777
git commit -m "procmap: InvalidateAddr no-op in-range, evict out-of-range"
```

---

## Task 5: `pprof.Frame` additions + intern key types

**Files:**
- Modify: `pprof/pprof.go`
- Create: `pprof/pprof_test.go` (if absent)

**Context:** Additive fields to `Frame`; new intern key types alongside the existing `frameKey`/`functionKey`. Zero-valued defaults preserve back-compat — existing callers that construct `Frame{Name:...}` still produce a valid frame. Builder logic changes come in Tasks 6 and 7.

- [ ] **Step 1: Write failing compile-level test**

Create or append to `pprof/pprof_test.go`:

```go
package pprof

import "testing"

func TestFrameHasAddressFields(t *testing.T) {
	f := Frame{
		Name:     "foo",
		Address:  0xdeadbeef,
		BuildID:  "abc123",
		MapStart: 0x400000,
		MapLimit: 0x500000,
		MapOff:   0x1000,
		IsKernel: false,
	}
	if f.Address != 0xdeadbeef {
		t.Fatalf("Address round-trip failed: %x", f.Address)
	}
	if f.BuildID != "abc123" {
		t.Fatalf("BuildID round-trip failed: %q", f.BuildID)
	}
}

func TestInternKeyTypesDeclared(t *testing.T) {
	// Compile-time checks: these types must exist with the documented shape.
	var _ = mappingKey{Path: "p", Start: 1, Limit: 2, Off: 3, BuildID: "b"}
	var _ = locationKey{MappingID: 1, Address: 0x400}
	var _ = locationFallbackKey{Name: "n", File: "f", Module: "m", Line: 10}
	var _ = functionKey{MappingID: 1, Name: "n"}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./pprof/ -run TestFrameHasAddressFields -v
```

Expected: FAIL with "unknown field Address" / "undefined: mappingKey".

- [ ] **Step 3: Modify `pprof/pprof.go` — extend Frame and add intern keys**

Replace the existing `Frame` struct:

```go
// Frame is a single symbolized stack frame. Name is always populated;
// other fields are filled when the symbolizer (DWARF for native
// binaries, perf-map decoding for Python/Node runtimes) or the
// Resolver can provide them. Address carries the absolute PC from
// the BPF stack so Locations stay distinguishable across samples
// that symbolize to the same (file,line,func).
type Frame struct {
	Name   string
	File   string
	Line   uint32
	Module string

	Address  uint64
	BuildID  string
	MapStart uint64
	MapLimit uint64
	MapOff   uint64
	IsKernel bool
}
```

Replace the existing `frameKey` and `functionKey` definitions with:

```go
// mappingKey interns per-binary pprof.Mapping entries. Two mappings
// with the same backing file but different load addresses (e.g., the
// same libc mapped into two PIDs with different ASLR slides) intern
// separately — pprof.Mapping's Start/Limit are absolute VAs.
type mappingKey struct {
	Path    string
	Start   uint64
	Limit   uint64
	Off     uint64
	BuildID string
}

// locationKey is the primary Location intern key. MappingID scopes
// Address to one binary so the same offset in two loaded copies
// dedups independently.
type locationKey struct {
	MappingID uint64
	Address   uint64 // binary-relative file offset (Address - MapStart + MapOff)
}

// locationFallbackKey is used when Address==0 (JIT runtime frames) or
// when the Resolver can't attribute the PC to any mapping. Falls back
// to the pre-S9 name/file/line dedup scheme.
type locationFallbackKey struct {
	Name, File, Module string
	Line               uint32
}

// functionKey interns pprof.Function entries per (binary, name).
// Adding MappingID means the same symbol name in two binaries (e.g.,
// main.main in a tool binary and a subprocess) produces two separate
// Functions — pprof-correct, but changes existing output fidelity.
type functionKey struct {
	MappingID uint64
	Name      string
}
```

- [ ] **Step 4: Delete the now-unused `locationKey` method receivers on Frame**

The `Frame.locationKey()` and `Frame.functionKey()` helper methods no longer return the right shape and are replaced by explicit construction in Task 6–7. Remove them:

```go
// Delete these two methods from pprof/pprof.go:
//   func (f Frame) locationKey() frameKey { ... }
//   func (f Frame) functionKey() frameKey { ... }
```

Also delete the old `frameKey` type since it's replaced by `locationKey` + `locationFallbackKey`.

- [ ] **Step 5: Update `ProfileBuilder` field types**

Change the `ProfileBuilder` struct so `locations` and `functions` map types align with the new keys. For now, keep the existing call sites compiling — Task 6 rewires `addLocation`. Minimal shim:

```go
type ProfileBuilder struct {
	// S9: maps keyed by any/interface{} so Task 5 keeps things compiling
	// while Task 6 replaces the actual key construction. (The only call
	// sites are addLocation/addFunction, which Task 6 rewrites.)
	locations          map[any]*profile.Location
	functions          map[any]*profile.Function
	sampleHashToSample map[uint64]*profile.Sample
	Profile            *profile.Profile
	tmpLocations       []*profile.Location
	tmpLocationIDs     []uint64
}
```

Update `BuilderForSample`'s struct literal to match (`make(map[any]*profile.Location)`, `make(map[any]*profile.Function)`).

Update `addLocation` and `addFunction` to cast through `any`:

```go
func (p *ProfileBuilder) addLocation(frame Frame) *profile.Location {
	frame = decodePerfMapFrame(frame)
	key := locationFallbackKey{
		Name: frame.Name, File: frame.File, Module: frame.Module, Line: frame.Line,
	}
	if loc, ok := p.locations[key]; ok {
		return loc
	}
	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: p.Profile.Mapping[0],
		Line: []profile.Line{{
			Function: p.addFunction(frame, p.Profile.Mapping[0].ID),
			Line:     int64(frame.Line),
		}},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}

func (p *ProfileBuilder) addFunction(frame Frame, mappingID uint64) *profile.Function {
	key := functionKey{MappingID: mappingID, Name: frame.Name}
	if f, ok := p.functions[key]; ok {
		return f
	}
	id := uint64(len(p.Profile.Function) + 1)
	f := &profile.Function{
		ID:       id,
		Name:     frame.Name,
		Filename: frame.File,
	}
	p.Profile.Function = append(p.Profile.Function, f)
	p.functions[key] = f
	return f
}
```

- [ ] **Step 6: Run to verify Task 5 tests pass and the package compiles**

```bash
go build ./...
go test ./pprof/ -v
```

Expected: build succeeds, all pprof tests pass. Existing callers still work — this is the fallback path, identical to pre-S9 behavior with dress-up.

- [ ] **Step 7: Commit**

```bash
git add pprof/pprof.go pprof/pprof_test.go
git commit -m "pprof: extend Frame with Address/mapping fields; new intern key types"
```

---

## Task 6: `pprof.BuildersOptions.Resolver` + fallback path preserved

**Files:**
- Modify: `pprof/pprof.go`
- Modify: `pprof/pprof_test.go`

**Context:** Add the `Resolver` field to `BuildersOptions`. When nil, behavior stays identical to Task 5. When non-nil, `addLocation` has a new primary branch (Task 7 implements it fully) — this task just adds the plumbing and asserts nil-path behavior.

- [ ] **Step 1: Write failing test**

Append to `pprof/pprof_test.go`:

```go
func TestBuildersOptionsResolverNilFallback(t *testing.T) {
	// With Resolver==nil, two Frames differing only in Address still
	// dedup to one Location (fallback path key has no Address).
	bs := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	s1 := &ProfileSample{Pid: 42, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "foo", File: "f.go", Line: 10, Address: 0x1000},
	}}
	s2 := &ProfileSample{Pid: 42, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "foo", File: "f.go", Line: 10, Address: 0x2000},
	}}
	bs.AddSample(s1)
	bs.AddSample(s2)

	b := bs.Builders[builderHashKey{sampleType: SampleTypeCpu}]
	if got := len(b.Profile.Location); got != 1 {
		t.Fatalf("Resolver=nil: expected 1 Location (fallback dedup), got %d", got)
	}
}
```

- [ ] **Step 2: Run — will pass trivially because Resolver doesn't exist yet as a field**

```bash
go test ./pprof/ -run TestBuildersOptionsResolverNilFallback -v
```

Expected: FAIL only if the field is misnamed; otherwise we might need to import the procmap package to even reference it. Let's force the failure by adding the field next.

- [ ] **Step 3: Add `Resolver` to `BuildersOptions`**

In `pprof/pprof.go`:

```go
import (
	// ... existing imports ...
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

type BuildersOptions struct {
	SampleRate    int64
	PerPIDProfile bool
	Comments      []string
	Resolver      *procmap.Resolver // nil → fallback to name-based Location dedup
}
```

Plumb it through `NewProfileBuilders` and `BuilderForSample`:

```go
type ProfileBuilders struct {
	Builders map[builderHashKey]*ProfileBuilder
	opt      BuildersOptions
}

// NewProfileBuilders stays unchanged — ProfileBuilders already stores opt.
// BuilderForSample propagates opt.Resolver into the new ProfileBuilder:

func (b *ProfileBuilders) BuilderForSample(sample *ProfileSample) *ProfileBuilder {
	// ... existing dispatch unchanged up to builder creation ...
	builder := &ProfileBuilder{
		resolver:           b.opt.Resolver,
		locations:          make(map[any]*profile.Location),
		functions:          make(map[any]*profile.Function),
		sampleHashToSample: make(map[uint64]*profile.Sample),
		Profile:            /* unchanged */,
		tmpLocationIDs:     make([]uint64, 0, 128),
		tmpLocations:       make([]*profile.Location, 0, 128),
	}
	// ... rest unchanged ...
}
```

Add `resolver *procmap.Resolver` as a field on `ProfileBuilder`.

- [ ] **Step 4: Thread `sample.Pid` into `addLocation`**

`addLocation` needs the PID to query the Resolver. Update signatures:

```go
func (p *ProfileBuilder) CreateSample(inputSample *ProfileSample) {
	sample := p.newSample(inputSample)
	p.addValue(inputSample, sample)
	for i, f := range inputSample.Stack {
		sample.Location[i] = p.addLocation(f, inputSample.Pid)
	}
	p.Profile.Sample = append(p.Profile.Sample, sample)
}

func (p *ProfileBuilder) CreateSampleOrAddValue(inputSample *ProfileSample) {
	p.tmpLocations = p.tmpLocations[:0]
	p.tmpLocationIDs = p.tmpLocationIDs[:0]
	for _, f := range inputSample.Stack {
		loc := p.addLocation(f, inputSample.Pid)
		p.tmpLocations = append(p.tmpLocations, loc)
		p.tmpLocationIDs = append(p.tmpLocationIDs, loc.ID)
	}
	h := xxhash.Sum64(uint64Bytes(p.tmpLocationIDs))
	sample := p.sampleHashToSample[h]
	if sample != nil {
		p.addValue(inputSample, sample)
		return
	}
	sample = p.newSample(inputSample)
	p.addValue(inputSample, sample)
	copy(sample.Location, p.tmpLocations)
	p.sampleHashToSample[h] = sample
	p.Profile.Sample = append(p.Profile.Sample, sample)
}

func (p *ProfileBuilder) addLocation(frame Frame, pid uint32) *profile.Location {
	frame = decodePerfMapFrame(frame)

	// S9: Resolver-driven path comes in Task 7. For now, when resolver
	// is nil OR not yet wired, take the fallback route.
	_ = pid
	key := locationFallbackKey{
		Name: frame.Name, File: frame.File, Module: frame.Module, Line: frame.Line,
	}
	if loc, ok := p.locations[key]; ok {
		return loc
	}
	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: p.Profile.Mapping[0],
		Line: []profile.Line{{
			Function: p.addFunction(frame, p.Profile.Mapping[0].ID),
			Line:     int64(frame.Line),
		}},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}
```

- [ ] **Step 5: Run to verify Task 5 + Task 6 tests pass and build is green**

```bash
go build ./...
go test ./pprof/ -v
```

Expected: all tests PASS, including `TestBuildersOptionsResolverNilFallback` (fallback dedup behavior).

- [ ] **Step 6: Commit**

```bash
git add pprof/pprof.go pprof/pprof_test.go
git commit -m "pprof: wire Resolver option + thread Pid into addLocation (fallback path)"
```

---

## Task 7: `pprof` — Resolver-driven Location/Mapping path + sentinels

**Files:**
- Modify: `pprof/pprof.go`
- Modify: `pprof/pprof_test.go`

**Context:** This is the behavior change. When `resolver != nil`, `addLocation` calls `resolver.Lookup(pid, addr)`. On hit: intern a real `pprof.Mapping` and address-keyed Location. On miss or `Address==0`: fall back to the Task 6 path. Add kernel and JIT sentinel mappings.

- [ ] **Step 1: Write failing test**

Append to `pprof/pprof_test.go`:

```go
import (
	"path/filepath"
	// ... existing imports ...

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestAddLocationAddressKeyed(t *testing.T) {
	// Use the Task 1 fixture: PID 4242, /usr/bin/target at 0x00400000-0x00420000.
	resolver := procmap.NewResolver(procmap.WithProcRoot(
		filepath.Join("..", "unwind", "procmap", "testdata", "proc")))
	defer resolver.Close()

	bs := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Resolver:   resolver,
	})

	// Two samples with same (func, file, line) but different Address.
	s1 := &ProfileSample{Pid: 4242, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "foo", File: "f.go", Line: 10, Address: 0x00401000},
	}}
	s2 := &ProfileSample{Pid: 4242, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "foo", File: "f.go", Line: 10, Address: 0x00402000},
	}}
	bs.AddSample(s1)
	bs.AddSample(s2)

	b := bs.Builders[builderHashKey{sampleType: SampleTypeCpu}]
	if got := len(b.Profile.Location); got != 2 {
		t.Fatalf("Resolver set + distinct Addresses: expected 2 Locations, got %d", got)
	}
	// Both Locations should attach to the /usr/bin/target Mapping.
	for _, loc := range b.Profile.Location {
		if loc.Mapping == nil || loc.Mapping.File != "/usr/bin/target" {
			t.Errorf("Location.Mapping wrong: %+v", loc.Mapping)
		}
	}
}

func TestAddLocationResolverMissFallback(t *testing.T) {
	resolver := procmap.NewResolver(procmap.WithProcRoot(
		filepath.Join("..", "unwind", "procmap", "testdata", "proc")))
	defer resolver.Close()

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 99, Resolver: resolver})
	// PID 9999 has no fixture → resolver Lookup misses.
	s := &ProfileSample{Pid: 9999, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "orphan", File: "o.go", Line: 5, Address: 0xdeadbeef},
	}}
	bs.AddSample(s)

	b := bs.Builders[builderHashKey{sampleType: SampleTypeCpu}]
	if got := len(b.Profile.Location); got != 1 {
		t.Fatalf("miss → fallback: expected 1 Location, got %d", got)
	}
	// Fallback attaches to the default single mapping.
	if b.Profile.Location[0].Mapping == nil {
		t.Fatal("Location.Mapping nil on fallback path")
	}
}

func TestKernelFrameUsesSentinel(t *testing.T) {
	resolver := procmap.NewResolver(procmap.WithProcRoot(
		filepath.Join("..", "unwind", "procmap", "testdata", "proc")))
	defer resolver.Close()

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 99, Resolver: resolver})
	bs.AddSample(&ProfileSample{Pid: 4242, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "schedule", IsKernel: true, Address: 0xffffffff80000000},
	}})
	bs.AddSample(&ProfileSample{Pid: 5555, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "schedule", IsKernel: true, Address: 0xffffffff80000100},
	}})

	b := bs.Builders[builderHashKey{sampleType: SampleTypeCpu}]
	// Exactly one kernel mapping interned, regardless of PID.
	var kernelCount int
	for _, m := range b.Profile.Mapping {
		if m.File == "[kernel]" {
			kernelCount++
		}
	}
	if kernelCount != 1 {
		t.Fatalf("expected 1 [kernel] mapping, got %d", kernelCount)
	}
}

func TestMappingFlags(t *testing.T) {
	resolver := procmap.NewResolver(procmap.WithProcRoot(
		filepath.Join("..", "unwind", "procmap", "testdata", "proc")))
	defer resolver.Close()

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 99, Resolver: resolver})
	bs.AddSample(&ProfileSample{Pid: 4242, SampleType: SampleTypeCpu, Value: 1, Stack: []Frame{
		{Name: "foo", File: "f.go", Line: 10, Address: 0x00401000},
	}})

	b := bs.Builders[builderHashKey{sampleType: SampleTypeCpu}]
	var target *profile.Mapping
	for _, m := range b.Profile.Mapping {
		if m.File == "/usr/bin/target" {
			target = m
		}
	}
	if target == nil {
		t.Fatal("target mapping not interned")
	}
	if !target.HasFunctions || !target.HasFilenames || !target.HasLineNumbers {
		t.Errorf("expected all Has* flags true, got funcs=%v files=%v lines=%v",
			target.HasFunctions, target.HasFilenames, target.HasLineNumbers)
	}
}
```

- [ ] **Step 2: Run to verify failures**

```bash
go test ./pprof/ -run 'TestAddLocationAddressKeyed|TestAddLocationResolverMissFallback|TestKernelFrameUsesSentinel|TestMappingFlags' -v
```

Expected: FAIL (each test fails because `addLocation` still takes the fallback-only path).

- [ ] **Step 3: Rewrite `addLocation` with the resolver-driven path**

Replace the `addLocation` body from Task 6 with the full logic:

```go
var (
	kernelSentinel = procmap.Mapping{Path: "[kernel]"}
	jitSentinel    = procmap.Mapping{Path: "[jit]"}
)

func (p *ProfileBuilder) addLocation(frame Frame, pid uint32) *profile.Location {
	frame = decodePerfMapFrame(frame)

	// 1. Kernel frames use a shared sentinel mapping regardless of PID.
	if frame.IsKernel {
		mapping := p.addMapping(kernelSentinel, frame)
		return p.addLocationByAddr(mapping, frame)
	}

	// 2. Perf-map runtime frames (Python/Node JIT): decodePerfMapFrame
	// sets Address=0 when the format is recognized (Task 8 changes there).
	// They intern under [jit] sentinel with fallback key.
	if frame.Address == 0 && looksJIT(frame) {
		mapping := p.addMapping(jitSentinel, frame)
		return p.addLocationByFallback(mapping, frame)
	}

	// 3. Resolver-driven primary path.
	if frame.Address != 0 && p.resolver != nil {
		if m, ok := p.resolver.Lookup(pid, frame.Address); ok {
			frame.MapStart = m.Start
			frame.MapLimit = m.Limit
			frame.MapOff = m.Offset
			frame.BuildID = m.BuildID
			mapping := p.addMapping(m, frame)
			return p.addLocationByAddr(mapping, frame)
		}
	}

	// 4. Fallback: name-based dedup on the default single mapping.
	return p.addLocationByFallback(p.Profile.Mapping[0], frame)
}

// looksJIT returns true when Frame.Module (or Name) matches the known
// perf-map runtime prefixes. Kept conservative — only recognized
// runtimes get the [jit] sentinel; everything else uses the generic
// fallback mapping.
func looksJIT(f Frame) bool {
	if strings.HasPrefix(f.Name, "py::") || strings.HasPrefix(f.Name, "JS:") ||
		strings.HasPrefix(f.Name, "LazyCompile:") || strings.HasPrefix(f.Name, "Function:") ||
		strings.HasPrefix(f.Name, "Builtin:") || strings.HasPrefix(f.Name, "Code:") ||
		strings.HasPrefix(f.Name, "Script:") {
		return true
	}
	return false
}

func (p *ProfileBuilder) addMapping(m procmap.Mapping, frame Frame) *profile.Mapping {
	key := mappingKey{
		Path: m.Path, Start: m.Start, Limit: m.Limit, Off: m.Offset, BuildID: m.BuildID,
	}
	if existing, ok := p.mappings[key]; ok {
		p.updateMappingFlags(existing, frame)
		return existing
	}
	id := uint64(len(p.Profile.Mapping) + 1)
	mapping := &profile.Mapping{
		ID:      id,
		Start:   m.Start,
		Limit:   m.Limit,
		Offset:  m.Offset,
		File:    m.Path,
		BuildID: m.BuildID,
	}
	p.updateMappingFlags(mapping, frame)
	p.Profile.Mapping = append(p.Profile.Mapping, mapping)
	p.mappings[key] = mapping
	return mapping
}

func (p *ProfileBuilder) updateMappingFlags(m *profile.Mapping, f Frame) {
	if f.Name != "" {
		m.HasFunctions = true
	}
	if f.File != "" {
		m.HasFilenames = true
	}
	if f.Line > 0 {
		m.HasLineNumbers = true
	}
}

func (p *ProfileBuilder) addLocationByAddr(mapping *profile.Mapping, frame Frame) *profile.Location {
	var offset uint64
	if frame.MapStart != 0 {
		offset = frame.Address - frame.MapStart + frame.MapOff
	} else {
		offset = frame.Address
	}
	key := locationKey{MappingID: mapping.ID, Address: offset}
	if loc, ok := p.locations[key]; ok {
		return loc
	}
	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: mapping,
		Address: offset,
		Line: []profile.Line{{
			Function: p.addFunction(frame, mapping.ID),
			Line:     int64(frame.Line),
		}},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}

func (p *ProfileBuilder) addLocationByFallback(mapping *profile.Mapping, frame Frame) *profile.Location {
	key := locationFallbackKey{
		Name: frame.Name, File: frame.File, Module: frame.Module, Line: frame.Line,
	}
	if loc, ok := p.locations[key]; ok {
		return loc
	}
	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: mapping,
		Line: []profile.Line{{
			Function: p.addFunction(frame, mapping.ID),
			Line:     int64(frame.Line),
		}},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}
```

Also add a `mappings` map to `ProfileBuilder`:

```go
type ProfileBuilder struct {
	resolver           *procmap.Resolver
	mappings           map[mappingKey]*profile.Mapping
	locations          map[any]*profile.Location
	functions          map[any]*profile.Function
	sampleHashToSample map[uint64]*profile.Sample
	Profile            *profile.Profile
	tmpLocations       []*profile.Location
	tmpLocationIDs     []uint64
}
```

Initialize `mappings: make(map[mappingKey]*profile.Mapping)` in `BuilderForSample`.

Add `"strings"` to the imports if not already present.

- [ ] **Step 4: Run to verify all Task 7 tests pass**

```bash
go test ./pprof/ -v
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pprof/pprof.go pprof/pprof_test.go
git commit -m "pprof: resolver-driven address-keyed Locations + kernel/jit sentinels"
```

---

## Task 8: Thread `Address` through FP `blazeSymToFrames`

**Files:**
- Modify: `profile/profiler.go`
- Modify: `pprof/pprof.go` (zero `Address` in `decodePerfMapFrame`)

**Context:** `profile/profiler.go` already extracts IPs via `bpfstack.ExtractIPs(stack)` (line 215). We zip them with the returned `symbols` and pass each IP into `blazeSymToFrames`. Additionally, `decodePerfMapFrame` zeroes `Address` when it detects a JIT runtime format (consumed by Task 7's `looksJIT` sentinel path).

- [ ] **Step 1: Write failing test**

Create/append to `profile/profiler_test.go`:

```go
package profile

import (
	"testing"

	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
)

func TestBlazeSymToFramesAddress(t *testing.T) {
	s := blazesym.Sym{
		Name:   "foo",
		Module: "/usr/bin/target",
	}
	frames := blazeSymToFrames(s, 0xdeadbeef)

	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Address != 0xdeadbeef {
		t.Fatalf("expected Address=0xdeadbeef, got %#x", frames[0].Address)
	}
	if frames[0].Name != "foo" {
		t.Errorf("Name mismatch: %q", frames[0].Name)
	}
	_ = pprof.Frame{} // keep import
}

func TestBlazeSymToFramesInlineSharesAddress(t *testing.T) {
	s := blazesym.Sym{
		Name:    "outer",
		Module:  "/usr/bin/target",
		Inlined: []blazesym.InlinedFn{{Name: "inner"}},
	}
	frames := blazeSymToFrames(s, 0x4000)

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames (1 inline + 1 outer), got %d", len(frames))
	}
	for i, f := range frames {
		if f.Address != 0x4000 {
			t.Errorf("frame %d Address=%#x, want 0x4000", i, f.Address)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./profile/ -run TestBlazeSymToFrames -v
```

Expected: FAIL with "too many arguments in call to blazeSymToFrames".

- [ ] **Step 3: Update `profile/profiler.go` `blazeSymToFrames`**

Replace the function (around line 53):

```go
// blazeSymToFrames converts a blazesym.Sym into one or more pprof.Frames.
// addr is the absolute PC from the BPF stack — it is copied onto every
// frame (inlined chain + outer real function) so pprof Locations stay
// distinguishable when two PCs symbolize to the same (file, line, func).
//
// blazesym reports Inlined in outer→inner order (see
// blazesym/src/symbolize/mod.rs:408), so we walk it in reverse to get
// leaf-first output.
func blazeSymToFrames(s blazesym.Sym, addr uint64) []pprof.Frame {
	out := make([]pprof.Frame, 0, 1+len(s.Inlined))
	for i := len(s.Inlined) - 1; i >= 0; i-- {
		in := s.Inlined[i]
		f := pprof.Frame{Name: in.Name, Module: s.Module, Address: addr}
		if in.CodeInfo != nil {
			f.File = in.CodeInfo.File
			f.Line = in.CodeInfo.Line
		}
		out = append(out, f)
	}
	outer := pprof.Frame{Name: s.Name, Module: s.Module, Address: addr}
	if s.CodeInfo != nil {
		outer.File = s.CodeInfo.File
		outer.Line = s.CodeInfo.Line
	}
	out = append(out, outer)
	return out
}
```

- [ ] **Step 4: Update the call site in `Collect`**

In `profile/profiler.go` around line 216–232:

```go
ips := bpfstack.ExtractIPs(stack)
if len(ips) > 0 {
	symbols, err := pr.symbolizer.SymbolizeProcessAbsAddrs(
		ips,
		samplePid,
		blazesym.ProcessSourceWithPerfMap(true),
		blazesym.ProcessSourceWithDebugSyms(true),
	)
	if err != nil {
		log.Printf("Failed to symbolize: %v", err)
	} else {
		// symbols and ips are parallel — one Sym per IP.
		for i, s := range symbols {
			if i >= len(ips) {
				break
			}
			for _, f := range blazeSymToFrames(s, ips[i]) {
				sb.append(f)
			}
		}
	}
}
```

- [ ] **Step 5: Zero `Address` in `decodePerfMapFrame` for JIT formats**

In `pprof/pprof.go`, update `decodePerfMapFrame`:

```go
func decodePerfMapFrame(f Frame) Frame {
	if f.File != "" || f.Name == "" {
		return f
	}
	if dec, ok := decodePython(f.Name); ok {
		f = mergeFrame(f, dec)
		f.Address = 0 // JIT code — file-offset is meaningless
		return f
	}
	if dec, ok := decodeNode(f.Name); ok {
		f = mergeFrame(f, dec)
		f.Address = 0
		return f
	}
	return f
}
```

- [ ] **Step 6: Run to verify all tests pass**

```bash
go build ./...
go test ./pprof/ ./profile/ -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add profile/profiler.go pprof/pprof.go profile/profiler_test.go
git commit -m "profile: thread Address through blazeSymToFrames; zero for JIT frames"
```

---

## Task 9: Thread `Address` through DWARF `blazeSymToFrames`

**Files:**
- Modify: `unwind/dwarfagent/symbolize.go`

**Context:** `unwind/dwarfagent/symbolize.go` has its own `blazeSymToFrames` copy and a helper `symbolizePID`. Mirror Task 8's change.

- [ ] **Step 1: Write failing test**

Create `unwind/dwarfagent/symbolize_test.go`:

```go
package dwarfagent

import (
	"testing"

	blazesym "github.com/libbpf/blazesym/go"
)

func TestDwarfBlazeSymToFramesAddress(t *testing.T) {
	s := blazesym.Sym{Name: "bar", Module: "/lib/x.so"}
	frames := blazeSymToFrames(s, 0x1000)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Address != 0x1000 {
		t.Fatalf("expected Address=0x1000, got %#x", frames[0].Address)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./unwind/dwarfagent/ -run TestDwarfBlazeSymToFramesAddress -v
```

Expected: FAIL with "too many arguments".

- [ ] **Step 3: Update `unwind/dwarfagent/symbolize.go`**

```go
package dwarfagent

import (
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
)

// blazeSymToFrames converts one address's resolution (including
// inlined frames) into pprof.Frames in leaf-first order. addr is
// copied onto every frame so pprof Locations stay distinguishable.
func blazeSymToFrames(s blazesym.Sym, addr uint64) []pprof.Frame {
	out := make([]pprof.Frame, 0, 1+len(s.Inlined))
	for i := len(s.Inlined) - 1; i >= 0; i-- {
		in := s.Inlined[i]
		f := pprof.Frame{Name: in.Name, Module: s.Module, Address: addr}
		if in.CodeInfo != nil {
			f.File = in.CodeInfo.File
			f.Line = in.CodeInfo.Line
		}
		out = append(out, f)
	}
	outer := pprof.Frame{Name: s.Name, Module: s.Module, Address: addr}
	if s.CodeInfo != nil {
		outer.File = s.CodeInfo.File
		outer.Line = s.CodeInfo.Line
	}
	out = append(out, outer)
	return out
}

// symbolizePID resolves ips for pid and returns pprof frames in the
// same order as ips. Failed IPs contribute a single synthetic
// "[unknown]" frame carrying the original PC as Address.
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
		out := make([]pprof.Frame, len(ips))
		for i := range out {
			out[i] = pprof.Frame{Name: "[unknown]", Address: ips[i]}
		}
		return out
	}
	var out []pprof.Frame
	for i, s := range syms {
		var addr uint64
		if i < len(ips) {
			addr = ips[i]
		}
		out = append(out, blazeSymToFrames(s, addr)...)
	}
	return out
}
```

- [ ] **Step 4: Run to verify passes**

```bash
go build ./...
go test ./unwind/dwarfagent/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add unwind/dwarfagent/symbolize.go unwind/dwarfagent/symbolize_test.go
git commit -m "dwarfagent: thread Address through blazeSymToFrames/symbolizePID"
```

---

## Task 10: Wire `Resolver` into FP profiler

**Files:**
- Modify: `profile/profiler.go`

**Context:** `Profiler` gains a `resolver *procmap.Resolver` field, created in `NewProfiler`, passed into `BuildersOptions`, closed in `Close`.

- [ ] **Step 1: Write failing test**

Append to `profile/profiler_test.go`:

```go
import (
	// ... existing ...
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestProfilerHasResolver(t *testing.T) {
	// Compile-time check: Profiler has a resolver field. Actual wiring
	// verified by integration tests (Task 11 / Task 12).
	var p Profiler
	_ = p.resolver
	var _ *procmap.Resolver = p.resolver
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./profile/ -run TestProfilerHasResolver -v
```

Expected: FAIL with "p.resolver undefined".

- [ ] **Step 3: Modify `profile/profiler.go`**

Add import:

```go
import (
	// ... existing ...
	"github.com/dpsoft/perf-agent/unwind/procmap"
)
```

Extend `Profiler`:

```go
type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver
	perfEvents []*perfEvent
	tags       []string
	sampleRate int
}
```

In `NewProfiler`, initialize `resolver: procmap.NewResolver()`:

```go
return &Profiler{
	objs:       objs,
	symbolizer: symbolizer,
	resolver:   procmap.NewResolver(),
	perfEvents: perfEvents,
	tags:       tags,
	sampleRate: sampleRate,
}, nil
```

In `Close`, call `pr.resolver.Close()` (before closing BPF objs):

```go
func (pr *Profiler) Close() {
	pr.symbolizer.Close()
	pr.resolver.Close()
	for _, pe := range pr.perfEvents {
		_ = pe.Close()
	}
	_ = pr.objs.Close()
}
```

In `Collect`, pass the resolver into `BuildersOptions`:

```go
builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
	SampleRate:    int64(pr.sampleRate),
	PerPIDProfile: false,
	Comments:      pr.tags,
	Resolver:      pr.resolver,
})
```

- [ ] **Step 4: Run to verify passes**

```bash
go build ./...
go test ./profile/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add profile/profiler.go profile/profiler_test.go
git commit -m "profile: create Resolver in NewProfiler, pass into BuildersOptions"
```

---

## Task 11: Wire `Resolver` into DWARF session + observer-driven invalidation

**Files:**
- Modify: `unwind/dwarfagent/common.go`
- Modify: `unwind/ehmaps/tracker.go`
- Modify: `unwind/dwarfagent/agent.go`
- Modify: `unwind/dwarfagent/offcpu.go`

**Context:** `session` gains `resolver *procmap.Resolver`. `PIDTracker.Run` gains a variadic observer callback that runs before the tracker's own switch dispatch. The session registers one observer that invalidates the resolver on MMAP2/EXIT events. Both `Profiler` and `OffCPUProfiler` (which embed `*session`) pass the resolver into their respective `BuildersOptions` calls inside `collect`.

- [ ] **Step 1: Write failing test for observer callback**

Append to `unwind/ehmaps/tracker_test.go`:

```go
func TestPIDTrackerRunObserver(t *testing.T) {
	tracker, w, cleanup := newTestTrackerAndWatcher(t)
	defer cleanup()

	var got []MmapEventRecord
	observer := func(ev MmapEventRecord) {
		got = append(got, ev)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		tracker.Run(ctx, w, observer)
		close(done)
	}()

	// Inject a synthetic Exit event.
	w.feed(MmapEventRecord{Kind: ExitEvent, PID: 42, TID: 42})
	// Give the goroutine a moment to drain, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if len(got) != 1 || got[0].Kind != ExitEvent || got[0].PID != 42 {
		t.Fatalf("observer didn't see event, got=%+v", got)
	}
}
```

This test assumes helper `newTestTrackerAndWatcher` and a `fakeWatcher` with `feed(ev)` exist. If they don't, add a minimal fake:

```go
type fakeWatcher struct{ ch chan MmapEventRecord }

func (w *fakeWatcher) Events() <-chan MmapEventRecord { return w.ch }
func (w *fakeWatcher) feed(ev MmapEventRecord)        { w.ch <- ev }
func (w *fakeWatcher) Close() error                   { close(w.ch); return nil }

func newTestTrackerAndWatcher(t *testing.T) (*PIDTracker, *fakeWatcher, func()) {
	// Use a mock TableStore / BPF maps — tracker.Detach is a no-op on
	// missing PIDs so the observer path is exercised without real BPF.
	tracker := NewPIDTracker(nil, nil, nil)
	w := &fakeWatcher{ch: make(chan MmapEventRecord, 8)}
	return tracker, w, func() { _ = w.Close() }
}
```

Add the required `"time"` / `"context"` imports if not present.

- [ ] **Step 2: Run to verify failure**

```bash
go test ./unwind/ehmaps/ -run TestPIDTrackerRunObserver -v
```

Expected: FAIL — `tracker.Run` takes two args today, not three; or `NewPIDTracker` panics on nil maps.

- [ ] **Step 3: Make `tracker.Detach` tolerate nil maps for test**

In `unwind/ehmaps/tracker.go`, guard the BPF map calls in `Detach` with `if t.pidMappings != nil`. Similarly for `Attach` — if the test uses nil maps, the observer-only path needs to survive without actually writing. Simplest: add early returns in Detach when `t.pidMappings == nil`:

```go
func (t *PIDTracker) Detach(pid uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.perPID[pid]
	if !ok {
		return nil
	}
	delete(t.perPID, pid)
	if t.pidMappings != nil {
		_ = t.pidMappings.Delete(pid)
	}
	if t.pidMapLens != nil {
		_ = t.pidMapLens.Delete(pid)
	}
	if t.store != nil {
		for tableID := range state.tableIDs {
			_ = t.store.ReleaseBinary(tableID, pid)
		}
	}
	return nil
}
```

(If the existing `Detach` already handles this, skip. Check before editing.)

- [ ] **Step 4: Extend `PIDTracker.Run` signature with variadic observers**

In `unwind/ehmaps/tracker.go`:

```go
// Run blocks consuming events from the watcher until ctx is canceled
// or the watcher's event channel closes. Call from a goroutine.
//
// Observers (if any) run BEFORE the tracker's own dispatch for each
// event — they see every event including those the tracker itself
// would filter out. Used by dwarfagent.session to keep a procmap
// Resolver's cache in sync with MMAP2/EXIT events.
func (t *PIDTracker) Run(ctx context.Context, w mmapEventSource, observers ...func(MmapEventRecord)) {
	seen := map[uint32]map[string]struct{}{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			for _, obs := range observers {
				obs(ev)
			}
			switch ev.Kind {
				// ... existing switch body unchanged ...
			}
		}
	}
}
```

- [ ] **Step 5: Run to verify the observer test passes**

```bash
go test ./unwind/ehmaps/ -run TestPIDTrackerRunObserver -v
```

Expected: PASS.

- [ ] **Step 6: Wire the resolver into `dwarfagent.session`**

In `unwind/dwarfagent/common.go`:

```go
import (
	// ... existing ...
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

type session struct {
	pid  int
	tags []string

	objs       sessionObjs
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    mmapEventSourceCloser
	ringReader *ringbuf.Reader
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64
	stacks  map[sampleKey][]uint64
}
```

In `newSession`, initialize it right after the symbolizer:

```go
return &session{
	pid:        pid,
	tags:       tags,
	objs:       objs,
	store:      store,
	tracker:    tracker,
	watcher:    watcher,
	ringReader: rd,
	symbolizer: symbolizer,
	resolver:   procmap.NewResolver(),
	stop:       make(chan struct{}),
	samples:    map[sampleKey]uint64{},
}, nil
```

- [ ] **Step 7: Close the resolver in `session.close`**

Find `session.close` in `common.go`. Before closing `symbolizer`, call `resolver.Close`:

```go
func (s *session) close() error {
	// ... existing stop/cancel logic ...
	if s.resolver != nil {
		s.resolver.Close()
	}
	// ... existing symbolizer/ringReader/watcher/objs close logic ...
}
```

- [ ] **Step 8: Register the invalidation observer in `runTracker`**

Replace the body of `runTracker` so it passes an observer closure into `tracker.Run`:

```go
func (s *session) runTracker() {
	s.trackerWG.Add(1)
	go func() {
		defer s.trackerWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-s.stop
			cancel()
		}()

		observer := func(ev ehmaps.MmapEventRecord) {
			switch ev.Kind {
			case ehmaps.MmapEvent:
				s.resolver.InvalidateAddr(ev.PID, ev.Addr)
			case ehmaps.ExitEvent:
				if ev.TID == ev.PID {
					s.resolver.Invalidate(ev.PID)
				}
			}
		}
		s.tracker.Run(ctx, s.watcher, observer)
	}()
}
```

`MmapEventRecord` already has `Addr uint64` (`unwind/ehmaps/mmap_watcher.go:29`) populated by `parseMmap2` from the perf_event payload.

- [ ] **Step 9: Pass the resolver into `collect`**

Find `session.collect` (the method that builds the pprof). Change its `BuildersOptions` construction to include `Resolver: s.resolver`:

```go
builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
	SampleRate: sampleRate,
	Comments:   s.tags,
	Resolver:   s.resolver,
})
```

- [ ] **Step 10: Run to verify build + unit tests green**

```bash
go build ./...
go test ./unwind/... ./pprof/ ./profile/ -v
```

Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add unwind/ehmaps/tracker.go unwind/ehmaps/tracker_test.go unwind/dwarfagent/common.go
git commit -m "dwarfagent: observer-driven Resolver invalidation on MMAP2/EXIT"
```

---

## Task 12: Integration tests — assert pprof fidelity end-to-end

**Files:**
- Modify: `test/integration_test.go`

**Context:** Run the FP and DWARF profilers against the existing test workloads, then parse the output pprof and assert the new fidelity properties: ≥2 distinct non-sentinel Mappings, at least one non-empty BuildID, every user-space Location has a non-zero Address, `go tool pprof` can decode the file without error.

- [ ] **Step 1: Write failing test extension**

If `test/go.mod` doesn't already require `github.com/google/pprof`, add it:

```bash
cd test && go get github.com/google/pprof/profile
```

In `test/integration_test.go`, locate `TestPerfAgentSystemWideDwarfProfile` and `TestProfileMode`. After each existing success check, add a helper call:

```go
import (
	// ... existing ...
	"github.com/google/pprof/profile"
)

func assertPprofFidelity(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open profile: %v", err)
	}
	defer f.Close()

	p, err := profile.Parse(f)
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	if err := p.CheckValid(); err != nil {
		t.Fatalf("pprof invalid: %v", err)
	}

	// At least 2 non-sentinel mappings (target binary + at least one SO
	// like libc). The hardcoded placeholder ID=1 mapping with empty
	// File counts as zero.
	var real int
	var hasBuildID bool
	for _, m := range p.Mapping {
		if m.File != "" && m.File != "[kernel]" && m.File != "[jit]" {
			real++
		}
		if m.BuildID != "" {
			hasBuildID = true
		}
	}
	if real < 2 {
		t.Errorf("expected ≥2 real mappings, got %d: %+v", real, p.Mapping)
	}
	if !hasBuildID {
		t.Errorf("expected at least one mapping with non-empty BuildID")
	}

	// Every Location attached to a real mapping must have non-zero Address.
	for _, loc := range p.Location {
		if loc.Mapping == nil {
			continue
		}
		m := loc.Mapping
		if m.File == "" || m.File == "[kernel]" || m.File == "[jit]" {
			continue
		}
		if loc.Address == 0 {
			t.Errorf("Location %d in %s has Address=0", loc.ID, m.File)
		}
	}
}
```

In `TestProfileMode`, after the existing pprof file existence check, call `assertPprofFidelity(t, profilePath)`.

In `TestPerfAgentSystemWideDwarfProfile`, same — add `assertPprofFidelity(t, profilePath)` at the end.

- [ ] **Step 2: Run both tests to verify they fail for the right reason**

```bash
cd test && sudo -E \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -v -run 'TestProfileMode|TestPerfAgentSystemWideDwarfProfile' ./...
```

Expected: if Tasks 1–11 landed correctly, tests PASS (the fidelity is now real). If earlier tasks have gaps, these tests catch them. Resolve any failures by going back to the task that owns the broken behavior.

- [ ] **Step 3: Commit**

```bash
cd ..
git add test/integration_test.go
git commit -m "test: assert pprof fidelity (≥2 mappings, build-id, non-zero Address)"
```

---

## Task 13: Cleanup — delete `cmd/` diagnostic binaries

**Files:**
- Delete: `cmd/perf-dwarf-test/`
- Delete: `cmd/perfreader-test/`
- Delete: `cmd/test_blazesym/`

**Context:** These were scaffolding for S2/S3 development (raw sample dumpers, blazesym smoke tests). They are not referenced by the Makefile, CI, `main.go`, or any test. Removing the entire `cmd/` tree.

- [ ] **Step 1: Verify nothing references them**

```bash
grep -rn 'perf-dwarf-test\|perfreader-test\|test_blazesym' --include='*.go' --include='Makefile' --include='*.yml' .
```

Expected: matches only inside the files about to be deleted (if any).

- [ ] **Step 2: Remove the directories**

```bash
rm -rf cmd/perf-dwarf-test cmd/perfreader-test cmd/test_blazesym
# cmd/ is now empty
rmdir cmd
```

- [ ] **Step 3: Verify build still passes**

```bash
go build ./...
go vet ./...
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add -A cmd
git commit -m "Remove cmd/ diagnostic binaries (S2/S3 scaffolding)"
```

---

## Task 14: Cleanup — strip `SX` stage markers from comments

**Files:**
- Modify (comment edits only): `bpf/offcpu_dwarf.bpf.c`, `bpf/perf_dwarf.bpf.c`, `bpf/unwind_common.h`, `unwind/dwarfagent/sample.go`, `unwind/ehmaps/ehmaps.go`, `unwind/ehmaps/store.go`, `unwind/ehmaps/tracker.go`, `unwind/ehmaps/tracker_test.go`, `unwind/ehcompile/types.go`, `profile/perf_dwarf_test.go`, `perfagent/options.go`, `test/integration_test.go`.

**Context:** Stage markers (`S2`, `S3`, …) reference the internal implementation plan, which future readers shouldn't need to know about. Each comment gets rewritten to describe what the code does today, not when it arrived.

- [ ] **Step 1: Scan the current set of references**

```bash
grep -nE '\bS[2-9]\b' bpf/*.c bpf/*.h
grep -rnE '\bS[2-9]\b' --include='*.go' .
```

Note each reference. Expected ~20 across the files listed above.

- [ ] **Step 2: Edit each reference**

Walk through each hit and rewrite the sentence. Examples:

- `bpf/unwind_common.h:9` — `// emitted samples, PID filter, and (as of S3) the CFI + classification` → `// emitted samples, PID filter, CFI tables, and per-instruction classification`
- `bpf/unwind_common.h:41` — `// in S2; maps declared in S3.` → delete the clause.
- `bpf/unwind_common.h:165` — `// ----- CFI maps (S3).` → `// ----- CFI maps.`
- `bpf/unwind_common.h:260` — `// ----- Lookup helpers (S3).` → `// ----- Lookup helpers.`
- `bpf/unwind_common.h:418-419` — `// SAME_VALUE (leaf on arm64) or REGISTER — S3 doesn't track / non-FP registers, so stop. S6+ can extend.` → `// SAME_VALUE (leaf on arm64) or REGISTER — we don't track non-FP / registers, so stop.`
- `bpf/perf_dwarf.bpf.c:14` — `// S3 wires the hybrid walker; S4 adds MMAP2-driven mapping ingestion so` → `// Hybrid walker + MMAP2-driven mapping ingestion so`
- `bpf/offcpu_dwarf.bpf.c:3` — `// offcpu_dwarf.bpf.c — DWARF-capable off-CPU sampler (S6).` → `// offcpu_dwarf.bpf.c — DWARF-capable off-CPU sampler.`
- `bpf/offcpu_dwarf.bpf.c:6` — `// stack of tasks going off-CPU using the S3 hybrid walker (walk_step in` → `// stack of tasks going off-CPU using the hybrid walker (walk_step in`
- `unwind/dwarfagent/sample.go:1` — `// Package dwarfagent wires the S3 perf_dwarf BPF program, the S4` → `// Package dwarfagent wires the perf_dwarf BPF program, the ehmaps`
- `unwind/dwarfagent/sample.go:39` — `// than errored, matching the resilience posture of the S3 ringbuf` → `// than errored, matching the resilience posture of the ringbuf`
- `unwind/ehmaps/ehmaps.go:2-3` — `// maps from unwind/ehcompile output. S3 scope: pure population — no MMAP2 / ingestion, no refcounting, no munmap cleanup. S4 adds the lifecycle layer` → `// maps from unwind/ehcompile output. Handles population plus lifecycle / management via the refcounting layer in this package.`
- `unwind/ehmaps/store.go:63-64` — `// tracking with actual map population. It is the S4 replacement for the / hand-wired calls to PopulateCFI/PopulateClassification in S3's tests.` → `// tracking with actual map population. Wraps Populate{CFI,Classification} / with refcounting so callers don't hand-manage table lifetimes.`
- `unwind/ehmaps/tracker.go:21-23` — `// S4 scope: Attach is called once per binary in the target's address / space. Subsequent calls for the same PID with a different binPath / append to the pid_mappings array. The S4 integration test exercises` → `// Attach is called once per binary in the target's address / space. Subsequent calls for the same PID with a different binPath / append to the pid_mappings array. The integration test exercises`
- `unwind/ehmaps/tracker_test.go:54` — `// newTestMaps creates S3-shape BPF maps that mirror bpf2go's output.` → `// newTestMaps creates BPF maps shaped like bpf2go's output.`
- `unwind/ehmaps/tracker_test.go:91` — `// TestTrackerAutoAttachOnMmap exercises the full S4 flow: launch a` → `// TestTrackerAutoAttachOnMmap exercises the full MMAP2 flow: launch a`
- `unwind/ehcompile/types.go:82` — `// in S2) — keep in sync. Arch-neutral: the same struct serves x86_64` → `// — keep in sync with the BPF header. Arch-neutral: the same struct serves x86_64`
- `profile/perf_dwarf_test.go:14` — `// This is the S2 smoke test: it exercises nothing user-visible, but catches` → `// Smoke test for the perf_dwarf BPF program: exercises nothing user-visible but catches`
- `perfagent/options.go:55` — `// "dwarf" (DWARF CFI), "auto" (currently routes to fp; see S8 in` → `// "dwarf" (DWARF CFI), "auto" (aliases to "dwarf"; the DWARF walker already takes the FP path for FP-safe frames)`
- `test/integration_test.go:956` — `// TestPerfDwarfWalker drives the S3 DWARF-walker pipeline end-to-end: start` → `// TestPerfDwarfWalker drives the DWARF-walker pipeline end-to-end: start`
- `test/integration_test.go:1222` — `// TestPerfDwarfMmap2Tracking validates the S4 flow: after starting the` → `// TestPerfDwarfMmap2Tracking validates the MMAP2 flow: after starting the`

Also remove the file-header reference: `cmd/perf-dwarf-test/main.go:1` is irrelevant because Task 13 already deleted that file.

- [ ] **Step 3: Verify no stage markers remain**

```bash
grep -nE '\bS[2-9]\b' bpf/*.c bpf/*.h
grep -rnE '\bS[2-9]\b' --include='*.go' .
```

Expected: zero matches (or only matches inside plan/spec documents under `docs/`, which are historical — leave those alone).

- [ ] **Step 4: Verify build + tests still pass**

```bash
go build ./...
go test ./pprof/ ./profile/ ./unwind/... -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add bpf/ unwind/ profile/ perfagent/ test/integration_test.go
git commit -m "Strip SX stage markers from comments; describe behavior, not history"
```

---

## Final Verification

After all 14 tasks are committed, run the full integration suite:

```bash
make build && sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent

cd test && \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -v ./...
```

Expected: all integration tests PASS, including the extended fidelity assertions in Task 12.

**Smoke check on real output:**

```bash
./perf-agent --profile --pid $$ --duration 5s
go tool pprof -list '.*' profile-*.pb.gz 2>&1 | head -30
```

Expected: pprof lists symbols with source line numbers, and at least one real binary path appears in `-proto` output's `mapping:` entries with `build_id:` populated.
