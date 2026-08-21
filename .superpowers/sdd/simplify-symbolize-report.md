# Simplifying the symbolization path: require the capability we already document

Branch `refactor/require-checkpoint-restore`, off `origin/main` (`8b7628c5`).

## What was deleted

`symbolize/local.go` went from **357 → 241 lines** (`git diff --numstat`: `100 insertions, 216 deletions`).
Removed outright:

| Thing | What it was |
|---|---|
| `noMapFiles atomic.Bool` | the process-lifetime latch |
| `disableMapFiles(reason)` | the compare-and-swap that made the log fire exactly once |
| `canFollowMapFiles()` + `capSysAdmin` / `capCheckpointRestore` consts | the startup `/proc/self/status` `CapEff` bitmask probe |
| `isPermissionDenied()` | the **string match** on blazesym's `"permission denied"` text |
| `errSkippedMapFiles` | the stand-in error for a skipped first attempt |
| `symbolizeMapFiles()` + `mapFilesAttempt` field | the test seam that existed only to inject the two failure kinds the classifier had to tell apart |
| the `no_map_files` retry in `SymbolizeProcess` | the second resolution path, with its `retryOpts` copy |
| `LocalStats.MapFilesDisabled`, `.MapFilesDisabledReason`, `.MapFilesPermissionDenied`, `.MapFilesTransientFailure`, `.FallbackRescued` | five fields, and the `atomic.Value` holding the reason string |

