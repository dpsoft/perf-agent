# Kernel-stack capture and symbolization in perf-agent

**Author:** D. Parra
**Date:** 2026-05-08
**Status:** Draft (post-brainstorm)
**Milestone:** kernel-stacks-M1
**Targeted release:** v1.2.0

## Summary

Add opt-in kernel-mode stack capture and symbolization to perf-agent's
pprof + `--perf-data-output` output. Today's BPF programs have a
kernel-stack capture path that's gated off (system-wide hard-disables it;
targeted mode reads a `CollectKernel` config bit that's wired to `0`).
This spec adds an explicit `--kernel-stacks` CLI flag (library:
`perfagent.WithKernelStacks()`) that, when set, enables those gates, walks
the kernel stack chain in userspace, symbolizes it via blazesym's kernel
source over cgo, merges with user frames, and emits both pprof + a
kernel-aware `perf.data` callchain (with `PERF_CONTEXT_KERNEL` /
`PERF_CONTEXT_USER` markers + a catch-all `[kernel.kallsyms]_text` MMAP2).
Result: KVM-bound, syscall-bound, and IRQ-bound workloads symbolize
correctly without any `/proc/kallsyms` post-processing hack. Default
behavior (no flag) is unchanged from v1.1.0.

## Motivation

A KVM-heavy workload spends > 80% of CPU inside `svm_vcpu_run` /
`vmx_vcpu_run` (kernel-mode guest entry/exit). epoll-bound and
io_uring-bound workloads spend significant time in `ep_item_poll`,
`sock_poll`, and friends. Today perf-agent reports those samples as
`<unknown>` because:

1. The BPF programs (`bpf/perf.bpf.c`, `bpf/offcpu.bpf.c`, and the dwarf
   variants) **write `KernStack` only when a `collect_kernel` gate is set**,
   and the gate is currently hard-coded to `false` in system-wide mode +
   read as `0` from `pid_config` in targeted mode. Even when `KernStack` is
   set, `profile/profiler.go` and `offcpu/profiler.go` only call
   `Stackmap.LookupBytes(key.UserStack)` — `key.KernStack` is dropped on
   the floor.
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
- **`--kernel-symbols={auto,require,disable}` flag.** The explicit opt-in
  `--kernel-stacks` flag covers M1; a multi-mode enum flag is a future
  operator-grade switch for hardened deployments.
- **Inline kernel function expansion.** blazesym's kernel source doesn't
  expose `inline` info today; revisit if upstream adds it.
- **Per-syscall classification labels** (e.g., `syscall:openat`).
- **PMU-mode kernel stacks.** PMU doesn't capture stacks today.
- **Reimplementing kallsyms parsing in Go.** blazesym already does it.

## Constraints from perf-agent

- **Go 1.26+** (matches existing `go.mod`).
- **blazesym pin bumped to ≥ commit `29a609f`** (post-v1.1.0 main). The
  Go binding (`github.com/libbpf/blazesym/go`) does **not** expose a
  kernel source; we wrap `blaze_symbolize_kernel_abs_addrs` from
  `libblazesym_c` directly via cgo, mirroring what `symbolize/debuginfod`
  already does for the process dispatcher. The new pin includes upstream
  commits (`f3cf4dc`, `29a609f`, etc.) that add **transparent
  kernel-module DWARF symbolization** — no C ABI changes; we just pass
  `debug_syms = true` (already planned) and blazesym walks
  `/proc/modules` + `/lib/modules/<release>/` automatically. When
  `linux-image-{ver}-dbgsym` (Ubuntu) / `kernel-debuginfo` (Fedora) is
  installed, module functions like `svm_vcpu_run` resolve to function
  name + source `:line`. When not installed, we fall back to
  kallsyms-only (function name, no source line) — same posture as M1's
  user-mode debuginfod path.
