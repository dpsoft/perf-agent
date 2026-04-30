# `--unwind auto`: Runtime Cost Refinement — Design Spec

> **Status (2026-04-28):** Option A2 implemented and shipped.
>
> - `--unwind auto` now invokes lazy CFI compile (Option A2) for system-wide mode.
> - `--unwind dwarf` retains eager-compile behavior as an explicit escape hatch.
> - `--pid N` always uses eager mode regardless of `--unwind` value (compile cost is already negligible per bench data).
> - Validated bench results: 76.5% p50 reduction on system-wide-mixed (78–83% on warm-cache runs).
> - See `docs/superpowers/specs/2026-04-27-unwind-auto-lazy-a2-design.md` for the implementation spec, plan, and validation numbers.
>
> The original brainstorm below is preserved for context; Option A1 and Option B were considered and rejected per the data captured in the implementation spec.

## Context

S8 shipped `--unwind auto` as an alias for `--unwind dwarf` — i.e. loads `perf_dwarf.bpf.c`, runs the hybrid FP-first-then-DWARF walker, eagerly compiles CFI tables for every binary visible at startup via `AttachAllProcesses` / `AttachAllMappings`.

At runtime this is what users want: every frame tries FP first (cheap), DWARF kicks in only for `FP_LESS`-classified PCs. Zero DWARF overhead for fully-FP code.

The *startup* is the wart:

- **System-wide `-a`:** `AttachAllProcesses` walks `/proc/*`, runs `ehcompile.Compile` on every distinct binary. On a ~500-process host this is ~40s wall time before the first sample.
- **Per-PID `--pid N`:** `AttachAllMappings` runs `ehcompile.Compile` on the main binary + every shared library. Typical Rust/C++ workload: 50–150 MB of binaries, ~1–3 seconds.
- **Memory:** CFI + classification tables × N binaries. A stripped-down libc contributes ~50 KB; larger binaries (chromium, rustc) contribute ~MB each.

For users whose processes are fully FP-equipped (Go binaries, release-built C++ with `-fno-omit-frame-pointer`, most Rust debug builds), this cost buys nothing — the walker will never consult the CFI tables it worked so hard to build.

## Goals

1. **Fast startup** when the workload is all-FP.
2. **No cliff** when the workload is FP-less — DWARF should kick in without user intervention or config churn.
3. **No regression** for the explicit modes (`--unwind fp` and `--unwind dwarf` keep their current semantics).

## Non-goals

- Dynamic switching at runtime between FP and DWARF programs (would require tearing down the BPF program and re-attaching; the cliff during tear-down is unacceptable).
- Automatic detection across stages of a process's lifetime (e.g., "exec'd a new binary that's FP-less" — MmapWatcher already handles that, but we don't re-examine the unwind strategy).
- Per-binary build-id cache persistent across perf-agent invocations.

---

## Option A — Lazy auto (reactive CFI compile)

**Core idea:** load `perf_dwarf.bpf.c` on startup but *skip* `AttachAllProcesses` / `AttachAllMappings`. Samples in FP-safe code work immediately (walker falls through to FP path since `classify_rel_pc` returns `MODE_FP_SAFE` on missing lookups). Samples in FP-less code initially truncate until the MmapWatcher-driven lifecycle notices the binary and `tracker.Attach` compiles its CFI.

**What's kept:** BPF program stays the same. `dwarfagent.Profiler` / `dwarfagent.OffCPUProfiler` stay the same shape. The only change is a new construction path that skips the eager attach.

**New behavior path (high level):**

```go
func NewProfiler(..., systemWide, eagerCompile bool, ...) (*Profiler, error) {
    // load BPF, create TableStore, create PIDTracker
    if eagerCompile {
        // existing: AttachAllProcesses or AttachAllMappings
    } else {
        // skip — tracker.Run handles future mmaps; initial samples in
        // FP-less code truncate until attach catches up.
    }
    // MmapWatcher + ringbuf reader + symbolizer, unchanged.
}
```