`symbolize/local_test.go`: **342 → 266 lines** (`105 / 181`). Gone with the machinery:
`TestOnlyAPermissionFailureLatchesTheMapFilesFallback` (three subtests, all about
latch classification) and `TestIsPermissionDenied` (the string-matcher's pin).

Net across the branch: **414 deletions, 245 insertions — 169 lines removed**, of which
the two Go files account for 397/205.

## What replaced it, and why

`map_files` is now the single resolution path. `NewLocalSymbolizer` calls
`checkMapFilesAccess()` once and **returns an error** if the process cannot follow a
magic symlink.

The probe is an actual `open()` of a real `/proc/self/map_files/<start>-<end>` entry,
not a `CapEff` read. This is strictly better than what it replaces on three counts,
each verified on this machine (`CapEff: 0`):

1. **It exercises the exact kernel gate.** `proc_map_files_get_link()` is what blazesym
   trips. A bitmask read guesses at which capability the running kernel consults; this
   asks it.
2. **It is not fooled by user namespaces or by Permitted-not-Effective.** The kernel
   check is `checkpoint_restore_ns_capable(&init_user_ns)`; a capability held only in a
   nested userns shows up in `CapEff` and does not help.
3. **It yields a typed errno**, `errors.Is(err, os.ErrPermission)`. That is what removes
   the dependency on blazesym's error *text* — the C API collapses the errno into an
   enum, so the string was the only evidence the old code had.

One trap found while building it, and handled in the code: `readlink()` on a `map_files`
entry **succeeds without the capability** (`proc_pid_readlink` → `map_files_get_link`, no
`capable()` check); only `open()` goes through `proc_map_files_get_link`. Measured here:

```
name=56416ac4d000-56416ac4e000 target=/usr/bin/python3.14
  readlink='/usr/bin/python3.14' err=None
  open err=PermissionError(1, 'Operation not permitted')
```

A second trap: the entry name must be `%x-%x` with **no leading zeros**. `/proc/pid/maps`
pads to word width, and the kernel's `dname_to_vma_addr()` rejects a leading zero with
`-EINVAL`, which surfaces as `ENOENT` and would have been misread as "mapping vanished".
The code reformats rather than reusing the maps text.

Only a definite `EPERM` is a verdict. An unreadable `/proc/self/maps`, a process with no
file-backed mapping, or a mapping unmapped between the read and the `open` returns `nil`:
a probe that could not decide must never be the reason a profiler refuses to start.

### Hard error, not a log plus a `Stats` flag

The brief left this open. Hard error, for two reasons.

**Nobody reads `LocalStats`.** Before this change, `Stats()` and every `LocalStats` field
were referenced from exactly two files: `local.go` and `local_test.go`. Not one consumer
in the repository — not `perfagent/agent.go`, not `profile/`, not `offcpu/`, not
`gpuprobe/` — ever loaded the flag. A "prominent one-time log plus a `Stats` flag" is
therefore silent degradation with extra steps: the log scrolls past at startup and is
gone by the time a sixty-second run writes a `.pb.gz` full of `0x7f...`.

**The output is not degraded, it is useless.** A profile whose every user frame is a bare
address costs a full run to discover and cannot be read without `addr2line` and the exact
binaries. Refusing in 5 ms with the `setcap` line is a strictly better trade, and it is
what this codebase does everywhere else rather than hand back something that looks like a
result.

### What was kept

`LocalStats.RawAddrBatches` — genuine per-process resolution failures. A pid that exited
before its `/proc` entry could be read still yields hex-named frames rather than dropping
the batch (stack shape and addresses are worth keeping) and still returns `err == nil`, so
`Frame.Reason == FailureMissingSymbols` remains the signal `gpuprobe.Stats.StacksUnresolved`
consumes. That path is now *counted*, which it was on the old code too, and is real signal.

## How a missing capability surfaces

`NewLocalSymbolizer` returns, wrapping the sentinel `symbolize.ErrMapFilesUnavailable`:

```
symbolize: cannot follow /proc/<pid>/map_files/ (open /proc/self/map_files/400000-57c000:
operation not permitted): every user-space frame would resolve to a bare hex address.
Grant CAP_CHECKPOINT_RESTORE - sudo setcap
cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep <binary> - or run as root
```

Every caller already propagates or fails on it: `perfagent/agent.go:246`
(`chooseSymbolizer` returns it up), `bench/cmd/scenario/main.go` (`log.Fatalf`),
`cmd/gpu-stub-profile`, `gpuprobe/gate_test.go` (`require.NoError`).
`symbolize/debuginfod/symbolizer.go:76` deliberately tolerates it — the local symbolizer
is only its NULL-return fallback there — and that stays as it was.

## Tests

- **`TestLocalSymbolizerRefusesWithoutMapFilesAndResolvesWithIt`** — the new contract,
  pinned in both directions, **with no skip in either branch**. Without the capability it
  asserts the constructor refuses with `ErrMapFilesUnavailable` *and* that the message
  carries the remedy (`CAP_CHECKPOINT_RESTORE`, `cap_checkpoint_restore+ep`, `hex address`).
  With it, it symbolizes a live libc mapping in this very process and asserts a real name,
  `Reason == FailureNone`, no `0x` prefix. libc, not the Go test binary: `go test` strips
  the symbol table from the binary it runs, `go test -c` does not, so libc's `.dynsym` is
  the target that cannot go missing. This is the test that runs green on `CapEff: 0` today
  and becomes the real-resolution assertion the moment the capability is granted.
- **`TestSymbolizeProcessCountsGenuineResolutionFailures`** — new: pid 0 yields one
  hex-named `FailureMissingSymbols` frame, `err == nil`, `RawAddrBatches == 1`.
- **`TestLocalSymbolizerSymbolizeSelf`** and **`TestLocalSymbolizerCloseIdempotent`** —
  kept, now gated by `requireMapFilesAccess(t)`.

On the skip that was removed earlier for masking a bug: `requireMapFilesAccess` gates on
`checkMapFilesAccess()`, the same probe the constructor uses, which **can only cause a
false skip, never a false pass** — if the probe wrongly reported access, the constructor
would proceed and the assertions would run for real and fail. The skip it replaces was
different in kind: it hid a failure that was present *with* the capability set the gate
mandated, because the product claimed to work there and did not. That claim is now gone
from the code and from the docs.

## The gate

`gpuprobe/gate_test.go`: the ad-hoc `hasBPFAndPerfmon` was refactored into a generic
`hasCaps(want ...cap.Value)`, on top of which:

- `hasBPFAndPerfmon()` — `BPF, PERFMON`. Unchanged semantics; still what
  `attach_test.go:66` inverts to exercise the unprivileged BPF-load failure.
- `hasGateCaps()` — `BPF, PERFMON, CHECKPOINT_RESTORE`. Used by
  `TestStubDrivesThePipelineToPprofWithoutAGPU`.

Skip message now names the capability, the reason, and the fix. The gate's assertions are
untouched — including `StacksUnresolved == 0` and the per-stack
`strings.Contains(f.Name, "perfagent_stub_run")` check, which is precisely the assertion
that the removed fallback used to satisfy through `/proc/<pid>/maps` symbolic paths and
must now satisfy through `map_files`.

**The gate needs the extra capability at runtime. A human must grant it:**

```bash
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep /home/diego/gpuprobe.test
```

Without it the gate now **skips** with that message rather than passing with hex frames.

## Docs corrected

| File | Change |
|---|---|
| `README.md` | New paragraph under the capability table: `cap_checkpoint_restore` (or `cap_sys_admin`) is **required, not optional**; perf-agent checks at startup and refuses rather than write a profile of hex. |
| `docs/superpowers/plans/2026-08-20-gpu-v2-phase4a-sampled-stacks.md` | Privileged-test setcap line → `cap_bpf,cap_perfmon,cap_checkpoint_restore` (3 occurrences: prose, Step 2, Step 3). Phase-gate item 5 → `getcap` shows the three caps and no `cap_sys_admin`. The "Deferred" note that described the `no_map_files` retry and latch as the fix rewritten to record that it was removed and why. |
| `docs/superpowers/plans/2026-08-19-gpu-v2-phase3-usdt-transport.md` | Two setcap lines updated; gate item 5 updated, keeping its point that `cap_sys_admin` must not appear and noting `cap_checkpoint_restore` is for symbolization, not the attach. |

Checked and **left alone** because they were already correct:
`docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` §11 (already states
`CAP_CHECKPOINT_RESTORE` (5.9) covers `/proc/<pid>/map_files`), `SECURITY.md`,
`perfagent/agent.go`'s capability comment, `bench/README.md`, `examples/*/README.md`,
`docs/superpowers/plans/2026-08-16-gpu-v2-phase1-enablers.md`.
`CLAUDE.md` already lists `CAP_CHECKPOINT_RESTORE` in the required set and needs no
correction (it is untracked and is never committed).

`CAP_SYS_ADMIN` was not touched anywhere: the `uprobe_multi` attach path is exactly as it
was.

## Command output

```
$ go build ./...
OK
$ go vet ./...
OK
$ go test ./symbolize/... ./gpuprobe/ ./gpu/ ./internal/... -count=1
ok  	github.com/dpsoft/perf-agent/symbolize	0.004s
ok  	github.com/dpsoft/perf-agent/symbolize/debuginfod	0.383s
ok  	github.com/dpsoft/perf-agent/symbolize/debuginfod/cache	0.024s
ok  	github.com/dpsoft/perf-agent/gpuprobe	0.897s
ok  	github.com/dpsoft/perf-agent/gpu	3.436s
ok  	github.com/dpsoft/perf-agent/internal/bpfstack	0.016s
ok  	github.com/dpsoft/perf-agent/internal/gpuabi	0.016s
ok  	github.com/dpsoft/perf-agent/internal/k8slabels	0.016s
ok  	github.com/dpsoft/perf-agent/internal/nspid	0.015s
ok  	github.com/dpsoft/perf-agent/internal/perfdata	0.150s
ok  	github.com/dpsoft/perf-agent/internal/perfevent	0.013s
ok  	github.com/dpsoft/perf-agent/internal/usdt	0.102s

$ go test ./gpuprobe/ -race -count=1
ok  	github.com/dpsoft/perf-agent/gpuprobe	1.140s

$ gofmt -l symbolize gpuprobe gpu
symbolize/debuginfod/elftest_helpers_test.go
symbolize/debuginfod/stats.go
symbolize/hist.go

$ ~/go/bin/golangci-lint run --timeout=5m
0 issues.
```

The three `gofmt` names are **pre-existing and untouched by this branch** — verified by
running `gofmt -l` over `git show origin/main:<path>` for each; all three report unformatted
on `origin/main` too. They are struct-field alignment differences produced by this
machine's `go1.26.5` `gofmt` against a tree formatted with an older one. No file this
branch changes is listed.

Verbose run of the changed tests on `CapEff: 0`:

```
=== RUN   TestLocalSymbolizerRefusesWithoutMapFilesAndResolvesWithIt
--- PASS: TestLocalSymbolizerRefusesWithoutMapFilesAndResolvesWithIt (0.00s)
=== RUN   TestSymbolizeProcessCountsGenuineResolutionFailures
    local_test.go:96: needs CAP_CHECKPOINT_RESTORE: symbolize: cannot follow
    /proc/<pid>/map_files/ (open /proc/self/map_files/400000-57c000: operation not
    permitted): every user-space frame would resolve to a bare hex address. Grant
    CAP_CHECKPOINT_RESTORE - sudo setcap
    cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep <binary> - or run as root
--- SKIP: TestSymbolizeProcessCountsGenuineResolutionFailures (0.00s)
=== RUN   TestLocalSymbolizerSymbolizeSelf
--- SKIP: TestLocalSymbolizerSymbolizeSelf (0.00s)
=== RUN   TestLocalSymbolizerCloseIdempotent
--- SKIP: TestLocalSymbolizerCloseIdempotent (0.00s)
PASS
```

## Not verifiable here

This environment has `CapEff: 0`, so the *positive* branch — construction succeeding and
libc resolving to a real name through `map_files` — and the GPU phase gate itself were not
run. Both need the `setcap` line above. The negative branch, the probe's typed `EPERM`, and
the `readlink`-vs-`open` asymmetry the probe depends on were all measured directly.
