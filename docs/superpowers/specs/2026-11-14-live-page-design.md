# M2b: the live page — design addendum

**Status:** design, not implemented. Addendum to
[2026-11-13-ai-flamegraph-design.md](2026-11-13-ai-flamegraph-design.md)
(the spec). It builds on M2 as M2 is after its Task 16
([plan](../plans/2026-11-14-mcp-server-m2.md), rulings M2-R1..R20 in
`.superpowers/sdd/2026-11-14-mcp-server-m2/`). Where this addendum and the
spec disagree, this addendum wins for M2b. Every other part of the spec
still binds.
**Date:** 2026-11-14
**Plan:** [2026-11-14-live-page-m2b.md](../plans/2026-11-14-live-page-m2b.md)

## 1. Intent

In M2 the agent's work ends in a file: `render_flamegraph` checks the
claims and writes a static HTML page, and the user opens it. Each follow-up
means a new file.

M2b makes the page **live**. `perf-agent-mcp` serves the explained flame
graph itself, from assets built into the binary. The agent drives the page
while it talks to the user:
- it publishes checked explanations to the page;
- it selects a frame;
- it zooms into a subtree;
- it opens a diff.

The user sees each change in the browser tab they already have open.

Nothing about the evidence changes:
- every figure on the page is still the engine's;
- every claim is still checked before it is shown;
- the page still lists the server's own record of rejected attempts.

What changes is the delivery: a page server bound to loopback instead of a
file.

Order after this milestone (user decision): **M2b live page → capture from
the MCP** (an unprivileged `perf-agent-mcp` invoking the setcap'd
`perf-agent` binary) **→ M3, the optimisation loop.**

## 2. User decisions (2026-11-14)

1. **AI mode is a live page.** `perf-agent-mcp` does not only write static
   HTML files. It serves the explained flame graph as a live page with its
   assets embedded in the binary, and MCP tools drive the page.
2. **Capture comes next, not now.** M2b still never starts a profiler and
   needs no capabilities.
3. **Then the loop (M3).**

What these decisions supersede in the spec:
- **§3 Non-goals, first bullet.** The spec said "Live, in-page chat. The
  HTML is a snapshot; follow-ups go through the agent." The *snapshot* part
  is superseded. In-page **chat** is still a non-goal: follow-ups still go
  through the agent.
- **§7.1 "speaks MCP over stdio only".** MCP itself stays stdio-only. The
  page server is a second, separate interface. It speaks HTTP to a browser
  on the same machine and is not an MCP transport.
- **§3 anti-goal "Leaking data: perf-agent opening a network connection".**
  Under D1 (below) the server opens a **loopback listening socket**. It
  never makes an outbound connection, and never listens on a non-loopback
  address. This reinterpretation is the largest change in M2b and is the
  user's to confirm (question 1 in §12).

## 3. Goals, non-goals, anti-goals

### Goals
- **The agent drives what the user sees.** It can publish checked
  explanations, select a node, focus a subtree, switch between
  publications and show a diff. The open tab updates within a second,
  with no reload.
- **The page can explore on its own.** Without the agent, the page can:
  - select any frame;
  - zoom into any subtree, with the frames fetched from the engine;
  - show the facts of frames too narrow to be drawn;
  - download a self-contained static copy.
- **Same evidence rules as M2.** Engine-only figures, the claim check, the
  "N figures and references checked" header, unchecked numbers marked, and
  the rejected history taken from the server's record (M2-R15..R20). On the
  live page the rejected history is **live**: a rejection made while the
  page is open appears on it.
- **Static files stay.** `render_flamegraph` is unchanged. Static pages
  remain the artifact for PRs and for M3.

### Non-goals (M2b)
- In-page chat, or any way for the page to ask the agent something. The
  follow-up chips still copy a prompt.
- Capturing profiles (the next milestone).
- Remote viewing, including SSH port forwarding (user decision 2026-11-14:
  a risk vector not needed now). The page is for a browser on the same host;
  the server listens only on 127.0.0.1 on a random port and the docs do not
  describe any way to reach it from another machine.
- Rendering inside an MCP client (MCP Apps). This is deferred, and the
  design keeps it possible (§10).
- Multi-user sharing. A page URL is a bearer secret for one person's
  session.

### Anti-goals (failures even if it works)
- **The browser can change server state.** No request from the browser may:
  - call a tool;
  - record or alter an attempt;
  - open a handle;
  - write a file;
  - change what the agent published.
  Every endpoint is `GET`, and no `GET` mutates.
- **Another web page can read the profile.** Neither a cross-site page, a
  DNS-rebinding page nor another local user may read anything.
- **A live page can show a view without its rejections.** This covers:
  - a facts-only publication;
  - a publication made through another handle of the same profile;
  - a rejection made after the publication.
  The page shows the server's current record in every case.