- **BPF gate via `kernel_stacks_enabled` volatile global.** Each BPF
  source (`bpf/perf.bpf.c`, `bpf/perf_dwarf.bpf.c`, `bpf/offcpu.bpf.c`,
  `bpf/offcpu_dwarf.bpf.c`) gains a `const volatile bool
  kernel_stacks_enabled = false;` global. Userspace flips it at load
  time (via `spec.Variables["kernel_stacks_enabled"].Set(true)`) when
  `--kernel-stacks` is passed. Three userspace `pid_config` setters
  currently hardcode `CollectKernel: 0` and must flip to `1` (gated by
  the BPF `kernel_stacks_enabled` global so the bit only takes effect
  when the flag is on): `profile/profiler.go:66` (FP CPU),
  `profile/dwarf_export.go:71` (DWARF CPU), and
  `profile/offcpu_dwarf_export.go:62` (DWARF off-CPU). The off-CPU FP
  profiler does not have its own `pid_config` setter — its targeted-mode
  kernel-stack capture is gated entirely by the BPF program's
  `kernel_stacks_enabled` global. `bpf2go` regenerates the embedded
  bytecode + accessor structs — same workflow as any other BPF edit. No
  new BPF maps, no new BPF helpers, no semantic shift.
- **Opt-in via explicit CLI flag.** Kernel-stack capture and symbolization
  are OFF by default. Users opt in with `--kernel-stacks` (CLI) or
  `perfagent.WithKernelStacks()` (library). Matches the v1.1.0 posture
  for `--debuginfod-url` and `--inject-python` — explicit feature flag,
  no auto-detect, no side channels. When the flag is set:
  - BPF kernel-stack capture is enabled (a `volatile bool` global flipped
    at load time, no per-sample cost when disabled).
  - The Agent constructs a `LocalKernelSymbolizer`; on
    `ErrKernelSymbolsUnavailable` it falls back to `NoopKernelSymbolizer`
    + a one-time warning (kernel frames render as raw `0xffff…`).
  - `AddKernelMmap` is invoked at writer init.
  - `SampleRecord.KernelIPs` is populated; the encoded callchain carries
    `PERF_CONTEXT_KERNEL` / `PERF_CONTEXT_USER` markers.

  When the flag is NOT set, none of the above happens — zero behavioral
  delta from v1.1.0 for users who don't opt in.

## Architecture

```
                       ┌─────────────────┐
                       │  perf_event /   │
                       │  sched_switch   │  ← BPF (one-line flip:
                       │  BPF programs   │     enable kernel-stack
                       │                 │     capture)
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
                      │  [kernel] to kernel locs.         │
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

`procmap.Resolver`, `pprof.ProfileBuilder`'s public surface, and the
existing user-mode `Symbolizer` are unchanged. The BPF programs get a
new `kernel_stacks_enabled` volatile global (off by default); userspace
flips it at load time when `--kernel-stacks` is set.

**Module symbols (e.g. `svm_vcpu_run` in kvm_amd):** `/proc/kallsyms` already
lists module symbols at their full kernel-virtual addresses, so blazesym's
kernel source resolves them correctly without any module-discovery work on
our side. With the bumped blazesym pin, when distro kernel-modules-debuginfo
is installed locally, blazesym also resolves source `:line` for module
functions automatically (it walks `/lib/modules/<release>/` internally).
The pprof side: every kernel frame goes through the existing `kernelSentinel`
mapping regardless of whether it came from vmlinux or a module — `Function.Name`
is what consumers care about. `perf.data` ships a catch-all kernel MMAP2 in
M1; per-module MMAP2 records (better DSO attribution) are M2.

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

// ToProfFramesKernel is ToProfFrames + IsKernel=true on every output frame.
// pprof.ProfileBuilder routes IsKernel frames through the existing
// [kernel] sentinel mapping at pprof/pprof.go:288 — no builder code
// changes needed. Used by every call site that converts symbolized
// kernel frames to pprof.Frame.
func ToProfFramesKernel(frames []Frame) []pprof.Frame
```

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
// Agent gains a sibling field.
type Agent struct {
    // ... existing ...
    symbolizer       symbolize.Symbolizer       // user-mode (existing)
    kernelSymbolizer symbolize.KernelSymbolizer // NEW
}

