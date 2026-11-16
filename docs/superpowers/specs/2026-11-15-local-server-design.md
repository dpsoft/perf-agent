# M2c-serve: one long-lived local server for the human layer — design addendum

**Status:** design only. Not implemented, not committed (this directory is
git-ignored). Revised on 2026-11-15 with the user's answers (§1.1,
decision 8): no adapter, a fixed port, a separate serve binary, and
capture first. Revised again after Codex review #400 (§10: rulings
SRV-R1..R4 and CAP-R6), and after Codex review #405 (§10.2: SRV-R2b,
R3b, R4b, R5 and R6), after review #408 (§10.3: SRV-R7, SRV-R4c,
CAP-R9, R10 and R11), and after review #411 (§10.4: CAP-R13..R16). Addendum to
[2026-11-14-live-page-design.md](../../docs/superpowers/specs/2026-11-14-live-page-design.md)
(M2b, draft PR #264), and a companion of the capture design
[../capture/2026-11-15-capture-design.md](../capture/2026-11-15-capture-design.md)
(the capture tool). Where this addendum and M2b disagree, this addendum
wins. The capture design owns everything about `capture`; this document
only says how the server hosts it (§D2.5, §10).
**Date:** 2026-11-15
**Verified against:** the `ai-mcp-m2b` worktree at fcf46625
(`internal/livehttp`, `internal/perfmcp`, `internal/mcpstdio`,
`cmd/perf-agent-mcp`, `internal/flamegraph/live.js`), the M2b rulings
M2b-R1..R16 and the M2 rulings M2-R1..R32.

Who decides each point:
- **[user]**: the user decided it (2026-11-15). Not open.
- **[user?]**: needs the user's confirmation. The default holds until then
  (§12).
- **[mine]**: settled here. The user may overrule it.

---

## 1. Intent and priority

### 1.1 What the user decided (2026-11-15) **[user]**

1. **The per-session page goes.** In M2b, `perf-agent-mcp --live` starts a
   loopback HTTP server inside each stdio session's process. That page
   dies with the session. Every session spawns its own server, and the MCP
   client does not know the server exists.
2. **One long-lived local process** replaces it. The user starts it
   (working name `perf-agent-mcp serve`; decision 8.3 makes it a
   separate binary, `perf-agent-hub`), and it owns all state:
   - the open profiles;
   - the publications;
   - the attempt and rejection record;
   - what the user selects on the page.
3. That process **serves the live page** (reusing M2b) **and speaks MCP
   over Streamable HTTP** on loopback, behind a token. Claude Code and
   Codex connect to it as an HTTP MCP server. Several agents share it, and
   the page outlives their sessions.
4. **Interactivity without the page acting.** The user's clicks, focus and
   "ask about this" go into server state, and agents read them with a
   tool. The page never runs tools. **MCP sampling is not used.**
5. **The capture tool lives in this server too.** It is designed in the
   capture addendum.
6. **The main user is an autonomous agent running the loop** (capture →
   analyse → change code → re-capture → compare → repeat). The UI is
   secondary and is for humans who need to understand a result.
   Consequences:
   - the agent path works fully headless over plain stdio, with no server
     running;
   - the server and the page are an **optional human layer** that no
     agent depends on;
   - nothing here may add latency, state coupling or a required step to
     the headless loop.
7. **Scope of "don't touch".** The pre-existing perf-agent stays
   unchanged: the capture binary and its flags, `cmd/flamegraph` and its
   outputs, `gpu/*`, and `profilemodel`/`explain` semantics. The M2 and
   M2b MCP code (`internal/perfmcp`, `internal/mcpstdio`,
   `internal/livehttp`, `cmd/perf-agent-mcp`, the live assets) is ours and
   may be reshaped. The headless stdio path must stay first and simplest.
8. **Answers to this design's first questions (2026-11-15):**
   1. **No stdio → serve adapter.** Stdio `perf-agent-mcp` stays
      independent and never connects anywhere.
   2. **A fixed `127.0.0.1` port, set with `--port`.** It is still
      loopback-only, with no remote or SSH use. This amends M2b-R7: a
      port flag is allowed, an address flag never is.
   3. **A separate binary for the server: `cmd/perf-agent-hub`.**
      The stdio binary then again links no `net`, `net/http` or
      `crypto/tls`, so M2's "links no networking" test comes back for it.
      M2b's network tests (source scan, every-socket-loopback, `nm`) move
      to the serve binary.
   4. **Controller defaults, recorded:**
      - trim #264 and stack a new PR;
      - the `page_*` tools exist only in serve and are not listed in
        stdio;
      - no free text in "ask about this";
      - marking asks as read is fine;
      - putting headless-loop captures on the page is a later follow-up.
   5. **Order of work: the headless capture milestone ships first, the
      serve binary second.** §9 marks which parts of this design depend
      on capture.

### 1.2 Ranking of this design's parts

| Tier | Part | Why it is in this tier |
|---|---|---|
| 0: must not regress | Plain stdio `perf-agent-mcp`: M2's tools (plus `capture`/`capture_list` from the capture design); no page tools; links no networking; no discovery; no new step | It is the product's main path (decision 6) |
| 1: the human layer | `perf-agent-hub`, a separate binary: one listener carrying the page (M2b engine, reused) and MCP over Streamable HTTP; shared state; the engine slot (§D2.4); tokens in the runtime directory | This is the user's architecture |
| 2: interactivity | The inbox, `page_state`, and the `perf-agent://page/state` resource for Claude Code's `@` mentions | Useful only once tier 1 exists |
| Deferred | See §1.3 | Not needed for a first useful server |

### 1.3 Deferred, with reasons

- **The stdio → server adapter.** Dropped, not merely deferred: the
  user's decision 8.1 (§D1.3).
- **Server-initiated notifications.** No GET stream and no
  `resources/subscribe`:
  - Claude Code documents `list_changed` but not resource updates;
  - the 2026-07-28 protocol revision removes the GET stream and replaces
    it with `subscriptions/listen`;
  - building the 2025-11-25 mechanism now would mean building it twice.
- **SSE responses to POST, progress and server-to-client requests:**
  not built (SRV-R6). The transport answers every request with
  `application/json`. The two-step capture API (CAP-R8) keeps every call
  within about 45 s, so no call needs a stream. Elicitation is not used
  anywhere.
- **The 2026-07-28 binding** (sessionless, `Mcp-Method` headers). State is
  designed not to be session-keyed (§D3), so adding it later is a
  transport-only change.
- **`serve stop`, a systemd user unit, auto-start.** v1 runs in the
  foreground and stops with Ctrl-C or SIGTERM.
- **A filesystem bridge from headless runs to the page** (§D3.5). It is
  a later follow-up (decision 8.4).
- **MCP Apps** (as in M2b §10).

---

## 2. Architecture

```
 headless loop (main path)                human layer (optional)
 ┌───────────────────────┐                ┌──────────────────────────────────────────────────┐
 │ agent ─stdio─► perf-  │                │ perf-agent-hub  (one per user, foreground) │
 │ agent-mcp (M2 tools + │                │                                                  │
 │ capture). Links no    │                │  127.0.0.1:PORT ── livehttp.Front ──┬─ /mcp ──► mcphttp ──┐
 │ networking.           │                │                                     │                     │
 └──────────┬────────────┘                │   (Host check, headers, routing)    │  (Bearer MCP token,  │
            │ files only                  │                                     │   sessions, no Origin)│
            ▼                             │                                     ├─ /s/<page tok>/… GET ─► livehttp.Guard ─► perfpage routes (perfmcp snapshot)
   --profiles / captures / --output ◄─────┤                                     └─ /s/<page tok>/inbox POST ─► inbox guard ─► perfmcp inbox (own mutex)
                                          │                                                          │
                                          │  perfmcp.Server ◄── tool calls from every session ───────┘
                                          │    engine slot: one holder at a time owns store,
                                          │    diffs, attempts, publications, steering
                                          │    capture waits OUTSIDE the slot (capture design §9)
                                          └──────────────────────────────────────────────────┘
   Claude Code / Codex (type: http, url http://127.0.0.1:PORT/mcp, header from `perf-agent-hub token`)
   browser tab (http://127.0.0.1:PORT/s/<page token>/)
```

What the headless loop and the human layer share is **files**, never a
socket:
- **the profiles root and the output root**, both read-shared;
- **a capture directory, written by exactly one kind of binary (SRV-R5,
  CAP-R6, CAP-R6b).**
  - **Distinct defaults.** Stdio writes `<profiles>/captures` and serve
    writes `<profiles>/captures-hub`, so the two never meet unless the
    user points them at the same place.
  - **Ownership is enforced, not just documented.** On first use a
    captures directory records its writer kind in `.owner` (`stdio` or
    `serve`). Serve refuses a stdio-owned directory, and stdio a
    serve-owned one, with an error naming the owner and the other
    default.
  - **Within a kind, one process at a time.** The cross-process lease
    (CAP-R6) is a `flock` on an open file description. The reaper
    inherits it and holds it until both its children are reaped
    (CAP-R6b).
  - **Crashes.** If serve crashes or is killed mid-capture, the reaper
    keeps the lease while perf-agent finishes or is stopped. A capture
    job in a restarted serve, or in a second one, finds the lease held.
    The job takes the lease nonblocking, in `preparing`, before any
    filesystem side effect, so it **fails** with `busy` and the reaper's
    pid. The `capture` call itself still returned `accepted`, because the
    request does no I/O (CAP-R13). It runs no retention or recovery on
    that directory until the lease is free. Recovery then checks the
    reaper's identity and start time before trusting any intent record
    (CAP-R6b, CAP-R5b).
  So serve's retention, sequence numbers and pins can never change a
  headless loop's captures, and a later read-only page bridge may still
  read a stdio-owned directory.

---

## 3. Decisions

### D1. Process model, names, start and stop, discovery

#### D1.1 Two binaries **[user, decision 8.3]**

- **`perf-agent-mcp [flags]`** (`cmd/perf-agent-mcp`): stdio, exactly as
  M2 plus capture. It has no page tools and no `--live`. **It links no
  `net`, `net/http` or `crypto/tls`**, and M2's link test comes back for
  it.
- **`perf-agent-hub [flags]`** (`cmd/perf-agent-hub`, new):
  the long-lived server.
  - It takes the same root and limit flags as stdio: `--profiles`,
    `--source`, `--source-manifest`, `--output`, `--source-git`,
    `--limit`, and the capture flags from the capture design.
  - Both binaries parse these through one new shared package,
    `internal/mcpflags`, moved out of `cmd/perf-agent-mcp/main.go`.
  - It adds `--port` and `--runtime-dir`.
- **`perf-agent-hub token`** prints
  `{"Authorization":"Bearer <token>"}` for a client's header helper
  (§D2.3).
- **`perf-agent-hub url`** prints the page URL for the user.

**What the separate binary requires: `perfmcp` must stop importing
`net/http`.** Today two M2b files bring it in:
- `live.go`, for the listener lifecycle (which this design removes
  anyway) and `livehttp.Hub`;
- `liveroutes.go`, for the HTTP handlers.

The split **[mine]**:
- **What stays in `perfmcp`**, none of it network code:
  - the publications, the snapshot, the page tools, the inbox data and
    validation, and the engine slot;
  - `live.hub` becomes a notifier, `func(state []byte)`, set through
    `Config.Page`. `commit()` calls it, and `serve` points it at
    `Hub.Publish`.
- **`internal/perfpage`** (new, linked only by the serve binary) gets:
  - the M2b routes from `liveroutes.go`, moved unchanged;
  - the inbox HTTP handler.
  It reads the snapshot through a small read-only accessor that `perfmcp`
  exports (`Server.PageSnapshot()`, which returns an immutable view with
  the publication lookup and the frame, focus and export computations).
  It never touches engine state, which is M2b D4's rule written as a
  package boundary.
- **`internal/livehttp`** and **`internal/mcphttp`** are imported only by
  `perfpage` and `cmd/perf-agent-hub`.

The stdio binary's dependency closure then contains no networking
package, and the restored M2 test proves it (§D7).

#### D1.2 Start and stop **[mine]**

- **Start.** The user runs `perf-agent-hub --profiles DIR …` in a
  terminal. It runs in the foreground. It logs to stderr, and no log line
  ever carries a token. **The page URL is printed once to stdout**,
  because that terminal is the user. Unlike M2b, stderr here is not an
  MCP client's log.
- **One per user.** The server takes `flock` on
  `$RUNTIME/perf-agent-mcp/serve.lock`. A second instance exits with
  code 1, saying `perf-agent-hub is already running (pid N, port
  P)`.
- **Port [user, decision 8.2].** The server listens on a fixed loopback
  port, `127.0.0.1:<default>`, which `--port N` changes.
  - **This amends M2b-R7.** A port flag is allowed; an address flag never
    is. The host is always `127.0.0.1`, and there is no remote or SSH
    use.
  - `livehttp.CheckAddr` still refuses anything that is not loopback.
  - If the port is busy, the server refuses to start, naming the port and
    `--port`.
  - A fixed port is needed because an HTTP MCP client's config names a
    URL (§D2.3).
  - The default number is picked at implementation from an unassigned
    range.
- **Stop.** On SIGINT or SIGTERM:
  1. stop accepting connections;
  2. close the page hub (streams end, and pages show M2b's "not running"
     banner);
  3. kill running captures (capture design §6.6);
  4. give in-flight tool calls the 1 s grace of M2-R27, then exit;
  5. remove `hub.json` and the token file.
- **Restart rotates both tokens.** Clients recover: see §D2.3.

#### D1.3 Discovery, and what happens with no server **[user, decision 8.1]**

**Plain stdio never discovers, forwards to or connects to anything.** The
first direction mentioned a thin stdio adapter that would forward to a
running server. The user dropped it, for these reasons:
- **It would couple the headless loop to hidden state.** The same config
  would behave differently depending on whether some terminal happens to
  run `serve`: other agents' handles, evictions and steering, with added
  latency. That breaks decision 6.
- **It needs an outbound dial.** Forwarding means a loopback client
  connection from the stdio process, which reverses M2b's "no client
  networking anywhere" source scan for the main path.
- **It is not needed.** Both target clients speak Streamable HTTP
  natively (§7).

So:
- **No server running:** stdio works standalone, exactly as M2 plus
  capture. It uses static files under `--output` and opens no network
  socket.
- **An interactive session with a human watching:** the user configures
  that client with the HTTP entry (`perf-agent-live` →
  `http://127.0.0.1:PORT/mcp`), served by `perf-agent-hub`.
- **Do not configure both in one session.** The tool names would collide
  (`mcp__perf-agent__open` against `mcp__perf-agent-live__open`). The
  docs say so.
- **A client configured for HTTP while no server runs** shows the server
  as failed to connect, and the agent has no perf-agent tools. This is
  visible and harmless. Headless configurations use stdio and never hit
  it.

The **runtime directory** exists for the server's own use (lock, token,
`hub.json`), not for discovery by stdio:
- it is `$XDG_RUNTIME_DIR/perf-agent-mcp/`, created with mode 0700;
- the server refuses a directory that is not owned by the uid or is not
  0700;
