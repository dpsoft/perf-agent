# Capture from the MCP (M2c-capture) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Plain stdio `perf-agent-mcp` gains `capture`, `capture_wait`, `capture_cancel` and `capture_list`. They run the user's setcap'd `perf-agent` binary as a supervised child against one PID or one launched command, write the pprof (and GPU executions log) into a numbered series under the profiles root, and hand back `open` handles plus a `compare_with` diff summary. An autonomous agent can then loop: capture → analyse → change → re-capture → compare.

**Architecture:**
- **`internal/capture`** (new, no transport knowledge) holds:
  - the binary and shim verifiers, the target preflight and recheck, and the argv builder;
  - the captures-directory `flock`;
  - the reaper and launcher internal modes of the same binary;
  - `Run`, the server side of one capture.
- **`internal/perfmcp`** holds:
  - the series layout, manifests and the intent/commit protocol;
  - recovery and retention;
  - the job state machine (§4.0) and the bounded registry with its lookup pool;
  - the engine lock;
  - the four tools.
- **`internal/mcpstdio`** gets a reader that never waits, control messages, and one bounded asynchronous writer.
- **`cmd/perf-agent-mcp`** adds the flags, routes the internal modes, and kills children on the forced-exit path.

**Tech Stack:** Go 1.26 standard library only (`os/exec`, `syscall`, `debug/buildinfo`, `debug/elf`, `crypto/sha256`, `sync/atomic`). Tests may use `golang.org/x/sys/unix`, as `main_test.go` already does. No new module.

**Spec:** `docs/superpowers/specs/2026-11-15-capture-design.md` (closed after six Codex rounds). §4.0 is the single lifecycle definition, §7.4 the single file-protocol order, and §15 maps every review finding. The rulings CAP-R1..R17 and SRV-R5/R6/R7, and the Codex #418 implementation carries, are at the end of `.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`. The serve design (`docs/superpowers/specs/2026-11-15-local-server-design.md`) is **not** built here; this plan only leaves the seams it names (see "Seams for serve").

**Base:** branch `ai-mcp-m2b` after the trim commit, which removes `--live` and the stdio `page_*` tools. Stdio `tools/list` is then M2's nine tools. Work on a new branch `ai-mcp-m2c-capture` cut from that commit.

## Global Constraints

