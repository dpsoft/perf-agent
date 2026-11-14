# AI-explained flame graphs and profile-driven optimization: design

**Status:** design, not implemented. Iterated in draft PR #262; revised after
two read-only reviews (Workforce messages #267 and #271).
**Date:** 2026-11-13
**Mocks:** [assets/2026-11-13-ai-flamegraph-mocks.html](assets/2026-11-13-ai-flamegraph-mocks.html).
The mocks are static and use invented numbers. They show the intended shape:
the explained HTML, an agent session over MCP and the report of an autonomous
loop. Where this document and the mocks disagree, this document wins.

## 1. Intent

perf-agent already produces profiles that very few tools can read well. They
contain CPU stacks, off-CPU time, and GPU kernels causally attributed to the
CPU call path that launched them. A wide frame in that graph raises the same
questions every time:
- Why is this wide?
- What is it waiting on?
- Is it the profiler's own overhead?
- What should I change, and did the change help?

Answering them today takes an expert who already knows that a width under
`[gpu:launch]` is a sample of launch stacks, not a share of GPU time.

The goal is to let a language model help answer those questions **without
letting it invent numbers**. The same machinery then has to drive an
autonomous loop: profile, explain, change code, measure again, keep or
discard.

Two kinds of users, one engine:

- **A human** asks an agent such as Claude Code, or any MCP client, to
  "explain this profile". The agent investigates with perf-agent's tools and
  leaves an HTML flame graph whose "Explain path" panel is already filled in.
  Follow-up questions continue in the agent's chat.
- **An autonomous agent** uses the same tools plus a diff. It runs a bounded
  optimization loop in a worktree and ends with a draft PR that carries the
  evidence.

### Prior art, briefly

- **Grafana Pyroscope "Flame graph AI":** sends the whole profile as a DOT
  graph and tells the model to omit numbers. The answers are qualitative and
  cannot be checked. This is the anti-pattern.
- **Polar Signals' MCP server (June 2026):** the agent navigates a funnel of
  query tools (table, sandwich, focused flame graph, source) instead of
  receiving the profile as a dump. Output is truncated breadth-first and
  says so. Evals replay questions against frozen data. This is the model for
  the tool design here.
- **Datadog DODO:**
  - feeds a pruned production profile tree to the model as ground truth;
  - freezes the benchmark and checks that its profile resembles production;
  - keeps the best result.

  This is the model for the loop.
- **Caution:** a 2026 replay study (arXiv 2607.01211) found that most
  published "performance patches" did not reproduce their gain across
  machines and rounds. Machine noise is easy to mistake for a patch effect.
- **Open space (as far as we found):** among the tools above, none explains a
  single selected path, and none attributes GPU kernels to the CPU launch
  stack. This is a reading of their public docs and code in November 2026, not
  a survey of every profiler.
- The descriptions above come from public docs, blog posts and, for Grafana,
  its source; their accuracy and eval claims were not reproduced here.

## 2. Decisions already made

These were decided by the user and are not open:

1. **No model or API key inside perf-agent.** perf-agent computes facts,
   checks the claims an agent makes, and renders pages. The model runs in
   whatever agent client the user already uses. perf-agent never sends
   profile data anywhere.
2. **One engine for humans and agents.** The MCP server is the engine. It
   answers queries and it renders the explained HTML. The page and the agent
   read the same numbers, computed by the same code.
3. **Autonomy ends at a draft PR.**
   - The autonomous loop may open a draft PR.
   - It never merges. Merging is done by the user, or by an agent only on the
     user's explicit instruction for that PR.
4. **Draft PR first.** Work is iterated in a draft PR shown to the user, and
   nothing is merged without the user's explicit approval.
5. **Order:**
   1. offline facts engine, with identity, provenance, quality and diff;
   2. MCP server and structured explanations;
   3. autonomous loop, as a separate client.

## 3. Goals, non-goals, anti-goals

### Goals

- **Facts:** a reader, human or model, can ask for the facts of any path in a
  perf-agent profile and get numbers that are exact, reproducible and labelled
  with their definition and denominator.
- **Diff:** two profiles can be diffed. The output says what changed, what
  appeared, what disappeared, and how well the two profiles can be compared.
- **Checked claims:** an agent can attach explanations to paths. Every number
  in them is rendered by perf-agent from a structured claim, and the page
  makes clear which parts are checked figures and which are unverified prose.
- **Loop evidence:** an autonomous loop can measure, change and re-measure,
  and accepts a change only by a criterion fixed before it starts.

### Non-goals (for this design)

- Live, in-page chat. The HTML is a snapshot; follow-ups go through the agent.
  Serving the page inside an MCP client is a possible later step.
- Capturing or controlling profiling from the MCP server. The first server is
  read-only over files.
- Continuous or fleet profiling, remote storage, or a hosted service.
- Explaining profiles from other profilers. pprof files from perf-agent are
  the target. Other pprof files may work and are not promised.

### Anti-goals: things that would make this a failure even if it works

- **A badge that implies verified conclusions.** The page may say "figures and
  references checked". It must never suggest that the reasoning was verified.
- **Read-outs that contradict the warnings.** A number shown to the user must
  never contradict the profile's own semantics:
  - a GPU width under `[gpu:launch]` must not be read as a share of GPU time;
  - an unsampled launch must not be given a caller;
  - nothing may be multiplied by the sample period.
- **"Zero" for missing data.** "Zero" and "unavailable" must never be
  interchangeable.
- **Accepting noise as a gain.** A loop that accepts a change because of
  machine noise, or because it altered the benchmark.
- **Leaking data.** perf-agent opening a network connection, or reading files
  outside directories the user allowed.

## 4. What exists today (verified at 13c31b88)

### Fold and render

- `internal/foldedstacks.Fold` (`internal/foldedstacks/fold.go:265`)
  aggregates samples by the concatenated frame **names** (`fold.go:357`).
- Location and function identity is lost at that point.
- Label summaries are kept only globally, not per path.
- It refuses negative values, that is, diff profiles (`fold.go:313-319`).
- The renderer also builds its tree keyed by name (`buildTree`,
  `internal/flamegraph/render.go:270`).

### Inexact samples and sampling warnings

- `Fold` marks samples as inexact when `gpu_join` is `heuristic` or
  `unmatched`, or when `gpu_ambiguous=true` (`fold.go:129-140`).
- `gpuSamplingWarnings` (`fold.go:625`) states the central semantic rule.
  Launch **stacks** are sampled, one in `gpu_sample_period`, while every
  execution's duration is recorded. So the width under `[gpu:launch]` is a
  sample of launches, not a share of GPU time. Nothing is scaled.

### GPU projection

- `gpu/projection.go:476-511` builds GPU samples leaf-first as
  `[gpu:kernel:<name>] → [gpu:launch] → <CPU stack>`.
- Without a sampled stack, it builds
  `[gpu:kernel:<name>] → [gpu:launch unsampled]`.
- The synthetic frames carry a name only (`pprof.FrameFromName`,
  `pprof/pprof.go:104`). They still get a mapping: the builder's fallback puts
  them on the profile's first mapping, which has an empty file name
  (`pprof/pprof.go:430`, `534-550`). "No mapping" therefore means
  `Mapping == nil` **or** `Mapping.File == ""`.
- With PC sampling, one execution yields several samples
  (`gpu/projection.go:158`).

### GPU aggregation and the executions log

- The aggregated GPU profile omits `gpu_correlation`
  (`gpuprofile/profiler.go:306-309`), so executions with the same stack and
  labels merge into one sample. Launch or execution counts are therefore
  **not** recoverable from the pprof.
- `gpu/execlog` (written with `--gpu-executions-output`) records one row per
  projected sample. Each row names the aggregated sample index, the execution
  ordinal and the correlation.
- Its trailer binds the log to the profile by SHA-256 and sample count
  (`Trailer.Verify`, `gpu/execlog/execlog.go:430`).
- Deduplicating `Execution` over the samples under a path gives an exact
  execution count for that path.

### Labels and CPU profiles

- `gpu_serialized` **is emitted on every GPU execution**
  (`gpu/projection.go:791-812`), with values `true`, `false` or `unknown` from
  `SerializationState.String()` (`gpu/types.go:515`); the zero value is
  `unknown`. The windows are classified in `gpu/serialization.go`. The comment
  at `gpu/projection.go:83-87` saying the label does not exist yet is stale and
  should be corrected separately. A sample may also lack the label (a profile
  from an older build); that is a fourth state, `missing`.
- `gpu_graph_refused` is emitted (`gpu/projection.go:765`).
- CPU and off-CPU profiles carry a mapping path, a GNU build-id and a file
  offset for locations the symbolizer could place in a module
  (`docs/output.md`). Not every location has them: unresolved frames fall back
  to name-based locations on the default mapping (`pprof/pprof.go:430`), and
  kernel and JIT frames sit on the `[kernel]` and `[jit]` sentinel mappings.

### Other building blocks

- Frame domains (`application`, `vendor`, `shim`, `boundary`, `gpu-kernel`
  and others) are inferred by `flamegraph.Classify`
  (`internal/flamegraph/domain.go:158`). The legend already says they are
  inferred.
- No pprof diff exists anywhere in the repository.
- No LLM or MCP code exists.

## 5. Architecture

```
            pprof (+ optional execlog sidecar)
                         │
              ┌──────────▼───────────┐
              │  profile model (M1)  │  identity, provenance, quality,
              │  - load + digest     │  path facts, bilateral diff
              │  - views             │  (pure Go, offline, no Agent)
              └───┬──────────────┬───┘
                  │              │
       ┌──────────▼───┐   ┌──────▼──────────────┐
       │ MCP server   │   │ HTML renderer        │
       │ (M2, stdio,  │──▶│ (existing renderer,  │
       │  read-only)  │   │  fed by the model)   │
       └──────┬───────┘   └──────────────────────┘
              │ tools
       ┌──────▼───────────────┐      ┌──────────────────────┐
       │ agent client          │◀────│ loop client (M3)     │
       │ (Claude Code, etc.)   │     │ bench, A/B, worktree, │
       └───────────────────────┘      │ draft PR             │
                                      └──────────────────────┘
```

- **Profile model (M1)** is the single source of truth for numbers. The
  renderer, the MCP tools and the loop's acceptance check all read from it.
- **MCP server (M2)** is a thin, read-only adapter over the model. It also
  owns the render tool that turns structured explanations into an HTML file.
- **Loop (M3)** is a separate client. It calls the MCP tools, or the model
  directly, and never lives inside the server.

## 6. Milestone 1: the offline profile model

M1 has no AI in it, and it is useful on its own: it feeds the "Profile facts"
section of the page and gives the repository its first diff.

### 6.1 Package location

**My call:**
- Put it in `internal/profilemodel` for M1.
- Everything that consumes it in M1 and M2 lives in this module (renderer,
  `cmd/flamegraph`, the MCP binary), so `internal/` does not block anything.
- Keeping it internal also avoids committing to a public API before the MCP
  contract settles.
- Promotion to a public package is a later, separate decision.

**Builder's call:** the package name.

### 6.2 Loading, provenance and evidence identity

- **Load a profile** from a `.pb.gz` or `.pb` file. Keep:
  - its SHA-256 over the raw bytes as read, which is the **profile digest**;
  - its sample types;
  - its comments, including `perf-agent.capture_id=`;
  - its mappings.
- **Input limits.** Loading is bounded, not only answering (defaults are the
  builder's call, all configurable; a profile over a limit is refused with the
  limit named; truncating answers does not bound the cost of loading):
  - **during parsing:** the raw file size and the decompressed byte count are
    capped before `profile.ParseData` sees the bytes. This is the only limit
    that applies while the protobuf is decoded.
  - **after parsing, before any tree is built:** the number of samples,
    locations and the stack depth. These are structural and cannot be checked
    until the profile is decoded; the decompressed-byte cap is what bounds that
    decoding.
  - **while reading the sidecar:** its size in bytes, its number of rows and
    its number of distinct execution references, enforced inside the row
    callback so reading stops at the limit. `execlog.Read` streams rows and
    bounds only its header strings (`gpu/execlog/execlog.go:324-426`), so the
    consumer must bound the rest. A sidecar over a limit makes executions
    `unavailable: sidecar exceeds limits`; it never fails the load.
- **Negative values and overflow.** The model bypasses `Fold`, so it repeats
  `Fold`'s guard: a negative value at the selected sample index refuses the
  view (the input is a diff or a malformed profile, not a measurement). All
  accumulation is checked: node cumulative and view totals in `int64`, sidecar
  value sums in `uint64`, and the diff's combined weight (base + new). An
  overflow refuses the operation with the quantity named; it never wraps.
- **Exact integers.** Values in the sample unit and counts are kept as exact
  integers end to end; a floating-point value is used only for ratios. A
  `float64` cannot hold every `int64`, so a fact never carries a large count as
  a float.
- **The executions log is optional and is checked before it is trusted.** If
  one is supplied:
  1. read it with `execlog.Read`, which checks its structure and CRC;
  2. `Trailer.Verify` against the same bytes (digest and aggregated sample
     count);
  3. the header's `SampleType` matches the profile's GPU sample type, and its
     `CaptureID` matches the profile's `capture_id` comment when both exist.
     The header carries the joined string `gpu/nanoseconds`
     (`gpuprofile/profiler.go:200`), so it is compared with
     `SampleType[i].Type + "/" + SampleType[i].Unit`, not with `Type` alone;
  4. **value conservation:** for every aggregated sample, the sum of
     `Row.Value` over its rows equals `Sample.Value` at the GPU index.
  Only a log that passes all four is used. Rows are held provisionally while
  reading and never reach the facts before the checks pass. Each row's
  `Sample` index is bounds-checked against the profile **before** it indexes
  anything: `execlog.Read` checks the sample range against the trailer only
  after all callbacks have run. Otherwise executions are `unavailable`, with
  the reason:
  - `no sidecar` (none was supplied);
  - `empty sidecar` (a file was supplied but holds no bytes);
  - `sidecar exceeds limits`;
  - `digest mismatch`;
  - `sample count mismatch`;
  - `metadata mismatch`;
  - `value mismatch`;
  - `corrupt`.
- **Rows with `Value == 0`** (a PC sample that received no weight,
  `gpu/execlog/execlog.go:70`) still name an execution that was observed. They
  count toward `executions below` for the sample they name, even though a view
  skips zero-weight samples when drawing.
- **No inference.** Never count pprof samples or rows as executions or
  launches, and never multiply by a period.
- **Evidence identity.** Two different things are identified:
  - the **profile digest**, a property of the file;
  - the **evidence handle**, which names an immutable bundle: one profile
    digest plus the exact sidecar selection (no log, or a log identified by
    its own SHA-256). Opening the same profile with and without a log gives
    two different handles. A handle never changes after it is created.
- **A view** is (handle, sample index, collapse options). Two views are equal
  only if all three are equal; the options are compared field by field.
- **Fact identity.** Every fact is tied to a handle and a view. Two facts from
  different views are never compared silently.

### 6.3 Identity

The model is built from `profile.Profile` **before** anything is folded by
name. Three identities are kept apart, because pprof itself keeps them apart:
`pprof/pprof.go:494-530` interns a Location by mapping and offset, while
`555-569` interns a Function by mapping and name.

- **"No mapping"** means `Mapping == nil` or `Mapping.File == ""`. The GPU
  builder and the fallback path put synthetic and unresolved frames on the
  first mapping with an empty file (§4), so testing for `nil` alone would treat
  all of them as one module.
- **Sentinel mappings.** `[kernel]` and `[jit]` are mapping file names, not
  frame names. A frame on them is identified by the sentinel plus the function
  name.
- **Location identity** (one PC, one inline level): the mapping's build-id (or
  its file path when there is no build-id), the file offset, and the inline
  index within the location's `Line` list. Two PCs of the same function are
  two locations.
