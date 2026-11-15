# The Live Page (M2b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `perf-agent-mcp --live` serves the explained flame graph as a live, read-only page on a loopback listener. The agent drives it through MCP tools: `page_open`, `page_explain`, `page_show` and `page_diff`. Every figure stays the engine's, every claim is checked first, and the rejected history comes from the server's own record.

**Architecture:**
- **`internal/livehttp`** (new, no perf-agent knowledge) holds the loopback listener, the token and request guard, and a Server-Sent Events hub.
- **`internal/flamegraph`** gains JSON "live models" built by the same builders as the static pages, plus fixed live assets (shell, CSS and scripts). Its panel script is shared with the static page.
- **`internal/perfmcp`** keeps M2's single-goroutine ownership of store, diffs and attempts. It adds:
  - immutable publications, exposed to the HTTP goroutines through an `atomic.Pointer` snapshot;
  - the HTTP routes;
  - the four `page_*` tools.
- **`cmd/perf-agent-mcp`** gains `--live` (127.0.0.1, random port; no address flag — ruling M2b-R7). The M2 "links no `net/http`" test is replaced by loopback-only, no-client-networking and every-socket-loopback tests.

**Tech Stack:** Go 1.26 standard library (`net/http`, `net/netip`, `crypto/rand`, `crypto/subtle`, `sync/atomic`, `go/parser` for the source scan), the existing `html/template` and `encoding/json`, and vanilla JS with no build step. No new module.

**Spec:** `docs/superpowers/specs/2026-11-14-live-page-design.md` (the M2b addendum; its D1–D7 are what this plan builds) on top of `docs/superpowers/specs/2026-11-13-ai-flamegraph-design.md`. Base: M2 as it is after `docs/superpowers/plans/2026-11-14-mcp-server-m2.md` Task 16. M2 as merged into this branch's base (ai-mcp-m2 @ 930191cd). M2 rulings M2-R1..R32 (listed in the M2 PR description and docs/mcp.md) still bind.

## Global Constraints

