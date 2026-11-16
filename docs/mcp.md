# perf-agent-mcp

`perf-agent-mcp` is an MCP server that lets an AI agent investigate perf-agent profiles and, when started with `--perf-agent`, capture new ones: an autonomous agent can run capture, analyse, change the code, capture again and compare. It speaks MCP over stdio only and works offline. It answers from the same profile model as `cmd/flamegraph`; every figure an agent states about a profile is checked against that engine before a page is written or published. A live page that a browser on the same machine reads will ship with a separate serve binary, not this one.

## What it does not do

- It opens no outgoing connection: it never dials, looks a name up or makes an HTTP request. It opens no listener: nothing in this process serves HTTP, and the stdio server registers no `page_*` tool. The live page engine is compiled in as library code for the future serve binary, so the binary still links `net/http` until that split. It starts no process and makes no raw system call except at reviewed sites: the read-only `git` probe in `internal/sourceroot/git.go` (`exec.LookPath("git")`, then `exec.CommandContext`), and for capture two files of `internal/capture`. `exec_linux.go` holds the only two `exec.Command` calls: `exec.Command(self)` in `selfCommand`, this server's own binary re-run as a capture's reaper or launcher, and `exec.Command(path)` in `startPerfAgent`, the verified perf-agent. `launch_linux.go` holds the launcher's one `syscall.Exec` of the command and the only raw system calls, two `syscall.RawSyscall(syscall.SYS_PRCTL, …)`: `PR_SET_NO_NEW_PRIVS` (the launcher, before that exec) and `PR_SET_CHILD_SUBREAPER` (the reaper). Tests read the source of every non-standard package the binary links (perf-agent's own and pprof's; the standard library is not scanned) for client calls, name resolution, raw system calls, process starts and listening calls, hold those sites to exactly those shapes and callers, refuse cgo, assembly and `go:linkname` in those packages, and check that every socket of a running server is TCP on loopback. The scan is drift protection for code review, not a defence against a committer set on evading it: it follows no values beyond its rules.
- It never loads BPF, links neither `perfagent` nor `cilium/ebpf`, and never holds or raises a capability. With `--perf-agent` it runs the user's setcap'd `perf-agent` binary as a child process for a bounded time (see Capturing); without it, it captures nothing and starts no profiler.
- It writes only pages under `--output` and captures in the capture directory, and never replaces an existing file. Retention in the capture directory is the one place it deletes files (see Retention). The one write outside its roots is a launched command's cgroup (see Guardrails).
- It is not shipped in releases, release archives or container images. Build it from source.

## Build

```bash
go build -o perf-agent-mcp ./cmd/perf-agent-mcp
```

## Flags

| Flag | Meaning |
|---|---|
| `--profiles DIR` | Required. The root profiles (`.pb.gz`) and executions logs (`.execlog`) are read from. |
| `--source DIR` | A source root, for the `source` tool. Repeatable. |
| `--source-manifest FILE` | A source manifest. Repeatable. |
| `--source-git` | Allow read-only, offline git probes in the source roots. On by default; `--source-git=false` turns it off. |
| `--output DIR` | The root rendered pages are written to. Without it `render_flamegraph` is disabled. |
| `--limit Name=Value` | Override a limit. Repeatable. Names are the fields of the limits tables below, such as `Model.MaxRawBytes=1073741824` or `Capture.MaxDuration=1m`. An unknown name exits 2 and lists the valid ones. |
| `--perf-agent PATH` | The absolute path of the setcap'd perf-agent binary. Without it every capture tool answers `disabled`. Checked at start-up (see Setting up capture); a binary that fails the checks exits 1, a relative path exits 2. |
| `--captures DIR` | The capture directory, a subdirectory of the profiles root (relative to it, or absolute inside it). Default `<profiles>/captures`. The profiles root itself, a non-directory, or a path or symlink that leads outside the root exits 2. Checked only with `--perf-agent`. |
| `--capture-command PATH` | Repeatable allow-list: when given, a capture may launch only these programs (absolute paths; each a regular, executable file, not setuid, setgid or setcap'd, or the server exits 1). Without it any program may be launched. |
| `--capture-ttl AGE` | How old an unkept capture may grow before retention deletes it, a Go duration (default `168h`, 7 days). `0` keeps captures of any age; negative exits 2. It wins over `--limit Capture.TTL`. |
| `--gpu-shim PATH` | The absolute path of the CUPTI adapter a `gpu` capture injects. Without it `gpu` captures are refused. A shim that fails its checks exits 1. |
| `--version` | Print the version. |

Exit status: 0 at end of input or on SIGINT/SIGTERM, 1 on a runtime error, 2 on a usage error. Logs go to stderr; stdout carries only JSON-RPC. `--captures`, `--capture-command`, `--capture-ttl` and `--gpu-shim` without `--perf-agent` have no effect, and the server says so on stderr.

### Roots

Every file is opened through an `os.Root`, so `..`, symlinks that leave the root and races cannot escape it. An agent may pass a path relative to a root or an absolute path inside it (under the root as given or under its resolved real path); anything else is refused by its text alone, without looking anything up, and the error names the root. Profiles and sources are only read, except the capture directory, a subdirectory of the profiles root that the server writes captures into and retention removes them from (a sub-root of its own). The output root receives pages (`.html`), published with a no-replace hard link: a name that exists is refused, so the agent picks a new one. Using the same directory for `--profiles` and `--output` works, but a page can never be written over an opened profile.

## Tools

Integers are decimal strings in every result, a failure's `explanation` index included, except source line numbers (a row's `line` and each `lines[].n`), which are JSON integers. A fact has `status` `ok`, `zero` or `unavailable`; an unavailable fact has a `reason` and no `value`.

