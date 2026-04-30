# Python perf-trampoline injector — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--inject-python` flag to `perf-agent --profile` that, via `ptrace`, calls `sys.activate_stack_trampoline("perf")` inside running CPython 3.12+ processes at startup and `sys.deactivate_stack_trampoline()` at shutdown — so users get Python JIT names in their profiles without restarting targets with `python -X perf`.

**Architecture:** A new `inject/` package tree with three subpackages: `inject/elfsym/` (shared ELF symbol resolution — any compiled runtime), `inject/ptraceop/` (shared low-level ptrace primitives — any C-ABI runtime), and `inject/python/` (Python-specific detector + manager + payload). A `python.Manager` runs in `perfagent.Agent.Start()` to detect Python 3.12+ targets and call `ptraceop.Injector.RemoteActivate()`, which performs a three-call ptrace dance (`PyGILState_Ensure` → `PyRun_SimpleString` → `PyGILState_Release`) inside one ptrace session per target. The manager tracks activated PIDs in memory and runs a bounded 5-second deactivation pass on shutdown.

**Tech Stack:** Go 1.25+, `golang.org/x/sys/unix` for ptrace and `process_vm_writev`, `debug/elf` stdlib for symbol resolution, `log/slog` for structured logging. No new C dependencies. Tests use `testing` stdlib + the synthetic `/proc` and ELF fixture patterns established in PR #11.

**Worktree:** All work happens in `/home/diego/github/perf-agent/.worktrees/python-perf-injector` on branch `feat/python-perf-injector`. Each task starts with `cd /home/diego/github/perf-agent/.worktrees/python-perf-injector && git rev-parse --abbrev-ref HEAD` to verify the branch is `feat/python-perf-injector`.

**Modern Go conventions:** Use `t.Context()` for tests that need a context, `wg.Go()` instead of `wg.Add(1) + go func()`, `errors.Is` for sentinel errors, `for range N` for count-based loops, `strings.SplitSeq` for one-shot splits.

**CGO env (every test/build that touches blazesym):**
```
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
```

---

## Task overview

| # | Task | Files | Test type |
|---|---|---|---|
| 1 | Sentinel errors + payload encoding | `inject/python/{errors,payload}.go` + tests | unit |
| 2 | SONAME parsing | `inject/elfsym/soname.go` + test | unit |
| 3 | ELF symbol resolution | `inject/elfsym/elfsym.go` + test | unit |
| 4 | Detection ladder | `inject/python/detector.go` + test | unit |
| 5 | ptraceop arch-generic + amd64 | `inject/ptraceop/{ptraceop,regs_amd64}.go` + test | unit (frame setup) |
| 6 | ptraceop arm64 | `inject/ptraceop/regs_arm64.go` + test | unit |
| 7 | Manager: lifecycle, dedupe, stats | `inject/python/{python,manager}.go` + test | unit |
| 8 | Wire into perfagent.Agent | `perfagent/agent.go`, `perfagent/options.go`, `main.go` | unit + manual |
| 9 | Mmap-watcher new-exec hook | `unwind/ehmaps/tracker.go` + `perfagent/agent.go` | unit |
| 10 | Bench harness flag | `bench/cmd/scenario/main.go`, `bench/internal/schema/schema.go` | unit |
| 11 | Integration tests | `test/integration_inject_python_test.go` | caps-gated integration |
| 12 | Documentation | `README.md`, `docs/python-profiling.md` | n/a |

---

## Task 1: Sentinel errors + payload encoding

**Files:**
- Create: `inject/python/errors.go`
- Create: `inject/python/payload.go`
- Test: `inject/python/payload_test.go`

This task ships two small, self-contained files. Errors are referenced by all later tasks; payload encoding has no dependencies.

- [ ] **Step 1: Verify worktree and branch**

```bash
cd /home/diego/github/perf-agent/.worktrees/python-perf-injector
git rev-parse --abbrev-ref HEAD
```
Expected: `feat/python-perf-injector`

- [ ] **Step 2: Create the errors file**

Create `inject/python/errors.go`:

```go
// Package python implements injection of CPython 3.12+'s perf trampoline
// (sys.activate_stack_trampoline) into running processes via ptrace, so that
// perf-agent can resolve Python JIT frames to qualnames without requiring the
// target to be launched with `python -X perf`.
package python

import "errors"

// Detection-result sentinels. All are non-fatal: detection returns one of
// these (wrapped) when a process should be skipped without aborting the run.
var (
	// ErrNotPython is returned when the target is not a CPython process.
	// Examples: a Go binary, a non-Python executable, or a process whose
	// libpython/exe lacks the interpreter symbols we need.
	ErrNotPython = errors.New("not a python process")

	// ErrPythonTooOld is returned when a Python process is detected but its
	// libpython SONAME indicates a version older than 3.12. The
	// sys.activate_stack_trampoline primitive does not exist on older versions.
	ErrPythonTooOld = errors.New("python version too old (need 3.12+)")

	// ErrNoPerfTrampoline is returned when libpython is 3.12+ but was compiled
	// without --enable-perf-trampoline, detected by absence of the
	// _PyPerf_Callbacks symbol in .dynsym/.symtab.
	ErrNoPerfTrampoline = errors.New("python built without --enable-perf-trampoline")

	// ErrStaticallyLinkedNoSymbols is returned when neither libpython nor
	// /proc/<pid>/exe exposes the libpython internal symbols needed for
	// remote calls (PyRun_SimpleString, PyGILState_Ensure, PyGILState_Release).
	ErrStaticallyLinkedNoSymbols = errors.New("python interpreter symbols not resolvable")

	// ErrPreexisting is recorded (not returned to callers) when /tmp/perf-<pid>.map
	// already exists at activation time, indicating the trampoline was activated
	// by a prior perf-agent run or by user code. We skip both activation and
	// deactivation in this case to avoid stomping on prior state.
	ErrPreexisting = errors.New("perf trampoline already active (preexisting marker)")
)
```

- [ ] **Step 3: Create the payload file**

Create `inject/python/payload.go`:

```go
package python

// activatePayload is the C string written into the target process's address
// space. PyRun_SimpleString reads it as a NUL-terminated cstring.
//
// We import sys explicitly each time rather than relying on it already being
// imported, because PyRun_SimpleString runs in a fresh "main" namespace.
var activatePayload = []byte("import sys; sys.activate_stack_trampoline('perf')\x00")

var deactivatePayload = []byte("import sys; sys.deactivate_stack_trampoline()\x00")

// ActivatePayload returns the byte slice (NUL-terminated) to write into the
// target's address space before calling PyRun_SimpleString. Caller must NOT
// mutate the returned slice.
func ActivatePayload() []byte { return activatePayload }

// DeactivatePayload returns the byte slice (NUL-terminated) for the
// shutdown deactivation call. Caller must NOT mutate the returned slice.
func DeactivatePayload() []byte { return deactivatePayload }
```

- [ ] **Step 4: Write the failing test**

Create `inject/python/payload_test.go`:

```go
package python

import (
	"bytes"
	"testing"
)

func TestEncodeActivatePayload(t *testing.T) {
	got := ActivatePayload()
	want := []byte("import sys; sys.activate_stack_trampoline('perf')\x00")
	if !bytes.Equal(got, want) {
		t.Fatalf("ActivatePayload mismatch:\n  got  %q\n  want %q", got, want)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("ActivatePayload not NUL-terminated; last byte = 0x%x", got[len(got)-1])
	}
}

func TestEncodeDeactivatePayload(t *testing.T) {
	got := DeactivatePayload()
	want := []byte("import sys; sys.deactivate_stack_trampoline()\x00")
	if !bytes.Equal(got, want) {
		t.Fatalf("DeactivatePayload mismatch:\n  got  %q\n  want %q", got, want)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("DeactivatePayload not NUL-terminated; last byte = 0x%x", got[len(got)-1])
	}
}

func TestPayloadsAreImmutable(t *testing.T) {
	// Sanity check: caller mutation must not corrupt the package state.
	// We verify by reading the slice twice and checking equality after the first
	// call; if a caller did mutate, the second call would return the mutated
	// version. Slices are aliased to the package-level vars by design — the
	// "must not mutate" contract is documented; this test just records it.
	a := ActivatePayload()
	b := ActivatePayload()
	if !bytes.Equal(a, b) {
		t.Fatalf("ActivatePayload returned different slices on consecutive calls")
	}
}
```