- Go 1.26 (`go 1.26.0` in go.mod). **Invoke `modern-go-guidelines:use-modern-go` before writing any Go**, not as a cleanup pass.
- Tests run with `-race`. Lint with golangci-lint **v2.11.4**: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`. A missing binary exits 127, which is not a pass.
- Commands that build the perf-agent root package (the contract test, `./...`) need `CGO_CFLAGS=-I/home/diego/github/blazesym/capi/include` and `CGO_LDFLAGS=-L/home/diego/github/perf-agent/.worktrees/ai-mcp-m2/.superpowers/sdd/2026-11-14-mcp-server-m2/blazesym-lib`. The MCP packages do not import blazesym.
- **Build on top [user].** No change to pre-existing perf-agent:
  - not `main.go`, `telemetry.go`, `perfagent/`, `cmd/flamegraph`, `gpu/*`, or `profilemodel` semantics;
  - no perf-agent flag is added.

  The one new file on that side is the test-only `capture_contract_test.go` in the root `package main`. MCP code (`internal/perfmcp`, `internal/mcpstdio`, `cmd/perf-agent-mcp`, guide, skill, `docs/mcp.md`) may change.
- **The server never holds or raises a capability.**
  - It never loads BPF and never links `perfagent`, `cilium/ebpf` or blazesym. `TestTheServerLinksNoProfiler` stays unchanged and passing, and the non-std modules stay ⊆ {`github.com/dpsoft/perf-agent`, `github.com/google/pprof`}, so **no `x/sys` in non-test code**.
  - It never runs `sudo`, `pkexec` or a shell.
  - Capture is disabled when the server's euid is 0 or its CapEff/CapPrm are non-zero.
- **Process starts.**
  - `os/exec` is imported only by `internal/sourceroot/git.go` and `internal/capture/exec_linux.go`. The latter has exactly two `exec.Command` calls: the verified perf-agent, and the server's own executable in reaper or launcher mode.
  - `syscall.Exec` and `syscall.RawSyscall` appear only in `internal/capture/launch_linux.go`. The raw call is only `RawSyscall(SYS_PRCTL, prSetNoNewPrivs|prSetChildSubreaper, …)`.
  - Signals go only through `syscall.Kill(-pgid, sig)` on a recorded pgid, or by recorded PID. **Never `pkill -f`**, in code, tests or the executor's shell.
- **Environments are exact.**
  - perf-agent gets exactly `PATH=/usr/local/bin:/usr/bin:/bin` and `LC_ALL=C`.
  - A workload gets only `PATH HOME USER LOGNAME LANG LC_* TZ TMPDIR XDG_RUNTIME_DIR CUDA_VISIBLE_DEVICES` from the server, plus `CUDA_INJECTION64_PATH=<shim>` for `gpu`.
- **Stdout carries only JSON-RPC.** Child output goes to bounded rings (16 KiB) and slog debug on stderr. The result returns only the workload's last 2 KiB per stream, labelled `untrusted`, and perf-agent's stderr tail only on failure. Results and manifests never carry `cmdline`, argv beyond `command[0]` and the argument count, or the environment.
- **Wire rules (M2).** Every integer is a decimal string, durations are Go duration strings, and errors repeat agent input only through `echo`. A capture failure is an `isError` result whose text is one JSON object `{"error":{…}}` (§4.4).
- **Fixed values** (spec):
  - `capture_wait`: default 30 s, cap 45 s;
  - registry: `MaxJobs` 32, `MaxJobBytes` 4 MiB; lookup pool 2 workers, queue 16;
  - heartbeat 2 s, stale after 10 s; stranded-rollback retry every 30 s;
  - target recheck 1 s; size, exit and staging poll 250 ms;
  - stdio queue 8 requests, overload code `-32000`; writer 64 messages and 4 × `MaxResponseBytes`;
  - the §6.1 limits table, with its defaults and compiled ceilings.
- **Wording (Codex #418).** Never call the manifest link + fsync window "bounded". Errors after a successful manifest link (directory fsync, staging, intent or heartbeat removal) follow the committed path: they are logged and the job is `done`.
- **Privileged runs** (memory notes):
  - `setcap` the binary; do not sudo-wrap test runs. Never run a setcap'd binary from `/tmp` (nosuid): build it in the worktree or under `~/`.
  - Gates check capabilities (file xattr and process status), never `Getuid`.
  - `kernel_stacks` needs `cap_syslog`.
  - The setcap'd perf-agent links blazesym statically (`make build`).
- Linux only: `capture_other.go` disables capture elsewhere. The kernel floor is 6.2.
- Comments say what the code does and why: no issue numbers, review IDs or ruling IDs. A spec section number (§7.4) is fine in a test's doc comment.
- **Commits.**
  - Package prefix (`capture: …`, `perfmcp: …`, `mcpstdio: …`, `perf-agent-mcp: …`, `docs: …`).
  - No Co-Authored-By, no AI attribution, no session URLs (POLICY.md). Never commit `CLAUDE.md`.
  - Push only in Task 19, as a draft PR. **Never merge**; ask the user before any merge.
- **Model routing:** Tasks 6, 7, 10, 13 and 16 are load-bearing (process supervision, the file protocol, the state machine). Route them, and their reviews, to opus.

## Review Focus

1. **`command: ["./bench"]` with `cwd`.** This is the most likely way an agent names its benchmark. The spec allows only an absolute path or a bare name. Expected: a synchronous `invalid_argument` whose `next` gives the absolute path, joined with `cwd` as a string (no I/O): "pass `/home/u/proj/bench`". Test in Task 14 (`TestCaptureRelativeCommandSuggestsTheAbsolutePath`).
2. **A command that exits or daemonizes before perf-agent attaches** (a wrapper that forks the real server and returns). Expected: `target_exited`, whose `next` says the launched PID ended after X s and that a daemon is captured with `pid`. The command's output tail is included. Test in Task 7 (`TestRunForkAndExitIsTargetExitedWithAPIDHint`).
3. **A workload that floods stdout or stderr, or writes invalid UTF-8.** Expected: the workload never stalls on a full pipe, so `profiled_duration` stays ≈ `duration`. The tail is at most `MaxOutputTail` bytes, cut on a rune boundary, and the result is valid JSON. Test in Task 7 (`TestRunWorkloadFloodNeverStallsAndTailIsValidUTF8`).
4. **`compare_with` naming a failed or interrupted capture** (the agent passes the id it just got back). Expected: `unknown_baseline`, which names the baseline's outcome ("003 failed: target_exited; it has no profile") and lists the series' last `ok` ids. Test in Task 13 (`TestCompareWithAFailedCaptureIsRefusedNamingItsOutcome`).
5. **A profiles root on a filesystem that refuses hard links.** Expected: the job fails in `preparing`, before perf-agent starts, with the M2 hard-link advice: a no-replace link probe inside the new staging directory, removed by identity. It never fails at step 10 after a full capture. Test in Task 10 (`TestStagingProbeRefusesAFilesystemWithoutHardLinks`).

## Builder's calls settled here

These are places where the spec leaves a choice, or where two of its sentences conflict. Each line is binding for the executor.

| Call | Decision | Reason |
|---|---|---|
| Fake perf-agent | A separate `main` package (`internal/capture/capturetest/fakeperfagent`), built once per test binary. It reads its mode from `fake-mode.json` beside its own path. Each test gets a hard link of it in its own directory | §10's "TestMain checks `PERFMCP_FAKE_PERF_AGENT=1`" cannot work: §5.3 gives perf-agent exactly `PATH` and `LC_ALL`, so no env var reaches it. The trust seam (`Config.Capture.Verify`) accepts the fake |
| Waiting for children | The reaper starts children with `exec.Cmd.Start`, never calls `Cmd.Wait`, and reaps everything with one `wait4(-1)` loop dispatched by PID. Pipes are passed as `*os.File`, so no copy goroutines are needed | §5.3's "`exec.Cmd.Cancel` and `WaitDelay`" predates the reaper (CAP-R2b). `Cmd.Wait` would race the subreaper loop |
| Admission vs I/O | `capture` checks only what needs no I/O: shapes, ranges, `kernel_stacks` against the binary's cached caps, `gpu` without `--gpu-shim` (`disabled`), the in-process slot (`busy`), owner mismatch, and the budget windows (`budget`). `cwd` containment, `command[0]` resolution and file checks, `MaxKeptPerSeries`, `compare_with` (`unknown_baseline`, `incomparable`) and the GPU maps check are **async** failures from `preparing` | §4.0: "pure checks, no I/O"; the windows are in-memory |
| Before the lease | Creating `<captures>` (0700, `MkdirAll` through the profiles root) and writing `.owner` (no-replace) come before the lease, since `.lock` lives inside the directory. Both are idempotent. The read-only resolution above also precedes it | §7.4 step 1, "before any filesystem side effect", is otherwise unsatisfiable on first use |
| Start-up recovery and retention | A background maintenance goroutine after `New`, under the in-process `dirMu` and a non-blocking lease. It is skipped, and logged, when the lease is busy. It never delays `initialize` | Codex's `startup_timeout_sec` is 10 s; hashing a 4 GiB directory cannot sit in `New` |
| Failure manifests | A failure before step 4 (no id) leaves no manifest; `capture_wait` by job id reports it. From step 4 on, every failure ends in a failure manifest | A manifest needs an id |
| `show`, `publication` | Not in stdio's schema or result. Serve adds them through the derivation hook | §4.2 says "serve mode with the live page only"; an accepted-but-ignored argument would mislead |
| `capture_cancel` of an unknown id | Registry only. A capture id this process is not running gives `unknown_capture`, with the next step "capture_list shows it; only a capture this server runs can be cancelled" | CAP-R10: the control path does no file I/O |
| Job ids | `j<N>-<4 hex>`, the hex being a per-process nonce chosen at `New` | A loop that restarts the server must not get another process's `j1` |
| `prctl` constants | `syscall` lacks them, so `launch_linux.go` defines `prSetChildSubreaper = 36` and `prSetNoNewPrivs = 38`. The netscan rule matches those identifiers | Keeps x/sys out of the binary |
| `Capture.TTL` 0 | Allowed: it disables the TTL. `checkLimits` gets the same exception as `Source.MaxContext` | §7.3, "`0` disables the TTL" |
| Cgroup | Created by `Run` (`CreateCgroup`) before the reaper starts; the reaper opens it for `CgroupFD`, kills through `cgroup.kill` and removes it after reaping. `Run` removes it if the reaper never started | One owner at each stage; the reaper outlives a dead server |
| Phases | `status` is the §4.0 state. `phase` is one of `lease`, `recovery`, `retention`, `staging`, `launching` (preparing); `attaching`, `profiling`, `finishing` (running); `validating`, `linking`, `manifest` (committing); `deriving` (done with `derived: pending`) | §4.0 shows `phase` without values |
| `eta`, `elapsed` | Go duration strings, rounded to 100 ms. `eta` is the expected remaining time, assuming `duration` + 5 s overhead | — |
| Compare views | For each mode, the new profile's default view, when the baseline has a view of the same name (otherwise a note). Each entry is the `diff` tool's own result for `limit = CompareRows`, plus `next`, the same call without `limit` | "the first `CompareRows` rows of the existing diff tool, same shape" |
| Recompute recipe | `{"steps": [ {tool, arguments, bind?} … ], "baseline": "present" \| "<id> was removed by retention" \| "none"}`. Every `open` step binds its handle to a name (`"bind": "new_cpu"`, `"base_cpu"`), and `diff` arguments use `"${base_cpu}"`. The baseline's `open` steps come first. When the baseline is gone, its steps and the diff are omitted and `baseline` says why | Codex #418 carries: it includes the baseline open, binds handles, and reports a baseline removed by retention |
| Handles in `capture_list` | From an atomic snapshot (path → handle) that the store publishes on every change, under the engine | §4.6 wants the handle, and `capture_list` never takes the engine |

## Seams for serve (leave them; do not build serve)

- `engine` is a one-slot `chan struct{}` with `acquire(ctx)` and `release()`, the serve design's §D2.4 "engine slot" exactly. `tools()` wraps every handler but the four capture tools.
- The registry's active slot is an `atomic.Pointer[job]` claimed by CAS, and the registry lives on `Server`, not on a session. `job` holds no session or transport reference.
- `Config.Capture.Owner` (`"stdio"` default; serve passes `"serve"`) and `Config.Capture.Dir`, whose default serve overrides with `captures-serve`.
- `mcpstdio.Tool.Control`: the serve transport's control class reads the same flag.
- `Server.capturePublish func(j *job, r *captureResult) error`, nil in stdio: the post-commit publication hook.
- The two internal modes are routed by `capture.RunInternal()`, which any binary embedding `perfmcp` calls first in `main`.

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/capture/{doc.go,errors.go,caps.go,verify.go}` | error codes; `vfs_cap_data`; binary, shim and self checks | 1 |
| `internal/capture/argv.go`, `capture_contract_test.go` (perf-agent root) | settings, the fixed flag list, the argv builder, readiness format; the contract test | 2 |
| `internal/capture/{target.go,command.go}` | preflight, recheck, start time, build-id, GPU maps; `command[0]` resolution, workload env | 3 |
| `internal/capture/capturetest/{capturetest.go,fakeperfagent/main.go,workload/main.go}` | test binaries and helpers | 4 |
| `internal/capture/lock.go` | the captures `flock` and its holder record | 5 |
| `internal/capture/{internal.go,reaper.go,exec_linux.go,launch_linux.go,cgroup_linux.go,capture_other.go}` | internal modes, reaper, launcher, cgroup | 6 |
| `internal/capture/run.go` | one capture, server side | 7 |
| `internal/mcpstdio/{server.go,protocol.go,writer.go}` | reader, queue, control, writer, progress | 8 |
| `internal/perfmcp/{limits.go,config.go,engine.go,server.go,tool_open.go,tool_diff.go,output.go}`, `cmd/perf-agent-mcp/main.go` (checkLimits only) | limits, config, engine lock, extractions | 9 |
| `internal/perfmcp/{capture_layout.go,capture_manifest.go,capture_commit.go,capture_recovery.go}` | series layout, `.seq`, `.owner`, manifests, intent/commit, rollback, recovery | 10 |
| `internal/perfmcp/capture_retention.go` | retention | 11 |
| `internal/perfmcp/capture_registry.go` | registry LRU, lookup pool, windows, decision word | 12 |
| `internal/perfmcp/{capture_job.go,capture_compare.go}` | the job state machine; derivations and recompute | 13 |
| `internal/perfmcp/{tool_capture.go,tool_capture_list.go,capture_wire.go}` | the four tools and their wire types | 14 |
| `cmd/perf-agent-mcp/{main.go,main_test.go,netscan_test.go}` | flags, routing, forced exit, network scan | 15 |
| `internal/perfmcp/capture_process_test.go` | multi-process and crash tests | 16 |
| `internal/perfmcp/guide.md`, `guide_test.go`, `skills/perf-agent-mcp/SKILL.md`, `docs/mcp.md`, `CHANGELOG.md` | docs | 17 |
| `internal/perfmcp/capture_real_test.go` | gated privileged capture | 18 |

Every test package that starts a capture has a `TestMain` that calls `capture.RunInternal()` first: the test binary is `/proc/self/exe`, and so it is the reaper and the launcher.

---

### Task 1: Error codes, file capabilities and the binary verifier

**Files:**
- Create: `internal/capture/doc.go`, `errors.go`, `caps.go`, `verify.go`, `caps_test.go`, `verify_test.go`. The cap fixtures are Go byte literals in the tests; there is no binary testdata.

**Interfaces:**
- Consumes: nothing.
- Produces:
  - **Errors.**
    - `type Code string`, with one constant per §4.4 row: `CodeDisabled` … `CodeStuckChild`, `CodeCancelled`, using exactly the spec's strings.
    - `var DefaultRetryable = map[Code]bool{…}`, from §4.4's column. `perf_agent_failed` defaults to `true` and is false for capability stems.
    - `type Error struct { Code Code; Message, Next string; Retryable bool; RetryAfter time.Duration; PerfAgentExit, StderrTail, Changed string; Survivors []Survivor }`, with `func (e *Error) Error() string`.
    - `func Errorf(code Code, next, format string, args ...any) *Error`, which sets `Retryable` from `DefaultRetryable`.
    - `type Survivor struct { PID int; Comm string; StartTime uint64; State string }`.
  - **Capabilities.**
    - `type Capability uint` (`CapDACOverride` 1, `CapDACReadSearch` 2, `CapFowner` 3, `CapSetuid` 7, `CapSysModule` 16, `CapSysPtrace` 19, `CapSysAdmin` 21, `CapSyslog` 34, `CapPerfmon` 38, `CapBPF` 39, `CapCheckpointRestore` 40).
    - `type CapSet uint64`, with `Has(Capability) bool` and `Names() []string` (`"cap_bpf"` …, in bit order).
    - `func ParseFileCaps(b []byte) (CapSet, error)`. It applies §5.1's rules: revision 2 or 3; `rootid` 0 for revision 3; the effective flag set; the inheritable set empty; the length exact for the revision.
  - **The binary.**
    - `type Identity struct { Dev, Ino uint64; Size int64; Mtime, Ctime int64; Xattr [32]byte }`, where `Xattr` is the SHA-256 of the raw `security.capability` bytes (zero when absent).
    - `func IdentityOf(path string) (Identity, error)`.
    - `type Binary struct { Path string; ID Identity; Caps CapSet; Version, Revision string; Warnings []string }`.
    - `func VerifyBinary(path string) (Binary, error)` runs every §5.1 row in order: absolute; `EvalSymlinks` once; regular, executable, not setuid or setgid; `mode&022 == 0`; owner root or `os.Getuid()`; `Statfs` flags without `MS_NOSUID|MS_NOEXEC` ("copy it outside /tmp, e.g. to the worktree or ~/bin, and setcap it there"); required caps `cap_bpf cap_perfmon cap_sys_ptrace cap_checkpoint_restore`; allowed extras `cap_syslog cap_sys_admin cap_dac_read_search`, the last with a warning; any other cap refused by name; buildinfo main module `github.com/dpsoft/perf-agent` with the main package path equal to it; a `DT_NEEDED` `*blazesym*` entry giving a warning.
    - The error for missing caps quotes `const SetcapLine = "setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep <path>"`, from `docs/requirements.md`.
    - `func (b Binary) Unchanged() *Error` re-stats and re-reads the xattr, giving `binary_changed` ("perf-agent changed since start-up; restart perf-agent-mcp").
  - **The shim and the server itself.**
    - `type Shim struct { Path string; Dev, Ino uint64 }` and `func VerifyShim(path string) (Shim, error)`: absolute, regular, not group- or other-writable.
    - `func CheckSelf(status []byte) error`: from `/proc/self/status` bytes, `Uid:` effective ≠ 0, and `CapEff:` and `CapPrm:` both zero.

- [ ] **Step 0: Set up the branch** (once, before this task)

```bash
git switch -c ai-mcp-m2c-capture   # from the trimmed ai-mcp-m2b head
mkdir -p docs/superpowers/specs docs/superpowers/plans
cp docs/superpowers/specs/2026-11-15-capture-design.md docs/superpowers/specs/
cp .superpowers/capture/2026-11-15-capture-plan.md docs/superpowers/plans/
git add docs/superpowers/specs/2026-11-15-capture-design.md docs/superpowers/plans/2026-11-15-capture-plan.md
git commit -m "docs: the capture design and its implementation plan"
```

Then check the base: `go test ./cmd/perf-agent-mcp -run TestRunRegistersNoPageTools` passes, and stdio `tools/list` has nine tools and no `page_*`. If it does not, stop: the trim has not landed.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseFileCapsAcceptsV2AndV3RootID0(t *testing.T)       // fixtures: v2 +ep {bpf,perfmon,sys_ptrace,checkpoint_restore}; v3 rootid 0 → same CapSet; Names() == the four, in bit order
func TestParseFileCapsRefuses(t *testing.T)                    // table: v3 rootid 1000; no effective flag; inheritable non-zero; truncated (19 bytes); oversized v2 (24 bytes); magic 0x01000000 → each error names the rule
func TestVerifyBinaryCapabilityPolicy(t *testing.T)            // pure policy fn checkCaps(CapSet): missing cap_checkpoint_restore → error naming it and quoting SetcapLine; +cap_dac_override → refused naming it; +cap_syslog/+cap_sys_admin ok; +cap_dac_read_search ok with one warning
func TestVerifyBinaryStatRules(t *testing.T)                   // temp files: relative path refused; group-writable refused; setuid bit refused; directory refused; a symlink is resolved and Path is the target
func TestVerifyBinaryRefusesNosuidMount(t *testing.T)          // a file in os.TempDir(); skip unless Statfs of /tmp has MS_NOSUID; error contains "copy it outside /tmp"
func TestVerifyBinaryReadsBuildInfo(t *testing.T)              // the test binary itself: refused "not perf-agent" (main path ends in .test); a tiny go-built program with module example.com/x: refused, naming the module
func TestBinaryUnchangedSeesAReplacement(t *testing.T)         // checkIdentity on a temp file: append one byte → binary_changed; touch mtime → binary_changed
func TestCheckSelfRefusesRootAndCapabilities(t *testing.T)     // status fixtures: Uid 0 → error; CapEff 0000000000000400 → error; CapPrm non-zero → error; all zero, uid 1000 → nil
func TestVerifyShim(t *testing.T)                              // relative refused; other-writable refused; regular 0644 ok with Dev/Ino of Stat
func TestEveryCodeHasADocumentedRetryable(t *testing.T)        // every Code constant (parsed from errors.go with go/parser) is a DefaultRetryable key; values match §4.4 (busy, budget, target_changed, target_exited, pid_reused, start_timeout, finish_timeout, no_output, perf_agent_failed true; the rest false)
```

`VerifyBinary` is split so that the cap policy (`checkCaps`) and the identity check (`checkIdentity`) are testable without file capabilities. No unprivileged test can set them.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/`
Expected: FAIL, because the package does not exist.

- [ ] **Step 3: Implement `errors.go`, `caps.go` and `verify.go`** with the signatures above. `syscall.Getxattr` reads the xattr (`ENODATA` means no caps), `syscall.Statfs` gives the mount flags, and `debug/buildinfo.ReadFile` and `debug/elf` `ImportedLibraries` do the rest. No x/sys.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/ && go vet ./internal/capture/`
Expected: PASS. The nosuid test passes or skips, giving its reason.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: error codes, file capabilities and the perf-agent binary verifier"
```

### Task 2: The argv builder and the contract test

**Files:**
- Create: `internal/capture/argv.go`, `internal/capture/argv_test.go`, `capture_contract_test.go` (perf-agent root, `package main`, test-only)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - **Settings.**
    - `type Mode string`: `ModeCPU = "cpu"`, `ModeOffCPU = "offcpu"`, `ModeGPU = "gpu"`.
    - `type Settings struct { Modes []Mode; Duration time.Duration; SampleRate int; Unwind string; KernelStacks bool }`.
    - `func (s Settings) Canonical() Settings`, with modes in the order cpu, offcpu, gpu.
  - **The argv.**
    - `type ArgvSpec struct { Binary string; PID int; Settings Settings; Staging string; GPUShim string; CaptureID string }`, where `Staging` is absolute.
    - `func BuildArgv(a ArgvSpec) []string`. `argv[0]` is `a.Binary`, then exactly §5.2's flags in its order. Values are separate arguments (`"--pid", "41234"`), and `--duration` is `Duration.String()`. The outputs are `Staging/cpu.pb.gz`, `Staging/offcpu.pb.gz`, `Staging/gpu.pb.gz` and `Staging/gpu.execlog`, and the last pair is `--tag`, `perf_agent_mcp_capture=<CaptureID>`.
  - **The flag list.** `type Flag struct { Name string; Bool bool }` and `var Flags = []Flag{{"pid",false},{"duration",false},{"sample-rate",false},{"unwind",false},{"no-debuginfod",true},{"profile",true},{"profile-output",false},{"offcpu",true},{"offcpu-output",false},{"gpu",true},{"gpu-shim",false},{"gpu-output",false},{"gpu-executions-output",false},{"kernel-stacks",true},{"tag",false}}`. This is the only list of flags the builder may emit.
  - **Files and environment.**
    - `func OutputFile(m Mode) string` and `const ExeclogFile = "gpu.execlog"`.
    - `func ExpectedFiles(modes []Mode) []string`: each mode's file, plus the execlog for gpu.
    - `var PerfAgentEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}`.
  - **Readiness.** `const ReadinessFormat = "Collecting data for %v (Ctrl+C to stop early)..."` and `const ReadinessPrefix = "Collecting data for "`.

- [ ] **Step 1: Write the failing tests**

```go
func TestBuildArgvEveryCombination(t *testing.T)   // modes ∈ all 7 non-empty subsets × kernel_stacks ∈ {f,t} × unwind ∈ {auto,fp,dwarf}: every "--x" token's name ∈ Flags; contains "--no-debuginfod" and "--duration"; never any of §5.2's "never emitted" list (table literal: "-a","--all","--pmu",…,"--gpu-per-execution-samples"); each Bool flag is followed by another flag or the end; "--profile" iff cpu ∈ modes, likewise offcpu/gpu; gpu adds --gpu-shim SHIM and --gpu-executions-output S/gpu.execlog
func TestBuildArgvExact(t *testing.T)              // {Binary:"/b/perf-agent", PID:41234, cpu, 10s, 99, "auto", Staging:"/p/captures/s/.staging-ab", CaptureID:"s/001"} == []string{"/b/perf-agent","--pid","41234","--duration","10s","--sample-rate","99","--unwind","auto","--no-debuginfod","--profile","--profile-output","/p/captures/s/.staging-ab/cpu.pb.gz","--tag","perf_agent_mcp_capture=s/001"}
func TestCanonicalOrdersModes(t *testing.T)        // {gpu,cpu} → {cpu,gpu}
```

In the perf-agent root (`capture_contract_test.go`):

```go
func TestCaptureContractFlagsExist(t *testing.T)     // for each capture.Flags f: flag.Lookup(f.Name) != nil; IsBoolFlag() of its Value == f.Bool
func TestCaptureContractReadinessLine(t *testing.T)  // main.go and telemetry.go each contain the Go literal `"` + capture.ReadinessFormat + `\n"` and a fmt.Printf of it with *flagDuration
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Argv|Canonical'`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement `argv.go`.**

- [ ] **Step 4: Run them to verify they pass, the root package included**

Run: `go test -race ./internal/capture/ && CGO_CFLAGS=… CGO_LDFLAGS=… go test -race -run TestCaptureContract .`
Expected: PASS. `git diff --stat main -- main.go telemetry.go perfagent gpu cmd/flamegraph internal/profilemodel` is empty.

- [ ] **Step 5: Commit**

```bash
git add internal/capture/argv.go internal/capture/argv_test.go capture_contract_test.go
git commit -m "capture: the one perf-agent argv builder, pinned to perf-agent's flags by a contract test"
```

### Task 3: Targets and commands

**Files:**
- Create: `internal/capture/target.go`, `command.go`, `target_test.go`, `command_test.go`

**Interfaces:**
- Consumes: Task 1 `Error`, `Shim`, `ParseFileCaps`.
- Produces:
  - **`/proc`.**
    - `type Proc struct { Root string; StartTimeHook func(pid int, start uint64) uint64; owner func(path string) (int, error) }`. `DefaultProc()` has `Root: "/proc"`. `owner` is an in-package test seam; nil means `Stat`'s uid.
    - `func StartTime(p Proc, pid int) (uint64, error)` reads `stat` field 22, parsed after the last `)`, and applies `StartTimeHook`.
    - `func Alive(p Proc, pid int, start uint64) bool`.
  - **The preflight.**
    - `type SelfID struct { UID, GID, PID int; Exclude func(pid int) bool }`.
    - `type TargetState struct { PID int; StartTime uint64; UIDs, GIDs [4]int; StatusOwner int; CapPrm uint64; Comm, Exe, BuildID string }`, where `Exe` is the readlink target.
    - `func Preflight(p Proc, pid int, self SelfID) (TargetState, *Error)` applies §6.3, in this order:
      - no such PID → `target_not_found`;
      - pid 1, the server, or `Exclude` → `target_refused`;
      - `readlink exe` fails → `target_refused`: "not dumpable, a kernel thread, or not yours";
      - the `status` owner ≠ uid → `target_refused`;
      - any of the four uids or the four gids ≠ the server's → `target_refused`, naming which;
      - `CapPrm` ≠ 0 → `target_refused`.

      It never stats the `/proc/<pid>` directory.
    - `func (was TargetState) Compare(now TargetState) *Error`: `pid_reused` when the start time differs, otherwise `target_changed` with `Changed` naming the fields ("dumpable", "gid", "capabilities").
  - **Build-id and the GPU maps check.**
    - `func ReadBuildID(p Proc, pid int) string`: the `.note.gnu.build-id` of `/proc/<pid>/exe` as hex, or `""`.
    - `func CheckShimMapped(p Proc, pid int, s Shim) *Error`: a `maps` line whose `major:minor` (hex) and inode equal the shim's, else `gpu_not_injected` with §8's text.
  - **Commands.**
    - `func ResolveCommand(name, pathEnv string, allow []string) (string, *Error)`:
      - an absolute name is used as is;
      - a bare name (no `/`) is looked up in `pathEnv`'s absolute entries, via `Stat`: regular and executable;
      - anything else is `invalid_argument`;
      - with `allow` non-empty, the result must equal an entry.

      No `exec.LookPath`.
    - `func CheckCommandFile(path string) *Error`: setuid, setgid or a `security.capability` xattr → `invalid_argument`, naming which.
    - `func WorkloadEnv(environ []string, shim string) []string`: §6.4's allow-list in input order, plus `CUDA_INJECTION64_PATH=shim` when shim ≠ "".

- [ ] **Step 1: Write the failing tests** (children are `exec.Command` of the test binary's own helper mode, `TestMain`-dispatched by `PERFMCP_TEST_CHILD=sleep|nondumpable|setresgid`)

```go
func TestPreflightAcceptsAChildOfOurs(t *testing.T)             // a sleeping child: ok; StartTime == StartTime(p, pid); Comm == the child's comm; BuildID non-empty for the Go test binary
func TestPreflightRefuses(t *testing.T)                         // pid 1 → target_refused; os.Getpid() → target_refused; Exclude(pid)=true → target_refused; pid 2 (kthreadd) → target_refused "kernel thread"; an exited, reaped PID → target_not_found
func TestPreflightRefusesANonDumpableChild(t *testing.T)        // child calls prctl(PR_SET_DUMPABLE,0) (x/sys in the test only) then signals ready → target_refused mentioning dumpable
func TestPreflightNeverConsultsTheProcDirOwner(t *testing.T)    // Proc.owner seam: returns 0 for "<root>/<pid>" and our uid for "<root>/<pid>/status" → accepted; returns our uid for the dir and 0 for status → refused; the seam records that the dir path was never asked
func TestCompareNamesWhatChanged(t *testing.T)                 // start time differs → pid_reused; GIDs[1] differs → target_changed, Changed contains "gid"; equal → nil
func TestStartTimeHookFeedsReuse(t *testing.T)                  // hook adds 1 → StartTime differs from the first read
func TestCheckShimMapped(t *testing.T)                          // a fake proc root whose maps names the shim's dev:ino → nil; same path but another inode → gpu_not_injected with "CUDA_INJECTION64_PATH"
func TestResolveCommand(t *testing.T)                           // "/bin/true" as is; "true" with PATH "/usr/bin:/bin" → its absolute path; "./x" and "bin/x" → invalid_argument; PATH entry "relative" skipped; allow ["/bin/true"] with "/usr/bin/true" → invalid_argument naming --capture-command
func TestCheckCommandFileRefusesSetuidAndCaps(t *testing.T)     // temp file chmod 04755 → refused "setuid"; 02755 → "setgid"; plain → nil
func TestWorkloadEnvIsTheAllowList(t *testing.T)                // input with OTEL_X, DEBUGINFOD_URLS, GODEBUG, KUBERNETES_POD_NAME, LC_ALL, HOME → exactly LC_ALL, HOME (input order); shim adds CUDA_INJECTION64_PATH last
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Preflight|Compare|StartTime|Shim|Resolve|CommandFile|WorkloadEnv'`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement `target.go` and `command.go`.** Build-ids are read with `debug/elf` `Section(".note.gnu.build-id")`, after the note's 16-byte header.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: the ptrace-like target preflight, its recheck, and command resolution"
```

### Task 4: Test binaries

**Files:**
- Create: `internal/capture/capturetest/capturetest.go`, `capturetest/fakeperfagent/main.go`, `capturetest/workload/main.go`, `capturetest/capturetest_test.go`

**Interfaces:**
- Consumes: Task 2 `capture.Flags`, `ReadinessFormat`, `OutputFile`, `ExeclogFile`; `pmfixture.CPUBasic`, `GPUBasic`, `GPUBasicExeclog`.
- Produces (package `capturetest` does **not** import `internal/capture`, so `capture`'s in-package tests can use it):
  - **Building the binaries.** `func Binaries(tb testing.TB) (fake, workload string)` builds both once per test process with `go build -buildvcs=false` into a directory under `os.TempDir()`, and removes it in `Cleanup` (call it from `TestMain` through `Main`). `func Main(m *testing.M, internal func() (int, bool)) int` runs `internal` first, so the caller passes `capture.RunInternal`, then the tests, then the cleanup.
  - **The fake.**
    - `type FakeMode string`: `FakeNormal`, `FakeNeverReady`, `FakeIgnoreSIGINT`, `FakeExit1Stderr` (stderr "failed to set capabilities"), `FakeExit2`, `FakeExit127Loader` (stderr "error while loading shared libraries: libblazesym_c.so"), `FakeNoFile`, `FakeHugeFile` (writes until killed), `FakeSymlinkOutput`, `FakeHardlinkOutput`, `FakeSpamStdout` (4 MiB to each stream), `FakeForkGrandchild`, `FakeHangAfterSIGINT`, `FakeNoGPUSamples`.
    - `type FakeOptions struct { Mode FakeMode; ReadyAfter time.Duration; Record string }`. `Record` is the path where the fake writes its argv and environment as JSON.
    - `func FakePerfAgent(tb testing.TB, o FakeOptions) string` returns a hard link (a copy when linking fails) of the fake in a fresh `tb.TempDir()`, with `fake-mode.json` beside it.
    - The fake parses its argv with a `flag.FlagSet` built from `capture.Flags` (`Bool` → `BoolVar`), exiting 2 on an unknown flag. It prints `fmt.Printf(ReadinessFormat+"\n", d)` after `ReadyAfter`, waits for the duration or SIGINT, and writes `CPUBasic` (or `GPUBasic` plus its execlog) to the requested outputs.
  - **The workload.** `func Workload(tb testing.TB) string`. Argv modes:
    - `busy D`, `sleep D`, `exit-after D CODE`;
    - `fork-and-exit D` (the child sleeps D; the parent exits at once), `double-fork-setsid D`;
    - `drop-dumpable-after A D`, `setresgid-after A D` (to its first supplementary gid; it exits 0 when it has none and prints `skip`);
    - `ignore-sigterm D`;
    - `flood-output D` (writes MiBs, including invalid UTF-8, to both streams);
    - `report` (prints JSON `{nonewprivs, tty_nr, env}` from its own `/proc/self/status` and `stat`);
    - `exec PATH` (it `syscall.Exec`s PATH).
  - **Gate.** `func SkipIfPrivileged(tb testing.TB)` skips when `/proc/self/status` shows euid 0 or a non-zero CapEff/CapPrm (the cap-aware gate).

- [ ] **Step 1: Write the failing tests**

```go
func TestFakeRefusesUnknownFlags(t *testing.T)     // fake --bogus → exit 2
func TestFakeWritesTheRequestedOutputs(t *testing.T) // fake --pid 1 --duration 200ms --sample-rate 99 --unwind auto --no-debuginfod --profile --profile-output D/cpu.pb.gz --tag k=v → stdout first line has prefix "Collecting data for 200ms"; D/cpu.pb.gz bytes == pmfixture.CPUBasic(); exit 0
func TestFakeStopsOnSIGINT(t *testing.T)           // --duration 1m, SIGINT after the readiness line → exits within 1 s and writes the file
func TestFakeRecordsArgvAndEnv(t *testing.T)       // Record set → JSON with argv and env as run
func TestWorkloadReport(t *testing.T)              // workload report → JSON with keys nonewprivs, tty_nr, env
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/capturetest/...`
Expected: FAIL.

- [ ] **Step 3: Implement the helpers and both programs.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/capturetest/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/capture/capturetest
git commit -m "capture: a fake perf-agent and a scripted workload for unprivileged tests"
```

### Task 5: The captures lock

**Files:**
- Create: `internal/capture/lock.go`, `lock_test.go`

**Interfaces:**
- Consumes: Task 1 `Error`; Task 3 `StartTime`.
- Produces:
  - `type Holder struct { ServerPID int; ServerStart uint64; ReaperPID int; ReaperStart uint64; CaptureID, Series string; Deadline time.Time }`, with JSON field names `server_pid`, `server_start`, `reaper_pid`, `reaper_start`, `capture_id`, `series` and `deadline`.
  - `func TryLock(dir *os.Root, h Holder) (*Lease, *Error)` opens `.lock` through `dir` (`O_RDWR|O_CREATE`, 0600) and takes `syscall.Flock(LOCK_EX|LOCK_NB)`.
    - On `EWOULDBLOCK` it reads the holder JSON and returns `busy`: "capture <id> by perf-agent-mcp pid <p> (reaper pid <r>), ends by <deadline UTC>". `RetryAfter` is `Deadline − now`, at least 1 s, and an unreadable holder still gives `busy`.
    - On success it truncates and writes `h`.
  - `func (l *Lease) File() *os.File`, `func (l *Lease) Update(h Holder) error` (truncate and pwrite) and `func (l *Lease) Close() error`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTryLockIsExclusiveAcrossProcesses(t *testing.T)  // a helper child process (TestMain mode PERFMCP_TEST_CHILD=hold-lock DIR) takes the lock and prints "held"; TryLock here → busy naming its pid, capture id and deadline; RetryAfter ≥ 1 s
func TestTheLockSurvivesInAnInheritedDescriptor(t *testing.T) // lease here; pass l.File() to a child as ExtraFiles[0]; Close here; TryLock from a fresh open → still busy; kill the child by recorded PID → TryLock succeeds
func TestTryLockRewritesTheHolder(t *testing.T)           // Update with ReaperPID set → a busy caller names the reaper pid
func TestBusyWithAGarbledHolderStillSaysBusy(t *testing.T) // .lock contains "x" while held → busy, message without holder fields
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Lock'`
Expected: FAIL.

- [ ] **Step 3: Implement `lock.go`.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/capture/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/capture/lock.go internal/capture/lock_test.go
git commit -m "capture: a cross-process captures lock that names its holder"
```

### Task 6: The reaper and the launcher (opus)

**Files:**
- Create: `internal/capture/internal.go`, `reaper.go`, `exec_linux.go`, `launch_linux.go`, `cgroup_linux.go`, `capture_other.go`, `reaper_test.go`, `sweep_test.go`, `cgroup_test.go`, `main_test.go` (`TestMain`: `os.Exit(capturetest.Main(m, RunInternal))`)

**Interfaces:**
- Consumes: Task 1 `Identity`, `IdentityOf`, `Survivor`; Task 2 `PerfAgentEnv`; Task 3 `StartTime`; Task 4 binaries.
- Produces:
  - **Mode routing.**
    - `const InternalEnv = "PERF_AGENT_MCP_INTERNAL"`.
    - `func RunInternal() (code int, handled bool)`. It returns `handled` only when `InternalEnv` is `reaper` or `launcher` **and** fd 3 is open. It is the first call of every `main` and `TestMain` that may be re-exec'd.
    - `const Supported = true` here, `false` in `capture_other.go` (`//go:build !linux`), where `StartReaper` returns an error.
  - **Specs.**
    - `type ReaperSpec struct { PerfAgent string; PerfAgentID Identity; Launch *LaunchSpec; Deadline, StopGrace time.Duration; Cgroup string; FaultPause string }`. `FaultPause` is a test hook: `"after-perf-agent-fork"` or `"after-launcher-fork"` makes the reaper sleep there until killed, and `"no-subreaper"` skips `setChildSubreaper`. The server sets it only from a test seam.
    - `type LaunchSpec struct { Path string; Args, Env []string; Dir string }`.
    - `type ReaperFiles struct { Lock, PerfOut, PerfErr, WorkOut, WorkErr *os.File }`.
  - **The handle.**
    - `func StartReaper(self string, spec ReaperSpec, f ReaperFiles) (*Reaper, error)`. It starts `self` with env exactly `InternalEnv=reaper`, `Setsid`, no `Pdeathsig`, and stdin `/dev/null`. The fd layout:

      | fd | Carries |
      |---|---|
      | 3 | the spec (a pipe) |
      | 4 | commands (read) |
      | 5 | events (write) |
      | 6 | the lock |
      | 7, 8 | perf-agent's stdout and stderr |
      | 9, 10 | the workload's stdout and stderr |

      The server calls `Cmd.Wait` for the reaper in a goroutine.
    - `func (r *Reaper) Send(c Command) error`, `Events() <-chan Event`, `PID() int`, and `Kill()`. `Kill` is a single `kill(2)` per recorded pgid (perf-agent's, then the workload's), then the reaper's PID. It never blocks.
  - **The protocol** (JSON lines):
    - `type Command struct { Op string; Argv []string; Dir string }`, with `Op` ∈ {`attach`, `stop`, `cancel`};
    - `type Event struct { Op string; Who string; PID, PGID int; StartTime uint64; Status int; Signal string; Survivors []Survivor; Cleanup string; Err string }`, with `Op` ∈ {`launched`, `attached`, `exited`, `reaped`, `error`} and `Who` ∈ {`perf-agent`, `workload`}.
  - **Reaper behaviour, in order:**
    1. `CloseOnExec` every inherited fd ≥ 4.
    2. `setChildSubreaper()`; a failure is noted for `Cleanup`.
    3. With `Launch`, start the launcher: `self` with `InternalEnv=launcher` and its spec on fd 3; `Setsid`; `Dir`; stdin `/dev/null`; stdout and stderr on fds 9 and 10; `CgroupFD` when `Cgroup` ≠ "". Send `launched`.
    4. On `attach`: `IdentityOf(PerfAgent) == PerfAgentID`, else `error` "binary changed". Start perf-agent with `Path` = `PerfAgent`, `Args` = `Argv`, `Dir`, `Env` = `PerfAgentEnv`, `Setpgid`, stdin `/dev/null`, stdout and stderr on fds 7 and 8. Send `attached`.
    5. Run one `wait4(-1)` loop, sending `exited` for each known child.
    6. `stop`: SIGINT to perf-agent's group. `cancel`: SIGINT, then SIGKILL after `StopGrace`.
    7. When perf-agent has exited, end the workload: `cgroup.kill` when there is a cgroup; otherwise SIGTERM to its group, SIGKILL after `StopGrace`, and the sweep.
    8. EOF on fd 4 before `reaped`, or the absolute `Deadline` since start: SIGKILL both groups, `cgroup.kill`, and the sweep.
    9. Wait up to `StopGrace` for the groups, the cgroup and adopted descendants to be gone.
    10. Send `reaped` with `Survivors` and `Cleanup` (`complete` or `incomplete: <reason>`), remove the cgroup, close the lock fd **last**, and exit 0.
  - `func sweep(children func() []int, kill func(pid int, sig syscall.Signal) error, alive func(pid int) bool, grace time.Duration, now func() time.Time) []int`: kills every PID whose PPid is the reaper (from `/proc/*/stat` field 4) until none is left or `grace` passes. It returns the survivors.
  - **The launcher.** `launch(spec LaunchSpec) int` runs `runtime.LockOSThread()`, then `noNewPrivs()`, then `syscall.Exec(spec.Path, spec.Args, spec.Env)`. `launch_linux.go` defines `prSetChildSubreaper = 36` and `prSetNoNewPrivs = 38`, and both `noNewPrivs()` and `setChildSubreaper()` are `syscall.RawSyscall(syscall.SYS_PRCTL, <const>, 1, 0)`.
  - **The cgroup.**
    - `func DelegatedCgroup(selfCgroup []byte, mount string) (parent, why string)`: the `0::<path>` line, with `mount+path` writable (`syscall.Access` W_OK) and `cgroup.kill` present.
    - `func CreateCgroup(parent, name string) (string, error)` (`Mkdir` 0755).
    - `func KillCgroup(path string) error` and `func RemoveCgroup(path string) error`.
  - **`exec_linux.go`** holds exactly two `exec.Command` calls: `exec.Command(spec.PerfAgent, …)` and `exec.Command(self, …)`, through `selfCommand(self, mode string)`. The netscan rule (Task 15) matches the selectors `PerfAgent` and the identifier `self`.

- [ ] **Step 1: Write the failing tests** (they drive `StartReaper` directly, with the Task 4 fake and workload, and `SkipIfPrivileged`)

```go
func TestReaperRunsPerfAgentWithTheExactEnvironment(t *testing.T) // fake with Record: env == PerfAgentEnv exactly; argv == the attach Argv; cwd == Dir; perf-agent pgid == its pid; events: attached then exited status 0 then reaped Cleanup "complete"
func TestReaperIsInItsOwnSession(t *testing.T)                    // /proc/<reaper>/stat session field == reaper pid ≠ ours
func TestTheLauncherSetsNoNewPrivsAndHasNoTTY(t *testing.T)       // workload report: nonewprivs 1, tty_nr 0, env == the LaunchSpec env exactly
func TestLaunchedSetuidProgramGainsNothing(t *testing.T)          // find a setuid-root binary on the system (/usr/bin/passwd, /usr/bin/su); skip if none; workload exec it with --help → its /proc status Uid effective stays ours (read via a spec that runs it under the launcher)
func TestReaperKillsOnEOF(t *testing.T)                           // for each phase: fake never-ready (start-up), normal 1m (profiling), hang-after-SIGINT after a stop (finalisation): close the command pipe → the fake's group is gone within StopGrace (checked by kill(-pgid,0) == ESRCH); the reaper exits
func TestReaperKillsAtTheAbsoluteDeadline(t *testing.T)           // Deadline 500ms, fake 1m, command pipe kept open → group gone by Deadline + StopGrace
func TestReaperCancelIsSIGINTThenSIGKILL(t *testing.T)            // fake ignore-SIGINT; cancel → exited with Signal "killed" after ≈StopGrace (± 300 ms)
func TestReaperEndsTheWorkloadAfterPerfAgent(t *testing.T)        // workload sleep 1m + fake 300ms → workload exited (SIGTERM) and reaped
func TestReaperSweepsSetsidGrandchildren(t *testing.T)            // Cgroup "" ; workload double-fork-setsid 1m → after reaped, the grandchild PID (printed by the workload) is gone; when prctl subreaper fails (seam: spec.FaultPause "no-subreaper"), Cleanup names "process group only"
func TestReaperKillsTheCgroup(t *testing.T)                       // skip unless DelegatedCgroup ok; double-fork-setsid → grandchild gone via cgroup.kill; the cgroup directory removed
func TestTheLockIsReleasedOnlyAfterReaping(t *testing.T)          // pass a held Lease's File; close the server copy; TryLock busy until reaped arrives, free after the reaper exits
func TestReaperForkReportGapStillKillsOnEOF(t *testing.T)         // FaultPause "after-perf-agent-fork" with a never-ready fake: close the command pipe while paused → the fake's group gone within StopGrace; same with "after-launcher-fork" and a sleep workload
func TestSweepReportsSurvivors(t *testing.T)                      // sweep with kill that succeeds but alive() stays true for pid 42 → returns [42] after grace; all dead → []
func TestDelegatedCgroupParsing(t *testing.T)                     // "0::/user.slice/x.scope\n" + a temp mount with a writable dir and cgroup.kill → parent; missing cgroup.kill → why "no cgroup.kill"; v1-only lines → why "not cgroup v2"
func TestRunInternalIgnoresAnUnsetMode(t *testing.T)              // InternalEnv unset → handled false; set but fd 3 closed → handled false
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'Reaper|Launch|Sweep|Cgroup|RunInternal'`
Expected: FAIL.

- [ ] **Step 3: Implement the six files.**

- [ ] **Step 4: Run them to verify they pass, and check that nothing is left behind**

Run: `go test -race -count=3 ./internal/capture/`
Expected: PASS three times. Afterwards, `ps -eo pid,pgid,comm | grep -E 'fakeperfagent|workload'` prints nothing; match on the comm column only, never `pkill -f`.

- [ ] **Step 5: Commit**

```bash
git add internal/capture
git commit -m "capture: a detached reaper that owns both children, and a no_new_privs launcher"
```

### Task 7: One capture, server side (opus)

**Files:**
- Create: `internal/capture/run.go`, `run_test.go`

**Interfaces:**
- Consumes: Tasks 1–6.
- Produces:
  - **The spec.**
    - `type Bounds struct { StartTimeout, FinishTimeout, StopGrace time.Duration; MaxOutputTail int; MaxProfileBytes, MaxExeclogBytes int64; Recheck, Poll time.Duration }`, with `Recheck` 1 s and `Poll` 250 ms.
    - `type RunSpec struct { Self string; Binary Binary; Proc Proc; SelfID SelfID; PID int; Launch *LaunchSpec; Settings Settings; Staging *os.Root; StagingPath string; CaptureID string; GPU *Shim; Bounds Bounds; Lock *os.File; Cgroup string; Log *slog.Logger; FaultPause string }`, with exactly one of `PID` and `Launch`.
  - **The handle.**
    - `func Start(spec RunSpec) (*Run, *Error)`.
    - `func (r *Run) Phase() string` (`attaching`, `profiling`, `finishing`).
    - `func (r *Run) Cancel()`: idempotent; sends `cancel`.
    - `func (r *Run) Kill()`: synchronous; `Reaper.Kill`.
    - `func (r *Run) Wait() Outcome`: blocks until `reaped`, or until the reaper's events end.
  - **The outcome.**
    - `type Tail struct { Stdout, Stderr string }`.
    - `type Outcome struct { Err *Error; StoppedBy string; Target TargetState; Launched bool; CommandPath string; ArgCount int; Started time.Time; AttachLatency, Profiled time.Duration; PerfAgentExit int; StderrTail string; Workload Tail; Survivors []Survivor; Cleanup string; Notes []string; Argv []string }`.
  - **The flow**, which runs entirely in `Run`'s goroutines:
    1. Create the cgroup when `DelegatedCgroup` allows, and start the reaper.
    2. For a launch: on `launched`, `Preflight` the workload PID; a refusal sends `cancel`.
    3. For a PID target: `Preflight`, then `CheckShimMapped` when the modes have gpu.
    4. `BuildArgv` and `attach`.
    5. Stdout readers feed 16 KiB rings and slog debug. The first stdout line with `ReadinessPrefix` fixes `AttachLatency`, measured from `attach`.
    6. Readiness must come within `StartTimeout`, else `start_timeout`. A target or workload exit before readiness gives `target_exited`, with the workload tail and `next` (Review Focus 2).
    7. Recheck: `Compare` at readiness, every `Recheck`, and at the end (`pid_reused` or `target_changed`).
    8. Every `Poll`:
       - the staging file sizes against the bounds: over a limit, `Kill` and `too_large`; a symlink or hard link in staging is caught by the commit's validation;
       - the PID target, through `os.FindProcess` and `Signal(0)`: gone means `stop` with `target_exit`.
    9. A `workload` `exited` event means `stop` with `command_exit`.
    10. Stop is readiness + duration, or the early stop. perf-agent must exit within `FinishTimeout` of it, else `Kill` and `finish_timeout`.
    11. Map the exit by §5.4:
        - 0 → ok;
        - 2 → `perf_agent_refused_args`, with `Binary.Version`;
        - 1 → `perf_agent_failed`. The stems "set capabilities" and "cap_checkpoint_restore" give `Next` = the setcap line and `Retryable` false;
        - 126/127, or "error while loading shared libraries" → `perf_agent_unstartable`; a blazesym library means "link blazesym statically".
    12. A `reaped` event with survivors gives `stuck_child`, with `Survivors` and `Cleanup`.
    13. `StoppedBy` is `duration`, `target_exit`, `command_exit` or `cancel`.
  - **Notes:**
    - "profiling began <AttachLatency> after the command started; the first <AttachLatency> are not in the profile" (launch only);
    - "only pid <N> was profiled, not processes it started";
    - "descendants may survive: neither a delegated cgroup nor a subreaper was available", when `Cleanup` says so;
    - with gpu: "the GPU profile has no samples: no kernel was launched through the adapter during the capture", left to perfmcp, which reads the profile.

- [ ] **Step 1: Write the failing tests** (fake + workload; the staging dir is a `t.TempDir()` sub-root)

```go
func TestRunCapturesAPIDTarget(t *testing.T)                       // workload sleep 1m; fake normal ReadyAfter 100ms; duration 500ms → Err nil, StoppedBy "duration", AttachLatency ≥ 100ms, Profiled ≈ 500ms (±250), staging has cpu.pb.gz
func TestRunLaunchesACommand(t *testing.T)                          // Launch workload busy 3s, duration 500ms → Launched, StoppedBy "duration", the launch note present; workload ended after
func TestRunStopsWhenTheCommandExits(t *testing.T)                  // workload exit-after 300ms 0, duration 10s → StoppedBy "command_exit", Profiled < 1s
func TestRunStopsWhenThePIDExits(t *testing.T)                      // PID target that exits after 300ms → "target_exit"
func TestRunTargetExitedBeforeReadiness(t *testing.T)               // fake ReadyAfter 1s, workload exit-after 100ms → target_exited, retryable, the workload tail included
func TestRunForkAndExitIsTargetExitedWithAPIDHint(t *testing.T)     // Review Focus 2: workload fork-and-exit → target_exited; Next contains "capture it with pid"
func TestRunTimeouts(t *testing.T)                                  // never-ready + StartTimeout 300ms → start_timeout naming "StartTimeout=300ms"; hang-after-SIGINT + FinishTimeout 300ms → finish_timeout; both: the fake's group gone after Wait
func TestRunExitCodes(t *testing.T)                                 // exit-2 → perf_agent_refused_args naming Binary.Version; exit-1-stderr → perf_agent_failed, Retryable false, Next contains "setcap"; exit-127-loader → perf_agent_unstartable "link blazesym statically"
func TestRunSizeKill(t *testing.T)                                  // huge-file, MaxProfileBytes 1 MiB → too_large within 1 s of passing it
func TestRunCancel(t *testing.T)                                    // Cancel during profiling → StoppedBy "cancel", Err cancelled; ignore-SIGINT fake → gone ≈ StopGrace
func TestRunRechecksTheTarget(t *testing.T)                         // workload drop-dumpable-after 600ms 1m, duration 2s → target_changed, Changed "dumpable"; setresgid-after → target_changed "gid" (skip when the workload prints skip)
func TestRunStartTimeHookIsPIDReuse(t *testing.T)                   // Proc.StartTimeHook +1 after the preflight read → pid_reused at readiness; a hook from the end only → pid_reused at the end
func TestRunTransitionAndRevertWithinOneIntervalIsMissed(t *testing.T) // documented limit: the hook changes and restores between two Recheck ticks → Err nil
func TestRunWorkloadFloodNeverStallsAndTailIsValidUTF8(t *testing.T) // Review Focus 3: workload flood-output 2s, duration 1s → Profiled ≈ 1s; len(Workload.Stdout) ≤ MaxOutputTail; utf8.ValidString of both after json.Marshal round trip
func TestRunSpamStdoutStaysInTheRings(t *testing.T)                 // fake spam-stdout → StderrTail ≤ 16 KiB; nothing written to os.Stdout (redirected to a pipe in the test, which stays empty)
func TestRunGPUNeedsTheShimMapped(t *testing.T)                     // PID target without the shim mapping → gpu_not_injected before perf-agent starts (Record file absent)
func TestRunSurvivorsAreStuckChild(t *testing.T)                    // reaper events seam: a reaped event with Survivors [{PID 42}] → stuck_child, Survivors copied, not retryable
func TestRunKillIsSynchronousAndNonBlocking(t *testing.T)           // Kill() returns in < 10 ms with a live fake; the group is gone within StopGrace
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/capture/ -run 'TestRun'`
Expected: FAIL.

- [ ] **Step 3: Implement `run.go`.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=2 ./internal/capture/`
Expected: PASS, with no leftover processes (the same `ps` check as Task 6).