- without `XDG_RUNTIME_DIR`, the server refuses to start unless
  `--runtime-dir DIR` names a directory that passes the same checks.
  Tokens are never written to a persistent disk by default (tmpfs,
  cleared at logout).

`hub.json` (0600) holds `{pid, port, mcp_url, started, version}` and no
token. The token is kept apart in `mcp-token` (0600), and the page URL in
`page-url` (0600).

---

### D2. MCP over HTTP

#### D2.1 Endpoint and protocol **[mine]**

- **One endpoint:** `http://127.0.0.1:PORT/mcp`. The spec requires a single
  MCP endpoint path ([MCP 2025-11-25, Transports]).
- **Revision:** 2025-11-25, which `mcpstdio` already speaks (2025-06-18 is
  also accepted).
- **POST** carries one JSON-RPC message:
  - a request gets `200 application/json` with one response (no SSE in
    v1);
  - a notification gets `202` with no body;
  - a JSON-RPC response from a client gets `202` and is dropped, as
    `mcpstdio` does today;
  - a batch is refused, as in `mcpstdio`.
- **Requests must carry:**
  - `Accept` listing both `application/json` and `text/event-stream`;
  - `Content-Type: application/json`.
  Otherwise the answer is `400`.
- **GET gets `405`.** The spec allows it: "the server MUST either return
  `Content-Type: text/event-stream` … or else return HTTP 405" (§1.3).
- **DELETE** ends the session: `204`, or `404` if the session is unknown.
- **`MCP-Protocol-Version`** must be one of the supported versions. A
  missing header means the version negotiated for that session. An
  unsupported version gets `400`, as the spec requires.
- **Clients on the 2026-07-28 revision** probe with modern requests first
  (M2 Review Focus 2). The server answers such a probe with a `400` whose
  body is not a recognised modern error, which per that revision's
  backward-compatibility section makes the client fall back to
  `initialize`. The real-client smoke tests this (§D7).

#### D2.2 Sessions **[mine]**

- **Issued on initialize.** A successful `initialize` mints
  `Mcp-Session-Id`: 32 random bytes, base64url.
- **Required afterwards.** A later request without it gets `400`, and an
  unknown one `404`. Per the spec, the client then re-initializes.
- **What a session holds is protocol state only:**
  - the `initialized` flag and the negotiated version;
  - a **client label** (`clientInfo.name`, at most 64 bytes, printable
    ASCII, else `client`);
  - its in-flight request ids, so that `notifications/cancelled` can be
    scoped to the session that sent the request (capture design §9).
    **Capture jobs are not session state (SRV-R7):** the server owns
    them, and no session's end affects them.
- **Sessions never authenticate.** "MCP Servers MUST NOT use sessions for
  authentication" ([MCP 2025-11-25, Security Best Practices]). Every
  request is checked against the bearer token (§D2.3).
- **Bounded.** At most `MaxSessions` (default 32), least recently used
  evicted. An evicted session's next request gets `404`, and the client
  re-initializes without the user noticing. There is no idle timeout:
  Claude Code sessions sit idle for hours.
- **Nothing durable is keyed by session** (§D3). Losing a session loses
  no work.

#### D2.3 The token: a header, not the URL **[mine]**

- **The MCP token** is 32 random bytes, base64url, created at each start
  and written to `$RUNTIME/perf-agent-mcp/mcp-token` (0600).
- **It is sent as `Authorization: Bearer <token>`, never in the URL.**
  - URLs end up in client config files, process listings, logs and
    `claude mcp list` output.
  - A header lets the client fetch the token fresh on each connection.
- **It is compared in constant time.** Missing or wrong gets `401` with a
  plain body and **no `WWW-Authenticate` resource metadata**, so no client
  is pointed into an OAuth discovery flow. The real-client smoke records
  what each client does on a `401`.
- **The page token is separate** (M2b's `/s/<token>/` path token, also
  new at each start). The page token can read the page and append to the
  inbox, and nothing else. The MCP token can call tools, including
  `capture`. If a page URL leaks (browser history, a screen share), no
  tool call follows.

**Client configuration** (both clients support header helpers, so a
restart's rotated token is picked up without editing config):

```jsonc
// Claude Code (.mcp.json or `claude mcp add-json`)
{ "mcpServers": { "perf-agent-live": {
    "type": "http",
    "url": "http://127.0.0.1:PORT/mcp",
    "headersHelper": "perf-agent-hub token" } } }
```

```toml
# Codex (~/.codex/config.toml)
[mcp_servers.perf-agent-live]
url = "http://127.0.0.1:PORT/mcp"
http_headers_helper = "perf-agent-hub token"
```

- **Claude Code** runs the helper on every connection, and "On a 401 or
  403, it re-runs and the call is retried once" ([Claude Code MCP
  docs]). So a server restart costs one retried call.
- **Codex** documents `http_headers_helper`, but not whether it re-runs
  on a `401`. That is to be checked in the smoke. The fallback is a Codex
  restart, or `bearer_token_env_var`, which goes stale on rotation.

#### D2.4 Concurrency: the engine slot replaces "only the request goroutine writes" **[mine]**

M2b's invariant is that the `mcpstdio` request goroutine alone writes the
store, the diffs, the attempts, the publications and the steering. With
several HTTP sessions there are many request goroutines.

**New invariant.** `perfmcp.Server` owns a one-slot semaphore, the
**engine slot** (`chan struct{}` of capacity 1, so acquiring it can honour
a context). **Only code holding the slot reads or writes** the store,
diffs, attempts, publication list, steering, or calls `commit()`.
Everything M2b says about "the request goroutine" now says "the slot
holder".

- **Where it is taken.** In `perfmcp`, not in the transport. `tools()`
  wraps every handler as `s.engine.run(ctx, handler)`, with **two
  exceptions**:
  - the capture tools never take it in the request path (capture design
    §4.0):
    - `capture` does a pure argument check plus the in-memory
      active-job admission, with no I/O, and enqueues a server-owned
      job. The job's goroutine takes the engine slot like any tool call,
      for prepare's shared inputs and for the derivations;
    - `capture_wait` and `capture_list` read the job registry, or
      committed manifests through the bounded lookup workers;
    - `capture_cancel` is a control message that only sets a flag
      (CAP-R10);
  - `page_state` (the tool and the resource) **never** takes the slot. It
    reads the atomic snapshot and the inbox only (SRV-R3b, §D4).
  Taking the slot in `perfmcp` keeps the rule transport-agnostic.
- **Cost on the headless path.** In stdio mode the slot is contended
  only by that session's own tool calls and its capture job's engine
  steps (prepare's shared inputs and the post-commit derivations,
  capture design §4.0), so a tool call may briefly wait behind a
  derivation. Uncontended, it costs one channel send and receive,
  nanoseconds. No other behaviour changes.
