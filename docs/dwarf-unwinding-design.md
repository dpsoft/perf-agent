# DWARF-based stack unwinding — design spec (Option A, BPF-side)

**Status:** scheduled — this is the direction `feat/dwarf-unwinding` is taking.
**Supersedes:** the earlier Option B draft (userspace libunwind CGO wrapper), preserved in git history on this branch.
**Goal:** when a sampled PC lives in code compiled without frame pointers (libstd, glibc, stripped C++), perf-agent should still produce a correct, deep stack — without ptrace, without external tools, at eBPF speed.

## Context

Phase 1 and Phase 2a on this branch established that perf-agent can:

- Capture `(regs, stack_bytes, callchain)` via `perf_event_open(REGS_USER | STACK_USER | CALLCHAIN)` — `unwind/perfreader/`.
- Reproduce the kernel's frame-pointer callchain byte-for-byte in pure Go — `unwind/fpwalker/`.

Phase 2b began as a userspace libunwind CGO wrapper (Option B). That path works in principle but has three drawbacks we don't want to carry:

- Per-sample CGO cost + userspace DWARF parse cost competes with the workload.
- Off-CPU profiling couldn't use it (no `PERF_SAMPLE_STACK_USER` path from `sched_switch`).
- Parking complex code in userspace that could live in BPF keeps the sample-time overhead higher than it needs to be.

This spec chooses **Option A**: perform DWARF unwinding inside the BPF program, using CFI tables pre-compiled in userspace. Symbolization stays where it is — blazesym takes PCs and produces `Frame` objects exactly as today.

## Constraints and non-goals

**We explicitly do *not* need to match parca-agent's scope.** We deliberately drop:

- **Full DWARF expression evaluation.** We handle simple `CFA = register + offset` rules only. PCs with complex expression rules (DW_CFA_expression) fall back to FP walking for that sample. Measured ratio on this system's glibc: 2 out of 3,919 FDEs use expressions (0.05%). The long tail is dominated by signal trampolines which are rarely sampled.
- **JIT / dynamic code.** No tracking of Python perf-maps, Node JIT regions, or similar — those already work via the perf-map decoder landed on `feat/pprof-frame-refactor`.
- **Always-on / high-sample-rate system-wide profiling.** We ship `--pid` first (sequencing A), then `-a` as a follow-up.

**We do keep all current features.** Every mode that works on `main` today (`--profile`, `--offcpu`, `--pmu`, `--pid`, `-a`) continues to work. DWARF is additive; it activates via `--unwind dwarf`. The existing `--unwind fp` path stays byte-for-byte identical at the BPF layer; the Go call path gains one new switch arm in the profiler constructor. `--unwind auto`'s default routing is a staging decision covered in the Staging section below.

## Architecture

```
                        ┌──────────────────────────────────────────┐
  perf_event (CPU)      │                                          │
  sched_switch (off-CPU)│   BPF (kernel-side)                      │
          │             │                                          │
          ▼             │   ┌────────────────────────────────┐     │
    ┌──────────┐        │   │  perf_dwarf.bpf.c  /           │     │
    │ capture  │◄───────┤   │  offcpu_dwarf.bpf.c            │     │
    │  regs    │        │   │  (share unwind_common.h)       │     │
    │ + stack  │        │   │                                 │     │
    └──────────┘        │   │  1. classify(IP) → FP|DWARF    │     │
                        │   │  2. if FP: walk rbp chain      │     │
                        │   │     — re-classify every PC     │     │
                        │   │  3. if DWARF: CFI lookup,      │     │
                        │   │     apply, step                │     │
                        │   │  4. emit PC chain via ringbuf  │     │
                        │   └────────────────────────────────┘     │
                        └──────────────────────────────────────────┘
                                       │
                         BPF ringbuf   │ BPF maps (CFI tables,
                                       │   classification, per-PID
                                       │   mappings)
                                       ▼
                        ┌──────────────────────────────────────────┐
                        │   userspace (Go)                         │
                        │                                          │
                        │  ┌─────────────┐   ┌────────────────┐   │
                        │  │ MMAP2       │   │ CFI compiler   │   │
                        │  │ watcher     ├──▶│ .eh_frame →    │   │
                        │  │(perf_event  │   │ flat rule tbl  │   │
                        │  │ mmap_data=1)│   └──────┬─────────┘   │
                        │  └─────────────┘          │ load         │
                        │                           ▼              │
                        │        BPF maps: CFI + classification   │
                        │                           ▲              │
                        │  ┌─────────────┐          │              │
                        │  │ ringbuf     │          │              │
                        │  │ reader      │──▶ PC ──▶ blazesym     │
                        │  │             │  chain                  │
                        │  └─────────────┘                         │
                        │                                          │
                        │       ───▶ existing pprof.Frame pipeline│
                        └──────────────────────────────────────────┘
```