// chooseKernelSymbolizer returns LocalKernelSymbolizer when
// cfg.KernelStacks is true and /proc/kallsyms is readable; otherwise
// NoopKernelSymbolizer (and a one-time warning if the user opted in but
// kallsyms is locked down). When cfg.KernelStacks is false, returns
// NoopKernelSymbolizer silently — the user did not opt in.
func chooseKernelSymbolizer(cfg *Config, logger *slog.Logger) symbolize.KernelSymbolizer
```

## Configuration

### CLI

```
--kernel-stacks            Enable kernel-mode stack capture and
                           symbolization. Default: false.
```

No additional flags — the kernel symbolizer doesn't have a separate cache
or URL. When `--kernel-stacks` is set and `/proc/kallsyms` is locked down
(`kptr_restrict=2`), the agent logs a one-time warning and emits raw
addresses for kernel frames; user-side resolution is unaffected.

Distro kernel debug-info packages (`linux-image-{ver}-dbgsym` on Ubuntu,
`kernel-debuginfo` on Fedora) are picked up automatically by the bumped
blazesym pin — no flag, no path config. When installed, kernel and
module functions resolve to function name + source `:line`.

### Library (perfagent package)

```go
// WithKernelStacks enables kernel-mode stack capture + symbolization.
// Default: off.
func WithKernelStacks() Option {
    return func(c *Config) { c.KernelStacks = true }
}
```

`Config.KernelStacks bool` is a new field. Zero-value (false) preserves
v1.1.0 behavior.

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

// Kernel frames go through ToProfFramesKernel so they carry IsKernel=true;
// the existing pprof builder routes them through kernelSentinel.
for _, f := range symbolize.ToProfFramesKernel(kernelFrames) {
    sb.append(f)
}
for _, f := range symbolize.ToProfFrames(userFrames) {
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
    // Kernel frames carry IsKernel=true via ToProfFramesKernel;
    // user frames via ToProfFrames (IsKernel=false).
    out := symbolize.ToProfFramesKernel(kernelFrames)
    out = append(out, symbolize.ToProfFrames(userFrames)...)
    return out
}
```

## pprof emission

Kernel frames are produced by the new `symbolize.ToProfFramesKernel`
helper, which flips `pprof.Frame.IsKernel = true` on every output. The
existing `pprof.ProfileBuilder` branch at `pprof/pprof.go:288` already
routes `IsKernel` frames through the `kernelSentinel` mapping
(`procmap.Mapping{Path: "[kernel]"}`). **No pprof builder code changes
needed**; we reuse the existing machinery.

Stack ordering at the pprof level: kernel frames are leaf-side (deepest
first), then user frames. After `pprof.Reverse` the on-disk pprof has
root → leaf as pprof prefers. Locations attached to the kernel mapping
carry `Address` (the kernel IP) for `pprof -diff_base` stability.

Source `:line` for kernel and module functions is populated when blazesym's
kernel source has DWARF available (the bumped pin auto-discovers
`/lib/modules/<release>/...` and reads distro kernel-modules-debuginfo when
installed). Without local debug info, the resolver returns function name
only; `:line` is empty.

## perf.data emission

`perfagent.Agent`, when `--perf-data-output` is set:

1. `perfdata.Open(path, EventSpec, MetaInfo)` opens the file (existing —
   see `internal/perfdata/perfdata.go:54`; the agent already constructs
   the writer this way at `perfagent/agent.go:315`).
