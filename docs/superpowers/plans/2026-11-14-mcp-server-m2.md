# MCP Server and Structured Explanations (M2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A read-only, offline, stdio-only MCP server, `perf-agent-mcp`, that answers path, function, kernel, diff and source queries over the M1 profile model and renders an explained flame graph whose every number comes from a structured claim the engine has checked.

**Architecture:** `internal/profilemodel` gains the few accessors M2 needs (structured rounding, function aggregation, kernels, a flat diff, source refs, loading through an `fs.FS`). A hand-written JSON-RPC 2.0 layer, `internal/mcpstdio`, speaks MCP over stdin/stdout. `internal/perfmcp` is the thin tool adapter: roots confined with `os.Root`, a handle store with limits fixed per server, cursors, and a wire format that carries every integer as a decimal string. `internal/explain` validates structured claims against the engine; `internal/flamegraph` renders a new explained page; `internal/sourceroot` resolves source files and their evidence status. `cmd/perf-agent-mcp` wires flags to the server.

**Tech Stack:** Go 1.26 standard library (`os.Root`, `math/big`, `encoding/json`, `html/template`, `os/exec` for read-only `git rev-parse`), `github.com/google/pprof/profile` (already a dependency). No new module.

**Spec:** `docs/superpowers/specs/2026-11-13-ai-flamegraph-design.md` (§7 is what M2 builds; §2, §3, §9, §10, §11, §12 bind it). M1 plan and ledger: `docs/superpowers/plans/2026-11-13-profile-model-m1.md`, `/home/diego/github/perf-agent/.worktrees/ai-flamegraph-design/.superpowers/sdd/2026-11-13-profile-model-m1/progress.md`.

## Global Constraints

