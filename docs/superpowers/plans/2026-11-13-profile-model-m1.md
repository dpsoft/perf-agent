# Profile model (M1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An offline Go model of a pprof profile with separate location, function and path-node identity, path facts with explicit status, quality (including perturbation), and a bilateral diff, exposed through `cmd/flamegraph -facts`, `-list-nodes` and `-diff`.

**Architecture:** New package `internal/profilemodel` builds a tree straight from `*profile.Profile` (before any fold-by-name), keyed by function identity per path, and computes facts, quality and diffs from it. It does not import `internal/flamegraph` (the domain classifier is injected), so `internal/flamegraph` can import it for the diff page. `cmd/flamegraph` wires the flags. The existing single-profile HTML path is untouched (its migration onto the model is M1b, a separate plan).

**Tech Stack:** Go 1.26, `github.com/google/pprof/profile`, `gpu/execlog`, `internal/foldedstacks`, `internal/framename`, testify (`assert`/`require`) as the existing tests use.

**Spec:** `docs/superpowers/specs/2026-11-13-ai-flamegraph-design.md` §6 (read §4 for the code facts it rests on).

## Global Constraints

- Go 1.26; invoke the modern-go guidelines skill before writing Go.
- Tests run with `-race`; lint with `golangci-lint` v2.11.4 (install with `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4`; a missing binary exits 127 and is not a pass).
- No network access from the model or the CLI paths added here.
- No new third-party dependency (if one becomes necessary, regenerate `THIRD_PARTY_NOTICES.md` with `scripts/third-party-notices.sh`).
- Existing CLI behaviour and output formats do not change; new flags are additive and listed in `CHANGELOG.md` under Unreleased.
- Never count pprof samples or execlog rows as executions or launches; never multiply by `gpu_sample_period`.
- Zero and unavailable are distinct values in code and in JSON; a zero denominator gives `unavailable: zero total`, never 0 or NaN.
- "No mapping" means `Mapping == nil || Mapping.File == ""` (synthetic and fallback frames sit on the first mapping with an empty file).
- Comments say what the code does and why; no issue numbers or release history in comments.
- Commits: no Co-Authored-By, no AI attribution, no session URLs. Push to `ai-flamegraph-design` (draft PR #262); never merge.

## Review Focus

1. **GPU profiles put every synthetic frame on `Mapping[0]` with an empty file.** Treating it as a module would make every `[gpu:…]` frame one module and break by-name matching. Test in Task 3.
2. **A view total of 0** (all samples zero at the index, or none). Shares are `unavailable: zero total`; JSON never contains NaN. Test in Task 4.
3. **Execlog rows with `Value == 0`** name an observed execution and count; value conservation still holds because they add 0. Test in Task 2 and Task 4.
4. **A name path that matches several nodes** (same names, different modules, or a collision). `-facts a;b;c` refuses and lists the candidate IDs. Test in Task 8.
5. **Multi-type profiles** (`alloc_objects` + `alloc_space`): the view's sample index decides every value, and a diff of views with different sample types, units or options is refused. Test in Task 6.

---

### Task 1: Share the GPU sampling note and sample-index rule

**Files:**
- Modify: `internal/foldedstacks/fold.go:637-662` (`gpuSamplingWarnings`), `fold.go:680` (`chooseSampleIndex`)
- Test: `internal/foldedstacks/fold_test.go`

**Interfaces:**
- Produces: `func SamplingNotes(unsampled, total int64, period string, periodSamples int) []string` and `func ChooseSampleIndex(p *profile.Profile) int` in package `foldedstacks`. `gpuSamplingWarnings` calls `SamplingNotes`; `chooseSampleIndex` is renamed and its callers updated. Output is unchanged byte for byte.

- [ ] **Step 1: Write the failing test** `TestSamplingNotesMatchesFoldWarnings`: a GPU profile with one sample under `[gpu:launch unsampled]` (30) and one sampled stack labelled `gpu_sample_period=8` (70); assert `Fold(...).Warnings` contains exactly `SamplingNotes(30, 100, "8", 1)`, and `SamplingNotes(0, 100, "", 0) == nil`.
- [ ] **Step 2: Run** `go test -race ./internal/foldedstacks -run TestSamplingNotesMatchesFoldWarnings` → FAIL (undefined: SamplingNotes).
- [ ] **Step 3: Implement** by moving the two `fmt.Sprintf` bodies into `SamplingNotes`; rename `chooseSampleIndex` → `ChooseSampleIndex`.
- [ ] **Step 4: Run** `go test -race ./internal/foldedstacks ./internal/flamegraph ./cmd/flamegraph` → PASS (render tests prove the text did not change).
- [ ] **Step 5: Commit** `foldedstacks: expose the GPU sampling note and sample-index rule`.

### Task 2: Load a profile, its digest, limits and a checked execlog

**Files:**
- Create: `internal/profilemodel/doc.go`, `internal/profilemodel/load.go`
- Test: `internal/profilemodel/load_test.go`, `internal/profilemodel/fixtures_test.go`

**Interfaces:**
- Produces:
  ```go
  type Digest [32]byte                 // SHA-256 of bytes as read
  func (d Digest) String() string      // lowercase hex
  type Availability struct{ OK bool; Reason string }
  const (
      ReasonNoSidecar        = "no sidecar"
      ReasonDigestMismatch   = "digest mismatch"
      ReasonSampleCount      = "sample count mismatch"
      ReasonMetadataMismatch = "metadata mismatch"
      ReasonValueMismatch    = "value mismatch"
      ReasonCorrupt          = "corrupt"
  )
  type Limits struct{ MaxDecompressedBytes, MaxSamples, MaxLocations, MaxDepth int }
  func DefaultLimits() Limits        // values are the builder's call; documented in doc.go
  var ErrLimit = errors.New("profilemodel: input exceeds limit") // wrapped with the limit's name
  type Handle string                   // "<profile digest hex>" or "<profile digest hex>+<execlog sha hex>"
  type Profile struct {
      Handle     Handle
      Digest     Digest
      CaptureID  string                // from "perf-agent.capture_id=", "" if absent
      Source     *profile.Profile
      Executions Availability
      execOf     [][]uint64            // per sample index: execution ordinals; nil unless Executions.OK
  }
  func Load(profilePath, execlogPath string, lim Limits) (*Profile, error) // execlogPath "" = none
  func FromBytes(raw []byte, execlog []byte, lim Limits) (*Profile, error) // execlog nil = none
  ```
- Decompression is bounded by `MaxDecompressedBytes` before `profile.ParseData` sees the bytes; sample, location and depth limits are checked after parsing, before any tree is built.
- The log is accepted only if, in order: `execlog.Read` succeeds; `Trailer.Verify(raw, len(p.Sample))` passes; `Header.SampleType` equals the profile's GPU sample type string and `Header.CaptureID` equals `CaptureID` when both are non-empty; for every sample index, the sum of its rows' `Value` equals `Sample.Value` at the GPU index. Rows are collected into a local map and copied into `execOf` only after all checks pass. A bad sidecar never fails `Load`; it sets `Executions`.

- [ ] **Step 1: Fixture helpers** in `fixtures_test.go`: `gpuProfile(samples ...fx) []byte` (sample type `gpu/nanoseconds`, all synthetic frames on `Mapping[0]` with `File: ""`, labels per sample, serialized bytes); `cpuProfile(...)`; `execlogFor(t, raw []byte, hdr execlog.Header, rows []execlog.Row, nSamples int) []byte` (writes with `execlog.Create` in `t.TempDir()`, `Finish(uint64(nSamples), execlog.ProfileSHA256(raw))`, returns the bytes).
- [ ] **Step 2: Write the failing tests**:
  - `TestLoadDigestsTheBytesRead` → `Digest == sha256.Sum256(raw)`, `Executions == {false, "no sidecar"}`, `Handle == Digest.String()`.
  - `TestLoadAcceptsACheckedExeclog`: rows (sample 0, exec 0, value 100), (0, 1, 200), (1, 1, 0) for samples valued 300 and 0 → `OK`, `execOf[0] == {0,1}`, `execOf[1] == {1}`, handle has a `+` suffix.
  - `TestLoadRejectsAnotherProfilesLog` → `digest mismatch`; `TestLoadRejectsSampleCountMismatch` → `sample count mismatch`; `TestLoadRejectsTruncatedLog` → `corrupt`.
  - `TestLoadRejectsMetadataMismatch`: header `SampleType: "cpu"` → `metadata mismatch`; header capture ID `other` vs comment `7f3a` → `metadata mismatch`.
  - `TestLoadRejectsValueMismatch`: rows summing to 250 for a sample of 300 → `value mismatch`, `execOf == nil`.
  - `TestLoadEnforcesLimits`: `MaxSamples: 1` with two samples → `errors.Is(err, ErrLimit)` and the message names `MaxSamples`; same for depth.
- [ ] **Step 3: Run** `go test -race ./internal/profilemodel` → FAIL.
- [ ] **Step 4: Implement** `load.go`; `doc.go` states: facts come from the profile and a checked log only; nothing is inferred or scaled.
- [ ] **Step 5: Run** → PASS. **Step 6: Commit** `profilemodel: load a profile with limits and a checked executions log`.

### Task 3: Location, function and path-node identity

**Files:**
- Create: `internal/profilemodel/identity.go`, `internal/profilemodel/tree.go`
- Test: `internal/profilemodel/tree_test.go`

**Interfaces:**
- Consumes: `Profile` (Task 2); `foldedstacks.ChooseSampleIndex` (Task 1); `framename.Format`, `framename.IsAddressOnly`.
- Produces:
  ```go
  type ModuleRef struct {
      File     string // "" = no mapping (nil, or Mapping.File == "")
      BuildID  string
      Sentinel string // "[kernel]" or "[jit]" when the mapping file is one of them
  }
  type LocationID struct { Module ModuleRef; Offset uint64; Inline int } // one PC, one inline level
  type FunctionID struct {
      Module    ModuleRef
      Name      string // display name, same rules as foldedstacks lineName/addressName
      Filename  string // used only when Module.File == "" and no sentinel
      StartLine int64  // likewise
  }
  func (f FunctionID) Key() string      // exact: build-id (else file, else sentinel) + name (+ filename/startline when no module)
  func (f FunctionID) NameKey() string  // by-name: base(file) + name; sentinel + name; or name + filename/startline
  type NodeID string                    // 16 lowercase hex chars of SHA-256 over the root-to-node FunctionID.Key() sequence
  type Node struct {
      ID                 NodeID
      Function           FunctionID
      CollisionPossible  bool          // no module, no filename, no start line
      Parent             *Node
      Children           []*Node       // sorted by Cum desc, then Function.Key()
      Depth              int
      CallSites          []LocationID  // distinct locations that produced this node
      Cum, Self, Inexact int64
      samples            []int         // sample indices contributing to Cum
      zeroSamples        []int         // zero-value samples whose stack passes through the node (for executions only)
  }
  type ViewOptions struct {
      SampleIndex        int  // -1 = foldedstacks.ChooseSampleIndex
      CollapseVendorRuns bool // rejected in M1 with ErrUnsupportedView
  }
  type View struct {
      Handle             Handle
      Options            ViewOptions
      SampleType, Unit   string
      Index              int
      Root               *Node // synthetic "all"
      Total              int64
      nodes              map[NodeID]*Node
  }
  var ErrUnsupportedView = errors.New("profilemodel: view option not supported yet")
  func (p *Profile) View(opts ViewOptions) (*View, error)
  func (v *View) Node(id NodeID) (*Node, bool)
  func (v *View) Walk(fn func(*Node) bool) // pre-order; false stops descent
  func (v *View) Path(n *Node) []*Node     // root excluded, root-first
  ```
- One node per inline level, inlined chain root-first, as `foldedstacks.framesOf` emits names, so display names match the existing page. Zero-value samples at the index add no value but are recorded in `zeroSamples` on the nodes they pass through, so their executions still count (Task 4); empty stacks go under `[unknown]`, as `Fold` does.

- [ ] **Step 1: Write the failing tests** (each fixture also added to `allFixtures()` in `fixtures_test.go`):
  - `TestTreeInvariantsHoldOnEveryFixture`: every node `Cum == Self + Σ children Cum`; `Root.Cum == Total == Σ sample values`.
  - `TestRecursionGetsDistinctNodesPerDepth`: `main→a→b→a→b` (10) → four distinct IDs under `main`, each `Cum == 10`.
  - `TestSameNameInDifferentModulesStaysSeparate`: `foo` in `libx.so` (build-id `x`) and `liby.so` (`y`) under `main` → two nodes, different `Key()`, same `Name`.
  - `TestSeveralPCsOfOneFunctionAreOneNode`: `foo` at offsets `0x10` (30) and `0x20` (70), same module → one node `Cum == 100`, two `CallSites`.
  - `TestEmptyFirstMappingIsNoModule`: GPU fixture → every node `Function.Module.File == ""`.
  - `TestSentinelMappingsIdentifyByName`: two frames `foo` on `[jit]` and `foo` on `libfoo.so` → two nodes; the `[jit]` one has `Module.Sentinel == "[jit]"`.
  - `TestUnmappedSameNameUsesFilenameAndStartLine`: two `__init__` with different `Filename` → two nodes, `CollisionPossible == false`; two with no filename/start line → one node, `CollisionPossible == true`.
  - `TestInlineFramesBecomeNodesRootFirst`: one location with `Line{inner}, Line{outer}` → nodes `outer` then `inner`.
  - `TestNodeIDsAreStableAcrossLoads`; `TestCollapsedViewIsRejected` (`ErrUnsupportedView`).
- [ ] **Step 2: Run** `go test -race ./internal/profilemodel` → FAIL. **Step 3: Implement** `identity.go`, `tree.go`. **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `profilemodel: separate location, function and path-node identity`.

### Task 4: Path facts with explicit status

**Files:**
- Create: `internal/profilemodel/facts.go`
- Test: `internal/profilemodel/facts_test.go`

**Interfaces:**
- Consumes: Tasks 2–3; `foldedstacks.DefaultInexactLabels`, `SamplingNotes`, `UnsampledLaunchFrame`, `SamplePeriodLabel`.
- Produces:
  ```go
  type Status string // "ok", "zero", "unavailable"
  type Fact struct {
      Value       float64 `json:"value"`
      Unit        string  `json:"unit"`                  // sample unit, "ratio" or "count"
      Status      Status  `json:"status"`
      Reason      string  `json:"reason,omitempty"`      // required when unavailable
      Denominator string  `json:"denominator,omitempty"` // "view_total" or "parent:<node id>"
      Rounding    string  `json:"rounding"`              // engine policy, e.g. "0.001 ratio", "3 significant figures"
  }
  type Classifier func(name, module string) string // domain key; nil = "unknown"
  type Serialization struct{ True, False, Unknown, Missing Fact } // value under the node per gpu_serialized state
  type PathFacts struct {
      Handle              Handle
      View                string   // "<type>/<unit>#<index>"
      Node                NodeID
      ParentNode          NodeID   `json:",omitempty"`
      Path                []string // display names root-first
      Cumulative, Self    Fact
      ShareOfTotal        Fact
      ShareOfParent       Fact
      ExecutionsBelow     Fact
      KernelNamesObserved Fact
      InexactPortion      Fact
      UnsampledPortion    Fact     // GPU views only; else unavailable "not a GPU view"
      Serialization       Serialization // GPU views only; else all unavailable
      SamplePeriods       []string
      Domain              string
      DomainInferred      bool     // always true
      CollisionPossible   bool
      Notes               []string // SamplingNotes for GPU views
  }
  func RoundingFor(metric string) string // the engine's per-metric policy, single source for §7.3
  func (v *View) Facts(id NodeID, classify Classifier) (PathFacts, error)
  ```
- A GPU view is one whose sample type is `gpu`. `ExecutionsBelow` counts distinct ordinals in `execOf[s]` over `node.samples` **and** over zero-value samples whose stack passes through the node (kept aside in Task 3 for this purpose); `unavailable` with `Profile.Executions.Reason` when the log is not OK.

- [ ] **Step 1: Write the failing tests**:
  - `TestAggregatedExecutionsAreCountedFromTheLog` (3 executions → `{3,"count","ok"}`; no log → `unavailable/no sidecar`; other log → `digest mismatch`).
  - `TestPCSamplingCountsAnExecutionOnce`: rows on samples 0,1,2 all `Execution: 7`, one with `Value: 0` → `ExecutionsBelow.Value == 1`.
  - `TestSamplingSemanticsCarryTheNoteUnscaled`: sampled 70 under `[gpu:launch]` with period 8, unsampled 30 → `Cumulative == 70` (not 560), `Notes == SamplingNotes(30,100,"8",n)`, `UnsampledPortion == 30`, `SamplePeriods == ["8"]`.
  - `TestInexactPortionCountsHeuristicUnmatchedAmbiguous`: 10/20/5 inexact of 100 → `35`.
  - `TestSerializationFourStates`: samples `gpu_serialized` true 10, false 50, unknown 30, absent 10 → `True 10, False 50, Unknown 30, Missing 10`; CPU view → all `unavailable`.
  - `TestZeroIsNotUnavailable`: CPU node with `Self` 0 → `{0,"zero"}` next to `ExecutionsBelow` `unavailable/no sidecar`.
  - `TestZeroTotalSharesAreUnavailable`: only sample is 0 → `ShareOfTotal{unavailable,"zero total"}`; `json.Marshal` succeeds and contains no `NaN`.
  - `TestDistinctKernelNamesAreObservedNames`: `k1,k1,k2` → 2.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/profilemodel` → PASS.
- [ ] **Step 5: Commit** `profilemodel: path facts with ok, zero and unavailable status`.

### Task 5: Quality

**Files:**
- Create: `internal/profilemodel/quality.go`
- Test: `internal/profilemodel/quality_test.go`

**Interfaces:**
- Produces:
  ```go
  type Quality struct {
      JoinExact, JoinHeuristic, JoinUnmatched Fact // shares of the view total; unavailable outside GPU views
      Ambiguous, GraphRefused                 Fact
      UnsampledLaunches                       Fact
      SamplePeriods                           []string
      Serialized                              Serialization // as shares of the view total
      AddressOnly                             Fact          // share whose leaf is an address-only or unknown frame
      AddressOnlyByModule                     map[string]Fact // keyed by module file base name ("" = no module)
      CollisionPossible                       Fact          // share under CollisionPossible nodes
      Executions                              Availability
  }
  func (v *View) Quality() Quality
  func (v *View) NodeQuality(id NodeID) (Quality, error) // restricted to the node's samples
  ```

- [ ] **Step 1: Write the failing tests**: `TestQualityJoinShares` (65/10/20/5 → 0.65/0.10/0.20), `TestQualitySerializedShares` (as Task 4 → 0.1/0.5/0.3/0.1), `TestAddressOnlyShareByModule` (`libcuda.so.1+0x1b71c6` leaf 40 of 100 → 0.4), `TestCPUViewJoinFactsAreUnavailable`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `profilemodel: profile and node quality, including perturbation`.

### Task 6: Bilateral diff

**Files:**
- Create: `internal/profilemodel/diff.go`
- Test: `internal/profilemodel/diff_test.go`

**Interfaces:**
- Consumes: `View`, `Quality` (Tasks 3, 5).
- Produces:
  ```go
  type Presence string       // "both", "only_base", "only_new"
  type Match string          // "exact", "by_path", "by_name", "ambiguous"
  type Interpretation string // "comparable", "not_comparable"
  type DiffNode struct {
      Base, New       *Node      // nil on the missing side
      Name            string
      Presence        Presence
      Match           Match
      Candidates      []NodeID   // when Match == "ambiguous"
      Interpretation  Interpretation
      Why             string     // set when not_comparable
      BaseCum, NewCum int64
      Combined        int64      // BaseCum + NewCum: the drawing weight, additive
      DeltaAbs        int64      // NewCum - BaseCum
      DeltaShare      Fact       // new share - base share; unavailable if a total is 0
      DeltaPerUnit    Fact       // unavailable unless both work counts are positive and finite
      Children        []*DiffNode // sorted by |DeltaAbs| desc, then Name
  }
  type WorkCounts struct{ Base, New float64 } // zero value = not supplied
  const (
      MaxAddressOnlyShift = 0.10
      MaxJoinShift        = 0.10
      MaxSerializedShift  = 0.10
  )
  type Diff struct {
      BaseHandle, NewHandle   Handle
      SampleType, Unit        string
      BaseTotal, NewTotal     int64
      BaseQuality, NewQuality Quality
      QualityWarnings         []string
      Root                    *DiffNode
  }
  var ErrIncompatibleViews = errors.New("profilemodel: views differ in sample type, unit or options")
  func Compare(base, new *View, work WorkCounts) (*Diff, error)
  ```
- Matching per parent, in order: `exact` on `FunctionID.Key()` when both sides have a build-id; `by_path` on file + name when either side lacks a build-id (never `exact`); `by_name` on `NameKey()` for the leftovers. A leftover with several candidates on the other side — in either direction — is `ambiguous`, keeps its own node and lists `Candidates`.
- `not_comparable` when the node's module `AddressOnlyByModule` shift, the `JoinExact` shift (GPU), or the `Serialized.True` shift exceeds its constant, or the sample periods differ; `Why` names the first cause.

- [ ] **Step 1: Write the failing tests**:
  - `TestDiffTotalsEqualInputs` (100 vs 80 → totals, root `DeltaAbs == -20`).
  - `TestUnionGeometryIsAdditive`: on every diff fixture, each node's `Combined == Σ children Combined + (BaseSelf + NewSelf)`.
  - `TestRebuiltBinaryMatchesByName` (build-id `a` vs `b` → all `both`, `by_name`) and `TestUnchangedLibraryMatchesExactly` (same build-id → `exact`) in one profile pair.
  - `TestMissingBuildIDIsNeverExact`: no build-id on either side, same path → `by_path`.
  - `TestMovedModulePathMatchesByName`: `/opt/a/libfoo.so` vs `/opt/b/libfoo.so`, different build-ids → `by_name`.
  - `TestAmbiguityInBothDirections`: one `foo` vs two candidates, and two vs one → `ambiguous` with `Candidates`, no merge.
  - `TestRemovedPathStaysInTheUnion`: kernel only in base, equal quality → `only_base`, `comparable`, `DeltaAbs == -37`.
  - `TestDegradedSymbolsArePresentButNotComparable`: base `foo` symbolized, new same module address-only → `only_base` **and** `not_comparable`, `Why` names the module.
  - `TestPerturbationShiftIsNotComparable`: base `gpu_serialized=false` everywhere, new 50% `true` → affected nodes `not_comparable`.
  - `TestDiffRefusesIncompatibleViews`: cpu vs gpu, and equal type with different `ViewOptions` → `ErrIncompatibleViews`.
  - `TestPerUnitDeltaNeedsPositiveFiniteWork`: none, `{0,100}`, `{+Inf,1}` → `unavailable`; `{100,100}` → `ok`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/profilemodel` → PASS.
- [ ] **Step 5: Commit** `profilemodel: bilateral diff with explicit matching and comparability`.

### Task 7: Diff page

**Files:**
- Create: `internal/flamegraph/diffpage.go`, `internal/flamegraph/diffpage.tmpl` (embedded)
- Test: `internal/flamegraph/diffpage_test.go`

**Interfaces:**
- Consumes: `profilemodel.Diff` (Task 6); palette variables in `internal/flamegraph/assets.go`.
- Produces: `func WriteDiffPage(w io.Writer, d *profilemodel.Diff, title string) error` — self-contained HTML (`html/template`, no external assets):
  - icicle of the union, width = `Combined / root Combined`, colour blue (less) → grey → red (more) by `DeltaShare` (grey when unavailable);
  - `only_base` hatched, `only_new` outlined, `not_comparable` marked; every node's tooltip shows base and new values;
  - legend reads "less / more accumulated <sample type> time", never "faster" or "slower";
  - a table of `only_base` and `only_new` paths with delta, interpretation and `Why`;
  - `QualityWarnings` and GPU sampling notes when present.

- [ ] **Step 1: Write the failing tests**: `TestDiffPageNeverSaysFasterOrSlower`, `TestDiffPageListsRemovedPathsWithInterpretation`, `TestDiffPageEscapesNames` (`<script>` frame escaped), `TestDiffPageHasNoExternalAssets`.
- [ ] **Step 2: Run** `go test -race ./internal/flamegraph -run DiffPage` → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `flamegraph: diff page over the union of both profiles`.

### Task 8: `cmd/flamegraph` flags

**Files:**
- Modify: `cmd/flamegraph/main.go`
- Create: `cmd/flamegraph/model.go`
- Test: `cmd/flamegraph/model_test.go`
- Modify: `docs/usage.md` (flamegraph section), `CHANGELOG.md` (Unreleased → Added)

**Interfaces:**
- Consumes: Tasks 2–7; `flamegraph.Classify(name, module).Info().Key` as the `Classifier`.
- New flags (additive): `-execlog PATH`, `-facts NODE` (node ID or root-first name path `a;b;c`; several matches → exit 1 listing candidate IDs), `-list-nodes N` (N widest nodes as JSON lines `{id, path, cum}`), `-diff BASE` with `-execlog-base PATH` (page to `-o`, default `<new>.diff.html`, plus `.json`). `-sample-index` applies to all. Combining any of them with `-folded` or `-fuse` exits 2. No sidecar is ever found implicitly.

- [ ] **Step 1: Write the failing tests** against an extracted `run(args []string, stdout, stderr io.Writer) int`: `TestFactsByNodeID`, `TestFactsByNamePath`, `TestFactsAmbiguousNamePathListsCandidates` (exit 1), `TestListNodes`, `TestDiffWritesPageAndJSON` (JSON totals match inputs), `TestModelFlagsRejectFolded` (exit 2).
- [ ] **Step 2: Run** `go test -race ./cmd/flamegraph` → FAIL. **Step 3: Implement** (refactor `main` into `run` without changing existing flags). **Step 4: Run** `go test -race ./cmd/flamegraph ./internal/...` → PASS.
- [ ] **Step 5: Document** the flags in `docs/usage.md`; CHANGELOG Unreleased "Added".
- [ ] **Step 6: Commit** `flamegraph: -facts, -list-nodes and -diff over the profile model`.

### Task 9: Verify and publish to the draft PR

- [ ] **Step 1:** `go test -race ./internal/... ./cmd/flamegraph` → PASS.
- [ ] **Step 2:** `golangci-lint run ./internal/profilemodel/... ./internal/foldedstacks/... ./internal/flamegraph/... ./cmd/flamegraph/...` → 0 issues.
- [ ] **Step 3:** Real-profile smoke test on checked-in captures in the main checkout (`python3-202604021640-on-cpu.pb.gz` and another on-CPU capture of the same program): `-list-nodes 5`, `-facts <id>`, `-diff`; open the page and check removed paths, interpretation and legend.
- [ ] **Step 4:** Existing HTML unchanged: render one GPU and one CPU profile with `cmd/flamegraph` on `main` and on the branch, `cmp` → identical.
- [ ] **Step 5:** `git push` to `ai-flamegraph-design`; update PR #262's description. Do not mark ready, do not merge.
