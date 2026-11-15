# perf-agent-mcp

`perf-agent-mcp` is an MCP server that lets an AI agent investigate perf-agent profiles. It speaks MCP over stdio only, is read-only and works offline. It answers from the same profile model as `cmd/flamegraph`; every figure an agent states about a profile is checked against that engine before a page is written or published. With `--live` it also shows a read-only [live page](#live-page) to a browser on the same machine.

## What it does not do

- It opens no outgoing connection: it never dials, looks a name up or makes an HTTP request. It listens only when started with `--live`, only from the first `page_*` call, and only on `127.0.0.1` at a port the kernel picks; it links `net/http` for that listener alone. Tests read the source of every package the binary links for client calls and listening calls, and check that every socket of a running server is TCP on loopback.
- It captures nothing: it never starts the perf-agent profiler, links neither `perfagent` nor `cilium/ebpf`, and needs no capabilities.
- It writes only pages under `--output`, and never replaces an existing file.
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
| `--live` | Enable the [live page](#live-page) and the `page_*` tools. Off by default; without it the server never listens. There is no flag for the address: the page is always served on `127.0.0.1` at a random port. |
| `--limit Name=Value` | Override a limit. Repeatable. Names are the fields of the limits table below, such as `Model.MaxRawBytes=1073741824`. An unknown name exits 2 and lists the valid ones. |
| `--version` | Print the version. |

Exit status: 0 at end of input or on SIGINT/SIGTERM, 1 on a runtime error, 2 on a usage error. Logs go to stderr; stdout carries only JSON-RPC.

### Roots

Every file is opened through an `os.Root`, so `..`, symlinks that leave the root and races cannot escape it. An agent may pass a path relative to a root or an absolute path inside it (under the root as given or under its resolved real path); anything else is refused by its text alone, without looking anything up, and the error names the root. Profiles and sources are only read. The output root receives pages (`.html`), published with a no-replace hard link: a name that exists is refused, so the agent picks a new one. Using the same directory for `--profiles` and `--output` works, but a page can never be written over an opened profile.

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
| `page_open` | Start the live page, unless it is started, and return its URL, its publications and how many tabs watch it. Needs `--live`. |
| `page_explain` | Check explanations exactly as `render_flamegraph` does and publish them on the live page instead of writing a file. Needs `--live`. |
| `page_show` | Steer the live page: switch publication, select a node, or lay the flame graph out from it. Needs `--live`. |
| `page_diff` | Compare two views exactly as `diff` does and publish the comparison on the live page. Needs `--live`. |

Every tool takes `handle` and `view` (a view name `open` returned, such as `cpu/nanoseconds#0`) unless noted: `open` takes a `path`, `diff` and `page_diff` take a base and a new handle and view, `page_open` takes no arguments, and `page_show` takes a `publication` and a `node`. One example call of each, as `arguments`:

```json
open               {"path": "cpu.pb.gz", "execlog": "run.execlog"}
top                {"handle": "<handle>", "view": "cpu/nanoseconds#0", "rows": "nodes", "by": "cum", "limit": 5}
path               {"handle": "<handle>", "view": "cpu/nanoseconds#0", "node": "<node id from top>", "limit": 200}
sandwich           {"handle": "<handle>", "view": "cpu/nanoseconds#0", "function": "<function ref from top with rows=functions>"}
gpu_kernels_for    {"handle": "<handle>", "view": "<a GPU view>", "node": "<launch node id from top>"}
facts              {"handle": "<handle>", "view": "cpu/nanoseconds#0", "node": "<node id from top>"}
diff               {"base_handle": "<handle>", "base_view": "cpu/nanoseconds#0", "new_handle": "<handle>", "new_view": "cpu/nanoseconds#0", "work_base": 1000, "work_new": 1200}
source             {"handle": "<handle>", "view": "cpu/nanoseconds#0", "node": "<node id from top>", "context": 5}
page_open          {}
page_explain       {"handle": "<handle>", "view": "cpu/nanoseconds#0", "explanations": []}
page_show          {"publication": "p1", "node": "<node id from top>", "focus": true}
page_diff          {"base_handle": "<handle>", "base_view": "cpu/nanoseconds#0", "new_handle": "<handle>", "new_view": "cpu/nanoseconds#0", "node": "<id from diff>"}
```

`render_flamegraph` has its own example under Explanations; `page_explain` takes the same `explanations`. `execlog`, `work_base`, `work_new`, `context`, `title`, `focus` and `page_diff`'s `node` are optional, and so is either of `page_show`'s `publication` and `node`; `top`, `path`, `sandwich`, `gpu_kernels_for`, `diff` and `source` also take `limit` and `cursor`, `top` takes `domain`.

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
- A range is written "between {c1} and {c2}". Text glued to a placeholder that changes its meaning is rejected as `glued_to_claim`: a sign, digit or decimal point before it, and after it anything but a space, the end of the text or closing punctuation (`. , ; : ! ? …`, a closing bracket or quote). A claim shows its own unit, so a `%`, `‰`, unit or letter in any script after it is glued.
- `title` is free text of at most `Explain.MaxTitleBytes` bytes with no placeholders; a longer one or one with a placeholder is refused before any attempt is recorded. Numbers in it are marked unchecked in the page's heading and counted in `unchecked_numbers`.
- The page's follow-up prompts name the profile's path relative to the profiles root and its `profile_digest`: whoever follows one opens that path and checks the digest matches.
- If anything fails, nothing is written. The result is `rejected`, lists every failure (with the engine's value when one can be computed; the result is itself bounded by `MaxResponseBytes`), and returns an `attempt` to pass when retrying. The page lists earlier rejected attempts for the same profile and view, made through any handle. That history is bounded (`MaxAttemptsPerView`, `MaxAttemptViews`, `MaxHistoryBytes` and the page's own size): what it leaves out, the page counts.

## Live page

Started with `--live`, the server can show what an agent explains on a page in the user's browser, and steer it while the conversation goes on. Nothing listens until the agent's first `page_*` call. That call opens a listener on `127.0.0.1` at a port the kernel picks, and stderr logs `live page listening addr=127.0.0.1:PORT`; the URL itself is only in the page tools' results.

The four tools:

- `page_open` starts the page and returns its URL. The agent gives the URL to the user: the server never opens a browser.
- `page_explain` runs `render_flamegraph`'s check, with the same failures, the same `attempt` rules and one attempt history shared by both tools, and publishes the explained flame graph instead of writing it. A rejected call publishes nothing. `"explanations": []` publishes a view's facts alone.
- `page_show` switches the page to a kept publication, selects a node (drawn or not) and, with `"focus": true`, lays the flame graph out from it.
- `page_diff` takes `diff`'s arguments without `limit` and `cursor`, refuses what `diff` refuses, and publishes the comparison. Its nodes are union IDs from `diff`.

An explained publication shows the server's own record of rejected attempts for its profile and view, made through any handle, and follows it as it grows. No argument hides it. It is bounded like `render_flamegraph`'s history, and the page counts what it leaves out.

What is served, and to whom:

- **This machine only.** The listener binds `127.0.0.1` and nothing else. Every request must name this machine in its `Host` (`localhost`, an IPv4 loopback address or `[::1]`), which defeats DNS rebinding. The page is for a browser on the same machine; remote use is not supported.
- **A token.** Every URL is under `/s/<token>/`, where the token is 256 random bits made for this run, compared in constant time. A wrong or missing token gets 404. The token is in the tool results only, never in a log line, so give the URL to the user only.
- **Read-only.** Only `GET` and `HEAD` are served, over HTTP/1 only, and a request with a body is refused. No handler changes server state: only the agent's tool calls do. No response carries an `Access-Control-*` header. Cross-site requests are refused, except a top-level navigation to the page itself, which holds no data.
- Every response carries a Content-Security-Policy that allows scripts, styles, images and fetches from the page's own origin only, with no inline code, no forms and no framing.

**Download a static copy.** The page's export link downloads the current publication as a standalone page: an explained page like the ones `render_flamegraph` writes, with its history, or a diff page like `cmd/flamegraph -diff`'s. The server builds it in memory within `MaxPageBytes` and refuses one that does not fit, with the reason on the page, before sending any of it. The browser saves it; the server writes no file.

What the page never does:

- It sends nothing anywhere: every request it makes is a relative `GET` to the server that served it.
- It runs no tool and changes nothing: the agent steers it, the user only reads and navigates.
- It does not work offline. It reads everything from the running server, so once `perf-agent-mcp` exits it says the server is gone and keeps the last state it showed. Download a static copy to keep one.

The page keeps at most `MaxPublications` publications, dropping the least recently shown and never the one on screen. `MaxRetainedBytes` is an estimated budget for what they keep: the profiles and views they pin, each counted once however many publications share it, plus their checked results, page models and histories. It is not a physical memory cap. The estimates are within half and twice the heap they stand for, so the real heap can reach about twice the budget, and responses being written are not counted. A publication that cannot fit is refused and changes nothing. `MaxPageClients` bounds the tabs that stream the page at once; a tab beyond it polls the server instead.

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

Defaults; override with `--limit Name=Value`. Sizes are bytes. Every limit must be positive (`Source.MaxContext` may be 0); a zero, negative or out-of-range value exits 2 and names the limit.

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
| `MaxRetainedBytes` (estimated memory the live page's publications keep, not a physical cap: the heap can reach about twice it; with `--live` at least `MaxPageBytes`) | 2147483648 (2 GiB) |
| `MaxPageClients` (live page streams at once) | 8 |
| `Source.MaxFileBytes` | 8388608 (8 MiB) |
| `Source.MaxLineBytes` | 512 |
| `Source.MaxContext` | 50 |
| `Source.MaxWalkEntries` | 200000 |
| `Source.MaxManifestBytes` | 16777216 (16 MiB) |
| `Source.GitTimeout` | 5s (a duration) |

Displayed frame names are cut at 1 KiB on pages; the node ID stays the reference. A response over `MaxResponseBytes` is refused with advice to page with a smaller `limit` or restart with a larger `--limit MaxResponseBytes=N`.

## Registering with a client

Claude Code:

```bash
claude mcp add perf-agent -- /path/to/perf-agent-mcp --profiles /path/to/profiles --output /path/to/pages
```

Add `--live` to either for the live page. Any other MCP client: run `/path/to/perf-agent-mcp --profiles DIR --output DIR` as a stdio server. The server answers protocol versions 2025-11-25 and 2025-06-18 and answers `server/discover` with method-not-found, so clients fall back to `initialize`.

## Evals

`scripts/mcp-evals.sh` replays a fixed set of questions through a real agent over frozen synthetic fixtures. It needs the `claude` CLI, `go` and `jq`, and it needs a model, so it costs tokens and is run by hand, never in CI. No capture leaves the machine.

The script builds `perf-agent-mcp` into a temporary directory, writes the fixtures (`cpu.pb.gz`, `cpu-new.pb.gz`, `gpu.pb.gz` and its `gpu.executions` log), and runs `claude -p` once per question in `internal/perfmcp/evals/questions.json` with a generated `--mcp-config` and `--strict-mcp-config`, so your own Claude MCP configuration is neither loaded nor changed. It then checks the rendered pages: each exists, its header counts at least one checked figure, each expected value sits inside a checked claim, and no forbidden text (for example reading a sampled share as GPU time) appears. Unchecked-number counts are printed per page.

Each question has `id`, `prompt`, `profile`, an optional `execlog`, and `expect` (`page`, `checked_values`, `require`, `forbid`). The checks run as `TestEvalPages` with `PERF_AGENT_MCP_EVAL_DIR` set to the scratch directory; without it both eval tests skip.
