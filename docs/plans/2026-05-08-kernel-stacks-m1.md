# Kernel-stack capture and symbolization — M1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make perf-agent's pprof + `--perf-data-output` resolve kernel-mode stack frames natively. The BPF programs have a kernel-stack capture path that is currently gated off; this plan flips those gates and wires the kernel chain through userspace stack lookup, symbolization (via blazesym's kernel source over cgo), pprof emission (using the existing `Frame.IsKernel` + `kernelSentinel` machinery), and the kernel `MMAP2` record in `--perf-data-output`.

**Architecture:** Flip the BPF kernel-stack capture gate (one .c line + two userspace `CollectKernel: 1` flags). New `symbolize.KernelSymbolizer` interface (separate type from user-mode `Symbolizer` — kernel resolution has no PID and a different blazesym source). One implementation in M1 — `LocalKernelSymbolizer` — wrapping `blaze_symbolize_kernel_abs_addrs` via cgo with `debug_syms=true` so blazesym's transparent module-DWARF support resolves source `:line` for module functions when distro debug-info is installed; `NoopKernelSymbolizer` fallback when `/proc/kallsyms` is locked down. Each profiler reads `key.KernStack`, looks it up in the same `Stackmap`, calls the kernel symbolizer, and merges the result with user frames; kernel frames carry `IsKernel=true` (via `ToProfFramesKernel`) so the existing pprof builder routes them through `kernelSentinel`. `internal/perfdata.Writer` gains `AddKernelMmap()` (catch-all kernel address range) invoked at writer init, plus `SampleRecord.KernelIPs` + `PERF_CONTEXT_{KERNEL,USER}` markers in the encoded callchain. Kernel-stack capture is opt-in via the new `--kernel-stacks` CLI flag (`perfagent.WithKernelStacks()` library setter, `Config.KernelStacks bool` field). When the flag is OFF (default), the BPF program's new `kernel_stacks_enabled` volatile global stays `false` and no kernel work runs in either kernel-side or userspace.

**Tech Stack:** Go 1.26, cgo against `libblazesym_c` (already in the build env), `/proc/kallsyms`, `/sys/kernel/notes`. No new external Go modules. BPF program changes are minimal: one-line gate flip per .bpf.c file + bpf2go regen.

**Authoritative spec:** `docs/specs/2026-05-08-kernel-stacks-design.md`. Read it before starting.

**Out of scope (deferred to M2+):** module debuginfod fetch (`.ko.debug` artifacts), `--kernel-symbols={auto,require,disable}` enum flag, inline kernel function expansion, per-syscall classification labels, PMU-mode kernel stacks.

---

## File structure

**New files:**

```
symbolize/
  kernel.go             KernelSymbolizer interface + NoopKernelSymbolizer + MergeKernelFirst + ToProfFramesKernel + ErrKernelSymbolsUnavailable
  kernel_test.go
  local_kernel.go       LocalKernelSymbolizer (cgo wrap of blaze_symbolize_kernel_abs_addrs)
  local_kernel_test.go
```

**Modified files:**

```
profile/profiler.go              read KernStack, symbolize, merge; constructor gains kernelSym param; kernel_stacks_enabled setter
profile/dwarf_export.go          kernel_stacks_enabled setter (LoadPerfDwarf); CollectKernel flip
offcpu/profiler.go               read KernStack, symbolize, merge; same shape as profile/profiler.go; kernel_stacks_enabled setter
profile/offcpu_dwarf_export.go   kernel_stacks_enabled setter (LoadOffCPUDwarf); CollectKernel flip
unwind/dwarfagent/symbolize.go   symbolizePID gains kernelSym param + kernelIPs
unwind/dwarfagent/common.go      session struct + newSession signature gain kernelSym
unwind/dwarfagent/agent.go       NewProfiler/NewProfilerWithMode/NewProfilerWithHooks signatures
unwind/dwarfagent/offcpu.go      NewOffCPUProfiler/WithHooks signatures
internal/perfdata/perfdata.go    new (*Writer).AddKernelMmap() method
internal/perfdata/perfdata_test.go test for AddKernelMmap
perfagent/agent.go               Agent gains kernelSymbolizer field; chooseKernelSymbolizer factory; threaded into all profilers; AddKernelMmap call at writer init
test/integration_test.go         kernel-stack pprof + perf.data MMAP2 tests
test/debuginfod_integration_test.go   (no changes — that test stays user-only)
```

---

## Phase 0 — Bump blazesym pin + enable kernel-stack capture

### Task 0a: Bump blazesym pin past commit `29a609f`

**Files:** Read-only on `/home/diego/github/blazesym/`; perf-agent's pin is implicit in the local checkout (Makefile expects `/home/diego/github/blazesym/`).

The kernel-module DWARF support landed on blazesym main past our v1.1.0 pin (`ebb5b50`). Fast-forward the local checkout to a commit ≥ `29a609f` ("Confine kernel module symbolization to module range"; the most recent of the kernel-module commits at spec-write time).

- [ ] **Step 1: Fast-forward the local blazesym checkout**

```bash
cd /home/diego/github/blazesym
git fetch origin
# Check that 29a609f exists on origin/main:
git log --oneline origin/main | head -20 | grep -E "29a609f|f3cf4dc" || \
  echo "WARNING: kernel-module DWARF commits not found in origin/main — pin may need updating"
# Check out a recent main:
git checkout origin/main
git log -1 --format="%H %s"
```

Expected: HEAD ≥ `29a609f`, `git log -1` shows a recent commit.

- [ ] **Step 2: Rebuild `libblazesym_c.a`**

```bash
cd /home/diego/github/blazesym
cargo build --release -p blazesym-c
ls -la target/release/libblazesym_c.a
```

Expected: file exists, mtime is recent.

- [ ] **Step 3: Confirm `make build` still works**

```bash
cd /home/diego/github/perf-agent/.worktrees/kernel-stacks-m1
make build
```

Expected: clean build, `perf-agent` binary produced.

- [ ] **Step 4: Update the Makefile guard** (the existing v1.1.0 guard checks for `process_dispatch`; extend to also check the kernel API exists for the bumped pin)

In `Makefile`, the existing `blazesym-check` target checks for `process_dispatch`. Add a sibling check for `blaze_symbolize_kernel_abs_addrs` in the same block:

```make
.PHONY: blazesym-check
blazesym-check:
	@if ! grep -q 'process_dispatch' $(LIBBLAZESYM_INC)/blazesym.h; then \
		echo "*** blazesym header missing process_dispatch — bump pin to ≥ 8891e70"; \
		exit 1; \
	fi
	@if ! grep -q 'blaze_symbolize_kernel_abs_addrs' $(LIBBLAZESYM_INC)/blazesym.h; then \
		echo "*** blazesym header missing blaze_symbolize_kernel_abs_addrs — bump pin to ≥ 29a609f"; \
		exit 1; \
	fi
```