- [ ] **Step 5: Commit**

```bash
git add internal/capture/run.go internal/capture/run_test.go
git commit -m "capture: Run supervises one capture: readiness, bounds, early stop, rechecks and exit mapping"
```

### Task 8: A stdio transport that never waits

**Files:**
- Create: `internal/mcpstdio/writer.go`, `internal/mcpstdio/control_test.go`
- Modify: `internal/mcpstdio/server.go`, `protocol.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - **API additions.**
    - `Tool.Control bool` (`json:"-"`): the handler runs on the reader goroutine, and its response is enqueued at once, ahead of the queued requests. It must not block or do file I/O.
    - `Options.MaxQueued int` (0 means 8), `Options.MaxOutMessages int` (0 means 64), `Options.MaxOutBytes int` (0 means 4 × `MaxToolErrorBytes`, or 4 MiB when that is 0), `Options.DrainTimeout time.Duration` (0 means 1 s) and `Options.OnClose func(CloseReason)`.
    - `type CloseReason int` (`CloseEOF`, `CloseClientGone`, `CloseContext`) and `var ErrClientGone = errors.New("mcpstdio: the client stopped reading its output")`.
    - `const codeServerBusy = -32000`.
    - `func Progress(ctx context.Context, progress, total float64, message string) bool` sends `notifications/progress` when the request carried `_meta.progressToken`, and reports whether it did.
  - **The reader goroutine.**
    - It decodes `method`, `id` and `params` itself.
    - `notifications/cancelled` (`params.requestId`, matched against the running id or a queued id by compact JSON bytes): the running handler's ctx is cancelled, and its response is suppressed. A queued request is dropped without a response. An unknown id does nothing.
    - A `tools/call` of a `Control` tool runs inline.
    - Anything else joins a FIFO of `MaxQueued`. One more gets the immediate error `-32000`, "server busy: 8 requests queued; retry".
    - Parse and oversize errors are enqueued as now.
  - **The dispatcher goroutine** runs one request at a time, in order. Its ctx is cancelled at EOF, at `ctx` done, and by a cancel for it.
  - **The writer goroutine** (`writer.go`) is the only writer of `w`, fed by a queue of `MaxOutMessages` messages and `MaxOutBytes` bytes.
    - An enqueue that does not fit means a dead client: `OnClose(CloseClientGone)`, every handler ctx cancelled, reading stops, and `Serve` returns `ErrClientGone` at once, without waiting for the blocked write.
  - **EOF:**
    1. `OnClose(CloseEOF)`;
    2. the handler contexts are cancelled;
    3. the requests already queued are still answered, in order;
    4. the writer drains for at most `DrainTimeout`;
    5. `Serve` returns nil, or `ErrClientGone` if the drain timed out.

    `ctx` done: `OnClose(CloseContext)` and return nil, as now.

- [ ] **Step 1: Write the failing tests** (`control_test.go`; every existing `server_test.go` test is unchanged)

```go
func TestCancelledEndsTheRunningHandlerWithNoResponse(t *testing.T)   // handler blocks on ctx; notifications/cancelled {requestId: 1} → handler ctx done; no line for id 1; next request answered
func TestCancelForAnotherIDDoesNothing(t *testing.T)
func TestCancelOfAQueuedRequestDropsIt(t *testing.T)
func TestEOFCancelsTheHandlerAndAnswersTheQueue(t *testing.T)          // 1 blocking + 3 queued, then EOF → OnClose(CloseEOF) called once; blocking handler sees ctx done; ids 2,3,4 answered in order; Serve returns nil
func TestOverloadIsImmediateAndOrdered(t *testing.T)                   // during a blocking handler: 20 requests then a cancel for it → exactly 8 answered in order after it, 12 get -32000 at once (before the handler ends), the cancel takes effect; every stdout line is one JSON-RPC message
func TestControlToolsAnswerAheadOfTheQueue(t *testing.T)              // a Control tool called while a 45 s blocking handler runs → its response arrives within 100 ms; the blocked one unaffected
func TestABlockedStdoutMeansADeadClient(t *testing.T)                  // w blocks forever after 1 KiB; MaxOutMessages 4; flood ping → OnClose(CloseClientGone); Serve returns ErrClientGone within 1 s
func TestEOFAndCancelAreSeenWhileStdoutIsBlocked(t *testing.T)         // w blocked; a notifications/cancelled reaches the handler; then EOF → OnClose(CloseEOF), Serve returns within DrainTimeout + 100ms
func TestProgressGoesThroughTheWriter(t *testing.T)                    // _meta.progressToken "p" → Progress returns true and a notifications/progress line {progressToken:"p", progress, total} precedes the response; without a token Progress returns false and writes nothing
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcpstdio/`
Expected: the new tests FAIL; the existing ones PASS.

- [ ] **Step 3: Implement the reader, dispatcher, writer and `Progress`.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=5 ./internal/mcpstdio/ && go test -race ./internal/perfmcp/ ./cmd/perf-agent-mcp/`
Expected: PASS. Every existing perfmcp and cmd test passes unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpstdio
git commit -m "mcpstdio: a reader that never waits, control messages, and one bounded writer"
```

### Task 9: Limits, config, the engine lock and three extractions

**Files:**
- Create: `internal/perfmcp/engine.go`, `engine_test.go`, `capture_limits_test.go`
- Modify: `limits.go`, `config.go`, `server.go` (wrap handlers; the ownership comment), `tool_open.go`, `tool_diff.go`, `output.go`, `cmd/perf-agent-mcp/main.go` (the `checkLimits` exception only)

**Interfaces:**
- Consumes: M2 `handleOpen`, `handleDiff`, `writePage`.
- Produces:
  - **Limits.** `Limits.Capture CaptureLimits`:
    - `type CaptureLimits struct { MaxDuration, StartTimeout, FinishTimeout, TTL, StopGrace, WindowDuration time.Duration; MaxSampleRate, KeepPerSeries, MaxKeptPerSeries, CompareRows, MaxJobs, MaxJobBytes, WindowCaptures, MaxOutputTail int; MaxProfileBytes, MaxExeclogBytes, MaxStoredBytes, MinFreeBytes int64 }`.
    - Defaults, from §6.1: 30 s, 15 s, 30 s, 168 h, 5 s, 30 min; 1000, 10, 4, 20, 32, 4 MiB, 60, 2 KiB; `Model.MaxRawBytes`, `Model.MaxSidecarBytes`, 4 GiB, 1 GiB.
    - `validate` refuses values above the ceilings, naming them: `MaxDuration` 5 min, `MaxSampleRate` 10 000, `StartTimeout` 5 min, `FinishTimeout` 10 min, `StopGrace` 30 s, `MaxOutputTail` 64 KiB, `CompareRows` ≤ `MaxDiffRows`. Every other field must be positive, except `TTL` ≥ 0.
    - `withDefaults` fills each zero field except `TTL`, which takes its default only when the whole `Capture` is zero.
  - **Config.** `Config.Capture CaptureConfig`: `type CaptureConfig struct { PerfAgent, Dir, GPUShim, Owner string; Commands []string; Verify func(path string) (capture.Binary, error) }`. `Dir` "" means `captures` (relative to the profiles root), `Owner` "" means `"stdio"`, and `Verify` nil means `capture.VerifyBinary`; `Verify` is a library seam that no flag reaches.
  - **The engine.**
    - `type engine struct{ slot chan struct{} }`, `func newEngine() *engine`, `func (e *engine) acquire(ctx context.Context) error` and `func (e *engine) release()`.
    - `func (s *Server) engineHandler(h mcpstdio.Handler) mcpstdio.Handler`.
    - `tools()` wraps every M2 tool. The `Server` doc comment now says that the engine's holder owns the store, diffs, attempts and publications, and the live page's HTTP handlers still read only the snapshot.
  - **Extractions.** None changes a result.
    - `func (s *Server) openResultFor(name, execlog string) (openResult, error)`, which `handleOpen` calls after decoding and `rel`;
    - `func (s *Server) diffPage(q diffQuery, limit int, cursor string) (diffResult, error)`, which `handleDiff` calls after decoding and the limit check;
    - `writeTemp` gains a `perm fs.FileMode` parameter (pages 0644);
    - `func (s *Server) linkNoReplace(root *os.Root, tmp, name string) error` returns an error wrapping `fs.ErrExist` when the name is taken;
    - `func (s *Server) publishNew(root *os.Root, dir, base string, b []byte, perm fs.FileMode) (name string, err error)` does `writeTemp`, `beforePublish`, `linkNoReplace` and the temporary's removal. `writePage` uses it.
  - **The store snapshot.** `store.published atomic.Pointer[map[string]profilemodel.Handle]` (profile name → handle) is rewritten after every change to `entries`. `func (s *store) openNames() map[string]profilemodel.Handle` reads it without the engine.
  - `cmd/perf-agent-mcp` `checkLimits` allows `Capture.TTL` = 0.

- [ ] **Step 1: Write the failing tests**

```go
func TestCaptureLimitsDefaults(t *testing.T)            // DefaultLimits().Capture equals the §6.1 table field by field; MaxProfileBytes == Model.MaxRawBytes
func TestCaptureLimitsCeilings(t *testing.T)            // MaxDuration 6m → New error naming "Capture.MaxDuration" and "5m0s"; MaxSampleRate 10001; CompareRows MaxDiffRows+1; TTL 0 → ok; StopGrace 0 with other fields set → refused
func TestTheEngineSerialisesToolsWithAJob(t *testing.T) // test goroutine acquires s.engine; a top call blocks; release → it answers; acquire with a cancelled ctx returns ctx.Err()
func TestOpenAndDiffResultsAreUnchanged(t *testing.T)   // the existing open/diff/diffgolden tests pass (run as the package); openResultFor and diffPage on CPUBasic/CPUBasicNew equal the tool results byte for byte
func TestPublishNewIsWritePagesPath(t *testing.T)       // the existing output and render tests pass; publishNew with perm 0600 → mode 0600; a taken name → errors.Is(err, fs.ErrExist), and the temporary is gone
func TestOpenNamesFollowsTheStore(t *testing.T)         // open → openNames has the path; eviction past MaxOpenHandles → it is gone
```

In `cmd/perf-agent-mcp`: `TestEveryLimitFieldCanBeSetByFlag` covers the new fields as it is, and `TestRunAcceptsAZeroCaptureTTL` checks that `--limit Capture.TTL=0` exits 0.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'CaptureLimits|Engine|Unchanged|PublishNew|OpenNames'`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/perfmcp/ ./cmd/perf-agent-mcp/`
Expected: PASS, every M2/M2b test unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp cmd/perf-agent-mcp/main.go
git commit -m "perfmcp: capture limits and config, an engine lock around the tools, and open/diff/publish extracted for reuse"
```

