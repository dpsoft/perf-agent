# Python perf-trampoline injector — design

**Date:** 2026-04-28
**Branch:** `feat/python-perf-injector`
**Status:** Spec — pending implementation plan

## 1. Problem

CPython 3.12+ ships an opt-in perf-compatible trampoline that emits per-function entries to `/tmp/perf-<pid>.map`, allowing external profilers (perf, perf-agent via blazesym) to attach human-readable Python qualnames to JITted frame addresses. The trampoline is activated either by launching the interpreter with `python3 -X perf` or by calling `sys.activate_stack_trampoline("perf")` from inside the process.

Today, perf-agent users who want Python JIT names must restart their target process with `-X perf`. That is a hostile UX for production debugging — many real targets (web servers under traffic, long-running pipelines, embedded interpreters) cannot be restarted on demand. Without injection, perf-agent's profiles for those targets render Python frames as `[jit]` placeholders or address-only entries.

This spec proposes a v1 injector that uses `ptrace` to remotely call `sys.activate_stack_trampoline("perf")` inside a running CPython 3.12+ process when the user passes `--profile --inject-python`, and to call `sys.deactivate_stack_trampoline()` on shutdown so the trampoline overhead does not persist past the profiling window.

## 2. Goals and non-goals

### Goals

- Activate Python's perf trampoline in a running CPython 3.12+ process via `ptrace`, without restarting the target.
- Deactivate the trampoline at end-of-profile so production overhead is bounded to the profiling window.
- Integrate cleanly into both `--pid N` and `-a` system-wide profiling modes.
- Cover both amd64 and arm64 architectures in v1.
- Provide structured stats (`Activated`, `Deactivated`, `Skipped[reason]`, `Failed`) for operator audit.
- Keep the injector self-contained — `inject/` package has no dependencies on `profile/`, `offcpu/`, or `cpu/`.

### Non-goals (v1)

- **Python < 3.12 support.** No equivalent activation primitive exists; older versions fall back to perf-agent's existing DWARF-through-interpreter unwinding (no JIT names).
- **CPython without `--enable-perf-trampoline`.** Detected via symbol-presence check; skipped with a structured reason.
- **PID-namespace translation for containers.** Host-side PIDs only. Documented limitation; small extension seam left for follow-up.
- **Persistent state across perf-agent restarts.** Each run is independent. Stale `/tmp/perf-PID.map` files are the user's responsibility to clean up if a previous run was killed.
- **Standalone subcommand for injection** (e.g., `perf-agent inject-python --pid N`). Considered and rejected; injection is bound to `--profile` runs.
- **Off-CPU / PMU integration.** `--inject-python` is `--profile`-only in v1.
- **Multi-version test matrix.** v1 tests against Python 3.12 only; broader matrix is a future PR.

## 3. Decisions reached during brainstorming

| # | Question | Decision |
|---|---|---|
| 1 | Python version scope | 3.12+ only |
| 2 | Integration UX | Profiling-time flag: `--profile --inject-python` |
| 3 | Lifecycle | Activate on profile start, deactivate on profile end |
| 4 | Implementation language | Pure Go via `golang.org/x/sys/unix` |
| 5 | Error model | Strict per-PID (`--pid N`); lenient system-wide (`-a`) |
| 6 | Architecture support | Both amd64 and arm64 in v1 |
| 7 | Detection + symbol resolution | SONAME or `/proc/<pid>/exe` match + `debug/elf` symbol resolution |
| 8 | Ordering | Inject before BPF attach |
| 9 | Test strategy | Unit tests + one full-pipe integration test + lenient mixed-fleet integration test |

## 4. Architecture

### 4.1 Package layout

A new top-level package, sibling to `unwind/`, `cpu/`, `offcpu/`:

```
inject/
  inject.go              # public types: Manager, Options, Stats, Target
  manager.go             # Manager struct, ActivateAll, DeactivateAll, dedupe
  detector.go            # Detector interface + concrete /proc-based impl
  errors.go              # ErrNotPython, ErrPythonTooOld, ErrNoPerfTrampoline,
                         # ErrStaticallyLinkedNoSymbols, ErrPreexisting
  payload.go             # Activate/deactivate C-string payload encoding
  elfsym/
    elfsym.go            # Public ResolveSymbols entrypoint
    soname.go            # SONAME parsing + version filter
  ptraceop/
    ptraceop.go          # Arch-generic coordination: attach → save → write →
                         # remote-call → restore → detach
    regs_amd64.go        # //go:build amd64 — call-frame setup,
                         # syscall trap encoding
    regs_arm64.go        # //go:build arm64 — call-frame setup,
                         # syscall trap encoding
```

Tests sit alongside (`*_test.go`).

### 4.2 Public surface

```go
package inject

type Options struct {
    StrictPerPID bool        // true for --pid N; false for -a
    Logger       *slog.Logger
}

type Manager struct { ... }

func NewManager(opts Options) *Manager
func (m *Manager) ActivateAll(targets []*Target) error
func (m *Manager) ActivateLate(pid uint32)        // mmap-watcher hook
func (m *Manager) DeactivateAll(ctx context.Context)
func (m *Manager) Stats() Stats

type Detector interface {
    Detect(pid uint32) (*Target, error)
}

type Target struct {
    PID            uint32
    LibPythonPath  string  // on-disk path used for ELF parsing
    LoadBase       uint64  // address from /proc/<pid>/maps
    PyGILEnsureAddr   uint64
    PyGILReleaseAddr  uint64
    PyRunStringAddr   uint64
}

type Stats struct {
    Activated         atomic.Uint64
    Deactivated       atomic.Uint64
    SkippedNotPython   atomic.Uint64
    SkippedTooOld      atomic.Uint64
    SkippedNoTramp     atomic.Uint64
    SkippedNoSymbols   atomic.Uint64
    SkippedPreexisting atomic.Uint64
    ActivateFailed     atomic.Uint64
    DeactivateFailed   atomic.Uint64
}
```

### 4.3 Integration with `perfagent.Agent`

```go
// perfagent/agent.go (additions only shown)
type Agent struct {
    // ...existing fields...
    injectMgr *inject.Manager  // nil unless --inject-python is set
}

func (a *Agent) Start(ctx context.Context) error {
    if a.injectMgr != nil {
        targets := a.scanForPythonTargets()  // detector ladder, see §5
        if err := a.injectMgr.ActivateAll(targets); err != nil {
            return fmt.Errorf("python injection: %w", err)
        }
    }
    // ...BPF attach, sampling start...
}

func (a *Agent) Stop(ctx context.Context) error {
    // ...profile finalization...
    if a.injectMgr != nil {
        a.injectMgr.DeactivateAll(ctx)
    }
    // ...close BPF, flush, exit...
}

func (a *Agent) InjectStats() inject.Stats  // nil-safe; zero value if off
```

For `-a` mode, the existing mmap watcher (`unwind/ehmaps/tracker.go`) gains a hook that calls `injectMgr.ActivateLate(pid)` when a new exec is detected.

## 5. Detection ladder

Detection runs per-PID inside ScanAndEnroll and the `--pid` shortcut equivalent. Each step short-circuits the next:

```
Step 1: read /proc/<pid>/maps
  └── Find executable mapping matching /libpython3\.(1[2-9]|[2-9][0-9])\.so/
       Capture: load_base, lib_path_on_disk
       └── Found      → goto Step 2 (dynamic-link path)
       └── Not found  → goto Step 1b (static-link fallback)

Step 1b: read /proc/<pid>/exe (resolves to on-disk path)
  └── Open as ELF; check .dynsym OR .symtab for libpython internal symbols
       (PyRun_SimpleString, PyGILState_Ensure)
       └── Found     → load_base = exe load offset; lib_path = exe path; goto Step 2
       └── Not found → return ErrNotPython

Step 2: parse ELF symbol table (debug/elf, .dynsym first then .symtab)
  └── Resolve four symbols by name:
       - PyGILState_Ensure       (required)
       - PyGILState_Release      (required)
       - PyRun_SimpleString      (required)
       - _PyPerf_Callbacks       (presence-only; address unused)
  └── If any of the first three are missing → ErrStaticallyLinkedNoSymbols
  └── If _PyPerf_Callbacks is missing       → ErrNoPerfTrampoline
  └── Compute remote addrs = load_base + symbol.Value
  └── Return *Target
```