(Read the existing target first; extend the body, don't replace it. The exact text for the existing guard may differ slightly.)

- [ ] **Step 5: Commit (Makefile only — blazesym is external)**

```bash
git add Makefile
git commit -m "build: extend blazesym header guard to require kernel-module DWARF API"
```

### Task 0b: Flip BPF kernel-stack capture gate

**Files:**
- Modify: `bpf/perf.bpf.c`
- Modify: `bpf/perf_dwarf.bpf.c` (mirror — confirm by reading the file first)
- Modify: `bpf/offcpu.bpf.c`
- Modify: `bpf/offcpu_dwarf.bpf.c` (mirror)
- Modify: `profile/profiler.go` (line 66 area: `pid_config.CollectKernel`)
- Modify: `profile/dwarf_export.go` (line 71 area: `perf_dwarfPidConfig.CollectKernel`)
- Modify: `offcpu/profiler.go` (offcpu equivalent of `pid_config` if it has one)
- Regenerate: `profile/perf_x86_bpfel.go`, `profile/perf_arm64_bpfel.go`, `profile/offcpu_dwarf_x86_bpfel.go`, `profile/offcpu_dwarf_arm64_bpfel.go`, `offcpu/offcpu_x86_bpfel.go`, `offcpu/offcpu_arm64_bpfel.go`

- [ ] **Step 1: Inspect every BPF source for the system-wide ternary**

```bash
grep -nE "system_wide \? false" bpf/*.bpf.c
```

Expected: matches in `bpf/perf.bpf.c` and at least one of `bpf/perf_dwarf.bpf.c`, `bpf/offcpu.bpf.c`, `bpf/offcpu_dwarf.bpf.c`. Each match needs the same flip.

- [ ] **Step 2: Add `kernel_stacks_enabled` volatile global + gate the kernel-stack capture**

In each affected BPF source (`bpf/perf.bpf.c`, `bpf/perf_dwarf.bpf.c`,
`bpf/offcpu.bpf.c`, `bpf/offcpu_dwarf.bpf.c`), add a new volatile global
near the existing `system_wide` global:

```c
const volatile bool kernel_stacks_enabled = false;
```

Then change the kernel-stack ternary from:

```c
bool collect_kernel = system_wide ? false : (config && config->collect_kernel);
```

to:

```c
bool collect_kernel = false;
if (kernel_stacks_enabled) {
    collect_kernel = system_wide ? true : (config && config->collect_kernel);
}
```

This gives zero per-sample cost when the flag is off (the entire kernel-stack
branch is gated by `kernel_stacks_enabled`).

If a particular BPF program lacks a `system_wide ? false` ternary, inspect
and adapt — read each file before editing.

- [ ] **Step 3: Flip `CollectKernel: 1` in the userspace `pid_config` setters**

`profile/profiler.go` around line 66:

```go
// before:
config := perfPidConfig{
    Type:          0,
    CollectUser:   1,
    CollectKernel: 0,
}
// after:
config := perfPidConfig{
    Type:          0,
    CollectUser:   1,
    CollectKernel: 1,
}
```

`profile/dwarf_export.go` around line 71: same pattern with `perf_dwarfPidConfig`.

`offcpu/profiler.go`: search for `pid_config` or `CollectKernel` and apply the same flip if present.

```bash
grep -rn "CollectKernel" --include="*.go"
```

The BPF gate (`kernel_stacks_enabled && collect_kernel`) means the
config bit only takes effect when the flag is on.

- [ ] **Step 4: Userspace setter for the volatile global**

The existing v1.1.0 setter pattern lives in each BPF loader path —
`spec.Variables["system_wide"].Set(systemWide)`. Add a sibling setter in
**all four** loaders:

- `profile/profiler.go` (FP CPU)
- `profile/dwarf_export.go` (DWARF CPU; `LoadPerfDwarf`)
- `offcpu/profiler.go` (FP off-CPU)
- `profile/offcpu_dwarf_export.go` (DWARF off-CPU; `LoadOffCPUDwarf`)

In each, after the existing `system_wide` setter:

```go
// Set kernel_stacks_enabled before LoadAndAssign so the BPF program's
// gate evaluates correctly on first sample.
if err := spec.Variables["kernel_stacks_enabled"].Set(cfg.KernelStacks); err != nil {
    return nil, fmt.Errorf("set kernel_stacks_enabled: %w", err)
}
```

The constructor signatures must thread `cfg.KernelStacks` (or the equivalent
single bool) through. Where loaders are called from `dwarfagent` constructors,
add the bool parameter to those constructors too — same pattern as the
v1.1.0 `symbolize.Symbolizer` plumbing in PR #19's commit `3b754842`.

`profile/profiler.go:66`, `profile/dwarf_export.go:71`, and
`profile/offcpu_dwarf_export.go:62` already set `CollectKernel: 0` in the
targeted-mode `pid_config`. Flip all three to `1`. The BPF gate
(`kernel_stacks_enabled && collect_kernel`) means the config bit only
takes effect when the flag is on.

(This step's code lands here; Task 6a below adds the `Config.KernelStacks`
field and CLI flag that `cfg.KernelStacks` refers to.)

Expected: `bpf2go` regenerates `*_bpfel.go` files (and their `.o` counterparts in `profile/`, `offcpu/`). Verify via `git status` that the regenerated files have small diffs (struct fields unchanged; only embedded bytecode bytes change).

- [ ] **Step 6: Run unit tests, confirm no regressions**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add bpf/ profile/ offcpu/
git commit -m "bpf: add kernel_stacks_enabled volatile global; gate kernel-stack capture"
```

---

## Phase 1 — `symbolize.KernelSymbolizer` interface + impls

### Task 1: Interface + `NoopKernelSymbolizer` + helpers

**Files:**
- Create: `symbolize/kernel.go`
- Create: `symbolize/kernel_test.go`

- [ ] **Step 1: Write the failing test**

`symbolize/kernel_test.go`:

```go
package symbolize

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/pprof"
)