### Task 10: Series files, the intent/commit protocol and recovery (opus)

**Files:**
- Create: `internal/perfmcp/capture_layout.go`, `capture_manifest.go`, `capture_commit.go`, `capture_recovery.go`, `capture_commit_test.go`, `capture_recovery_test.go`, `capture_layout_test.go`

**Interfaces:**
- Consumes: Task 9 `writeTemp`, `linkNoReplace`; Task 1 `capture.Error`; Task 3 `capture.Alive`, `StartTime`; Task 2 `capture.Settings`.
- Produces:
  - **Layout.**
    - `type captureDir struct { root *os.Root; rel, owner string }` and `func (s *Server) openCaptureDir() (*captureDir, *capture.Error)`. It runs `MkdirAll` 0700 through the profiles root and writes `.owner` = `{"kind": owner, "format": 1}` through `publishNew` (0600). An `EEXIST` there is read back, and another kind gives `owner_mismatch` naming the directory and its kind.
    - `func (d *captureDir) series(name string) (*os.Root, error)` (`Mkdir` 0700 when missing).
    - `func validName(arg, v string, max int) *capture.Error`: `[A-Za-z0-9._-]`, 1..max bytes, no leading `.`.
    - `func formatID(n int) string` (`%03d`, wider past 999).
    - `func parseName(name string) (id int, label, kind string, ok bool)`, with `kind` ∈ {`capture.json`, `intent.json`, `intent.hb`, `cpu.pb.gz`, `offcpu.pb.gz`, `gpu.pb.gz`, `gpu.execlog`}.
    - `func finalName(id int, label string, file string) string`, as in `"002-cand1-cpu.pb.gz"` and `"002-cand1.capture.json"`.
    - `func nextID(series *os.Root) (int, error)`. A missing `.seq` means the highest manifest or intent id + 1. It writes `.seq` = id + 1 through temp, fsync, rename and directory fsync **before** returning.
  - **The manifest.**
    - `type wireSettings struct { Modes []string; Duration, SampleRate, Unwind string; KernelStacks bool }` (JSON `modes`, `duration`, `sample_rate`, `unwind`, `kernel_stacks`), built by `func toWire(capture.Settings) wireSettings`.
    - `func settingsDigest(w wireSettings) string`: `"sha256:"` + the hex of the compact JSON.
    - `type fileIdentity struct { Name string; Dev, Ino uint64; Size int64; SHA256 string }`.
    - `type manifest struct { Format int; ID, Series, Label, Outcome string; Error *wireError; Settings wireSettings; SettingsDigest string; Target manifestTarget; Command *manifestCommand; PerfAgent perfAgentInfo; Start time.Time; AttachLatency, ProfiledDuration, StoppedBy, CompareWith string; Files []fileIdentity; Kept bool; Notes []string; Survivors []capture.Survivor; Cleanup, Cancel string; WrittenBy string }`, with JSON names per §7.2 and `written_by` = `"perf-agent-mcp"`.
    - `type manifestTarget struct { PID, Comm, Exe, BuildID string; Launched bool }` and `type manifestCommand struct { Path string; Args string }`; `Args` is the count as a decimal string.
    - `func readManifest(series *os.Root, name string) (manifest, error)` reads at most 1 MiB and checks `Format == 1` and `WrittenBy`.
    - `func seriesManifests(series *os.Root) ([]manifest, error)` returns them sorted by id.
  - **The transaction.**
    - `type intent struct { State string; ID, Series, Label string; Settings wireSettings; Finals []string; Staging string; StagingDev, StagingIno uint64; Token string; ServerPID int; ServerStart uint64; ReaperPID int; ReaperStart uint64; Files []fileIdentity; Probe *fileIdentity }`, with `State` ∈ {`prepared`, `linking`, `rolling_back`}.
    - `type captureTxn struct{ … }`, created by `func (s *Server) beginTxn(d *captureDir, series string, id int, label string, settings wireSettings, finals []string) (*captureTxn, error)`. It writes `NNN-label.intent.json` (no-replace, fsync) with a 128-bit hex `Token`, and starts the heartbeat: the `Chtimes` of `NNN-label.intent.hb` every 2 s.
    - `func (t *captureTxn) makeStaging() (*os.Root, string, error)`: `.staging-<16 hex>` (`Mkdir` 0700), its dev and inode recorded in the intent, then the **link probe**: write `.probe`, record its identity in the intent, `linkNoReplace` it to `.probe-link` inside staging, and remove both by identity. A refused link gives the M2 hard-link advice.
    - `func (t *captureTxn) setReaper(pid int, start uint64) error`.
    - `func (t *captureTxn) validate(staging *os.Root, expected []string, lim CaptureLimits) ([]fileIdentity, *capture.Error)` implements §7.4 step 8: `O_NOFOLLOW`, regular, `nlink == 1`, owner uid, size within limits, `Chmod 0600`, fsync, SHA-256. A missing or empty file is `no_output`. A symlink, a file with `nlink` > 1 or a non-regular file fails the job with `perf_agent_failed`: "output <f> was not a regular file with one link".
    - `func (t *captureTxn) record(files []fileIdentity) error` writes `state: linking` with the identities (step 9).
    - `func (t *captureTxn) link(files []fileIdentity) error` links each file to its final name and fsyncs the series directory (step 10). `EEXIST` gives `exists` and the names this capture already linked are rolled back.
    - `func (t *captureTxn) commit(m manifest, d *decisionWord) (committed bool, err error)` implements step 11 and the terminal decision rule: write the temporary manifest and fsync it; `d.beginManifest()`, and when that returns false, roll back to `cancelled`; link the manifest, fsync the directory, and `d.settle(linked)`. Then remove staging, the intent and the heartbeat. **Errors after a successful link are logged, and `committed` stays true.**
    - `func (t *captureTxn) rollback(code capture.Code, m manifest) error` implements live rollback steps 1–4. A failed manifest write keeps the intent at `rolling_back` and keeps the heartbeat running, and `s.strandedRollbacks` records the transaction.
    - `func (s *Server) retryStranded() error` runs before each capture and every 30 s.
    - `func removeByIdentity(root *os.Root, name string, id fileIdentity, final bool) (removed bool, err error)` follows §7.4's rules exactly: `nlink` 1 or 2, dev and inode equal, size and SHA-256 equal for finals; anything else is left and logged.
  - **Recovery.** `func (s *Server) recoverCaptures(d *captureDir) (notes []string, err error)` runs for every series and every intent, by §7.4's recovery list:
    - a committed intent (a manifest exists) has its leftovers removed;
    - a live owner (heartbeat < 10 s, and the server or the reaper alive by `s.ownerAlive`) is left alone;
    - an abandoned intent is removed by identity, then an `interrupted` manifest is committed;
    - a `prepared` intent has only its identified staging directory removed;
    - an unidentified `.staging-*` is an `orphan_staging` note.

    `ENOSPC` stops recovery and leaves the intent.
  - **Seams**, all unexported `Server` fields:
    - `s.now func() time.Time`;
    - `s.ownerAlive func(pid int, start uint64) bool`;
    - `s.captureFault func(p faultPoint) error`. It returns nil, an error (the step fails), or `errCrash`: the method returns at once, with no cleanup, and the heartbeat stops.
    - `s.writeErr func(op string) error`: injects `ENOSPC` into a named write.
  - **Fault points**, one constant each: `faultAfterLease`, `faultAfterRecovery`, `faultInEngine`, `faultAfterEngine`, `faultAfterSeq`, `faultAfterIntent`, `faultAfterSpace`, `faultAfterStaging`, `faultAfterValidate` (per file), `faultAfterRecord`, `faultBetweenLinks`, `faultAfterLinks`, `faultAfterManifestTemp`, `faultInManifestLink` (blocking allowed), `faultAfterManifestLink`, `faultAfterStagingRemoval`, `faultInRollback`, `faultInRecovery`, `faultHash`, `faultRetentionHash`, `faultFsync`.

