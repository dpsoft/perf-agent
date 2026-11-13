# AI-explained flame graphs and profile-driven optimization: design

**Status:** design, not implemented. Iterated locally before any draft PR.
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
- **Open space:** no existing tool explains a single selected path, and none
  attributes GPU kernels to the CPU launch stack. perf-agent can do both.

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
4. **Local first.** Work is iterated locally and shown to the user before any
   draft PR is opened.
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
  aggregates samples by the concatenated frame **names** (`fold.go:352`).
- Location and function identity is lost at that point.
- Label summaries are kept only globally, not per path.
- It refuses negative values, that is, diff profiles (`fold.go:310-316`).
- The renderer also builds its tree keyed by name
  (`internal/flamegraph/render.go:268-300`).

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
  `pprof/pprof.go:104`), with no mapping.
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

- `gpu_serialized` is **not emitted** in this tree (`gpu/projection.go:83-87`).
  Quality must report it as unavailable, never as false.
- `gpu_graph_refused` is emitted (`gpu/projection.go:765`).
- CPU and off-CPU profiles carry a mapping path, a GNU build-id and file
  offsets for every location (`docs/output.md`).

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

### 6.2 Loading and provenance

- **Load a profile** from a `.pb.gz` or `.pb` file. Keep:
  - its SHA-256 over the raw bytes as read, which is the **profile digest**;
  - its sample types;
  - its comments, including `perf-agent.capture_id=`;
  - its mappings.
- **The executions log is optional.** If one is supplied, read it with
  `execlog.Read` and accept it only if `Trailer.Verify` passes against the
  same bytes. Otherwise mark executions as `unavailable`, with the reason:
  - `no sidecar`;
  - `digest mismatch`;
  - `sample count mismatch`;
  - `corrupt`.
- **No inference.** Never count pprof samples or rows as executions or
  launches, and never multiply by a period.
- **A view** is (digest, sample index, collapse options), for example the raw
  vendor frames switch that already exists in `cmd/flamegraph`.
- **Fact identity.** Every fact is tied to a profile digest and a view. Two
  facts from different views are never compared silently.

### 6.3 Identity

The model is built from `profile.Profile` **before** anything is folded by
name.

- **Frame identity:**
  - For a location with a mapping: the mapping's build-id (or path when there
    is no build-id), the function name, and the file offset when present.
  - For synthetic frames (`[gpu:launch]`, `[gpu:launch unsampled]`,
    `[gpu:kernel:…]`, `[kernel]`, `[jit]`): a kind plus the name.
  - Two functions with the same name in different modules are different
    frames.
- **Path identity:** the root-to-leaf sequence of frame identities.
- **Node ID:**
  - A node is one occurrence of a path prefix, so recursion produces distinct
    nodes for each depth.
  - The node ID is a short hash of the path identity, stable for the same
    profile and view.
  - It is also the key for matching across profiles (§6.6).
- **Frames without a mapping:** frames such as interpreter frames from the
  Python unwinder are identified by kind plus name, like the synthetic
  frames.
- **Function identity:** the aggregate of all nodes for one frame identity.
  It is used by `top` and `sandwich`. Recursion is counted once per sample
  for cumulative values, the same rule pprof uses.
- **IDs in the page:** short IDs like `f9` in the page are display aliases.
  They are never the contract. The contract is the node ID plus the digest.

### 6.4 Path facts

For a node in a view:

| Fact | Definition | Source |
|---|---|---|
| cumulative | sum of sample values at or below the node | pprof |
| self | sum of values where the node is the leaf | pprof |
| share of total | cumulative / view total; the denominator is named | pprof |
| share of parent | cumulative / parent cumulative; the parent node ID is named | pprof |
| executions below | distinct `Execution` ordinals over the samples under the node | verified execlog only, else `unavailable` |
| distinct kernel names below | count of distinct `[gpu:kernel:…]` names observed | pprof; named "observed", not "binary-identical kernels" |
| inexact portion | value under the node from samples matching the existing inexact rules | pprof labels |
| unsampled portion | for GPU views: value under `[gpu:launch unsampled]` at view level, never redistributed | pprof |
| sample period | the `gpu_sample_period` values seen under the node | pprof labels |
| domain | the result of `flamegraph.Classify`, marked "inferred" | heuristic |

