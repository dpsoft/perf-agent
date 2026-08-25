# Issue #63 — `TestPMUIOWorkload` asserted only that some context switch happened

Branch: `fix/pmu-io-value-assertions`. Scope: `test/` only — `metrics/console.go`, the
collector and the BPF programs are untouched.

## The defect, restated

`TestPMUIOWorkloadHasIOWait` asked:

```go
hasIOActivity := strings.Contains(outputStr, "I/O Wait (D state):") ||
    strings.Contains(outputStr, "Voluntary (sleep/mutex):")
```

`metrics/console.go:88-99` prints all three reason lines inside one `if totalSwitches > 0`
guard, so both strings appear together or not at all. The `||` therefore could not
distinguish the two branches, and the effective assertion was **"at least one context
switch of any kind was recorded"**. A collector that filed every blocked `read` under
*Preempted* and classified zero I/O wait passed a test named for I/O accounting.

`TestPMUCPUWorkloadMostlyRunning` asserted `Contains(out, "Preempted (running):")` — the
same line, from the same guard. Same defect, mirrored: a collector that filed every switch
as *Voluntary* passed a test named for a workload that never voluntarily yields.

## What was built

### 1. `test/pmu_output.go` (new) — a strict parser for the block

`parseContextSwitchReasons(out string) (contextSwitchCounts, error)` reads the counts off
the lines that already carry them:

```
Context Switch Reasons:
  Preempted (running):     19.3%  (812 times)
  Voluntary (sleep/mutex): 45.3%  (1904 times)
  I/O Wait (D state):      35.3%  (1483 times)
```

- Handles both emitters: `printSinglePIDMetrics` (`Context Switch Reasons:`) and
  `printAggregateMetrics` (`Context Switch Reasons (aggregate):`, `console.go:160-168`).
- Sums across every block in the output, so `--per-pid` system-wide reports (one block per
  process) aggregate correctly. `Blocks` records how many were summed.
- Requires the three labels, in `console.go`'s order, with the percentage **and** the count
  present. The percentage is parsed but not aggregated — parsing it is what makes a change
  to that half of the line a loud failure rather than a silent one. Summed percentages
  would be meaningless, so `Percent()` recomputes shares from the totals for diagnostics.
- `contextSwitchCounts.Blocking() = Voluntary + IOWait`, per the issue: which bucket a file
  operation lands in depends on the filesystem and page-cache state, so requiring `IOWait`
  alone would make the test a property of the runner's storage stack rather than of the
  collector.

### 2. `test/pmu_output.go` — `collectPMUContextSwitches`, the collect-until wrapper

