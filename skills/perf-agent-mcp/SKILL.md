---
name: perf-agent-mcp
description: Use when asked to investigate, explain or compare CPU or GPU profiles, flame graphs, hot functions, pprof .pb.gz files or perf-agent output, to capture a program's profile and compare a change with a baseline, or when perf-agent MCP tools (open, top, render_flamegraph, capture) are available.
---

# Using the perf-agent MCP server

perf-agent-mcp answers questions about perf-agent profiles (pprof `.pb.gz`, with an optional GPU `.execlog`), and, when started with `--perf-agent`, captures new ones. Every figure comes from an engine, not from you.

## First

Read the server's own guide before the first tool call: the MCP prompt `perf-agent-guide`, or the resource `perf-agent://guide` if your client shows resources instead. The guide is the authority. This skill is a short orientation and a list of the mistakes agents make most; where the two differ, the guide wins.

## When to use

- A question about where time goes in a profile, why something is slow, what changed between two profiles, or what a GPU kernel launch costs.
- A request for a flame graph with explanations.
- Profiling a process or a command of yours, and comparing a change with a baseline, when the server was started with `--perf-agent` (see Capture loop).

## Workflow

1. `open` the profile (`path`, and `execlog` for GPU). Keep the `handle`, pick a `view`, read the `notes`.
2. `top` with `rows: "functions"` for the shape, `rows: "nodes"` for node IDs. Only node facts are claimable (`top` with `rows: "nodes"`, `path`, `facts`); function aggregates (`top` with `rows: "functions"`, `sandwich`) are not.
3. Drill into a node: `path` (its stack), `facts` (every fact), `sandwich` (callers and callees of a function), `gpu_kernels_for` (kernels under a launch node), `source` (source lines, when roots are configured).
4. Two profiles: `open` both, then `diff`.
5. Write each explanation as prose that places every figure with a claim placeholder such as `{c1}` and names frames with `{n:<node id>}`; give each claim its `metric`, `denominator`, `unit` and `value`.
6. `render_flamegraph` checks the claims and writes the page. If it is rejected, nothing was written: read every failure, fix them all (for a mismatch, re-check the claim's node and metric rather than copying the value), and call again with `attempt` set to the returned attempt. An existing `out` is never replaced, so use a new name for a new page.
7. The live page is not part of this server: it will ship with a separate long-lived serve binary. Use `render_flamegraph` when the user wants a page.

## Capture loop

1. `capture` with `command` (an absolute path, or a bare name in PATH; never `./x`) or `pid`, a `series` per loop and a `label`; keep the base with `keep`. It answers a `job` at once.
2. `capture_wait` with that `job` (or the capture `id`, `series/NNN`) until it returns the result: each call blocks at most its `timeout` (default 30s, at most 45s) and returns the state meanwhile. Send one capture call at a time and wait for its answer: a call queued behind another spends its `timeout` waiting there. The result holds the `handle` of each profile; use it with the tools above.
3. After a change, `capture` again with `compare_with` set to the base's id: it inherits the base's settings, and the result's `compare` carries the diff rows and the `next` call for the rest.
4. A failure is one JSON `error` with a `code`, `retryable`, `retry_after` and `next`: follow `next`. `busy` and `budget` say when to retry; `incomparable` and `unknown_baseline` mean the compare is wrong, not the capture.
5. `capture_cancel` stops a job at once; `capture_list` shows the running job and each series' history. A `capture_wait` on a capture this server no longer holds in memory answers from its manifest with `handles` set to `reopen` and the `recompute` calls that reopen it.

Every call names its `handle` and `view`; nothing is "current". The exceptions: `open` takes a `path`; `diff` takes `base_handle`, `base_view`, `new_handle` and `new_view`; if a handle is gone, `open` again. Lists are paged: pass `next_cursor` back as `cursor` with the same arguments.

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
| `glued_to_claim` | Write a range as "between {c1} and {c2}". No sign or digit before a placeholder; after it only a space, the end, or closing punctuation (. , ; : ! ? … or a closing bracket or quote). The claim shows its own unit, so no `%`, unit or letter after it. |
| `unplaced_claim` | Every claim needs its `{cN}` in the explanation's text; place it or drop the claim. |
| `unknown_metric` | The `metric` is a claimable-metric name, not a path in the `facts` JSON. Use `share_of_total`, `cumulative`, `quality_join_exact`, `serialization_true` and the other names the guide lists. |
| `wrong_scope` | `unsampled_portion` is a view-level fact: `"scope": "view"` and no node. Quality metrics take `"scope": "node"` or `"scope": "view"`. |
| `wrong_denominator` | A node claim copies the denominator and unit of the node's fact in `facts`; a view-scope quality claim copies them from the view's quality in `facts` under `view_scope`. |
| `unavailable_fact` | The engine has no value for it. Say the figure is unavailable; do not claim it or write it as 0. |
| `unresolved_node_ref` | Name a frame with `{n:<id>}` using an ID from `top` or `path`, never the frame's text. |
| `unknown_node` | The ID must exist in the view you passed; take it from `top` or `path` on that view. |

Other mistakes with no failure code: a claim renders with its unit, so never repeat the unit after the placeholder ("{c2} ns" shows "411 ns ns"; the result's notices say `unit_repeated` where it happened, and the page is still made); a figure typed freely in prose is shown as an unchecked number on the page, so put it in a claim; a frame name containing braces must be written as `{n:<id>}`, never pasted into prose; and a share under a GPU launch is not GPU time.

## Verify

After a successful render, the page shows the engine's values and the sampling notes. Quote those, not your draft.