**Boundaries:**

- **BPF** does sampling, FP walking, CFI rule application, hybrid decisions, and emits a flat PC chain.
- **Userspace** parses `.eh_frame`, compiles CFI tables, watches MMAP2 events, orchestrates BPF maps, consumes PC chains, and symbolizes via blazesym (unchanged).

## File layout

### BPF side

```
bpf/
  perf.bpf.c              # unchanged — existing FP-only CPU profiler
  offcpu.bpf.c            # unchanged — existing FP-only off-CPU
  unwind_common.h         # new — shared types, maps, inline helpers
  perf_dwarf.bpf.c        # new — DWARF-capable CPU profiler
  offcpu_dwarf.bpf.c      # new — DWARF-capable off-CPU
```

Each BPF program has one job:

| Program              | Behavior                                      |
|----------------------|-----------------------------------------------|
| `perf.bpf.c`         | FP-only CPU sampling (unchanged from today)   |
| `offcpu.bpf.c`       | FP-only off-CPU (unchanged from today)        |
| `perf_dwarf.bpf.c`   | Hybrid FP+DWARF CPU sampling                  |
| `offcpu_dwarf.bpf.c` | Hybrid FP+DWARF off-CPU                       |

Userspace routes each `--unwind` value to one of these at load time. The `auto` default routing changes across stages:

| `--unwind` value | Stages S1–S5 (initial) | After S5 validation |
|------------------|------------------------|---------------------|
| `fp`             | `perf.bpf.c` / `offcpu.bpf.c`       | same                           |
| `dwarf`          | `perf_dwarf.bpf.c` / `offcpu_dwarf.bpf.c` | same                |
| `auto`           | routes to `fp` programs             | routes to `dwarf` programs     |

This means anyone who upgrades during S1–S5 sees no behavior change until they opt in via `--unwind dwarf`. After the flip, `auto` becomes the sensible default.

Same-file diffs in `perf.bpf.c` / `offcpu.bpf.c` are off the table — the default path must remain byte-identical at the BPF layer.

### Userspace side (Go)

```
unwind/
  perfreader/        # (exists, from Phase 1) — perf_event ring-buffer reader
  fpwalker/          # (exists, from Phase 2a) — pure-Go FP walker (kept as reference / fallback)
  ehcompile/         # new — .eh_frame → flat CFI table; classification emitter
  ehmaps/            # new — BPF-map orchestration: load/evict tables, mmap2 tracking
  dwarfagent/        # new — lifecycle glue: open perf_events, load BPF, consume ringbuf, wire blazesym
```

The existing `profile/`, `offcpu/`, and `pprof/` packages **do not change**. They consume `[]pprof.Frame`, and we keep producing `[]pprof.Frame`. The new code sits below that interface.

## BPF map schemas

All maps are created from userspace at start-up; entries added/removed lazily as processes mmap binaries.

### `cfi_rules` — per build-id CFI tables

