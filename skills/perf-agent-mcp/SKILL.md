---
name: perf-agent-mcp
description: Use when asked to investigate, explain or compare CPU or GPU profiles, flame graphs, hot functions, pprof .pb.gz files or perf-agent output, or when perf-agent MCP tools (open, top, render_flamegraph) are available.
---

# Using the perf-agent MCP server

perf-agent-mcp answers questions about perf-agent profiles (pprof `.pb.gz`, with an optional GPU `.execlog`). It is read-only and offline. Every figure comes from an engine, not from you.

## First

Read the server's own guide before the first tool call: the MCP prompt `perf-agent-guide`, or the resource `perf-agent://guide` if your client shows resources instead. The guide is the authority. This skill is a short orientation and a list of the mistakes agents make most; where the two differ, the guide wins.

## When to use

- A question about where time goes in a profile, why something is slow, what changed between two profiles, or what a GPU kernel launch costs.
- A request for a flame graph with explanations.
- Not for profiling a live process or writing profiles: this server only reads existing files.

## Workflow

1. `open` the profile (`path`, and `execlog` for GPU). Keep the `handle`, pick a `view`, read the `notes`.
2. `top` with `rows: "functions"` for the shape, `rows: "nodes"` for node IDs. Only node facts are claimable (`top` with `rows: "nodes"`, `path`, `facts`); function aggregates (`top` with `rows: "functions"`, `sandwich`) are not.
3. Drill into a node: `path` (its stack), `facts` (every fact), `sandwich` (callers and callees of a function), `gpu_kernels_for` (kernels under a launch node), `source` (source lines, when roots are configured).
4. Two profiles: `open` both, then `diff`.
5. Write each explanation as prose that places every figure with a claim placeholder such as `{c1}` and names frames with `{n:<node id>}`; give each claim its `metric`, `denominator`, `unit` and `value`.
6. `render_flamegraph` checks the claims and writes the page. If it is rejected, nothing was written: read every failure, fix them all, and call again with `attempt` set to the returned attempt. An existing `out` is never replaced, so use a new name for a new page.

Every call names its `handle` and `view`; nothing is "current". The exceptions: `open` takes a `path`, and `diff` takes `base_handle`, `base_view`, `new_handle` and `new_view`. If a handle is gone, `open` again. Lists are paged: pass `next_cursor` back as `cursor` with the same arguments.

## Hard rules

- Integers are decimal strings. Quote them as strings; never turn them into floats. The exception is source line numbers, which are JSON integers.
- A fact is `ok`, `zero` or `unavailable`. Unavailable is not zero: never report it as 0.
- Say which denominator a share uses (`view_total`, the parent, ...).
- A width under `[gpu:launch]` is a sample of launches, not a share of GPU time. Never multiply anything by the sample period to get launches, executions or time.
- Executions come only from an accepted execlog. Never count samples as executions.
- `source_unverified` lines are NOT VERIFIED as the code that ran: they may be the exact file, but nothing proves it. Do not present them as verified.
- A diff shows presence, not cause: a function missing from one side may have moved, been inlined or lost its symbols.
- Report what the engine returned. Do not compute your own percentages or round values.

## Common rejections

| Failure | Fix |
|---|---|
| `value_mismatch` | Re-check that the claim's node and metric are what the sentence says; rewrite the sentence or the claim. Never just copy the engine's value into the claim. |
| `glued_to_claim` | Write a range as "between {c1} and {c2}". No sign, exponent, dash, `%` or letter next to a placeholder: the claim shows its own unit. |
| `unplaced_claim` | Every claim needs its `{cN}` in the explanation's text; place it or drop the claim. |
| `unknown_metric` | The `metric` is a claimable-metric name, not a path in the `facts` JSON. Use `share_of_total`, `cumulative`, `quality_join_exact`, `serialization_true` and the other names the guide lists. |
| `wrong_scope` | `unsampled_portion` is a view-level fact: `"scope": "view"` and no node. Quality metrics take `"scope": "node"` or `"scope": "view"`. |
| `wrong_denominator` | A node claim copies the denominator and unit of the node's fact in `facts`; a view-scope quality claim copies them from the view's quality in `facts` under `view_scope`. |
| `unavailable_fact` | The engine has no value for it. Say the figure is unavailable; do not claim it or write it as 0. |
| `unresolved_node_ref` | Name a frame with `{n:<id>}` using an ID from `top` or `path`, never the frame's text. |
| `unknown_node` | The ID must exist in the view you passed; take it from `top` or `path` on that view. |

Other mistakes with no failure code: a claim renders with its unit, so never repeat the unit after the placeholder ("{c2} ns" shows "411 ns ns"); a figure typed freely in prose is shown as an unchecked number on the page, so put it in a claim; a frame name containing braces must be written as `{n:<id>}`, never pasted into prose; and a share under a GPU launch is not GPU time.

## Verify

After a successful render, the page shows the engine's values and the sampling notes. Quote those, not your draft.