- **Function identity:** the mapping's build-id (or file path, or sentinel) plus
  the function name. Two PCs of one function are one function. Two functions
  with the same name in different modules are two functions.
  - **No mapping:** the function name plus its `Filename` and `StartLine` when
    the profile has them. Python frames from the unwinder and synthetic frames
    (`[gpu:launch]`, `[gpu:launch unsampled]`, `[gpu:kernel:…]`) are
    identified this way.
  - **Unresolvable collisions are declared, not hidden.** Two different
    functions with the same name, no mapping and no file or start line (two
    `__init__` with nothing else known) cannot be told apart. The model merges
    them and marks the function `collision_possible`, so a consumer knows the
    identity is weaker.
- **Path node identity:** a node is one occurrence of a path prefix of
  **function** identities, one per inline level. Recursion therefore produces
  a distinct node at each depth. The node ID is a short hash of that sequence,
  stable for the same handle and view.
- **Call-site detail.** The locations (PCs) that make up a node are kept on the
  node as its call sites, so source lookup and a future per-line view can use
  them, but they do not split the node.
- **Recursion in aggregates.** `top` and `sandwich` aggregate by function and
  count a function once per sample for cumulative values, the rule pprof uses.
- **IDs in the page:** short IDs like `f9` are display aliases. The contract
  is the node ID plus the handle and view.