Every result about a GPU view carries the view's sampling `notes`, `render_flamegraph` results included. A result about a CPU view has no `notes` key (nor `diff`'s `base_notes` and `new_notes`), except `open`, whose `notes` is always present.

Arguments match by exact name; an unknown argument, a name given twice or a differently cased name is refused. A tool error is a result with `isError: true` that says what to call next; it repeats at most a short prefix of any argument and is itself bounded by `MaxResponseBytes` (at least 1 KiB).

| Tool | Use |
|---|---|
| `open` | Open a profile (and optional execlog). Returns a handle, the views, capabilities, quality and sampling notes. |
| `top` | Hottest functions or call-tree nodes by cumulative or self value, paged. |
| `path` | The root-first frames of one node, paged. |
| `sandwich` | Callers and callees of one function. |
| `gpu_kernels_for` | The kernels under a GPU launch node. |
| `facts` | Every fact of one node, plus view-level values under `view_scope`: the unsampled portion and the view's quality (`open` gives the default view's only). |
| `diff` | Compare two opened profiles of the same view. |
| `source` | Source lines for a node and how well they are tied to the profiled build. |
| `render_flamegraph` | Check explanations and write an explained flame graph page. |
| `capture` | Start capturing a profile of a process (`pid`) or a launched command into a series. Answers a job id at once. |
| `capture_wait` | Wait, at most `timeout`, for a capture job by job id or capture id; answers its state, or once done its result. |
| `capture_cancel` | Cancel a capture job. Answered at once, ahead of queued calls. |
| `capture_list` | The running job, then each series' history from the manifests on disk. |

Every tool takes `handle` and `view` (a view name `open` returned, such as `cpu/nanoseconds#0`) unless noted: `open` takes a `path`, `diff` takes a base and a new handle and view. One example call of each, as `arguments`:

```json
open               {"path": "cpu.pb.gz", "execlog": "run.execlog"}
top                {"handle": "<handle>", "view": "cpu/nanoseconds#0", "rows": "nodes", "by": "cum", "limit": 5}
path               {"handle": "<handle>", "view": "cpu/nanoseconds#0", "node": "<node id from top>", "limit": 200}
sandwich           {"handle": "<handle>", "view": "cpu/nanoseconds#0", "function": "<function ref from top with rows=functions>"}
gpu_kernels_for    {"handle": "<handle>", "view": "<a GPU view>", "node": "<launch node id from top>"}
facts              {"handle": "<handle>", "view": "cpu/nanoseconds#0", "node": "<node id from top>"}
diff               {"base_handle": "<handle>", "base_view": "cpu/nanoseconds#0", "new_handle": "<handle>", "new_view": "cpu/nanoseconds#0", "work_base": 1000, "work_new": 1200}
source             {"handle": "<handle>", "view": "cpu/nanoseconds#0", "node": "<node id from top>", "context": 5}
```

`render_flamegraph` has its own example under Explanations, and the capture tools theirs under Capturing. `execlog`, `work_base`, `work_new`, `context`, `title`, `focus` are optional; `top`, `path`, `sandwich`, `gpu_kernels_for`, `diff` and `source` also take `limit` and `cursor`, `top` takes `domain`.

