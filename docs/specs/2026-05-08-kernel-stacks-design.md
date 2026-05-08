# Kernel-stack capture and symbolization in perf-agent

**Author:** D. Parra
**Date:** 2026-05-08
**Status:** Draft (post-brainstorm)
**Milestone:** kernel-stacks-M1
**Targeted release:** v1.2.0

## Summary

Make perf-agent's pprof + `--perf-data-output` resolve kernel-mode stack
frames natively. Today the BPF programs already capture the kernel stack
ID alongside the user stack ID, but userspace discards it; this spec wires
the kernel chain through stack lookup, symbolization, pprof emission, and
the kernel `MMAP2` record in `--perf-data-output`. Result: KVM-bound,
syscall-bound, and IRQ-bound workloads symbolize correctly without any
`/proc/kallsyms` post-processing hack.

## Motivation

A KVM-heavy workload spends > 80% of CPU inside `svm_vcpu_run` /
`vmx_vcpu_run` (kernel-mode guest entry/exit). epoll-bound and
io_uring-bound workloads spend significant time in `ep_item_poll`,
`sock_poll`, and friends. Today perf-agent reports those samples as
`<unknown>` because:

1. The BPF programs (`bpf/perf.bpf.c`, `bpf/offcpu.bpf.c`) write a
   `KernStack int64` field into the per-sample key, but `profile/profiler.go`
   and `offcpu/profiler.go` only call `Stackmap.LookupBytes(key.UserStack)`
   — `key.KernStack` is dropped on the floor.
2. The `symbolize.Symbolizer` interface only exposes
   `SymbolizeProcess(pid, ips)` (per-PID user-mode resolution); there is
   no kernel symbolizer wired into the agent.
3. The `internal/perfdata.Writer` (`--perf-data-output`) emits user-side
   `MMAP2` records but no `[kernel.kallsyms]_text` mapping — so
   `perf report` falls back to "Kernel address maps were restricted"
   even when `/proc/kallsyms` is fine on the host.

Operators currently work around this by capturing perf-agent's
`--perf-data-output` and then manually decoding hot kernel addresses via
`awk '$1 <= addr' /proc/kallsyms`. This spec eliminates the workaround.

## Non-goals (M1)

- **Module debuginfo via debuginfod.** `/proc/kallsyms` lists
  function-name symbols for every loaded module already (when
  `kptr_restrict=0`); fetching `.ko.debug` for source `:line` resolution
  is M2. Mirrors the user-mode debuginfod pattern from v1.1.0.
- **`--kernel-symbols` CLI flag.** Auto-detect (fail-quiet on
  `kptr_restrict`) covers M1; an explicit flag is M3 if needed.
- **Inline kernel function expansion.** blazesym's kernel source doesn't
  expose `inline` info today; revisit if upstream adds it.
- **Per-syscall classification labels** (e.g., `syscall:openat`).
- **PMU-mode kernel stacks.** PMU doesn't capture stacks today.
- **Reimplementing kallsyms parsing in Go.** blazesym already does it.

## Constraints from perf-agent

- **Go 1.26+** (matches existing `go.mod`).
- **blazesym pin** unchanged from v1.1.0. The Go binding
  (`github.com/libbpf/blazesym/go`) does **not** expose a kernel source;
  we wrap `blaze_symbolize_kernel_abs_addrs` from `libblazesym_c` directly
  via cgo, mirroring what `symbolize/debuginfod` already does for the
  process dispatcher.
- **No BPF program changes.** The kernel-stack capture is already there.
- **Behavior-preserving for non-affected users.** A profile of a
  kallsyms-readable host gets kernel frames automatically; a host with
  `kptr_restrict=2` still profiles cleanly with raw kernel addresses.
  No new flag gates the behavior.

## Architecture

