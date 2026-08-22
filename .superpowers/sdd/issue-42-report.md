# Issue #42 — the integration suite asserted on a fixed wall-clock window

Branch `fix/integration-collect-until`, worktree `.worktrees/ci-flake-fix`, cut from
`origin/main` (`35d5d496`).

Scope was `test/` only. Nothing outside `test/` was changed. One thing arguably *should*
change outside it — see §6 — and is flagged rather than done.

**Read §3 first if you read nothing else: this change does not make issue #42's third
symptom green, and that is a deliberate, defended decision rather than an oversight.**

Revised twice after review; §11 records what each round changed.

---

## 1. The diagnosis this fix acts on

The issue body and its three comments record three symptoms:

| symptom | seen on |
|---|---|
| samples captured, every frame kernel-side | `TestKernelStackResolution`, both arches |
| zero samples captured at all → empty profile → bare `EOF` | `TestKernelStackResolution`, arm64 (PR #46) |
| samples + kernel symbols, no real user mapping | `TestPerfAgentSystemWideDwarfProfile`, amd64 (PR #48) |

and the datum that settles causation for the first two: commit `8403e54e` on arm64 failed
**twice with two different symptoms and passed on the third attempt**, same runner image.
A failure non-deterministic on fixed code is not caused by that code.

The shared structure: a test starts a real workload, samples it for a fixed wall-clock
window (3–10 s at 99 Hz), and asserts on whatever landed. A `cpu-clock` sample only fires
while the target is **on CPU**, so against an I/O-bound workload on a contended runner
"whatever landed" is a coin toss.

The fix is the issue's first suggested direction: **collect until the condition is
observed, with a deadline**. Symptom 3 turns out not to belong to this family at all —
§3.

---

## 2. What must be retried, and what must never be

This is the distinction the whole change turns on, and the one the first version of it
got wrong at three sites.

A collect-until loop is only honest if its condition is a **precondition** — "did this
capture exercise the thing under test at all?" — and never the **assertion** itself. If
the loop waits for the assertion to hold, then on the success path the assertion cannot
fail, and an intermittent regression is retried into a pass: at a 60% per-capture success
rate, three attempts turn a real 40%-of-the-time bug into a 94% green rate.

The two halves are separable, and `fidelityDiagnosis` computes the split for the reader:

| observation | half | treatment |
|---|---|---|
| too few samples to judge | precondition unmet | retry |
| samples, but every frame kernel-side | precondition unmet — never on-CPU in user space | retry |
| enough samples, user-space PCs, resolving to no binary mapping | assertion violated — mapping resolution is the suspect | **fail now** |

So the loop condition for the three fidelity-checking sites is `profileFidelityJudgeable`:
`Samples >= degenerateSampleFloor && userSpaceFrames(sum) > 0`.

Two properties make that safe:

- **Neither conjunct implies `RealMappings >= 1`.** `UserFrames > 0` alone would *not*
  have worked — a location's mapping must appear in the profile's mapping table, so it
  implies `RealMappings >= 1` just as surely as the original condition did, and would
  have preserved the defect in a subtler form. Counting the **unmapped** frames is what
  makes it a precondition rather than a restatement.
  `TestUserSpacePreconditionDoesNotImplyRealMapping` pins both directions.
- **The sample floor is not invented here.** `isDegenerateProfile` already uses
  `degenerateSampleFloor` to decide when a zero-real-mapping profile is tolerated and
  when it is "a genuine bug worth a loud failure" — its own words. Reusing the constant
  keeps the retry policy and the tolerance policy from disagreeing: re-collect exactly
  while the capture is below the bar the suite already set for judging it, then hand it
  over the moment it clears.

---

## 3. Does this change fix symptom 3? No — and here is the derivation

The reviewer asked this directly, and it is worth the space. **The answer is no**: the
capture behind symptom 3 clears the precondition, so it is judged rather than retried,
and `assertPprofFidelity` fails on the first attempt.

### 3.1 The message that seeded symptom 3 is uninterpretable

```
integration_test.go:1322: expected >=1 real mapping, got 0: [0x1311ff8c230]
integration_test.go:1322: pprof fidelity: real_mappings=0 has_build_id=false
```

The old code was `t.Errorf("... got %d: %+v", real, p.Mapping)`, and `p.Mapping` is
`[]*profile.Mapping`. `fmt` only uses the `&{...}` form at depth 0; inside a slice it
prints pointers as bare addresses. Verified directly:

```
one mapping,  %+v:  [0x3a0e45bf4a50]
two mappings, %+v:  [0x3a0e45bf4a80 0x3a0e45bf4ab0]
top-level ptr %+v:  &{ID:1 Start:0 File: BuildID:}
```

So `[0x1311ff8c230]` is a **Go heap pointer to one `profile.Mapping`**. It is not an
address from the capture, not a mapping's `Start`, and carries no field of any mapping.
The only datum in it is the count: **one mapping**. The reading recorded in the issue —
"every user PC landed in one anonymous region, and the symbolized functions came from
`[kernel]`" — could not have been derived from this message, and §3.2 shows the
`[kernel]` half is contradicted by the code.

### 3.2 What the code does let us recover

| fact | source |
|---|---|
| the builder **always** pre-creates `Mapping[0] = {ID: 1}` with `File: ""` | `pprof/pprof.go:182-186` |
| any kernel frame appends a `[kernel]` sentinel mapping | `addLocation` case 1 + `addMapping` (appends) |
| any JIT frame appends a `[jit]` sentinel mapping | `addLocation` case 2 |
| any resolver hit appends a file-backed mapping | `addLocation` case 3 |
| everything else falls back onto `Mapping[0]` | `addLocation` case 4 |
| the write path is `Profile.WriteUncompressed` — no `Compact`, no `Merge` | `ProfileBuilder.Write`, `profile/profiler.go:255` |
| a write/parse round trip keeps unreferenced mappings | verified — `TestProfileRoundTripKeepsUnreferencedMappings` |

`len(p.Mapping) == 1` therefore means the single mapping **is** the always-present
default, and that **no kernel, JIT or resolver-derived mapping was ever created**. There
were no kernel frames in that profile at all.

One more fact falls out of the tolerance ladder. The failure reached `default:`, so
`isDegenerateProfile(p)` returned false; with `real == 0` and no JIT mapping its own loop
cannot return false, so it returned false because `len(p.Sample) >= degenerateSampleFloor`.

**The capture was: ≥ 20 samples, every frame on the default anonymous mapping, zero
kernel frames.**

### 3.3 The three answers

1. **What is `[0x1311ff8c230]`?** A Go heap pointer to one `profile.Mapping` struct. The
   message printed a mapping *count* and nothing else.
2. **Would `profileHasUserSpaceFrames` return true on it?** **Yes** — every one of its
   ≥ 20 frames counts as an unmapped user-space frame. It also clears the sample floor.
   So the loop exits on the first capture and `assertPprofFidelity` hard-fails. **This
   change does not fix symptom 3.** It is not "intended behaviour" in the sense of
   solving it; it is a deliberate refusal to solve it by retrying.
3. **Profiler defect or starved capture?** Neither reading in the issue survives contact
   with the code. Not "all kernel-side" — there were no kernel frames. Not starved by the
   suite's own standard — it was above `degenerateSampleFloor`. What it actually was:
   **every sampled PC failed resolver attribution**, which is the shape of a
   mapping-resolution failure, not of a workload that was never on-CPU in user space.
   The sibling `TestPerfAgentSystemWideDwarfOffCPU` resolving 28 mappings in the same run
   is consistent with that reading, though it was a separate agent invocation.

### 3.4 Why fail rather than retry, and what I cannot prove

I **cannot** prove the cause from the available evidence. A `-a` capture in which every
sampled PID exited before `/proc/<pid>/maps` could be read would produce the same profile
and would be environmental. Ruling that in or out needs data the log does not contain.

Given the uncertainty, the tie-break comes from the issue's own thesis: a suite that
fails randomly "makes a genuine regression indistinguishable from noise". We cannot show
this is noise, so we must not encode it as noise. The change therefore fails fast and
explains itself, rather than retrying until the mapping appears — which would have made a
possible resolver bug invisible. `TestSymptomThreeIsJudgedNotRetried` pins the outcome so
that nobody later "fixes" the red by widening the loop condition back.

The practical difference from `main` for this one site is that it fails **faster and
legibly** rather than being fixed. Reconstructing the capture and asking the new message
what it says:

```
expected >=1 real (file-backed) mapping, got 0.
  captured: samples=40 value_total=40 locations=1 functions=1(named=1) frames{user=0 kernel=0 jit=0 unmapped=40} mappings{real=0 kernel=0 jit=0 anon=1} build_id=false
  reading:  40 frame(s) carried a user-space PC that resolved to no binary mapping — mapping resolution (procmap/blazesym) is the suspect
  mappings: 1 mapping(s): [#1 file="" start=0x0 limit=0x0 off=0x0 build_id=""]
```

Everything §3.2 needed a code read to derive is now in the failure text.

### 3.5 What evidence would settle it

If it recurs, these decide it — the first two are now printed automatically:

- **the frame split and the mapping table** (above): `unmapped=N, kernel=0` with N large
  rules out the "never on-CPU in user space" reading on the spot;
- **the agent's own `symbolize:` / `dwarfagent:` counter line** on stderr, which the CI
  log captures for other tests but the issue comment did not quote;
- **whether the samples came from one PID or many** — these pprof samples carry no pid
  label, so this needs either a label added to the builder (outside `test/`) or a
  cross-read against the perf.data from the same run.

If it recurs with a large `unmapped` count and zero real mappings, that is a
mapping-resolution bug and deserves its own issue, not a retry.

---

## 4. The harness — `test/collect_until.go` (new)

```go
func collectUntil(t *testing.T, what string, window, budget time.Duration,
        attempt func(n int) (ok bool, detail string)) (bool, string)

func collectProfileUntil(t *testing.T, what string, window time.Duration,
        collect func(n int) (*profile.Profile, error),
        cond func(*profile.Profile) bool) (*profile.Profile, bool, string)
```

One *attempt* is one complete collection: spawn the agent for `window`, or run one
in-process `Start`/sleep/`Stop` cycle. Attempts repeat until `cond` holds or a budget is
spent. The workload stays alive across attempts, so this re-samples the same live target;
it does not re-run the test.

**How a timeout is handled is per-site, and is not uniform.** Counted exactly: 24 call
sites, 15 of which report the loop's failure with `t.Logf("WARN: %s", report)` and then
let the test's own assertions run on the last capture and decide — for the sites with a
tolerance ladder (`TestProfileMode`, `assertPprofFidelity`) that ladder is reached
exactly as it was before this change. The other 9 make the timeout fatal.

Of those 9, **7 are followed by assertions that restate the loop condition** and are
therefore unreachable-if-satisfied: `integration_test.go:1696`, `:1757`, `:1850`,
`:1928`, `:2118`, `:2743` (which restates it verbatim) and
`debuginfod_integration_test.go:189`. They are kept as executable documentation of intent
and as a guard against a mistake in `cond`, but they do no independent verification and
this report does not claim they do. One more (`runStripped`, `:2637`) has nothing after
it. The ninth is `TestOffBoxLibcResolution` (`:2909`), where the fatal guards a
*precondition* and the assertion that follows — "libc was not fetched" — is genuinely
independent of it; that is the shape the other fatal sites would ideally have.

**Per-loop budget** (`collectBudgetFor`): `min(max(3×window, 15s), 25s)` — 3 s → 15 s
(~5 attempts), 5 s → 15 s (3), 6 s → 18 s (3), 10 s → 25 s (2).

**Package-wide allowance** (`suiteRetryBudget`, 3 min): §6.

**Workload lifetime.** Workloads spawned by converted tests now run for
`workloadRuntime = 150s` (`workloadRuntimeFlag` = `-duration=150s` for the Go workloads,
`workloadRuntimeSecs` = `"150"` for the Rust/Python ones, which take bare seconds as
`argv[1]`). All are killed in the existing `defer`, so nothing actually runs longer than
before. `requireWorkloadAlive` re-checks liveness before every attempt via
`/proc/<pid>/stat` (a killed-but-unreaped child still answers signal 0) and fails loudly
rather than letting a dead target masquerade as a broken profiler.

**A hazard this change introduced, and closed.** Raising workload lifetimes meant passing
a duration to `spawnBinaryAsWorkload`, which on `main` took no arguments at all. Passing
the Rust form (bare `"150"`) to the Go workload would have been silently wrong — Go's
`flag` ignores positional arguments and the workload would have kept its 30 s default,
dying mid-budget. The helper now *requires* an explicit duration and its doc names the
two incompatible forms. Self-inflicted, caught, guarded — not a pre-existing bug.

`test/collect_until_test.go` (new) unit-tests the harness: 18 tests, no BPF, no workload,
no capabilities. They **were executed and pass** (§10).

---

## 5. Tests converted — before / after control flow

*Before* in every case: run the agent once for a fixed `--duration`, parse, assert.
*After*: `collectProfileUntil` with the condition below; then the test's own assertions.

| test | window → budget | loop condition (precondition) |
|---|---|---|
| `TestProfileMode` (×5 subtests) | 10s → 25s | samples > 0 ∧ has stacks ∧ has symbols |
| `TestOffCPUMode` (×2) | 10s → 25s | samples > 0 |
| `TestCombinedMode` | 10s → 25s | CPU-profile samples > 0 |
| `TestSystemWideProfile` | 5s → 15s | samples > 0 |
| `TestSystemWideOffCPU` | 5s → 15s | **fidelity-judgeable** (≥ floor samples ∧ ≥ 1 user-space frame) |
| `TestStreamingProfileOutput` | 3s → 15s | samples > 0 ∧ has symbols |
| `TestStreamingOffCPUProfileOutput` | 3s → 15s | samples > 0 |
| `TestStreamingCombinedProfileOutput` | 3s → 15s | CPU samples > 0 |
| `TestLibraryPMUMetrics` | 3s → 15s | a snapshot process with `SampleCount > 0` |
| `TestPerfDwarfWalker` | 5s → 20s | samples > 5 ∧ maxFrames > 2 ∧ dwarfSamples > 0 |
| `TestPerfAgentSystemWideDwarfProfile` | 5s → 15s | **fidelity-judgeable** ∧ functions > 0 |
| `TestPerfAgentSystemWideDwarfOffCPU` | 5s → 15s | **fidelity-judgeable** ∧ blocking-ns > 0 |
| `TestPerfAgentDwarfUnwind` | 5s → 15s | samples > 0 ∧ a `cpu_intensive_work` frame |
| `TestPerfAgentOffCPUDwarfUnwind` | 5s → 15s | samples > 0 ∧ blocking-ns > 0 |
| `TestPerfDataOutput` | 5s → 15s | perf.data > 200 B ∧ `perf script` output non-empty |
| `TestKernelStackResolution` | 3s → 15s | samples > 0 ∧ a `main.*`/`runtime.*` frame ∧ (kptr_restrict≠0 ∨ a resolved kernel symbol) |
| `TestPerfDataUserspaceMmap2_SystemWide` | 3s → 15s | io_bound MMAP2 present ∧ ≥ 2 distinct PIDs |
| `TestStrippedRustOffBoxSymbolization` | 6s → 18s | either expected Rust symbol present |
| `TestStrippedGoOffBoxSymbolization` | 6s → 18s | any expected Go symbol present |
| `TestFileModeFrameAddressPreservesMapping` | 6s → 18s | ≥ 1 symbolized `rust_workload::` frame |
| `TestStrippedSidecarUnreachableSymbolicPath` | 6s → 18s | `rust_workload::cpu_intensive_work` present |
| `TestOffBoxLibcResolution` | 6s → 18s | samples > 0 ∧ ≥ 1 symbolized `rust_workload::` frame |
| `TestFileModeParseFailDemotes` (2nd run) | 3s → 15s | samples > 0 ∧ a `rust-workload` mapping |
| `runStripped` (used by 2 tests) | 3s → 15s | the fetched `.debug` is in the symbol cache |
| `profileAndAssert` (debuginfod, `-tags integration`) | 3s → 15s | any fixture function present |

The three bolded rows are §2 and §3's subject. A side effect of adding the sample floor
there: a capture with a handful of samples, all unmapped, is now **re-collected**, where
the previous revision would have stopped and judged it. That is the starved shape, and
retrying it is the right move; if the budget runs out, `isDegenerateProfile` tolerates it
exactly as it always did.

Four others deserve their reasoning spelled out.

**`TestKernelStackResolution`** — the test in symptoms 1 and 2, and the one this change
genuinely fixes. The Go `io_bound` workload writes 1 MB, `fsync`s, reads it back, then
sleeps 100 ms, so it is on-CPU in user space only in short bursts. Before: one 3 s window,
then `t.Fatalf("no user-side function…")` — or, when the profile was empty, `EOF` out of
`gzip.NewReader` inside `parseProfile`. After: up to ~5 windows, condition evaluated per
capture, both hard assertions still present afterwards.

**`TestPerfDwarfWalker`** — already had a collect-until loop (`for samples < 40 &&
before(deadline)`), but the deadline stopped the *collection* while the assertions
(`maxFrames > 2`, `dwarfSamples > 0`) were checked afterwards on whatever those 40 samples
held. The condition is now `(samples < 40 || !enough())`, so a healthy run still gathers
at least the 40 samples it used to and an unhealthy one keeps collecting up to 20 s. The
only conversion that is strictly *more* collection, never less. (It reads the ringbuf
directly rather than going through `collectUntil`, so it does not draw on the
package-wide allowance; its 20 s deadline is fixed.)

**`TestPerfDataUserspaceMmap2_SystemWide`** — not obviously sampling-dependent, but
`perfagent/agent.go:411` emits COMM+MMAP2 **lazily, on the first sample seen per PID** in
`--all` mode (eagerly in `--pid` mode). "io_bound has an MMAP2 record" is therefore a
claim about whether io_bound got sampled. Its `--pid` siblings go through the eager path
and were left alone.

**`TestOffBoxLibcResolution`** — its only hard assertion is negative ("libc was *not*
fetched from debuginfod"), and a capture that sampled nothing symbolizes nothing, fetches
nothing, and satisfies it vacuously. It now collects until the workload's own symbols
resolve, proving symbolization ran end to end, before asking the question. The
precondition is deliberately the *workload's* symbols and not libc's: the test documents
libc frames as environment-dependent and only logs when they are absent, so requiring them
would invent a requirement the test declines to make. **Residual gap:** a run that
resolved `rust_workload` but never sampled a libc PC still passes without exercising the
libc path. Narrower than the vacuous pass it replaces, but not zero.

### `TestStrippedCachedHitNoFetch` passed vacuously on `main`

Worth stating on its own, because it is a worse defect than the flakiness this issue is
about: **this test could pass on `main` having verified nothing.**

It runs `runStripped` twice and asserts the second run adds no `GET
/buildid/<id>/debuginfo` entries. The fetch is driven by symbolizing a sampled PC inside
the stripped binary. If the first 3 s window sampled nothing, no fetch happened, the cache
stayed empty, the second run also fetched nothing — and `delta == 0` passed. A green
result proving the cache-hit short-circuit works was indistinguishable from a green result
proving the profiler collected nothing at all. Converting `runStripped` to collect until
the `.debug` is actually in the cache closes it.

Its sibling `TestFileModeParseFailDemotes` calls the same helper but then does
`os.Stat(cached)` → `t.Fatalf`, so the same empty window made it fail **loudly**. That one
was flaky, not vacuous. The conversion helps both, for different reasons.

### Deliberately not converted

- `TestPMUMode`, `TestPMURunqueueLatency`, `TestPMUTaskStateClassification`,
  `TestPMUIOWorkloadHasIOWait`, `TestPMUCPUWorkloadMostlyRunning`, `TestSystemWidePMU`,
  `TestSystemWidePMUPerPID`, `TestSystemWidePMUWithNewMetrics` — assert on the presence of
  *labels* in the agent's stdout, printed unconditionally. Not sample-dependent.
- `TestPerfDataKernelMmap2`, `TestPerfDataUserspaceMmap2` — eager `/proc` walk, above.
- `TestMutuallyExclusiveFlags`, `TestRequiresPIDOrAll`, `TestPerPIDRequiresAll`,
  `TestPerPIDRequiresPMU` — argument validation; the agent exits before sampling.
- `TestPerfDwarfMmap2Tracking` — asserts on BPF map contents after a `dlopen`, driven by
  an MMAP2 event, not by sampling. Left at its original 20 s workload.
- `TestStrippedCachedHitNoFetch` — its own body counts fetches; the sampling dependency is
  entirely inside `runStripped`, which is where the fix went.
- `integration_inject_python_test.go` — asserts on agent stdout and `/proc` state.

### One defect found and left alone

`integration_test.go:914-917` (`TestPMUIOWorkloadHasIOWait`) reads:

```go
if assert.Contains(t, outputStr, "I/O Wait (D state):") ||
    assert.Contains(t, outputStr, "Voluntary (sleep/mutex):") {
```

`assert.Contains` records a failure as a side effect, so the first branch fails the test
whether or not the second would have matched: the `||` tolerance is fake and the test is
effectively stricter than its own message claims. Fixing it means switching to
`strings.Contains`, which **weakens** the assertion that currently executes — and I cannot
run the suite to check whether the `I/O Wait (D state):` label is printed unconditionally
(no-op) or conditionally (behaviour change). Pre-existing and outside every conversion
here; being filed separately.

---

## 6. Suite runtime — corrected

**The first version of this report claimed the worst case added "roughly four minutes".
That was wrong.** Summing the per-loop budgets across the 30 collect-until instances a
default-build run executes (24 call sites, of which `TestProfileMode` runs 5×,
`TestOffCPUMode` 2× and `runStripped` 3×; the two `-tags integration` debuginfod fixtures
are not built in CI) — 8 at 25 s (window 10 s), 7 at 15 s (5 s), 10 at 15 s (3 s), 5 at
18 s (6 s), plus the walker's fixed 20 s — gives **565 s of budget**, and a loop may run
to `budget + one window`, adding **175 s of overshoot**: **~12.3 minutes** on top of the
suite's own work, against the `-timeout 15m` at `.github/workflows/tests.yml:154`. Add
per-attempt agent startup and BPF load — real, and unmeasured here — and a
comprehensively degraded run lands at or past the timeout. A timeout kills the run with a
goroutine dump and destroys precisely the diagnostics this change exists to produce: a
strictly worse outcome than a clean assertion failure.

Three ways out. Cutting the per-loop budgets weakens the fix for the one-flaky-test case
that is actually common. Raising the CI timeout is a change to `.github/workflows/`,
outside the scope I was given — **flagged, not done**: if the maintainer prefers it,
`tests.yml:154` is the line, and it would let the budgets stay as they are.

What is implemented instead is a **package-wide allowance**: `suiteRetryBudget = 3m`,
shared by every collect-until loop in the run. The first attempt of any loop is free — it
is the collection the test would have made before this change — and only re-collections
draw on it. Once it is spent, later loops run that single free attempt and let their own
assertions decide; the suite degrades to its pre-#42 behaviour instead of overrunning.

That makes the bound `baseline + 3m + one in-flight window` regardless of how many tests
degrade:

| | |
|---|---|
| baseline (first attempts = today's fixed windows) | ~195 s |
| package-wide re-collection allowance | 180 s |
| in-flight attempt when the allowance runs out | ≤ 10 s |
| non-converted tests and per-test setup sleeps | ~120 s |
| **worst case** | **~8.5 min** |

Two properties worth noting. The allowance is charged by **elapsed time**, so per-attempt
agent startup and BPF-load cost are inside the bound rather than excluded from it — which
is what makes the number hold despite that cost being unmeasured. And 3 min is sized for
the case it must absorb, not the pathological one: a normal run has zero or one flaking
tests, spending at most one per-loop budget (≤ 25 s) between them.

---

## 7. Assertions changed — what each caught before and after

Two changed in substance; both are message-only. Everything else in §5 kept its predicate
and gained a retry budget in front of it.

### 7.1 `assertPprofFidelity`

Before:

```
expected >=1 real mapping, got 0: [0x1311ff8c230]
pprof fidelity: real_mappings=0 has_build_id=false
```

After (rendered from the reconstructed symptom-3 capture):

```
expected >=1 real (file-backed) mapping, got 0.
  captured: samples=40 value_total=40 locations=1 functions=1(named=1) frames{user=0 kernel=0 jit=0 unmapped=40} mappings{real=0 kernel=0 jit=0 anon=1} build_id=false
  reading:  40 frame(s) carried a user-space PC that resolved to no binary mapping — mapping resolution (procmap/blazesym) is the suspect
  mappings: 1 mapping(s): [#1 file="" start=0x0 limit=0x0 off=0x0 build_id=""]
```

- **Caught before:** a profile whose mappings are all sentinel/empty, subject to the
  pre-existing `hasJit` and `isDegenerateProfile` tolerances.
- **Caught after:** the same set. Predicate, both tolerance branches, the per-location
  `Address != 0` check and the observational BuildID log are unchanged. Three things are
  new in the message: the sample count, the kernel/user/jit/unmapped frame split, and —
  via `describeMappings` — the mapping table's actual *contents* instead of `%+v`'s heap
  pointers (§3.1). The same split now also decides which captures get retried (§2), which
  is what keeps this assertion able to fail at all.

### 7.2 `parseProfile` (empty profile → `EOF`)

Before, a zero-byte profile failed at `gzip.NewReader` with:

```
Error: Received unexpected error:
       EOF
```

After, via the new `readProfile`:

```
parse profile: profile /tmp/.../profile.pb.gz is EMPTY (0 bytes): the agent collected 0 samples in this window
```

and for a truncated one:

```
parse profile: profile /tmp/.../profile.pb.gz (312 bytes) did not parse: <err> — a truncated or empty profile means the collection captured no samples
```

- **Caught before:** any unreadable profile, reported as a parser error.
- **Caught after:** the same set, split into "empty" and "corrupt". `parseProfile` still
  does **not** assert on sample count — each caller keeps its own `len(prof.Sample) > 0` —
  so no test became stricter by accident.

### 7.3 Assertions that now catch less

**One, deliberately.** `TestPerfAgentDwarfUnwind`'s final check is
`require.True(hasFunctionContaining(prof, "cpu_intensive_work"))` rather than a
hand-rolled loop — identical predicate. On the failure path it prints up to 10 *named*
functions via `topFunctionNames`, where the old code printed the first 10 entries of
`prof.Function` including empty names. Marginally fewer entries when many are unnamed;
`describeProfile` prints the named/total count alongside.

**And the seven `t.Fatal(report)` sites**, where the assertions after the loop no longer
verify anything independent (§4). Not weaker than before — the loop condition carries the
same predicate — but not carrying their own weight either, and that is stated rather than
papered over.

Nothing else was weakened: no `t.Skip` added, no `assert` downgraded to a log, no
threshold lowered. The tolerance ladders in `TestProfileMode` and
`TestStreamingOffCPUProfileOutput` are untouched; the loop runs *in front of* them, so a
healthy run reaches the strict branch more often than before and a broken profiler lands
where it always did.

---

## 8. New failure messages, quoted

Deadline, rendered from the real format string:

```
timed out waiting for a kernel-stacks profile containing at least one user-side (main.*/runtime.*) frame and at least one resolved kernel symbol: 5 attempt(s) over 15.2s (per-test budget 15s, collection window 3s per attempt); last attempt captured: no usable profile: profile /tmp/x/profile.pb.gz is EMPTY (0 bytes): the agent collected 0 samples in this window
```

Deadline when the *package-wide* allowance is what stopped it — the signal that the whole
run was degraded, not just this test:

```
timed out waiting for a system-wide DWARF profile with enough samples to judge, a symbolized function and at least one user-space frame: 1 attempt(s) over 0s (per-test budget 15s, collection window 5s per attempt); last attempt captured: an EMPTY profile: 0 samples (the collection window caught the workload off-CPU the whole time). NOTE: the package-wide re-collection allowance (3m0s) is spent (2s left), so this test stopped at 1 attempt(s) rather than the 4 its own budget allows — earlier tests in this run were degraded too
```

`TestKernelStackResolution` appends the symbol set it did see:

```
  functions in the last capture: [__schedule ext4_writepages vfs_fsync ...]
```

`TestOffBoxLibcResolution` names why it refuses to proceed:

```
symbolization never resolved a workload symbol, so the "libc was not fetched" assertion below would pass vacuously: timed out waiting for a profile in which the workload's own symbols resolved (proving symbolization ran): ...
```

Per-attempt progress is logged as it happens:

```
collect-until: attempt 1 did not satisfy a CPU profile with samples, stack traces and at least one symbolized function after 10.4s; an EMPTY profile: 0 samples (the collection window caught the workload off-CPU the whole time)
collect-until: a CPU profile with samples, stack traces and at least one symbolized function satisfied on attempt 2 after 21.1s; samples=612 ...
```

The dead-workload guard:

```
workload go/io_bound (pid 9136) exited before the collect-until budget was spent — its run duration is shorter than the collection budget, so the condition under test is unreachable
```

---

## 9. Termination, spin-freedom, reachability

Walked by hand and pinned by the unit tests in `test/collect_until_test.go`.

**Terminates.** `maxAttempts = budget/window + 1` bounds the iteration count independently
of the clock. Separately, the loop breaks when `elapsed + window > budget`, and again when
the package-wide allowance cannot fund another attempt. Upper bound per loop is
`budget + one window`.

**Cannot spin.** The clock-based break alone would be insufficient if an attempt returned
instantly — `elapsed` would stay near zero and the loop would iterate as fast as the CPU
allows. That is why the attempt-count bound exists. `TestCollectUntilCannotSpin` pins it.

**Deadlines enforced.** `TestCollectUntilStopsWhenNoAttemptFits` pins the per-loop clock
bound (an attempt consuming the whole budget is not retried: exactly 1 call).
`TestCollectUntilRespectsTheSuiteWideAllowance` pins the global one.
`TestCollectUntilChargesOnlyReCollections` pins the accounting.

**Preconditions do not imply assertions.**
`TestUserSpacePreconditionDoesNotImplyRealMapping` and
`TestSymptomThreeIsJudgedNotRetried` (§2, §3).

**Conditions are reachable in the environment the tests create:**

- `main.*`/`runtime.*` frames from `io_bound` — the workload allocates a 1 MB buffer,
  `io.ReadAll`s the file back, and is a Go program with a GC, so user-space frames run on
  every iteration; only the *sampling* of them is probabilistic.
- resolved kernel symbols under `kptr_restrict=0` — the regex includes `vfs_`, `ksys_`,
  `do_sys_` and `__schedule`, all present on arm64 as well as amd64.
- `cpu_intensive_work` — `#[inline(never)]`, the Rust workload's only hot loop.
- ≥ `degenerateSampleFloor` samples with ≥ 1 user-space frame in a system-wide capture —
  a 5 s window at 99 Hz on a runner with a CPU-bound rust workload the test started
  itself; 20 is a low bar, and a timeout here only logs and falls through.
- ≥ 2 distinct PIDs in a system-wide perf.data — the runner is not idle.
- the debuginfod cache entry in `runStripped` — populated by symbolizing any sampled PC in
  a CPU-bound spinner.

**Workload outlives the budget:** 150 s against a worst case of `budget + window` ≤ 35 s
plus ≤ 3 s of setup.

---

## 10. Cannot verify — what was NOT executed

**The integration suite was not run. Not once, in whole or in part.** This session has
`CapEff: 0` and no root: every test in `test/integration_test.go` requires root or
`CAP_BPF`, and `requireBPFRunnable` skips them regardless. No claim here about profiler
behaviour under load is backed by an observed run, and the integration-test messages in §8
are renderings of the new format strings, not transcripts.

What *was* executed:

| command | result |
|---|---|
| `go build ./...` (root module) | pass |
| `go vet ./...` (root module) | pass |
| `golangci-lint run --timeout=5m` (root module) | `0 issues.` |
| `cd test && go vet ./...` | pass |
| `cd test && go vet -tags integration ./...` | pass |
| `cd test && go build ./...` | pass |
| `cd test && go test -c` | builds |
| `cd test && go test -tags integration -c` | builds |
| `gofmt -l test/` | clean |
| `cd test && golangci-lint run` | 26 issues, all pre-existing (baseline on `main`: 29; the test module is not linted by CI) |
| `cd test && go test -run '<harness tests>'` | **18 tests pass** |
| standalone `fmt` probe on `[]*Mapping` (§3.1) | pointers confirmed |

That last group is the only executed *behavioural* evidence. It covers the loop bounds,
spin-freedom, both deadlines, the retry accounting, the precondition/assertion separation,
the symptom-3 reconstruction and its verdict, the round-trip mapping-preservation premise,
the mapping renderer, the frame and mapping split, the empty-vs-corrupt profile
distinction, the three `fidelityDiagnosis` branches, and `processAlive`. It touches
neither BPF, nor the agent binary, nor a real workload.

Specifically unverified, worth watching on the first CI run:

1. Whether a *second* collection attempt against the same live workload behaves like the
   first — repeated BPF load/attach/detach, repeated `--perf-data-output` overwrites, and
   for the three streaming tests repeated in-process
   `perfagent.New`/`Start`/`Stop`/`Close` cycles. Nothing in the code suggests otherwise;
   it has not been observed.
2. The real per-attempt wall-clock cost. It is inside the package-wide allowance by
   construction (§6), but it determines how many attempts a test actually gets, so the
   attempt counts in §5 are upper bounds.
3. Whether `TestPerfDwarfWalker`'s widened loop condition is ever *not* met within 20 s on
   a healthy runner. The old 5 s/40-sample loop was evidently sufficient; the new bound is
   untested.
4. **Whether `TestPerfAgentSystemWideDwarfProfile` still goes red on CI.** Per §3 it will,
   on any run that reproduces the PR #48 capture — this change does not fix that symptom.
   If it recurs, §3.5 lists the evidence that decides whether it is a resolver bug, and
   the message now prints most of it.

---

## 11. What the reviews changed

**Round 1**

| finding | disposition |
|---|---|
| 🔴 three sites looped on `RealMappings >= 1`, which `assertPprofFidelity` asserts | Accepted and fixed — §2. Implemented as user-space frames rather than the suggested `UserFrames > 0`, because that alone still implies `RealMappings >= 1`. |
| unreachable-if-satisfied assertions at the 7 `t.Fatal` sites | Accepted. Code left as-is; the report's claim was the error and is corrected in §4 and §7.3. |
| runtime figure wrong, headroom thin | Accepted — ~12.3 min is right. Fixed with a package-wide 3 min allowance rather than by cutting budgets or touching the CI timeout (out of scope, flagged) — §6. |
| `TestOffBoxLibcResolution` left behind with the defect just fixed | Accepted and converted, residual gap stated — §5. |
| report claims to correct | Done — the "loop condition IS the assertion" claim, the `spawnBinaryAsWorkload` framing, the vacuous-pass finding narrowed to its one real caller and promoted. |
| minor: fake `||` tolerance at `:914-917` | Left alone with reasons; being filed separately. |

**Round 2 — "does this still fix symptom 3?"**

| question | answer |
|---|---|
| What is `[0x1311ff8c230]`? | A Go heap pointer to one `profile.Mapping`; `%+v` on a slice of struct pointers prints addresses, not fields. The message carried a mapping count and nothing else — §3.1. |
| Would `profileHasUserSpaceFrames` return true on that profile? | **Yes.** The loop exits on the first capture and the assertion fails. **This change does not fix symptom 3**, and §3.3 and §10 now say so plainly instead of filing it as a watch item. |
| Profiler defect or starved capture? | Neither of the issue's readings survives the code: no kernel frames existed, and it was above the sample floor. It was every sampled PC failing resolver attribution. The cause cannot be proven from the log — §3.4 — and §3.5 lists the evidence that would settle it. |
| precondition needs to be something a starved capture genuinely fails | Done: `profileFidelityJudgeable` adds `Samples >= degenerateSampleFloor`, the suite's own threshold for when a zero-real-mapping profile is "a genuine bug worth a loud failure". A side effect is that few-samples-all-unmapped captures are now retried where the previous revision judged them. |
| don't paper over it either way | The verdict is pinned by `TestSymptomThreeIsJudgedNotRetried`, and the message defect that made it undiagnosable is fixed by `describeMappings` — §7.1. |
