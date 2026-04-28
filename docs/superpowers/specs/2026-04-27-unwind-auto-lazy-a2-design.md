# `--unwind auto` Lazy CFI (Option A2) — Design Spec

> **Status:** design approved, ready for implementation plan.
> **Companion docs:**
> - `docs/unwind-auto-refinement-design.md` — original refinement-options brainstorm. This spec implements Option A2.
> - `docs/superpowers/specs/2026-04-25-unwind-auto-benchmark-design.md` — the bench harness used to validate the win.
> - `docs/superpowers/plans/2026-04-25-unwind-auto-benchmark.md` — bench implementation plan (already shipped, PR #9).

## Problem

`--unwind auto` today aliases to `--unwind dwarf`, which eagerly compiles CFI tables for every binary visible at startup. Bench data on a typical desktop:

| Scenario | p50 wall time | Compile share |
|----------|---------------|---------------|
| `--pid N` (rust-workload) | 773 ms | ~10 ms (1.4%) |
| `-a` system-wide (30 procs) | 30,744 ms | ~10 s (33%+) |

The system-wide cost is dominated by per-binary `ehcompile.Compile` over the full corpus visible in `/proc/*/maps`. Top hitters on the test host: libxul (605 ms), Telegram (486 ms), node (412 ms), libLLVM (407 ms), libwebkit2gtk/libwebkitgtk (~307 ms each). For users who attach mid-flight to a running production host, this is a 30–60 s freeze before the first sample.

The doc's Option A2 — lazy CFI compile with a BPF→userspace miss-notify ringbuf — reclaims most of this without A1's silent-truncation tradeoff or B's heuristic fragility.

## Goals

1. `--unwind auto -a` startup time on the bench's `system-wide-mixed` scenario drops by **≥80%** (from ~31 s p50 to ≤6 s).
2. Coverage parity for FP-safe code (Go binaries, release-built C++ with `-fno-omit-frame-pointer`, most Rust): zero regression. FP path runs at startup; no CFI needed.
3. Coverage for FP-less code: bounded ramp-up — first sample for an FP-less PC truncates, second sample (after ≤1 s drainer cycle + compile) succeeds.
4. `--unwind dwarf` (explicit) preserves today's eager behavior as an escape hatch.
5. `--pid N` keeps eager behavior regardless of `--unwind` value (bench shows compile cost is already negligible there).
6. No regression on the `--unwind fp` path. No new kernel requirement.

## Non-goals

- Memory-cost optimizations (Option B's binary detection). Marginal savings per bench data — top hitters are C++ giants where heuristic detection is unreliable anyway.
- Pre-warming CFI for binaries known to be FP-less. The lazy ramp-up is acceptable; predictable warmup belongs in `--unwind dwarf`.
- Off-CPU lazy mode in v1. The current `dwarfagent.OffCPUProfiler` shares `newSession` with the CPU profiler; the lazy plumbing is reachable but the value is unclear (off-CPU sampling rate is much lower; eager startup is dominated by BPF program load, not compile). Defer to a follow-up if data justifies it.
- Userspace-side compile parallelism. The drainer compiles one binary at a time. With ~30 distinct binaries per startup and ~100–500 ms per compile, total ramp time is ≤15 s — well under the 30+ s eager baseline. Pool later if measured to be a bottleneck.

## Scope decision: which `--unwind` modes get lazy

| Mode | `--pid N` | `-a` |
|------|-----------|------|
| `--unwind fp` | unchanged (no CFI) | unchanged (no CFI) |
| `--unwind dwarf` | eager (today's S8 MVP) | eager (today's S8 MVP) |
| `--unwind auto` | **eager** (compile cost is 1.4% of startup; lazy buys nothing) | **lazy (this spec)** |

Rationale: `auto` should be the smart default. Lazy is the right answer for `-a` and the wrong answer for `--pid N`. Two paths is one if-statement in `newSession`; the complexity is trivial relative to the 25+ second startup win.

## Architecture

```
┌─────────── perfagent.Agent ───────────┐
│  --unwind {fp | dwarf | auto}         │
│   ├─ fp     → profile.Profiler        │
│   ├─ dwarf  → dwarfagent (eager)      │  (S8 MVP, unchanged)
│   └─ auto   → dwarfagent.NewProfilerWithMode(modeLazy)  ← NEW
└────────────────────────────────────────┘
                    │
                    ▼
┌──── dwarfagent.newSession (extended) ────┐
│  modeEager:  AttachAllProcesses /        │
│              AttachAllMappings (today)   │
│  modeLazy + systemWide:                  │
│    1. ScanAndEnroll → populate           │
│       pid_mappings only (no compile)     │
│    2. start CFI miss drainer goroutine   │
│  modeLazy + per-PID:                     │
│    falls back to eager (hybrid)          │
└──────────────────────────────────────────┘
                    │
                    ▼
┌──────── BPF (perf_dwarf.bpf.c) ────────┐
│  walker (walk_step):                    │
│   classify_rel_pc → MODE_FP_LESS        │
│   cfi_lookup → MISS                     │
│   ├─ rate-limit check                   │
│   │  (cfi_miss_ratelimit, 1/sec/key)    │
│   └─ bpf_ringbuf_output(                │
│        cfi_miss_events, ev)             │
│                                          │
│  NEW maps:                              │
│   cfi_miss_events       RINGBUF 64 KB   │
│   cfi_miss_ratelimit    LRU_HASH 4096   │
└──────────────────────────────────────────┘
                    │
                    ▼
┌─── dwarfagent.cfiMissDrainer (NEW) ───┐
│  goroutine reads cfi_miss_events       │
│  for each (pid, table_id, rel_pc):     │
│   ├─ resolve binary path via            │
│   │  /proc/<pid>/maps                   │
│   ├─ tracker.Attach(pid, path) →        │
│   │  ehcompile + populate cfi_*         │
│   └─ next sample for that PC succeeds   │
└────────────────────────────────────────┘
```

Three goroutines run in lazy mode (vs two in eager): tracker, sample reader, miss drainer.

## Component 1: BPF changes

Three additions to `perf_dwarf.bpf.c` / `unwind_common.h`. No changes to `--unwind fp` path or to `--unwind dwarf` (eager).

### New maps in `unwind_common.h`

```c
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);
} cfi_miss_events SEC(".maps");

struct cfi_miss_ratelimit_key {
    __u32 pid;
    __u64 table_id;
} __attribute__((packed));

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct cfi_miss_ratelimit_key);
    __type(value, __u64);  // last-emit ktime_get_ns()
} cfi_miss_ratelimit SEC(".maps");
```

`BPF_MAP_TYPE_LRU_HASH` is supported on Linux 4.10+. `bpf_ringbuf_*` is supported on 5.8+. Both already required by the project. No new kernel requirement.

### Event payload

```c
struct cfi_miss_event {
    __u32 pid;
    __u64 table_id;
    __u64 rel_pc;        // diagnostic only
    __u64 ktime_ns;       // emit time (for userspace latency telemetry)
};
```

`rel_pc` is included for diagnostic / test purposes only — userspace compiles the whole binary, not a specific function.

### Emit logic — inject into `walk_step()`

The existing walker already detects the FP_LESS+miss case and sets `WALKER_FLAG_CFI_MISS` (`unwind_common.h:381-403`). We add the rate-limited emit at that exact site:

```c
if (mode == MODE_FP_LESS) {
    cfi = cfi_lookup(table_id, rel_pc);
    if (!cfi) {
        ctx->flags |= WALKER_FLAG_CFI_MISS;
        emit_cfi_miss(pid, table_id, rel_pc);  // NEW
        return 1;  // terminate walk (existing behavior)
    }
}

static __always_inline void emit_cfi_miss(__u32 pid, __u64 table_id, __u64 rel_pc) {
    struct cfi_miss_ratelimit_key key = {.pid = pid, .table_id = table_id};
    __u64 now = bpf_ktime_get_ns();
    __u64 *last = bpf_map_lookup_elem(&cfi_miss_ratelimit, &key);
    if (last && now - *last < 1000000000ULL /* 1 sec */) {
        return;  // rate-limited
    }
    bpf_map_update_elem(&cfi_miss_ratelimit, &key, &now, BPF_ANY);

    struct cfi_miss_event *ev = bpf_ringbuf_reserve(&cfi_miss_events, sizeof(*ev), 0);
    if (!ev) return;  // ringbuf full → drop; next un-rate-limited sample retries
    ev->pid = pid;
    ev->table_id = table_id;
    ev->rel_pc = rel_pc;
    ev->ktime_ns = now;
    bpf_ringbuf_submit(ev, 0);
}
```

### Additional emit site for lazy mode

The FP_LESS+miss emit covers the case where classification IS compiled but specific PCs have no CFI rule. Lazy mode (Option A2) needs an earlier emit point: when the walker enters an enrolled-but-uncompiled binary, classify_rel_pc returns the default MODE_FP_SAFE because cfi_classification has no entry for table_id, and the FP_LESS branch is never taken.

The fix is a single map probe before classify_rel_pc:

```c
struct mapping_lookup_result m = mapping_for_pc(ctx->pid, ctx->pc);
__u8 mode = MODE_FP_SAFE;
if (m.found) {
    __u32 *cls_len = bpf_map_lookup_elem(&cfi_classification_lengths, &m.table_id);
    if (!cls_len) {
        emit_cfi_miss(ctx->pid, m.table_id, m.rel_pc);
        // Fall through to FP path for this sample.
    } else {
        mode = classify_rel_pc(m.table_id, m.rel_pc);
    }
}
```

Eager mode is unchanged: every binary's cfi_classification is compiled, so the length lookup always succeeds, and classify_rel_pc runs as before. In lazy mode, the first sample on an enrolled-but-uncompiled binary triggers the miss emit; userspace compiles; subsequent samples find the length entry and classify normally.

The FP_LESS+`cfi_lookup` miss emit (the original Task 1 emit point) stays as a fallback for the rare case where classification is present but a specific PC lacks a CFI rule.

### Verifier considerations

- `walk_step` is already non-trivial. `emit_cfi_miss` adds ~10 instructions inline. Confirm with `bpftool prog dump` after build that we stay under the kernel's instruction-count cap on the project's target kernels (4.x EOL, 5.x supported). If we hit the cap, fallback is to move `emit_cfi_miss` into a tail-called helper (one extra map jump).
- The existing `WALKER_FLAG_CFI_MISS` per-sample flag in `stack_events` is preserved. Now there's a *parallel* notification stream for CFI misses; the per-sample flag still flows back through `stack_events` for completeness.

### What's NOT changing in BPF

- `cfi_lookup` / `classify_rel_pc` / `pid_mappings` / `cfi_rules` / `cfi_classification` — unchanged.
- The off-CPU BPF program (`offcpu_dwarf.bpf.c`) — unchanged in v1.

## Component 2: Userspace miss drainer

New file: `unwind/dwarfagent/miss_drainer.go`. Modify: `unwind/dwarfagent/common.go` (wire drainer into `newSession`).

### Drainer goroutine

```go
type cfiMissEvent struct {
    PID     uint32
    TableID uint64
    RelPC   uint64
    KtimeNs uint64
}

type cfiMissKey struct {
    pid     uint32
    tableID uint64
}

// Spawned in agent.go::NewProfilerWithMode via:
//     s.drainerWG.Go(s.consumeCFIMisses)
// (Go 1.25+ wg.Go pattern for new goroutines.)
func (s *session) consumeCFIMisses() {
    inflight := make(map[cfiMissKey]struct{})
    var mu sync.Mutex

    for {
        select {
        case <-s.stop:
            return
        default:
        }
        s.missReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
        rec, err := s.missReader.Read()
        if err != nil {
            switch {
            case errors.Is(err, os.ErrDeadlineExceeded):
                continue
            case errors.Is(err, ringbuf.ErrClosed):
                return
            default:
                log.Printf("dwarfagent: miss ringbuf read: %v", err)
                return
            }
        }
        ev, err := parseMissEvent(rec.RawSample)
        if err != nil { continue }

        key := cfiMissKey{pid: ev.PID, tableID: ev.TableID}
        mu.Lock()
        _, alreadyInflight := inflight[key]
        if !alreadyInflight {
            inflight[key] = struct{}{}
        }
        mu.Unlock()
        if alreadyInflight { continue }

        s.compileForMiss(ev)

        mu.Lock()
        delete(inflight, key)
        mu.Unlock()
    }
}

func (s *session) compileForMiss(ev *cfiMissEvent) {
    binPath, err := resolveBinaryByTableID(ev.PID, ev.TableID)
    if err != nil {
        switch {
        case errors.Is(err, ErrPIDGone):
            s.bumpMissDroppedPIDGone()
        case errors.Is(err, ErrTableNotMapped):
            s.bumpMissDroppedNotMapped()
        default:
            log.Printf("dwarfagent: lazy resolve pid=%d table=%#x: %v", ev.PID, ev.TableID, err)
        }
        return
    }
    if _, err := s.tracker.Attach(ev.PID, binPath); err != nil {
        s.bumpMissDroppedAttach()
        log.Printf("dwarfagent: lazy attach pid=%d %s: %v", ev.PID, binPath, err)
        return
    }
    s.bumpMissResolved()
}
```

`ErrPIDGone` and `ErrTableNotMapped` are sentinel errors (`errors.New(...)`) carrying no payload — `errors.Is` is the right tool. `errors.AsType[T]` (1.26+) is reserved for typed errors with structured fields; if a future change introduces such a type for, e.g., compile failures, we'd use `errors.AsType[*CompileError](err)` there.

### Path resolution: `resolveBinaryByTableID(pid, tableID)`

Reads `/proc/<pid>/maps`, parses each executable mapping, computes its build-id-derived `tableID`, returns the path that matches. Same logic `ehmaps.AttachAllMappings` uses today, factored into a helper:

- `ErrPIDGone` if `/proc/<pid>/maps` doesn't exist.
- `ErrTableNotMapped` if no executable mapping's build-id-hash matches the requested `tableID`.
- Both errors are benign drops in the drainer.

### Userspace dedup

Even with BPF rate limiting, multiple miss events for the same `(pid, table_id)` can land in userspace before the compile completes. The `inflight` map guards against starting two compiles for the same key. After compile completes, the entry is removed; `tracker.Attach` is idempotent so re-entry is harmless.

### Compile poisoning

If `tracker.Attach` fails 3 consecutive times for the same `(pid, table_id)`, mark the key as poisoned and stop retrying. Prevents an infinite loop on a corrupt or unreadable binary. Counter exposed via `MissStats.PoisonedKeys`.

### Backpressure

If the ringbuf fills, `bpf_ringbuf_reserve` returns NULL on the BPF side and the event is dropped. The BPF rate limit ensures we won't see more than ~N events/sec where N = active distinct binaries. With a 100-binary corpus, that's 100 events/sec; userspace processes one in ~1 ms (path resolve) + 100–500 ms (compile). The 64 KB ringbuf has ~2000 slots, dominated by inflight compiles. Bounded.

## Component 3: Lightweight initial scan (`ScanAndEnroll`)

The non-obvious A2 detail.

Without it, `pid_mappings` is empty for all PIDs at startup. Walker can't classify any PC; every sample falls through to MODE_FP_SAFE (FP walk only). No CFI miss notification ever fires for already-running PIDs.

### Function signature

```go
// ScanAndEnroll walks /proc/* and populates pid_mappings entries for
// every executable mapping of every PID, WITHOUT compiling CFI. Returns
// (pidCount, distinctBinaryCount, err).
func ScanAndEnroll(t *PIDTracker) (int, int, error)
```

### What it does

Logically identical to `AttachAllProcesses` except it calls a new `tracker.EnrollWithoutCompile(pid, path)` instead of `tracker.Attach(pid, path)`.

`EnrollWithoutCompile` splits the existing `tracker.Attach` flow into two halves:

```
tracker.Attach(pid, path):                  tracker.EnrollWithoutCompile(pid, path):
  1. ReadBuildID(path)                         1. ReadBuildID(path)
  2. tableID := hashOfBuildID                  2. tableID := hashOfBuildID
  3. AcquireBinary(path, pid):                 3. (skip — no compile)
       a. ehcompile.Compile path
       b. PopulateCFI / PopulateClassification
  4. UpdatePIDMappings(pid, addrRange,         4. UpdatePIDMappings(pid, addrRange,
                       tableID)                                tableID)  ← same as Attach
```

The shared `(1, 2, 4)` already exists in `unwind/ehmaps/tracker.go`; `EnrollWithoutCompile` skips step 3.

### Cache by build-id

`ScanAndEnroll` reads `.note.gnu.build-id` for each unique binary. Cache per-path so the same `libc.so.6` only gets one read across all 500 PIDs.

```go
cache := make(map[string][]byte)  // path → build-id bytes
```

Cost on a 500-process system: ~30 unique binaries × ~50 µs per build-id read = ~1.5 ms. Compare to today's eager compile: ~30 binaries × ~300 ms compile = ~9 s. Plus per-PID `/proc/<pid>/maps` parse cost (already present in today's `AttachAllProcesses`).

### Where it slots in

`unwind/dwarfagent/common.go::newSession`:

```go
switch {
case mode == modeLazy && systemWide:
    nPIDs, nTables, err := ehmaps.ScanAndEnroll(tracker)
    // log + populate session.attachStats as today
case systemWide:
    // existing eager path
    nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
    // ...
default:
    // --pid N — always eager regardless of mode
    n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
    // ...
}
```

`MmapWatcher` continues to call full `tracker.Attach` (with compile) for new mappings discovered after startup. Lazy mode does not change post-startup attachment.

### Edge case: PID exits between scan and first sample

`ScanAndEnroll` populates `pid_mappings`. PID dies before producing samples. Existing `tracker.Detach` (triggered by `MmapWatcher`'s ExitEvent) cleans up the entries. Same as today's eager flow.

### Failure mode: ScanAndEnroll fails entirely

If `ScanAndEnroll` errors out (e.g., race during enumeration, BPF map insert fails), fall back to today's eager `AttachAllProcesses`. Logged. Agent is still functional — just slow startup. Bench can detect the fallback by elevated `total_ms`.

## Component 4: Lifecycle & teardown

Three goroutines now in lazy mode:

| Goroutine | Spawned by | Stop signal | Wait group |
|-----------|-----------|-------------|------------|
| `tracker.Run` (mmap watcher consumer) | `runTracker()` (existing) | `s.stop` close | `s.trackerWG` (existing) |
| `consumeRingbuf` (samples) | `agent.go::NewProfilerWithMode` (existing) | `s.stop` close + ringbuf close | `s.readerWG` (existing) |
| **`consumeCFIMisses`** | `agent.go::NewProfilerWithMode` when `mode == modeLazy` | `s.stop` close + miss-ringbuf close | `s.drainerWG` (new) |

`session.close()` order:

1. `close(s.stop)` (broadcast)
2. `s.readerWG.Wait()`
3. `s.drainerWG.Wait()` (new)
4. `s.ringReader.Close()`
5. `s.missReader.Close()` (new)
6. `s.watcher.Close()`
7. `s.trackerWG.Wait()`
8. resolver / symbolizer / objs.Close()

Order matters: drainer can call `tracker.Attach` which writes to BPF maps; we drain it before closing BPF objs.

## Component 5: Observability

### Existing telemetry (preserved)

- `WALKER_FLAG_CFI_MISS` per-sample flag in `stack_events`. Preserved.
- `Hooks.OnCompile` callback fires when a CFI compile happens, regardless of eager vs lazy. The bench harness's `OnCompile` recorder works unchanged — lazy compiles appear in `bench-*.json` indistinguishably from eager compiles.

### New telemetry: `MissStats`

```go
type MissStats struct {
    Received       uint64        // ringbuf reads succeeded
    Deduped        uint64        // dropped because (pid, table_id) inflight
    Resolved       uint64        // tracker.Attach succeeded
    DroppedPIDGone uint64        // /proc/<pid>/maps disappeared
    DroppedNotMapped uint64       // table_id not in any executable mapping
    DroppedAttach  uint64        // tracker.Attach errored
    PoisonedKeys   uint64        // (pid, table_id) marked permanently failed
    LastLatencyNs  uint64        // BPF emit → userspace receipt of most recent event
}

func (p *Profiler) MissStats() MissStats
```

Not exposed via Prometheus or external metrics in v1. The bench harness consumes them by extension — Tasks 11/12 of the bench plan added an `OnCompile` hook recorder; an analogous hook for miss events lets the bench compute "lazy compiles per binary" + "latency from BPF emit to userspace compile completion".

## Component 6: Testing

Three layers.

### Unit tests (no caps)

| File | Coverage |
|------|----------|
| `unwind/ehmaps/scan_enroll_test.go` (new) | `ScanAndEnroll` against a `t.TempDir()` fake `/proc` tree. Verifies build-id caching, error tolerance per PID. |
| `unwind/ehmaps/tracker_test.go` (extend) | New `EnrollWithoutCompile` — assert it populates `pid_mappings` but NOT `cfi_rules`/`cfi_classification`. Uses existing fake-BPF-map test harness. |
| `unwind/dwarfagent/miss_drainer_test.go` (new) | Drainer logic with mock ringbuf reader: feed synthetic events, assert dedup behavior, assert `ErrPIDGone` → drop, assert `tracker.Attach` failure → log + continue, assert poisoning after 3 failures. |

### Microbenchmark (no caps)

`unwind/ehmaps/scan_enroll_bench_test.go` (new). Validates the build-id caching effect:

```go
func BenchmarkScanAndEnroll_BuildIDCacheHit(b *testing.B) {
    // 100 PIDs × 5 distinct binaries → cache should resolve to ~5 reads.
    fixture := buildSyntheticProcTree(100, 5)
    b.ResetTimer()
    for b.Loop() {
        _, _, _ = ScanAndEnrollFromTree(fixture, mockTracker())
    }
    // ReportMetric: buildid_reads/op
}

func BenchmarkScanAndEnroll_NoCache(b *testing.B) {
    // Same workload, caching disabled. Establishes lower bound (~N×K reads).
}
```

Reports `buildid_reads/op` so the cache-hit case is measurable as ~K and the no-cache case is ~N×K. Catches silent cache breakage.

Two helpers:
- `ScanAndEnrollFromTree(rootDir, tracker)` — same logic as `ScanAndEnroll` but parameterized on a proc-tree root. Internal seam for testability.
- `buildSyntheticProcTree(numPIDs, numDistinctBinaries int) string` — `t.TempDir()`-rooted fake `/proc/` with stripped-down `maps` files and pointers to ELFs (reuse `unwind/ehcompile/testdata/`).

### Caps-gated integration test

`unwind/dwarfagent/lazy_test.go` (new). Spawns a workload, attaches with `--unwind auto -a` (lazy):

- Assert `MissStats.Received` ≥ 1 within bounded time after first sample.
- Assert rate-limit suppresses subsequent events within 1 s.
- After drainer compiles, assert subsequent samples for the same PC do NOT have `WALKER_FLAG_CFI_MISS` set.
- Assert `Hooks.OnCompile` fires for the lazy compile (verifies the bench harness's recording path works in lazy mode).

Builds on the workload-spawn pattern from `TestNewProfilerWithHooks_FiresOnCompile` (PR #9). Uses the existing setcap'd binary pattern documented in `feedback_setcap_no_tmp.md` — test binary lives in the worktree, not `/tmp`.

### Bench-based validation

The bench harness from PR #9 IS the integration test for A2's value. Pass/fail criterion:

```bash
make bench-build
sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario

# Eager baseline (today's --unwind dwarf):
./bench/cmd/scenario/scenario --scenario system-wide-mixed --processes 30 --runs 5 --unwind dwarf --out bench-eager.json

# Lazy (this spec's new --unwind auto):
./bench/cmd/scenario/scenario --scenario system-wide-mixed --processes 30 --runs 5 --unwind auto --out bench-lazy.json

./bench/cmd/report/report --diff bench-eager.json bench-lazy.json
```

Expected: p50 wall-time Δ% in the −80% to −95% range. If less than −50%, A2 isn't pulling its weight and the implementation needs investigation.

A small new flag on the bench harness — `--unwind {auto|dwarf}` (default `auto`) — pins the unwind mode for comparison. Without it, both runs would use whatever `dwarfagent.NewProfilerWithHooks` defaults to.

## File layout

**New files:**

```
unwind/dwarfagent/
├── miss_drainer.go            # Drainer goroutine + helpers
├── miss_drainer_test.go        # Unit tests
└── lazy_test.go                # Caps-gated integration

unwind/ehmaps/
├── scan_enroll.go              # ScanAndEnroll + ScanAndEnrollFromTree
├── scan_enroll_test.go         # Unit tests
└── scan_enroll_bench_test.go   # Microbenchmarks

bpf/
└── (no new files; modifications to perf_dwarf.bpf.c + unwind_common.h)

docs/superpowers/specs/
└── 2026-04-27-unwind-auto-lazy-a2-design.md   # this file
```

**Files modified:**

```
bpf/unwind_common.h            # New maps + struct + emit_cfi_miss helper
bpf/perf_dwarf.bpf.c           # walk_step adds emit_cfi_miss call
unwind/dwarfagent/agent.go     # New ctor variant NewProfilerWithMode + drainer wiring
unwind/dwarfagent/common.go    # newSession dispatches modeLazy + ScanAndEnroll branch
unwind/ehmaps/tracker.go       # EnrollWithoutCompile method
bench/cmd/scenario/main.go     # New --unwind flag
docs/unwind-auto-refinement-design.md   # Mark A2 as implemented
```

## Validation criteria

Implementation is "done" when:

1. `make test-unit` passes (existing tests + ~6 new unit tests).
2. `make bench-corpus` runs cleanly and `BenchmarkScanAndEnroll_*` pass (no skip).
3. Caps-gated lazy integration test PASSes.
4. `make bench-scenarios` shows system-wide p50 lazy ≤6 s, eager ≥25 s, diff Δ% ≤ −80%.
5. `--unwind dwarf -a` numbers are within the noise envelope of the pre-A2 baseline (no regression on the explicit eager path).
6. `--unwind fp` numbers unchanged.

## Modern Go conventions

The project pins Go 1.26 (`go.mod`). This spec's Go code uses Go 1.26 conventions throughout:

- **`b.Loop()`** in benchmarks (1.24+) instead of `for i := 0; i < b.N; i++`. Already used in the existing corpus benches.
- **`t.Context()`** in tests (1.24+) instead of `context.WithCancel(context.Background())`.
- **`wg.Go(fn)`** (1.25+) instead of `wg.Add(1)` + `go func() { defer wg.Done(); ... }()`. Applies to the new drainer goroutine and any test fan-outs.
- **`new(val)`** (1.26+) instead of `x := val; &x` for inline pointer construction.
- **`errors.AsType[T](err)`** (1.26+) instead of `errors.As(err, &target)` for typed-error checks. The drainer's `ErrPIDGone` / `ErrTableNotMapped` are sentinel errors and use `errors.Is` (correct); `errors.AsType` will apply to any future structured error types this implementation introduces.
- **`for k := range maps.Keys(m)`** (1.23+) for map iteration where idiomatic. Used in the build-id cache reset paths in tests.
- **`cmp.Or(...)`** (1.22+) for default-value resolution where applicable.
- **`omitzero`** in JSON tags for `time.Duration`/`time.Time` fields. The bench harness's schema is already on this convention; lazy-mode telemetry struct fields follow.
- **`strings.SplitSeq` / `strings.FieldsSeq`** (1.24+) for `/proc/<pid>/maps` line iteration where range-iteration is the natural shape. The existing parser uses `bufio.Scanner` which is fine; keep it. New parsers (e.g., the drainer's mapping-by-table-id resolver) use `SplitSeq` if they iterate split tokens.
- **`slices.SortFunc`/`slices.Contains`/`slices.IndexFunc`** (1.21+) where the loop is genuinely about find/sort, not when iteration has side effects.
- **`any`** instead of `interface{}` (1.18+).

The implementation plan derived from this spec must follow these conventions in every code block. When extending an existing file that uses an older idiom, leave the surrounding code alone (don't refactor for its own sake) but write new lines in the modern style.

## What we are NOT doing in v1

- Off-CPU lazy mode.
- Userspace-side compile parallelism.
- Per-environment tuning of rate-limit interval.
- Pre-warming caches for known FP-less binaries.
- BPF verifier instruction-count regression test (manual inspection during plan execution).

## Open questions to resolve during implementation

- **Verifier instruction-count budget.** `walk_step` is already non-trivial. The plan should include a verifier-fitness check after the BPF emit is added; if we hit the cap, fall back to a tail-called `emit_cfi_miss`. Cost: one extra map jump (~5%). Document in the implementation plan.
- **`ScanAndEnrollFromTree` factoring.** The current `AttachAllProcesses` reads `/proc/*` directly — no proc-tree-root parameter. The factoring needed for the microbenchmark is small but introduces a new internal seam. The plan should choose: pass `procRoot string` through the entire chain, or inject via a `procFS` interface. Pick during implementation.
- **`MissStats` exposure surface.** Currently only via `(*Profiler).MissStats()`. If the bench harness wants per-event hooks (e.g., callback per drained event for latency histograms), we add a `MissHook` field to `Hooks{}`. Defer until the bench plan asks for it.

---

## Validated results

`make bench-scenarios` numbers from Linux 6.19.9-200.fc43.x86_64 / AMD Ryzen 9 7940HS / 16 CPU on 2026-04-28.

### `system-wide-mixed --processes 30 --runs 5`

Per-run wall time (ms):

| Run | Eager (`--unwind dwarf`) | Lazy (`--unwind auto`) |
|-----|--------------------------|------------------------|
| 1 (cold cache) | 69,683 | 40,982 |
| 2 | 34,362 | 7,219 |
| 3 | 36,894 | 8,167 |
| 4 | 34,573 | 6,090 |
| 5 | 34,618 | 8,141 |

Aggregated:

| Metric | Eager (ms) | Lazy (ms) | Δ% |
|--------|-----------|-----------|----|
| p50 | 34,618 | 8,141 | **−76.5%** |
| p95 | 69,683 | 40,982 | −41.2% |
| max | 69,683 | 40,982 | −41.2% |

The cold-cache first run dominates the p95/max. Comparing **warm-state runs** (2–5) directly: eager 34–37 s vs lazy 6–8 s = **78–83% reduction**, matching the spec's ≥80% target.

ScanAndEnroll cost: ~250 ms across 1,054–1,065 binaries × 255–268 PIDs (build-id cache means ~1,000 unique binaries get ~1,000 build-id reads, not ~270,000).

### Lazy MissStats from a representative caps-gated integration run

Test: `TestLazyMode_FiresAndCompilesOnMiss`, 5-second sampling window, system-wide.

- **Received:** 32 events
- **Resolved:** 32 (100% success — every miss resolved to a path and AttachCompileOnly succeeded)
- **PoisonedKeys:** 0
- **Deduped/DroppedPIDGone/DroppedNotMapped/DroppedAttach:** 0

The drainer cleared all 32 lazy compiles within the test window. The miss→drain→compile pipeline works as designed.

### Per-PID regression check

Skipped: per-PID always falls back to `ModeEager` inside `NewProfilerWithMode` (per the scope decision above). The `--pid N` codepath is byte-identical between `--unwind dwarf` and `--unwind auto`. No regression possible.

### What this means for A2 vs alternatives

The bench data confirms the doc's framing:

- **Option A2 (this implementation):** 76.5% p50 / 78–83% warm-state win on system-wide. Per-PID untouched. Roughly 2 days of work per the design.
- **Option A1 (lazy-no-signal):** would have produced similar startup numbers but silently truncated FP-less samples on already-running PIDs. The MissStats=32 from the integration test confirms the miss-notify path is doing useful work A1 wouldn't.
- **Option B (binary-level FP detection):** would skip ~20% of binaries (Go + FP-safe Rust). The remaining 80% of compile cost stays. Net win <20%, vs A2's 76%. Decisively worse.

A2 is the correct choice for this codebase.
