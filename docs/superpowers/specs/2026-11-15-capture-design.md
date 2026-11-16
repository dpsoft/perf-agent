# M2c: capturing profiles from the MCP server — design addendum

**Status:** design only, not implemented, not committed. Addendum to
[2026-11-13-ai-flamegraph-design.md](../../docs/superpowers/specs/2026-11-13-ai-flamegraph-design.md)
(the spec) and
[2026-11-14-live-page-design.md](../../docs/superpowers/specs/2026-11-14-live-page-design.md)
(M2b). Where this addendum and those disagree, this addendum wins for the
capture milestone; everything else in them still binds.
**Date:** 2026-11-15
**Verified against:** perf-agent `main` at 13c31b88 (`main.go`,
`telemetry.go`, `perfagent/agent.go`, `docs/requirements.md`) and the
`ai-mcp-m2b` worktree (`internal/perfmcp`, `internal/mcpstdio`,
`cmd/perf-agent-mcp`).

Each decision says who decides:
- **[user]** — decided by the user; not open.
- **[user?]** — needs the user's confirmation; the stated default holds
  until then (questions in §13).
- **[mine]** — settled here; the user may overrule it.

---

## 1. Intent

Today the agent can explain a profile only after the user has captured it.
M2c adds one tool, `capture`. It runs the already-setcap'd `perf-agent`
binary for a bounded time against one process, writes the pprof (and the
GPU executions log) into a capture directory inside the profiles root, and
opens the result through the existing `open` path. It returns a handle, so
the agent goes straight on to `top`, `path` and `diff`.

**The main user is an autonomous agent running the loop [user]:**

```
capture → analyse → change code → re-capture → compare → repeat
```

The UI is secondary and is for humans. The design therefore aims first at
the following.

- **Fully headless in plain stdio `perf-agent-mcp`.** No long-lived server
  is required. The serve process (§9) reuses the same code.
- **Comparable runs.**
  - Every capture's settings are recorded in a manifest on disk (§7.2).
  - A re-capture can inherit them from its baseline (`compare_with`).
  - A re-capture whose settings differ from its baseline is refused.
- **Obvious base and new pairs.**
  - Captures are grouped in a **series**, one directory per series.
  - Files are numbered in order and labelled: `001-base`, `004-cand3`.
- **Quick compare.** `compare_with` returns the diff summary in the same
  call, with both handles, so a full `diff` is one call away.
- **Bounded disk over many iterations.** A retention policy applies only
  to files this server wrote in the capture directory (§7.3).
- **Failures an agent can act on.** Errors are structured, with a code,
  whether a retry can help, and the next step (§4.4).

This is the step the spec's M3 loop needs ("explanatory profiles are
captured separately", §8).

## 2. User decisions

1. **[user] Approach.** `perf-agent-mcp` stays unprivileged and never loads
   BPF itself. It runs the existing `perf-agent` binary as a subprocess, for
   a bounded duration, against a PID or a command it launches. Limits apply
   to duration and destination.
2. **[user] Scope of targets.** The user accepts that this lets the agent
   profile any process the user can profile. Guardrails still apply (§6).
3. **[user] Build on top (2026-11-15, clarified the same day).** What
   predates the MCP work stays unchanged and is used as-is:
   - the `perf-agent` capture binary and its flags;
   - `cmd/flamegraph` and its outputs;
   - `gpu/*`;
   - `profilemodel` semantics.

   The MCP code is ours and **may** change: `internal/perfmcp`,
   `internal/mcpstdio`, `internal/explain`, `cmd/perf-agent-mcp` and the
   explained page. New code still goes in new files where that is natural.
   §11 lists every change to an existing file. **No perf-agent flag is
   added, and no perf-agent file changes.**
5. **[user] Loop first, headless first (2026-11-15).** The capture tool
   must work completely in plain stdio `perf-agent-mcp`; serve only reuses
   it. Optimise for repeated captures:
   - comparable runs, with their settings recorded;
   - naming that makes base and new obvious;
   - a quick open and diff;
   - bounded disk across iterations;
   - clear failure results.
4. **[user] Long-lived server (2026-11-15).** The live page moves into one
   long-lived local process the user starts (`perf-agent-mcp serve`, the same
   binary). It owns all state, serves the page, and speaks MCP over
   Streamable HTTP on loopback, with a token and the existing `livehttp`
   guard. Plain stdio `perf-agent-mcp` becomes a thin adapter to that
   server, or works standalone without the UI.
   - `capture` lives in that server.
   - One capture runs at a time across all connected agents.
   - A finished capture appears on the page.
   - The server, not a session, owns the child processes and cleans them
     up.

   §9 describes how this changes concurrency, cancellation and the handling
   of several agents. The rest of this design holds in both modes.
6. **[user] Answers (2026-11-15).**
   - **Command launch is allowed by default.** The agent may launch
     commands and profile them, with argv, no shell and a scrubbed
     environment. `--capture-command` becomes optional hardening.
   - **Retention:** yes, with a TTL (`--capture-ttl`, default 7 days) plus
     the byte budget. It never deletes pinned, open or current-baseline
     captures, and deletes only server-written, manifest-verified files.
   - **Targets:** "any ordinary process of yours", by the ptrace-like rule
     of §6.3. Other users' and root's processes stay refused, because the
     child holds BPF capabilities.
   - **`kernel_stacks`:** an opt-in argument. The file then contains
     kernel addresses, and the binary needs `cap_syslog`.