func TestNoopKernelSymbolizer(t *testing.T) {
	var s NoopKernelSymbolizer
	frames, err := s.SymbolizeKernel([]uint64{0xffffffff8100abcd, 0xffffffff8100ef01})
	if err != nil {
		t.Fatalf("SymbolizeKernel: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Name != "0xffffffff8100abcd" {
		t.Errorf("frame[0].Name = %q, want hex form", frames[0].Name)
	}
	if frames[0].Address != 0xffffffff8100abcd {
		t.Errorf("frame[0].Address = %#x, want input IP", frames[0].Address)
	}
	if frames[0].Reason != FailureMissingSymbols {
		t.Errorf("frame[0].Reason = %s, want FailureMissingSymbols", frames[0].Reason)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNoopKernelSymbolizerEmpty(t *testing.T) {
	var s NoopKernelSymbolizer
	frames, err := s.SymbolizeKernel(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if frames != nil {
		t.Fatalf("frames = %+v, want nil", frames)
	}
}

func TestMergeKernelFirst(t *testing.T) {
	kernel := []Frame{{Name: "kfn1"}, {Name: "kfn2"}}
	user := []Frame{{Name: "ufn1"}, {Name: "ufn2"}}

	got := MergeKernelFirst(kernel, user)
	want := []Frame{{Name: "kfn1"}, {Name: "kfn2"}, {Name: "ufn1"}, {Name: "ufn2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeKernelFirst: got %+v, want %+v", got, want)
	}
}

func TestMergeKernelFirstEmptyKernel(t *testing.T) {
	user := []Frame{{Name: "ufn1"}}
	got := MergeKernelFirst(nil, user)
	if !reflect.DeepEqual(got, user) {
		t.Fatalf("got %+v, want %+v", got, user)
	}
}

func TestMergeKernelFirstEmptyUser(t *testing.T) {
	kernel := []Frame{{Name: "kfn1"}}
	got := MergeKernelFirst(kernel, nil)
	if !reflect.DeepEqual(got, kernel) {
		t.Fatalf("got %+v, want %+v", got, kernel)
	}
}

func TestToProfFramesKernelSetsIsKernel(t *testing.T) {
	in := []Frame{{Address: 0xffff800000001000, Name: "do_sys_openat2", Module: "[kernel.kallsyms]"}}
	got := ToProfFramesKernel(in)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if !got[0].IsKernel {
		t.Errorf("IsKernel = false, want true")
	}
	if got[0].Name != "do_sys_openat2" {
		t.Errorf("Name = %q", got[0].Name)
	}
	// Sanity: ToProfFrames-without-kernel shouldn't set the flag.
	plain := ToProfFrames(in)
	if plain[0].IsKernel {
		t.Errorf("ToProfFrames set IsKernel; should only be set by ToProfFramesKernel")
	}
}

func TestErrKernelSymbolsUnavailable(t *testing.T) {
	if !errors.Is(ErrKernelSymbolsUnavailable, ErrKernelSymbolsUnavailable) {
		t.Fatal("ErrKernelSymbolsUnavailable should be matchable via errors.Is")
	}
	if ErrKernelSymbolsUnavailable.Error() == "" {
		t.Fatal("ErrKernelSymbolsUnavailable.Error() must be non-empty")
	}
	// Type-only smoke-check — the symbol type must satisfy the interface.
	var _ KernelSymbolizer = NoopKernelSymbolizer{}
	_ = pprof.Frame{} // keep import used
}
```

- [ ] **Step 2: Run, confirm fail**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./symbolize/... -run "Noop|Merge|ToProfFramesKernel|ErrKernelSymbolsUnavailable"
```

Expected: FAIL — undefined `KernelSymbolizer`, `NoopKernelSymbolizer`, `MergeKernelFirst`, `ToProfFramesKernel`, `ErrKernelSymbolsUnavailable`.

- [ ] **Step 3: Implement `symbolize/kernel.go`**

```go
package symbolize

import (
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/pprof"
)

// ErrKernelSymbolsUnavailable indicates /proc/kallsyms is unreadable or
// kptr-restricted (kernel addresses come back as zeros). Callers SHOULD
// construct a NoopKernelSymbolizer and continue rather than fail.
var ErrKernelSymbolsUnavailable = errors.New("symbolize: kernel symbols unavailable (kptr_restrict?)")

// KernelSymbolizer resolves kernel-mode addresses to symbolic frames.
// Kernel-mode resolution has no PID — kernel + module symbols are
// global. Implementations are safe for concurrent use.
type KernelSymbolizer interface {
	SymbolizeKernel(ips []uint64) ([]Frame, error)
	Close() error
}

// NoopKernelSymbolizer returns a Frame per IP with Name = "0x<hex>" and
// Reason = FailureMissingSymbols. Used when kallsyms is locked down.
type NoopKernelSymbolizer struct{}

// SymbolizeKernel returns one Frame per input IP with the address
// rendered as a hex string in Name. Address is preserved so pprof
// Locations stay distinguishable.
func (NoopKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]Frame, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	out := make([]Frame, len(ips))
	for i, ip := range ips {
		out[i] = Frame{
			Address: ip,
			Name:    fmt.Sprintf("0x%x", ip),
			Reason:  FailureMissingSymbols,
		}
	}
	return out, nil
}

// Close is a no-op.
func (NoopKernelSymbolizer) Close() error { return nil }

// MergeKernelFirst returns a leaf-first frame chain by prepending kernel
// frames (already leaf-first per blazesym convention) onto user frames.
// Either slice may be nil.
func MergeKernelFirst(kernel, user []Frame) []Frame {
	if len(kernel) == 0 {
		return user
	}
	if len(user) == 0 {
		return kernel
	}
	out := make([]Frame, 0, len(kernel)+len(user))
	out = append(out, kernel...)
	out = append(out, user...)
	return out
}

// ToProfFramesKernel is ToProfFrames + IsKernel=true on every output frame.
// pprof.ProfileBuilder routes IsKernel frames through the [kernel] sentinel
// mapping, regardless of PID.
func ToProfFramesKernel(frames []Frame) []pprof.Frame {
	out := ToProfFrames(frames)
	for i := range out {
		out[i].IsKernel = true
	}
	return out
}
```

- [ ] **Step 4: Run, confirm pass**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 -v ./symbolize/... -run "Noop|Merge|ToProfFramesKernel|ErrKernelSymbolsUnavailable"
```

Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add symbolize/kernel.go symbolize/kernel_test.go
git commit -m "symbolize: add KernelSymbolizer interface, NoopKernelSymbolizer, merge helpers"
```

---

### Task 2: `LocalKernelSymbolizer` (cgo wrap of `blaze_symbolize_kernel_abs_addrs`)

**Files:**
- Create: `symbolize/local_kernel.go`
- Create: `symbolize/local_kernel_test.go`

- [ ] **Step 1: Write the failing test**

`symbolize/local_kernel_test.go`:

```go
package symbolize

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// kallsymsReadable reports whether /proc/kallsyms returns real (non-zero)
// addresses to the current process. With kptr_restrict=2 the kernel zeros
// every address; with kptr_restrict=0 we get the real ones (root or
// CAP_SYSLOG required when kptr_restrict=1).
func kallsymsReadable() bool {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 1 {
		return false
	}
	n, err := strconv.ParseUint(fields[0], 16, 64)
	if err != nil {
		return false
	}
	return n != 0
}

// pickKnownKernelSymbol returns one (addr, name) pair from /proc/kallsyms
// suitable for round-trip testing. Picks the first 'T' (text) symbol with
// a non-zero address.
func pickKnownKernelSymbol(t *testing.T) (uint64, string) {
	t.Helper()
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		t.Skipf("/proc/kallsyms: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		typ := fields[1]
		if typ != "T" && typ != "t" {
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil || addr == 0 {
			continue
		}
		return addr, fields[2]
	}
	t.Skip("no usable kernel text symbol in /proc/kallsyms")
	return 0, ""
}

func TestNewLocalKernelSymbolizer(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0 (or root + CAP_SYSLOG)")
	}
	s, err := NewLocalKernelSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalKernelSymbolizer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLocalKernelSymbolizerRoundTrip(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0 (or root + CAP_SYSLOG)")
	}
	s, err := NewLocalKernelSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalKernelSymbolizer: %v", err)
	}
	defer func() { _ = s.Close() }()

	addr, name := pickKnownKernelSymbol(t)
	frames, err := s.SymbolizeKernel([]uint64{addr})
	if err != nil {
		t.Fatalf("SymbolizeKernel: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Name != name {
		t.Errorf("name = %q, want %q (addr %#x)", frames[0].Name, name, addr)
	}
}

func TestLocalKernelSymbolizerCloseIdempotent(t *testing.T) {
	if !kallsymsReadable() {
		t.Skip("requires kptr_restrict=0")
	}
	s, err := NewLocalKernelSymbolizer()
	if err != nil {
		t.Fatalf("NewLocalKernelSymbolizer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not panic; either err or nil is acceptable.
	if err := s.Close(); err != nil && !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: unexpected err %v", err)
	}
}

// Compile-time check that LocalKernelSymbolizer satisfies KernelSymbolizer.
var _ KernelSymbolizer = (*LocalKernelSymbolizer)(nil)

// Keep cap import used (mirrors the existing local_test.go cap-aware skip
// pattern; useful when kallsyms-readable depends on CAP_SYSLOG).
var _ = cap.Effective
```

- [ ] **Step 2: Run, confirm fail**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./symbolize/... -run LocalKernel
```

Expected: FAIL — `LocalKernelSymbolizer` undefined.

- [ ] **Step 3: Implement `symbolize/local_kernel.go`**

```go
package symbolize

/*
#include <stdlib.h>
#include <string.h>
#include "blazesym.h"

static blaze_symbolizer_opts make_kernel_opts(_Bool code_info, _Bool inlined_fns, _Bool demangle) {
    blaze_symbolizer_opts opts;
    memset(&opts, 0, sizeof(opts));
    opts.type_size = sizeof(opts);
    opts.auto_reload = 1;
    opts.code_info = code_info;
    opts.inlined_fns = inlined_fns;
    opts.demangle = demangle;
    return opts;
}

static blaze_symbolize_src_kernel make_kernel_src(void) {
    blaze_symbolize_src_kernel src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
    // kallsyms = NULL, vmlinux = NULL → blazesym uses /proc/kallsyms +
    // discovers vmlinux on disk if it can.
    return src;
}

// sym_at_kernel mirrors the user-mode sym_at helper in
// symbolize/debuginfod/dispatcher.go: lets Go index the trailing
// flexible array without performing pointer arithmetic on the Go side.
static const blaze_sym* sym_at_kernel(const blaze_syms* syms, size_t i) {
    return &syms->syms[i];
}
*/
import "C"

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// LocalKernelSymbolizer wraps blazesym's kernel source: /proc/kallsyms
// for vmlinux + every loaded module's symbols.
type LocalKernelSymbolizer struct {
	csym   *C.blaze_symbolizer
	closed atomic.Bool
	mu     sync.Mutex
}

// NewLocalKernelSymbolizer returns a kernel symbolizer or
// ErrKernelSymbolsUnavailable when /proc/kallsyms is unreadable or
// kptr-restricted (first symbol address is 0).
func NewLocalKernelSymbolizer() (*LocalKernelSymbolizer, error) {
	if !kallsymsReadableInternal() {
		return nil, ErrKernelSymbolsUnavailable
	}

	copts := C.make_kernel_opts(
		C._Bool(true), // code_info — currently unused for kernel but harmless
		C._Bool(true), // inlined_fns — same
		C._Bool(true), // demangle — Rust kernel symbols, etc.
	)
	csym := C.blaze_symbolizer_new_opts(&copts)
	if csym == nil {
		return nil, fmt.Errorf("blaze_symbolizer_new_opts returned NULL")
	}
	return &LocalKernelSymbolizer{csym: csym}, nil
}

// SymbolizeKernel resolves kernel addresses via blazesym's kernel source.
// Returns one Frame per IP. Frames whose name couldn't be resolved get
// Name = "0x<hex>" + Reason = FailureMissingSymbols (matches Noop posture).
func (s *LocalKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}

	// Defend against blazesym calling back during a Close race: take the
	// lock; Close blocks on the same lock before freeing csym.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrClosed
	}

	src := C.make_kernel_src()
	caddr := (*C.uint64_t)(unsafe.Pointer(&ips[0]))
	syms := C.blaze_symbolize_kernel_abs_addrs(s.csym, &src, caddr, C.size_t(len(ips)))
	if syms == nil {
		return nil, fmt.Errorf("blaze_symbolize_kernel_abs_addrs returned NULL")
	}
	defer C.blaze_syms_free(syms)

	out := make([]Frame, 0, int(syms.cnt))
	for i := 0; i < int(syms.cnt); i++ {
		csym := C.sym_at_kernel(syms, C.size_t(i))
		out = append(out, frameFromKernelCSym(csym, ips[i]))
	}
	return out, nil
}

// Close releases the underlying blazesym symbolizer. Idempotent.
func (s *LocalKernelSymbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.csym != nil {
		C.blaze_symbolizer_free(s.csym)
		s.csym = nil
	}
	return nil
}

// frameFromKernelCSym mirrors the user-mode frameFromCSym helper from
// symbolize/debuginfod/dispatcher.go: copies name + offset + code_info
// (file/line/column) when blazesym resolved them, and walks the inlined
// chain so kernel module functions get full source attribution when
// blazesym has DWARF for the loaded modules.
func frameFromKernelCSym(c *C.blaze_sym, addr uint64) Frame {
	f := Frame{Address: addr, Module: "[kernel.kallsyms]"}
	if c.name == nil {
		f.Name = fmt.Sprintf("0x%x", addr)
		f.Reason = FailureMissingSymbols
		return f
	}
	f.Name = C.GoString(c.name)
	f.Offset = uint64(c.offset)
	if c.code_info.file != nil {
		f.File = C.GoString(c.code_info.file)
		f.Line = int(c.code_info.line)
		f.Column = int(c.code_info.column)
	}
	for j := C.size_t(0); j < c.inlined_cnt; j++ {
		in := (*C.blaze_symbolize_inlined_fn)(unsafe.Pointer(uintptr(unsafe.Pointer(c.inlined)) + uintptr(j)*unsafe.Sizeof(*c.inlined)))
		inFrame := Frame{Address: addr, Module: f.Module}
		if in.name != nil {
			inFrame.Name = C.GoString(in.name)
		}
		if in.code_info.file != nil {
			inFrame.File = C.GoString(in.code_info.file)
			inFrame.Line = int(in.code_info.line)
			inFrame.Column = int(in.code_info.column)
		}
		f.Inlined = append(f.Inlined, inFrame)
	}
	return f
}

// The inlined-array indexing pattern is the same one used by the user-mode
// `frameFromCSym` in `symbolize/debuginfod/dispatcher.go`. blazesym lays
// out `c.inlined` as a flexible array; we walk it via pointer arithmetic
// rather than a C indexing helper to match the existing M1 idiom.

// kallsymsReadableInternal mirrors the test helper but lives here so the
// constructor can short-circuit before allocating any cgo state.
func kallsymsReadableInternal() bool {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 1 {
		return false
	}
	n, err := strconv.ParseUint(fields[0], 16, 64)
	if err != nil {
		return false
	}
	return n != 0
}
```

- [ ] **Step 4: Run, confirm pass (or skip if no kptr_restrict access)**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 -v ./symbolize/... -run LocalKernel
```

Expected: PASS (3 tests) on a host with `kptr_restrict=0`. Otherwise SKIP. To enable: `sudo sysctl kernel.kptr_restrict=0`.

- [ ] **Step 5: Commit**

```bash
git add symbolize/local_kernel.go symbolize/local_kernel_test.go
git commit -m "symbolize: add LocalKernelSymbolizer cgo wrap of blaze_symbolize_kernel_abs_addrs"
```

---

## Phase 2 — Wire kernel-stack walk into the three profilers

### Task 3: `profile/profiler.go` — read KernStack, symbolize, merge

**Files:**
- Modify: `profile/profiler.go`
- Modify: `profile/profiler_test.go` (compile-time field check)

- [ ] **Step 1: Add the kernelSymbolizer field + constructor parameter**

In `profile/profiler.go`, the `Profiler` struct currently holds `symbolizer symbolize.Symbolizer`. Add a sibling:

```go
type Profiler struct {
	objs             *perfObjects
	symbolizer       symbolize.Symbolizer
	kernelSymbolizer symbolize.KernelSymbolizer
	resolver         *procmap.Resolver
	perfSet          *perfevent.Set
	tags             []string
	sampleRate       int
	labels           map[string]string
	perfData         *perfdata.Writer
}
```

Update `NewProfiler` to accept a `kernelSym symbolize.KernelSymbolizer` parameter — append it to the existing parameter list (after `sym`). Pass it to the struct literal:

```go
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int,
	labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec,
	sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer,
) (*Profiler, error) {
	// ... existing body unchanged until the return ...
	return &Profiler{
		objs:             objs,
		symbolizer:       sym,
		kernelSymbolizer: kernelSym,
		resolver:         procmap.NewResolver(),
		perfSet:          perfSet,
		tags:             tags,
		sampleRate:       sampleRate,
		labels:           labels,
		perfData:         perfData,
	}, nil
}
```

- [ ] **Step 2: Walk the kernel stack inside `Collect`**

Find the existing user-stack lookup (around `pr.objs.Stackmap.LookupBytes(uint32(key.UserStack))`). Add a parallel kernel-stack lookup before it. The merged shape:

```go
// inside the for loop over BatchLookupAndDelete results:

userStack, err := pr.objs.Stackmap.LookupBytes(uint32(key.UserStack))
if err != nil {
	log.Printf("Failed to lookup user stack: %v", err)
	continue
}
if len(userStack) == 0 {
	continue
}

var kernelIPs []uint64
if key.KernStack >= 0 {
	if kernBytes, err := pr.objs.Stackmap.LookupBytes(uint32(key.KernStack)); err == nil {
		kernelIPs = bpfstack.ExtractIPs(kernBytes)
	}
}

sb := new(stackBuilder)
begin := len(sb.stack)

ips := bpfstack.ExtractIPs(userStack)
if len(ips) > 0 {
	userFrames, err := pr.symbolizer.SymbolizeProcess(samplePid, ips)
	if err != nil {
		log.Printf("Failed to symbolize user: %v", err)
	}
	kernelFrames, err := pr.kernelSymbolizer.SymbolizeKernel(kernelIPs)
	if err != nil {
		log.Printf("Failed to symbolize kernel: %v", err)
	}
	merged := symbolize.MergeKernelFirst(kernelFrames, userFrames)
	// kernel frames need IsKernel=true so the pprof builder routes them
	// through the kernelSentinel mapping rather than the user-mode resolver.
	for _, f := range symbolize.ToProfFramesKernel(kernelFrames) {
		sb.append(f)
	}
	for _, f := range symbolize.ToProfFrames(userFrames) {
		sb.append(f)
	}
	_ = merged // (kept as documentation of the conceptual merge; the builder
	// receives leaf-first kernel frames followed by leaf-first user frames)
}
```

Wait — the `_ = merged` is a smell. Tighten the implementation to avoid the unused variable:

```go
// Inside Collect, the per-sample symbolize block becomes:

sb := new(stackBuilder)
begin := len(sb.stack)

ips := bpfstack.ExtractIPs(userStack)
if len(ips) > 0 || len(kernelIPs) > 0 {
	var userFrames, kernelFrames []symbolize.Frame
	if len(ips) > 0 {
		userFrames, err = pr.symbolizer.SymbolizeProcess(samplePid, ips)
		if err != nil {
			log.Printf("Failed to symbolize user: %v", err)
		}
	}
	if len(kernelIPs) > 0 {
		kernelFrames, err = pr.kernelSymbolizer.SymbolizeKernel(kernelIPs)
		if err != nil {
			log.Printf("Failed to symbolize kernel: %v", err)
		}
	}
	// Order: kernel leaf-side, then user. Kernel frames carry IsKernel=true
	// so the pprof builder routes them through the [kernel] sentinel mapping.
	for _, f := range symbolize.ToProfFramesKernel(kernelFrames) {
		sb.append(f)
	}
	for _, f := range symbolize.ToProfFrames(userFrames) {
		sb.append(f)
	}
}

end := len(sb.stack)
pprof.Reverse(sb.stack[begin:end])
```

- [ ] **Step 3: Update the only caller — `perfagent/agent.go`**

The existing `profile.NewProfiler(...)` call currently passes a `symbolize.Symbolizer` as the last positional argument. Add the kernel symbolizer to the call. Use `symbolize.NoopKernelSymbolizer{}` as a stopgap until Task 7 wires the real one through the Agent:

```go
// in perfagent/agent.go, find profile.NewProfiler(...) and append:
sym, err := symbolize.NewLocalSymbolizer()  // existing — unchanged
if err != nil {
	return fmt.Errorf("create symbolizer: %w", err)
}
profiler, err := profile.NewProfiler(
	hostPID, a.config.SystemWide, cpus, a.config.Tags, a.config.SampleRate,
	labels, a.perfDataWriter, profilerEventSpec,
	sym,
	symbolize.NoopKernelSymbolizer{}, // stopgap; Task 7 replaces with chosen impl
)
```

- [ ] **Step 4: Run tests, confirm green**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./profile/... ./perfagent/...
```

Expected: PASS — behavior is unchanged for existing tests because `NoopKernelSymbolizer` returns no kernel frames when `kernelIPs` is empty (typical for the test workloads which don't trigger kernel-stack capture).

- [ ] **Step 5: Commit**

```bash
git add profile/profiler.go perfagent/agent.go
git commit -m "profile: walk and symbolize kernel stack alongside user stack"
```

---

### Task 4: `offcpu/profiler.go` — same pattern

**Files:**
- Modify: `offcpu/profiler.go`
- Modify: `perfagent/agent.go` (one call site)

The off-CPU profiler has the same structure as `profile/profiler.go`. Apply the same shape:

- [ ] **Step 1: Add the field + constructor parameter to `offcpu/profiler.go`**

Mirror Task 3 Step 1 verbatim — `Profiler` struct gains `kernelSymbolizer symbolize.KernelSymbolizer`; `NewProfiler` gains a `kernelSym symbolize.KernelSymbolizer` parameter; struct literal populates it.

- [ ] **Step 2: Walk the kernel stack inside `Collect`**

Find the existing user-stack block in `Collect`. Insert the kernel-stack lookup + parallel symbolize, identical pattern to Task 3 Step 2. The off-CPU sample key has `KernStack int64` already — confirm by reading `offcpu/offcpu_x86_bpfel.go` line 33.

- [ ] **Step 3: Update the caller in `perfagent/agent.go`**

```go
// in perfagent/agent.go, find offcpu.NewProfiler(...) and append:
profiler, err := offcpu.NewProfiler(
	hostPID, a.config.SystemWide, a.config.Tags, labels,
	sym,
	symbolize.NoopKernelSymbolizer{}, // stopgap; Task 7 replaces
)
```

- [ ] **Step 4: Run tests**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./offcpu/... ./perfagent/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add offcpu/profiler.go perfagent/agent.go
git commit -m "offcpu: walk and symbolize kernel stack alongside user stack"
```

---

### Task 5: `unwind/dwarfagent/` — same pattern, but multi-file

**Files:**
- Modify: `unwind/dwarfagent/symbolize.go` (add kernelSym + kernelIPs args)
- Modify: `unwind/dwarfagent/common.go` (session struct + newSession)
- Modify: `unwind/dwarfagent/agent.go` (NewProfiler / NewProfilerWithMode / NewProfilerWithHooks)
- Modify: `unwind/dwarfagent/offcpu.go` (NewOffCPUProfiler / NewOffCPUProfilerWithHooks)
- Modify: `perfagent/agent.go` (DWARF call sites)
- Modify: `bench/cmd/scenario/main.go` (bench callers)
- Modify: `unwind/dwarfagent/agent_test.go`, `offcpu_test.go`, `lazy_test.go` (test callers)

This is the same shape as the user-mode `Symbolizer` migration that landed in v1.1.0 (commits `3b754842` and follow-ups in PR #19).

- [ ] **Step 1: Update `symbolize.go` to take a kernel symbolizer + kernel IPs**

Replace the body of `symbolize.go` (currently the user-only `symbolizePID`):

```go
package dwarfagent

import (
	"log"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

// symbolizeStack resolves user IPs (per-PID) and kernel IPs (global), and
// returns a leaf-first []pprof.Frame with kernel frames first (so they
// land at the leaf side of the pprof Sample.Location).
func symbolizeStack(
	userSym symbolize.Symbolizer,
	kernelSym symbolize.KernelSymbolizer,
	pid uint32,
	userIPs, kernelIPs []uint64,
) []pprof.Frame {
	if len(userIPs) == 0 && len(kernelIPs) == 0 {
		return nil
	}

	var userFrames, kernelFrames []symbolize.Frame
	if len(userIPs) > 0 {
		fs, err := userSym.SymbolizeProcess(pid, userIPs)
		if err != nil {
			log.Printf("dwarfagent: user symbolize: %v", err)
			fs = unknownFrames(userIPs)
		}
		userFrames = fs
	}
	if len(kernelIPs) > 0 {
		fs, err := kernelSym.SymbolizeKernel(kernelIPs)
		if err != nil {
			log.Printf("dwarfagent: kernel symbolize: %v", err)
			fs = unknownFrames(kernelIPs)
		}
		kernelFrames = fs
	}

	out := symbolize.ToProfFramesKernel(kernelFrames)
	out = append(out, symbolize.ToProfFrames(userFrames)...)
	return out
}

// unknownFrames returns one synthetic [unknown] Frame per IP, used when
// the upstream symbolizer fails. Address is preserved for pprof Locations.
func unknownFrames(ips []uint64) []symbolize.Frame {
	out := make([]symbolize.Frame, len(ips))
	for i, ip := range ips {
		out[i] = symbolize.Frame{Address: ip, Name: "[unknown]"}
	}
	return out
}
```

(`symbolizePID` is removed; callers will be updated in Step 2.)

- [ ] **Step 2: Add `kernelSymbolizer` to the `session` struct + `newSession`**

In `unwind/dwarfagent/common.go`:

- Add field: `kernelSymbolizer symbolize.KernelSymbolizer`
- Add parameter to `newSession(...)`: `sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer`
- Pass through into the struct literal.
- Update the existing `s.symbolizer.Close()` line to also call `s.kernelSymbolizer.Close()` — but per the design spec, the Agent owns these and closes them. Mirror the existing pattern: `session.close` does NOT close the symbolizer (look for an existing comment "Symbolizer is owned by the Agent; do not close it here").

The existing close-skip pattern from PR #19's commit `3b754842` should already say something like:

```go
// Symbolizer is owned by the Agent; do not close it here.
```

Extend the comment: `// Symbolizer and KernelSymbolizer are owned by the Agent.`

- [ ] **Step 3: Add `kernelSym` parameter to all dwarfagent constructors**

`unwind/dwarfagent/agent.go`:

```go
func NewProfilerWithMode(... existing args ...,
	sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer,
) (*Profiler, error) { ... }

func NewProfilerWithHooks(... existing args ...,
	sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer,
) (*Profiler, error) {
	return NewProfilerWithMode(... existing args ..., sym, kernelSym)
}

func NewProfiler(... existing args ...,
	sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer,
) (*Profiler, error) {
	return NewProfilerWithHooks(... existing args ..., sym, kernelSym)
}
```

`unwind/dwarfagent/offcpu.go`:

```go
func NewOffCPUProfilerWithHooks(... existing args ...,
	sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer,
) (*OffCPUProfiler, error) { ... }

func NewOffCPUProfiler(... existing args ...,
	sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer,
) (*OffCPUProfiler, error) {
	return NewOffCPUProfilerWithHooks(... existing args ..., sym, kernelSym)
}
```

- [ ] **Step 4: Update the dwarfagent's per-sample symbolize call site**

Find the existing call to `symbolizePID(...)` in `dwarfagent/agent.go` or `common.go` (whichever does the sample loop). Replace with `symbolizeStack(s.symbolizer, s.kernelSymbolizer, pid, userIPs, kernelIPs)`. The kernel IPs come from a sibling stackmap lookup — same shape as Task 3 Step 2.

- [ ] **Step 5: Update all callers**

```bash
grep -rn "dwarfagent.NewProfiler\|dwarfagent.NewOffCPUProfiler\|dwarfagent.NewProfilerWithMode\|dwarfagent.NewProfilerWithHooks" --include="*.go"
```

Each call site appends a `symbolize.NoopKernelSymbolizer{}` argument as the last arg (stopgap until Task 7). Specifically:

- `perfagent/agent.go` — three call sites (DWARF CPU, DWARF auto, DWARF off-CPU).
- `bench/cmd/scenario/main.go` — two call sites.
- `unwind/dwarfagent/agent_test.go` — `NewProfiler` + `NewProfilerWithHooks` calls.
- `unwind/dwarfagent/offcpu_test.go` — `NewOffCPUProfiler` call.
- `unwind/dwarfagent/lazy_test.go` — `NewProfilerWithMode` call.

Each gets `symbolize.NoopKernelSymbolizer{}` appended. Use a helper in the test files:

```go
// near newTestSymbolizer in agent_test.go:
func newTestKernelSymbolizer(t *testing.T) symbolize.KernelSymbolizer {
	t.Helper()
	return symbolize.NoopKernelSymbolizer{}
}
```

…then call `newTestKernelSymbolizer(t)` everywhere the test passes a symbolizer.

- [ ] **Step 6: Run tests, confirm green**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./unwind/dwarfagent/... ./perfagent/... ./bench/...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add unwind/dwarfagent/ perfagent/agent.go bench/cmd/scenario/main.go
git commit -m "dwarfagent: thread KernelSymbolizer through sessions and constructors"
```

---

## Phase 3 — `perf.data` kernel MMAP

### Task 6: `internal/perfdata.Writer.AddKernelMmap`

**Files:**
- Modify: `internal/perfdata/perfdata.go` (add `AddKernelMmap` method)
- Modify: `internal/perfdata/perfdata_test.go` (or create `kernel_mmap_test.go`)

- [ ] **Step 1: Write the failing test**

Create `internal/perfdata/kernel_mmap_test.go`:

```go
package perfdata

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAddKernelMmap writes a Writer with one kernel MMAP2 record and
// asserts the on-disk shape: pid=-1, tid=0, filename=[kernel.kallsyms]_text.
// We don't validate the address range against the running kernel; the
// helper falls back to the catch-all kernel range when /proc/kallsyms is
// unreadable, so Len is always non-zero in the on-disk output.
func TestAddKernelMmap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	// Use the same EventSpec/MetaInfo args as the existing perfdata tests
	// — a software cpu-clock event at 99 Hz is the conventional default.
	w, err := Open(path, EventSpec{
		Type:         1,    // PERF_TYPE_SOFTWARE
		Config:       0,    // PERF_COUNT_SW_CPU_CLOCK
		SamplePeriod: 99,
		Frequency:    true,
	}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.AddKernelMmap(); err != nil {
		t.Fatalf("AddKernelMmap: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The kernel MMAP2 must include the literal filename "[kernel.kallsyms]_text".
	if !bytes.Contains(body, []byte("[kernel.kallsyms]_text")) {
		t.Fatalf("perf.data missing kernel MMAP2 filename")
	}
	// pid=-1 = 0xffffffff little-endian. We don't pin its byte offset
	// (depends on header), but the bytes must appear in the file.
	if !bytes.Contains(body, []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("perf.data missing pid=-1 marker")
	}
}
```

- [ ] **Step 2: Run, confirm fail**

```bash
GOTOOLCHAIN=auto go test -count=1 ./internal/perfdata/... -run AddKernelMmap
```

Expected: FAIL — `AddKernelMmap` undefined.

- [ ] **Step 3: Implement `AddKernelMmap`**

Append to `internal/perfdata/perfdata.go`:

```go
// AddKernelMmap emits PERF_RECORD_MMAP2 for [kernel.kallsyms]_text so
// `perf report` resolves kernel symbols against /proc/kallsyms (or its
// own kallsyms snapshot). Should be called once at writer init, before
// any sample records. pid=-1 (kernel-or-any), tid=0.
//
// When /proc/kallsyms is unreadable or kptr-restricted, the Addr/Len
// fields are emitted as zero — `perf report` still reads the filename
// and resolves against its own /proc/kallsyms snapshot.
func (w *Writer) AddKernelMmap() error {
	addr, length := readKernelTextRange()
	w.AddMmap2(Mmap2Record{
		Pid:      uint32(0xffffffff), // -1
		Tid:      0,
		Addr:     addr,
		Len:      length,
		Pgoff:    0,
		Prot:     0x5, // PROT_READ | PROT_EXEC
		Flags:    0x2, // MAP_PRIVATE
		Filename: "[kernel.kallsyms]_text",
	})
	return nil
}

// readKernelTextRange returns (start, len) for the kernel _text section,
// or (0, 0) when /proc/kallsyms is unreadable or kptr-restricted.
func readKernelTextRange() (uint64, uint64) {
	const path = "/proc/kallsyms"
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()

	var textStart, etext uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil || addr == 0 {
			continue
		}
		switch fields[2] {
		case "_text":
			textStart = addr
		case "_etext":
			etext = addr
		}
		if textStart != 0 && etext != 0 {
			break
		}
	}
	// On x86_64, kernel addresses span the upper half (0xffffffff80000000..).
	// On arm64 it's the upper half too. Use _text..(end of address space) so
	// module text (which lives outside _text..(_etext)) is also claimed by
	// this MMAP2 — perf report falls back to /proc/kallsyms for symbol
	// resolution, which lists module symbols too.
	if textStart == 0 {
		// kallsyms unreadable — emit a known-kernel range so perf report still
		// routes via [kernel.kallsyms]. 0xffffffff80000000 is the conventional
		// x86_64 kernel base; arm64's range covers the same value.
		return 0xffffffff80000000, 0x80000000
	}
	if etext <= textStart {
		etext = textStart + 0x80000000 // ~2 GiB catch-all above _text
	}
	return textStart, etext - textStart
}
```

(Add imports: `"bufio"`, `"os"`, `"strconv"`, `"strings"`.)

- [ ] **Step 4: Run, confirm pass**

```bash
GOTOOLCHAIN=auto go test -count=1 -v ./internal/perfdata/... -run AddKernelMmap
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfdata/perfdata.go internal/perfdata/kernel_mmap_test.go
git commit -m "perfdata: add Writer.AddKernelMmap for [kernel.kallsyms]_text MMAP2"
```

---

### Task 6b: `SampleRecord.KernelIPs` + PERF_CONTEXT_{KERNEL,USER} markers

**Files:**
- Modify: `internal/perfdata/records.go` (add `KernelIPs []uint64` field to `SampleRecord`; rename `Callchain` → `UserIPs` for clarity)
- Modify: `internal/perfdata/records.go` (`encodeSample` to emit context markers + both chains)
- Modify: `internal/perfdata/records_test.go` (add a kernel+user golden test)
- Modify: every caller of `perfdata.SampleRecord{...}` (sweep with `grep`)

The kernel `perf.data` convention encodes mixed kernel+user callchains as
a single `Callchain` array with magic sentinel values marking the
boundaries:

| Marker | Hex value (LE u64) |
|---|---|
| `PERF_CONTEXT_KERNEL` | `(uint64)-128 = 0xffffffffffffff80` |
| `PERF_CONTEXT_USER`   | `(uint64)-512 = 0xfffffffffffffe00` |

Per-sample shape:

```
[PERF_CONTEXT_KERNEL, kIP_leaf, ..., kIP_root,
 PERF_CONTEXT_USER,   uIP_leaf, ..., uIP_root]
```

- [ ] **Step 1: Add the new `SampleRecord` shape**

`internal/perfdata/records.go`:

```go
const (
    perfContextKernel uint64 = 0xffffffffffffff80 // (uint64)-128
    perfContextUser   uint64 = 0xfffffffffffffe00 // (uint64)-512
)

type SampleRecord struct {
    IP       uint64
    Pid      uint32
    Tid      uint32
    Period   uint64
    UserIPs  []uint64 // formerly Callchain
    KernelIPs []uint64 // NEW
}
```

(The `Callchain` rename is part of the same diff — do a search-and-replace
across the package + callers.)

- [ ] **Step 2: Update `encodeSample` to emit markers**

```go
func encodeSample(w io.Writer, r SampleRecord) {
    // ... existing header / IP / TID / TIME / CPU / PERIOD writes ...

    // Build the merged callchain: kernel first (with marker), then user.
    chain := make([]uint64, 0, 2+len(r.KernelIPs)+len(r.UserIPs))
    if len(r.KernelIPs) > 0 {
        chain = append(chain, perfContextKernel)
        chain = append(chain, r.KernelIPs...)
    }
    if len(r.UserIPs) > 0 {
        chain = append(chain, perfContextUser)
        chain = append(chain, r.UserIPs...)
    }

    writeUint64LE(w, uint64(len(chain)))
    for _, ip := range chain {
        writeUint64LE(w, ip)
    }
}
```

(Reuse the existing `writeUint64LE` helper in the file; preserve all
non-callchain encoding unchanged.)

- [ ] **Step 3: Write the failing test**

`internal/perfdata/records_test.go` — append a new test:

```go
func TestEncodeSample_KernelAndUserChains(t *testing.T) {
    var buf bytes.Buffer
    encodeSample(&buf, SampleRecord{
        IP:        0xffffffff80100000,
        Pid:       42,
        Tid:       42,
        Period:    1,
        KernelIPs: []uint64{0xffffffff80100000, 0xffffffff80200000},
        UserIPs:   []uint64{0x401000, 0x402000},
    })
    body := buf.Bytes()

    // The two kernel + two user IPs + two markers = 6 chain entries
    // (after the existing IP/TID/TIME/CPU/PERIOD prefix). We validate by
    // searching for the kernel and user marker bytes:
    kernelMarker := []byte{0x80, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff} // -128 LE
    userMarker   := []byte{0x00, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff} // -512 LE
    if !bytes.Contains(body, kernelMarker) {
        t.Fatalf("missing PERF_CONTEXT_KERNEL marker; body: % x", body)
    }
    if !bytes.Contains(body, userMarker) {
        t.Fatalf("missing PERF_CONTEXT_USER marker; body: % x", body)
    }
    // Kernel marker must precede user marker.
    if bytes.Index(body, kernelMarker) >= bytes.Index(body, userMarker) {
        t.Fatalf("kernel marker must precede user marker")
    }
}

func TestEncodeSample_UserOnly_NoKernelMarker(t *testing.T) {
    var buf bytes.Buffer
    encodeSample(&buf, SampleRecord{
        Pid: 42, Tid: 42, Period: 1,
        UserIPs: []uint64{0x401000},
    })
    kernelMarker := []byte{0x80, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
    if bytes.Contains(buf.Bytes(), kernelMarker) {
        t.Fatalf("user-only sample should not emit PERF_CONTEXT_KERNEL")
    }
}
```

- [ ] **Step 4: Update every `SampleRecord{...}` literal**

```bash
grep -rn "perfdata.SampleRecord{" --include="*.go"
```

Two existing call sites — both must be updated:

- **`profile/profiler.go:193`** (FP CPU profiler):

  ```go
  pr.perfData.AddSample(perfdata.SampleRecord{
      IP:        userIPs[0],
      Pid:       samplePid,
      Tid:       samplePid,
      Period:    value,
      UserIPs:   userIPs,   // renamed from Callchain
      KernelIPs: kernelIPs, // NEW
  })
  ```

  (Replace the existing `ips` variable with `userIPs` to match the new
  naming; `kernelIPs` was already added in Task 3.)

- **`unwind/dwarfagent/agent.go:137`** (DWARF CPU profiler — `s.perfData.AddSample(...)`):

  Mirror the same shape. The dwarfagent's per-sample loop has its own
  user/kernel IP extraction (added in Task 5); `kernelIPs` is the same
  slice produced there. Apply the rename + add `KernelIPs:` at this
  site too. Without this update, `--unwind dwarf` and the default
  `--unwind auto` continue emitting user-only perf.data callchains.

If a future grep finds additional `perfdata.SampleRecord{...}` sites
(e.g., off-CPU adds one in M2), apply the same shape there.

- [ ] **Step 5: Run tests**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./internal/perfdata/... ./profile/... ./offcpu/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/perfdata/records.go internal/perfdata/records_test.go profile/profiler.go offcpu/profiler.go
git commit -m "perfdata: encode mixed kernel+user callchains with PERF_CONTEXT markers"
```

---

### Task 6c: `--kernel-stacks` CLI flag + `WithKernelStacks()` Option setter

**Files:**
- Modify: `perfagent/options.go` (add `Config.KernelStacks bool` + `WithKernelStacks()`)
- Modify: `main.go` (add `--kernel-stacks` flag + wire it into `buildOptions()`)

- [ ] **Step 1: Add the Config field + Option setter**

`perfagent/options.go`:

```go
// In Config struct:
type Config struct {
    // ... existing ...

    // KernelStacks enables kernel-mode stack capture and symbolization.
    // Default: false. Opt in via --kernel-stacks (CLI) or
    // WithKernelStacks() (library).
    //
    // When set:
    //   - BPF programs enable the kernel-stack capture path (a volatile
    //     bool global flipped at load time; no per-sample cost when off).
    //   - The Agent constructs a LocalKernelSymbolizer; on
    //     ErrKernelSymbolsUnavailable, falls back to NoopKernelSymbolizer
    //     + a one-time warning.
    //   - --perf-data-output emits a kernel MMAP2 record at writer init,
    //     and SampleRecord callchains carry PERF_CONTEXT_{KERNEL,USER}
    //     markers around the merged kernel+user IPs.
    KernelStacks bool
}

// WithKernelStacks enables kernel-mode stack capture + symbolization.
// Default: off.
func WithKernelStacks() Option {
    return func(c *Config) { c.KernelStacks = true }
}
```

- [ ] **Step 2: Add the CLI flag**

`main.go`:

```go
// Alongside the other flag declarations:
flagKernelStacks = flag.Bool("kernel-stacks", false,
    "Enable kernel-mode stack capture and symbolization (default: off).")
```

In `buildOptions()`, append:

```go
if *flagKernelStacks {
    opts = append(opts, perfagent.WithKernelStacks())
}
```

- [ ] **Step 3: Run build + help check**

```bash
make build
./perf-agent -h 2>&1 | grep "kernel-stacks"
```

Expected: clean build, `--kernel-stacks` shown in `--help`.

- [ ] **Step 4: Run tests**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./perfagent/... ./symbolize/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/options.go main.go
git commit -m "perfagent: add --kernel-stacks CLI flag + WithKernelStacks() option"
```

---

## Phase 4 — Agent wiring

### Task 7: `chooseKernelSymbolizer` + thread to all profilers + invoke `AddKernelMmap`

**Files:**
- Modify: `perfagent/agent.go`

This task replaces every stopgap `symbolize.NoopKernelSymbolizer{}` from Tasks 3-5 with the Agent-owned chosen impl, mirroring how `chooseSymbolizer` was added in v1.1.0 (commit `56868b09`).

- [ ] **Step 1: Add the `Agent.kernelSymbolizer` field**

In the `Agent` struct in `perfagent/agent.go`:

```go
type Agent struct {
	// ... existing ...
	symbolizer       symbolize.Symbolizer
	kernelSymbolizer symbolize.KernelSymbolizer // NEW
}
```

- [ ] **Step 2: Add `chooseKernelSymbolizer`**

Append to `perfagent/agent.go` near `chooseSymbolizer`:

```go
// chooseKernelSymbolizer returns LocalKernelSymbolizer when
// cfg.KernelStacks is true and /proc/kallsyms is readable; otherwise
// NoopKernelSymbolizer (and a one-time warning if the user opted in but
// kallsyms is locked down). When cfg.KernelStacks is false, returns
// NoopKernelSymbolizer silently — the user did not opt in.
func chooseKernelSymbolizer(cfg *Config, logger *slog.Logger) symbolize.KernelSymbolizer {
	if !cfg.KernelStacks {
		return symbolize.NoopKernelSymbolizer{}
	}
	s, err := symbolize.NewLocalKernelSymbolizer()
	if err != nil {
		if logger != nil {
			logger.Warn("kernel symbols unavailable; kernel frames will be raw addresses",
				"error", err,
				"hint", "sysctl kernel.kptr_restrict=0 (and ensure perf_event_paranoid <= 2)")
		}
		return symbolize.NoopKernelSymbolizer{}
	}
	return s
}
```

(Update the call site in the Start path to pass `a.config` to the factory.)

- [ ] **Step 3: Construct the kernel symbolizer in `Agent.Start`**

Find the existing `chooseSymbolizer(...)` call. Add a sibling call:

```go
sym, err := chooseSymbolizer(a.config, procmap.NewResolver(), slog.Default())
if err != nil {
	return fmt.Errorf("symbolizer: %w", err)
}
a.symbolizer = sym
a.kernelSymbolizer = chooseKernelSymbolizer(a.config, slog.Default())
```

- [ ] **Step 4: Replace every stopgap `NoopKernelSymbolizer{}` with `a.kernelSymbolizer`**

Search:

```bash
grep -n "NoopKernelSymbolizer{}" perfagent/agent.go
```

Replace each occurrence with `a.kernelSymbolizer`. This affects the five profiler-construction sites added in Tasks 3-5 (FP CPU, FP off-CPU, DWARF CPU, DWARF auto, DWARF off-CPU).

- [ ] **Step 5: Invoke `AddKernelMmap` at writer init (gated on the flag)**

In `Agent.Start`, find the existing `perfdata.Open(a.config.PerfDataOutput, perfdata.EventSpec{...}, perfdata.MetaInfo{...})` call (around `perfagent/agent.go:315`). Immediately after the `Open` returns success, before any profilers are spun up, call:

```go
if a.perfDataWriter != nil && a.config.KernelStacks {
	if err := a.perfDataWriter.AddKernelMmap(); err != nil {
		log.Printf("perfdata: AddKernelMmap: %v", err)
	}
}
```

(Best-effort; don't fail the agent.)

- [ ] **Step 6: Close the kernel symbolizer in `Agent.cleanup`**

In `cleanup()`, after the existing `a.symbolizer.Close()`:

```go
if a.kernelSymbolizer != nil {
	_ = a.kernelSymbolizer.Close()
}
```

- [ ] **Step 7: Run all tests**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./perfagent/... ./profile/... ./offcpu/... ./symbolize/... ./unwind/... ./internal/perfdata/...
```

Expected: PASS — behavior unchanged for users on hosts where `kptr_restrict=2` (NoopKernelSymbolizer used), and kernel frames begin to resolve where readable.

- [ ] **Step 8: Commit**

```bash
git add perfagent/agent.go
git commit -m "perfagent: own KernelSymbolizer; chooseKernelSymbolizer; AddKernelMmap on writer init"
```

---

## Phase 5 — Integration tests

### Task 8: Kernel-stack pprof end-to-end

**Files:**
- Modify: `test/integration_test.go` (add a new test)

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
//go:build integration

func TestKernelStackResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	// Cap-aware skip — same pattern as existing perf-agent integration tests.
	if !hasProfilingCaps(t) {
		t.Skip("requires CAP_BPF / CAP_PERFMON / CAP_SYS_ADMIN")
	}
	// Read kptr_restrict so we know whether to expect resolved or raw kernel frames.
	kptrZero := readKptrRestrictZero(t)

	// Spawn an io_bound workload (existing fixture; reads from /dev/zero in a tight
	// loop, which spends meaningful CPU in the kernel).
	bin := buildWorkload(t, "test/workloads/go", "io_bound")
	cmd := exec.Command(bin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	agent := exec.Command(agentBinary(t),
		"--profile",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", out,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	p := readPProfFile(t, out)
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}

	if kptrZero {
		// Expect at least one resolved kernel symbol (regex on common kernel prefixes).
		matched := false
		for name := range got {
			if regexp.MustCompile(`^(do_sys_|ksys_|__x64_sys_|vfs_|__schedule|read_)`).MatchString(name) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("no kernel symbol matched expected regex; got functions: %v",
				sortedKeys(got))
		}
	} else {
		// kptr_restrict != 0 → kernel frames present as raw addresses.
		matched := false
		for name := range got {
			if strings.HasPrefix(name, "0xffff") {
				matched = true
				break
			}
		}
		if !matched {
			t.Logf("kptr_restrict != 0 and no raw 0xffff... names; this may be normal for io_bound on this kernel")
		}
	}

	// Also assert at least one user function from io_bound appears alongside kernel.
	hasUser := false
	for name := range got {
		if strings.Contains(name, "main.") || strings.Contains(name, "runtime.") {
			hasUser = true
			break
		}
	}
	if !hasUser {
		t.Fatalf("no user-side function in profile; got: %v", sortedKeys(got))
	}
}

// readKptrRestrictZero returns true when /proc/sys/kernel/kptr_restrict == 0.
func readKptrRestrictZero(t *testing.T) bool {
	t.Helper()
	body, err := os.ReadFile("/proc/sys/kernel/kptr_restrict")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) == "0"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

(Add helpers `buildWorkload`, `readPProfFile`, `hasProfilingCaps`, `agentBinary` — they may already exist in `test/integration_test.go` from prior PRs; reuse if so.)

- [ ] **Step 2: Build the agent + run the test**

```bash
cd /home/diego/github/perf-agent/.worktrees/kernel-stacks-m1
make build
PERF_AGENT_BIN="$(pwd)/perf-agent" \
  GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  bash -c 'cd test && go test -tags integration -run TestKernelStackResolution -v -timeout 60s ./...'
```

Expected outcome on a `kptr_restrict=0` setcap'd box: PASS with at least one matched kernel symbol. SKIP on cap-less environments.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: kernel-stack pprof end-to-end"
```

---

### Task 9: Kernel MMAP2 in `--perf-data-output`

**Files:**
- Modify: `test/integration_test.go` (add a second test)

- [ ] **Step 1: Add the test**

Append to `test/integration_test.go`:

```go
//go:build integration

func TestPerfDataKernelMmap2(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if !hasProfilingCaps(t) {
		t.Skip("requires CAP_BPF")
	}

	bin := buildWorkload(t, "test/workloads/go", "io_bound")
	cmd := exec.Command(bin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	pb := filepath.Join(t.TempDir(), "profile.pb.gz")
	pd := filepath.Join(t.TempDir(), "perf.data")
	agent := exec.Command(agentBinary(t),
		"--profile",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", pb,
		"--perf-data-output", pd,
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run: %v", err)
	}

	body, err := os.ReadFile(pd)
	if err != nil {
		t.Fatalf("read perf.data: %v", err)
	}
	if !bytes.Contains(body, []byte("[kernel.kallsyms]_text")) {
		t.Fatalf("perf.data missing [kernel.kallsyms]_text MMAP2 filename")
	}
	// pid=-1 marker (the kernel mmap's pid field).
	if !bytes.Contains(body, []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("perf.data missing pid=-1 marker for kernel mmap")
	}
}
```

- [ ] **Step 2: Run**

```bash
PERF_AGENT_BIN="$(pwd)/perf-agent" \
  GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  bash -c 'cd test && go test -tags integration -run TestPerfDataKernelMmap2 -v -timeout 60s ./...'
```

Expected: PASS on cap'd host; SKIP otherwise.

- [ ] **Step 3: Commit**

```bash
git add test/integration_test.go
git commit -m "test: assert [kernel.kallsyms]_text MMAP2 in --perf-data-output"
```

---

## Phase 6 — Final verification

### Task 10: Verification matrix

- [ ] **Step 1: Default-build unit tests**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test -count=1 ./...
```

Expected: PASS.

- [ ] **Step 2: `go vet`**

```bash
GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go vet ./...
```

Expected: clean.

- [ ] **Step 3: Build the binary**

```bash
make build
```

Expected: clean; `perf-agent` binary present.

- [ ] **Step 4: Confirm zero behavior change for users on `kptr_restrict=2`**

```bash
# As a normal user on a kptr-restricted host, run the agent on yourself
# briefly and confirm it still produces a profile.
sudo sysctl kernel.kptr_restrict=2 2>/dev/null || true
./perf-agent --profile --pid $$ --duration 1s --profile-output /tmp/test.pb.gz 2>&1 | head -5
ls -la /tmp/test.pb.gz
sudo sysctl kernel.kptr_restrict=0 2>/dev/null || true
rm -f /tmp/test.pb.gz
```

Expected: profile produced, log line warning about kernel symbols unavailable.

- [ ] **Step 5: Integration tests (cap-gated)**

```bash
PERF_AGENT_BIN="$(pwd)/perf-agent" \
  GOTOOLCHAIN=auto LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  bash -c 'cd test && go test -tags integration -run "TestKernelStack|TestPerfDataKernelMmap2" -v -timeout 120s ./...'
```

Expected: PASS on a setcap'd box with `kptr_restrict=0`. SKIP otherwise.

- [ ] **Step 6: Commit any final touch-ups**

```bash
git add -p   # review carefully
git commit -m "test: housekeeping for kernel-stacks M1" || true
```

---

## End of M1

After this plan completes:

- BPF programs have a new `kernel_stacks_enabled` volatile global (default `false`); userspace flips it at load time when `--kernel-stacks` is set. bpf2go bytecode regenerated.
- `symbolize.KernelSymbolizer` is the new abstraction; `LocalKernelSymbolizer` (cgo) and `NoopKernelSymbolizer` (fallback) are the two impls.
- All three profilers (`profile`, `offcpu`, `dwarfagent`) walk both stack chains, symbolize each, and merge with kernel frames at the leaf side.
- pprof emission uses the existing `Frame.IsKernel` + `kernelSentinel` machinery — no new pprof builder code.
- `--perf-data-output` emits a catch-all `PERF_RECORD_MMAP2` for `[kernel.kallsyms]_text` at writer init; per-sample callchains carry `PERF_CONTEXT_KERNEL` / `PERF_CONTEXT_USER` markers.
- The agent fail-quiet's on `kptr_restrict=2` — startup is unaffected, kernel frames degrade to raw addresses.
- Behavior is **opt-in**: users get kernel symbols by passing `--kernel-stacks` (CLI) or `perfagent.WithKernelStacks()` (library). Without the flag, perf-agent runs identically to v1.1.0 — no BPF per-sample cost, no kallsyms read, no kernel MMAP2.

**Deferred to M2+** (separate plans):
- Module debuginfod fetch (`.ko.debug` from distro debuginfod).
- `--kernel-symbols={auto,require,disable}` enum flag.
- Inline kernel function expansion.
- Per-syscall classification labels.
