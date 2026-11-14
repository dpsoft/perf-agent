# perf-agent-mcp

`perf-agent-mcp` is an MCP server that lets an AI agent investigate perf-agent profiles. It speaks MCP over stdio only, is read-only and works offline. It answers from the same profile model as `cmd/flamegraph`; every figure an agent states about a profile is checked against that engine before a page is written.

## What it does not do

- It opens no network connection and links neither `net`, `net/http` nor `crypto/tls`. A test checks this.
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

Every tool takes `handle` and `view` (a view name `open` returned, such as `cpu/nanoseconds#0`) unless noted. One example call of each, as `arguments`:

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

`render_flamegraph` has its own example under Explanations. `execlog`, `work_base`, `work_new` and `context` are optional; `top`, `path`, `sandwich`, `gpu_kernels_for`, `diff` and `source` also take `limit` and `cursor`, `top` takes `domain`.

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
- A range is written "between {c1} and {c2}". Text glued to a placeholder that changes its meaning (a sign, an exponent, a decimal point, or a `%` or letter after it: the claim shows its own unit) is rejected as `glued_to_claim`.
- `title` is free text of at most `Explain.MaxTitleBytes` bytes with no placeholders; a longer one or one with a placeholder is refused before any attempt is recorded. Numbers in it are marked unchecked in the page's heading and counted in `unchecked_numbers`.
- The page's follow-up prompts name the profile's path relative to the profiles root and its `profile_digest`: whoever follows one opens that path and checks the digest matches.
- If anything fails, nothing is written. The result is `rejected`, lists every failure (with the engine's value when one can be computed; the result is itself bounded by `MaxResponseBytes`), and returns an `attempt` to pass when retrying. The page lists earlier rejected attempts for the same profile and view.

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

Any other MCP client: run `/path/to/perf-agent-mcp --profiles DIR --output DIR` as a stdio server. The server answers protocol versions 2025-11-25 and 2025-06-18 and answers `server/discover` with method-not-found, so clients fall back to `initialize`.

## Evals

`scripts/mcp-evals.sh` replays a fixed set of questions through a real agent over frozen synthetic fixtures. It needs the `claude` CLI, `go` and `jq`, and it needs a model, so it costs tokens and is run by hand, never in CI. No capture leaves the machine.

The script builds `perf-agent-mcp` into a temporary directory, writes the fixtures (`cpu.pb.gz`, `cpu-new.pb.gz`, `gpu.pb.gz` and its `gpu.executions` log), and runs `claude -p` once per question in `internal/perfmcp/evals/questions.json` with a generated `--mcp-config` and `--strict-mcp-config`, so your own Claude MCP configuration is neither loaded nor changed. It then checks the rendered pages: each exists, its header counts at least one checked figure, each expected value sits inside a checked claim, and no forbidden text (for example reading a sampled share as GPU time) appears. Unchecked-number counts are printed per page.

Each question has `id`, `prompt`, `profile`, an optional `execlog`, and `expect` (`page`, `checked_values`, `require`, `forbid`). The checks run as `TestEvalPages` with `PERF_AGENT_MCP_EVAL_DIR` set to the scratch directory; without it both eval tests skip.