### 6.4 Path facts

For a node in a view:

| Fact | Definition | Source |
|---|---|---|
| cumulative | sum of sample values at or below the node | pprof |
| self | sum of values where the node is the leaf | pprof |
| share of total | cumulative / view total; the denominator is named | pprof |
| share of parent | cumulative / parent cumulative; the parent node ID is named | pprof |
| executions below | distinct `Execution` ordinals over the samples under the node, including rows with `Value == 0` | checked execlog only (§6.2), else `unavailable` |
| distinct kernel names below | count of distinct `[gpu:kernel:…]` names observed | pprof; named "observed", not "binary-identical kernels" |
| inexact portion | value under the node from samples matching the existing inexact rules | pprof labels |
| unsampled portion | for GPU views: value under `[gpu:launch unsampled]` at view level, never redistributed | pprof |
| sample period | the `gpu_sample_period` values seen under the node | pprof labels |
| serialization | value under the node by `gpu_serialized`: `true`, `false`, `unknown`, `missing` | pprof labels |
| domain | the result of `flamegraph.Classify`, marked "inferred" | heuristic |

**Rules:**
- **Value and status.** Every fact has a value and a status: `ok`, `zero` or
  `unavailable` (with a reason). Zero and unavailable are distinct in code
  and in output. A share whose denominator is zero is `unavailable` with
  reason `zero total`, never 0 and never NaN.
