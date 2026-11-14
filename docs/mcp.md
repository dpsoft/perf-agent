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

Exit status: 0 at end of input, 1 on a runtime error, 2 on a usage error. Logs go to stderr; stdout carries only JSON-RPC.

### Roots

Every file is opened through an `os.Root`, so `..`, symlinks that leave the root and races cannot escape it. An agent may pass a path relative to a root or an absolute path inside it; anything else is refused and the error names the root. Profiles and sources are only read. The output root receives pages (`.html`), published with a no-replace hard link: a name that exists is refused, so the agent picks a new one. Using the same directory for `--profiles` and `--output` works, but a page can never be written over an opened profile.

## Tools

Integers are decimal strings in every result. A fact has `status` `ok`, `zero` or `unavailable`; an unavailable fact has a `reason` and no `value`.

| Tool | Use |
|---|---|
| `open` | Open a profile (and optional execlog). Returns a handle, the views, capabilities, quality and sampling notes. |
| `top` | Hottest functions or call-tree nodes by cumulative or self value, paged. |
| `path` | The root-first frames of one node, paged. |
| `sandwich` | Callers and callees of one function. |
| `gpu_kernels_for` | The kernels under a GPU launch node. |
| `facts` | Every fact of one node, plus view-level values under `view_scope`. |
| `diff` | Compare two opened profiles of the same view. |
| `source` | Source lines for a node and how well they are tied to the profiled build. |
| `render_flamegraph` | Check explanations and write an explained flame graph page. |

Example, opening a profile and listing its widest nodes:

```json
{"name": "open", "arguments": {"path": "cpu.pb.gz"}}
{"name": "top", "arguments": {"handle": "<handle from open>", "view": "cpu/nanoseconds#0", "rows": "nodes", "limit": 5}}
```

Lists carry `total_rows`, `skipped`, `omitted_count`, `omitted_weight` (the sum of the omitted rows' own values, which overlap, so never a remaining share) and `next_cursor`; pass the cursor back with the same arguments.

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
- Prose, hypotheses and `next_measurement` place figures with `{cN}`, a claim of the same explanation, and frames with `{n:<id>}`. Any other number is shown as unchecked.
- `unsampled_portion` is a view-level fact: claim it with `"scope": "view"` and no node.
- A range is written "between {c1} and {c2}". Text glued to a placeholder that changes its meaning (a sign, an exponent, a decimal point) is rejected as `glued_to_claim`.
- If anything fails, nothing is written. The result is `rejected`, lists every failure with the engine's value, and returns an `attempt` to pass when retrying. The page lists earlier rejected attempts for the same profile and view.

## Source statuses

| Status | Meaning |
|---|---|
| `build_verified` | A manifest ties the file to the build. A PC-sampled (`gpu_pc`) line is verified only by a manifest naming the capture id. |
| `user_selected` | The file is in a git checkout under the named source root and matches its committed blob. |
| `source_unverified` | The file exists but nothing ties it to the profiled build; the reason says why. |
| `ambiguous` | Several candidate files (at most 16 are listed). |
| `unresolved` | No file was found. |

## Limits

Defaults; override with `--limit Name=Value`. Sizes are bytes; every limit must be positive.

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