- **Page HTTP handlers are unchanged.** They read only the
  `atomic.Pointer[liveSnapshot]` and the immutable publications it holds
  (M2b D4, R5), so the page never waits on the slot. The agent never
  waits on the page either.
- **Admission is bounded (SRV-R4, R4b, R4c).** `MaxEngineWait` bounds
  time, not capacity. In 2025-11-25 every message is a `POST /mcp`, so
  nothing tells a tool call from a control message until the body is
  parsed. The transport therefore admits in four bounded stages:
  1. **Header-only rejection first.** These checks all run before any
     body byte is read (§D2.6):
     - Front's Host check;
     - the MCP guard's `Origin`/`Sec-Fetch-*` refusal;
     - the bearer token;
     - `Content-Type`;
     - `Content-Length`, which is required (a chunked body gets `411`);
     - `MaxMessageBytes` (over it gets `413`).
  2. **Bounded ingress (SRV-R4c).** A body is read only if both of these
     have room. Otherwise the answer is `503` at once, without reading.
     - **A reader count:** at most `MaxIngressReaders` (default 16)
       bodies are being read at once, server-wide.
     - **A byte budget:** `MaxIngressBytes` (default 8 MiB), reserved
       from `Content-Length` before reading.

     Each body must arrive in full within **`IngressBodyTimeout`**
     (default 2 s; a loopback client sends 4 MiB in milliseconds), or the
     connection is dropped. Reader and bytes are released as soon as the
     message is parsed, or refused.
  3. **Parse, then classify.** Classification happens **only after**
     parsing: the server reads the JSON-RPC `method`, `id` and, for
     `tools/call`, the tool name. Then it admits the message into its
     class:

     | Class | Members | Bound | When full |
     |---|---|---|---|
     | **control** | `notifications/cancelled`, `ping`, `initialize`, `tools/list` and the other list methods, `DELETE`, and **`capture_cancel`** (CAP-R10) | `MaxControlInFlight` (default 32), apart from every other class | `503` |
     | **page_state** | the `page_state` tool and `resources/read` of `perf-agent://page/state` | `MaxPageStateInFlight` (default 4) | `503` |
     | **calls** | every other `tools/call` and `resources/read`, `capture_wait` included | `MaxCalls` (default 16) active plus queued server-wide, `MaxCallsPerSession` (default 4), and `MaxRequestBytes` (default 16 MiB) of retained decoded arguments; never the raw body | `503` with a JSON-RPC error carrying the request's id, saying which cap was hit |
  4. **Dispatch.** Control and `page_state` messages are answered inline
     and never wait for the engine slot. Calls that need the engine queue
     for it. `capture_wait` and `capture_list` read the job registry and
     do not queue.

  **The overload guarantee, stated conditionally (SRV-R4c):**
  - Once a message has been read and parsed, a control message is served
    ahead of calls, from its own class, and never waits for the engine.
  - A full control class gets `503`.
  - **Under a slow-body flood**, while `MaxIngressReaders` readers are
    busy, **every new request, controls included, is refused** with
    `503`, until readers free. That happens within `IngressBodyTimeout`.
  - Only the token holder can mount such a flood, since every request
    is authenticated from its headers before its body is read.
- **Waiting.** An admitted call waits for the slot in FIFO order.
  - **Its context is not `Request.Context`.** That one ends when the
    connection closes, and a dropped connection is not a cancel (capture
    design §9). Each call's context derives from the server's lifetime
    context, with an explicit cancel function registered in a
    server-level in-flight table keyed by (session, request id).
    - `notifications/cancelled` from that session cancels it.
    - `DELETE` of that session cancels its in-flight **requests**,
      because the owner has ended the session. It **never cancels a
      capture job** (SRV-R7). A cancelled `capture_wait` request just
      stops waiting.
    - Server shutdown cancels everything.
    - **Ownership survives the session table:** LRU eviction never picks
      a session with in-flight requests. If every session slot is pinned
      this way, `initialize` gets `503`. Capture jobs pin no session:
      they outlive requests and sessions alike, and eviction never
      touches them (SRV-R7).
  - If its context ends while it waits, it leaves the queue and runs
    nothing.
  - After `MaxEngineWait` (default 45 s, under Codex's default 60 s tool
    timeout) it returns `isError`: `perf-agent-mcp is busy with <tool>
    from <client label> (running Ns); call again`. The other client's
    label and tool name are the user's own information.
- **The inbox is not engine state.** It has its own mutex, held only for
  appends and copies (§D4), so a page click never waits on a tool call.
- **Why not finer locks.** `store.view` caches views lazily, and `touch`
  reorders the LRU on every read. So even the "read" tools write, and an
  RWMutex would be wrong. A lock per structure would mean auditing every
  handler for lock order, for no gain at the scale of one user and a few
  agents. Read concurrency can come later behind the same slot API.

#### D2.5 Hosting the capture tool **[CAP-R13: the capture design defines the lifecycle; only serve-specific parts here]**

**The capture design is the single definition of a capture's lifecycle
(CAP-R13).** It defines:
- the job state machine, the tools and their result shapes (its §4.0);
- the files, the manifest as commit point, retention and the
  intent/commit protocol (its §7);
- the stdio and serve hosting rules (its §9).

This design keeps no copy of any of it. In short, as the capture design
states it:
- a request does a pure argument check and an in-memory active-job
  admission, with no I/O;
- the job takes the nonblocking captures-directory lease before any
  filesystem side effect, so cross-process `busy` is an **async job
  failure**, not a synchronous refusal;
- the engine is held for prepare's shared inputs (baseline, pins,
  protected inputs, retention) and for derivation and pin release;
- the child wait and the file commit run outside the engine, under the
  lease.

**What serve adds:**

- **The engine is serve's engine slot (§D2.4).** The capture job takes
  it like any tool call, for exactly the steps the capture design names.
  Waiting for it is bounded by `MaxEngineWait` like any caller.
- **Admission classes (§D2.4).**
  - `capture`, `capture_wait` and `capture_list` are in the **calls**
    class, so they count against `MaxCalls` and `MaxCallsPerSession`.
    They never queue for the engine slot.
  - `capture_cancel` is in the **control** class (CAP-R10).
- **Result shapes on the wire** are the capture design's:
  - `capture` → `{job, status:"accepted", eta}`, or a synchronous
    `invalid_argument` or in-process `busy`;
  - every later outcome, cross-process `busy` included, appears as the
    job's state through `capture_wait`.
- **Page publication of results.** In serve, a committed capture's
  post-commit derivations include the facts-only publication (capture
  design §4.0, `done` row), made under the engine slot with `by` set to
  the label of the session that started the job. A derivation failure,
  the publication's included, is reported (`derived: failed`) and never
  rolls files back.
- **Multi-session waits (SRV-R7).** Jobs are the server's. Any session
  holding the MCP token may `capture_wait` or `capture_cancel` any job,
  and several sessions may wait on one job at once, each bounded by its
  own timeout (capped at 45 s). `DELETE` and session eviction never
  cancel a job, and `notifications/cancelled` ends only the wait it
  names.
- **Historical lookups (CAP-R14).** After a registry eviction or a
  restart, a `capture_wait` by capture ID reads the committed manifest
  through the capture design's bounded lookup workers (2 workers, a
  bounded queue). The wait waits only to its own deadline and returns
  `loading` otherwise. Serve starts no goroutine per timed-out wait.
- **Where results live (CAP-R11, CAP-R17).**
  - The outcome, settings, notes and error go in the committed series
    manifest.
  - **Derived results** (the `open` and diff summaries, and serve's
    publication ID) **live only in the in-memory job registry.** Nothing
    derived is persisted: there is no sidecar and no journal.
  - After a registry eviction or a restart, `capture_wait` answers
    terminally from the manifest with `derived: "recompute"`, plus the
    exact `open` and `diff` calls to run (and, in serve, `page_explain`
    with `explanations: []` to publish again).
- **Bounds and names (CAP-R16).** The registry is the capture design's:
  - `Capture.MaxJobs` 32 and `Capture.MaxJobBytes` 4 MiB;
  - a result whose handles are gone says `"handles": "reopen"`, with the
    arguments to recreate them.
- **Cancellation (CAP-R16).** The linearization point is the manifest
  write. `capture_cancel` succeeds until the manifest is committed.
  Afterwards it answers "too late: committed", and the files stay.

**Long calls.** `livehttp.NewServer`'s `WriteTimeout` (30 s) would cut a
`capture_wait` or a call that waits for the slot (up to 45 s). The `/mcp`
handler therefore:
1. reads the body under a 30 s read deadline;
2. **clears the read deadline** before any wait;
3. clears the write deadline on entry, through `http.ResponseController`;
4. sets a 10 s write deadline just before writing the response, so a
   client that stops reading is bounded.

Tests pin each step.

**JSON only (SRV-R6).** Every response is one `application/json` body.
The server never opens an SSE response to a POST, sends no progress
notifications and makes no server-to-client requests: there is no
sampling and no elicitation. The capture design drops its POST-SSE and
progress requirement accordingly.

#### D2.6 One front door, three route classes **[mine]**

Every request goes through **`livehttp.Front`**, which is new:
- it sets M2b's security headers;
- it checks `Host` with M2b's `loopbackHost` (localhost or a loopback
  literal, with a port), else `403`;
- it bounds headers through `NewServer`;
- it then routes by path. Everything else gets `404`.

| Route | Methods | Guard (after Front) |
|---|---|---|
| `/mcp` | POST, DELETE (GET → 405) | **MCP guard:** any `Origin` header → 403, including an empty value or a repeated header (the spec says a present, invalid Origin gets 403, and for this endpoint every Origin is invalid: no browser is a client); any `Sec-Fetch-*` header → 403; Bearer token (constant time) → else 401; `Content-Type: application/json`; `Content-Length` required (chunked → 411); body ≤ `MaxMessageBytes` (`http.MaxBytesReader`, 413 over) read under a 30 s read deadline; the admission caps (§D2.4) |
| `/s/<page token>/…` | GET, HEAD | **`livehttp.Guard` unchanged** (M2b D5, R6, R8): body refused, same-origin fetch metadata, shell navigation exception |
| `/s/<page token>/inbox` | POST | **Inbox guard:** page token; `Origin` must appear exactly once, be non-empty and equal `http://` + Host; `Sec-Fetch-Site` must be present and `same-origin`; `Content-Type: application/json` (a non-simple type, so a cross-origin request needs a preflight that never succeeds: no CORS); custom header `X-Perf-Agent-Page: 1`; body ≤ 1 KiB under a 2 s read deadline; rate 5 per second per server (token bucket), else 429 |

**Order of checks.** Every guard runs completely **before** any body
byte is read and before dispatch. A refused request is answered from its
headers alone, and is dropped with `Connection: close` and a short read
deadline, as M2b-R8 does.

**Why the MCP route refuses browsers outright.** It makes the strongest
reading of the spec's Origin rule. A page that XSSes the live page, or a
DNS-rebinding page, sends `Origin`, so it cannot reach tools even with the
MCP token. And neither has the token.

