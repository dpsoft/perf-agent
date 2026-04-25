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

### Option B: raw stack capture + userspace unwinding (recommended next step)

- **Where:** kernel copies the top-of-stack bytes + register set into a ring buffer per sample. Userspace does the DWARF unwinding using blazesym (which already implements it).
- **How:** swap current `perf_event_open(PERF_TYPE_SOFTWARE, PERF_COUNT_SW_CPU_CLOCK)` sampling for `perf_event_open` with `sample_type = PERF_SAMPLE_CALLCHAIN | PERF_SAMPLE_REGS_USER | PERF_SAMPLE_STACK_USER`. `sample_regs_user` = the x86_64 GPR mask blazesym needs. `sample_stack_user` = ~8KB. Read from perf ring buffer via `perf_event_open` mmap + `BPF_RINGBUF_OUTPUT` or userspace `perf_event` consumer.
- **Scope:** medium. A week of focused work:
  - New userspace perf-event reader (replace/augment the eBPF-only collector for CPU mode).
  - Pass `(regs, stack_bytes)` to blazesym's unwinder API.
  - Integrate results into the existing `Frame` pipeline.
  - Off-CPU mode keeps `bpf_get_stackid` + inline expansion, since off-CPU samples from `sched_switch` don't have the same perf-event ergonomics.
- **Wins:** correct stacks on everything blazesym can unwind (FP-less Rust, stripped C++, most of glibc). Bounded implementation. No kernel-side DWARF parsing.
- **Costs:**
  - ~8KB per sample through the ring buffer. At 99 Hz × N CPUs that's ~100–800 KB/s. Manageable.
  - Userspace CPU spent unwinding competes with the workload. Measure against the existing FP path.
  - Doesn't work in `BPF_F_USER_STACK` style for arbitrary eBPF hook points — only the perf-event sample path gets `PERF_SAMPLE_STACK_USER`. So this improves CPU profiling but not, say, off-CPU via `sched_switch` tracepoint (that path keeps FP walking).

### Option C: `--call-graph dwarf` style with kernel help only

Not really a separate option — the kernel doesn't do DWARF unwinding on its own. `perf record --call-graph dwarf` is actually Option B under the hood (kernel captures stack bytes, `perf report` unwinds them).

## Recommendation

**Option B.** Medium scope, uses infrastructure (blazesym) we already depend on, solves the concrete limitation for CPU profiling. If a future use case demands always-on system-wide at scale, Option A becomes worth revisiting.

## Scope for Option B (when picked up)

### Changes

1. **New CPU sampling path** in `profile/`:
   - Replace `newPerfEvent` (profile/profiler.go:249) with a `perf_event_open` that requests `PERF_SAMPLE_CALLCHAIN | PERF_SAMPLE_REGS_USER | PERF_SAMPLE_STACK_USER`.
   - `sample_regs_user` mask: the x86_64 GPRs blazesym needs (RIP, RBP, RSP, plus general-purpose for register restoration during unwinding — check blazesym's expected format).
   - `sample_stack_user` size: start with 8192. Make configurable.
   - Consume perf ring buffer via mmap + `perf_event_poll`.

2. **Userspace unwinder** in a new `unwind/` package:
   - Accept `(pid, regs, stack_bytes)` per sample.
   - Call blazesym's unwinding API. Check current blazesym Go binding — may need extension if raw-stack unwind isn't exposed yet (as of this writing the Go binding covers `SymbolizeProcessAbsAddrs` but has incomplete coverage of normalize/unwind APIs).
   - Produce `[]uint64` instruction pointers, then feed into the existing `symbolize` path.

3. **Keep the eBPF path** for off-CPU mode — `sched_switch` tracepoint samples don't have the perf-event stack-sample ergonomics. Off-CPU keeps FP walking + inline expansion.

4. **Feature flag / fallback**:
   - CLI option `--unwind {fp,dwarf}` defaulting to `fp` during rollout, then flipping after validation.
   - If DWARF unwinder fails on a sample (exotic CFI, missing debug info), fall back to whatever the kernel's `PERF_SAMPLE_CALLCHAIN` produced for that sample.

### Architecture diagram (text)

```
Before:
  perf_event → eBPF (bpf_get_stackid, FP walk) → stackmap → userspace → blazesym → Frame

After (Option B):
  perf_event (SAMPLE_REGS_USER|SAMPLE_STACK_USER) → perf ring buffer
      → userspace unwinder (regs, stack, blazesym DWARF) → []PC
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