### Why pre-flight `_PyPerf_Callbacks`

Symbol presence in `.dynsym` is a deterministic, free signal of `--enable-perf-trampoline`. Without this filter, perf-agent would attempt the full ptrace round trip (attach → write → remote call → trap → detach) only to have `PyRun_SimpleString` return -1. Filtering at symbol-table level skips those processes in microseconds.

### Edge cases

- **Multiple `libpython` mappings** (extension modules sometimes embed a second interpreter): use first match. Activation failure on a wrong-libpython cohort is non-corrupting (the SIGSEGV-at-0 sentinel returns control to the injector, which logs and skips).
- **PIE binaries (static fallback path):** `load_base` from `/proc/<pid>/maps` already accounts for ASLR.
- **Stripped binaries:** `.dynsym` is required at runtime; if it has the symbols, we proceed. `.symtab` fallback handles non-stripped-but-unusual builds.

### Explicitly out of scope for detection

- Reading `Py_Version` from process memory.
- Touching `_PyRuntime` or other interpreter state.

## 6. Injection sequence

The sequence is identical for activate and deactivate; only the payload string differs.

```
Activate payload:    "import sys; sys.activate_stack_trampoline('perf')\0"
Deactivate payload:  "import sys; sys.deactivate_stack_trampoline()\0"
```

### 6.1 Why three remote calls, not one

`PyRun_SimpleString` requires the calling thread to hold the GIL. The thread we attach to via `ptrace` is whatever thread happened to be running (usually the main thread, but not guaranteed) — and it may not hold the GIL at attach time. Calling `PyRun_SimpleString` directly from a thread that does not hold the GIL is undefined behavior (typically a deadlock against another thread that does).