```
                       ┌─────────────────┐
                       │  perf_event /   │
                       │  sched_switch   │  ← BPF (UNCHANGED)
                       │  BPF programs   │
                       └────────┬────────┘
                                │
                  perfSampleKey { Pid, UserStack, KernStack }
                                │
                                ▼
  ┌────────────────────────────────────────────────────────────┐
  │  profile.Profiler / offcpu.Profiler / dwarfagent.session    │
  │                                                            │
  │   Stackmap.LookupBytes(KernStack)  ──┐                     │
  │   Stackmap.LookupBytes(UserStack)  ──┼──┐                  │
  └──────────────────────────────────────┼──┼──────────────────┘
                                         │  │
                       kernelIPs []uint64│  │userIPs []uint64
                                         ▼  ▼
                  ┌──────────────────────┐  ┌──────────────────────┐
                  │ KernelSymbolizer     │  │ Symbolizer           │
                  │  (NEW)               │  │  (existing)          │
                  │  blazesym Kernel src │  │  Local | Debuginfod  │
                  │  /proc/kallsyms      │  │  Process source      │
                  │  + module sections   │  │                      │
                  └──────────┬───────────┘  └──────────┬───────────┘
                             │                         │
                       kernelFrames                userFrames
                             │                         │
                             └────────────┬────────────┘
                                          │
                                          ▼
                      ┌───────────────────────────────────┐
                      │  symbolize.MergeKernelFirst()     │
                      │    leaf: kernel → user: root      │
                      │  pprof Mapping cache attaches     │
                      │  [kernel.kallsyms] to kernel locs.│
                      └────────┬────────────────┬─────────┘
                               │                │
                               ▼                ▼
                          pprof builder    perfdata.Writer
                                                │
                                                ▼
                                  PERF_RECORD_MMAP2 for
                                  [kernel.kallsyms]_text
                                  emitted at writer init
```

`procmap.Resolver`, `pprof.ProfileBuilder`'s public surface, the BPF
programs, and the existing user-mode `Symbolizer` are unchanged.

## Public API

### `symbolize/` (additions)

```go
package symbolize

// KernelSymbolizer resolves kernel-mode addresses to symbolic frames.
// Kernel-mode resolution has no PID — kernel + module symbols are global.
// Implementations are safe for concurrent use.
type KernelSymbolizer interface {
    SymbolizeKernel(ips []uint64) ([]Frame, error)
    Close() error
}

// LocalKernelSymbolizer wraps blazesym's kernel source: /proc/kallsyms
// for vmlinux + every loaded module's symbols.
type LocalKernelSymbolizer struct { /* unexported: cgo handle */ }

// NewLocalKernelSymbolizer returns a kernel symbolizer or
// ErrKernelSymbolsUnavailable if /proc/kallsyms is unreadable / kptr-restricted.
// Callers are expected to fall back to NoopKernelSymbolizer on that error.
func NewLocalKernelSymbolizer() (*LocalKernelSymbolizer, error)

func (s *LocalKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]Frame, error)
func (s *LocalKernelSymbolizer) Close() error

// ErrKernelSymbolsUnavailable indicates /proc/kallsyms is locked down or
// missing. Callers SHOULD construct a NoopKernelSymbolizer and proceed.
var ErrKernelSymbolsUnavailable = errors.New("symbolize: kernel symbols unavailable (kptr_restrict?)")

// NoopKernelSymbolizer returns a Frame per IP with Name = "0x<hex>" and
// Reason = FailureMissingSymbols. Used when kallsyms is locked down.
type NoopKernelSymbolizer struct{}

func (NoopKernelSymbolizer) SymbolizeKernel(ips []uint64) ([]Frame, error)
func (NoopKernelSymbolizer) Close() error

// MergeKernelFirst returns a leaf-first frame chain by prepending kernel
// frames (already leaf-first per blazesym convention) onto user frames.
// Either slice may be nil.
func MergeKernelFirst(kernel, user []Frame) []Frame
```

### `pprof/` (one helper)

```go
// SyntheticKernelMapping returns a Mapping the builder attaches to
// kernel-side Locations. Filename is "[kernel.kallsyms]"; build-id is
// read from /sys/kernel/notes when available; address range comes from
// /proc/kallsyms _text/_etext. Process-independent — shared across all
// PIDs in a system-wide profile.
func SyntheticKernelMapping() *Mapping
```

The `pprof.ProfileBuilder`'s existing per-build Mapping cache (keyed by
`Filename + BuildID`) gets one new branch: when a `Frame.Module` starts
with `"[kernel."`, attach to the synthetic kernel Mapping.

### `internal/perfdata/` (one method)

