# perf-agent guide

perf-agent-mcp answers questions about perf-agent profiles (pprof `.pb.gz`, with an optional GPU `.execlog`). It is read-only, offline, and every figure comes from an engine, not from you.

## Investigation order

`open` -> `top` -> `path` / `gpu_kernels_for` / `sandwich` -> `facts` -> `source` -> `render_flamegraph`. For two profiles, open both and call `diff`.

1. `open {path, execlog?}` (`execlog` is a GPU executions log in the profiles root) returns a `handle`, the `views` (pass one as `view`), the default view and its quality and sampling `notes`. Paths are relative to the profiles root or absolute inside it.
2. `top` with `rows: "functions"` (aggregated by function) or `"nodes"` (call-tree nodes, which carry the node IDs). `by: cum|self`. Only node facts are claimable: the values `top` with `rows: "nodes"`, `path` and `facts` give for a node. Function aggregates (`top` with `rows: "functions"`, `sandwich`) sum a function over every node it appears in; no claim can state them.
3. `path {node}` is the root-first stack of a node; `sandwich {function}` its callers and callees; `gpu_kernels_for {node}` the kernels under a launch node.
4. `facts {node}` is the full fact set of one node.
5. `source {node}` shows source lines for a node, when source roots were configured.
6. `render_flamegraph` writes an explained page (see Explanations).

Handles and views are explicit and immutable: every call names its `handle` and `view`, nothing is "current" (`open` takes a `path` instead; `diff` takes `base_handle`, `base_view`, `new_handle` and `new_view`). A handle names evidence (the profile digest and the execlog) and stays valid while the server keeps it open; if it is gone, `open` again.

## Reading the numbers

- Every integer (values, counts, weights, a failure's `explanation` index) is a decimal string. Do not convert it to a float; quote it as the string. Only source line numbers (`line`, `lines[].n`) are JSON integers.
- A fact has `status` `ok`, `zero` or `unavailable`. An unavailable fact has a `reason` and no `value`: unavailable is not zero, never report it as 0.
- A share names its `denominator` (`view_total`, parent, ...). Say which one, and copy the fact's denominator and unit into the claim.
- Lists are paged: `total_rows`, `skipped`, `omitted_count`, `omitted_weight` (with `omitted_weight_basis`) and `next_cursor`. Pass the cursor back with the same arguments. The omitted rows overlap (a callee is inside its caller), so `omitted_weight` is a sum of the rows' own values, never a remaining share.
- `unsampled_portion` and the view's quality are view-level. They are in the `facts` response under `view_scope`, for whichever view you pass (`open` gives the quality of the default view only), never on any node.

## GPU rules

- A width under `[gpu:launch]` is a sample of launches, not a share of GPU time.
- `[gpu:launch unsampled]` has no caller and is never attributed to a stack.
- Nothing is multiplied by the sample period to get launches, executions or time: it is never multiplied by the sample period.
- Executions come only from a checked execlog (`open` reports whether one was accepted and why not). Never count samples or execlog rows as executions.
- Every GPU answer, `render_flamegraph` results included, carries the view's sampling `notes`; read them. A CPU answer has no `notes` key, except `open`, whose `notes` is always present.

## Diff

`diff` reports presence (a node is only in one profile) separately from interpretation. Absence never proves the cost was eliminated: it may have moved, been inlined, or lost its symbols. Both views must have the same sample type, unit and index. Pass `work_base` and `work_new` for per-unit deltas.

## Source

`source` statuses: `build_verified` (a manifest ties the file to the build), `user_selected` (the file is in a git checkout the user named and matches its committed blob; this does not tie it to the profiled build), `source_unverified` (it exists but nothing ties it to the profiled build), `ambiguous` (several candidates), `unresolved` (also an unreadable file). A PC-sampled (`gpu_pc`) source line is `build_verified` only by a manifest naming the capture id. Do not present `source_unverified` lines as the code that ran.

## Explanations (render_flamegraph)

Write each figure as a claim and refer to it from the prose. The engine checks the claim against its own fact at its own rounding and the page shows the engine's value.

```json
{
  "handle": "<handle>", "view": "cpu/nanoseconds#0", "out": "hot.html",
  "explanations": [{
    "node": "<node id>",
    "claims": [
      {"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": "0.411"}
    ],
    "prose": "{n:<node id>} holds {c1} of the view.",
    "hypotheses": ["Contention on the allocator"],
    "next_measurement": "Profile again with the allocator pinned."
  }]
}
```

- A claim's `metric` is one of the names under Claimable metrics below, not a path in the `facts` JSON. Node-level metrics (the default scope) need a `node` (or the explanation's node). Quality metrics take either scope: `"scope": "node"` for the node, `"scope": "view"` with no `node` for the whole view. `unsampled_portion` is view-level only: `"scope": "view"` and no node.
- Put every number in prose from a claim placeholder `{cN}` (`{c1}`). `{cN}` refers to a claim of the same explanation, and every claim must be placed by its `{cN}` (else `unplaced_claim`). Numbers typed freely are marked unchecked on the page.
- A claim renders with its unit, so never repeat the unit after the placeholder ("{c2} ns" shows "411 ns ns"); a `%` or letter glued after it ("{c1}%", "{c1}ms") is rejected.
- `title` is free text of at most 200 bytes (`Explain.MaxTitleBytes`) with no placeholders. Numbers in it are marked unchecked and counted in `unchecked_numbers`; keep figures in claims.
- To name a frame use `{n:<id>}`, especially for frame names containing braces; frame names are never parsed for placeholders.
- For a range write "between {c1} and {c2}". A bare range like "{c1}-{c2}" is glued_to_claim and rejected, as is a sign or exponent next to a placeholder.
- A rejected call writes nothing. It returns every failure, with the engine's value when one can be computed, and is itself bounded by MaxResponseBytes; fix them and call again with `attempt` set to the returned attempt. Rejected renders stay on the page's history, so retries are visible.
- An existing `out` is never replaced: choose a new name. You cannot set rounding or tolerance.
- When a tool result is an error it says what to call next.
- A page's follow-up prompts name the profile's path relative to the profiles root and its digest: open that path and check that `profile_digest` matches before answering them.

## Claimable metrics

A metric name is the fact's path in the `facts` JSON with the dots turned into underscores, and the fields of `quality` named quality_ followed by the path.

- Node facts: `cumulative`, `self`, `share_of_total`, `share_of_parent`, `executions_below`, `kernel_names_observed`, `inexact_portion`; `serialization.true|false|unknown|missing` is `serialization_true`, `serialization_false`, `serialization_unknown`, `serialization_missing`.
- Quality, node or view scope: `quality_join_exact`, `quality_join_heuristic`, `quality_join_unmatched`, `quality_ambiguous`, `quality_graph_refused`, `quality_unsampled_launches`, `quality_unresolved`, `quality_collision_possible`; `quality.serialized.true|false|unknown|missing` is `quality_serialized_true`, `quality_serialized_false`, `quality_serialized_unknown`, `quality_serialized_missing`.
- `quality_unresolved_by_module` also needs `module`, a key of `quality.unresolved_by_module` (`""` is the no-module key).
- View only: `unsampled_portion`.
