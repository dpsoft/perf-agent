# Capture from the MCP (M2c-capture) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Plain stdio `perf-agent-mcp` gains four tools: `capture`, `capture_wait`, `capture_cancel` and `capture_list`. They run the user's setcap'd `perf-agent` binary as a supervised child against one PID or one launched command. The pprof (and the GPU executions log) goes into a numbered series under the profiles root, and the result carries `open` handles plus a `compare_with` diff summary. An autonomous agent can then loop: capture → analyse → change → re-capture → compare.

**Architecture:**
- **`internal/capture`** is new and knows nothing about the transport. It holds:
  - the binary and shim verifiers, the target preflight and recheck, and the argv builder;
  - the captures-directory `flock`;
  - the reaper and launcher internal modes of the same binary;
  - `Run`, the server side of one capture.

  Linux code is in `_linux.go` files. `capture_other.go` disables everything elsewhere.
- **`internal/perfmcp`** holds:
  - the series layout, the manifests and the intent/commit protocol;
  - recovery and retention;
  - the job state machine (§4.0), and the bounded registry with its lookup pool;
  - the engine lock;
  - the four tools.
- **`internal/mcpstdio`** gets a reader that never waits, control messages, and one bounded asynchronous writer.
- **`cmd/perf-agent-mcp`** adds the flags and routes the internal modes. It kills children on every signal path.

**Tech Stack:** Go 1.26 standard library only (`os/exec`, `syscall`, `debug/buildinfo`, `debug/elf`, `crypto/sha256`, `sync/atomic`). Tests may use `golang.org/x/sys/unix`, as `main_test.go` already does. No new module.

**Spec:** `docs/superpowers/specs/2026-11-15-capture-design.md`, closed after six Codex rounds. Within it:
- §4.0 is the single lifecycle definition;
- §7.4 is the single file-protocol order;
- §15 maps every review finding.