- [ ] **Step 5: Run the tests — expect failure (package doesn't exist yet to the test runner if we forgot something)**

```bash
go test ./inject/python/...
```
Expected: PASS (we wrote both source and tests; this verifies the package compiles and tests pass).

- [ ] **Step 6: Commit**

```bash
git add inject/python/errors.go inject/python/payload.go inject/python/payload_test.go
git commit -m "inject/python: sentinel errors and trampoline payload encoding"
```

---

## Task 2: SONAME parsing for libpython

**Files:**
- Create: `inject/elfsym/soname.go`
- Test: `inject/elfsym/soname_test.go`

A small pure-function package: given a path-like string (`/usr/lib/x86_64-linux-gnu/libpython3.12.so.1.0`), return the major+minor Python version, or report "not a Python SONAME". The version filter (>= 3.12) lives here so the detector can short-circuit cleanly.

- [ ] **Step 1: Write the failing test**

Create `inject/elfsym/soname_test.go`:

```go
package elfsym

import "testing"

func TestParseLibpythonSONAME(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantMajor   int
		wantMinor   int
		wantIsPy    bool
	}{
		{"py312_typical", "/usr/lib/x86_64-linux-gnu/libpython3.12.so.1.0", 3, 12, true},
		{"py312_no_minor_suffix", "/usr/lib/libpython3.12.so", 3, 12, true},
		{"py313_future", "/usr/lib/libpython3.13.so.1.0", 3, 13, true},
		{"py399_far_future", "/usr/lib/libpython3.99.so", 3, 99, true},
		{"py311_too_old", "/usr/lib/libpython3.11.so.1.0", 3, 11, true},
		{"py27_legacy", "/usr/lib/libpython2.7.so", 2, 7, true},
		{"non_python_lib", "/usr/lib/libfoo.so.1", 0, 0, false},
		{"non_python_lib_with_python_substr", "/opt/mypython-helper.so", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, ok := ParseLibpythonSONAME(tc.path)
			if ok != tc.wantIsPy {
				t.Fatalf("ParseLibpythonSONAME(%q): ok=%v, want %v", tc.path, ok, tc.wantIsPy)
			}
			if major != tc.wantMajor || minor != tc.wantMinor {
				t.Fatalf("ParseLibpythonSONAME(%q): major=%d minor=%d, want %d %d",
					tc.path, major, minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

func TestIsPython312Plus(t *testing.T) {
	tests := []struct {
		major int
		minor int
		want  bool
	}{
		{3, 12, true},
		{3, 13, true},
		{3, 99, true},
		{4, 0, true},
		{3, 11, false},
		{3, 10, false},
		{2, 7, false},
		{0, 0, false},
	}
	for _, tc := range tests {
		got := IsPython312Plus(tc.major, tc.minor)
		if got != tc.want {
			t.Fatalf("IsPython312Plus(%d,%d) = %v, want %v",
				tc.major, tc.minor, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run tests — expect failure (package doesn't exist)**

```bash
go test ./inject/elfsym/...
```
Expected: FAIL with "no Go files" or "package not found".

- [ ] **Step 3: Implement the parser**

Create `inject/elfsym/soname.go`:

```go
// Package elfsym provides ELF symbol resolution and SONAME parsing primitives
// shared across language-specific injectors (inject/python, future inject/nodejs,
// etc.). Pure stdlib (debug/elf + regexp); no CGO, no external dependencies.
package elfsym

import (
	"path/filepath"
	"regexp"
	"strconv"
)

// libpythonRE matches typical CPython library SONAMEs, e.g.:
//   libpython3.12.so
//   libpython3.12.so.1.0
//   libpython2.7.so
// It is deliberately liberal — caller decides whether the version is
// acceptable via IsPython312Plus.
var libpythonRE = regexp.MustCompile(`^libpython(\d+)\.(\d+)\.so(?:\..*)?$`)

// ParseLibpythonSONAME extracts the major and minor version from a libpython
// shared-library path. It accepts either a bare basename or a full path; only
// the basename is matched against the libpython regex. Returns (major, minor,
// true) on a successful match; (0, 0, false) otherwise.
func ParseLibpythonSONAME(path string) (major, minor int, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	base := filepath.Base(path)
	m := libpythonRE.FindStringSubmatch(base)
	if m == nil {
		return 0, 0, false
	}
	maj, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// IsPython312Plus reports whether the given (major, minor) version is at least
// 3.12 — the minimum CPython version that ships sys.activate_stack_trampoline.
func IsPython312Plus(major, minor int) bool {
	if major > 3 {
		return true
	}
	if major == 3 && minor >= 12 {
		return true
	}
	return false
}
```

- [ ] **Step 4: Run tests — expect pass**

```bash
go test ./inject/elfsym/...
```
Expected: PASS for both `TestParseLibpythonSONAME` and `TestIsPython312Plus`.

- [ ] **Step 5: Commit**

```bash
git add inject/elfsym/soname.go inject/elfsym/soname_test.go
git commit -m "inject/elfsym: parse libpython SONAME and version-gate at 3.12+"
```

---

## Task 3: ELF symbol resolution

**Files:**
- Create: `inject/elfsym/elfsym.go`
- Test: `inject/elfsym/elfsym_test.go`

Resolve a list of named symbols against an on-disk ELF file. Tries `.dynsym` first (always present in shared libs); falls back to `.symtab` (may be stripped). Returns absolute file offsets — caller adds the load base to get remote addresses.

- [ ] **Step 1: Write the failing test using a synthetic ELF**

Create `inject/elfsym/elfsym_test.go`:

```go
package elfsym

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// We don't hand-craft ELF bytes (too fragile across Go versions); instead, we
// exercise the resolver against a real binary on the test host. The Go runtime
// itself is always available via runtime.GOROOT()'s "go" tool, but we use the
// Go test binary itself to keep things simple — it is a real ELF with a
// symbol table.

func TestResolveSymbols_DynsymHit(t *testing.T) {
	// Use libc, which is universally available on Linux and has stable
	// symbol names. We probe one known symbol: "malloc".
	libc := findLibcPath(t)
	resolved, err := ResolveSymbols(libc, []string{"malloc"})
	if err != nil {
		t.Fatalf("ResolveSymbols(%q): %v", libc, err)
	}
	if resolved["malloc"] == 0 {
		t.Fatalf("ResolveSymbols(%q): malloc not resolved (got 0)", libc)
	}
}

func TestResolveSymbols_MissingByName(t *testing.T) {
	libc := findLibcPath(t)
	resolved, err := ResolveSymbols(libc, []string{"this_symbol_does_not_exist_xyz"})
	if err != nil {
		t.Fatalf("ResolveSymbols: unexpected error: %v", err)
	}
	if v, present := resolved["this_symbol_does_not_exist_xyz"]; present {
		t.Fatalf("expected symbol absent; got address 0x%x", v)
	}
}

func TestResolveSymbols_MultipleSymbols(t *testing.T) {
	libc := findLibcPath(t)
	resolved, err := ResolveSymbols(libc, []string{"malloc", "free", "this_does_not_exist"})
	if err != nil {
		t.Fatalf("ResolveSymbols: %v", err)
	}
	if resolved["malloc"] == 0 {
		t.Fatal("malloc not resolved")
	}
	if resolved["free"] == 0 {
		t.Fatal("free not resolved")
	}
	if _, present := resolved["this_does_not_exist"]; present {
		t.Fatal("nonexistent symbol unexpectedly present")
	}
}

func TestResolveSymbols_FileDoesNotExist(t *testing.T) {
	_, err := ResolveSymbols("/path/that/definitely/does/not/exist", []string{"malloc"})
	if err == nil {
		t.Fatal("expected error for nonexistent file; got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected wrapped os.ErrNotExist; got %v", err)
	}
}

// findLibcPath locates a libc.so.6 on the test host. Skips if not found.
func findLibcPath(t *testing.T) string {
	candidates := []string{
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib/aarch64-linux-gnu/libc.so.6",
		"/lib64/libc.so.6",
		"/usr/lib/x86_64-linux-gnu/libc.so.6",
		"/usr/lib/aarch64-linux-gnu/libc.so.6",
		"/usr/lib64/libc.so.6",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// Fallback: glob /usr/lib*/libc.so.6
	matches, _ := filepath.Glob("/usr/lib*/libc.so*")
	for _, m := range matches {
		if _, err := os.Stat(m); err == nil {
			return m
		}
	}
	t.Skip("libc.so.6 not found on test host; skipping ELF resolver test")
	return ""
}
```

- [ ] **Step 2: Run tests — expect failure (no package)**

```bash
go test ./inject/elfsym/...
```
Expected: FAIL — `ResolveSymbols` undefined.

- [ ] **Step 3: Implement the resolver**

Create `inject/elfsym/elfsym.go`:

```go
package elfsym

import (
	"debug/elf"
	"fmt"
)

// ResolveSymbols opens the ELF file at path and resolves each symbol name in
// names to its file-offset value (the symbol's st_value). Returned map only
// contains entries for symbols that were found; missing symbols are silently
// absent. The caller adds the runtime load base to each value to compute the
// remote process's address.
//
// .dynsym is searched first (the dynamic symbol table is always present in
// shared libraries and required at runtime). If a name is not found there
// AND .symtab is present (i.e. binary not stripped), .symtab is searched as a
// fallback. This matters for some Python distributions that intentionally
// strip non-API symbols from .dynsym but leave .symtab intact.
//
// Returns os.ErrNotExist (wrapped) if path does not exist; other errors are
// wrapped with context.
func ResolveSymbols(path string, names []string) (map[string]uint64, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ELF %q: %w", path, err)
	}
	defer f.Close()

	out := make(map[string]uint64, len(names))
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}

	// Try .dynsym first.
	if dynsyms, derr := f.DynamicSymbols(); derr == nil {
		for _, sym := range dynsyms {
			if _, needed := want[sym.Name]; needed && sym.Value != 0 {
				out[sym.Name] = sym.Value
			}
		}
	}

	// Fall back to .symtab for any names still unresolved.
	stillMissing := false
	for _, n := range names {
		if _, ok := out[n]; !ok {
			stillMissing = true
			break
		}
	}
	if stillMissing {
		if syms, serr := f.Symbols(); serr == nil {
			for _, sym := range syms {
				if _, needed := want[sym.Name]; needed {
					if _, already := out[sym.Name]; already {
						continue
					}
					if sym.Value != 0 {
						out[sym.Name] = sym.Value
					}
				}
			}
		}
	}

	return out, nil
}
```

- [ ] **Step 4: Run tests — expect pass**

```bash
go test ./inject/elfsym/...
```
Expected: PASS for `TestResolveSymbols_DynsymHit`, `TestResolveSymbols_MissingByName`, `TestResolveSymbols_MultipleSymbols`, `TestResolveSymbols_FileDoesNotExist`. (May skip on hosts without libc.so.6.)

- [ ] **Step 5: Commit**

```bash
git add inject/elfsym/elfsym.go inject/elfsym/elfsym_test.go
git commit -m "inject/elfsym: ELF symbol resolution with dynsym→symtab fallback"
```

---

## Task 4: Detection ladder

**Files:**
- Create: `inject/python/detector.go`
- Test: `inject/python/detector_test.go`

The detector wires `/proc/<pid>/maps` parsing, SONAME matching, and ELF symbol resolution into the ladder defined in spec §5. Output is a `*Target` (success) or one of the sentinel errors from Task 1.

- [ ] **Step 1: Define the Target type**

Create `inject/python/detector.go` (initial — extended later in same task):

```go
package python

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/inject/elfsym"
)

// Target describes a Python 3.12+ process that detection has confirmed is
// suitable for trampoline injection. All address fields are absolute remote
// addresses (load_base + symbol_offset) ready to be passed to ptraceop.
type Target struct {
	PID              uint32
	LibPythonPath    string // on-disk path used for ELF parsing
	LoadBase         uint64 // address from /proc/<pid>/maps for libpython
	PyGILEnsureAddr  uint64
	PyGILReleaseAddr uint64
	PyRunStringAddr  uint64
	Major, Minor     int // detected libpython version
}

// Detector inspects a process and reports whether it is a Python 3.12+
// candidate suitable for trampoline injection.
type Detector interface {
	Detect(pid uint32) (*Target, error)
}

// requiredSymbols are resolved by the detector; addresses are recorded on Target.
var requiredSymbols = []string{
	"PyGILState_Ensure",
	"PyGILState_Release",
	"PyRun_SimpleString",
}

// markerSymbol is a presence-only check; if absent in the symbol table, the
// target was compiled without --enable-perf-trampoline.
const markerSymbol = "_PyPerf_Callbacks"

// New /proc-based Detector. procRoot is configurable for testing; production
// callers pass "/proc".
func NewDetector(procRoot string, log *slog.Logger) Detector {
	if log == nil {
		log = slog.Default()
	}
	return &procDetector{procRoot: procRoot, log: log}
}

type procDetector struct {
	procRoot string
	log      *slog.Logger
}

// Detect implements Detector. Errors are sentinel-wrapped (errors.Is friendly):
//   ErrNotPython, ErrPythonTooOld, ErrNoPerfTrampoline, ErrStaticallyLinkedNoSymbols.
// Any other error (e.g. EACCES on /proc) is returned as a hard error for the
// caller to decide whether to abort.
func (d *procDetector) Detect(pid uint32) (*Target, error) {
	mapsPath := filepath.Join(d.procRoot, strconv.FormatUint(uint64(pid), 10), "maps")
	f, err := os.Open(mapsPath)
	if err != nil {
		// /proc/<pid>/maps disappeared — process exited between scan and detect.
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: process gone", ErrNotPython)
		}
		return nil, fmt.Errorf("open %s: %w", mapsPath, err)
	}
	defer f.Close()

	libpythonPath, libpythonBase, ok := scanForLibpython(f)
	if ok {
		return d.resolveDynamic(pid, libpythonPath, libpythonBase)
	}

	// Fall back to /proc/<pid>/exe (statically-linked CPython).
	return d.resolveStatic(pid)
}

// scanForLibpython walks the maps file looking for an executable mapping whose
// path matches a libpython SONAME. Returns the on-disk path and the load
// (start) address, or ("", 0, false).
func scanForLibpython(maps *os.File) (string, uint64, bool) {
	sc := bufio.NewScanner(maps)
	for sc.Scan() {
		line := sc.Text()
		// Format: "addrStart-addrEnd perms offset dev inode pathname"
		// We want executable mappings (perms includes 'x') with a path that
		// matches the libpython regex.
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		perms := fields[1]
		if !strings.Contains(perms, "x") {
			continue
		}
		path := fields[5]
		if _, _, isPy := elfsym.ParseLibpythonSONAME(path); !isPy {
			continue
		}
		// Parse start address.
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		startHex := fields[0][:dash]
		start, err := strconv.ParseUint(startHex, 16, 64)
		if err != nil {
			continue
		}
		return path, start, true
	}
	return "", 0, false
}

// resolveDynamic handles the case where libpython is mapped as a shared lib.
// libpath is the on-disk path; loadBase is the runtime mapping start.
func (d *procDetector) resolveDynamic(pid uint32, libpath string, loadBase uint64) (*Target, error) {
	major, minor, _ := elfsym.ParseLibpythonSONAME(libpath)
	if !elfsym.IsPython312Plus(major, minor) {
		return nil, fmt.Errorf("%w: detected %d.%d", ErrPythonTooOld, major, minor)
	}
	resolved, err := elfsym.ResolveSymbols(libpath, append([]string{markerSymbol}, requiredSymbols...))
	if err != nil {
		return nil, fmt.Errorf("resolve symbols in %s: %w", libpath, err)
	}
	if _, ok := resolved[markerSymbol]; !ok {
		return nil, fmt.Errorf("%w: %s missing in %s", ErrNoPerfTrampoline, markerSymbol, libpath)
	}
	for _, sym := range requiredSymbols {
		if _, ok := resolved[sym]; !ok {
			return nil, fmt.Errorf("%w: %s missing in %s", ErrStaticallyLinkedNoSymbols, sym, libpath)
		}
	}
	return &Target{
		PID:              pid,
		LibPythonPath:    libpath,
		LoadBase:         loadBase,
		PyGILEnsureAddr:  loadBase + resolved["PyGILState_Ensure"],
		PyGILReleaseAddr: loadBase + resolved["PyGILState_Release"],
		PyRunStringAddr:  loadBase + resolved["PyRun_SimpleString"],
		Major:            major,
		Minor:            minor,
	}, nil
}

