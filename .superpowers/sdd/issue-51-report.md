# Issue #51 — Timeline join counters were computed and never surfaced

Branch `fix/surface-join-stats`, worktree `.worktrees/surface-join-stats`.

New: `gpu/joinhealth.go` (`gpu.JoinHealth(Snapshot) []string`) plus `gpu/joinhealth_test.go`.
Wired into `cmd/gpu-cuda-profile/main.go` and `cmd/gpu-stub-profile/main.go` — three lines
each, immediately after the existing `c.Stats()` log. No counter's computation changed.

## 1. Rendered output

Produced by `go test ./gpu/ -run TestJoinHealthRenderedOutput -v -count=1`, which renders
three real `Snapshot` values through the shipped code path. Reproduced verbatim; in the
drivers each line arrives through its own `log.Print`, so each carries a timestamp prefix.

### Healthy — 512 executions all joined exactly, nothing evicted

```
gpu join: 512 executions, all exact; 256 launches, all matched; cache 256 live; no anomalies
```

One line. No zero-valued field appears anywhere in it.

### Anomalous — the same run with every counter the timeline can raise

```
gpu join: 512 executions (470 exact, 22 heuristic, 20 unmatched); 260 launches (240 matched, 20 unmatched); cache 250 live; pc samples 900 attributed, 12 pending; 13 anomalies
gpu join ANOMALY: 20 of 512 executions unmatched — GPU time arrived with no launch to attach it to; it is in the profile under [gpu:launch unsampled] carrying no CPU stack
gpu join ANOMALY: 22 of 512 executions joined heuristically — matched on queue, kernel name and timing rather than vendor correlation; the CPU stack may be another launch's
gpu join ANOMALY: 4 heuristic joins flagged ambiguous — more than one launch qualified and one was chosen; these are the least trustworthy stacks in the profile
gpu join ANOMALY: 7 of the unmatched executions had a candidate launch just outside LaunchEventJoinWindowNs — widen that window or snapshot more often
gpu join ANOMALY: launch cache evicted 47 launches at capacity (250 live) — too small for the launch rate, so their executions cannot join; raise TimelineConfig.LaunchCache.Capacity
gpu join ANOMALY: launch cache evicted 3 launches past HorizonNs — they aged out before their execution arrived; raise the horizon if kernels sit queued that long
gpu join ANOMALY: launch cache replaced 2 live entries — the same correlation ID came back while still live; the earlier launch and its stack are gone
gpu join ANOMALY: 1 launch carried an out-of-range timestamp — the producer's clock is suspect and horizon eviction is running against a clamped anchor
gpu join ANOMALY: 128 PC samples evicted while waiting for their execution — never attributed, so the stall detail on those kernels is under-reported
gpu join ANOMALY: 9 executions evicted from the timeline ring before this snapshot — that GPU time is missing from the profile entirely; snapshot more often or raise the capacity
gpu join ANOMALY: 11 timeline events evicted before this snapshot — raise TimelineConfig.EventCapacity
gpu join ANOMALY: 1 module evicted before this snapshot — kernels from evicted modules resolve to bare addresses
gpu join ANOMALY: sink dropped 64 PC samples at admission (60 full, 4 invalid, 0 downstream) — they never reached the timeline, so no join counter above accounts for them
```

### Empty snapshot — the degenerate worst case

```
gpu join: no executions; no launches; cache 0 live; 1 anomaly
gpu join ANOMALY: no executions in this snapshot — nothing was attributed at all; check the probe attachment and the sink before reading anything below
```

### Counters that do not reconcile with the executions present

```
gpu join: 512 executions (500 exact, 0 heuristic, 0 unmatched); 256 launches, all matched; cache 256 live; 1 anomaly
gpu join ANOMALY: join outcomes sum to 500 but the snapshot holds 512 executions — the join counters disagree with what is actually present; treat every figure below as unreliable
```

Without this condition the first line prints on its own, a breakdown that quietly does not
add up and that reads exactly as authoritative as a correct one.

## 2. Design reasoning

**Why not `%+v`.** The obvious fix — add `join=%+v launch_cache=%+v` next to the existing
`stats=%+v` — is what created the problem it is supposed to solve. `JoinStats` has eight
fields, `LaunchCacheStats` five, `TimelineDropStats` four; on a healthy run seventeen of
those print as `Field:0`. A line that is a wall of zeros on every good run is a line the
reader learns to skip, and it is then skipped on the one run where
`UnmatchedExecutionCount` was not zero. So the healthy case is one short sentence
containing no zero-valued field at all, and the anomalous case is *longer* — length itself
is the signal. Grepping `gpu join ANOMALY` finds every bad run in a log directory.