- **Semantics note.** GPU views always carry the sampling note from
  `gpuSamplingWarnings`, attached to the facts as a structured field. A
  consumer cannot get the share without the note next to it.
- **Units.** Numbers keep their sample-type unit (`gpu/nanoseconds` and so
  on).
- **Rounding policy is the engine's.** Each metric has a display rounding
  fixed by the engine (for example shares to 0.1 percentage point, durations
  to three significant figures). It is reported next to each value and is the
  only tolerance the claim check in §7.3 uses.

### 6.5 Quality

Quality is profile-level and per-node:

- **Joins:**
  - the exact, heuristic and unmatched shares of `gpu_join`;
  - the share with `gpu_ambiguous=true`;
  - the share with `gpu_graph_refused`.
- **Sampling:**
  - the unsampled launch share;
  - the sample period, or periods.
- **Perturbation (`gpu_serialized`):** the shares labelled `true`, `false`,
  `unknown` and the share where the label is `missing`, reported separately.
  `true` means the execution ran inside a serialized sampling window (Tier A
  PC sampling), so its duration is perturbed. A perturbed profile is still
  evidence: it is reported as perturbed, not dropped as unavailable.
- **Symbols:** the share of the view whose stack contains an unresolved frame
  (module plus offset, a bare address, or `[unknown]`) **anywhere** on it, not
  only at the leaf. A GPU stack's leaf is a named kernel, so a leaf-only rule
  would miss an unsymbolized CPU caller beneath it. Reported per module, with
  each sample's weight counted once per module however many of its frames are
  unresolved there. A module is identified unambiguously (build-id, else full
  path, else sentinel), never by file name alone.
- **Identity strength:** the share under functions marked
  `collision_possible` (§6.3).
- **Executions log:** whether it is available, and if not, why.
- **Comparability.** A diff compares the quality of both sides and flags any
  material difference: degraded symbolization or join coverage, a different
  sample period, or a different perturbation picture. With such a difference,
  "path disappeared" or "path got cheaper" is not trustworthy.
- **Perturbation comparison policy (my call).** All four `gpu_serialized`
  states are compared, not only `true`: the sides are not comparable when any
  state's share moves by more than the threshold, or when perturbation is
  known on one side and unknown (`unknown` or `missing`) on the other. A base
  that is entirely `missing` and a new profile that is entirely `false` are
  therefore not comparable, although `true` is zero on both. An `unavailable`
  share (a zero total, a CPU view) never takes part as a numeric zero: when it
  is unavailable on either side, the comparison is reported as unavailable
  with the reason.

### 6.6 Bilateral diff

- **Not a subtracted pprof.** The diff is a model of both profiles, not a
  subtracted pprof; `Fold` correctly refuses negative values.
- **Inputs:** a base view and a new view, each selected independently on its
  own handle. They must have the same sample type and unit and equal view
  options (§6.2); otherwise the diff is refused.
- **Matching**, top-down, per parent:
  1. **exact:** the same function identity (§6.3), which includes the build-id.
     When a mapping has **no build-id** on either side, a match on file path
     and name is never labelled exact, because two different builds at the
     same path would look identical; it is labelled `by_path`.
     `by_path` applies only to frames with a real, non-empty module path. A
     frame with no mapping or on a sentinel mapping never matches by path.
  2. **by name:** the remaining children match on a name key that depends on
     the frame's kind:
     - a frame in a real module: module **file name** (not full path, so a
       module that moved directories still matches) plus function name. This
       is the normal case whenever the code was rebuilt;
     - a frame on a sentinel mapping (`[kernel]`, `[jit]`): sentinel plus name;
     - a frame with no mapping: name plus `Filename` and `StartLine` when the
       profile has them. Two same-named unmapped functions whose file names
       differ never merge, and two same-named unmapped siblings whose file
       names are stable on both sides match one to one rather than becoming
       ambiguous.
  3. **ambiguous:** when, in either direction, one node has several
     candidates on the other side (repeated names, a recursive function
     appearing twice under one parent, two modules with the same file name),
     the nodes are not merged. Each is listed with its candidates and stays
     separate in the union. A candidate is named by side (`base` or `new`),
     handle and node ID, because the same node ID can occur in both views.
  The match label (`exact`, `by_path`, `by_name`, `ambiguous`) is reported per
  node.