```go
// AddKernelMmap emits PERF_RECORD_MMAP2 for [kernel.kallsyms]_text so
// `perf report` resolves kernel symbols against /proc/kallsyms (or its
// own kallsyms snapshot). Should be called once at writer init, before
// any sample records. pid=-1 (kernel-or-any), tid=0.
//
// When /sys/kernel/notes has a GNU build-id note, also queues a
// HEADER_BUILD_ID feature record for the kernel build-id at file finish.
func (w *Writer) AddKernelMmap() error
```

### `perfagent/` (Agent + factory)

```go
// Agent gains a sibling field; no new Option setters in M1.
type Agent struct {
    // ... existing ...
    symbolizer       symbolize.Symbolizer       // user-mode (existing)
    kernelSymbolizer symbolize.KernelSymbolizer // NEW
}

// chooseKernelSymbolizer mirrors chooseSymbolizer: tries the local
// blazesym kernel source; on ErrKernelSymbolsUnavailable returns a
// NoopKernelSymbolizer + logs one warning. Never returns an error.
func chooseKernelSymbolizer(logger *slog.Logger) symbolize.KernelSymbolizer
```

No new CLI flag in M1. No `WithKernel*` setters. The agent constructs
the kernel symbolizer unconditionally; the impl chooses real-vs-noop
based on what kallsyms gives back.

## Stack-walk changes per call site

All three sites get the same diff: read `KernStack` alongside `UserStack`,
look it up in the same `Stackmap`, extract IPs, symbolize, merge.

### `profile/profiler.go`

```go
// Constructor gains a kernelSym parameter (sibling to sym).
// Inside Collect():
userIPs   := bpfstack.ExtractIPs(userStackBytes)
var kernelIPs []uint64
if key.KernStack >= 0 {
    if kernBytes, err := pr.objs.Stackmap.LookupBytes(uint32(key.KernStack)); err == nil {
        kernelIPs = bpfstack.ExtractIPs(kernBytes)
    }
}

userFrames,   _ := pr.symbolizer.SymbolizeProcess(samplePid, userIPs)
kernelFrames, _ := pr.kernelSymbolizer.SymbolizeKernel(kernelIPs)

frames := symbolize.MergeKernelFirst(kernelFrames, userFrames)
for _, f := range symbolize.ToProfFrames(frames) {
    sb.append(f)
}
```

### `offcpu/profiler.go`

Identical shape to `profile/profiler.go`. Same `Stackmap` map; same
extractor; same merge.

### `unwind/dwarfagent/symbolize.go`

`symbolizePID` gains a kernel-symbolizer parameter. Already returns
`[]pprof.Frame`; extends to merge kernel frames first.

```go
func symbolizeStack(userSym symbolize.Symbolizer,
                    kernelSym symbolize.KernelSymbolizer,
                    pid uint32, userIPs, kernelIPs []uint64) []pprof.Frame {
    // ... user side identical to today ...
    // ... kernel side via kernelSym.SymbolizeKernel ...
    return symbolize.ToProfFrames(symbolize.MergeKernelFirst(kernel, user))
}
```

## pprof emission

The synthetic kernel Mapping is created on first kernel-side Frame
encountered by the builder, then deduped across the rest of the profile.
Filename `[kernel.kallsyms]`, build-id read once from `/sys/kernel/notes`.

Frame ordering at the pprof level: kernel frames are leaf-side (deepest
first), then user frames (`main` is root). After `pprof.Reverse` the
on-disk pprof has root → leaf as pprof prefers, with the kernel block as
the leaf-most segment of every kernel-impacted sample. This matches what
`perf script` produces for `[k]` frames and what FlameGraph expects.

Locations attached to the kernel Mapping carry `Address` (the kernel IP
itself) for `pprof -diff_base` stability. Source `:line` is empty in M1
(blazesym kernel source returns symbol name + offset; module-level DWARF
fetch is M2).

## perf.data emission

`perfagent.Agent`, when `--perf-data-output` is set:

1. `perfdata.NewWriter(...)` opens the file (existing).
2. **NEW:** immediately after, call `w.AddKernelMmap()` — emits one
   `PERF_RECORD_MMAP2`:
   ```
   { pid=-1, tid=0,
     start = (kallsyms _text address, or 0 if not readable),
     len   = _etext - _text,
     pgoff = 0,
     prot/flags = canonical kernel,
     filename = "[kernel.kallsyms]_text" }
   ```
