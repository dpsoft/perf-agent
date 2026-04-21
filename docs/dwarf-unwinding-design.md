# DWARF-based stack unwinding — design spec

**Status:** deferred / not scheduled
**Author:** design context captured 2026-04-21
**Related:** branch `feat/pprof-frame-refactor` (inline-frame expansion landed; this is the next step beyond that)

## Problem

perf-agent's eBPF collector uses frame-pointer (FP) walking via `bpf_get_stackid(ctx, &stackmap, BPF_F_USER_STACK)` in `bpf/perf.bpf.c` and `bpf/offcpu.bpf.c`. This walks the `rbp` chain: at each frame, `[rbp]` holds the caller's `rbp`, `[rbp+8]` holds the return address.

The chain breaks whenever any function in the call path was compiled without a `push rbp; mov rbp, rsp` prologue. Everything deeper than the first FP-less frame is invisible.

**Where this bites in practice:**

- Pre-built Rust `libstd` / `liballoc` / `libcore` (shipped via rustup) has FPs elided. Any user code whose hot path sits beneath a libstd call is invisible.
- Much of glibc (distro-dependent) is built without FPs.
- C++ release builds almost always use `-fomit-frame-pointer`.
- Workloads that call through any shared library the user didn't compile themselves.

**What it looks like:** shallow stacks (1–5 frames) on deep call graphs, hot functions orphaned from thread entry points, narrow-tall flamegraphs.