- **The page computes a figure.** The script formats no number. Every
  displayed value is a string the engine produced.
- **A slow or stuck tab delays the agent.** Tool calls never wait on a
  browser.
- **An outbound connection** of any kind.

## 4. Decisions

Each decision says **who decides**:
- **[mine]**: settled here; the user may overrule it.
- **[user]**: needs the user's confirmation. The stated default holds until
  then.

### D1. Transport: a loopback HTTP listener, not MCP Apps **[user]**

**Decision.** `perf-agent-mcp --live` serves the page over HTTP/1.1. The
rules:
- **Address:** `127.0.0.1` on a random port (no flag: always
  `127.0.0.1:0`). `::1` and any other loopback literal are accepted. A
  hostname, `0.0.0.0`, `::` or any non-loopback address is refused at
  start-up.
- **Token:** every URL carries a per-run, unguessable token as its first
  path segment: `http://127.0.0.1:PORT/s/<token>/`, with 32 random bytes
  encoded as base64url.
- **Host:** must be a loopback literal or `localhost`, with any port.
- **Origin and `Sec-Fetch-Site`:** when present, they must be same-origin
  (`none` is also allowed for `Sec-Fetch-Site`). The one exception is a
  top-level navigation to the shell route (M2b-R6, D8).
- **No CORS.** No `Access-Control-*` header is ever sent.

**Why not MCP Apps (yet).** MCP Apps is the `io.modelcontextprotocol/ui`
extension, stable since 2026-01-26. A server declares a `ui://` resource
of type `text/html;profile=mcp-app`, and a tool links it through
`_meta.ui.resourceUri`. The host renders it in a sandboxed iframe, and the
view calls tools by `postMessage` through the host. Three facts decide
against it for M2b:
1. **The user's client cannot render it.** Hosts listed in the extension's
   README (2026): ChatGPT, Claude (web, desktop), VS Code, Goose, Postman,
   MCPJam and others. **Claude Code is not a host.** It is a terminal, and
   it is where this user drives perf-agent.
2. **No server push.** The spec defines no push to an already rendered
   view, only `ui/notifications/tool-result` for the call that created it,
   and views fetch more by calling tools. "The agent selects a node and the
   open page follows" would mean a new view per tool call, or the view
   polling tools.
3. **A weaker default security model.** The default CSP allows
   `'unsafe-inline'` script, and a view's `tools/call` requests go to
   tools marked `"app"` visibility. The second fact is in tension with the
   anti-goal "the browser can never trigger tool calls".