- [ ] **Step 1: Write the failing tests** (no processes: the tests stage synthetic files in the staging root, with an in-test `decisionWord` (Task 12 defines it; this task adds a minimal one in `capture_commit.go` that Task 12 moves) and an `invariant(t, d)` checker. The checker requires each series to hold only committed manifests with verified files, failure or `interrupted` manifests with no files, `.seq`, and logged orphan staging directories.)

```go
func TestNamesAndIDs(t *testing.T)                          // validName: "parse-opt" ok, ".x" "a/b" "" 65 bytes refused; formatID(7)=="007", (1234)=="1234"; parseName round trips finalName
func TestNextIDIsPersistedFirstAndNeverReused(t *testing.T) // nextID twice → 1, 2; .seq == "3"; remove every manifest → nextID → 3
func TestOwnerFileRefusesTheOtherKind(t *testing.T)         // stdio writes .owner; a server with Owner "serve" → owner_mismatch naming the dir and "stdio"; and the reverse
func TestCommitHappyPath(t *testing.T)                      // begin → staging → validate → record → link → commit → manifest has identities, staging/intent/hb gone, finals 0600 nlink 1, invariant holds
func TestEveryFaultPointLeavesAValidState(t *testing.T)     // for each faultPoint on the commit path × {error, errCrash}: error → live rollback → failure manifest with code, no capture files; errCrash → ownerAlive=false, now += 11s, recoverCaptures → committed (if past the manifest link) or interrupted; invariant holds in every case; no file outside the series' own names
func TestENOSPCAtEveryWrite(t *testing.T)                   // writeErr returns ENOSPC per op (seq, intent, hb, staging, manifest temp, manifest link, dir fsync) → a valid state after rollback or recovery
func TestALostLinkRaceSparesTheUserFile(t *testing.T)       // beforePublish of the second final creates that name with "user" bytes → exists; first final removed by identity; the user file is byte for byte intact; failure manifest present
func TestLiveOwnerENOSPCIsFinishedByTheNextCapture(t *testing.T) // manifest write ENOSPC during rollback → intent rolling_back stays, heartbeat fresh; recoverCaptures leaves it (owner alive); retryStranded with ENOSPC gone → failure manifest, intent removed
func TestPostLinkErrorsFollowTheCommittedPath(t *testing.T) // fault after the manifest link: fsync error, staging removal error, intent removal error → committed true, job-level ok; the next recoverCaptures removes the leftovers by identity
func TestTheDecisionRule(t *testing.T)                      // cancel before beginManifest → rolled back cancelled, no manifest; faultInManifestLink blocks, cancel → requestCancel answers "cancelling"; release with success → committed, Cancel == "too late: committed"; release with an injected link error → cancelled, rolled back; cancel after commit → "too late: committed"
func TestRemoveByIdentityNeverRemovesWhatItCannotProve(t *testing.T) // other inode → left; same inode, other size → left; nlink 2 staging+final → both removed; symlink → left
func TestRecoveryJudgesTheOwner(t *testing.T)               // heartbeat fresh + owner alive → untouched; heartbeat stale but reaper (pid, start) alive → untouched; both gone → interrupted manifest; prepared intent → only its staging removed, a final name present is left
func TestAnUnrecordedStagingDirIsALoggedOrphan(t *testing.T) // crash between Mkdir and the intent update → recovery note "orphan_staging"; the directory remains
func TestStagingProbeRefusesAFilesystemWithoutHardLinks(t *testing.T) // Review Focus 5: s.link returns EPERM → makeStaging fails with the hard-link advice; the staging dir and probe removed by identity
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Names|NextID|OwnerFile|Commit|Fault|ENOSPC|LinkRace|LiveOwner|PostLink|DecisionRule|RemoveByIdentity|Recovery|Orphan|StagingProbe'`
Expected: FAIL.

