# Task 4 — `gpu.ModuleStore`, the bounded CRC-keyed store

Branch `feat/gpu-module-store`. One commit. Plan:
`docs/superpowers/plans/2026-08-25-gpu-pc-sampling.md`, Task 4. Predecessor:
`internal/cubin` (Task 1, merged) and `.superpowers/sdd/task-1-cubin-report.md`.

Two files, both new: `gpu/modulestore.go` and `gpu/modulestore_test.go`. Nothing
else in the repository was touched — `Timeline`, `LaunchCache`, the join and the
projection are untouched, and nothing consumes the store yet.

**Cannot verify.** The plan states of this task: *"Must be measured on the RTX
3090 afterwards: nothing."* That held — but three things are premises rather than
findings and must be read as untested:

- **`functionIndex` is assumed to be the cubin's `.symtab` index.** The store
  keys its `functionIndex → name` table on `cubin.Function.SymIndex`, which is
  the design's finding-2 premise and is measured by Task 6, not here. Task 1
  exposed `SymIndex` without asserting it and this task consumes it the same
  way: the tests read the index out of the fixture rather than hard-coding one,
  so they assert the store uses it *consistently*, never that CUPTI agrees. If
  Task 6 measures otherwise, the pre-approved fallback (`gpu_pc_sample_batch_v2`
  with `kernel_id`) changes one map in `indexFunctions` and the `Resolve`
  lookup, not the status logic or any counter.
- **Nothing here has seen a real cubin from `MODULE_LOADED`.** Every module the
  store has ever held is one of Task 1's four `sm_86` / CUDA 13.3 fixtures, or a
  deliberate corruption of one.
- **This machine has `CapEff: 0` and no GPU.** No BPF, no CUPTI, no shim was run.

---

## 1. The API as built

```go
type ModuleStore struct{ /* unexported */ }

func NewModuleStore(cfg ModuleStoreConfig) *ModuleStore
func (s *ModuleStore) Put(crc uint64, b []byte) error
func (s *ModuleStore) Resolve(crc uint64, functionIndex uint32, pcOffset uint64) Resolution
func (s *ModuleStore) Len() int
func (s *ModuleStore) Stats() ModuleStoreStats

type ModuleStoreConfig struct {
    Capacity int   // modules; 0 -> 512
    MaxBytes int64 // total stored cubin bytes; 0 -> 64 MiB
}

type Resolution struct{ /* unexported */ }
func (r Resolution) Status() SrcStatus
func (r Resolution) Source() (function, file string, line uint32, ok bool)

type SrcStatus uint8
const (SrcResolved; SrcNoLineInfo; SrcNoModule; SrcUnmapped) // plus an unexported zero
func SrcStatuses() []SrcStatus
func (s SrcStatus) String() string
func (s SrcStatus) MarshalJSON() ([]byte, error)
```

`Put` and `Resolve` are exactly the two entry points the plan specifies, with
the specified signatures. `Resolution` carries `{Status, Function, File, Line}`
as specified — through two methods rather than four fields, for the reason in
§2.

Two deviations from a literal reading, both deliberate:

**`Put` does not retain `b`.** It copies. This is not defensiveness about
mutation: Task 3 hands the store an `mmap` of a sealed memfd and drops the
mapping once the offer is handled, which is precisely why Task 1 wrote
`TestParseDoesNotRetainInput`. A store that retained the caller's slice would
put that use-after-unmap hazard straight back one layer up. The copy is what
`MaxBytes` bounds. `TestPutDoesNotRetainCallerBytes` scribbles `0xAA` over the
caller's buffer after `Put` and resolves correctly afterwards.

**`Put`'s error is diagnostic, not a rejection.** An unparseable cubin is
*stored* (so a re-offer is not re-parsed), counted in `ModulesUnparseable`, and
resolves as `no-module` — and the parse error is returned so the caller can log
*why*. That is documented on `Put` with an explicit warning that a non-nil error
does not mean "retry"; the counters, not the error, are the record. There is
consequently no special case for empty or nil bytes: `cubin.Parse` rejects them
like any other non-ELF, so there is exactly one path and no uncounted refusal.

---

## 2. How the four-valued status is made structurally unavoidable

The plan requires that the store be the single place `gpu_src_status` is
decided, "structurally true, not merely documented". Four mechanisms, each
closing a different hole:

**(a) `Resolution` has no exported fields and no exported constructor.** Its
four fields — `status`, `function`, `file`, `line` — are unexported and the type
lives in `package gpu`. No code outside this package can build a `Resolution`
carrying a status the store did not choose, or attach a file and line to a
status that has none. This is the load-bearing one: everything downstream (Task
9's labels) reads a `Resolution`, and a `Resolution` can only come from
`Resolve`.

**(b) The source location is reachable only through `Source`, which returns
`ok` in the same expression.** There are deliberately no `File()` / `Line()` /
`Function()` getters. A caller who wants the location must take the `ok`
alongside it, so emitting `gpu_src_func` under a `no-lineinfo` status requires
*ignoring a returned bool*, not merely forgetting a check. `Source` returns
`("", "", 0, false)` for every status but `SrcResolved`, so there is no partial
location to leak even if someone does ignore it.

**(c) Four unexported constructors, one per status, and three of them take no
arguments.** `resolvedAt(fn, file, line)`, `noModule()`, `noLineInfo()`,
`unmapped()`. The three non-resolved ones *cannot* carry a location because
they have no parameters — the type signature does the work, not a convention.
`Resolve` has exactly four return points and each calls exactly one constructor
and increments exactly one counter, adjacent, on the same two lines.

**(d) The zero value is not one of the four.** `srcStatusInvalid` is the
unexported `iota` zero, so the four real values start at 1. A struct that was
never given a status therefore holds a status that is not a status, and it says
so: `String()` returns `"unset-src-status"` for the zero and
`"invalid-src-status-N"` for a fabricated one (two different causes, named
apart), and `MarshalJSON` **errors** on both, matching `ClockDomain` and
`GPUCapability` in this package. So a status nobody decided cannot reach a
serialized profile at all — it fails at the boundary rather than shipping a
string a consumer would read as meaningful. `Resolve` never returns it.

`SrcStatuses()` returns the four in stable order so a downstream switch can be
tested for exhaustiveness against the enum itself rather than a hand-copied
list. The four strings — `resolved`, `no-lineinfo`, `no-module`, `unmapped` —
are pinned by `TestSrcStatusesIsExhaustiveAndStable`; they are the label values,
not cosmetics, and rewording one would silently change the profile.

What is *not* claimed: `SrcStatus` is a `uint8`, so a caller can still write
`gpu.SrcStatus(99)` in their own variable. They cannot get it out of a
`Resolution`, cannot get it into one, and cannot serialize it. A struct-wrapped
enum would have closed even that, at the cost of turning the constants into
mutable package-level `var`s — a strictly worse hole. The chosen shape puts the
guarantee where the data actually flows.

---

## 3. The damaged-table decision, and why it gets its own counter

Task 1 found a third line-info state its two-valued split did not cover and
recommended: *"map the damaged-table case to `no-module`, not `no-lineinfo`."*
**Followed exactly.** `Resolve`'s decision order:

| condition | status | counter |
| --- | --- | --- |
| CRC not held (never arrived, or evicted) | `no-module` | `ResolveNoModule` |
| bytes did not parse (`cubin.Parse` failed) | `no-module` | `ResolveNoModule` |
| `.debug_line` present but damaged (`LineInfoErr() != nil`) | `no-module` | `ResolveNoModule` |
| no `.debug_line` at all | `no-lineinfo` | `ResolveNoLineInfo` |
| `functionIndex` not in the module's symbol table | `unmapped` | `ResolveUnmapped` |
| line table does not cover this `pcOffset` | `unmapped` | `ResolveUnmapped` |
| otherwise | `resolved` | `ResolveResolved` |

The reasoning Task 1 gave is the reasoning implemented: `no-lineinfo` tells the
operator to add a build flag they already passed, which is an active lie about
the build. `no-module` says "we hold nothing usable", which is true.

A fourth row deserves naming because the plan's table does not cover it: **a
module that has a line table, but whose particular function carries no sequence
in it**, answers `unmapped`, not `no-lineinfo` — by the same rule. The module
*was* built with `-lineinfo`; saying otherwise would misdirect the reader
identically.

### The counter: separate, not folded into `ModulesUnparseable`

The brief invited either "`ModulesUnparseable` covers this case too" or a
reason a separate counter is better. **A separate counter — `ModulesDamagedLineInfo`
— is better, and here is why.**

The two conditions produce the same *status* but point at different *causes*:

- `ModulesUnparseable` means the bytes did not survive transport, or are not a
  cubin at all. The operator inspects the Task 3 channel.
- `ModulesDamagedLineInfo` means the ELF is fine and **our reader** refused the
  DWARF inside it. Task 1's `relocSequences` refuses the whole table on any
  layout it does not recognize — the exact shape a future toolkit emitting
  `.debug_line` differently would take. The operator inspects the toolkit and
  the reader, not the channel.

Folded together, a toolkit change would climb `ModulesUnparseable` and send an
operator to audit a transport that is working perfectly. That is this project's
recurring defect class — a counter that reads the wrong colour when things are
worst — arriving by a new route. The plan's own rationale for keeping
`ModulesUnparseable` and `ModulesWithoutLineInfo` apart ("what makes a transport
bug distinguishable from a build-flag choice") is the same argument one level
down, so splitting here is consistent with the plan rather than a departure from
it.

Both counters are asserted non-interchangeably:
`TestDamagedLineTableResolvesAsNoModuleNotNoLineInfo` requires
`ModulesDamagedLineInfo == 1` *and* `ModulesUnparseable == 0` *and*
`ModulesWithoutLineInfo == 0` on a module whose ELF parses and whose DWARF does
not; `TestUnparseableIsNotWithoutLineInfo` requires the mirror.

---

## 4. Counters, and the two sum identities

All eight counters the plan names are present under exactly those names.
`ModulesEvicted` gained a two-way breakdown and there are two additions
(`ModulesWithLineInfo`, `ModulesDamagedLineInfo`), both so that the module side
*reconciles* rather than merely reports.

```
Live, LiveBytes                                   gauges of what is held now

ModulesStored                                     distinct modules ever admitted
ModulesEvicted = ModulesEvictedCapacity
               + ModulesEvictedBytes              identity, tested

ModulesStored = ModulesWithLineInfo
              + ModulesWithoutLineInfo
              + ModulesDamagedLineInfo
              + ModulesUnparseable                identity, tested

Resolve calls = ResolveResolved + ResolveNoModule
              + ResolveNoLineInfo
              + ResolveUnmapped                   THE identity, tested
```

The plan's identity — "the four `Resolve*` counters must sum to every `Resolve`
call" — is tested by wrapping the store in a `counting` type in the test file
that increments a local call counter on every `Resolve` and comparing it against
`Stats().ResolveTotal()`. It is asserted at the end of five separate tests,
including a 384-call mixed workload that reaches all four statuses
(`TestModuleStoreResolveCountersSumToEveryCall`) and the concurrent test.

The classification counters are cumulative, not gauges — an evicted module's
classification is not un-counted — which is documented on the type, because
`ModulesStored` being the denominator rather than `Live` is exactly the kind of
thing that gets misread.

---

## 5. LRU behaviour and bounds

**It is a real LRU: recency is refreshed by `Resolve`, not only by `Put`.** A
module under active PC sampling must survive a burst of unrelated module loads,
which is precisely what an insertion-ordered FIFO would fail to do — its samples
would silently start answering `no-module` while the store still had room for
it.

**`orderedFIFO` was deliberately not reused**, despite being the package's
shared bounded-FIFO mechanism. It reclaims superseded positions only while
something is evicting, so touching an entry on every `Resolve` — a per-PC-sample
operation — would grow its `order` slice without bound whenever the store sits
*under* its capacity. That is the opposite of "bounded means bounded". A
`container/list` LRU has no such garbage: a touch is `MoveToFront`, O(1), zero
allocation. `Resolve` therefore takes the write lock; PC-sample decoding is
batched, so contention is per batch, not per sample.

**Two bounds, because one is not a bound.** `Capacity` caps distinct modules;
`MaxBytes` caps total held bytes. A count bound alone is not a memory bound —
cubins run from a few KB to hundreds of KB, so 512 modules is anywhere from a
few MB to a hundred — and a byte bound alone misses map and list overhead under
a flood of tiny modules. Capacity evictions and byte evictions are counted
separately and sum to `ModulesEvicted`.

`MaxBytes` is **absolute**: a single module larger than the whole budget is
stored, then evicted along with everything else, leaving the store empty rather
than one entry over the limit. Its `Resolve`s then answer `no-module`, which is
true — it is not held. `TestModuleStoreOversizedModuleLeavesTheStoreEmpty` pins
that.

**There is no memoized resolution anywhere in the type.** Every answer is
derived from the stored bytes on every call. That is what makes the
after-eviction guarantee absolute rather than probabilistic.

---

## 6. Tests

`go test ./gpu/ -run ModuleStore -count=1` — 16 tests, all pass. Table-driven
over Task 1's fixtures, read from `../internal/cubin/testdata/` rather than
copied into `gpu/testdata/`: two copies of a binary fixture drift, and the point
is that this store answers correctly about the *same bytes* Task 1 asserts its
own claims against.

- `TestModuleStoreAllFourStatusesAreReachable` — the core table. Seven cases
  producing all four statuses, each asserted against `SrcStatuses()` so a
  status no case reaches fails the test; exact per-counter values; the sum
  identity; and, for every non-resolved case, that `Source()` returns `false`
  with all three data returns zero.
- `TestModuleStoreResolveCountersSumToEveryCall` — 384 calls across a
  `-lineinfo` module, a no-`-lineinfo` module, a truncated module and an absent
  CRC, with every status positive and the identity holding.
- `TestSrcStatusZeroValueIsNotAStatus`, `TestSrcStatusesIsExhaustiveAndStable` —
  §2(d): the zero is not in `SrcStatuses()`, names itself apart from a
  fabricated value, and neither serializes; the four wire strings are pinned;
  `SrcStatuses()` returns a copy.
- `TestDamagedLineTableResolvesAsNoModuleNotNoLineInfo`,
  `TestUnparseableIsNotWithoutLineInfo` — §3, both directions.
- `TestModuleStoreResolvesExactSourceLines` — the exact `(pcOffset → line)`
  table for `addOne`, all eight row boundaries plus four interior PCs plus
  three past `end_sequence`. Task 1 notes lines 10, 9, 10 at `0xa0/0xb0/0xc0`:
  the table is not monotonic, so a merely plausible-looking answer does not
  pass.
- `TestModuleStoreBindsFunctionIndexToTheRightKernel` — the two-kernel fixture,
  whose kernels occupy the identical PC range with disjoint source-line ranges.
  A store that swapped the two indices would still answer `resolved`, with
  confidently wrong lines.
- `TestModuleStoreEvictionIsExactUnderPressure` — capacity 3, 50 puts,
  `ModulesEvicted == 47` exactly, breakdown exact, bound checked at every step.
- `TestModuleStoreLRUKeepsWhatIsBeingResolved` — a `Resolve` saves a module a
  `Put` would otherwise have evicted; a re-offer counts as use too.
- `TestResolveAfterEvictionIsNoModuleNotStale` — the plan's explicit test.
  Resolve at `0x10` → line 6; evict; then every offset that resolved before
  answers `no-module` at the same coordinates, three times over. It also
  asserts that the `Resolution` *value* taken before eviction still reads as it
  did — that is not staleness in the store, and the assertion exists so the two
  are not confused.
- `TestModuleStoreByteBoundEvicts`, `TestModuleStoreOversizedModuleLeavesTheStoreEmpty`,
  `TestModuleStoreDefaultsAreApplied` — §5.
- `TestPutOfAKnownCRCIsANoOp`, `TestPutDoesNotRetainCallerBytes` — §1.
- `TestModuleStoreConcurrentPutAndResolve` — 8 goroutines × 200 mixed
  operations under `-race`, asserting the resolve identity, the classification
  identity, the eviction identity, and `ModulesStored - Live == ModulesEvicted`.

**Mutation-checked.** Four deliberate defects were introduced and each was
caught before being reverted:

| defect | caught by |
| --- | --- |
| damaged table mapped to `no-lineinfo` | `AllFourStatusesAreReachable`, `DamagedLineTable...` |
| `Resolve` does not refresh LRU recency | `LRUKeepsWhatIsBeingResolved` |
| eviction removes from the LRU list but not the map (the classic stale-answer bug) | `ResolveAfterEvictionIsNoModuleNotStale` + 4 others |
| the `unmapped` return forgets its counter | both sum-identity tests |

A test that cannot fail is not a test.

---

## 7. Scope

Additive only. `Timeline`, `LaunchCache`, `orderedFIFO`, the join and
`projection.go` are unchanged; `git diff --stat` against `origin/main` shows two
new files and nothing else. Nothing consumes the store — Task 7 decodes and Task
8b joins, and wiring it in here was explicitly out of scope.

**One gap flagged for Task 8b.** `Resolution` carries a function name *only*
when the status is `resolved`, following the design's representation table
literally (`gpu_src_func` is absent for all three non-resolved statuses). But
Tier B's attribution chain — `cubin_crc → module → function → symbol`, reaching
the kernel name for the `[gpu:kernel:<name>]` frame — needs a name even when the
source line is unavailable, e.g. from a module built without `-lineinfo`. The
store holds that name (`moduleEntry.byIndex` is populated for every parseable
module, with or without line info) but does not currently expose it. Task 8b
will need a small `FunctionName(crc, functionIndex) (string, bool)` accessor. It
is deliberately not added here: the plan says nothing consumes the store yet,
and an accessor with no caller is an accessor with no test.

---

## 8. Verification run

```
go build ./...                          ok
go vet ./...                            ok
go test ./gpu/ ./internal/... -count=1  ok
go test ./gpu/ -race -count=4           ok  (19.3s)
go test ./... -count=1                  ok  (whole repo, no regressions)
golangci-lint run --timeout=5m          0 issues
```

Not run, and not runnable here: anything needing capabilities or a GPU. Per the
plan, this task needed neither.
