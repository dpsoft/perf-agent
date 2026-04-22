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

**We do keep all current features.** Every mode that works on `main` today (`--profile`, `--offcpu`, `--pmu`, `--pid`, `-a`) continues to work. DWARF is additive; it activates via `--unwind dwarf` or the default `--unwind auto`. The existing `--unwind fp` path stays byte-for-byte identical.

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
                                       │   mappings, counts)
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

Userspace picks which program to load based on `--unwind`:

| `--unwind` value | CPU program loaded        | Off-CPU program loaded      |
|------------------|---------------------------|-----------------------------|
| `fp`             | `perf.bpf.c`              | `offcpu.bpf.c`              |
| `dwarf` / `auto` | `perf_dwarf.bpf.c`        | `offcpu_dwarf.bpf.c`        |

Same-file diffs in `perf.bpf.c` / `offcpu.bpf.c` are off the table — the default path must remain byte-identical.

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
  - Outer key: `u32 table_id` (build-id hashed to 32 bits, dense-packed).
  - Inner value per entry: 24 bytes.

```c
struct cfi_entry {
    u64 pc_start;       // PC in binary's VA (not target VA) — lookup uses relative PC
    u32 pc_end_delta;   // pc_end - pc_start
    u8  cfa_reg;        // enum: RSP=0, RBP=1, ... (small set)
    u8  flags;          // bit 0: simple rule; bit 1: has_rbp_save
    s16 cfa_offset;     // signed 16-bit offset in bytes
    s16 ra_offset;      // return-addr @ [CFA + ra_offset], typically -8
    s16 rbp_offset;     // saved-rbp @ [CFA + rbp_offset], typically -16 or unused
    u8  _pad[4];
};
```

Sorted by `pc_start` so BPF can binary-search.

### `pc_classification` — per build-id FP-safe vs FP-less ranges

- **Type:** `BPF_MAP_TYPE_ARRAY` inside the same outer hash, parallel to `cfi_rules`.
- **Entry:** `{u64 pc_start; u32 pc_end_delta; u8 mode;}` where `mode ∈ {FP_SAFE, FP_LESS, FALLBACK}`.

Classification comes from the same CFI parse — if all FDEs in a range use the identity CFA rule and have frame pointers reliably, the range is FP_SAFE; otherwise FP_LESS; ranges with complex rules get FALLBACK (BPF treats as FP_SAFE with truncate-on-failure).

### `pid_mappings` — PID → list of (vma_range, build_id, base)

- **Type:** `BPF_MAP_TYPE_HASH` key `u32 pid` → `BPF_MAP_TYPE_ARRAY` of mapping entries.
- **Entry:** `{u64 vma_start; u64 vma_end; u64 load_bias; u32 table_id;}`.

Populated and updated on `PERF_RECORD_MMAP2` events from userspace. BPF uses this to: given a sampled PC, find `(table_id, relative_pc = PC - load_bias)` for lookup in `cfi_rules` and `pc_classification`.

### `stack_events` — BPF ringbuf for PC chains

- **Type:** `BPF_MAP_TYPE_RINGBUF`, 256KB default.
- **Record:** `{u32 pid; u32 tid; u64 time_ns; u64 value; u8 mode; u8 n_pcs; u64 pcs[128];}`.
  - `mode` indicates which walker produced the chain (for telemetry).
  - `value` is sample count (CPU) or blocking-ns (off-CPU).

Userspace consumes with `ringbuf.Reader.Read()`.

## Hybrid walking algorithm (pseudocode, runs in BPF)