**Rules:**
- **Value and status.** Every fact has a value and a status: `ok`, `zero` or
  `unavailable` (with a reason). Zero and unavailable are distinct in code
  and in output.
- **Semantics note.** GPU views always carry the sampling note from
  `gpuSamplingWarnings`, attached to the facts as a structured field. A
  consumer cannot get the share without the note next to it.
- **Units.** Numbers keep their sample-type unit (`gpu/nanoseconds` and so
  on). Rounding for display is declared next to each value.

### 6.5 Quality

Quality is profile-level and per-node:

- **Joins:**
  - the exact, heuristic and unmatched shares of `gpu_join`;
  - the share with `gpu_ambiguous=true`;
  - the share with `gpu_graph_refused`.
- **Sampling:**
  - the unsampled launch share;
  - the sample period, or periods.
- **`gpu_serialized`:** `unavailable` in this tree (§4).
- **Symbols:** the share in unsymbolized frames (module plus offset, or a bare
  address), by frame resolution.
- **Executions log:** whether it is available and verified.
- **Comparability.** A diff compares the quality of both sides and flags any
  material difference. For example, if symbolization or join coverage
  degraded between the two profiles, "path disappeared" is not trustworthy.

### 6.6 Bilateral diff

- **Not a subtracted pprof.** The diff is a model of both profiles, not a
  subtracted pprof; `Fold` correctly refuses negative values.
- **Matching:**
  - Nodes match by path identity.
  - When build-ids differ (a rebuilt binary), they fall back to matching by
    module path plus function-name path. Such a match is labelled `by name`.
  - A many-to-one fallback is reported as `ambiguous`; it is never resolved
    silently.
- **Classification:** each node in the union is `both`, `only base` (removed)
  or `only new` (added). A removed node is reported together with the
  comparability of its side: was its module still symbolized, and was join
  coverage similar? Absence alone does not prove the cost was eliminated.
  The path may have been renamed, inlined or fused, or may have lost its
  symbols or its stack.
- **Normalization:**
  - Absolute deltas are always shown.
  - Share deltas are shown against each side's own total.
  - Per-unit-of-work deltas are shown only when the caller supplies the work
    count for each side, such as iterations from the loop (§8).
  - The diff never guesses the work count.
- **Matching in the loop:** in the loop every candidate is a rebuilt binary,
  so the build-ids always differ there. Matching by name is the normal case,
  not the exception, and it must be tested as the main path.
- **Incompatible views:** the diff refuses views with different sample types
  or units.
- **Rendering:** the diff HTML draws the **union** of both trees, so removed
  paths stay visible (hatched). It also lists removed and added paths in a
  table. Colour means "less or more accumulated GPU time" (or CPU time). It
  never means "faster or slower", because accumulated GPU time is neither
  wall time nor the critical path.

### 6.7 Surface in M1

**My call:**
- `cmd/flamegraph` gains two flags:
  - `-facts <node-id|path>`, which writes JSON facts;
  - `-diff <base>`, which writes a diff HTML and its JSON.
- The renderer reads its numbers from the model.

The existing HTML output for a single profile must stay byte-for-byte
equivalent except where the model fixes a known name-collision bug. Any such
change is listed in the PR.

**Builder's call:** the exact flag names and JSON field names.

### 6.8 Fixtures and tests

Fixtures are small synthetic pprof files built in tests, plus one or two real
captures checked in only if they are small. Required cases:

1. **Aggregation:** several executions merged into one sample. With a
   verified execlog, executions are counted exactly. Without one, the value is
   `unavailable`. With a log for a different profile, it is
   `unavailable: digest mismatch`.
2. **PC sampling:** one execution produces several samples. Executions are
   counted once, and rows are never reported as executions.
3. **Recursion:** `a → b → a → b`. Cumulative values are not double counted,
   and nodes at different depths have distinct IDs.
4. **Same name, different modules:** `foo` in `libx.so` and in `liby.so`
   stay separate frames and separate functions.
5. **Degraded coverage:** a base with symbols and a new profile where the
   same module is unsymbolized. The diff flags the result as not comparable
   instead of reporting the path as "removed".
6. **Sampling semantics:** a GPU profile with a period of 8. The facts carry
   the note, and no value is scaled.
7. **Inexact joins:** heuristic, unmatched and ambiguous samples are counted
   in the inexact portion.