Related records:
- **Design rulings** (CAP-R1..R17, SRV-R5/R6/R7) and the Codex #418 carries: the end of the M2b live-page progress log (`<m2b-worktree>/.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`).
- **Plan rulings** (pre-flight P-1..P-29, Codex #428 PLAN-R1..R7, and the adopted notes): `.superpowers/sdd/2026-11-15-capture-plan/progress.md` in this worktree.
- **The serve design** (`docs/superpowers/specs/2026-11-15-local-server-design.md`) is **not** built here. This plan only leaves the seams it names (see "Seams for serve").

**Base:** branch `ai-mcp-m2c-capture`. It holds `ai-mcp-m2b` at 34a9d1c1 (the trimmed #264: no `--live`, no stdio `page_*` tools) plus the commit with the spec and this plan.

## Global Constraints

- Go 1.26 (`go 1.26.0` in go.mod). **Invoke `modern-go-guidelines:use-modern-go` before writing any Go**, not as a cleanup pass.
- **Every task ends green, with no forward dependency (PLAN-R4).** A task declares every type, helper, scan allowance and test-expectation update that its own code or tests first use. Each task's Step 4 runs, besides its own tests:

  ```bash
  go test -race ./internal/... ./cmd/perf-agent-mcp/
  GOOS=darwin go build ./internal/capture/ ./internal/perfmcp/ ./cmd/perf-agent-mcp/   # PLAN-R5
  ```

  No existing guard is weakened to get there.
- Tests run with `-race`. Lint with golangci-lint **v2.11.4**: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`. A missing binary exits 127, which is not a pass.
- Commands that build the perf-agent root package (the contract test, `./...`) need these variables. The MCP packages do not import blazesym.

  ```bash
  CGO_CFLAGS=-I<blazesym>/capi/include
  CGO_LDFLAGS=-L<blazesym-lib-dir>
  ```
- **Build on top [user].** Nothing that predates the MCP work changes:
  - not `main.go`, `telemetry.go`, `perfagent/`, `cmd/flamegraph` or `gpu/*`;
  - not `profilemodel`'s semantics;
  - no perf-agent flag is added.

  The one new file on that side is the test-only `capture_contract_test.go` in the root `package main`. MCP code may change: `internal/perfmcp`, `internal/mcpstdio`, `cmd/perf-agent-mcp`, the guide, the skill and `docs/mcp.md`.
- **The server never holds or raises a capability.**
  - It never loads BPF, and never links `perfagent`, `cilium/ebpf` or blazesym. `TestTheServerLinksNoProfiler` stays unchanged and passing. The non-std modules stay ⊆ {`github.com/dpsoft/perf-agent`, `github.com/google/pprof`}, so there is **no `x/sys` in non-test code**.
  - It never runs `sudo`, `pkexec` or a shell.
  - Capture is disabled when the server's euid is 0, when its CapEff or CapPrm is non-zero, or when it runs with `NoNewPrivs: 1` (P-24).
- **Process starts.**
  - `os/exec` is imported only by `internal/sourceroot/git.go` and `internal/capture/exec_linux.go`. Every `os/exec` identifier lives in that file, `*exec.Cmd` included; other files hold an opaque wrapper (P-14).
  - `exec_linux.go` has exactly two `exec.Command` calls: the verified perf-agent, and the server's own executable in reaper or launcher mode.
  - `syscall.Exec` and `syscall.RawSyscall` appear only in `internal/capture/launch_linux.go`. The raw call is only `RawSyscall(SYS_PRCTL, prSetNoNewPrivs|prSetChildSubreaper, …)`.
  - The network scan rules for these land in Task 9, the first task whose code the binary links.
- **Signals.**
  - Signals go only through `syscall.Kill(-pgid, sig)` on a recorded pgid. **Never `pkill -f`**, in code, tests or the executor's shell.
  - **The server never kills the reaper (PLAN-R1).** Its kill path is: a best-effort kill of the recorded groups, then closing the reaper's command pipe. The reaper then runs its cleanup (cgroup.kill, the PID sweep, the survivor report) and exits only afterwards.
- **Environments are exact.**
  - perf-agent gets exactly `PATH=/usr/local/bin:/usr/bin:/bin` and `LC_ALL=C`.
  - A workload gets from the server only `PATH HOME USER LOGNAME LANG LC_* TZ TMPDIR XDG_RUNTIME_DIR CUDA_VISIBLE_DEVICES`, plus `CUDA_INJECTION64_PATH=<shim>` for `gpu`.
- **Stdout carries only JSON-RPC.**
  - Child output goes to bounded rings (16 KiB) and to slog debug on stderr.
  - The result returns only the workload's last 2 KiB per stream, labelled `untrusted`, and perf-agent's stderr tail only on failure.
  - Results and manifests never carry `cmdline`, the environment, or the argv beyond `command[0]` and the argument count.
- **Wire rules (M2).**
  - Every integer is a decimal string, and durations are Go duration strings.
  - Errors repeat agent input only through `echo`.
  - A capture failure is an `isError` result whose text is one JSON object `{"error":{…}}` (§4.4).
- **Fixed values** from the spec:
  - `capture_wait`: default 30 s, cap 45 s;
  - registry `MaxJobs` 32 and `MaxJobBytes` 4 MiB; lookup pool of 2 workers with a queue of 16;
  - heartbeat every 2 s, stale after 10 s; stranded-rollback retry every 30 s;
  - target recheck every 1 s; size, exit and staging polls every 250 ms;
  - stdio queue of 8 requests, overload code `-32000`; writer of 64 messages and 4 × `MaxResponseBytes`;
  - stdio drain on shutdown 800 ms (P-2);
  - the §6.1 limits table, with its defaults and compiled ceilings.
- **Wording (Codex #418).** Never call the manifest link + fsync window "bounded". Errors after a successful manifest link (directory fsync, removal of staging, intent or heartbeat) follow the committed path: they are logged and the job is `done`.
- **Privileged runs** (memory notes):
  - `setcap` the binary; never sudo-wrap a test run.
  - Never run a setcap'd binary from `/tmp`, which is mounted nosuid. Build it in the worktree or under `~/`.
  - Gates check capabilities (the file xattr and the process status), never `Getuid`.
  - `kernel_stacks` needs `cap_syslog`.
  - The setcap'd perf-agent links blazesym statically (`make build`).
- **Platforms (PLAN-R5).** Linux only, kernel floor 6.2. File by file:
  - **Common (untagged):** the types and pure logic: `doc.go`, `errors.go`, `caps.go`, `verify.go` (pure checkers), `types.go`, `argv.go`, `command.go`, `selfcheck.go`.
  - **`_linux.go`:** everything that touches xattrs, mount flags, `/proc`, `flock`, `prctl`, processes or cgroups.
  - **`capture_other.go` (`//go:build !linux`):** `const Supported = false` plus a stub of every exported function and method the Linux files define. Each stub returns `Errorf(CodeDisabled, "", "capture needs Linux")`. `supported_linux.go` holds `const Supported = true`.

  Each task adds the stubs for what it adds. Tests that need Linux are `_linux_test.go`.
- Comments say what the code does and why. No issue numbers, review IDs or ruling IDs in comments; a spec section number (§7.4) is fine in a test's doc comment.
- **Commits.**
  - Use the package prefix: `capture: …`, `perfmcp: …`, `mcpstdio: …`, `perf-agent-mcp: …`, `docs: …`.
  - No Co-Authored-By, no AI attribution and no session URLs (POLICY.md).
  - Never commit `CLAUDE.md`.
  - Push only in Task 19, as a draft PR. **Never merge**; ask the user before any merge.
- **Model routing.** Tasks 6, 7, 10, 13 and 16 are load-bearing: process supervision, the file protocol and the state machine. Route them, and their reviews, to opus.

## Review Focus

1. **`command: ["./bench"]` with `cwd`.** This is the most likely way an agent names its benchmark. The spec allows only an absolute path or a bare name. Expected: a synchronous `invalid_argument` whose `next` gives the absolute path, joined with `cwd` as strings (no I/O): "pass `/home/u/proj/bench`". Test in Task 14 (`TestCaptureRelativeCommandSuggestsTheAbsolutePath`).
2. **A command that exits or daemonizes before perf-agent attaches**, such as a wrapper that forks the real server and returns. Expected: `target_exited`, whose `next` says the launched PID ended after X s and that a daemon is captured with `pid`. The output tail is included. Test in Task 7 (`TestRunForkAndExitIsTargetExitedWithAPIDHint`).
3. **A workload that floods stdout or stderr, or writes invalid UTF-8.** Expected:
   - it never stalls on a full pipe, so `profiled_duration` ≈ `duration`;
   - the tail is at most `MaxOutputTail` bytes, cut on a rune boundary;
   - the result is valid JSON.

   Test in Task 7 (`TestRunWorkloadFloodNeverStallsAndTailIsValidUTF8`).
4. **`compare_with` naming a failed or interrupted capture.** Expected: `unknown_baseline`, which names the outcome ("003 failed: target_exited; it has no profile") and lists the series' last `ok` ids. Test in Task 13 (`TestCompareWithAFailedCaptureIsRefusedNamingItsOutcome`).
5. **A profiles root on a filesystem that refuses hard links.** Expected: the job fails in `preparing`, before perf-agent starts, with the M2 hard-link advice. The check is a no-replace link probe inside the new staging directory. Test in Task 10 (`TestStagingProbeRefusesAFilesystemWithoutHardLinks`).

## Builder's calls settled here

| Call | Decision | Reason |
|---|---|---|
| Fake perf-agent | A separate `main` package, `internal/capture/capturetest/fakeperfagent`, built once per test process. It reads its mode from `fake-mode.json`, and its fixtures from `fixtures/`, beside its own path. Each test gets a hard link named `fakeperfagent` in its own directory | §5.3 gives perf-agent only `PATH` and `LC_ALL`, so no env var reaches a fake. The basename keeps `comm` checkable (P-26) |
| Waiting for children | The reaper starts children with `Start`, never calls `Cmd.Wait`, and reaps everything with one `wait4(-1)` loop dispatched by PID. After each reap it calls `Process.Release` and closes that child's pipe ends | `Cmd.Wait` would race the subreaper loop |
| Admission vs I/O (PLAN-R3) | `capture` checks only shapes, ranges, the cached binary caps and shim, the cached owner, the in-process slot and the windows. It **reserves** window duration at a safe upper bound: the explicit `duration`; else `MaxDuration` when `compare_with` is given; else 10 s. The job resolves the effective (inherited) settings and validates them against *this* server's limits, caps and shim. It then **reconciles** the reservation, or fails async. `cwd`, `command[0]`, `MaxKeptPerSeries`, the baseline and `incomparable` are async too | §4.0 says "pure checks, no I/O"; Codex #428-3 |
| Before the lease | Creating `<captures>` (0700) and writing `.owner` (no-replace) come before the lease, since `.lock` is inside the directory. So does the read-only resolution | Otherwise unsatisfiable on first use |
| Start-up recovery and retention | A background maintenance goroutine after `New`, under the in-process `dirMu`, a non-blocking lease and the engine, exactly as a job. While it holds the engine, only the non-engine calls are guaranteed responsive (P-8) | Codex's `startup_timeout_sec` is 10 s |
| Failure manifests | A failure before step 4 (no id yet) leaves no manifest. From step 4 on, every failure ends in a failure manifest, cancellations included | A manifest needs an id |
| `show`, `publication` | Not in stdio's schema or result; serve adds them through `capturePublish` | §4.2 |
| `capture_cancel` of an unknown id | Registry only: an id this process is not running gives `unknown_capture` | CAP-R10: no file I/O |
| Cancellation (PLAN-R2) | Each job has `cancel chan struct{}`, closed once (`sync.Once`) by `requestCancel` and `cancelAllJobs`. The job goroutine selects on the outcome of `Run.Wait` and on `cancel`, and calls `Run.Cancel`. A cancel that wins before `Run` exists is honoured as soon as it does. The control handler does a CAS and a channel close only | Codex #428-2 |
| Kill path (PLAN-R1) | `Run.Kill`, `KillCaptures`, `Close` and the forced exit: `kill(-pgid, SIGKILL)` for the recorded groups, then close the command pipe. The reaper is never signalled | A setsid'd grandchild is in neither group; only the subreaper or cgroup can reach it |
| Engine acquire (P-1) | `acquire(ctx)` tries the slot without blocking first, and selects on `ctx.Done()` only when the slot is held | M2 pipelines (`printf … \| perf-agent-mcp`) still get answers at EOF |
| Signals (P-2) | On ctx done, `Serve` drains for ≤ `DrainTimeout`, which perf-agent-mcp sets to 800 ms. `main` calls `KillCaptures` on **every** signal path, before `Close`. After `KillCaptures`, `Close` skips its job wait | Otherwise the forced branch is unreachable and `Close` blocks on rollback |
| Job ids | `j<N>-<4 hex>`, where the hex is a per-process nonce | A restarted server must not answer another process's `j1` |
| `prctl` constants | `prSetChildSubreaper = 36` and `prSetNoNewPrivs = 38`, in `launch_linux.go` | `syscall` lacks them; this keeps x/sys out |
| `Capture.TTL` 0 | Allowed: it disables the TTL. `withDefaults` fills `Capture` only when the whole struct is zero; any other zero field is refused | §7.3; Codex note on `StopGrace=0` |
| Cgroup (P-21) | `DelegatedCgroup` requires `mount+path` writable, `cgroup.kill` present, and `<parent>/cgroup.procs` writable. `Run` creates the cgroup; the reaper uses it and removes it. If the launcher start fails with `EACCES`, `EBUSY` or `EINVAL` on `CgroupFD`, the reaper retries once without it and reports `cgroup: false` | Mis-detected delegation must not fail captures |
| PID-target exit poll (P-20) | `capture.Alive(p, pid, start)`, never `os.FindProcess` + `Signal(0)` | No pidfd per poll, and reuse is seen |
| Phases | `status` is the §4.0 state. `phase` is one of: `lease`, `recovery`, `retention`, `staging` or `launching` (preparing); `attaching`, `profiling` or `finishing` (running); `validating`, `linking` or `manifest` (committing); `deriving` (`done` with `derived: pending`) | §4.0 shows `phase` without values |
| `eta`, `elapsed` | Go duration strings rounded to 100 ms. `eta` is the expected remaining time: the effective duration + 5 s | — |
| Compare views | For each mode, the new profile's default view, when the baseline has a view of that name; otherwise a note. Each entry is the `diff` tool's own result for `limit = CompareRows`, plus `next`, which is the same call without `limit` | "the first `CompareRows` rows of the existing diff tool" |
| Recompute recipe | `{"steps": [ {tool, arguments, bind?} … ], "baseline": "present" \| "<id> was removed by retention" \| "none"}`. The baseline's `open` steps come first. Each `open` binds its handle (`"bind": "base_cpu"`), and `diff` arguments use `"${base_cpu}"`. A missing baseline omits its steps and the diff | The Codex #418 carries |
| Handles in `capture_list` | From an atomic snapshot (path → handle) that the store publishes under the engine | `capture_list` never takes the engine |
| tools/list order (P-25) | The nine M2 tools, then `capture`, `capture_wait`, `capture_cancel` and `capture_list`, then `page_*` (Live, test-only) | — |
| `owner_mismatch` (P-29) | Synchronous once `.owner` has been read (by maintenance or by an earlier job); otherwise async, from `openCaptureDir` | Admission does no I/O |
| `cwd` containment (P-28) | Under a `--source` root, judged lexically against `s.src.Dirs()` after `EvalSymlinks` of `cwd`. Under the profiles root, `s.rel` | `sourceroot.Roots` exposes only `Dirs()` |
| Publication pins (P-22) | stdio retention does not read `s.live`. Serve protects publications through `captureProtect`, by digest | A publication holds its profile in memory, not a file identity |
| Temporaries (P-9) | Intent-rewrite and manifest temporaries are `.<final>.tmp-<token[:16]>` (O_EXCL). `.seq.tmp-<hex>` is written only under the lease. Recovery removes token-named temporaries and, under the lease, `.seq.tmp-*`, each with a log line | Otherwise a crash leaves files no rule may remove |
| Writer queue (P-18, P-19) | An enqueue into an empty writer queue always fits. A `Control` tool runs inline only after initialize has completed; before that it queues | One large escaped answer is not a dead client |

## Seams for serve (leave them; do not build serve)

- `engine` is a one-slot `chan struct{}` with `acquire(ctx)` and `release()`, the serve design's §D2.4 "engine slot". `tools()` wraps every handler except the four capture tools, including the `page_*` tools (P-7).
- The registry's active slot is an `atomic.Pointer[job]`, and the registry lives on `Server`. A `job` holds no session or transport reference.
- `Config.Capture.Owner` and `Config.Capture.Dir` (serve passes `"serve"` and `captures-serve`).
- `mcpstdio.Tool.Control`, which serve's control class reads.
- `Server.capturePublish func(j *job, r *captureResult) error` and `Server.captureProtect func() []fileKey`, both nil in stdio.
- `capture.RunInternal()`, which every binary embedding `perfmcp` calls first in `main`.

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/capture/{doc.go,errors.go,caps.go,verify.go,selfcheck.go,types.go,verify_linux.go,supported_linux.go,capture_other.go}` | error codes, `vfs_cap_data`, the pure checkers, the binary/shim/self checks, the platform split | 1 |
| `internal/capture/argv.go`, `capture_contract_test.go` (root) | settings, the flag list, argv, the readiness format, the contract test | 2 |
| `internal/capture/{target_linux.go,command.go,command_linux.go,main_test.go}` | preflight, recheck, start time, build-id, GPU maps; command resolution and env; the test `TestMain` | 3 |
| `internal/capture/capturetest/{capturetest.go,fakeperfagent/main.go,capworkload/main.go}` | test binaries and helpers | 4 |
| `internal/capture/lock_linux.go` | the captures `flock` | 5 |
| `internal/capture/{internal_linux.go,reaper_linux.go,exec_linux.go,launch_linux.go,cgroup_linux.go}` | internal modes, reaper, launcher, cgroup | 6 |
| `internal/capture/run_linux.go` | one capture, server side | 7 |
| `internal/mcpstdio/{server.go,protocol.go,writer.go}` | reader, queue, control, writer, drain, progress | 8 |
| `internal/perfmcp/{limits.go,config.go,engine.go,server.go,live.go,pins.go,store.go,tool_open.go,tool_diff.go,output.go}`, `cmd/perf-agent-mcp/{main.go,netscan_test.go}` | limits, config and start-up verification; the engine; the extractions; the scan rules | 9 |
| `internal/perfmcp/{capture_layout.go,capture_manifest.go,capture_wire.go,capture_decision.go,capture_commit.go,capture_recovery.go}` | layout, `.seq`, `.owner`, manifests, `wireError`, the decision word, intent/commit, rollback, recovery | 10 |
| `internal/perfmcp/capture_retention.go` | retention | 11 |
| `internal/perfmcp/{capture_registry.go,capture_job.go}` | registry, lookup pool, windows; `job` and `historical` declared | 12 |
| `internal/perfmcp/{capture_job.go,capture_run.go,capture_compare.go}` | the state machine, the cancel relay, derivations, recompute | 13 |
| `internal/perfmcp/{tool_capture.go,tool_capture_list.go}`, `skills/perf-agent-mcp/SKILL.md`, `cmd/perf-agent-mcp/main_test.go` | the four tools; the skill's loop lines; the CLI tool list | 14 |
| `cmd/perf-agent-mcp/{main.go,main_test.go}` | flags, routing, signal paths, the dead client | 15 |
| `internal/perfmcp/{capture_process_test.go,procserver_test.go}` | multi-process and crash tests | 16 |
| `internal/perfmcp/guide.md`, `guide_test.go`, `skills/perf-agent-mcp/SKILL.md`, `docs/mcp.md`, `CHANGELOG.md` | docs | 17 |
| `internal/perfmcp/capture_real_linux_test.go` | the gated privileged capture | 18 |

---

### Task 1: Error codes, file capabilities, the verifiers and the platform split

**Files:**
- Create in `internal/capture/`:
  - `doc.go`, `errors.go`, `caps.go`, `verify.go`, `selfcheck.go`, `types.go`;
  - `verify_linux.go`, `supported_linux.go`, `capture_other.go`;
  - `caps_test.go`, `verify_test.go`, `verify_linux_test.go`, `selfcheck_test.go`.

  The cap fixtures are Go byte literals in the tests; there is no binary testdata.

**Interfaces:**
- Consumes: nothing.
- Produces:
  - **`errors.go`.**
    - `type Code string`, with one constant per §4.4 row (`CodeDisabled` … `CodeStuckChild`, `CodeCancelled`), each the spec's exact string.
    - `var DefaultRetryable = map[Code]bool{…}`, from §4.4's column.
    - `type Error struct { Code Code; Message, Next string; Retryable bool; RetryAfter time.Duration; PerfAgentExit, StderrTail, Changed string; Survivors []Survivor }`, with `Error() string`.
    - `func Errorf(code Code, next, format string, args ...any) *Error`, which takes `Retryable` from `DefaultRetryable`.
    - `type Survivor struct { PID int; Comm string; StartTime uint64; State string }`.
  - **`caps.go`** (pure).
    - `type Capability uint`: `CapDACOverride` 1, `CapDACReadSearch` 2, `CapFowner` 3, `CapSetuid` 7, `CapSysModule` 16, `CapSysPtrace` 19, `CapSysAdmin` 21, `CapSyslog` 34, `CapPerfmon` 38, `CapBPF` 39, `CapCheckpointRestore` 40.
    - `type CapSet uint64`, with `Has` and `Names() []string` (`"cap_bpf"` …, in bit order).
    - `func ParseFileCaps(b []byte) (CapSet, error)`: revision 2 or 3; `rootid` 0 for revision 3; the effective flag set; the inheritable set empty; the exact length.
    - `func checkCaps(c CapSet) (warnings []string, err error)`:
      - required: `cap_bpf`, `cap_perfmon`, `cap_sys_ptrace` and `cap_checkpoint_restore`;
      - also allowed: `cap_syslog`, `cap_sys_admin` and `cap_dac_read_search` (with a warning);
      - any other capability is refused by name;
      - a missing one quotes `const SetcapLine = "setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep <path>"`.
  - **`types.go`.**
    - `type Identity struct { Dev, Ino uint64; Size int64; Mtime, Ctime int64; Xattr [32]byte }`, where `Xattr` is the SHA-256 of the raw xattr (zero when absent).
    - `type Binary struct { Path string; ID Identity; Caps CapSet; Version, Revision string; Warnings []string }`.
    - `type Shim struct { Path string; Dev, Ino uint64 }`.
    - Later tasks add their types to this file.
  - **`verify.go`** (pure, so testable without file capabilities, PLAN-R6).
    - `func checkBuildInfo(path string) (version, revision string, err error)`: `debug/buildinfo`. The main module must be `github.com/dpsoft/perf-agent`, with the main package path equal to it.
    - `func checkELF(path string) []string`: a warning when a `DT_NEEDED` names `*blazesym*`.
    - `func checkIdentity(was, now Identity) *Error`: `binary_changed`, "perf-agent changed since start-up; restart perf-agent-mcp".
  - **`verify_linux.go`.**
    - `func VerifyBinary(path string) (Binary, error)` runs §5.1's rows in production order:
      1. the path is absolute, and is resolved with `EvalSymlinks` once;
      2. `checkStat`: regular, executable, not setuid or setgid, `mode&022 == 0`, owner root or `os.Getuid()`;
      3. `Statfs` without `MS_NOSUID|MS_NOEXEC`, else "copy it outside /tmp, to a private directory you own (e.g. under ~), and setcap it there";
      4. the `security.capability` xattr, then `ParseFileCaps` and `checkCaps`;
      5. `checkBuildInfo`;
      6. `checkELF`.
    - `func IdentityOf(path string) (Identity, error)`.
    - `func (b Binary) Unchanged() *Error`.
    - `func VerifyShim(path string) (Shim, error)`: absolute, regular, not group- or other-writable.
    - `func ReadSelfStatus() ([]byte, error)`.
  - **`selfcheck.go`** (pure). `func CheckSelf(status []byte) error` refuses:
    - `Uid:` effective 0;
    - a non-zero `CapEff:` or `CapPrm:`;
    - `NoNewPrivs: 1`, with "this server inherited no_new_privs (a sandbox?); perf-agent's file capabilities cannot apply" (P-24).
  - **The platform split.** `supported_linux.go` holds `const Supported = true`. `capture_other.go` holds `const Supported = false` and stubs `VerifyBinary`, `IdentityOf`, `(Binary).Unchanged`, `VerifyShim` and `ReadSelfStatus`.

- [ ] **Step 0: Check the base** (P-12: the branch and the docs commit exist already)

Run: `git log --oneline -1 && go test ./cmd/perf-agent-mcp -run TestRunRegistersNoPageTools`
Expected: HEAD is the docs commit, and the test passes: stdio has nine tools and no `page_*`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseFileCapsAcceptsV2AndV3RootID0(t *testing.T)    // v2 +ep {bpf,perfmon,sys_ptrace,checkpoint_restore}; v3 rootid 0 → same CapSet; Names() in bit order
func TestParseFileCapsRefuses(t *testing.T)                 // v3 rootid 1000; no effective; inheritable ≠ 0; 19 bytes; 24-byte v2; bad magic → each error names the rule
func TestCheckCapsPolicy(t *testing.T)                      // missing cap_checkpoint_restore → names it and quotes SetcapLine; +cap_dac_override refused by name; +cap_syslog, +cap_sys_admin ok; +cap_dac_read_search ok with one warning
func TestCheckBuildInfo(t *testing.T)                       // PLAN-R6: the test binary → refused "not perf-agent" (main path ends .test); a go-built program with module example.com/x → refused naming it; a go-built program whose go.mod says module github.com/dpsoft/perf-agent with main at the root → version and revision returned
func TestCheckELFWarnsOnSharedBlazesym(t *testing.T)        // the test binary → no warning (DT_NEEDED has no blazesym)
func TestCheckIdentity(t *testing.T)                        // size or mtime or xattr digest differs → binary_changed; equal → nil
func TestVerifyBinaryStatRules(t *testing.T)                // (linux) relative refused; group-writable refused; setuid refused; directory refused; a symlink resolves and Path is the target; a plain 0755 file → refused for missing capabilities (the order reaches the xattr step)
func TestVerifyBinaryRefusesNosuidMount(t *testing.T)       // (linux) a 0755 file in os.TempDir(); skip unless /tmp is MS_NOSUID; error contains "copy it outside /tmp"
func TestCheckSelf(t *testing.T)                            // fixtures: Uid 0; CapEff 0000000000000400; CapPrm ≠ 0; NoNewPrivs 1 → each refused with its reason; uid 1000 with all zero and NoNewPrivs 0 → nil
func TestVerifyShim(t *testing.T)                           // (linux) relative refused; other-writable refused; 0644 regular ok with Dev/Ino
func TestEveryCodeHasADocumentedRetryable(t *testing.T)     // every Code constant (go/parser over errors.go) is a key; true exactly for busy, budget, target_changed, target_exited, pid_reused, start_timeout, finish_timeout, no_output, perf_agent_failed
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/`
Expected: FAIL (no package).

- [ ] **Step 3: Implement the files above.** Use `syscall.Getxattr` (`ENODATA` means no caps) and `syscall.Statfs`, both only in `_linux.go`, with no x/sys.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/ && go vet ./internal/capture/`, then the Global Constraints' green check (`GOOS=darwin go build ./internal/capture/` included).
Expected: PASS; the nosuid test passes or skips with its reason; darwin builds.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: error codes, file capabilities, the binary and self checks, and the Linux split"
```

### Task 2: The argv builder and the contract test

**Files:**
- Create: `internal/capture/argv.go`, `internal/capture/argv_test.go`, `capture_contract_test.go` (perf-agent root, `package main`, test-only)

**Interfaces:**
- Consumes: nothing.
- Produces (all common, untagged):
  - **Settings.**
    - `type Mode string`: `ModeCPU = "cpu"`, `ModeOffCPU = "offcpu"`, `ModeGPU = "gpu"`.
    - `type Settings struct { Modes []Mode; Duration time.Duration; SampleRate int; Unwind string; KernelStacks bool }`.
    - `func (s Settings) Canonical() Settings` orders the modes cpu, offcpu, gpu.
  - **The argv.**
    - `type ArgvSpec struct { Binary string; PID int; Settings Settings; Staging string; GPUShim string; CaptureID string }`.
    - `func BuildArgv(a ArgvSpec) []string` gives §5.2's flags in its order. Values are separate arguments. `--duration` is `Duration.String()`. The outputs are `Staging/{cpu.pb.gz,offcpu.pb.gz,gpu.pb.gz,gpu.execlog}`. The last pair is `--tag`, `perf_agent_mcp_capture=<CaptureID>`.
  - **The flag list.** `type Flag struct { Name string; Bool bool }` and `var Flags = []Flag{{"pid",false},{"duration",false},{"sample-rate",false},{"unwind",false},{"no-debuginfod",true},{"profile",true},{"profile-output",false},{"offcpu",true},{"offcpu-output",false},{"gpu",true},{"gpu-shim",false},{"gpu-output",false},{"gpu-executions-output",false},{"kernel-stacks",true},{"tag",false}}`.
  - **Files and environment.**
    - `func OutputFile(m Mode) string` and `const ExeclogFile = "gpu.execlog"`.
    - `func ExpectedFiles(modes []Mode) []string`.
    - `var PerfAgentEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}`.
  - **Readiness.** `const ReadinessFormat = "Collecting data for %v (Ctrl+C to stop early)..."` and `const ReadinessPrefix = "Collecting data for "`.

- [ ] **Step 1: Write the failing tests**

```go
func TestBuildArgvEveryCombination(t *testing.T)   // 7 mode subsets × kernel_stacks × 3 unwinds: every "--x" name ∈ Flags; has "--no-debuginfod" and "--duration"; none of §5.2's never-emitted list; each Bool flag is followed by a flag or the end; --profile iff cpu (likewise offcpu, gpu); gpu adds --gpu-shim SHIM and --gpu-executions-output S/gpu.execlog
func TestBuildArgvExact(t *testing.T)              // {"/b/perf-agent", 41234, cpu, 10s, 99, "auto", "/p/captures/s/.staging-ab", "s/001"} == {"/b/perf-agent","--pid","41234","--duration","10s","--sample-rate","99","--unwind","auto","--no-debuginfod","--profile","--profile-output","/p/captures/s/.staging-ab/cpu.pb.gz","--tag","perf_agent_mcp_capture=s/001"}
func TestCanonicalOrdersModes(t *testing.T)        // {gpu,cpu} → {cpu,gpu}
// capture_contract_test.go (root package main)
func TestCaptureContractFlagsExist(t *testing.T)    // each capture.Flags f: flag.Lookup(f.Name) != nil and IsBoolFlag() == f.Bool
func TestCaptureContractReadinessLine(t *testing.T) // main.go and telemetry.go each contain `"` + capture.ReadinessFormat + `\n"` printed with *flagDuration
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Argv|Canonical'`
Expected: FAIL.

- [ ] **Step 3: Implement `argv.go`.**

- [ ] **Step 4: Run them to verify they pass**

Run:

```bash
go test -race ./internal/capture/
CGO_CFLAGS=… CGO_LDFLAGS=… go test -race -run TestCaptureContract .
```

Then run the green check.
Expected: PASS. `git diff --stat ai-mcp-m2b -- main.go telemetry.go perfagent gpu cmd/flamegraph internal/profilemodel` is empty (P-11).

- [ ] **Step 5: Commit**

```bash
git add internal/capture/argv.go internal/capture/argv_test.go capture_contract_test.go
git commit -m "capture: the one perf-agent argv builder, pinned to perf-agent's flags by a contract test"
```

### Task 3: Targets, commands and the capture test `TestMain`

**Files:**
- Create: `internal/capture/target_linux.go`, `command.go`, `command_linux.go`, `main_test.go`, `target_linux_test.go`, `command_test.go`, `command_linux_test.go`
- Modify: `types.go`, `capture_other.go` (stubs)

**Interfaces:**
- Consumes: Task 1 `Error`, `Shim`, `ParseFileCaps`.
- Produces:
  - **The test `TestMain`** (P-5). `main_test.go` dispatches `PERFMCP_TEST_CHILD` ∈ {`sleep`, `nondumpable`, `setresgid`} to helper child bodies before `os.Exit(m.Run())`. Task 5 adds `hold-lock`; Task 6 puts `RunInternal` first and wraps the run in `capturetest.Main`.
  - **`types.go` additions.**
    - `type Proc struct { Root string; StartTimeHook func(pid int, start uint64) uint64; owner func(path string) (int, error) }` and `func DefaultProc() Proc` (Root `/proc`).
    - `type SelfID struct { UID, GID, PID int; Exclude func(pid int) bool }`.
    - `type TargetState struct { PID int; StartTime uint64; UIDs, GIDs [4]int; StatusOwner int; CapPrm uint64; Comm, Exe, BuildID string }`.
  - **`target_linux.go`.**
    - `func StartTime(p Proc, pid int) (uint64, error)`: `stat` field 22, read after the last `)`, then the hook.
    - `func Alive(p Proc, pid int, start uint64) bool`.
    - `func Preflight(p Proc, pid int, self SelfID) (TargetState, *Error)` applies §6.3, in this order:
      - `target_not_found`;
      - pid 1, the server itself or `Exclude` → `target_refused`;
      - a failing `readlink exe` → `target_refused`, "not dumpable, a kernel thread, or not yours";
      - the `status` owner ≠ uid → `target_refused`;
      - any of the four uids or gids ≠ the server's → `target_refused`, naming which;
      - `CapPrm` ≠ 0 → `target_refused`.

      It never stats the `/proc/<pid>` directory.
    - `func (was TargetState) Compare(now TargetState) *Error`: a changed start time is `pid_reused`; anything else is `target_changed`, with `Changed` naming what changed.
    - `func ReadBuildID(p Proc, pid int) string`: the hex of `.note.gnu.build-id`, or "".
    - `func CheckShimMapped(p Proc, pid int, s Shim) *Error`: `gpu_not_injected`, with §8's text.
  - **`command.go`** (common).
    - `func ResolveCommand(name, pathEnv string, allow []string) (string, *Error)`:
      - an absolute name is used as is;
      - a bare name is looked up in `pathEnv`'s absolute entries (regular and executable);
      - anything else is `invalid_argument`;
      - with `allow` set, the result must be one of its entries.

      No `exec.LookPath`.
    - `func WorkloadEnv(environ []string, shim string) []string`: §6.4's allow-list, plus `CUDA_INJECTION64_PATH`.
  - **`command_linux.go`.** `func CheckCommandFile(path string) *Error`: setuid, setgid or a `security.capability` xattr is `invalid_argument`.
  - **Stubs** for every Linux function above.

- [ ] **Step 1: Write the failing tests**

```go
func TestPreflightAcceptsAChildOfOurs(t *testing.T)          // PERFMCP_TEST_CHILD=sleep: ok; StartTime matches; Comm; BuildID non-empty
func TestPreflightRefuses(t *testing.T)                      // pid 1; os.Getpid(); Exclude true; pid 2 "kernel thread"; a reaped PID → target_not_found
func TestPreflightRefusesANonDumpableChild(t *testing.T)     // PERFMCP_TEST_CHILD=nondumpable (prctl via x/sys in the test) → target_refused mentioning dumpable
func TestPreflightNeverConsultsTheProcDirOwner(t *testing.T) // Proc.owner seam: dir 0 + status ours → accepted; dir ours + status 0 → refused; the dir path is never asked
func TestCompareNamesWhatChanged(t *testing.T)               // start → pid_reused; GIDs[1] → target_changed "gid"; equal → nil
func TestStartTimeHookFeedsReuse(t *testing.T)
func TestAliveSeesExitAndReuse(t *testing.T)                 // a child: Alive true; reaped → false; same pid, start+1 via hook → false
func TestCheckShimMapped(t *testing.T)                       // fake proc root maps with the shim's dev:ino → nil; another inode → gpu_not_injected "CUDA_INJECTION64_PATH"
func TestResolveCommand(t *testing.T)                        // "/bin/true" as is; "true" with "/usr/bin:/bin"; "./x", "bin/x" → invalid_argument; relative PATH entry skipped; allow mismatch → invalid_argument naming --capture-command
func TestCheckCommandFileRefusesSetuidAndCaps(t *testing.T)  // 04755 "setuid"; 02755 "setgid"; plain nil
func TestWorkloadEnvIsTheAllowList(t *testing.T)             // OTEL_X, DEBUGINFOD_URLS, GODEBUG, KUBERNETES_POD_NAME dropped; LC_ALL, HOME kept in order; shim last
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Preflight|Compare|StartTime|Alive|Shim|Resolve|CommandFile|WorkloadEnv'`
Expected: FAIL.

- [ ] **Step 3: Implement.** Build-ids come from `debug/elf` `Section(".note.gnu.build-id")`, after the 16-byte note header.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/`, then the green check.
Expected: PASS; darwin builds.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: the ptrace-like target preflight, its recheck, and command resolution"
```

### Task 4: Test binaries

**Files:**
- Create: `internal/capture/capturetest/capturetest.go`, `main_test.go`, `capturetest_test.go`, `capturetest/fakeperfagent/main.go`, `capturetest/capworkload/main.go`

**Interfaces:**
- Consumes: Task 2 `capture.Flags`, `ReadinessFormat`, `OutputFile`, `ExeclogFile`; `pmfixture.CPUBasic`, `GPUBasic`, `GPUBasicExeclog`.
- Produces:
  - **Imports (P-6).** Only `fakeperfagent/main.go` imports `internal/capture`; it is built by `go build`, never imported. `capturetest.go` does not import it, so `capture`'s in-package tests can use `capturetest`.
  - **Lifetime.** `func Main(m *testing.M) int` is the package-level lifetime owner. It creates the build directory under `os.TempDir()` (the fakes have no caps, so `/tmp` is fine), runs `m.Run()`, then removes the directory. `func Binaries(tb testing.TB) (fake, workload string)` builds both once (`sync.Once`) into that directory with `go build -buildvcs=false`, and fails `tb` when `Main` is not the package's `TestMain` driver.
  - **The fake.**
    - `type FakeMode string`: `FakeNormal`, `FakeNeverReady`, `FakeIgnoreSIGINT`, `FakeExit1Stderr` ("failed to set capabilities"), `FakeExit2`, `FakeExit127Loader` ("error while loading shared libraries: libblazesym_c.so"), `FakeNoFile`, `FakeHugeFile`, `FakeSymlinkOutput`, `FakeHardlinkOutput`, `FakeSpamStdout` (4 MiB per stream), `FakeForkGrandchild`, `FakeHangAfterSIGINT`, `FakeNoGPUSamples`.
    - `type FakeOptions struct { Mode FakeMode; ReadyAfter time.Duration; Record string }`. `Record` is where the fake writes its argv, environment, pid and pgid as JSON.
    - `func FakePerfAgent(tb testing.TB, o FakeOptions) string` returns `<tb.TempDir()>/fakeperfagent`: a hard link, or a copy, of the built fake. Beside it go `fake-mode.json` and `fixtures/` with `cpu.pb.gz` = `CPUBasic()`, `gpu.pb.gz` = `GPUBasic()` and `gpu.execlog` = `GPUBasicExeclog(tb)`. The fake copies the fixtures it needs.
    - The fake parses its argv with a `flag.FlagSet` built from `capture.Flags`, and exits 2 on an unknown flag. It prints the readiness line after `ReadyAfter`, waits for the duration or SIGINT, and writes its outputs.
  - **The workload.** `func Workload(tb testing.TB) string` returns the binary `capworkload` (P-26). Its argv modes:
    - `busy D`, `sleep D`, `exit-after D CODE`;
    - `fork-and-exit D`, `double-fork-setsid D` (prints the grandchild's PID);
    - `drop-dumpable-after A D`, `setresgid-after A D` (prints `skip` with no supplementary gid);
    - `ignore-sigterm D`, `flood-output D` (MiBs, including invalid UTF-8);
    - `report` (JSON `{nonewprivs, tty_nr, env}` from its own `/proc/self`);
    - `exec PATH ARGS…` (`syscall.Exec`).
  - **The gate.** `func SkipIfPrivileged(tb testing.TB)` skips when euid is 0 or CapEff/CapPrm are non-zero.

- [ ] **Step 1: Write the failing tests** (`main_test.go`: `os.Exit(capturetest.Main(m))`)

```go
func TestFakeRefusesUnknownFlags(t *testing.T)        // --bogus → exit 2
func TestFakeWritesTheRequestedOutputs(t *testing.T)  // a full cpu argv with --duration 200ms → first stdout line has prefix "Collecting data for 200ms"; cpu.pb.gz == CPUBasic(); exit 0
func TestFakeStopsOnSIGINT(t *testing.T)              // --duration 1m; SIGINT after readiness → exits < 1 s with the file
func TestFakeRecordsArgvEnvAndPgid(t *testing.T)
func TestFakeKeepsItsComm(t *testing.T)               // /proc/<pid>/comm == "fakeperfagent"
func TestWorkloadReportAndExecChain(t *testing.T)     // report → keys nonewprivs, tty_nr, env; exec <workload> report → the same JSON (the chain works)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/capturetest/...`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/capturetest/...`, then the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/capture/capturetest
git commit -m "capture: a fake perf-agent and a scripted workload for unprivileged tests"
```

### Task 5: The captures lock

**Files:**
- Create: `internal/capture/lock_linux.go`, `lock_linux_test.go`
- Modify: `types.go` (`Holder`), `main_test.go` (`hold-lock` child), `capture_other.go` (stubs)

**Interfaces:**
- Consumes: Task 1 `Error`; Task 3 `StartTime`.
- Produces:
  - `type Holder struct { ServerPID int; ServerStart uint64; ReaperPID int; ReaperStart uint64; CaptureID, Series string; Deadline time.Time }`, with JSON names `server_pid`, `server_start`, `reaper_pid`, `reaper_start`, `capture_id`, `series`, `deadline`.
  - `func TryLock(dir *os.Root, h Holder) (*Lease, *Error)`:
    - it opens `.lock` (`O_RDWR|O_CREATE`, 0600) and takes `syscall.Flock(LOCK_EX|LOCK_NB)`;
    - `EWOULDBLOCK` reads the holder and returns `busy`: "capture <id> by perf-agent-mcp pid <p> (reaper pid <r>), ends by <deadline UTC>". `RetryAfter` is at least 1 s. An unreadable holder is still `busy`;
    - success truncates the file and writes `h`.
  - `(*Lease).File() *os.File`, `Update(h Holder) error` and `Close() error`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTryLockIsExclusiveAcrossProcesses(t *testing.T)     // PERFMCP_TEST_CHILD=hold-lock DIR prints "held"; TryLock → busy naming its pid, capture id and deadline; RetryAfter ≥ 1 s
func TestTheLockSurvivesInAnInheritedDescriptor(t *testing.T) // pass l.File() to a child as ExtraFiles[0]; Close here; TryLock busy; kill the child by recorded PID → free
func TestTryLockRewritesTheHolder(t *testing.T)              // Update with ReaperPID → busy names it
func TestBusyWithAGarbledHolderStillSaysBusy(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run Lock`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/`, then the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: a cross-process captures lock that names its holder"
```

### Task 6: The reaper and the launcher (opus)

**Files:**
- Create: `internal/capture/internal_linux.go`, `reaper_linux.go`, `exec_linux.go`, `launch_linux.go`, `cgroup_linux.go`, `reaper_linux_test.go`, `sweep_test.go`, `cgroup_linux_test.go`
- Modify: `types.go` (the specs and the protocol types), `main_test.go`, `capture_other.go`

**Interfaces:**
- Consumes: Task 1 `Identity`, `IdentityOf`, `Survivor`; Task 2 `PerfAgentEnv`; Task 3 `StartTime`; Task 4 binaries.
- Produces:
  - **The test `TestMain`.** `main_test.go` becomes: `if code, ok := RunInternal(); ok { os.Exit(code) }`, then the `PERFMCP_TEST_CHILD` dispatch, then `os.Exit(capturetest.Main(m))`.
  - **Mode routing.**
    - `const InternalEnv = "PERF_AGENT_MCP_INTERNAL"`, common.
    - `func RunInternal() (code int, handled bool)`. It returns `handled` only when `InternalEnv` is `reaper` or `launcher` **and** fd 3 is open. The stub returns `(0, false)`.
  - **`types.go` additions.**
    - `type ReaperSpec struct { PerfAgent string; PerfAgentID Identity; Launch *LaunchSpec; Deadline, StopGrace time.Duration; Cgroup string; FaultPause string }`.
    - `type LaunchSpec struct { Path string; Args, Env []string; Dir string }`.
    - `type ReaperFiles struct { Lock, PerfOut, PerfErr, WorkOut, WorkErr *os.File }`.
    - `type Command struct { Op string; Argv []string; Dir string }`, where `Op` ∈ {`attach`, `stop`, `cancel`}.
    - `type Event struct { Op, Who string; PID, PGID int; StartTime uint64; Status int; Signal string; Cgroup bool; Survivors []Survivor; Cleanup, Err string }`, where `Op` ∈ {`launched`, `attached`, `exited`, `reaped`, `error`} and `Who` ∈ {`perf-agent`, `workload`}.
  - **`FaultPause`** is a test hook. Each value suspends **only the publication** of one event; EOF and deadline cleanup stay live:

    | Value | Effect |
    |---|---|
    | `after-perf-agent-fork` | holds back the `attached` event |
    | `after-launcher-fork` | holds back the `launched` event |
    | `no-subreaper` | skips `setChildSubreaper` |
    | `before-exit:<path>` | after cleanup, the reaper keeps the lock until `<path>` disappears (P-23) |
  - **The server-side handle** (`reaper_linux.go`; the `*exec.Cmd` stays behind an opaque `child` from `exec_linux.go`, P-14).
    - `func StartReaper(self string, spec ReaperSpec, f ReaperFiles) (*Reaper, error)`. It starts the reaper with env exactly `InternalEnv=reaper`, `Setsid`, no `Pdeathsig` and stdin `/dev/null`. The fds:

      | fd | Carries |
      |---|---|
      | 3 | the spec (a pipe) |
      | 4 | commands, read |
      | 5 | events, written |
      | 6 | the lock |
      | 7, 8 | perf-agent's stdout and stderr |
      | 9, 10 | the workload's stdout and stderr |

      A goroutine waits for the reaper process.
    - `(*Reaper).Send(c Command) error`, `Events() <-chan Event` and `PID() int`.
    - `(*Reaper).Kill()` (PLAN-R1): `kill(-pgid, SIGKILL)` for each recorded group, then close the command pipe. It **never** signals the reaper, and never blocks. `(*Reaper).Done() <-chan struct{}` closes when the reaper process exits.
  - **The reaper**, in order:
    1. `CloseOnExec` every inherited fd ≥ 4.
    2. `setChildSubreaper()`; a failure is noted for `Cleanup`.
    3. With `Launch`: start the launcher (`self`, `InternalEnv=launcher`, its spec on fd 3, `Setsid`, `Dir`, stdin `/dev/null`, fds 9 and 10). With `Cgroup`, add `CgroupFD`; on `EACCES`, `EBUSY` or `EINVAL`, retry once without it and set `Cgroup: false` (P-21). Publish `launched`.
    4. On `attach`: `IdentityOf(PerfAgent) == PerfAgentID`, else an `error` event. Start perf-agent with `Args` = `Argv`, `Dir`, `Env` = `PerfAgentEnv`, `Setpgid`, stdin `/dev/null` and fds 7 and 8. Publish `attached`.
    5. One `wait4(-1)` loop. Publish `exited` for each known child, then `Process.Release` it and close its pipe ends.
    6. `stop`: SIGINT to perf-agent's group. `cancel`: SIGINT, then SIGKILL after `StopGrace`.
    7. When perf-agent has exited, end the workload: `cgroup.kill`, or SIGTERM to its group, SIGKILL after `StopGrace`, and the sweep.
    8. EOF on fd 4 before `reaped`, or the absolute `Deadline`: SIGKILL both groups, `cgroup.kill`, and the sweep.
    9. Wait up to `StopGrace` for the groups, the cgroup and adopted descendants.
    10. Publish `reaped` with `Survivors` and `Cleanup` (`complete` or `incomplete: <reason>`). Remove the cgroup. Honour `before-exit`. Close the lock fd **last**, and exit 0.
  - `func sweep(children func() []int, kill func(pid int, sig syscall.Signal) error, alive func(pid int) bool, grace time.Duration, now func() time.Time) []int` returns the survivors.
  - **The launcher** (`launch_linux.go`). `launch(spec LaunchSpec) int`: `runtime.LockOSThread()`, then `noNewPrivs()`, then `syscall.Exec`. It holds `prSetChildSubreaper = 36`, `prSetNoNewPrivs = 38`, `noNewPrivs()` and `setChildSubreaper()`.
  - **The cgroup** (`cgroup_linux.go`).
    - `func DelegatedCgroup(selfCgroup []byte, mount string) (parent, why string)` requires the `0::` line, `mount+path` writable, `cgroup.kill` present, and `cgroup.procs` writable (P-21).
    - `func CreateCgroup(parent, name string) (string, error)`, `func KillCgroup(path string) error` and `func RemoveCgroup(path string) error`.
  - **`exec_linux.go`** holds exactly `exec.Command(spec.PerfAgent, …)` and `exec.Command(self, …)` (inside `selfCommand(self, mode string)`), and the opaque `child` type.

- [ ] **Step 1: Write the failing tests** (`StartReaper` driven directly; `SkipIfPrivileged`)

```go
func TestReaperRunsPerfAgentWithTheExactEnvironment(t *testing.T) // Record: env == PerfAgentEnv; argv == Argv; cwd == Dir; pgid == pid; events attached, exited 0, reaped "complete"
func TestReaperIsInItsOwnSession(t *testing.T)
func TestTheLauncherSetsNoNewPrivsAfterExec(t *testing.T)         // PLAN-R6: capworkload report → nonewprivs 1, tty_nr 0, env == LaunchSpec.Env; exec chain capworkload exec <capworkload> report → nonewprivs still 1
func TestReaperKillsOnEOF(t *testing.T)                           // never-ready (start-up), normal 1m (profiling), hang-after-SIGINT after stop (finalisation): close the command pipe → group gone within StopGrace; reaper exits
func TestReaperKillsAtTheAbsoluteDeadline(t *testing.T)           // Deadline 500ms → group gone by Deadline + StopGrace
func TestReaperCancelIsSIGINTThenSIGKILL(t *testing.T)            // ignore-SIGINT → exited Signal "killed" ≈ StopGrace
func TestReaperEndsTheWorkloadAfterPerfAgent(t *testing.T)
func TestReaperSweepsSetsidGrandchildren(t *testing.T)            // no cgroup; double-fork-setsid → grandchild gone after reaped; FaultPause "no-subreaper" → Cleanup names "process group only"
func TestKillLeavesTheReaperToCleanUp(t *testing.T)               // PLAN-R1: double-fork-setsid workload + live fake; Reaper.Kill() → the reaper PID is still alive right after; the grandchild is gone and reaped arrives within StopGrace + 500 ms; then the reaper exits
func TestKillDuringAnUnreportedFork(t *testing.T)                 // PLAN-R1: FaultPause after-perf-agent-fork (and after-launcher-fork): Kill → children gone within StopGrace; no event was ever published for them
func TestReaperForkReportGapStillKillsOnEOF(t *testing.T)         // the same pauses with the pipe closed instead of Kill
func TestReaperKillsTheCgroup(t *testing.T)                       // skip unless DelegatedCgroup ok; grandchild gone via cgroup.kill; directory removed
func TestACgroupStartFailureFallsBack(t *testing.T)               // Cgroup = a path not writable → launched Cgroup false; the capture still runs
func TestTheLockIsReleasedOnlyAfterReaping(t *testing.T)          // busy until the reaper exits; FaultPause before-exit:<f> holds it until f is removed
func TestSweepReportsSurvivors(t *testing.T)                      // alive stays true for 42 → [42]
func TestDelegatedCgroupParsing(t *testing.T)                     // writable dir + cgroup.kill + cgroup.procs → parent; no cgroup.kill; cgroup.procs read-only → why; v1 lines → why
func TestRunInternalIgnoresAnUnsetMode(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Reaper|Launch|Kill|Sweep|Cgroup|RunInternal'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/capture/`, then the green check.
Expected: PASS three times. `ps -eo pid,comm | grep -E '^ *[0-9]+ (fakeperfagent|capworkload)$'` prints nothing (never `pkill -f`).

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: a detached reaper that owns both children, and a no_new_privs launcher"
```

### Task 7: One capture, server side (opus)

**Files:**
- Create: `internal/capture/run_linux.go`, `run_linux_test.go`
- Modify: `types.go` (`Bounds`, `RunSpec`, `Outcome`, `Tail`), `capture_other.go` (`Start` and the `Run` method stubs)

**Interfaces:**
- Consumes: Tasks 1–6.
- Produces:
  - **The spec.**
    - `type Bounds struct { StartTimeout, FinishTimeout, StopGrace time.Duration; MaxOutputTail int; MaxProfileBytes, MaxExeclogBytes int64; Recheck, Poll time.Duration }`, with `Recheck` 1 s and `Poll` 250 ms.
    - `type RunSpec struct { Self string; Binary Binary; Proc Proc; SelfID SelfID; PID int; Launch *LaunchSpec; Settings Settings; Staging *os.Root; StagingPath string; CaptureID string; GPU *Shim; Bounds Bounds; Lock *os.File; Cgroup string; Log *slog.Logger; FaultPause string; Events func(<-chan Event) <-chan Event }`. `Events` is a test seam that may rewrite the event stream.
  - **The handle.**
    - `func Start(spec RunSpec) (*Run, *Error)` and `(*Run).Phase() string`.
    - `(*Run).Cancel()`: idempotent and safe from any goroutine; it sends `cancel`.
    - `(*Run).Kill()` is `Reaper.Kill` (PLAN-R1).
    - `(*Run).Wait() Outcome` blocks until `reaped` arrives, or until the reaper exits.
  - **The outcome.** `type Tail struct { Stdout, Stderr string }` and `type Outcome struct { Err *Error; StoppedBy string; Target TargetState; Launched bool; CommandPath string; ArgCount int; Started time.Time; AttachLatency, Profiled time.Duration; PerfAgentExit int; StderrTail string; Workload Tail; Survivors []Survivor; Cleanup string; Notes []string; Argv []string }`.
  - **The flow:**
    1. Create the cgroup when `DelegatedCgroup` allows it, and start the reaper.
    2. A launch: `Preflight` the workload PID on `launched`. A PID target: `Preflight` it, then `CheckShimMapped` for gpu.
    3. `BuildArgv`, then `attach`.
    4. Readers fill the 16 KiB rings and slog debug. The first stdout line carrying `ReadinessPrefix` fixes `AttachLatency`.
    5. No readiness within `StartTimeout` is `start_timeout`. An exit before readiness is `target_exited`, with the tail and the PID hint (Review Focus 2).
    6. `Compare` the target at readiness, every `Recheck`, and at the end.
    7. Every `Poll`:
       - the staging sizes (over a limit: `Kill`, then `too_large`);
       - a PID target through `Alive(Proc, pid, start)` (P-20); gone means `stop`, with `target_exit`.
    8. A workload `exited` event means `stop`, with `command_exit`.
    9. Stop is readiness + duration, or the early stop. A perf-agent that has not exited within `FinishTimeout` of it is `Kill`ed, with `finish_timeout`.
    10. §5.4's exit mapping (2, 1 with its stems, 126/127 and the loader text).
    11. Survivors in `reaped` are `stuck_child`.
    12. `StoppedBy` is `duration`, `target_exit`, `command_exit` or `cancel`.

    The size kill, the finish timeout and the start timeout all use `Kill`, so the reaper's sweep still runs.
  - **The notes:** the launch attach-latency note, "only pid <N> was profiled, not processes it started", and the "descendants may survive" note when `Cleanup` says so.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunCapturesAPIDTarget(t *testing.T)                        // sleep 1m + normal ReadyAfter 100ms, duration 500ms → ok, "duration", AttachLatency ≥ 100ms, Profiled ≈ 500ms ±250, cpu.pb.gz in staging
func TestRunLaunchesACommand(t *testing.T)
func TestRunStopsWhenTheCommandExits(t *testing.T)                   // exit-after 300ms → "command_exit", Profiled < 1s
func TestRunStopsWhenThePIDExits(t *testing.T)                       // → "target_exit" (via Alive)
func TestRunTargetExitedBeforeReadiness(t *testing.T)
func TestRunForkAndExitIsTargetExitedWithAPIDHint(t *testing.T)      // Review Focus 2: Next contains "capture it with pid"
func TestRunTimeouts(t *testing.T)                                   // start_timeout "StartTimeout=300ms"; finish_timeout; both: the fake's group gone after Wait
func TestRunKillsKeepTheSweep(t *testing.T)                          // PLAN-R1: double-fork-setsid workload; for each of Run.Kill, the size kill (huge-file), the finish timeout: the reported grandchild PID is gone by Wait's return; Cleanup "complete"
func TestRunExitCodes(t *testing.T)                                  // exit-2, exit-1-stderr (Retryable false, Next "setcap"), exit-127-loader ("link blazesym statically")
func TestRunSizeKill(t *testing.T)                                   // too_large within 1 s of passing MaxProfileBytes
func TestRunCancel(t *testing.T)                                     // Cancel from another goroutine mid-profiling → "cancel", Err cancelled, gone ≈ StopGrace
func TestRunRechecksTheTarget(t *testing.T)                          // drop-dumpable-after → target_changed "dumpable"; setresgid-after → "gid" (skip on "skip")
func TestRunStartTimeHookIsPIDReuse(t *testing.T)
func TestRunTransitionAndRevertWithinOneIntervalIsMissed(t *testing.T) // the documented limit: Err nil
func TestRunWorkloadFloodNeverStallsAndTailIsValidUTF8(t *testing.T)  // Review Focus 3
func TestRunSpamStdoutStaysInTheRings(t *testing.T)
func TestRunGPUNeedsTheShimMapped(t *testing.T)                      // gpu_not_injected before perf-agent starts (no Record file)
func TestRunSurvivorsAreStuckChild(t *testing.T)                     // Events seam injects reaped Survivors [{42}]
func TestRunKillIsNonBlocking(t *testing.T)                          // Kill < 10 ms
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run TestRun`
Expected: FAIL.

- [ ] **Step 3: Implement `run_linux.go`.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=2 ./internal/capture/`, then the green check.
Expected: PASS, with no leftover `fakeperfagent` or `capworkload`.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: Run supervises one capture: readiness, bounds, early stop, rechecks and exit mapping"
```

### Task 8: A stdio transport that never waits

**Files:**
- Create: `internal/mcpstdio/writer.go`, `control_test.go`
- Modify: `internal/mcpstdio/server.go`, `protocol.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - **API.**
    - `Tool.Control bool` (`json:"-"`).
    - `Options` gains:

      | Field | Zero means |
      |---|---|
      | `MaxQueued` | 8 |
      | `MaxOutMessages` | 64 |
      | `MaxOutBytes` | 4 × `MaxToolErrorBytes`, or 4 MiB |
      | `DrainTimeout` | 1 s |
      | `OnClose func(CloseReason)` | no callback |
    - `type CloseReason int`: `CloseEOF`, `CloseClientGone`, `CloseContext`.
    - `var ErrClientGone = errors.New("mcpstdio: the client stopped reading its output")`.
    - `const codeServerBusy = -32000`.
    - `func Progress(ctx context.Context, progress, total float64, message string) bool`.
  - **The reader** decodes `method`, `id` and `params` itself.
    - `notifications/cancelled` (`params.requestId`, matched by compact JSON bytes): it cancels the running handler, whose response is then dropped. A queued request is dropped with no response; an unknown id does nothing.
    - A `Control` tool runs inline **once initialize has completed** (an atomic flag the dispatcher sets); before that it queues (P-19).
    - Anything else joins a FIFO of `MaxQueued`. One more gets `-32000`, "server busy: 8 requests queued; retry", at once.
  - **The dispatcher** handles one request at a time, in order.
  - **The writer** is the only writer of `w`. Its queue is bounded by `MaxOutMessages` and `MaxOutBytes`, but **an enqueue into an empty queue always fits** (P-18). An enqueue that does not fit means a dead client: `OnClose(CloseClientGone)`, every handler context cancelled, reading stopped, and `Serve` returns `ErrClientGone` at once, without waiting for the blocked write.
  - **EOF:**
    1. `OnClose(CloseEOF)`;
    2. the handler contexts are cancelled;
    3. the requests already queued are still answered, in order;
    4. the writer drains for ≤ `DrainTimeout`;
    5. `Serve` returns nil, or `ErrClientGone` when the drain ran out.
  - **ctx done** (P-2): `OnClose(CloseContext)`, the handlers are cancelled, the writer drains the messages already enqueued for ≤ `DrainTimeout`, and `Serve` returns nil (or `ErrClientGone`).

- [ ] **Step 1: Write the failing tests** (every existing `server_test.go` test is unchanged)

```go
func TestCancelledEndsTheRunningHandlerWithNoResponse(t *testing.T)
func TestCancelForAnotherIDDoesNothing(t *testing.T)
func TestCancelOfAQueuedRequestDropsIt(t *testing.T)
func TestEOFCancelsTheHandlerAndAnswersTheQueue(t *testing.T)     // 1 blocking + 3 queued, EOF → OnClose(CloseEOF) once; ids 2..4 answered in order; nil
func TestPipelinedRequestsBeforeEOFAreAllAnswered(t *testing.T)   // P-1: initialize + 3 tools/call through strings.NewReader → 4 responses, none isError
func TestOverloadIsImmediateAndOrdered(t *testing.T)              // 20 requests + a cancel during a blocking handler → 8 answered in order, 12 × -32000 before the handler ends; every line one JSON-RPC message
func TestControlToolsAnswerAheadOfTheQueue(t *testing.T)          // < 100 ms during a blocking handler
func TestAControlToolBeforeInitializeQueues(t *testing.T)         // P-19: initialize pipelined with a Control call → both answered, the control one after initialize, not "send initialize first"
func TestABigAnswerIntoAnEmptyQueueFits(t *testing.T)             // P-18: MaxOutBytes 1 KiB, one 64 KiB result → delivered, no OnClose
func TestABlockedStdoutMeansADeadClient(t *testing.T)             // MaxOutMessages 4, w blocks → OnClose(CloseClientGone); ErrClientGone ≤ 1 s
func TestEOFAndCancelAreSeenWhileStdoutIsBlocked(t *testing.T)
func TestContextDoneDrainsThenReturns(t *testing.T)               // P-2: a response enqueued just before cancel is written; Serve returns ≤ DrainTimeout + 100 ms with w blocked
func TestProgressGoesThroughTheWriter(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcpstdio/`
Expected: the new tests FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=5 ./internal/mcpstdio/`, then the green check.
Expected: PASS. Every perfmcp and cmd test passes unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpstdio
git commit -m "mcpstdio: a reader that never waits, control messages, and one bounded writer"
```

### Task 9: Limits, config, start-up verification, the engine, extractions and the scan rules

**Files:**
- Create: `internal/perfmcp/engine.go`, `engine_test.go`, `capture_limits_test.go`
- Modify:
  - `internal/perfmcp/`: `limits.go`, `config.go`, `server.go`, `live.go`, `pins.go`, `store.go` (the ownership comments and `openNames`), `tool_open.go`, `tool_diff.go`, `output.go`;
  - `cmd/perf-agent-mcp/`: `main.go` (the `checkLimits` exception), `netscan_test.go` (P-3).

**Interfaces:**
- Consumes: Task 1 `capture.Binary`, `VerifyBinary`, `VerifyShim`, `CheckSelf`, `ReadSelfStatus`; Task 3 `CheckCommandFile`; Task 8 `Options`.
- Produces:
  - **Limits.** `Limits.Capture CaptureLimits`, defined as `{ MaxDuration, StartTimeout, FinishTimeout, TTL, StopGrace, WindowDuration time.Duration; MaxSampleRate, KeepPerSeries, MaxKeptPerSeries, CompareRows, MaxJobs, MaxJobBytes, WindowCaptures, MaxOutputTail int; MaxProfileBytes, MaxExeclogBytes, MaxStoredBytes, MinFreeBytes int64 }`.
    - The defaults are §6.1's.
    - `withDefaults` fills `Capture` only when the whole struct is zero. Any other zero field is refused, except `TTL`.
    - `validate` refuses values over the ceilings: `MaxDuration` 5 min, `MaxSampleRate` 10 000, `StartTimeout` 5 min, `FinishTimeout` 10 min, `StopGrace` 30 s, `MaxOutputTail` 64 KiB, `CompareRows` ≤ `MaxDiffRows`.
  - **Config.** `Config.Capture CaptureConfig`, defined as `{ PerfAgent, Dir, GPUShim, Owner string; Commands []string; Verify func(path string) (capture.Binary, error) }`.
  - **Start-up verification in `New`** (P-17). With `PerfAgent` set, `New` runs `Verify` (`capture.VerifyBinary` when nil), plus `VerifyShim` and `CheckCommandFile` for each command, and a failure is a `New` error. `CheckSelf(ReadSelfStatus())` failing is not: capture is then disabled, and `s.captureDisabled` keeps the reason. With `PerfAgent` empty, the reason is §4.1's DISABLED sentence. On non-Linux the reason is "capture needs Linux".
  - **The engine.**
    - `type engine struct{ slot chan struct{} }`, `func newEngine() *engine`.
    - `func (e *engine) acquire(ctx context.Context) error` first tries the slot **without blocking**; only when it is held does it `select` on the slot and `ctx.Done()` (P-1).
    - `release()`.
    - `func (s *Server) engineHandler(h mcpstdio.Handler) mcpstdio.Handler` wraps every tool, the `page_*` tools when Live included (P-7).
    - `commit`, `refreshHistory`, `ensureLive`, and every mutation of pins, the store or attempts run only under the engine.
    - The ownership comments in `server.go`, `pins.go`, `store.go` and `live.go` change from "request goroutine" to "engine holder". HTTP handlers still read only `snap`.
  - **`New` passes** `MaxOutBytes` = 4 × `MaxResponseBytes` and `DrainTimeout` = 800 ms to mcpstdio.
  - **Extractions.**
    - `func (s *Server) openResultFor(name, execlog string) (openResult, error)`;
    - `func (s *Server) diffPage(q diffQuery, limit int, cursor string) (diffResult, error)`;
    - `writeTemp(root *os.Root, kind, dir, base string, b []byte, perm fs.FileMode)`;
    - `func (s *Server) linkNoReplace(root *os.Root, kind, tmp, name string) error`, whose error wraps `fs.ErrExist` when the name is taken;
    - `func (s *Server) publishNew(root *os.Root, kind, dir, base string, b []byte, perm fs.FileMode) (string, error)`.

    `kind` names the root in messages (P-10). `writePage`'s messages stay byte-identical.
  - **The store snapshot.** `store.published atomic.Pointer[map[string]profilemodel.Handle]` and `func (s *store) openNames() map[string]profilemodel.Handle`.
  - **The scan** (P-3, PLAN-R4; `netscan_test.go`):
    - `execFiles = []string{"internal/sourceroot/git.go", "internal/capture/exec_linux.go"}`, each checked to import os/exec;
    - in `exec_linux.go`, exactly two `exec.Command` calls, whose first arguments are the selector `PerfAgent` and the identifier `self`. No `exec.LookPath`, no `exec.CommandContext`, and no `"sh"`, `"bash"`, `"sudo"` or `"pkexec"` literals;
    - `launchFile = "internal/capture/launch_linux.go"` may use `syscall.Exec`, and `syscall.RawSyscall` only as `(syscall.SYS_PRCTL, prSetNoNewPrivs|prSetChildSubreaper, …)`;
    - `allowedFiles` becomes per-file sets, and the fixture tests are updated to match.

- [ ] **Step 1: Write the failing tests**

```go
func TestCaptureLimitsDefaults(t *testing.T)                 // §6.1 field by field; MaxProfileBytes == Model.MaxRawBytes
func TestCaptureLimitsCeilings(t *testing.T)                 // MaxDuration 6m names "Capture.MaxDuration" and "5m0s"; MaxSampleRate 10001; CompareRows MaxDiffRows+1; TTL 0 ok; StopGrace 0 with the rest set → refused (no fill)
func TestNewVerifiesThePerfAgentBinary(t *testing.T)         // PerfAgent = a plain file, Verify nil → New error naming the missing capabilities; Verify seam returning a Binary → New ok
func TestCaptureIsDisabledForAPrivilegedOrSandboxedServer(t *testing.T) // a CheckSelf seam returning the NoNewPrivs error → s.captureDisabled carries it
func TestAcquireTakesAFreeSlotEvenWithACancelledContext(t *testing.T) // P-1: cancelled ctx + free slot → nil; held slot + cancelled ctx → ctx.Err()
func TestTheEngineSerialisesToolsWithAJob(t *testing.T)      // the test holds the engine; top blocks; release → answers
func TestPipelinedCallsAtEOFAreAnswered(t *testing.T)        // P-1 through perfmcp: open + top + diff via strings.NewReader → all answered, none isError
func TestLivePageToolsHoldTheEngine(t *testing.T)            // P-7, -race: a Live server; a goroutine loops s.store.open under the engine while page_explain and page_show run → no race, every call answers
func TestOpenAndDiffResultsAreUnchanged(t *testing.T)        // openResultFor/diffPage == the tool results byte for byte; the existing tests pass
func TestPublishNewNamesItsRoot(t *testing.T)                // kind "profiles" → a message names the profiles root; writePage's texts unchanged; perm 0600; taken name → fs.ErrExist, temp gone
func TestOpenNamesFollowsTheStore(t *testing.T)
// cmd/perf-agent-mcp
func TestTheNetworkScanSeesTheCaptureExecRules(t *testing.T) // fixtures: a third exec.Command, exec.LookPath, a "sh" literal, RawSyscall(SYS_GETPID…), RawSyscall(SYS_PRCTL, PR_SET_DUMPABLE…) → findings; the allowed forms → none
func TestRunAcceptsAZeroCaptureTTL(t *testing.T)
// TestNoClientNetworkingInTheServer passes with internal/capture linked
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ ./cmd/perf-agent-mcp/`
Expected: the new tests FAIL. `TestNoClientNetworkingInTheServer` fails once perfmcp imports capture, until the rules land.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check, plus `go test -race -run 'Network|Links' ./cmd/perf-agent-mcp/`.
Expected: PASS. Every M2/M2b test is unchanged, `TestTheServerLinksNoProfiler` included, and darwin builds.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp cmd/perf-agent-mcp
git commit -m "perfmcp: capture limits and config, start-up verification, an engine around every engine-state tool, and the capture scan rules"
```

### Task 10: Series files, the intent/commit protocol and recovery (opus)

**Files:**
- Create in `internal/perfmcp/`: `capture_layout.go`, `capture_manifest.go`, `capture_wire.go`, `capture_decision.go`, `capture_commit.go`, `capture_recovery.go`, `capture_commit_test.go`, `capture_recovery_test.go`, `capture_layout_test.go`

**Interfaces:**
- Consumes: Task 9 `writeTemp`, `linkNoReplace`, `publishNew`; Task 1 `capture.Error`; Task 3 `capture.Alive`, `StartTime`; Task 2 `Settings`.
- Produces:
  - **`capture_wire.go`** (PLAN-R4: it is declared here, where it is first used).
    - `type wireError struct { Code, Message string; Retryable bool; RetryAfter, Next, CaptureID, Job, Status, PerfAgentExit, StderrTail, Changed string; Survivors []capture.Survivor }`, with §4.4's JSON names and empty fields omitted.
    - `func toWireError(e *capture.Error) *wireError`.
  - **`capture_decision.go`.**
    - `type decisionWord struct{ v atomic.Int32; cancelInFlight atomic.Bool }`, with `decisionOpen`, `decisionCancelRequested`, `decisionManifestInFlight` and `decisionCommitted`.
    - `requestCancel() string` returns `"cancelling"` or `"too late: committed"`.
    - `beginManifest() bool`, `settle(linked bool) (cancel string)` and `cancelled() bool`.
  - **Layout.**
    - `type captureDir struct { root *os.Root; rel, owner string }`.
    - `func (s *Server) openCaptureDir() (*captureDir, *capture.Error)`: `MkdirAll` 0700, and `.owner` through `publishNew`. On `EEXIST` the existing file is read back; another kind is `owner_mismatch`. The kind is cached in `s.ownerSeen` (P-29).
    - `func (d *captureDir) series(name string) (*os.Root, error)`.
    - `validName`, `formatID` (`%03d`), `parseName`, `finalName`.
    - `func nextID(series *os.Root) (int, error)` writes `.seq` (temp `.seq.tmp-<hex>`, fsync, rename, directory fsync) **before** it returns.
  - **The manifest.**
    - `type wireSettings struct { Modes []string; Duration, SampleRate, Unwind string; KernelStacks bool }` and `toWire(capture.Settings)`.
    - `func settingsDigest(wireSettings) string`.
    - `type fileIdentity struct { Name string; Dev, Ino uint64; Size int64; SHA256 string }`. `Dev`, `Ino` and `Nlink` come from `Sys().(*syscall.Stat_t)` converted with `uint64()`, which builds on darwin too.
    - `type manifest struct { Format int; ID, Series, Label, Outcome string; Error *wireError; Settings wireSettings; SettingsDigest string; Target manifestTarget; Command *manifestCommand; PerfAgent perfAgentInfo; Start time.Time; AttachLatency, ProfiledDuration, StoppedBy, CompareWith string; Files []fileIdentity; Kept bool; Notes []string; Survivors []capture.Survivor; Cleanup, Cancel string; WrittenBy string }`.
    - `manifestTarget`, `manifestCommand` (`Path`, and `Args` as a count) and `perfAgentInfo`.
    - `readManifest` (≤ 1 MiB; checks `Format` and `WrittenBy`) and `seriesManifests`.
  - **The transaction** (`capture_commit.go`).
    - `type intent struct{ State; ID, Series, Label; Settings; Finals []string; Staging string; StagingDev, StagingIno uint64; Token string; ServerPID int; ServerStart uint64; ReaperPID int; ReaperStart uint64; Files []fileIdentity; Probe *fileIdentity }`.
    - `func (s *Server) beginTxn(d *captureDir, series string, id int, label string, settings wireSettings, finals []string) (*captureTxn, error)` writes the intent no-replace and starts the 2 s heartbeat.
    - Intent rewrites and the manifest go through `.<final>.tmp-<token[:16]>` temporaries (P-9).
    - `makeStaging()` adds the link probe (Review Focus 5).
    - `setReaper`, `validate`, `record`, `link`, `commit(m manifest, d *decisionWord) (committed bool, err error)` and `rollback`, each as §7.4 describes.
    - `validate`'s failures: a missing or empty file is `no_output`; a symlink, `nlink` > 1 or a non-regular file is `perf_agent_failed`, "output <f> was not a regular file with one link".
    - `commit` with `beginManifest()` false rolls back to a **cancelled failure manifest**. Errors after a successful link leave `committed` true.
    - `retryStranded() error`, and `removeByIdentity(...)` as in §7.4.
  - **Recovery.** `func (s *Server) recoverCaptures(d *captureDir) (notes []string, err error)` follows §7.4's list. It also removes temporaries named with an intent's token, and `.seq.tmp-*` under the lease, each with a log line (P-9).
  - **Seams:** `s.now`, `s.ownerAlive`, `s.captureFault func(faultPoint) error` (nil, an error, or `errCrash`), and `s.writeErr func(op string) error`.
  - **Fault points:** `faultAfterLease`, `faultAfterRecovery`, `faultInEngine`, `faultAfterEngine`, `faultAfterSeq`, `faultAfterIntent`, `faultAfterSpace`, `faultAfterStaging`, `faultAfterValidate`, `faultAfterRecord`, `faultBetweenLinks`, `faultAfterLinks`, `faultAfterManifestTemp`, `faultInManifestLink`, `faultAfterManifestLink`, `faultAfterStagingRemoval`, `faultInRollback`, `faultInRecovery`, `faultHash`, `faultRetentionHash`, `faultFsync`, `faultBeforeOpen`, `faultBeforeDiff`. Each is listed with a flag `postLink` (true for `faultAfterManifestLink` and `faultAfterStagingRemoval`).

- [ ] **Step 1: Write the failing tests** (no processes; synthetic files in staging)

The oracle `invariant(t, d)` allows in each series only:
- committed manifests with verified files;
- failure or `interrupted` manifests with no files;
- `.seq`;
- logged orphan staging directories.

```go
func TestNamesAndIDs(t *testing.T)
func TestNextIDIsPersistedFirstAndNeverReused(t *testing.T)
func TestOwnerFileRefusesTheOtherKind(t *testing.T)            // both directions; the kind is cached for synchronous checks
func TestCommitHappyPath(t *testing.T)
func TestEveryFaultPointLeavesAValidState(t *testing.T)        // PLAN-R7, partitioned by the manifest link's outcome: for points before the link × {error → live rollback → failure manifest, no capture files; errCrash → ownerAlive false, now += 11s, recovery → interrupted}; for postLink points × {error, errCrash} → committed true, the ok manifest and its files intact, and the next recovery removes the leftovers by identity; invariant holds in every case
func TestENOSPCAtEveryWrite(t *testing.T)                      // PLAN-R7: ENOSPC at seq, intent, hb, staging, manifest temp, manifest link → valid failure state; ENOSPC at the directory fsync after a successful manifest link → committed, files kept
func TestALostLinkRaceSparesTheUserFile(t *testing.T)
func TestLiveOwnerENOSPCIsFinishedByTheNextCapture(t *testing.T)
func TestPostLinkErrorsFollowTheCommittedPath(t *testing.T)
func TestTheDecisionRule(t *testing.T)                         // PLAN-R7: cancel before beginManifest → a cancelled FAILURE manifest, no success manifest, no capture files; blocked link + cancel → "cancelling"; success → committed, Cancel "too late: committed"; injected link error → cancelled failure manifest; after commit → "too late: committed"
func TestRemoveByIdentityNeverRemovesWhatItCannotProve(t *testing.T)
func TestRecoveryJudgesTheOwner(t *testing.T)
func TestRecoveryRemovesItsOwnTemporaries(t *testing.T)        // P-9: crash after the manifest temp → recovery removes .<final>.tmp-<token16>; a crash mid .seq rename → .seq.tmp-* removed under the lease; a foreign ".x.tmp-…" is left
func TestAnUnrecordedStagingDirIsALoggedOrphan(t *testing.T)
func TestStagingProbeRefusesAFilesystemWithoutHardLinks(t *testing.T) // Review Focus 5
func TestCaptureMessagesNameTheProfilesRoot(t *testing.T)      // P-10: a write error names the profiles root and --captures, never --output
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Names|NextID|OwnerFile|Commit|Fault|ENOSPC|LinkRace|LiveOwner|PostLink|DecisionRule|RemoveByIdentity|Recovery|Orphan|StagingProbe|CaptureMessages'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/perfmcp/`, then the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: series files, an intent/commit protocol with identity-matched deletion, and recovery"
```

### Task 11: Retention

**Files:**
- Create: `internal/perfmcp/capture_retention.go`, `capture_retention_test.go`

**Interfaces:**
- Consumes: Task 10.
- Produces:
  - `type fileKey struct{ Dev, Ino uint64 }` and `type protection struct { files map[fileKey]bool; captures map[string]bool }`.
  - `func (s *Server) protected(d *captureDir, baseline string) protection`, run under the engine, protects:
    - the store's open files (`entry.opened`);
    - `s.captureProtect()` when it is set; it is nil in stdio, and stdio does not read `s.live` (P-22);
    - every capture named by a live intent;
    - `baseline`;
    - each series' newest `compare_with`;
    - every `kept` capture.
  - `func (s *Server) retain(d *captureDir, p protection, now time.Time) (evicted []string, err error)` follows §7.3's order and rules. Its identity checks go through `faultRetentionHash`.
  - `func dirBytes(root *os.Root) (int64, error)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRetentionKeepsPerSeriesNewestFirst(t *testing.T)
func TestRetentionByStoredBytes(t *testing.T)
func TestRetentionTTL(t *testing.T)
func TestRetentionNeverDeletesProtected(t *testing.T)     // kept, the newest's compare_with, the call's baseline, an open handle's file, a live intent's capture, a captureProtect key
func TestRetentionSparesAReplacedFile(t *testing.T)
func TestRetentionFinishesAPartialEviction(t *testing.T)
func TestRetentionDoesNotReuseIDs(t *testing.T)
func TestFailedManifestsAreRetainedLikeCaptures(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run Retention`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp/capture_retention*.go
git commit -m "perfmcp: retention deletes only manifest-verified, unprotected captures in the capture directory"
```

### Task 12: The registry, the lookup pool and the windows

**Files:**
- Create: `internal/perfmcp/capture_registry.go`, `capture_job.go` (the declarations only), `capture_registry_test.go`

**Interfaces:**
- Consumes: Task 10 `readManifest`, `decisionWord`.
- Produces:
  - **The job declarations** (PLAN-R4: declared here, where the registry first uses them).
    - `type jobStatus string`, with the seven §4.0 states.
    - `type job struct { id string; created time.Time; reserve time.Duration; d decisionWord; cancel chan struct{}; cancelOnce sync.Once; mu sync.Mutex; status jobStatus; phase, captureID, derived string; err *capture.Error; result []byte; changed chan struct{}; run atomic.Pointer[capture.Run] }`.
    - `func (j *job) requestCancel() string` calls `d.requestCancel()` and closes `cancel` once. That is a CAS and a channel close, no I/O.
    - `type historical struct { m manifest; base *manifest; baseGone string }`.

    Task 13 adds `req`, `pins` and the behaviour.
  - **The registry.**
    - `type registry struct{ mu sync.Mutex; … }`, where the mutex guards the LRU, the id maps and the windows and is never held across I/O or a channel wait (P-15). `active` is an `atomic.Pointer[job]`. It is created by `newRegistry(lim CaptureLimits, nonce string)`.
    - `newJobID()` returns `"j<N>-<nonce>"`.
    - `func (r *registry) admit(j *job, now time.Time) *capture.Error` works under `mu`: it checks that the slot is free (`busy`) **and** that the windows allow `j.reserve` (`budget`, with `retry_after`), then claims the slot and records the reservation. A rejected budget never touches the slot (PLAN-R3).
    - `func (r *registry) reconcile(j *job, effective time.Duration) *capture.Error` replaces the job's reservation with the effective duration. An effective duration that no longer fits is `budget`, and that reservation is released.
    - `finish(j *job, resultBytes int)`, `get(id) (*job, bool)`, `put(captureID string, h *historical)` and `list() []*job`.
  - **The windows.** `type window struct{…}`, with `allow(now, d, maxCaptures, maxDuration)`, `reserve(now, d)` and `replace(old, new)`.
  - **The lookup pool.** `newLookupPool(2, 16)`; `get(key, load) (*lookup, bool)`, shared per key, returning `(nil, false)` when full; `close()`. No goroutine is started per wait.

- [ ] **Step 1: Write the failing tests**

```go
func TestAdmitIsOneAtATime(t *testing.T)
func TestARejectedBudgetNeverHoldsTheSlot(t *testing.T)   // PLAN-R3: WindowCaptures 1 used; admit j2 → budget; active() nil; after the window frees, admit j3 → ok
func TestReconcileMovesTheReservation(t *testing.T)       // reserve 30m (MaxDuration) then reconcile to 20s → a following 29m request fits; reconcile above the window → budget, reservation released
func TestWindowsBound(t *testing.T)
func TestRegistryLRUByCountAndBytes(t *testing.T)
func TestJobIDsCarryTheProcessNonce(t *testing.T)
func TestRequestCancelClosesTheChannelOnce(t *testing.T)  // twice → no panic; channel closed; answer "cancelling"
func TestRegistryIsSafeFromTheReaderGoroutine(t *testing.T) // -race: get + requestCancel in one goroutine, admit/finish/put in another
func TestLookupsAreSharedAndBounded(t *testing.T)
func TestWaitingDoesNotAddGoroutines(t *testing.T)
func TestDecisionWord(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Admit|RejectedBudget|Reconcile|Windows|RegistryLRU|JobIDs|RequestCancel|RegistryIsSafe|Lookups|WaitingDoesNot|DecisionWord'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: a bounded job registry, a shared lookup pool and rolling capture windows with reservations"
```

### Task 13: The job state machine, the cancel relay and the derivations (opus)

**Files:**
- Create: `internal/perfmcp/capture_run.go`, `capture_compare.go`, `capture_job_test.go`, `capture_main_test.go`
- Modify: `capture_job.go`, `server.go` (`New`, `Close`, `KillCaptures`, maintenance, `OnClose`)

**Interfaces:**
- Consumes: Tasks 7 and 9–12.
- Produces:
  - **`capture_main_test.go`.** `TestMain` runs `capture.RunInternal` first, then `os.Exit(capturetest.Main(m))`.
  - **The request.** `type captureRequest struct { PID int; Command []string; Cwd string; Settings capture.Settings; Explicit map[string]bool; Series, Label, CompareWith string; Keep bool }`, added to `job` as `req`, with `pins []any`.
  - **The view.** `func (j *job) view() jobView`, where `type jobView struct { Job, ID, Status, Phase, Elapsed, ETA, Derived string }`, and `func (j *job) wait(ctx, d) jobView`.
  - **Admission.** `func (s *Server) startJob(req captureRequest) (*job, *capture.Error)` sets `j.reserve` = the explicit `duration`; else `MaxDuration` when `CompareWith` ≠ ""; else 10 s. It then calls `admit` and `go s.runJob(j)`.
  - **`func (s *Server) runJob(j *job)`**, following §7.4:
    1. **Resolve, with no side effect.** `cwd` (P-28: under `s.src.Dirs()` lexically after `EvalSymlinks`, or `s.rel` under the profiles root), `ResolveCommand`, `CheckCommandFile`; then `openCaptureDir`.
    2. `dirMu`, then `TryLock`; then `retryStranded` and `recoverCaptures`.
    3. **The engine section.**
       - Resolve the baseline: `unknown_baseline` names a missing or non-`ok` capture's outcome and lists the last 5 `ok` ids; `incomparable` names a conflicting setting.
       - **Effective settings (PLAN-R3).** Inherit the non-explicit settings from the baseline, then validate them against **this** server: `MaxDuration` and `MaxSampleRate` (`invalid_argument`); gpu without a shim (`disabled`); `kernel_stacks` without `cap_syslog` (`invalid_argument`).
       - `registry.reconcile(j, effective.Duration)`.
       - `MaxKeptPerSeries`; pin the baseline; `protected`; `retain`.
    4. `nextID`, `beginTxn`, the space check (`capture.FreeBytes` for `MinFreeBytes`), `makeStaging`, then `capture.Start` and `j.run.Store(r)`.
    5. **The cancel relay (PLAN-R2).** Once `j.run` is stored, the job checks `j.d.cancelled()` and calls `r.Cancel()` if it is set. A goroutine sends `r.Wait()` on a channel, and the job `select`s on that outcome and on `j.cancel`; when `cancel` fires it calls `r.Cancel()` and keeps waiting for the outcome. Before `Start`, a cancel is noticed at each boundary.
    6. `committing`, then `done` and the derivations under the engine, then the pins are released and `registry.finish`.
  - **Shutdown.**
    - `func (s *Server) cancelAllJobs()`: `requestCancel` on each job. It is `OnClose`.
    - `func (s *Server) KillCaptures()` reads `j.run` atomically, takes no lock, and calls `Run.Kill`, which leaves the reaper alive (P-16, PLAN-R1). It sets `s.killed`.
    - `Close`: `cancelAllJobs`, then `KillCaptures`. Unless `s.killed` was already set by a signal path (P-2c), it waits up to `StopGrace` + 1 s for job goroutines. It never waits on a maintenance pass mid-hash.
  - **Maintenance.** `dirMu`, then `TryLock` (skipped when busy), then recovery, then retention under the engine; plus `retryStranded` every 30 s.
  - **Derivations** (`capture_compare.go`). `captureResult`, `profileResult`, `compareResult`, `compareView`, `toolCall` and `recompute`, as before. `derive(j, m, base)`, `recomputeFor(m, base, baseGone)`, and the GPU note.
  - **`capture.FreeBytes(path string) (int64, error)`** is added in `internal/capture/statfs_linux.go`, with its stub.

- [ ] **Step 1: Write the failing tests**

```go
func TestJobRunsThroughEveryState(t *testing.T)
func TestStartJobDoesNoIO(t *testing.T)
func TestBlockedSeamsNeverBlockAdmissionOrWait(t *testing.T)
func TestACommitThatOutlivesAWait(t *testing.T)
func TestDerivationFailureNeverRollsBack(t *testing.T)
func TestCompareWithInheritsAndRefuses(t *testing.T)
func TestCompareWithAFailedCaptureIsRefusedNamingItsOutcome(t *testing.T) // Review Focus 4
func TestInheritedSettingsAreValidatedAndBudgeted(t *testing.T)     // PLAN-R3: baseline duration 20s, cand omits it → runs 20s, window charged 20s (reservation MaxDuration reconciled); restart with MaxDuration 10s → async invalid_argument naming the inherited 20s; baseline gpu on a server without a shim → async disabled; baseline kernel_stacks without cap_syslog → async invalid_argument
func TestTheBaselineIsPinnedBeforeRetention(t *testing.T)
func TestCancelStopsTheChildPromptly(t *testing.T)                  // PLAN-R2: a 1m capture; requestCancel in running → the fake's group gone within StopGrace + 1 s; manifest cancelled
func TestCancelBeforeRunExistsIsHonoured(t *testing.T)              // PLAN-R2: faultAfterStaging blocks; cancel; unblock → no perf-agent (no Record) or stopped within StopGrace; cancelled
func TestCancelAtEachState(t *testing.T)                            // preparing, committing, the blocked manifest link → as §7.4
func TestCloseCancelsAndKills(t *testing.T)
func TestKillCapturesNeverBlocks(t *testing.T)                      // < 10 ms with a job blocked in faultFsync
func TestMaintenanceKeepsControlResponsive(t *testing.T)            // P-8: faultRetentionHash blocks at start; New returns; initialize, tools/list, capture_list, capture_wait, capture_cancel answer; top answers after unblock
func TestALiveServerDerivesWhilePageToolsRun(t *testing.T)          // P-7 -race: Live server, a job deriving while page_explain runs
func TestRecomputeRecipeShape(t *testing.T)
func TestTheGPUNoteForAnEmptyProfile(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'TestJob|StartJob|BlockedSeams|Outlives|DerivationFailure|CompareWith|Inherited|Baseline|Cancel|CloseCancels|KillCaptures|Maintenance|ALiveServer|Recompute|GPUNote'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=2 ./internal/perfmcp/`, then the green check.
Expected: PASS, with no leftover processes.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp internal/capture
git commit -m "perfmcp: the capture job state machine, its cancel relay, and post-commit open and compare derivations"
```

### Task 14: The four tools, the skill's loop and the CLI tool list

**Files:**
- Create: `internal/perfmcp/tool_capture.go`, `tool_capture_list.go`, `tool_capture_test.go`, `capture_loop_test.go`, `capture_errors_test.go`
- Modify:
  - `internal/perfmcp/`: `server.go` (`tools()` order, P-25; `capture_cancel` is `Control`), `harness_test.go` (`send`/`recv` by id), `skill_test.go` (`skillArgs`);
  - `skills/perf-agent-mcp/SKILL.md` (P-4);
  - `cmd/perf-agent-mcp/main_test.go` (the expected tool list).

**Interfaces:**
- Consumes: Tasks 8, 10, 12 and 13.
- Produces:
  - **Errors.** `func toolError(e *capture.Error, j *job) mcpstdio.Result`, built with `toWireError` (Task 10).
  - **`capture`.**
    - It has §4.2's schema, without `show`.
    - Admission:
      1. disabled (`s.captureDisabled`);
      2. `owner_mismatch`, when it is cached;
      3. pid xor command; `./x` gives `invalid_argument`, "pass <cwd joined>";
      4. distinct modes;
      5. explicit settings against the limits and caps; gpu without a shim is `disabled`;
      6. `series` and `label`;
      7. the `compare_with` format;
      8. `startJob`, which may answer `busy` or `budget`.
    - Result `{job, status: "accepted", eta}`. Annotations per §4.5.
  - **`capture_wait`.** Its timeout defaults to 30 s, is capped at 45 s, and must be > 0. It looks in the registry, then in the lookup pool:
    - `loading`, with `retry_after` `"1s"`;
    - `unknown_capture`;
    - the full result;
    - `toolError` for failed and cancelled;
    - a historical answer, with `derived: "recompute"` and `handles: "reopen"`.

    It applies the size cut, and sends progress every 5 s and at each change.
  - **`capture_cancel`** (`Control`): `j.requestCancel()` only.
  - **`capture_list`**: as §4.6, through the pool, waiting at most 10 s.
  - **The skill** (P-4) gets five lines on the capture loop, naming all four tools. `skillArgs` gains the capture arguments and codes the skill cites.
  - **The CLI test.** `TestRunRegistersNoPageTools` becomes `TestRunRegistersTheProfileAndCaptureTools`: the nine M2 tools, then the four capture tools, and no `page_*`.

- [ ] **Step 1: Write the failing tests**

```go
// tool_capture_test.go
func TestCaptureIsDisabledWithoutPerfAgent(t *testing.T)
func TestCaptureAcceptsInMicroseconds(t *testing.T)
func TestCaptureArgumentRefusals(t *testing.T)
func TestCaptureRelativeCommandSuggestsTheAbsolutePath(t *testing.T) // Review Focus 1
func TestCaptureWaitTimeouts(t *testing.T)
func TestCaptureWaitDuringALongCaptureWithLoad(t *testing.T)
func TestCaptureCancelIsAControlMessage(t *testing.T)
func TestCaptureCancelStopsTheChildPromptly(t *testing.T)           // PLAN-R2: tool cancel during a 1m capture → the fake's group gone within StopGrace + 1 s (pgid from Record)
func TestEOFStopsTheChildPromptly(t *testing.T)                     // PLAN-R2: close the harness writer during a 1m capture → group gone within StopGrace + 1 s; manifest cancelled
func TestCaptureCancelUnknownIDDoesNoIO(t *testing.T)
func TestCaptureListShowsTheRunningCaptureFirst(t *testing.T)
func TestCaptureListPagesAndLoads(t *testing.T)
func TestEveryCallStaysUnderTheClientBudget(t *testing.T)
func TestProgressNotificationsAreInPhaseOrder(t *testing.T)
func TestTheResultIsCutToMaxResponseBytes(t *testing.T)
func TestToolsListOrder(t *testing.T)                               // P-25: nine M2, four capture, page_* (Live)
// capture_loop_test.go
func TestALoopOfABaseAndThreeCandidates(t *testing.T)
func TestCompareWithAcrossAServerRestart(t *testing.T)
func TestRecomputeReplaysExactly(t *testing.T)                      // executes every emitted step, substituting binds → rows equal the live job's
func TestRecomputeReportsARetainedAwayBaseline(t *testing.T)
// capture_errors_test.go
func TestEveryErrorCodeIsProducedOnce(t *testing.T)
// skill_test.go: TestSkillNamesExistInTheServer passes with the four tools in the skill
// cmd: TestRunRegistersTheProfileAndCaptureTools
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race -short ./internal/perfmcp/ ./cmd/perf-agent-mcp/`
Expected: the new tests FAIL.

- [ ] **Step 3: Implement the tools, the skill lines and the list update.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check, without `-short`.
Expected: PASS. `TestSkillNamesExistInTheServer` and the CLI list test pass.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp skills/perf-agent-mcp/SKILL.md cmd/perf-agent-mcp/main_test.go
git commit -m "perfmcp: capture, capture_wait, capture_cancel and capture_list"
```

### Task 15: The command: flags, internal modes, signal paths and the dead client

**Files:**
- Modify: `cmd/perf-agent-mcp/main.go`, `main_test.go`
- Modify: `internal/perfmcp/config.go` (`Config.OutputQueueMessages int`: 0 means 64, test-only use, no flag; PLAN-R6)

**Interfaces:**
- Consumes: Task 6 `capture.RunInternal`; Task 9 `CaptureConfig` and start-up verification; Task 13 `KillCaptures`; Task 8 `ErrClientGone`.
- Produces:
  - **`main()`.** `if code, ok := capture.RunInternal(); ok { os.Exit(code) }` comes first.
  - **Flags.**
    - `--perf-agent`, `--captures`, `--capture-command` (repeatable), `--capture-ttl` (applied after `--limit`) and `--gpu-shim`.
    - A `perfmcp.New` error, a failed verification included, exits 1 with the reason.
    - The package doc comment gets the M2c wording.
  - **Signal paths (P-2).**
    - On ctx done, `srv.KillCaptures()` runs **before** anything else: before waiting on `serveDone`, and in the forced branch.
    - `Close` then skips the job wait, and recovery records `interrupted` later.
    - `serveErr == ErrClientGone` goes the same way: kill, then `Close` in a goroutine bounded by 100 ms, then `return 0`.
  - **Test re-exec (P-17).**
    - `TestMain` runs `capture.RunInternal` first.
    - `liveEnv` and `PERF_AGENT_MCP_TEST_FAKE_VERIFY=1` compose into one `configHook`, which sets `Live` and/or `Config.Capture.Verify`.
    - `PERF_AGENT_MCP_TEST_OUTQ=<n>` sets `OutputQueueMessages`.
    - `fakeCommand(t, env, args...)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunRefusesABadPerfAgent(t *testing.T)               // relative → exit 2 or 1 with the reason; a plain file → exit 1 naming the capabilities and the setcap line
func TestRunCaptureFlagsReachTheConfig(t *testing.T)
func TestStdoutStaysJSONRPCWhileCapturing(t *testing.T)      // spam-stdout fake
func TestSignalKillsCapturesOnEveryPath(t *testing.T)        // P-2, PLAN-R1: double-fork-setsid workload + ignore-SIGINT fake; (a) stdout drained: SIGTERM → exit 0 ≤ shutdownGrace + 300 ms, the in-flight response written; (b) stdout blocked: the same bound; both: the fake's group (Record) and the reported grandchild gone within StopGrace + 500 ms
func TestADeadClientEndsCaptures(t *testing.T)               // PLAN-R6: OUTQ=2 and a 4 KiB stdout pipe never read; flood 200 pings with an overall 20 s timeout; assert the saturation log line "client is not reading" on stderr before awaiting exit; exit 0; group gone within StopGrace
func TestEOFCancelsTheCapture(t *testing.T)
func TestTheServerLinksNoProfiler(t *testing.T)              // unchanged
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp/`
Expected: the new tests FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp internal/perfmcp/config.go
git commit -m "perf-agent-mcp: capture flags, internal reaper and launcher modes, and a capture kill on every exit path"
```

### Task 16: Multi-process and crash tests (opus)

**Files:**
- Create: `internal/perfmcp/capture_process_test.go`, `procserver_test.go`
- Modify: `capture_main_test.go` (`PERFMCP_TEST_SERVER=1` routing)

**Interfaces:**
- Consumes: everything above.
- Produces (test-only):
  - `TestMain` routes `PERFMCP_TEST_SERVER=1`: a stdio server driven by a `Config` taken from `PERFMCP_TEST_CONFIG` (JSON).
  - In that server, crash point `X` makes `captureFault(X)` call `os.Exit(3)`.
  - `startServerProcess(t, cfg) *procClient`, with `call`, `pid` and `kill9`.
  - A `RunSpec.FaultPause` seam in the config.

- [ ] **Step 1: Write the failing tests**

```go
func TestTwoServersShareOneCapturesDir(t *testing.T)
func TestAKilledServerLeavesTheLockWithItsReaper(t *testing.T)     // P-23: FaultPause before-exit:<f>; kill9(A) during running → B's capture fails busy naming the reaper pid (deterministic while f exists); remove f → the reaper exits; B's next capture succeeds and recovery wrote "interrupted", judged by the reaper's pid and start time
func TestKillingTheServerInEachPhaseKillsTheChildren(t *testing.T)
func TestAHungServerIsBoundedByTheDeadline(t *testing.T)
func TestTheSpawnReportGapStillKills(t *testing.T)
func TestRealCrashesAtCommitBoundaries(t *testing.T)               // partitioned as Task 10: before the manifest link → interrupted; after → committed, leftovers recovered
func TestStdioAndServeRefuseEachOthersDir(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'TwoServers|KilledServer|EachPhase|HungServer|SpawnReport|RealCrashes|RefuseEachOthers'`
Expected: FAIL.

- [ ] **Step 3: Implement the harness.** Fix what the tests expose in Tasks 5–13's code, in this task's commit, each fix with the test that found it.

- [ ] **Step 4: Run them to verify they pass**

Run: the same tests with `-count=3`, then the green check.
Expected: PASS three times, with no leftover processes.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: multi-process lock, reaper and crash-recovery tests"
```

### Task 17: Guide, docs and changelog

**Files:**
- Modify: `internal/perfmcp/guide.md`, `guide_test.go`, `skills/perf-agent-mcp/SKILL.md` (refinements only; the loop lines landed in Task 14), `docs/mcp.md`, `CHANGELOG.md`

**Interfaces:**
- Consumes: Task 14.
- Produces:
  - **The guide.** A "Capture loop" section before "Claimable metrics" covers:
    - one series per loop, `keep` the base, `compare_with`;
    - `capture`, then `capture_wait` until `done`;
    - read `comparability`;
    - branch on `error.code` and `retryable`;
    - capture only the targets the user's task names;
    - after a restart, wait by capture id and run `recompute`.

    It stays ≤ 12288 bytes.
  - **`docs/mcp.md`.**
    - The M2c wording.
    - **Line 7 names the two exceptions to "no process start, no raw system call"** (P-3): `exec_linux.go`'s two exec sites, and the `prctl` calls in `launch_linux.go`.
    - The flags, the tools, the limits, the codes, and retention as "the one place it deletes files".
    - The guardrails and the residual exposures, and the cgroup write.
    - Client timeouts, the setcap line, "not in /tmp", and the no_new_privs refusal.
  - **`CHANGELOG.md`**: an `[Unreleased]` entry.

- [ ] **Step 1: Write the failing tests**

```go
// guide_test.go: + "## Capture loop", "`compare_with`", "`capture_wait`", "`comparability`", "`retryable`", "`recompute`"
func TestDocsListEveryCaptureCodeAndLimit(t *testing.T)   // every capture.Code and every Capture.* field name appear in docs/mcp.md
func TestDocsNameTheProcessExceptions(t *testing.T)       // docs/mcp.md names exec_linux.go and launch_linux.go
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Guide|Docs'`
Expected: FAIL.

- [ ] **Step 3: Write the text.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp/guide.md internal/perfmcp/guide_test.go skills/perf-agent-mcp/SKILL.md docs/mcp.md CHANGELOG.md
git commit -m "docs: the capture loop in the guide and docs/mcp.md"
```

### Task 18: The real capture (gated, privileged)

**Files:**
- Create: `internal/perfmcp/capture_real_linux_test.go`

**Interfaces:**
- Consumes: the real `capture.VerifyBinary`; Task 4's `capworkload`.
- Produces: `TestRealCapture` and `TestRealKernelStacks`.

- [ ] **Step 1: Write the tests** (PLAN-R6: only an unset variable skips)

```go
func TestRealCapture(t *testing.T) {
    // bin := os.Getenv("PERF_AGENT_BIN"); "" → t.Skip("PERF_AGENT_BIN is not set")
    // from here every setup failure is t.Fatalf, not Skip: VerifyBinary(bin) error → Fatalf naming it;
    //   CheckSelf(ReadSelfStatus()) error (root, caps, NoNewPrivs 1) → Fatalf with that reason (P-24)
    // capture {command: [capworkload, "busy", "5s"], duration "2s"} → capture_wait done → top has a row containing "main.spin"
}
func TestRealKernelStacks(t *testing.T) // same gate; bin without cap_syslog → Fatalf ("PERF_AGENT_BIN lacks cap_syslog"); kernel_stacks true → the note on kernel addresses; ≥ 1 kernel frame
```

- [ ] **Step 2: Prepare the binary** (the user runs `setcap`; ask first)

```bash
make build
mkdir -p ~/bin && cp perf-agent ~/bin/perf-agent-captest   # NOT /tmp (nosuid)
sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep ~/bin/perf-agent-captest   # the user
getcap ~/bin/perf-agent-captest
```

- [ ] **Step 3: Run it**

Run: `PERF_AGENT_BIN=$HOME/bin/perf-agent-captest go test -race -run 'TestReal' -v ./internal/perfmcp/ 2>&1 | tee /dev/stderr | grep -c -- '--- PASS: TestReal'`
Expected: `2`, with no `--- SKIP`. With the variable unset, both tests SKIP. With it set to a non-setcap'd file, both FAIL.

- [ ] **Step 4: Commit**

```bash
git add internal/perfmcp/capture_real_linux_test.go
git commit -m "perfmcp: a gated real capture with the setcap'd perf-agent"
```

### Task 19: Verification, review and the draft PR

**Files:** none new.

- [ ] **Step 1: Run the whole suite.** `CGO_CFLAGS=… CGO_LDFLAGS=… go test -race ./...` must PASS. `GOOS=darwin go build ./internal/capture/ ./internal/perfmcp/ ./cmd/perf-agent-mcp/` must build.
- [ ] **Step 2: Lint.** `golangci-lint version` must say 2.11.4; install it if the command exits 127. Then `golangci-lint run ./...` must have no findings.
- [ ] **Step 3: Check the pre-existing tree is untouched.** `git diff --stat ai-mcp-m2b -- main.go telemetry.go perfagent gpu cmd/flamegraph internal/profilemodel` must be empty.
- [ ] **Step 4: Run the real capture and a smoke test.** Run Task 18's command: exactly 2 PASS and 0 SKIP. Then do a manual Claude Code stdio smoke: `capture` → `capture_wait` → `top`. Record the transcripts under `.superpowers/sdd/2026-11-15-capture-plan/`.
- [ ] **Step 5: Get a Codex review.** Ask the perf-agent Codex session, through Workforce, to review the branch against the spec. Verify every claim, and record the rulings in `.superpowers/sdd/2026-11-15-capture-plan/progress.md`.
- [ ] **Step 6: Push a draft PR** stacked on the trimmed #264, with no AI attribution and no session URLs. **Do not merge**; ask the user.

```bash
git push -u origin ai-mcp-m2c-capture
gh pr create --draft --base ai-mcp-m2b --title "perf-agent-mcp: capture profiles from the MCP server" --body-file .superpowers/sdd/2026-11-15-capture-plan/pr-body.md
```

---

## Revision 2 (after the pre-flight scan and Codex #428)

| Finding | Change, by owning task |
|---|---|
| PLAN-R1 (#428-1) | Global Constraints and the Kill-path row. T6: `Reaper.Kill` kills the recorded groups and closes the command pipe, and never signals the reaper; `Done()`; tests `TestKillLeavesTheReaperToCleanUp` and `TestKillDuringAnUnreportedFork`. T7: every kill goes through it (`TestRunKillsKeepTheSweep`). T13: `KillCaptures`. T15: the forced exit with a setsid grandchild |
| PLAN-R2 (#428-2) | T12: `job.cancel` and `requestCancel` closing it once. T13: the relay `select` on the outcome and `cancel`, and the cancel-before-`Run` case (`TestCancelStopsTheChildPromptly`, `TestCancelBeforeRunExistsIsHonoured`). T14: the tool cancel and EOF stop the child within StopGrace + 1 s |
| PLAN-R3 (#428-3) | The Admission row. T12: `admit` checks the slot and the windows together, so a rejected budget never holds the slot; `reconcile`. T13: the effective settings validated against this server, the reservation reconciled (`TestInheritedSettingsAreValidatedAndBudgeted`) |
| PLAN-R4 (#428-4, P-3, P-4, P-5) | The green check in every Step 4. The scan rules move from T15 to T9; the skill lines and the CLI tool list to T14; `internal/capture/main_test.go` to T3 (T5 and T6 extend it); `wireError` and `decisionWord` to T10; the `job` and `historical` declarations to T12 |
| PLAN-R5 (#428-5, P-13) | Global Constraints, Platforms: a common vs `_linux.go` split file by file, `Supported` only in `supported_linux.go` and `capture_other.go`, a stub for every exported Linux symbol, the darwin cross-build from T1, and `uint64()` conversions of `Stat_t` fields |
| PLAN-R6 (#428-6) | T1: `checkBuildInfo` is tested directly, and the production order is kept. T6: NoNewPrivs after exec, with an exec chain; the passwd/su sampling is dropped. T15: `OutputQueueMessages`, a 4 KiB pipe, and the saturation asserted before the wait. T18: an invalid but explicitly set `PERF_AGENT_BIN` FAILs, and T19 requires 2 PASS and 0 SKIP |
| PLAN-R7 (#428-7) | T10: the oracle is partitioned by the manifest link's outcome (`postLink` points), the same for ENOSPC at the directory fsync, and a cancel before the link gives a cancelled failure manifest. T16 uses the same partition |
| Codex notes | `StopGrace` 0 is refused (T9). The maintenance test asks only for control calls (T13). `FaultPause` suspends publication only (T6). `Process.Release` and pipe close after `wait4` (T6). `capturetest.Main` owns the binaries' lifetime (T4) |
| P-1 | T9 `acquire` tries the free slot first; tests in T8 and T9 |
| P-2 | T8: drain ≤ `DrainTimeout` on ctx done. T9: 800 ms. T13: `s.killed` skips the `Close` wait. T15: `KillCaptures` on every signal path |
| P-6 | T4: `fixtures/` beside the fake; only `fakeperfagent` imports capture |
| P-7 | T9: the engine wraps `page_*`; the comments in four files; T13 adds the Live race test |
| P-8 | T13 `TestMaintenanceKeepsControlResponsive` |
| P-9 | T10: token-named temporaries, recovery removes them; `TestRecoveryRemovesItsOwnTemporaries` |
| P-10 | T9 `kind` parameters; T10 `TestCaptureMessagesNameTheProfilesRoot` |
| P-11 | T2 diffs against `ai-mcp-m2b` |
| P-12 | T1 Step 0 is the base check only |
| P-14 | `*exec.Cmd` only in `exec_linux.go`, behind the opaque `child` (T6) |
| P-15 | T12: the registry mutex; `TestRegistryIsSafeFromTheReaderGoroutine` |
| P-16 | T12/T13: `job.run` is `atomic.Pointer`; `KillCaptures` takes no lock |
| P-17 | T9: verification inside `New`. T15: the composed `configHook` |
| P-18, P-19 | T8: an empty queue always fits; `Control` only after initialize |
| P-20 | T7: the PID poll uses `Alive` |
| P-21 | T6: `cgroup.procs` W_OK and the fallback without `CgroupFD` |
| P-22 | T11: no `s.live`; `captureProtect` for serve |
| P-23 | T6 `before-exit` pause; T16 uses it |
| P-24 | T1 `CheckSelf` and NoNewPrivs; T9 disables capture; T18 fails on it (see below) |
| P-25 | The tools/list order row; T14 `TestToolsListOrder` |
| P-26 | T4: `fakeperfagent` and `capworkload` basenames |
| P-28 | T13: `cwd` under `s.src.Dirs()` |
| P-29 | T10 caches the owner; T14's admission uses the cache |

**One deliberate divergence.** P-24 says Task 18 *skips* when the test process has NoNewPrivs 1. PLAN-R6 says an explicitly requested real run must not go green without running. Task 18 therefore **fails** with the P-24 reason when `PERF_AGENT_BIN` is set, and skips only when it is unset.

## Self-review (re-run for revision 2)

**1. Spec coverage**

| Spec section | Task |
|---|---|
| §4.0 | 12, 13, 14 |
| §4.1–4.6 | 14; errors also 1, 7, 10, 13 |
| §5.1 | 1, 9 |
| §5.2 | 2 |
| §5.3–5.4 | 6, 7 |
| §6.1 | 9, 5, 7, 12 |
| §6.2 | 10, 16 |
| §6.3 | 3, 7 |
| §6.4 | 3, 6 |
| §6.6 | 6, 7, 13, 15, 16 |
| §7 | 10, 11, 12 |
| §8 | 3, 7, 13 |
| §9 and §9.1 | 8, 14, 15 |
| §10 | distributed; process-level in 16 |
| §11 | 9, 14, 15, 17 |
| §12 | 17 |

The Codex #418 carries are in 10, 13, 14 and Global Constraints. Every PLAN-R and P ruling is placed in the Revision 2 table.

**2. Step scan.**
- Each code step names its files, signatures and values. Task 16 Step 3 is the one open step by design: it fixes what the process tests find.
- Each Step 4 has the green check.
- No step depends on a later task:
  - T3/T5 use only their own `TestMain`;
  - T9 lands the scan rules for code from T6, which already exists;
  - T10 declares `wireError`, used later in 14;
  - T12 declares `job`, which 13 extends;
  - T14 updates the skill and the CLI list it breaks.

**3. Type consistency.** Each name has one owning task:

| Task | Declares |
|---|---|
| 1 | `capture.Error`, `Code`, `CapSet`, `Identity`, `Binary`, `Shim`, `CheckSelf` |
| 2 | `Settings`, `Flags` |
| 3 | `Proc`, `SelfID`, `TargetState`, `Alive` |
| 5 | `Holder`, `Lease` |
| 6 | `ReaperSpec`, `LaunchSpec`, `ReaperFiles`, `Command`, `Event`, `Reaper.Kill`/`Done` |
| 7 | `Bounds`, `RunSpec`, `Run`, `Outcome` |
| 9 | `engine`, `CaptureLimits`, `CaptureConfig` |
| 10 | `wireError`, `decisionWord`, `captureDir`, `captureTxn`, `manifest`, `fileIdentity`, `wireSettings` |
| 11 | `fileKey`, `protection` |
| 12 | `registry`, `job` (`cancel`, `run`), `historical`, `lookupPool` |
| 13 | `captureRequest`, `jobView`, `captureResult`, `recompute`, `toolCall`, `capture.FreeBytes` |
| 15 | `Config.OutputQueueMessages` |

The seams `captureFault`, `writeErr`, `ownerAlive`, `now`, `capturePublish` and `captureProtect` are named the same everywhere they appear.

**4. Review Focus.** The five tests are in Tasks 14, 7, 7, 13 and 10. The revision adds none: the Codex and pre-flight cases now have their own named tests.

**5. Proportion.** The plan is about the spec's size in bytes, and its code blocks are test names with their assertions plus a few exact literals. Revision 2 replaced bodies with rulings, and added no function bodies.