- Go 1.26; invoke the modern-go guidelines skill before writing Go (`new(v)`, `errors.AsType`, `t.Context()`, `omitzero`, `wg.Go`, `strings.SplitSeq`, `slices`/`maps`/`cmp`).
- Tests run with `-race`; lint with `golangci-lint` v2.11.4 (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`; a missing binary exits 127 and is not a pass).
- Packages that import blazesym need cgo with the static library only. For whole-repo commands export `CGO_CFLAGS=-I/home/diego/github/blazesym/capi/include` and `CGO_LDFLAGS=-L/home/diego/github/perf-agent/.worktrees/ai-flamegraph-design/.superpowers/sdd/2026-11-13-profile-model-m1/blazesym-lib`. The M2 packages themselves do not import blazesym.
- "No network from the model, server or renderer." The server binary links none of `net`, `net/http`, `crypto/tls` (checked by a test in Task 14; `net/url` and `net/netip`, which `html/template` pulls in, open nothing).
- "It never starts `perfagent.Agent` or any profiler, and never needs capabilities." The binary links neither `github.com/dpsoft/perf-agent/perfagent` nor `github.com/cilium/ebpf` (same test).
- "It speaks MCP over **stdio only**, is read-only and works offline." Stdout carries only JSON-RPC messages; logs go to stderr. The only writes are pages under `--output`.
- "Root enforcement uses `os.Root` ... not a textual prefix check, so symlinks, `..` and races cannot escape a root."
- No new third-party dependency (the MCP layer is hand-written); `scripts/third-party-notices.sh --check` stays green without regenerating.
- "Values in the sample unit and counts are kept as exact integers end to end." In MCP JSON every such integer (and every count or weight) is a JSON **string** of decimal digits; a test carries 9007199254740993 (2^53+1).
- "Zero" and "unavailable" are never interchangeable: a wire fact has `status` `ok`, `zero` or `unavailable`; an unavailable fact has no `value` and always a `reason`.
- Never count pprof samples or execlog rows as executions or launches; never multiply by `gpu_sample_period`. Every response about a GPU view carries the view's sampling `notes`.
- `UnsampledPortion` and `Notes` are view-level; no claim may state them about a node.
- A Handle identifies evidence, not validation policy: each server's `Limits` are fixed at construction and every cache is per server; there is no process-wide handle cache.
- "Rounding policy is the engine's." Agents cannot pass or widen a tolerance; claims are compared as decimals at `profilemodel.PolicyFor(metric)`.
- Existing outputs do not change: `RenderHTML`, `RenderFragment` and `WriteDiffPage` stay byte-identical (golden tests from Task 1), `-facts`/`-list-nodes` JSON is unchanged. The one stated change is the unreleased `-diff` JSON, which becomes a flat node list (Task 4, CHANGELOG).
- `perf-agent-mcp` is source-only (spec §12, question 2 default): it is not added to the Makefile `build` target, Dockerfiles, `.github/workflows/release.yml` or the binary list in `scripts/third-party-notices.sh`.
- Comments say what the code does and why; no issue numbers or release history in comments.
- Commits: no Co-Authored-By, no AI attribution, no session URLs; commit subjects use the package prefix (`profilemodel: ...`, `perfmcp: ...`). Push only in Task 16, to `ai-mcp-m2`; never merge.

## Review Focus

1. **Agents pass absolute paths, `..` and symlinks.** An agent holds absolute paths from its shell. An absolute path inside `--profiles` must work; `..`, an absolute path elsewhere and a symlink pointing out of the root must be refused with the root named. Test in Task 6.
2. **Current clients speak protocol 2026-07-28 and probe `server/discover` first.** The official Go SDK v1.8.0 client does, and falls back to `initialize` on any non-modern error. `server/discover` must answer `-32601` and the following `initialize` must succeed at `2025-11-25`. Test in Task 5; real-client smoke in Task 16.
3. **Stacks deeper than 512 frames** (recursion, Python). Nested JSON broke decoders at depth 257 in M1, and HTML parsers stop nesting elements around depth 512. The MCP diff and the CLI diff JSON are flat, and the explained page draws frames as flat positioned elements. Tests in Task 4 and Task 10.
4. **The same directory for `--profiles` and `--output`.** Rendering onto an opened profile's name, a hard link to it or a symlink must be refused; a new name in the same directory must work. Test in Task 11.
5. **Frame names and agent text that look like placeholders or markup** (`operator{}`, `{c1}` inside a symbol, `</script>`, `javascript:` URLs). Frame names are never parsed for placeholders; all agent text and names are escaped; nothing the agent wrote becomes a link. Tests in Task 9 and Task 10.

## Builder's calls settled here (spec §11)

| Call | Decision | Reason |
|---|---|---|
| MCP plumbing | Hand-written JSON-RPC 2.0 over newline-delimited stdio in `internal/mcpstdio`, not `github.com/modelcontextprotocol/go-sdk` | The SDK's latest release is v1.8.0 (2026-09-04, go 1.25; LICENSE mid-transition from MIT to Apache-2.0, compatible). Its `mcp` package links `net/http`, `golang.org/x/oauth2`, `golang-jwt`, `segmentio/encoding`, `jsonschema-go`, `uritemplate` and `x/tools`: eight new modules for a server that needs `initialize`, `ping`, `tools/*`, `prompts/*` and `resources/*` over stdio. Hand-written, the notices stay untouched and Task 14 can *prove* the binary links no networking package, which turns the "leaking data" anti-goal into a test. The cost is tracking protocol revisions; it is bounded by negotiating `2025-11-25`/`2025-06-18` and answering `server/discover` with method-not-found, which SDK v1.8.0 clients treat as "fall back to `initialize`" (`mcp/client.go`). Task 16 checks conformance against that SDK client from a throwaway module outside the repository. |
| Binary | `perf-agent-mcp`, at `cmd/perf-agent-mcp` | The spec's suggestion; matches `cmd/flamegraph`. Source-only per §12. |
| Package layout | `internal/mcpstdio` (protocol, no perf-agent knowledge), `internal/perfmcp` (tools, store, wire, cursors, guide), `internal/explain` (claims and text tokens), `internal/sourceroot` (source resolution and evidence), `internal/pmfixture` (importable fixture builders for tests), additions to `internal/profilemodel` and `internal/flamegraph` | Each has one responsibility and its own tests; `explain` and `sourceroot` are pure and testable without a server. `pmfixture` imports no `profilemodel` so profilemodel's own tests can use it. |
| Explained page | A **new page**, `flamegraph.WriteExplainedPage`, with its own template and script; not an opt-in path in `RenderHTML` | `RenderHTML` draws a name-keyed `foldedstacks` tree; explanations are keyed by model `NodeID`s. A new page reuses `paletteCSS`, the domain keys and the flat frame geometry, and leaves `RenderHTML` and `WriteDiffPage` byte-identical until M1b migrates them. |
| Rounding | `profilemodel.Rounding` (kind + digits) and `PolicyFor(metric)`; `RoundingFor` keeps its strings | No string parsing in the claim check; JSON unchanged. |
| Integers on the wire | Decimal strings for every value, count and weight; ratios as the canonical decimal at the engine's rounding (`"0.411"`) | Lossless in JS clients; an agent copying the shown ratio into a claim passes the check. |
| Tool results | One `text` content block holding compact JSON; no `structuredContent`/`outputSchema` | One representation, so the response limit counts it once; every client reads text. |
| Diff flattening | `profilemodel.(*Diff).Flatten` shared by MCP and the CLI `-diff` JSON | One flat format; removes the nesting problem everywhere. |
| Source evidence | `build_verified` from a `--source-manifest` file; `user_selected` from read-only `git rev-parse` in the source root (bytes hash to the HEAD blob); else `source_unverified` | Implements the three statuses with no filter drivers or index writes (no `git status`). `--source-git=false` turns the probe off. |
| Request handling | Sequential, one goroutine | Deterministic; no locks in the store. A long `open` delays later requests, which a single-client stdio server can accept. |
| Limits | All in `perfmcp.Limits` (wrapping `profilemodel.Limits`, `explain.Limits`, `sourceroot.Limits`), set by one repeatable `--limit Name=Value` flag | Every default is a named, documented field; immutable per server (R10). |

## Wire contract (shared by Tasks 6-13)

- **Fact:** `{"value": "<decimal>", "unit": "...", "status": "ok|zero|unavailable", "reason": "...", "denominator": "...", "rounding": "..."}`. `value` is `Fact.Canonical()`: an exact integer for sample-unit values and counts, the ratio rounded at its policy for shares, a 3-significant-figure float for derived per-unit deltas. Absent exactly when unavailable.
- **View name:** `"<sample type>/<unit>#<index>"`, the form `PathFacts.View` already uses; a tool's `view` argument must equal one of the names `open` returned.
- **Truncated lists:** `{"rows": [...], "total_rows": "<n>", "skipped": "<offset>", "omitted_count": "<n>", "omitted_weight": "<decimal>", "omitted_weight_basis": "<what>", "next_cursor": "c1...."}`. `omitted_*` cover the rows after this page; `omitted_weight` is "the sum of the omitted rows' own values" and the field `omitted_weight_basis` says which value (`"cum"`, `"self"`, `"abs_delta"`, `"value"`); the guide says rows overlap and the sum is never a remaining share.
- **Cursor:** `c1.<offset>[,<offset2>].<binding>`, binding = first 16 hex of SHA-256 of the tool name, NUL, and the JSON of the query arguments other than `limit` and `cursor`. Row order is total: weight descending, then ID ascending.
- **Errors:** tool failures are results with `isError: true` whose text says what to call next. Unknown tools and malformed JSON-RPC are protocol errors.

## File structure

| Path | Responsibility | Task |
|---|---|---|
| `internal/flamegraph/golden_test.go`, `internal/flamegraph/testdata/*.golden.html` | pin current page output | 1 |
| `internal/profilemodel/rounding.go` | `Rounding`, `PolicyFor`, `Canonical`, `Fact.Exact` | 1 |
| `internal/profilemodel/loadfs.go`, `corefacts.go`, `kernels.go`, `notes.go` | `LoadFS`, `CoreFacts`, `KernelsBelow`, `Notes`, `UnsampledPortion` | 2 |
| `internal/pmfixture/pmfixture.go`, `fixtures.go` | importable pprof/execlog builders and named fixtures | 2 |
| `internal/profilemodel/functions.go` | `Functions`, `Sandwich` | 3 |
| `internal/profilemodel/flatdiff.go` | `Flatten`, `UnionID` | 4 |
| `internal/mcpstdio/server.go`, `protocol.go` | JSON-RPC framing, lifecycle, registries | 5 |
| `internal/perfmcp/{config,limits,roots,store,wire,cursor,server,tool_open}.go` | server core and `open` | 6 |
| `internal/perfmcp/{tool_top,tool_path,tool_facts,tool_kernels}.go` | query tools | 7 |
| `internal/perfmcp/{tool_sandwich,tool_diff}.go` | sandwich and diff | 8 |
| `internal/explain/{explain,decode,check,tokens}.go` | claims, validation, text segments | 9 |
| `internal/flamegraph/{explainpage.go,explainpage.tmpl,explainpage.js}` | explained page | 10 |
| `internal/perfmcp/{tool_render,attempts,output}.go` | `render_flamegraph`, attempts, safe writes | 11 |
| `internal/profilemodel/source.go`, `internal/sourceroot/{roots,resolve,git,manifest}.go` | source refs, resolution, evidence | 12 |
| `internal/perfmcp/tool_source.go` | `source` | 13 |
| `cmd/perf-agent-mcp/main.go`, `internal/perfmcp/guide.md`, `docs/mcp.md` | binary, agent guide, docs | 14 |
| `scripts/mcp-evals.sh`, `internal/perfmcp/evals/questions.json`, `internal/perfmcp/evals_test.go` | local model-in-the-loop evals | 15 |

---

### Task 1: Pin current pages, and give rounding a structured policy

**Files:**
- Create: `internal/flamegraph/golden_test.go`, `internal/flamegraph/testdata/render.golden.html`, `internal/flamegraph/testdata/fragment.golden.html`, `internal/flamegraph/testdata/diffpage.golden.html`
- Create: `internal/profilemodel/rounding.go`, `internal/profilemodel/rounding_test.go`
- Modify: `internal/profilemodel/facts.go` (`intFact`, `unavailableFact`, `shareFact`, `RoundingFor`, `Fact`), `internal/profilemodel/diff.go` (`shareDelta`, `perUnitDelta`)

**Interfaces:**
- Consumes: M1 `Fact`, `RoundingFor`, `shareFact`, `(*differ).shareDelta`.
- Produces:
  - `type RoundingKind uint8` with `RoundExact`, `RoundDecimalPlaces`, `RoundSignificantFigures` (iota+1; the zero value means "no policy").
  - `type Rounding struct { Kind RoundingKind; Digits int }`; `func (r Rounding) String() string` (`"exact"`, `"0.001 ratio"` for places 3, `"3 significant figures"`).
  - `func PolicyFor(metric string) (Rounding, bool)`; `RoundingFor(m)` returns `PolicyFor(m).String()` or `""`.
  - `func (r Rounding) Canonical(x *big.Rat) (string, bool)`: exact → integer digits (false if `x` is not an integer); decimal places → half away from zero, exactly `Digits` decimals, never `"-0.000"`; significant figures → false.
  - `func (f Fact) Policy() Rounding`, `func (f Fact) Exact() (*big.Rat, bool)`, `func (f Fact) Canonical() (string, bool)` (`Derived` uses `strconv.FormatFloat(v, 'g', Digits, 64)`; unavailable → false). `Fact` gains unexported `policy Rounding` and `exact *big.Rat` (ratios only); JSON is unchanged.

- [ ] **Step 1: Pin the existing pages before any change.** Write `TestRenderHTMLMatchesGolden`, `TestRenderFragmentMatchesGolden` (the fixture `TestRenderHTMLIsDeterministic` uses) and `TestWriteDiffPageMatchesGolden` (a CPU base/new pair with one only_base and one only_new path, built in the test). A package-level `var updateGolden = flag.Bool("update-golden", false, ...)` rewrites the files. Generate them on the unchanged tree (`go test ./internal/flamegraph/ -run Golden -update-golden`), then run without the flag: PASS.

- [ ] **Step 2: Commit the pins**

```bash
git add internal/flamegraph/golden_test.go internal/flamegraph/testdata
git commit -m "flamegraph: pin the single-profile and diff pages byte for byte"
```

- [ ] **Step 3: Write the failing rounding tests** in `rounding_test.go`:

```go
func TestRoundingForIsUnchanged(t *testing.T) // every Metric* and MetricQuality* constant → the M1 string table in RoundingFor's doc comment; "nope" → ""
func TestPolicyForKnowsEveryMetric(t *testing.T) // ok for each constant; PolicyFor("nope") → ok false
func TestCanonicalRoundsHalfAwayFromZero(t *testing.T) {
	p3 := Rounding{Kind: RoundDecimalPlaces, Digits: 3}
	// 411/1000 "0.411"; 4115/10000 "0.412"; 1/2000 "0.001"; 4/10000 "0.000";
	// -1/2000 "-0.001"; -4/10000 "0.000"; 1/3 "0.333"; 2/3 "0.667"; 1/1 "1.000"
	ex := Rounding{Kind: RoundExact}
	// 9007199254740993/1 → "9007199254740993", ok; 1/2 → ok false
	// Rounding{Kind: RoundSignificantFigures, Digits: 3}.Canonical(...) → ok false
}
func TestFactExactIsTheRationalNotTheFloat(t *testing.T) // shareFact(MetricShareOfTotal,1,3,..).Exact() == 1/3 (Rat.Cmp); intFact(...,9007199254740993,..) exact; unavailable → false
func TestShareDeltaStatusIsExact(t *testing.T) // views with totals 3 and 6, node 1 and 2: DeltaShare status "zero" and Canonical "0.000"
func TestFactJSONIsUnchanged(t *testing.T) // json.Marshal(shareFact(MetricShareOfTotal, 411, 1000, DenominatorViewTotal)) == `{"ratio":0.411,"unit":"ratio","status":"ok","denominator":"view_total","rounding":"0.001 ratio"}`
```

- [ ] **Step 4: Run them to verify they fail**

Run: `go test -race ./internal/profilemodel/ -run 'Rounding|PolicyFor|Canonical|FactExact|ShareDeltaStatus|FactJSON'`
Expected: FAIL (undefined `Rounding`, `PolicyFor`).

- [ ] **Step 5: Implement `rounding.go` and thread the policy.** `intFact`/`unavailableFact`/`shareFact` set `policy` from `PolicyFor(metric)`; `shareFact` sets `exact = big.NewRat(part, whole)`; `shareDelta` sets `exact` to new/total − base/total as a `big.Rat` and takes its status from the rational's sign (the float `Ratio` is unchanged). Rounding half away from zero on a rational: `q, r := QuoRem(|num|·10^d, den)`, add 1 when `2r ≥ den`, restore the sign, format with `d` decimals.

- [ ] **Step 6: Run the package and the pins**

Run: `go test -race ./internal/profilemodel/ ./internal/flamegraph/ ./cmd/flamegraph/`
Expected: PASS, goldens included.

- [ ] **Step 7: Commit**

```bash
git add internal/profilemodel
git commit -m "profilemodel: rounding is a structured policy, and shares keep their exact ratio"
```

### Task 2: Load through a root, and the view accessors the tools read

**Files:**
- Create: `internal/pmfixture/pmfixture.go`, `internal/pmfixture/fixtures.go`, `internal/pmfixture/pmfixture_test.go`
- Create: `internal/profilemodel/corefacts.go`, `kernels.go`, `notes.go`, `kernels_test.go`, `corefacts_test.go`
- Modify: `internal/profilemodel/load.go` (Load and LoadFS share one reader), `load_test.go`, `facts.go` (Facts builds on CoreFacts)

**Interfaces:**
- Consumes: Task 1 `Fact` policy; M1 `Load`, `assemble`, `readCapped`, `View`, `Node.samples/zeroSamples`, `executionsBelow`, `viewSamplingNotes`, `unsampledTotal`.
- Produces (profilemodel):
  - `func LoadFS(fsys fs.FS, profilePath, execlogPath string, lim Limits) (*Profile, error)`, same semantics as `Load` (same handle rules, a missing sidecar is `ReasonCorrupt`, over `MaxSidecarBytes` is unread).
  - `type CoreFacts struct { Node, ParentNode NodeID; Cumulative, Self, ShareOfTotal, ShareOfParent, InexactPortion Fact }` (snake_case JSON tags); `func (v *View) CoreFacts(id NodeID) (CoreFacts, error)`.
  - `var ErrNotGPUView = errors.New("profilemodel: not a GPU view")`.
  - `type KernelRow struct { Name string; Value, ShareOfNode, ShareOfTotal, Executions Fact }`; `func (v *View) KernelsBelow(id NodeID) ([]KernelRow, error)`. `Value` is the sum of values of samples through the node whose frames at or below it include `[gpu:kernel:<Name>]`, once per sample (policy of `cumulative`); `ShareOfNode` has denominator `node:<id>`, `ShareOfTotal` `view_total`; `Executions` as `executions_below` over those samples, zero-value ones included. Kernels seen only in zero-value samples are rows with `Value` status `zero`. Order: value desc, then name.
  - `func (v *View) Notes() ([]string, error)` (the view-level sampling notes, nil off GPU or at total 0) and `func (v *View) UnsampledPortion() (Fact, error)` (unavailable `not a GPU view` off GPU; the error is an overflow), both view-level.
- Produces (pmfixture; no `profilemodel` import):
  - `type Line struct { Name, Filename string; StartLine, Line int64 }`, `type Frame struct { File, BuildID string; Addr uint64; Lines []Line }` (`File == ""` puts it on `Mapping[0]`, which has no file), `type Sample struct { Frames []Frame; Value int64; Labels map[string][]string }` (frames root first).
  - `func Build(typ, unit string, comments []string, samples ...Sample) []byte` (leaf-first, gzipped by `profile.Write`); `func Fn(name string) Frame`; `func In(file, buildID string, addr uint64, lines ...Line) Frame`; `func L(name, filename string, line int64) Line`; `func Execlog(tb testing.TB, profile []byte, hdr execlog.Header, rows []execlog.Row) []byte`.
  - Named fixtures, values pinned in their doc comments:
    - `CPUBasic()`: `cpu/nanoseconds`, module `/opt/app/bin/app` build id `b1`; `main;a;b`=411, `main;a`=100, `main;c`=489 (total 1000; `a` cum 511, `b` 411 = 0.411 of total and 411/511 → `"0.804"` of parent). Lines: `main` `/build/app/src/main.c:10`, `a` `/build/app/src/app.c:20`, `b` `/build/app/src/app.c:30`, `c` `/build/app/src/util.c:40`.
    - `CPUBasicNew()`: same names, build id `b2`; `main;a;b`=300, `main;a`=100, `main;c`=489, `main;d`=111.
    - `GPUBasic()` and `GPUBasicExeclog(tb)`: `gpu/nanoseconds`, comment `perf-agent.capture_id=cap-1`, every sample `gpu_sample_period=8`; `train;conv` in `/opt/app/lib/libtrain.so` build id `t1`, synthetic frames on `Mapping[0]`:
      `train;conv;[gpu:launch];[gpu:kernel:k1]`=300 (`gpu_join=exact`, `gpu_serialized=false`, `gpu_src_status=resolved`, `gpu_src_file=conv.cu`, `gpu_src_line=42`, `gpu_src_func=k1`),
      `train;conv;[gpu:launch];[gpu:kernel:k2]`=100 (`gpu_join=heuristic`, `gpu_serialized=true`),
      `[gpu:launch unsampled];[gpu:kernel:k1]`=100 (`gpu_join=exact`, `gpu_serialized=false`),
      `train;conv;[gpu:launch];[gpu:kernel:k3]`=0 (`gpu_serialized=false`).
      Log rows: sample 0 → executions 0 (200) and 1 (100); sample 1 → 2 (100); sample 2 → 3 (100); sample 3 → 4 (0). Total 500; `conv` cum 400, executions below 4, kernel names observed 3; unsampled 100.
    - `HugeValue()`: `cpu/nanoseconds`, `main;big` = 9007199254740993.
    - `Recursive()`: `a;b;a;b`=10, `a`=5.
    - `Deep(n int)`, `DeepNew(n int)`: one chain `f0;…;f(n-1)` of value 1 in module `/opt/deep` (build ids `d1`, `d2`); `DeepNew` adds leaf `g` of value 1.
    - `SameNameTwoModules()`: `main;foo` in `/lib/libx.so` (=7) and `main;foo` in `/lib/liby.so` (=3).

- [ ] **Step 1: Write the failing tests**

```go
// pmfixture_test.go
func TestNamedFixturesParseWithTheirPinnedTotals(t *testing.T) // each fixture: profile.ParseData ok, Σ values == pinned total, GPUBasicExeclog reads with execlog.Read

// load_test.go
func TestLoadFSMatchesLoad(t *testing.T)            // CPUBasic and GPUBasic(+log) written to t.TempDir(): Load vs LoadFS(os.DirFS(dir)) → equal Handle, Digest, Executions
func TestLoadFSThroughARootRefusesEscapes(t *testing.T) // root, _ := os.OpenRoot(dir); LoadFS(root.FS(), "../x.pb.gz") error; a symlink in dir to a file outside → error
func TestLoadFSSidecarOverLimitIsUnread(t *testing.T)  // MaxSidecarBytes=8 → Executions.Reason == ReasonSidecarLimits, Handle contains "+unread:"

// corefacts_test.go
func TestCoreFactsAgreeWithFacts(t *testing.T) // every node of CPUBasic and GPUBasic: CoreFacts fields reflect.DeepEqual the same Facts fields

// kernels_test.go
func TestKernelsBelowCountsExecutionsFromTheLogOnly(t *testing.T) // GPUBasic+log, node conv: [{k1 300, node 0.750, total 0.600, exec 2}, {k2 100, 0.250, 0.200, exec 1}, {k3 0 status zero, exec 1}]; len == Facts(conv).KernelNamesObserved
func TestKernelsBelowWithoutALogSaysWhy(t *testing.T)            // no sidecar → every Executions unavailable "no sidecar"
func TestKernelsBelowNeverScalesBySamplePeriod(t *testing.T)     // k1 Value 300, not 2400
func TestKernelsBelowTheUnsampledRoot(t *testing.T)              // node [gpu:launch unsampled] → [{k1 100}]
func TestKernelsBelowOnACPUViewIsNotGPU(t *testing.T)            // errors.Is(err, ErrNotGPUView)
func TestViewNotesAndUnsampledAreViewLevel(t *testing.T)         // GPUBasic: Notes() == Facts(root).Notes; UnsampledPortion() Int 100; CPUBasic: nil notes, unavailable "not a GPU view"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/pmfixture/ ./internal/profilemodel/ -run 'Fixtures|LoadFS|CoreFacts|KernelsBelow|ViewNotes'`
Expected: FAIL (packages and functions undefined).

- [ ] **Step 3: Implement.** `readCapped` takes an opener (`func(string) (io.ReadCloser, error)`); `Load` passes `os.Open`, `LoadFS` passes `fsys.Open`; `assemble` is shared, so handles are identical. `Facts` takes its first five facts from `CoreFacts`. `KernelsBelow` walks `n.samples` then `n.zeroSamples` with `builder.framesOf` from depth `n.Depth-1`, as `kernelNamesBelow` does. pmfixture shares mappings/functions/locations whose fields are equal, as M1's `rich` does.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/pmfixture/ ./internal/profilemodel/ ./internal/flamegraph/ ./cmd/flamegraph/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pmfixture internal/profilemodel
git commit -m "profilemodel: load through an fs.FS, and core facts, kernels and view notes for the tools"
```

### Task 3: Function aggregation and sandwich

**Files:**
- Create: `internal/profilemodel/functions.go`, `internal/profilemodel/functions_test.go`

**Interfaces:**
- Consumes: `View`, `builder.framesOf`, `FunctionID.Key`, `shareFact`; pmfixture `CPUBasic`, `Recursive`, `SameNameTwoModules`.
- Produces:
  - `type FunctionRef string`: first 16 lowercase hex of SHA-256 of `FunctionID.Key()`; `func RefOf(f FunctionID) FunctionRef`.
  - `type FunctionRow struct { Ref FunctionRef; Function FunctionID; Cum, Self int64; ShareOfTotal Fact }`.
  - `type EdgeRow struct { Ref FunctionRef; Function FunctionID; Value int64; ShareOfFunction Fact }` (denominator `function:<ref>`).
  - `type Sandwich struct { Function FunctionRow; Callers, Callees []EdgeRow; RootValue int64; Recursive bool }`.
  - `var ErrUnknownFunction = errors.New("profilemodel: no such function in the view")`.
  - `func (v *View) Functions() ([]FunctionRow, error)`: Cum is the value of samples whose stack contains the function at least once (pprof's rule: once per sample); Self the value of samples whose leaf frame is it. Order: Cum desc, then Ref.
  - `func (v *View) Sandwich(ref FunctionRef) (Sandwich, error)`: a caller row X is the value of samples in which some occurrence of the function has X as its immediate parent frame, once per sample per X; callees likewise for the immediate child; `RootValue` is the value of samples in which some occurrence is the outermost frame; `Recursive` is set when any sample holds the function twice. Rows ordered by Value desc, then Ref. Zero-value samples add nothing. All sums checked (`addChecked`).

- [ ] **Step 1: Write the failing tests**

```go
func TestFunctionsCountRecursionOncePerSample(t *testing.T) // Recursive: a cum 15 self 5; b cum 10 self 10
func TestFunctionsKeepSameNameInTwoModulesApart(t *testing.T) // SameNameTwoModules: two "foo" rows, Cum 7 and 3, distinct Refs
func TestFunctionsSelfSumsToTheTotal(t *testing.T)   // every pmfixture fixture: Σ Self == v.Total
func TestFunctionsOrderIsTotal(t *testing.T)         // sorted by Cum desc then Ref; two calls give equal slices
func TestSandwichCallersAndCallees(t *testing.T)     // CPUBasic, a: Function cum 511 self 100; Callers [main 511, share 1.000]; Callees [b 411, "0.804"]; RootValue 0
func TestSandwichRecursionCountsEachEdgeOncePerSample(t *testing.T) // Recursive, a: Callers [b 10], RootValue 15, Callees [b 10], Recursive true; b: Callers [a 10], Callees [a 10]
func TestEdgeValuesNeverExceedTheFunction(t *testing.T) // every fixture and function: each edge Value ≤ Function.Cum and RootValue ≤ Function.Cum
func TestSandwichUnknownRef(t *testing.T)            // errors.Is(err, ErrUnknownFunction)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/profilemodel/ -run 'Functions|Sandwich|EdgeValues'`
Expected: FAIL.

- [ ] **Step 3: Implement `functions.go`.** One pass over samples with non-zero value: frames via a fresh `builder`, a per-sample `map[string]struct{}` of keys seen, of (caller, callee) pairs for the requested function, and of root occurrences.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/profilemodel/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profilemodel/functions.go internal/profilemodel/functions_test.go
git commit -m "profilemodel: aggregate by function and build sandwiches, counting a function once per sample"
```

### Task 4: A flat diff, for MCP and for the CLI

**Files:**
- Create: `internal/profilemodel/flatdiff.go`, `internal/profilemodel/flatdiff_test.go`
- Modify: `cmd/flamegraph/model.go` (`runDiff` marshals `d.Flatten()`), `cmd/flamegraph/model_test.go`, `docs/usage.md` (the paragraph at "The diff JSON is compact and its format is unstable"), `CHANGELOG.md` (Unreleased entry)

**Interfaces:**
- Consumes: `Diff`, `DiffNode`; pmfixture `CPUBasic`/`CPUBasicNew`, `Deep`/`DeepNew`.
- Produces:
  - `type UnionID string`: `"<base node id>/<new node id>"`, with `-` for an absent side (`"<b>/-"`, `"-/<n>"`).
  - `type FlatDiffNode struct` with `ID UnionID` (`id`), `Parent UnionID` (`parent,omitempty`), `Depth int` (`depth`) and every `DiffNode` field except `Base`, `New`, `Children`, with the same JSON names.
  - `type FlatDiff struct` with every `Diff` field except `Root`, same JSON names, plus `Nodes []FlatDiffNode` (`nodes`), in pre-order with children in `Diff` order.
  - `func (d *Diff) Flatten() FlatDiff`.

- [ ] **Step 1: Write the failing tests**

```go
func TestFlattenIsPreOrderAndParentsComeFirst(t *testing.T) // every diffPairs() pair: parent ID seen earlier; Nodes[0].Parent == ""; len == union size
func TestFlattenUnionIDsNameBothSides(t *testing.T)        // CPUBasic→CPUBasicNew: both "<b>/<n>", main;d "-/<n>"; IDs unique
func TestFlattenCarriesEveryField(t *testing.T)            // each node equal to its DiffNode field by field; header equal to Diff's
func TestFlatDiffJSONNestingIsBounded(t *testing.T)        // Deep(600) vs DeepNew(600): json.Marshal(Flatten()) has max nesting ≤ 6 (count with json.Decoder.Token)
// cmd/flamegraph
func TestDiffJSONIsAFlatNodeList(t *testing.T) // -diff output: top-level "nodes" array, no "root" key, nodes[0].parent absent; page bytes unchanged vs WriteDiffPage of the same Diff
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/profilemodel/ ./cmd/flamegraph/ -run 'Flatten|FlatDiff|DiffJSON'`
Expected: FAIL.

- [ ] **Step 3: Implement `Flatten` (iterative stack walk, not recursion) and switch `runDiff` to `json.Marshal(d.Flatten())`.** Update the usage.md paragraph and the CHANGELOG entry: the diff JSON is a flat `nodes` list in pre-order; each node has `id` and `parent`.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/profilemodel/ ./internal/flamegraph/ ./cmd/flamegraph/`
Expected: PASS (goldens unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/profilemodel/flatdiff.go internal/profilemodel/flatdiff_test.go cmd/flamegraph docs/usage.md CHANGELOG.md
git commit -m "profilemodel: flatten the diff into a node list with parent IDs; -diff JSON uses it"
```

### Task 5: MCP over stdio (`internal/mcpstdio`)

**Files:**
- Create: `internal/mcpstdio/protocol.go`, `internal/mcpstdio/server.go`, `internal/mcpstdio/server_test.go`

**Interfaces:**
- Consumes: nothing from perf-agent.
- Produces:
  - `type Implementation struct { Name, Title, Version string }`.
  - `type Result struct { Text string; IsError bool }`; `type Handler func(ctx context.Context, args json.RawMessage) (Result, error)` (a returned error becomes an `isError` result with `err.Error()` as text).
  - `type Annotations struct { ReadOnly, Destructive, Idempotent, OpenWorld bool }` (JSON `readOnlyHint` and so on).
  - `type Tool struct { Name, Title, Description string; InputSchema json.RawMessage; Annotations Annotations; Handler Handler }`.
  - `type Prompt struct { Name, Title, Description, Text string }` (one user message).
  - `type Resource struct { URI, Name, Title, Description, MIMEType, Text string }`.
  - `type Options struct { Info Implementation; Instructions string; Tools []Tool; Prompts []Prompt; Resources []Resource; MaxMessageBytes int; Log *slog.Logger }`.
  - `func New(o Options) *Server`; `func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error` (nil at EOF or when ctx ends).
  - `const LatestVersion = "2025-11-25"`; `var SupportedVersions = []string{"2025-11-25", "2025-06-18"}`.
  - Behaviour: newline-delimited messages; `initialize` echoes a supported `protocolVersion`, else answers `LatestVersion`, with capabilities `tools`, `prompts`, `resources` (all `listChanged:false`, `subscribe:false`) and `instructions`; `ping` works at any time; any other request before `initialize` → `-32600` "send initialize first"; `notifications/*` get no response; unknown method → `-32601` (this covers `server/discover`); unknown tool or prompt or resource → `-32602`; malformed JSON → `-32700` with `id: null`; a JSON array → `-32600` "batches are not supported"; a line over `MaxMessageBytes` → `-32600` naming the limit, the rest of the line discarded; a handler panic → `-32603` and the server continues; ids are echoed as raw JSON; responses are compact JSON plus `\n`, written only to `w`.

- [ ] **Step 1: Write the failing tests** (in-process: `io.Pipe` both ways, `Serve` in a goroutine, a test client that writes lines and reads responses)

```go
func TestInitializeEchoesASupportedVersion(t *testing.T)      // "2025-06-18" → "2025-06-18"; capabilities has tools, prompts, resources; serverInfo.name set
func TestInitializeAnswersLatestForOtherVersions(t *testing.T) // "2024-11-05" and "2026-07-28" → "2025-11-25"
func TestServerDiscoverIsMethodNotFoundSoClientsFallBack(t *testing.T) // server/discover → -32601; then initialize with "2025-11-25" succeeds and tools/list works
func TestRequestsBeforeInitializeAreRefused(t *testing.T)     // tools/list first → -32600; ping first → {}
func TestToolsListIsDeterministic(t *testing.T)               // two calls byte-identical; tools in registration order; annotations present
func TestToolErrorsAreResultsWithIsError(t *testing.T)        // handler returns error "call open first" → result isError true, text == message
func TestUnknownToolIsInvalidParams(t *testing.T)             // -32602
func TestNotificationsGetNoResponse(t *testing.T)             // notifications/initialized then ping: the next line is the ping response
func TestMalformedJSONIsAParseErrorAndTheServerGoesOn(t *testing.T) // "{oops" → -32700 id null; then ping works
func TestOversizedMessageIsRefusedAndTheServerGoesOn(t *testing.T)  // MaxMessageBytes=1024, 4 KiB line → -32600 naming MaxMessageBytes; then ping works
func TestBatchIsRefused(t *testing.T)                         // "[...]" → -32600
func TestIDsAreEchoedVerbatim(t *testing.T)                   // "abc", 7 and 9007199254740993 come back byte for byte
func TestAPanickingHandlerIsAnInternalError(t *testing.T)     // -32603; next call works
func TestEveryOutputLineIsJSONRPC(t *testing.T)               // all lines in the session above: valid JSON, "jsonrpc":"2.0", no embedded newline
func TestServeReturnsAtEOF(t *testing.T)                      // closing the writer ends Serve with nil
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/mcpstdio/`
Expected: FAIL (package missing).

- [ ] **Step 3: Implement.** Read with `bufio.Reader.ReadSlice('\n')`, accumulating up to `MaxMessageBytes` and discarding the rest of an oversized line. Decode the envelope into `{jsonrpc, id json.RawMessage, method, params json.RawMessage}`; `id` absent means notification. Prompt and resource lists are static.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/mcpstdio/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpstdio
git commit -m "mcpstdio: a minimal MCP server over newline-delimited stdio"
```

### Task 6: perfmcp core, roots, store, wire, cursors and `open`

**Files:**
- Create: `internal/perfmcp/{config.go,limits.go,roots.go,store.go,wire.go,cursor.go,server.go,tool_open.go}`
- Test: `internal/perfmcp/{harness_test.go,open_test.go,wire_test.go,cursor_test.go,store_test.go}`

**Interfaces:**
- Consumes: Task 1 `Fact.Canonical`; Task 2 `LoadFS`, `Notes`, pmfixture; Task 5 `mcpstdio`.
- Produces:
  - `type Config struct { ProfilesDir string; SourceDirs, SourceManifests []string; SourceGit bool; OutputDir string; Limits Limits; Version string; Log *slog.Logger }`.
  - `type Limits struct { Model profilemodel.Limits; MaxMessageBytes, MaxResponseBytes, MaxOpenHandles int }` with `func DefaultLimits() Limits` = `{profilemodel.DefaultLimits(), 4 MiB, 1 MiB, 8}`. Later tasks add fields and defaults.
  - `func New(cfg Config) (*Server, error)` (opens every root with `os.OpenRoot`, refuses a missing `ProfilesDir`); `func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error`; `func (s *Server) Close() error`; `func (s *Server) tools() []mcpstdio.Tool` (each later task appends its tool here).
  - `func (s *Server) rel(root *os.Root, p string) (string, error)`: a relative path is used as is; an absolute path is made relative to the root's directory with `filepath.Rel` and refused when that starts with `..`; `os.Root` does the enforcement either way. Errors name the root: `path %q is outside the profiles root %s; pass a path inside it`.
  - Store: `type entry struct { prof *profilemodel.Profile; name string; views map[string]*profilemodel.View; opened []os.FileInfo }`; `(*store).open(name, execlog string) (*entry, error)`, `(*store).view(h profilemodel.Handle, view string) (*entry, *profilemodel.View, error)`; LRU over `MaxOpenHandles`; the store keeps every opened file's `FileInfo` in `s.inputs` (for Task 11). Limits are copied at `New` and never change; there is no package-level cache.
  - `func viewNames(p *profilemodel.Profile) []string`, `func parseView(p *profilemodel.Profile, name string) (int, error)`.
  - Wire: `type Fact struct { Value, Unit, Status, Reason, Denominator, Rounding string }` (`value,omitempty`); `func wireFact(f profilemodel.Fact) Fact`; `func wireInt(v int64) string`; `type Quality` mirroring `profilemodel.Quality` with wire facts.
  - Cursor: `func binding(tool string, query any) string`; `func encodeCursor(b string, offsets ...int) string`; `func decodeCursor(c, b string, n int) ([]int, error)`.
  - Paging: `func page[T any](rows []T, weight func(T) int64, offset, limit int) (pageOut[T], error)` returning the rows, `omitted_count`, the checked `omitted_weight` sum and the next offset.
  - `respond(v any) (mcpstdio.Result, error)`: compact JSON; over `MaxResponseBytes` → error `the %s response is %d bytes, over MaxResponseBytes=%d; call again with a smaller limit`.
  - Tool `open` (annotations read-only, idempotent): args `{path: string, execlog?: string}`; result `{handle, profile_digest, profile_name, capture_id, views: [{view, sample_type, unit, index}], default_view, capabilities: {executions: {ok, reason}, pc_sampling: bool, source_roots: [...], render: {enabled, output_root}}, quality: <wire Quality of default view>, notes: [...]}`. `pc_sampling` is true when a sample carries `gpu_pc` or `gpu_src_status`. A named execlog that does not exist in the root is a tool error (`execlog %q not found under the profiles root %s`), nothing opened. Tool args are decoded with `DisallowUnknownFields`; an unknown argument names the tool's arguments.

- [ ] **Step 1: Write the failing tests.** `harness_test.go`: `newTestServer(t, cfg Config) *testClient` (pipes, `initialize`, cleanup with `t.Context()`), `(*testClient).call(tool string, args any) (text string, isError bool)`, `writeFixture(t, dir, name string, b []byte)`.

```go
func TestOpenReturnsAHandleViewsAndCapabilities(t *testing.T) // CPUBasic: handle == hex digest; views ["cpu/nanoseconds#0"]; executions {ok:false, reason:"no sidecar"}; render.enabled false without OutputDir
func TestOpenWithAnExeclogGivesADifferentHandle(t *testing.T)  // GPUBasic with and without log: handles differ, digest equal; executions ok; notes non-empty
func TestOpenAcceptsAnAbsolutePathInsideTheRoot(t *testing.T)   // filepath.Join(dir, "cpu.pb.gz") works
func TestOpenRefusesPathsOutsideTheRoot(t *testing.T)           // "../x", "/etc/passwd", a symlink in the root to t.TempDir()/other file, a symlinked dir → isError naming the profiles root
func TestOpenRefusesAMissingExeclog(t *testing.T)               // isError mentions "not found"; a later open of the same profile without it still works
func TestOpenRefusesAProfileOverALimit(t *testing.T)            // Limits.Model.MaxSamples=1 → isError contains "MaxSamples"
func TestUnknownArgumentsAreNamed(t *testing.T)                 // open {path, color} → isError lists "path, execlog"
func TestHandlesAreEvidenceNotPolicy(t *testing.T)              // two servers, MaxSidecarRows 1 vs default, same GPUBasic+log: same handle string; executions unavailable "sidecar exceeds limits" vs ok
func TestTheStoreEvictsTheLeastRecentlyUsedHandle(t *testing.T) // MaxOpenHandles=1: open A, open B, a call on A → isError "call open(path) again"
func TestWireIntegersAreDecimalStrings(t *testing.T)            // HugeValue: open's quality JSON text contains no bare number above 2^53; wireFact of Cumulative == "9007199254740993"
func TestWireNeverRendersUnavailableAsZero(t *testing.T)        // unavailable fact → no "value" key, reason set; zero fact → value "0" status "zero"
func TestCursorsAreBoundToTheirQuery(t *testing.T)              // decodeCursor with another binding → error; offset beyond n → error; garbage → error
func TestOversizedResponsesSayHowToShrinkThem(t *testing.T)     // MaxResponseBytes=64 → isError contains "smaller limit"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/`
Expected: FAIL (package missing).

- [ ] **Step 3: Implement the files listed.** Input schemas are hand-written JSON literals next to each tool. Log to `cfg.Log` (stderr in the binary).

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: server core with os.Root-confined profiles, a per-server store and the open tool"
```

### Task 7: `top`, `path`, `facts` and `gpu_kernels_for`

**Files:**
- Create: `internal/perfmcp/{tool_top.go,tool_path.go,tool_facts.go,tool_kernels.go}`, `internal/perfmcp/query_test.go`
- Modify: `internal/perfmcp/limits.go` (`MaxRows int`, default 200), `internal/perfmcp/server.go` (register), `internal/flamegraph/domain.go` (`func DomainKeys() []string`, legend order)

**Interfaces:**
- Consumes: Task 2 `CoreFacts`, `KernelsBelow`, `Notes`; Task 3 `Functions`; Task 6 store, wire, `page`, cursors; `flamegraph.Classify`.
- Produces (all read-only, idempotent; every result has `handle`, `view` and, on GPU views, `notes`):
  - `top` args `{handle, view, rows: "functions"|"nodes", by?: "cum"|"self" (default "cum"), domain?: string, limit?: 1..MaxRows (default 20), cursor?}`. Node rows `{node, name, module, domain, cumulative, self, share_of_total}` (synthetic root excluded); function rows `{function, name, module, domain, cumulative, self, share_of_total}`. Domain is `Classify(name, module).Info().Key`; an unknown domain lists `DomainKeys()`; filtered rows are counted in `filtered_out_count`, not in `omitted_count`. `omitted_weight_basis` is `by`. Sorted rows are cached per (view, rows, by).
  - `path` args `{handle, view, node}`: `{node, frames: [{node, name, module, build_id, depth, domain, domain_inferred: true, collision_possible, cumulative, self, share_of_total, share_of_parent}]}`, root-first, root excluded.
  - `facts` args `{handle, view, node}`: wire `PathFacts` (every field, `path` names) plus `quality` (wire `NodeQuality`) and `call_sites` count.
  - `gpu_kernels_for` args `{handle, view, node, limit?, cursor?}`: rows `{kernel, value, share_of_node, share_of_total, executions}`, `omitted_weight_basis: "value"`; a CPU view → isError `node is in a cpu view; gpu_kernels_for needs a GPU view (open returned the views)`.
  - Node errors: `node %s is not in view %s of handle %s; call top with rows="nodes" or path to find node IDs`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTopNodesByCumulative(t *testing.T)      // CPUBasic: rows main 1000, a 511, c 489, b 411; values are strings; share_of_total.value "0.511" for a
func TestTopFunctionsBySelf(t *testing.T)        // CPUBasic: c 489, b 411, a 100, then main with self "0" status "zero" (present, last)
func TestTopTruncatesWithOmittedCountAndWeight(t *testing.T) // limit 2 by cum nodes: omitted_count "2", omitted_weight "900" (489+411), basis "cum", next_cursor set; the cursor page returns c then b and no next_cursor
func TestTopCursorFromAnotherQueryIsRefused(t *testing.T)    // cursor from rows=nodes reused with rows=functions → isError
func TestTopDomainFilter(t *testing.T)           // domain "app" keeps all CPUBasic rows; "bogus" → isError listing DomainKeys()
func TestRecursionInTopFunctions(t *testing.T)   // Recursive: a cumulative "15"
func TestPathIsRootFirstWithPerFrameFacts(t *testing.T) // CPUBasic b: frames main, a, b; b share_of_parent value "0.804", denominator "parent:<a id>"
func TestFactsCarryHugeIntegersLosslessly(t *testing.T) // HugeValue big: cumulative.value "9007199254740993" in the raw text
func TestFactsZeroAndUnavailableStayApart(t *testing.T) // CPUBasic: b executions_below unavailable "no sidecar" with no "value" key; main self status "zero" value "0"
func TestKernelsForConv(t *testing.T)            // GPUBasic+log conv: k1 "300" exec "2", k2 "100" exec "1", k3 "0" status zero exec "1"
func TestEveryGPUViewResponseCarriesTheSamplingNotes(t *testing.T) // top, path, facts, gpu_kernels_for on GPUBasic: notes equal View.Notes(); on CPUBasic: no notes key
func TestNodeErrorsSayWhatToCall(t *testing.T)   // unknown node → isError contains "call top"
func TestAnAllZeroViewHasUnavailableShares(t *testing.T) // pmfixture.Build with one sample of value 0: top rows share_of_total unavailable "zero total"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Top|Path|Facts|Kernels|EveryGPU|NodeErrors|AllZero'`
Expected: FAIL.

- [ ] **Step 3: Implement the four tools and `DomainKeys`.**

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/perfmcp/ ./internal/flamegraph/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp internal/flamegraph/domain.go
git commit -m "perfmcp: top, path, facts and gpu_kernels_for over the model"
```

### Task 8: `sandwich` and `diff`

**Files:**
- Create: `internal/perfmcp/{tool_sandwich.go,tool_diff.go}`, `internal/perfmcp/diff_test.go`, `internal/perfmcp/sandwich_test.go`
- Modify: `internal/perfmcp/limits.go` (`MaxCachedDiffs int` default 4, `MaxDiffRows int` default 500), `server.go`

**Interfaces:**
- Consumes: Task 3 `Sandwich`; Task 4 `Flatten`, `UnionID`; `profilemodel.Compare`, `WorkCounts`; Task 6 paging and cursors.
- Produces:
  - `sandwich` args `{handle, view, function, limit?, cursor?}`: `{function: <function row>, recursive, root_value, rule: "each caller and callee is counted once per sample; under recursion rows overlap and may sum to more than the function", callers: <list>, callees: <list>}`; each list paged by `limit` with its own `omitted_*`; the cursor carries both offsets. Unknown ref → `function %s is not in view %s; call top with rows="functions" to list function refs`.
  - `diff` args `{base_handle, base_view, new_handle, new_view, work_base?: number, work_new?: number, limit?: 1..MaxDiffRows (default 50), cursor?}`: header `{base_handle, new_handle, view, sample_type, unit, base_total, new_total, base_quality, new_quality, quality_warnings, base_notes, new_notes, per_unit}` plus rows of flat nodes (all wire: `id, parent, depth, base_id, new_id, name, presence, match, candidates, interpretation, why, base_cum, new_cum, base_self, new_self, combined, delta_abs, delta_share, delta_per_unit`), sorted by |delta_abs| desc, combined desc, id asc; `omitted_weight_basis: "abs_delta"`. Diffs are cached per (handles, views, work) in an LRU of `MaxCachedDiffs`. `ErrIncompatibleViews` → isError naming both views.

- [ ] **Step 1: Write the failing tests**

```go
func TestSandwichOverMCP(t *testing.T)                 // CPUBasic a: callers [main "511"], callees [b "411"], root_value "0"
func TestSandwichRecursionRuleIsStated(t *testing.T)    // Recursive a: recursive true, rule text present
func TestDiffRowsAreFlatWithParentIDs(t *testing.T)    // CPUBasic→CPUBasicNew: every row has id; every non-root parent is an id of the full flat list; main;d presence only_new
func TestDiffSortsByAbsoluteDeltaAndTruncates(t *testing.T) // CPUBasic→CPUBasicNew (totals 1000→1000), limit 2: rows main;a (511→400, combined 911) then main;a;b (411→300); omitted_count "4", omitted_weight "111" (main;d +111, the rest 0)
func TestDiffCursorIsBoundToBothSidesAndWork(t *testing.T) // cursor reused with work_base 2 → isError
func TestDiffPerUnitNeedsBothWorkCounts(t *testing.T)  // no counts: per_unit {ok:false, reason:"no work count"}, rows have no delta_per_unit; work 10/10: rows carry delta_per_unit
func TestDiffOfDeepStacksStaysFlat(t *testing.T)       // Deep(600)/DeepNew(600), limit 500: JSON nesting ≤ 8
func TestDiffRefusesIncompatibleViews(t *testing.T)    // CPUBasic vs GPUBasic → isError names "cpu/nanoseconds#0" and "gpu/nanoseconds#0"
func TestDiffHugeValues(t *testing.T)                  // HugeValue vs HugeValue: base_total "9007199254740993"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Sandwich|Diff'`
Expected: FAIL.

- [ ] **Step 3: Implement both tools.**

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: sandwich and a flat, paged diff"
```

### Task 9: Structured explanations and the claim check (`internal/explain`)

**Files:**
- Create: `internal/explain/{explain.go,decode.go,check.go,tokens.go}`, `internal/explain/{decode_test.go,check_test.go,tokens_test.go}`

**Interfaces:**
- Consumes: Task 1 `PolicyFor`, `Rounding.Canonical`, `Fact.Exact`; Task 2 `CoreFacts`, `UnsampledPortion`; `View.Facts`, `NodeQuality`, `Quality`; pmfixture.
- Produces:
  - `type Limits struct { MaxExplanations, MaxClaimsPerExplanation, MaxClaimsTotal, MaxProseBytes, MaxHypotheses, MaxHypothesisBytes, MaxNextBytes int }`; `DefaultLimits()` = `{64, 32, 512, 8192, 8, 2048, 2048}`.
  - `type Explanation struct { Node, Prose, NextMeasurement string; Claims []Claim; Hypotheses []string }` (JSON `node, claims, prose, hypotheses, next_measurement`).
  - `type Claim struct { ID, Metric, Scope, Node, Module, Denominator, Unit string; Value Literal }` (JSON `id, metric, scope, node, module, denominator, unit, value`); `Scope` is `"node"` (default) or `"view"`.
  - `type Literal struct { Text string; Kind LiteralKind }` with `LiteralNumber`, `LiteralString`, `LiteralOther`: the JSON literal exactly as sent (`json.Decoder.UseNumber`), never a `float64`.
  - `type Notice struct { Path, Message string }`; `func Decode(raw json.RawMessage, lim Limits) ([]Explanation, []Notice, error)`. Shape errors (not an array, a claim not an object) are errors; unknown fields are ignored with a notice, and `rounding`/`tolerance` get the notice `ignored: the engine's rounding for <metric> is <policy>`.
  - `type Code string`: `duplicate_claim_id`, `bad_claim_id`, `unresolved_claim_ref`, `unresolved_node_ref`, `unknown_node`, `unknown_metric`, `diff_metric`, `wrong_scope`, `unknown_module`, `module_not_applicable`, `unavailable_fact`, `wrong_unit`, `wrong_denominator`, `bad_value`, `non_finite`, `out_of_range`, `not_integer`, `value_mismatch`, `limit_exceeded`.
  - `type EngineValue struct { Metric, Scope string; Node profilemodel.NodeID; Module, Denominator, Unit, Value, Rounding string; Status profilemodel.Status; Reason string }` (JSON snake_case; `Value` is `Fact.Canonical()`).
  - `type Failure struct { Explanation int; Claim, Ref string; Code Code; Message string; Agent string; Engine *EngineValue }` (`Agent` is the literal the agent sent).
  - `type SegmentKind string` (`text`, `claim`, `node`, `unchecked_number`); `type Segment struct { Kind SegmentKind; Text, Claim string; Node profilemodel.NodeID }`; `func Tokenize(s string) []Segment`.
  - `type CheckedClaim struct { ID string; EngineValue }`; `type Checked struct { Node profilemodel.NodeID; Facts profilemodel.PathFacts; Claims []CheckedClaim; Prose []Segment; Hypotheses [][]Segment; NextMeasurement []Segment }`.
  - `type Result struct { OK bool; Checked []Checked; Failures []Failure; Figures, References int }`; `func Check(v *profilemodel.View, exps []Explanation, lim Limits) Result`. `Checked.Facts` comes from `View.Facts(id, nil)`: claims never depend on domains, and the page classifies frames itself. `Figures` is the number of claims, `References` the number of distinct `{n:…}` nodes.
  - `type RejectedAttempt struct { Attempt, RetryOf string; Failures []Failure }`.
- Rules (`Check`), in this order per claim, every failing claim and reference reported:
  - claim IDs match `^[A-Za-z][A-Za-z0-9_-]{0,31}$` and are unique across the call;
  - node-scope metrics: `cumulative`, `self`, `share_of_total`, `share_of_parent`, `executions_below`, `kernel_names_observed`, `inexact_portion`, `serialization_*`, `quality_*` (from `NodeQuality`); view-scope metrics: `unsampled_portion` and `quality_*` (from `Quality()`), with `node` empty. `unsampled_portion` at node scope → `wrong_scope` (`unsampled_portion is a view-level fact: claim it with "scope": "view" and no node`); `share_delta`, `per_work_delta` → `diff_metric`;
  - `quality_unresolved_by_module` needs `module`, a key of `unresolved_by_module` (else `unknown_module` listing the keys); `module` on any other metric → `module_not_applicable`;
  - an unavailable fact → `unavailable_fact` with the fact's reason;
  - `unit` must equal the fact's unit (`nanoseconds`, `ratio`, `count`…);
  - `denominator` must equal the fact's denominator exactly (`view_total`, `parent:<id>`, `node:<id>`); a fact without one refuses any;
  - the value parses as a decimal (`big.Rat.SetString` on the literal; strings allowed) else `bad_value`, and `NaN`/`Inf` spellings → `non_finite`; shares must lie in [0, 1] and counts and sample-unit values be ≥ 0 (`out_of_range`); exact metrics need an integer (`not_integer`);
  - `policy.Canonical(agent) == fact.Canonical()` else `value_mismatch`;
  - every `{cN}` resolves to a claim in the call; every `{n:<id>}` and every explanation's `node` exist in the view;
  - over a limit → `limit_exceeded` naming it. Nothing is accepted when any failure exists (`OK` false).
- `Tokenize`: `{<claim id>}` → claim segment; `{n:<16 hex>}` → node segment; any other `{` is text. A numeric literal starts at a digit not preceded by a letter, digit, `_` or `.`, continues over digits and a `.` or `,` followed by a digit, and takes one trailing `%`; it becomes `unchecked_number`. Frame names are never tokenized.

- [ ] **Step 1: Write the failing tests** (CPUBasic node `b` and GPUBasic, node IDs looked up by name path in a test helper)

```go
func TestACorrectClaimPasses(t *testing.T)            // b share_of_total "view_total" ratio 0.411 → OK, Figures 1; agent 0.4114 also passes (rounds to 0.411)
func TestAWrongValueIsRejectedWithTheEngineValue(t *testing.T) // 0.62 → value_mismatch, Engine.Value "0.411", Agent "0.62"
func TestTheRightValueWithTheWrongParentIsRejected(t *testing.T) // share_of_parent 0.804 with denominator "parent:<main id>" → wrong_denominator; Engine.Denominator "parent:<a id>"
func TestAClaimOnAnUnavailableFactIsRejected(t *testing.T) // executions_below without log → unavailable_fact, Message contains "no sidecar"
func TestNonFiniteValuesAreRejected(t *testing.T)     // "NaN", "Inf", "-Inf", "+Inf" → non_finite
func TestToleranceAndRoundingAreIgnored(t *testing.T) // claim {value 0.42, tolerance 0.05, rounding "1 ratio"} → value_mismatch and two notices; {value 0.411, rounding "1 ratio"} → OK
func TestDuplicateIDsAndUnresolvedRefsAreRejected(t *testing.T) // two c1 → duplicate_claim_id; "{c9}" → unresolved_claim_ref; "{n:0000000000000000}" → unresolved_node_ref; node "nope" → unknown_node
func TestViewLevelFactsAreNotNodeClaims(t *testing.T) // GPUBasic: unsampled_portion on conv → wrong_scope; scope view value "100" unit nanoseconds → OK
func TestHugeIntegersCompareExactly(t *testing.T)     // HugeValue big cumulative: "9007199254740993" and 9007199254740993 (number literal) pass; 9007199254740992 → value_mismatch
func TestRangeAndIntegerChecks(t *testing.T)          // share 1.2 → out_of_range; cumulative -1 → out_of_range; cumulative 1.5 → not_integer
func TestUnitMustMatch(t *testing.T)                  // cumulative unit "ms" → wrong_unit, Engine.Unit "nanoseconds"
func TestDiffMetricsAreNotClaimable(t *testing.T)     // share_delta → diff_metric
func TestModuleOnlyForUnresolvedByModule(t *testing.T) // quality_unresolved_by_module without module → unknown_module; cumulative with module → module_not_applicable
func TestLimitsAreEnforced(t *testing.T)              // MaxProseBytes 8 → limit_exceeded naming MaxProseBytes
func TestTokenizeMarksFreeNumbers(t *testing.T) {
	// "{c1} of time; 62% claimed; 1,416 runs; sm86_xmma v2 f9" →
	// claim c1, text, unchecked "62%", text, unchecked "1,416", text without numbers for sm86_xmma, v2, f9
	// "{n:0123456789abcdef} and {x" → node, text "{x"; "operator{}" → text
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/explain/`
Expected: FAIL (package missing).

- [ ] **Step 3: Implement the package.** One fact lookup function `engineFact(v, claim) (profilemodel.Fact, error)` maps metric and scope to the PathFacts, NodeQuality or Quality field.

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/explain/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/explain
git commit -m "explain: structured claims checked against the engine at its own rounding"
```

### Task 10: The explained page

**Files:**
- Create: `internal/flamegraph/explainpage.go`, `explainpage.tmpl`, `explainpage.js`, `explainpage_test.go`
- Modify: none of `render.go`, `diffpage.go`, `diffpage.tmpl`, `assets.go` beyond reading their constants (`paletteCSS`, `jitterCSS`, `lightChrome`, `darkChrome`, `minDrawFraction`, `FormatValue`)

**Interfaces:**
- Consumes: Task 2 `CoreFacts`, `Notes`, `UnsampledPortion`; Task 9 `explain.Result`, `Checked`, `Segment`, `RejectedAttempt`; `View`, `Quality`; `Classify`.
- Produces:
  - `type ExplainedPageOptions struct { Title, ProfileName, Digest, Attempt string; Rejected []explain.RejectedAttempt; MaxNodes int }`.
  - `func WriteExplainedPage(w io.Writer, v *profilemodel.View, r explain.Result, opts ExplainedPageOptions) error` (refuses `!r.OK` and a nil view).
- Page, following mock screen 1 (copy in English, pinned by tests):
  - top bar pills: `Explained by an agent via MCP`, `<Figures+References> figures and references checked`, `prose is not verified`, and `<n> claims rejected at render` when `Rejected` has failures; never the words "verified explanation";
  - the icicle: flat absolutely positioned frames (`left`/`width` in percent, `top` by depth), `data-domain` from `Classify`, `paletteCSS`; nodes under `minDrawFraction` or beyond `MaxNodes` are not drawn and the page states `N nodes narrower than 0.05% are not drawn`; explained nodes carry `✦`;
  - the axis caption on GPU views: `width = GPU time launched from the sampled stacks of this path, not CPU time`; on others: `width = <sample type> (<unit>) at or below the frame`;
  - `Paths the agent explained`: one row per explanation;
  - side panel for the selected frame, server-rendered for the first explanation: name; `Profile facts — computed by perf-agent` (cumulative, self, share of total, share of parent with the parent's name, inexact portion; for explained nodes also executions below with its availability and kernel names observed); `Agent interpretation — prose, not verified` (segments: claim → the engine's canonical value formatted for display, `title` = exact value and rounding; node → chip `f<k>` with the frame name, `title` = node ID; unchecked number → `<mark class="unchecked" title="not checked by perf-agent">`); `Hypotheses` each prefixed `Hypothesis:`; `Next measurement`; `Rejected at render` (per failure: metric, node, the agent's literal struck through, engine value, reason); `Path (root → frame)`; `Follow up in your agent` chips whose `data-prompt` names the profile digest, the view and the node ID (`Why is node <id> (<name>) wide? Use perf-agent-mcp: profile digest <digest>, view <view>.`, `Is any of node <id> profiler overhead? …`, `Compare node <id> with another profile using diff. …`, and on GPU views `Explain the CPU→GPU causality under node <id>. …`);
  - always visible: the sampling notes (GPU) and a quality summary (unresolved share, executions availability; on GPU also exact-join share, unsampled share and the four serialized shares), each value from the engine with its status;
  - per-frame data for the script in `<script type="application/json" id="model">` produced by `json.Marshal` (which escapes `<`, `>`, `&`, U+2028/2029): integers as strings, displayed strings precomputed in Go; the script sets text with `textContent` only.
  - display formatting: shares from `Fact.Canonical()` as percent (`"0.411"` → `"41.1%"`, exact decimal shift); sample-unit values with `FormatValue`, exact digits in `title`; unavailable → `unavailable: <reason>`; zero → `0`.

- [ ] **Step 1: Write the failing tests** (build `explain.Result` by running `explain.Check` on hand-written explanations for CPUBasic and GPUBasic)

```go
func TestExplainedPageHeaderCountsOnlyCheckedFigures(t *testing.T) // 2 claims + 1 node ref → "3 figures and references checked"; contains "prose is not verified"; no "verified explanation" (case-insensitive)
func TestExplainedPageRendersTheEngineValueNotTheAgents(t *testing.T) // claim value 0.4114 → page shows "41.1%", never "0.4114"
func TestExplainedPageMarksFreeNumbers(t *testing.T)   // prose "62% of time" → <mark class="unchecked"…>62%</mark>
func TestExplainedPageEscapesNamesAndText(t *testing.T) // frame name "operator{}<script>"; prose "</script><img src=x onerror=alert(1)> {c1}"; hypothesis "javascript:alert(1)": no raw "<img", no "</script><img", the name is not tokenized
func TestExplainedPageLinksNothing(t *testing.T)       // prose with "https://evil.example/x": no "href=" anywhere, no external src/link (reuse TestRenderHTMLFetchesNothing's check)
func TestExplainedPageAlwaysShowsSamplingNotesAndQuality(t *testing.T) // GPUBasic: notes text and quality summary present with no frame selected; CPU: no GPU note, quality present
func TestExplainedPageFramesWithoutExplanationShowFactsOnly(t *testing.T) // model JSON entry for c has facts and no "explanation" key
func TestExplainedPageChipsNameDigestViewAndNode(t *testing.T) // each data-prompt contains opts.Digest, "cpu/nanoseconds#0" and the node ID
func TestExplainedPageListsRejectedAttempts(t *testing.T) // Rejected [{a1, failures [value_mismatch 0.62 vs 0.411]}] → "1 claims rejected at render", section shows "0.62" struck and "41.1%"
func TestExplainedPageIsFlatOnDeepStacks(t *testing.T)  // Deep(600): every frame div is a child of one container (max element nesting < 20)
func TestExplainedPageIsDeterministic(t *testing.T)     // two renders byte-identical
func TestExplainedPageHugeValues(t *testing.T)          // HugeValue: model JSON contains "9007199254740993" as a string
func TestExplainedPageScriptUsesTextContentOnly(t *testing.T) // explainpage.js contains no "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/flamegraph/ -run 'ExplainedPage'`
Expected: FAIL.

- [ ] **Step 3: Implement `explainpage.go` (data assembly), the template and the script** (selection: click or Enter on a frame or a chip updates name, facts, path and explanation together from the model JSON; chips copy with `navigator.clipboard.writeText`, falling back to selecting a read-only `<textarea>`).

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/flamegraph/`
Expected: PASS, goldens unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/flamegraph/explainpage.go internal/flamegraph/explainpage.tmpl internal/flamegraph/explainpage.js internal/flamegraph/explainpage_test.go
git commit -m "flamegraph: an explained page whose figures all come from checked claims"
```

### Task 11: `render_flamegraph`, attempts and safe writes

**Files:**
- Create: `internal/perfmcp/{tool_render.go,attempts.go,output.go}`, `internal/perfmcp/render_test.go`
- Modify: `internal/perfmcp/limits.go` (`Explain explain.Limits`, `MaxPageBytes int` default 32 MiB, `MaxPageNodes int` default 20000, `MaxAttemptsPerView int` default 100), `server.go`

**Interfaces:**
- Consumes: Task 9 `Decode`, `Check`, `RejectedAttempt`; Task 10 `WriteExplainedPage`; Task 6 store, `s.inputs`, `rel`.
- Produces:
  - Tool `render_flamegraph` (annotations: not read-only, not destructive, not idempotent, closed world): args `{handle, view, explanations: array, out: string, attempt?: string, overwrite?: bool, title?: string}`; top-level `rounding`/`tolerance` are accepted and ignored with a notice. Without `OutputDir` the description starts `DISABLED: start perf-agent-mcp with --output DIR to enable rendering.` and calls are isError with that text.
  - Flow: resolve handle and view; an `attempt` that is unknown or bound to another handle/view → isError (no attempt recorded); then assign `a<n>` (per-server sequence) and record `{id, handle, view, retry_of, outcome}`; `Decode` (shape error → isError, recorded `outcome: "invalid"`); `Check` failures → result `isError: true` with `{attempt, status: "rejected", written: false, failures, notices}` recorded as rejected; otherwise render into memory (over `MaxPageBytes` → isError), write, and return `{attempt, status: "written", path, bytes, figures_checked, unchecked_numbers, notices}`.
  - `type attempts struct`; `(*attempts).add(h, view, retryOf string, outcome string, failures []explain.Failure) string`; `(*attempts).rejected(h, view string) []explain.RejectedAttempt` (the page's "Rejected at render" source; at most `MaxAttemptsPerView` kept, oldest dropped and counted).
  - `func (s *Server) writePage(out string, page []byte, overwrite bool) (string, error)` in `output.go`: `out` relative to the output root (absolute converted as in `rel`), cleaned, ending in `.html`, parent directory existing; `root.Lstat(out)`: a symlink → refuse; an existing file that `os.SameFile`s any opened input → `refusing to overwrite input %s`; any other existing file → refuse unless `overwrite` and it is a page this server rendered (same `FileInfo` and the SHA-256 recorded at write time). Write to `.<base>.tmp-<16 hex>` created `O_CREATE|O_EXCL` in the same directory, then `root.Link(tmp, out)` (no clobber) and remove the temporary; on `overwrite`, `root.Rename(tmp, out)`; when the filesystem refuses hard links, fall back to `Lstat`-then-`Rename`. Every error path removes the temporary.

- [ ] **Step 1: Write the failing tests**

```go
func TestRenderWritesAPageForCheckedClaims(t *testing.T)  // CPUBasic, claim b share 0.411 → status written, file under OutputDir starts with "<!doctype html>" and contains "41.1%"
func TestRenderRejectsAndWritesNothing(t *testing.T)      // value 0.62 → isError, status rejected, failures[0].engine.value "0.411", no file and no temporary left in OutputDir
func TestRejectedAttemptsComeFromTheServersRecord(t *testing.T) // a1 rejected, a2 (attempt "a1") written: page lists a1's failure; explanation prose claiming "nothing was rejected" does not remove it
func TestAttemptsAreBoundToHandleAndView(t *testing.T)    // attempt "a1" passed with another handle → isError; unknown "a99" → isError
func TestRenderIsDisabledWithoutAnOutputRoot(t *testing.T) // tools/list description starts "DISABLED"; call → isError mentioning --output
func TestRenderRefusesToLeaveTheOutputRoot(t *testing.T)  // out "../x.html", "/tmp/x.html", a symlinked subdir pointing outside → isError
func TestRenderNeverOverwritesAnInput(t *testing.T)       // ProfilesDir == OutputDir: out "cpu.pb.gz.html" fine; out = a hard link name to cpu.pb.gz (renamed .html) → isError "input"; a symlink to it → isError
func TestRenderRefusesAnExistingFileUnlessItRenderedIt(t *testing.T) // pre-existing x.html → isError even with overwrite; render y.html, render y.html again with overwrite → written; without overwrite → isError
func TestOverwriteRefusesAPageChangedSinceItWasRendered(t *testing.T) // render, then edit the file, overwrite → isError
func TestRenderIgnoresAgentTolerance(t *testing.T)        // top-level tolerance 0.5 with value 0.62 → still rejected, notice present
func TestRenderPageSizeLimit(t *testing.T)                // MaxPageBytes 1024 → isError naming MaxPageBytes, no file
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run 'Render|RejectedAttempts|AttemptsAreBound|Overwrite'`
Expected: FAIL.

- [ ] **Step 3: Implement the three files.**

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: render_flamegraph with server-recorded attempts and no-clobber writes under the output root"
```

### Task 12: Source references, resolution and evidence (`internal/sourceroot`)

**Files:**
- Create: `internal/profilemodel/source.go`, `internal/profilemodel/source_test.go`
- Create: `internal/sourceroot/{roots.go,resolve.go,git.go,manifest.go}`, `internal/sourceroot/{resolve_test.go,git_test.go,manifest_test.go}`
- Modify: `internal/profilemodel/tree.go` (`frameRef` gains `file string; line int64`, filled in `locationFrames` from `ln.Function.Filename` and `ln.Line`; no output changes)

**Interfaces:**
- Consumes: `View`, `builder`; pmfixture `CPUBasic`, `GPUBasic`.
- Produces (profilemodel):
  - `type SourceRef struct { Kind string; Function, Filename string; Line int64; Module ModuleRef; Value int64 }` with `Kind` `"call_site"` (the node's own frame in each sample through it) or `"gpu_pc"` (labels `gpu_src_file` (a basename), `gpu_src_line`, `gpu_src_func` on samples through the node whose `gpu_src_status` is `resolved`); distinct by (Kind, Filename, Line, Function, Module key); `Value` sums sample values once per sample per ref.
  - `func (v *View) SourceRefs(id NodeID) ([]SourceRef, error)`: Value desc, then Kind, Filename, Line.
- Produces (sourceroot):
  - `type Limits struct { MaxFileBytes int64; MaxLineBytes, MaxContext, MaxWalkEntries int; MaxManifestBytes int64; GitTimeout time.Duration }`; `DefaultLimits()` = `{8 MiB, 512, 50, 200000, 16 MiB, 5s}`.
  - `type Status string`: `build_verified`, `user_selected`, `source_unverified`, `unresolved`, `ambiguous`.
  - `type Prober interface { Head(dir string) (commit, format string, err error); Blob(dir, rel string) (string, error) }`; `func GitProber(timeout time.Duration) Prober` runs `git -C <dir> rev-parse --verify --quiet HEAD^{commit}`, `git -C <dir> rev-parse --show-object-format` and `git -C <dir> rev-parse --verify --quiet HEAD:./<rel>` with env `GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0`, `GIT_NO_REPLACE_OBJECTS=1`, `LC_ALL=C`; it never runs `status`, `diff` or any filter.
  - `type Manifest struct { Format string; BuildIDs, CaptureIDs []string; Commit string; Files map[string]string }` (JSON `format` = `"perf-agent-source-manifest/v1"`, `build_ids`, `capture_ids`, `commit`, `files`: path relative to its source root → SHA-256 hex); `func ReadManifest(path string, max int64) (Manifest, error)`.
  - `type Roots struct`; `func Open(dirs []string, manifests []Manifest, probe Prober, lim Limits) (*Roots, error)` (`os.OpenRoot` each; `probe` nil disables git); `func (r *Roots) Close() error`; `func (r *Roots) Dirs() []string`.
  - `type Resolution struct { Status Status; Reason string; Root, Path string; Candidates []string; Commit string; VerifiedBy string }` (JSON snake_case); `type Line struct { N int64; Text string; Truncated bool }`.
  - `func (r *Roots) Resolve(filename, buildID, captureID string) Resolution`: clean the profile's slash path, drop leading `/` and `..` components, then try suffixes longest first (`a/b/c.c`, `b/c.c`, `c.c`) with `root.Stat` at each root's top; the first length with matches decides: one → resolved, several → `ambiguous` with candidates, none → `unresolved`.
  - `func (r *Roots) ResolveBasename(base, buildID, captureID string) Resolution`: a bounded `fs.WalkDir(root.FS(), ".")` index per root (skipping `.git`, not following symlinks), built once; more than one hit → `ambiguous`, never a guess; over `MaxWalkEntries` → `unresolved` reason `source walk limit reached`.
  - Evidence for a resolved file, in order: a manifest naming the build ID or capture ID and listing the file → `build_verified` (`verified_by: "manifest"`) when the SHA-256 matches, else `source_unverified` reason `file differs from the source manifest`; else git: `Head` and `Blob` succeed and the file's blob id (SHA-1 or SHA-256 per object format over `"blob <len>\x00"` + bytes) equals the HEAD blob → `user_selected` with `commit`; otherwise `source_unverified` with the reason (`not a git checkout`, `not tracked at HEAD`, `file differs from HEAD <commit>`, `git not available`, `git probe disabled`).
  - `func (r *Roots) Lines(res Resolution, line int64, context int) ([]Line, error)`: reads at most `MaxFileBytes`, returns `[line-context, line+context]` clamped, each line cut at `MaxLineBytes` and made valid UTF-8; the opened file's `FileInfo` is returned through `func (r *Roots) Opened() []os.FileInfo` for the render tool's input check.

- [ ] **Step 1: Write the failing tests**

```go
// profilemodel
func TestSourceRefsForACallSite(t *testing.T) // CPUBasic b: [{call_site, "b", "/build/app/src/app.c", 30, module b1, Value 411}]
func TestSourceRefsFromPCSampling(t *testing.T) // GPUBasic k1 under conv: includes {gpu_pc, "k1", "conv.cu", 42, Value 300}
// sourceroot (a t.TempDir() tree: src/app.c, src/util.c, other/src/app.c)
func TestResolveTakesTheLongestSuffix(t *testing.T)      // "/build/app/src/util.c" → src/util.c in root 0
func TestResolveIsAmbiguousAcrossRoots(t *testing.T)     // two roots both holding src/app.c → ambiguous, two candidates
func TestResolveUnresolved(t *testing.T)                 // "/nowhere/x.c" → unresolved
func TestResolveCannotEscapeARoot(t *testing.T)          // "../../etc/passwd", a symlink in the root to an outside file → unresolved, nothing read
func TestBasenameLookupIsAmbiguousNotAGuess(t *testing.T) // "app.c" in src/ and other/src/ → ambiguous; "util.c" → resolved
func TestBasenameWalkLimit(t *testing.T)                 // MaxWalkEntries 1 → unresolved "source walk limit reached"
func TestManifestGivesBuildVerified(t *testing.T)        // manifest {build_ids [b1], files {"src/app.c": sha}} → build_verified; a changed byte → source_unverified "file differs from the source manifest"
func TestGitCleanFileIsUserSelected(t *testing.T)        // skip without git; git init + commit (GIT_CONFIG_GLOBAL=/dev/null, -c user.*): user_selected, commit == HEAD
func TestGitModifiedOrUntrackedIsUnverified(t *testing.T) // edit → "file differs from HEAD"; new file → "not tracked at HEAD"; plain dir → "not a git checkout"
func TestNoProberMeansUnverified(t *testing.T)           // probe nil → source_unverified "git probe disabled"
func TestLinesAreBoundedAndValidUTF8(t *testing.T)       // line 3 context 1 → lines 2..4; a 2 KiB line → Truncated true at 512 bytes; invalid bytes replaced
func TestGitProberNeverRunsStatusOrFilters(t *testing.T) // a fake git on PATH logs its argv: only rev-parse invocations
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/profilemodel/ ./internal/sourceroot/ -run 'SourceRefs|Resolve|Basename|Manifest|Git|NoProber|Lines'`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/profilemodel/ ./internal/sourceroot/ ./internal/flamegraph/ ./cmd/flamegraph/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profilemodel internal/sourceroot
git commit -m "sourceroot: resolve source inside named roots and say what ties it to the binary"
```

### Task 13: The `source` tool

**Files:**
- Create: `internal/perfmcp/tool_source.go`, `internal/perfmcp/source_test.go`
- Modify: `internal/perfmcp/limits.go` (`Source sourceroot.Limits`), `config.go`/`server.go` (open `sourceroot.Roots` from `SourceDirs`, `SourceManifests`, `SourceGit`; register the tool; add `Roots.Opened()` to the input set)

**Interfaces:**
- Consumes: Task 12 `SourceRefs`, `Roots`, `Resolve`, `ResolveBasename`, `Lines`, `ReadManifest`, `GitProber`; Task 6 paging.
- Produces: tool `source` (read-only, idempotent): args `{handle, view, node, context?: 0..MaxContext (default 5), limit?: 1..50 (default 10), cursor?}`; result `{handle, view, node, source_roots, rows: [{kind, function, filename, line, module, build_id, value, resolution, lines}], omitted_*}` plus `notes` on GPU views with `omitted_weight_basis: "value"`; `call_site` refs use `Resolve`, `gpu_pc` refs use `ResolveBasename`; lines only for `build_verified`, `user_selected` and `source_unverified`; with no `--source` roots every row is `unresolved` reason `no source roots: start perf-agent-mcp with --source DIR`.

- [ ] **Step 1: Write the failing tests**

```go
func TestSourceShowsLinesWithTheirStatus(t *testing.T) // CPUBasic b, SourceDirs [dir with src/app.c], no git: row status source_unverified, lines around 30
func TestSourceGPUBasenameAmbiguity(t *testing.T)      // GPUBasic k1 with conv.cu in two subdirs → ambiguous, no lines
func TestSourceWithoutRootsSaysHowToAddThem(t *testing.T)
func TestSourceBuildVerifiedThroughAManifest(t *testing.T) // SourceManifests [manifest for t1 listing src/conv.cu] → build_verified
func TestSourceFilesCountAsInputsForRender(t *testing.T)    // OutputDir == the source dir; after source opened src/app.c, a hard link app.html → src/app.c: render out "app.html" → isError containing "input"
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./internal/perfmcp/ -run Source`
Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run tests**

Run: `go test -race ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/perfmcp
git commit -m "perfmcp: the source tool, inside the source roots and with its evidence status"
```

### Task 14: `cmd/perf-agent-mcp`, the agent guide and the docs

**Files:**
- Create: `cmd/perf-agent-mcp/main.go`, `cmd/perf-agent-mcp/main_test.go`, `internal/perfmcp/guide.md`, `internal/perfmcp/guide.go`, `internal/perfmcp/guide_test.go`, `docs/mcp.md`
- Modify: `internal/perfmcp/server.go` (prompt `perf-agent-guide`, resource `perf-agent://guide` `text/markdown`, initialize `instructions`), `CHANGELOG.md` (Unreleased/Added), `README.md` (one line linking `docs/mcp.md`)

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int` (0 at EOF, 1 on a runtime error, 2 on usage); `main` calls it with `signal.NotifyContext(SIGINT, SIGTERM)` and a `slog` text handler on stderr.
  - Flags: `--profiles DIR` (required), `--source DIR` (repeatable), `--source-manifest FILE` (repeatable), `--output DIR`, `--source-git` (default true), `--limit Name=Value` (repeatable; names are the fields of `perfmcp.Limits` and its nested `Model.`, `Explain.`, `Source.` structs, e.g. `--limit Model.MaxRawBytes=1073741824`; unknown names exit 2 listing the valid ones), `--version`.
  - `instructions` (initialize): `perf-agent-mcp answers questions about perf-agent profiles. Read the perf-agent-guide prompt (or resource perf-agent://guide) before the first call.`
  - `guide.md` (≤ 12 KiB, about 3k tokens), sections: investigation order `open → top → path / gpu_kernels_for / sandwich → facts → source → render_flamegraph`, `diff` for two profiles; handles and views are explicit and immutable; integers are decimal strings; zero vs unavailable; shares name their denominators; truncation, `omitted_weight` overlap and cursors; the GPU rules (a width under `[gpu:launch]` is a sample of launches, not a share of GPU time; `[gpu:launch unsampled]` has no caller and is never attributed; nothing is multiplied by the sample period; executions come only from a checked execlog); diff presence vs interpretation and "absence never proves the cost was eliminated"; source statuses; the explanation schema with an example, `{cN}`/`{n:<id>}` placeholders, "put every number in a claim; free numbers are marked unchecked", `scope: "view"` for `unsampled_portion`, retries with `attempt`.

- [ ] **Step 1: Write the failing tests**

```go
// guide_test.go
func TestGuideIsShortAndStatesTheRules(t *testing.T) // len ≤ 12288; contains "not a share of GPU time", "never multiplied by the sample period", "unavailable is not zero", "{c1}", `"scope": "view"`
func TestGuideIsServedAsPromptAndResource(t *testing.T) // prompts/get perf-agent-guide and resources/read perf-agent://guide return guide.md; initialize instructions mention perf-agent-guide
// main_test.go
func TestRunNeedsAProfilesRoot(t *testing.T)          // no --profiles → 2, stderr names --profiles
func TestRunRefusesUnknownLimits(t *testing.T)        // --limit Bogus=1 → 2, lists "Model.MaxRawBytes"
func TestRunServesOverStdio(t *testing.T)             // pipes: initialize, tools/list (open top path sandwich gpu_kernels_for facts diff source render_flamegraph), open CPUBasic, render to --output; EOF → 0
func TestStdoutCarriesOnlyJSONRPC(t *testing.T)       // every stdout line from the session above parses as JSON-RPC 2.0; logs only on stderr
func TestTheServerLinksNoNetworkingOrProfiler(t *testing.T) // `go list -deps ./cmd/perf-agent-mcp`: none of net, net/http, crypto/tls, github.com/dpsoft/perf-agent/perfagent, github.com/cilium/ebpf, github.com/libbpf/blazesym/go; non-std modules ⊆ {github.com/dpsoft/perf-agent, github.com/google/pprof}
func TestTheServerIsNotShipped(t *testing.T)          // Makefile, Dockerfile.agent, Dockerfile.shim, .github/workflows/release.yml and scripts/third-party-notices.sh do not mention perf-agent-mcp
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -race ./cmd/perf-agent-mcp/ ./internal/perfmcp/ -run 'Guide|Run|Stdout|Links|NotShipped'`
Expected: FAIL.

- [ ] **Step 3: Implement `main.go`, `guide.md`/`guide.go` (`//go:embed`), and write `docs/mcp.md`**: building from source (`go build ./cmd/perf-agent-mcp`), the flags, roots and what may be written, the tools with one example each, the claim schema and the check, source statuses, limits, registering with Claude Code (`claude mcp add perf-agent -- /path/perf-agent-mcp --profiles DIR --output DIR`) and any MCP client, and what is not done (no network, no capture, not shipped in releases). CHANGELOG Unreleased/Added: one entry for `perf-agent-mcp`.

- [ ] **Step 4: Run tests**

Run: `go test -race ./cmd/perf-agent-mcp/ ./internal/perfmcp/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/perf-agent-mcp internal/perfmcp docs/mcp.md CHANGELOG.md README.md
git commit -m "perf-agent-mcp: the server binary, its agent guide and its documentation"
```

### Task 15: Local model-in-the-loop evals (manual, not CI)

**Files:**
- Create: `scripts/mcp-evals.sh`, `internal/perfmcp/evals/questions.json`, `internal/perfmcp/evals_test.go`
- Modify: `docs/mcp.md` (an "Evals" section)

**Interfaces:**
- Consumes: the binary; pmfixture; the explained page's markup (`class="claim"`, `class="unchecked"`, the header pill).
- Produces:
  - `TestWriteEvalFixtures` (skipped unless `PERF_AGENT_MCP_EVAL_DIR` is set): writes `cpu.pb.gz` (CPUBasic), `cpu-new.pb.gz` (CPUBasicNew), `gpu.pb.gz` + `gpu.executions` (GPUBasic + log) into `$PERF_AGENT_MCP_EVAL_DIR/profiles`.
  - `questions.json`: `[{id, prompt, profile, expect: {page: bool, checked_values: ["41.1%"], forbid: ["% of GPU time"]}}]` with at least: explain why `b` is wide in cpu.pb.gz; explain `conv` in gpu.pb.gz (expects the sampling caveat, forbids reading the share as GPU time); what changed between cpu and cpu-new; how many executions ran under `conv` (expects `4`).
  - `TestEvalPages` (skipped unless `PERF_AGENT_MCP_EVAL_DIR` is set): for each question, the page `<dir>/out/<id>.html` exists when `page` is true, its header counts ≥ 1 checked figure, every `checked_values` string appears inside a claim span, no `forbid` string appears; prints unchecked-number counts per page.
  - `scripts/mcp-evals.sh`: builds `perf-agent-mcp` into a temp dir, runs `TestWriteEvalFixtures`, then for each question runs `claude -p "<prompt> Render the explanation to out/<id>.html." --mcp-config <generated json> --allowedTools 'mcp__perf-agent__*'`, then `TestEvalPages`; exits non-zero on failure. It uses synthetic fixtures only, so no capture leaves the machine. The script header states it needs a model and is not run in CI.

- [ ] **Step 1: Write `evals_test.go` and `questions.json`; run `go test ./internal/perfmcp/ -run 'Eval'`.** Expected: both tests SKIP (env unset).

- [ ] **Step 2: Write the script and its docs section; `bash -n scripts/mcp-evals.sh` passes.**

- [ ] **Step 3: Commit**

```bash
git add scripts/mcp-evals.sh internal/perfmcp/evals internal/perfmcp/evals_test.go docs/mcp.md
git commit -m "perfmcp: a local eval suite that replays questions through a real agent over frozen fixtures"
```

### Task 16: Verification and push

**Files:** none created; fixes go in their own commits.

- [ ] **Step 1: Race tests over every package M2 touched and the whole repo**

Run: `go test -race ./internal/... ./cmd/... ./gpu/execlog/` with the CGO variables from Global Constraints, then `go test -race ./...`.
Expected: `ok` for every package; no `FAIL`, no race report.

- [ ] **Step 2: Lint**

Run: `golangci-lint version` (must print 2.11.4; install if exit 127), then `golangci-lint run ./...` with the CGO variables.
Expected: `0 issues.`

- [ ] **Step 3: Notices and generated files**

Run: `BLAZESYM_SRC=/home/diego/github/blazesym scripts/third-party-notices.sh --check` and `git diff --exit-code go.mod go.sum`.
Expected: exit 0 for both (no new module).

- [ ] **Step 4: Existing outputs unchanged.** Run `go test ./internal/flamegraph/ -run Golden` (PASS) and compare `go run ./cmd/flamegraph` HTML and `-folded` output on two profiles under `examples/` between `fb99c3d8` and HEAD (`git worktree add` a temporary checkout at fb99c3d8 under the worktree, not `/tmp`): byte-identical.

- [ ] **Step 5: Protocol conformance with the official SDK client.** In the scratchpad, a throwaway module (never committed) requiring `github.com/modelcontextprotocol/go-sdk v1.8.0` connects with `mcp.CommandTransport` to the built binary (`--profiles` with the eval fixtures, `--output` a scratch dir), lists tools, calls `open` and `top`, and prints the negotiated version. Expected: connects (after its `server/discover` probe falls back), version `2025-11-25`, both calls return text.

- [ ] **Step 6: Manual session smoke with a real client.** `claude mcp add` (or `--mcp-config`) the built binary over the eval fixtures and ask it to explain node `b` and render a page; open the page in a browser and check: header count, a claim value, a marked free number, a frame click updates name, facts, path and explanation together, a chip copies a prompt with digest and node ID. Run one question from `scripts/mcp-evals.sh`. Record what was checked in the PR body.

- [ ] **Step 7: Push**

```bash
git push -u origin ai-mcp-m2
```

Expected: the branch is on the remote; the controller opens the draft PR. Never merge.

---

## Self-review

- **Spec coverage.** §7.1 process/stdio/read-only (Tasks 5, 6, 14), never starts the profiler (Task 14 dependency test), library choice (decision table), roots and `os.Root` (Tasks 6, 11, 12), never overwrite inputs (Task 11, Task 13 input set), limits (Tasks 5, 6, 9, 11, 12, 14). §7.2 every tool: `open` (6), `top`/`path`/`facts`/`gpu_kernels_for` (7), `sandwich`/`diff` (8), `source` (12, 13), `render_flamegraph` (11); truncation, cursors, prescriptive errors (6-8); agent guide (14); source evidence statuses and `gpu_src_file` basenames (12). §7.3 claim shape, free numbers, every validation rule and the listed deterministic tests (9), engine-rendered numbers and escaping (10), attempts and the rejected section from the server's record (11), atomic writes (11), honest labels (10). §7.4 marks, joint selection, facts-only frames, chips, notes and quality always visible (10). §9 constraints (Global Constraints, Task 16). §10 deterministic tests throughout, protocol tests over pipes (5, 6, 14), manual evals (15). §12 Q2: source-only (Task 14 test).
- **M1 obligations.** R10 (Task 6 `TestHandlesAreEvidenceNotPolicy`, per-server store); #290 (Task 9 `TestViewLevelFactsAreNotNodeClaims`); flat diff (Tasks 4, 8); 2^53 (Tasks 1, 6, 7, 8, 9, 10); structured rounding (Task 1); truncation fields (Tasks 6-8, 13); source statuses, roots, basenames, limits (Tasks 12, 13); render rules (Tasks 9-11); read-only, offline, no Agent (Task 14).
- **Type consistency.** `Fact.Canonical` (T1) feeds `wireFact` (T6), `EngineValue.Value` (T9) and page display (T10). `CoreFacts`, `KernelsBelow`, `Notes`, `UnsampledPortion` (T2) are used by T7, T9, T10. `FunctionRef`/`Sandwich` (T3) by T7/T8. `Flatten`/`UnionID` (T4) by T8. `explain.Result`/`RejectedAttempt` (T9) by T10/T11. `sourceroot.Roots.Opened` (T12) by T13. `perfmcp.Limits` grows in T7, T8, T11, T13 and is flag-mapped in T14.
- **Open question for the user:** the `source` tool runs read-only `git rev-parse` inside each `--source` checkout to report `user_selected`; for a git worktree that reads the main repository's object store outside the named root (only a commit id and a match result reach the response). `--source-git=false` disables it, and then nothing is ever `user_selected`.