- Go 1.26. Invoke the modern-go guidelines skill before writing Go (`new(v)`, `errors.AsType`, `t.Context()`, `omitzero`, `wg.Go`, `strings.SplitSeq`, `slices`/`maps`/`cmp`).
- Tests run with `-race`. Lint with `golangci-lint` v2.11.4 (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`); a missing binary exits 127, and that is not a pass.
- Whole-repo commands need the CGO variables: `CGO_CFLAGS=-I/home/diego/github/blazesym/capi/include` and `CGO_LDFLAGS=-L/home/diego/github/perf-agent/.worktrees/ai-mcp-m2/.superpowers/sdd/2026-11-14-mcp-server-m2/blazesym-lib`. The M2b packages do not import blazesym.
- **Network (amends M2's "links none of `net`, `net/http`, `crypto/tls`").**
  - The server opens no outbound connection, ever: no dial, no resolver lookup, no HTTP client.
  - It listens only when started with `--live`, only after the first `page_*` call, and only on a loopback IP literal. Hostnames, `0.0.0.0`, `::` and non-loopback addresses are refused.
  - It links `net/http` for that listener only. Tests: Tasks 1 and 11.
- "It never starts `perfagent.Agent` or any profiler, and never needs capabilities." The binary links neither `github.com/dpsoft/perf-agent/perfagent`, `github.com/cilium/ebpf` nor `github.com/libbpf/blazesym/go`. Its non-std modules are ⊆ {`github.com/dpsoft/perf-agent`, `github.com/google/pprof`}.
- MCP stays stdio-only. Stdout carries only JSON-RPC; every log line, the HTTP server's included, goes to stderr. **The page token never appears in a log line**: it appears only in `page_*` tool results.
- The only file writes are `render_flamegraph` pages under `--output`, unchanged from M2. The live page writes no file; export is a download the browser saves.
- **The browser never changes server state.**
  - Only `GET` (and `HEAD`) is served, and no request handler mutates the store, diffs, attempts, publications or snapshot.
  - No `Access-Control-*` header is ever sent.
- **Ownership.** The mcpstdio request goroutine alone writes the store, diffs, attempts, publication list and listener lifecycle. HTTP handlers read only the `*liveSnapshot` loaded from an `atomic.Pointer`, and the immutable publications it holds (with their pinned `*profilemodel.View`/`*profilemodel.Diff`).
- Root enforcement through `os.Root` as in M2; no textual prefix checks.
- No new third-party dependency. `scripts/third-party-notices.sh --check` stays green without regenerating.
- Every value, count and weight in JSON (MCP and HTTP) is a decimal **string**; a test carries 9007199254740993 (2^53+1). "Zero" and "unavailable" are never interchangeable. Every GPU view shows its sampling notes. Nothing is multiplied by `gpu_sample_period`.
- "Rounding policy is the engine's." `page_explain` runs exactly `render_flamegraph`'s `explain.Decode` → `explain.Check` flow and refuses to publish on any failure.
- **Rejected history** (M2-R15..R20) is keyed by (profile digest, view), bounded as in M2, and never taken from the agent. A live publication of an explained view, facts-only included, shows the server's **current** record for its key. That record is refreshed whenever it changes, from either tool and through any handle of the digest. No argument hides it.
- **Existing outputs do not change.**
  - `RenderHTML`, `RenderFragment` and `WriteDiffPage` stay byte-identical; their goldens are unchanged.
  - `render_flamegraph`'s results and written bytes are unchanged for the same inputs.
  - The static explained page is byte-identical except inside its inline `<script>` element (golden from Task 3).
- **Live page script rules.**
  - No inline script or style, and no `style=` attributes, in the shell. Geometry is set through CSSOM properties only (no `setAttribute('style'`, no `cssText`).
  - DOM text through `textContent` only: no `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write` or `eval(`.
  - The script never formats a number (no `toFixed`, `toPrecision`, `toLocaleString`, `Intl.`, `parseFloat`, `Math.round`).
  - Every URL it requests is relative. No agent-supplied string becomes a URL.
- CSP on every live response: `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`. Export responses use `sandbox; default-src 'none'` instead.
- **Review rulings M2b-R1..R6 (design D8) bind:**
  - **R1:** the page commits a fetch only if its generation is still current. It pairs a publication with its history version, and keeps `seq` as a monotonic decimal string, never a `Number`.
  - **R2:** every export renders into memory first. A diff export is bounded before construction, and refused with `422` before any `200` body.
  - **R3:** diff frame lookups and `page_show` use the full union index, not the serialized model.
  - **R4:** `MaxRetainedBytes` is an **estimated** retained-engine budget, not a physical ceiling (the estimates are within [0.5×, 2×] of heap). It covers pinned engine memory plus model bytes. Pins are shared and counted once. Admission simulates its evictions and commits only on success; a publication that cannot fit is refused and changes nothing.
  - **R5:** the snapshot holds immutable structured history, and no HTTP goroutine reads `s.attempts`.
  - **R6:** cross-site top-level navigations reach the shell route only.
- M2-R20..R25 keep applying: the history bytes budget, `MaxMetaBytes`/`MaxCommentBytes`, no full-history render before fitting, displayed names capped at 1 KiB, and the `--source-git` CLI default on with `Config.SourceGit` false.
- `perf-agent-mcp` stays source-only: no Makefile `build`, Dockerfile, release workflow or notices-script change.
- Comments say what the code does and why. No issue numbers or release history in comments.
- **Commits.**
  - No Co-Authored-By, no AI attribution, no session URLs.
  - Subjects use the package prefix (`livehttp: …`, `flamegraph: …`, `perfmcp: …`, `perf-agent-mcp: …`).
  - Work on branch `ai-mcp-m2b`, created from `ai-mcp-m2` at the M2 Task 16 head.
  - Push only in Task 12. Never merge.

## Review Focus

1. **A web page in the same browser targets the port** (cross-site fetch, `<script src>` include, EventSource, DNS rebinding through `evil.example` → 127.0.0.1). Expected: nothing is readable without the token. With the token, cross-site and rebinding requests still get `403`. Tests in Task 1, and through the real routes in Task 7.
2. **The browser names the server as `localhost:<port>` instead of `127.0.0.1:<port>`** (a user retyping the URL). The browser sends `Host: localhost:<port>` and `Origin: http://localhost:<port>`; the page must work. Test in Task 1. Remote/SSH use is out of scope (M2b-R7) and is not documented.
3. **A reload, a second tab, a laptop resume, or a tab that stops reading.**
   - Every (re)connecting tab must show the current publication and selection.
   - A stuck tab must never delay a tool call.

   Tests in Task 2, and end to end in Task 7.
4. **Claude Code restarts the MCP server while the tab is open** (a new port and token). The old tab must say the server is gone and keep its last content. The old token must be `404` on the new server. Tests in Task 5 (banner, headless Chrome) and Task 7.
5. **Laundering rejections through the live page.** The cases: publishing facts-only after a rejection; rejecting through an execlog handle of the same digest while a page is shown; publishing through a second handle. Expected: the page shows every rejection the server recorded for that (digest, view), and it updates while the page is open. Tests in Task 8.

## Builder's calls settled here

| Call | Decision | Reason |
|---|---|---|
| Package split | `internal/livehttp` (listener, guard, hub; knows nothing of profiles), live builders in `internal/flamegraph`, routes and tools in `internal/perfmcp` | Mirrors M2's `mcpstdio`/`perfmcp` split; the security surface is testable alone. |
| Listener start | Lazily, at the first `page_*` call; `--live` is the consent | No open port in sessions that never use the page. |
| Token | 32 random bytes, base64url without padding (43 chars), first path segment `/s/<token>/` | Not a cookie: cookies are not port-scoped, so other local servers would receive one. |
| Host check | Host is `localhost`, an IPv4 loopback literal or `[::1]`, with **any** port | Defeats DNS rebinding; any port because the port is random. |
| Push | SSE with a one-slot coalescing mailbox per client; keep-alive 15 s; `MaxPageClients` 8 | Stdlib only; auto-reconnect; never blocks a tool call. |
| State shape | `GET state` and SSE data: `{"seq","current","selected","focus","publications":[{"id","kind","title","view","digest","history"}]}`; `history` is a decimal-string version per publication | The page refetches a model only when `current` changes, and history only when its version changes. |
| Publications | `p1, p2, …`, immutable, pin their Profile and View (or Diff and DiffIndex) and model bytes; `MaxPublications` 8, `MaxRetainedBytes` 2 GiB over a pointer-keyed pin ledger shared with the store; LRU by last shown; current never evicted; a new current that cannot fit is refused (M2b-R4) | Valid after store eviction; the budget bounds real engine memory, not only JSON. |
| Static output | `render_flamegraph` unchanged; `GET pub/<p>/export` streams the static page as an attachment; no `page_export` tool | One write path keeps M2-R4 in one place; a download is a read. |
| Diff on the live page | A new flat JSON diff model drawn by `live.js`; frames and `page_show` resolved through a full `DiffIndex`; `WriteDiffPageBounded` serves the export only | `WriteDiffPage` nests one `div` per depth, uses `style=` attributes and has no budget; the live page must be flat and inline-style-free, and the export bounded (M2b-R2, R3). |
| Shared panel code | `explainpanel.js` defines `perfAgentPanel(model, hooks)`; the static page inlines it before `explainpage.js`, and the live page serves it as `assets/panel.js` | One implementation of the panel; M2 noted mirrored-logic drift. |

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/livehttp/{doc.go,listen.go,guard.go}` | loopback listener, address check, token, request guard, security headers, `*http.Server` | 1 |
| `internal/livehttp/hub.go` | SSE hub | 2 |
| `internal/flamegraph/{explainpanel.js,explainpage.css}`, `explainpage.{go,js,tmpl}`, `testdata/explainpage-*.golden.html` | shared panel and CSS; static page refactor pinned by goldens | 3 |
| `internal/flamegraph/livemodel.go` | `ExplainedLiveModel`, `ExplainedLiveFrame`, `ExplainedLiveHistory` | 4 |
| `internal/flamegraph/{livepage.html,live.css,live.js,liveassets.go}` | the fixed shell, its CSS and script, `LiveAsset` | 5 |
| `internal/perfmcp/{live.go,pins.go}`, `internal/profilemodel/{retained.go,concurrent_test.go}` | publications, snapshot, structured history, pin ledger and retained budget; size estimates; View read safety | 6 |
| `internal/perfmcp/liveroutes.go` | listener lifecycle and GET routes | 7 |
| `internal/perfmcp/tool_page.go`, `tool_render.go` (shared check) | `page_open`, `page_explain`, `page_show` | 8 |
| `internal/profilemodel/diffindex.go`, `internal/flamegraph/{livediff.go,diffbound.go}`, `live.js` | `DiffIndex`, `DiffLiveModel`, `DiffLiveFrame`, `WriteDiffPageBounded` and the diff drawing | 9 |
| `internal/perfmcp/tool_page_diff.go`, `tool_diff.go` (shared compare) | `page_diff` | 10 |
| `cmd/perf-agent-mcp/main.go`, `main_test.go`, `internal/perfmcp/guide.md`, `docs/mcp.md`, `CHANGELOG.md` | flags, network tests, guide, docs | 11 |

---

### Task 1: Loopback listener and request guard (`internal/livehttp`)

**Files:**
- Create: `internal/livehttp/doc.go`, `listen.go`, `guard.go`, `listen_test.go`, `guard_test.go`

**Interfaces:**
- Consumes: nothing from perf-agent.
- Produces:
  - **Address check.** `func CheckAddr(addr string) (netip.AddrPort, error)`:
    - parses with `netip.ParseAddrPort` and accepts only an IP literal whose `Addr().Unmap().IsLoopback()` is true;
    - a hostname gets `live address %q must be a loopback IP literal such as 127.0.0.1:0 or [::1]:0, not a hostname`;
    - any other address gets `live address %q is not loopback; perf-agent-mcp serves the live page to this machine only`.
  - **Listener.** `func Listen(addr string) (net.Listener, error)` calls `CheckAddr`, then `net.Listen("tcp", ap.String())`. It is the only listening call in the module (Task 11 scans for that).
  - **Token.** `type Token string`; `func NewToken() (Token, error)` makes 32 bytes from `crypto/rand`, encoded with `base64.RawURLEncoding`.
  - **CSP constants.** `const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"`; `const ExportCSP = "sandbox; default-src 'none'"`.
  - **Guard.** `func Guard(tok Token, next http.Handler) http.Handler`, in this order:
    1. Set the security headers on every response: `CSP`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin`, `X-Frame-Options: DENY`.
    2. A method other than `GET`/`HEAD` gets `405`.
    3. A Host whose host part is not `localhost`, an IPv4 loopback literal or `::1` (any port) gets `403` `bad Host`.
    4. An `Origin` present and not equal to `"http://" + r.Host` gets `403`.
    5. A `Sec-Fetch-Site` present and not `same-origin` or `none` gets `403`, **except** (M2b-R6) on the shell route (`/s/<tok>/` and its `/s/<tok>` redirect) when `Sec-Fetch-Mode` is `navigate` and `Sec-Fetch-Dest` is `document`; such a request skips steps 4 and 5. The token and Host checks still apply. `func isShellNavigation(r *http.Request, tok Token) bool` decides it.
    6. A path that is not `/s/<tok>` or below gets `404`, with the token compared by `subtle.ConstantTimeCompare` and the body never echoing the path.
    7. `/s/<tok>` gets `301` with `Location: /s/<tok>/`.
    8. Otherwise, call `next` with the prefix `/s/<tok>` stripped, so `next` sees `/`, `/state` and so on.

    Handlers may replace `Content-Security-Policy` (export does).
  - **Server.** `func NewServer(h http.Handler, log *slog.Logger) *http.Server` sets `ReadHeaderTimeout: 5s`, `IdleTimeout: 60s`, `MaxHeaderBytes: 8 << 10` and `ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)`.

- [ ] **Step 1: Write the failing tests** (a real listener from `Listen("127.0.0.1:0")`, `NewServer(Guard(tok, echo))`, and requests built with `http.NewRequest` so Host, Origin and Sec-Fetch-Site can be set)

```go
func TestCheckAddrAcceptsLoopbackLiterals(t *testing.T) // "127.0.0.1:0", "127.0.0.2:7777", "[::1]:0", "[::ffff:127.0.0.1]:0" → nil
func TestCheckAddrRefusesEverythingElse(t *testing.T)   // "0.0.0.0:0", "[::]:0", "192.168.1.10:0", "10.0.0.1:80", ":0", "", "localhost:0", "example.com:80" → error; hostnames name "hostname", others "not loopback"
func TestListenBindsOnlyTheCheckedAddress(t *testing.T) // Listen("127.0.0.1:0").Addr() is 127.0.0.1:<port>; Listen("0.0.0.0:0") errors and binds nothing
func TestTokensAreLongURLSafeAndDistinct(t *testing.T)  // 1000 tokens: len 43, match ^[A-Za-z0-9_-]{43}$, all distinct
func TestGuardRefusesAWrongOrMissingToken(t *testing.T) // "/", "/s/", "/s/<other token>/", "/s/<tok>x/" → 404; body does not contain the requested path; next not called
func TestGuardRedirectsThePrefixToItsSlash(t *testing.T) // "/s/<tok>" → 301 Location "/s/<tok>/"; "/s/<tok>/state" → next sees "/state"
func TestGuardChecksHostAgainstRebinding(t *testing.T)  // Host "evil.example:PORT", "127.0.0.1.evil.example:PORT", "[fe80::1]:PORT" → 403; "127.0.0.1:PORT", "localhost:9000", "[::1]:5", "127.0.0.5:1" → 200
func TestGuardChecksOriginAndFetchMetadata(t *testing.T) // Origin "http://evil.example" / "null" / "http://localhost:9001" with Host localhost:9000 → 403; Sec-Fetch-Site "cross-site" / "same-site" → 403; "same-origin", "none", absent → 200
func TestGuardAcceptsLocalhostName(t *testing.T)  // Host "localhost:9000" + Origin "http://localhost:9000" + Sec-Fetch-Site same-origin → 200
func TestNavigationHeaderMatrix(t *testing.T)           // table over route {"/s/<tok>/", "/s/<tok>", "/s/<tok>/state", "/s/<tok>/events", "/s/<tok>/assets/live.js", "/s/<tok>/pub/p1/export"} × Sec-Fetch-Site {absent, none, same-origin, same-site, cross-site} × (Mode, Dest) {(navigate, document), (navigate, iframe), (cors, empty), (no-cors, script), (absent, absent)}: shell routes with (navigate, document) pass for every site (200, or 301 for the slashless form); every other cell passes only for site absent/none/same-origin; a cross-site navigate+document to the shell with a wrong token is 404 and with Host evil.example is 403
func TestGuardServesOnlyReads(t *testing.T)             // POST, PUT, DELETE, PATCH, OPTIONS → 405, next never called; HEAD → 200
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) // for 200, 301, 403, 404, 405: Content-Security-Policy == CSP, nosniff, no-referrer, no-store, COOP, CORP same-origin, X-Frame-Options DENY; no header starting "Access-Control-"
func TestServerBoundsRequestHeaders(t *testing.T)       // a 16 KiB header → 431
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/livehttp/`
Expected: FAIL (package missing).

- [ ] **Step 3: Implement `doc.go`, `listen.go` and `guard.go`.** `doc.go` states the package's promise: loopback only, read-only, no client. Parse the Host with `net.SplitHostPort`; a host without a port is refused.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/livehttp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/livehttp
git commit -m "livehttp: a loopback-only listener and a read-only request guard for the live page"
```

### Task 2: The SSE hub

**Files:**
- Create: `internal/livehttp/hub.go`, `internal/livehttp/hub_test.go`

**Interfaces:**
- Consumes: Task 1 `NewServer`, `Listen` (for tests).
- Produces:
  - `type Hub struct`; `func NewHub(maxClients int, keepAlive, writeTimeout time.Duration) *Hub`.
  - `func (h *Hub) Publish(data []byte) error`:
    - refuses `data` containing `\n` or `\r` (`ErrMultiline`);
    - stores it as the latest;
    - puts it in every client's one-slot mailbox, replacing an unsent value;
    - never blocks.
  - `func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request)`:
    - beyond `maxClients` → `503`, and after `Close` → `503`;
    - otherwise `Content-Type: text/event-stream`, `Cache-Control: no-store`;
    - writes the latest (if any) at once, then each mailbox value as `event: state\ndata: <data>\n\n`, and `: keep-alive\n\n` after `keepAlive` of silence;
    - every write gets a deadline of `writeTimeout` through `http.ResponseController.SetWriteDeadline` and is flushed; a failed write ends the stream;
    - ends when the request context ends or at `Close`.
  - `func (h *Hub) Clients() int`; `func (h *Hub) Close()` (idempotent).

- [ ] **Step 1: Write the failing tests** (the hub behind a real loopback server; the client reads lines with `bufio.Reader`)

```go
func TestHubSendsTheLatestStateOnConnect(t *testing.T)     // Publish(`{"seq":"1"}`) then connect → first event data {"seq":"1"}; a second client also gets it
func TestHubConvergesOnTheNewestState(t *testing.T)        // publish seq 1..200 without pause → the client's last event is seq 200, seqs never decrease
func TestAStuckClientNeverBlocksPublish(t *testing.T)      // a client that connects and never reads; 10000 Publish calls return within 1s; a reading client still converges
func TestAStuckClientIsDropped(t *testing.T)               // writeTimeout 100ms, a client that never reads, 256 published states of 64 KiB each → its stream ends within 5s and Clients() drops to 0
func TestHubLimitsClients(t *testing.T)                    // maxClients 2: third connect → 503; after one disconnects, a new connect succeeds
func TestHubSendsKeepAlives(t *testing.T)                  // keepAlive 50ms → a ": keep-alive" line within 500ms
func TestHubCloseEndsStreamsAndRefusesNew(t *testing.T)    // Close → open streams read EOF; a new connect → 503; Clients() == 0
func TestPublishRefusesMultilineData(t *testing.T)         // "a\nb" → ErrMultiline; nothing sent
func TestAReconnectGetsTheCurrentState(t *testing.T)       // connect, publish 3, disconnect, publish 4, reconnect → first event seq 4
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/livehttp/ -run 'Hub|Stuck|Publish|Reconnect'`
Expected: FAIL.

- [ ] **Step 3: Implement `hub.go`.** Use a `sync.Mutex` over the latest value and the client set. Each client has a `chan []byte` of capacity 1; Publish replaces its value with a non-blocking drain-then-send under the hub's lock. `Close` closes a `done` channel.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/livehttp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/livehttp
git commit -m "livehttp: a server-sent events hub whose clients never block the publisher"
```

### Task 3: Share the explained page's panel script and CSS

**Files:**
- Create: `internal/flamegraph/explainpanel.js`, `internal/flamegraph/explainpage.css`, `internal/flamegraph/explainpage_golden_test.go`, `internal/flamegraph/testdata/explainpage-cpu.golden.html`, `internal/flamegraph/testdata/explainpage-gpu.golden.html`
- Modify: `internal/flamegraph/explainpage.go`, `explainpage.tmpl`, `explainpage.js`

**Interfaces:**
- Consumes: M2 `WriteExplainedPage` and its test helpers (`exView`, `exCheck`, `exPage`, `exOpts`, `exNode`).
- Produces:
  - **`explainpanel.js`** defines one global function, `perfAgentPanel(model, hooks)`. `hooks` is `{getFrame(i), onSelect(i)}`. It returns `{select(i, scroll), factRows(f), explanation(parent, e), segments(parent, segs)}`. This is the code now inside `explainpage.js` from `make` to `select`, unchanged in behaviour.
  - **`explainpage.js`** keeps only the static page's wiring: it reads `#model`, calls `perfAgentPanel`, and handles clicks, keys, chips and copy.
  - **The static page's `<script>`** is `explainPanelScript + "\n" + explainPageScript`.
  - **CSS.** `explainpage.css` holds the template's first `<style>` body, with `/*LIGHT*/` and `/*DARK*/` where `{{.LightChrome}}` and `{{.DarkChrome}}` are now. Its declaration is `func explainedCSS() string`; it replaces the markers with `lightChrome`/`darkChrome` and is computed once. The template emits `<style>{{.CSS}}</style>` (a `template.CSS`), so the bytes are unchanged.
  - **Goldens.** `func stripInlineScript(page string) string` removes the content between the last `<script>` and `</script>`.

- [ ] **Step 1: Write the golden test against the current page, before any refactor**

```go
func TestExplainedPageGoldenOutsideItsScript(t *testing.T) // CPUBasic (claim b share 0.411, prose with {c1}, an unchecked "62%", a {n:} ref) and GPUBasic (conv explained, notes): stripInlineScript(page) == testdata/explainpage-{cpu,gpu}.golden.html; -update flag writes them
func TestExplainedPageScriptDefinesTheSharedPanel(t *testing.T) // the inline script contains "function perfAgentPanel(" exactly once and the static wiring calls it
func TestPanelScriptUsesTextContentOnly(t *testing.T)           // explainpanel.js: no innerHTML, outerHTML, insertAdjacentHTML, document.write, "eval(", setAttribute('style', cssText
```

Run `go test ./internal/flamegraph/ -run Golden -update` **once, on the unmodified code**, and commit nothing yet.

- [ ] **Step 2: Run the new tests to see the panel tests fail**

Run: `go test -race ./internal/flamegraph/ -run 'ExplainedPageGolden|SharedPanel|PanelScript'`
Expected: the golden test PASSes; the two panel tests FAIL.

- [ ] **Step 3: Split the script and the CSS as specified.** Move code without rewriting it; the M2 headless-Chrome geometry test is the behavioural check.

- [ ] **Step 4: Run every flamegraph test**

Run: `go test -race ./internal/flamegraph/`
Expected: PASS. The goldens are unchanged (including `RenderHTML`, `RenderFragment`, `WriteDiffPage` and the new explained ones), and `TestExplainedPageFramesAreNoWiderThanTheirShareInChrome` passes or skips.

- [ ] **Step 5: Commit**

```bash
git add internal/flamegraph
git commit -m "flamegraph: share the explained page's panel script and CSS, pinned by goldens outside the script"
```

### Task 4: Live models of an explained view (JSON)

**Files:**
- Create: `internal/flamegraph/livemodel.go`, `internal/flamegraph/livemodel_test.go`
- Modify: `internal/flamegraph/explainpage.go` (the builder gains a focus root and exposes its data; the static output is unchanged)

**Interfaces:**
- Consumes: Task 3. M2 `explainedBuilder`, `explainedPageData`, `exModel`, `exFrame`, `exRejected`, the layout byte budget and `rejected()`. `explain.Result` and `explain.RejectedAttempt`.
- Produces:
  - **Options.** `type LiveOptions struct { Title, ProfileName, Digest string; MaxNodes, MaxBytes int; Focus profilemodel.NodeID }`.
  - **`func ExplainedLiveModel(v *profilemodel.View, r explain.Result, opts LiveOptions) ([]byte, error)`.** Compact JSON: `{"kind":"explained","title","view","digest","profile_name","checked_pill","axis","total","total_title","notes":[…],"quality":[{"k","v","t","s","n"}],"legend":[{"key","label"}],"height","drawn":[{"i","left","width","top","domain","cls","star","tip","res"}],"narrow_note","limit_note","size_note","rows":[{"ref","node","name","share","cum"}],"focus","model":<exModel>}`.
    - Every string is the one the static template would print for the same field.
    - It refuses `!r.OK` and a nil view, like `WriteExplainedPage`.
    - With `MaxBytes > 0` it fits frames into the budget the same way the page does, leaving frames out and saying so in `size_note`. It errors only when the explained and referenced paths alone do not fit.
    - With `Focus` set, the layout starts at that node: it is drawn at depth 0 and full width, its descendants below it. `axis` ends `; widths relative to <name>, shares are of the view total`, and the focused node's ancestors stay in `model.frames` for the path panel.
    - An unknown `Focus` returns `profilemodel.ErrUnknownNode`.
  - **`func ExplainedLiveFrame(v *profilemodel.View, r explain.Result, id profilemodel.NodeID, digest string) ([]byte, error)`.** JSON `{"frame": <exFrame with facts, extra, explanation>, "path": [{"ref","node","name","share"}]}` for any node of the view, drawn or not. An unknown node returns `profilemodel.ErrUnknownNode`.
  - **`func ExplainedLiveHistory(pub, version string, rejected []explain.RejectedAttempt, dropped int, incomplete bool, maxBytes int) []byte`.** JSON `{"publication","version","rejected_pill","limit_pill","history_pill","rows":[{"attempt","evidence","kind","label","metric","node","agent","engine","code","reason"}]}`. `publication` and `version` let the page pair a history with its model (M2b-R1).
    - Rows and pills come from the same function the static page's rejected section uses.
    - Over `maxBytes`, the oldest rows are dropped and counted in the pill, as M2-R20(c) does on the page.

- [ ] **Step 1: Write the failing tests** (fixtures and helpers as in `explainpage_test.go`)

```go
func TestLiveModelMatchesTheStaticPage(t *testing.T)       // CPUBasic and GPUBasic: checked_pill, axis, total, notes, quality values, rows and every drawn frame's left/width/top/domain equal what the static page prints (parsed from its HTML)
func TestLiveModelRendersTheEngineValueNotTheAgents(t *testing.T) // claim 0.4114 → model contains "41.1%" and never "0.4114"
func TestLiveModelIsJSONEscaped(t *testing.T)              // frame "operator{}<script>", prose "</script><img src=x onerror=alert(1)> {c1}": bytes contain no "<" at all (json.Marshal escapes), the name is not tokenized
func TestLiveModelIsFlatOnDeepStacks(t *testing.T)         // Deep(2000): JSON nesting depth (counted with a json.Decoder token walk) ≤ 8; every drawn frame has a numeric top
func TestLiveModelHugeValues(t *testing.T)                 // HugeValue: "9007199254740993" appears as a JSON string, no bare number above 2^53
func TestLiveModelStaysUnderItsByteBudget(t *testing.T)    // many wide frames, MaxBytes 64 KiB → len ≤ 64 KiB, size_note non-empty, the explained path still drawn
func TestLiveModelRefusesAFailedCheck(t *testing.T)        // !r.OK and nil view → error
func TestLiveModelFocusLaysOutTheSubtree(t *testing.T)     // Focus = a (CPUBasic): drawn[0] is a at left 0 width 100, no frame outside a's subtree drawn, axis ends "shares are of the view total", model.frames still holds main for the path; unknown focus → ErrUnknownNode
func TestLiveFrameGivesFactsForAnUndrawnNode(t *testing.T) // a node under minDrawFraction: frame has facts and the path from root; unknown id → ErrUnknownNode
func TestLiveHistoryMatchesTheStaticRejectedSection(t *testing.T) // same RejectedAttempts as TestExplainedPageListsRejectedAttempts: rows and pill texts equal the static page's
func TestLiveHistoryIsBudgeted(t *testing.T)               // 40 rejected rows, maxBytes 2 KiB → len ≤ 2 KiB and the pill counts the rows left out
func TestLiveModelCapsLongNames(t *testing.T)              // a 64 KiB frame name in 30 rejected rows and on the explained path: displayed names ≤ 1 KiB + "…" (M2-R24), node IDs intact, peak TotalAlloc of the build < 8× MaxBytes
func TestStaticPageUnchanged(t *testing.T)                 // reruns the Task 3 goldens (guards the builder change)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/flamegraph/ -run 'Live|StaticPageUnchanged'`
Expected: FAIL (undefined functions).

- [ ] **Step 3: Implement `livemodel.go`.**
  - Give `explainedBuilder` a `root *profilemodel.Node`, nil meaning the view root, used by `layout`.
  - Build the JSON from the same `explainedPageData` the template reads, through one struct with JSON tags, so no displayed string is computed twice.
  - The fit loop is the page's loop, measuring `len(json)` instead of the rendered template.

- [ ] **Step 4: Run all flamegraph tests**

Run: `go test -race ./internal/flamegraph/`
Expected: PASS, goldens unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/flamegraph
git commit -m "flamegraph: JSON live models of an explained view, its frames and its rejected history, from the page's own builder"
```

### Task 5: The live shell, CSS and script

**Files:**
- Create: `internal/flamegraph/livepage.html`, `internal/flamegraph/live.css`, `internal/flamegraph/live.js`, `internal/flamegraph/liveassets.go`, `internal/flamegraph/liveassets_test.go`

**Interfaces:**
- Consumes: Task 3 `explainedCSS`, `explainpanel.js`; Task 4 model and history JSON shapes; Task 1 `livehttp.Guard`, `CSP` (tests only).
- Produces:
  - **`func LiveAsset(name string) (body []byte, contentType string, ok bool)`** serves these names:
    - `""` → `livepage.html`, `text/html; charset=utf-8`;
    - `"assets/live.css"` → `explainedCSS()` + palette CSS + live-only rules, `text/css; charset=utf-8`;
    - `"assets/panel.js"` → `explainpanel.js`, `text/javascript; charset=utf-8`;
    - `"assets/live.js"` → `live.js`, same type.

    Bodies are computed once and are byte-stable.
  - **`livepage.html`** is fixed markup with no template data. It holds:
    - the same region ids as the static page (`chart`, `sel-ref`, `sel-name`, `sel-id`, `facts`, `exp`, `noexp`, `path`, `chips`, `copybox`, `toast`);
    - live-only ids: `pubs` (the publication list), `banner`, `rejected`, `pills`, `notes`, `quality`, `rows`, `legend`, `axis`, `export`, `focus-btn` and `whole-btn`;
    - `<link rel="stylesheet" href="assets/live.css">`, `<script src="assets/panel.js" defer>` and `<script src="assets/live.js" defer>`.

    Its initial text says `Waiting for your agent to publish a page (page_explain or page_diff).`
  - **`live.js` behaviour:**
    - **Transport.** One object, `transport = {getJSON(path), subscribe(onState)}`, over `fetch(path, {cache: 'no-store'})` and `new EventSource('events')`. Every path is relative.
    - **On load:** `getJSON('state')`, render, then `subscribe`.
    - **Ordering (M2b-R1).**
      - `newer(a, b)` compares decimal strings by length, then lexically. `acceptedSeq` is the highest seq accepted so far. It is set synchronously, before the first `await` of that state's work. A state from SSE or from `GET state` that is not `newer` than `acceptedSeq` is ignored, so a slower, older fallback can never overtake a state still loading. `renderedSeq` is kept apart for display only.
      - A counter, `gen`, is incremented on every accepted agent state and on every local navigation (a frame click, focus, whole view, switching publication). Each async chain captures `gen` before its first `await` and commits to the DOM only if `gen` is unchanged.
      - On a new `current`, model and history are fetched together (`Promise.all`) and committed together.
      - A later history response is applied only if its `publication` is the one shown and its `version` is `newer` than the shown history's.
      - Frame and focus responses are applied only if their `node` equals the node requested.
    - **On a state:**
      - when `current` changes, fetch `pub/<id>/model` and `pub/<id>/history`, then render;
      - when only that publication's `history` version changes, fetch the history;
      - when `selected` or `focus` changes, select the node. If the node is not in `model.frames`, fetch `pub/<id>/frame/<node>`. If `focus` is set, fetch `pub/<id>/focus/<node>` and draw it. The page shows the toast `Your agent selected <ref> <name>`.
    - **Drawing.** Frames are drawn from `drawn` with `el.style.left/width/top` set from the model strings plus `%`/`px`.
    - **The user's own actions** stay local until the next state: clicking frames, the `Focus here` and `Whole view` buttons, and the publication list.
    - **Export.** `#export` is an `<a download>` whose `href` is `pub/<id>/export`, where `<id>` is the server's `p<n>`.
    - **Server gone.** When the `EventSource` errors and `getJSON('state')` fails, `#banner` shows `perf-agent-mcp is not running; this is the last state it sent. Ask your agent for a new page URL, or download a static copy while it runs.` and the content stays.

- [ ] **Step 1: Write the failing tests**

```go
func TestLiveShellHasNoInlineScriptOrStyle(t *testing.T) // shell: no "<script>" without src, no "<style", no " style=", no " on[a-z]+=" attribute, every src/href relative (no "//", no ":" before the first "/")
func TestLiveScriptRules(t *testing.T)                    // live.js + explainpanel.js: none of innerHTML, outerHTML, insertAdjacentHTML, document.write, "eval(", "setAttribute('style'", "cssText", "toFixed", "toPrecision", "toLocaleString", "Intl.", "parseFloat", "Math.round", "http:", "https:", "//" inside a string passed to fetch/EventSource
func TestLiveAssetsAreStable(t *testing.T)                // two LiveAsset calls per name byte-identical; content types as listed; unknown name → ok false
func TestLivePageInChrome(t *testing.T)                   // skip without Chrome or with -short; see below
```

`TestLivePageInChrome` builds an `httptest`-style server: `livehttp.Guard` over a mux serving `LiveAsset`, plus fixed `state`, `pub/p1/model` (CPUBasic explained), `pub/p1/history` and `pub/p1/frame/<id>` responses, and an `events` handler. That handler sends state seq 1 (selected = b), waits 300 ms, then sends seq 2 (selected = c). It also serves a modified shell carrying one extra `<script src="assets/probe.js" defer>` (allowed by `script-src 'self'`). The probe counts `securitypolicyviolation` events and, after 1.5 s, writes `#sel-name` text, the violation count and `#banner.hidden` into `<pre id="probe">` with `textContent`. Chrome runs `--headless=new --virtual-time-budget=3000 --dump-dom http://127.0.0.1:PORT/s/<tok>/`. Assert: sel-name is `c`, the violation count is `0`, and the banner is hidden. In a second run the server closes after seq 1; assert the banner text is present and sel-name is `b`.

`TestLivePageOrderingInChrome` (skipped like the other Chrome tests) uses the same server with per-path response delays and a probe that can click a frame. Every case asserts the final DOM.

| Case | Setup | Final DOM shows |
|---|---|---|
| a | state seq 1 → p1, whose model is delayed 600 ms and history 50 ms; then seq 2 → p2, served at once | p2's title, rows and history |
| b | history v1 delayed 500 ms, v2 at once | v2's rows |
| c | the probe clicks frame `a` (its `frame` fetch delayed 800 ms); at 200 ms state seq 3 selects `c` | `c` selected |
| d | state seq 4 focuses `a` (focus fetch delayed 800 ms); at 200 ms seq 5 clears focus | the whole view, not the focus |
| e | SSE delivers seq 7, then the `GET state` fallback answers seq 6 | seq 7's selection |
| g | DOM shows seq 5 (p1). SSE delivers seq 7 → p2, whose model is delayed 800 ms. At 200 ms the SSE stream drops, and the `GET state` fallback answers seq 6 → p3, served at once | p2 (seq 7). Seq 6 is refused because it is ≤ `acceptedSeq`, and never bumps `gen` |
| f | seq `"10"` after `"9"` | accepted (no lexical-only comparison) |

The JS-rules test also forbids `Number(` applied to `seq`; `seq` is only ever passed to `newer`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/flamegraph/ -run 'LiveShell|LiveScript|LiveAssets|LivePageInChrome|LivePageOrdering'`
Expected: FAIL.

- [ ] **Step 3: Implement the four files.** The live CSS adds only `#banner`, `#pubs` and the focus buttons. Everything else comes from `explainedCSS()` and the palette.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/flamegraph/`
Expected: PASS (the Chrome test passes, or skips with its reason).

- [ ] **Step 5: Commit**

```bash
git add internal/flamegraph
git commit -m "flamegraph: the live page shell, its stylesheet and a script that only reads relative URLs"
```

### Task 6: Publications and the live snapshot

**Files:**
- Create: `internal/perfmcp/live.go`, `internal/perfmcp/pins.go`, `internal/perfmcp/live_test.go`, `internal/perfmcp/pins_test.go`, `internal/profilemodel/retained.go`, `internal/profilemodel/retained_test.go`, `internal/profilemodel/concurrent_test.go`
- Modify: `internal/perfmcp/limits.go` (adds `MaxPublications` 8, `MaxRetainedBytes` 2 GiB and `MaxPageClients` 8, filled when zero; validate refuses `MaxRetainedBytes < MaxPageBytes` with `MaxRetainedBytes must be at least MaxPageBytes`), `internal/perfmcp/store.go` (an entry registers its Profile and views with the ledger and releases them on eviction), `internal/perfmcp/config.go` (`Live bool; LiveAddr string`, where `""` means `"127.0.0.1:0"`), `internal/perfmcp/server.go` (the `Server` gains `live *live`, nil unless `Config.Live`; `New` validates `LiveAddr` with `livehttp.CheckAddr`; the comment on ownership is updated to the Global Constraints rule)

**Interfaces:**
- Consumes: Task 4 `ExplainedLiveModel`, `ExplainedLiveHistory`, `LiveOptions`; Task 2 `Hub` (`Publish`); M2 `attempts.rejected(p, view)`, `entry`.
- Produces:
  - **Size estimates** (profilemodel, `retained.go`):
    - `func (p *Profile) RetainedBytes() int64` and `func (v *View) RetainedBytes() int64`. Both are computed once (at load, and at view build) from counts: samples, locations, lines, labels, execution ordinals, nodes, name bytes and sample-index slices.
    - The per-item weights are named constants, calibrated so the estimate is within [0.5×, 2×] of the measured heap.
    - Task 9 adds `(*Diff).RetainedBytes` and `(*DiffIndex).RetainedBytes` the same way.
  - **Pin ledger** (`pins.go`): `type pins struct`, keyed by pointer (`*Profile`, `*View`, `*Diff`, `*DiffIndex`), with a holder count and the bytes of each pin.
    - `func (l *pins) hold(holder any, objs ...retained)` and `func (l *pins) release(holder any)` maintain it; `type retained interface{ RetainedBytes() int64 }`.
    - `func (l *pins) heldBy(holders ...any) int64` is the sum over the distinct objects the given holders hold.
    - The store and the publications are holders. A profile held by both, or by two publications, counts once.
  - `type pubKind string` with `pubExplained = "explained"` and `pubDiff = "diff"`.
  - **The publication.** `type publication struct { id string; kind pubKind; title, view string; digest profilemodel.Digest; prof *profilemodel.Profile; v *profilemodel.View; result explain.Result; d *profilemodel.Diff; baseName, newName string; opts flamegraph.LiveOptions; model []byte }`. It is immutable after `add`.
  - **Structured history (M2b-R5).** `type liveHistory struct { version int64; rejected []explain.RejectedAttempt; dropped int; incomplete bool; json []byte }`. `rejected` is deep-copied from `attempts.rejected` (new `Failures` slices, new `EngineValue` values), so later changes to the record never reach it.
  - **The snapshot.** `type liveSnapshot struct { seq int64; current, selected, focus string; pubs map[string]*publication; order []string; history map[string]*liveHistory; state []byte }`. `state` is the compact state JSON from the settled-calls table, with `seq` and `history` as decimal strings.
  - **The live state.** `type live struct` holds the `atomic.Pointer[liveSnapshot]`, and fields only the request goroutine touches: `pubs []*publication` in LRU order, `next int`, `seq int64`, `selected`, `focus`, `hub *livehttp.Hub`, `lim Limits`, and the listener fields Task 7 adds.
  - **Publishing an explained view.** `func (s *Server) publishExplained(e *entry, v *profilemodel.View, view string, r explain.Result, title string) (*publication, error)`:
    - builds the model with `MaxNodes: MaxPageNodes, MaxBytes: MaxPageBytes`;
    - on error, publishes nothing;
    - on success passes it to `admit` (whose refusal it returns unchanged), makes it current, selects the first explanation's node (else `""`), clears focus and commits.
  - **Admission (preflighted).** `func (s *Server) admit(p *publication) error`.
    1. **Simulate.** On a copy of the LRU order, drop the least recently shown other publications while there are more than `MaxPublications`, or while the estimate over the remaining set plus `p` exceeds `MaxRetainedBytes`. The estimate is `pins.estimate(objects…)`: the distinct-object sum, computed without holding anything.
    2. **If it fits,** commit: release the dropped publications' pins, hold `p`'s, assign `p<n>` and make `p` current.
    3. **If `p` does not fit even alone,** touch nothing and return `the page would keep %s in memory (profile, view and page model), over MaxRetainedBytes=%d; publish a smaller view or restart perf-agent-mcp with a larger --limit MaxRetainedBytes=N`. The order, the current publication, every kept publication and the ledger stay exactly as they were.
  - **Documentation.** `Limits.MaxRetainedBytes`'s comment calls it an estimated budget: real heap can reach about twice it.
  - `func (l *live) show(id, node string, focus bool) error`: selection rules are checked by Task 8; this only sets the fields and touches the LRU.
  - `func (s *Server) refreshHistory()` reads `s.attempts.rejected(p.prof, p.view)` for every kept explained publication. It builds a new `liveHistory` (version +1, JSON from `ExplainedLiveHistory(p.id, version, …, MaxPageBytes)`) only when the structured history differs from the kept one.
  - `func (s *Server) commit()` builds a new snapshot (copying maps), sets `seq++`, stores it and calls `hub.Publish(state)`. It is the only writer of the pointer.
  - `func (l *live) snapshot() *liveSnapshot`. The zero state, before any publication, is `{"seq":"0","current":"","selected":"","focus":"","publications":[]}`.

- [ ] **Step 1: Write the failing tests**

```go
// profilemodel/concurrent_test.go
func TestViewReadsAreSafeConcurrently(t *testing.T) // GPUBasic+execlog view: 16 goroutines × 200 iterations of Facts, CoreFacts, NodeQuality, Quality, Notes, KernelsBelow, Path, Walk on every node; -race clean; results equal a sequential run
// perfmcp/live_test.go (package perfmcp; servers built with New, explanations checked with explain.Check)
func TestPublishMakesTheNewPublicationCurrent(t *testing.T) // p1 then p2: state current p2, selected = first explained node of p2; seq strictly increases; state JSON seq is a string
func TestPublicationsAreImmutable(t *testing.T)            // p1.model bytes equal before and after publishing p2..p5 and after refreshHistory
func TestPublicationsAreBoundedByCount(t *testing.T)      // MaxPublications 2: p1,p2,p3 → p1 evicted, its pins released
func TestRetainedEstimatesTrackTheHeap(t *testing.T)       // profilemodel: a synthetic 100k-sample profile, its view, Deep(5000): RetainedBytes within [0.5×, 2×] of the HeapAlloc delta (runtime.GC around a held reference)
func TestALargeProfileWithATinyModelIsChargedForItsEngine(t *testing.T) // that profile, facts-only publication (model < 64 KiB): MaxRetainedBytes below p.RetainedBytes() → publish refused naming MaxRetainedBytes, state unchanged; above it → admitted, heldBy ≥ p.RetainedBytes()
func TestPinsAreSharedWithTheStore(t *testing.T)           // two publications of one handle while it is open → its Profile counted once in heldBy and in the ledger total
func TestStoreEvictionWhilePublishedKeepsOnePin(t *testing.T) // MaxOpenHandles 1: publish A, open B (store releases A) → A's Profile still held once (by p1), p1 serves its view; evict p1 → A's pins gone from the ledger
func TestRetainedBudgetEvictsOlderPublications(t *testing.T) // budget fits two of three equal publications → the least recently shown one is evicted; the current one never
func TestARefusedAdmissionChangesNothing(t *testing.T)     // p1 current, p0 also kept; p2 over budget even alone → error; order, current p1, both publications, ledger total and every pin count identical to before (deep compare)
func TestSnapshotHistoryIsACopy(t *testing.T)              // take a snapshot's liveHistory, add and evict attempts → the kept rejected slices and strings are unchanged
func TestHistoryVersionMovesOnlyWhenTheRecordChanges(t *testing.T) // refreshHistory twice with no new attempt → same version; add a rejection for p1's digest+view → version +1; a rejection on another view → p1 unchanged
func TestNoLiveWithoutTheFlag(t *testing.T)                // Config.Live false → s.live nil; New with LiveAddr "0.0.0.0:0" and Live true → error naming the address
func TestLimitsValidateTheRetainedBudget(t *testing.T)     // MaxRetainedBytes < MaxPageBytes → New error naming both
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/profilemodel/ ./internal/perfmcp/ -run 'Concurrently|Retained|Publish|Publications|Pins|StoreEviction|SnapshotHistory|HistoryVersion|NoLive|RefusedAdmission'`
Expected: FAIL. `TestViewReadsAreSafeConcurrently` may already pass; that is its job. A failure means the View must be made safe before going on.

- [ ] **Step 3: Implement `live.go` and the limit and config changes.** `commit` is called only from the request goroutine. Document that on `live`.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/profilemodel/ ./internal/perfmcp/`
Expected: PASS, with the M2 tests unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp internal/profilemodel/concurrent_test.go
git commit -m "perfmcp: immutable live publications behind an atomic snapshot, bounded by count and bytes"
```

### Task 7: The listener lifecycle and the page's routes

**Files:**
- Create: `internal/perfmcp/liveroutes.go`, `internal/perfmcp/liveroutes_test.go`
- Modify: `internal/perfmcp/live.go` (listener fields), `internal/perfmcp/server.go` (`Close` shuts the live server down), `internal/perfmcp/harness_test.go` (`(*testClient).httpGet(url string, hdr map[string]string) (*http.Response, []byte)` and `liveURL(t, srv) string` for in-package tests)

**Interfaces:**
- Consumes: Task 1 `Listen`, `NewToken`, `Guard`, `NewServer`, `ExportCSP`; Task 2 `Hub`; Task 4 `ExplainedLiveFrame`, `ExplainedLiveModel` (focus); Task 5 `LiveAsset`; Task 6 snapshot and publications; M2 `flamegraph.WriteExplainedPage`, `WriteDiffPage`.
- Produces:
  - **Starting the listener.** `func (s *Server) ensureLive() (url string, err error)`. Request goroutine only, idempotent. It runs `NewToken`, then `Listen(LiveAddr)`, then `NewServer(Guard(tok, s.liveMux()))`, then `go srv.Serve(ln)`, then `commit()`. It logs `live page listening` with `addr` (never the token) and returns `http://<ln.Addr()>/s/<tok>/`, with IPv6 bracketed.
  - **Shutdown.** `Close` calls `hub.Close()`, then `srv.Shutdown` with a 2 s context, then `srv.Close()`.
  - **Routes.** `func (s *Server) liveMux() http.Handler` is a `http.ServeMux` with these `GET` patterns:
    - `/{$}` → `LiveAsset("")`;
    - `/assets/{name}` → `LiveAsset("assets/"+name)`;
    - `/state` → `snap.state`;
    - `/events` → hub;
    - `/pub/{id}/model` → `p.model`;
    - `/pub/{id}/history` → `snap.history[id].json` (`{"publication","version":"0","rows":[]}` for a diff);
    - `/pub/{id}/frame/{node}` → `ExplainedLiveFrame(p.v, p.result, node, digest)`;
    - `/pub/{id}/focus/{node}` → `ExplainedLiveModel(p.v, p.result, p.opts with Focus: node)`;
    - `/pub/{id}/export` → `WriteExplainedPage` from the snapshot's `liveHistory` (`Rejected`, `DroppedRejected`, `HistoryIncomplete`; `MaxBytes: MaxPageBytes`), rendered into a buffer first (M2b-R2, R5). A render error answers `422` `text/plain` with the reason and no partial body. Success answers `200` with `Content-Length`, `Content-Disposition: attachment; filename="<id>.html"` and the CSP replaced by `ExportCSP`. Diff exports are added in Task 10.

    An unknown publication or node gets `404` with the text `no such publication or node; the page will refresh`. A path value over 64 bytes gets `404` without lookup. JSON responses are `application/json; charset=utf-8`. Every handler starts with `snap := s.live.snapshot()` and touches nothing else on `s`.

- [ ] **Step 1: Write the failing tests** (in-package: start with `ensureLive`, publish with Task 6's `publishExplained`, request with the real client against the loopback URL)

```go
func TestTheLiveURLIsLoopbackWithAToken(t *testing.T)      // ^http://127\.0\.0\.1:\d+/s/[A-Za-z0-9_-]{43}/$; ensureLive twice → same URL; the server log contains the addr and not the token
func TestEveryRouteIsGuarded(t *testing.T)                 // for each route: without the token 404; Host evil.example 403; Sec-Fetch-Site cross-site 403; POST 405; every response has the CSP (export: ExportCSP) and no Access-Control-*
func TestStateAndModelServeThePublication(t *testing.T)    // publish p1 → GET state current p1; GET pub/p1/model == p1.model bytes; GET pub/p9/model → 404
func TestFrameAndFocusAreComputedByTheEngine(t *testing.T) // GET pub/p1/frame/<undrawn node> has its facts; focus/<a> has drawn[0] == a; unknown node → 404
func TestExportIsTheStaticPageAsADownload(t *testing.T)    // GET pub/p1/export: Content-Disposition attachment, CSP sandbox, body starts "<!doctype html>", contains the checked figure and the current rejected rows
func TestExportReadsOnlyTheSnapshot(t *testing.T)         // -race: 4 goroutines export p1 in a loop while the test records 50 rejections for p1's key, evicts attempt views (MaxAttemptViews 2, other views), and evicts publications; every export is a complete 200 or a 422, never partial; each 200 lists the rows of some snapshot's history
func TestNoGETChangesServerState(t *testing.T)             // record attempts.next, len(store.entries), snapshot seq; GET every route 20 times (incl. events briefly) → all unchanged
func TestASecondTabAndAReconnectSeeTheCurrentState(t *testing.T) // two SSE clients get the current state first; after a publish both converge; a reconnect after disconnect gets the latest
func TestAStuckTabNeverDelaysATool(t *testing.T)           // one SSE client that never reads; 200 publishExplained calls finish within the time 200 calls take with no client + 1s
func TestAnotherServersTokenIs404(t *testing.T)            // two servers: server B with server A's token path → 404
func TestCloseStopsTheListener(t *testing.T)               // Close → a new connection to the port is refused; open SSE streams end
func TestLivePageRacesNothing(t *testing.T)                // -race: 8 goroutines GET model/frame/focus/history/state in a loop while the test publishes 50 times, records 20 rejections (attempts.add, refreshHistory, commit, as the request goroutine would) and opens 10 profiles
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'LiveURL|Guarded|StateAndModel|FrameAndFocus|Export|NoGET|SecondTab|StuckTab|AnotherServers|CloseStops|RacesNothing'`
Expected: FAIL.

- [ ] **Step 3: Implement `liveroutes.go`** and the lifecycle as specified.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: serve the live page read-only from the snapshot, behind the loopback guard"
```

### Task 8: `page_open`, `page_explain` and `page_show`

**Files:**
- Create: `internal/perfmcp/tool_page.go`, `internal/perfmcp/page_test.go`
- Modify: `internal/perfmcp/tool_render.go` (extract the shared check; `render_flamegraph`'s results unchanged), `internal/perfmcp/attempts.go` (`outcomePublished = "published"`, in the "others" bound), `internal/perfmcp/server.go` (register the tools after `render_flamegraph`)

**Interfaces:**
- Consumes: Task 6 `publishExplained`, `show`, `refreshHistory`, `commit`; Task 7 `ensureLive`; M2 `explain.Decode`, `explain.Check`, `attempts`, `store.view`.
- Produces:
  - **The shared check.** `type explainCall struct { Handle profilemodel.Handle; View, Attempt, Title string; Explanations, Rounding, Tolerance json.RawMessage }`; `func (s *Server) checkCall(tool string, c explainCall) (checked, error)` with `type checked struct { e *entry; v *profilemodel.View; r explain.Result; notices []explain.Notice; rejection *mcpstdio.Result }`. It moves from `handleRender` the handle and view lookup, the attempt lookup, the notices, `Decode` (an invalid shape is recorded as `invalid`) and `Check`. On failure it records `rejected` and returns `rejection`: the exact `renderRejected` result (`isError`) that `render_flamegraph` returns today. `handleRender` calls it after its `out` check. M2's render tests are the guard.
  - **History refresh.** After every `attempts.add` or `setOutcome` (in both tools), when `s.live` is started, call `s.refreshHistory()` and then `s.commit()` if a version moved.
  - **Disabled tools.** Without `Config.Live`, every `page_*` description starts `DISABLED: start perf-agent-mcp with --live to enable the live page.` and every call is `isError` with that sentence.
  - **`page_open`** (annotations: not read-only, idempotent, closed world). Arguments `{}`. Result `{url, current, publications: [{id, kind, title, view}], viewers}`; `viewers` is `hub.Clients()` as a string. The description tells the agent to give the URL to the user.
  - **`page_explain`** (not read-only, not idempotent). Arguments `{handle, view, explanations, attempt?, title?, rounding?, tolerance?}`, with the same schema text as `render_flamegraph` minus `out`. The flow is `checkCall`, then `attempts.add(outcomePublished)`, then `publishExplained`. If that fails: `setOutcome(failed)` and `isError` with the M2 page-size advice and "nothing was published". Result `{attempt, status: "published", publication, url, figures_checked, unchecked_numbers, notices, viewers}`. `explanations: []` publishes a facts-only page.
  - **`page_show`** (not read-only, idempotent). Arguments `{publication?, node?, focus?: bool}`, at least one of them (else `isError` `pass publication, node or both`). The rules:
    - an unknown publication gives `publication %q is not kept; the kept ones are: …; call page_explain or page_diff to publish again`;
    - `node` must exist in the publication's view (a union ID for a diff), else `node %q is not in view %s of publication %s; find node IDs with top or path` (names bounded with `echo`);
    - `focus` without `node` focuses the selected node;
    - result `{publication, selected, focus, url, viewers}`.

- [ ] **Step 1: Write the failing tests** (stdio harness from M2; the test is the browser through `httpGet` on the URL from `page_open`)

```go
func TestPageToolsAreDisabledWithoutLive(t *testing.T)    // tools/list descriptions start "DISABLED"; each call isError mentioning --live; nothing listens (s.live nil)
func TestPageOpenGivesTheURLAndStartsOnce(t *testing.T)   // two page_open → same url; GET url → 200 text/html; viewers "0"
func TestPageExplainPublishesCheckedClaims(t *testing.T)  // CPUBasic b share 0.411 → status published, publication p1; GET state current p1, selected b's id; GET pub/p1/model contains "41.1%"
func TestPageExplainRejectsExactlyLikeRender(t *testing.T) // value 0.62 via page_explain and via render_flamegraph: same JSON except attempt IDs; isError; state current unchanged; no publication
func TestPageExplainSharesTheAttemptRecord(t *testing.T)  // render_flamegraph a1 rejected; page_explain attempt "a1" → published a2; GET history shows a1's row; attempt from another view → isError
func TestRenderIsUnchangedByTheRefactor(t *testing.T)     // every M2 render_test passes (run as part of the package) and a written page's bytes equal a page written before this task for the same inputs (golden in testdata/render-cpu.golden.html, generated at the start of this task)
func TestLiveHistoryFollowsNewRejections(t *testing.T)    // publish p1 (GPUBasic, no execlog); open GPUBasic with execlog (another handle, same digest); render_flamegraph rejection there → an SSE client gets a state with p1's history version +1; GET history lists the row labelled with the other handle
func TestAFactsOnlyPageStillShowsRejections(t *testing.T) // rejection, then page_explain explanations [] → GET history pill "1 claims rejected at render"
func TestPageShowSelectsAndFocuses(t *testing.T)          // page_show {node: <undrawn narrow node>} → state selected it; {focus: true} → state focus = selected; {publication: "p1"} after p2 → current p1
func TestPageShowRefusesWhatIsNotThere(t *testing.T)      // {} → isError; unknown publication lists kept IDs; node of another view → isError naming top or path; a 4 KiB node string → error text bounded
func TestViewersCountsOpenStreams(t *testing.T)           // two SSE GETs open → page_show result viewers "2"
```

- [ ] **Step 2: Run them to verify they fail**

Before the refactor, generate `testdata/render-cpu.golden.html` from the current `render_flamegraph` once.
Run: `go test -race ./internal/perfmcp/ -run 'Page|RenderIsUnchanged|LiveHistory|FactsOnly|Viewers'`
Expected: FAIL (tools missing). The render golden passes.

- [ ] **Step 3: Implement `checkCall`, the history refresh and the three tools.**

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS, including every M2 render test.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: page_open, page_explain and page_show drive the live page with render_flamegraph's check"
```

### Task 9: A flat live model of a diff

**Files:**
- Create: `internal/flamegraph/livediff.go`, `internal/flamegraph/livediff_test.go`
- Create also: `internal/profilemodel/diffindex.go`, `internal/profilemodel/diffindex_test.go`, `internal/flamegraph/diffbound.go`, `internal/flamegraph/diffbound_test.go`
- Modify: `internal/profilemodel/retained.go` (`(*Diff).RetainedBytes`), `internal/flamegraph/diffpage.go` (extract the per-node colour, marker and tooltip computation into `func diffAppearance(n *profilemodel.DiffNode, scale float64, unit string) (hue, tint, classes, marker, tooltip string)`; `WriteDiffPage` bytes unchanged), `internal/flamegraph/live.js` (draws `kind: "diff"`), `internal/flamegraph/liveassets_test.go` (the Chrome test gains a diff publication)

**Interfaces:**
- Consumes: M1 `profilemodel.Diff`, `DiffNode`, `Flatten`, `UnionID`; the `diffBuilder` helpers; Task 5 `live.js`.
- Produces:
  - **The union index (M2b-R3).** `type DiffIndex struct`; `func NewDiffIndex(d *Diff) *DiffIndex` builds an iterative pre-order walk over the whole union. Methods: `(*DiffIndex).Node(id UnionID) (*DiffNode, bool)`, `(*DiffIndex).Path(id UnionID) []*DiffNode` (root first) and `(*DiffIndex).RetainedBytes() int64`. It is immutable after construction.
  - **`func DiffLiveFrame(d *profilemodel.Diff, ix *profilemodel.DiffIndex, id profilemodel.UnionID) ([]byte, error)`.** JSON `{"node","frame":{…the model's frame fields…},"path":[{"id","ref","name","delta_share"}]}` for any union node, drawn or not. An unknown ID returns `profilemodel.ErrUnknownNode`.
  - **Bounded export (M2b-R2).** `func WriteDiffPageBounded(w io.Writer, d *profilemodel.Diff, opts DiffPageOptions, maxBytes, maxNodes int) error`, with `var ErrPageTooLarge = errors.New("flamegraph: page over its budget")`.
    1. `diffPageBound(d)` walks iteratively, applying `diffBuilder.view`'s omission rule. It sums an upper bound of each drawn node's markup (escaped name, tooltip bound, fixed overhead), the rows table (at most `maxTableRows`), the warnings and the fixed chrome, and counts drawn nodes. It allocates nothing proportional to the output.
    2. Over `maxBytes` or `maxNodes`, it returns `ErrPageTooLarge` wrapped with both figures, having built nothing.
    3. Otherwise it renders `WriteDiffPage` into a `bytes.Buffer` grown to the bound, re-checks the length against `maxBytes`, and only then writes to `w`.
  - **The diff model also states whether export is available:** `"export":{"ok":bool,"reason":"…"}` from `diffPageBound`. The page disables the button with the reason.
  - **`func DiffLiveModel(d *profilemodel.Diff, opts DiffLiveOptions) ([]byte, error)`**, with `type DiffLiveOptions struct { Title, BaseName, NewName string; MaxNodes, MaxBytes int; Focus profilemodel.UnionID }`. Compact JSON: `{"kind":"diff","title","view","base_name","new_name","base_total","new_total","warnings","base_notes","new_notes","legend","axis","height","drawn":[{"i","left","width","top","hue","tint","cls","marker","tip"}],"narrow_note","size_note","frames":[{"id","ref","name","parent","presence","interpretation","why","base_cum","new_cum","delta_abs","delta_share","delta_per_unit"}]}`.
    - Every value is a display string computed in Go (`signed`, `withHuman`, `Fact.Canonical` as a percent, `unavailable: <reason>`), with exact digits in titles.
    - Widths are `Combined` over the root's `Combined`, as on `WriteDiffPage`. The layout is flat, positioned in pre-order. The byte budget and `Focus` work as in Task 4.
  - **`live.js`:**
    - **Drawing.** For `kind: "diff"` it draws frames with `--h`/`--t` through `el.style.setProperty`.
    - **Panel.** Name, presence, interpretation and `why`, base, new, delta and delta share, and the path.
    - **No explanation sections.** Diff metrics are not claimable.
    - **Export** links to `pub/<id>/export` as for explained pages.

- [ ] **Step 1: Write the failing tests**

```go
func TestDiffLiveModelMatchesTheDiffPage(t *testing.T)   // CPUBasic vs CPUBasicNew: every node's width, hue/tint, marker and tooltip equal what WriteDiffPage prints for it
func TestDiffLiveModelIsFlatOnDeepStacks(t *testing.T)   // Deep(2000) vs DeepNew(2000): JSON depth ≤ 8
func TestDiffLiveModelNeverShowsUnavailableAsZero(t *testing.T) // a node with an unavailable delta_share → "unavailable: <reason>", never "0"
func TestDiffLiveModelHugeValues(t *testing.T)           // HugeValue vs itself: "9007199254740993" as a string
func TestDiffLiveModelStaysUnderItsByteBudget(t *testing.T)
func TestWriteDiffPageUnchanged(t *testing.T)            // the M1 diff golden still matches
func TestDiffIndexCoversTheWholeUnion(t *testing.T)      // profilemodel: Deep(2000) vs DeepNew(2000) and CPUBasic vs CPUBasicNew: index size == len(Flatten().Nodes), Path(id) equals the Flatten parent chain for every node
func TestDiffLiveFrameFindsANodeTheModelLeftOut(t *testing.T) // a wide diff with MaxBytes so small the model omits node X: X not in model frames; DiffLiveFrame(X) gives its facts and its root path
func TestBoundedDiffExportMatchesWhenItFits(t *testing.T) // CPUBasic vs CPUBasicNew, Deep(600): WriteDiffPageBounded bytes == WriteDiffPage bytes; the bound ≥ the real length on every fixture
func TestBoundedDiffExportRefusesBeforeBuilding(t *testing.T) // 1 KiB names × 3000 wide nodes × depth 400: DiffLiveModel at MaxBytes 4 MiB succeeds (frames left out), WriteDiffPageBounded at 4 MiB → ErrPageTooLarge, w received 0 bytes, TotalAlloc delta of the refusal < 1 MiB; the model's export.ok is false with the reason
```

The Chrome test gains a run: state current = a diff publication; assert the drawn frame count is > 0, the panel shows a delta string from the model, and there are 0 CSP violations.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/profilemodel/ ./internal/flamegraph/ -run 'DiffIndex|DiffLive|BoundedDiffExport|WriteDiffPageUnchanged|LivePageInChrome'`
Expected: FAIL.

- [ ] **Step 3: Implement `diffindex.go`, `livediff.go`, `diffbound.go`, the extraction and the script branch.** For a diff, `live.js` fetches `pub/<id>/frame/<node>` for any node not in the model, as for explained pages.

- [ ] **Step 4: Run all flamegraph tests**

Run: `go test -race ./internal/flamegraph/`
Expected: PASS, goldens unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/flamegraph
git commit -m "flamegraph: a flat live model of a diff, drawn by the live page"
```

### Task 10: `page_diff`

**Files:**
- Create: `internal/perfmcp/tool_page_diff.go`, `internal/perfmcp/page_diff_test.go`
- Modify: `internal/perfmcp/tool_diff.go` (extract `func (s *Server) compare(a diffArgs) (*profilemodel.Diff, baseEntry, newEntry *entry, err error)` from `handleDiff`; the diff tool's results are unchanged), `internal/perfmcp/live.go` (`func (s *Server) publishDiff(d *profilemodel.Diff, baseName, newName, title string) (*publication, error)`, which builds `NewDiffIndex` once and pins the Diff, the index and both Profiles through `admit`), `internal/perfmcp/liveroutes.go` (for diff publications, focus through `DiffLiveModel` with `Focus`, frame through `DiffLiveFrame` over the pinned index, export through `WriteDiffPageBounded(MaxPageBytes, MaxPageNodes)` with `ErrPageTooLarge` → `422` before any body)

**Interfaces:**
- Consumes: Task 9 `DiffLiveModel`; Tasks 6–8; M2 diff argument validation and comparability.
- Produces:
  - **The tool.** `page_diff` (not read-only, not idempotent). Arguments `{base_handle, base_view, new_handle, new_view, work_base?, work_new?, title?, node?}`. Result `{publication, url, base_total, new_total, quality_warnings, viewers}`.
    - The diff tool's own errors (incompatible views, unknown handles) come back unchanged, with the same prescriptive text.
    - `node` must be a union ID of the diff, checked with the publication's `DiffIndex` (never the model's frames), else `isError` naming `diff` as the way to find IDs. `page_show` checks diff nodes the same way.
    - It records **no** attempt: a diff carries no claims.
  - **Diff publications** pin the `*profilemodel.Diff`, its `DiffIndex`, both Profiles and both names. Their history route answers `{"publication","version":"0","rows":[]}`, and their export is `WriteDiffPageBounded`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPageDiffPublishesTheComparison(t *testing.T)    // CPUBasic vs CPUBasicNew → publication p1 kind diff; GET model kind "diff"; base_total/new_total equal the diff tool's
func TestPageDiffRefusesWhatDiffRefuses(t *testing.T)    // incompatible views → isError with diff's text; nothing published
func TestPageDiffSelectsAUnionNode(t *testing.T)         // node = a union ID from the diff tool → state selected it; a plain node ID → isError naming diff
func TestPageDiffExportIsTheM1DiffPage(t *testing.T)     // GET pub/p1/export == WriteDiffPage bytes for the same diff
func TestPageDiffExportTooLargeIs422(t *testing.T)       // the Task 9 long-name deep-wide fixture as two profiles: page_diff succeeds, GET export → 422 text/plain naming MaxPageBytes, no Content-Disposition, body is only the reason
func TestPageShowFindsADiffNodeTheModelLeftOut(t *testing.T) // MaxPageBytes small enough that the model omits X: page_show {node: X} → selected X; GET pub/p1/frame/X has its facts and root path
func TestDiffToolUnchangedByTheRefactor(t *testing.T)    // M2 diff tests pass; a recorded diff response for CPUBasic vs CPUBasicNew is byte-identical (golden generated before the refactor)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'PageDiff|PageShowFindsADiffNode|DiffToolUnchanged'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: page_diff shows a comparison on the live page"
```

### Task 11: Flags, network tests, guide and docs

**Files:**
- Modify: `cmd/perf-agent-mcp/main.go`, `cmd/perf-agent-mcp/main_test.go`, `internal/perfmcp/guide.md`, `internal/perfmcp/guide_test.go`, `docs/mcp.md`, `CHANGELOG.md`
- Create: `cmd/perf-agent-mcp/netscan_test.go`

**Interfaces:**
- Consumes: every task above; M2 Task 14's `run`, the flag set and `--limit` reflection (the new `Limits` fields map automatically).
- Produces:
  - **Flags.**
    - `--live` (bool, default false) sets `Config.Live`.
    - The listener always binds `127.0.0.1:0`; there is no address flag (M2b-R7). `livehttp.CheckAddr` still guards the bind address internally.
  - **The binary's dependency test** is renamed `TestTheServerLinksNoProfiler`. It keeps the forbidden packages `perfagent`, `cilium/ebpf` and blazesym and the non-std module set; `net`, `net/http` and `crypto/tls` are now allowed.
  - **`netscan_test.go`, `TestNoClientNetworkingInTheServer`:**
    - takes every non-std package from `go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/perf-agent-mcp` and parses its non-test `.go` files with `go/parser`;
    - fails on any selector `net.Dial`, `net.DialTCP`, `net.DialUDP`, `net.DialIP`, `net.DialUnix`, `net.DialTimeout`, `net.Dialer`, `net.Resolver`, `net.LookupHost`, `net.LookupIP`, `net.LookupAddr`, `net.LookupCNAME`, `net.LookupTXT`, `net.LookupMX`, `net.LookupSRV`, `net.LookupNS`, `net.LookupPort`, `http.Get`, `http.Post`, `http.PostForm`, `http.Head`, `http.Client`, `http.DefaultClient`, `http.DefaultTransport`, `http.Transport`, `http.NewRequest`, `http.NewRequestWithContext`, `tls.Dial`, `tls.DialWithDialer`, `net.ListenPacket` or `net.ListenUDP`;
    - fails on any `net.Listen`/`net.ListenTCP`/`http.ListenAndServe*` outside `internal/livehttp/listen.go`.

    Import aliases are resolved per file.
  - **`TestEverySocketIsLoopback`** (Linux only, skipped elsewhere):
    1. build the binary into `t.TempDir()` and start it with `--profiles` (fixtures), `--output` and `--live`;
    2. over its stdin and stdout, run `initialize`, `open`, `page_open` and `page_explain`;
    3. the test, as the browser, opens an SSE stream and GETs the model;
    4. read `/proc/<pid>/fd` socket inodes and match them in `/proc/<pid>/net/{tcp,tcp6,udp,udp6}`.

    Assert that every socket of the process has a loopback local address, that each is LISTEN or has a loopback remote address, and that no UDP socket exists. Then close stdin and assert the process exits 0 and the port refuses connections.
  - **Guide.** `guide.md` gains a "Live page" section (still ≤ 12 KiB in all). It covers:
    - starting with `page_open` and giving the user the URL;
    - `page_explain` runs the same check as `render_flamegraph` and publishes instead of writing;
    - `page_show` to point the user at a node or focus a subtree;
    - `page_diff`;
    - "the page shows every rejection the server recorded for the view";
    - `render_flamegraph` when a file is wanted.
  - **`docs/mcp.md` "Live page"** covers:
    - `--live`;
    - what is served and to whom: loopback, token, read-only;
    - local use only: the page is for a browser on the same machine (remote use is not supported);
    - the four tools;
    - download a static copy;
    - what the page never does: send anything, run tools, or work offline;
    - the new limits, with `MaxRetainedBytes` described as an estimated retained-engine budget (estimates within [0.5×, 2×] of heap), not a physical memory cap.
  - **CHANGELOG.** Unreleased/Added gets one entry for the live page.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunHasNoLiveAddressFlag(t *testing.T)     // --live-addr anything → 2 (unknown flag); with --live the listener's address is 127.0.0.1
func TestRunServesTheLivePage(t *testing.T)               // pipes: initialize, open CPUBasic, page_open, page_explain; GET url → 200; EOF → run returns 0 and the port refuses connections
func TestStdoutCarriesOnlyJSONRPC(t *testing.T)           // M2's test, the session extended with page_open, page_explain, page_show and a browser GET; still only JSON-RPC lines; stderr has the listen addr and not the token
func TestTheServerLinksNoProfiler(t *testing.T)           // renamed and amended as above
func TestNoClientNetworkingInTheServer(t *testing.T)
func TestEverySocketIsLoopback(t *testing.T)
func TestGuideIsShortAndStatesTheRules(t *testing.T)      // M2's limit (≤ 12288) plus "page_open", "give the user the URL", "same check as render_flamegraph", "every rejection the server recorded"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp/ ./internal/perfmcp/ -run 'LiveAddr|ServesTheLivePage|Stdout|LinksNoProfiler|NoClientNetworking|EverySocket|Guide'`
Expected: FAIL.

- [ ] **Step 3: Implement the flags, the tests' targets, the guide section, the docs and the CHANGELOG entry.** Delete `TestTheServerLinksNoNetworkingOrProfiler` in this commit; it is replaced, not skipped.

- [ ] **Step 4: Run the tests**

Run: `go test -race ./cmd/perf-agent-mcp/ ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp internal/perfmcp/guide.md internal/perfmcp/guide_test.go docs/mcp.md CHANGELOG.md
git commit -m "perf-agent-mcp: --live serves the page on loopback; tests that it never dials and every socket is loopback"
```

### Task 12: Verification and push

**Files:** none created; fixes go in their own commits.

- [ ] **Step 1: Race tests**

Run `go test -race ./internal/... ./cmd/... ./gpu/execlog/` with the CGO variables, then `go test -race ./...`.
Expected: `ok` everywhere; no `FAIL`, no race report. The Chrome tests ran, not skipped, on this machine (check `-v` output for `TestLivePageInChrome`).

- [ ] **Step 2: Lint**

Run `golangci-lint version` (it must print 2.11.4; install it on exit 127), then `golangci-lint run ./...` with the CGO variables.
Expected: `0 issues.`

- [ ] **Step 3: Notices and modules**

Run `BLAZESYM_SRC=/home/diego/github/blazesym scripts/third-party-notices.sh --check` and `git diff --exit-code ai-mcp-m2 -- go.mod go.sum`.
Expected: exit 0 for both.

- [ ] **Step 4: Existing outputs unchanged**

Run `go test ./internal/flamegraph/ -run Golden` and `go test ./internal/perfmcp/ -run 'RenderIsUnchanged|DiffToolUnchanged'`.
Expected: PASS.

- [ ] **Step 5: Manual session with a real client** (M2-R1: `claude -p … --mcp-config <scratch json>` only, never `claude mcp add`)

Configure the built binary with `--profiles` (the eval fixtures), `--output` (scratch) and `--live`. Ask it to open the live page and explain `b`; open the URL in a browser. Then ask it to select `c`, focus `a`, and diff `cpu` against `cpu-new`. Check:
- the tab follows each step without a reload;
- the header counts the figures;
- a rejected attempt made during the session appears on the open page;
- "Download static copy" saves a page that works from `file://`;
- stopping the client shows the banner.

Record what was checked in the PR body.

- [ ] **Step 6: Push**

```bash
git push -u origin ai-mcp-m2b
```

Expected: the branch is on the remote; the controller opens a draft PR stacked on M2's. Never merge.

---

## Self-review

- **Spec coverage.** Each part of the addendum maps to tasks:
  - D1, transport: Tasks 1 and 7; the replaced network test in Task 11.
  - D2, SSE: Tasks 2 and 7.
  - D3, the tools and what the page does on its own:
    - `page_open`, `page_explain` and `page_show`: Task 8;
    - `page_diff`: Tasks 9 and 10;
    - frame, focus and export: Tasks 4, 7 and 9;
    - the publication list and the banner: Task 5.
  - D4, ownership: Task 6, with `TestLivePageRacesNothing` in Task 7.
  - D5, security:
    - guard and headers: Task 1; through the routes: Task 7;
    - CSP and the script rules: Task 5;
    - token not logged: Tasks 7 and 11;
    - shutdown: Tasks 7 and 11;
    - offline banner: Task 5.
  - D6, static output: Task 8 (`TestRenderIsUnchangedByTheRefactor`) and the export in Task 7.
  - D7, fit with existing tests: Tasks 3, 4, 9, 10 and 11.
  - The threat-model rows are each tested in Tasks 1, 2, 5, 7 and 8. The history rules M2-R15..R20 on the live page are in Task 8.
- **Step scan.** Every code step names its file, its signature and the values the addendum pins. Tests carry their assertions inline. The Chrome probe and the socket test are spelled out, because their mechanics decide whether they test anything.
- **Type consistency.**
  - `LiveOptions` (T4) is used in T6 and T7.
  - `ExplainedLiveModel`, `ExplainedLiveFrame` and `ExplainedLiveHistory` (T4) are used in T6 and T7.
  - `LiveAsset` (T5) is used in T7.
  - `Hub.Publish` and `Clients` (T2) are used in T6–T8.
  - `CheckAddr` (T1) is used in T6 and T11.
  - `publication` and `liveSnapshot` (T6) are used in T7, T8 and T10.
  - `checkCall` (T8) is used by `render_flamegraph` and `page_explain`.
  - `DiffLiveModel`, `DiffLiveOptions`, `DiffLiveFrame`, `WriteDiffPageBounded`, `ErrPageTooLarge` and `profilemodel.DiffIndex` (T9) are used in T10.
  - `pins`, `admit`, `liveHistory` and `MaxRetainedBytes` (T6) are used in T7, T8 and T10. `ExplainedLiveHistory` takes `pub, version` (T4), and T6 passes them.
  - `compare` (T10) is used by `diff` and `page_diff`.
  - The state JSON field names are the same in the settled-calls table, T5 and T6.
- **Review Focus.** Each line has its owning test:
  1. `TestGuardChecksHostAgainstRebinding` and `TestEveryRouteIsGuarded`;
  2. `TestGuardWorksThroughAForwardedPort`;
  3. `TestAStuckClientNeverBlocksPublish`, `TestASecondTabAndAReconnectSeeTheCurrentState` and `TestAStuckTabNeverDelaysATool`;
  4. `TestLivePageInChrome` (banner) and `TestAnotherServersTokenIs404`;
  5. `TestLiveHistoryFollowsNewRejections` and `TestAFactsOnlyPageStillShowsRejections`.
- **Rulings M2b-R1..R6.** Each has its owning test:
  - R1: Task 5, `TestLivePageOrderingInChrome`;
  - R2: Tasks 7, 9 and 10, `TestExportReadsOnlyTheSnapshot`, `TestBoundedDiffExportRefusesBeforeBuilding` and `TestPageDiffExportTooLargeIs422`;
  - R3: Tasks 9 and 10, `TestDiffLiveFrameFindsANodeTheModelLeftOut` and `TestPageShowFindsADiffNodeTheModelLeftOut`;
  - R4: Task 6, `TestALargeProfileWithATinyModelIsChargedForItsEngine` and `TestStoreEvictionWhilePublishedKeepsOnePin`;
  - R5: Tasks 6 and 7, `TestSnapshotHistoryIsACopy` and `TestExportReadsOnlyTheSnapshot`;
  - R6: Task 1, `TestNavigationHeaderMatrix`.

  M2-R24's name cap is in Task 4 (`TestLiveModelCapsLongNames`).
- **Proportion.** Twelve tasks over three packages and the binary. The only code blocks are test lists and commit commands.
- **Open questions for the user** (from the addendum §12; defaults hold until answered):
  1. loopback listener at all (D1);
  2. `--live` default off;
  3. no browser-to-agent selection signal;
  4. the server never opens a browser;
  5. Remote/SSH use: out of scope (user decision, M2b-R7).
