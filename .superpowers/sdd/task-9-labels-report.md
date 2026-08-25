# Task 9 — the projection label set

Branch `feat/pc-projection-labels`, one commit off `main`
(`b1468a7a`, the Task 8b merge). Plan:
`docs/superpowers/plans/2026-08-25-gpu-pc-sampling.md`, Task 9.

Five files: `gpu/projection.go`, `gpu/joinhealth.go`,
`gpu/projection_test.go`, and the two drivers
(`cmd/gpu-stub-profile/main.go`, `cmd/gpu-cuda-profile/main.go`) so the new
counter actually reaches an operator.

**`gpu_serialized` is NOT in this commit.** See §7. Everything else Task 9
lists is.

---

## 1. The labels, and when each is emitted

Every one of these is written **after** the `Tags` copy, on a clone of the
execution's shared label map, so a producer-supplied tag can never forge one.
Frames are untouched.

| label | on | when |
| --- | --- | --- |
| `gpu_stall` | PC-derived samples | unchanged — when the producer named a reason |
| `gpu_pc` | PC-derived samples | unchanged — subject to the new cardinality budget (§3) |
| `gpu_pc_attrib` | PC-derived samples | **unconditional** |
| `gpu_src_status` | PC-derived samples | **unconditional** |
| `gpu_src_file` | PC-derived samples | only under `resolved`, **basename only** (§2) |
| `gpu_src_line` | PC-derived samples | only under `resolved` |
| `gpu_src_func` | PC-derived samples | only under `resolved` |

"PC-derived sample" means a sample projected from a `GPUPCSample`. An
execution carrying no PC samples projects one sample with none of these — it
has nothing to say about an instruction that was never sampled.

### `gpu_src_status` — four values, decided nowhere but the store

`ModuleStore.Resolve(crc, functionIndex, pcOffset)` returns a `Resolution`
whose `Status()` is one of `resolved` / `no-lineinfo` / `no-module` /
`unmapped`, and the projection **spells** it rather than deciding it. That
was already structurally enforced by Task 4 (`Resolution` has unexported
fields and no exported constructor, and its location is reachable only
through `Source`, which returns `ok` in the same expression as the data), and
this task did not weaken it: `setSourceLabels` takes the `ok` and returns
early, so there is no branch anywhere that could pair a file with
`no-lineinfo`.

It is unconditional for `gpu_join`'s reason, and that is the property the
brief singled out: an absent label must never be readable as a positive
answer. Absence here would read as "this sample was never source-mapped",
which is a fourth fact the four values do not include and the one a reader
would wrongly assume.

**A nil `ModuleStore` is answered by an empty store, not by the projection.**
`ProjectionConfig.Modules` is nil for both shipping drivers today (nothing
feeds cubins yet). Rather than branch — which would put a second decision
site for the enum into the codebase, and the first place a fifth value could
appear — `ProjectExecutionsWith` constructs `NewModuleStore(ModuleStoreConfig{})`
and asks it. An empty store answers `no-module` to everything, which is
exactly true: no usable module bytes exist for that CRC. This is the same
accounting Task 8b chose for a nil store at the join
(`PCJoinStats.GroupsUnresolvedName`), for the same reason — a missing store
must not look like a healthy run.

### `gpu_pc_attrib` — and what a join bug looks like

Read straight off `ExecutionView.PCAttrib`, which Task 8b decides. Also
unconditional.

The one addition: a view carrying PC samples with an **empty or fabricated**
`PCAttrib` renders `"unset-pc-attrib"` rather than being omitted. That is not
a fifth value of the label's domain — it is what a join bug looks like from
outside, in the same shape and for the same reason as `SrcStatus.String`'s
`"unset-src-status"`. Omitting the label instead would hide the bug behind
the single reading that must never be reachable by accident (`exact` is the
only one of the four that is not an inference). The conformance suite's
`assertPCAttribAccompaniesSamples` already makes this unreachable through the
real join; this is the belt for the profile a consumer actually reads.

---

## 2. `gpu_src_file` carries the basename; the directory goes nowhere

`srcFileBase` reduces the line table's file name to its last path element.

