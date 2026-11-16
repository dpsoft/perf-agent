# The serve binary (M2c-serve) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A second, long-lived binary, `perf-agent-mcp-serve`, gives the human layer one loopback server. It serves the M2b live page and speaks MCP over Streamable HTTP behind a bearer token, so several Claude Code and Codex sessions share one engine and one page. The user's clicks reach agents through an inbox they read with `page_state`. The plain stdio `perf-agent-mcp` goes back to linking no networking.

**Architecture:**
- **`internal/perfmcp`** loses every network import. It keeps:
  - the page engine: publications, the snapshot, the page tools;
  - a read-only `PageView` accessor that the HTTP side reads;
  - a state bus that joins the engine's state and the inbox's state into one envelope;
  - the inbox and `page_state`;
  - the engine slot, which now records its holder and bounds a tool call's wait.
- **`internal/perfpage`** (new) holds the page's HTTP routes, moved from `liveroutes.go`, and the inbox handler. Only the serve binary links it.
- **`internal/mcphttp`** (new) is the Streamable HTTP binding over `mcpstdio`'s exported per-message API. It holds the sessions, the bounded ingress, the admission classes, the in-flight table that scopes cancellation, and the deadlines.
- **`internal/livehttp`** gains `Front`, `MCPGuard` and `InboxGuard`.
- **`internal/mcpflags`** (new) holds the flag parsing that both binaries share.
- **`cmd/perf-agent-mcp-serve`** (new) wires it all together. It owns the runtime directory, the lock, the tokens, the one listener and the shutdown order. It also hosts the capture jobs, with page publication of their results.

**Tech Stack:** Go 1.26 standard library only (`net/http`, `net/netip`, `crypto/subtle`, `crypto/rand`, `syscall.Flock`, `sync/atomic`, `os.Root`). Tests may use `golang.org/x/sys/unix`, as the existing tests do. No new module.

**Spec:** `docs/superpowers/specs/2026-11-15-local-server-design.md`. Its §9 orders the work and §10 holds the rulings SRV-R1..R7. The §8 defaults hold: a fixed default port with `--port`, and `MaxOpenHandles` 32 in serve and 8 in stdio.

**The spec predates capture.** Capture as shipped is defined by:
- `docs/superpowers/specs/2026-11-15-capture-design.md`;
- the rulings CAP-R17..R90 in `<capture-worktree>/.superpowers/sdd/2026-11-15-capture-plan/progress.md`;
- `docs/mcp.md`;
- the code in `internal/perfmcp`, `internal/capture`, `internal/mcpstdio` and `cmd/perf-agent-mcp`.

Where the serve spec and shipped capture differ, this plan follows capture as shipped. The next section lists each such case.