- **Type:** `BPF_MAP_TYPE_HASH` of outer-handle → `BPF_MAP_TYPE_ARRAY` of entries.
  - Outer key: `u64 table_id` (FNV-1a of the 20-byte build-id). 64-bit hash to push the birthday-collision threshold well out of practical range.
  - Inner value per entry: 24 bytes.

```c
struct cfi_entry {
    u64 pc_start;       // PC in binary's VA (not target VA) — lookup uses relative PC
    u32 pc_end_delta;   // pc_end - pc_start
    u8  cfa_type;       // enum: SP=1, FP=2 (arch-neutral — on x86_64 these map to RSP/RBP; on arm64 to SP/x29)
    u8  fp_type;        // enum: Undefined=0, OffsetCFA=1, SameValue=2, Register=3
    s16 cfa_offset;     // signed 16-bit offset in bytes
    s16 fp_offset;      // saved FP @ [CFA + fp_offset], valid when fp_type == OffsetCFA
    s16 ra_offset;      // saved return addr @ [CFA + ra_offset]; typically -8 on x86_64, varies on arm64
    u8  _pad[6];
};
```

**Arch-neutral naming.** `FP` is "frame pointer" abstractly — RBP (DWARF reg 6) on x86_64, x29 (DWARF reg 29) on arm64. Similarly `SP` maps to RSP (reg 7) on x86_64, SP (reg 31) on arm64. The CFI compiler's `archInfo` translates DWARF register numbers to these neutral slots at compile time; BPF doesn't need to know the arch.

**RA is not hardcoded.** On x86_64 the return address is conventionally always at `[CFA-8]`, but arm64 uses the LR register (x30) whose save location is set explicitly per FDE. We emit `ra_offset` for every row to handle both.

Sorted by `pc_start` so BPF can binary-search.

### `pc_classification` — per build-id FP-safe vs FP-less ranges

- **Type:** `BPF_MAP_TYPE_ARRAY` inside the same outer hash, parallel to `cfi_rules`.
- **Entry:** `{u64 pc_start; u32 pc_end_delta; u8 mode;}` where `mode ∈ {FP_SAFE, FP_LESS, FALLBACK}`.

Classification comes from the same CFI parse — if all FDEs in a range use the identity CFA rule and have frame pointers reliably, the range is FP_SAFE; otherwise FP_LESS; ranges with complex rules (DW_CFA_expression etc.) get FALLBACK.

At runtime, **FALLBACK behaves exactly like FP_SAFE** — BPF tries the FP walk and accepts whatever it produces (truncating naturally if FP fails). The distinction is telemetry-only: we count FALLBACK hits separately so we can quantify the "long tail" of code the DWARF walker deliberately skipped.

### `pid_mappings` — PID → list of (vma_range, build_id, base)

- **Type:** `BPF_MAP_TYPE_HASH` key `u32 pid` → `BPF_MAP_TYPE_ARRAY` of mapping entries.
- **Entry:** `{u64 vma_start; u64 vma_end; u64 load_bias; u64 table_id;}`.

Populated and updated on `PERF_RECORD_MMAP2` events from userspace. BPF uses this to: given a sampled PC, find `(table_id, relative_pc = PC - load_bias)` for lookup in `cfi_rules` and `pc_classification`.

### `stack_events` — BPF ringbuf for PC chains

- **Type:** `BPF_MAP_TYPE_RINGBUF`, 256KB default.
- **Record — variable length:** fixed 32-byte header `{u32 pid; u32 tid; u64 time_ns; u64 value; u8 mode; u8 n_pcs; u8 walker_flags; u8 _pad;}` followed by `n_pcs × u64` PC entries. Typical stack is <30 frames → ~270 bytes vs 1056 bytes for a fixed-128-slot layout.
  - `mode` indicates which walker produced each PC (FP vs DWARF, optionally per-frame if worth tracking).
  - `value` is sample count (CPU) or blocking-ns (off-CPU).