// resolveStatic handles statically-linked CPython, where libpython symbols are
// in the executable itself rather than a shared library.
func (d *procDetector) resolveStatic(pid uint32) (*Target, error) {
	exePath := filepath.Join(d.procRoot, strconv.FormatUint(uint64(pid), 10), "exe")
	// Resolve the symlink to get the real on-disk path for ELF parsing.
	realExe, err := os.Readlink(exePath)
	if err != nil {
		return nil, fmt.Errorf("%w: readlink %s: %v", ErrNotPython, exePath, err)
	}
	resolved, err := elfsym.ResolveSymbols(realExe, append([]string{markerSymbol}, requiredSymbols...))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotPython, err)
	}
	for _, sym := range requiredSymbols {
		if _, ok := resolved[sym]; !ok {
			return nil, fmt.Errorf("%w: %s missing in %s", ErrNotPython, sym, realExe)
		}
	}
	if _, ok := resolved[markerSymbol]; !ok {
		return nil, fmt.Errorf("%w: %s missing in %s", ErrNoPerfTrampoline, markerSymbol, realExe)
	}
	// For static binaries, find the executable's load base from /proc/<pid>/maps.
	mapsPath := filepath.Join(d.procRoot, strconv.FormatUint(uint64(pid), 10), "maps")
	f, err := os.Open(mapsPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", mapsPath, err)
	}
	defer f.Close()
	loadBase := scanForExeBase(f, realExe)
	if loadBase == 0 {
		return nil, fmt.Errorf("%w: cannot find exe load base in %s", ErrNotPython, mapsPath)
	}
	return &Target{
		PID:              pid,
		LibPythonPath:    realExe,
		LoadBase:         loadBase,
		PyGILEnsureAddr:  loadBase + resolved["PyGILState_Ensure"],
		PyGILReleaseAddr: loadBase + resolved["PyGILState_Release"],
		PyRunStringAddr:  loadBase + resolved["PyRun_SimpleString"],
		Major:            0, // static path; SONAME version unknown
		Minor:            0,
	}, nil
}