---

### D3. State ownership across sessions **[mine]**

**Everything engine-side is shared.** One user runs several agents, and
they are all that user's. The trust boundary is the token, not the agent.

| State | Scope | Notes |
|---|---|---|
| Handles (store) | shared | Handles are content digests (`profilemodel` derives them from the profile digest and execlog), so two agents opening the same file get the same handle. `MaxOpenHandles` is shared: one agent's opens can evict another's handle, which gets M2's error "call open(path) again", deterministic and safe. `serve` raises the default to 32 (stdio keeps 8) |
| Diff cache | shared | as above |
| Attempts record | shared, keyed by (digest, view) as M2-R18 | It must be shared: M2b's anti-goal "a live page can show a view without its rejections" requires a rejection by any agent to appear on every publication of that key. IDs `a1, a2, …` are server-wide. A retry may name any agent's attempt of the same key |
| Publications, steering | shared; last writer wins | The page is one screen. Each publication and each steering change records `by` (the session's client label), and the page shows "p4 · codex" |
| Captures | shared and **server-owned jobs** (capture design §4.0, §9; SRV-R7) | one active, server-wide. Any session may wait on or cancel any job; no session's end cancels one. Derived results are in the bounded registry only (`Capture.MaxJobs`, `Capture.MaxJobBytes`); the committed manifest is the only thing on disk, and after eviction it answers `derived: "recompute"` (CAP-R17). `capture_list` shows all |
| Inbox, user view | shared | §D4 |
| Session | per session | protocol state and label only (§D2.2) |

**What one agent can see or change of another's work:** everything,
through the same tools. It can read another agent's handles, retry its
attempts, steer the page away from its publication, or open enough
profiles to evict its handles. It cannot forge another's label or erase
a rejection: the record is append-only within its bounds. The design does
not pretend to isolate agents, and the docs say so.

**Attribution is display only.** The label is the client's own claim.
Recording `by` changes the **live** state and history JSON only (an added
field). `render_flamegraph`'s static page and its goldens do not change,
and in stdio mode the label is empty.

**D3.5 A bridge from headless runs (a later follow-up, decision 8.4).**
The serve process can read the profiles root, and with it a headless
loop's stdio-owned capture directory, **read-only**. SRV-R5's `.owner`
check stops it from ever writing to that directory or running retention
on it. A later user action on the
page (list the captures on disk, and publish one facts-only) would let a
human look at what an autonomous loop produced, with no coupling beyond
reading files. This is not in v1.

---

### D4. The inbox and `page_state` **[mine; no free text and read marking: user, decision 8.4]**

**What the page reports** (from `live.js`, by POST to `inbox`, with the
body `{kind, publication, node, focus}`):
- `select`: the user selected a frame (local navigation);
- `focus`: the user zoomed to a node, or back to the whole view
  (`node: ""`);
- `ask`: the user pressed **"Ask my agent about this"**, a new chip next
  to "Focus here" in the panel.

The server **validates against the current snapshot**:
- `publication` must be kept;
- `node` must be a node of its view, or a union node for a diff, found
  through the same lookups the frame route uses (M2b R3).

It derives every displayed value itself (the frame name capped at 1 KiB as
in M2-R24, the view, the digest and the time). Nothing the page sends is
stored as text. An invalid report gets `422` and is not recorded.

**Two structures, both behind the inbox mutex:**
- **`user`**: the latest `select` and `focus` (publication, node, name,
  focus, time). Selections **coalesce**: only the latest is kept, so
  clicking around creates no backlog.
- **`asks`**: a ring of at most `MaxInboxEntries` (default 32) `ask`
  entries, with IDs `i1, i2, …` and a `dropped` count. A duplicate ask
  (same publication and node within 2 s) is ignored.
  Each entry: `{id, publication, node, name, view, digest, at, read_by}`.

**Free text is not accepted in v1.** An `ask` carries no typed question:
the user types the question in the agent's chat, which keeps M2b's rule
that follow-ups go through the agent. It also means a stolen page token
cannot inject instructions into an agent's context: the most it can
inject is a node ID the server resolved. **[user, decision 8.4]**

**How agents read it (SRV-R3b).** `page_state` is a **snapshot-and-inbox
handler**:
- it never takes the engine slot;
- it reads the atomic `liveSnapshot` (for `agent`) and copies the inbox
  under the inbox mutex (for `user`, `asks` and the `read_by` marking);
- it has its own small admission class (§D2.4).

So it answers at once while another agent's long tool holds the slot, and
it can never stall the engine. The tool and the resource share one
handler. It is a new tool, listed in serve mode only:
- `page_state(since?, limit?)` returns:

```json
{"viewers":"1",
 "agent":{"seq":"12","current":"p3","selected":"9f…","focus":"","by":"claude-code"},
 "user":{"publication":"p3","selected":"a1…","name":"runtime.mallocgc","focus":"","at":"2026-11-15T10:02:03Z"},
 "asks":{"rows":[{"id":"i7","publication":"p3","node":"a1…","name":"runtime.mallocgc","view":"cpu/nanoseconds#1","digest":"…","at":"…","read_by":""}],
         "next":"i7","dropped":"0"}}
```
  Counts are strings (the M2 wire rule). `user` is absent until the page
  reports something.
- **Acknowledgement is a cursor.** The agent passes `since: "i7"` next
  time. There is no per-agent server state, so several agents each keep
  their own cursor and it survives session churn.
- **Reads are marked.** The first `page_state` that returns an ask
  records the reader's label in `read_by`, and the page shows "read by
  codex". That makes the tool not strictly read-only, so its annotations
  are `readOnlyHint:false, idempotentHint:true`.
- **The resource `perf-agent://page/state`** returns the same JSON, with
  the last 8 asks and no cursor. Claude Code attaches a referenced
  resource to the prompt, so the user can write "explain
  @perf-agent-live:perf-agent://page/state" right after clicking. That is
  interactivity with no push at all.
- **Instructions** (serve mode only) add one line: when the user refers to
  something they selected or asked about on the page, call `page_state`.

**Notifications: none in v1** (§1.3). Agents learn of a click when the
user tells them or when they look. That is acceptable, because the user
is in the chat anyway.

**The page side.** After an `ask`, a toast says "Sent. Ask your agent to
look (it reads page_state)." Every tab shows the inbox count.

**The state envelope carries two independent revisions (SRV-R3).** The
problem with the obvious approach:
- in M2b, `commit()` is the only writer of the snapshot, and every commit
  bumps `seq`;
- `live.js` drops a state whose `seq` is not newer than the one it has
  accepted (M2b-R1).

So inbox counts added to the engine state could not reach a second tab
without a commit, and a commit would couple the inbox to the engine.
Instead:

- **Two parts, two writers.** The `state` route and every `event: state`
  carry
  `{"engine":{"seq":"N",…the M2b state…},"inbox":{"rev":"M","asks":"n","unread":"m"}}`.
  - `engine` is written only by `commit()`, under the slot, exactly as in
    M2b.
  - `inbox` is written only by inbox changes (an append, or `page_state`
    marking `read_by`), under the inbox mutex. Each change bumps `rev`
    and never `seq`.
- **The state bus combines them.** It is a small struct with its own
  mutex, holding the latest bytes of each part. Each writer replaces its
  part, then builds the envelope and publishes it to the hub while
  holding that mutex. The hub never blocks (M2b D2), so the mutex is held
  only to build the frame. `GET state` reads the same combined bytes.
  Neither writer ever waits for the other's lock (the engine slot or the
  inbox mutex).
- **`live.js` accepts each part by its own revision.**
  - `engine` goes through the M2b-R1/R9..R12 logic unchanged, keyed on
    `seq`.
  - `inbox` is applied when `rev` is newer than the last accepted rev,
    compared as a decimal string (by length, then lexically), whatever the
    `seq`.
  - An envelope whose `seq` is old but whose `rev` is new updates only
    the inbox, and the reverse. Neither part's arrival resets the other's
    state or generation.

#### Why not MCP sampling **[user decision; reasons recorded]**

Sampling lets a server ask the client's model for a completion.
1. **It would make the page an LLM trigger.** A click would cause model
   work without the user asking in the chat. That is "the page acting" in
   all but name, and it breaks M2b's rule that follow-ups go through the
   agent.
2. **It is outside the evidence rules.** A sampled completion is not a
   tool call: its prose would bypass `explain.Check` unless it were routed
   back through `page_explain`, and it would run without the
   conversation's context.
3. **There is no right client.** With several sessions, the server would
   have to pick whose model, tokens and permission prompt to use.
4. **Consent and support.** The spec has the client keep a human in the
   loop for each sampling request. Claude Code's MCP docs list
   elicitation as supported but do not cover sampling. Codex's docs list
   neither.
5. **The protocol is moving away from it.** Revision 2026-07-28 removes
   server-initiated requests altogether: sampling exists only as an
   input request embedded in the result of a client's own request (MRTR).
   A page-triggered completion is impossible by construction there.

---

### D5. What of PR #264 is kept, moved or dropped

| #264 task | Fate | What changes |
|---|---|---|
| 1. Loopback listener and request guard | **Kept** | `Guard`, `Listen`, `CheckAddr` and `NewToken` unchanged; `Listen` is still the only listening call. **Added:** `Front`, the MCP guard and the inbox guard in new files (§D2.6), and `loopbackHost` exported for them |
| 2. SSE hub | **Kept** unchanged | serves page streams; MCP has no stream in v1 |
| 3. Shared panel script and CSS | **Kept** unchanged | — |
| 4. Live models (JSON) | **Kept** | the state becomes an envelope with `engine` (the M2b state, plus `by`) and `inbox` (its own `rev`) (SRV-R3) |
| 5. Live shell, CSS and script | **Kept, extended** | `transport.post(path, body)`; local select and focus report to `inbox`; the "Ask my agent about this" chip; the inbox count. `onState` reads the envelope: `engine` through the unchanged R1/R9..R12 logic, `inbox` by its own `rev` (SRV-R3). Reports are fire-and-forget and never touch `acceptedSeq` or the generation |
| 6. Publications and snapshot | **Kept** | the ownership comments move from "request goroutine" to "engine slot holder"; `by` recorded |
| 7. Listener lifecycle and routes | **Routes moved; lifecycle moved** | `perfmcp` no longer listens: `live.start`, `ensureLive`'s listener branch and `live.srv/ln/mu` go, and the hub becomes a notifier. `liveroutes.go` moves, unchanged in behaviour, to `internal/perfpage`, which reads `Server.PageSnapshot()`. `perf-agent-hub` mounts it with the inbox handler, and `page_open` returns the URL the serve binary gives it |
| 8. `page_open`, `page_explain`, `page_show` | **Kept; serve-only** | registered only when `Config.Page` is set (decision 8.4). Stdio does not list the page tools, so a headless agent's tool list and context are M2's |
| 9. Flat live diff model | **Kept** unchanged | — |
| 10. `page_diff` | **Kept; serve-only** | as for 8 |
| 11. Flags, network tests, guide, docs | **Reworked** | `--live` dropped. The network tests (source scan, every-socket-loopback, `nm`) move to `cmd/perf-agent-hub`, and M2's "links no networking" test comes back for `cmd/perf-agent-mcp` (§D7). The guide's live section moves to serve-mode instructions. `docs/mcp.md` gets a "watching with the live server" section with both client configs |
| 12. Verification and push | **Redone** | a new smoke with real Claude Code and Codex over HTTP |