Call sites: `perfagent/agent.go` dispatch passes `eagerCompile = true` for `"dwarf"`, `eagerCompile = false` for `"auto"`. `"fp"` still takes the FP path entirely.

**What breaks in the first few ms:**

- Samples that arrive before a `MmapWatcher` → `PIDTracker.Attach` cycle for a PID will hit `classify_rel_pc` → `MODE_FP_SAFE` (miss case) → FP walk. For FP-safe code this is perfect. For FP-less code, FP walk truncates at the first FP-less frame. Those samples are lost.
- Steady state (after the first mmap event for each binary): identical to S8 eager-compile behavior.

**How "lost" is "lost":**

- Each executable mapping already present at agent start is visible via `PERF_RECORD_MMAP2` ONLY if it was created after the watcher attached. Pre-existing mappings don't re-fire. So Option A needs a *lightweight* initial-state scan that enrolls PIDs into the tracker WITHOUT compiling CFI — just populates `pid_mappings` pointing to a placeholder table_id that the walker will miss on, and deferred-compiles on first sample miss.
- Alternative: do a very cheap `/proc/<pid>/maps` scan that records *paths* only. CFI compile happens on first sample miss for that path — but BPF can't trigger compile; userspace doesn't know which PC missed. Workaround: emit an additional kind of ringbuf record when `cfi_lookup` misses, carrying the rel_pc + pid. Userspace receives, maps back to a path, triggers compile.

The complexity is in the "miss-signal" path. Two sub-options:

### Option A1 — No miss signal, accept truncation

Simplest. MmapWatcher covers only new mappings. Pre-existing binaries in already-running processes never get CFI until the process mmaps a new library or exits-then-starts. Early samples in FP-less code truncate forever (until a new mapping event triggers an attach for that PID).

**Scope:** ~½ day. Just a constructor flag + dispatch.

**Trade-off:** works great for short-lived workloads and freshly-started processes. For long-running processes attached mid-flight (common for production debugging), FP-less code stays invisible until the process execs or reloads something.

### Option A2 — BPF emits a "cfi_miss" notification; userspace compiles on demand

More robust. Add a second ringbuf map `cfi_miss_events`. When `cfi_lookup` misses inside a frame classified `FP_LESS`, push `(pid, rel_pc)` to the ringbuf. Userspace drains this ringbuf in a goroutine; for each miss, reads `/proc/<pid>/maps`, identifies the binary containing rel_pc, and compiles + installs its CFI. Next sample in that binary works.

**Scope:** ~1–1.5 days. New ringbuf + drainer + /proc lookup + tracker.Attach.

**Trade-off:** covers every FP-less code path as it's exercised. Pays ~1 MMAP2-style compile delay on the first miss for each binary. Memory stays bounded since only actually-sampled binaries consume CFI slots.

**Risk:** high-cardinality miss floods the ringbuf if a process has many small FP-less functions. Rate-limit per `(pid, path)` to one miss per second.

---

## Option B — Binary-level detection (prologue / .eh_frame heuristic)

**Core idea:** on startup, open the target binary (and optionally its dependent shared libraries), inspect it, and decide whether FP is trustworthy. If yes, load `perf.bpf.c` (the cheap pure-FP program). If no, load `perf_dwarf.bpf.c` (the hybrid). Result: binaries that "look FP-safe" get the same runtime cost as `--unwind fp`.

**Detection signals** (heuristic composition):