```
walk_stack(regs, stack_base, max_depth):
  pcs = [regs.IP]                         # leaf first
  pc  = regs.IP
  bp  = regs.BP
  sp  = regs.SP

  for step in 1..max_depth:
    cls = classify(pc)                    # FP_SAFE | FP_LESS | FALLBACK
    if cls == FP_SAFE or cls == FALLBACK:
      # Advance one frame using rbp chain
      if not (stack_base <= bp and bp + 16 <= stack_base + STACK_BYTES):
        break
      saved_bp  = probe_read(bp)          # captured stack or bpf_probe_read_user
      ret_addr  = probe_read(bp + 8)
      pcs.push(ret_addr)
      if saved_bp == 0 or saved_bp <= bp:
        break
      pc = ret_addr
      bp = saved_bp
      sp = bp + 16
    else:                                 # FP_LESS → DWARF
      rule = cfi_lookup(pc)               # binary search in cfi_rules table for pc's binary
      if rule == NULL:
        break                             # no CFI info — stop, don't extrapolate
      cfa  = (rule.cfa_reg == RSP ? sp : bp) + rule.cfa_offset
      ra   = probe_read(cfa + rule.ra_offset)
      pcs.push(ra)
      # Restore regs for next iteration
      sp = cfa
      if rule.flags & HAS_RBP_SAVE:
        bp = probe_read(cfa + rule.rbp_offset)
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

- Linux **5.17+** for `bpf_loop()`, `BPF_MAP_TYPE_RINGBUF`, mature `bpf_probe_read_user()`, CO-RE.
- `CAP_BPF + CAP_PERFMON + CAP_SYS_PTRACE` — same as today.
- `PERF_SAMPLE_REGS_USER + PERF_SAMPLE_STACK_USER` (Linux 3.7+ — non-issue).
- `PERF_RECORD_MMAP2` with `prot/flags` fields (Linux 4.9+ — non-issue).

## Staging

| Stage | Scope                                        | Rough effort |
|-------|----------------------------------------------|--------------|
| S1    | `unwind/ehcompile/` with unit tests          | 3-4 days     |
| S2    | BPF: FP walker in `perf_dwarf.bpf.c` with per-frame classification | 3-4 days     |
| S3    | BPF: DWARF walker with simple-CFA rules      | 3-4 days     |
| S4    | `unwind/ehmaps/` + MMAP2 ingestion           | 2-3 days     |
| S5    | `unwind/dwarfagent/` + profile/ integration  | 2-3 days     |
| S6    | Port same pattern to `offcpu_dwarf.bpf.c`    | 2 days       |
| S7    | System-wide (`-a`) — multi-PID map tables    | 3-4 days     |

**Total: ~3-4 focused weeks.** Each stage is independently mergeable — the branch keeps working through the progression because `--unwind fp` (the default) never regresses.

## Testing

- **Unit tests** for `ehcompile/` — canned ELFs with known CFI, assert table contents.
- **Unit tests** for the hybrid algorithm — synthetic (regs, stack, CFI table) inputs, known expected chain.
- **Integration tests** against real workloads in `test/workloads/`:
  - Go `cpu_bound` — FP-only path, both `fp` and `auto` modes must produce identical chains (Go has FPs).
  - Rust release with `#[inline(never)] cpu_intensive_work` — FP mode truncates at libstd; `auto` mode must unwind to `cpu_intensive_work`.
  - C++ `-O2 -fomit-frame-pointer` synthetic — new workload in `test/workloads/cpp/`.
  - Python and Node under PYTHONPERFSUPPORT / --perf-basic-prof — no regression in perf-map decoder.
- **Overhead budget:** DWARF profiler at 99 Hz `--pid`, per-PID-mode CPU cost ≤ 2× the FP path, measured on a CPU-bound Go workload.

## Open questions (to resolve during implementation, not blocking)

1. **Classification granularity.** Per-function (one entry per FDE) vs per-instruction (one entry per CFI state transition). Per-function is smaller and simpler; per-instruction is more precise for function prologues/epilogues. Start per-function; refine if we see false-negative truncations.
2. **Table eviction policy.** Exact ref-counting works but adds complexity. For the first cut: free all of a PID's tables on PID exit; rely on build-id sharing to keep steady-state memory bounded.
3. **BPF program size.** Close to verifier limits once both walkers are present. Mitigation: factor shared helpers into `static __always_inline` functions in `unwind_common.h` so the inlined copies dedupe cleanly.
4. **`--unwind auto` default flip.** Ship with `auto = fp` default initially, flip to `auto = hybrid` once S5 is validated on real workloads. Keeps anyone who upgrades from regressing unintentionally.

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