2. **NEW:** immediately after, call `w.AddKernelMmap()` — emits one
   `PERF_RECORD_MMAP2` covering all kernel addresses:
   ```
   { pid=-1, tid=0,
     start = (kallsyms _text address, or 0xffffffff80000000 if not readable),
     len   = (catch-all extending past _etext to cover all loaded modules),
     pgoff = 0,
     prot/flags = canonical kernel,
     filename = "[kernel.kallsyms]_text" }
   ```
   The catch-all `len` is sized to cover the full kernel address range
   above `_text` (typically `0x7fffffff` on x86_64) so that module-loaded
   addresses are attributed to this MMAP2. `perf report` resolves them
   against `/proc/kallsyms` (or its own snapshot), which lists module
   symbols at their full virtual addresses. **Per-module MMAP2 records
   (one per loaded module from `/proc/modules` + `/sys/module/<name>/sections/.text`)
   are M2** — they give better DSO attribution but the catch-all is
   already correct for symbol resolution.
3. **NEW:** the per-sample writer is updated to encode kernel + user
   callchains separately with the `PERF_CONTEXT_KERNEL` / `PERF_CONTEXT_USER`
   markers (see "perf.data Callchain encoding" above). `SampleRecord` gains
   a `KernelIPs []uint64` field; `encodeSample` writes the marker + kernel
   chain (when present), then the marker + user chain.
4. **NEW (optional):** if `/sys/kernel/notes` exposes a GNU build-id
   note, queue a `HEADER_BUILD_ID` feature record for the kernel image.
   `perf report --debuginfod-urls=...` will then fetch matching
   `vmlinux.debug` for source-line info via the operator's debuginfod
   chain — same machinery as M1's user-mode debuginfod work.

### perf.data Callchain encoding

`PERF_RECORD_SAMPLE`'s `Callchain` array is a single chain that interleaves
kernel and user IPs. The kernel convention uses two magic sentinel values
to mark the boundaries:

| Marker                  | Hex value (LE u64)         |
|-------------------------|----------------------------|
| `PERF_CONTEXT_KERNEL`   | `(uint64)-128 = 0xffff…ff80` |
| `PERF_CONTEXT_USER`     | `(uint64)-512 = 0xffff…fe00` |

`perf report` walks the array, switches DSO context on each marker, and
attributes each subsequent IP to the right side. The shape we emit per
sample:

```
[PERF_CONTEXT_KERNEL, kIP_leaf, kIP_2, ..., kIP_root,
 PERF_CONTEXT_USER,   uIP_leaf, uIP_2, ..., uIP_root]
```

When kernel IPs are absent, we emit only the user portion (no
`PERF_CONTEXT_KERNEL` marker). When user IPs are absent (extremely
unusual), we emit only the kernel portion. The encoding lives in
`internal/perfdata/`: `SampleRecord` gains a `KernelIPs []uint64` field
alongside the existing `Callchain` (renamed `UserIPs []uint64` to remove
ambiguity); `encodeSample` writes the marker + kernel chain when
`KernelIPs` is non-empty, then the marker + user chain.