**What inline-frame expansion solves (and doesn't):**

The branch `feat/pprof-frame-refactor` added inline-frame expansion via `blazesym.Sym.Inlined`. This recovers user code that was *inlined* into a visible frame (our Rust test case: `cpu_intensive_work` was inlined into `FnOnce::call_once{{vtable.shim}}`).

It does **not** recover real, non-inlined frames hidden behind FP-less code. A Rust workload with `#[inline(never)] cpu_intensive_work` sitting below a libstd call would still be invisible.

## Approach options

### Option A: DWARF unwinding inside eBPF (parca-agent / pyroscope-ebpf model)

- **Where:** userspace parses every loaded ELF's `.eh_frame` into compact lookup tables, ships those into BPF maps keyed by `(binary, PC range)`. The eBPF program does the unwinding at sample time using the maps.
- **Scope:** large. `/proc/<pid>/maps` watcher, ELF `.eh_frame` parser, table compiler, BPF-side walker implementing a subset of DWARF CFI (PLT stubs, common cases; bail to raw-stack on exotic CFI expressions), map eviction on exec/mmap.
- **Reference:** parca-agent has ~5k lines of Go + eBPF dedicated to this. Not a weekend project.
- **Wins:** fast at sample time (no per-sample userspace cost), scales to system-wide mode at high sample rate.
- **When to pick:** only if perf-agent becomes a product where high-volume always-on system-wide profiling matters.

### Option B: raw stack capture + userspace unwinding

- **Where:** kernel copies the top-of-stack bytes + register set into a ring buffer per sample. Userspace does the DWARF unwinding with a separate library, then feeds the resulting PCs through blazesym for symbolization.
- **How:** swap current `perf_event_open(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_CPU_CLOCK)` sampling for `perf_event_open` with `sample_type = PERF_SAMPLE_CALLCHAIN | PERF_SAMPLE_REGS_USER | PERF_SAMPLE_STACK_USER`. `sample_regs_user` = the target arch's GPR mask the unwinder needs (RIP, RBP, RSP at minimum on x86_64). `sample_stack_user` = ~8KB. Read from perf ring buffer via mmap + `perf_event_poll`.
- **Wins:** correct stacks on everything the unwinder handles (FP-less Rust, stripped C++, most of glibc). No kernel-side DWARF parsing.
- **Costs:**
  - ~8KB per sample through the ring buffer. At 99 Hz × N CPUs that's ~100–800 KB/s. Manageable.
  - Userspace CPU spent unwinding competes with the workload. Must be measured against the current FP path.
  - `PERF_SAMPLE_STACK_USER` is a perf-event feature only — off-CPU via `sched_switch` tracepoint cannot use it. Off-CPU stays on FP + inline expansion.

**Important correction:** earlier drafts of this spec assumed blazesym could unwind from `(regs, stack_bytes)`. **It cannot.** blazesym is a symbolizer, not an unwinder. The full C API surface (`blazesym.h`) offers only `blaze_symbolize_*`, `blaze_normalize_user_addrs`, `blaze_symbolize_kernel_abs_addrs`, `blaze_read_elf_build_id`, and `blaze_inspect_*`. The Rust crate's modules are `symbolize`, `normalize`, `inspect`, `kernel` — there is no `unwind` module and no plans to add one (confirmed via upstream activity through PR #1328). So Option B needs a *separate* unwinder library.

#### Unwinder library choice (decide when picked up)

Three realistic paths, in order of pragmatism:

1. **libunwind via CGO.** The traditional C library. Mature, handles amd64 + arm64, well-documented. Adds a C build dependency, but we already link blazesym's C library, so the build chain is set up for it. Ballpark: 1–2 weeks of integration work. API shape: `unw_init_remote` + a memory-access callback that reads from our captured stack bytes + an address-space (PID-aware) to read `.eh_frame` from the target process's ELFs.

2. **`github.com/go-delve/delve/pkg/dwarf/frame`.** Pure-Go DWARF CFI evaluator. No new C dep, but we own more code — we'd write the walker loop ourselves on top of delve's CFI rule evaluator, plus handle ELF loading and PC-range lookup. Ballpark: 2–3 weeks. Attractive because everything stays in Go; unattractive because delve's DWARF package is internal API and could change.

3. **Shell out to `perf record --call-graph dwarf` + `perf script`.** Zero unwinder code — let perf do it. Subprocess orchestration, output parsing, different deploy requirement (`perf` must be on the host). Not production-grade long-term but useful as a reference / validation harness while building #1 or #2.

Additional option if perf-agent ever gets serious about this: consider upstreaming an `unwind` module to blazesym. The blazesym authors have declined to build it so far (scope), but a well-written Rust unwinder on top of their existing DWARF infra would likely be accepted. Not a short-term play.

### Option C: `--call-graph dwarf` style with kernel help only

Not really a separate option — the kernel doesn't do DWARF unwinding on its own. `perf record --call-graph dwarf` is actually Option B under the hood (kernel captures stack bytes, `perf report` unwinds them via libunwind).

## Recommendation

**Option B with libunwind** is the most pragmatic first implementation. The kernel-side work is unchanged; the userspace-side picks up a proven C library via CGO. Delve-frame becomes attractive only if the CGO build-chain overhead is a real problem, which it isn't today. Option A stays reserved for a potential future where always-on system-wide profiling at high sample rates matters.

## Scope for Option B (when picked up)

### Changes

1. **New CPU sampling path** in `profile/`:
   - Replace `newPerfEvent` (profile/profiler.go:249) with a `perf_event_open` that requests `PERF_SAMPLE_CALLCHAIN | PERF_SAMPLE_REGS_USER | PERF_SAMPLE_STACK_USER`.
   - `sample_regs_user` mask: minimum RIP, RBP, RSP on x86_64. Validate against libunwind's expected register encoding.
   - `sample_stack_user` size: start with 8192. Make configurable.
   - Consume perf ring buffer via mmap + `perf_event_poll`.

2. **Userspace unwinder** in a new `unwind/` package:
   - Wrap libunwind (`libunwind-ptrace` / `libunwind-x86_64`) via CGO.
   - Accept `(pid, regs, stack_bytes)` per sample.
   - Provide libunwind with a custom address-space that reads registers from our captured regs, stack memory from our captured bytes, and `.eh_frame` / process memory from the target PID's maps (via `process_vm_readv` or `/proc/<pid>/mem`).
   - Produce `[]uint64` instruction pointers, then feed into the existing `SymbolizeProcessAbsAddrs` → `blazeSymToFrames` pipeline — no change to the `Frame` type.

3. **Keep the eBPF path** for off-CPU mode — `sched_switch` tracepoint samples don't have the perf-event stack-sample ergonomics. Off-CPU keeps FP walking + inline expansion.

4. **CLI flag `--unwind {fp,dwarf,auto}`** — default **`auto`**:
   - `fp` — kernel callchain (existing path). Cheapest; breaks on FP-less frames.
   - `dwarf` — always DWARF-unwind from captured regs+stack. Most accurate; highest per-sample cost.
   - `auto` **(default)** — hybrid:
     1. Consult pre-scanned `.eh_frame` classification for the innermost PC's binary/range.
     2. If PC is in an FP-safe range, use the kernel's `PERF_SAMPLE_CALLCHAIN` directly.
     3. If PC is in an FP-less range (libstd, glibc, C++ release builds), DWARF-unwind.
     4. If callchain was short for no obvious reason (≤ 2 frames when stack looks deep), DWARF-unwind as a fallback even for nominally FP-safe PCs.
   - The heuristic is per-sample and cheap — a range lookup + length check.

5. **Per-binary `.eh_frame` classification** in the `unwind/` package:
   - At `/proc/<pid>/maps` attach (and on mmap-change events), parse each ELF's `.eh_frame`.
   - For each FDE, inspect the CFA rule. CFA = `rbp+N` → FP-safe. Anything else (SP-relative, register-indirect, DWARF expression) → FP-less.
   - Cache by `(build-id, function range)` — blazesym's `blaze_read_elf_build_id` can keyfield this — so repeated mmaps of the same `.so` don't re-parse.
   - Result: `map[PID]rangeSet[uint64]` that the sample-time decision consults in O(log n).

### Architecture diagram (text)

```
Before:
  perf_event → eBPF (bpf_get_stackid, FP walk) → stackmap → userspace → blazesym → Frame

After (Option B):
  perf_event (SAMPLE_REGS_USER|SAMPLE_STACK_USER) → perf ring buffer
      → userspace libunwind (regs, stack, .eh_frame from PID) → []PC
      → blazesym symbolize → Frame
```

## Success criteria

1. **Rust workload with `#[inline(never)] cpu_intensive_work`**: `cpu_intensive_work` visible in the profile even when it sits below a libstd call.
2. **C++ release build** (any reasonable sample, e.g. a `-O2 -fomit-frame-pointer` benchmark): user functions visible through at least one shared-library frame.
3. **No regression** on Go, Python (PYTHONPERFSUPPORT), Node (--perf-basic-prof) — stacks remain at least as deep as the FP+inline path.
4. **Overhead budget:** DWARF-unwind CPU profiler at 99 Hz, per-PID mode, adds ≤ 2× the CPU cost of the current FP path. Measure with a dedicated benchmark.

## Out of scope

- Kernel-side DWARF (Option A). Revisit only if sample-rate scaling becomes a product requirement.
- Off-CPU DWARF unwinding. Off-CPU stays on FP + inline expansion.
- PMU mode. Already doesn't use stacks.
- Windows/macOS. Not relevant.

## Open questions (decide when picked up)

1. **blazesym binding coverage** — Does `github.com/libbpf/blazesym/go` expose a raw-stack unwind API today? If not, either extend the Go binding upstream or call the C API via CGO. Check `libblazesym_c.a` surface — `blaze_symbolizer_*` vs. the normalize/unwind entry points.
2. **Kernel version floor** — `PERF_SAMPLE_STACK_USER` has been available since Linux 3.7, so no concern there. But confirm the `sample_regs_user` register encoding blazesym expects matches what the kernel supplies on the target arches (amd64 first, arm64 later).
3. **Ring-buffer backpressure** — if userspace unwinding can't keep up, do we drop samples, queue them to disk, or reduce sample rate? Prototype under synthetic load and decide.
4. **Integration with the `Frame` type** — inline expansion already handled by `blazeSymToFrames`. The unwinder returns `[]uint64` PCs; each PC goes through `SymbolizeProcessAbsAddrs` and then `blazeSymToFrames`. No change to `Frame` needed.

## Pre-work before picking this up

- Benchmark the current FP-only path: samples per second per CPU, ring-buffer bytes/sec.
- Write a synthetic workload with deliberate FP-less frames (e.g. C library built with `-fomit-frame-pointer` wrapping user code) — this becomes the regression test.
- Verify blazesym's raw-stack unwind API is mature enough, or file an upstream issue.