One `perf-agent --pmu --pid N --duration 5s` run per attempt, looping until the report is
**judgeable**, bounded by `collectBudgetFor(5s)` = 15s (4 attempts max, per
`collect_until.go`'s own `maxAttempts = budget/window + 1`).

The loop condition is `counts.Total() >= ctxSwitchJudgeableFloor` (20) — a *data
sufficiency* precondition, deliberately **not** the property either caller asserts:

- It is satisfied by a report in which all 20+ switches were filed under the *wrong*
  reason. That is exactly the regression the assertions must still be able to report, so
  the loop cannot retry a genuine failure into a pass.
- Neither `Blocking() > 0` nor `Preempted > 0` is implied by it; the loop exits on the
  first attempt that has data, and then the assertion decides.

Floor of 20 mirrors `degenerateSampleFloor` in `collect_until.go`: a verdict on *how*
switches were classified is meaningless when there were two of them. Both Go workloads
produce switches in the hundreds-to-thousands per 5s window (the Go runtime's sysmon thread
alone parks ~100×/s), so 20 is a "did the collection see the target at all" bar, not a
performance expectation.

Three outcomes are kept distinct:

| condition | handling |
|---|---|
| agent exited non-zero | `t.Fatalf` immediately, with the output |
| block present but unparseable (`errContextSwitchFormat`) | `t.Fatalf` immediately, quoting the parse error and the full output, pointing at `test/pmu_output.go` — **no fallback** |
| no block at all (`errNoContextSwitchBlock`), or fewer than 20 switches | unsatisfied attempt → retried; fatal at the deadline with the last output attached |

### 3. The two rewritten tests

`TestPMUIOWorkloadHasIOWait` (name kept — it is referenced in `issue-42-report.md`):

```go
counts, output := collectPMUContextSwitches(t, agentPath, workload, wl.Name,
    "--pmu", "--pid", pid)
assert.Positive(t, counts.Blocking(), ...)
```

`TestPMUCPUWorkloadMostlyRunning`:

```go
assert.Positive(t, counts.Preempted, ...)
assert.Less(t, counts.Percent(counts.IOWait), 50.0, ...)
```

## What each new assertion catches that the old one did not

| assertion | catches |
|---|---|
| `counts.Blocking() > 0` (I/O test) | every switch of an I/O-bound workload filed as *Preempted* — D-state and S-state classification both lost. The old `\|\|` passed on this output; `TestParseContextSwitchReasonsSeesTheRegression` pins that the parser reports `Blocking() == 0` for it while both labels are still printed. |
| `counts.Preempted > 0` (CPU test) | every switch of a spinning workload filed as *Voluntary* or *I/O Wait* — the mirror loss. Pinned by `TestParseContextSwitchReasonsSeesZeroPreempted`. |
| `IOWait share < 50%` (CPU test) | a collector that files the majority of a compute-only workload's switches as uninterruptible I/O wait, i.e. D-state over-attribution rather than under-attribution. Nothing in the suite tested that direction. |
| `Total() >= 20` reached within the budget | a run that records essentially nothing now fails with a diagnostic naming the condition and the last output, instead of silently satisfying a substring check. |
| the parse succeeding at all | any drift in `console.go`'s line format. Previously a renamed label would have turned the `\|\|` false and produced "output had neither line" plus a raw dump; now the failure says *which* line, at *which* line number, what shape was expected and what was read. |

Nothing was weakened. Every old check is implied by the new preconditions: `Total() >= 20`
implies `totalSwitches > 0`, which implies `console.go` printed all three labels, which is
all the old `Contains` calls verified.

The one behavioural change worth flagging explicitly: a run that records between 1 and 19
context switches used to **pass** the I/O test and now **fails** it at the deadline. That
is a deliberate tightening, not an accident — a distribution over fewer than 20 switches
cannot support the claim either test makes — and such a run is far outside anything the Go
workloads produce (the io_bound workload does ~10 write/fsync/read cycles per second per
thread across 2 threads; the cpu_bound workload's runtime threads alone clear the floor).

## Verdict on `TestPMUCPUWorkloadMostlyRunning`

**It had the same defect and is fixed.** `assert.Contains(outputStr, "Preempted (running):")`
tests the presence of a line printed under the shared `totalSwitches > 0` guard, so it is
the same "some context switch happened" assertion wearing the opposite name.

Two judgement calls in the fix:

1. **The workload is now deliberately oversubscribed**: `2 × runtime.NumCPU()` spinning
   goroutines *with `GOMAXPROCS` raised to match*, via `cmd.Env`. Raising the goroutine
   count alone would not have worked — Go would still run only `GOMAXPROCS` OS threads and
   multiplex the goroutines in user space, producing no extra *kernel* preemption. Without
   oversubscription, `Preempted > 0` is a coin flip on the machine: on an idle many-core
   box, 4 spinning threads can each own a CPU and never be preempted in a 5s window (CFS
   does not preempt a task that is alone on its runqueue), which would have made the fix
   flaky exactly where the constraint says not to be. With every CPU carrying ≥2 runnable
   threads, preemption is a property of the run, not of the runner.
   (`exec.Cmd` dedups `Env` keeping the last occurrence, so appending `GOMAXPROCS=` after
   `os.Environ()` overrides an inherited value.)

2. **Preemption being the *majority* is deliberately not asserted**, despite the test's
   name. The Go runtime's sysmon thread parks and wakes ~100×/s and every park is a
   voluntary switch, so the majority reason for a spinning Go program is a property of the
   runtime's bookkeeping threads, not of the workload. Asserting a majority would be
   asserting a Go-runtime implementation detail; `Preempted > 0` plus "I/O wait is not the
   dominant reason" are the two claims the workload actually licenses. Both are recorded in
   the test's doc comment.

## Parser failure behaviour (real output, captured on this machine)

Fixtures rendered by driving the real `metrics.ConsoleExporter`, then mutated:

```
[count renamed]  -> could not parse the counts from this output: line 15 should be the
   "Preempted (running)" line of a "Context Switch Reasons:" block, formatted like
   "  Preempted (running):      37.2%  (1483 times)", but reads
   "  Preempted (running):     19.3%  (812 switches)"

[label reworded] -> could not parse the counts from this output: line 17 of the
   "Context Switch Reasons:" block is labelled "IO Wait (D-state)", expected
   "I/O Wait (D state)" (console.go prints the three reasons in a fixed order)

[truncated]      -> could not parse the counts from this output: line 16 should be the
   "Voluntary (sleep/mutex)" line of a "Context Switch Reasons:" block, formatted like
   "  Voluntary (sleep/mutex):      37.2%  (1483 times)", but reads "(truncated)"

[no block]       -> the output contains no "Context Switch Reasons" block
```

The first three wrap `errContextSwitchFormat` → the test dies immediately with the full
output and a pointer to the parser. The fourth is `errNoContextSwitchBlock` → retried, then
fatal at the deadline. The two are never conflated, and there is no path on which a parse
failure degrades into a substring check.

Successful parse renders as:

```
context switches: total=4199 preempted=812 (19.3%) voluntary=1904 (45.3%) io_wait=1483 (35.3%) [1 block(s)]
```

quoted by every collect-until attempt log and by every assertion failure message.

## `test/pmu_output_test.go` (new, unprivileged)

The happy-path fixtures are **not** hand-written strings: they are produced by driving
`metrics.ConsoleExporter` over a constructed `MetricsSnapshot`, which makes this a contract
test between the parser and the emitter. If `console.go`'s format drifts, it fails here, on
an unprivileged machine, rather than as an unexplained integration failure on a runner.

Coverage: single-PID block; `(aggregate)` variant; `--per-pid` multi-block summing;
all-preempted output (the regression the old test missed); zero-preempted output (the
mirror); block omitted for zero switches; `No PMU metrics collected`; the string
diagnostic and its divide-by-zero guard; and seven format mutations (count field renamed,
count dropped, percentage dropped, label reworded, reasons reordered, report truncated
after the header, block truncated mid-way) each required to wrap `errContextSwitchFormat`.

Each mutation is guarded by an `m.out == good` check, so a mutation that no longer applies
(because the format moved) fails rather than passing vacuously.

## Control-flow walk (by hand)

`collectPMUContextSwitches`:

- `collectUntil` computes `maxAttempts = 15s/5s + 1 = 4`; the `for n := 1; n <= maxAttempts`
  loop is bounded independently of the clock, so an attempt that returns instantly cannot
  spin. **Terminates.**
- Every attempt runs one `--duration 5s` agent invocation, so an attempt cannot return
  faster than the window except on a hard failure, which is `t.Fatalf` (immediate exit via
  `runtime.Goexit`; the closure runs synchronously on the test goroutine).
- `requireWorkloadAlive` fires first on every attempt, so a workload that outlived its
  duration is reported as such rather than blamed on the profiler. `workloadRuntime` is
  150s against a worst case of budget + one window = 20s.
- Exit paths: satisfied → return counts; deadline or shared-retry starvation → `t.Fatalf`
  with the report and the last output. There is no path that returns "ok" without a parsed
  block, and none that returns zeroed counts as a success.
- **Cannot pass vacuously**: the only success return carries counts from a successfully
  parsed report with `Total() >= 20`, and the assertions afterwards read fields of those
  counts. An empty/absent block never reaches the assertion; a mis-classified block does,
  and fails.

## Verification

Run on this machine:

```
go build ./... && go vet ./...                    # clean
cd test && go vet -tags integration ./...          # clean
        && go test -c -tags integration -o /dev/null ./...   # compiles
~/go/bin/golangci-lint run --timeout=5m            # root module: 0 issues
go test -run 'TestParseContextSwitchReasons|TestContextSwitchCountsString' -v ./...
```

All parser tests pass (9 top-level, 7 subtests). The harness tests in
`collect_until_test.go` still pass. `golangci-lint` inside `test/` reports 25 findings, all
of them pre-existing `errcheck`/`staticcheck` hits at `integration_test.go` lines 93–1617,
none inside the rewritten region (lines 873–980) and none in the new files.

## Cannot verify

**Nothing in the integration suite was executed.** This session has `CapEff: 0` and is not
root, so `requireBPFRunnable` would skip every PMU test and no BPF program could be loaded.
Specifically **not** verified by execution:

- that `TestPMUIOWorkloadHasIOWait` and `TestPMUCPUWorkloadMostlyRunning` pass against a
  real agent run — including whether the real `perf-agent --pmu` output parses (only
  `metrics.ConsoleExporter`'s output, rendered in-process, was parsed; that is the same
  code path, but not the same process, and any wrapper text the CLI adds around it was not
  observed);
- the empirical context-switch counts either workload produces in a 5s window, and so the
  real headroom above the floor of 20;
- that the `GOMAXPROCS` oversubscription does in fact drive `Preempted > 0` on a real
  runner — the argument for it is mechanical (≥2 runnable threads per CPU forces CFS
  preemption) but unmeasured here;
- the wall-clock cost of the added retries (worst case +15s per test, bounded by the
  package-wide `suiteRetryBudget`, which these two loops now also draw on).