// scanForExeBase finds the lowest start address of an executable mapping whose
// path matches realExe. Used for statically-linked CPython where load base
// must be derived from the exe's own mapping.
func scanForExeBase(maps *os.File, realExe string) uint64 {
	var lowest uint64 = 0
	sc := bufio.NewScanner(maps)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		if fields[5] != realExe {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
		if err != nil {
			continue
		}
		if lowest == 0 || start < lowest {
			lowest = start
		}
	}
	return lowest
}
```

- [ ] **Step 2: Write the failing tests using a synthetic /proc tree**

Create `inject/python/detector_test.go`:

```go
package python

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// buildSyntheticProc creates a /proc-like tree under tmp for one PID, with a
// maps file containing the given lines and an exe symlink pointing at exeTarget.
// Returns the procRoot path.
func buildSyntheticProc(t *testing.T, pid uint32, mapsLines []string, exeTarget string) string {
	t.Helper()
	root := t.TempDir()
	pidDir := filepath.Join(root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapsContent := ""
	for _, l := range mapsLines {
		mapsContent += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(pidDir, "maps"), []byte(mapsContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if exeTarget != "" {
		if err := os.Symlink(exeTarget, filepath.Join(pidDir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// findRealLibpython finds a libpython3.12+ on the test host, or skips.
// Used because we need a real ELF with the trampoline symbols for detect tests.
func findRealLibpython(t *testing.T) (path string, major, minor int) {
	t.Helper()
	candidates := []string{
		"/usr/lib/x86_64-linux-gnu/libpython3.12.so.1.0",
		"/usr/lib/x86_64-linux-gnu/libpython3.13.so.1.0",
		"/usr/lib/aarch64-linux-gnu/libpython3.12.so.1.0",
		"/usr/lib/libpython3.12.so.1.0",
		"/usr/lib64/libpython3.12.so.1.0",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			// Confirm symbols (not all distros build with --enable-perf-trampoline).
			return c, 0, 0
		}
	}
	matches, _ := filepath.Glob("/usr/lib*/libpython3.1*.so*")
	for _, m := range matches {
		return m, 0, 0
	}
	t.Skip("no libpython3.12+ found on test host")
	return "", 0, 0
}

func TestDetect_DynamicLinkedPython312(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}
	libpath, _, _ := findRealLibpython(t)
	pid := uint32(12345)
	mapsLine := fmt.Sprintf("00400000-00500000 r-xp 00000000 00:00 0 %s", libpath)
	root := buildSyntheticProc(t, pid, []string{mapsLine}, "")

	d := NewDetector(root, nil)
	got, err := d.Detect(pid)
	if err != nil {
		// libpython might not have been built with --enable-perf-trampoline on
		// this host. Accept ErrNoPerfTrampoline as a valid skip.
		if errors.Is(err, ErrNoPerfTrampoline) {
			t.Skipf("host libpython lacks --enable-perf-trampoline: %v", err)
		}
		t.Fatalf("Detect: %v", err)
	}
	if got.PID != pid {
		t.Errorf("PID = %d, want %d", got.PID, pid)
	}
	if got.LibPythonPath != libpath {
		t.Errorf("LibPythonPath = %q, want %q", got.LibPythonPath, libpath)
	}
	if got.LoadBase != 0x00400000 {
		t.Errorf("LoadBase = 0x%x, want 0x00400000", got.LoadBase)
	}
	if got.PyGILEnsureAddr == 0 || got.PyRunStringAddr == 0 || got.PyGILReleaseAddr == 0 {
		t.Errorf("symbol addrs not populated: %+v", got)
	}
}

func TestDetect_NonPython(t *testing.T) {
	pid := uint32(22222)
	// Maps with no libpython: a Go binary scenario.
	root := buildSyntheticProc(t, pid, []string{
		"00400000-00500000 r-xp 00000000 00:00 0 /usr/bin/cat",
	}, "/usr/bin/cat")

	d := NewDetector(root, nil)
	_, err := d.Detect(pid)
	if err == nil {
		t.Fatal("expected error for non-python; got nil")
	}
	if !errors.Is(err, ErrNotPython) && !errors.Is(err, ErrNoPerfTrampoline) {
		t.Fatalf("expected ErrNotPython or ErrNoPerfTrampoline; got %v", err)
	}
}

func TestDetect_PythonTooOld(t *testing.T) {
	pid := uint32(33333)
	// Synthetic libpython 3.11 path; the file doesn't need to exist for the
	// SONAME check to fire — that's the early gate.
	mapsLine := "00400000-00500000 r-xp 00000000 00:00 0 /usr/lib/libpython3.11.so.1.0"
	root := buildSyntheticProc(t, pid, []string{mapsLine}, "")

	d := NewDetector(root, nil)
	_, err := d.Detect(pid)
	if err == nil {
		t.Fatal("expected ErrPythonTooOld")
	}
	if !errors.Is(err, ErrPythonTooOld) {
		t.Fatalf("expected ErrPythonTooOld; got %v", err)
	}
}

func TestDetect_ProcessGone(t *testing.T) {
	pid := uint32(99999)
	// Don't create any /proc/<pid> entry — simulates process exit.
	root := t.TempDir()
	d := NewDetector(root, nil)
	_, err := d.Detect(pid)
	if err == nil {
		t.Fatal("expected error for missing /proc/<pid>")
	}
	if !errors.Is(err, ErrNotPython) {
		t.Fatalf("expected ErrNotPython (wrapping process-gone); got %v", err)
	}
}
```

- [ ] **Step 3: Run the tests — expect pass**

```bash
go test ./inject/python/...
```
Expected: PASS for `TestDetect_NonPython`, `TestDetect_PythonTooOld`, `TestDetect_ProcessGone`. `TestDetect_DynamicLinkedPython312` may skip if no libpython3.12+ is installed.

- [ ] **Step 4: Commit**

```bash
git add inject/python/detector.go inject/python/detector_test.go
git commit -m "inject/python: detection ladder (SONAME match + ELF symbol resolve)"
```

---

## Task 5: ptraceop arch-generic + amd64 register frame

**Files:**
- Create: `inject/ptraceop/ptraceop.go`
- Create: `inject/ptraceop/regs_amd64.go`
- Test: `inject/ptraceop/regs_amd64_test.go`

This is the biggest task. We split into three files:
- `ptraceop.go` — public types (`Injector`, `SymbolAddrs`), `RemoteActivate`/`RemoteDeactivate` orchestrators, internal helpers (attach, detach, write, the per-call inner loop). Compiles on any GOARCH (no arch-specific code here).
- `regs_amd64.go` — build-tagged `setupCallFrame` and `extractReturn` for amd64 System V.
- `regs_arm64.go` — Task 6.

Real `ptrace` is not exercised by unit tests — those run only in integration. Unit tests cover the per-arch register-frame logic.

- [ ] **Step 1: Implement the arch-generic skeleton**

Create `inject/ptraceop/ptraceop.go`:

```go
// Package ptraceop provides low-level ptrace primitives for remote function
// invocation: attach, save registers, write a payload, run a sequence of
// remote function calls (each returning via SIGSEGV at address 0), restore
// registers, detach. Language-agnostic — Python uses it today; future runtimes
// (Node.js if extending V8 internals, JVM via JNI shims, etc.) can reuse it.
package ptraceop

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// SymbolAddrs holds the absolute remote addresses of the three CPython
// functions we need to call. ptraceop deliberately does not depend on
// inject/python — naming is Python-flavored but the struct is just three
// uint64s; future runtimes can pass equivalent triples.
type SymbolAddrs struct {
	PyGILEnsure  uint64
	PyGILRelease uint64
	PyRunString  uint64
}

// Injector performs ptrace-based remote function calls.
type Injector struct {
	log *slog.Logger
}

// New creates an Injector. log may be nil (uses slog.Default()).
func New(log *slog.Logger) *Injector {
	if log == nil {
		log = slog.Default()
	}
	return &Injector{log: log}
}

// RemoteActivate runs the three-call sequence
// (PyGILState_Ensure → PyRun_SimpleString(payload) → PyGILState_Release)
// inside one ptrace session. Returns nil on success, a wrapped error otherwise.
//
// The payload must be NUL-terminated; the caller (typically inject/python)
// supplies python.ActivatePayload() or python.DeactivatePayload().
func (i *Injector) RemoteActivate(pid uint32, addrs SymbolAddrs, payload []byte) error {
	return i.runSequence(pid, addrs, payload)
}

// RemoteDeactivate is identical to RemoteActivate; the only difference is the
// payload string. Kept as a separate method for callsite clarity.
func (i *Injector) RemoteDeactivate(pid uint32, addrs SymbolAddrs, payload []byte) error {
	return i.runSequence(pid, addrs, payload)
}

// runSequence implements the full attach → 3 calls → detach sequence from
// design §6.2.
func (i *Injector) runSequence(pid uint32, addrs SymbolAddrs, payload []byte) error {
	if pid == 0 {
		return errors.New("ptraceop: pid is zero")
	}
	if len(payload) == 0 {
		return errors.New("ptraceop: empty payload")
	}
	if payload[len(payload)-1] != 0 {
		return errors.New("ptraceop: payload not NUL-terminated")
	}
	if addrs.PyGILEnsure == 0 || addrs.PyGILRelease == 0 || addrs.PyRunString == 0 {
		return errors.New("ptraceop: SymbolAddrs has zero entry")
	}

	// Step 1-2: attach + waitpid.
	if err := unix.PtraceAttach(int(pid)); err != nil {
		return fmt.Errorf("ptrace attach pid=%d: %w", pid, err)
	}
	defer func() {
		// Best-effort detach. If we error before reaching the explicit detach
		// below, this ensures the target is resumed.
		_ = unix.PtraceDetach(int(pid))
	}()

	var status unix.WaitStatus
	if _, err := unix.Wait4(int(pid), &status, 0, nil); err != nil {
		return fmt.Errorf("waitpid for attach stop pid=%d: %w", pid, err)
	}
	if !status.Stopped() {
		return fmt.Errorf("expected stopped after attach pid=%d, got status %v", pid, status)
	}

	// Step 3: save original registers.
	var orig unix.PtraceRegs
	if err := unix.PtraceGetRegs(int(pid), &orig); err != nil {
		return fmt.Errorf("ptrace getregs pid=%d: %w", pid, err)
	}

	// Step 4: find stack mapping and verify headroom.
	stackLow, ok := stackLowAddr(pid)
	if !ok {
		return fmt.Errorf("cannot determine stack mapping for pid=%d", pid)
	}
	currentSP := stackPointer(orig)
	if currentSP < stackLow+1024 {
		// Headroom check failed; spec §6.4 specifies remote-mmap fallback.
		// v1 ships without the fallback (rare in practice); we fail with a
		// clear sentinel-style message so the caller can decide.
		return fmt.Errorf("ptraceop: insufficient stack headroom (sp=0x%x, low=0x%x); remote-mmap fallback not implemented in v1",
			currentSP, stackLow)
	}

	// Step 5-6: choose payload_addr, write payload to stack.
	payloadAddr := (currentSP - 256) &^ 0xF
	if _, err := unix.PtracePokeData(int(pid), uintptr(payloadAddr), payload); err != nil {
		return fmt.Errorf("ptrace pokedata payload pid=%d: %w", pid, err)
	}

	// Step 7-9: three remote calls (Ensure → Run → Release).
	gstate, err := i.remoteCall(pid, orig, addrs.PyGILEnsure, payloadAddr, 0)
	if err != nil {
		return fmt.Errorf("PyGILState_Ensure: %w", err)
	}
	runResult, runErr := i.remoteCall(pid, orig, addrs.PyRunString, payloadAddr, payloadAddr)
	// Always release the GIL we acquired, even if PyRun_SimpleString failed.
	if _, relErr := i.remoteCall(pid, orig, addrs.PyGILRelease, payloadAddr, gstate); relErr != nil {
		i.log.Warn("PyGILState_Release failed after attempted activation",
			"pid", pid, "err", relErr)
	}
	if runErr != nil {
		return fmt.Errorf("PyRun_SimpleString: %w", runErr)
	}
	if runResult != 0 {
		return fmt.Errorf("PyRun_SimpleString returned non-zero: %d (likely activation refused at runtime)", runResult)
	}

	// Step 10-11: restore registers, detach (defer above handles detach).
	if err := unix.PtraceSetRegs(int(pid), &orig); err != nil {
		return fmt.Errorf("ptrace setregs restore pid=%d: %w", pid, err)
	}
	if err := unix.PtraceDetach(int(pid)); err != nil {
		return fmt.Errorf("ptrace detach pid=%d: %w", pid, err)
	}
	return nil
}

// remoteCall performs one remote function invocation:
//  1. Build call frame from orig with fnAddr/arg, return-addr=0 (SIGSEGV
//     sentinel), payload_addr-derived stack pointer.
//  2. PtraceSetRegs.
//  3. PtraceCont with signal=0.
//  4. Wait4 for SIGSEGV (or other terminal stop).
//  5. Read regs, return the call's return value.
//
// payloadAddr is used to derive a fresh stack pointer for each call so that
// successive calls don't trample each other's frames.
func (i *Injector) remoteCall(pid uint32, orig unix.PtraceRegs, fnAddr, payloadAddr, arg1 uint64) (uint64, error) {
	frame, err := setupCallFrame(orig, fnAddr, arg1, payloadAddr)
	if err != nil {
		return 0, fmt.Errorf("setup call frame: %w", err)
	}
	// Some arches (amd64) require us to write the sentinel return address (0)
	// to *(SP) before the call. setupCallFrame returns the SP that needs the
	// sentinel; we write a 0 word there.
	zero := make([]byte, 8)
	if _, err := unix.PtracePokeData(int(pid), uintptr(stackPointer(frame)), zero); err != nil {
		return 0, fmt.Errorf("write return-addr sentinel: %w", err)
	}
	if err := unix.PtraceSetRegs(int(pid), &frame); err != nil {
		return 0, fmt.Errorf("setregs: %w", err)
	}
	if err := unix.PtraceCont(int(pid), 0); err != nil {
		return 0, fmt.Errorf("ptrace cont: %w", err)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(int(pid), &status, 0, nil); err != nil {
		return 0, fmt.Errorf("wait4 after remote call: %w", err)
	}
	if !status.Stopped() {
		return 0, fmt.Errorf("expected stop after remote call; got status %v", status)
	}
	if sig := status.StopSignal(); sig != unix.SIGSEGV {
		return 0, fmt.Errorf("expected SIGSEGV sentinel; got signal %v", sig)
	}
	var post unix.PtraceRegs
	if err := unix.PtraceGetRegs(int(pid), &post); err != nil {
		return 0, fmt.Errorf("getregs post-call: %w", err)
	}
	return extractReturn(post), nil
}

// stackLowAddr reads /proc/<pid>/maps and returns the low address of the
// [stack] mapping.
func stackLowAddr(pid uint32) (uint64, bool) {
	mapsPath := filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "maps")
	f, err := os.Open(mapsPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if !strings.Contains(line, "[stack]") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		low, err := strconv.ParseUint(fields[0][:dash], 16, 64)
		if err != nil {
			continue
		}
		return low, true
	}
	return 0, false
}
```

- [ ] **Step 2: Implement amd64 register-frame setup**

Create `inject/ptraceop/regs_amd64.go`:

```go
//go:build amd64

package ptraceop

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setupCallFrame builds a register frame for one remote function call on
// amd64 System V. The frame:
//   - RIP = fnAddr (entry of remote function)
//   - RDI = arg1   (first integer/pointer arg)
//   - RSP = payloadAddr - 8, 16-byte aligned post-write of return-addr sentinel
//   - *(RSP) gets a 0 written to it by the caller, serving as the return
//     address; when the function returns, target jumps to 0 → SIGSEGV →
//     ptrace catches it.
//
// All other registers are inherited from orig — we only edit what we must.
func setupCallFrame(orig unix.PtraceRegs, fnAddr, arg1, payloadAddr uint64) (unix.PtraceRegs, error) {
	frame := orig
	frame.Rip = fnAddr
	frame.Rdi = arg1
	// SP layout: ... [return-addr sentinel = 0] [payload string]
	// We choose RSP = payloadAddr - 8 so *(RSP) holds the sentinel.
	// Then ensure 16-byte alignment after the simulated CALL push: in System V,
	// at function entry, RSP must be 8 mod 16 (CALL pushes the return addr
	// making RSP %16 == 8). We achieve that by ensuring (payloadAddr - 8) % 16 == 8,
	// i.e., payloadAddr % 16 == 0. The caller chose payloadAddr aligned to 16,
	// so we're good.
	frame.Rsp = payloadAddr - 8
	if frame.Rsp%16 != 8 {
		return unix.PtraceRegs{}, fmt.Errorf("RSP alignment broken: 0x%x %% 16 = %d (want 8)",
			frame.Rsp, frame.Rsp%16)
	}
	return frame, nil
}

// extractReturn reads the integer return value from a post-call register set.
// On amd64 System V, integer/pointer returns are in RAX.
func extractReturn(post unix.PtraceRegs) uint64 {
	return post.Rax
}

// stackPointer returns the SP register value for arch-generic code.
func stackPointer(r unix.PtraceRegs) uint64 {
	return r.Rsp
}
```

- [ ] **Step 3: Write the failing test for amd64 frame setup**

Create `inject/ptraceop/regs_amd64_test.go`:

```go
//go:build amd64

package ptraceop

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetupCallFrame_amd64(t *testing.T) {
	orig := unix.PtraceRegs{
		Rip: 0xdeadbeef00,
		Rdi: 0x11,
		Rsi: 0x22,
		Rsp: 0x7ffffff00000,
		Rax: 0xaaaa,
	}
	const fnAddr = 0x12340000
	const arg1 = 0xCAFEBABE
	const payloadAddr = 0x7ffffff00100 // 16-byte aligned

	frame, err := setupCallFrame(orig, fnAddr, arg1, payloadAddr)
	if err != nil {
		t.Fatalf("setupCallFrame error: %v", err)
	}
	if frame.Rip != fnAddr {
		t.Errorf("RIP = 0x%x, want 0x%x", frame.Rip, fnAddr)
	}
	if frame.Rdi != arg1 {
		t.Errorf("RDI = 0x%x, want 0x%x", frame.Rdi, arg1)
	}
	if frame.Rsp != payloadAddr-8 {
		t.Errorf("RSP = 0x%x, want 0x%x", frame.Rsp, payloadAddr-8)
	}
	if frame.Rsp%16 != 8 {
		t.Errorf("RSP alignment broken: 0x%x %% 16 = %d", frame.Rsp, frame.Rsp%16)
	}
	// Other registers should be inherited from orig.
	if frame.Rsi != orig.Rsi {
		t.Errorf("RSI clobbered: got 0x%x, want 0x%x", frame.Rsi, orig.Rsi)
	}
	if frame.Rax != orig.Rax {
		t.Errorf("RAX clobbered: got 0x%x, want 0x%x", frame.Rax, orig.Rax)
	}
}

func TestSetupCallFrame_amd64_BadAlignment(t *testing.T) {
	orig := unix.PtraceRegs{Rsp: 0x7ffffff00000}
	const payloadAddr = 0x7ffffff00104 // NOT 16-byte aligned
	_, err := setupCallFrame(orig, 0x1000, 0, payloadAddr)
	if err == nil {
		t.Fatal("expected alignment error; got nil")
	}
}

func TestExtractReturn_amd64(t *testing.T) {
	post := unix.PtraceRegs{Rax: 0xCAFEF00D}
	got := extractReturn(post)
	if got != 0xCAFEF00D {
		t.Errorf("extractReturn = 0x%x, want 0xCAFEF00D", got)
	}
}

func TestStackPointer_amd64(t *testing.T) {
	r := unix.PtraceRegs{Rsp: 0xC0DEFACE}
	if got := stackPointer(r); got != 0xC0DEFACE {
		t.Errorf("stackPointer = 0x%x, want 0xC0DEFACE", got)
	}
}
```

- [ ] **Step 4: Run tests — expect pass**

```bash
go test ./inject/ptraceop/...
```
Expected: PASS for `TestSetupCallFrame_amd64`, `TestSetupCallFrame_amd64_BadAlignment`, `TestExtractReturn_amd64`, `TestStackPointer_amd64`.

- [ ] **Step 5: Commit**

```bash
git add inject/ptraceop/ptraceop.go inject/ptraceop/regs_amd64.go inject/ptraceop/regs_amd64_test.go
git commit -m "inject/ptraceop: arch-generic remote-call sequence + amd64 frame setup"
```

---

## Task 6: ptraceop arm64 register frame

**Files:**
- Create: `inject/ptraceop/regs_arm64.go`
- Test: `inject/ptraceop/regs_arm64_test.go`

Mirror image of Task 5's amd64 file but for AAPCS64. On arm64, args are in X0-X7, return in X0, PC is the program counter, LR (X30) is the link register (return address).

- [ ] **Step 1: Implement arm64 register-frame setup**

Create `inject/ptraceop/regs_arm64.go`:

```go
//go:build arm64

package ptraceop

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setupCallFrame builds a register frame for one remote function call on
// arm64 AAPCS64. The frame:
//   - PC = fnAddr (entry of remote function)
//   - X0 = arg1
//   - X30 (LR) = 0 (sentinel — when function returns via RET, target
//     jumps to 0 → SIGSEGV → ptrace catches it)
//   - SP = payloadAddr - 16, 16-byte aligned (AAPCS64 requires 16-byte SP at
//     all public interfaces)
//
// All other registers are inherited from orig.
func setupCallFrame(orig unix.PtraceRegs, fnAddr, arg1, payloadAddr uint64) (unix.PtraceRegs, error) {
	frame := orig
	frame.Pc = fnAddr
	frame.Regs[0] = arg1
	frame.Regs[30] = 0 // LR (X30) — return address sentinel
	frame.Sp = payloadAddr - 16
	if frame.Sp%16 != 0 {
		return unix.PtraceRegs{}, fmt.Errorf("SP alignment broken: 0x%x %% 16 = %d (want 0)",
			frame.Sp, frame.Sp%16)
	}
	return frame, nil
}

// extractReturn reads the integer return value from a post-call register set.
// On arm64 AAPCS64, integer/pointer returns are in X0.
func extractReturn(post unix.PtraceRegs) uint64 {
	return post.Regs[0]
}

// stackPointer returns the SP register value for arch-generic code.
func stackPointer(r unix.PtraceRegs) uint64 {
	return r.Sp
}
```

- [ ] **Step 2: Write the failing test for arm64**

Create `inject/ptraceop/regs_arm64_test.go`:

```go
//go:build arm64

package ptraceop

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetupCallFrame_arm64(t *testing.T) {
	var orig unix.PtraceRegs
	orig.Pc = 0xdeadbeef00
	orig.Sp = 0x7ffffff00000
	orig.Regs[0] = 0x11
	orig.Regs[1] = 0x22
	orig.Regs[30] = 0xCCCC // some other LR
	const fnAddr = 0x12340000
	const arg1 = 0xCAFEBABE
	const payloadAddr = 0x7ffffff00100 // 16-byte aligned

	frame, err := setupCallFrame(orig, fnAddr, arg1, payloadAddr)
	if err != nil {
		t.Fatalf("setupCallFrame error: %v", err)
	}
	if frame.Pc != fnAddr {
		t.Errorf("PC = 0x%x, want 0x%x", frame.Pc, fnAddr)
	}
	if frame.Regs[0] != arg1 {
		t.Errorf("X0 = 0x%x, want 0x%x", frame.Regs[0], arg1)
	}
	if frame.Regs[30] != 0 {
		t.Errorf("LR (X30) = 0x%x, want 0 (sentinel)", frame.Regs[30])
	}
	if frame.Sp != payloadAddr-16 {
		t.Errorf("SP = 0x%x, want 0x%x", frame.Sp, payloadAddr-16)
	}
	if frame.Sp%16 != 0 {
		t.Errorf("SP not 16-aligned: 0x%x", frame.Sp)
	}
	if frame.Regs[1] != orig.Regs[1] {
		t.Errorf("X1 clobbered: 0x%x, want 0x%x", frame.Regs[1], orig.Regs[1])
	}
}

func TestSetupCallFrame_arm64_BadAlignment(t *testing.T) {
	var orig unix.PtraceRegs
	orig.Sp = 0x7ffffff00000
	const payloadAddr = 0x7ffffff00108 // NOT 16-aligned
	_, err := setupCallFrame(orig, 0x1000, 0, payloadAddr)
	if err == nil {
		t.Fatal("expected alignment error; got nil")
	}
}

func TestExtractReturn_arm64(t *testing.T) {
	var post unix.PtraceRegs
	post.Regs[0] = 0xCAFEF00D
	got := extractReturn(post)
	if got != 0xCAFEF00D {
		t.Errorf("extractReturn = 0x%x, want 0xCAFEF00D", got)
	}
}

func TestStackPointer_arm64(t *testing.T) {
	var r unix.PtraceRegs
	r.Sp = 0xC0DEFACE
	if got := stackPointer(r); got != 0xC0DEFACE {
		t.Errorf("stackPointer = 0x%x, want 0xC0DEFACE", got)
	}
}
```

- [ ] **Step 3: Run tests — expect pass when running on arm64; on amd64 the file is skipped (build tag)**

```bash
go test ./inject/ptraceop/...
```
Expected: PASS on both archs (the build-tagged file compiles only on the matching arch; tests on the *other* arch run only the other file).

To cross-check arm64 compiles cleanly on an amd64 host:

```bash
GOOS=linux GOARCH=arm64 go build ./inject/ptraceop/...
```
Expected: clean build.

- [ ] **Step 4: Commit**

```bash
git add inject/ptraceop/regs_arm64.go inject/ptraceop/regs_arm64_test.go
git commit -m "inject/ptraceop: arm64 frame setup (AAPCS64)"
```

---

## Task 7: Manager — lifecycle, dedupe, stats

**Files:**
- Create: `inject/python/python.go` (Manager, Options, Stats)
- Create: `inject/python/manager.go` (ActivateAll, DeactivateAll, ActivateLate)
- Test: `inject/python/manager_test.go`

The Manager owns the in-memory tracked-PID set, runs the strict/lenient policy, runs the bounded-time deactivation pass on shutdown, and dedupes late-arriving exec events.

- [ ] **Step 1: Define the public surface (python.go)**

Create `inject/python/python.go`:

```go
package python

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Manager.
type Options struct {
	// StrictPerPID makes ActivateAll fail-fast on the first error. Used for
	// --pid N --inject-python; lenient (false) for -a --inject-python.
	StrictPerPID bool

	// Logger receives structured log lines for every detect/activate/deactivate
	// event. nil → slog.Default().
	Logger *slog.Logger

	// PreexistingMarkerCheck overrides the default check for "is this PID's
	// trampoline already active?" — set in tests to a stub. Production passes
	// nil and gets the default /tmp/perf-<pid>.map check.
	PreexistingMarkerCheck func(pid uint32) bool

	// Injector overrides the default ptraceop-based injector. Tests inject
	// stubs; production passes nil and gets the real ptraceop.Injector.
	Injector LowLevelInjector

	// Detector overrides the default /proc-based detector. Tests inject stubs;
	// production passes nil.
	Detector Detector

	// DeactivateDeadline caps the total time spent in DeactivateAll. Default
	// 5 seconds.
	DeactivateDeadline time.Duration
}

// LowLevelInjector is the contract Manager uses for the ptrace dance. The
// production implementation wraps inject/ptraceop.Injector; tests can stub it.
type LowLevelInjector interface {
	RemoteActivate(pid uint32, addrs SymbolAddrsForTarget) error
	RemoteDeactivate(pid uint32, addrs SymbolAddrsForTarget) error
}

// SymbolAddrsForTarget is the data the LowLevelInjector needs to perform one
// remote-call sequence — independent of inject/ptraceop's exact struct layout
// to keep the test boundary clean.
type SymbolAddrsForTarget struct {
	PyGILEnsure  uint64
	PyGILRelease uint64
	PyRunString  uint64
}

// Stats holds counters that operators inspect after a run. All counters are
// safe for concurrent use.
type Stats struct {
	Activated          atomic.Uint64
	Deactivated        atomic.Uint64
	SkippedNotPython   atomic.Uint64
	SkippedTooOld      atomic.Uint64
	SkippedNoTramp     atomic.Uint64
	SkippedNoSymbols   atomic.Uint64
	SkippedPreexisting atomic.Uint64
	ActivateFailed     atomic.Uint64
	DeactivateFailed   atomic.Uint64
}

// Manager orchestrates Python perf-trampoline injection across a profile run:
// detection ladder, strict/lenient policy, in-memory tracked-PID set, bounded
// shutdown deactivation, and idempotent late-arrival activation.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	tracked  map[uint32]*trackedTarget
	pending  sync.Map // pid uint32 → struct{} (in-flight late activation)

	stats Stats
}

type trackedTarget struct {
	target      *Target
	activatedAt time.Time
	preexisting bool
}

// NewManager constructs a Manager. opts.Logger may be nil. opts.Detector and
// opts.Injector default to /proc + ptraceop in production; tests inject stubs.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.DeactivateDeadline == 0 {
		opts.DeactivateDeadline = 5 * time.Second
	}
	if opts.PreexistingMarkerCheck == nil {
		opts.PreexistingMarkerCheck = defaultPreexistingMarkerCheck
	}
	return &Manager{
		opts:    opts,
		log:     opts.Logger,
		tracked: make(map[uint32]*trackedTarget),
	}
}

// Stats returns a snapshot pointer (not a copy — atomic counters are observed
// in-place). Callers should treat it as read-only.
func (m *Manager) Stats() *Stats { return &m.stats }

// ActivateAll runs detection and activation for the given PIDs. Returns nil
// in lenient mode (errors are logged + counted); returns the first error in
// strict mode. Caller blocks on this; it is fine to call once at profile
// start before BPF attach.
func (m *Manager) ActivateAll(pids []uint32) error {
	for _, pid := range pids {
		if err := m.activateOne(pid); err != nil {
			if m.opts.StrictPerPID {
				return err
			}
		}
	}
	m.logSummary()
	return nil
}

// ActivateLate is the mmap-watcher hook for new exec events during -a mode.
// Always lenient (logs + counts on error). Idempotent: dedupes by tracked
// and pending sets.
func (m *Manager) ActivateLate(pid uint32) {
	if _, loaded := m.pending.LoadOrStore(pid, struct{}{}); loaded {
		return // in-flight activation for this pid
	}
	defer m.pending.Delete(pid)

	m.mu.Lock()
	_, already := m.tracked[pid]
	m.mu.Unlock()
	if already {
		return
	}
	_ = m.activateOne(pid) // lenient: errors logged inside
}

// DeactivateAll runs the bounded shutdown deactivation pass. Skips entries
// marked preexisting; tolerates ESRCH (process gone). Honors ctx cancellation
// AND the configured deactivation deadline (5s default).
func (m *Manager) DeactivateAll(ctx context.Context) {
	deadline, cancel := context.WithTimeout(ctx, m.opts.DeactivateDeadline)
	defer cancel()

	snapshot := m.snapshotTracked()
	for pid, tt := range snapshot {
		select {
		case <-deadline.Done():
			m.log.Warn("python deactivate cancelled (deadline or ctx)",
				"abandoned", len(snapshot)-int(m.stats.Deactivated.Load()))
			return
		default:
		}
		if tt.preexisting {
			continue
		}
		addrs := SymbolAddrsForTarget{
			PyGILEnsure:  tt.target.PyGILEnsureAddr,
			PyGILRelease: tt.target.PyGILReleaseAddr,
			PyRunString:  tt.target.PyRunStringAddr,
		}
		if err := m.opts.Injector.RemoteDeactivate(pid, addrs); err != nil {
			if isProcessGone(err) {
				continue
			}
			m.log.Warn("python deactivate failed", "pid", pid, "err", err)
			m.stats.DeactivateFailed.Add(1)
			continue
		}
		m.stats.Deactivated.Add(1)
	}
}
```

- [ ] **Step 2: Implement the activation logic (manager.go)**

Create `inject/python/manager.go`:

```go
package python

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// activateOne runs detection + activation for one PID. Always returns nil in
// lenient mode after logging; returns wrapped error in strict mode.
func (m *Manager) activateOne(pid uint32) error {
	target, err := m.opts.Detector.Detect(pid)
	if err != nil {
		m.recordSkipReason(err)
		m.log.Warn("python inject skipped",
			"pid", pid, "reason", reasonString(err))
		if m.opts.StrictPerPID {
			return fmt.Errorf("inject pid=%d: %w", pid, err)
		}
		return nil
	}

	if m.opts.PreexistingMarkerCheck(pid) {
		m.mu.Lock()
		m.tracked[pid] = &trackedTarget{target: target, preexisting: true, activatedAt: time.Now()}
		m.mu.Unlock()
		m.stats.SkippedPreexisting.Add(1)
		m.log.Warn("python inject skipped",
			"pid", pid, "reason", "preexisting_perf_map")
		return nil
	}

	addrs := SymbolAddrsForTarget{
		PyGILEnsure:  target.PyGILEnsureAddr,
		PyGILRelease: target.PyGILReleaseAddr,
		PyRunString:  target.PyRunStringAddr,
	}
	if err := m.opts.Injector.RemoteActivate(pid, addrs); err != nil {
		m.stats.ActivateFailed.Add(1)
		m.log.Warn("python inject failed", "pid", pid, "err", err)
		if m.opts.StrictPerPID {
			return fmt.Errorf("activate pid=%d: %w", pid, err)
		}
		return nil
	}
	m.mu.Lock()
	m.tracked[pid] = &trackedTarget{target: target, activatedAt: time.Now()}
	m.mu.Unlock()
	m.stats.Activated.Add(1)
	m.log.Info("python inject activated",
		"pid", pid, "libpython", target.LibPythonPath,
		"version", fmt.Sprintf("%d.%d", target.Major, target.Minor))
	return nil
}

func (m *Manager) recordSkipReason(err error) {
	switch {
	case errors.Is(err, ErrPythonTooOld):
		m.stats.SkippedTooOld.Add(1)
	case errors.Is(err, ErrNoPerfTrampoline):
		m.stats.SkippedNoTramp.Add(1)
	case errors.Is(err, ErrStaticallyLinkedNoSymbols):
		m.stats.SkippedNoSymbols.Add(1)
	case errors.Is(err, ErrNotPython):
		m.stats.SkippedNotPython.Add(1)
	default:
		m.stats.SkippedNotPython.Add(1) // unknown errors classified as "not python"
	}
}

func reasonString(err error) string {
	switch {
	case errors.Is(err, ErrPythonTooOld):
		return "python_too_old"
	case errors.Is(err, ErrNoPerfTrampoline):
		return "no_perf_trampoline"
	case errors.Is(err, ErrStaticallyLinkedNoSymbols):
		return "no_libpython_symbols"
	case errors.Is(err, ErrNotPython):
		return "not_python"
	default:
		return "other"
	}
}

func (m *Manager) snapshotTracked() map[uint32]*trackedTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uint32]*trackedTarget, len(m.tracked))
	for k, v := range m.tracked {
		out[k] = v
	}
	return out
}