- [ ] **Step 3: Implement the four files.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp/capture_*.go
git commit -m "perfmcp: series files, an intent/commit protocol with identity-matched deletion, and recovery"
```

### Task 11: Retention

**Files:**
- Create: `internal/perfmcp/capture_retention.go`, `capture_retention_test.go`

**Interfaces:**
- Consumes: Task 10 `captureDir`, `seriesManifests`, `removeByIdentity`, the seams.
- Produces:
  - `type fileKey struct{ Dev, Ino uint64 }` and `type protection struct { files map[fileKey]bool; captures map[string]bool }`, keyed `"series/NNN"`.
  - `func (s *Server) protected(d *captureDir, baseline string) protection` runs under the engine. It protects:
    - every file the store holds open (`entry.opened`);
    - every publication pin (none in stdio; it reads `s.live` when present);
    - every capture named by a live intent;
    - `baseline`;
    - each series' newest manifest's `compare_with`;
    - every `kept` capture.
  - `func (s *Server) retain(d *captureDir, p protection, now time.Time) (evicted []string, err error)` works in §7.3's order:
    1. TTL, when `TTL` > 0, measured from `Start`;
    2. per series, unkept captures beyond `KeepPerSeries`, oldest first;
    3. while the directory's total bytes exceed `MaxStoredBytes`, the oldest unkept capture in any series, the least recently written series first.

    Each eviction verifies every file (inode, size, SHA-256 through `faultRetentionHash`). Any mismatch spares the whole capture, with a log line. Files go first and the manifest last. A file already missing counts as removed. `evicted` holds `"series/NNN-label"`.
  - `func dirBytes(root *os.Root) (int64, error)`.

- [ ] **Step 1: Write the failing tests** (synthetic committed captures built with Task 10's transaction)

```go
func TestRetentionKeepsPerSeriesNewestFirst(t *testing.T)    // 12 unkept in a series, KeepPerSeries 10 → 001, 002 evicted, in that order
func TestRetentionByStoredBytes(t *testing.T)                // two series; MaxStoredBytes just under total → the oldest unkept of the least recently written series goes first
func TestRetentionTTL(t *testing.T)                          // now = Start + 8 days → unprotected evicted; TTL 0 → nothing by age
func TestRetentionNeverDeletesProtected(t *testing.T)        // kept, the newest's compare_with, the call's baseline, an open handle's file (store.open of it), a live intent's capture → all survive TTL and KeepPerSeries
func TestRetentionSparesAReplacedFile(t *testing.T)          // overwrite a final's bytes (same size) → digest mismatch → capture spared, logged; a .pb.gz with no manifest → untouched
func TestRetentionFinishesAPartialEviction(t *testing.T)     // files removed, manifest left (crash) → next retain removes the manifest; evicted lists it once
func TestRetentionDoesNotReuseIDs(t *testing.T)              // evict 001..003, nextID → 013 (after 012)
func TestFailedManifestsAreRetainedLikeCaptures(t *testing.T) // a failure manifest counts toward KeepPerSeries and ages out
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run Retention`
Expected: FAIL.

- [ ] **Step 3: Implement `capture_retention.go`.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp/capture_retention*.go
git commit -m "perfmcp: retention deletes only manifest-verified, unprotected captures in the capture directory"
```

### Task 12: The registry, the lookup pool, the windows and the decision word

**Files:**
- Create: `internal/perfmcp/capture_registry.go`, `capture_registry_test.go`
- Modify: `capture_commit.go` (move `decisionWord` here unchanged)