Lists carry `total_rows`, `skipped`, `omitted_count`, `omitted_weight` (the sum of the omitted rows' own values, which overlap, so never a remaining share), `omitted_weight_basis` (which value was summed: `cum`, `self`, `abs_delta` or `value`) and `next_cursor`; pass the cursor back with the same arguments.

The agent guide is served as the `perf-agent-guide` prompt and the `perf-agent://guide` resource (`text/markdown`), and the initialize `instructions` point to it.

## Explanations and the check

`render_flamegraph` takes explanations: a node, structured claims, and prose that refers to them.

```json
{
  "handle": "<handle>", "view": "cpu/nanoseconds#0", "out": "hot.html",
  "explanations": [{
    "node": "<node id>",
    "claims": [{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": "0.411"}],
    "prose": "{n:<node id>} holds {c1} of the view."
  }]
}
```

- A claim is `{id, metric, scope, node, module, denominator, unit, value}`. The engine compares `value` with its own fact as decimals at the rounding of that metric; the agent cannot pass or widen a tolerance.
- Prose, hypotheses and `next_measurement` place figures with `{cN}`, a claim of the same explanation, and frames with `{n:<id>}`. Any other number is shown as unchecked. Every claim must be placed by its `{cN}`; a claim placed nowhere is rejected as `unplaced_claim`, so the page's checked count is the figures it shows.
- Only node facts are claimable: what `top` with `rows: "nodes"`, `path` and `facts` give for a node. Function aggregates (`top` with `rows: "functions"`, `sandwich`) are not.
- A claim's `metric` is a claimable metric name (the guide lists them), not a path in the `facts` JSON: `serialization.true` is `serialization_true`, `quality.serialized.true` is `quality_serialized_true`. Quality metrics take `"scope": "node"` or `"scope": "view"`; `quality_unresolved_by_module` also needs `module`. `unsampled_portion` is view-level only: claim it with `"scope": "view"` and no node.
- A range is written "between {c1} and {c2}". Text glued to a placeholder that changes its meaning is rejected as `glued_to_claim`: a sign, digit or decimal point before it, and after it anything but a space, the end of the text or closing punctuation (`. , ; : ! ? …`, a closing bracket or quote). A claim shows its own unit, so a `%`, `‰`, unit or letter in any script after it is glued. A unit the claim already shows, written after the placeholder and a space ("{c2} ns"), is not refused: the result's `notices` carry a `unit_repeated` notice at that text's path.
- `title` is free text of at most `Explain.MaxTitleBytes` bytes with no placeholders; a longer one or one with a placeholder is refused before any attempt is recorded. Numbers in it are marked unchecked in the page's heading and counted in `unchecked_numbers`.
- The page's follow-up prompts name the profile's path relative to the profiles root and its `profile_digest`: whoever follows one opens that path and checks the digest matches.
- If anything fails, nothing is written. The result is `rejected`, lists every failure (with the engine's value when one can be computed; the result is itself bounded by `MaxResponseBytes`), and returns an `attempt` to pass when retrying. The page lists earlier rejected attempts for the same profile and view, made through any handle. That history is bounded (`MaxAttemptsPerView`, `MaxAttemptViews`, `MaxHistoryBytes` and the page's own size): what it leaves out, the page counts.

## Capturing

With `--perf-agent`, the agent can profile a process of the user's, or a command it launches, and compare the result with an earlier capture. The server stays unprivileged: it runs the user's setcap'd `perf-agent` as a child for a bounded time, writes the files into the capture directory inside the profiles root and opens them through the same `open` path as any profile.

### Setting up capture

1. Build perf-agent with blazesym linked statically (`make build`), and give it its capabilities, as `docs/requirements.md` does:

   ```bash
   sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore,cap_syslog+ep /path/to/perf-agent
   ```

   `cap_bpf`, `cap_perfmon`, `cap_sys_ptrace` and `cap_checkpoint_restore` are required; `cap_syslog` enables `kernel_stacks`; `cap_sys_admin` and `cap_dac_read_search` (with a warning) are accepted. Any other capability is refused by name.
2. Keep the binary **not in `/tmp`**: `/tmp` is usually mounted `nosuid`, where file capabilities do not apply, and a binary on a `nosuid` or `noexec` mount is refused. Put it in a private directory you own, such as under `~`. A shared blazesym library only draws a warning, but under secure execution `LD_LIBRARY_PATH` is ignored, so link it statically.
3. Start the server with `--perf-agent /path/to/perf-agent`, as your own user, without capabilities and outside any sandbox.

At start-up, and again before every capture, the server checks the binary without running it: an absolute path, resolved once; a regular file, not setuid or setgid, not writable by group or other, owned by root or the server's user and executable by it; no directory above it that another user could replace it through (up to a private directory you own); a filesystem that honours file capabilities; its `security.capability`; and its Go build information, which must name perf-agent. A binary that changed since start-up is refused with `binary_changed`: restart the server. `--gpu-shim` gets the owner and directory checks.

The server itself must run with euid other than 0, empty effective and permitted capability sets, `no_new_privs` off, and a capability bounding set that keeps every capability perf-agent's file grants. Otherwise the server still starts, but capture is disabled and every capture tool answers `disabled` with the reason (under `no_new_privs`, or with those capabilities missing from the bounding set, perf-agent's file capabilities could not apply at exec). Capture also needs Linux.

### The loop

```json
capture        {"command": ["/home/u/proj/bench", "-n", "3"], "cwd": "/home/u/proj", "series": "parse-opt", "label": "base", "duration": "20s", "keep": true}
               → {"job": "j1-3f9a", "status": "accepted", "eta": "…"}
capture_wait   {"id": "j1-3f9a"}                         → the result: capture "parse-opt/001", each profile's open result with its handle
… change the code, rebuild …
capture        {"command": ["/home/u/proj/bench", "-n", "3"], "cwd": "/home/u/proj", "series": "parse-opt", "label": "cand1", "compare_with": "001"}
capture_wait   {"id": "<job>", "timeout": "45s"}        → the result, with compare: comparability and the first diff rows
capture_wait   {"id": "parse-opt/002"}                  → after a restart: from the manifest, with recompute
capture_cancel {"id": "<job>"}
capture_list   {"series": "parse-opt"}
```

`capture` arguments:

| Argument | Default | Rule |
|---|---|---|
| `pid` | — | A running process of the user's. Exactly one of `pid` and `command`. |
| `command` | — | argv, no shell. `command[0]` is an absolute path or a bare name looked up once in the PATH the workload gets; a relative path (`./x`) is refused. With `--capture-command`, it must resolve to one of those paths exactly. A setuid, setgid or setcap'd program is refused. |
| `cwd` | the first `--source` root, else the profiles root | `command` only. Inside a `--source` root or the profiles root; a relative one is relative to the profiles root. |
| `modes` | `["cpu"]` | `cpu`, `offcpu`, `gpu`, each once. `gpu` needs `--gpu-shim`; a `pid` target must already have the shim mapped. |
| `duration` | `10s` | A Go duration from 1s to `Capture.MaxDuration`. |
| `sample_rate` | 99 | Hz, from 1 to `Capture.MaxSampleRate`. |
| `kernel_stacks` | false | Needs `cap_syslog` on perf-agent. The profile then holds kernel addresses. |
| `unwind` | `auto` | `auto`, `fp` or `dwarf`, perf-agent's `--unwind`. |
| `series` | `adhoc` | One series per loop: 1–64 bytes of `[A-Za-z0-9._-]`, not starting with a dot. |
| `label` | `run` | Names the capture in its series, as `base` or `cand3`: 1–32 bytes of the same set. |
| `compare_with` | — | A capture id of the same series: `001`, or `<series>/001` naming this capture's own series. The capture inherits the baseline's `modes`, `duration`, `sample_rate`, `unwind` and `kernel_stacks`; passing a different value is `incomparable`. The target may differ. |
| `keep` | false | Exempts the capture from retention, as for a loop's baseline; at most `Capture.MaxKeptPerSeries` per series. |

- `capture` checks its arguments and admits the job in memory, with no disk I/O, and answers `{job, status: "accepted", eta}` at once. Everything after that is the job's: `accepted → preparing → running → committing → done | failed | cancelled`.
- `capture_wait {id, timeout?}` takes the job id, or a capture id `<series>/NNN`: `capture_id` in the job's state, the cancel answer and a job's error, and `capture` in the result's `capture` and in `capture_list`'s rows, whose `id` is the bare number `NNN` that `compare_with` takes. A bare `NNN` given to `capture_wait` or `capture_cancel` is `invalid_argument`, naming `<series>/NNN`. It waits at most `timeout` (default 30s, at most 45s), counted from when the server read the call, not from when it ran: a call queued behind another spends its budget there, so an agent sends one capture call at a time. Until the job settles it answers its state (`status`, `phase`, `elapsed`, `eta`, `derived`); a done job whose open and diff are still being made is `done` with `derived: "pending"`. A client that passes `_meta.progressToken` gets progress notifications at each change and every 5 s; they are optional and may be dropped under output back-pressure.
- The result holds `capture` (id, series, label, manifest, settings and `settings_digest`, target, durations, `attach_latency`, `stopped_by`, the perf-agent version and capabilities, and the workload's output tails, labelled untrusted), `profiles` (each with the `open` tool's own result and its handle), with `compare_with` a `compare` (both sides, `comparability`, and per view the first `Capture.CompareRows` rows of the `diff` tool and `next`, the call for the rest), `evicted` (what retention removed) and `notes`. `comparability` reports, never hides: `settings`, `target_exe` (same or changed build-id), and both sides' `profiled_duration` and `attach_latency`. Totals are not normalised.
- A failed open or diff after the commit is `derived: "failed"` with `derive_error`; the files stay. When the store has closed a handle of the result, it says `handles: "reopen"` and carries `recompute`.
- After a registry eviction or a server restart, `capture_wait` by capture id answers from the manifest: the terminal status, `derived: "recompute"`, `handles: "reopen"` and `recompute`, the `open` and `diff` calls that remake the handles and the compare (`${<bind>.handle}` names the handle of the step bound as `<bind>`). A manifest not read by the deadline answers `status: "loading"` with `retry_after` and a `next` that says when to stop waiting; so does a capture id asked before the server's start-up recovery has run (its pass runs again while another process holds the captures lock). A capture whose intent is on disk and whose manifest is not yet answers `status: "in_progress"`, with `capture`, `retry_after` (1s to 5s), `next`, and `holder`, the server (and reaper) writing it, or `, which is gone` when recovery is yet to record it as interrupted; it is never `unknown_capture`. Finished job ids stay resolvable through their capture id (up to 4096).
- `capture_cancel {id}` answers `cancelling`, `cancelling (decision pending)`, `too late: committed`, `too late: failed` or `cancelled`. Job status, manifest outcome and answer always agree.
- `capture_list {series?, limit?, cursor?}` lists the series with their counts, bytes and leftovers, or one series' captures newest first; it reads the manifests, so a loop finds its history after a restart. A history not read in time is `loading`.

A stdio session's jobs end with it: EOF on stdin cancels them, and so does a client that stops reading its output (see Guardrails).

### Failures

A failure is an `isError` result whose text is one JSON object, so a loop can branch on it:

```json
{"error": {"code": "busy", "message": "capture job j3-3f9a is running; one capture runs at a time", "retryable": true, "retry_after": "5s", "next": "capture_wait or capture_cancel j3-3f9a; its reserved duration ends by 2026-11-15T10:42:07Z"}}
```

`retryable` is always present, and a retryable failure always has `retry_after`, in whole seconds (`0s`: at once). A job's failure also names `job`, `capture_id` and `status`; perf-agent's exit and stderr tail come only on failure. Messages repeat agent input only cut, and name no absolute server path or temporary: the profiles root is `the profiles root` and the staging directory `staging`, in the programs' output tails too.