func (m *Manager) logSummary() {
	m.log.Info("python inject summary",
		"activated", m.stats.Activated.Load(),
		"skipped",
		m.stats.SkippedNotPython.Load()+
			m.stats.SkippedTooOld.Load()+
			m.stats.SkippedNoTramp.Load()+
			m.stats.SkippedNoSymbols.Load()+
			m.stats.SkippedPreexisting.Load(),
		"failed", m.stats.ActivateFailed.Load(),
	)
}

// defaultPreexistingMarkerCheck returns true iff /tmp/perf-<pid>.map exists
// and is non-empty. This is the conservative idempotency check from spec §7.2.
func defaultPreexistingMarkerCheck(pid uint32) bool {
	path := "/tmp/perf-" + strconv.FormatUint(uint64(pid), 10) + ".map"
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Size() > 0
}

// isProcessGone returns true if the error is a "no such process" (ESRCH),
// indicating the target exited between our snapshot and the deactivate call.
func isProcessGone(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ESRCH)
}
```

- [ ] **Step 3: Write the failing tests for Manager**

Create `inject/python/manager_test.go`:

```go
package python

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// stubDetector implements Detector by mapping PID → result.
type stubDetector struct {
	results map[uint32]stubResult
}

type stubResult struct {
	target *Target
	err    error
}