**Why HTTP, given that.** HTTP works with every client and every browser.
The Go standard library supports it (`net/http`, and SSE with
`http.ResponseController`), so there is no new module. And a loopback
listener with a token, a Host check and fetch-metadata checks is a
well-understood pattern (Jupyter and pprof's `-http`).

**What it costs.** The M2 test "the binary links no `net`, `net/http`,
`crypto/tls`" is **reversed**: the binary now links `net/http`. It is
replaced by three tests that carry the same promise:
- the listener refuses any non-loopback or hostname bind
  (`internal/livehttp`);
- a source scan finds no client-side networking call (`net.Dial*`,
  `http.Get`/`Post`/`Head`, `http.Client`, `http.DefaultClient`,
  `http.Transport`, `tls.Dial`, resolver lookups) anywhere in the module
  packages the binary links, and no listening call outside
  `internal/livehttp`;
- a runtime test runs the built binary through a session and checks
  `/proc/<pid>/net/tcp{,6}` and `udp{,6}`: every socket the process holds
  is a loopback listener or a connection between loopback addresses.

The dependency test keeps its other halves. The binary links neither
`perfagent` nor `cilium/ebpf` nor blazesym, and its non-std modules are
still ⊆ {perf-agent, google/pprof}.

**Forward-compatible.** The page script reaches the server through one
small transport object (`getJSON(path)`, `subscribe(onState)`). A later MCP
Apps host can supply a `postMessage` transport and the same assets as one
`ui://` resource (§10).

### D2. Push: Server-Sent Events **[mine]**

- **How it works.** `GET …/events` streams `event: state` messages whose
  data is the agent-driven state (§6). The page fetches anything larger
  (models, history) with plain `GET`s when the state says it changed.
- **Why SSE:**
  - It needs only the standard library: `text/event-stream`, a flush per
    event, and a deadline cleared through `http.ResponseController`.
  - The browser's `EventSource` reconnects by itself, and every new
    connection is sent the current state first, so a reload, a second tab
    or a resumed laptop converges.
  - The direction matches the anti-goal: the server pushes and the page
    never sends.
- **WebSocket** is rejected. It needs a new module or hand-written framing
  and masking, and it is bidirectional, which we must not want.
- **Polling** is rejected. It costs latency against waste for no gain;
  `GET …/state` remains as the fallback the page uses while `EventSource`
  is reconnecting.
- **Never blocking.** Each subscriber has a one-slot, coalescing mailbox:
  the newest state replaces an unsent one. Publishing a state never waits
  on a socket. A keep-alive comment is sent every 15 s.
  `MaxPageClients` (default 4) bounds concurrent streams; one more gets
  `503`, and that tab polls `GET …/state` instead. A browser opens at most
  six HTTP/1.1 connections to one host and port, and each stream holds
  one, so the default leaves two for the tabs' fetches: with more streams
  than that, every tab's model and frame requests queue behind the
  streams and no tab follows a new publication.

### D3. What the agent drives, and what the page does on its own **[mine]**

**New tools.** Each is listed even without `--live`. Without `--live`, a
tool's description starts `DISABLED: start perf-agent-mcp with --live to
enable the live page.` and every call is `isError` with that text, as
`render_flamegraph` does without `--output`.

| Tool | Arguments | Effect | Result |
|---|---|---|---|
| `page_open` | none | starts the listener on first use of any `page_*` tool | `{url, publications, current, viewers}` |
| `page_explain` | `handle, view, explanations, attempt?, title?` | **the `render_flamegraph` check**, with the same `Decode`, `Check`, attempt recording, failure response and notices; on success publishes and makes current; selects the first explained node | on success `{attempt, status:"published", publication, url, figures_checked, unchecked_numbers, notices}`; on rejection byte-for-byte the shape `render_flamegraph` returns (`status:"rejected"`, every failure with the engine value, `isError`) and nothing is published |
| `page_show` | `publication?, node?, focus?` (at least one) | switches the current publication, selects a node (any node of its view, drawn or not), and/or focuses the panel and the zoom on it | `{publication, selected, focus, url, viewers}` |
| `page_diff` | `base_handle, base_view, new_handle, new_view, work_base?, work_new?, node?` | the `diff` tool's comparison, published as a diff view and made current | `{publication, url, base_total, new_total, quality_warnings, viewers}` |

How the tools behave:
- `page_explain` with `explanations: []` publishes a **facts-only** page of
  the view, with its rejected history. Every attempt, published or not, is
  recorded in the same attempts record, under the same IDs `a1, a2, …`
  that `render_flamegraph` uses. A retry may name an attempt made by
  either tool, and the outcome `published` joins the "others" side of
  M2-R15.
- Publications are `p1, p2, …`. They are immutable: a new explanation is a
  new publication. The server keeps `MaxPublications` (default 8)
  publications, least recently shown first, within `MaxRetainedBytes`
  (default 2 GiB, an *estimated* budget; M2b-R4). That budget counts the **estimated engine memory** the kept
  publications pin (profiles, views, diffs and their indexes, each counted
  once however many holders share it) plus their model bytes (M2b-R4, D8).
  The current publication is never evicted. A new publication that cannot
  fit even alone is refused.
- `viewers` is the number of open event streams, so the agent can tell
  whether the user has the page open. The server **never opens a
  browser**: it starts no processes besides M2's read-only `git`, and the
  agent gives the user the URL.

**What the page does on its own**, all through `GET`s that change nothing:
- select any drawn frame, using the publication's model;
- show facts for an undrawn node: `GET pub/<p>/frame/<node>`, computed by
  the engine from the publication's view;
- zoom: `GET pub/<p>/focus/<node>`, the subtree under a node laid out by
  the engine. It is bounded by `MaxPageBytes` like a page, and frames left
  out are counted. Widths are relative to the focused node and the axis
  says so, while the shares shown stay shares of the view total, as
  labelled;
- download a static copy: `GET pub/<p>/export` (D6);
- list the kept publications and switch between them. This is the user's
  local navigation; the agent's next state change wins.

**Unchanged rules:**
- **Figures** come from the engine. The live model is built in Go by the
  same builder as the static page, and the script formats no number.
- **Claims** are checked by `explain.Check` before anything is published.
  A rejected `page_explain` publishes nothing.
- **Rejected history (M2-R15..R20).** It is keyed by (profile digest,
  view), so every handle of a digest shares it and it survives store
  eviction. Its bounds stay: per view, per server, bytes, failures per
  record (32), and the size of each kept failure. The page's rejected
  section, pill and "history may be incomplete" note are built from it, by
  the same row builder as the static page.
- On the live page the history is fetched (`GET pub/<p>/history`) and
  **refreshed whenever the record for that key changes**, from either tool
  and any handle of the digest. A facts-only publication shows it too, and
  no tool argument can hide it.

### D4. Concurrency and ownership **[mine]**

`mcpstdio` handles one request at a time on its `Serve` goroutine. M2's
store, diff cache and attempts record are lock-free because of that. M2b
keeps them that way:

- **The request goroutine owns all mutable state:** store, diffs,
  attempts, the publication list and the listener's lifecycle. Tool
  handlers are the only writers, as in M2.
- **The HTTP goroutines read only an immutable snapshot.** It is
  published through an `atomic.Pointer[liveSnapshot]`:
  - `seq`, the current publication, the selected node and the focus;
  - the map of kept publications;
  - per publication, its rejected history: the structured, bounded
    `[]explain.RejectedAttempt` with its dropped count and incomplete flag,
    deep-copied from the record, plus the JSON built from it (M2b-R5, D8).

  A tool handler builds a new snapshot and swaps it in. The handlers that
  do so are `page_*`, and `render_flamegraph` when it records a rejection
  for a key some publication shows. Then the handler wakes the SSE hub. No
  HTTP handler ever touches the store, the diff cache or the attempts.
- **Publications are immutable.** Each pins the `*profilemodel.Profile`
  and `*profilemodel.View` (or the `*profilemodel.Diff` and its union
  index), the `explain.Result` it was built from, and its model bytes.
  Pinning keeps a publication valid after the store evicts its handle. Pins
  are reference-counted by pointer in one ledger the store shares, so a
  profile that is both open and published is counted once. The pins are
  bounded by `MaxPublications` and `MaxRetainedBytes` (M2b-R4).
- **Engine reads on HTTP goroutines** (frame, focus, export) call only
  read methods of a pinned `View` or `Diff`, and the flamegraph builders,
  which hold no package-level mutable state. `View` holds no lazy caches
  today: its tree and node map are complete when `Profile.View` returns. A
  `-race` test keeps it that way.
- **Why not a mutex around everything.** A tool call such as `open` of a
  large profile can take seconds. One lock would stall every page request
  behind it. With snapshots, the page never waits on the agent, and the
  agent never waits on the page.

### D5. Security **[mine; threat model in §7]**

- **Read-only.** Only `GET` is served (the mux also answers `HEAD` for
  `GET` routes), so every endpoint is a read. Any other method gets `405`
  with no `Allow` of a write and no CORS header. A test drives every
  endpoint and checks three things stay unchanged: the attempts record,
  the store, and the snapshot's `seq`.
- **Token.** It is checked in constant time (`crypto/subtle`) as the first
  path segment. A wrong or missing token gets `404` (not `403`, so there is
  no oracle). It is a per-run secret:
  - it never reaches stderr or the log, which say
    `live page listening on 127.0.0.1:PORT`;
  - it never reaches the disk;
  - it appears only in `page_*` tool results.

  It is not in a cookie, because cookies are not port-scoped, and every
  other local server would receive it.
- **DNS rebinding and cross-site requests.**
  - `Host` must be `localhost`, `127.x.x.x` or `[::1]`, with any port, so
    a browser that names the server `localhost:<port>` works.
  - `Origin`, when present, must equal `http://` + `Host`.
  - `Sec-Fetch-Site`, when present, must be `same-origin` or `none`. The
    exception: on the **shell route only**, a request with
    `Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document` passes
    whatever its `Sec-Fetch-Site`. This covers a link clicked in docs or a
    web chat. The shell has no side effects and holds no data, and every
    API, SSE and asset route keeps the strict same-origin check (M2b-R6).
  - Anything else gets `403` with a short text body.
- **Headers on every response:**
  - `Content-Security-Policy: default-src 'none'; script-src 'self';
    style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none';
    form-action 'none'; frame-ancestors 'none'`;
  - `X-Content-Type-Options: nosniff`;
  - `Referrer-Policy: no-referrer`;
  - `Cache-Control: no-store`;
  - `Cross-Origin-Opener-Policy: same-origin`;
  - `Cross-Origin-Resource-Policy: same-origin`;
  - `X-Frame-Options: DENY`.

  JSON is served as `application/json; charset=utf-8`.
- **No inline script or style on the live page.** The shell is a fixed
  HTML file with no template data in it. Script and style are separate
  assets (`assets/live.css`, `assets/panel.js`, `assets/live.js`). Frame
  geometry is set through CSSOM (`el.style.left = …`), which CSP allows,
  and never through `setAttribute('style', …)`. A headless-Chrome test
  counts `securitypolicyviolation` events: zero.
- **XSS rules as today.** Model JSON comes from `json.Marshal`, so it
  carries no raw `<`. The script uses `textContent` only: no `innerHTML`,
  `outerHTML`, `insertAdjacentHTML`, `document.write` or `eval(`. No
  agent-supplied string becomes a URL, an attribute name or a style.
  Frame names are never parsed for placeholders.
- **The server's own limits:**
  - `ReadHeaderTimeout` 5 s and `MaxHeaderBytes` 8 KiB;
  - path segments bounded (a node ID is 16 hex, a union ID 33 bytes, a
    publication ID at most 12 bytes);
  - `MaxPageClients` on streams;
  - focus and export bounded by `MaxPageBytes`;
  - `http.Server.ErrorLog` goes to the slog logger on stderr, never to
    stdout.
- **Shutdown.** When stdio reaches EOF, `Server.Close` shuts the HTTP
  server down (2 s grace, then `Close`) and ends every stream. A test
  checks that the port refuses connections afterwards.
- **Offline use of a saved live page.** It is not supported: the live
  shell is empty without its server. When the server goes away, the page
  keeps the last content it received and shows the banner:

  > perf-agent-mcp is not running; this is the last state it sent. Ask
  > your agent for a new page URL, or download a static copy while it
  > runs.

  "Download static copy" (D6) is the way to keep a page.

### D6. Static file output: keep `render_flamegraph`; export is a read **[mine]**

- **`render_flamegraph` is unchanged.** Same arguments, same checks, same
  bytes for the same inputs, same no-clobber writes under `--output`
  (M2-R4). It works without `--live`. Static files are the PR artifact for
  M3 and what the evals check.
- **Export is a read.** The page's "Download static copy" button is a link
  to `GET pub/<p>/export`. The server renders the publication's static page
  on the fly:
  - for an explained publication, `WriteExplainedPage` with the **current**
    rejected history;
  - for a diff, M1's `WriteDiffPage`, through a bounded path (M2b-R2, D8).
    An iterative walk first bounds its output bytes and node count without
    building the nested presentation. Over `MaxPageBytes` or
    `MaxPageNodes`, the export is refused with `422` and a plain-text reason
    before any `200` body. Otherwise it renders into a buffer, checks the
    size again, and sends `200` with `Content-Length`. The bytes are
    unchanged for diffs that fit. The model says whether export is
    available, so the button is disabled with the reason.

  Every export renders fully into memory first, so there is never a
  partial `200`.

  It is sent as `Content-Disposition: attachment` and with CSP
  `sandbox; default-src 'none'`, so it can never run in the server's
  origin. The browser saves it, and the server writes no file.
- **No `page_export` tool.** An agent that wants a file calls
  `render_flamegraph` with the same explanations. That records an attempt
  and writes under `--output`, as in M2. One write path keeps M2-R4's
  guarantees in one place.

### D7. Fit with the existing tests and constraints **[mine]**

- "Never starts `perfagent.Agent` or any profiler, never needs
  capabilities" still holds and is still tested. The binary links neither
  `perfagent` nor `cilium/ebpf`.
- "The only writes are pages under `--output`" still holds: the live page
  writes nothing.
- "Stdout carries only JSON-RPC" still holds. HTTP logs go to stderr, and
  the `TestStdoutCarriesOnlyJSONRPC` session gains `page_open` and
  `page_explain`.
- The goldens for `RenderHTML`, `RenderFragment` and `WriteDiffPage` stay
  byte-identical. The static explained page may change **only inside its
  `<script>` element**: its script is split so the panel code is shared
  with the live page. A new golden pins the page with that element
  stripped.
- The test that the binary links no networking is replaced, as D1
  describes. The same commit amends the M2 global constraint in this
  plan's Global Constraints.
- `perf-agent-mcp` stays source-only (spec §12, question 2). M2b changes no
  Makefile, Dockerfile or release workflow.

### D8. Rulings after review (Codex, 2026-11-14) **[binding]**

Codex reviewed this design and plan and found no DNS-rebinding or CSRF
bypass. Its findings were settled as these rulings. Each names the plan
task that owns it and the test that pins it.

- **M2b-R1: async fetch ordering in the page (Task 5).**
  - `seq` stays a decimal string end to end. The script compares it by
    length, then lexically, never through `Number`. An incoming state is
    compared with the **highest seq accepted so far** (`acceptedSeq`), not
    with the one on screen. `acceptedSeq` is set synchronously, before the
    first `await`, so a state still loading cannot be overtaken by an older
    one: shown 5, accepted 7 (model delayed), and a fallback returning 6
    refuses 6. A state with seq ≤ `acceptedSeq` is ignored, whether it came
    from SSE or from the `GET state` fallback. `renderedSeq` is tracked
    apart and only for display.
  - Every agent state the page accepts, and every local navigation,
    increments a generation counter. Each `await → DOM commit` chain
    captures the generation and drops its result if it changed.
  - A publication's model and history are committed together.
  - A history response carries `publication` and `version`. It is applied
    only to that publication, and only when its version is newer than the
    one shown.
  - Test: the Chrome ordering run. It reverses the completion order of
    model and history, history v1 against v2, and frame and focus fetches,
    and lets a stale local-navigation response arrive after an agent
    update. The final DOM reflects the newest agent state each time.
- **M2b-R2: bounded diff export (Tasks 9 and 10).** `WriteDiffPage` has no
  byte or node budget and builds the whole nested presentation first.
  `WriteDiffPageBounded` therefore:
  - bounds the output and the node count by an iterative walk that builds
    nothing, and refuses with `ErrPageTooLarge` before any construction;
  - otherwise renders into a buffer of at most `MaxPageBytes`.

  The route answers `422` before any `200` body. The bytes equal
  `WriteDiffPage` for inputs that fit. Test: a diff with long names that
  is both deep and wide, where the live model fits (frames left out) but
  the full export exceeds the limit. It must give `422`, no partial body,
  and a bounded allocation during refusal.
- **M2b-R3: `page_show` on a diff works for any union node (Tasks 9 and
  10).** A `profilemodel.DiffIndex` over the full pinned union backs the
  page and its routes. It is built once at publication, iteratively, and
  counted in the retained budget. Both `page_show` validation and
  `DiffLiveFrame` (facts and path) use it; neither looks in the serialized
  model's frames. Test: select a node the budget left out of the model,
  and check its facts and its root path.
- **M2b-R4: the retained-engine budget (Task 6).** `MaxRetainedBytes`
  replaces `MaxPublishedBytes`. It bounds the **estimated** engine memory
  publications pin (profile, view, diff, index), plus model bytes. The
  estimates are calibrated against measured heap to within [0.5×, 2×], so
  the budget is an estimated retained-engine budget, not a physical memory
  ceiling: real heap may be up to twice it. The docs and the limit's own
  comment say so.
  - Admission is preflighted: the evictions are simulated on a copy of the
    order, and committed only when the result fits. A refused publication
    leaves order, current and pins exactly as they were.
  - Pins are keyed by pointer in one ledger the store also registers with,
    so a profile held by the store and by publications is counted once.
  - The current publication is admitted only if it fits once older ones
    are evicted, else it is refused with a message naming the limit.
  - Tests: a large input with a tiny visible model is charged for its
    engine memory; store eviction while a page is published keeps the pin,
    counted once, and releases it when the publication goes.
- **M2b-R5: the snapshot carries structured history (Tasks 6 and 7).**
  Each publication's history in the snapshot is an immutable, bounded
  copy: `[]explain.RejectedAttempt` (already bounded by M2-R17, R20 and
  R24) with its dropped count and incomplete flag, plus its JSON. Export
  renders `WriteExplainedPage` from that copy, so no HTTP goroutine reads
  `s.attempts`. Test (`-race`): exports run while new rejections arrive
  and attempt views and publications are evicted.
- **M2b-R6: navigations to the shell (Task 1).** A token-authenticated
  request with `Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document`
  passes whatever its `Sec-Fetch-Site`, on the shell route (and its slash
  redirect) only. API, SSE, asset and export routes keep the strict check.
  Test: a matrix of real navigation headers over route, `Sec-Fetch-Site`,
  mode and destination.

**M2 rulings that keep applying.** M2's runtime rulings M2-R20..R25 bind
the live page as they bind the static one:
- the history byte budget (`MaxHistoryBytes`), with oversized failures
  counted rather than kept (R20);
- `MaxMetaBytes` for sample type and unit names, and `MaxCommentBytes` for
  comments (R20, R23);
- user-selected source only inside the named root (R22);
- never rendering the full history before fitting, with displayed frame
  and row names capped at 1 KiB with an ellipsis, the node ID staying the
  reference (R24);
- `--source-git` on by default in the CLI, with `Config.SourceGit` false by
  default (R25).

The live models and the history JSON reuse the builders those rulings
already constrain, and Task 4's tests include a 64 KiB frame name.

## 5. Architecture

```
  agent client (Claude Code)          browser tab (same host)
        │ stdio JSON-RPC                     ▲ GET + SSE, loopback, token
        ▼                                    │
  ┌──────────────┐  tool calls   ┌───────────┴────────────┐
  │ mcpstdio     │──────────────▶│ perfmcp.Server          │
  │ (sequential) │               │  request goroutine owns │
  └──────────────┘               │  store, diffs, attempts,│
                                 │  publications           │
                                 │        │ builds         │
                                 │        ▼                │
                                 │  liveSnapshot (immut.)  │
                                 │  atomic.Pointer ────────┼──▶ livehttp handlers
                                 └─────────────────────────┘    (guard, hub, routes)
                                          │ pinned View/Diff
                                          ▼
                                 flamegraph live builders (read-only)
```

- **`internal/livehttp`** (new, no perf-agent knowledge, like `mcpstdio`)
  holds three pieces:
  - the loopback listener (`Listen`);
  - the request guard (token, Host, Origin, fetch metadata, method,
    headers);
  - the SSE hub.
- **`internal/flamegraph`** gains:
  - the live model builders (`ExplainedLiveModel`, `ExplainedLiveFrame`,
    `ExplainedLiveHistory`, `DiffLiveModel`), the same builder as the
    static page with JSON output;
  - the shared panel script;
  - the fixed live shell, its CSS and its script.
- **`internal/perfmcp`** gains the publications, the snapshot, the routes
  and the four `page_*` tools.

## 6. HTTP surface

All paths are below `/s/<token>/` and all are `GET`. `<p>` is a publication
ID; `<node>` is a node ID, or a union ID for a diff.

| Path | Returns |
|---|---|
| `` (the prefix itself; `/s/<token>` redirects to it) | the fixed shell HTML |
| `assets/live.css`, `assets/panel.js`, `assets/live.js` | embedded assets |
| `state` | `{"seq":"N","current":"p3","selected":"<id>","focus":"<id>","publications":[{"id","kind","title","view","digest","history":"<version>"}]}`; `history` moves when that publication's rejected record changes |
| `events` | SSE: on connect and on every change, `event: state` with the `state` JSON as data; `: keep-alive` every 15 s |
| `pub/<p>/model` | the publication's model JSON (explained or diff), fixed at publish time |
| `pub/<p>/history` | `{"publication","version",…}`: rejected rows and pill texts from the snapshot's copy of the server's record (explained only; empty rows for a diff) |
| `pub/<p>/frame/<node>` | one frame's facts, path and explanation (if any), from the engine. For a diff it comes from the full union index, not from the serialized model, which the budget may cut (M2b-R3) |
| `pub/<p>/focus/<node>` | the subtree under node, laid out, bounded by `MaxPageBytes` |
| `pub/<p>/export` | the static page as an attachment (D6), or `422` with a reason when it does not fit (M2b-R2) |

Everything else returns `404`. Integers are decimal strings everywhere, as
on the MCP wire.

## 7. Threat model

| Adversary | Can | Cannot, because |
|---|---|---|
| A web page in the user's browser (another site, an ad) | make the browser send requests to `127.0.0.1:*` | read anything: it lacks the token. With a leaked token, cross-site fetch and EventSource fail on `Sec-Fetch-Site`/`Origin`, a `<script src>` include fails on `Sec-Fetch-Site: cross-site`, and responses carry `nosniff` and CORP `same-origin`. It cannot write: there are no write endpoints. A cross-site *navigation* to the shell is allowed (M2b-R6), but the shell holds no data and every request it then makes is same-origin |
| A DNS-rebinding page (`evil.example` → 127.0.0.1) | reach the port as a "same-origin" request to `evil.example` | pass the `Host` check, which allows loopback literals and `localhost` only |
| Another local user | connect to the loopback port | guess 256 bits of token. Token comparison is constant time, and a wrong token gets `404` |
| Someone on the network | — | reach a loopback-only socket; a non-loopback bind is refused at start-up |
| Hostile profile content (frame names, units, comments) | put markup or placeholder-like text into names | execute: `textContent` only, JSON-escaped model, CSP `script-src 'self'`, metadata bounded at load (M2-R20) |
| The agent | publish prose, select frames, switch pages | make an unchecked figure look checked (`explain.Check` before publishing, engine values only), hide rejections (history from the server's record, refreshed live), add links or styles (no agent string becomes a URL or a style) |
| A stuck or malicious tab holding the token | open streams, request focus and export repeatedly | block the agent (coalescing mailboxes, snapshot reads), exceed `MaxPageClients` streams, or produce responses over `MaxPageBytes`. CPU use by focus and export is the user's own tab's, which is accepted |

Out of scope: browser extensions and malware running as the user; they
can already read the profile files.

## 8. What changes vs M2

| M2 | M2b |
|---|---|
| Binary links no `net`, `net/http`, `crypto/tls` (test) | Links `net/http`; tests: loopback-only bind, no client networking in source, every runtime socket loopback |
| Request handling sequential, no locks | Still sequential and lock-free for store, diffs, attempts; HTTP reads an atomic immutable snapshot |
| Only output: files under `--output` | Unchanged; the live page writes nothing |
| `render_flamegraph` writes the page | Unchanged; `page_explain` runs the same check and publishes instead |
| Rejected history at write time | Static page: unchanged. Live page: the current record, refreshed on change |
| Static page script inline | Static page still inline; the panel code is shared with the live assets, and the page is byte-identical outside `<script>` |
| Flags: `--profiles --source --source-manifest --output --source-git --limit --version` | Adds `--live` (default off; 127.0.0.1 on a random port, not configurable); new limits `MaxPublications`, `MaxRetainedBytes`, `MaxPageClients` through `--limit` |
| Guide: investigation order ends in `render_flamegraph` | Adds a live-page section: `page_open`, give the user the URL, then `page_explain`, `page_show` and `page_diff`; `render_flamegraph` for files |

## 9. Testing

- **Deterministic, no model, `-race`:**
  - guard and listener tests against a real loopback listener;
  - hub tests: slow subscriber, reconnect, limit, close;
  - live model parity with the static page: same pills, frames, rows,
    rejected rows and figures;
  - publication and snapshot tests;
  - tool tests through the stdio harness, with the test acting as the
    browser over HTTP;
  - a race test: tool calls and page `GET`s at once;
  - "no `GET` mutates";
  - the replaced dependency test, the source scan and the runtime socket
    test.
- **Headless Chrome** (skipped without Chrome, as in M2 Task 10). The live
  page loads against a test server with the real CSP, and the test checks:
  - zero CSP violations;
  - a state pushed over SSE changes the selected frame within the virtual
    time budget;
  - after the server closes, the banner text appears.
- **Manual smoke** (verification task): Claude Code with `--live` over the
  eval fixtures. The agent publishes, selects and diffs, and the user's tab
  follows. Record what was checked in the PR body.

## 10. Forward compatibility

- **MCP Apps.**
  - `live.js` talks to the server only through
    `transport = {getJSON(path), subscribe(onState)}`.
  - A later `ui://perf-agent/live` resource can inline the same assets and
    supply a `postMessage` transport, reading the same JSON through
    app-visible read-only tools.
  - The data contracts in §6 are what such tools would return.

  This is not built in M2b.
- **Capture (next milestone).** The listener and the snapshot are where a
  capture's progress would appear. Capture itself would be a tool on the
  request goroutine, which invokes the setcap'd `perf-agent`.
  `perf-agent-mcp` itself still needs no capabilities.

## 11. Research notes (MCP Apps, November 2026)

- The extension is `io.modelcontextprotocol/ui`, spec version `2026-01-26`
  (stable). Its resource type is `text/html;profile=mcp-app`, and a tool
  links its UI through `_meta.ui.resourceUri`.
- Clients advertise it under `capabilities.extensions`.
- The default CSP is `default-src 'none'`, with `script-src` and
  `style-src` set to `'self' 'unsafe-inline'` and `connect-src 'none'`.
- A view calls `tools/call` by `postMessage` through the host. Tools with
  `"app"` visibility are callable by views.
- Updates reach the view as `ui/notifications/tool-input` and
  `ui/notifications/tool-result` for the call that rendered it. The spec
  defines no server push to a rendered view.
- Hosts in the README: ChatGPT, Claude (web and desktop), VS Code, Goose,
  Postman, MCPJam, mcp-use Inspector and Alpic Playground. Claude Code is
  not listed.
- Sources: `@modelcontextprotocol/ext-apps` 2.0.0 README
  (cdn.jsdelivr.net/npm/@modelcontextprotocol/ext-apps@2.0.0/README.md);
  the specification at
  github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx;
  modelcontextprotocol.io/extensions/apps. These were read from the
  published docs, not tested against a host.

## 12. Open questions for the user

**Decided 2026-11-14 by the user: every default below holds** (loopback listener opt-in with `--live`; `--live` off by default; the page stays read-only and never reports clicks to the agent; the server never opens a browser, the agent gives the URL). Question 5 was answered differently: **no remote or SSH use in M2b** — the user considers it a risk vector not needed now; there is no `--live-addr` flag (127.0.0.1, random port only) and the docs do not describe SSH forwarding (ruling M2b-R7).

Most important first.

1. **May `perf-agent-mcp` open a loopback listening socket?** This is D1.
   It reinterprets the spec's anti-goal "perf-agent opening a network
   connection" as "no outbound connection, no non-loopback listener", and
   reverses M2's "links no `net/http`" test.
   - **Alternative:** wait for Claude Code to host MCP Apps.
   - **Default:** yes, loopback HTTP, opt-in with `--live`.
2. **Should `--live` be on by default?** If it is on, a session that never
   uses the page still has an open (token-protected) port, though only
   after the first `page_*` call, since the listener starts lazily.
   - **Default:** off; the user adds `--live` to the server's command in
     the MCP config, as they add `--output`.
3. **Should the page be able to tell the agent what the user selected**
   (for example `page_state` returning the user's click)? That needs the
   browser to send something, which this design forbids.
   - **Default:** no. The page is read-only, and the follow-up chips copy a
     prompt naming the node.
4. **Should the server open the browser itself** (`xdg-open`)?
   - **Default:** no. It runs no process besides M2's read-only `git`; the
     agent prints the URL and the user clicks it.
5. **Remote GPU lab use.** *(Answered: out of scope, see above.)* Was:
   documenting SSH port forwarding with an address flag for the lab machine.
   - **Default:** yes; the Host check accepts any loopback port for this
     reason.