The existing `counts` map and `stackmap` (STACK_TRACE) used by `perf.bpf.c` are **not** shared with the DWARF programs. Their lifecycles stay independent — the FP-only programs keep aggregating in `counts` as today, the DWARF programs emit per-sample via ringbuf.

Userspace consumes with `ringbuf.Reader.Read()`.

**No `counts` map in the DWARF programs.** Unlike the existing `perf.bpf.c` (which aggregates same-stack samples in a BPF hash map and batch-reads from userspace), the DWARF programs emit every sample's PC chain individually through the ringbuf. Userspace aggregates after symbolization. This costs more ringbuf bandwidth but keeps the BPF program smaller and simpler, and the per-sample bandwidth (few hundred bytes — see variable-length record below) is well within budget at 99 Hz.

## Hybrid walking algorithm (pseudocode, runs in BPF)

```
walk_stack(regs, stack_base, max_depth):
  pcs = [regs.IP]                         # leaf first
  pc  = regs.IP
  fp  = regs.FP                           # rbp on x86_64, x29 on arm64
  sp  = regs.SP

  for step in 1..max_depth:
    cls = classify(pc)                    # FP_SAFE | FP_LESS | FALLBACK
    if cls == FP_SAFE or cls == FALLBACK:
      # Advance one frame using rbp chain
      if not (stack_base <= fp and fp + 16 <= stack_base + STACK_BYTES):
        break
      saved_fp  = probe_read(fp)          # captured stack or bpf_probe_read_user
      ret_addr  = probe_read(fp + 8)      # arch-specific offset — 8 on x86_64 and arm64 AAPCS
      pcs.push(ret_addr)
      if saved_fp == 0 or saved_fp <= fp:
        break
      pc = ret_addr
      fp = saved_fp
      sp = fp + 16
    else:                                 # FP_LESS → DWARF
      rule = cfi_lookup(pc)               # binary search in cfi_rules table for pc's binary
      if rule == NULL:
        break                             # no CFI info — stop, don't extrapolate
      cfa  = (rule.cfa_type == SP ? sp : fp) + rule.cfa_offset
      ra   = probe_read(cfa + rule.ra_offset)
      pcs.push(ra)
      # Restore regs for next iteration
      sp = cfa
      if rule.fp_type == OffsetCFA:
        fp = probe_read(cfa + rule.fp_offset)
      pc = ra

  return pcs
```

Two subtleties:

- **Per-frame reclassification** — we check `classify(pc)` at the top of every iteration, not just the initial PC. This handles "user code calls libstd which calls back into user code" — when FP-safe user code's chain leads into libstd, we notice at the next step and switch to DWARF for the libstd portion.
- **`bpf_loop()` helper** — the unwind loop is bounded via `bpf_loop()` (Linux 5.17+) rather than unrolled, so the verifier has an easier time. Kernel floor lands at 5.17 anyway because we want per-CPU maps with `BPF_MAP_TYPE_RINGBUF`.

## Userspace components

### `unwind/ehcompile/` — .eh_frame → CFI table

Given an open ELF file:

1. Parse the `.eh_frame_hdr` (small, fast — we already know how from the Option B work).
2. Walk `.eh_frame` CIE + FDE entries. For each FDE:
   - Simulate the DWARF CFI program to produce a sequence of `(pc, CFA rule, RA rule, RBP rule)` tuples at each CFI state transition.
   - If the CFA rule is `reg + offset`, emit a `cfi_entry`.
   - If the CFA rule uses `DW_CFA_expression` or `DW_CFA_val_expression`, emit a `FALLBACK` classification entry for that PC range and no `cfi_entry`.
3. Merge adjacent entries with identical rules to minimize table size.
4. Emit both the sorted CFI array and the classification array.

Pure Go, ~800 LOC estimate. Unit-testable in isolation against known ELFs.

### `unwind/ehmaps/` — BPF-map orchestration