func (s *stubDetector) Detect(pid uint32) (*Target, error) {
	r, ok := s.results[pid]
	if !ok {
		return nil, ErrNotPython
	}
	return r.target, r.err
}

// stubInjector counts and optionally errors.
type stubInjector struct {
	mu              sync.Mutex
	activated       []uint32
	deactivated     []uint32
	activateErr     error
	deactivateErr   error
	deactivateDelay time.Duration
}

func (s *stubInjector) RemoteActivate(pid uint32, _ SymbolAddrsForTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activateErr != nil {
		return s.activateErr
	}
	s.activated = append(s.activated, pid)
	return nil
}

func (s *stubInjector) RemoteDeactivate(pid uint32, _ SymbolAddrsForTarget) error {
	if s.deactivateDelay > 0 {
		time.Sleep(s.deactivateDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deactivateErr != nil {
		return s.deactivateErr
	}
	s.deactivated = append(s.deactivated, pid)
	return nil
}

func newTestManager(t *testing.T, det Detector, inj LowLevelInjector, strict bool) *Manager {
	t.Helper()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(Options{
		StrictPerPID:           strict,
		Logger:                 silent,
		Detector:               det,
		Injector:               inj,
		PreexistingMarkerCheck: func(uint32) bool { return false },
	})
}

func makeTarget(pid uint32) *Target {
	return &Target{
		PID:              pid,
		LibPythonPath:    "/fake/libpython3.12.so",
		LoadBase:         0x400000,
		PyGILEnsureAddr:  0x401000,
		PyGILReleaseAddr: 0x402000,
		PyRunStringAddr:  0x403000,
		Major:            3,
		Minor:            12,
	}
}

func TestActivateAll_StrictExitsOnFirstError(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
		200: {err: ErrPythonTooOld},
		300: {target: makeTarget(300)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, true)

	err := m.ActivateAll([]uint32{100, 200, 300})
	if err == nil {
		t.Fatal("expected strict error; got nil")
	}
	if !errors.Is(err, ErrPythonTooOld) {
		t.Fatalf("expected ErrPythonTooOld; got %v", err)
	}
	// 100 was activated before the error on 200; 300 must NOT have been attempted.
	if got := m.stats.Activated.Load(); got != 1 {
		t.Errorf("Activated = %d, want 1", got)
	}
	for _, p := range inj.activated {
		if p == 300 {
			t.Errorf("strict mode should not have activated pid 300")
		}
	}
}

func TestActivateAll_LenientContinuesOnError(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
		200: {err: ErrPythonTooOld},
		300: {target: makeTarget(300)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, false)

	if err := m.ActivateAll([]uint32{100, 200, 300}); err != nil {
		t.Fatalf("lenient ActivateAll returned error: %v", err)
	}
	if got := m.stats.Activated.Load(); got != 2 {
		t.Errorf("Activated = %d, want 2", got)
	}
	if got := m.stats.SkippedTooOld.Load(); got != 1 {
		t.Errorf("SkippedTooOld = %d, want 1", got)
	}
}

func TestActivateAll_PreexistingMarkerSkips(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{}
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(Options{
		Logger:                 silent,
		Detector:               det,
		Injector:               inj,
		PreexistingMarkerCheck: func(pid uint32) bool { return pid == 100 },
	})
	if err := m.ActivateAll([]uint32{100}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	if got := m.stats.SkippedPreexisting.Load(); got != 1 {
		t.Errorf("SkippedPreexisting = %d, want 1", got)
	}
	if got := m.stats.Activated.Load(); got != 0 {
		t.Errorf("Activated = %d, want 0", got)
	}
	// Now Deactivate — preexisting must be skipped (not in tracked? actually it
	// IS in tracked but with preexisting=true, and deactivation skips it).
	m.DeactivateAll(t.Context())
	if got := m.stats.Deactivated.Load(); got != 0 {
		t.Errorf("Deactivated = %d, want 0 (preexisting must be skipped)", got)
	}
}

func TestDeactivateAll_HonorsDeadline(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
		200: {target: makeTarget(200)},
		300: {target: makeTarget(300)},
	}}
	// Inject delay so the second-or-later Deactivate hits the deadline.
	inj := &stubInjector{deactivateDelay: 100 * time.Millisecond}
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(Options{
		Logger:                 silent,
		Detector:               det,
		Injector:               inj,
		PreexistingMarkerCheck: func(uint32) bool { return false },
		DeactivateDeadline:     50 * time.Millisecond,
	})
	if err := m.ActivateAll([]uint32{100, 200, 300}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	start := time.Now()
	m.DeactivateAll(t.Context())
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Errorf("DeactivateAll took %v; deadline should have capped near 50-150ms", elapsed)
	}
}

func TestDeactivateAll_ToleratesESRCH(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{deactivateErr: syscall.ESRCH}
	m := newTestManager(t, det, inj, false)
	if err := m.ActivateAll([]uint32{100}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	m.DeactivateAll(t.Context())
	if got := m.stats.DeactivateFailed.Load(); got != 0 {
		t.Errorf("DeactivateFailed = %d on ESRCH; want 0", got)
	}
}

func TestActivateLate_DedupesViaTracked(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, false)

	if err := m.ActivateAll([]uint32{100}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	// Now late-arrival event for the same PID — must not trigger second activation.
	m.ActivateLate(100)
	if got := atomic.LoadUint64((*uint64)(&inj.activated[0])); got != 0 && len(inj.activated) > 1 {
		t.Errorf("ActivateLate triggered duplicate activation: %v", inj.activated)
	}
	if len(inj.activated) != 1 {
		t.Errorf("activate count = %d, want 1", len(inj.activated))
	}
}

func TestActivateLate_DedupesViaPending(t *testing.T) {
	// Two concurrent ActivateLate calls for the same PID — only one must
	// reach the injector.
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, false)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { m.ActivateLate(100) })
	}
	wg.Wait()
	if len(inj.activated) > 1 {
		t.Errorf("ActivateLate not deduped under concurrency: %v", inj.activated)
	}
}

// Compile-time check: ensure context is imported correctly.
var _ = context.Background
```

- [ ] **Step 4: Run tests — expect pass**

```bash
go test ./inject/python/...
```
Expected: PASS for all eight Manager tests plus the earlier payload + detector tests.

- [ ] **Step 5: Wire ptraceop adapter (so production passes a real Injector)**

Append to `inject/python/python.go`:

```go
// adaptPtraceOp wraps inject/ptraceop.Injector to satisfy LowLevelInjector.
// We deliberately decouple via this small adapter so manager_test can stub
// LowLevelInjector without importing ptraceop.
type ptraceopAdapter struct {
	inj           ptraceopInjector
	activatePyld  []byte
	deactivPyld   []byte
}

// ptraceopInjector is the slice of inject/ptraceop.Injector we depend on.
type ptraceopInjector interface {
	RemoteActivate(pid uint32, addrs ptraceopSymbolAddrs, payload []byte) error
	RemoteDeactivate(pid uint32, addrs ptraceopSymbolAddrs, payload []byte) error
}

// ptraceopSymbolAddrs mirrors inject/ptraceop.SymbolAddrs to keep the import
// boundary one-way (inject/python imports ptraceop; not vice versa).
type ptraceopSymbolAddrs struct {
	PyGILEnsure  uint64
	PyGILRelease uint64
	PyRunString  uint64
}

func (a *ptraceopAdapter) RemoteActivate(pid uint32, addrs SymbolAddrsForTarget) error {
	return a.inj.RemoteActivate(pid, ptraceopSymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, a.activatePyld)
}