1. **`.eh_frame` absence.** Go binaries don't emit `.eh_frame` at all — they're pure `.gopclntab`. Detection: no `.eh_frame` → if FP is present, use FP path; if `.gopclntab` present, the runtime emits FP prologues, so FP path works fine.
2. **Prologue sampling.** Disassemble N (~5–20) functions randomly picked from the symbol table, check for canonical `push rbp; mov rbp, rsp` on x86-64 or `stp x29, x30, [sp, #-16]!` on arm64. If ≥90% match, trust FP.
3. **`-fomit-frame-pointer` marker.** Some compilers leave tell-tale patterns: functions jump straight to stack allocation without touching rbp/x29. Detecting those pushes the confidence toward FP-less → DWARF path.
4. **Build-system hints.** Rust release binaries default to `-C force-frame-pointers=no` unless overridden. C++ with `-O2 -fomit-frame-pointer` is the hostile case. Could check `DW_AT_producer` (compiler-set DWARF attribute) but that's already DWARF territory — if we need DWARF to detect, we may as well use DWARF.

For `-a` (system-wide): apply detection per-PID. Some PIDs get FP path, others get DWARF. This gets complicated fast — one BPF program can't serve both programs without loading twice. Either:

- Load both `perf.bpf.c` AND `perf_dwarf.bpf.c`, and per-PID route via different perf_event setups.
- Load only `perf_dwarf.bpf.c` — its hybrid walker already handles FP-safe binaries fine — and use detection only to decide whether to `AttachAllMappings` or not.

The latter collapses Option B into "lazy with smarter heuristic" — i.e., a refinement of Option A rather than a distinct path.

**Scope:** ~2–3 days per the initial estimate. ELF/disassembly inspection + heuristic tuning + integration with the profiler selection logic.

**Trade-off:** matches user intent (binary-by-binary), but the detection heuristic is inherently probabilistic — binaries with mixed FP/FP-less code (statically linked C++ calling into Rust) fool the prologue sampler. False positives (detected as FP-safe when some functions aren't) cause silent truncation. False negatives (detected as FP-less when FP is fine) pay DWARF startup cost for no gain.

---

## Comparison

| Dimension                   | S8 MVP (auto=dwarf) | Option A1 (lazy, no signal) | Option A2 (lazy + miss-notify) | Option B (binary detect) |
|-----------------------------|--------------------|-----------------------------|--------------------------------|--------------------------|
| Startup time (all-FP host)  | Slow (~40s/-a)     | Fast                        | Fast                           | Fast                     |
| Startup time (FP-less host) | Slow               | Fast                        | Fast                           | Slow (detected to DWARF) |
| Runtime cost (all-FP)       | Near-zero          | Near-zero                   | Near-zero                      | Zero (uses perf.bpf.c)   |
| Coverage of early samples   | Complete           | Partial (miss ≈ FP walk)    | Complete within 1 mmap        | Binary-correct from start|
| Implementation effort       | 0 (shipped)        | ~½ day                      | ~1–1.5 days                    | ~2–3 days                |
| Edge cases                  | None new           | Long-running target misses  | Ringbuf flood, rate-limit      | Mixed binaries fool detect|

## Recommended next step

**Benchmark first.** Before committing to A or B, measure S8's eager-compile cost on:

1. A system running ~10 services (typical dev laptop) with `-a` — is 40s acceptable for users? Would 10s be?
2. `--pid N` on a large Rust binary (rust-analyzer, cargo) — is 2s acceptable? Would 500ms be?

If S8 is "acceptable" in both, neither A nor B is worth doing. If only `-a` is painful, **Option A1** solves it for most users (pre-existing mmaps are rare in short-duration `-a` captures anyway). If `--pid N` on large binaries is painful too, **Option A2** is the minimal robust solve. Option B's maintainability cost (heuristic drift as compilers change) probably isn't worth it.

## Open questions (record during implementation)

- Should `cfi_miss_events` (Option A2) be a separate ringbuf, or piggyback on `stack_events` with a tag byte? Separate is cleaner but costs another BPF map.
- For `--pid N`, would it be acceptable to eagerly compile ONLY the main binary's CFI (cheap, ~100ms) and lazily compile shared libraries? That's a hybrid between S8 and A1, probably good for most users.
- What's the right rate-limit for per-(pid, path) miss events? 1 per second is a safe default; could be per-binary-path global if userspace is the choke point.