`internal/perfdata.SampleRecord` is unexported as a public-API surface
(it's part of `internal/`), so renaming is unobserved outside the
project. **Two existing call sites are updated**:

- `profile/profiler.go:193` — FP CPU profiler.
- `unwind/dwarfagent/agent.go:137` — DWARF CPU profiler.

Both are updated together so that `--unwind dwarf` and the default
`--unwind auto` emit kernel+user callchains, not just the FP path.
Off-CPU profilers (FP and DWARF) don't currently write `perf.data`
samples; if that changes in the future, the same shape applies.

## Error handling

Five boundaries, all degrade-not-fail (matches v1.1.0's debuginfod
posture):

| # | Boundary | Policy |
|---|---|---|
| 1 | `NewLocalKernelSymbolizer()` returns `ErrKernelSymbolsUnavailable` at agent start | Log one warning at `slog.Warn`, fall back to `NoopKernelSymbolizer`. Agent does NOT fail to start. |
| 2 | `LocalKernelSymbolizer.SymbolizeKernel` errors mid-run | Log once + return `[]Frame` of raw-address frames. User-side resolution is unaffected. |
| 3 | `Stackmap.LookupBytes(KernStack)` returns nothing or `KernStack < 0` | Skip kernel branch silently; user-only chain is emitted. |
| 4 | pprof builder dedup of kernel Mapping | The existing `kernelSentinel` (`procmap.Mapping{Path: "[kernel]"}`) is a singleton inside `pprof.ProfileBuilder`; all `IsKernel` frames share it. No new code needed. |
| 5 | `Writer.AddKernelMmap` cannot read `/proc/kallsyms` `_text`/`_etext` | Emit a catch-all `MMAP2` covering the conventional kernel address range (`Addr=0xffffffff80000000, Len=0x80000000` on x86_64) so `perf report` still routes kernel addresses through the kernel mapping; resolution falls back to its own `/proc/kallsyms` snapshot. Log once. |

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
| 0 | Bump blazesym pin past `29a609f` (module DWARF) + add `kernel_stacks_enabled` volatile global to all four BPF programs + wire the userspace `spec.Variables["kernel_stacks_enabled"].Set(cfg.KernelStacks)` call in every BPF loader (FP CPU, DWARF CPU, FP off-CPU, DWARF off-CPU) + flip `CollectKernel: 1` in all three `pid_config` setters | `bpf/{perf,offcpu,perf_dwarf,offcpu_dwarf}.bpf.c`, `profile/profiler.go`, `profile/dwarf_export.go`, `offcpu/profiler.go`, `profile/offcpu_dwarf_export.go`, regenerated bpf2go output | 0.5 |
| 1 | `KernelSymbolizer` interface + `NoopKernelSymbolizer` + `MergeKernelFirst` + `ToProfFramesKernel` | `symbolize/kernel.go`, test | 0.5 |
| 2 | `LocalKernelSymbolizer` cgo wrap (uses `debug_syms=true` so module DWARF lights up automatically) | `symbolize/local_kernel.go`, test | 1.0 |
| 3 | Stack-walk changes in `profile/`, `offcpu/`, `dwarfagent/` | three call sites | 1.0 |
| 4 | `Writer.AddKernelMmap` (catch-all kernel address range) | `internal/perfdata/perfdata.go`, test | 0.5 |
| 5 | `SampleRecord.KernelIPs` + `encodeSample` PERF_CONTEXT_{KERNEL,USER} markers | `internal/perfdata/records.go`, test, `profile/profiler.go` + `unwind/dwarfagent/agent.go` callers | 1.0 |
| 6a | `--kernel-stacks` CLI flag + `WithKernelStacks()` Option setter + `Config.KernelStacks bool` + BPF `kernel_stacks_enabled` volatile global plumbing | `main.go`, `perfagent/options.go`, `bpf/{perf,offcpu,perf_dwarf,offcpu_dwarf}.bpf.c`, `profile/profiler.go` (LoadCollectionSpec setter), `offcpu/profiler.go`, regenerated bpf2go | 0.5 |
| 6b | Agent owns + threads `KernelSymbolizer`; invokes `AddKernelMmap` at writer init (both gated on `cfg.KernelStacks`) | `perfagent/agent.go`, profiler constructor signatures | 0.5 |
| 7 | Integration tests: kernel-stack pprof + perf.data callchain | `test/integration_test.go` | 0.5 |

Total: ~6.5 days. Single feature branch `feat/kernel-stacks-m1`, single PR.

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

- **Per-module MMAP2 records in `--perf-data-output`.** Walk `/proc/modules`
  + `/sys/module/<name>/sections/.text` at writer init, emit one MMAP2 per
  loaded module. Improves `perf report` DSO attribution from "everything
  is `[kernel.kallsyms]_text`" to per-module (`[kvm_amd]`, etc.). Symbol
  resolution itself is unaffected — already correct via the M1 catch-all.
- **Module debuginfod fetch.** Per-module `.ko.debug` artifact fetch via
  distro debuginfod (Fedora kernel-modules-debuginfo, Ubuntu
  linux-image-{ver}-dbgsym, etc.) — fetching debuginfo when it's NOT
  locally installed. M1 uses only locally-installed debuginfo. Slot into
  `symbolize/debuginfod` as a `KernelSymbolizer` impl; the existing
  build-id-keyed cache handles it.
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