func (a *ptraceopAdapter) RemoteDeactivate(pid uint32, addrs SymbolAddrsForTarget) error {
	return a.inj.RemoteDeactivate(pid, ptraceopSymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, a.deactivPyld)
}
```

The actual wiring of `ptraceopInjector` to `inject/ptraceop.Injector` happens in Task 8 (perfagent), where we have a clear place to do the import. The interface in this file is purely structural.

- [ ] **Step 6: Commit**

```bash
git add inject/python/python.go inject/python/manager.go inject/python/manager_test.go
git commit -m "inject/python: Manager with strict/lenient policy, dedupe, bounded shutdown"
```

---

## Task 8: Wire into perfagent.Agent

**Files:**
- Modify: `perfagent/options.go` (add InjectPython option, validation)
- Modify: `perfagent/agent.go` (Agent.pyInjector field, Start/Stop integration, PythonInjectStats accessor)
- Modify: `main.go` (--inject-python flag)

This connects the inject/python package to the rest of perf-agent.

- [ ] **Step 1: Read the current perfagent options + agent files to understand integration points**

```bash
sed -n '1,80p' perfagent/options.go
sed -n '1,120p' perfagent/agent.go
grep -n "inject-python\|InjectPython" perfagent/*.go main.go
```

- [ ] **Step 2: Add the option, the agent field, and the lifecycle wiring**

The implementer will modify:

In `perfagent/options.go`, add:

```go
// WithInjectPython enables Python perf-trampoline injection during profiling.
// Caller must hold cap_sys_ptrace. Strict mode (--pid N) returns an error on
// any per-target failure; lenient mode (-a) logs and continues.
func WithInjectPython(enabled bool) Option {
	return func(o *Config) { o.InjectPython = enabled }
}
```

And add an `InjectPython bool` field to whatever the Config struct is named. Validation: if `o.InjectPython` is true AND mode is not `--profile` (off-cpu/PMU), return a structured error.

In `perfagent/agent.go`, near the existing field declarations, add:

```go
import (
	"github.com/dpsoft/perf-agent/inject/python"
	"github.com/dpsoft/perf-agent/inject/ptraceop"
)

type Agent struct {
	// ...existing fields...
	pyInjector *python.Manager
}
```

In the agent constructor (where Config is consumed and the Agent is built), if `cfg.InjectPython` is true, construct the Manager:

```go
if cfg.InjectPython {
	low := &ptraceopBridge{inj: ptraceop.New(cfg.Logger)}
	a.pyInjector = python.NewManager(python.Options{
		StrictPerPID: cfg.PID != 0, // single-PID is strict; -a is lenient
		Logger:       cfg.Logger,
		Detector:     python.NewDetector("/proc", cfg.Logger),
		Injector:     low,
	})
}
```

Add the bridge type in `perfagent/agent.go`:

```go
// ptraceopBridge adapts ptraceop.Injector to the python.LowLevelInjector
// interface, supplying the activate/deactivate payloads from the inject/python
// package.
type ptraceopBridge struct {
	inj *ptraceop.Injector
}

func (b *ptraceopBridge) RemoteActivate(pid uint32, addrs python.SymbolAddrsForTarget) error {
	return b.inj.RemoteActivate(pid, ptraceop.SymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, python.ActivatePayload())
}

func (b *ptraceopBridge) RemoteDeactivate(pid uint32, addrs python.SymbolAddrsForTarget) error {
	return b.inj.RemoteDeactivate(pid, ptraceop.SymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, python.DeactivatePayload())
}
```

Modify `Agent.Start()` (or its equivalent) to run injection BEFORE BPF attach:

```go
func (a *Agent) Start(ctx context.Context) error {
	if a.pyInjector != nil {
		pids := a.scanPythonTargets()
		if err := a.pyInjector.ActivateAll(pids); err != nil {
			return fmt.Errorf("python injection: %w", err)
		}
	}
	// ...existing BPF attach code...
}

// scanPythonTargets returns the PIDs to consider for injection. For --pid mode,
// just [a.cfg.PID]. For -a mode, walks /proc and returns all numeric PID
// directories (Detect filters down to actual Python processes).
func (a *Agent) scanPythonTargets() []uint32 {
	if a.cfg.PID != 0 {
		return []uint32{a.cfg.PID}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	pids := make([]uint32, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		pids = append(pids, uint32(v))
	}
	return pids
}
```

Modify `Agent.Stop()`:

```go
func (a *Agent) Stop(ctx context.Context) error {
	// ...existing profile finalization...
	if a.pyInjector != nil {
		a.pyInjector.DeactivateAll(ctx)
	}
	// ...existing close code...
}
```

Add the public stats accessor:

```go
// PythonInjectStats returns counters for the Python injector. Returns a
// zero-value Stats pointer if --inject-python was not enabled.
func (a *Agent) PythonInjectStats() *python.Stats {
	if a.pyInjector == nil {
		return &python.Stats{}
	}
	return a.pyInjector.Stats()
}
```

In `main.go`, add the flag and wire it through:

```go
injectPython := flag.Bool("inject-python", false,
	"Inject sys.activate_stack_trampoline('perf') into running CPython 3.12+ targets via ptrace. Requires cap_sys_ptrace. Off by default.")
// ...later, when constructing the Agent:
opts = append(opts, perfagent.WithInjectPython(*injectPython))
```

Add the validation:
- `--inject-python` with `--offcpu` or `--pmu` (without `--profile`): return error.
- `--inject-python` without `--pid` and without `-a`: return same error as today (already handled by existing pid/all checks).
- `--inject-python` with neither `--profile` nor `--pid`/`-a`: error.

The cap precheck uses `unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, unix.CAP_SYS_PTRACE, 0, 0)` style — but in this codebase, look at how the existing cap checks are done (`cpu/cpu.go` has a precedent). Use the same helper.

- [ ] **Step 3: Build and verify it compiles**

```bash
make generate
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go build .
```
Expected: clean build.

- [ ] **Step 4: Run unit tests for affected packages**

```bash
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./inject/... ./perfagent/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go perfagent/agent.go main.go
git commit -m "perfagent: wire --inject-python flag into Agent lifecycle"
```

---

## Task 9: Mmap-watcher new-exec hook for late arrivals

**Files:**
- Modify: `unwind/ehmaps/tracker.go` (or wherever the mmap watcher lives — check via grep)
- Modify: `perfagent/agent.go` (subscribe to new-exec events)

For `-a --inject-python`, processes that exec during the run also need injection. The mmap watcher already detects new execs for CFI; we add a parallel hook for the Python injector.

- [ ] **Step 1: Map the existing watcher API**

```bash
grep -rn "OnNewExec\|NewMmapWatcher\|MmapWatcher" unwind/ perfagent/
```

- [ ] **Step 2: Add the hook with LAZY subscription**

**Hard requirement: zero overhead when `--inject-python` is off.** The watcher must not produce, buffer, or dispatch new-exec events when nobody is subscribed. This rules out:

- Always-on channels (even if no goroutine drains them, populating the channel costs CPU and may eventually block whatever produces the events).
- Slice-of-callbacks where an empty slice is iterated on every event.

Pick exactly one of these two patterns based on what the watcher already does:

**Pattern A — callback registration (preferred if the watcher has a constructor or hook-registration phase):**

```go
// In the watcher's package:
type Watcher struct {
    // ... existing fields ...
    onNewExec func(pid uint32)  // nil → no hook; producer skips dispatch entirely
}

// SetOnNewExec registers a hook. Pass nil to clear. Must be called before
// Start() (or while holding the watcher's lock) to avoid races.
func (w *Watcher) SetOnNewExec(fn func(pid uint32)) {
    w.onNewExec = fn
}

// Inside the watcher's event loop:
func (w *Watcher) handleExec(pid uint32) {
    // ... existing CFI-related work ...
    if w.onNewExec != nil {
        w.onNewExec(pid)  // single nil check; if no subscriber, fully skipped
    }
}
```

In `perfagent/agent.go`, register only when injection is enabled:

```go
if a.pyInjector != nil && a.cfg.PID == 0 {  // -a mode only
    a.watcher.SetOnNewExec(a.pyInjector.ActivateLate)
}
```

No goroutine, no channel, no producer-side cost when `pyInjector == nil`.

**Pattern B — Subscribe()-on-demand channel (use only if the watcher already serves multiple consumers via channels):**

```go
// In the watcher's package:
type Watcher struct {
    mu          sync.Mutex
    subscribers []chan<- uint32  // nil/empty → producer skips dispatch
}

// Subscribe returns a buffered channel (size 64) that receives new-exec PIDs.
// The channel is closed when the watcher shuts down. Returns a cancel func
// the caller should invoke to drop the subscription before its context ends.
func (w *Watcher) Subscribe() (<-chan uint32, func()) {
    ch := make(chan uint32, 64)
    w.mu.Lock()
    w.subscribers = append(w.subscribers, ch)
    w.mu.Unlock()
    cancel := func() {
        w.mu.Lock()
        defer w.mu.Unlock()
        for i, s := range w.subscribers {
            if s == ch {
                w.subscribers = append(w.subscribers[:i], w.subscribers[i+1:]...)
                close(ch)
                return
            }
        }
    }
    return ch, cancel
}

// Inside the watcher's event loop:
func (w *Watcher) handleExec(pid uint32) {
    w.mu.Lock()
    if len(w.subscribers) == 0 {
        w.mu.Unlock()
        return  // zero subscribers → zero work
    }
    subs := append([]chan<- uint32(nil), w.subscribers...)
    w.mu.Unlock()
    for _, ch := range subs {
        select {
        case ch <- pid:
        default:
            // subscriber's buffer full; drop. Slow consumers must not stall
            // the watcher.
        }
    }
}
```

In `perfagent/agent.go`:

```go
if a.pyInjector != nil && a.cfg.PID == 0 {
    ch, cancelSub := a.watcher.Subscribe()
    a.lateExecWG.Go(func() {
        defer cancelSub()
        for {
            select {
            case <-a.ctx.Done():
                return
            case pid, ok := <-ch:
                if !ok {
                    return
                }
                a.pyInjector.ActivateLate(pid)
            }
        }
    })
}
```

When no one calls `Subscribe()`, `len(w.subscribers) == 0` and `handleExec` returns after one mutex acquire — no allocation, no channel send.

**Choosing between A and B:**
- Pattern A is simpler and cheaper. Use it unless the existing watcher already has a multi-consumer channel API.
- Pattern B is appropriate if the codebase already uses Subscribe()-style channels elsewhere.

In either case: **the producer (the watcher's event loop) must do zero work when no subscriber is registered.** That's the invariant; the test below verifies it.

- [ ] **Step 3: Add unit tests — including a no-subscriber zero-overhead test**

Two tests required:

**Test 1: `TestNewExec_NoSubscribersZeroDispatch`** (in the watcher's test file). Construct a Watcher without registering any hook (Pattern A: don't call `SetOnNewExec`; Pattern B: don't call `Subscribe`). Drive a synthetic exec event into the watcher's `handleExec`. Assert that no observable side-effect occurs and (Pattern A) the `onNewExec` field is still nil.

The literal assertion is "the test passes" — what we're verifying is that the no-subscriber path doesn't panic, doesn't allocate (for B), and exits early. This guards the lazy-subscription invariant against future refactors.

**Test 2: `TestNewExec_HookFires`** (same file). Register a hook (Pattern A) or subscribe (Pattern B). Drive a synthetic exec event for `pid=12345`. Assert the hook saw `pid=12345`.

If the watcher is internally hard to test (e.g., spawns goroutines that read from a real perf event ring), introduce an internal `func handleExec(pid uint32)` method that the test can call directly while the production event loop calls the same method.

- [ ] **Step 4: Build + test**

```bash
go test ./inject/... ./perfagent/... ./unwind/...
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add unwind/ehmaps/tracker.go perfagent/agent.go
git commit -m "perfagent: subscribe inject/python to mmap-watcher new-exec events"
```

---

## Task 10: Bench harness flag

**Files:**
- Modify: `bench/cmd/scenario/main.go` (add `--inject-python` flag, plumb to Agent)
- Modify: `bench/internal/schema/schema.go` (add `InjectPython bool` field on Document)

- [ ] **Step 1: Read the existing flag definitions**

```bash
sed -n '1,60p' bench/cmd/scenario/main.go
grep -n "Document\|UnwindMode" bench/internal/schema/schema.go
```

- [ ] **Step 2: Add the schema field**

In `bench/internal/schema/schema.go`, on the Document struct, add:

```go
// InjectPython records whether --inject-python was enabled for this run.
// Persisted in the bench JSON for cross-run comparison; absent or false on
// runs predating the feature.
InjectPython bool `json:"inject_python,omitempty"`
```

- [ ] **Step 3: Add the bench flag and plumb it**

In `bench/cmd/scenario/main.go`, add a flag:

```go
injectPython := flag.Bool("inject-python", false, "Enable Python trampoline injection for the profiled fleet")
```

Pass it through to the Agent constructor:

```go
opts = append(opts, perfagent.WithInjectPython(*injectPython))
```

Persist into the schema:

```go
doc.InjectPython = *injectPython
```

- [ ] **Step 4: Build + test**

```bash
go build ./bench/...
go test ./bench/...
```
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add bench/cmd/scenario/main.go bench/internal/schema/schema.go
git commit -m "bench: --inject-python flag + InjectPython field on Document"
```

---

## Task 11: Integration tests (caps-gated)

**Files:**
- Create: `test/integration_inject_python_test.go`

Three integration tests per spec §9.2.

- [ ] **Step 1: Read existing integration test infrastructure**

```bash
sed -n '1,60p' test/integration_test.go
grep -n "func Test\|capsOK\|setcap" test/integration_test.go
```

- [ ] **Step 2: Implement TestInjectPython_ActivatesTrampoline**

Create `test/integration_inject_python_test.go`:

```go
package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInjectPython_ActivatesTrampoline(t *testing.T) {
	skipIfNoPerfAgentCaps(t)
	skipIfNoPython312Plus(t)

	// 1. Launch python WITHOUT -X perf
	pyCmd := exec.Command("python3", "test/workloads/python/cpu_bound.py", "10", "2")
	if err := pyCmd.Start(); err != nil {
		t.Fatalf("start python workload: %v", err)
	}
	pid := pyCmd.Process.Pid
	t.Cleanup(func() { _ = pyCmd.Process.Kill(); _ = pyCmd.Wait() })

	// 2. Wait for warmup
	time.Sleep(1 * time.Second)

	// 3. Run perf-agent --profile --inject-python
	profileOut := filepath.Join(t.TempDir(), "profile.pb.gz")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pa := exec.CommandContext(ctx, "../perf-agent",
		"--profile",
		"--inject-python",
		"--pid", fmt.Sprintf("%d", pid),
		"--duration", "5s",
		"--profile-output", profileOut,
	)
	out, err := pa.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\n%s", err, out)
	}

	// 4. Assert /tmp/perf-<PID>.map exists
	perfMap := fmt.Sprintf("/tmp/perf-%d.map", pid)
	st, err := os.Stat(perfMap)
	if err != nil {
		t.Fatalf("perf map %s not created: %v\nperf-agent output:\n%s", perfMap, err, out)
	}
	if st.Size() == 0 {
		t.Fatalf("perf map %s is empty", perfMap)
	}
	pmContent, _ := os.ReadFile(perfMap)
	if !strings.Contains(string(pmContent), "py::") {
		t.Errorf("perf map missing py:: entries:\n%s", pmContent)
	}

	// 5. Assert profile has Python frame names
	pprofTop := exec.CommandContext(ctx, "go", "tool", "pprof", "-top", "-nodecount=20", profileOut)
	topOut, err := pprofTop.CombinedOutput()
	if err != nil {
		t.Logf("pprof -top error (non-fatal): %v", err)
	}
	if !strings.Contains(string(topOut), "py::") &&
		!strings.Contains(string(topOut), "cpu_bound.py") {
		t.Errorf("profile lacks Python frame names:\n%s", topOut)
	}

	// 6. perf-map size should not grow after perf-agent exits (deactivation ran)
	size1 := fileSize(t, perfMap)
	time.Sleep(1 * time.Second)
	size2 := fileSize(t, perfMap)
	if size2 != size1 {
		t.Errorf("perf-map grew after perf-agent exit (deactivation didn't run): %d → %d",
			size1, size2)
	}
}

func TestInjectPython_StrictFailsOnNonPython(t *testing.T) {
	skipIfNoPerfAgentCaps(t)

	// Launch a Go workload (not Python)
	goCmd := exec.Command("test/workloads/go/cpu_bound")
	if err := goCmd.Start(); err != nil {
		t.Fatalf("start go workload: %v", err)
	}
	pid := goCmd.Process.Pid
	t.Cleanup(func() { _ = goCmd.Process.Kill(); _ = goCmd.Wait() })

	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pa := exec.CommandContext(ctx, "../perf-agent",
		"--profile", "--inject-python",
		"--pid", fmt.Sprintf("%d", pid),
		"--duration", "2s",
	)
	out, err := pa.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; got success\noutput:\n%s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected ExitError; got %T: %v", err, err)
	}
	if !strings.Contains(string(out), "not_python") &&
		!strings.Contains(string(out), "not a python") {
		t.Errorf("output missing structured 'not python' reason:\n%s", out)
	}
}

func skipIfNoPerfAgentCaps(t *testing.T) {
	t.Helper()
	// Reuse the helper from integration_test.go (assumes capsOK or similar exists).
	if !haveCaps(t) {
		t.Skip("requires cap_sys_admin/cap_bpf/cap_perfmon/cap_sys_ptrace; setcap on ../perf-agent")
	}
}

func skipIfNoPython312Plus(t *testing.T) {
	t.Helper()
	out, err := exec.Command("python3", "-c",
		"import sys; print(sys.version_info >= (3, 12))").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "True") {
		t.Skipf("requires python3 >= 3.12; got: %s (err=%v)", out, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}

// haveCaps is a helper that should be implemented based on integration_test.go's
// existing capsOK / capPresent helpers. If they exist with another name,
// rename here; the spec is "true if perf-agent will be able to use BPF +
// ptrace without sudo".
func haveCaps(t *testing.T) bool {
	t.Helper()
	// Implementer: copy the pattern from integration_test.go. If that file uses
	// a function called `capsOK`, change this body to `return capsOK(t)`. Same
	// for `capPresent`.
	return true // placeholder — implementer fills in real cap check
}
```

The `haveCaps` placeholder MUST be replaced by the implementer with the actual helper from `integration_test.go`. Read that file first and copy the exact helper signature and body.

- [ ] **Step 3: Build the perf-agent binary in the worktree (NOT /tmp — caps don't survive nosuid)**

```bash
make generate
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go build .
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
```

- [ ] **Step 4: Run integration tests**

```bash
cd test
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test -v -run "TestInjectPython" ./...
```
Expected: PASS for both tests (or skip if Python 3.12+ not available).

- [ ] **Step 5: Commit**

```bash
git add test/integration_inject_python_test.go
git commit -m "test: integration tests for --inject-python (caps-gated)"
```

---

## Task 12: Documentation

**Files:**
- Modify: `README.md` (add Python profiling section)
- Create: `docs/python-profiling.md`

- [ ] **Step 1: Add to README.md**

Find the "Examples" or "Usage" section in README.md. Append:

```markdown
### Profiling running Python processes

For Python 3.12+ processes, perf-agent can activate the perf trampoline at
profile start without restarting the target — no need for `python -X perf`:

```bash
sudo perf-agent --profile --pid $(pgrep -f myapp.py) \
                --duration 30s --inject-python
```

The trampoline emits Python qualnames to `/tmp/perf-<PID>.map`, which
perf-agent reads via blazesym to attach human-readable names to JIT'd
frames. perf-agent automatically deactivates the trampoline at end of
profile, so the per-call overhead does not persist past the profiling window.

For system-wide injection (`-a`), perf-agent activates every detected Python
3.12+ process and tolerates per-process failures (e.g., processes built
without `--enable-perf-trampoline`):

```bash
sudo perf-agent --profile -a --duration 30s --inject-python
```

Requires `cap_sys_ptrace` (already in the standard cap set).
See [docs/python-profiling.md](docs/python-profiling.md) for details.
```

- [ ] **Step 2: Create docs/python-profiling.md**

Create `docs/python-profiling.md`:

```markdown
# Python profiling with perf-agent

perf-agent supports two paths for Python frame symbolization:

1. **DWARF unwinding through the interpreter** (default for any Python).
   Profiles work; frames render with C-level names (`_PyEval_EvalFrameDefault`,
   etc.) — no Python-level qualnames.

2. **Perf trampoline** (`--inject-python`, Python 3.12+). Activates CPython's
   built-in perf integration so perf-agent can resolve every JIT'd Python
   function to its qualname.

## Quickstart

```bash
# Profile a running Python web server for 30 seconds
sudo perf-agent --profile --pid $(pgrep -f gunicorn) \
                --duration 30s --inject-python \
                --profile-output gunicorn.pb.gz

# Visualize
go tool pprof -http :8080 gunicorn.pb.gz
```

## How it works

When `--inject-python` is set, perf-agent:

1. Walks `/proc` (or just the target PID) and identifies CPython 3.12+
   processes via libpython SONAME and ELF symbol presence.
2. For each candidate, attaches via `ptrace`, calls
   `PyGILState_Ensure` → `PyRun_SimpleString("import sys; sys.activate_stack_trampoline('perf')")` → `PyGILState_Release`,
   and detaches.
3. The trampoline emits perf-map entries to `/tmp/perf-<PID>.map`.
4. perf-agent samples and reads the perf-map via blazesym to attach Python
   names to frames.
5. On profile end, the same ptrace dance runs `sys.deactivate_stack_trampoline()`
   so the trampoline overhead does not persist.

## When injection is skipped

| Reason | Cause |
|---|---|
| `not_python` | Target is not a CPython process (e.g., Go binary) |
| `python_too_old` | libpython version < 3.12 (`activate_stack_trampoline` is 3.12+) |
| `no_perf_trampoline` | CPython compiled without `--enable-perf-trampoline` (e.g., some Alpine builds) |
| `no_libpython_symbols` | Statically-linked CPython without exported PyGILState/PyRun symbols |
| `preexisting_perf_map` | `/tmp/perf-<PID>.map` already exists — trampoline activated by a prior run or user code; we never deactivate state we didn't activate |

In `--pid N` mode (strict), any of these failures aborts the run.
In `-a` mode (lenient), failures are logged and the profile continues for
all targets where injection succeeded.

## Idempotency

perf-agent never deactivates a trampoline it didn't activate. If
`/tmp/perf-<PID>.map` exists at activation time, perf-agent records the
target as "preexisting" and skips both activation and deactivation. This
guarantees that:

- Concurrent perf-agent runs don't stomp on each other.
- A user who runs `python -X perf` and then runs perf-agent against the
  result keeps the trampoline active after perf-agent exits.

If a previous perf-agent run was killed and left a trampoline active, the
next run won't deactivate it. Manual cleanup: `rm /tmp/perf-PID.map` after
process exit.

## Container and namespace caveats

v1 uses host-side PIDs. If perf-agent runs outside a container and targets a
Python process inside one:

- The host PID works for ptrace and detection (`/proc/<pid>/maps` + on-disk
  libpython path).
- The perf-map file `/tmp/perf-<host_pid>.map` is created on the host —
  the container itself does not see it. This matches `python -X perf`
  behavior under host-mounted `/tmp`.
- For exotic mount namespace setups, detection may fail with "library not
  found on disk" — log + skip in lenient mode.

A future PR can add namespace-aware path resolution; the seam is small (one
function: "given pid, give me the on-disk libpython path"). File an issue if
you hit this.

## Performance impact

The CPython 3.12 perf trampoline adds 1–5% per-call overhead on hot Python
workloads, depending on call shape. For typical web servers and pipelines,
overhead is in the noise. perf-agent's deactivation pass at end of profile
removes this overhead immediately.

## Disabling injection

Don't pass `--inject-python`. Profiles still work — Python frames just
render with C interpreter names instead of qualnames.

## Troubleshooting

**`ptrace_eperm` errors:** the target's `/proc/sys/kernel/yama/ptrace_scope`
is restricting ptrace. Set `0` (or `1` for same-uid attach), or grant
`cap_sys_ptrace` to perf-agent (already in the standard cap set).

**`ESRCH` during deactivate:** the target exited during the profile.
Harmless; logged with `pid=N reason=process_gone`.

**`/tmp/perf-PID.map` missing after activation:** the target may not have
called any new Python code during the profile, so the trampoline had nothing
to emit. Lengthen `--duration`.

**Statically-linked Python skipped (`no_libpython_symbols`):** the binary's
symbol table is stripped. Distributions that ship `python-build-standalone`
sometimes do this. No workaround in v1.
```

- [ ] **Step 3: Commit**

```bash
git add README.md docs/python-profiling.md
git commit -m "docs: --inject-python README section + docs/python-profiling.md"
```

---

## Self-review

Before declaring the plan complete, walk through the spec one last time:

**Spec coverage:**

- §3.1 Python 3.12+ only → Task 4 detection ladder enforces version gate.
- §3.2 `--profile --inject-python` flag → Task 8 main.go.
- §3.3 Activate on start, deactivate on end → Tasks 7 (ActivateAll/DeactivateAll), 8 (wired into Agent.Start/Stop).
- §3.4 Pure Go via x/sys/unix → Tasks 5, 6.
- §3.5 Strict per-PID, lenient -a → Task 7 ActivateAll, Task 8 sets StrictPerPID = (PID != 0).
- §3.6 Both archs in v1 → Tasks 5 (amd64), 6 (arm64).
- §3.7 SONAME or /proc/<pid>/exe + debug/elf → Tasks 2, 3, 4.
- §3.8 Inject before BPF attach → Task 8 calls ActivateAll before BPF.
- §3.9 Unit + integration → Tasks 1-7 (unit), Task 11 (integration).

- §4.1 Layout (inject/elfsym, inject/ptraceop, inject/python) → Tasks 1-7.
- §4.2 Public surface → Task 7.
- §4.3 perfagent.Agent integration → Task 8.

- §5 Detection ladder → Task 4.
- §6 Injection sequence (3-call wrap, SIGSEGV-at-0, stack payload) → Tasks 5, 6.
- §7 Lifecycle (preexisting marker, bounded shutdown, late arrivals) → Tasks 7, 9.
- §8 CLI flag + behavior matrix + cap precheck + logging + bench → Tasks 8, 10.
- §9 Test plan → Tasks 1-7 unit + Task 11 integration.
- §10 Documentation → Task 12.
- §11 Risks (latency, GIL re-entrance, stack headroom, symbol versioning) → noted in code comments.
- §12 References → in spec, not in plan.

**Placeholder scan:** One placeholder remains in Task 11 step 2: the `haveCaps` helper. The plan explicitly tells the implementer to copy the existing helper from `integration_test.go`. This is acceptable — the plan doesn't have access to read other files at write time, and the instruction is concrete.

**Type consistency:**

- `Target` (defined in Task 4) used by Tasks 7, 8, 9. ✓
- `SymbolAddrsForTarget` (Task 7) used by Tasks 7, 8, 9. ✓
- `LowLevelInjector` interface (Task 7) implemented by `ptraceopBridge` (Task 8). ✓
- `Stats` (Task 7) referenced as `*python.Stats` (Task 8). ✓
- `Detector` interface (Task 4) implemented by `procDetector` (Task 4) and stub (Task 7). ✓

**Spec requirement with no task: none found.**