- Maintains a `build_id → table_id` map and ref-counts per PID.
- On a new mapping in a tracked PID:
  - If this build-id already has a table loaded, increment refcount.
  - Otherwise, compile via `ehcompile`, allocate a `table_id`, populate the inner arrays.
  - Add to `pid_mappings`.
- On munmap or PID exit: decrement, free tables at zero refs.

### `unwind/dwarfagent/` — lifecycle glue

- Opens the `perf_event_open` FDs for sampling and for MMAP2 tracking (two separate events — samples on one, MMAP2 on a metadata event with `mmap_data=1, sample_period=0`).
- Loads the correct BPF program per `--unwind`.
- Spawns goroutines: one to consume the MMAP2 ring-buffer and feed `ehmaps`, one to consume the `stack_events` ring-buffer and produce `pprof.ProfileSample` values via blazesym symbolization.
- Presents the same `SamplesCollector` interface the existing profiler uses.

## Integration with the existing pipeline

Zero change to `pprof/pprof.go`, `profile/profiler.go`, `offcpu/profiler.go` beyond the profiler constructor, which gets a new branch:

```go
switch cfg.UnwindMode {
case UnwindFP, UnwindDefault:
    return fpProfiler{...}  // existing path, unchanged
case UnwindDWARF, UnwindAuto:
    return dwarfProfiler{...}  // new path, implements the same interface
}
```

Both produce `[]pprof.Frame` samples through the same collector callback. Symbolization, Frame de-duplication, inline-frame expansion (already landed), and pprof emission are unchanged.

## Kernel / feature requirements

- **Linux 6.0+.** All BPF features we use (`bpf_loop()`, `BPF_MAP_TYPE_RINGBUF`, `bpf_probe_read_user()`, CO-RE, `PERF_SAMPLE_REGS_USER`/`STACK_USER`, `PERF_RECORD_MMAP2`) are mature well before 6.0. Picking 6.x as the floor keeps the compatibility matrix simple and matches the kernels on realistic production targets.
- `CAP_BPF + CAP_PERFMON + CAP_SYS_PTRACE` — same as today.

## Staging

Each stage is independently mergeable. `--unwind fp` (default during S1–S5) never regresses, so the branch stays usable throughout.

| Stage | Scope                                                      | Effort  | Exit criteria                                                                                  |
|-------|------------------------------------------------------------|---------|------------------------------------------------------------------------------------------------|
| S1    | `unwind/ehcompile/` with unit tests                        | 3-4d    | Given a test ELF, compiler emits CFI + classification tables matching a hand-verified fixture. Pure Go; no BPF yet. |
| S2    | BPF: FP walker in `perf_dwarf.bpf.c` with per-frame classification lookup | 3-4d    | Loaded program runs on Go `cpu_bound` workload, emits PC chains to ringbuf identical to `fpwalker` output. DWARF path not yet active — classification always returns FP_SAFE. |
| S3 ✅  | BPF: DWARF walker with simple-CFA rules                    | 3-4d    | On Rust workload with `#[inline(never)] cpu_intensive_work`, FP-less frames now resolve. Chain depth strictly ≥ FP path on same workload. **Shipped**: see `docs/superpowers/plans/2026-04-22-s3-bpf-dwarf-walker.md`. |
| S4 ✅  | `unwind/ehmaps/` + MMAP2 ingestion                         | 2-3d    | New `.so` loaded at runtime (via `dlopen`-style test) produces correct unwinds without restart. PID exit cleans up maps. **Shipped**: see `docs/superpowers/plans/2026-04-23-s4-ehmaps-lifecycle.md`. Known limitation: MmapWatcher attaches per-TID (kernel forbids `inherit=1` with a mmap-able ring buffer); works for single-threaded dlopen flows, S5/S7 upgrades to per-CPU watchers for full multi-thread coverage. |
| S5 ✅  | `unwind/dwarfagent/` + `profile/` integration              | 2-3d    | End user runs `perf-agent --pid N --unwind dwarf` and gets a pprof profile. `--unwind auto` still routes to FP programs at this stage. **Shipped**: see `docs/superpowers/plans/2026-04-23-s5-dwarfagent-integration.md`. `TestPerfAgentDwarfUnwind` validates the full binary end-to-end against the rust workload. |
| S6    | `offcpu_dwarf.bpf.c` — off-CPU variant                     | 2-3d    | Entry path differs: tracepoint on `sched_switch` rather than perf_event; regs via `bpf_task_pt_regs(task)`, stack via explicit `bpf_probe_read_user()` from `task->thread.sp`. Walker and classification logic reused from common.h unchanged. End state: `--unwind dwarf --offcpu` produces deep stacks through libpthread/glibc-futex. |
| S7    | System-wide (`-a`) — multi-PID map management              | 3-4d    | `perf-agent -a --unwind dwarf` correctly tracks mmaps across all processes. Build-id sharing keeps memory bounded. |
| S8    | Flip `--unwind auto` default to DWARF-routed programs      | <1d     | CLI default change + release notes. Gated on real-workload validation from S5–S7. |