- **Presence and interpretation are separate fields:**
  - **presence** is structural: `both`, `only_base` or `only_new`. It is never
    hidden; an `only_base` branch stays in the union and in the table.
  - **interpretation** says whether the change can be read as a change in
    cost: `comparable` or `not_comparable`, with the reason. A node is
    `not_comparable` when its module's symbolization, the join coverage, the
    sample period or the perturbation share differs materially between the
    sides (thresholds are named constants; the defaults are the builder's
    call). Absence alone never proves the cost was eliminated: the path may
    have been renamed, inlined or fused, or may have lost its symbols or its
    stack.
- **Deltas:**
  - the **raw** delta (new minus base, in the sample unit) is always shown;
  - the **share** delta is shown against each side's own total, and is
    `unavailable` when either total is zero;
  - a **per-unit-of-work** delta is shown only when the caller supplies a work
    count for each side, and both counts are positive and finite; otherwise it
    is `unavailable` with the reason. The diff never guesses the work count.
- **Rebuilt code is common, not universal.** In the loop, the changed binary
  is rebuilt, so its build-id changes and its nodes match by name. Python
  code, configuration changes and libraries that were not recompiled keep their
  build-ids, so the same diff mixes exact and by-name matches. Both are main
  paths and both are tested.
- **Union geometry (my call).** Each node of the union has two weights, base
  and new. The drawing uses the **combined weight** base + new: it is additive
  (a node's combined weight equals the sum of its children's combined weights
  plus its combined self), so the icicle never overflows when cost moves
  between branches, and removed or added nodes keep a visible width. The sum
  is checked for overflow. Every node shows both values, base and new.
- **Colour and legend (my call).** Colour shows the **share** delta, and the
  legend says so: "lower / higher share of total <sample type> (<unit>)",
  built from the view's real sample type and unit. It does not say "time": an
  `alloc_space`/bytes or `alloc_objects`/count view is not time. Share and
  absolute change can disagree: with totals 100 → 200 and a node 50 → 60, the
  node's accumulated value rose by 10 while its share fell from 50% to 30%, so
  it is coloured "lower share". The raw delta, with its unit, is shown next to
  every node and in the table, so that case is never hidden. The legend never
  says "faster or slower": accumulated time is neither wall time nor the
  critical path.
- **All-zero diff.** When both totals are zero, the combined root weight is
  zero and there is nothing to divide: the page states that both views are
  empty and draws no icicle; shares and share deltas are `unavailable`.
- **Rendering:** the diff HTML draws that union; `only_base` nodes are
  hatched, `only_new` nodes outlined, `not_comparable` nodes marked. It also
  lists removed and added paths in a table with presence, interpretation,
  reason and raw delta.

### 6.7 Surface in M1

**My call:**
- `cmd/flamegraph` gains flags:
  - `-facts <node-id|path>`, which writes JSON facts;
  - `-diff <base>`, which writes a diff HTML and its JSON;
  - `-execlog <file>` (and `-execlog-base <file>` for the diff) to select the
    sidecar explicitly. A log is never picked up implicitly from a file name.

The existing single-profile HTML is not migrated onto the model in M1. That
migration, and the byte-for-byte equivalence it must prove, is a separate step
(M1b) after the model is reviewed, so M1 cannot change the existing page.

**Builder's call:** the exact flag names and JSON field names.

### 6.8 Fixtures and tests

Fixtures are small synthetic pprof files built in tests, plus one or two real
captures checked in only if they are small. Required cases:

1. **Aggregation:** several executions merged into one sample. With a checked
   execlog, executions are counted exactly. Without one, the value is
   `unavailable: no sidecar`. With a log for a different profile, it is
   `unavailable: digest mismatch`; with a wrong sample type or capture ID,
   `metadata mismatch`; with rows that do not add up to the sample,
   `value mismatch`.
2. **PC sampling:** one execution produces several samples, one row with
   `Value == 0`. Executions are counted once, the zero-value row's execution
   is counted, and rows are never reported as executions.
3. **Recursion:** `a → b → a → b`. Cumulative values are not double counted,
   and nodes at different depths have distinct IDs.
4. **Same name, different modules:** `foo` in `libx.so` and in `liby.so`
   stay separate frames and separate functions.
5. **Several PCs per function:** `foo` at offsets `0x10` and `0x20` in the same
   module is two locations, one function and one node; both PCs are its call
   sites.
6. **Empty mapping:** a GPU profile whose synthetic frames sit on the first
   mapping with an empty file is treated as "no mapping".
7. **JIT and same name without a mapping:** two `__init__` with different
   `Filename`/`StartLine` stay separate; two with nothing else known merge and
   are marked `collision_possible`; a frame on the `[jit]` sentinel is
   identified by sentinel plus name.
8. **Inline frames:** a location with two `Line` entries yields two nodes,
   inlined chain root-first, as `Fold` emits them.
9. **Degraded coverage:** a base with symbols and a new profile where the
   same module is unsymbolized. The base path is `only_base` (presence) and
   `not_comparable` (interpretation), with the module named as the reason.
   Also the GPU ancestor case: base `main → foo → [gpu:kernel:k]`, new
   `main → libfoo.so+0x123 → [gpu:kernel:k]`. The kernel leaf is named on both
   sides, yet the vanished `foo` is `not_comparable`, because `libfoo.so`
   became unresolved on the new side.
10. **Sampling semantics:** a GPU profile with a period of 8. The facts carry
    the note, and no value is scaled.
11. **Inexact joins:** heuristic, unmatched and ambiguous samples are counted
    in the inexact portion.
12. **Perturbation:** samples with `gpu_serialized` `true`, `false`,
    `unknown` and absent are reported as four shares; a diff between a
    perturbed and an unperturbed capture marks the affected nodes
    `not_comparable`, and so do `missing` → `false` and `unknown` → `false`
    with `true` at zero on both sides. A zero-total side makes the comparison
    unavailable, not zero.