3. **NEW (optional):** if `/sys/kernel/notes` exposes a GNU build-id
   note, queue a `HEADER_BUILD_ID` feature record for the kernel image.
   `perf report --debuginfod-urls=...` will then fetch matching
   `vmlinux.debug` for source-line info via the operator's debuginfod
   chain.

Per-sample writes are unchanged. Kernel IPs sit alongside user IPs in
the existing callchain encoding; `perf report` resolves them against the
emitted kernel MMAP2 + the host's `/proc/kallsyms`.

## Error handling

Five boundaries, all degrade-not-fail (matches v1.1.0's debuginfod
posture):

| # | Boundary | Policy |
|---|---|---|
| 1 | `NewLocalKernelSymbolizer()` returns `ErrKernelSymbolsUnavailable` at agent start | Log one warning at `slog.Warn`, fall back to `NoopKernelSymbolizer`. Agent does NOT fail to start. |
| 2 | `LocalKernelSymbolizer.SymbolizeKernel` errors mid-run | Log once + return `[]Frame` of raw-address frames. User-side resolution is unaffected. |
| 3 | `Stackmap.LookupBytes(KernStack)` returns nothing or `KernStack < 0` | Skip kernel branch silently; user-only chain is emitted. |
| 4 | pprof builder dedups `[kernel.kallsyms]` Mapping | Builder cache extended to recognize kernel mapping by Filename prefix. Single Mapping instance per output. |
| 5 | `Writer.AddKernelMmap` fails (kallsyms unreadable, `_text` missing) | Emit `MMAP2` with `start=0, len=0`; log once. `--perf-data-output` user path is unaffected. |

Posture matches the v1.1.0 debuginfod work: "if it works, you get nicer
profiles; if it doesn't, the profile still produces and tells you why
exactly once."

## Testing strategy

Five rings:

### Unit (no root, no BPF)

- `symbolize.MergeKernelFirst` — table-driven on order, empty inputs,
  single-side inputs.
- `symbolize.NoopKernelSymbolizer` — IPs come back as `0x…`-named frames
  with `FailureMissingSymbols`.
- `pprof.SyntheticKernelMapping` — golden tests with a fixture
  `/sys/kernel/notes` and a fixture kallsyms.
- `internal/perfdata` records test extension: golden bytes for the
  kernel MMAP2 record.

### Cgo + `/proc/kallsyms` integration (no root)

- `LocalKernelSymbolizer` against the running kernel: pick a known
  symbol address, feed back through `SymbolizeKernel`, expect the
  matching name. Cap-aware skip when `kptr_restrict != 0`.
- `NewLocalKernelSymbolizer()` returns `ErrKernelSymbolsUnavailable`
  when fed a fake all-zero `/proc/kallsyms` (test-only constructor for
  injection).

### End-to-end pprof (root or setcap'd)

New test in `test/integration_test.go`: spawn `test/workloads/go/io_bound`
(read-loop) for 3s, profile, assert at least one `Function.Name` matches
a kernel-symbol regex (`^(do_sys_|ksys_|__x64_sys_|vfs_)`) AND at least
one user function appears alongside it (proves merge ordering). System-
wide variant: brief `-a` profile, expect `__schedule` or similar. All
gated by the existing cap-aware skip.

### End-to-end perf.data (root or setcap'd)

Extend the existing `--perf-data-output` test: parse the produced
`perf.data`, find at least one `MMAP2` with
`filename == "[kernel.kallsyms]_text"` and non-zero `len`. We do not
shell out to `perf report`; we validate the on-disk shape.

### Failure-mode regression

Construct an Agent with a fake all-zero `/proc/kallsyms` (kptr_restrict=2
simulation). Profile a real PID. Assert: agent starts cleanly; pprof has
kernel frames named `0xffff…`; user frames resolve normally. Confirms
fail-quiet posture across the system.

## Build & dependencies

- **No new Go module deps.**
- **No new system packages.**
- Cgo build env is unchanged from v1.1.0 (Makefile already passes
  `-I ${LIBBLAZESYM_INC}` and links `libblazesym_c`).
- The new `LocalKernelSymbolizer` adds one cgo file in `symbolize/`. Cgo
  preamble is small (a single `blaze_symbolize_kernel_abs_addrs` wrap).

## Phasing

Single milestone. Sub-tasks (each landing in one commit, single PR):