**Total: ~3-4 focused weeks.**

## Testing

- **Unit tests** for `ehcompile/` — canned ELFs with known CFI, assert table contents byte-for-byte against a fixture.
- **Unit tests for the hybrid algorithm — via a Go mirror.** The walker runs in BPF, which is hard to unit-test directly. Strategy: port the walker to Go (as a sibling of `fpwalker`), unit-test the Go version against synthetic `(regs, stack, CFI table)` inputs, and fuzz-compare BPF vs Go outputs on captured real samples. The Go mirror also serves as executable documentation of the algorithm.
- **Integration tests** against real workloads in `test/workloads/`:
  - Go `cpu_bound` — FP-only path; `fp`, `dwarf`, and (post-S8) `auto` modes must produce identical chains since Go has frame pointers.
  - Rust release with `#[inline(never)] cpu_intensive_work` — FP mode truncates at libstd; `dwarf` mode must unwind to `cpu_intensive_work`.
  - C++ `-O2 -fomit-frame-pointer` synthetic — new workload in `test/workloads/cpp/`.
  - Python and Node under `PYTHONPERFSUPPORT` / `--perf-basic-prof` — no regression in perf-map decoder.
- **Overhead budget:** DWARF profiler at 99 Hz `--pid`, per-PID-mode CPU cost ≤ 2× the FP path, measured on a CPU-bound Go workload.

## Open questions (to resolve during implementation, not blocking)

1. **Classification granularity.** Per-function (one entry per FDE) vs per-instruction (one entry per CFI state transition). Per-function is smaller and simpler; per-instruction is more precise for function prologues/epilogues. Start per-function; refine if we see false-negative truncations.
2. **Table eviction policy.** Exact ref-counting works but adds complexity. For the first cut: free all of a PID's tables on PID exit; rely on build-id sharing to keep steady-state memory bounded.
3. **BPF program size.** Close to verifier limits once both walkers are present. Mitigation: factor shared helpers into `static __always_inline` functions in `unwind_common.h` so the inlined copies dedupe cleanly.

## Success criteria

1. Rust release build with `#[inline(never)] cpu_intensive_work` — hot function visible in the profile.
2. Go, Python, Node regressions: none. Chains in `auto` mode at least as deep as `fp` mode.
3. Off-CPU samples with DWARF unwind through libpthread / glibc-futex paths.
4. Overhead: within the 2× budget.
5. Binary size: no new runtime dependencies; libunwind removed from the build.

## Out of scope

- Option A full parity with parca-agent (full DWARF expressions, VLA support).
- Kernel-stack DWARF (kernel still uses its own callchain via `bpf_get_stackid` kernel-mode path).
- Windows/macOS — not a Linux feature concern.
- JIT code unwinding — covered by the perf-map decoder for Python/Node; other JITs stay out.
