# perf-agent guide

perf-agent-mcp answers questions about perf-agent profiles (pprof `.pb.gz`, with an optional GPU `.execlog`). It is read-only, offline, and every figure comes from an engine, not from you.

## Investigation order

`open` -> `top` -> `path` / `gpu_kernels_for` / `sandwich` -> `facts` -> `source` -> `render_flamegraph`. For two profiles, open both and call `diff`.

1. `open {path}` returns a `handle`, the `views` (pass one as `view`), the default view and its quality and sampling `notes`. Paths are relative to the profiles root or absolute inside it.
2. `top` with `rows: "functions"` (aggregated by function) or `"nodes"` (call-tree nodes, which carry the node IDs). `by: cum|self`.
3. `path {node}` is the root-first stack of a node; `sandwich {function}` its callers and callees; `gpu_kernels_for {node}` the kernels under a launch node.
4. `facts {node}` is the full fact set of one node.
5. `source {node}` shows source lines for a node, when source roots were configured.
6. `render_flamegraph` writes an explained page (see Explanations).

Handles and views are explicit and immutable: every call names its `handle` and `view`, nothing is "current". A handle names evidence (the profile digest and the execlog) and stays valid while the server keeps it open; if it is gone, `open` again.

## Reading the numbers

- Every integer (values, counts, weights) is a decimal string. Do not convert it to a float; quote it as the string.
- A fact has `status` `ok`, `zero` or `unavailable`. An unavailable fact has a `reason` and no `value`: unavailable is not zero, never report it as 0.
- A share names its `denominator` (`view_total`, parent, ...). Say which one.
- Lists are paged: `total_rows`, `skipped`, `omitted_count`, `omitted_weight` (with `omitted_weight_basis`) and `next_cursor`. Pass the cursor back with the same arguments. The omitted rows overlap (a callee is inside its caller), so `omitted_weight` is a sum of the rows' own values, never a remaining share.
- `unsampled_portion` is a view-level fact. It is in the `facts` response under `view_scope`, not on any node.

## GPU rules

- A width under `[gpu:launch]` is a sample of launches, not a share of GPU time.
- `[gpu:launch unsampled]` has no caller and is never attributed to a stack.
- Nothing is ever multiplied by the sample period: it is never multiplied by the sample period to get launches, executions or time.
- Executions come only from a checked execlog (`open` reports whether one was accepted and why not). Never count samples or execlog rows as executions.
- Every GPU answer carries the view's sampling `notes`; read them.

## Diff

`diff` reports presence (a node is only in one profile) separately from interpretation. Absence never proves the cost was eliminated: it may have moved, been inlined, or lost its symbols. Both views must have the same sample type, unit and index. Pass `work_base` and `work_new` for per-unit deltas.

## Source

`source` statuses: `build_verified` (a manifest ties the file to the build), `user_selected` (the file is in a git checkout the user named), `source_unverified` (it exists but nothing ties it to the profiled build), `ambiguous` (several candidates), `unresolved`. A PC-sampled (`gpu_pc`) source line is `build_verified` only by a manifest naming the capture id. Do not present `source_unverified` lines as the code that ran.

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

- Metrics are the fact names `facts` returns. `"scope": "view"` with no `node` is for `unsampled_portion` (view-level); every other metric is node-level.
- Put every number in prose from a claim placeholder `{cN}` (`{c1}`). `{cN}` refers to a claim of the same explanation. Numbers typed freely are marked unchecked on the page.
- To name a frame use `{n:<id>}`, especially for frame names containing braces; frame names are never parsed for placeholders.
- For a range write "between {c1} and {c2}". A bare range like "{c1}-{c2}" is glued_to_claim and rejected, as is a sign or exponent next to a placeholder.
- A rejected call writes nothing. It returns every failure with the engine's value; fix them and call again with `attempt` set to the returned attempt. Rejected renders stay on the page's history, so retries are visible.
- An existing `out` is never replaced: choose a new name. You cannot set rounding or tolerance.
- When a tool result is an error it says what to call next.