13. **Zero vs unavailable:** a fact whose source exists but sums to zero, next
    to one whose source is missing, and a share over a zero total.
14. **Diff matching:** a rebuilt binary (by name), an unchanged library
    (exact), a module without a build-id (`by_path`, never exact), a moved
    module path (by name), an ambiguity in each direction, a same-named
    unmapped function whose file name changed (no merge), and two same-named
    unmapped siblings with stable file names (one-to-one match).
15. **Input limits:** a profile over each limit is refused with the limit
    named; a sidecar over its row or byte limit stops reading at the limit and
    reports `sidecar exceeds limits`.
16. **Arithmetic:** a negative value at the selected index refuses the view;
    accumulation that would overflow `int64`, a combined base + new weight that
    would overflow, and sidecar value sums that would wrap `uint64` are all
    refused or rejected, never wrapped.
17. **Colour semantics:** the 100 → 200 total / 50 → 60 node counterexample is
    coloured by share with the raw +10 shown, and a bytes or count view's
    legend names its own sample type and unit.

**Invariants checked on every fixture:**
- the children's cumulative values plus self equal the parent's cumulative;
- the root equals the view total;
- the diff's base and new totals equal those of the two inputs;
- in the diff union, each node's combined weight equals its children's
  combined weights plus its combined self.

## 7. Milestone 2: MCP server and structured explanations

### 7.1 Server