The portable, well-tested solution (used by pyrasite, manhole's historical ptrace path, and several other Linux ptrace-injection tools) is to wrap the call:

```
gstate = PyGILState_Ensure()           // safely acquires GIL from any thread
result = PyRun_SimpleString(payload)   // executes payload under GIL
PyGILState_Release(gstate)             // restores prior GIL state
```

Each line is its own remote call (its own SIGSEGV-return trip). Three remote calls per activation; three more for deactivation on shutdown. We do all three within a single ptrace session (one `PTRACE_ATTACH`, one `PTRACE_DETACH`).

### 6.2 Per-target steps

```
1. ptrace(PTRACE_ATTACH, pid)                — stops target with SIGSTOP
2. waitpid(pid)                              — confirm stopped
3. ptrace(PTRACE_GETREGS, pid, &orig)        — save full register state
4. Read /proc/<pid>/maps; find target's [stack] mapping; verify
   below-SP headroom: current_SP - stack_low_addr >= 1024 bytes
   (stack grows downward; we write payload BELOW current SP, so the
   relevant slack is the gap between SP and the low end of the mapping)
   - If insufficient headroom: fall back to remote-mmap path (§6.4)
5. Choose payload_addr = orig.SP - 256 (16-byte aligned)
6. process_vm_writev(pid, payload_bytes) → writes payload at payload_addr

7. Remote call A: gstate = PyGILState_Ensure()
   - Build call frame from orig:
     amd64: RIP=PyGILState_Ensure_addr, RSP=payload_addr-8, *(RSP)=0
     arm64: PC=PyGILState_Ensure_addr,  SP=payload_addr-16 (16-aligned), LR=0
   - PTRACE_SETREGS, PTRACE_CONT, waitpid (blocks until SIGSEGV at addr 0)
   - Capture return: gstate = RAX (amd64) / X0 (arm64)
   - On non-SIGSEGV stop or unexpected status: abort, restore orig, detach,
     report error.

8. Remote call B: result = PyRun_SimpleString(payload_addr)
   - Build call frame:
     amd64: RDI=payload_addr, RIP=PyRun_SimpleString_addr, RSP=payload_addr-8, *(RSP)=0
     arm64: X0=payload_addr,  PC=PyRun_SimpleString_addr,  SP=payload_addr-16, LR=0
   - PTRACE_SETREGS, PTRACE_CONT, waitpid
   - Capture return: result = RAX / X0  (must be 0 = success)
   - If result != 0: activation/deactivation failed; still proceed to call C
     to release the GIL we acquired in A. Record error reason.

9. Remote call C: PyGILState_Release(gstate)
   - Build call frame:
     amd64: RDI=gstate, RIP=PyGILState_Release_addr, RSP=payload_addr-8, *(RSP)=0
     arm64: X0=gstate,  PC=PyGILState_Release_addr,  SP=payload_addr-16, LR=0
   - PTRACE_SETREGS, PTRACE_CONT, waitpid (return value ignored — function is void)
   - Always run this call, even if call B failed, so we never leave the
     target with an unbalanced GIL state.

10. ptrace(PTRACE_SETREGS, pid, &orig)       — restore original registers
11. ptrace(PTRACE_DETACH, pid, 0, 0)         — resume target; do NOT
                                               deliver the SIGSEGV (signal=0)
```

### 6.3 Why SIGSEGV-at-0 as the return sentinel

When each remote call returns, it pops its return address off the stack (or branches to LR on arm64) and jumps there. Setting that return address to `0` causes the target to dereference address 0 on return → SIGSEGV → ptrace stops the target → injector reclaims control. We never deliver the SIGSEGV signal (`PTRACE_DETACH` with `signal=0`, and intermediate `PTRACE_CONT` calls also use `signal=0`), so the target never sees it.

This is the standard approach used by pyrasite, manhole's historical ptrace path, and several Linux JIT injectors. The alternative (writing `int3`/`brk #0` into a target-allocated executable scratch page) requires a remote `mmap` round trip and adds complexity for no semantic gain in our use case (no other observer is debugging the target while perf-agent injects).

### 6.4 Stack-with-mmap-fallback for payload bytes

Default: write payload onto the target's existing stack at `SP - 256`. The 50-byte payload and 8-byte sentinel return-address slot fit easily in the gap. The 1024-byte minimum headroom check (step 4) is conservative — it guarantees we never write past the low end of the stack mapping even if we ever extend the payload.

Fallback: if `[stack]` mapping has less than 1024 bytes between the low end of the mapping and current SP (rare; extension threads can have very tight stacks, especially after deep recursion), perform a remote `mmap(NULL, 4096, PROT_READ|PROT_WRITE, MAP_PRIVATE|MAP_ANONYMOUS, -1, 0)` via the same remote-call machinery (one extra round trip), use the returned address for the payload, and `munmap` it on detach (one more round trip).

The fallback adds two extra ptrace round trips when triggered, but typical Python processes never hit it. We don't pre-emptively use mmap because the stack path is faster and simpler under normal conditions.

### 6.5 Per-arch encapsulation

`ptraceop/ptraceop.go` owns the arch-generic ptrace coordination (steps 1, 2, 3, 4, 5, 6, 10, 11) and the per-call wrapper that runs `PTRACE_SETREGS` → `PTRACE_CONT` → `waitpid` (which is identical across all three remote calls — only the input register frame differs). `ptraceop/regs_amd64.go` and `regs_arm64.go` own the per-arch register-frame setup for steps 7, 8, 9 and the return-value extraction. Cross-arch behavior is identical; only the register names and the calling convention differ.

## 7. Lifecycle and state tracking

### 7.1 Manager state

```go
type Manager struct {
    mu       sync.Mutex
    tracked  map[uint32]*trackedTarget
    pending  sync.Map  // uint32 → struct{}; in-flight late activations
    stats    Stats
    log      *slog.Logger
    detector Detector
    injector *injector  // internal type; calls into ptraceop/
    opts     Options
}

type trackedTarget struct {
    target      *Target
    activatedAt time.Time
    preexisting bool  // /tmp/perf-<pid>.map already existed at activation time
}
```

### 7.2 Idempotency: the preexisting marker

Before activation, the injector checks for an existing `/tmp/perf-<pid>.map` file. If present and non-empty, the trampoline is already running (either from a prior perf-agent run or from the user's own activation). The injector:

1. Records the target with `preexisting=true`.
2. Increments `Stats.SkippedPreexisting`.
3. **Skips both activation and deactivation** for this target.

This is intentionally conservative. False positives (skipping when we could have activated) cost the user no JIT names for that target during this run. False negatives (deactivating someone else's instrumentation) are a correctness violation. We err on the side of correctness.

### 7.3 ActivateAll — strict and lenient policies

```go
func (m *Manager) ActivateAll(targets []*Target) error {
    for _, t := range targets {
        if m.alreadyActiveOnDisk(t.PID) {
            m.recordPreexisting(t); continue
        }
        if err := m.injector.Activate(t); err != nil {
            if m.opts.StrictPerPID {
                return fmt.Errorf("activate pid=%d: %w", t.PID, err)
            }
            m.log.Warn("activate failed (lenient)", "pid", t.PID, "err", err)
            m.stats.ActivateFailed.Add(1)
            continue
        }
        m.recordActivated(t)
        m.stats.Activated.Add(1)
    }
    return nil  // lenient never returns from this loop
}
```

### 7.4 DeactivateAll — bounded shutdown

```go
func (m *Manager) DeactivateAll(ctx context.Context) {
    deadline := time.Now().Add(5 * time.Second)
    deadlineCtx, cancel := context.WithDeadline(ctx, deadline)
    defer cancel()

    for pid, tt := range m.snapshotTracked() {
        if tt.preexisting { continue }
        select {
        case <-deadlineCtx.Done():
            m.log.Warn("deactivate cancelled (deadline)", "abandoned", ...)
            return
        default:
        }
        if err := m.injector.Deactivate(tt.target); err != nil {
            if errors.Is(err, syscall.ESRCH) { continue }
            m.log.Warn("deactivate failed", "pid", pid, "err", err)
            m.stats.DeactivateFailed.Add(1)
            continue
        }
        m.stats.Deactivated.Add(1)
    }
}
```

The 5s hard cap exists because a target in uninterruptible sleep (D state) can hang `waitpid` indefinitely. Shutdown must never block on a hostile target. Abandoned targets keep the trampoline active until process exit; the user can clean up `/tmp/perf-PID.map` post-exit.

### 7.5 Late-arriving processes (`-a` mode only)

The mmap watcher fires per-mapping change. New Python execs trigger `Manager.ActivateLate(pid)`, which:

1. Skips immediately if `tracked[pid]` exists (already activated).
2. Skips immediately if `pending[pid]` exists (in-flight; mmap watcher emits multiple events per exec as extension modules load).
3. Inserts into `pending`, runs `Detect` + `Activate` (always lenient, regardless of `--inject-python-strict`), removes from `pending`.
4. PID recycle: when the mmap watcher's existing exit-notification path fires, evict `tracked[pid]`.

Bounded concurrency: `min(runtime.NumCPU(), 4)` worker goroutines drain a channel of late-arrival PIDs. A burst (e.g., a forking web server) doesn't fire 32 concurrent ptrace operations.

## 8. CLI surface

### 8.1 Flag

```
perf-agent --profile --inject-python [--pid N | -a] ...
```

Single boolean flag. No `--inject-python=auto|on|off|force`. Future flags (`--inject-python-strict` to force strict mode for `-a`, `--inject-python-deactivate-on-exit=false`) are added when there's actual user demand. YAGNI for v1.

### 8.2 Behavior matrix

| Combination | Behavior |
|---|---|
| `--profile --pid N --inject-python` | Strict per-PID. Detect+activate; any failure → exit non-zero with structured reason. |
| `--profile -a --inject-python` | Lenient. Walk /proc, detect+activate every Python 3.12+ process. Failures logged + counted. New execs during the run also activated (lenient). |
| `--profile --inject-python` (no `--pid`/`-a`) | Same error as today: requires `--pid` or `-a`. |
| `--inject-python` without `--profile` | Error: "`--inject-python` requires `--profile`." |
| `--offcpu --inject-python` | Error: "`--inject-python` is only supported with `--profile`." |
| `--pmu --inject-python` | Error: same as off-cpu. |

### 8.3 Cap precheck

When `--inject-python` is set, `perfagent.Agent.validateOptions()` confirms `cap_sys_ptrace` is held by the running process **before** scanning. Absent → structured error, non-zero exit, no profile attempted. (A setcap'd binary that dropped `cap_sys_ptrace` would otherwise silently no-op all injections.)

### 8.4 Logging

All injector log lines are structured (slog) with stable fields:

```
level=info  msg="python inject activated"  pid=12345 libpython=/usr/lib/libpython3.12.so.1.0 mode=dynamic
level=warn  msg="python inject skipped"    pid=23456 reason=no_perf_trampoline
level=warn  msg="python inject skipped"    pid=34567 reason=preexisting_perf_map
level=warn  msg="python inject failed (lenient)" pid=45678 reason=ptrace_eperm err="operation not permitted"
level=info  msg="python inject summary"    activated=8 skipped=3 failed=1
```

The summary line at end-of-scan gives operators a one-glance audit without grepping individual events.

### 8.5 Bench harness integration

`bench/cmd/scenario/main.go` already plumbs `--unwind`. We add `--inject-python` parallel:

- `--unwind dwarf` (no inject) — DWARF through interpreter, no JIT names.
- `--unwind dwarf --inject-python` — DWARF + JIT names.
- `--unwind auto --inject-python` — lazy CFI + JIT names (recommended production combo).

`Document` schema (`bench/internal/schema/schema.go`) gains `InjectPython bool` so reports can compare with/without.

## 9. Test plan

### 9.1 Unit tests (no caps, no real Python)

```
inject/elfsym/
  symtab_test.go
    TestParseSONAME_PythonVariants
    TestParseSONAME_NotPython
    TestResolveSymbols_DynsymHit
    TestResolveSymbols_FallsBackToSymtab
    TestResolveSymbols_MissingPyRunSimpleString  → ErrStaticallyLinkedNoSymbols
    TestResolveSymbols_MissingPerfCallbacks      → ErrNoPerfTrampoline
    TestResolveStaticBinary

inject/payload_test.go
  TestEncodeActivatePayload     (exact bytes, null-terminated)
  TestEncodeDeactivatePayload   (exact bytes, null-terminated)

inject/ptraceop/
  regs_amd64_test.go            (//go:build amd64)
    TestSetupCallFrame_amd64    (RDI/RSP/RIP set; sentinel return-addr)
    TestRegsRoundTrip_amd64     (save → modify → restore == original)

  regs_arm64_test.go            (//go:build arm64)
    TestSetupCallFrame_arm64    (X0/SP/PC/LR set; sentinel)
    TestRegsRoundTrip_arm64

inject/manager_test.go
  TestActivateAll_StrictExitsOnFirstError
  TestActivateAll_LenientContinuesOnError
  TestActivateAll_PreexistingMarkerSkips
  TestDeactivateAll_HonorsDeadline
  TestDeactivateAll_SkipsPreexisting
  TestDeactivateAll_ToleratesESRCH
  TestNewExec_DedupesViaTracked
  TestNewExec_DedupesViaPending
```

Synthetic-procfs pattern reused from PR #11's `scan_enroll_test.go::buildSyntheticProcTree`. Synthetic ELF files for symbol-resolution tests use `debug/elf.NewFile` over an in-memory byte buffer with a minimal `.dynsym`. Register tests in `ptraceop_*_test.go` exercise frame setup logic over hand-crafted `unix.PtraceRegs` values (real `ptrace` is exercised in integration).

### 9.2 Integration tests (caps-gated)

```
test/integration_inject_test.go
  TestInjectPython_ActivatesTrampoline
    1. Launch python3.12 test/workloads/python/cpu_bound.py 10 2 (no -X perf)
    2. Wait 1s for warmup
    3. Run perf-agent --profile --inject-python --pid <PID> --duration 5s
    4. Assert /tmp/perf-<PID>.map exists, contains py:: lines
    5. Assert profile.pb.gz has Python frame names
    6. Assert agent.InjectStats(): Activated=1, Deactivated=1
    7. After perf-agent exit: re-stat perf-map twice with 1s gap; size unchanged
       (proves deactivation ran)

  TestInjectPython_StrictFailsOnNonPython
    1. Launch test/workloads/go/cpu_bound (Go binary)
    2. Run perf-agent --profile --inject-python --pid <PID>
    3. Assert non-zero exit
    4. Assert stderr contains structured "not_python" reason

  TestInjectPython_LenientSystemWideMixedFleet
    1. Launch fleet: 2× python3.12, 1× python3.10 (no perf-trampoline),
       1× Go binary
    2. Run perf-agent --profile -a --inject-python --duration 5s
    3. Assert exit 0
    4. Assert agent.InjectStats(): Activated=2, SkippedTooOld=1,
       SkippedNotPython=1, Failed=0
    5. Assert profile contains Python frames from the two 3.12 processes
```

Setup pattern (per established convention):
- Build perf-agent in the worktree (NOT `/tmp` — saved as feedback memory; `/tmp` is `nosuid` and file capabilities don't survive exec).
- `sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent`.
- Test setup probes `cap_get_proc()` and skips with a clear reason if caps are missing.

### 9.3 Out of scope for v1

- Multi-Python-version matrix.
- Container scenarios (no Docker test infrastructure).
- Concurrent perf-agent runs against the same target (preexisting marker handles correctness; no dedicated test).
- Bench results comparing with/without `--inject-python` (informational, follow-up PR).

## 10. Documentation

- **`README.md`** — new section on Python profiling with a worked example: launch a Python script *without* `-X perf`, run `perf-agent --profile --pid $! --inject-python`, see Python frames.
- **`docs/python-profiling.md`** (new) — deeper dive: how the trampoline works, when injection is skipped and why, the preexisting marker, perf-map cleanup, container limitations, how to disable in CI.

## 11. Risks and open questions

- **Activation latency on large fleets.** 100 Python processes × ~2 ms per ptrace round trip = 200 ms before sampling starts. Acceptable for a profiling tool but worth measuring once v1 ships.
- **`PyRun_SimpleString` re-entrance.** If the target is mid-execution of `PyRun_SimpleString` when we attach (rare; would require user code calling exec/eval), the GIL handoff our payload performs may serialize behind the in-flight string. We accept the brief delay.
- **Stack headroom on extension threads.** The 1024-byte fallback threshold is conservative; we should verify it on real-world targets (e.g., Apache+mod_wsgi, gunicorn workers). If too tight, raise to 2048 — costs nothing.
- **Symbol versioning (`@@GLIBC_2.34`-style).** CPython doesn't use versioned symbols on its public ABI, but ELF tooling sometimes returns them. `debug/elf` handles the parsing; we just need to match by base name. Documented as test coverage in `TestResolveSymbols_DynsymHit`.

## 12. References

- CPython 3.12 release notes: <https://docs.python.org/3.12/whatsnew/3.12.html#perf-profiler-support-with-stack-pointers>
- `sys.activate_stack_trampoline` docs: <https://docs.python.org/3.12/library/sys.html#sys.activate_stack_trampoline>
- pyrasite (reference implementation of remote Python ptrace injection): <https://github.com/lmacken/pyrasite>
- Linux `ptrace(2)` manual: <https://man7.org/linux/man-pages/man2/ptrace.2.html>
- AMD64 System V ABI: <https://gitlab.com/x86-psABIs/x86-64-ABI>
- AArch64 AAPCS64: <https://github.com/ARM-software/abi-aa/blob/main/aapcs64/aapcs64.rst>