**Interfaces:**
- Consumes: Task 10 `readManifest`.
- Produces:
  - **The decision word.**
    - `type decisionWord struct{ v atomic.Int32; cancelInFlight atomic.Bool }`, with the constants `decisionOpen`, `decisionCancelRequested`, `decisionManifestInFlight` and `decisionCommitted`.
    - `func (d *decisionWord) requestCancel() string` returns `"cancelling"` or `"too late: committed"`.
    - `func (d *decisionWord) beginManifest() bool`.
    - `func (d *decisionWord) settle(linked bool) (cancel string)` returns `"too late: committed"`, `"cancelled"` or `""`.
    - `func (d *decisionWord) cancelled() bool`.
  - **The registry.**
    - `type registry struct{ … }`, created by `func newRegistry(lim CaptureLimits, nonce string) *registry`.
    - `func (r *registry) newJobID() string` (`"j<N>-<nonce>"`).
    - `func (r *registry) admit(j *job, now time.Time) *capture.Error` CASes the `atomic.Pointer[job]` active slot (`busy` names the running capture's id and its `retry_after` end), then checks the windows (`budget` with `retry_after` = when the window frees). It records a window entry only on success.
    - `func (r *registry) finish(j *job, resultBytes int)` clears the active slot and moves the job into the terminal LRU, bounded by `MaxJobs` and `MaxJobBytes`. Lookup answers share that LRU.
    - `func (r *registry) get(id string) (*job, bool)` takes a job id or a capture id. `func (r *registry) put(captureID string, a *historical)` caches a lookup answer.
    - `func (r *registry) list() []*job`: the active job first, then the most recent.
  - **The windows.** `type window struct{ … }` and `func (w *window) allow(now time.Time, d time.Duration, maxCaptures int, maxDuration time.Duration) (retryAfter time.Duration, ok bool)`, over a rolling hour.
  - **The lookup pool.**
    - `type lookupPool struct{ … }`, created by `func newLookupPool(workers, queue int) *lookupPool` (2 and 16).
    - `func (p *lookupPool) get(key string, load func() (any, error)) (*lookup, bool)`. A key in flight is shared; a full queue returns `(nil, false)`.
    - `type lookup struct { done chan struct{}; val any; err error }`.
    - `func (p *lookupPool) close()`.
    - No goroutine is started per wait: waiters `select` on `done` and on their own deadline.

- [ ] **Step 1: Write the failing tests**

```go
func TestAdmitIsOneAtATime(t *testing.T)                 // admit j1 ok; admit j2 → busy naming j1's capture id; finish j1 → admit j2 ok
func TestWindowsBound(t *testing.T)                      // WindowCaptures 2: third admit within the hour → budget, RetryAfter = first+1h−now; WindowDuration 30m: two 20m requests → second budget
func TestRegistryLRUByCountAndBytes(t *testing.T)        // MaxJobs 2 → oldest terminal evicted; MaxJobBytes 100 with results of 60 → one kept; the active job is never evicted
func TestJobIDsCarryTheProcessNonce(t *testing.T)        // two registries → j1-xxxx ≠ j1-yyyy
func TestLookupsAreSharedAndBounded(t *testing.T)        // two gets of one key while load blocks → same *lookup, load called once; 2 workers + 16 queued blocked → the 19th distinct key returns (nil,false)
func TestWaitingDoesNotAddGoroutines(t *testing.T)       // load blocked; 200 waits that each time out at 1 ms → runtime.NumGoroutine() ≤ before + 4
func TestDecisionWord(t *testing.T)                      // the four transitions and settle outcomes of §7.4's rule, including a cancel recorded while manifest_in_flight
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Admit|Windows|RegistryLRU|JobIDs|Lookups|WaitingDoesNot|DecisionWord'`
Expected: FAIL.

- [ ] **Step 3: Implement `capture_registry.go`.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp/capture_registry*.go internal/perfmcp/capture_commit.go
git commit -m "perfmcp: a bounded job registry, a shared lookup pool and rolling capture windows"
```

### Task 13: The job state machine and its derivations (opus)

**Files:**
- Create: `internal/perfmcp/capture_job.go`, `capture_compare.go`, `capture_job_test.go`, `capture_main_test.go` (`TestMain` → `capturetest.Main(m, capture.RunInternal)`)
- Modify: `server.go` (`New` builds the registry, the pool and maintenance; `Close`; `KillCaptures`)

**Interfaces:**
- Consumes: Tasks 7 and 9–12.
- Produces:
  - **The request.** `type captureRequest struct { PID int; Command []string; Cwd string; Settings capture.Settings; Explicit map[string]bool; Series, Label, CompareWith string; Keep bool }`. `Explicit` names which of `modes`, `duration`, `sample_rate`, `unwind` and `kernel_stacks` the agent passed.
  - **The job.**
    - `type jobStatus string`, with the seven states of §4.0.
    - `type job struct { id string; req captureRequest; created time.Time; d decisionWord; mu sync.Mutex; status jobStatus; phase, captureID, derived string; err *capture.Error; result []byte; changed chan struct{}; run *capture.Run; pins []any }`.
    - `func (j *job) view() jobView`, where `type jobView struct { Job, ID, Status, Phase, Elapsed, ETA, Derived string }` (JSON `job`, `id`, `status`, `phase`, `elapsed`, `eta`, `derived`; empty values omitted).
    - `func (j *job) wait(ctx context.Context, d time.Duration) jobView` returns at a terminal state (`done` with `derived` ≠ `pending`, `failed` or `cancelled`), or at `d`, or when ctx is done.
  - **The lifecycle.**
    - `func (s *Server) startJob(req captureRequest) (*job, *capture.Error)` runs admission only (Task 12 `admit`), then `go s.runJob(j)`.
    - `func (s *Server) runJob(j *job)` follows §7.4 exactly:
      1. Resolve, with no side effect: `cwd` with `s.rel` against `--source` roots and the profiles root; `ResolveCommand` and `CheckCommandFile`; then `openCaptureDir`.
      2. Take `dirMu`, then `TryLock`.
      3. `retryStranded`, then `recoverCaptures`.
      4. Engine section: resolve and validate the baseline (`unknown_baseline` names a missing or non-`ok` capture's outcome and lists the last 5 `ok` ids; `incomparable` names the setting and both values; the inherited settings fill the non-explicit ones; the baseline's files are digest-checked); `MaxKeptPerSeries` (`invalid_argument`); pin the baseline; `protected`; `retain`.
      5. `nextID`, then the job's `captureID`.
      6. `beginTxn`.
      7. The space check: `dirBytes` ≥ `MaxStoredBytes`, or `Statfs` free < `MinFreeBytes`, gives `disk`, naming the largest series.
      8. `makeStaging`, then `capture.Start`, then `setReaper` and `Lease.Update` with the reaper.
      9. `running`: `Run.Wait`.
      10. `committing`: `validate`, `record`, `link`, `commit`.
      11. `done`: release the lease copy, then derive under the engine; `derived` becomes `ready` or `failed`; then release the pins.

      Every failure from step 6 on goes through `rollback`, and the job reports its failure only after the rollback's removal step. A cancel is checked at every boundary through `j.d.cancelled()`.
    - `func (s *Server) cancelAllJobs()` uses the decision word only, and `func (s *Server) KillCaptures()` calls `Run.Kill` for the active job; neither blocks.
    - `Close` runs `cancelAllJobs`, then `KillCaptures`, waits up to `StopGrace` + 1 s for job goroutines, stops maintenance and the pool, and closes the roots.
    - Maintenance: at `New`, when capture is enabled, a goroutine runs `dirMu`, then `TryLock` (skipped when busy, with a log line), then recovery and retention. A ticker runs `retryStranded` every 30 s.
  - **Derivations.**
    - `type captureResult struct { Capture captureInfo; Profiles []profileResult; Compare *compareResult; Evicted []string; Notes []string; Cancel string; Derived string; Handles string }` follows §4.3. `Derived` and `Handles` are set only for history answers.
    - `type profileResult struct { Mode, Path string; Execlog string; Open *openResult; OpenError string }`.
    - `type compareResult struct { Base, New compareSide; Comparability map[string]string; Views []compareView }` and `type compareView struct { diffResult; Next toolCall }`.
    - `type toolCall struct { Tool string; Arguments map[string]any; Bind string }` and `type recompute struct { Steps []toolCall; Baseline string }`.
    - `func (s *Server) derive(j *job, m manifest, base *manifest) (captureResult, error)` runs under the engine: `openResultFor` for each profile (with the execlog for gpu), then `diffPage` for each common view with `limit = CompareRows`. An `open` or `diff` failure sets `derived: failed` with the reason and never touches files. Comparability holds `settings: "equal"`; `target_exe`: `same (build-id X)`, `changed (build-id X → Y)` or `unknown`; and the profiled durations and attach latencies of both sides.
    - `func recomputeFor(m manifest, base *manifest, baseGone string) recompute` builds the recipe from the builder's-calls table.
    - The GPU note when the gpu view's total is zero.
  - **Seams.** `s.captureFault` points `faultHash`, `faultFsync`, `faultRetentionHash`, `faultInEngine`, `faultBeforeOpen` and `faultBeforeDiff` may block on a test channel. `s.capturePublish` stays nil.

- [ ] **Step 1: Write the failing tests** (internal API; fake perf-agent through `Config.Capture.Verify` returning a `capture.Binary` for the fake; durations of 300–800 ms)

```go
func TestJobRunsThroughEveryState(t *testing.T)                // states seen in order accepted→preparing→running→committing→done; derived pending→ready; manifest outcome ok; result Profiles[0].Open.Handle opens in the store
func TestStartJobDoesNoIO(t *testing.T)                        // with the captures dir unwritable (chmod 0500 on the profiles root): startJob returns a job, the failure arrives async (disk/open error), not from startJob
func TestBlockedSeamsNeverBlockAdmissionOrWait(t *testing.T)   // for each of faultHash, faultRetentionHash, faultFsync, the engine (test holds it), faultBeforeOpen, faultBeforeDiff: startJob returns in < 1 ms; j.wait(100ms) returns the current view in ≤ 150 ms; unblock → done
func TestACommitThatOutlivesAWait(t *testing.T)                // faultAfterRecord blocks; wait(100ms) → status committing; unblock; next wait → done
func TestDerivationFailureNeverRollsBack(t *testing.T)         // Model.MaxNodes tiny → open fails → done, derived failed with the reason; finals and manifest present
func TestCompareWithInheritsAndRefuses(t *testing.T)           // base kept; cand with compare_with "001" and no settings → same settings_digest; explicit sample_rate 199 → incomparable naming sample_rate 99 vs 199; for each inherited setting
func TestCompareWithAFailedCaptureIsRefusedNamingItsOutcome(t *testing.T) // Review Focus 4: 002 failed target_exited; compare_with "002" → unknown_baseline, message contains "failed" and "target_exited", lists "001"
func TestTheBaselineIsPinnedBeforeRetention(t *testing.T)      // compare_with the oldest while KeepPerSeries 1 and TTL would evict it → it survives; diff rows present
func TestCancelAtEachState(t *testing.T)                       // cancel in preparing (faultAfterSeq blocks), running, committing (faultAfterRecord blocks) → cancelled, failure manifest with "cancelled", no capture files; cancel during a blocked manifest link → "cancelling", then link success → done with Cancel "too late: committed"
func TestCloseCancelsAndKills(t *testing.T)                    // running job; Close → returns within StopGrace+1s; the fake's group gone; manifest cancelled
func TestKillCapturesNeverBlocks(t *testing.T)                 // KillCaptures < 10 ms during running
func TestMaintenanceRunsAtStartWithoutBlockingNew(t *testing.T) // faultRetentionHash blocks; New returns at once; tools answer; unblock → the old capture evicted
func TestRecomputeRecipeShape(t *testing.T)                    // m with cpu + base present → steps: open base (bind base_cpu), open new (bind new_cpu), diff with ${base_cpu}/${new_cpu}, limit CompareRows; base gone → only the new open, Baseline "001 was removed by retention"
func TestTheGPUNoteForAnEmptyProfile(t *testing.T)             // fake no-gpu-samples, modes gpu (Shim seam) → note "the GPU profile has no samples"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'TestJob|StartJob|BlockedSeams|Outlives|DerivationFailure|CompareWith|Baseline|CancelAt|CloseCancels|KillCaptures|Maintenance|Recompute|GPUNote'`
Expected: FAIL.

- [ ] **Step 3: Implement `capture_job.go`, `capture_compare.go` and the `server.go` wiring.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=2 ./internal/perfmcp/`
Expected: PASS, with no leftover processes.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: the capture job state machine, with post-commit open and compare derivations"
```

### Task 14: The four tools

**Files:**
- Create: `internal/perfmcp/tool_capture.go`, `tool_capture_list.go`, `capture_wire.go`, `tool_capture_test.go`, `capture_loop_test.go`, `capture_errors_test.go`
- Modify: `server.go` (`tools()` appends `capture`, `capture_wait`, `capture_cancel` and `capture_list` after `render_flamegraph`, unwrapped by the engine; `capture_cancel` is `Control: true`; `Options` passes `MaxOutBytes` = 4 × `MaxResponseBytes` and `OnClose` = `cancelAllJobs`), `harness_test.go` (`send`/`recv` by id for overlapping calls)

**Interfaces:**
- Consumes: Tasks 8, 12 and 13.
- Produces:
  - **Wire types.** `type wireError struct { Code, Message string; Retryable bool; RetryAfter, Next, CaptureID, Job, Status, PerfAgentExit, StderrTail, Changed string; Survivors []capture.Survivor }`, with JSON per §4.4 and empty values omitted. `func toolError(e *capture.Error, j *job) mcpstdio.Result` returns `IsError` true with text `{"error": wireError}`, its message cut to `MaxToolErrorBytes`.
  - **`capture`.** The schema follows §4.2's table without `show`. `captureArgs` uses pointer fields for the inheritable settings, so `Explicit` is known.
    - Admission checks, in order:
      - capture disabled (`disabled`, the §4.1 text);
      - `owner_mismatch`;
      - exactly one of `pid` and `command`;
      - `command[0]` absolute or bare: `./x` gives `invalid_argument` with "pass <cwd joined>" (Review Focus 1);
      - `cwd` only with `command`;
      - distinct modes;
      - `gpu` without a shim (`disabled`, naming `--gpu-shim`);
      - `duration` within [1 s, `MaxDuration`];
      - `sample_rate` within [1, `MaxSampleRate`];
      - `kernel_stacks` without `cap_syslog` (`invalid_argument`, naming the setcap line);
      - `unwind`;
      - `series` and `label` (`validName`);
      - `compare_with` matching `^[0-9]{3,}$`;
      - then `startJob` (`busy`, `budget`).
    - Result `{"job", "status": "accepted", "eta"}`.
    - The description starts `DISABLED: start perf-agent-mcp with --perf-agent PATH (the setcap'd perf-agent binary) to enable capturing.` when there is no binary.
    - Annotations: not read-only, not idempotent, destructive, open world (§4.5).
  - **`capture_wait {id, timeout?}`.**
    - `timeout` is a Go duration, default 30 s; over 45 s, or ≤ 0, is `invalid_argument`. The wait never runs past the call's own context.
    - A job id or a capture id (`"series/NNN"`) is first looked up in the registry. Otherwise it goes to the lookup pool (`load` = `readManifest`, then a historical answer). That gives `loading` with `retry_after` `"1s"` at the deadline or when the queue is full, and `unknown_capture` when the manifest is absent.
    - `done` with `derived: ready` returns the full `captureResult` (§4.3).
    - Failed and cancelled are `toolError` with `job`, `capture_id` and `status`.
    - A historical answer is terminal: `status` from the manifest outcome, `derived: "recompute"` with `recompute`, `handles: "reopen"`, and never `pending`.
    - Over `MaxResponseBytes`, each `open` is cut to `handle`, `views`, `default_view` and `quality`, with the note "call `open` with path for the full result".
    - With a progress token, `mcpstdio.Progress` every 5 s and at each status or phase change: `progress` = elapsed seconds, `total` = the duration in seconds.
  - **`capture_cancel {id}`** (`Control`). Registry only: `requestCancel`, returning `{job, status: "cancelling"}`, or `{job, status: "done", cancel: "too late: committed"}`. An unknown id gives `unknown_capture`. No file I/O.
  - **`capture_list {series?, limit?, cursor?}`** (read-only, idempotent).
    - Rows: in-memory jobs first, then manifests through the lookup pool (key `"list:<series>"`, cached until this server's next commit or retention pass, or 2 s).
    - A row has id, label, outcome (`ok` or the code), `stopped_by`, settings, `settings_digest`, `comm`, `build_id`, `start`, paths, `kept`, `compare_with`, and `handle` from `openNames()`.
    - Without `series`: `{series: [{name, captures, bytes, orphan_staging}]}`.
    - Paged with `binding("capture_list", …)` and `pageRows`, like `top`. A page not read in time is `{"loading": true, "retry_after": "1s"}`. It waits at most 10 s.

- [ ] **Step 1: Write the failing tests**

```go
// tool_capture_test.go
func TestCaptureIsDisabledWithoutPerfAgent(t *testing.T)            // description prefix; every capture* call isError {"error":{"code":"disabled",…"--perf-agent"}}; capture_list still works (empty)
func TestCaptureAcceptsInMicroseconds(t *testing.T)                 // wall time of the capture call < 5 ms (with the harness's round trip measured separately and subtracted); result status "accepted", job matches ^j\d+-[0-9a-f]{4}$
func TestCaptureArgumentRefusals(t *testing.T)                     // table: both pid and command; neither; duration "0s"/"31s"; sample_rate 0/1001; modes ["cpu","cpu"]; series ".x"; label 33 bytes; compare_with "1"; kernel_stacks without cap_syslog; gpu without shim (code disabled) → each isError with the documented code, retryable false, valid JSON
func TestCaptureRelativeCommandSuggestsTheAbsolutePath(t *testing.T) // Review Focus 1: command ["./bench"], cwd "<profiles>/proj" → invalid_argument; next contains "<profiles>/proj/bench"
func TestCaptureWaitTimeouts(t *testing.T)                          // timeout "46s" → invalid_argument; a 60 s fake with timeout "45s" → returns running at ≈45 s (run with Capture.MaxDuration raised; marked with t.Skip under -short), then cancel; default 30 s
func TestCaptureWaitDuringALongCaptureWithLoad(t *testing.T)        // during a 45 s capture_wait: 20 tools/call top then a notifications/cancelled for the wait → the wait ends without a response; 8 queued answered in order; 12 get -32000
func TestCaptureCancelIsAControlMessage(t *testing.T)               // capture_wait 45 s in flight; capture_cancel → answered < 100 ms with "cancelling"; the wait then returns isError code cancelled
func TestCaptureCancelUnknownIDDoesNoIO(t *testing.T)               // capture_cancel "s/001" (on disk, not running) → unknown_capture; a seam on openCaptureDir records zero calls
func TestCaptureListShowsTheRunningCaptureFirst(t *testing.T)       // running job + 2 on disk → rows[0] is the running one; no series → series summary with orphan_staging count
func TestCaptureListPagesAndLoads(t *testing.T)                     // limit 1 → next_cursor; blocked manifest read seam → loading true at ≤ 10 s
func TestEveryCallStaysUnderTheClientBudget(t *testing.T)           // every call in the loop test measured < 50 s
func TestProgressNotificationsAreInPhaseOrder(t *testing.T)        // progressToken → notifications precede the result, statuses non-decreasing in §4.0 order; every stdout line JSON-RPC
func TestTheResultIsCutToMaxResponseBytes(t *testing.T)             // MaxResponseBytes small enough → open objects carry only handle, views, default_view, quality and the note

// capture_loop_test.go
func TestALoopOfABaseAndThreeCandidates(t *testing.T)               // series "parse-opt": base keep, cand1..3 compare_with "001" → ids 001..004; each wait result has compare.views; for each, calling compare.views[0].next with limit CompareRows returns rows identical to the result's rows
func TestCompareWithAcrossAServerRestart(t *testing.T)              // base in server A; Close; server B on the same dirs: cand compare_with "001" → compare present, base handle reopened
func TestRecomputeReplaysExactly(t *testing.T)                     // live job's compare rows R; MaxJobs 1 then another capture (evicts it), and separately a restart: capture_wait by capture id → derived "recompute", handles "reopen"; the test executes every emitted step in order, substituting each bind into later ${…} → the diff rows equal R exactly
func TestRecomputeReportsARetainedAwayBaseline(t *testing.T)        // baseline evicted by a later retention → recompute.baseline "001 was removed by retention", no diff step

// capture_errors_test.go
func TestEveryErrorCodeIsProducedOnce(t *testing.T)                 // one subtest per §4.4 code, each producing it end to end through the tools (fake modes, workloads, seams: binary_changed by touching the fake after New; owner_mismatch via a "serve" .owner; disk via MinFreeBytes 1<<62; exists via beforePublish; stuck_child via the reaper events seam; gpu_not_injected; target_refused pid 1; target_not_found; cancelled) → valid JSON, the documented retryable, a non-empty next, message ≤ MaxToolErrorBytes
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race -short ./internal/perfmcp/ -run 'Capture|Loop|Recompute|EveryErrorCode|Progress|TheResultIsCut|EveryCall'`
Expected: FAIL.

- [ ] **Step 3: Implement the tools and the wire types.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/perfmcp/` (the full run, including the 45 s tests)
Expected: PASS. `TestSkillNamesExistInTheServer` still passes, since the skill does not change until Task 17.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: capture, capture_wait, capture_cancel and capture_list"
```

### Task 15: The command: flags, internal modes, forced exit and the network scan

**Files:**
- Modify: `cmd/perf-agent-mcp/main.go`, `main_test.go`, `netscan_test.go`

**Interfaces:**
- Consumes: Task 6 `capture.RunInternal`; Task 9 `CaptureConfig`; Task 13 `KillCaptures`; Task 8 `ErrClientGone`.
- Produces:
  - **`main()`.** It calls `if code, ok := capture.RunInternal(); ok { os.Exit(code) }` before anything else.
  - **Flags:**
    - `--perf-agent PATH`;
    - `--captures DIR` (default `captures`, relative to `--profiles`, or absolute inside it);
    - `--capture-command PATH` (repeatable);
    - `--capture-ttl DURATION`, applied after `--limit`, which sets `Limits.Capture.TTL`;
    - `--gpu-shim PATH`.

    A `--perf-agent` that fails `VerifyBinary`, and an invalid shim or command path, are start-up errors (exit 1, the reason on stderr). A privileged server (`CheckSelf`) starts with capture disabled and the reason in the tool description and in every call. The package doc comment changes to the M2c wording.
  - **Exits:**
    - in the forced-exit branch, `srv.KillCaptures()` is called synchronously before `return 0`;
    - `serveErr == mcpstdio.ErrClientGone` takes the same path, without `Close` blocking: kill, then `Close` in a goroutine bounded by 100 ms, then `return 0`.
  - **Test re-exec.** `TestMain` calls `capture.RunInternal` first. A new env `PERF_AGENT_MCP_TEST_FAKE_VERIFY=1` makes `configHook` set `Config.Capture.Verify` to accept the fake. `fakeCommand(t, args...)` is like `liveCommand`.
  - **The network scan.**
    - `execFile` becomes `execFiles = []string{"internal/sourceroot/git.go", "internal/capture/exec_linux.go"}`.
    - In `exec_linux.go`: exactly two `exec.Command` calls, the first argument of one a selector named `PerfAgent` and of the other the identifier `self`; no `exec.LookPath`, `exec.CommandContext`, or string literals `"sh"`, `"bash"`, `"sudo"`, `"pkexec"`.
    - `launchFile = "internal/capture/launch_linux.go"` may use `syscall.Exec`, and `syscall.RawSyscall` only with first argument `syscall.SYS_PRCTL` and second the identifier `prSetNoNewPrivs` or `prSetChildSubreaper`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunRegistersTheCaptureTools(t *testing.T)              // tools/list == the nine M2 tools then capture, capture_wait, capture_cancel, capture_list; no page_*; capture's annotations destructive and open world; capture_cancel listed like any tool
func TestRunRefusesABadPerfAgent(t *testing.T)                  // --perf-agent relative → exit 2 or 1 with the reason; a plain file → exit 1 naming the missing capabilities and the setcap line
func TestRunCaptureFlagsReachTheConfig(t *testing.T)            // configHook captures cfg: --captures sub, --capture-command /bin/true, --capture-ttl 0, --gpu-shim <file> → the fields set
func TestStdoutStaysJSONRPCWhileCapturing(t *testing.T)         // fakeCommand with a spam-stdout fake and a sleep workload: capture, capture_wait to done → every stdout line is JSON-RPC 2.0
func TestSignalKillsCapturesOnTheForcedExit(t *testing.T)       // fake ignore-SIGINT capturing, stdout blocked (4 KiB pipe, unread) → SIGTERM → exit 0 within shutdownGrace + 100 ms + 200 ms; the fake's group gone within StopGrace (kill(-pgid,0) == ESRCH; the pgid from the fake's Record file)
func TestADeadClientEndsCaptures(t *testing.T)                  // stdout never read until the writer queue is full (progress tokens on a long wait) → the process exits without a signal; the fake's group is gone within StopGrace
func TestEOFCancelsTheCapture(t *testing.T)                      // capture, then close stdin → exit 0; manifest outcome cancelled; group gone
func TestTheServerLinksNoProfiler(t *testing.T)                  // unchanged, still passes
func TestTheNetworkScanSeesTheCaptureExecRules(t *testing.T)    // fixtures like TestTheNetworkScanSeesSyscallsExecAndDirectives: a third exec.Command, exec.LookPath, "sh" literal, a RawSyscall with SYS_GETPID or PR_SET_DUMPABLE → findings; the allowed forms → none
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp/`
Expected: the new tests FAIL; `TestNoClientNetworkingInTheServer` FAILs on the new files until the rules land.

- [ ] **Step 3: Implement the flags, routing, exit paths and scan rules.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./cmd/perf-agent-mcp/ ./internal/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp
git commit -m "perf-agent-mcp: capture flags, internal reaper and launcher modes, and a capture kill on every exit path"
```

### Task 16: Multi-process and crash tests (opus)

**Files:**
- Create: `internal/perfmcp/capture_process_test.go`, `procserver_test.go` (the subprocess harness)

**Interfaces:**
- Consumes: everything above.
- Produces (test-only): `TestMain` gains `PERFMCP_TEST_SERVER=1`. It serves stdio on stdin/stdout with a `Config` read from `PERFMCP_TEST_CONFIG` (JSON: dirs, fake path, owner, limits, crash point). Crash point `X` makes `captureFault(X)` call `os.Exit(3)`. `startServerProcess(t, cfg) *procClient` gives `call`, `pid` and `kill9`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTwoServersShareOneCapturesDir(t *testing.T)               // A captures 3 s; B capture → accepted, then wait → busy naming A's pid and capture id, retryable, retry_after > 0
func TestAKilledServerLeavesTheLockWithItsReaper(t *testing.T)     // A captures 2 s, kill9(A) during running → B's capture fails busy naming the reaper pid; after the reaper exits (the fake's group gone within StopGrace of A's death), B's next capture succeeds and recovery wrote "interrupted" for A's id, judged by the reaper's pid and start time
func TestKillingTheServerInEachPhaseKillsTheChildren(t *testing.T) // never-ready fake (start-up), normal (profiling), hang-after-SIGINT after stop (finalisation): kill9(server) → group gone within StopGrace
func TestAHungServerIsBoundedByTheDeadline(t *testing.T)           // SIGSTOP the server process during running → the fake's group gone by StartTimeout+duration+FinishTimeout+StopGrace (limits shrunk); SIGCONT; the job fails
func TestTheSpawnReportGapStillKills(t *testing.T)                 // FaultPause after-perf-agent-fork (through a seam that sets it on the RunSpec) → kill9(server) while paused → group gone within StopGrace; same after-launcher-fork
func TestRealCrashesAtCommitBoundaries(t *testing.T)               // crash points faultAfterRecord, faultBetweenLinks, faultAfterManifestTemp, faultAfterManifestLink: server exits 3; a fresh server's recovery → committed or interrupted per the point; invariant holds; capture_list shows it
func TestStdioAndServeRefuseEachOthersDir(t *testing.T)            // owner "serve" server on a stdio-owned dir → every capture owner_mismatch naming kind "stdio"; and the reverse
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'TwoServers|KilledServer|EachPhase|HungServer|SpawnReport|RealCrashes|RefuseEachOthers'`
Expected: FAIL (no harness).

- [ ] **Step 3: Implement the harness.** Fix what the tests expose in Tasks 5–13's code, in this task's commit, each fix with the test that found it.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/perfmcp/ -run 'TwoServers|KilledServer|EachPhase|HungServer|SpawnReport|RealCrashes|RefuseEachOthers'`
Expected: PASS three times. No leftover `fakeperfagent` or `workload` processes, checked with `ps -eo pid,comm`.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: multi-process lock, reaper and crash-recovery tests"
```

### Task 17: Guide, skill, docs and changelog

**Files:**
- Modify: `internal/perfmcp/guide.md`, `guide_test.go`, `skills/perf-agent-mcp/SKILL.md`, `skill_test.go` (`skillArgs` additions only), `docs/mcp.md`, `CHANGELOG.md`

**Interfaces:**
- Consumes: Task 14's tool names, arguments and codes.
- Produces:
  - **The guide.** A "Capture loop" section before "Claimable metrics":
    - one series per loop, `keep: true` on the base, `compare_with` the base id;
    - call `capture`, then `capture_wait` until `done`;
    - read `compare.comparability` before trusting a diff;
    - branch on `error.code` and `retryable`;
    - capture only targets the user's task names;
    - after a restart, use `capture_wait` by capture id and run `recompute`.

    The guide stays ≤ 12288 bytes; trim elsewhere if needed, keeping every string `guide_test` requires.
  - **The skill.** The same loop in five lines.
  - **`docs/mcp.md`.**
    - "What it does not do": the M2c wording, from spec §2 "What this supersedes".
    - The new flags, the four tools, the `Capture.*` limits table and the error codes table.
    - Retention, "the one place it deletes files", next to "never replaces a file".
    - The guardrails, and the threat model's residuals: the arbitrary-code row, the CAP-R1b exposure in plain words, and units started through other daemons.
    - The cgroup write outside the roots.
    - Client timeouts: Codex `tool_timeout_sec`, Claude Code `MCP_TOOL_TIMEOUT`.
    - The setcap line, and "not in /tmp".
  - **`CHANGELOG.md`.** `[Unreleased]` gets "perf-agent-mcp can capture profiles" with the flags.

- [ ] **Step 1: Write the failing tests**

```go
// guide_test.go: TestGuideIsShortAndStatesTheRules gains
"## Capture loop", "`compare_with`", "`capture_wait`", "`comparability`", "`retryable`", "`recompute`"
// skill_test.go: TestSkillNamesExistInTheServer passes with the four tools named in the skill
func TestDocsListEveryCaptureCodeAndLimit(t *testing.T) // docs/mcp.md contains every capture.Code string and every Capture.* limit name (reflection over CaptureLimits)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Guide|Skill|Docs'`
Expected: FAIL.

- [ ] **Step 3: Write the text.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp/guide.md internal/perfmcp/guide_test.go internal/perfmcp/skill_test.go skills/perf-agent-mcp/SKILL.md docs/mcp.md CHANGELOG.md
git commit -m "docs: the capture loop in the guide, the skill and docs/mcp.md"
```

### Task 18: The real capture (gated, privileged)

**Files:**
- Create: `internal/perfmcp/capture_real_test.go`

**Interfaces:**
- Consumes: the real `capture.VerifyBinary`; the Task 4 workload.
- Produces: `TestRealCapture` and `TestRealKernelStacks`.

- [ ] **Step 1: Write the test**

```go
func TestRealCapture(t *testing.T) {
    // bin := os.Getenv("PERF_AGENT_BIN"); empty → t.Skip("PERF_AGENT_BIN is not set")
    // b, err := capture.VerifyBinary(bin); err → t.Skipf("the real verifier refuses %s: %v", bin, err)  (cap-aware gate: xattr caps + not nosuid; never Getuid)
    // capturetest.SkipIfPrivileged(t)  (the server itself must be unprivileged)
    // server with Config.Capture.PerfAgent = bin; capture {command: [workload, "busy", "5s"], duration "2s", modes ["cpu"]} → capture_wait → done
    // top on the returned handle → a row whose name contains "main.spin" (the workload's busy leaf)
}
func TestRealKernelStacks(t *testing.T) // as above; skip unless b.Caps.Has(capture.CapSyslog); kernel_stacks true → result notes kernel addresses; the profile has at least one kernel frame
```

- [ ] **Step 2: Prepare the binary** (this needs the user: `setcap` needs root, and the executor asks first)

```bash
make build                                   # static blazesym
mkdir -p ~/bin && cp perf-agent ~/bin/perf-agent-captest   # NOT /tmp: nosuid drops file caps
# ask the user to run:
sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep ~/bin/perf-agent-captest
getcap ~/bin/perf-agent-captest
```

- [ ] **Step 3: Run it**

Run: `PERF_AGENT_BIN=$HOME/bin/perf-agent-captest go test -race -run 'TestReal' -v ./internal/perfmcp/`
Expected: PASS, both tests (no sudo on the test run itself). Without `PERF_AGENT_BIN`, both SKIP with their reasons.

- [ ] **Step 4: Commit**

```bash
git add internal/perfmcp/capture_real_test.go
git commit -m "perfmcp: a gated real capture with the setcap'd perf-agent"
```

### Task 19: Verification, review and the draft PR

**Files:** none new.

- [ ] **Step 1: The whole suite**

Run: `CGO_CFLAGS=… CGO_LDFLAGS=… go test -race ./...`
Expected: PASS.

Run: `go build ./... && GOOS=darwin go build ./internal/capture/ ./internal/perfmcp/ ./cmd/perf-agent-mcp/`
Expected: builds, since `capture_other.go` covers non-Linux.

- [ ] **Step 2: Lint**

Run: `golangci-lint version` (it must print 2.11.4; exit 127 means it is not installed, so install it), then `golangci-lint run ./...`.
Expected: no findings.

- [ ] **Step 3: The pre-existing tree is untouched**

Run: `git diff --stat ai-mcp-m2b -- main.go telemetry.go perfagent gpu cmd/flamegraph internal/profilemodel`
Expected: empty. `git diff --stat ai-mcp-m2b -- '*.go' | grep -v -E '^ (internal/(capture|perfmcp|mcpstdio)|cmd/perf-agent-mcp|capture_contract_test.go)'` prints only the summary line.

- [ ] **Step 4: The real capture once more** (Task 18's command), and a manual smoke with Claude Code over stdio: a `capture` of a busy loop, then `capture_wait`, then `top`. Record the transcripts under `.superpowers/sdd/2026-11-15-capture/`.

- [ ] **Step 5: Codex review through Workforce.** Ask the perf-agent Codex session to review the branch diff against the spec. Verify each claim before acting on it, and record the findings and the rulings in `.superpowers/sdd/2026-11-15-capture/progress.md`.

- [ ] **Step 6: Push a draft PR** stacked on the trimmed #264. Its body has no AI attribution and no session URLs. **Do not merge**; ask the user.

```bash
git push -u origin ai-mcp-m2c-capture
gh pr create --draft --base ai-mcp-m2b --title "perf-agent-mcp: capture profiles from the MCP server" --body-file .superpowers/sdd/2026-11-15-capture/pr-body.md
```

---

## Self-review

**1. Spec coverage**

| Spec section | Where |
|---|---|
| §4.0 state machine, engine points, ownership, registry, historical lookups | Tasks 12, 13, 14 |
| §4.1–4.6 tools, arguments, result, errors, annotations, list | Task 14 (errors also Tasks 1, 7, 13) |
| §5.1 trust | Task 1 |
| §5.2 argv and contract test | Task 2 |
| §5.3 process setup, readiness, timeouts, early stop, cancel, stuck child | Tasks 6, 7 |
| §5.4 exit codes | Task 7 |
| §6.1 bounds, lock, size kill | Tasks 9, 5, 7 |
| §6.2 destination, owner | Task 10, and Task 16 for the cross-kind test |
| §6.3 PID rule and recheck | Tasks 3, 7 |
| §6.4 commands, launcher, containment, env | Tasks 3, 6 |
| §6.6 reaper and survivors | Tasks 6, 7, 16 |
| §7.1–7.4 layout, manifest, retention, intent/commit, decision rule, recovery, fault injection | Tasks 10, 11, 12 |
| §8 GPU | Tasks 3, 7, 13 |
| §9 stdio control and writer | Task 8; §9.1 progress: Tasks 8, 14 |
| §10 tests | distributed as named in each task, with the process-level ones in Task 16 |
| §11 changes | Tasks 9, 14, 15, 17 |
| §12 threat model | Task 17 (the docs) |
| Codex #418 carries | Task 10 (post-link errors committed), Global Constraints (wording), Task 13 (recipe), Task 14 (replay executes the steps) |

**2. Step scan:** every code step names its file, signatures and the spec's values; no "handle edge cases". Task 16 Step 3 is the one open step by design: it fixes what the process tests expose, and each fix carries its test.

**3. Type consistency:** these names are used the same way in every task:
- `capture.Error`/`Code`, `Binary`, `Identity`, `Shim`, `Settings`, `TargetState`, `Proc`, `SelfID`, `Lease`, `ReaperSpec`, `RunSpec`, `Outcome`;
- perfmcp's `captureDir`, `captureTxn`, `manifest`, `fileIdentity`, `wireSettings`, `decisionWord` (defined in Task 10, moved in Task 12), `registry`, `lookupPool`, `job`, `jobView`, `captureRequest`, `captureResult`, `recompute`, `toolCall`;
- the seams `captureFault`, `writeErr`, `ownerAlive`, `now`, `capturePublish`.

**4. Review Focus:** the five tests are placed in Tasks 14, 7, 7, 13 and 10.

**5. Proportion:** the plan is about the spec's size in bytes and has fewer lines. Its code blocks are test names with their assertions and a few exact literals (argv, fd layout, constants), not function bodies.