- **Process:** a separate binary, `perf-agent-mcp` (**my call**; the name is
  the builder's call). It speaks MCP over **stdio only**, is read-only and
  works offline.
- **Never starts the profiler.** It never starts `perfagent.Agent` or any
  profiler, and never needs capabilities.
- **Library:** the builder chooses between the official Go MCP SDK and a
  minimal hand-written JSON-RPC. Any new dependency must pass the
  third-party notices check (`scripts/third-party-notices.sh --check`), and
  its license must be compatible with Apache-2.0.
- **Roots are configured explicitly** at start-up, one flag each:
  `--profiles DIR` (profiles and execlogs, read), `--source DIR` (source, read,
  repeatable) and `--output DIR` (rendered pages, write). There is no default
  output root: without `--output`, `render_flamegraph` is disabled.
- **Root enforcement** uses `os.Root` (Go 1.24+; the chosen primitive, not yet used elsewhere in perf-agent), not a textual prefix check, so
  symlinks, `..` and races cannot escape a root. Paths named by a profile's own
  mappings are subject to the same roots.
- **Render never overwrites inputs.** The output file must not resolve (through
  any alias or symlink) to a profile, a sidecar or a source file the server has
  opened, and must not already exist unless the call says `overwrite` and the
  existing file is a page this server rendered.
- **Limits:** the §6.2 input limits, plus a maximum size for each tool
  response, for explanation text and for a rendered page.

### 7.2 Tools

Every tool takes an explicit evidence `handle` (§6.2) and `view`, never an
implicit "current profile".

| Tool | Returns |
|---|---|
| `open(path, execlog?)` | a new immutable `handle` (profile digest + sidecar selection), the profile digest, sample types, available views, capabilities (execlog checked and why not, PC sampling, source roots), profile-level quality, semantic notes |
| `top(handle, view, by, domain?, limit, cursor?)` | functions or path nodes (the caller says which), with cumulative, self and share. Truncated with `omitted_count` and `omitted_weight` (defined below) and a `cursor` for the tail |
| `path(handle, view, node)` | the root-to-node path with per-frame facts and domain |
| `sandwich(handle, view, function, limit, cursor?)` | callers and callees of a function, with explicit aggregation rules for recursion |
| `gpu_kernels_for(handle, view, node)` | kernels under the node: time, share, and executions (or `unavailable` with a reason) |
| `facts(handle, view, node)` | the §6.4 facts and quality for one node |
| `diff(base_handle, base_view, new_handle, new_view, work_base?, work_new?, limit, cursor?)` | the §6.6 diff, truncated like `top` |
| `source(handle, view, node)` | source lines for the node's call sites, only within the source roots and only with source evidence (below); otherwise `unresolved` or `source_unverified`, with the reason |
| `render_flamegraph(handle, view, explanations, out, attempt?)` | see §7.3 |

**Rules for all tools:**
- **Truncation:** results are truncated by weight. `omitted_count` is the
  number of rows not returned. `omitted_weight` is the **sum of the omitted
  rows' own values** and is labelled that way: rows of `top(by=cum)` overlap
  (nested functions and paths share samples), so it is not "the rest of the
  total" and is never shown as a remaining percentage.
- **Cursors** are bound to the handle, the view and the query parameters, and
  the row order is stable (weight, then node or function ID). A cursor used
  with anything else is rejected.
- **Errors:** messages are prescriptive. They say what to call next, for
  example "node not in view; call `path` with …".
- **Agent guide:** a short guide for the agent, about 3k tokens, shipped as an
  MCP prompt or resource. It sets the investigation order (open → top →
  path / gpu_kernels_for → facts → render) and repeats the semantic rules.

**Source evidence.** A build-id proves which binary ran, not that a local
`.cc` or `.py` file is the one it was built from. `source` reports one of
three statuses:
- **`build_verified`:** there is provenance that ties the profiled binary
  (its build-id, or the profile itself) to the source revision: a source
  manifest (path → SHA-256) recorded with that build and matching the file,
  or a build record that names the commit the binary with that build-id was
  built from. Only this status claims the lines are the ones that ran.
- **`user_selected`:** the source root is a checkout the user named when
  starting the server, the file is unmodified there, but nothing ties that
  revision to the binary. An unrelated binary and an arbitrary clean commit
  satisfy this, so it is reported as the user's choice, not as verified. It
  is still useful, with that status shown.
- **`source_unverified`:** neither holds.
`unresolved` is returned when no file matches. `gpu_src_file` from PC sampling is a basename, not a path; it
is looked up inside the source roots and an ambiguous basename is reported as
ambiguous, never resolved by guess.

### 7.3 Structured explanations and the render check

What is guaranteed is narrow and stated: **only structured claims are
checked.** Prose, hypotheses and next steps are not, and the page says so.

An explanation for one node has these parts:

```json
{
  "node": "<node id>",
  "claims": [
    {"id": "c1", "metric": "share_of_total", "node": "<node id>",
     "denominator": "view_total", "value": 0.411}
  ],
  "prose": "Text that references claims as {c1} and nodes as {n:<node id>}.",
  "hypotheses": ["Text, rendered as a hypothesis, never as a finding."],
  "next_measurement": "Text."
}
```

**Numbers in free text (my call).** A digit sequence in prose, hypotheses or
next-measurement text that is not inside a `{cN}` placeholder is **not
rejected**; it is rendered visibly marked as an unchecked figure. Rejecting
would push agents to spell numbers out; marking keeps them honest and visible.
The header counts only checked figures.

**Validation before anything is written:**
- claim IDs are unique within the call, and every `{cN}` and `{n:…}` reference
  resolves;
- every referenced node exists in the handle and view;
- every claim's metric is a known fact for that node, with the claim's unit
  and type (a share is a ratio, a duration has the view's unit);
- the claim's `denominator` is the one that fact actually uses (for
  `share_of_parent`, the node's real parent), not merely a valid name;
- the fact is available; a claim against an `unavailable` fact is rejected
  with the fact's reason;
- the value is finite, and inside the metric's range (shares in [0, 1],
  counts and durations ≥ 0);
- the value equals the engine's value **at the engine's rounding for that
  metric** (§6.4), compared as decimals. The agent does not supply a
  tolerance and cannot widen it.

**Rendering:**
- The engine renders every checked number. Claim placeholders are replaced by
  the engine's canonical value, never the agent's.
- Prose is rendered as untrusted text. All prose, frame names and paths are
  HTML-escaped, and no agent-supplied URL becomes a link.

**Failure and attempts:**
- The tool returns every failing claim and reference, with the engine's
  values, so the agent can correct and retry.
- Every call gets an **attempt ID** recorded by the server, bound to the
  handle and view. A retry passes the previous attempt ID. The page's
  "rejected at render" section is built from the server's own record of the
  rejected attempts for that handle and view, not from anything the agent
  says about earlier attempts.
- A claim check does not validate causality. Hypotheses stay hypotheses.

**Writing:**
- The output goes to a temporary file in the destination directory and is
  renamed into place, inside the output root (§7.1).

**Tests (deterministic, no model):** a numeric literal without a claim (marked
unchecked), a correct value with the wrong parent as denominator (rejected), a
claim against an unavailable fact (rejected), NaN and ±Inf values (rejected),
an attempt to pass a rounding or tolerance (ignored; the engine's policy
applies), duplicate claim IDs and unresolved references (rejected).

The page labels the parts honestly:
- **Profile facts:** computed by perf-agent.
- **Agent interpretation:** prose, not verified; unchecked figures marked.
- **Hypotheses:** shown as hypotheses.
- **Header:** "N figures and references checked". It never says "verified
  explanation".

### 7.4 HTML

The page extends the existing renderer and keeps its domains and palette. It
follows mock screen 1:
- paths that have explanations are marked;
- selecting a frame changes the frame name, facts, path and explanation
  **together**;
- frames with no explanation show facts only;
- follow-up chips copy a prompt that names the profile digest and node ID,
  ready to paste into the agent;
- the sampling note and the quality summary are always visible.

## 8. Milestone 3: the autonomous loop

The loop is a separate client. It can be a skill or script driven by an agent
plus a small helper binary. The builder decides the split. It uses the M1
model and M2 tools.

### Setup, fixed before the first iteration and recorded in the report

- **Target:** the node or function to improve.
- **Benchmark:**
  - the command, its inputs and the working directory;
  - the environment, the dependency lockfile and the toolchain versions;
  - for each of these, a hash.
- **Measurement protocol:**
  - warm-up iterations;
  - device synchronization for GPU work, such as `cuda.synchronize()` per
    iteration;
  - iteration count;
  - the reported unit of work.
- **Correctness:**
  - the test command;
  - a numeric tolerance for the workload's outputs against baseline, because
    passing tests do not prove numerical equivalence.
- **Acceptance criterion:**
  - a minimum improvement;
  - a confidence level, with the method named (for example a bootstrap CI on
    paired differences);
  - the number of A/B pairs.
- **Limits:**
  - maximum iterations, maximum wall time, and a scope of files the agent may
    change;
  - all work happens in a dedicated git worktree.

Defaults (**my call**, all configurable):
- minimum gain of 3%;
- a 95% bootstrap CI on 10 interleaved pairs;
- a maximum of 5 iterations;
- a maximum of 2 hours.

The loop's PR goes to the repository that owns the benchmarked code. That may
not be perf-agent.

### Measuring

- **Flaky tests:** the test command runs N times before starting, and the
  report says "0 failures observed in N runs", not "no flaky tests".
- **Benchmark without the profiler:** baseline and candidate are measured
  **interleaved** in balanced order (ABBA…).
- **Explanatory profiles** are captured separately, and only their coverage
  (symbols, joins, sample period, perturbation) is compared. A candidate whose profile
  coverage differs materially from the baseline is not judged by the profile
  diff.
- **Accepting a candidate:**
  - the **lower bound** of its confidence interval for the improvement is
    above the minimum gain (a point estimate above 3% with an interval that
    includes 0 does not pass);
  - the target path's diff agrees;
  - tests pass;
  - outputs stay within tolerance;
  - the benchmark files are unchanged (hash check).
- **Fresh confirmation:** the selected winner is re-measured once, from
  scratch, with the same protocol and criterion. A winner that fails
  re-measurement is reported as such and not proposed; it is not re-measured
  again until it passes.
- **Keep everything:** failed and rejected attempts stay in the report with
  their reason.

### Output

- **The report:**
  - the setup, the per-iteration table (paired results with intervals) and
    the diffs;
  - the explained HTML for the baseline and for the winner;
  - the command to reproduce the measurement.
  It follows mock screen 3.
- **The PR:** a draft PR containing only the winning change, with the report
  attached. The loop stops there. It never merges, never pushes to the
  default branch and never marks the PR ready unless the user explicitly
  asks.

## 9. Constraints

- **Language and style:**
  - Go 1.26;
  - follow the modern-go guidelines;
  - tests with `-race`;
  - `golangci-lint` v2.11.4, as in CI.
- **No network** from the model, server or renderer.
- **Licenses:** new dependencies must be added to the third-party notices.
- **No regressions:** existing CLI behavior and output formats do not change
  unless a change is stated in the CHANGELOG.
- **Process:** commits and PRs follow the repository's POLICY (no session
  URLs or AI attribution). Work is iterated in the draft PR; nothing is
  merged without the user's explicit approval.