**Migration path [user, decision 8.4]: trim #264, then stack. Capture
ships first, the server second (decision 8.5).**
1. **Trim #264.** On `ai-mcp-m2b`, one commit removes the `--live` flag
   and the stdio registration of the `page_*` tools.
   - #264 then ships the reviewed page engine with no per-session server
     exposed, and its stdio tool list is M2's.
   - Its 12 tasks' reviews stay valid, because nothing reviewed is
     rewritten.
   - Until the server PR, the stdio binary still links `net/http` through
     the page code it no longer reaches. The M2b network tests keep
     guarding it in the meantime.
2. **Capture**, its own PR stacked on the trimmed #264, built per the
   capture design. It is headless and stdio-only, and adds
   `capture`/`capture_list` to `tools()`.
3. **The server**, a new draft PR stacked on capture (branch
   `ai-mcp-m2c-serve`). It adds:
   - the `perfmcp`/`perfpage` split, after which the stdio binary links no
     networking and the M2 link test is restored;
   - `cmd/perf-agent-hub` and `internal/mcpflags`;
   - `mcphttp`, the engine slot, the inbox and `page_state`;
   - the slot-held prepare and commit steps of `capture`, in SRV-R2b's
     order, with the two-step `capture`/`capture_wait` API (CAP-R8);
   - the moved network tests and the docs.

The alternative, amending #264 in place, would mix two milestones in one
review and invalidate the recorded per-task reviews.

---

### D6. Security and threat model delta **[mine]**

What is new: the server is **long-lived**, carries **tool-calling
authority over TCP**, and accepts **one kind of browser write**.