| Code | Retryable | Meaning, and what `next` says |
|---|---|---|
| `disabled` | no | Capture is off: no `--perf-agent`, or why it is disabled; `gpu` without `--gpu-shim` |
| `invalid_argument` | no | Names the argument and the valid range; also too many kept captures, and a command that cannot be executed |
| `incomparable` | no | A setting differs from the `compare_with` baseline; names it and both values |
| `unknown_baseline` | no | No usable baseline with that id in the series: none, a failed or interrupted one, one whose files no longer verify, or settings this server cannot read; lists the series' last ok ids |
| `busy` | yes | Another capture holds the slot or the captures lock: this server, another server, or a reaper still ending one; names the holder. `retry_after` is the time to the holder's recorded end, rounded up to whole seconds and at most 5s (a holder records its worst case); `next` names that latest end |
| `unknown_capture` | no | `capture_wait` or `capture_cancel` named an id this server has no job or manifest for, or whose manifest is gone |
| `target_changed` | yes | The target's credentials, dumpable flag, capabilities or start time changed during the capture; it was discarded |
| `owner_mismatch` | no | The capture directory belongs to another kind of server |
| `budget` | yes | `Capture.WindowCaptures` or `Capture.WindowDuration` is used up; `retry_after` is when the window frees, rounded up to whole seconds |
| `disk` | no | `Capture.MaxStoredBytes` or `Capture.MinFreeBytes` cannot be met even after retention, or a write failed; names the series using most space |
| `exists` | no | A final name is taken; pass another `label` |
| `target_refused` | no | The target is not an ordinary process of the user's (see Guardrails) |
| `target_not_found` | no | No such PID |
| `target_exited` | no; yes when the command was never started | The target or command ended before profiling began; the command's output tail is included. A gone pid or a command that ended on its own ends the same way again: `next` says what to change (more work for the command, the pid of the process it started, or the new pid of a restarted process) |
| `pid_reused` | yes | The PID's start time changed |
| `gpu_not_injected` | no | The `pid` target has not loaded the CUPTI adapter; restart it with `CUDA_INJECTION64_PATH`, or capture it as a command |
| `perf_agent_refused_args` | no | perf-agent exited 2: it is older or newer than this server expects |
| `perf_agent_failed` | yes, except for a missing capability | perf-agent exited 1; the stderr tail and advice |
| `perf_agent_unstartable` | no | perf-agent could not start (exit 126 or 127, a loader error) |
| `binary_changed` | no | perf-agent changed since start-up; restart the server |
| `start_timeout` | yes | perf-agent was not ready within `Capture.StartTimeout` |
| `finish_timeout` | yes | perf-agent did not finish within `Capture.FinishTimeout` |
| `too_large` | no | A file passed `Capture.MaxProfileBytes` or `Capture.MaxExeclogBytes` while written; capture shorter or at a lower rate |
| `no_output` | yes | perf-agent exited 0 without the expected file, or a failure recorded no cause |
| `stuck_child` | no | Processes survived `Capture.StopGrace`; `survivors` lists their PID, comm, start time and state. Do not retry; look at them |
| `closing` | no | The server is shutting down and starts no capture; start a new one |
| `cancelled` | no; yes when `status` is `interrupted` | The job was cancelled. `status: "interrupted"` (with `retry_after: "0s"`) means the server ended under the capture and recovery recorded it: capture again |

A failed capture keeps no profile; its manifest is kept with the code, so `capture_list` shows the failure.

### Retention

Retention is the one place the server deletes files. It runs at start-up and before each capture, only inside the capture directory, and there is no delete tool.

- **What it may delete:** a capture whose manifest says this server wrote it, in the series it names, and each of whose files still has the manifest's device, inode, size and SHA-256 and exactly one link. A file the user replaced, rewrote, linked elsewhere or turned into a symlink, or anything without a manifest, is never touched; if any file of a capture is not provably its own, the whole capture is kept and the pass notes it.
- **Order:** finish the evictions an earlier pass began; then captures older than `--capture-ttl`; then, per series, unkept captures beyond `Capture.KeepPerSeries`, oldest first (failed and interrupted captures have a budget of their own of the same size); then, while the directory lacks room for the largest capture under `Capture.MaxStoredBytes`, the oldest unkept capture of the least recently written series, but only when removing everything it may remove would make that room. What it removed is logged and listed in the result's `evicted`.
- **What it never deletes:** a `keep: true` capture, whatever its age; the newest `compare_with` baseline of each series; the baseline of the capture being prepared; a capture an intent or an unfinished rollback names, including a commit whose outcome could not be read (undecided until a later pass settles it); a capture open in this server's store; an incomplete capture (a file missing with no eviction marker); anything when a manifest cannot be read; anything in a series it cannot list.
- **How:** it first links an eviction marker, `NNN-label.evicting`, to the manifest, so an interrupted eviction is finished by the next pass. Each file is moved to a private quarantine name (`.del-…`), checked there by its stat and SHA-256, then unlinked; a file that is not the recorded one puts every moved file back. The manifest goes next and the marker last. A pass first puts back what an interrupted one left aside.
- Ids are never reused: `.seq` is never lowered, and must not be removed by hand. The TTL step is skipped when the clock reads earlier than the newest capture's start; a clock that jumps forward by more than the TTL makes unkept captures look expired. On a filesystem that reports another device number after a remount (a btrfs subvolume, for one), identities no longer match and the captures are kept: remove them by hand.

### Guardrails