## 10. Testing the AI parts

- **Without a model:** the render check, the claim validation and every tool
  are tested deterministically. The tests use hand-written explanations,
  both correct and wrong, against fixtures.
- **With a model:** evals in the style of Polar Signals (questions replayed
  over frozen fixtures, with checks that the rendered claims validate) are a
  local, manual suite. They are not CI, because they need a model.

## 11. Left to the builder

- **Names:** package, binary, flag and JSON field names.
- **Node ID details:** the hash function and length.
- **MCP plumbing:** SDK vs hand-written JSON-RPC (§7.1).
- **Diff by-name fallback:** whether it matches through inlined frames
  (the matching levels themselves are fixed in §6.6).
- **Defaults:** input limits (§6.2) and the comparability thresholds (§6.6),
  as named, configurable constants.
- **Loop packaging:** a skill plus a helper binary, or a single binary.
- **Checked-in data:** which real captures, if any, are small enough to
  commit as fixtures.

## 12. Open questions for the user

Until these are answered, the calls marked "my call" stand.

1. **May the loop run benchmarks unattended on the GPU lab machine?** GPU
   workloads there can hang (the CUDA hang under host WiFi traffic), and the
   loop runs many A/B pairs. The answer decides whether M3 needs a
   supervisor, a hang watchdog and a "pause when the network is busy" rule.
   Until then, the loop runs only when the user starts it, and it stops on the
   first hang.
2. **Should `perf-agent-mcp` ship in releases and images, or stay
   build-from-source until M2 settles?** The answer decides the release and
   notices work in M2. Until then: source only.

## Appendix A: builder check

The design was read as a builder who could not ask the authors. Questions
that came up and how they were settled:

- **Answered from the conversation or the code; now in the document:**
  - which sample types are in scope: all three, CPU, off-CPU and GPU (§6.4);
  - which repository the loop's PR targets (§8);
  - whether the existing HTML may change (§6.7).
- **Minor; settled as my call:**
  - identity of frames without a mapping (§6.3);
  - by-name matching as the main path in the loop (§6.6);
  - loop defaults (§8);
  - how the AI parts are tested (§10).
- **Theirs:** the two questions in §12.

## Appendix B: changes after review #271

- `gpu_serialized` is emitted (four states, including `missing`) and feeds
  quality and comparability (§4, §6.4, §6.5, §6.6).
- Location, function and path-node identity are separate; "no mapping"
  includes the empty first mapping; sentinel mappings and unresolvable
  collisions are explicit (§6.3).
- Only structured claims are checked; numeric literals in free text are marked
  unchecked; denominator, unit, availability, range and engine-owned rounding
  are validated; attempts are recorded by the server (§7.3).
- Evidence handle vs profile digest; independent base and new views; cursors
  bound to handle, view and query; explicit output root enforced with
  `os.Root`; render never overwrites inputs (§6.2, §7.1, §7.2).
- Diff: `by_path` for missing build-ids, ambiguity in both directions,
  presence separate from interpretation, additive union geometry, deltas with
  explicit availability (§6.6).
- Source needs a manifest or a named checkout, else `source_unverified`;
  input and output limits (§6.2, §7.1, §7.2). Refined after review #274:
  a named checkout alone is `user_selected`, only build provenance is
  `build_verified`.
- Execlog: metadata and value-conservation checks, `Value == 0` rows count as
  observed executions (§6.2).
- Loop: accept only when the CI lower bound clears the minimum gain;
  re-measure the winner once (§8).

## Appendix C: changes after review #274

- Symbol quality counts unresolved frames anywhere on a stack, weighted once
  per module, with unambiguous module identity; the GPU ancestor case is a
  fixture (§6.5, §6.8).
- Perturbation comparison uses all four `gpu_serialized` states and treats
  unavailable as unavailable, never zero (§6.5).
- `by_path` only for real module paths; sentinel and unmapped frames have
  their own name keys; ambiguous candidates carry side and handle (§6.6).
- The diff legend describes share, uses the view's real sample type and unit,
  shows the raw delta, and defines the all-zero case (§6.6).
- Limits: during-parse vs after-parse limits stated precisely; sidecar byte,
  row and reference limits enforced while reading; row sample index
  bounds-checked first; `empty sidecar` and `sidecar exceeds limits` reasons
  (§6.2).
- Negative values refused; checked arithmetic; exact integers for values and
  counts (§6.2, §6.8).
- Source status splits `user_selected` from `build_verified` (§7.2).
- The execlog header's joined `gpu/nanoseconds` sample type is compared
  against `Type + "/" + Unit` (§6.2).