This is not cosmetic. The Task 1 fixtures resolve to
`/tmp/perf-agent-cubin-fixtures/single.cu` — a real build-host absolute path,
straight out of the cubin's DWARF. Carrying it costs three ways: it varies
per build, so the same kernel built twice yields two distinct label values
for one file; it is a long string in the pprof string table that no reader
acts on; and it leaks the build environment's layout into a profile that may
be shared outside the organisation that produced it. The basename beside
`gpu_src_func` is enough to find the line in the repository it came from,
which is the only thing a reader does with it.

`path.Base`, not `filepath.Base`: the separator in a cubin's line table is
the **build host's**, and the result must not depend on the agent's own OS.
The agent is Linux-only and nvcc emits `/` there. A Windows-built cubin's
backslashes would not be split; that is stated rather than guessed at,
because a filename may legally contain a backslash on Linux and splitting on
it would corrupt a legitimate name.

`TestProjectionSrcFileIsABasenameNotABuildHostPath` asserts the basename
**and** that no other label smuggles the directory back in.

---

## 3. The `gpu_pc` cap

`ProjectionConfig.MaxDistinctPCLabels`, tracked per `ProjectExecutions` call
by `pcLabelBudget`. Past the ceiling, `gpu_pc` is dropped and every dropped
sample is counted in `ProjectionStats.PCLabelsSuppressed` (JSON
`projection_pc_labels_suppressed` — the design's `ProjectionPCLabelsSuppressed`).

**Only `gpu_pc` gives way.** `gpu_stall`, `gpu_pc_attrib` and the whole
`gpu_src_*` family are untouched, and the sample keeps its full share of the
execution's duration — the suppression costs a label, never a measurement.
`gpu_pc` is the most numerous label in the set (one value per distinct
sampled instruction) and the least actionable alone: a bare offset tells a
reader nothing that the stall reason and the source line do not tell them
better.

**An offset already emitted is always readmitted.** The cap bounds the pprof
**string table**, which stores one entry per distinct label *value*; a repeat
costs nothing there. So the rule is "admit no NEW value past the ceiling",
not "emit nothing past the ceiling". Distinct values are bounded at exactly
the ceiling either way, and the second reading would discard strictly more of
the profile for no saving. Suppression is counted per **sample**, because the
sample is what went out incomplete.

`%#x` is injective over `uint64`, so the set of admitted offsets is exactly
the set of distinct label values — the budget's `map[uint64]struct{}` is not
an approximation of what it bounds.

### The ceiling: 20,000, and on what basis

`defaultMaxDistinctPCLabels = 20_000`. **Reasoned, not measured.** The number
is the design's own pathological estimate: 20,000 distinct PCs ≈ 400 KB of
string table pre-gzip, ~140 KB after, which the design calls *tolerable*.
Setting the ceiling at the top of the range the design already accepted means
it cannot fire on any workload that design considered reasonable, and fires
only past it. `gpu_pc` saturates — once every hot instruction has been
sampled once, a longer run adds no new values — so this bounds the profile's
string table, not its length. §8 says what would turn it into a measured
number.

### Surfacing it

`JoinHealthWith(snap, ProjectionStats)` raises an anomaly whenever
`PCLabelsSuppressed > 0`, naming the ceiling and the two things that cause it
(a workload with far more distinct hot code than the budget expects, or a cap
set too low). `JoinHealth(snap)` is now a wrapper passing the zero stats, so
every existing caller and test is unchanged.

The split exists because `ProjectionStats` cannot be in the `Snapshot` — it
is produced by the projection, which runs *after* the snapshot is taken. It
is the same shape as `SinkStats`, which `Timeline` also cannot see and which
a caller supplies through `CountingSink.SnapshotWith`.

**Both drivers were switched to `ProjectExecutionsWith` + `JoinHealthWith`.**
A counter no shipping path prints is exactly the kind of thing that reads
green when it matters; this is a two-line change in each driver and nothing
else about them moved.

---

## 4. The anti-forgery extension — including the direction the conditionals opened

`TestProjectionReservedLabelsWinOverTags` grew five names
(`gpu_pc_attrib`, `gpu_src_status`, `gpu_src_file`, `gpu_src_line`,
`gpu_src_func`), now driven through a real module store so the resolved
values exist to win. The `gpu_src_*` family is the sharpest case in the whole
label set: those labels name a file and a line in the profiled program's own
source, which is the single most believable thing a profile can say, so a tag
that could set them would let a producer point every stalled instruction at a
source line of its choosing.

**Overwriting alone turned out to be insufficient, and this is the one
substantive addition beyond the plan's letter.** The new labels are
*conditional*: `gpu_src_file` only under `resolved`, `gpu_pc` only inside the
budget, `gpu_stall` only when the producer named a reason, and none of them
on an execution with no PC samples. A tag named `gpu_src_file` would
therefore have survived untouched in exactly the cases where this package has
no value of its own — a forged source location standing beside
`gpu_src_status="no-module"`, which is worse than any value it could have
overwritten. So `projectionLabels` now **clears** every per-PC-sample
reserved name (`pcSampleReservedLabels`) immediately after the `Tags` copy,
before anything is derived. Reserved names win by absence too.

That also closes the same pre-existing hole for `gpu_stall` (a tag survived
whenever the producer named no stall reason) and for every one of these names
on a no-PC-sample execution. `gpu_stall`'s and `gpu_pc`'s emission is
otherwise exactly as it was.

`TestProjectionSourceLabelsCannotBeForgedByAbsence` drives all three shapes
in one snapshot: an unresolvable module, a sample past the cap, and an
execution with no PC samples.

---

## 5. Frames do not change

`projectionFrames` is byte-for-byte unmodified. `TestProjectionAddsNoFrames`
asserts it negatively: no frame name contains `gpu:pc`, `gpu:src`, a stall
reason, a source file name, a rendered PC offset, or an attribution value,
and the two PC samples of one kernel share one stack. The kernel name is
deliberately *not* on the forbidden list — `[gpu:kernel:<name>]` is one of
the three frames the design fixes.

---

## 6. Verification

`CapEff: 0`, no GPU, no capabilities, no BPF, no CUPTI, no shim. From the
worktree with the plan's build environment.

```
go build ./...                                     ok
go vet ./...                                       ok
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1 ok (12 packages)
go test ./... -count=1                             ok (whole repo, no regressions)
go test ./gpu/ -race -count=4                      ok  22.4s
~/go/bin/golangci-lint run --timeout=5m            0 issues.
```

The plan's offline checks for this task, and the test that is each one:

| the plan asks | test |
| --- | --- |
| all four `gpu_src_status` values | `TestProjectionEmitsAllFourSrcStatuses` (exhaustive against `SrcStatuses()`) |
| all four `gpu_pc_attrib` values | `TestProjectionEmitsAllFourPCAttribValues` (exhaustive against `PCAttribs()`) |
| `kernel-ambiguous` never coincides with `gpu_ambiguous="true"` | `TestProjectionKernelAmbiguousNeverCoincidesWithGpuAmbiguous` (end to end through the real join) |
| a `Tags` entry named `gpu_src_file` loses to the derived value | `TestProjectionReservedLabelsWinOverTags`, plus `…CannotBeForgedByAbsence` for the case with no derived value |
| the cap suppresses `gpu_pc` and **only** `gpu_pc`, exact count | `TestProjectionCapSuppressesGpuPCAndOnlyGpuPC` (5 offsets, ceiling 2, exactly 3 suppressed) |
| no frame carries `gpu:pc`, `gpu:src` or a stall reason | `TestProjectionAddsNoFrames` |

Nine new tests plus one extended: also
`TestProjectionWithoutAModuleStoreStillAnswersEveryPCSample`,
`TestProjectionSrcFileIsABasenameNotABuildHostPath`,
`TestSrcFileBaseRejectsWhatIsNotAName`,
`TestProjectionPCAttribIsNeverSilentlyAbsent`,
`TestProjectionCapReadmitsAnOffsetItAlreadyEmitted`,
`TestProjectionCapIsSurfacedInJoinHealth`,
`TestProjectionSourceLabelsCannotBeForgedByAbsence`.

### Mutation checks — the tests bite

Each applied alone to the finished code, `go test ./gpu/ -count=1`:

| mutation | caught by |
| --- | --- |
| M1 — `gpu_src_status` made conditional on a resolved status | 3 tests, incl. `…EmitsAllFourSrcStatuses` |
| M2 — the directory kept instead of the basename | 5 tests, incl. `…SrcFileIsABasenameNotABuildHostPath` |
| M3 — the cap also drops `gpu_stall` | `…CapSuppressesGpuPCAndOnlyGpuPC` |
| M4 — `gpu_pc_attrib` omitted when the join decided nothing | `…PCAttribIsNeverSilentlyAbsent` |
| M5 — reserved names no longer cleared from producer `Tags` | `…SourceLabelsCannotBeForgedByAbsence` |
| M6 — suppression not surfaced in `joinhealth` | `…CapIsSurfacedInJoinHealth` |
| M7 — the cap never fires | 4 tests |

A test that cannot fail is not a test.

---

## 7. `gpu_serialized` is deferred to Task 10 — deliberately absent

The design puts a fourth group in this task: `gpu_serialized`, unconditional
on **every** execution, `"true"` / `"false"` / `"unknown"`. **It is not in
this commit, not stubbed, and not present with a default value.**

Its three values are decided by the Tier A sampling windows
(`gpu_sampling_window_v1`), which Task 10 introduces and which do not exist
in this tree. There is no way to compute the label here, and the only value
it could take by default is `"false"` — which the design names as the one
answer that must never be reachable by accident, because "not perturbed" and
"cannot tell" are different facts and a profile that confuses them is
precisely the failure §4 forbids. A `gpu_serialized` that is present and
meaningless is worse than one that is absent; absent is honest while Tier A
does not exist.

**Follow-up for Task 10:** add it in `projectionLabels` (it rides on every
execution, not only PC-bearing ones, so it belongs in the shared map rather
than the per-PC-sample block), add its name to `pcSampleReservedLabels`'
equivalent for the shared map — or, more simply, set it after the `Tags` copy
like every other label there — and extend
`TestProjectionReservedLabelsWinOverTags` with it. Task 10's own honesty
obligations (an `end_ns == 0` window is open, not zero-length; missing
windows are `"unknown"`, never `"false"`) are label *inputs* and belong with
the windows, not here.

Also carried forward from the design and **not** this task's business: CPU
and off-CPU samples taken during a Tier A burst are distorted and carry no
marking at all, because those profilers are a separate path with no window
awareness. Task 11's standing operator warning is where that is disclosed.

---

## 8. Cannot verify

**Nothing here has been run against a GPU, and no claim in this report is
about hardware.** `CapEff: 0`, no NVIDIA device. Every PC sample these labels
have ever described is synthetic, and every module they resolved against is
one of Task 1's `sm_86` / CUDA 13.3 fixtures.

The plan defers exactly one thing about this task to the RTX 3090, and it is
open:

- **The real distinct-value counts per label on a genuine profile** — which
  is what would confirm the design's cardinality table and turn the `gpu_pc`
  ceiling into a measured number instead of the reasoned 20,000 in §3. The
  instrument is in the output: `ProjectionStats.DistinctPCLabels` beside
  `PCLabelCap` on every projection, so "we were nowhere near the ceiling" and
  "we sat exactly on it" are distinguishable without recomputing anything
  from the profile. The reading that says the ceiling is wrong is
  `PCLabelsSuppressed > 0` on an ordinary workload; the reading that says it
  is generous is `DistinctPCLabels` an order of magnitude below `PCLabelCap`
  on the hottest run available.

Unestablished and inherited, all load-bearing for whether these labels say
anything true on real data:

- **That `functionIndex` is the cubin's `.symtab` index** (Tasks 4, 8b; Task
  6 measures it). If it is not, `gpu_src_status` will read `unmapped` on
  everything — visibly wrong, which is the point of the enum, but wrong.
- **That `pcOffset` is function-relative in the sense the line table is.**
  The design calls this Task 9's half of the question. It is not testable
  here: the fixtures' line tables are function-relative by construction (see
  `internal/cubin`'s sequence-window reader), and whether CUPTI's
  `pcOffset` is measured from the same origin has never been observed. A
  mismatch would show as `resolved` statuses pointing at the wrong line —
  the one failure in this set that is **not** self-announcing, because a
  wrong line looks exactly like a right one. Confirming it needs a cubin
  whose SASS-to-source mapping is known independently, sampled on hardware.
- **That real Tier B samples arrive with a usable `cubin_crc`** (Task 8b). If
  they arrive zero, every sample reads `no-module`.
- **Whether `KernelName` and the cubin's `.symtab` spelling agree for C++
  kernels** (Task 8b). Unrelated to these labels' correctness but decides
  whether any PC sample reaches an execution at all on a real workload.

The reserved-name clearing in §4 is verified only against tags a test set. It
is a fixed list (`pcSampleReservedLabels`); a label added to the projection
and forgotten there is a name a producer can forge again, and nothing but
review catches that.