| Adversary | Can | Cannot, because |
|---|---|---|
| Another local **user** | connect to `127.0.0.1:PORT` | guess either 256-bit token, or read the 0600 token files in the 0700 per-uid runtime directory. Both tokens compare in constant time |
| A process with **the user's uid** (malware, a compromised tool) | read `mcp-token` and call every tool, `capture` included | do more than it already could: it can read the profiles itself and run the setcap'd `perf-agent` directly (the capture design's §12 makes the same point). Out of scope, as in M2b. The stdio-over-HTTP design keeps this cost visible: it is why the default stays stdio for headless use |
| A **web page** in the user's browser | send requests to `127.0.0.1:*`, including a cross-site navigation to the page shell (M2b-R6) | **MCP:** it has no bearer token, cannot set `Authorization` cross-origin without a CORS preflight that never succeeds, and any request it makes carries `Origin` and `Sec-Fetch-Site`, which `/mcp` refuses. **Inbox:** it lacks the page token; with a leaked token it still fails the `Origin`, `Sec-Fetch-Site`, content-type and custom-header checks |
| A **DNS-rebinding** page | reach the port under its own hostname | pass `Front`'s Host check |
| Someone **with the page URL** (history, screen share) | read the page and append `select`, `focus` and `ask` reports | call a tool, or inject text: reports are validated IDs, and names come from the engine (§D4). Rotation: restart `serve` |
| A **misbehaving agent** | flood tool calls, steer the page, evict others' handles | exhaust memory or goroutines: `MaxCalls`, `MaxCallsPerSession` and `MaxRequestBytes` refuse excess at once (SRV-R4). It also cannot starve the page (snapshot reads), delay a parsed control message behind calls or the engine (SRV-R4b), or hold the slot forever (`MaxEngineWait`); every tool keeps M2's bounds. **With the token it can flood slow bodies:** while `MaxIngressReaders` are busy, new requests, controls included, get `503` until readers free, within `IngressBodyTimeout` (SRV-R4c, stated conditionally). The flood holds no engine state, and capture jobs keep running |
| **Hostile profile content** | as in M2b | as in M2b; the inbox stores no page-supplied text |

**Long-lived consequences:**
- **Memory.** It is bounded as in M2b (`MaxRetainedBytes`, an estimated
  2 GiB, plus the handle, diff and attempt bounds), but held for the
  process's life instead of one session's. `serve` logs its retained
  estimate at each publication (no token in it).
- **Rotation.** Both tokens are rotated at every start. There is no
  in-process rotation in v1. The token files are deleted at exit, and the
  tmpfs runtime directory is cleared at logout.
- **Capture.** The token is now the only thing between a local
  same-uid process and the capture tool over TCP. The capture design's
  guardrails (PID rule, command allow-list, rate and window limits, the
  client's permission prompt) are the defence. The page token can never
  reach `capture`: the browser still cannot start, stop or change one,
  consistent with capture design §9.
- **Amended M2b anti-goal.** "The browser can change server state"
  becomes: **the browser can change no engine state** (store, attempts,
  publications, steering, captures). It may append validated reports to
  the inbox, which no tool's behaviour depends on except `page_state`'s
  output. The M2b global constraint "only GET is served" is amended
  likewise: `POST …/inbox` is the one exception.

---

### D7. Tests

**Stdio binary `cmd/perf-agent-mcp` (tier 0): M2's guarantee comes back
(decision 8.3).**
- **M2's link test is restored.** The binary links none of `net`,
  `net/http` or `crypto/tls` (`net/url` and `net/netip` are allowed, as in
  M2). It also links none of `internal/livehttp`, `internal/mcphttp` or
  `internal/perfpage` (SRV-R1).
- **Package-level checks (SRV-R1).** `go list -deps` checks:
  - `internal/perfmcp` imports no `net`, `net/http`, `crypto/tls`,
    `internal/livehttp`, `internal/mcphttp` or `internal/perfpage`;
  - `cmd/perf-agent-mcp` has no `serve` subcommand and no file that
    imports them;
  - **the positive half:** `cmd/perf-agent-hub` links
    `internal/perfpage`, `internal/livehttp` and `internal/mcphttp`, so
    the negative half is not vacuous.
- **The dependency halves are kept.** No `perfagent`, `cilium/ebpf` or
  blazesym; non-std modules ⊆ {perf-agent, google/pprof}. The capture
  design's `os/exec` rule for its runner applies here.
- **The tool list is pinned.** A golden test: stdio `tools/list` equals
  M2's list plus the capture tools, with no `page_*` and no
  `page_state`.
- **No regression.** A benchmark on the stdio call path (`top` on a
  fixture) shows no measurable change from the engine-slot wrapper.

**Serve binary `cmd/perf-agent-hub`: M2b's network tests move here
(decision 8.3).**
- **Source scan** of every non-standard package the serve binary links:
  - no client networking (`net.Dial*`, HTTP clients, resolver lookups,
    raw syscalls, cgo, linkname), as in M2b. There is no adapter, so no
    dial is ever needed;
  - the one listening call, `livehttp.Listen`, called only from
    `cmd/perf-agent-hub`;
  - the `os/exec` exceptions are `git.go` plus the capture runner (capture
    design §11).
- **Every-socket-loopback** (Linux): the built server runs while the test
  drives MCP over HTTP (a hand-written client in `_test.go`, so no new
  dependency), makes page GETs and inbox POSTs, and checks that every
  socket of the process is a loopback listener or a loopback-to-loopback
  connection.
- **`nm` symbol check:** the client-symbol denylist is unchanged, run on
  the serve binary. Its "links the page's handlers" assertion keeps the
  check meaningful.

**Transport conformance** (`internal/mcphttp`, `-race`):
- initialize mints a session;
- missing session → 400, unknown → 404, DELETE → 204;
- GET → 405;
- a bad `Accept` or `Content-Type` → 400;
- an unsupported `MCP-Protocol-Version` → 400;
- a 2026-07-28-style probe → a 400 that makes a client fall back;
- a notification → 202; a batch → refused; a body over the limit → 413;
- any `Origin` (including an empty value and a repeated header) or
  `Sec-Fetch-*` on `/mcp` → 403;
- every guard refusal is answered before a body byte is read and before
  dispatch, checked with a body that blocks;
- a chunked body → 411;
- a missing or wrong bearer → 401, with no `WWW-Authenticate` metadata;
- a non-loopback Host → 403;
- a long call outlives the server's 30 s `WriteTimeout`, the body read
  deadline is cleared before the wait, and a client that stops reading
  the response is dropped after the 10 s write bound;
- `notifications/cancelled` from another session does not cancel;
- **a closed connection does not cancel**: the call's context derives
  from the server's lifetime, not `Request.Context`;
- `DELETE` of a session cancels its in-flight requests, and never a
  capture job (SRV-R7);
- LRU eviction never picks a session with in-flight calls; when all are
  pinned, `initialize` → 503.

**Concurrency** (`-race`):
- **Serialised execution.** Eight sessions issue tool calls at once while
  the page GETs and the inbox POSTs. An instrumented handler records how
  many run at once: never more than one. Each session gets its own
  responses (id echo).
- **Releases.** A waiter whose context ends leaves the queue without
  running. `MaxEngineWait` produces the busy error.
- **Same-session saturation (SRV-R4).** One authenticated session sends
  calls far beyond `MaxCallsPerSession`, and then the server-wide caps are
  hit from several sessions. The test checks:
  - the excess gets `503` at once;
  - goroutines and retained request bytes stay under their caps;
  - `ping`, `tools/list`, `notifications/cancelled` and `DELETE` from the
    saturating session and from others are still answered;
  - the queued calls complete.
- **Cancel behind fast small bodies (SRV-R4b, R4c).** Many small, fast
  tool POSTs fill the call class to `MaxCalls`, while a tool holds the
  engine slot. A `notifications/cancelled` and a `capture_cancel` sent
  behind them are read, parsed, classified as control and answered
  before any queued call starts, never waiting for the engine (CAP-R10).
- **Slow incomplete bodies (SRV-R4c).** Authenticated requests announce
  a `Content-Length` and send only part of it, filling
  `MaxIngressReaders`. The test checks:
  - new requests, controls included, get `503` at once, without being
    read (the conditional guarantee);
  - each stalled reader is dropped at `IngressBodyTimeout`, and its
    reader and bytes are released;
  - service resumes then, and a cancel sent after that is served;
  - goroutine and byte counts return to baseline.
- **Control-class saturation (SRV-R4b).** The test fills
  `MaxControlInFlight`. It checks that the next control message gets
  `503` promptly, that calls and `page_state` are unaffected beyond
  their own caps, and that controls are served again as soon as the
  class frees.
- **Header-only rejection (SRV-R4b).** A body that never arrives is not
  waited for when the headers already fail, and an oversize
  `Content-Length` gets `413` or `503` without a read.
- **`page_state` without the engine (SRV-R3b).** While a tool holds the
  slot for seconds, `page_state` and `resources/read` of
  `perf-agent://page/state` answer at once, with the current snapshot and
  inbox, and `read_by` is recorded. Filling `MaxPageStateInFlight` gets
  `503` for `page_state` only.
- **JSON only (SRV-R6).** No `/mcp` response ever has
  `Content-Type: text/event-stream`, and the server never sends a
  JSON-RPC request.
- **Capture hosting in serve.** The lifecycle tests are the capture
  design's (§10 there). The server adds only these, with a stub
  perf-agent:
  - **async shape:** `capture` returns `{job, status:"accepted"}` at
    once, with no file touched, while another tool holds the engine
    slot;
  - **in-process busy:** a second `capture` while one is active is
    refused synchronously (`busy`), and nothing changes: files, IDs,
    retention, reservations and pins;
  - **cross-process busy:** the captures directory's lease is held by
    another process (a stdio server, or a reaper left by a crashed
    serve). `capture` still returns `accepted`; the job then fails with
    `busy` naming the holder, having made no filesystem side effect;
  - **derivations contend like tools:** a job's prepare and derivation
    steps wait for the engine slot behind other sessions' tools, and the
    reverse, both bounded by `MaxEngineWait`. A blocked hash or fsync in
    `committing` never holds the slot;
  - **page publication:** a committed capture is published facts-only
    with `by`, and a publication failure leaves the capture committed
    with `derived: failed`;
  - **multi-session waits (SRV-R7):** `capture` from session A, then A's
    eviction or `DELETE`, then `capture_wait` from sessions B and C at
    once and `capture_cancel` from B. The job is untouched by A's end,
    both waits are bounded by their own timeouts, and B's cancel stops
    the job;
  - **the cancel point (CAP-R10, R16):** `capture_cancel` returns
    `cancelling` while `MaxCalls` is full and a tool holds the slot.
    Before the manifest write the job rolls back; after it the answer is
    "too late: committed" and the files stay;
  - **historical lookups (CAP-R14, R17):** with the registry evicted
    (`Capture.MaxJobs` = 1), and after a restart, waits by capture ID read
    the manifest through the lookup workers. A blocked manifest read
    makes repeated waits return `loading` at their deadlines, with no
    goroutine growth. A later wait gets the terminal answer
    `derived: "recompute"`, with the `open` and `diff` calls (and
    `page_explain`) to run, and the server writes no derived file;
  - retention overlapping another session's `open`, its eviction through
    `MaxOpenHandles`, a `page_explain` publication, and a `compare_with`
    on the same baseline; nothing in use is deleted;
  - the baseline pin and the reservation are released on every failure
    path: a validation error after pinning, a perf-agent failure, a
    cancel during the wait, a commit error, and shutdown. The pin ledger
    and the reserved bytes return to their baseline.
- **Writer ownership and crashes (SRV-R5, CAP-R6b):**
  - serve refuses a stdio-owned directory and stdio a serve-owned one;
  - the defaults `captures` and `captures-hub` are distinct;
  - a serve SIGKILLed mid-capture leaves the reaper holding the lease. A
    restarted serve refuses capture, naming the reaper's pid, and runs no
    retention or recovery there until the reaper has reaped both children;
  - recovery then checks the reaper's identity and start time.

**Inbox:**
- **The CSRF matrix:** Origin missing or other, `Sec-Fetch-Site` missing,
  cross-site or `none`, `text/plain`, no custom header, wrong page token,
  an oversize body, and the rate limit.
- **Validation:** an unknown publication or node → 422, not recorded.
- **Engine untouched:** an inbox POST changes only the inbox ("no GET
  mutates" becomes "no page request mutates engine state"; the snapshot
  `seq`, attempts and store are compared before and after).
- **Bounds:** coalescing, the ring with `dropped`, and the cursor
  semantics with two agents.
- **Two revisions (SRV-R3)**, with two tabs as two `events` streams:
  - an ask appended, and then marked `read_by` by `page_state`, with no
    engine activity: both tabs see each `inbox.rev` and the new counts,
    and `engine.seq` is unchanged;
  - the same while another session's tool holds the engine slot: the
    inbox updates arrive without waiting for it;
  - interleaved commits and inbox changes: neither part ever goes back,
    and an envelope with a new `rev` and an old `seq` (or the reverse)
    leaves the other part as it was.

**Runtime directory:**
- 0700 and 0600 modes are enforced, and a foreign-owned directory is
  refused;
- a second instance is refused;
- the files are removed at exit;
- no token in stderr (a grep over a full run's log);
- the page URL appears on stdout only.

**Chrome (skipped without Chrome, as in M2b):**
- "Ask my agent about this" posts and the toast shows;
- zero CSP violations, as before;
- the inbox count updates in a second tab, with no engine change and
  while a slow tool holds the slot (SRV-R3);
- an inbox update arriving during an agent chain's publication switch
  does not abort the chain or bump the generation (M2b-R1/R9..R12 hold).

**Real-client smoke (manual, recorded in the PR):**
- Claude Code with `claude -p --mcp-config <scratch json>` (M2-R1: never
  `claude mcp add`) and the `headersHelper`;
- Codex with a scratch `config.toml` and `http_headers_helper`;
- restart `serve` mid-session and record how each client recovers from
  the `404` and `401`;
- two agents at once on one page.

---

## 4. HTTP surface (serve mode)

| Path | Method | Auth | Returns |
|---|---|---|---|
| `/mcp` | POST | Bearer MCP token | JSON-RPC response (`application/json`) or 202 |
| `/mcp` | DELETE | Bearer + session | 204 or 404 |
| `/mcp` | GET | — | 405 |
| `/s/<page token>/…` | GET, HEAD | page token | every M2b route unchanged (M2b §6); `state` gains `by` per publication and `inbox` counts |
| `/s/<page token>/inbox` | POST | page token + same-origin | 204; 422 for an invalid report; 429 for the rate limit |
| anything else | any | — | 404 |

---

## 5. Changes to existing code

**Untouched (pre-existing perf-agent, user decision 7):** the `perf-agent`
binary and its flags, `cmd/flamegraph` and its outputs, `gpu/*`,
`profilemodel`, `explain` and `sourceroot` semantics. The static-page
goldens (`RenderHTML`, `RenderFragment`, `WriteDiffPage`, the explained
page outside its script) and `render_flamegraph`'s results and bytes stay
byte-identical. `live.js` and `livepage.html` are live-only assets that
`cmd/flamegraph` never emits.

**New code:**
- `internal/mcphttp`: the Streamable HTTP binding: sessions, the guard,
  admission caps (SRV-R4), the in-flight table that scopes cancellation,
  and deadlines. It knows nothing about perf-agent,
  like `mcpstdio`.
- `internal/livehttp/front.go`, `mcpguard.go`, `inboxguard.go`.
- `internal/perfmcp/engine.go` (the slot), `statebus.go` (the two-part
  state envelope, SRV-R3), `inbox.go`,
  `tool_page_state.go` and `pageview.go` (`PageConfig`, the notifier,
  and the read-only `PageSnapshot()` accessor). None of these imports
  networking.
- `internal/perfpage`: the page routes moved from `liveroutes.go`, and the
  inbox HTTP handler. Only the serve binary links it.
- `internal/mcpflags`: the root, limit and capture flag parsing, shared by
  both binaries.
- `cmd/perf-agent-hub` (`main.go`, `runtimedir.go`): the
  `token`/`url` subcommands, the runtime directory, the lock, the token
  files and the wiring.
- Their tests.

**Changes to existing (M2 and M2b) files, each with its reason:**

| File | Change | Why it is needed |
|---|---|---|
| `internal/mcpstdio/server.go` | Extract `dispatch` into exported `(*Server).Handle(ctx, *Session, []byte) []byte` with a `Session` type (the `initialized` flag, the negotiated version, the client label). `Serve` uses it with one session, and its bytes are unchanged | The HTTP binding must feed one message at a time per session. It is the same code, not a fork, so both transports answer identically |
| `internal/mcpstdio/protocol.go` | `Resource.Read func() (string, error)` (optional, dynamic); `initialize` records `clientInfo.name` into the `Session` | `perf-agent://page/state` is dynamic; the label is needed for `by` and `read_by` |
| `internal/perfmcp/server.go` | `tools()` wraps handlers with the engine slot; page tools and `page_state` registered only with `Config.Page`; `Close` no longer stops a listener | The ownership invariant must hold with many request goroutines (§D2.4) |
| `internal/perfmcp/config.go` | `Live bool` becomes `Page *PageConfig` (`URL string`, set by `serve`) | `perfmcp` no longer listens; the host tells it the URL |
| `internal/perfmcp/live.go` | Remove `start`, the listener fields, `mu` and the `livehttp` import; `hub` becomes the `Config.Page` notifier; `ensureLive` returns `Config.Page.URL` and commits on first use; publications and state record `by`; comments say "slot holder" | The lifecycle moves to the serve binary, and `perfmcp` must import no networking (decision 8.3; §D5, task 7) |
| `internal/perfmcp/liveroutes.go` | **Moved** to `internal/perfpage`; the snapshot-only reads are unchanged. `stateOf` stays in `perfmcp`, adds `by`, and feeds the `engine` part of the state bus (`statebus.go`, new, in `perfmcp`) | It is the file that imports `net/http`; SRV-R3 |
| `internal/perfmcp/attempts.go` | `attempt.by` (display only; not in static output) | attribution across agents (§D3) |
| `internal/perfmcp/limits.go` | `MaxSessions`, `MaxEngineWait`, `MaxIngressReaders`, `IngressBodyTimeout`, `MaxIngressBytes`, `MaxControlInFlight`, `MaxPageStateInFlight`, `MaxCalls`, `MaxCallsPerSession`, `MaxRequestBytes`, `MaxInboxEntries`; serve-mode default `MaxOpenHandles` 32. The capture limits (`Capture.MaxJobs` 32, `Capture.MaxJobBytes` 4 MiB) are the capture design's, unchanged | `--limit` reflects over `Limits` |
| `internal/perfmcp/tool_page.go` | the descriptions drop the `--live` DISABLED text; `page_open` says the page is served by `perf-agent-hub` | the text would be false |
| `internal/livehttp/guard.go` | export `LoopbackHost` (it wraps the existing function; `Guard` is unchanged) | `Front` and the new guards reuse the exact Host rule |
| `internal/flamegraph/live.js`, `livepage.html`, `live.css` | `transport.post`; reports to `inbox`; the "Ask my agent about this" chip; inbox count; toast | interactivity (§D4); no static-page change |
| `cmd/perf-agent-mcp/main.go` | remove `--live`; parse flags through `internal/mcpflags` | entry point; the flags are shared with the serve binary |
| `cmd/perf-agent-mcp/main_test.go` | restore M2's "links no `net`, `net/http`, `crypto/tls`" test, and add the no-`livehttp`/`mcphttp`/`perfpage` check | decision 8.3 |
| `cmd/perf-agent-mcp/netscan_test.go` | **Moved** to `cmd/perf-agent-hub`, with the M2b socket and `nm` tests, retargeted at that binary (§D7) | the network promises now belong to the binary that has a network |
| `docs/mcp.md`, the guide, `skills/perf-agent-mcp/SKILL.md`, CHANGELOG | the serve section; `--live` removed; the page tools described as serve-only | docs would be false |

The capture design's own changes (its §11) are separate and compatible.
They meet in three places:
- **`server.go`'s `tools()`:** `capture` is registered without the slot
  wrapper.
- **The capture tools:** their prepare and commit steps take the slot,
  in SRV-R2b's order.
- **`netscan_test.go`:** the capture design edits its `os/exec` rule, and
  the server PR moves the file.

---

## 6. Alignment with the capture design (summary)

| Capture design says | This design provides |
|---|---|
| Headless stdio first; serve reuses the same handler (§9) | stdio unchanged apart from the uncontended slot; serve hosts the same `perfmcp.Server` |
| The single capture lifecycle (its §4.0, §7, §9; CAP-R13) | referenced, not copied (§D2.5). Serve supplies the engine slot that the job takes for prepare's shared inputs and for the derivations. The child wait and the file commit run outside it, under the lease |
| CAP-R6, CAP-R6b: a cross-process lease on the captures directory, held by the reaper until both children are reaped | the job takes it nonblocking before any filesystem side effect, and a held lease is an async job failure (CAP-R13). Plus SRV-R5: enforced `.owner` writer kinds, with distinct defaults (`captures` for stdio, `captures-hub` for serve). A crashed serve's reaper keeps the lease, and a restarted serve's jobs fail `busy` until it is free (§2) |
| `notifications/cancelled` only from the starting session; a dropped connection is not a cancel | sessions track their in-flight ids; the transport never cancels on disconnect (§D2.2, D2.5) |
| A finished capture → a facts-only publication | done under the slot, with `by` recorded |
| The browser cannot start or stop a capture | yes: the amended anti-goal covers engine state, captures included (§D6) |
| "Any client with the server's token" | the Bearer MCP token; the page token is separate and cannot call tools |
| Its POST-SSE progress and elicitation (former §9.1 and question 5) | dropped from both designs: the transport is JSON-only (SRV-R6), and the two-step `capture`/`capture_wait ≤ 45 s` keeps every call under client timeouts (CAP-R8) |
| CAP-R9..R11, R14..R16: the job state machine, `capture_cancel` on the control path, the bounded registry (`MaxJobs` 32, `MaxJobBytes` 4 MiB), derived results in memory only, with the manifest answering `derived: "recompute"` after eviction (CAP-R17), bounded historical lookups, cancel until the manifest write | §D2.5 lists only what serve adds: admission classes, page publication as a derivation, multi-session waits (SRV-R7), and the engine slot |
| Question 4: long blocking calls against client timeouts | settled by CAP-R8: `capture` returns at once, and `capture_wait` takes at most 45 s. `MaxEngineWait` is 45 s for waiters. `/mcp` write deadlines are cleared for these calls and bounded at 10 s for the write itself |

---

## 7. Research notes (read 2026-11-15)

- **[MCP 2025-11-25, Transports]**
  (modelcontextprotocol.io/specification/2025-11-25/basic/transports):
  - a single MCP endpoint, POST and GET;
  - POST carries one message; a notification or response gets 202; a
    request gets `application/json` or `text/event-stream`;
  - GET returns SSE or 405;
  - the session is optional and minted on `InitializeResult` as
    `MCP-Session-Id` (visible ASCII, cryptographically secure). Without it
    → 400 (SHOULD); terminated → 404, and the client re-initializes;
    DELETE ends it (405 allowed);
  - `MCP-Protocol-Version` is required on later requests; unsupported →
    400; absent → assume 2025-03-26 unless the version is known
    otherwise;
  - resumability through `Last-Event-ID` (MAY);
  - security: "Servers MUST validate the Origin header … If the Origin
    header is present and invalid, servers MUST respond with HTTP 403";
    bind to localhost; "SHOULD implement proper authentication".
- **[MCP 2025-11-25, Security Best Practices]**
  (…/2025-11-25/basic/security_best_practices):
  - "MCP Servers MUST NOT use sessions for authentication";
  - session IDs secure and non-deterministic;
  - for local servers, "Use the stdio transport to limit access to just
    the MCP client", or, with HTTP, "Require an authorization token" or
    use unix sockets. (Neither target client documents unix-socket HTTP,
    so this design uses TCP loopback with a token.)
- **MCP 2026-07-28, Streamable HTTP**
  (…/2026-07-28/basic/transports/streamable-http):
  - removes the GET stream, protocol-level sessions and `Last-Event-ID`;
  - adds `Mcp-Method` and `Mcp-Name` headers;
  - long-lived notifications come through `subscriptions/listen`;
  - sampling, elicitation and roots become input requests embedded in
    results (MRTR, SEP-2322); "The server MUST NOT send independent
    JSON-RPC requests";
  - backward compatibility: a client falls back to `initialize` on a 400
    without a recognised modern error.
  This is why state here is not session-keyed and v1 builds no GET
  stream.
- **[Claude Code MCP docs]** (code.claude.com/docs/en/mcp):
  - `claude mcp add --transport http <name> <url> --header "…"`, or JSON
    `{"type":"http","url":…,"headers":…}`;
  - `${VAR}` expansion in `url` and `headers`;
  - `headersHelper` runs on each connection with a 10 s timeout, and "On
    a 401 or 403, it re-runs and the call is retried once";
  - `list_changed` is supported; resource subscriptions and sampling are
    not covered; elicitation is supported in interactive sessions;
  - resources can be referenced with `@server:protocol://path` and are
    attached.
- **Codex MCP docs** (developers.openai.com/codex/mcp, which redirects to
  learn.chatgpt.com/docs/extend/mcp):
  - `[mcp_servers.<name>]` with `url`, `bearer_token_env_var`,
    `http_headers`, `env_http_headers` and `http_headers_helper`;
  - `tool_timeout_sec` defaults to 60 and `startup_timeout_sec` to 10;
  - sampling, resources and notifications are not covered.
- **Not verified:** whether Codex re-runs `http_headers_helper` on a 401,
  and how either client handles a 2026-07-28-style fallback against this
  server. Both are in the smoke (§D7).

---

## 8. Open questions for the user (most important first)

The questions of the first draft were answered on 2026-11-15 (decision
8). What remains is minor, and the defaults hold:

1. **The default port number.**
   - **Default:** one from an unassigned range, chosen at implementation
     and written in the docs, with `--port` to change it.
2. **`MaxOpenHandles` in serve.**
   - **Default:** 32, since several agents share one store; stdio keeps 8.

**To verify, not to ask** (in the real-client smoke):
- whether Codex re-runs `http_headers_helper` after a `401`;
- how both clients fall back from a 2026-07-28-style probe against this
  2025-11-25 server.

**Review.** Per the standing practice, the perf-agent Codex session
should review this design (via Workforce) before a plan is written,
together with the capture design, since they meet in `tools()` and the
engine slot.

---

## 9. Order of work and what depends on capture (decision 8.5)

**First: the headless capture milestone** (capture design; stdio only),
stacked on the trimmed #264 (§D5). Nothing in it needs this design: in
stdio the request loop is sequential, so capture needs no engine slot.

**Second: the serve binary.** Its parts, by dependence on capture:

| Part | Depends on capture? |
|---|---|
| `perfmcp`/`perfpage` split; restored M2 link test for stdio (§D1.1, D7) | No |
| `cmd/perf-agent-hub`, runtime directory, lock, tokens, `--port` (§D1) | No |
| `internal/mcpflags` (§D1.1) | **Yes, mildly:** it must carry capture's flags (`--perf-agent`, `--captures`, `--capture-command`, `--gpu-shim`), so it is written after they exist |
| `mcphttp`: endpoint, sessions, guards, deadlines (§D2.1–D2.3, D2.6) | No. The long-call deadline handling (§D2.5) is motivated by capture but tested with any slow tool |
| Cancellation scoping per session (§D2.2, D2.5) | **Yes:** it is capture's requirement (capture design §9), and capture is the first handler that honours its context |
| Hosting the capture lifecycle (capture design §4.0, §7, §9; CAP-R13..R16): the engine slot for its engine steps, the admission classes, page publication of results, and multi-session waits (SRV-R7) | **Yes**. The whole lifecycle (state machine, lease, manifest, registry, lookup workers) ships in the capture milestone; serve adds only these hosting parts |
| Ingress reader cap and body deadline (SRV-R4c) | No |
| `.owner` writer kinds and the `captures-hub` default (SRV-R5) | **Yes**: the `.owner` file and its check belong in the capture milestone, so stdio enforces its side from the start |
| The engine slot and admission caps (§D2.4) | No for the slot and the caps. **Yes** for capture's three phases (SRV-R2): the prepare and commit steps under the slot, failure cleanup, and the retention-overlap tests |
| Captures as facts-only publications on the page (capture design §9) | **Yes** |
| Shared state, attribution `by` (§D3) | No; capture's `capture_list` attribution reuses the label |
| Inbox, `page_state`, the resource, page assets (§D4) | No |
| The trimmed-#264 work (§D5 step 1) | No; it comes before capture |
| Threat model (§D6) | Capture rows only: token → capture authority, page token cannot reach capture |
| Tests (§D7) | Source scan: the `os/exec` exception set includes capture's runner. Every-socket test and smoke: include a capture with a stub `perf-agent` (capture design §10). The rest: no |
| `page_state` without the slot (SRV-R3b), the admission classes (SRV-R4b), JSON only (SRV-R6) | No |

So the serve milestone can start independently. Its capture-dependent
items (cancellation scoping, the SRV-R2b capture order, capture
publications, the capture rows of the tests) are its last tasks, and they
are written against capture as it shipped.

---

## 10. Review resolutions

### 10.1 Review #400 (Codex, 2026-11-15)

Codex reviewed a pinned copy of this design (930 lines, taken during the
decision-8 revision). It found **no browser-to-`/mcp` bypass** in the guard
combination: a fixed loopback bind, a validated Host, refusal of every
present `Origin` and `Sec-Fetch-*`, a separate bearer authority, and no
CORS. It also found the inbox guard consistent with the inbox's narrower
authority. Its four P2 findings and one cross-design dependency became
rulings, recorded in
`.superpowers/sdd/2026-11-14-live-page-m2b/progress.md`.

| Ruling | Finding | Resolution here |
|---|---|---|
| **SRV-R1** | The inventory contradicted the separate binary: `PageHandler`/`InboxHandler` exported from `perfmcp`, the mux left in `perfmcp/liveroutes.go`, a `cmd/perf-agent-mcp/serve.go` dispatched from the stdio main, and D7 pinning that call site | The pinned copy predated part of the decision-8 rewrite. Now everything is built around `cmd/perf-agent-hub` and `internal/perfpage`: §D1.1, §D5 tasks 7 and 11, §D7 and §5. `perfmcp` exposes no HTTP (only the `PageSnapshot()` read accessor and the state-bus notifier). The question 5 of the first draft is gone (the user decided it). The dependency tests name these paths, with a positive half on the serve binary (§D7) |
| **SRV-R2** | Capture's retention and `compare_with` read and mutate shared store state outside the engine slot | Three phases (§D2.5): prepare under the slot (validate, pin the baseline, resolve the protected inputs, run retention, reserve capacity, produce immutable inputs), wait outside it, then commit, compare and publish under it. Everything prepare took is released on every failure path. Tests overlap retention with `open`, eviction, publication and comparison (§D7) |
| **SRV-R3** | Inbox-only changes could not reach other tabs: `commit()` is the only snapshot writer and bumps `seq`, and `live.js` drops states whose `seq` is not new | The state envelope has two parts with independent revisions, `engine.seq` and `inbox.rev`, combined by a state bus that neither the engine slot nor the inbox mutex waits on. `live.js` applies each part by its own revision (§D4). Tests use two tabs, with no engine activity and while a tool holds the slot, and check that neither part regresses the other (§D7) |
| **SRV-R4** | The admission queue was unbounded: a goroutine and a body per waiting request, while `MaxSessions` does not cap calls | The global and per-session caps `MaxCalls` and `MaxCallsPerSession`, plus `MaxRequestBytes`. A required `Content-Length` lets bytes be reserved before reading. Excess gets `503` at once. Control traffic (`initialize`, `ping`, the list methods, `notifications/cancelled`, `DELETE`) never waits for the slot. *SRV-R4b refines this: controls are served ahead of calls from their own reserve, and can still get `503` once that reserve is full.* Tested with a same-session saturation test (§D2.4, §D7) |
| **CAP-R6** (cross-design) | Shared capture directories between independent stdio and serve processes would let serve's retention, `.seq` and pins change a headless loop's captures | The capture design's cross-process `flock` on a captures directory holds whichever binary takes it. The docs tell users to give the two binaries different `--captures` directories. The later bridge (§D3.5) reads a headless directory read-only and never owns it (§2) |

**Implementation notes, recorded as test requirements (§D7):**
- **Contexts.** Execution contexts derive from the server's lifetime plus
  an explicit per-request cancel, never from `net/http`'s
  `Request.Context`, which ends when the connection closes.
- **Cancellation ownership** is kept when a session is evicted or
  `DELETE`d:
  - LRU eviction skips sessions with in-flight calls;
  - `DELETE` cancels its own session's calls;
  - no other session can cancel them.
- **Deadlines.** The body read deadline is cleared before any long wait,
  and the 10 s response-write bound is kept.
- **Origin variants.** An empty or repeated `Origin` header is refused
  explicitly on `/mcp`. On the inbox route, `Origin` must appear exactly
  once, non-empty and exact.
- **Order.** Every guard runs before a body is read or anything is
  dispatched.
- **Unverified until the smoke:** client restart recovery and header-
  helper behaviour (§8). Codex ran no paid agents and changed no client
  configuration.

### 10.2 Review #405 (Codex, 2026-11-15)

Codex closed SRV-R1 and confirmed the two-revision envelope (SRV-R3).
Its remaining findings became the round-3 rulings recorded in
`progress.md`. They are applied here, together with the capture side's
CAP-R2b, R6b and R8.

| Ruling | Finding | Resolution here |
|---|---|---|
| **SRV-R2b** | The capture slot was claimed only after prepare's side effects (pins, retention, reservations) | One order (§D2.5): nonblocking capture admission and the cross-process lease **first**, then prepare under the engine slot, the wait outside it, commit or cleanup under it, and the lease released **last**. The server never waits for a lease while holding the engine slot, and a refused second capture changes nothing: no files, IDs, retention or reservations. The stale "only the final step" wording in §D5, §5, §6 and §9 is rewritten. Tested (§D7) |
| **SRV-R4b** | The admission classifier could not decide from headers, because in 2025-11-25 every message is a `POST /mcp` | Four stages (§D2.4): header-only rejection first; bodies read within a bounded ingress budget; method and id parsed; then three admission classes (control with its own reserve and priority, `page_state`, calls). **The exact guarantee:** controls are served ahead of calls and never wait for the engine. Beyond their reserve they too get `503`. Tests: a cancel behind many small tool POSTs at full capacity, and control-reserve saturation (§D7) |
| **SRV-R3b** | `page_state` was still behind the engine wrapper | `page_state`, the tool and the resource, is a snapshot-and-inbox-only handler, with no engine slot and its own small admission class (§D2.4, §D4). Tested while a tool holds the slot (§D7) |
| **SRV-R5** | Headless isolation was overclaimed while stdio and serve shared a default captures directory | Writer ownership is enforced: a `.owner` file records the writer kind on first use, and each binary refuses the other kind's directory. The defaults are distinct (`<profiles>/captures` for stdio, `<profiles>/captures-hub` for serve). The reaper-held lease (CAP-R6b) carries into crash concurrency: a restarted serve refuses capture until the reaper has reaped both children, and recovery checks the reaper's identity and start time. The later page bridge may only read a stdio directory (§2, §D3.5, §D7) |
| **SRV-R6** | The transport contradicted itself: the capture design's §9.1 used POST SSE while this design is JSON-only | The transport is JSON-only, with no SSE responses, progress or server-to-client requests. CAP-R8's two-step `capture`, then `capture_wait` with a timeout of at most 45 s, keeps every call under the 60 s first-byte limit. **Elicitation is removed from both documents**: §1.3, §D2.5, §6 and §9 no longer mention it as an option. The capture design drops its POST-SSE and progress requirement (§D2.5) |

### 10.3 Review #408 (Codex, 2026-11-15)

Codex closed SRV-R1, R2b, R3b, R5 and R6 at design level. The remaining
findings became the round-4 rulings in `progress.md`.

| Ruling | Finding | Resolution here |
|---|---|---|
| **CAP-R9** | §D2.5 had commit run on a server goroutine and roll back on failure, while the capture design says `open` and diff failures do not roll back | One state machine for both transports (`accepted → preparing → running → committing → done/failed/cancelled`). Every phase runs in a server-owned job goroutine, and `capture` only admits, enqueues and returns the ID. **The commit point is the manifest write.** File steps run outside the engine slot. `store.open`, the diff and the publication are post-commit derivations under the slot: their failure is reported and never rolls files back. Every wait is bounded (§D2.5). Tests: blocked hash, fsync, `open` and diff, and a commit outliving a wait (§D7) |
| **CAP-R10** | `capture_cancel` sat in the ordinary call class, behind the engine wrapper | `capture_cancel` is in the control class (§D2.4): no queue, no engine. It only flags the job and returns `cancelling`, and the job does the cleanup (§D2.5). Tests: behind a full `MaxCalls` with the engine slot held (serve), and behind an in-progress wait (stdio, the capture design's test) |
| **SRV-R7** | LRU eviction pinned only sessions with in-flight requests, while capture jobs outlive requests | Jobs are owned by the server. Any session with the token may wait on or cancel any job. `DELETE` and eviction never cancel a job (§D2.2, D2.4, D2.5, D3). Tests: `capture`, then eviction or `DELETE`, then wait and cancel from a new session (§D7) |
| **SRV-R4c** | Pre-classification cannot reserve for bodies not yet read, and a byte cap alone admits many stalled tiny bodies | `MaxIngressReaders` caps how many bodies are read at once, and each body must arrive within `IngressBodyTimeout`. Classification happens only after parsing, and the per-size control byte reserve of r3 is removed. The guarantee is now stated conditionally: under a slow-body flood, new requests, controls included, are refused until readers free (§D2.4, D6). Tests with slow incomplete bodies and with fast small ones (§D7) |
| **CAP-R11** | The result cache was unbounded | *(Names and storage superseded by CAP-R15/R16 in §10.4.)* One active capture. Terminal jobs are kept in memory up to `MaxCaptureJobs` and `MaxCaptureJobBytes` (least recently used evicted first), and every terminal result is in the series manifest, so `capture_wait` after eviction or restart answers from disk, saying when a handle must be reopened. Heavy pins are held only while the job needs them (§D2.5, §D7) |

CAP-R12 (the staging-mkdir orphan wording) is the capture design's alone,
and changes nothing here.

### 10.4 Review #411 (Codex, 2026-11-15)

Codex closed SRV-R7 and SRV-R4c. It found the refinement of §10.3 sound:
hash, fsync, link and the manifest write need the directory lease, not
the engine. Its remaining findings became the round-5 rulings in
`progress.md`.

| Ruling | Finding | Resolution here |
|---|---|---|
| **CAP-R13** | The two documents prescribed different capture lifecycles. This one's step 0 did the `.owner` check, the `flock` and the ID allocation inside the `capture` call, and refused cross-process `busy` synchronously | §D2.5's own lifecycle is **deleted**. It now references the capture design (§4.0, §7, §9) as the single definition: the request does a pure check plus in-memory admission with no I/O, and cross-process `busy` is an async job failure. Only serve-specific parts remain: the engine slot, the admission classes, page publication of results and multi-session waits. §2, §6, §9, §5 and the §D7 server tests now use the async `busy` and result shapes |
| **CAP-R14** | `capture_wait`'s fallback to disk ran outside its timeout | Historical lookups run on the capture design's bounded workers (2, a bounded queue). A wait returns `loading` at its own deadline, with no goroutine per timed-out wait (§D2.5). Server test with a blocked result read, repeated waits and a later completion (§D7) |
| **CAP-R15** | "The whole final result is in the series manifest" did not match the storage | *(Superseded by CAP-R17, §10.5.)* Results were to be the manifest plus a result sidecar with its own lifecycle |
| **CAP-R16** | The parameter names and values had drifted | `Capture.MaxJobs` 32, `Capture.MaxJobBytes` 4 MiB and `"handles": "reopen"` replace `MaxCaptureJobs`, 1 MiB and `handle_open`, and `limits.go` here adds no capture limits. The cancellation linearization point is the manifest write: cancel succeeds before it and answers "too late: committed" after it. The stale "stdio slot always free" is gone: §D2.4 now says that in stdio the session's tool calls and its capture job's derivations contend for the engine too |

### 10.5 Review r5 (Codex, 2026-11-15)

Codex found no new serve-only issue. One shared change is mirrored here:

| Ruling | Change | Resolution here |
|---|---|---|
| **CAP-R17** | The `result.json` sidecar and the `.journal` are removed. Derived results live only in the in-memory job registry. After eviction or a restart, `capture_wait` answers terminally from the committed manifest with `derived: "recompute"` and the exact `open`/`diff` calls to run | §D2.5, §D3, §6, §9 and the §D7 server test now say so; no serve text mentions a sidecar, a journal or a derived result read from disk. In serve the recompute answer also names the `page_explain` call that publishes the capture again |