**Base:** branch `ai-mcp-m2c-serve` at 4aea4a60, the head of `ai-mcp-m2c-capture` (draft PR #265). That PR stacks on `ai-mcp-m2b` (draft PR #264). The executor's ledger is `.superpowers/sdd/2026-11-16-serve-plan/progress.md` in this worktree, git-ignored as in the capture worktree.

## Spec reconciliation

Seventeen places where the serve spec assumed something about capture that shipped differently. In each row: what the spec says, what shipped, and what this plan does.

| # | Spec text | Shipped | Plan |
|---|---|---|---|
| 1 | §2, §9, SRV-R5: the `.owner` writer kinds and the `captures-serve` default are to be built, with an error "naming the owner and the other default" | Built in capture. `.owner` holds `{kind, format: 1}` and is written no-replace by `openCaptureDir`. `CaptureConfig.Owner` exists, and the default is `captures-serve` for serve. `owner_mismatch` is synchronous once cached, otherwise async. Its text names the kinds and says "give this server its own directory with --captures" | Serve passes `Owner: "serve"` and no `Dir`, and builds nothing new. Task 13 adds one binary-level test; the docs name both defaults |
| 2 | §2, §D7: the job takes the lease "before any filesystem side effect", so a cross-process `busy` job changed nothing | `<captures>` (0700) and `.owner` are created **before** the lease, because `.lock` lives inside the directory | Task 13's cross-process test allows the directory and `.owner` on first use. It asserts that no series, `.seq`, intent or staging appears |
| 3 | §2, §D7: "a restarted serve refuses capture, naming the reaper's pid … until the reaper has reaped both children" | `capture` is accepted, and the job fails with `busy` naming the holder. `retry_after` is min(remaining, 5 s) (CAP-R89). A wait by capture id whose intent is on disk answers `in_progress` with `holder` (CAP-R84). A skipped start pass is re-armed (CAP-R87). `Run` waits ≤ 1 s for the reaper (CAP-R79) | Task 13's crash test expects these shapes: `accepted`, then a `busy` job failure, `in_progress` naming the dead server's reaper, and finally `interrupted`, retryable |
| 4 | §D1.2: stop order is listener, hub, kill captures, 1 s grace, files | `KillCaptures` must run **first** on every signal path. It never signals the reaper, only closes its command pipe (CAP-R20), and `Close` then skips its job wait. Shutdown log lines are bounded at 100 ms | Task 10's shutdown order: `KillCaptures`, hub `Close`, `http.Server.Shutdown` (1 s), transport `Close`, perfmcp `Close`, then the files are removed. Logs on that path go through `logBounded` |
| 5 | §D2.2, §D2.5, SRV-R7: jobs outlive sessions | A stdio session's `OnClose` (EOF or a dead client) cancels every job (CAP-R34) | `mcpstdio.Handle` has no session-end hook, and `mcphttp` never calls `OnClose`. Only `Close` and `KillCaptures` end jobs (Tasks 4 and 6) |
| 6 | §D2.4, §D2.5, §D7: a job's engine wait "is bounded by `MaxEngineWait` like any caller" | The job's engine sections (`engineSection`, `done`, maintenance) wait with the job's or the server's context. Only a cancel or shutdown ends that wait | Kept as shipped. `MaxEngineWait` bounds tool calls only. A Go channel queues blocked senders in FIFO order, so a job is not starved. Task 12 tests that a derivation waits behind tools for longer than `MaxEngineWait` and still ends `ready` |
| 7 | §D2.4: the engine slot is new, and `tools()` wraps handlers | It exists (`engine.go`). `acquire` tries a free slot first (P-1). It wraps the nine M2 tools and `page_*`, never the capture tools | Task 5 only adds the holder record and the `MaxEngineWait` bound |
| 8 | §D2.5: `capture` answers `{job, status:"accepted", eta}`, a synchronous `invalid_argument`, or an in-process `busy` | Failures are `isError` results whose text is `{"error":{code, message, retryable, retry_after, next, …}}`. 28 codes, and the synchronous ones include `disabled`, cached `owner_mismatch`, `budget` and `closing` | Serve reuses these unchanged. The transport's own refusals are not capture codes: a cap refusal is a JSON-RPC error `-32000` naming the cap (Task 7), and `MaxEngineWait` gives M2's plain-text `isError` (Task 5) |
| 9 | §D2.5, §D7 name jobs and captures loosely | A job id is `j<N>-<4 hex>`. A capture id is `<series>/NNN`: it appears as `capture_id` in states, cancels and errors, and as `capture` in results and list rows, whose `id` is `NNN`. A bare `NNN` given to wait or cancel is `invalid_argument` (CAP-R86) | The multi-session tests use these forms. Publication titles use `<series>/NNN-<label>` (Task 12) |
| 10 | §D4: `page_state` counts "are strings (the M2 wire rule)" | The M2 rule now has an exception: the capture tools' integers are JSON numbers (CAP-R90), listed in `docs/mcp.md` line 43 | `page_state` keeps decimal strings: none of its counts is passed back. Its `limit` argument is a JSON integer. Task 14 names `page_state` in that docs line |
| 11 | §D2.5: `capture_wait` is bounded at 45 s | Its deadline counts from **receipt**, through `mcpstdio.Received` (CAP-R74) | `mcphttp` stamps `mcpstdio.WithReceived` once a body is parsed, so time queued for a slot counts against the budget (Tasks 4 and 6) |
| 12 | §5: extract `dispatch` into `(*Server).Handle(ctx, *Session, []byte) []byte`; `Serve` "uses it with one session" | mcpstdio is a reader, a dispatcher and a bounded writer per session. A Control tool runs inline after initialize. `Progress` and `Received` read the context | Task 4 exports `Session` (protocol state only), `Parse`/`Request` and `Handle`. The stdio session holds a `*Session` and calls the same answer path, and its bytes are unchanged |
| 13 | §1.3, SRV-R6: no progress | stdio sends `notifications/progress` to a `capture_wait` that carries a `progressToken` | `Handle` installs no progress target, so `Progress` returns false over HTTP. Task 4 tests it, and Task 6 checks that no SSE response ever goes out |
| 14 | §9: the shared flags are `--perf-agent`, `--captures`, `--capture-command` and `--gpu-shim` | There is also `--capture-ttl`, and a no-effect warning when capture flags come without `--perf-agent`. `--captures` is checked at start-up, and only with `--perf-agent`. It may not be the profiles root, including through a symlink (CAP-R70, R78, R78b) | `internal/mcpflags` moves all of it unchanged (Task 1) |
| 15 | The capture plan's "Seams for serve": `capturePublish`, the `show` argument, the `publication` field | Only `captureProtect` shipped. There is no `capturePublish`, no `show` and no `publication`, and the recompute recipe has no page step | Task 12 builds all of them |
| 16 | §D2.5, CAP-R17: serve's recompute also names `page_explain` with `explanations: []` | The recipe is `{steps:[{tool, arguments, bind}], baseline}`, with `${<bind>.handle}` substitution | With `Config.Page` set, Task 12 appends the `page_explain` step (and `page_diff` for a compare). A test executes the steps, which checks them against the tools' schemas |
| 17 | §9, §D3: "`capture_list` attribution reuses the label" | The manifest has no client field | `by` lives in memory only: on the job (the running row of `capture_list`) and on the publication. Manifests do not change (Task 12) |

## Global Constraints

- **Go 1.26** (`go 1.26.0` in go.mod). **Invoke `modern-go-guidelines:use-modern-go` before writing any Go**, not as a cleanup pass.
- **Every task ends green, with no forward dependency.** A task declares every type, helper, scan rule and test expectation that its own code first uses. Each task's verification step runs, besides its own tests, the commands below, covering each listed package once it exists:

  ```bash
  go test -race ./internal/... ./cmd/perf-agent-mcp/ ./cmd/perf-agent-mcp-serve/
  GOOS=darwin go build ./internal/... ./cmd/perf-agent-mcp/ ./cmd/perf-agent-mcp-serve/
  ```

  No existing guard is weakened to get there. A test that moves keeps its name, or the task names the test that replaces it.
- **Lint** with golangci-lint **v2.11.4**: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`. A missing binary exits 127, which is not a pass.
- **cgo environment.** Commands that build the perf-agent root package (`./...`) need these variables. The MCP packages do not import blazesym.

  ```bash
  CGO_CFLAGS=-I<blazesym>/capi/include
  CGO_LDFLAGS=-L<blazesym-lib-dir>
  ```
- **Build on top [user].** Pre-existing perf-agent code does not change:
  - not `main.go`, `telemetry.go`, `perfagent/`, `gpu/`, `cmd/flamegraph` or `internal/profilemodel`;
  - not the semantics of `explain` or `sourceroot`;
  - not the static-page goldens (`RenderHTML`, `RenderFragment`, `WriteDiffPage`, the explained page outside its script), nor `render_flamegraph`'s bytes.

  `git diff --stat ai-mcp-m2c-capture -- main.go telemetry.go perfagent gpu cmd/flamegraph internal/profilemodel` must be empty at every task. The AI layer may change: `internal/perfmcp`, `internal/mcpstdio`, `internal/livehttp`, `internal/capture`, `cmd/perf-agent-mcp`, the live-only assets (`live.js`, `livepage.html`, `live.css`), the guide, the skill and the docs.
- **Loopback only [user, M2b-R7, decision 8.2].**
  - The host is always `127.0.0.1`. `--port` is the only address flag; there is no `--addr`, `--host` or `--listen`.
  - There is no remote use, and the docs never describe SSH forwarding.
  - `livehttp.Listen` stays the one listening call. Only `cmd/perf-agent-mcp-serve` calls it.
  - No client networking anywhere: no dial, no resolver and no HTTP client in non-test code.
- **Secrets.**
  - The MCP token and the page token are each 32 random bytes in base64url, rotated at every start.
  - They are never logged and never put in a URL that a client config holds. The MCP token travels only in `Authorization: Bearer`.
  - The page URL goes to stdout once, and to `page-url` (0600).
  - Both tokens compare in constant time.
- **Wire rules.**
  - M2's decimal-string integers hold everywhere except the capture tools' listed integers (CAP-R90). `page_state` uses strings.
  - Errors repeat agent input only through `echo`.
  - Every `/mcp` answer is `application/json` or an empty 202, 204 or error body. The server never sends SSE, never sends a progress notification over HTTP, and never sends a JSON-RPC request (SRV-R6).
- **Dependencies.**
  - The non-std modules of both binaries stay ⊆ {`github.com/dpsoft/perf-agent`, `github.com/google/pprof`}. There is no `x/sys` in non-test code.
  - `TestTheServerLinksNoProfiler` holds for both binaries.
  - The `os/exec`, `syscall.Exec` and `prctl` rules of the capture scan are unchanged.
  - Every binary that embeds `perfmcp` calls `capture.RunInternal()` first in `main`.
- **No AI attribution, no session URLs and no issue references** in comments, commits or PR text (POLICY.md). Comments say what the code does and why. A spec section number (§D2.4) is fine in a test's doc comment; a ruling ID is not.
- **Tests.**
  - Gates check capabilities (the file xattr and the process status), never `Getuid`.
  - A test leaves no process, cgroup, socket file or runtime file behind.
  - Children are stopped by recorded PID or process group. **Never `pkill -f`.**
  - Chrome tests skip when Chrome is absent, as in M2b.
- **Real clients.**
  - Never run `claude mcp add` or edit a client's own configuration.
  - Claude Code is driven only through `claude -p --mcp-config <scratch json> --strict-mcp-config`.
  - Codex (codex-cli is installed) is driven through `codex exec` with `-c mcp_servers.perf-agent-live.*` overrides. If those cannot express the header helper, or cannot keep the user's own servers out, the Codex smoke is a manual step, recorded as such.
- **Commits.**
  - Package prefixes: `mcpflags:`, `perfmcp:`, `perfpage:`, `livehttp:`, `mcpstdio:`, `mcphttp:`, `flamegraph:`, `perf-agent-mcp:`, `perf-agent-mcp-serve:`, `docs:`.
  - No Co-Authored-By, and never commit `CLAUDE.md`.
  - Push only in Task 15, as a draft PR. **Never merge.**
- **Model routing.** Tasks 2, 6, 7, 12 and 13 are load-bearing: the package split, the transport and capture hosting. Route them, and their reviews, to opus. Every task's commit range also goes to the second reviewer through Workforce; message bodies with code go through a quoted heredoc file.

## Review Focus

1. **A restarted server, met by a client that kept its old session and token.** Expected:
   - an old `Mcp-Session-Id` gets `404`, never `400`, so the client re-initializes;
   - an old bearer token gets `401` with no `WWW-Authenticate`, so Claude Code re-runs its headers helper once.

   Test in Task 6 (`TestAnOldSessionIs404AndAnOldTokenIs401AfterARestart`).
2. **`Accept` and `Content-Type` headers as real clients write them:** `application/json;q=0.9, text/event-stream`, `Application/JSON; charset=utf-8`, `*/*`, and a repeated `Accept` header. Expected: parsed as media ranges. Wildcards and parameters are accepted; a range with `q=0` is not acceptable. A POST whose `Accept` covers both types passes. Test in Task 3 (`TestAcceptAndContentTypeAreParsedAsMediaRanges`).
3. **`perf-agent-mcp-serve token` when no server runs**, including a stale `mcp-token` left by a SIGKILLed server. Expected: exit 1, the reason on stderr, and **nothing on stdout**, so a client sends no stale header and shows "failed to connect". Test in Task 10 (`TestTokenWithoutARunningServerPrintsNothing`).
4. **The default port is taken**, by another program or by a stale `serve.json` from a killed server. Expected:
   - a busy port exits 1, naming the port and `--port`;
   - a stale `serve.json` with the lock free never blocks a start: it is replaced.

   Tests in Task 10 (`TestABusyPortNamesItAndTheFlag`, `TestAStaleServeJSONDoesNotBlockStart`).
5. **A `page_state` cursor kept across a serve restart.** Ask ids would restart at `i1`, and an old cursor `i40` would hide every new ask. Expected: ids carry a per-process nonce (`i7-3f9a`). A cursor from another process returns every retained ask, with a note saying so. Test in Task 8 (`TestACursorFromAnEarlierServerReturnsEveryRetainedAsk`).

## Builder's calls settled here

| Call | Decision | Reason |
|---|---|---|
| Default port (§8.1) | **7341** (`DefaultPort`). It sits in the 7284–7364 block, unassigned in the IANA registry as shipped in `/etc/services` (registry date 2021-01-19). The implementer re-checks the current IANA registry and records the check in the ledger | A fixed URL in client configs (§D2.3) |
| `MaxOpenHandles` (§8.2) | 32 in serve, set by `mcpflags.Serve` before `--limit`; 8 in stdio | Several agents share one store |
| Transport limits | `Limits.Serve ServeLimits`: `MaxSessions` 32, `MaxIngressReaders` 16, `MaxIngressBytes` 8 MiB, `IngressBodyTimeout` 2 s, `MaxControlInFlight` 32, `MaxPageStateInFlight` 4, `MaxCalls` 16, `MaxCallsPerSession` 4, `MaxRequestBytes` 16 MiB, `MaxInboxEntries` 32. `--limit Serve.X=…` sets them. stdio refuses `Serve.*` names, saying they are perf-agent-mcp-serve's | `--limit` reflects over `Limits`. A flat field would appear in stdio with no effect |
| `MaxEngineWait` | A flat `Limits` field, 45 s by default and at most 55 s. It applies to tool calls in both binaries; in stdio it is reachable only behind its own capture's derivations | Under Codex's 60 s, and with no behaviour change in practice for stdio |
| Validation | Refused at start-up, naming both fields: `MaxCallsPerSession > MaxCalls`, `MaxIngressBytes < MaxMessageBytes`, `MaxRequestBytes < MaxMessageBytes`, `IngressBodyTimeout > 30s`, `MaxEngineWait > 55s` | Each otherwise makes a cap unreachable or a call unanswerable |
| Body read deadline | `IngressBodyTimeout` (2 s), not §D2.5's 30 s | SRV-R4c came later and is the stricter rule |
| Check order on `/mcp` | Front's Host check, then MCPGuard: Origin or `Sec-Fetch-*` → 403; a method other than POST or DELETE → 405 (`Allow: POST, DELETE`); bearer → 401; for POST, `Accept` → 400, `Content-Type` → 400, `Content-Length` missing or chunked → 411, over `MaxMessageBytes` → 413 | §4 gives GET 405 without auth. A refusal for a browser comes first |
| Missing `MCP-Protocol-Version` | The session's negotiated version | §D2.1 |
| `initialize` | Always mints a new session; an `Mcp-Session-Id` header on it is ignored. The session id is 32 random bytes, base64url | Re-initialize after a 404 |
| A request without a session | Anything but `initialize` without `Mcp-Session-Id` gets 400 with the JSON-RPC error `-32600` "no session: send initialize first", a notification included. This is the 2026-07-28 probe answer | §D2.1 fallback |
| A cancelled request's POST | It is answered with whatever its handler returns under a cancelled context (an `isError` text for a tool). The client has already stopped waiting | HTTP must answer the POST; mcpstdio's own answer is reused |
| Client label | `clientInfo.name`: at most 64 bytes of printable ASCII (0x20–0x7e); otherwise, or when absent, `client` | §D2.2 |
| Engine holder text | `perf-agent-mcp is busy with <tool> from <label> (running <N>s); call again`. A capture job holds as `capture <capture id or job id>`, and maintenance as `capture retention`; an empty label reads `another session` | §D2.4 |
| Test seam for the engine | `Config.HoldEngine func(tool string)`, called with the engine held, before a tool's handler; nil in production | The cmd tests must hold the engine across processes |
| Test seam for the reaper | `CaptureConfig.ReaperPause string`, which sets the unexported `reaperPause`; tests only | Task 13's crash tests run the serve binary |
| perfpage tests | They drive a real `perfmcp.Server` through `Serve` over an `io.Pipe`, with a small JSON-RPC client in `harness_test.go`. They build fixtures from `pmfixture` | No import cycle and no unexported access |
| Tests that read engine internals | They stay in perfmcp and are rewritten against `PageView` | The routes move; the computations do not |
| Ask ids | `i<N>-<4 hex nonce>` | Review Focus 5 |
| `page_state` arguments | `since` (an ask id) and `limit` (default 8, 1..`MaxInboxEntries`) | §D4 |
| Inbox part of the envelope | `{"rev","asks","unread","read_by"}`. `read_by` is the newest ask's reader, so the page can show "read by codex" | §D4's page side, with the envelope of SRV-R3 |
| Attempt attribution (`attempt.by`) | **Deferred.** Publications and steering record `by`; attempts do not | History rows are built by `flamegraph.ExplainedLiveHistory` from `explain.RejectedAttempt`, and the export uses them too. Changing that risks the static export bytes for a display nicety |
| Capture publications | One facts-only `page_explain` publication of the **first** mode (cpu, offcpu, gpu order). With `compare_with`, add one `page_diff` of that mode's default views. Fields: `publication` and `diff_publication`. `show` (default true) makes the diff current when there is one, else the facts page | The capture design (§9) names one publication |
| Publication title | `<series>/NNN-<label> · <comm> (<pid>) · <mode> · <profiled_duration>`, cut to `Explain.MaxTitleBytes` | The capture design's example |
| A publication failure | `derived: "failed"`, with `derive_error` starting `publication: `. The recipe includes the page step. Files are never touched | §D2.5 |
| Disabled texts | `Config.Command` (default `perf-agent-mcp`) names the binary in `captureDisabledText` and in the page tools' texts | Otherwise serve's disabled message names the wrong binary |
| Runtime files | Written through `os.Root` on the runtime directory as a temporary file plus rename, 0600. `serve.lock` is never removed. `serve.json` integers are JSON numbers (it is not MCP wire) | Readers never see a half-written token |
| The `token` subcommand | Prints only while the lock is held (a `LOCK_SH|LOCK_NB` probe fails). Otherwise exit 1 | Review Focus 3 |

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/mcpflags/{flags.go,limits.go,flags_test.go}` | the shared flags, `--limit`, `--captures` checks | 1 |
| `internal/perfmcp/{pageview.go,live.go,config.go,server.go,tool_page.go,tool_page_diff.go,limits.go}`, `internal/perfpage/{doc.go,routes.go,hub.go,harness_test.go,routes_test.go,chrome_test.go}` | the split: no networking in perfmcp; page routes in perfpage | 2 |
| `internal/livehttp/{front.go,mcpguard.go,inboxguard.go,guard.go}` | front door, MCP guard, inbox guard | 3 |
| `internal/mcpstdio/{handle.go,session.go,protocol.go,server.go}` | the exported per-message API | 4 |
| `internal/perfmcp/{engine.go,limits.go,config.go,live.go,tool_page*.go}`, `internal/mcpflags/limits.go` | engine holder and wait bound, serve limits, `by` | 5 |
| `internal/mcphttp/{doc.go,transport.go,session.go,inflight.go,deadline.go}` | endpoint, sessions, cancellation, deadlines | 6 |
| `internal/mcphttp/{ingress.go,admit.go}` | ingress budget, classes and caps | 7 |
| `internal/perfmcp/{statebus.go,inbox.go,tool_page_state.go}`, `internal/perfpage/inbox.go` | the envelope, inbox, `page_state` | 8 |
| `internal/flamegraph/{live.js,livepage.html,live.css,liveassets_test.go}` | page reports, chip, toast, inbox count | 9 |
| `cmd/perf-agent-mcp-serve/{main.go,serve.go,runtimedir.go,main_test.go,runtimedir_test.go}` | the binary | 10 |
| `cmd/perf-agent-mcp-serve/{netscan_test.go,sockets_linux_test.go,link_test.go,chrome_test.go}`, `cmd/perf-agent-mcp/main_test.go` | the network promises, moved; end-to-end page | 11 |
| `internal/perfmcp/{capture_publish.go,capture_compare.go,capture_run.go,tool_capture.go,tool_capture_list.go,config.go}` | capture hosting: publication, `show`, protect, recipe, `by` | 12 |
| `cmd/perf-agent-mcp-serve/capture_linux_test.go` | serve-level capture and crash tests | 13 |
| `docs/mcp.md`, `internal/perfmcp/guide.md`, `guide.go`, `skills/perf-agent-mcp/SKILL.md`, `CHANGELOG.md` | docs | 14 |

---

### Task 1: The shared flags, `internal/mcpflags`

**Files:**
- Create: `internal/mcpflags/flags.go`, `limits.go`, `flags_test.go`
- Modify: `cmd/perf-agent-mcp/main.go` (parse through mcpflags), `cmd/perf-agent-mcp/main_test.go` (`TestEveryLimitFieldCanBeSetByFlag` moves to mcpflags)

**Interfaces:**
- Consumes: `perfmcp.Config`, `perfmcp.CaptureConfig`, `perfmcp.DefaultLimits`.
- Produces:
  - `type Kind string`, with `const Stdio Kind = "stdio"` and `const Serve Kind = "serve"`.
  - `func Register(fs *flag.FlagSet, kind Kind) *Values` registers every flag the stdio command has today: `--profiles`, `--source`, `--source-manifest`, `--source-git`, `--output`, `--limit`, `--perf-agent`, `--captures`, `--capture-command`, `--capture-ttl`, `--gpu-shim` and `--version`. The usage texts are byte-identical for `Stdio`. For `Serve`, the `--captures` default reads `<profiles>/captures-serve`.
  - `func (v *Values) Version() bool`.
  - `func (v *Values) Config(fs *flag.FlagSet, stderr io.Writer, version string) (perfmcp.Config, error)`:
    - it runs today's checks in today's order, and warns once about capture flags given without `--perf-agent`;
    - a usage error is a `*UsageError` (exit 2), whose text is prefixed `fs.Name() + ": "`;
    - `Stdio` sets `Capture.Owner` to `"stdio"`; `Serve` sets it to `"serve"` and `MaxOpenHandles` to 32 before applying `--limit`.
  - `type UsageError struct{ Msg string }`.
  - Exported from `limits.go`: `SetLimit(l *perfmcp.Limits, kv string) error`, `CheckLimits(l *perfmcp.Limits) error`, `LimitFields(l *perfmcp.Limits) map[string]reflect.Value`, `CheckCapturesDir(profiles, dir string) (string, error)`.

- [ ] **Step 0: Check the base**

Run: `git log --oneline -1 && go test ./cmd/perf-agent-mcp -run TestRunRegistersTheProfileAndCaptureTools`
Expected: HEAD is 4aea4a60 (or the commit that adds this plan), and the test passes: 13 tools, no `page_*`.

- [ ] **Step 1: Write the failing tests**

```go
func TestStdioParsesAsBefore(t *testing.T)                 // table: the arg sets of main_test's TestRun* cases → Config equals the one main built before (roots, sources, manifests, git, output, limits, Capture{PerfAgent, Dir, GPUShim, Owner "stdio", Commands}); usage errors and stderr text byte-identical
func TestServeDefaultsOpenHandlesTo32AndOwnerServe(t *testing.T)
func TestServeLimitOverridesTheOpenHandlesDefault(t *testing.T) // --limit MaxOpenHandles=8 → 8
func TestCaptureFlagsWithoutPerfAgentWarnOnce(t *testing.T)  // "--captures, --gpu-shim have no effect without --perf-agent" once, prefixed by the program name
func TestCapturesDirRules(t *testing.T)                    // moved cases: outside, the root itself, the root through a symlink, a file, an escaping symlink → errors; "caps" and an absolute path inside → relative name
func TestEveryLimitFieldCanBeSetByFlag(t *testing.T)       // moved from cmd/perf-agent-mcp
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcpflags/`
Expected: FAIL (no package).

- [ ] **Step 3: Implement `internal/mcpflags` and make `runContext` use it.** The checks keep their exact wording, so every stdio test passes unchanged. Keep `captureFlags` as the list of flags that only matter with `--perf-agent`.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/mcpflags/ ./cmd/perf-agent-mcp/`, then the green check.
Expected: PASS. Every `TestRun*` in `cmd/perf-agent-mcp` passes unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpflags cmd/perf-agent-mcp/main.go cmd/perf-agent-mcp/main_test.go
git commit -m "mcpflags: the flags both perf-agent-mcp binaries share, moved out of the stdio command"
```

### Task 2: perfmcp stops networking; the page routes move to `internal/perfpage` (opus)

**Files:**
- Create:
  - `internal/perfmcp/pageview.go`, `pageview_test.go`;
  - `internal/perfpage/doc.go`, `routes.go`, `hub.go`, `harness_test.go`, `routes_test.go`, `routes_unix_test.go`, `routes_other_test.go`, `chrome_test.go`, `chrome_linux_test.go`, `chrome_other_test.go`.
- Delete: `internal/perfmcp/liveroutes.go`, `liveroutes_test.go`, `liveroutes_unix_test.go`, `liveroutes_other_test.go`, `livechrome_test.go`, `livechrome_linux_test.go`, `livechrome_other_test.go`.
- Modify:
  - `internal/perfmcp/`: `config.go` (`Live bool` → `Page *PageConfig`), `live.go`, `server.go`, `tool_page.go`, `tool_page_diff.go`, `limits.go` (`validate(page bool)`), `live_test.go`, and every test that sets `Live: true`;
  - `cmd/perf-agent-mcp/`: `main_test.go`, `netscan_test.go`.

**Interfaces:**
- Consumes: the existing `live`, `publication`, `liveSnapshot`, `stateOf`, `commit`.
- Produces:
  - **`pageview.go`.**
    - `type PageConfig struct { URL string; Notify func(state []byte); Viewers func() int }`. `URL` is what `page_open` returns. `Notify` receives every new state and must never block. `Viewers` counts the open page streams. A nil func is a no-op, or 0.
    - `type PageView struct{ … }`: an immutable view of one snapshot.
    - `func (s *Server) PageSnapshot() PageView`. Its methods:
      - `State() []byte`;
      - `Model(pub string) ([]byte, error)`;
      - `History(pub string) ([]byte, error)`;
      - `Frame(pub, node string) ([]byte, error)`;
      - `Focus(pub, node string) ([]byte, error)`;
      - `Export(pub string) ([]byte, error)`.

      Each computes exactly what the route computed. A zero view, on a server without `Page`, answers `ErrPageNotFound` everywhere.
    - `var ErrPageNotFound = errors.New("no such publication or node; the page will refresh")`. It also covers `profilemodel.ErrUnknownNode` and an ID over 64 bytes.
    - `type PageLimitError struct{ Msg string }`: "cannot be built within the page limits", answered with 422. Any other error is 500.
  - **`live.go`.**
    - It drops `start`, `srv`, `ln`, `url`, the `liveAddr`, `liveShutdown`, `liveKeepAlive` and `liveWriteTimeout` constants, the `livehttp` import and the `net/http` import.
    - `hub` becomes `notify func([]byte)`, and `commit` calls it.
    - `close()` only marks the page closed.
    - `ensureLive()` returns `Config.Page.URL` and commits on first use; after `Close` it returns `errLiveClosed`.
    - Ownership comments say "engine holder".
  - **`tool_page.go`.** `viewers()` reads `PageConfig.Viewers`.
  - **`internal/perfpage`.**
    - `func Routes(srv *perfmcp.Server, events http.Handler) http.Handler`: the M2b routes below Guard's prefix, unchanged in status codes, headers and bytes. `cleanPathsOnly` and the `heavy` slots (2) move here.
    - `func NewHub(lim perfmcp.Limits) *livehttp.Hub`: `MaxPageClients` streams, keep-alive 15 s, write 10 s.
  - **perfpage's test harness** (`harness_test.go`): `func newPage(t *testing.T, edit func(*perfmcp.Config)) *page` builds the hub, the server with `Page`, `livehttp.Guard(tok, Routes(…))` on an `httptest.Server`, and an MCP client over `io.Pipe`. Its methods are `call(tool string, args any) string`, `openAndPublish() (pub string)` and `diffPublished() (pub string)`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/perfmcp/pageview_test.go: the engine-side tests that read internals, rewritten against PageView
func TestPageViewServesWhatTheRoutesDid(t *testing.T)   // explained and diff publications: Model == p.model; History == h.json or emptyHistory; Frame/Focus == the flamegraph calls; Export == ExplainedPage/WriteDiffPageBounded bytes
func TestPageViewNotFoundAndLimits(t *testing.T)        // unknown pub, 65-byte id, a diff union id outside the index → ErrPageNotFound; a frame over MaxPageBytes and an export over it → *PageLimitError naming MaxPageBytes
func TestNotifyGetsEveryCommittedState(t *testing.T)    // Notify called once per commit with the snapshot's state; seq increasing; nil Notify is safe
func TestPageOpenReturnsTheConfiguredURL(t *testing.T)
func TestEnsureLiveAfterCloseRefuses(t *testing.T)      // replaces TestCloseDuringEnsureLive
func TestNoPageToolsWithoutPage(t *testing.T)           // renamed from TestNoLiveWithoutTheFlag
// kept in perfmcp, moved onto PageView: TestADiffPinsBothSides, TestDiffRoutesRaceNothing, TestExportReadsOnlyTheSnapshot, TestADiffIsChargedForItsDerivedUnits, TestAnExportThatFailsOtherwiseSaysSo
// internal/perfpage/routes_test.go: the HTTP tests, moved with their names and assertions
// TestEveryRouteIsGuarded, TestOnlyCleanPathsAreServed, TestStateAndModelServeThePublication, TestADiffPublicationHasAnEmptyHistory,
// TestFrameAndFocusAreComputedByTheEngine, TestExportIsTheStaticPageAsADownload, TestTheExportShowsOnlyWhatTheHistoryKeeps,
// TestAnExportOverTheBudgetIs422, TestASecondTabAndAReconnectSeeTheCurrentState, TestAStuckTabNeverDelaysATool,
// TestAnotherServersTokenIs404, TestHeavyRoutesAreBounded, TestAFrameIsBoundedByMaxPageBytes, TestLivePageRacesNothing
func TestNoGETChangesServerState(t *testing.T)          // perfpage version: State() bytes and a page_open + top result are byte-equal before and after every route's GET
// internal/perfpage/chrome_test.go: TestADiffNodeOutsideTheModelIsShownInChrome, TestSixTabsAllFollowThePageInChrome, TestTheLivePageWordsRejectionsForAnyOrderInChrome, TestThePageChromeFitsThePublicationInChrome
// cmd/perf-agent-mcp/main_test.go
func TestTheServerLinksNoNetworking(t *testing.T)       // go list -deps .: none of net, net/http, crypto/tls, net/textproto, log/syslog; none of internal/livehttp, internal/mcphttp, internal/perfpage; net/url and net/netip allowed
func TestPerfmcpImportsNoNetworking(t *testing.T)       // go list -deps ../../internal/perfmcp: the same denylist
```

The stdio network tests that need a listener leave stdio here: `TestLivePageListensOnLoopback`, `TestRunServesTheLivePage`, `TestTheServerOpensNoConnection` (nm), `TestEverySocketIsLoopback`, and in perfmcp `TestTheLiveURLIsLoopbackWithAToken`, `TestCloseStopsTheListener` and `TestCloseIsBoundedByStuckClients`. Their serve versions land in Tasks 10 and 11, under the names given there. In `netscan_test.go`:
- `TestNoClientNetworkingInTheServer` drops `internal/livehttp` from its coverage list;
- it now asserts `got.listens` is empty, since stdio links no listener;
- `liveEnv` and `withLive` are removed from `main_test.go`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ ./internal/perfpage/ ./cmd/perf-agent-mcp/`
Expected: FAIL (no `PageView`, no `perfpage`; the link tests fail because perfmcp imports `net/http`).

- [ ] **Step 3: Implement the split.** Move the route bodies into `PageView` methods (the computation) and `perfpage.Routes` (the status mapping: `ErrPageNotFound` → 404, `*PageLimitError` → 422, anything else → 500 for an export and 422 otherwise, as before). Export's `Content-Security-Policy: livehttp.ExportCSP` and `Content-Disposition` stay in perfpage.

- [ ] **Step 4: Run them to verify they pass**

Run: the green check, plus `go test -race -run 'Links|Imports' -v ./cmd/perf-agent-mcp/`
Expected: PASS. The static goldens pass unchanged, and `TestNoClientNetworkingInTheServer` passes with no listener in stdio.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp internal/perfpage cmd/perf-agent-mcp
git commit -m "perfmcp, perfpage: the page routes move to perfpage, and the stdio binary links no networking again"
```

### Task 3: The front door and two guards (`internal/livehttp`)

**Files:**
- Create: `internal/livehttp/front.go`, `mcpguard.go`, `inboxguard.go`, `front_test.go`, `mcpguard_test.go`, `inboxguard_test.go`
- Modify: `internal/livehttp/guard.go` (export `LoopbackHost`; `Guard` unchanged), `doc.go` (the amended promise: `POST /mcp` and `POST …/inbox` are the only non-reads)

**Interfaces:**
- Consumes: `Guard`, `Token`, `securityHeaders`, `single`.
- Produces:
  - `func LoopbackHost(hostport string) bool`, the existing rule.
  - `func Front(page Token, mcp, inbox, pages http.Handler) http.Handler`:
    - it sets the security headers;
    - a Host that fails `LoopbackHost` gets 403;
    - it routes `/mcp` to `mcp`, `POST /s/<seg>/inbox` to `inbox`, other `/s/…` to `pages`, and anything else to 404.
  - `func MCPGuard(bearer Token, maxBody int64, next http.Handler) http.Handler`, in the check order of Builder's calls:
    - `Accept` must hold ranges covering `application/json` and `text/event-stream`;
    - `Content-Type` must be `application/json`, parameters allowed;
    - DELETE carries no body (else 400);
    - every refusal is answered from the headers, with `Connection: close` and a 100 ms read deadline.
  - `type Rate struct{ PerSecond, Burst int }`.
  - `func InboxGuard(page Token, rate Rate, next http.Handler) http.Handler`:
    - the token is compared in constant time; a wrong one gets 404;
    - `Origin` must appear once, be non-empty and equal `http://`+Host (else 403);
    - `Sec-Fetch-Site` must be present and `same-origin` (else 403);
    - `Content-Type: application/json` (else 400);
    - `X-Perf-Agent-Page: 1` (else 403);
    - `Content-Length` is required, chunked gets 411, and over 1024 gets 413;
    - the body is read under a 2 s read deadline;
    - a token bucket at `Rate{5, 5}` per server answers 429 with `Retry-After: 1`.

- [ ] **Step 1: Write the failing tests** (`httptest`; a body that blocks is a raw TCP connection that sends headers only)

```go
func TestFrontRoutesByPathAndChecksHost(t *testing.T)       // /mcp, POST /s/x/inbox, GET /s/x/, /other → the right handler or 404; Host evil.com:80, 127.0.0.1 (no port), [::1]:7341 ok, localhost:7341 ok
func TestFrontSetsSecurityHeadersOnEveryAnswer(t *testing.T) // 403, 404, and each route's answer carry the seven headers
func TestMCPGuardRefusesAnyOrigin(t *testing.T)             // Origin "", "http://127.0.0.1:7341", "null", repeated → 403; next not called
func TestMCPGuardRefusesFetchMetadata(t *testing.T)         // Sec-Fetch-Site same-origin, Sec-Fetch-Mode cors, Sec-Fetch-Dest empty → 403
func TestMCPGuardMethods(t *testing.T)                      // GET, PUT, OPTIONS → 405 with Allow "POST, DELETE", without a bearer
func TestMCPGuardBearer(t *testing.T)                       // missing, wrong, "Basic …", a token with a trailing space → 401 and no WWW-Authenticate header; "bearer <tok>" → passes
func TestAcceptAndContentTypeAreParsedAsMediaRanges(t *testing.T) // Review Focus 2: the five forms in the Review Focus pass; "application/json" alone, "text/event-stream;q=0, application/json" → 400; Content-Type "Application/JSON; charset=utf-8" passes, "text/plain" → 400
func TestMCPGuardBodyRules(t *testing.T)                    // chunked → 411; no Content-Length → 411; maxBody+1 → 413; DELETE with a body → 400
func TestEveryRefusalIsAnsweredBeforeTheBody(t *testing.T)  // each refusal above on a body that never arrives → the answer within 1 s, next never called, connection closed
func TestInboxGuardCSRFMatrix(t *testing.T)                 // Origin missing/other/empty/repeated → 403; Sec-Fetch-Site missing/cross-site/none → 403; text/plain → 400; no X-Perf-Agent-Page → 403; wrong token → 404; 1025 bytes → 413; a 6th POST within one second → 429 Retry-After 1
func TestLoopbackHostIsTheGuardsRule(t *testing.T)          // LoopbackHost agrees with Guard's Host decision on a table
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/livehttp/`
Expected: FAIL.

- [ ] **Step 3: Implement.** Write the media-range parser in `mcpguard.go`. A token bucket is a mutex and two fields; no goroutine.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/livehttp/`, then the green check.
Expected: PASS, and the M2b guard tests pass unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/livehttp
git commit -m "livehttp: a front door, a bearer guard for /mcp and a same-origin guard for the inbox"
```

### Task 4: mcpstdio's per-message API

**Files:**
- Create: `internal/mcpstdio/handle.go`, `handle_test.go`
- Modify: `internal/mcpstdio/session.go`, `server.go`, `protocol.go`

**Interfaces:**
- Consumes: the existing `parse`, `answer`, `handle`, `initialize`, `callTool`, `requestKey`, `Received`.
- Produces:
  - **Sessions.**
    - `type Session struct{ … }`, holding the initialized flag, the negotiated version and the client label.
    - `func NewSession() *Session`, with `(*Session).Initialized() bool`, `Version() string` and `Label() string`.
    - The stdio `session` holds one `*Session`, and its `initialized` flag moves there.
  - **Requests.**
    - `type Request struct { Method string; ID, Params json.RawMessage; Tool, URI string }`, with `(*Request).Notification() bool`. `Tool` is filled for `tools/call`, and `URI` for `resources/read`.
    - `func (s *Server) Parse(msg []byte) (*Request, []byte)` returns either a request or the encoded error answer. Both are nil for a dropped client response.
  - **Answers.** `func (s *Server) Handle(ctx context.Context, ss *Session, r *Request) []byte` answers one request as the dispatcher does:
    - it returns nil for a notification;
    - it puts `ss` in ctx;
    - it installs no progress target;
    - it calls no `OnClose`.
  - **Lookups.** `func (s *Server) Tool(name string) (Tool, bool)` and `func (s *Server) Resource(uri string) (Resource, bool)`.
  - **Context.** `func ClientLabel(ctx context.Context) string` returns "" outside a session. `func WithReceived(ctx context.Context, at time.Time) context.Context`; `Received` honours it.
  - **New fields.**
    - `Tool.Snapshot bool` (`json:"-"`): a call the HTTP transport admits in its own small class.
    - `Resource.Snapshot bool`, and `Resource.Read func(ctx context.Context) (string, error)`: when set, it is called per read, and an error becomes JSON-RPC `-32603` with the text.
    - `initialize` records the label by Builder's calls' rule.

- [ ] **Step 1: Write the failing tests**

```go
func TestHandleAnswersAsServeDoes(t *testing.T)        // the server_test request script through Serve and through Parse+Handle on one Session → identical answer bytes per id
func TestHandleRefusesBeforeInitialize(t *testing.T)   // tools/call on a fresh Session → "send initialize first"; ping answers
func TestInitializeRecordsTheClientLabel(t *testing.T) // "claude-code" → itself; 65 bytes, "a\nb", "é", absent → "client"; Version() is the negotiated one
func TestClientLabelReachesTheHandler(t *testing.T)
func TestWithReceivedIsWhatReceivedReads(t *testing.T)
func TestADynamicResourceIsReadEachTime(t *testing.T)  // Read called per resources/read with the session's ctx; an error → -32603 carrying its text
func TestHandleSendsNoProgress(t *testing.T)           // a handler's Progress returns false through Handle, even with _meta.progressToken
func TestParseRefusesWhatServeRefuses(t *testing.T)    // batch, non-object, bad jsonrpc, 1025-byte id → the same error lines Serve writes
// every existing mcpstdio test passes unchanged
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcpstdio/`
Expected: the new tests FAIL.

- [ ] **Step 3: Implement.** `Handle` is `answer` behind the exported names. The stdio dispatcher calls the same function, so the two cannot drift.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/mcpstdio/`, then the green check.
Expected: PASS. Every perfmcp and cmd test passes unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpstdio
git commit -m "mcpstdio: an exported session and one-message handler, so another transport answers exactly as stdio does"
```

### Task 5: The engine holder, `MaxEngineWait`, the serve limits and `by`

**Files:**
- Create: `internal/perfmcp/engine_wait_test.go`, `serve_limits_test.go`, `by_test.go`
- Modify:
  - `internal/perfmcp/`: `engine.go`, `server.go` (`tools()` passes names), `limits.go`, `config.go` (`HoldEngine`), `capture_run.go` and `capture_shutdown.go` (holder at the three `acquire` sites), `live.go` (`by` on publications and steering), `tool_page.go`, `tool_page_diff.go` (ctx and label);
  - `internal/mcpflags/limits.go` (stdio refuses `Serve.*`).

**Interfaces:**
- Consumes: Task 4's `mcpstdio.ClientLabel`.
- Produces:
  - **The holder.**
    - `type holder struct{ tool, by string; since time.Time }`.
    - `func (e *engine) acquire(ctx context.Context, h holder) error`: as today, and it records `h` while held.
    - `func (e *engine) holding() (holder, bool)`, read atomically.
  - **The wait bound.** `func (s *Server) engineHandler(tool string, h mcpstdio.Handler) mcpstdio.Handler`:
    - it waits at most `MaxEngineWait` (the try-free-first fast path is unchanged, and no timer is made when the slot is free);
    - on timeout it returns the busy text of Builder's calls;
    - it calls `Config.HoldEngine(tool)` when set, holding the engine.
  - **Limits.** `Limits.MaxEngineWait time.Duration` and `Limits.Serve ServeLimits{ MaxSessions, MaxIngressReaders, MaxControlInFlight, MaxPageStateInFlight, MaxCalls, MaxCallsPerSession, MaxInboxEntries int; MaxIngressBytes, MaxRequestBytes int64; IngressBodyTimeout time.Duration }`, with the defaults and validation of Builder's calls. `resolve` fills `Serve` only when the whole struct is zero.
  - **Attribution.** `publication.by`, and `live.by` for the steering. `liveStateEntry.By` and `liveState.By` are always present (`json:"by"`).
  - **mcpflags.** `Stdio`'s `SetLimit` refuses `Serve.` names: "Serve.MaxCalls is a perf-agent-mcp-serve limit".

- [ ] **Step 1: Write the failing tests**

```go
func TestABusyEngineAnswersWhoHoldsIt(t *testing.T)          // HoldEngine blocks top from Session "A"; Session "B" calls facts with MaxEngineWait 200ms → isError "perf-agent-mcp is busy with top from A (running 0s); call again" within 200..400 ms
func TestAWaiterWhoseContextEndsRunsNothing(t *testing.T)    // cancelled while waiting → handler never ran; the engine is free after
func TestTheUncontendedEngineAllocatesNothing(t *testing.T)  // testing.AllocsPerRun(acquire+release) == 0
func BenchmarkTopThroughTheEngine(b *testing.B)              // recorded in the ledger before/after; no measurable change (spec §D7)
func TestCaptureJobsHoldTheEngineByName(t *testing.T)        // a job's engine section holds as "capture <id>", maintenance as "capture retention"
func TestServeLimitsDefaultsAndValidation(t *testing.T)      // each default; each of the five refusals names both fields
func TestPublicationsAndSteeringRecordBy(t *testing.T)       // sessions "claude-code" and "codex": page_explain by one, page_show by the other → state entries by, steering by; render_flamegraph goldens unchanged
// internal/mcpflags
func TestStdioRefusesServeLimits(t *testing.T)               // Stdio: --limit Serve.MaxCalls=4 → UsageError naming perf-agent-mcp-serve; Serve: accepted
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ ./internal/mcpflags/ -run 'Busy|Waiter|Uncontended|HoldTheEngine|ServeLimits|RecordBy|RefusesServe'`
Expected: FAIL.

- [ ] **Step 3: Implement.** The busy timer starts only after the free-slot try fails.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/perfmcp/ -run 'Engine|Busy|Waiter'`, then the green check.
Expected: PASS. Every capture test passes unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp internal/mcpflags
git commit -m "perfmcp: the engine says who holds it and bounds a tool's wait; serve limits; publications and steering record who made them"
```

### Task 6: The Streamable HTTP binding: endpoint, sessions, cancellation, deadlines (opus)

**Files:**
- Create: `internal/mcphttp/doc.go`, `transport.go`, `session.go`, `inflight.go`, `deadline.go`, `transport_test.go`, `fake_test.go` (a fake `Handler` with blocking, slow and recording tools)

**Interfaces:**
- Consumes: Task 4's API, and Task 3's `MCPGuard` (in tests only).
- Produces:
  - **The handler interface.** `type Handler interface { Parse([]byte) (*mcpstdio.Request, []byte); Handle(context.Context, *mcpstdio.Session, *mcpstdio.Request) []byte; Tool(string) (mcpstdio.Tool, bool); Resource(string) (mcpstdio.Resource, bool) }`. `*mcpstdio.Server` satisfies it.
  - **Options.** `type Options struct { Lifetime context.Context; MaxSessions int; MaxMessageBytes int64; IngressBodyTimeout, WriteBound time.Duration; Log *slog.Logger }`. Task 7 adds the admission fields. `WriteBound` 0 means 10 s.
  - **The transport.**
    - `func New(h Handler, o Options) *Transport`.
    - `(*Transport).ServeHTTP` runs below `MCPGuard`.
    - `(*Transport).Close()` cancels every in-flight context; later requests get 503.
    - `(*Transport).Stats() Stats` (sessions, in-flight count, readers, reserved bytes), for tests.
  - **Headers.** `const HeaderSession = "Mcp-Session-Id"` and `const HeaderVersion = "MCP-Protocol-Version"`.
  - **Behaviour:**
    - **POST.** It clears the write deadline on entry (`http.ResponseController`). It reads the body under a read deadline of `IngressBodyTimeout` and then clears that deadline. It parses, then stamps `mcpstdio.WithReceived`.
    - **Answers.** A request gets 200 `application/json`. A notification or a client response gets 202. A batch or a parse error gets 400 with the JSON-RPC error body.
    - **Sessions.** `initialize` mints a session (`Mcp-Session-Id` on the answer). Otherwise a missing session gets 400 and an unknown one 404.
    - **Versions.** An unsupported `MCP-Protocol-Version` gets 400.
    - **Writing.** A 10 s write deadline is set just before the answer is written.
    - **Contexts.** A call's context derives from `Lifetime`, never from `r.Context()`, with a cancel registered under (session, compact id). A duplicate in-flight id in one session is refused with a JSON-RPC `-32600` error.
    - **Cancellation.** `notifications/cancelled` cancels only its own session's entry. DELETE cancels that session's entries and removes the session (204, or 404 when unknown).
    - **Eviction.** Least recently used, and never a session with in-flight calls. When every session is pinned, `initialize` gets 503.

- [ ] **Step 1: Write the failing tests** (`-race`; the chain under test is `livehttp.Front` + `MCPGuard` + `Transport` on `httptest.Server`)

```go
func TestInitializeMintsASession(t *testing.T)                // 43-char base64url; distinct per initialize; initialize with an old header still mints
func TestSessionRules(t *testing.T)                           // missing → 400; unknown → 404; DELETE → 204; again → 404; DELETE without header → 400
func TestProtocolVersionHeader(t *testing.T)                  // "1999-01-01" → 400; absent → the session's; "2025-06-18" session works
func TestAModernProbeGets400AndInitializeThenWorks(t *testing.T) // POST server/discover with Mcp-Method and no session → 400, body a JSON-RPC -32600 "no session"; then initialize + tools/list → 200
func TestNotificationsAndResponsesGet202(t *testing.T)
func TestABatchIsRefused(t *testing.T)
func TestOnlyJSONIsEverAnswered(t *testing.T)                 // every answer of the suite: Content-Type application/json or no body; never text/event-stream; never a body with "method" and "id" from the server
func TestALongCallOutlivesTheServersWriteTimeout(t *testing.T) // http.Server{WriteTimeout: 300ms}; a 600 ms tool → full 200 answer
func TestTheBodyDeadlineIsClearedBeforeTheWait(t *testing.T)  // IngressBodyTimeout 100ms; a tool that runs 500ms → answered, connection reusable
func TestAClientThatStopsReadingIsDroppedAfterTheWriteBound(t *testing.T) // WriteBound 200ms; a 16 MiB answer to a conn that never reads → the handler goroutine returns within 1 s; Stats back to baseline
func TestAClosedConnectionDoesNotCancel(t *testing.T)         // client closes after sending; the recording tool completes and records ctx.Err()==nil
func TestCancelledIsScopedToItsSession(t *testing.T)          // B sends notifications/cancelled for A's id → A's call unaffected; A's own → A's ctx cancelled
func TestADuplicateInFlightIDIsRefused(t *testing.T)
func TestDeleteCancelsItsSessionsRequests(t *testing.T)       // and not another session's
func TestEvictionSkipsSessionsWithInFlightCalls(t *testing.T) // MaxSessions 2: A in flight, B idle; initialize C evicts B; B → 404; all pinned → initialize 503
func TestAnOldSessionIs404AndAnOldTokenIs401AfterARestart(t *testing.T) // Review Focus 1: a second Transport+guard with new tokens; old session → 404, old token → 401 without WWW-Authenticate
func TestReceiptIsStampedAtParse(t *testing.T)                // the tool sees mcpstdio.Received ok, between send and parse completion
func TestCloseCancelsEverything(t *testing.T)                 // in-flight ctx cancelled; next request 503; goroutines back to baseline
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcphttp/`
Expected: FAIL (no package).

- [ ] **Step 3: Implement.** One mutex guards the session table and the in-flight table; it is never held across a handler or a write.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=5 ./internal/mcphttp/`, then the green check.
Expected: PASS five times.

- [ ] **Step 5: Commit**

```bash
git add internal/mcphttp
git commit -m "mcphttp: MCP over Streamable HTTP, JSON only, with server-scoped contexts and per-session cancellation"
```

### Task 7: Bounded ingress and admission classes (opus)

**Files:**
- Create: `internal/mcphttp/ingress.go`, `admit.go`, `admit_test.go`
- Modify: `internal/mcphttp/transport.go`, `transport.go`'s `Options`

**Interfaces:**
- Consumes: Task 6.
- Produces:
  - **Options** gain `MaxIngressReaders, MaxControlInFlight, MaxPageStateInFlight, MaxCalls, MaxCallsPerSession int` and `MaxIngressBytes, MaxRequestBytes int64`.
  - `type Class int`, with `ClassControl`, `ClassPageState` and `ClassCalls`. `func classify(h Handler, r *mcpstdio.Request) Class`:
    - **control:** notifications, `ping`, `initialize`, the `*/list` methods, and a `tools/call` of a `Control` tool;
    - **page state:** a `tools/call` of a `Snapshot` tool, or `resources/read` of a `Snapshot` resource;
    - **calls:** everything else.

    DELETE counts in the control class.
  - **Ingress.** A reader slot and `Content-Length` bytes are reserved **before** the first body byte, else 503 with `Retry-After: 1` and no read. Both are released as soon as the body is parsed or refused. A body not complete within `IngressBodyTimeout` drops the connection.
  - **Admission** after parsing:
    - control and page state are bounded by their in-flight caps;
    - calls are bounded by `MaxCalls` (active plus waiting), `MaxCallsPerSession` and `MaxRequestBytes` (`len(Params)` retained, never the raw body);
    - excess gets 503 with a JSON-RPC error that carries the request's id, code `-32000`, and "server busy: <Cap>=<n> reached; retry".

- [ ] **Step 1: Write the failing tests** (the fake handler's blocking tool stands in for a held engine; `capture_cancel` is a fake `Control` tool)

```go
func TestSameSessionSaturation(t *testing.T)          // one session sends 40 calls: 4 admitted, 36 → 503 at once; then 5 sessions × 4 → MaxCalls 16 reached, the rest 503; ping, tools/list, notifications/cancelled, DELETE from the saturating and other sessions answered < 100 ms; goroutines and Stats under caps; the queued calls complete
func TestControlIsServedBehindAFullCallClass(t *testing.T) // MaxCalls of small fast POSTs blocked; then notifications/cancelled and capture_cancel → read, classified control, answered < 100 ms, before any blocked call starts
func TestSlowIncompleteBodiesFillOnlyTheReaders(t *testing.T) // 16 connections announce 1 KiB and send 10 bytes → new requests (ping included) 503 unread; at IngressBodyTimeout each is dropped and released; a cancel after that is served; goroutines and bytes back to baseline
func TestControlClassSaturation(t *testing.T)         // MaxControlInFlight blocked controls → the next control 503 promptly; calls and page_state unaffected; controls served again once freed
func TestHeaderOnlyRejection(t *testing.T)            // a Content-Length over MaxIngressBytes left → 503 without a read; over MaxMessageBytes → 413 (MCPGuard)
func TestPageStateHasItsOwnClass(t *testing.T)        // MaxPageStateInFlight 4 blocked Snapshot calls → the 5th 503; calls still admitted
func TestRequestBytesAreRetainedArguments(t *testing.T) // MaxRequestBytes 1 MiB: two 600 KiB-argument calls → the second 503 naming MaxRequestBytes; released on completion
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcphttp/ -run 'Saturation|Behind|Slow|Header|PageState|RequestBytes'`
Expected: FAIL.

- [ ] **Step 3: Implement.** Each class is a counter under the transport mutex; nothing waits in admission.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=5 ./internal/mcphttp/`, then the green check.
Expected: PASS five times.

- [ ] **Step 5: Commit**

```bash
git add internal/mcphttp
git commit -m "mcphttp: bounded ingress, then control, page-state and call classes that refuse excess at once"
```

### Task 8: The state envelope, the inbox and `page_state`

**Files:**
- Create:
  - `internal/perfmcp/statebus.go`, `inbox.go`, `tool_page_state.go`, `statebus_test.go`, `inbox_test.go`, `page_state_test.go`;
  - `internal/perfpage/inbox.go`, `inbox_test.go`.
- Modify:
  - `internal/perfmcp/`: `live.go` (`commit` feeds the bus), `pageview.go` (`State` is the envelope), `server.go` (register `page_state` and the resource with `Page`; the serve instruction line), `guide.go`;
  - `internal/perfpage/routes.go`.

**Interfaces:**
- Consumes: Task 2 `PageView`; Task 4 `Tool.Snapshot`, `Resource.Read`, `Resource.Snapshot` and `ClientLabel`; Task 5 `Limits.Serve.MaxInboxEntries` and `HoldEngine`.
- Produces:
  - **The bus.** `type stateBus struct{…}`:
    - `setEngine(seq int64, b []byte)` and `setInbox(rev int64, b []byte)` each ignore a part that is not newer;
    - each builds `{"engine":<M2b state>,"inbox":{"rev","asks","unread","read_by"}}` under the bus mutex and calls `PageConfig.Notify`;
    - `envelope() []byte`.
  - **Reports.**
    - `type PageReport struct{ Kind, Publication, Node, Focus string }`, where `Kind` is `select`, `focus` or `ask`.
    - `var ErrBadReport = errors.New("invalid page report")`, wrapped with the reason.
    - `func (s *Server) Report(r PageReport) error` validates against the current snapshot and never takes the engine. The publication must be kept. The node must be a node of its view, or a union node of a diff, through the same lookups as `PageView.Frame`. `focus` is "" or a node; an `ask` needs a node.
    - Every value the inbox stores is derived by the server: the name cut to 1 KiB, the view, the digest and the time.
  - **The inbox** (`inbox.go`, its own mutex):
    - `user` holds the latest `select` and `focus`, coalesced;
    - `asks` is a ring of `MaxInboxEntries`, with ids `i<N>-<nonce>` and a `dropped` count;
    - a duplicate ask (same publication and node within 2 s) is ignored, and `Report` returns nil;
    - each change bumps `rev`.
  - **`page_state`** (`tool_page_state.go`):
    - the tool is `page_state {since?: string, limit?: integer}`, with `Snapshot: true` and annotations `readOnlyHint: false, idempotentHint: true`;
    - the resource is `perf-agent://page/state`, `Snapshot: true`, with `Read`: the last 8 asks and no cursor;
    - both share one handler, which never takes the engine and marks `read_by` with `ClientLabel` on asks it returns that have none;
    - the result is §D4's JSON, counts as decimal strings, plus `note` when `since` is another process's;
    - the instruction line, with `Page` only: "When the user refers to something they selected or asked about on the page, call page_state."
  - **perfpage.**
    - `func Inbox(srv *perfmcp.Server) http.Handler` decodes ≤ 1 KiB of strict JSON, where an unknown field gets 422. It answers 204, or 422 with the reason.
    - `Routes`' `GET state` serves `PageView.State()`, the envelope.

- [ ] **Step 1: Write the failing tests**

```go
func TestTheEnvelopeHasTwoRevisions(t *testing.T)        // {"engine":{"seq":…},"inbox":{"rev":"0","asks":"0","unread":"0","read_by":""}} at start
func TestACommitBumpsOnlySeqAndAReportOnlyRev(t *testing.T)
func TestNeitherWriterWaitsForTheOther(t *testing.T)     // HoldEngine held: Report returns and Notify fires < 50 ms
func TestReportsAreValidatedAgainstTheSnapshot(t *testing.T) // unknown pub, unknown node, 65-byte id, kind "bogus", ask without node → ErrBadReport; nothing recorded; rev unchanged; a diff union node accepted
func TestNoPageRequestMutatesEngineState(t *testing.T)   // seq, the attempts record and the store's handles equal before and after 50 reports
func TestSelectionsCoalesce(t *testing.T)
func TestTheAskRingDropsTheOldest(t *testing.T)          // MaxInboxEntries 3, 5 asks → rows of the last 3, dropped "2"
func TestADuplicateAskWithinTwoSecondsIsIgnored(t *testing.T) // s.now seam
func TestPageStateCursors(t *testing.T)                  // two sessions, each with its own since: each sees every ask once; next is the last row's id
func TestACursorFromAnEarlierServerReturnsEveryRetainedAsk(t *testing.T) // Review Focus 5: since "i40-ffff" → every retained ask and a note
func TestReadsAreMarkedOnce(t *testing.T)                // "codex" reads first → read_by codex; "claude-code" later → still codex; rev bumped once
func TestPageStateNeverTakesTheEngine(t *testing.T)      // HoldEngine held: the tool and resources/read answer < 100 ms with current data
func TestThePageStateResourceShowsTheLastEightAsks(t *testing.T)
func TestPageStateIsServeOnly(t *testing.T)              // without Page: tools/list is the 13 tools, resources/list has no page/state
// internal/perfpage
func TestInboxHandlerAnswers204And422(t *testing.T)      // valid → 204; unknown field "name" → 422; unknown node → 422 with the reason
func TestTwoTabsSeeInboxRevisionsWithoutCommits(t *testing.T) // two events streams: an ask, then page_state's read_by → both tabs get each inbox.rev and engine.seq unchanged; repeated while HoldEngine holds the engine
func TestInterleavedCommitsAndReportsNeverGoBack(t *testing.T) // 200 interleaved; per stream, seq and rev never decrease
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ ./internal/perfpage/ -run 'Envelope|Bumps|Writer|Report|Coalesce|Ring|Duplicate|Cursor|Marked|PageState|Inbox|TwoTabs|Interleaved'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/perfmcp/ ./internal/perfpage/`, then the green check.
Expected: PASS. The Task 2 route tests read `engine.seq` from the envelope.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp internal/perfpage
git commit -m "perfmcp, perfpage: a two-revision page state, a validated inbox, and page_state that never waits for the engine"
```

### Task 9: The page reports, the "Ask my agent" chip and the inbox count

**Files:**
- Modify: `internal/flamegraph/live.js`, `livepage.html`, `live.css`, `liveassets_test.go`

**Interfaces:**
- Consumes: Task 8's envelope and the inbox contract (`POST inbox`, its body, and 204, 422 and 429).
- Produces:
  - `transport.post(path, body)`, `fetch` with `method: 'POST'`, `Content-Type: application/json` and `X-Perf-Agent-Page: 1`.
  - `onState(env)`:
    - `env.engine` goes through the unchanged R1/R9..R12 logic;
    - `env.inbox` is applied when `newer(rev, acceptedRev)`;
    - neither part resets the other's state or `gen`.
  - Local select and focus report to `inbox`, fire and forget. Reports never touch `acceptedSeq` or `gen`.
  - An "Ask my agent about this" chip, inserted by `live.js` next to "Focus here". `panel.js` is unchanged, since the static page shares it.
  - Toasts:
    - 204: "Sent. Ask your agent to look (it reads page_state).";
    - 422: "That frame is no longer on the page; nothing was sent.";
    - 429: "Too many reports; try again in a moment.".
  - The inbox count in the header, "n asks · m unread", and "read by <label>" when `read_by` is set.

- [ ] **Step 1: Write the failing tests**

```go
// TestLiveScriptRules: still no "/" in fetched paths and no number parsing; exactly one POST, to 'inbox', with the custom header; rev compared by newer() only
func TestLivePageInboxInChrome(t *testing.T) {
	// fixture server: the envelope on state/events and POST inbox (204/422/429 by node)
	// t.Run("an ask posts and the toast shows")                          — body {kind:"ask",publication,node,focus}; toast text exact
	// t.Run("a 422 says nothing was sent")
	// t.Run("a new rev with an old seq updates only the count")          — gen and the shown publication unchanged
	// t.Run("a new seq with an old rev leaves the count")
	// t.Run("a report during an agent chain's switch keeps the chain")  — the chain completes; gen bumped only by the agent state
	// t.Run("no CSP violation")
}
// every existing live Chrome test passes with its fixtures moved to the envelope
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/flamegraph/ -run 'LiveScriptRules|LivePage'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race ./internal/flamegraph/`, then the green check.
Expected: PASS, or the Chrome tests SKIP without Chrome. Every static-page golden is byte-identical.

- [ ] **Step 5: Commit**

```bash
git add internal/flamegraph
git commit -m "flamegraph: the live page reports selections and asks to the inbox, and applies the inbox part by its own revision"
```

### Task 10: `cmd/perf-agent-mcp-serve`: runtime directory, lock, tokens, wiring and shutdown

**Files:**
- Create: `cmd/perf-agent-mcp-serve/main.go`, `serve.go`, `runtimedir.go`, `main_test.go`, `runtimedir_test.go`, `serve_test.go`

**Interfaces:**
- Consumes: Task 1 `mcpflags.Serve`; Tasks 2 and 8 `perfpage.Routes`, `Inbox` and `NewHub`; Task 3 `Front`, `MCPGuard` and `InboxGuard`; Tasks 6–7 `mcphttp`; Task 5 `Limits.Serve`; `capture.RunInternal`.
- Produces:
  - **`main()`.**
    - `capture.RunInternal()` runs first.
    - Then the subcommands: `token [--runtime-dir DIR]` and `url [--runtime-dir DIR]`.
    - Otherwise `runServe(ctx, args, stdout, stderr) int`. Exit 0 on SIGINT or SIGTERM, 1 on a runtime error, 2 on usage. `--port N` (1..65535, default `DefaultPort` = 7341), `--runtime-dir DIR`, and the mcpflags.
  - **`runtimedir.go`.**
    - `type runtimeDir struct{ root *os.Root; path string; lock *os.File }`.
    - `func openRuntimeDir(flag string, getenv func(string) string) (*runtimeDir, error)`:
      - with `XDG_RUNTIME_DIR`, it uses `$XDG_RUNTIME_DIR/perf-agent-mcp`, created 0700 when missing;
      - without it, `--runtime-dir` is required, and its absence is an error naming both;
      - it refuses a symlink, a non-directory, a mode other than 0700, or an owner other than the uid (through the `ownerOf` seam).
    - `(*runtimeDir).take() (serveInfo, error)`: `syscall.Flock(LOCK_EX|LOCK_NB)` on `serve.lock`. A held lock gives "perf-agent-mcp-serve is already running (pid N, port P)", read from `serve.json`.
    - `write(info serveInfo, mcpToken livehttp.Token, pageURL string) error` writes the three files (0600, a temporary file plus rename).
    - `remove()`.
    - `func running(dir string) bool`: a `LOCK_SH|LOCK_NB` probe fails.
  - `type serveInfo struct{ PID int; Port int; MCPURL string; Started time.Time; Version string }`, with JSON names `pid`, `port`, `mcp_url`, `started` and `version`, and no token.
  - **`serve.go`.**
    - `func start(ctx context.Context, o options) (*instance, error)` does, in order:
      1. the runtime directory and its lock;
      2. the tokens;
      3. the hub;
      4. `perfmcp.New` with `Page` and `Config.Command = "perf-agent-mcp-serve"`;
      5. `mcphttp.New(srv.MCP(), …)` from `Limits.Serve`;
      6. `livehttp.Listen("127.0.0.1:<port>")`. A busy port gives "port P is in use; start with --port N";
      7. `livehttp.NewServer(Front(page, MCPGuard(mcpTok, MaxMessageBytes, tr), InboxGuard(page, Rate{5, 5}, perfpage.Inbox(srv)), Guard(page, perfpage.Routes(srv, hub))))`;
      8. the files;
      9. the page URL, once, to stdout;
      10. the "listening" log line, with `mcp_url` and no token.
    - `(*instance).shutdown()` follows reconciliation row 4's order. Each step is bounded, and nothing blocks on stderr.
    - `func (s *Server) MCP() *mcpstdio.Server` is added to perfmcp.
  - **Test seams.**
    - `var configHook func(*perfmcp.Config)` and `var killCaptures, closeServer`, as in stdio.
    - `TestMain` routes `PERF_AGENT_MCP_SERVE_TEST_MAIN=1` to `main()` after `capture.RunInternal`.
    - `serveCommand(t, env, args...)` re-executes the test binary.

- [ ] **Step 1: Write the failing tests** (`XDG_RUNTIME_DIR` = `t.TempDir()`; ports from a probe listener closed just before)

```go
func TestRuntimeDirModesAndOwner(t *testing.T)          // 0755 → refused naming the mode; ownerOf seam returns another uid → refused; a symlink → refused; created 0700 when missing
func TestRuntimeDirNeedsXDGOrAFlag(t *testing.T)        // no XDG_RUNTIME_DIR, no --runtime-dir → exit 1 naming both
func TestASecondInstanceIsRefused(t *testing.T)         // exit 1, "already running (pid <first>, port <p>)"
func TestFilesAreWrittenAndRemoved(t *testing.T)        // serve.json, mcp-token, page-url all 0600; serve.json has no token; after SIGTERM all three gone, serve.lock kept
func TestNoTokenEverReachesStderr(t *testing.T)         // a full run (initialize, tools, page GETs, an inbox POST, SIGTERM): neither token appears in stderr
func TestThePageURLGoesToStdoutOnly(t *testing.T)       // stdout is exactly one line, the page URL == page-url's content
func TestTokenAndURLSubcommands(t *testing.T)           // token → {"Authorization":"Bearer <mcp-token>"}\n; url → the page URL
func TestTokenWithoutARunningServerPrintsNothing(t *testing.T) // Review Focus 3: no server; a stale mcp-token with the lock free → exit 1, stderr says not running, stdout empty
func TestABusyPortNamesItAndTheFlag(t *testing.T)       // Review Focus 4
func TestAStaleServeJSONDoesNotBlockStart(t *testing.T) // Review Focus 4: a serve.json naming a dead pid, lock free → starts and replaces it
func TestServeHasNoAddressFlag(t *testing.T)            // --addr, --host, --listen → exit 2
func TestThePageURLIsLoopbackWithAToken(t *testing.T)   // replaces the perfmcp listener test: 127.0.0.1:<port>/s/<43 chars>/
func TestShutdownOrder(t *testing.T)                    // seams record: KillCaptures, hub close, http shutdown, transport close, Close, files removed; with a tool in flight, exit ≤ 1 s + 300 ms after SIGTERM
func TestShutdownStopsTheListener(t *testing.T)         // replaces TestCloseStopsTheListener: no connection accepted after exit
func TestShutdownIsBoundedByStuckTabs(t *testing.T)     // replaces TestCloseIsBoundedByStuckClients
func TestServeToolList(t *testing.T)                    // the 13 stdio tools, then page_open, page_explain, page_show, page_diff, then page_state; resources include perf-agent://page/state
func TestOneCallRunsAtATime(t *testing.T)               // 8 sessions call tools at once while tabs GET and POST inbox; HoldEngine counts concurrent holders: max 1; each session's answers echo its ids (§D7)
func TestTheServeBinaryIsNotShipped(t *testing.T)       // Makefile, Dockerfiles, release workflow, notices script do not mention perf-agent-mcp-serve
func TestTheServeBinaryLinksNoProfiler(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp-serve/`
Expected: FAIL (no package).

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=2 ./cmd/perf-agent-mcp-serve/`, then the green check, now with `./cmd/perf-agent-mcp-serve/`.
Expected: PASS twice, with no leftover process and no file left in the temporary runtime directories.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp-serve internal/perfmcp/server.go
git commit -m "perf-agent-mcp-serve: one loopback server for the page and MCP over HTTP, with a per-user lock and rotated tokens"
```

### Task 11: The network promises move to the serve binary, and the page end to end

**Files:**
- Move: `cmd/perf-agent-mcp/netscan_test.go` → `cmd/perf-agent-mcp-serve/netscan_test.go` (`git mv`, then retarget)
- Create: `cmd/perf-agent-mcp-serve/sockets_linux_test.go`, `link_test.go`, `chrome_test.go`, `httpclient_test.go` (a hand-written MCP-over-HTTP client, no dependency)
- Modify: `cmd/perf-agent-mcp/main_test.go` (keeps `TestTheServerLinksNoNetworking`, `TestPerfmcpImportsNoNetworking` and `TestTheServerLinksNoProfiler`)

**Interfaces:**
- Consumes: Task 10.
- Produces (test-only):
  - **The scan** covers the serve binary's closure, a superset of stdio's:
    - its coverage list adds `internal/livehttp`, `internal/perfpage`, `internal/mcphttp`, `internal/mcpflags` and the main package;
    - `got.listens == ["net.Listen"]` in `listen.go` again;
    - `allowedFiles.listenCallers = []string{"cmd/perf-agent-mcp-serve/serve.go"}`: `livehttp.Listen` is referenced once, there, and anywhere else is a finding;
    - the exec, launch and helper rules are unchanged.
  - **nm.** `liveSymbols` become `perfpage.Routes.func1`, `(*livehttp.Hub).ServeHTTP` and `(*mcphttp.Transport).ServeHTTP`. `clientSymbols` are unchanged.
  - **`httpClient`.** `initialize(label)`, `call(tool, args)`, `notify(method, params)`, `delete()` and `get(path)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestNoClientNetworkingInTheServer(t *testing.T)         // retargeted at "."; listens == [net.Listen]; listen caller pinned
func TestTheNetworkScanPinsTheListenCaller(t *testing.T)     // fixtures: livehttp.Listen called in perfpage, or twice in serve.go → findings; once in serve.go → none
// the other scan fixture tests move unchanged
func TestTheServeBinaryOpensNoConnection(t *testing.T)       // only internal/livehttp imports net; net/http only livehttp, perfpage, mcphttp, the main package; nm: liveSymbols linked, clientSymbols absent
func TestTheServeBinaryLinksThePageAndTransport(t *testing.T) // the positive half: perfpage, livehttp, mcphttp linked
func TestEverySocketIsLoopback(t *testing.T)                 // the built server; MCP over HTTP (initialize, open, page_explain, page_state, DELETE), page GETs, an events stream, an inbox POST; every socket in /proc/<pid> is a 127.0.0.1 listener or a loopback-to-loopback connection
func TestAnAskReachesPageStateInChrome(t *testing.T)         // real server: publish, click a frame, press the chip → toast; page_state returns the ask with the frame's name; a second tab's count updates while HoldEngine holds the engine
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp-serve/ -run 'Network|Opens|Links|Socket|InChrome'`
Expected: FAIL.

- [ ] **Step 3: Implement the retargeting and the client.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check, plus `go test -race -run 'Links|Imports|Network' ./cmd/perf-agent-mcp/ ./cmd/perf-agent-mcp-serve/`
Expected: PASS. The stdio binary links no networking, and the serve binary links the page and the transport with no client symbol.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp cmd/perf-agent-mcp-serve
git commit -m "perf-agent-mcp-serve: the source scan, socket and symbol checks now guard the binary that has a network"
```

### Task 12: Capture hosting in perfmcp: publication, `show`, protection, the recipe and `by` (opus)

**Files:**
- Create: `internal/perfmcp/capture_publish.go`, `capture_serve_test.go`
- Modify:
  - `internal/perfmcp/`: `capture_run.go` (`done` publishes under the engine it already holds), `capture_compare.go` (`recomputeFor` adds the page steps with `Page`), `capture_job.go` (`job.by`, `captureRequest.Show`), `tool_capture.go` (the `show` schema and `publication` fields with `Page`), `tool_capture_list.go` (`listJob.By`), `server.go` (`captureProtect` set with `Page`; `captureDisabledText` from `Config.Command`), `config.go` (`Command`, `CaptureConfig.ReaperPause`), `live.go` (`publication.files []fileKey`).

**Interfaces:**
- Consumes: Task 5's holder and `by`; Task 8's bus; the shipped `derive`, `recomputeFor`, `captureProtect`, `toolCall`, `recompute` and registry.
- Produces:
  - `func (s *Server) capturePublish(j *job, m manifest, res *captureResult) error`. It runs under the engine inside `done`, after `derive`:
    - the first mode's facts-only publication goes through `publishExplained` with an empty explanation set and the title of Builder's calls;
    - with a compare, a `publishDiff` follows;
    - both have `by = j.by`;
    - with `j.req.Show` it steers to the diff, else to the facts page;
    - it sets `res.Publication` and `res.DiffPublication`;
    - an error makes `derived: "failed"`, with `derive_error` starting `publication: `.
  - `captureResult` gains `Publication`, `DiffPublication string` (`omitzero`).
  - `captureRequest.Show bool` (default true). The `capture` schema has `show` only with `Page`; stdio still refuses it as an unknown argument.
  - `captureProtect` returns the `fileKey`s of every kept publication. `publication.files` is copied from the store entry's `opened` at publish time.
  - With `Page`, `recomputeFor` appends `{"tool":"page_explain","arguments":{"handle":"${new_<mode>.handle}","view":"${new_<mode>.default_view}","explanations":[]}}`, plus a `page_diff` step for a compare.
  - `job.by` comes from `mcpstdio.ClientLabel` at admission. It appears in `listJob.by`, and in the engine holder as `capture <id>`.

- [ ] **Step 1: Write the failing tests** (fake perf-agent via `capturetest`, `Config.Capture.Verify`; sessions through `s.MCP().Handle` with distinct labels)

```go
func TestServeAcceptsWhileAnotherToolHoldsTheEngine(t *testing.T) // HoldEngine held by session A's top; B's capture → accepted < 50 ms; no series, .seq or intent appears until released
func TestAJobsEngineStepsWaitBehindToolsAndComplete(t *testing.T) // reconciliation 6: HoldEngine held 3×MaxEngineWait during deriving → B's waiting top gets busy; after release the job ends done/derived ready
func TestADoneCaptureIsPublishedFactsOnly(t *testing.T)   // result.publication "pN"; envelope lists it with by = A's label, current; title "adhoc/001-run · capworkload (<pid>) · cpu · <profiled>"
func TestShowFalseOnlyLists(t *testing.T)                 // current unchanged; listed
func TestACompareAddsADiffPublication(t *testing.T)        // diff_publication current; both by A
func TestAPublicationFailureIsDerivedFailedAndKeepsTheFiles(t *testing.T) // MaxPageBytes tiny → derived failed, derive_error "publication: …", manifest and files intact, recompute has the page step
func TestRecomputeInServeRepublishes(t *testing.T)         // registry evicted (MaxJobs 1): wait by capture id → derived "recompute"; executing every step with binds → a new publication; each step's arguments decode against its tool's schema
func TestRetentionSparesPublishedCaptures(t *testing.T)    // KeepPerSeries 1: 001 published; 002 committed → 001 kept (captureProtect); with the publication evicted → 001 evicted on the next capture
func TestStdioHasNoShowAndNoPublication(t *testing.T)      // no Page: show → invalid argument naming it; result has neither field
func TestMultiSessionWaitsAndCancel(t *testing.T)          // A starts; A's Session dropped; B waits 1s and C waits 3s at once, each returns at its own deadline; B cancels → cancelling; job cancelled; C's next wait sees cancelled
func TestHistoricalLookupsInServe(t *testing.T)            // MaxJobs 1, lookupHook blocks: repeated waits return loading at their deadlines with no goroutine growth; unblock → done, derived recompute, recompute names page_explain; no derived file written
func TestCaptureListShowsWhoStartedTheRunningJob(t *testing.T)
func TestTheDisabledTextNamesTheServeBinary(t *testing.T)  // Command "perf-agent-mcp-serve" → "start perf-agent-mcp-serve with --perf-agent PATH"
func TestPinsAndReservationsReturnAfterAnEngineWaitCancel(t *testing.T) // a job cancelled while waiting for the engine in preparing → pin ledger and reserved window back to baseline
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'ServeAccepts|EngineSteps|Published|ShowFalse|DiffPublication|PublicationFailure|Republishes|SparesPublished|NoShow|MultiSession|HistoricalLookupsInServe|WhoStarted|DisabledText|EngineWaitCancel'`
Expected: FAIL.

- [ ] **Step 3: Implement.** Nothing in the shipped lifecycle changes order. The publication is one more derivation, done inside the engine section `done` already holds.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -race -count=3 ./internal/perfmcp/`, then the green check.
Expected: PASS three times. Every capture test passes unchanged, and no `fakeperfagent` or `capworkload` process is left.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: in serve a committed capture is published on the page, kept from retention, and republished by its recipe"
```

### Task 13: Serve-level capture and crash tests (opus)

**Files:**
- Create: `cmd/perf-agent-mcp-serve/capture_linux_test.go`
- Modify: `cmd/perf-agent-mcp-serve/main_test.go` (`PERF_AGENT_MCP_SERVE_TEST_FAKE_VERIFY=1` and `…_REAPER_PAUSE=<v>` compose into `configHook`)

**Interfaces:**
- Consumes: Tasks 10–12; `capturetest.FakePerfAgent`, `Workload`, `SkipIfPrivileged`.
- Produces (test-only): `serveProc` (`start`, `kill9`, `sigterm`, `client`), each with its own `--runtime-dir`, `--port` and profiles root. Two serve processes may share one captures directory through distinct runtime directories.

- [ ] **Step 1: Write the failing tests**

```go
func TestServeCapturesOverHTTP(t *testing.T)              // capture → accepted; capture_wait → done, derived ready, publication; GET pub/<p>/model 200
func TestDeleteAndEvictionNeverCancelAJob(t *testing.T)   // A captures then DELETEs; MaxSessions 2 forces A's eviction too; B and C wait at once (own timeouts); B cancels → C sees cancelled; files rolled back
func TestCaptureCancelUnderFullCallsWithTheEngineHeld(t *testing.T) // MaxCalls of blocked tools + HoldEngine: capture_cancel → "cancelling" < 100 ms; before the manifest write → cancelled; a later cancel of a committed one → "too late: committed", files kept
func TestAnotherServeHoldsTheLease(t *testing.T)          // reconciliation 2: serve 2 shares serve 1's captures dir mid-capture → accepted; the job fails busy naming serve 1; only the dir and .owner may exist from serve 2
func TestAKilledServesReaperBlocksTheNext(t *testing.T)   // reconciliation 3: REAPER_PAUSE before-exit:<f>; kill9 mid-capture; restart: capture → busy (retry_after ≤ 5 s); capture_wait by capture id → in_progress naming the dead server's reaper; remove f → interrupted, retryable
func TestStdioAndServeRefuseEachOthersDirAtTheBinary(t *testing.T) // reconciliation 1: owner_mismatch both ways; the message points at --captures
func TestEverySocketIsLoopbackWhileCapturing(t *testing.T) // the socket check of Task 11 during a capture with the fake
func TestSIGTERMKillsTheCaptureFirst(t *testing.T)        // double-fork-setsid workload: SIGTERM → the fake's group and the grandchild gone within StopGrace + 500 ms; exit 0 within 1 s + 300 ms
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp-serve/ -run 'OverHTTP|NeverCancel|FullCalls|HoldsTheLease|KilledServe|EachOthers|WhileCapturing|KillsTheCapture'`
Expected: FAIL.

- [ ] **Step 3: Implement the harness.** Fix what the tests expose in Tasks 6, 10 and 12, each fix with the test that found it.

- [ ] **Step 4: Run them to verify they pass**

Run: the same tests with `-count=3`, then the green check.
Expected: PASS three times. `ps -eo pid,comm | grep -E '^ *[0-9]+ (fakeperfagent|capworkload|perf-agent-mcp-s)'` prints nothing, and no `perf-agent-mcp-*` cgroup is left.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp-serve
git commit -m "perf-agent-mcp-serve: capture over HTTP across sessions, with crash, lease and shutdown tests"
```

### Task 14: Docs, guide, skill and changelog

**Files:**
- Modify: `docs/mcp.md`, `internal/perfmcp/guide.md`, `guide_test.go`, `skills/perf-agent-mcp/SKILL.md`, `internal/perfmcp/skill_test.go`, `CHANGELOG.md`
- Create: `internal/perfmcp/docs_serve_test.go`

**Interfaces:**
- Consumes: Tasks 1–13.
- Produces:
  - **`docs/mcp.md`.**
    - Line 3 and "What it does not do" now say the stdio binary links no networking, and name the serve binary as the only listener.
    - A new section, "Watching with the live server":
      - start it, with `--port` and its default 7341, and `--runtime-dir`;
      - the Claude Code JSON (`type: http`, `url`, `headersHelper: "perf-agent-mcp-serve token"`) and the Codex TOML (`url`, `http_headers_helper`);
      - what a restart does to clients;
      - "do not configure both in one session: the same tools appear twice";
      - the inbox and `page_state`;
      - the threat-model delta;
      - the `captures-serve` default and `.owner`;
      - the `Serve.*` limits table and `MaxEngineWait`.
    - Line 43 names `page_state` among the decimal-string results.
    - No SSH or remote text anywhere.
  - **The guide** names the page tools as serve-only, and `page_state` for "what the user selected or asked about". It stays ≤ 12288 bytes.
  - **The skill**: line 7 now says the live page is `perf-agent-mcp-serve`'s, configured as an HTTP server, and that headless loops use stdio.
  - **`CHANGELOG.md`**: an `[Unreleased]` entry.

- [ ] **Step 1: Write the failing tests**

```go
func TestDocsListEveryServeLimit(t *testing.T)            // every Serve.* field and MaxEngineWait, with its default from DefaultLimits, appears in docs/mcp.md
func TestDocsShowBothClientConfigs(t *testing.T)          // "headersHelper", "http_headers_helper", "http://127.0.0.1:7341/mcp", "perf-agent-mcp-serve token"
func TestDocsNeverDescribeRemoteUse(t *testing.T)         // no "ssh", "-L ", "0.0.0.0", "--addr" in docs/mcp.md, the guide or the skill
// guide_test: + "page_state"; skill_test: the skill's names exist in the serve tool list
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Docs|Guide|Skill'`
Expected: FAIL.

- [ ] **Step 3: Write the text.**

- [ ] **Step 4: Run them to verify they pass**

Run: the green check.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add docs/mcp.md internal/perfmcp/guide.md internal/perfmcp/guide_test.go internal/perfmcp/docs_serve_test.go internal/perfmcp/skill_test.go skills/perf-agent-mcp/SKILL.md CHANGELOG.md
git commit -m "docs: the live server, its client configuration, the inbox and page_state"
```

### Task 15: Verification, real-client smoke, review and the draft PR

**Files:** none new. Transcripts and the PR body go under `.superpowers/sdd/2026-11-16-serve-plan/`.

- [ ] **Step 1: Run the whole suite.** `CGO_CFLAGS=… CGO_LDFLAGS=… go test -race ./...` must PASS. The darwin build of the green check must build.
- [ ] **Step 2: Lint.** `golangci-lint version` must say 2.11.4; install it if the command exits 127. `golangci-lint run ./...` must report no findings.
- [ ] **Step 3: Check the pre-existing tree is untouched.** `git diff --stat ai-mcp-m2c-capture -- main.go telemetry.go perfagent gpu cmd/flamegraph internal/profilemodel` must be empty.
- [ ] **Step 4: Run the Claude Code smoke.**
  - Build `perf-agent-mcp-serve` into the worktree, not `/tmp`.
  - Start it with a fixture profiles root and, optionally, `--perf-agent <setcap'd perf-agent>`.
  - Write a scratch JSON: `{"mcpServers":{"perf-agent-live":{"type":"http","url":"http://127.0.0.1:7341/mcp","headersHelper":"<abs path>/perf-agent-mcp-serve token"}}}`.
  - Run `claude -p --mcp-config <scratch json> --strict-mcp-config` through open, `page_explain` and `page_state`, plus a capture when perf-agent is given.
  - Then restart the server and ask again, recording how the client recovers from the 404 and the 401.
  - Then run two `claude -p` sessions at once on one page.
  - Save the transcripts.
- [ ] **Step 5: Run the Codex smoke.** Run `codex exec` with `-c mcp_servers.perf-agent-live.url="http://127.0.0.1:7341/mcp"` and `-c mcp_servers.perf-agent-live.http_headers_helper=…`, never editing `~/.codex/config.toml`. Record whether Codex re-runs the helper after a restart's 401, and how it handles the 400 fallback. If the overrides cannot express the helper, or would load the user's own servers, record the Codex smoke as a **manual step for the user**, with the exact TOML.
- [ ] **Step 6: Get the reviews.** Send the whole branch (4aea4a60..HEAD) to an opus whole-branch review and to the second reviewer through Workforce. Verify every claim, and record the rulings in the ledger.
- [ ] **Step 7: Push a draft PR** stacked on #265, with no AI attribution and no session URLs. **Do not merge**; ask the user.

```bash
git push -u origin ai-mcp-m2c-serve
gh pr create --draft --base ai-mcp-m2c-capture --title "perf-agent-mcp-serve: one local server for the live page and MCP over HTTP" --body-file .superpowers/sdd/2026-11-16-serve-plan/pr-body.md
```

---

## Self-review

**1. Spec coverage**

| Spec section | Task |
|---|---|
| §1.1, §1.2 (tiers) | Tier 0 in 2 and 11 (stdio links no networking; the 13-tool list stays pinned); tier 1 in 3, 6, 7 and 10; tier 2 in 8 and 9 |
| §1.3 (deferred) | Not built: the GET stream, SSE and progress, the 2026-07-28 binding, `serve stop` and a unit, the headless bridge, MCP Apps. The probe fallback is tested in 6 |
| §2 | 10, 12 and 13; reconciliation rows 1–3 |
| D1.1 | 1, 2, 10 and 11 |
| D1.2 | 10; reconciliation row 4 |
| D1.3 | 10 (runtime directory) and 14 |
| D2.1 | 3 (headers) and 6 |
| D2.2 | 4 (label) and 6 |
| D2.3 | 3, 10, 14 and 15 |
| D2.4 | 5 (slot, holder, wait), 7 (stages and classes), 10 (`TestOneCallRunsAtATime`) |
| D2.5 | 6 (deadlines, contexts), 12 and 13; reconciliation rows 5–11, 15 and 16 |
| D2.6 | 3 and 10 |
| D3 | 5 (`by`) and 12 (`job.by`, protection); D3.5 is deferred |
| D4 | 8 and 9 |
| D5 | 2, 8, 9, 10, 11 and 14 (#264 is already trimmed) |
| D6 | 3, 7, 10 and 14 |
| D7 | stdio in 2 and 11; serve network tests in 11; transport in 6 and 7; concurrency in 7 and 10; capture hosting in 12 and 13; writer ownership in 13; inbox in 8; runtime directory in 10; Chrome in 9 and 11; smoke in 15 |
| §4 | 3, 6, 8 and 10 |
| §5 | every task; the `attempts.go` row is deferred (Builder's calls) |
| §8 | Builder's calls: port 7341, `MaxOpenHandles` 32 |

**2. Step scan.**
- Each code step names its files, signatures and values. Task 13's Step 3 is open by design: it fixes what the process tests find.
- Each verification step runs the green check.
- No step uses a later task:
  - Task 2 removes the listener tests, and Tasks 10–11 bring them back under the names listed in Task 2;
  - Task 5's `HoldEngine` comes before Tasks 8, 10 and 12, which use it;
  - Task 4's `Snapshot` and `Read` come before Task 8;
  - `perfmcp.Server.MCP()` is declared in Task 10, where it is first used. Task 12's tests reach the same server internally.

**3. Type consistency.** Each name has one owning task:

| Task | Declares |
|---|---|
| 1 | `mcpflags.Kind`, `Register`, `Values`, `UsageError`, `SetLimit`, `CheckLimits`, `LimitFields`, `CheckCapturesDir` |
| 2 | `PageConfig`, `PageView`, `ErrPageNotFound`, `PageLimitError`, `perfpage.Routes`, `perfpage.NewHub` |
| 3 | `LoopbackHost`, `Front`, `MCPGuard`, `InboxGuard`, `Rate` |
| 4 | `mcpstdio.Session`, `NewSession`, `Request`, `Parse`, `Handle`, `Tool`/`Resource` lookups, `ClientLabel`, `WithReceived`, `Tool.Snapshot`, `Resource.Snapshot`/`Read` |
| 5 | `holder`, `engineHandler(tool, h)`, `MaxEngineWait`, `ServeLimits`, `Config.HoldEngine`, `by` fields |
| 6 | `mcphttp.Handler`, `Options`, `Transport`, `Stats`, `HeaderSession`, `HeaderVersion` |
| 7 | `Class`, `classify`, the admission `Options` fields |
| 8 | `stateBus`, `PageReport`, `ErrBadReport`, `Report`, `page_state`, `perfpage.Inbox` |
| 10 | `runtimeDir`, `serveInfo`, `DefaultPort`, `start`, `instance`, `Server.MCP` |
| 12 | `capturePublish`, `captureResult.Publication`/`DiffPublication`, `captureRequest.Show`, `job.by`, `listJob.By`, `publication.files`, `Config.Command`, `CaptureConfig.ReaperPause` |

`HoldEngine`, `configHook`, `killCaptures` and `closeServer` are named the same everywhere they appear.

**4. Review Focus.** The five tests sit in Tasks 6, 3, 10, 10 and 8. Inputs the spec covers already have their own tests:
- the Origin variants (3);
- slow bodies (7);
- an evicted publication's ask (8, `TestReportsAreValidatedAgainstTheSnapshot`, and 9, "a 422 says nothing was sent").

**5. Proportion.** The plan is about the spec's size in bytes (fewer lines). Its code blocks are test names with assertions and a few exact literals: the title format, the busy text, the JSON shapes. No function bodies.