- **One capture at a time**, across processes: the capture directory's `.lock` is an exclusive `flock` the reaper holds until both of its children are reaped. A second capture gets `busy`, naming the holder. Several stdio servers may share a capture directory; a server of another kind may not (`.owner`).
- **Targets:** a `pid` must be an ordinary process of the user's: real, effective, saved and filesystem uid and gid all the server's, dumpable (`/proc/<pid>/exe` readable), no permitted capabilities, not a kernel thread, not pid 1, not the server or its children. The check repeats at readiness, every second while profiling and at the end; any change discards the capture (`target_changed`, `pid_reused`).
- **Commands:** launched with argv and no shell, in their own session with no controlling terminal, by a launcher that sets `no_new_privs` before the exec, so no exec in the workload or its descendants gains privilege. The workload gets only `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `XDG_RUNTIME_DIR` and `CUDA_VISIBLE_DEVICES` from the server's environment, plus `CUDA_INJECTION64_PATH` for `gpu`; there is no `env` argument. Its stdin is `/dev/null`; its output goes to bounded buffers, of which only the last `Capture.MaxOutputTail` bytes per stream are returned, labelled untrusted. Only the launched PID is profiled, not processes it starts.
- **perf-agent** is started by one argv builder with a fixed list of flags (always `--no-debuginfod`, never `-a`, `--metrics-listen` or the debuginfod flags) and the environment `PATH=/usr/local/bin:/usr/bin:/bin` and `LC_ALL=C`. Its stdout and stderr never reach the MCP stream.
- **The cgroup write:** when the server's own cgroup is in a delegated, writable cgroup v2 subtree (as under a systemd user session) that has `cgroup.kill` (Linux 5.14 or later), each launched command gets a child cgroup `perf-agent-mcp-<id>` there. The server process creates it just before it starts the capture's reaper (and removes it if the reaper cannot start); the reaper starts the launcher in it, ends it with `cgroup.kill` and removes it. Only cgroups named `perf-agent-mcp-*` under that subtree are created, killed or removed. Without such a subtree, or when the cgroup cannot be created (the result notes why), the reaper is a child subreaper and kills orphaned descendants by PID; when neither works, only the command's process group is signalled, and the result says descendants may survive.
- **The reaper:** each capture has a detached reaper, this same binary started by the server in an internal mode, in its own session. It starts perf-agent and the launcher itself and sends every signal; the server process signals nothing, and to end a capture at once it only closes the reaper's command pipe. On `capture_cancel` the reaper sends perf-agent SIGINT, then SIGKILL after `Capture.StopGrace`. When its command pipe closes (the server's shutdown, a dead client, the server's death, SIGKILL included) or at an absolute deadline (`Capture.StartTimeout` + duration + `Capture.FinishTimeout` + `Capture.StopGrace`, even when the server is alive but hung), it SIGKILLs both children, ends the workload's cgroup or sweeps its descendants, and reports what survived. A workload still running when perf-agent finishes gets `cgroup.kill`, or SIGTERM and then SIGKILL after `Capture.StopGrace`.
- **A dead client:** EOF on stdin, or a client that stops reading stdout (a response finds the output queue full, or no write finishes for 30 s while output waits), cancels every capture and ends the server as at EOF; a capture can run for those 30 s before the client is found dead. SIGINT or SIGTERM kills the captures first.
- **Files:** the capture directory is created mode 0700, files 0600, published with no-replace links from a private staging directory; a file over its limit is killed while written.

### Residual exposures

These are stated as they are; the guardrails bound the agent, not malware running as the user, which could run the setcap'd perf-agent directly.

- A launched command is arbitrary code as the user for its window, which is accepted. `--capture-command` narrows what may run, but listing an interpreter (`python3`, `bash`) still allows arbitrary code through its arguments. Requests to an already-privileged service (polkit, D-Bus, `systemd-run --user`) and processes started through another daemon are not bounded.
- A target that execs a privileged image or turns non-dumpable after the check can be profiled for up to `Capture.MaxDuration` plus perf-agent's start and finish (`Capture.StartTimeout`, `Capture.FinishTimeout`); a change that reverts between two checks can be missed. `no_new_privs` does not freeze dumpability.
- Killing both the server and the reaper (for example `kill -9` of both) leaves the children unbounded. Processes still alive after `Capture.StopGrace`, such as a CUDA process stuck in D state, are reported as `survivors` (`stuck_child`), never retried.
- A crash inside a single system call can leave a file nothing proves the server made: a `.seq` temporary, a temporary not yet recorded, an empty staging directory, a quarantined `.del-*` name. Recovery keeps and logs each (`orphan_temp`, `orphan_staging`, `left`), never deletes it, and `capture_list` counts it under `leftovers`; remove such files by hand.
- Removals check the file at its quarantine name right before the unlink. That narrows, but does not close, the window in which another process of the same user could rename a file onto that name; the intent rewrite has the same narrowed window.
- Another server's open handles are invisible to retention; deleting their files is harmless, since a handle holds its profile in memory. Another server's retention can remove a capture this server remembers: before answering from memory the server re-checks that the manifest is still there (through the lookup pool, answering `loading` when slow) and answers `unknown_capture` when it is gone.
- `Capture.WindowCaptures` and `Capture.WindowDuration` count per server process; a loop that restarts the server resets them, and `Capture.MaxStoredBytes` is what bounds it across restarts.
- The source scan is drift protection for review, not a defence against a committer set on evading it.

### Client timeouts

`capture` returns at once and `capture_wait` blocks at most 45 s, so no call comes near the smallest client limit, Codex's default of 60 s per tool call (`tool_timeout_sec`); no client configuration is needed. Claude Code's per-call limit (`MCP_TOOL_TIMEOUT`, or a per-server `timeout`) is far larger by default. Progress notifications do not extend either client's limit.

## Live page

The live page, a read-only page a browser on the same machine reads while the agent explains profiles, will ship with a separate long-lived serve binary. The stdio server has no `--live` flag, registers no `page_*` tool and opens no listener.

## Agent skill

`skills/perf-agent-mcp/SKILL.md` is a short skill (the `SKILL.md` format Claude Code and Codex both read) that tells an agent when to use these tools, the order to call them in, the rules it must keep, and how to fix the most common rejections. It sends the agent to the server's own guide, which stays the authority; a test keeps the tool, metric and failure names it cites in step with the server.

Install it by copying or symlinking the directory into your agent's skills directory:

```sh
# Claude Code
mkdir -p ~/.claude/skills && ln -s "$PWD/skills/perf-agent-mcp" ~/.claude/skills/perf-agent-mcp
# Codex (user skills live in ~/.agents/skills; symlinks are supported)
mkdir -p ~/.agents/skills && ln -s "$PWD/skills/perf-agent-mcp" ~/.agents/skills/perf-agent-mcp
```

## Source statuses

| Status | Meaning |
|---|---|
| `build_verified` | A manifest ties the file to the build. A PC-sampled (`gpu_pc`) line is verified only by a manifest naming the capture id. |
| `user_selected` | The file is in a git checkout under the named source root and matches its committed blob. |
| `source_unverified` | The file exists but nothing ties it to the profiled build; the reason says why. |
| `ambiguous` | Several candidate files (at most 16 are listed). |
| `unresolved` | No file was found, or the file could not be read. |

## Limits

Defaults; override with `--limit Name=Value`. Sizes are bytes. Every limit must be positive (`Source.MaxContext` and `Capture.TTL` may be 0); a zero, negative or out-of-range value exits 2 and names the limit. The capture limits are in their own table below.

| Name | Default |
|---|---|
| `Model.MaxRawBytes` | 268435456 (256 MiB) |
| `Model.MaxDecompressedBytes` | 1073741824 (1 GiB) |
| `Model.MaxSamples` | 5000000 |
| `Model.MaxLocations` | 5000000 |
| `Model.MaxDepth` | 8192 |
| `Model.MaxFrames` | 50000000 |
| `Model.MaxSidecarBytes` | 268435456 (256 MiB) |
| `Model.MaxSidecarRows` | 10000000 |
| `Model.MaxSidecarExecutions` | 10000000 |
| `Model.MaxMetaBytes` (sample type and unit names) | 256 |
| `Model.MaxCommentBytes` (all comments together) | 65536 (64 KiB) |
| `MaxMessageBytes` (one JSON-RPC line) | 4194304 (4 MiB) |
| `MaxResponseBytes` (one tool result) | 1048576 (1 MiB) |
| `MaxOpenHandles` | 8 |
| `MaxRows` | 200 |
| `MaxCachedDiffs` | 4 |
| `MaxDiffRows` | 500 |
| `Explain.MaxExplanations` | 64 |
| `Explain.MaxClaimsPerExplanation` | 32 |
| `Explain.MaxClaimsTotal` | 512 |
| `Explain.MaxProseBytes` | 8192 |
| `Explain.MaxHypotheses` | 8 |
| `Explain.MaxHypothesisBytes` | 2048 |
| `Explain.MaxNextBytes` | 2048 |
| `Explain.MaxTitleBytes` (the page title) | 200 |
| `MaxPageBytes` | 33554432 (32 MiB) |
| `MaxPageNodes` | 20000 |
| `MaxAttemptsPerView` | 20 |
| `MaxAttemptViews` | 64 |
| `MaxHistoryBytes` (all attempt records) | 4194304 (4 MiB) |
| `MaxPublications` (live page publications kept) | 8 |
| `MaxRetainedBytes` (estimated memory the live page's publications keep, not a physical cap: the heap can reach about twice it; at least `MaxPageBytes`); used by the live page engine, not by the stdio server | 2147483648 (2 GiB) |
| `MaxPageClients` (live page streams at once) | 4 |
| `Source.MaxFileBytes` | 8388608 (8 MiB) |
| `Source.MaxLineBytes` | 512 |
| `Source.MaxContext` | 50 |
| `Source.MaxWalkEntries` | 200000 |
| `Source.MaxManifestBytes` | 16777216 (16 MiB) |
| `Source.GitTimeout` | 5s (a duration) |

Displayed frame names are cut at 1 KiB on pages; the node ID stays the reference. A response over `MaxResponseBytes` is refused with advice to page with a smaller `limit` or restart with a larger `--limit MaxResponseBytes=N`.

### Capture limits

Each is set with `--limit Capture.Name=Value` (`--capture-ttl` sets `Capture.TTL` too, and wins). They are checked at start-up whether or not capture is enabled. A non-positive value exits 2 (`Capture.TTL` may be 0, meaning no age limit). A value over its ceiling, or one that does not fit the limit it depends on, is refused at start-up with exit 1, naming both: `Capture.MaxProfileBytes` over `Model.MaxRawBytes`, `Capture.MaxExeclogBytes` over `Model.MaxSidecarBytes`, `Capture.CompareRows` over `MaxDiffRows`, `Capture.WindowDuration` under `Capture.MaxDuration`, `Capture.MaxStoredBytes` under 3 × `Capture.MaxProfileBytes` + `Capture.MaxExeclogBytes` + 1 MiB, and four `Capture.MaxOutputTail` over a quarter of `MaxResponseBytes`.

A limit left at its default (or set to it) follows the limit it depends on instead of being refused: `Capture.CompareRows`, `Capture.MaxProfileBytes`, `Capture.MaxExeclogBytes` and `Capture.MaxOutputTail` are lowered to fit (the tail to at most a sixteenth of `MaxResponseBytes`, and at least 1 byte), then `Capture.MaxStoredBytes` is raised to hold the largest capture and 1 MiB. Each move is logged on stderr as `capture limit fitted`. Raising `Model.MaxRawBytes` does not raise `Capture.MaxProfileBytes`: set it too.

| Name | Default | Ceiling | Meaning |
|---|---|---|---|
| `Capture.MaxDuration` | 30s | 5m | The longest `duration` |
| `Capture.MaxSampleRate` | 1000 | 10000 | The highest `sample_rate`, in Hz |
| `Capture.StartTimeout` | 15s | 5m | perf-agent's start, to its readiness line; `start_timeout` |
| `Capture.FinishTimeout` | 30s | 10m | perf-agent's finish after the capture's end; `finish_timeout` |
| `Capture.StopGrace` | 5s | 30s | How long a stopped child gets before SIGKILL; then survivors are `stuck_child` |
| `Capture.TTL` | 168h (7 days) | — | Retention's age limit for unkept captures; 0 means none |
| `Capture.KeepPerSeries` | 10 | — | Unkept captures retained per series, newest first; failures have a budget of the same size |
| `Capture.MaxKeptPerSeries` | 4 | — | `keep: true` captures per series; one more is `invalid_argument` |
| `Capture.CompareRows` | 20 | `MaxDiffRows` | Diff rows per view in a `compare_with` result |
| `Capture.MaxJobs` | 32 | — | Finished jobs kept in memory, least recently used first |
| `Capture.MaxJobBytes` | 4194304 (4 MiB) | — | Bytes of finished results kept in memory; the newest is always kept, so the bound is this plus one result. A job whose commit is undecided is never evicted |
| `Capture.WindowCaptures` | 60 | — | Captures started per rolling hour; `budget` |
| `Capture.WindowDuration` | 30m | — | Capture time per rolling hour, at least `Capture.MaxDuration`; a capture reserves its duration and is charged what it profiled; `budget` |
| `Capture.MaxOutputTail` | 2048 (2 KiB) | 65536 | Bytes of each output stream a result returns |
| `Capture.MaxProfileBytes` | 268435456 (256 MiB, `Model.MaxRawBytes`) | `Model.MaxRawBytes` | Each profile perf-agent writes; a larger one is killed while written (`too_large`) |
| `Capture.MaxExeclogBytes` | 268435456 (256 MiB, `Model.MaxSidecarBytes`) | `Model.MaxSidecarBytes` | The GPU executions log, likewise |
| `Capture.MaxStoredBytes` | 4294967296 (4 GiB) | — | The capture directory's total size; retention makes room first, then `disk` |
| `Capture.MinFreeBytes` | 1073741824 (1 GiB) | — | Free space the capture directory's filesystem must have before a capture; `disk` |

Fixed, not limits: `capture_wait` waits 30 s by default and at most 45 s; `capture_list` waits up to 10 s for the history; historical manifests are read by 2 workers with a queue of 16; up to 4096 finished job ids stay resolvable; the target is re-checked every second.

## Registering with a client

Claude Code:

```bash
claude mcp add perf-agent -- /path/to/perf-agent-mcp --profiles /path/to/profiles --output /path/to/pages
```

To let the agent capture, add `--perf-agent /path/to/perf-agent` (the setcap'd binary, not in `/tmp`), and `--gpu-shim` for GPU captures.

Any other MCP client: run `/path/to/perf-agent-mcp --profiles DIR --output DIR` as a stdio server. The server answers protocol versions 2025-11-25 and 2025-06-18 and answers `server/discover` with method-not-found, so clients fall back to `initialize`.

## Evals

`scripts/mcp-evals.sh` replays a fixed set of questions through a real agent over frozen synthetic fixtures. It needs the `claude` CLI, `go` and `jq`, and it needs a model, so it costs tokens and is run by hand, never in CI. No capture leaves the machine.

The script builds `perf-agent-mcp` into a temporary directory, writes the fixtures (`cpu.pb.gz`, `cpu-new.pb.gz`, `gpu.pb.gz` and its `gpu.executions` log), and runs `claude -p` once per question in `internal/perfmcp/evals/questions.json` with a generated `--mcp-config` and `--strict-mcp-config`, so your own Claude MCP configuration is neither loaded nor changed. It then checks the rendered pages: each exists, its header counts at least one checked figure, each expected value sits inside a checked claim, and no forbidden text (for example reading a sampled share as GPU time) appears. Unchecked-number counts are printed per page.

Each question has `id`, `prompt`, `profile`, an optional `execlog`, and `expect` (`page`, `checked_values`, `require`, `forbid`). The checks run as `TestEvalPages` with `PERF_AGENT_MCP_EVAL_DIR` set to the scratch directory; without it both eval tests skip.