| # | Task | Files | Day-est |
|---|---|---|---|
| 1 | `KernelSymbolizer` interface + `NoopKernelSymbolizer` + `MergeKernelFirst` | `symbolize/kernel.go`, `kernel_test.go` | 0.5 |
| 2 | `LocalKernelSymbolizer` cgo wrap | `symbolize/local_kernel.go`, test | 1.0 |
| 3 | `SyntheticKernelMapping` in pprof + builder dedup | `pprof/kernel.go`, builder edit, test | 0.5 |
| 4 | Stack-walk changes in `profile/`, `offcpu/`, `dwarfagent/` | three files | 0.5 |
| 5 | `Writer.AddKernelMmap` + Agent invocation at writer init | `internal/perfdata/perfdata.go`, `perfagent/agent.go` | 0.5 |
| 6 | Agent owns + threads `KernelSymbolizer` | `perfagent/agent.go`, profiler constructors | 0.5 |
| 7 | Integration tests (rings 3, 4, 5) | `test/integration_test.go` | 0.5 |

Total: ~4 days. Single feature branch `feat/kernel-stacks-m1`, single PR.

## Risks

- **Cgo callback for kernel source: blazesym's API may evolve.** Today
  `blaze_symbolize_kernel_abs_addrs` accepts a `blaze_symbolize_src_kernel`
  struct with paths to kallsyms / vmlinux. We pin the existing blazesym
  commit; future bumps may require small wrap edits.
- **Kernel address range straddles user.** Already mitigated: we keep
  the streams separate (BPF gives us two stack IDs); we don't try to
  classify by address range.
- **Per-binary mapping cache key collisions.** The synthetic kernel
  Mapping uses a stable Filename + BuildID; cache key is unambiguous.
- **`/sys/kernel/notes` parse failures.** Build-id read is best-effort;
  empty BuildID is acceptable on the synthetic Mapping.

## Success criteria

- pprof from a KVM-bound workload shows `svm_vcpu_run` /
  `vmx_vcpu_run` (kvm_amd / kvm_intel modules) instead of `<unknown>`.
- pprof from an epoll-heavy workload shows `ep_item_poll` /
  `sock_poll` resolved.
- `--perf-data-output` produces a `perf.data` that `perf report`
  resolves kernel symbols on **without** the manual `awk /proc/kallsyms`
  workaround.
- Agent on a host with `kptr_restrict=2` still starts cleanly, emits
  raw `0xffff…` kernel frames, and user-side resolution is unaffected.
- All v1.1.0 tests still pass — no regression for non-kernel-stack users.

## Future directions (tracked, not M1)

- **Module debuginfod fetch.** Per-module `.ko.debug` artifact fetch via
  distro debuginfod (Fedora kernel-modules-debuginfo, Ubuntu
  linux-image-{ver}-dbgsym, etc.). Slot into `symbolize/debuginfod` as a
  `KernelSymbolizer` impl; the existing build-id-keyed cache handles it.
  Unlocks source `:line` for module functions.
- **`--kernel-symbols={auto,require,disable}` flag.** Auto-detect is M1;
  the flag is a future operator-grade switch (e.g., for hardened
  deployments that explicitly forbid kernel-symbol resolution).
- **Inline kernel function expansion.** When blazesym's kernel source
  exposes inline info, mirror what `frameFromBlazesymSym` does for user
  code.
- **Per-syscall classification labels.** A `syscall:NAME` label per
  sample, derived from kernel-frame leafs that match the syscall entry
  prefix.

## References

- BPF programs: `bpf/perf.bpf.c:104`, `bpf/offcpu.bpf.c:102`. Both already
  capture kernel stack IDs; v1.1.0 generated bindings expose
  `KernStack int64`.
- Existing user-mode symbolizer: `docs/debuginfod-symbolization.md` and
  the v1.1.0 design spec for the cgo + blazesym pattern this work mirrors.
- blazesym kernel source: `blaze_symbolize_kernel_abs_addrs` in
  `capi/include/blazesym.h` (no Go-binding wrapper today).
- `perf.data` format: kernel `tools/perf/Documentation/perf.data-file-format.txt`.
- elfutils `debuginfod` (for kernel debuginfo fetch in M2):
  https://sourceware.org/elfutils/Debuginfod.html