8. **Zero vs unavailable:** a fact whose source exists but sums to zero, next
   to one whose source is missing.

**Invariants checked on every fixture:**
- the children's cumulative values plus self equal the parent's cumulative;
- the root equals the view total;
- the diff's base and new totals equal those of the two inputs.

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
- **Allowed roots:** the server is started with explicit allowed roots for
  profiles, the execlog and source code, for example
  `--profiles DIR --source DIR`. It refuses paths outside them, including
  paths that the profile's own mappings name.

### 7.2 Tools

Every tool takes an explicit `handle` and `view`, never an implicit "current
profile".

| Tool | Returns |
|---|---|
| `open(path, execlog?)` | `handle` (= digest), sample types, available views, capabilities (execlog verified?, PC sampling?, source roots), profile-level quality, semantic notes |
| `top(handle, view, by, domain?, limit, cursor?)` | functions or path nodes (the caller says which), with cumulative, self and share. Truncated with `omitted_count`, `omitted_weight` and a `cursor` for the tail |
| `path(handle, view, node)` | the root-to-node path with per-frame facts and domain |
| `sandwich(handle, view, function, limit, cursor?)` | callers and callees of a function, with explicit aggregation rules for recursion |
| `gpu_kernels_for(handle, view, node)` | kernels under the node: time, share, and executions (or `unavailable` with a reason) |
| `facts(handle, view, node)` | the §6.4 facts and quality for one node |
| `diff(base_handle, new_handle, view, work_base?, work_new?, limit, cursor?)` | the §6.6 diff, truncated like `top` |
| `source(handle, node)` | source lines for the node, only within the source roots and only when the build matches; otherwise `unresolved` with the reason |
| `render_flamegraph(handle, view, explanations, out)` | see §7.3 |

**Rules for all tools:**
- **Truncation:** results are truncated by weight. Every truncation states
  how much was omitted, and a cursor reaches the rest.
- **Errors:** messages are prescriptive. They say what to call next, for
  example "node not in view; call `path` with …".
- **Agent guide:** a short guide for the agent, about 3k tokens, shipped as an
  MCP prompt or resource. It sets the investigation order (open → top →
  path / gpu_kernels_for → facts → render) and repeats the semantic rules.

### 7.3 Structured explanations and the render check

An explanation for one node has these parts:

```json
{
  "node": "<node id>",
  "claims": [
    {"id": "c1", "metric": "share_of_total", "node": "<node id>",
     "denominator": "view_total", "value": 0.411, "rounding": 0.001}
  ],
  "prose": "Text that may reference claims as {c1} and nodes as {n:<node id>}.",
  "hypotheses": ["Text, rendered as a hypothesis, never as a finding."],
  "next_measurement": "Text."
}
```

**Validation before anything is written:**
- every referenced node exists in the handle and view;
- every claim's metric is a known fact for that node;
- every stated value equals the engine's value within the **declared
  rounding** of the rendered figure, with no blanket tolerance.

**Rendering:**
- The engine renders every number in the page. Claim placeholders in the
  prose are replaced by the engine's value, not the agent's.
- Prose is rendered as untrusted text. All prose, frame names and paths are
  HTML-escaped, and no agent-supplied URL becomes a link.

**Failure:**
- The tool returns every failing claim and node, with the engine's values,
  so the agent can correct and retry.
- Rejected claims and their corrections are recorded in the page's
  "rejected at render" section.

**Writing:**
- The output goes to a temporary file in the destination directory and is
  renamed into place.
- The destination must be inside an allowed output root.

The page labels the parts honestly:
- **Profile facts:** computed by perf-agent.
- **Agent interpretation:** prose, not verified.
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
  (symbols, joins, sample period) is compared. A candidate whose profile
  coverage differs materially from the baseline is not judged by the profile
  diff.
- **Accepting a candidate:**
  - its paired result meets the criterion;
  - the target path's diff agrees;
  - tests pass;
  - outputs stay within tolerance;
  - the benchmark files are unchanged (hash check).
- **Fresh confirmation:** the selected winner is re-measured from scratch
  before the PR. A winner that fails re-measurement is reported as such and
  not proposed.
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
  URLs or AI attribution). Work stays local until the user asks for a draft
  PR.

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
- **Diff by-name fallback:** how far it goes when build-ids differ (for
  example, whether to match through inlined frames).
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