7. **[controller] Rulings (2026-11-15).**
   - Captures go under `<profiles>/captures` by default.
   - The call was to block until done, with progress. **Superseded by
     CAP-R8 (review #404):** a two-step API, `capture` then
     `capture_wait` (§4.0), because Codex's 60 s tool timeout ruled out a
     75 s blocking call.
   - The SIGKILL orphan window was accepted as "bounded by `--duration`".
     Review #399 showed it is not bounded, and the reaper (CAP-R2, §6.6)
     now closes it.
   - `perf-agent-mcp` stays source-only.

### What this supersedes

- **Spec §3, non-goal 2** ("Capturing or controlling profiling from the
  MCP server"): superseded for `capture` only. Continuous or fleet
  profiling is still a non-goal.
- **Spec §7.1** ("Never starts the profiler … never needs capabilities").
  It now reads: the server never links or starts `perfagent.Agent`
  in-process, never loads BPF, and never holds or raises a capability. It
  may run the verified `perf-agent` binary as a child process. The
  `TestTheServerLinksNoProfiler` link test is unchanged and must still
  pass.
- **M2b D3** ("it starts no processes besides M2's read-only `git`").
  Superseded: the server may also start the verified `perf-agent` binary
  and workload commands the agent asks for (§6.4). It still never opens a
  browser.

## 3. Goals, non-goals, anti-goals

**Goals**
- An agent can capture a CPU, off-CPU or GPU profile of a process and get
  a handle in a single tool call.
- Every capture is bounded:
  - in time;
  - in sampling rate;
  - in bytes, per capture and on disk;
  - in count, per window;
  - in concurrency: one capture at a time.
- The result says what was captured and what was not:
  - the attach latency;
  - whether the target exited;
  - whether kernel stacks were skipped;
  - whether the GPU profile is empty.

**Non-goals (M2c)**
- System-wide (`-a`) captures and cgroup targets. perf-agent has no cgroup
  target, and `-a` covers other users' processes.
- Profiling the children of a launched command. `perf-agent --pid`
  follows one process and its threads.
- Attaching the GPU to a running process that did not load the CUPTI
  adapter at start. That is the dynamic-attach PoC, a separate project.
- PMU counters, perf.data output, flame graph files written by perf-agent,
  debuginfod, the metrics endpoint and GPU telemetry.
- A progress stream. The tool call itself is the progress indicator, and a
  live-page banner is deferred (§10).

**Anti-goals (failures even if it works)**
- The server holds, raises or passes on a capability, or runs anything
  through `sudo`, `pkexec` or a shell.
- The agent profiles something the user could not ptrace: another user's
  process, a setgid or capability-holding process, or a non-dumpable one
  (§6.3).
- A capture outlives its bounds silently. The reaper kills on server
  death or at an absolute deadline. Survivors after `StopGrace` are
  reported by identity (CAP-R3b). The reaper being killed as well is the
  documented unbounded case (§6.6).
- A capture overwrites a file, or writes outside the capture directory.
- Retention deletes anything the server did not write, or a capture that
  is kept, open, pinned or the current baseline (§7.3).
- A diff is presented as comparable when the settings differ. Mismatched
  settings are refused, and target differences are reported (§4.3).
- Child output reaches the MCP stream (stdout in stdio mode, the HTTP
  response in serve mode).
- `perf-agent` is started with an argument that opens a network connection
  (`--debuginfod*`, `--metrics-listen`), or with any flag outside a fixed
  list.

## 4. The tools **[mine; the two-step shape is CAP-R8]**

### 4.0 Jobs: one state machine, server-owned, bounded calls (CAP-R8, CAP-R9..R11, SRV-R7)

**Why.**
- **Review #404:** Codex's MCP client times a tool call out after 60 s by
  default, so a blocking `capture` (worst case 75 s) was not safe end to
  end. The two-step API of CAP-R8 fixed that.
- **Review #408** then found three problems in it:
  - `prepare` still ran inside `capture`, with unbounded latency from
    digests, retention hashing, fsync and waiting for the engine;
  - `open` and `diff` ran on whichever request first saw the completion,
    with no owner that could resume them;
  - the two transports disagreed on rollback after an `open` or `diff`
    failure.

  CAP-R9 replaces all of that with one state machine. Both clients'
  timeouts are in §9.1.

**This section is the single definition of the capture lifecycle
(CAP-R13).** The serve design references it and keeps no copy of its
own.

**The state machine (CAP-R9).** It is the same in stdio and in serve:

```
accepted → preparing → running → committing → done | failed | cancelled
```

| State | Holds | Work |
|---|---|---|
| `accepted` | nothing | `capture` has validated the arguments (pure checks, no I/O) and admitted the job in memory, to the single active-job slot; the job is queued |
| `preparing` | first the **dir lease**, then the **engine** for shared inputs | The job takes the non-blocking captures lock (the dir lease, §6.1) **before any filesystem side effect**. A cross-process `busy` is therefore an asynchronous job failure. It then runs recovery. Next, **holding the engine**, it resolves and pins the `compare_with` baseline, snapshots the protected inputs (handles open in the store, publication pins) and runs retention against them. It releases the engine, then writes `.seq` and the intent, checks space, creates staging, runs the target preflight and starts the reaper. This is the one order, §7.4 steps 1–7 |
| `running` | the lease only | the reaper and its children capture (§5, §6); rechecks every 1 s |
| `committing` | the lease only | validate, hash, record the identities, link, and write the **manifest, which is the commit point** (§7.4 steps 8–11). The file commit never holds the engine |
| `done` | the engine, for the derivations and pin release; then nothing | committed. **Post-commit derivations** then run in the same job: `store.open` of each profile and the `compare_with` diff, plus in serve the publication. Their state is `derived: pending \| ready \| failed` while the job is in the registry, and `recompute` once it has left (CAP-R17). Then the job releases the baseline and protected-input pins. A derivation failure (for example `open` over the model limits) is **reported in the result and never rolls files back** |
| `failed`, `cancelled` | the lease, until the rollback is done | rolled back by the job (§7.4 live rollback); a failure manifest is written |

The lease (the captures `flock`, held by the reaper too, CAP-R6b) is
released once the reaper has reaped both children and the files are
committed or rolled back. **The child wait and the file commit run
outside the engine.** The engine sections do file I/O by design: baseline
verification, retention and `store.open` (§7.4 step 3, and the
derivations).

- **Every phase runs in a server-owned job goroutine.** `capture` only
  enqueues the job and returns:

  ```json
  {"job": "j7", "status": "accepted", "eta": "…"}
  ```

  It does no disk I/O, takes no lock, and never waits on the engine. The
  capture id (`parse-opt/002`) appears in the job's state once
  `preparing` has assigned it.
- **The job owns its rollback.** The transports never decide one.
- **The engine is taken only at two points (CAP-R13):**
  - `preparing`'s shared inputs (baseline, pins, protected inputs,
    retention);
  - `done`'s derivations and pin release.

  The engine is the store, the diff cache, attempts and publications,
  serialised by one engine lock in stdio and by the serve design's
  engine in serve. This replaces M2's "the request goroutine owns all
  mutable state": the engine's holder owns it. The live page's HTTP
  handlers still read snapshots only.

**The tools:**

| Tool | Blocks for | Returns |
|---|---|---|
| `capture` (arguments §4.2) | microseconds: no I/O, no lock, no engine | `{job, status: "accepted", eta}`, or a synchronous `invalid_argument` or `busy` (in-process slot) error. Cross-process `busy` and every later error arrive as the job's `failed` state |
| `capture_wait {id, timeout?}` (a job id or a capture id) | at most `timeout`, default 30 s, **capped at 45 s**, and never past its own deadline. It reads the job registry; a historical id goes to the lookup pool (CAP-R14). Never the engine | the current state. When `done` with `derived: ready`, the §4.3 result. Otherwise `{job, id?, status, phase, elapsed, eta, derived?}`, or `status: "loading"` when a historical lookup has not finished by the deadline |
| `capture_cancel {id}` | the **control path** (CAP-R10): no request queue, no engine | `{job, status: "cancelling"}`. It does one CAS on the job's decision word and no file I/O; the job does the cleanup. **The terminal decision rule is in §7.4 (CAP-R16):** a cancel wins against everything except a manifest link already in flight, and `too late: committed` is said only once the manifest link has succeeded |
| `capture_list` (§4.6) | the in-memory jobs at once; the history through the lookup pool, to the call's deadline | active and recent jobs first, then the history from the manifests, with `"loading": true` for pages not read in time |

**Ownership (SRV-R7).** Jobs belong to the server, not to sessions:
- in serve, any session holding the server token may `capture_wait` or
  `capture_cancel` any job, since the state is one user's;
- a session's `DELETE`, or its eviction, never cancels a job;
- in stdio there is one session. EOF there, or a dead client
  (CAP-R7b), cancels all jobs, because nobody would collect them.

**The bounded registry (CAP-R11):**
- One active capture at a time.
- Terminal jobs stay in memory up to `Capture.MaxJobs` (32) and
  `Capture.MaxJobBytes` (4 MiB) of results, least recently used first.
- **Derived results live only in memory (CAP-R17).** The open and diff
  summaries, with their handles, are kept only in the job registry,
  under `MaxJobs` and `MaxJobBytes`. Nothing derived is persisted: there
  is no sidecar, no journal and no extra lease. The terminal outcome,
  settings, notes and error live in the committed series manifest
  (§7.2), as before.
- **After a registry eviction or a server restart,** `capture_wait` by
  capture id answers **terminally** from the manifest: `status` from
  its outcome, and `derived: "recompute"` with the exact calls to run.
  For example:

  ```json
  "recompute": [
    {"tool": "open", "arguments": {"path": "captures/parse-opt/002-cand1-cpu.pb.gz"}},
    {"tool": "diff", "arguments": {"base_handle": "<from open of 001>", "base_view": "cpu/nanoseconds#0",
                                   "new_handle": "<from open of 002>", "new_view": "cpu/nanoseconds#0"}}
  ]
  ```

  It is never `pending`. `pending` exists only for a live job in `done`.
- **Historical lookups (CAP-R14):**
  - manifest reads run on a bounded worker pool of **2 workers** with a
    queue of 16;
  - a lookup for an id already queued or running is shared;
  - `capture_wait` and `capture_list` wait only to their own deadline,
    then return `"loading"`. The lookup continues, its answer goes into
    the registry LRU, and the next wait gets it at once;
  - a full queue answers `"loading"` with a `retry_after`;
  - no goroutine is started per timed-out wait.
- An answer from disk, or one whose handles the store has since evicted,
  says `"handles": "reopen"` and carries the `open` and `diff` arguments
  to recreate them.
- **Heavy pins** (the loaded profiles, the baseline, the diff) are held
  only while the job derives. After that the handles live in the store
  under its normal LRU, and the job keeps only its result JSON.

**A loop iteration:**

```
capture → {job} → capture_wait (repeated while not done/ready)
        → result with handles and compare
```

A 10 s capture is typically one `capture_wait`; a 30 s one is one or
two.

`notifications/cancelled` on a `capture_wait` only ends that wait.

### 4.1 Name

The tool is `capture`, not `profile`. Every other tool uses "profile" for
the file, as in `profile_digest` and `profile_name`. perf-agent's own word
for the act is capture (`CaptureID`, `CaptureWindow`).

Without `--perf-agent`, the tool is still listed. Its description starts
with `DISABLED: start perf-agent-mcp with --perf-agent PATH (the setcap'd
perf-agent binary) to enable capturing.`, and every call returns
`isError` with that text. This is the same pattern as `render_flamegraph`
without `--output`.

### 4.2 Arguments

| Argument | Type | Default | Rule |
|---|---|---|---|
| `pid` | integer | — | Exactly one of `pid` and `command`. Checked by §6.3. |
| `command` | array of strings | — | argv, on by default [user]. `command[0]` is an absolute path, or a bare name looked up once in the scrubbed `PATH`. No shell. With `--capture-command`, it must be one of those paths (§6.4). |
| `cwd` | string | first `--source` root, else the profiles root | `command` only. A directory inside a `--source` root or the profiles root, judged as `rel` judges paths. |
| `modes` | array of `"cpu"`, `"offcpu"`, `"gpu"` | `["cpu"]` | Distinct values. `gpu` needs `--gpu-shim` (§8). |
| `duration` | string, Go duration | `"10s"` | In [1s, `Capture.MaxDuration`]. Same syntax and default as perf-agent's `--duration`. |
| `sample_rate` | integer, Hz | 99 | In [1, `Capture.MaxSampleRate`]. Same default as perf-agent. |
| `kernel_stacks` | bool | false | Opt-in [user]. Refused unless the binary has `cap_syslog`, since without it kernel frames come out unsymbolized and no error is reported. The result and the manifest note that the file contains kernel addresses. |
| `unwind` | `"auto"`, `"fp"` or `"dwarf"` | `"auto"` | Passed to perf-agent's `--unwind`. |
| `series` | string | `"adhoc"` | The series (a subdirectory of the capture directory): `[A-Za-z0-9._-]`, 1–64 bytes, not starting with `.`. One loop is one series. |
| `label` | string | `"run"` | Names the capture inside its series, as in `base` or `cand3`. Same character rule, 1–32 bytes. |
| `compare_with` | string | — | A capture id from this series (as `capture_list` shows it, for example `001`). The baseline's settings (`modes`, `duration`, `sample_rate`, `unwind`, `kernel_stacks`) are **inherited**. Passing any of them with a different value is refused (`incomparable`, §4.4). The result carries the diff (§4.3). The target may differ: changed code is the point of the loop. |
| `keep` | bool | false | Exempts this capture from retention (§7.3), as for the baseline of a long loop. |
| `show` | bool | true | Serve mode with the live page only: make the facts-only publication current (§9). |

Durations are strings and other integers are JSON integers, as in existing
tool inputs (`limit`, `context`). There is no `env` argument (§6.4) and no
`tags` argument.

**A loop iteration**, as the agent calls it:

```json
capture {"command": ["/home/u/proj/bench"], "cwd": "/home/u/proj", "series": "parse-opt", "label": "base", "duration": "20s", "keep": true}
   → {id "parse-opt/001", status "running"}; capture_wait {"id": "parse-opt/001"} → handle h1
… change code, rebuild …
capture {"command": ["/home/u/proj/bench"], "cwd": "/home/u/proj", "series": "parse-opt", "label": "cand1", "compare_with": "001"}
   → running; capture_wait → handle h2, diff summary 001 → 002, with the diff call to run for more
```

### 4.3 Result

`capture_wait`'s result once the capture is done (CAP-R8, §4.0):

```json
{
  "capture": {
    "id": "002",
    "series": "parse-opt",
    "label": "cand1",
    "manifest": "captures/parse-opt/002-cand1.capture.json",
    "settings": {"modes": ["cpu"], "duration": "20s", "sample_rate": "99", "unwind": "auto", "kernel_stacks": false},
    "settings_digest": "sha256:…",
    "target": {"pid": "41234", "comm": "bench", "exe": "bench",
               "start_time": "1234567", "launched": true,
               "exit": "exited 0 after 8.2s"},
    "modes": ["cpu"],
    "requested_duration": "10s",
    "profiled_duration": "8.2s",
    "attach_latency": "1.4s",
    "sample_rate": "99",
    "kernel_stacks": false,
    "stopped_by": "command_exit",
    "perf_agent": {"path": "/home/u/bin/perf-agent", "version": "v1.5.2",
                   "revision": "13c31b88", "capabilities": ["cap_bpf", "cap_perfmon", "cap_sys_ptrace", "cap_checkpoint_restore", "cap_syslog"]},
    "workload_output_tail": {"stdout": "...", "stderr": "...", "untrusted": true}
  },
  "profiles": [
    {"mode": "cpu", "path": "captures/parse-opt/002-cand1-cpu.pb.gz",
     "open": { "...": "exactly the open tool's result: handle, views, default_view, capabilities, quality, notes" }}
  ],
  "compare": {
    "base": {"id": "001", "label": "base", "handle": "<reopened handle of 001>"},
    "new":  {"id": "002", "label": "cand1", "handle": "<handle above>"},
    "comparability": {"settings": "equal", "target_exe": "changed (build-id a1… → b7…)", "profiled_duration": "19.6s vs 19.8s", "attach_latency": "1.3s vs 1.4s"},
    "views": [
      {"view": "cpu/nanoseconds#0", "base_total": "…", "new_total": "…",
       "rows": ["… the first Capture.CompareRows rows of the existing diff tool, same shape …"],
       "next": {"tool": "diff", "arguments": {"base_handle": "…", "base_view": "cpu/nanoseconds#0", "new_handle": "…", "new_view": "cpu/nanoseconds#0"}}}
    ]
  },
  "evicted": ["parse-opt/003-cand2"],
  "notes": [
    "profiling began 1.4s after the command started; the first 1.4s are not in the profile",
    "only pid 41234 was profiled, not processes it started"
  ],
  "publication": "p4"
}
```

- **`compare`:** present with `compare_with`. The baseline is reopened
  from its files with the existing `store.open`, so it works in a new
  server process too. The diff is computed by the existing `diff` code,
  with the same rows, quality warnings and comparability. `next` is the
  exact call for the full, paged diff.
- **`comparability`:** reports, never hides:
  - the settings, equal by construction;
  - whether the target executable changed. Its build-id is read from
    `/proc/<pid>/exe` with `debug/elf`;
  - the profiled duration and the attach latency of each side.

  Differences in those are notes for the agent to weigh. Sample totals
  are not normalised: per the spec, nothing is multiplied by the period.
  Normalisation by work belongs to the diff's existing `work_base` and
  `work_new`.
- **`evicted`:** the captures retention removed to make room (§7.3).

- **`stopped_by`:** one of `duration`, `target_exit`, `command_exit` or
  `cancel` (`cancel` appears only in `capture_list`, §9).
- **`profiled_duration`:** the time from the readiness line (§5.3) to the
  stop.
- **`profiles[].open`:** the `open` tool's own result for that file,
  produced by the same code path. If the whole response would exceed
  `MaxResponseBytes`, each `open` is cut to `handle`, `views`,
  `default_view` and `quality`, with the note "call `open` with path for
  the full result".
- **`target`:** carries `comm` and the basename of `exe`, never `cmdline`,
  which can hold secrets. The workload's argv is not echoed back either:
  the agent sent it.
- **`workload_output_tail`:** the last `Capture.MaxOutputTail` bytes (2 KiB
  each) of the launched command's stdout and stderr. It is labelled
  untrusted program output. perf-agent's own stdout is never returned, and
  its stderr only on failure.
- **`publication`:** present only when the live page is on (§9).

### 4.4 Failures an agent can act on **[mine]**

A failure is an `isError` result. Its text is one JSON object, not prose,
because a loop branches on it:

```json
{"error": {"code": "target_exited", "message": "pid 41234 exited 0.3s after the start, before profiling began",
           "retryable": true, "retry_after": "0s",
           "next": "the command finishes faster than perf-agent attaches (1.4s last time); give it more work, e.g. more iterations",
           "capture_id": "003", "perf_agent_exit": "1", "stderr_tail": "…"}}
```

| `code` | `retryable` | Meaning, and what `next` says |
|---|---|---|
| `disabled` | no | A flag is missing; it names the flag |
| `invalid_argument` | no | Names the argument and the valid range |
| `incomparable` | no | A setting differs from the `compare_with` baseline; it names the setting and both values. Drop the argument, or start a new series |
| `unknown_baseline` | no | No such capture in the series; it lists the series' last ids |
| `busy` | yes | Another capture holds the captures lock: this process, another stdio server, or an orphaned reaper (CAP-R6b). The message names the holder (server and reaper PIDs, capture id); `retry_after` is its latest end |
| `unknown_capture` | no | `capture_wait` or `capture_cancel` named an id this server never started; `capture_list` shows the ids |
| `target_changed` | yes | The target's credentials, dumpable flag, capabilities or start time changed during the capture; it was discarded (CAP-R1b) |
| `owner_mismatch` | no | The capture directory belongs to the other server kind (SRV-R5, §6.2) |
| `budget` | yes | A window limit; `retry_after` is when the window frees |
| `disk` | no | `MaxStoredBytes` or `MinFreeBytes` cannot be met even after retention; it names the series using most space |
| `exists` | no | The final name is taken (M2-R4); pass another `label` |
| `target_refused` | no | Which rule of §6.3 failed (another uid, setgid, non-dumpable, capabilities, kernel thread) |
| `target_not_found` | no | No such PID |
| `target_exited` | yes | The target or command ended before readiness; the command's output tail is included |
| `pid_reused` | yes | The start time changed (§6.3) |
| `gpu_not_injected` | no | §8 |
| `perf_agent_refused_args` | no | Exit 2: perf-agent is too old or too new for this server; the version is included |
| `perf_agent_failed` | maybe | Exit 1, with the stderr tail and advice for known stems (§5.4); `retryable` is false for capability errors |
| `perf_agent_unstartable` | no | Exit 126/127 or a loader error (§5.4) |
| `binary_changed` | no | §5.1: restart the server |
| `start_timeout`, `finish_timeout` | yes | Names the limit and its value |
| `too_large` | no | A file passed its limit; suggests a shorter duration or a lower rate |
| `no_output` | yes | Exit 0 without the expected file |
| `stuck_child` | no | Processes survived `StopGrace`; `survivors` lists their PID, `comm`, start time and state (CAP-R3b); do not retry; tell the user |
| `cancelled` | — | Only in `capture_list`; no response is sent for a cancelled request |

Every message is bounded, and repeats agent input only through `echo`, as
M2 does. A failed capture keeps no profile. Its manifest is kept, with
`outcome: "failed"` and the code, so `capture_list` shows the failure in
the series' history. It is subject to retention like any capture.

### 4.5 Annotations

- `readOnlyHint: false`.
- `idempotentHint: false`.
- `destructiveHint: true` in every configuration, PID-only included. Two
  things can destroy: retention deletes old captures (§7.3), and a launched
  command can do anything.
- `openWorldHint: true`, because launch is on by default.

### 4.6 `capture_list` **[mine]**

`capture_list(series?, limit?, cursor?)` is read-only. It is built from
the **manifests on disk** (§7.2), not from memory, so a loop whose stdio
server restarts between iterations still finds its history. Each row
holds:
- id, label, outcome (`ok` or the failure code), `stopped_by`;
- settings and `settings_digest`, the target `comm` and build-id;
- the time and the paths;
- `kept`, and the baseline it was compared with;
- the handle, if this server has it open.

With no `series`, it lists the series with their capture counts and
bytes. It pages like `top`.

It also serves §9: with several agents, and connections that can drop, a
capture's result must be findable without the call that started it.

### 4.7 How it composes

```
capture(series, label=base, keep) ─► capture_wait ─► handle ──► top / path / facts / source
   ... agent changes code ...
capture(series, label=candN, compare_with=base id) ─► capture_wait ─► compare summary ──► diff (paged) ──► accept / reject
   └─► render_flamegraph for the PR artifact (M3), page_explain when a human watches (serve)
```

The capture files are ordinary inputs. Once opened they count in
`isInput`, so a later `render_flamegraph` can never overwrite them.

## 5. Running perf-agent

### 5.1 Binary discovery and trust **[mine]**

`--perf-agent PATH` names the binary. There is no default and no PATH
lookup. The path must be absolute, and is resolved once with
`EvalSymlinks` at start-up. Nothing is ever run to check it. The checks,
at start-up and before every exec:

| Check | How | Why |
|---|---|---|
| Regular file, executable by the server's user, not setuid or setgid | `Lstat` of the resolved path before opening it (a FIFO or device is never opened), then `Fstat` of the one descriptor opened with `O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC`, which must be the same dev/inode. "Executable" uses the kernel's class: the owner bits when the server owns the file, else the group bits when it is in the file's group, else the other bits | Setuid would change uids, which the target rule (§6.3) depends on. A user-owned 0405 file is refused rather than failing at exec |
| Not writable by group or other (`mode & 022 == 0`); owner is root or the server's uid | The same `Lstat` and `Fstat` | A user-owned file is accepted, since the user builds in their own directories. Writing to a file clears its `security.capability`, so the user cannot alter a capped file and keep its caps |
| No other user can replace it through a directory (CAP-R18, CAP-R18b) | `Lstat` of each ancestor of the resolved path, walking from the file upward. Each must be owned by root or the server's uid, and not writable by group or other unless sticky (`+t`). The walk is satisfied by a **gate**: an ancestor owned by root or the server's uid whose mode denies search to both group and other (`mode & 011 == 0`, e.g. a 0700 home). A failing directory below a gate is excused; directories above it are still checked. An ancestor that is no longer a directory is refused at once | Whoever can write a directory, or owns it, can rename the file away and put another in its place. Nobody else can reach anything below a private directory. The refusal names the nearest uncovered directory and says "move perf-agent under a private directory you own (e.g. under ~), or chmod go-w <dir>" |
| Filesystem not mounted `nosuid` or `noexec` | `syscall.Fstatfs` on the descriptor, `Flags & (MS_NOSUID|MS_NOEXEC)` | File capabilities are ignored on a nosuid mount, as `/tmp` is here (memory note). Refused with "copy it outside /tmp, to a private directory you own (e.g. under ~), and setcap it there" |
| File capabilities | `syscall.Getxattr("/proc/self/fd/N", "security.capability")` on the descriptor (the `syscall` package has no `fgetxattr`), parsed as `vfs_cap_data` | See the next table. This is the getcap check without running getcap |
| It is perf-agent | `debug/buildinfo.Read` on the descriptor: main module `github.com/dpsoft/perf-agent`, main package path is the module root. Echoed paths, the version and the revision are cut to 200 bytes | Identifies the file as perf-agent without running it (a marker, not authentication; the capability xattr is the trust anchor). The version and `vcs.revision` go into each result |
| Shared blazesym | `debug/elf.NewFile` on the descriptor: a `DT_NEEDED` names a `*blazesym*` library | A warning, not a refusal. Under secure execution `LD_LIBRARY_PATH` is ignored, so the library must be in a system path or linked statically (memory note) |
| Unchanged during verification | The identity from the `Fstat` plus the raw xattr bytes, compared with the path's identity after the reads | The path still names the inode that was read, and its size, times and xattr did not change while it was read. Otherwise `binary_changed`: "check that no build, copy or setcap is still writing it, then restart perf-agent-mcp" |
| Unchanged since start-up | `(dev, ino, size, mtime, ctime)` and the xattr bytes compared before each exec | Refused with "perf-agent changed since start-up; restart perf-agent-mcp" |

How `security.capability` is parsed:
- The magic must be `VFS_CAP_REVISION_2` or `_3`. For revision 3, `rootid`
  must be 0: a namespaced capability does not apply in the initial user
  namespace.
- `VFS_CAP_FLAGS_EFFECTIVE` must be set (`+ep`).
- The inheritable set must be empty.
- **Required in permitted:** `cap_bpf`, `cap_perfmon`, `cap_sys_ptrace` and
  `cap_checkpoint_restore`. These are the documented minimal set for
  kernels ≥ 5.9, and the floor here is 6.2.
- **Allowed in addition:** `cap_syslog`, which enables `kernel_stacks`;
  `cap_sys_admin`, which the user accepts; and `cap_dac_read_search`.
  `cap_dac_read_search` is accepted with a start-up warning: it only helps
  with other users' processes, which `capture` refuses.
- **Anything else is refused,** with the capability named: for example
  `cap_dac_override`, `cap_fowner`, `cap_setuid` or `cap_sys_module`. With
  DAC override, a symlink planted in the staging path could make the child
  write anywhere.

**The server itself** must run with euid ≠ 0 and empty effective and
permitted capability sets, read from `/proc/self/status`. Otherwise
`capture` is disabled: under root, the same-uid rule (§6.3) would cover
every root daemon. The server never runs `sudo`. The error text for missing
capabilities quotes the setcap line from `docs/requirements.md`, not
"run as root".

**Swap race, accepted.** Between the check and the exec, a process with
the user's uid could replace the file in a user-owned directory. It gains
nothing it did not have: the replacement either has no capabilities and
runs as the user, or is another capped binary that receives perf-agent's
argv. Executing through a held fd (`/proc/self/fd/N`) would close the race,
but it renames the child's `comm` to `N` and still guards against no
escalation, so it is not done.

### 5.2 The argv **[mine]**

The argv is built by one function. It is the only producer of the
`perf-agent` argv, and it emits flags from a fixed list only:

```
<verified path> --pid N --duration D --sample-rate R --unwind U --no-debuginfod
                [--profile --profile-output S/cpu.pb.gz]
                [--offcpu  --offcpu-output  S/offcpu.pb.gz]
                [--gpu --gpu-shim SHIM --gpu-output S/gpu.pb.gz --gpu-executions-output S/gpu.execlog]
                [--kernel-stacks]
                --tag perf_agent_mcp_capture=<id>
```

- `S` is the private staging directory (§7).
- `--no-debuginfod` is always passed. The scrubbed environment (§5.3)
  already removes `DEBUGINFOD_URLS`, and this is a second guard.
- **Never emitted:** `-a`/`--all`, `--pmu*`, `--per-pid`,
  `--perf-data-output`, `--flamegraph-output`, `--metrics-listen`,
  `--debuginfod`, `--debuginfod-url`, `--debuginfod-prefetch`,
  `--symbol-cache-*`, `--gpu-telemetry*`, `--continuous`,
  `--gpu-ring-bytes`, `--gpu-stack-memo` and `--gpu-per-execution-samples`.
- A table test enumerates every argument combination and checks that the
  argv's flags are a subset of the fixed list and that each argv contains
  `--no-debuginfod` and `--duration`.
- `--duration` is the backstop for orphans (§6.6): the child stops itself
  after D even if the server is gone.

**Contract test (a new file, no change to perf-agent):**
`capture_contract_test.go` in perf-agent's root `package main`. For every
flag the builder can emit, it checks that `flag.Lookup(name)` is non-nil.
It also reads `main.go` and `telemetry.go` and checks that the readiness
format (§5.3) is still a literal there. When perf-agent's CLI drifts, CI
fails in the repository that owns both binaries.

### 5.3 Process setup, readiness and timeouts **[mine]**

- **Exec:** done by the reaper, not the server (CAP-R2b, §6.6), through
  `os/exec`, after it re-verifies the binary's identity:
  - `Cmd.Path` is the verified path, with `Args` from §5.2;
  - `Dir` is the staging directory, so any stray relative write lands
    where it is discarded;
  - `Stdin` is `/dev/null`;
  - `SysProcAttr{Setpgid: true}`: its own process group, which is signalled
    as a group.
- **Environment:** the whole environment is exactly `PATH=/usr/local/bin:/usr/bin:/bin`
  and `LC_ALL=C`. Nothing else from the server's environment is passed:
  - `DEBUGINFOD_URLS`;
  - the Kubernetes label variables perf-agent reads
    (`internal/k8slabels`);
  - the `OTEL_*` variables;
  - `GODEBUG`.
- **Stdout and stderr:** each goes to its own pipe, read into a bounded
  ring (`Capture.MaxOutputTail` × 8, 16 KiB) and to the slog logger on the
  server's stderr at debug level. The child is never given the server's
  stdout. In stdio mode that stream is the JSON-RPC stream.
- **Readiness:** both of perf-agent's capture paths print `Collecting data
  for %v (Ctrl+C to stop early)...` on stdout after `agent.Start` returns
  (`main.go` legacy path, `telemetry.go`). The stdout reader watches for
  that prefix:
  - its arrival time is `attach_latency`, measured from exec;
  - `profiled_duration` starts there.

  There is no new `--ready-fd` flag: the user's no-new-flags rule holds,
  and the line exists today. The coupling is pinned by the contract test
  (§5.2).
- **Bounds:**
  - `Capture.StartTimeout`: exec → readiness. Default 15 s, which covers
    BPF load and unwind tables for a large binary.
  - `Capture.FinishTimeout`: stop → exit, for symbolization and writing.
    Default 30 s.
  - A capture at the maximum 30 s duration typically returns in about
    35–40 s: 1–3 s to attach, plus the duration, plus a few seconds of
    symbolization and the open.
  - The hard bound is `StartTimeout + duration + FinishTimeout`, 75 s at
    the defaults. That is well inside Claude Code's limits (§9.1).
  - Missing either bound sends SIGKILL to the group and fails the call
    with the bound named.
- **Early stop:**
  - when the target exits (`target_exit`), or a launched command exits
    (`command_exit`);
  - the server sends SIGINT to the perf-agent group;
  - perf-agent's own handler then stops and writes the profile (legacy
    path: `agent.Stop` after the signal).

  For a PID target, a pidfd-backed `os.FindProcess` handle (Go ≥ 1.23 on
  Linux) is polled with `Signal(0)` every 250 ms.
- **Cancel** (§9): SIGINT to the group, then SIGKILL after
  `Capture.StopGrace` (5 s). The outputs are discarded. This uses
  `exec.Cmd.Cancel` and `WaitDelay`.
- **A child that does not die:** a CUDA process wedged in D state may
  survive SIGKILL (the GPU hang memory note). `Wait` is bounded by
  `StopGrace` after SIGKILL. The process is then left to a background
  reaper, and the call fails with "pid N did not exit after SIGKILL; it was
  left running". Nothing is retried. Process groups are signalled only with
  `syscall.Kill(-pgid, sig)` on the recorded pgid, never by name: no
  `pkill -f`, per the memory note.

### 5.4 Exit codes and stderr

| perf-agent outcome | `capture` says |
|---|---|
| 0, all expected files present | success |
| 0, a file missing or empty | "perf-agent exited 0 but wrote no <mode> profile", with the stderr tail |
| 2 (flag parse, usage) | "perf-agent refused its arguments; it may be older than perf-agent-mcp expects (found <version>)", with the stderr tail |
| 1 (`log.Fatal`: capabilities, target gone, attach failure) | "the capture failed", with the stderr tail and advice for known stems (`set capabilities` → the setcap line; `cap_checkpoint_restore` → the same) |
| 126/127 (loader) | "perf-agent could not start: <tail>"; for `error while loading shared libraries … blazesym`, link blazesym statically |
| killed by a bound | the bound's name and value |

## 6. Guardrails

### 6.1 Bounds **[mine]**

These go in a new `Limits.Capture` struct, so they are settable through
`--limit Capture.Name=V`; `setLimit` already handles durations.

| Limit | Default | Ceiling (compiled; `--limit` cannot exceed it) | Reason |
|---|---|---|---|
| `Capture.MaxDuration` | 30 s | 5 min | 30 s is Go's `/debug/pprof/profile` default. The call blocks, and clients time out tool calls |
| default `duration` | 10 s | — | perf-agent's own default |
| `Capture.MaxSampleRate` | 1000 Hz | 10 kHz | Perturbation and sample volume; the default rate stays 99 |
| `Capture.StartTimeout` | 15 s | 5 min | §5.3 |
| `Capture.FinishTimeout` | 30 s | 10 min | §5.3 |
| `Capture.TTL` (`--capture-ttl`) | 7 days | — | Retention (§7.3) [user] |
| `Capture.StopGrace` | 5 s | 30 s | §5.3 |
| `Capture.MaxProfileBytes` | `Model.MaxRawBytes` (256 MiB) | — | A larger file could not be opened anyway |
| `Capture.MaxExeclogBytes` | `Model.MaxSidecarBytes` | — | Same reason |
| `Capture.MaxStoredBytes` | 4 GiB | — | The total size of the capture directory, measured before each capture (it includes earlier runs). Retention (§7.3) makes room first; `disk` only if that is not enough |
| `Capture.KeepPerSeries` | 10 | — | Unkept captures retained per series, newest first (§7.3) |
| `Capture.MaxKeptPerSeries` | 4 | — | `keep: true` captures per series; one more is refused (`invalid_argument`), so `keep` cannot defeat retention |
| `Capture.CompareRows` | 20 | `MaxDiffRows` | Diff rows in a `compare_with` result |
| `Capture.MaxJobs` | 32 | — | Terminal jobs kept in memory, LRU (CAP-R11, CAP-R16) |
| `Capture.MaxJobBytes` | 4 MiB | — | Bytes of terminal results kept in memory (CAP-R11, CAP-R16) |
| lookup pool | 2 workers, a queue of 16 | — | Historical result reads (CAP-R14); fixed, not a `--limit` |
| `Capture.MinFreeBytes` | 1 GiB | — | `Statfs` of the capture directory before starting |
| `Capture.WindowCaptures` | 60 per rolling hour | — | A runaway loop. A rolling window suits a long-lived server (§9); in stdio it is per process, so a loop that restarts the server resets it, and `MaxStoredBytes` is what bounds it across restarts |
| `Capture.WindowDuration` | 30 min of requested duration per rolling hour | — | Same reason |
| `Capture.MaxOutputTail` | 2 KiB | 64 KiB | The response budget |

**One at a time [user], across processes (CAP-R6).** Several stdio
servers (one per agent session) can share one `--captures` directory, so
the slot is a cross-process lock:
- an exclusive non-blocking `flock` on `<captures>/.lock`. The server
  takes it in the prepare phase and hands the locked descriptor to the
  reaper, which holds it until both children are reaped (CAP-R6b, §6.6);
- the holder writes `{server pid, start_time, reaper pid, start_time,
  capture id, series, deadline}` into the file;
- a second capture, from this process or any other, is refused at once
  with `busy`, naming the holder ("capture parse-opt/004 by perf-agent-mcp
  pid 5123, ends by 10:15:42Z") and `retry_after`.

It is not queued: a queued capture would start at a moment nobody chose.
The lock is per capture, not per server lifetime, so a second session can
capture once the first is done. The kernel releases the lock only when
both the server and the reaper have closed it, so an orphaned capture
keeps the directory locked until it is reaped. Recovery (§7.4) then
cleans up. `flock` is not a raw system call and the network scan allows
it.

**Output size while writing:** the staging directory is polled every
250 ms, and a file over its limit gets SIGKILL to the group. The overshoot
is bounded by about 250 ms of write rate. `RLIMIT_FSIZE` would need
`prlimit`, which needs a raw syscall the network scan forbids.

### 6.2 Destination **[controller ruling]**

Captures go to `<profiles>/captures` by default. The server creates the
directory, mode 0700, at the first capture. `--captures DIR` overrides
it, and must also be inside the profiles root. It is checked through the
profiles `os.Root`, and the server keeps a sub-root
(`profiles.OpenRoot(rel)`) for it. The existing `store.open` reads
captures with no new root kind, and captures stay out of the user's own
files. With `--perf-agent` given, capture is enabled. Without it, capture
is disabled.

**Writer ownership (SRV-R5).**
- The stdio server and the serve process do not share a capture
  directory:
  - stdio defaults to `<profiles>/captures`;
  - serve defaults to `<profiles>/captures-serve`.
- On first use the server writes `<captures>/.owner` (no-replace) with
  `{"kind": "stdio" | "serve", "format": 1}`.
- A server of the other kind refuses the directory at start-up, and
  every `capture` call returns `owner_mismatch`, naming the directory and
  its kind.
- Several stdio servers sharing one directory remain normal; the captures
  lock (§6.1) serialises them.

### 6.3 Which PIDs **[user: "any ordinary process of yours"; CAP-R1]**

The setcap'd binary has `CAP_SYS_PTRACE`, so it could profile anything,
including root's processes. "Can profile" is therefore defined as the
kernel's own `ptrace_may_access` credential test, minus Yama. The user
accepted profiling non-descendants. The **preflight**:

- `Uid:` in `/proc/<pid>/status`: real, effective, saved and fs are all
  the server's uid;
- `Gid:`: real, effective, saved and fs are all the server's gid. This
  refuses setgid programs such as `ssh-agent` where it is setgid;
- **dumpable** (corrected after review #399). `readlink("/proc/<pid>/exe")`
  must succeed. The kernel gates it on `ptrace_may_access(PTRACE_MODE_READ_FSCREDS)`,
  so it fails with `EACCES` for a non-dumpable task such as `gpg-agent` or
  a setcap'd process, and for any task the server could not ptrace-read.
  The owner of `/proc/<pid>/status` (root for a non-dumpable task) is
  checked too, and must be the server's uid. The ownership of the
  `/proc/<pid>` directory is **not** used: it is not the dumpable
  indicator;
- `CapPrm:` is zero, so a capability-holding process is refused (this is
  ptrace's cap-subset rule against a server with no capabilities);
- not a kernel thread (the exe readlink above fails for one); not pid 1;
  not the server's own pid; not one of its perf-agent children or reapers.

PID reuse is handled by the start time:
- `/proc/<pid>/stat` field 22 is recorded with the preflight;
- it is re-read at readiness and at the end;
- a change at readiness aborts and discards the capture;
- a change at the end discards it ("pid N was reused during the capture").

**What the rule is, and what remains exposed (CAP-R1, CAP-R1b).** The
target rule is a **defence against the agent, not an enforced
boundary**. The user accepted a broad target scope ("any ordinary process
of yours").

- **What `no_new_privs` gives launched commands, and what it does not.**
  - It is set before the exec (§6.4), so no `execve` in the workload or
    its descendants gains privilege. setuid, setgid and file capabilities
    are ignored, so the uids, gids and capabilities cannot change.
  - It does **not** freeze dumpability. A probe confirmed it: with
    `NoNewPrivs: 1`, `prctl(PR_SET_DUMPABLE, 0)` still succeeds.
  - So a launched command can still turn non-dumpable, and it gets the
    same recheck-and-discard as a `--pid` target.
- **`--pid` on an existing process.**
  - Between the preflight and perf-agent's attach, and during the
    capture, the target may exec a setuid or capability program, or turn
    non-dumpable. perf-agent, which follows the PID, keeps profiling it.
  - PID reuse between the preflight and the attach is detected only
    afterwards, by the start time.
- **The recheck,** for both kinds of target. The preflight (credentials,
  dumpable, `CapPrm`, start time) is repeated:
  - at readiness;
  - every 1 s while profiling;
  - at the end.

  Any change discards the capture: no file is published, the manifest
  says `target_changed` with what changed, and the error says so.
- **The residual exposure,** stated as it is. A transition can be
  profiled for up to `MaxDuration` (30 s by default, configurable to the
  5 min ceiling), plus perf-agent's start-up and finalisation (up to
  `StartTimeout` and `FinishTimeout`). This applies when the transition
  is not caught, or before it is. Two limits apply:
  - **periodic checks can miss a transition that reverts between two
    checks**, for example dumpable 0 then 1 within a second;
  - perf-agent may already have read the target's maps or memory in that
    window.

  It grants nothing beyond what the setcap'd perf-agent already gives the
  user run by hand, against any PID.
- **Other limits of the checks, as they stand.**
  - A process whose leader thread called `pthread_exit` while its other
    threads run shows `Z` in `/proc/<pid>/stat`, so the capture ends there
    as `target_exit`.
  - After a kill, `Wait` on a reaper that never answers returns once the
    bound (`StopGrace + FinishTimeout + 5 s`) has passed, plus the 0.5 s
    the output readers get to drain.

Under the server's PID namespace the PID is as the server sees it.
perf-agent translates it to the host PID itself (`resolveTarget`).

### 6.4 Commands **[user: allowed by default; the hardening is mine]**

- **On by default [user].** The agent may launch any command and profile
  it; that is the point of the autonomous loop. The command is given as
  argv, with no shell and a scrubbed environment.
- **Optional hardening:** `--capture-command PATH` (repeatable; absolute;
  resolved once at start-up; regular, executable, not setuid or setgid,
  no file capabilities). When it is given, `command[0]` must equal one of
  those paths. The docs warn that listing an interpreter (`python3`,
  `node`, `bash`) still allows arbitrary code through its arguments.
- **Resolving `command[0]`** when there is no allow-list:
  - an absolute path is used as is;
  - a bare name is looked up once in the scrubbed `PATH` below. That is
    exactly the `PATH` the command will see, so the agent's `"python3"`
    means what it would in a shell;
  - the resolved path is reported in the result and the manifest.

  It is refused when the file is setuid or setgid, or has file
  capabilities. This gives the agent a clear early error; `no_new_privs`
  (below) is the actual boundary.
- **Launch (CAP-R1, CAP-R3):** the reaper (§6.6) starts the workload
  through a second internal mode of the same binary, the launcher:
  - argv only, with no shell; `Dir` is `cwd`, which must be inside a
    `--source` root or the profiles root [controller];
  - `Setsid`: its own session and process group, and **no controlling
    terminal**, so a tty-scoped `sudo` timestamp cannot be reused and
    `sudo` cannot prompt;
  - the launcher locks its OS thread, calls
    `prctl(PR_SET_NO_NEW_PRIVS, 1)`, then `syscall.Exec`s `command[0]`.
    `no_new_privs` is a per-thread attribute inherited across `execve` and
    `fork`, which is why the thread is locked. From then on, no `execve`
    in the workload or its descendants can gain privilege: setuid, setgid
    and file capabilities are ignored;
  - the server and the reaper never set `no_new_privs` on themselves. That
    would strip perf-agent's file capabilities at its exec;
  - stdin is `/dev/null`, and stdout and stderr go back to the server
    through pipes into bounded rings.
- **Containment [controller: yes when allowed]:**
  - When the server's own cgroup (from `/proc/self/cgroup`) is in a
    delegated, user-writable cgroup v2 subtree, as under a systemd user
    session, the launcher is started in a fresh child cgroup
    `perf-agent-mcp-<id>` (`SysProcAttr.UseCgroupFD`). The end is
    `cgroup.kill`.
  - Otherwise the subreaper sweep applies (§6.6).
  - The cgroup is a write outside the roots, to the user's own cgroupfs.
    It is removed after the kill, and listed in the docs.
- **Environment:**
  - from the server's environment, only `PATH`, `HOME`, `USER`, `LOGNAME`,
    `LANG`, `LC_*`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR` and
    `CUDA_VISIBLE_DEVICES`;
  - plus `CUDA_INJECTION64_PATH=<shim>` for `gpu`;
  - the agent sets nothing: there is no `env` argument. A benchmark
    needing variables gets a wrapper script in the repository. That is
    reproducible, and the manifest records its path.
- **Order:**
  1. the command starts;
  2. its PID passes the §6.3 preflight (by construction, under
     `no_new_privs`);
  3. perf-agent starts with `--pid`.

  Before step 2, `/proc/<pid>/exe` must no longer be the launcher. That
  proves the exec happened, not that the program is the one resolved at
  the start: a command swapped in after resolution is accepted with the
  note "executed a program other than the resolved command".

  There is no start gate: a gate would have to `exec` the target after
  perf-agent attaches. perf-agent's interpreter enrolment would then see
  the gate process instead of the workload (`unwind/interp` enrols at
  attach, with bounded retries), and a gate needs either a new perf-agent
  launch mode or ptrace. The result instead reports `attach_latency` and
  the note that the first part of the run is missing.
- **End:** the capture ends at `duration` or when the command exits,
  whichever is first.
  - A command still running is then ended: `cgroup.kill` when it has a
    cgroup; otherwise SIGTERM to its group, SIGKILL after `StopGrace`,
    and the subreaper sweep.
  - Processes still alive after `StopGrace` are reported as `survivors`
    (CAP-R3b). There is no absolute claim.
  - Only the launched PID is profiled, not processes it forks, and the
    result notes this.

**What bounds a default-on launcher:**
- one capture at a time, so one launched command at a time;
- wall time of at most `StartTimeout + duration + StopGrace`;
- the user's uid with no capabilities, no tty and no agent environment;
- bounded output rings, never returned beyond `MaxOutputTail`;
- `no_new_privs`: no privilege gain by any exec;
- a kill on timeout, cancel, EOF, shutdown and server death (the reaper);
- rolling-window limits.

It is still arbitrary code execution as the user for that window, which
the user accepted (§12).

### 6.5 Rate and perturbation

The window limits (§6.1) plus one capture at a time bound the total
overhead an agent can impose. A capture of the same PID within 1 s of the
previous one ending is allowed. The window limit is the bound, not a
per-target cooldown.

### 6.6 Children and their bounds (CAP-R2, CAP-R3)

**Correction after review #399.** The earlier text said a perf-agent
orphan was "bounded by `--duration`". That is wrong for the unchanged
`main.go`:
- `agent.Start` runs **before** the duration timer starts;
- `agent.Stop` and `Close` have no deadline.

After a server SIGKILL, a perf-agent stuck in start-up (BPF load,
interpreter enrolment) or in finalisation (symbolization) can run
indefinitely. The kernel also clears the parent-death signal on exec of a
file-capability binary, so `Pdeathsig` cannot cover it. For the launched
command, `Pdeathsig` is tied to the creating *thread*, and is not
inherited by its descendants. It is not relied on.

**The reaper (CAP-R2, CAP-R2b, CAP-R6b).** Each capture has a detached
reaper.

- **What it is:** the same `perf-agent-mcp` binary (`/proc/self/exe`,
  resolved at start-up), re-exec'd in an internal reaper mode. It reads
  its spec from fd 3 as JSON, never from argv. It runs in its own session
  (`Setsid`), so signals to the server's process group do not reach it,
  and it has no `Pdeathsig`, since it must outlive a dead server.
- **It creates both children itself (CAP-R2b).** The server never spawns
  perf-agent or the workload. It passes the reaper:
  - the spec (the verified perf-agent's path and identity: dev, inode,
    xattr digest; the argv; the command; cwd; the cgroup fd);
  - the write ends of the children's stdout and stderr pipes;
  - the control pipe.

  The reaper:
  1. re-verifies the perf-agent identity;
  2. starts the launcher (`Setsid`, cgroup) and perf-agent (`Setpgid`);
  3. reports both PIDs, pgids and start times back on the control pipe.

  Because the reaper is their parent, it holds each child's identity from
  the moment `fork` returns, before the server hears of it. The old gap
  is gone: the server spawned, then registered the pgid with the reaper,
  and a server death in between left an unregistered perf-agent stuck in
  start-up.
- **It kills when:**
  - **EOF** arrives on the control pipe: the server died, whatever the
    cause, SIGKILL included;
  - **the absolute deadline** passes: `StartTimeout + duration +
    FinishTimeout + StopGrace` after the start, even if the server is
    alive and hung;
  - **`cancel`** arrives from the server.

  It sends SIGKILL to the perf-agent group, `cgroup.kill` or SIGKILL to
  the workload, and runs the subreaper sweep. It waits up to `StopGrace`.
  On a normal end the server sends `done` and nothing is killed.
- **The captures lock is the reaper's too (CAP-R6b).** `flock(2)` locks
  belong to the open file description. The server passes its locked
  `<captures>/.lock` fd to the reaper, which keeps it, without
  `CLOEXEC`-leaking it to the children (Go marks every fd but `ExtraFiles`
  close-on-exec). The reaper closes it **only after both children are
  reaped** and the cleanup is done. So:
  - a server that dies while the reaper and the children live leaves the
    directory locked;
  - a second server gets `busy`, naming the holder: the lock file holds
    the server PID and start time, the reaper PID and start time, and the
    capture id;
  - recovery (§7.4) runs only with the lock taken, and judges an intent
    abandoned only when both the recorded server and the recorded reaper
    (PID **and** start time) are gone.
- **Documented limit:** a process that kills the reaper as well as the
  server (for example `kill -9` of both by PID) leaves the children
  unbounded. The lock is then freed, but it was held for the children's
  sake.

**Descendants (CAP-R3, CAP-R3b).** There is no absolute containment
claim:
1. **cgroup v2, when the user's tree is delegated and writable** (the
   controller's ruling: yes, when allowed). The workload starts in
   `perf-agent-mcp-<id>`, and the end is `cgroup.kill`.
2. **Otherwise,** the reaper sets `PR_SET_CHILD_SUBREAPER` before
   creating the children. Orphaned descendants, double-forked and
   `setsid` ones included, reparent to the reaper, which kills them by
   PID (from `/proc/*/stat` field 4 and its own `wait4` results) until
   none is left or `StopGrace` passes.
3. **When neither works** (no delegated cgroup, and `prctl` refused),
   only the command's own process group is signalled.

Neither 1 nor 2 reaches processes started through another daemon (for
example `systemd-run --user` or a D-Bus activation).

**When `StopGrace` expires with processes left,** the result and the
manifest report them:
- `survivors: [{pid, comm, start_time, state}]`;
- `cleanup: "incomplete"`, with the reason: still in the cgroup, not
  reaped, D-state, or `kill: EPERM`.

The error code is `stuck_child`. Nothing is retried, and the docs tell
the user to look.

| Event | perf-agent child | Launched command and descendants |
|---|---|---|
| Normal end | exits by itself; the server tells the reaper `done` | cgroup kill, or SIGTERM then SIGKILL to the group plus the subreaper sweep |
| `capture_cancel` | the reaper: SIGINT, then SIGKILL after `StopGrace`; live rollback (§7.4) | as normal end |
| EOF on stdin, or the output backlog exhausted (standalone stdio: the client is gone, CAP-R7b) | as cancel | as normal end |
| Session disconnect in serve | nothing: the capture finishes and is listed (§9) | runs to the capture's end |
| Server shutdown (SIGINT/SIGTERM, `serve` stop, the forced-exit path) | SIGKILL to the group, synchronously; the reaper's pipe closes too | the reaper kills on EOF |
| Server SIGKILLed or crashed | the reaper kills on EOF | the reaper kills on EOF |
| Server alive but hung | the reaper kills at the absolute deadline | the same |
| Server **and** reaper SIGKILLed | unbounded (documented); the lock is freed | unbounded beyond the cgroup or session |
| Anything left after `StopGrace` | reported in `survivors`, with `cleanup: incomplete` (CAP-R3b) | the same |

The shutdown kill is a single `kill(2)` per recorded pgid and never
blocks. The forced-exit path in `cmd/perf-agent-mcp/main.go` calls it
before returning (§11).

## 7. Writing the files **[mine]**

### 7.1 Layout and names

```
<captures>/
  parse-opt/                       one directory per series, created by the server
    001-base-cpu.pb.gz
    001-base.capture.json          the manifest (§7.2)
    002-cand1-cpu.pb.gz
    002-cand1.capture.json
    003-cand2.capture.json         a failed capture: manifest only
    004-cand3-gpu.pb.gz
    004-cand3-gpu.execlog
    004-cand3.capture.json
  adhoc/
```

- The id is the series' next three-digit (or wider) number. It is taken
  from the persisted `.seq` under the cross-process captures lock (CAP-R5,
  CAP-R6), so two servers never assign the same id.
- `ls` order is capture order, and `base` and `cand` read at a glance.
- Retention never reuses an id: `.seq` is written and fsynced before an
  id is used (§7.4).

### 7.2 The manifest

`NNN-label.capture.json` is the commit record of the intent/commit
protocol (§7.4). It is written last, through the same no-replace
temporary-and-link path. It holds:
- `format: 1`, the id, label and series, `outcome`, and the error code on
  failure;
- the settings and `settings_digest`, a SHA-256 of the canonical settings
  JSON;
- the target: PID, `comm`, exe basename, exe build-id, `launched`;
  `command[0]` and the argument count, never the arguments;
- the perf-agent path, version, revision and capabilities;
- start time, `attach_latency`, `profiled_duration`, `stopped_by`;
- `compare_with`;
- each file's name, size, SHA-256 and inode;
- `kept`;
- `written_by: "perf-agent-mcp"`.

It is how `compare_with` finds and checks a baseline, how
`capture_list` works across restarts (§4.6), and how retention recognises
its own files (§7.3).

### 7.3 Retention **[user]**

Retention bounds disk use across many iterations. It runs at start-up and
before each capture, and only inside the capture directory.

- **What it may delete:** a capture's files only when its manifest says
  `written_by: perf-agent-mcp`, and each file's inode, size and SHA-256
  still match the manifest. A file the user replaced, or anything without
  a manifest, is never touched.
- **What it never deletes:**
  - `kept` captures (`keep: true` is the pin; at most
    `MaxKeptPerSeries`), whatever their age;
  - the baseline named by the newest capture of the series, whatever its
    age;
  - **the baseline of the call in progress:** `compare_with` is resolved,
    validated and pinned before any eviction (CAP-R4, §7.4 step 3);
  - any capture named by a live intent;
  - a capture whose handle is open in this server, or that a live
    publication pins. Another process's open handles are not visible.
    Deleting a file they opened is harmless to them, since a handle holds
    its loaded profile in memory. Their `compare_with` baselines are
    protected by the newest-capture rule above.
- **Order:**
  1. every unprotected capture older than `--capture-ttl` (default 7
     days, from the manifest's start time; `0` disables the TTL);
  2. per series, unkept captures beyond `KeepPerSeries`, oldest first;
  3. then, while `MaxStoredBytes` would be exceeded, the oldest unkept
     capture in any series, least recently written series first.
- **How:** `root.Remove` of each file, then the manifest last, so a crash
  part-way leaves a manifest that a later pass finishes. Every eviction is
  logged to stderr and listed in the result's `evicted`.
- **No other deletion:** there is no delete tool, and nothing outside the
  capture directory is ever removed. This is the one place where the
  server deletes files, so the docs state it next to "never replaces a
  file".

### 7.4 Steps: an intent/commit protocol (CAP-R5, CAP-R5b; order fixed after #413)

**Why it changed.**
- **Review #399:** publish, then open, then manifest left undiscoverable
  and undeletable files after a crash or `ENOSPC`, and `.seq` could
  collide.
- **Review #404** found three more gaps:
  - a crash after linking left an `nlink=2` staging+final pair, which
    recovery's `nlink == 1` rule refused;
  - name-only ownership could delete a user's file after a lost `Link`
    race;
  - a **live** server's failure after the intent had no rollback, and
    recovery ignored live-owned intents for ever.
- **Review #413:** §4.0 and this section gave two different orders for
  retention, `.seq` and the intent. There is now one order, this one, and
  §4.0 refers to it.

Every step runs in the server-owned job goroutine (CAP-R9, §4.0),
through the capture sub-root. From step 1 on, the job holds the dir lease
(the captures lock, §6.1, CAP-R6b). "fsync" means the file, then its
directory. The job's states map onto the steps:
- steps 1–7 are `preparing`;
- the capture itself is `running`;
- steps 8–11 are `committing`, and step 11's manifest link is the commit
  point;
- `open`, the diff and publication are post-commit derivations, outside
  this protocol. They never roll back.

**`preparing`:**
1. **Lease.** Take the non-blocking captures lock before any filesystem
   side effect. A `busy` here fails the job.
2. **Recovery** (below), under the lease and without the engine.
3. **Engine section (CAP-R13).** Holding the engine:
   - resolve and validate `compare_with`: its manifest must exist, be
     `ok`, and have files matching their digests;
   - **pin** the baseline (CAP-R4);
   - snapshot the protected inputs (open handles, publication pins);
   - run **retention** (§7.3) against the pins and protected inputs.

   This section does file I/O (digest checks, manifest reads, removals)
   while holding the engine, by design: retention must not race the
   store's view of what is open. Then the engine is released.
4. **Id.** Read `<series>/.seq` (a missing file means "highest manifest
   or intent id + 1"), write `id + 1` through temp + fsync + rename +
   directory fsync, **before** the id is used. A crash after this step
   wastes an id and never reuses one.
5. **Intent.** Write `NNN-label.intent.json` (no-replace, fsync) with:
   - `state: "prepared"`;
   - the id, series, label and settings;
   - the final names it may create and the staging directory's name;
   - an **owner token** (128 random bits);
   - the owner identity: server PID and start time; the reaper's, once
     known.

   Then it creates `NNN-label.intent.hb`, which holds the owner token, and
   records its identity with an intent rewrite (CAP-R45: the intent
   first, so a crash between the two leaves an intent judged by its
   owner's liveness, never a heartbeat no intent names). The owner
   refreshes the heartbeat's mtime every 2 s while the operation is in
   flight. That is the **heartbeat**.
6. **Space.** Check `MaxStoredBytes` and `MinFreeBytes` (retention has
   already run). A failure goes to live rollback.
7. **Staging, preflight, reaper.** Create `.staging-<16 hex>`
   (`Mkdir(0o700)`) and record its dev and inode in the intent. Run the
   target preflight (§6.3) and start the reaper (§6.6). The job enters
   `running`.

**`committing`:**

8. **Validate** each expected file in staging, through the root with
   `O_NOFOLLOW`:
   - regular, `nlink == 1`, owner is the server's uid, size within limits;
   - then `Chmod 0o600`, fsync, and compute its SHA-256.
9. **Record the identities before linking.** Rewrite the intent (temp +
   fsync + rename; it is the owner's own file) with `state: "linking"`
   and, per file, `{final name, dev, inode, size, sha256}`.
10. **Publish** each file with `s.link(root, staging/f, final)`, the
    no-replace hard link (M2-R4), then fsync the series directory.
    - If a name is taken meanwhile, `Link` fails with `EEXIST` and that
      name is **not** ours: it is never removed.
    - The files this capture already linked are rolled back by identity
      (below).
11. **Commit.** Write the manifest's temp (`outcome: ok`, the
    identities from step 9) and fsync it. Then apply the **terminal
    decision rule** (below), and link the temp no-replace to
    `NNN-label.capture.json` and fsync the directory. **The successful
    link is the commit point.** A link that answers an error (`EEXIST`
    included, as a retransmitted NFS request answers for its own link)
    while the manifest name now is the temp's dev and inode did succeed
    (CAP-R46); a name taken by another file is `exists`. Then remove the
    staging directory (which drops each inode to `nlink 1`), the
    heartbeat, then the intent (CAP-R45), and fsync. The reaper is told `done`, and releases the lease once its
    children are reaped (CAP-R6b).

**The terminal decision rule (cancel vs commit; CAP-R16, refined after
#413).** It is one atomic word per job, `decision ∈ {open,
cancel_requested, manifest_in_flight, committed}`:
- **The control handler** for `capture_cancel` (CAP-R10) does no file
  I/O. It does one CAS and answers from the word:
  - `open → cancel_requested`, and it answers `cancelling`;
  - an existing `cancel_requested` answers `cancelling`;
  - `manifest_in_flight`: it records the request in the job (a second
    flag) and answers `cancelling` (decision pending);
  - `committed` answers `too late: committed`.
- **The job,** at step 11 before the link, does
  `CAS(open → manifest_in_flight)`:
  - if that fails because the word is `cancel_requested`, it does not
    link, and rolls back to `cancelled`;
  - otherwise it links. A successful link stores `committed`, and a
    cancel recorded during the link is reported as `cancel: "too late:
    committed"`. A failed link is a failure, rolled back, and a pending
    cancel is reported as `cancelled`.

So **`too late: committed` is only ever said once the manifest link has
succeeded**, and a cancel never loses to a manifest that did not
commit. The one window where a cancel cannot win is a manifest link
already in flight. It is a single bounded `link(2)` + fsync, and the
answer there is `cancelling` until the link resolves.

**Identity-matched deletion (the only way this protocol deletes):**
- a name is removed only if `Lstat` shows a regular file whose dev and
  inode equal the recorded ones;
- and whose size and SHA-256 match, for every name, temporaries and the
  heartbeat (which holds the token) included: an inode number can be
  reused, and the server's own links, renames and heartbeat updates
  change a file's ctime, so the contents are the proof (CAP-R44);
- the name is first renamed to a private quarantine name beside it
  (`.del-<token[:16]>-<16 hex>` for a capture's own removals), checked
  again there, and only then unlinked; a file that is not the recorded
  one is put back without replacing anything (CAP-R43). Recovery proves a
  quarantine name a crash left against what its intent records, or by
  the token its bytes hold; an intent quarantined after its manifest, and
  an intent or manifest temporary written but not yet recorded, are
  proven the same way;
- a staging directory is opened as a root and proven to be the recorded
  dev and inode before perf-agent's outputs in it are removed by name
  (CAP-R48);
- `nlink` may be 1 or 2. 2 is the staging+final pair after a crash in
  steps 10–11, and both names are then removed;
- a name whose identity differs, or that was never recorded (a crash
  before step 9), is **left in place and logged**. That covers a user's
  file that won a `Link` race, and anything else the server cannot prove
  it created.

**Live rollback** is done by the owner, on any failure after step 5: an
`ENOSPC`, a size kill, `target_changed`, a cancel, a publish or manifest
write error.
1. The owner sets `state: "rolling_back"` in the intent (best effort,
   under `ENOSPC`).
2. It removes the linked finals and the staging files by identity.
3. It writes a manifest with the failure code and no files. If the
   manifest cannot be written, for example under `ENOSPC`, the intent
   stays at `rolling_back`, the heartbeat continues, and the owner retries
   the rollback before its next `capture` and every 30 s.
4. It removes the intent and the heartbeat.

`capture_wait` reports the failure only after step 2, so the agent never
sees an error while capture files are still published.

**Recovery** (step 2, and at start-up), under the lease. For each intent:
- **A manifest exists:** the capture was committed. Remove the leftovers
  by identity (staging names), then the intent.
- **The owner is alive:** its heartbeat is newer than 10 s (one dated
  more than 10 s ahead, after a clock jump, is stale), and the
  recorded server or reaper PID and start time is alive. The intent is
  left to its owner, which is still rolling back.
- **Abandoned:** heartbeat stale, server and reaper (PID and start time)
  both gone, and the lease held. Recovery removes by identity what step 9
  recorded. It commits an `interrupted` manifest with no files, then
  removes the intent. An intent still `prepared` (a crash before step 9)
  has no file identities, so recovery removes only the staging directory
  it created (by its recorded dev and inode) and leaves any final name
  alone.
- **The narrow orphan (CAP-R12).** Step 7 creates `.staging-<hex>`,
  then records its dev and inode in the intent. A crash between the two
  leaves a staging directory that no intent identifies. It is an
  **explicit, logged, retained orphan**:
  - it is never removed, because identity-matched deletion is not
    weakened for it;
  - it is created just before the children start, so it is empty;
  - it is mode 0700 and tiny;
  - recovery logs it as `orphan_staging`, and `capture_list`'s series
    summary counts it, so the user can remove it.
- Any other `.staging-*` directory that no intent names, and whose dev
  and inode cannot be matched, is likewise left and logged.
- A token temporary whose dev, inode and contents equal the intent's own
  file (a crash between linking the intent and removing its temporary's
  name) is a second name of the intent, and is removed. So is an intent
  temporary or a heartbeat that no rewrite recorded yet but whose
  contents carry the intent's owner token (an intent with that token, or
  the token itself): those bytes are only ever written by this server.
- **Single-syscall windows (CAP-R47).** A crash inside one system call
  of a write, after a new name exists and before any record of it is
  durable, leaves a file nothing can prove: a `.seq` temporary, a
  heartbeat or a temporary created but not yet written, a quarantined
  `.del-*` name nothing records, a staging directory (CAP-R12
  above), or a temporary of `.owner` in the capture directory itself.
  Each is retained, never deleted without proof, and logged by recovery
  as `orphan_temp`, `orphan_staging` or `left`; `capture_list`'s series
  summary counts them, left heartbeats included.
- Recovery that hits `ENOSPC` stops and leaves the intent. Every step is
  idempotent.

**Boundary tests (fault injection).** A seam fails, or simulates a crash
(the process exits without cleanup), at each boundary:
- after the lease; after recovery; inside and after the engine section;
- after `.seq`; after the intent; after the space check; after staging;
- after each validation; after the identity record;
- between each pair of links; after the links;
- after the manifest temp; after the manifest link; after the staging
  removal;
- inside live rollback and inside recovery;
- plus `ENOSPC` at every write.

After each, recovery or the live owner leaves either a committed capture
with verified files, or a failure or `interrupted` manifest with no
capture files, and nothing else: a fault at a boundary leaves no orphan.
Only a crash inside a single system call can leave one, as listed above,
and `capture_list` shows it.

Two dedicated cases:
- **lost `Link` race:** a user file is created at a final name between
  steps 9 and 10. It survives, byte for byte, while the capture rolls
  back;
- **live-owner `ENOSPC`:** the manifest write fails, the intent is left,
  and the next `capture` completes the rollback.

## 8. GPU captures **[mine]**

- **Flag:** `--gpu-shim PATH` (absolute, resolved once; a regular file
  owned by root or the server's uid and not group or other writable, in
  directories that pass the §5.1 directory rule, CAP-R18/R18b and CAP-R19)
  names the CUPTI adapter. Without it, `modes: ["gpu"]` is
  refused with "start perf-agent-mcp with --gpu-shim PATH". The shim is
  loaded by the user's own process, not by the privileged child, so it
  needs no capability check.
- **PID target:** before starting, `/proc/<pid>/maps` (readable under
  §6.3) must show a mapping with the shim's `(dev, inode)`. perf-agent
  attaches by inode, so a copy at another path captures nothing.
  Otherwise the call is refused: "the target did not load the CUPTI
  adapter at start; restart it with `CUDA_INJECTION64_PATH=<shim>`, or
  capture it as a command". Injection into a running process is the
  dynamic-attach PoC, not this milestone.
- **Command target:** the server sets `CUDA_INJECTION64_PATH`.
- **Execlog:** always requested with `--gpu-executions-output`. It is
  opened as `open`'s `execlog`, so executions facts are available and
  their verdict (accepted, or why not) comes from the existing check. It
  is bounded by `MaxExeclogBytes` while written (§6.1).
- **Privileges:** the same file capabilities; the uprobe needs
  `cap_perfmon` and `cap_bpf` (`uprobe_multi` on ≥ 6.6; `perf_uprobe` is
  acceptable).
- **No GPU or no driver:** the server does not probe hardware. If
  `/dev/nvidiactl` is missing, the refusal or empty-result text says so as
  a hint. A capture that ran but produced no GPU samples returns the
  profile with the note "the GPU profile has no samples: no kernel was
  launched through the adapter during the capture". The quality fields
  already distinguish this from zero.
- **Hangs:** see §5.3. A wedged CUDA workload is reported and left alone,
  never retried. The spec's question 1 (unattended GPU loops) still gates
  M3. One `capture` is user-initiated by construction.

## 9. Headless stdio first; the long-lived server reuses it **[mine, following the user's architecture]**

**Standalone stdio is the primary mode [user].** Everything in §4–§8
works in plain stdio `perf-agent-mcp`, with no `--live` and no serve
process:
- the capture is a tool call on the sequential request loop;
- results are opened in that process's store;
- comparability, history and retention live on disk (§7), so a loop that
  starts a fresh stdio server per session loses nothing but the open
  handles. `compare_with` reopens the baseline from its files.

`internal/capture` holds the runner, the verifier and the target checks,
and knows no transport. `perfmcp`'s tool handler is a thin layer over it,
and the serve process registers the same handler.

**Cancellation and the stdio streams (CAP-R7, CAP-R7b):** `mcpstdio` is
changed (§11).

- **Input.** The reader goroutine never waits on anything:
  - it decodes each line's `method` and `id` itself;
  - `notifications/cancelled` and EOF go to a control channel. A cancel
    ends the named request's handler (with two-step calls this is a
    `capture_wait`, §4.0), and no response is sent. **EOF in stdio
    cancels every running capture**: the client is gone, and nobody would
    collect the result. **EOF means the client is gone (CAP-R34):** the
    running handler and the requests still queued run with a cancelled
    context and are answered in order (as cancelled, `isError`, where they
    honour it), and the server-owned capture jobs are cancelled with the
    server. A script that pipes requests in must keep stdin open until it
    has its answers;
  - **`capture_cancel` is a control message too (CAP-R10).** The reader
    recognises a `tools/call` whose `name` is `capture_cancel`. It sets
    the flag in the cancellation registry (a mutex-guarded map the reader
    may touch, separate from the engine) and enqueues the
    `{status: "cancelling"}` response on the output queue at once,
    ahead of any queued requests. JSON-RPC responses are matched by id,
    so this reordering is valid;
  - other requests queue, in order, up to 8. One over that bound gets an
    immediate overload error (`-32000`, "server busy: 8 requests queued;
    retry"), which goes onto the output queue below. It is not written
    by the reader.
- **Output (CAP-R7b).** Every outgoing message goes through one bounded
  asynchronous writer queue:
  - responses, progress notifications and overload errors alike;
  - bounded at 64 messages and 4 × `MaxResponseBytes`;
  - a single writer goroutine drains it to stdout, so lines never
    interleave;
  - **back-pressure (CAP-R35):** the dispatcher starts the next queued
    request only once the backlog is under half of both bounds (the
    reader stays live meanwhile), so a slow but live client is never
    mistaken for a dead one;
  - **notifications are lossy (CAP-R33):** a progress notification is
    dropped while the backlog is at half a bound or more, and that is
    never fatal.

  Neither the reader nor a handler ever waits on stdout.
- **When the client stops reading.** If a response finds the backlog
  already at a bound (one line of any size fits behind a smaller
  backlog), or no write ends for `StallTimeout` (30 s) while lines wait,
  the client has stopped reading stdout. The server then **treats the
  client as dead**:
  - it cancels every handler and every capture (§6.6);
  - it stops reading;
  - it exits exactly as on EOF, including the forced-exit path's
    synchronous kill, without waiting for the blocked write.

This lets an autonomous client stop a runaway capture at once
(`capture_cancel`). No capture outlives a stdio client that went away,
whether it closed stdin or stopped reading.

In the long-lived server, the decisions above hold. These are the
differences:

- **Concurrency.** M2b's single request goroutine no longer holds with
  several sessions over Streamable HTTP, and that redesign is the serve
  milestone's, not this one's. What `capture` needs from it:
  - a server-wide capture slot (`atomic.Pointer[runningCapture]`, CAS to
    claim it), in front of the cross-process captures lock (CAP-R6),
    which other stdio servers on the same directory also respect;
  - **the engine is held only at the two points of §4.0 (CAP-R13):**
    `preparing`'s shared inputs (baseline, pins, protected inputs,
    retention), and the post-commit derivations and pin release. The
    child wait and the file commit run outside it, under the dir lease.
    `capture`, `capture_wait`, `capture_cancel` and `capture_list` never
    take it, so other agents' tools and the page are never blocked behind
    a capture.
- **Ownership.** The server owns the runner, its children and the staging
  directory. Children are signalled by a server-level registry, not a
  session's context. `Close` of the server kills them (§6.6).
- **Cancellation.**
  - **Jobs are server-owned (SRV-R7).** Any session holding the server
    token may `capture_wait` or `capture_cancel` any job. A session's
    `DELETE`, or its eviction, never cancels a job.
  - `capture_cancel` goes through the control path (CAP-R10). It is
    classified at ingress before the `MaxCalls` admission and the engine,
    and only flags the job. `notifications/cancelled` on a `capture_wait`
    ends only that wait.
  - **A dropped connection is not a cancel.** Streamable HTTP says
    disconnection should not be read as cancellation. The capture
    finishes, its files are kept, it is opened and published, and it
    appears in `capture_list` with its handle.
  - Server shutdown kills everything.
  - **Contrast with stdio:** there, EOF does cancel, since the process
    *is* the session and nobody would collect the result. In serve a
    dropped session leaves the result to `capture_list`.
- **Several agents.**
  - One capture at a time, server-wide; the second agent is refused with
    the running capture's end time.
  - Windows and stored-bytes budgets are server-wide.
  - Names include the capture id, so agents never collide.
  - `capture_list` shows every agent's captures, because they are all the
    user's.
- **The page.**
  - A finished capture becomes a facts-only publication: `page_explain`'s
    path with `explanations: []` (M2b D3), over the capture's handle,
    titled `parse-opt/002-cand1 · bench (41234) · cpu · 19.6s`. With
    `compare_with`, a `page_diff` publication of the pair is added too.
  - It is made current only if `show` is true (the default). Otherwise it
    is only listed.
  - The browser still cannot start, stop or alter a capture: M2b's
    anti-goal (no state change from the browser) holds.
  - A "capturing…" banner in the snapshot is deferred.
- **Who may call.** Any client with the server's token. That is any
  process with the user's uid that can read it, and such a process could
  already run the setcap'd `perf-agent` directly, against any process. The
  guardrails here bound the **agent**, not same-uid malware (§12).

**The source scan is drift protection (CAP-R39).** The scan in
`cmd/perf-agent-mcp/netscan_test.go` exists for code review: it fails the
build when a change adds a network client, a second listener, a raw system
call or a process start outside the reviewed call sites, so such an
addition is seen before it lands. It is not a defence against a committer
set on evading it. It reads every non-test Go file of the packages linked
for linux/amd64, linux/arm64 and darwin/arm64 (a file for another target
or a custom tag by the name rules only, and an import no target links is
a finding), and fails on cgo, go:linkname, assembly or object files for
any target and on go/build, go/importer or plugin. It
resolves names with go/types and holds the reviewed files to exact shapes:
`exec_linux.go` to `exec.Command(self)` in `selfCommand` and
`exec.Command(path)` in `startPerfAgent`, each argument the function's own
parameter, never reassigned; `git.go` to `exec.LookPath("git")` and
`exec.CommandContext(ctx, bin, argv...)` in `revParse`, `bin` the
LookPath result; `launch_linux.go` to one `syscall.Exec` and the two prctl
calls with `syscall.SYS_PRCTL` and package-level constants of the kernel's
values declared in that file. `selfCommand` is called only from
`startReaperProcess` and `startLauncher` with their own `self`, and
`startPerfAgent` only from the reaper's `attach` with `r.spec.PerfAgent`,
once each (CAP-R41); any other reference, a func value included, is a
finding. No literal of exec.Cmd's struct is allowed anywhere, whatever
the type is named, no Cmd's Path is ever set, and its Args, Env and Dir
are set only in those two files. It tracks no values beyond these rules:
what those callers are passed (the reaper's spec, and the callers of
`startReaperProcess` and `startLauncher`), a value returned or built at
run time, reflect and unsafe are not followed, and string-building evasions (concatenated literals) are
out of scope; the shell-name rule matches whole literals only.

### 9.1 Client timeouts and progress (CAP-R8)

**The tool timeouts of both clients:**

| Client | Per-call limit | Other limits |
|---|---|---|
| **Claude Code** (code.claude.com/docs/en/mcp, checked 2026-11-15) | `MCP_TOOL_TIMEOUT`, or a per-server `timeout` in ms; **unset, about 28 h**. Progress does not extend it | Idle abort: 30 min for stdio, 5 min for HTTP. HTTP has a first-byte timer of at least 60 s. `MCP_TIMEOUT` is start-up. Calls past 2 min move to the background |
| **Codex** (config reference, `mcp_servers.<id>.tool_timeout_sec`, checked 2026-11-15) | **60 s by default**, per server | `startup_timeout_sec`, 10 s by default |

**The design keeps every call far under the smallest of these, 60 s:**
- `capture` returns in microseconds, since it only enqueues (CAP-R9);
- `capture_wait` blocks for at most 45 s, which leaves 15 s of margin
  for the commit phase and the response;
- `capture_wait` reads only the registry or the disk, never the engine.
  It returns the current state (`committing`, or `done` with
  `derived: pending`) when its timeout ends first (CAP-R9).

No user configuration is needed. The docs still show how to raise Codex's
`tool_timeout_sec` for those who prefer longer waits; the cap stays 45 s.

**Progress (stdio only, optional).** When a stdio `capture_wait` carries
`_meta.progressToken`, the server may send `notifications/progress` every
5 s and at phase changes, with `progress` = elapsed and `total` = the
expected duration. They go through the async writer (§9). Correctness
never depends on them: Codex's 60 s is a wall-clock limit that progress
does not extend either.

**Serve stays JSON-only (SRV-R6).** Its Streamable HTTP transport answers
every call with a plain JSON response, with no POST-SSE and no progress
notifications. Under CAP-R8 no call exceeds about 45 s, so HTTP's 60 s
first-byte timer is never reached.

## 10. Testing without privileges

- **Fake perf-agent.** The test binary re-executes itself (`TestMain`
  checks `PERFMCP_FAKE_PERF_AGENT=1`). It parses its argv with a flag set
  that holds exactly perf-agent's flag names, and exits 2 on an unknown
  one, as `flag` does. Then it prints the readiness line, waits for the
  duration or SIGINT, and writes `pmfixture.CPUBasic()` (and
  `pmfixture.Execlog`) to the requested outputs.
- **Misbehaviour switches** (`PERFMCP_FAKE_MODE`):
  - `never-ready`, `ignore-sigint`, `exit-1-stderr`, `exit-2`;
  - `no-file`, `huge-file`, `symlink-output`, `hardlink-output`;
  - `spam-stdout` (MiBs on stdout and stderr);
  - `fork-grandchild`, `sleep-after-sigkill-like` (a stuck child,
    simulated by a grandchild in a new session).
- **The trust seam.** The xattr check cannot pass for the fake: an
  unprivileged test cannot set file capabilities. So `verifyBinary` is an
  unexported `Server` seam, as `link` and `evalSymlinks` are. The real
  verifier has its own tests:
  - `vfs_cap_data` byte fixtures: v2; v3 with `rootid` 0 and ≠ 0; no
    effective bit; missing required capabilities; extra capabilities;
    truncated and oversized data; a non-zero inheritable set;
  - the stat rules on temp files;
  - a nosuid refusal, with a `/tmp` file when `/tmp` is mounted nosuid
    (skipped otherwise);
  - buildinfo from a tiny built program (wrong module) and from the test
    binary itself.
- **Behaviour tests (`-race`):**
  - the argv table (§5.2) and the environment, which are exact;
  - readiness and the three timeouts;
  - early stop on target exit;
  - cancel, then SIGINT, then SIGKILL;
  - the size kill;
  - no clobber against a pre-existing name, and against a name created
    between the start and the link (the M2-R4 interposition pattern,
    through `beforePublish`);
  - staging cleanup on every path;
  - the target rule, against a child the test starts (accepted), pid 1
    (refused), the server itself (refused) and an exited and reused PID
    (start-time mismatch);
  - one at a time, with two concurrent calls in serve mode;
  - window limits;
  - the response cut at `MaxResponseBytes`.
- **Loop tests (`-race`, through the stdio harness, with the fake):**
  - a base and three candidates in one series: ids `001`–`004`,
    `compare` on each, `next` replayed against `diff` with identical rows;
  - `compare_with` across a **server restart**: a new server reopens the
    baseline from its manifest;
  - `incomparable` for each inherited setting;
  - `unknown_baseline`;
  - every error `code` in §4.4 produced at least once, each with valid
    JSON and the documented `retryable`.
- **Retention tests:**
  - `KeepPerSeries` and `MaxStoredBytes` evict oldest first;
  - `kept` captures, the current baseline, open handles and pinned
    publications survive;
  - a file the user replaced (digest mismatch), or one without a
    manifest, is never removed;
  - a crash between file removal and manifest removal is finished by the
    next pass;
  - ids are never reused after eviction.
- **Launch and progress:**
  - a bare `command[0]` resolved through the scrubbed `PATH`;
  - setuid, setgid and file-capability programs refused;
  - `--capture-command` enforced when it is given;
  - the child has no controlling tty (`/proc/<pid>/stat` `tty_nr` is 0);
  - with a delegated cgroup, a double-forked grandchild is killed by
    `cgroup.kill`; without one, the note is present;
  - progress notifications in phase order, with every stdout line still
    JSON-RPC;
  - the TTL removes an old unprotected capture and spares a kept one and
    the current baseline.
- **Review #399 tests (no privileges needed unless noted):**
  - **Credential and dumpability transitions (CAP-R1):** a test child
    passes the preflight, then after readiness calls
    `prctl(PR_SET_DUMPABLE, 0)`, or `setresgid` to a supplementary group.
    The end recheck discards the capture with the transition note. A
    launched command that `exec`s a setuid test binary (where one exists;
    skipped otherwise) runs it without privilege (`no_new_privs`, read
    from `/proc/<pid>/status` `NoNewPrivs: 1`).
  - **Exit and reuse (CAP-R1):** the target exits before attach
    (`target_exited`) and during the capture (`target_exit`). PID reuse is
    simulated through a seam that feeds a different start time at
    readiness and at the end, and each gives `pid_reused` with nothing
    published.
  - **The dumpable check:** a non-dumpable child (`PR_SET_DUMPABLE 0`) is
    refused through the `readlink exe` and status-owner rule, and the test
    asserts the `/proc/<pid>` directory owner is not consulted.
  - **The reaper (CAP-R2):** the server is SIGKILLed during each phase
    (start-up with a fake that never becomes ready, profiling, and
    finalisation with a fake that hangs after SIGINT). The fake's group is
    gone within `StopGrace` of the reaper's EOF. A hung, live server is
    covered by the absolute deadline. The reaper itself is in its own
    session.
  - **Descendants (CAP-R3):** a workload double-forks with `setsid` and
    sleeps. It is killed by `cgroup.kill` (delegated cgroup; skipped
    otherwise), and separately with the cgroup seam off, by the subreaper
    sweep. With both off, the narrowed note is present.
  - **Baseline pin (CAP-R4):** `compare_with` names the oldest capture
    while TTL and `KeepPerSeries` would evict it; it survives, and the
    diff runs.
  - **Cross-process lock (CAP-R6, CAP-R6b):**
    - two server processes on one `--captures` directory: the second gets
      `busy` naming the first's PID and capture;
    - the first is SIGKILLed while its reaper and children live: the
      lock stays held (by the reaper) and `busy` names the reaper;
    - after the reaper reaps and exits, the lock frees, recovery runs,
      and recovery judges the intent by the reaper's PID and start time;
    - a stdio server refuses a serve-owned directory (`owner_mismatch`,
      SRV-R5), and the reverse.
  - **Control messages under load (CAP-R7, CAP-R7b):** during a
    `capture_wait`, 20 requests then a `notifications/cancelled` arrive.
    The cancel takes effect, the overflow requests get the overload
    error, the queued ones are answered in order, and every stdout line is
    one JSON-RPC message.
    - **Full output queue + blocked stdout:** the test stops reading
      stdout until the writer queue is full. Then the server treats the
      client as dead: every capture's group is gone within `StopGrace`,
      and the process exits without blocking.
    - The same with a `notifications/cancelled` and with EOF sent while
      stdout is blocked: the reader still sees them.
  - **Credential transitions on launched commands (CAP-R1b):** a launched
    workload with `NoNewPrivs: 1` calls `prctl(PR_SET_DUMPABLE, 0)`
    mid-capture: `target_changed`, nothing published. A transition and
    revert within one check interval is not detected, and the test pins
    that as the documented limit.
  - **Spawn→registration gap (CAP-R2b):** a fault point in the reaper
    exactly between `fork` of perf-agent (a fake that hangs in start-up)
    and the report of its PID. The server is SIGKILLed there, and the
    reaper still kills the child on EOF. The same between the launcher's
    `fork` and its report.
  - **Survivors (CAP-R3b):** a descendant that ignores SIGTERM and is in
    D-state (simulated by a seam) outlives `StopGrace`. The result lists
    it in `survivors` with `cleanup: incomplete`, and the code is
    `stuck_child`.
  - **Job state machine (CAP-R9, CAP-R11, SRV-R7):**
    - seams block each of these: the digest hash, retention hashing, an
      fsync, the engine lock, `open`, `diff`. Each time, `capture` still
      returns `accepted` in microseconds, `capture_wait` returns the
      current state within its timeout, and the job finishes once
      unblocked;
    - a commit that outlives a wait: the first `capture_wait` returns
      `committing`, and the next returns `done`;
    - an `open` failure and a `diff` failure after the commit: `done` with
      `derived: failed` and the reason, and the files are still published
      with their manifest;
    - registry eviction (`MaxJobs` = 1) and a server restart: in both
      cases `capture_wait` by capture id answers terminally from the
      manifest with `derived: "recompute"`. Running the returned
      `open`/`diff` calls reproduces the live job's compare rows exactly;
    - serve: `capture`, then the session is evicted and a `DELETE` is
      sent, then `capture_wait` and `capture_cancel` succeed from a new
      session.
  - **Historical lookups (CAP-R14):**
    - a seam blocks the manifest read. `capture_wait` returns
      `loading` at its own deadline, and repeated waits do not add
      goroutines (counted via `runtime.NumGoroutine` against a bound);
    - once unblocked, the next wait answers from the registry;
    - a full queue answers `loading` with `retry_after`;
    - `capture_list` pages are marked `loading` in the same way.
  - **Cancellation and the terminal decision rule (CAP-R16, §7.4):**
    - a cancel before the manifest step (a seam holds `committing`
      earlier) gives `cancelled` and a rollback;
    - **a cancel during a deliberately blocked manifest publication:** a
      seam blocks the manifest `link(2)` and `capture_cancel` is sent. It
      answers `cancelling` at once; the control handler does no file I/O
      (a seam asserts no filesystem call from it). Releasing the seam
      with a successful link gives `done` with
      `cancel: "too late: committed"`; releasing it with a failing link
      gives `cancelled`, rolled back. In both cases there is exactly one
      terminal state, and `too late: committed` is never answered before
      the link has returned success;
    - a cancel after the commit answers `too late: committed`
      immediately.
  - **Cancel through the control path (CAP-R10):**
    - stdio: `capture_cancel` sent while a 45 s `capture_wait` is in
      progress returns `cancelling` at once, and the wait then reports
      `cancelled`;
    - serve: the same with `MaxCalls` full and the engine held by a
      blocked derivation.
  - **Two-step API (CAP-R8):**
    - `capture` returns `accepted` in microseconds;
    - `capture_wait` with `timeout` 45 s returns `running` for a 60 s
      fake capture, then the result;
    - a `timeout` over 45 s is refused;
    - `capture_list` shows the running capture first;
    - `capture_cancel` returns `cancelling`, then the job rolls back;
    - every call's wall time is under 50 s, against a 60 s client
      budget.
  - **Fault injection (CAP-R5):** §7.4.
- **Cancellation (`mcpstdio`):**
  - `notifications/cancelled` for the running id cancels the handler, and
    no response is written;
  - a cancel for another id does nothing;
  - EOF during a handler cancels it;
  - queued requests are answered in order afterwards;
  - the existing `mcpstdio` tests pass unchanged.
- **The stdout guarantee:** a new test, in the style of
  `TestStdoutCarriesOnlyJSONRPC`, runs the built binary with the fake in
  `spam-stdout` mode and checks that every stdout line is JSON-RPC.
- **Forced exit:** a new test, in the style of
  `TestSignalEndsAServerBlockedOnOutput`. The fake runs in `ignore-sigint`
  mode and stdout is blocked. After SIGTERM, the server exits within
  `shutdownGrace` + 100 ms and the fake's process group is gone.
- **Real capture (optional):**
  - Gated on `PERF_AGENT_BIN`, a binary outside `/tmp` (memory note).
  - It skips with a reason unless the real verifier accepts it: file
    capabilities present, not nosuid. This is the cap-aware gate the repo
    uses, not `Getuid`.
  - It launches a busy-loop child, captures 2 s of `cpu`, opens the
    result, and checks that the leaf function is present.
  - It also runs `kernel_stacks` only when `cap_syslog` is present (memory
    note).
- **Contract test** in perf-agent's root package (§5.2).
- **Netscan:** the new exec-site rules (§11) with positive and negative
  fixtures, as `TestTheNetworkScanSeesSyscallsExecAndDirectives` does.

## 11. Changes to existing code

### 11.1 Pre-existing perf-agent: no change

The user's constraint applies here. No file changes in perf-agent proper:
- the capture binary and its flags (`main.go`, `telemetry.go`,
  `perfagent/`);
- `cmd/flamegraph` and its outputs;
- `gpu/*`;
- `profilemodel`'s semantics.

No flag is added. Three things a perf-agent flag would have made cleaner
are worked around without one, and the workarounds hold:

| Would have wanted | Workaround |
|---|---|
| `--ready-fd` | parse the existing `Collecting data for …` stdout line (§5.3), pinned by a contract test |
| a parent-death watch | a detached reaper, the same MCP binary in an internal mode (CAP-R2, §6.6). `--duration` alone is not a bound: `agent.Start` runs before the timer, and Stop/Close have no deadline |
| a `-- cmd` launch mode with a start gate | launch first, attach after, and report `attach_latency` (§6.4) |

The only new file on the perf-agent side is
`capture_contract_test.go` in the root `package main`. It is test-only,
adds no flag and changes no output. `profilemodel` is used only through
its existing API (`store.open`, `View`, `Diff`).

### 11.2 New code

- `internal/capture` (new package, no transport knowledge):
  - `verify.go` (stat rules, `vfs_cap_data`, buildinfo, ELF);
  - `target.go` (§6.3);
  - `argv.go` (§5.2);
  - `run.go` (timeouts, readiness, rings, kill registry);
  - `reaper.go` (reaper mode: pipe, deadline, subreaper sweep, cgroup
    kill) and `cgroup_linux.go`;
  - `lock.go` (the captures `flock`, CAP-R6);
  - `exec_linux.go`, the only file there that imports `os/exec`. It holds
    exactly two `exec.Command` sites: the verified perf-agent, and the
    server's own executable (`/proc/self/exe`, resolved at start-up) in
    reaper or launcher mode;
  - `launch_linux.go`, the launcher mode: `runtime.LockOSThread`, then
    `syscall.RawSyscall(SYS_PRCTL, PR_SET_NO_NEW_PRIVS, 1, …)`, then
    `syscall.Exec(command[0], …)`. It is the only file allowed those
    names, and the reaper's `PR_SET_CHILD_SUBREAPER` prctl lives there
    too;
  - `capture_other.go`, which disables capture off Linux.
- `internal/perfmcp`:
  - `tool_capture.go`, `tool_capture_list.go`;
  - `capture_store.go`: layout, ids, manifests, retention;
  - `capture_compare.go`, which calls the existing diff code.
- The tests in §10.

### 11.3 Changes to existing MCP files (ours; allowed)

Each change is kept small and has a reason:

| File | Change | Why |
|---|---|---|
| `cmd/perf-agent-mcp/main.go` | Register `--perf-agent`, `--captures` (default `<profiles>/captures`), `--capture-command`, `--capture-ttl` and `--gpu-shim`, and pass them into `Config`. In the forced-exit branch, call `srv.KillCaptures()` before `return 0`. Before flag parsing, dispatch the two internal modes (reaper, launcher) to `internal/capture` when fd 3 carries a spec and `PERF_AGENT_MCP_INTERNAL` names the mode | Flags are parsed only here. The forced path returns without waiting on `Close` (100 ms), so the kill must be synchronous there. The reaper and launcher are this binary re-exec'd (CAP-R2), so `main` must route to them |
| `internal/perfmcp/config.go` | Add `Capture CaptureConfig` | `Config` is the only way into `New` |
| `internal/perfmcp/limits.go` | Add `Capture CaptureLimits`, with defaults and compiled ceilings in `validate` | `--limit` reflects over `Limits` |
| `internal/perfmcp/server.go` | `New` builds the job runner and the registry; `tools()` appends `capture`, `capture_wait`, `capture_cancel` and `capture_list`; `Close` cancels jobs and kills children. An **engine lock** wraps every tool handler **except** `capture` (pure admission), `capture_wait`, `capture_cancel` and `capture_list` (CAP-R13). The job takes it only for `preparing`'s shared inputs and for derivations and pin release (§4.0) | The tool list and the lifecycle live here. M2's "request goroutine owns all state" becomes "the engine lock's holder owns it", because job derivations run off the request goroutine. In stdio, handlers are sequential, so the lock contends only with a job |
| `internal/perfmcp/output.go` | Factor `writeTemp` plus the `s.link` publication out of `writePage` into `publishNew(root, dir, base, bytes)`, used by pages and manifests. Profiles are linked from staging with the same no-replace rule | One no-clobber implementation (M2-R4) instead of two. Page bytes and behaviour are unchanged, and the existing output tests pin that |
| `internal/perfmcp/diff` code (`tool_diff.go`) | Extract the body of `handleDiff` into a function `capture_compare.go` can call with two handles and a row limit | `compare` must be the `diff` tool's answer, not a second implementation. The `diff` results are unchanged, and `diff_test`/`diffgolden_test` pin them |
| `internal/mcpstdio/server.go` (+ `protocol.go`) | A reader that never waits: control messages (`notifications/cancelled`, EOF) on their own channel, an 8-request ordered queue, and immediate overload errors (CAP-R7). All output goes through one bounded asynchronous writer queue, and an exhausted backlog means a dead client: cancel everything and exit as on EOF (CAP-R7b). Optional stdio progress through `_meta.progressToken` and a `Progress(ctx, …)` helper that enqueues on that writer | Without it, a stdio capture cannot be stopped when the client goes away or stops reading, and stdio is the primary mode (decision 5) |
| `internal/perfmcp/guide.md` (and `guide_test.go`) | A "Capture loop" section: series, `keep` the base, `compare_with`, read `comparability` before trusting a diff, branch on `error.code`, and capture only targets the user's task names | The guide is where agents learn the investigation order |
| `cmd/perf-agent-mcp/netscan_test.go` | `execFile` becomes a set: `git.go` plus `internal/capture/exec_linux.go`. A rule for that file: exactly two `exec.Command` calls, whose first arguments are the verified-binary field and the self-executable field; no `exec.LookPath`; no `"sh"`, `"bash"`, `"sudo"` or `"pkexec"` literals. A second exception: `internal/capture/launch_linux.go` may use `syscall.Exec` and `syscall.RawSyscall`, but only with `SYS_PRCTL` as the first argument and `PR_SET_NO_NEW_PRIVS` or `PR_SET_CHILD_SUBREAPER` as the second (checked in the AST). `syscall.Kill` and `syscall.Flock` are already allowed | The test exists to forbid process starts and raw system calls elsewhere; each exception must be written into it, as narrowly as the AST allows |
| `cmd/perf-agent-mcp/main_test.go` | none expected: `TestTheServerLinksNoProfiler` still passes, since `debug/buildinfo`, `debug/elf` and `syscall.Getxattr` are standard library and no x/sys is needed | — |
| `docs/mcp.md` | "It captures nothing … needs no capabilities" and "writes only pages" become the M2c wording; new flags, the tools, the limits, retention (the one deletion) and guardrails | The current text would be false |

**Unchanged:** `internal/explain`, `internal/sourceroot`,
`internal/livehttp`, and every other tool handler.

## 12. Threat model

The adversary of interest is **text in the agent's context**: a prompt
injection in a web page, an issue, a source file, a frame name in a
profile, or a launched program's output. It steers the agent into calling
`capture`.

Command launch is on by default (decision 6). For a client that gives the
agent no shell, `capture` therefore *is* a shell: it runs arbitrary code
as the user for a bounded window. The user accepted this. The guardrails
bound **how long, how often and with what privilege**, not *what* the
code does.

| It could try to | Bounded by |
|---|---|
| Run arbitrary code (a downloaded script, `curl … \| sh` passed as `["sh","-c",…]`) | Not prevented by default: it runs as the user, which was accepted. Bounded by:<ul><li>one at a time, and the rolling windows;</li><li>a wall time of at most `StartTimeout + MaxDuration + StopGrace`;</li><li>no capabilities, no controlling tty, no agent environment;</li><li>a kill on timeout, cancel, EOF and shutdown, through the session and, where delegated, `cgroup.kill`.</li></ul>Hardening: `--capture-command` |
| Gain privilege through the launcher | **`PR_SET_NO_NEW_PRIVS`** (CAP-R1) stops privilege gain by exec. It does not stop a drop of dumpability, which is covered by the CAP-R1b recheck: no `execve` in the workload or its descendants gains privilege, so setuid `sudo`/`pkexec` and capability binaries run without their privilege and fail. The early refusal of a setuid or capability `command[0]` is a clearer error, not the boundary. A request to an already-privileged service over IPC (polkit, D-Bus, a `sudo` configured outside `no_new_privs`, `systemd-run`) is not stopped. polkit would prompt on the desktop, where the user would see it |
| Leave something running (fork + `setsid`, a daemon, a cron entry, a `systemd-run --user` unit) | Fork + `setsid` is killed by `cgroup.kill` (delegated cgroup), or else by the reaper's subreaper sweep (CAP-R3); when neither holds, the result says descendants may survive. The reaper also kills after server death (CAP-R2). Units started through another daemon, and persistence through the user's files, are not bounded, as with any shell the agent has. Stated in the docs |
| Profile a root daemon, another user's process, `gpg-agent`/`ssh-agent`, or a setcap'd tool, to read its code paths or (via perf-agent's `/proc/<pid>/mem` reads) its interpreter state | §6.3 preflight: the same uid and gid for all four ids, `readlink exe` allowed (dumpable), no capabilities. The child holds BPF capabilities, so this rule, not the kernel, keeps those targets out [user]. **Residual (CAP-R1b, accepted):** a target (existing, or launched: `no_new_privs` does not freeze dumpability) that execs a privileged image (`--pid` only) or drops dumpable after the preflight can be profiled for up to `MaxDuration` (30 s by default, configurable to 5 min), plus start-up and finalisation. The rechecks (readiness, every 1 s, end) discard such captures, but a transition that reverts between checks can be missed. That is no more than the setcap'd perf-agent grants the user by hand |
| Escalate through perf-agent's capabilities | One argv builder with a fixed flag list; the binary's capabilities a subset of the documented set (no DAC override); no `-a`; the server unprivileged and not root; never `sudo`. A launched command never inherits them: it is the server's child, not perf-agent's |
| Leak secrets through the result | No `cmdline` and no environment in results or manifests. Profiles hold function names, paths and build-ids, not memory. With `kernel_stacks`, the file holds kernel addresses (§4.2). Files are 0600. A launched program's output tail is returned and labelled untrusted |
| Reach the network | perf-agent: `--no-debuginfod`, no `--metrics-listen`, the environment scrubbed of `DEBUGINFOD_URLS`. The server's own network rules are unchanged. A launched command may use the network: that is the arbitrary-code row above |
| Fill the disk or overwrite files | The capture directory: per-file and total budgets, a TTL, a free-space floor, a size kill, no-replace links, private staging. A launched command can write wherever the user can: the arbitrary-code row again |
| Perturb the machine | Duration, rate and window limits, one capture at a time, children killed at their bounds |
| Hide what it did | Every capture is logged to stderr. The log has the target PID, `comm`, modes, duration and perf-agent's argv; for commands, the resolved `command[0]` and the argument count, never the arguments. It is listed in `capture_list`, in the manifests and on the page |
| Delete the user's files through retention | Retention removes only manifest-verified files the server wrote in the capture directory (§7.3) |

**Out of scope:**
- malware with the user's uid: it can run the setcap'd `perf-agent`
  directly, which is the user's configuration choice;
- browser extensions;
- root.

**The human check** is the client's per-tool permission prompt, which an
unattended loop will usually allow. The tool's annotations are honest:
`destructiveHint` and `openWorldHint` are true.

## 13. Open questions

**Answered 2026-11-15** (user and controller), folded in above:
- **Commands:** on by default; `--capture-command` is optional hardening.
- **Retention:** yes, with `--capture-ttl` (7 d) and the byte budget.
- **PID rule:** as designed.
- **`kernel_stacks`:** opt-in, with the note about kernel addresses.
- **Destination:** `<profiles>/captures` by default.
- **Two-step calls** (CAP-R8, superseding "the call blocks").
- **The orphan window is accepted.**
- **Source-only.**

**Controller rulings, 2026-11-15:**
- the best-effort per-command cgroup: **yes, when the user's tree allows
  it** (§6.4, §6.6);
- `cwd` must be inside a `--source` root or the profiles root (§4.2).

No open questions remain. Reviews #399 and #404 are resolved in §15.

## 14. Community defaults (brief, from memory; not re-verified this session)

- **Continuous eBPF profilers** (Parca Agent, the OpenTelemetry eBPF
  profiler, Grafana's eBPF profiler) run as privileged, system-wide
  DaemonSets with low fixed rates of about 19–100 Hz. They have no
  on-demand bounded-capture API to match.
- **Go's `net/http/pprof`** defaults to `seconds=30`, and refuses a
  duration longer than the server's `WriteTimeout`. That is the precedent
  for a 30 s default bound on a blocking on-demand capture.
- **`perf record`** has no bound of its own.

M2c therefore follows perf-agent's own defaults (10 s, 99 Hz) for the
capture and Go pprof's 30 s for the maximum.

## 15. Review resolutions

### 15.1 Review #399 (Codex, 2026-11-15)

Rulings CAP-R1..R7 are recorded in
`.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`.

| Finding | Ruling | Change in this design |
|---|---|---|
| **P1** Target TOCTOU: after the preflight, the target can exec a setuid or capability program or turn non-dumpable. The profiler follows the PID, and PID reuse is detected only afterwards | CAP-R1 | §6.3: launched commands run under `PR_SET_NO_NEW_PRIVS`, plus their own cgroup when available. That is the enforced boundary (§6.4, launcher mode). `--pid` keeps the preflight as a defence against the agent, with a recheck at readiness and at the end that discards the capture on a transition. The residual TOCTOU is an explicit accepted exposure: no more than the setcap'd perf-agent grants by hand, ≤ 30 s. Threat-model row rewritten; tests for transitions, exit and reuse (§10) |
| **P2** Orphan window: unchanged `main.go` runs `agent.Start` before the duration timer, and Stop/Close have no deadline, so after a server SIGKILL start-up or finalisation can hang indefinitely | CAP-R2 | §6.6: "bounded by `--duration`" corrected. A detached reaper (the same binary in a reaper mode, its own session, a pipe to the server) kills the capture's group and cgroup on EOF or at an absolute deadline. Killing the reaper as well is the documented unbounded case. §11.1 and §11.3 updated; reaper tests |
| **P2** Descendants escape through fork + `setsid`; `Pdeathsig` is not inherited and is tied to the creating thread | CAP-R3 | §6.6: `Pdeathsig` no longer relied on. cgroup v2 `cgroup.kill` when the tree is delegated, else `PR_SET_CHILD_SUBREAPER` on the reaper (which launches the workload) and a PID-specific sweep. The promise narrows, and the result says so, when neither holds. Fork + `setsid` tests |
| **P2** The `compare_with` baseline was not protected before retention | CAP-R4 | §7.3 and §7.4 step 1: the baseline is resolved, validated and pinned before any eviction; test |
| **P2** Publish → open → manifest: a crash or `ENOSPC` leaves files that cannot be found or deleted; `.seq` collisions | CAP-R5 | §7.4 rewritten as an intent/commit protocol: `.seq` persisted first, the intent naming every possible file, fsync ordering, the manifest as the commit point, start-up recovery to an `interrupted` manifest, and fault injection per stage. §7.1 and §7.2 updated |
| **P2** One-at-a-time was per process, while stdio sessions share `--captures` | CAP-R6 | §6.1: a cross-process `flock` on `<captures>/.lock` per capture. A second owner is refused with `busy`, naming the holder's PID, capture and deadline; test |
| **P2** A full stdin queue pauses the reading of cancel and EOF | CAP-R7 | §9: control messages are always read on their own channel; a request over the queue bound gets a bounded overload error; one writer mutex; test |
| **Minor** The ownership of the `/proc/<pid>` directory is not the dumpable indicator | — | §6.3: dumpable is now `readlink(/proc/<pid>/exe)` succeeding (the kernel's ptrace read check) plus the owner of `/proc/<pid>/status`. Explanation and test corrected |
| **Minor** `destructiveHint` must reflect retention even in PID-only mode | — | §4.5: `destructiveHint: true` in every configuration, naming retention and launch |

**Controller rulings recorded with the review:**
- the best-effort per-command cgroup: yes, when allowed;
- `cwd` must be inside a `--source` root or the profiles root (§13).

### 15.2 Review #404 (Codex, 2026-11-15)

Round-3 rulings are recorded at the end of
`.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`. Review #404
confirmed CAP-R4, the procfs explanation and `destructiveHint` closed.

| Finding | Ruling | Change in this design |
|---|---|---|
| **R1 partial:** `PR_SET_NO_NEW_PRIVS` does not freeze dumpability; a probe showed NNP=1 then `PR_SET_DUMPABLE=0` succeeds | CAP-R1b | §6.3: the target rule is called a defence, not an enforced boundary. Launched commands get the same recheck-and-discard as `--pid` (readiness, every 1 s, end → `target_changed`). The exposure is restated as `MaxDuration` (configurable to 5 min) plus start-up and finalisation, not "30 s", and a periodic check can miss a transition that reverts. §12 rows and §10 tests updated |
| **R2 open:** the spawn → pgid-registration gap left an unregistered perf-agent stuck in start-up | CAP-R2b | §6.6: the reaper creates both children itself (perf-agent and the launcher) and knows each identity when `fork` returns. The server only asks and receives PIDs. §5.3: the reaper re-verifies the binary. Test: a fault point exactly between spawn and report |
| **R3:** absolute "no process outlives" wording | CAP-R3b | §6.4 and §6.6: the claim is dropped. When `StopGrace` expires, `survivors` (PID, `comm`, start time, state) and `cleanup: incomplete` are reported, with `stuck_child`; test |
| **R5 open:** (a) a crash after linking left `nlink=2`, and recovery refused its own files; (b) name-only ownership could delete a user file after a lost `Link` race; (c) a live server's failures had no rollback, and live-owned intents were never recovered | CAP-R5b | §7.4 rewritten: dev, inode, size and digest recorded in the intent **before** linking; identity-matched deletion only (`nlink` 1 or 2; never a name it cannot prove); an owner token plus a 2 s heartbeat; live rollback by the owner, retried under `ENOSPC`; recovery of abandoned intents only when heartbeat, server and reaper are all gone. Boundary fault-injection tests, a lost-`Link`-race case and a live-owner `ENOSPC` case |
| **R6 partial:** the `flock` was released on server death while the reaper and children lived | CAP-R6b | §6.1 and §6.6: the locked descriptor (an open-file-description lock) is inherited and held by the reaper until both children are reaped. The lock file and recovery name the reaper's PID and start time; test |
| **R7 open:** immediate overload replies under the shared writer mutex could block the reader when stdout is not drained | CAP-R7b | §9: a bounded asynchronous writer queue for all output, so the reader never waits. An exhausted backlog means a dead client: cancel everything and exit as on EOF. Test with a full queue, blocked stdout and cancel or EOF |
| **New:** Codex's MCP client has a 60 s tool timeout, so the 75 s bound was not end to end | CAP-R8 | §4.0: a two-step API, `capture` (returns `{id, status: "running", eta}`), `capture_wait {id, timeout ≤ 45 s}`, `capture_cancel`, and `capture_list` showing running captures. Phases prepare / wait / commit, the same in stdio and serve. §9.1 documents Claude Code's and Codex's timeouts (Codex: `tool_timeout_sec`, 60 s by default; `startup_timeout_sec`, 10 s) |

**Rulings from Codex #405 on the server design, folded in here:**
- **SRV-R5, writer ownership:** a captures directory records its owning
  binary kind in `.owner` on first use. The defaults are
  `<profiles>/captures` for stdio and `<profiles>/captures-serve` for
  serve, and the other kind is refused (`owner_mismatch`, §6.2).
- **SRV-R6, JSON-only transport:**
  - under CAP-R8 no call exceeds about 45 s, so the POST-SSE and
    progress-notification requirement for serve is dropped (§9.1);
  - progress stays optional for stdio only;
  - elicitation is removed from the design entirely.

### 15.3 Review #408 (Codex, 2026-11-15)

Review #408 closed CAP-R1b, R2b, R3b, R5b, R6b, R7b and CAP-R4 at the
design level. The round-4 rulings are recorded at the end of
`.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`.

| Finding | Ruling | Change in this design |
|---|---|---|
| Two-step timing and ownership: `prepare` ran synchronously in `capture` (digests, retention hashes, fsync, the engine wait), so its latency was unbounded. `open` and `diff` ran on whichever request first saw the completion, with no resumable owner. `capture` and serve disagreed on rollback after an `open` or `diff` failure | CAP-R9 | §4.0 rewritten as one state machine for both transports, `accepted → preparing → running → committing → done \| failed \| cancelled`. Every phase runs in a server-owned job goroutine; `capture` only enqueues. The commit point is the manifest. `open` and `diff` are post-commit derivations whose failure is reported (`derived: failed`) and never rolls files back. Every wait is bounded (≤ 45 s, returns the current state). §7.4 maps the steps onto the states; the engine lock (§11.3). Tests with a blocked hash, fsync, engine, `open` and `diff`, and a commit outliving a wait |
| `capture_cancel` was a queued ordinary call | CAP-R10 | §4.0 and §9: the control path in both transports, ahead of the request queue (stdio) and of `MaxCalls` and the engine (serve). It only flags the job and returns `cancelling`; the job cleans up. Tests: a cancel behind an in-progress wait (stdio), and behind a full `MaxCalls` with the engine held (serve) |
| Session ownership vs eviction | SRV-R7 | §4.0 and §9: jobs are server-owned. Any token-holding session may wait on or cancel any job, and `DELETE` or eviction never cancels one. Test: capture, eviction or `DELETE`, then wait or cancel from a new session |
| The job registry was unbounded | CAP-R11 | §4.0: one active capture; terminal jobs in an LRU bounded by `MaxJobs` and `MaxJobBytes`. Every terminal result is also in the manifest plus `result.json`, so `capture_wait` after an eviction or a restart answers from disk with `"handles": "reopen"`. Heavy pins are held only while deriving. Tests for eviction and restart |
| The staging `mkdir` gap | CAP-R12 | §7.4 recovery: a staging directory created but not yet recorded is an explicit, logged, retained narrow orphan, empty and 0700, counted in `capture_list`. Identity-matched deletion is not weakened |

### 15.4 Review #410 (Codex r4, #410/#411, 2026-11-15)

Closed by this round of review: CAP-R10, CAP-R12, SRV-R7 and SRV-R4c.
Codex judged CAP-R9 and CAP-R11 to be in the right direction, and the
file commit under the dir lease (not the engine) to be sound. The
round-5 rulings are recorded at the end of
`.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`.

| Finding | Ruling | Change in this design |
|---|---|---|
| Two lifecycle definitions drifted. §9 said the job takes the engine only for derivations, and §11.3's engine wrapper covered `capture` | CAP-R13 | §4.0 is declared the **single lifecycle definition**, which the serve design references. The state table names what each state holds. The request is a pure check plus in-memory admission. The job takes the non-blocking dir lease before any filesystem side effect, so cross-process `busy` is an async failure. The engine is held for `preparing`'s shared inputs (baseline, pins, protected inputs, retention) and for derivations and pin release. The child wait and the file commit run outside the engine, under the lease. §7.4 step 1, §9 serve concurrency and §11.3 (the wrapper excludes `capture`, `capture_wait`, `capture_cancel` and `capture_list`) aligned |
| `capture_wait`'s disk fallback could block past 45 s | CAP-R14 | §4.0: historical lookups run on a bounded pool (2 workers, a queue of 16), shared per id. Waits end at their own deadline with `"loading"`, with no goroutine per timed-out wait; `capture_list` likewise. §6.1 row; tests with a blocked read, repeated waits and later completion |
| The `result.json` sidecar had no ownership, retention or recovery | CAP-R15 | §4.0 and §7.3: it is published atomically, with its identity in the series `.journal`, counted in `MaxStoredBytes`, and deleted with its capture by identity. A replaced sidecar is spared. A missing or failed sidecar after a restart answers terminally `derived: unavailable`, with the reason and the calls to redo it. Tests: many evictions with sidecars, a replaced sidecar spared, a crash before the link, a restart |
| Parameter drift | CAP-R16 | One set of names and values: `MaxJobs` 32 and `MaxJobBytes` 4 MiB (now also in the §6.1 limits table) and `"handles": "reopen"`. **The cancellation linearization point is the manifest write:** a cancel succeeds until the manifest is committed and answers "too late: committed" afterwards (§4.0 tools table; tests). The stale "stdio slot always free" wording does not appear in this design; it was the serve design's |

### 15.5 Review #413 (Codex r5, 2026-11-15)

Closed by this round: CAP-R13, CAP-R14 and CAP-R16. The ruling is
recorded at the end of `.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`.

| Finding | Ruling | Change in this design |
|---|---|---|
| **P2:** the sidecar was published after the lease was released, so it raced another process's retention (an orphaned `result.json`) and left the journal unserialised | CAP-R17 | The `result.json` sidecar and the `.journal` are **removed entirely**. Derived results live only in the in-memory job registry (`MaxJobs`, `MaxJobBytes`). After an eviction or a restart, `capture_wait` answers terminally from the committed manifest with `derived: "recompute"` and the exact `open`/`diff` calls (§4.0). No extra lease, journal or compaction is needed |
| **P2:** the append-only `.journal` grew for ever | CAP-R17 | Gone with the journal |
| §4.0 and §7.4 gave different orders for retention vs `.seq` and the intent | — | One order, §7.4: lease → recovery → engine section (pin, protected inputs, retention) → `.seq` → intent → space → staging, preflight, reaper. §4.0's `preparing` row cites it. Steps were renumbered, and recovery, deletion and orphan references updated |
| "The engine is never held on file I/O" was too broad | — | Narrowed to "the child wait and the file commit run outside the engine". Baseline verification, retention and `store.open` do I/O under it by design (§4.0, §7.4 step 3) |
| An obsolete two-column header remained in the §4.0 state table | — | Removed |
| Cancel vs a blocked manifest publication | — | §7.4's **terminal decision rule**: one atomic word, a control handler that does no file I/O, and `too late: committed` only once the manifest link has succeeded. A cancel recorded during an in-flight link resolves to "too late" if the link succeeds, or to `cancelled` if it fails. Test with a deliberately blocked manifest link (§10) |

**Removed tests**, which existed only for the sidecar: many evictions with
sidecars, a replaced sidecar spared, a crash between the journal append
and the link, and a restart with a missing sidecar. The eviction and
restart test now checks `derived: "recompute"` and that the returned
calls reproduce the compare rows.

**Note for review:** the decision rule refines CAP-R16's "linearization
point = manifest write". The point is the successful manifest link. A
cancel that arrives while that single link is in flight is answered
`cancelling`, and is decided by the link's outcome rather than being
able to stop it.