**Anomalies get their own line, with a consequence attached.** `EvictedCapacity: 47` says
nothing to someone who has not read `launchcache.go`. Each anomaly line is
`<count and what was counted> — <what it costs, and the knob>`, capped at one line. The
unmatched-executions line names `[gpu:launch unsampled]` because that is the frame the
reader will find those samples under in the resulting pprof, which is the fastest way to
confirm the counter against the artifact.

**The one derived figure, and what it reads at both ends.** The summary ends with
`no anomalies` / `N anomalies`. It is a count of raised conditions, not a ratio:

- **Normal:** `no anomalies` — and it can only print that when *every* condition is
  unraised, including "no executions in this snapshot".
- **Worst:** it grows. Nothing arrived → 1 (the empty-snapshot condition). Everything went
  wrong → 13, as above. There is no input on which it reads `no anomalies` while a counter
  it covers is non-zero; `TestJoinHealthSummaryCountMatchesTheLinesBelowIt` asserts the
  number equals `len(lines)-1` for all three fixtures.

The trap this deliberately avoids is the ratio form. A "join success rate" would print
`100%` for a snapshot with zero executions — green precisely when the profiler produced
nothing — which is the exact shape of the seven defects already found on this project.
`TestJoinHealthEmptySnapshotIsAnAnomalyNotAllExact` pins that: an empty `Snapshot` must
not say "all exact".

**Executions are counted from `len(snap.Executions)`, not from a `JoinStats` field, and
the renderer checks the sum.** The three outcomes are mutually exclusive and every
execution takes exactly one of them, so they must sum to the slice length. Quoting the
slice gives a denominator that does not come from the counters being audited — but leaving
the comparison to the reader's arithmetic is not surfacing it. A breakdown that fails to
add up is not something anyone spots by eye, and unchecked it makes every figure beside it
look authoritative while being wrong. So the mismatch is its own anomaly, carrying both
figures so the discrepancy is legible rather than computed, and it is raised **first** —
before the conditions whose numbers it casts doubt on — because it decides whether the rest
of the line can be believed at all. It fires in both directions (under- and over-reporting)
and stays silent on all three reconciling fixtures.

**`UnmatchedLaunchCount` is in the summary but is not an anomaly.** A launch whose
execution has not landed yet is the normal state at any snapshot boundary. Raising it would
fire on every healthy periodic snapshot and devalue the word ANOMALY for the counters that
do mean something. It is still printed whenever it is non-zero.

**`SinkStats` is included even though both drivers leave it zero.** They pass the
`Timeline` as the sink directly, so nothing populates it today — but it is a field of
`Snapshot`, and a renderer that silently ignored a populated one would be a fresh instance
of this same bug the moment the wiring uses `CountingSink.SnapshotWith`. Being zero, it
costs nothing on the healthy line.

**Scope boundary, stated in the doc comment.** `JoinHealth` sees only what `Snapshot`
knows. Loss upstream of the sink — `gpuprobe.Stats.KernelDropped`, `SequenceGaps`,
`Malformed`, `ZeroCorrelation` — is invisible to it, which is why both drivers keep their
existing `c.Stats()` line and why `no anomalies` is phrased under a `gpu join:` prefix: it
means the join is clean, not that nothing was lost reaching it.

**Time scales differ and are documented.** `JoinStats` is per-snapshot (`Timeline` resets
`launchesSinceSnapshot` on every `Snapshot` call); `LaunchCacheStats` and
`TimelineDropStats` are cumulative over the `Timeline`'s life. Both drivers snapshot
exactly once at the end, so the two coincide there. A periodic caller must read the
eviction figures as running totals — noted in the `JoinHealth` doc comment so the eventual
exporter wiring does not mix them.

## 3. Exported-metric recommendation (`metrics/exporter.go`)

`metrics.MetricsSnapshot` today is `{Timestamp, SystemWide, Processes map[uint32]*ProcessMetrics}`
and `Exporter` is `Export(ctx, *MetricsSnapshot) error` / `Name() string`. GPU is not wired
into `perfagent/` at all yet. Recommendation for when it is:

**Add a process-independent `GPU *GPUMetrics` field to `MetricsSnapshot`, not a per-PID
one.** The join happens in one `Timeline` shared across every profiled process; the launch
cache is process-qualified internally (issue #36) but its eviction counters are not
attributable to a PID. Forcing these into `ProcessMetrics` would require inventing a
per-process split that the code does not have.

**Export as counters (monotonic), converted to per-interval deltas by the exporter, with
one gauge.** Concretely:

| Field | Kind | Why it must be exported |
|---|---|---|
| `ExecutionsExact` | counter | the denominator every other join figure is read against |
| `ExecutionsHeuristic` | counter | attribution quality; rising means stacks are guesses |
| `ExecutionsUnmatched` | counter | **the alarm.** GPU time with no CPU stack |
| `HeuristicAmbiguous` | counter | subset of heuristic; the least trustworthy stacks |
| `OutOfWindowDrops` | counter | subset of unmatched with a specific, fixable cause |
| `LaunchCacheEvictedCapacity` | counter | **the alarm.** joins dropped because the cache is undersized |
| `LaunchCacheEvictedHorizon` | counter | joins dropped because launches aged out |
| `LaunchCacheReplaced` | counter | correlation-ID reuse while live; a launch's stack was overwritten |
| `LaunchCacheAnomalousTimestamp` | counter | producer clock fault; poisons horizon eviction |
| `PendingSamplesEvicted` | counter | PC samples that will never be attributed |
| `TimelineEvictedExecutions` | counter | GPU time never reaching the profile at all |
| `LaunchCacheLive` | gauge | capacity headroom; the thing `EvictedCapacity` is a symptom of |

Deliberately **not** exported: `LaunchCount` / `MatchedLaunchCount` / `UnmatchedLaunchCount`
(unmatched-at-boundary is normal and would alert constantly; the ratio is recoverable from
the execution counters), and `TimelineEvictedEvents` / `TimelineEvictedModules` (raw
syscall traffic and symbol tables — real losses, but they degrade detail rather than
attribution, and belong in logs). `SinkStats` should be exported too, but as part of
whatever wires `CountingSink`, since it is ingestion rather than join.

**Two rules the wiring must inherit:**

1. **Export counters, never a success rate.** Any exporter-side "join health %" must be
   computed by the *consumer* of the metrics from the exported counters, so that the
   zero-traffic case shows as `executions_total == 0` rather than as `100%`. A ratio
   computed inside `perf-agent` and exported as a single number is the failure mode this
   issue exists to prevent, one abstraction layer up.
2. **The alert is on the delta, not the level.** `ExecutionsUnmatched` is cumulative;
   alerting on its absolute value fires forever after one bad minute. Alert on
   `rate(ExecutionsUnmatched) > 0` alongside `rate(ExecutionsExact) > 0`, so a stopped
   pipeline (both rates zero) is distinguishable from a healthy one.

`metrics/console.go` should render them the same way `JoinHealth` does — compact when
clean, expanded per anomaly — rather than a table of zeros.

## 4. Cannot verify

**No GPU run was performed and none is possible here.** `CapEff: 0` — no `CAP_BPF`, no
`CAP_PERFMON`, so `gpuprobe.Attach` cannot load or attach anything, and there is no NVIDIA
device reachable from this environment. Consequently:

- Neither `cmd/gpu-cuda-profile` nor `cmd/gpu-stub-profile` was executed. Their two added
  call sites (`for _, line := range gpu.JoinHealth(snap) { log.Print(line) }`) are verified
  only by `go build` / `go vet` / `golangci-lint`, not by observing their output.
- Every rendering in §1 comes from `Snapshot` values constructed in the test, four of them
  hand-built and one produced by a real `gpu.Timeline`
  (`TestJoinHealthAgainstARealTimelineSnapshot`). The *renderer* is exercised end to end;
  the *counter values* a real CUDA run would produce are not, so the specific numbers above
  are illustrative, not measured.
- The claim "these lines appear after the `wrote …` line in a real run" is inferred from
  the call-site position, not observed.

**Not verified either:** that the counters are computed correctly. This change is
surfacing-only, as the issue scopes it. Reading `Timeline.Snapshot` while writing the
renderer surfaced no counter that reads green when things are worst — `UnmatchedExecutionCount`
is incremented on both miss paths, `OutOfWindowDropCount` is a strict subset of it, and
`MatchedLaunchCount` is `len(matched)` over a set, so it cannot exceed `LaunchCount` and the
subtraction guarding `UnmatchedLaunchCount` is already conditional. No defect to report. The
reconciliation anomaly added above is a check on the numbers being surfaced, not a change to
how any of them is computed, so it stays inside #51's scope — and it is the mechanism that
would make such a defect visible if one is ever introduced.

## 5. Verification performed

```
go build ./...                                        OK
go vet ./...                                          OK
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1     all ok
~/go/bin/golangci-lint run --timeout=5m                0 issues
gofmt -l gpu cmd                                       clean
```

(with `CGO_CFLAGS` / `CGO_LDFLAGS` / `LD_LIBRARY_PATH` pointing at blazesym as in CLAUDE.md)
