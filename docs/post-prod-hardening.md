# Post-Prod-Hardening — Improvement Track

Captures follow-up work surfaced while fixing the v1.2.0 kernel-stacks
production gaps (lockdown=integrity symbolization + perf.data
userspace MMAP2 records). None of these are blocking the current PR;
each can land as its own focused change.

## Observability of perf-agent itself

perf-agent is invisible to its own users. The lockdown bug shipped in
M1 because nothing measured "did kernel symbolization actually
succeed?" — operators just saw partial flames and moved on. The
improvements below close that gap.

### 1. Self-profile lane

A second perf-agent profiling the first perf-agent during a fixed
workload. Asserts:

- perf-agent's CPU ≤ N% of sampled CPU (overhead regression gate)
- perf-agent's kernel-symbol resolution rate ≥ M (would have caught
  the lockdown bug — the first agent's flames would show kernel side
  empty, the second agent's pprof would carry the evidence)

Suggested location: `bench/self/`. Wires into the existing
`bench/cmd/scenario/` harness — runs in CI on every PR touching
`profile/`, `offcpu/`, `symbolize/`.

### 2. `metrics.Exporter` histograms

Today: nothing. Add:

- `symbolize.kernel.batch_duration_us` (p50, p99)
- `symbolize.user.batch_duration_us` (p50, p99)
- `symbolize.kernel.fallback_engaged` (counter; bumped once when
  blazesym → kallsymsSymbolizer switch fires)
- `samples.drops` (BPF ring-buffer overruns — currently silent)
- `proc.maps.parses_per_sec`

Existing `metrics.Exporter` interface (`metrics/exporter.go`) already
accepts arbitrary counters; just need wire-up at the call sites.

### 3. `--metrics-listen` HTTP endpoint

`/metrics` (Prometheus-style) + `/debug/pprof` (Go runtime
self-pprof). Trivial to ship and zero overhead when not scraped.

### 4. Symbolizer error counter by reason

Today: `log.Printf("Failed to symbolize kernel: %v", err)` into the
void. Bucket the errors:

- `lockdown_eperm`
- `kallsyms_unreadable`
- `kallsyms_unknown_address` (raw-hex fallback engaged)
- `blazesym_misc`
- `no_buildid` (user-side)

Exposed via #2 above so dashboards can alert on the ratio.

## Regression gates (cheap to add)

### 5. CI lockdown lane

Run the existing kernel-stacks integration test with
`PERFAGENT_FORCE_KERNEL_FALLBACK=1`. The env-var faker added in this
PR makes this a one-line CI matrix entry: every kernel-stacks PR
exercises the pure-Go fallback path on whatever host runs CI.

### 6. `go test -bench` for the symbolize hot path

Today: no benchmarks for symbolize. Budget per-batch CGO cost. Catches
blazesym pin regressions and pure-Go kallsyms-walker regressions.
Suggested file: `symbolize/bench_test.go`.

### 7. Allocations budget

`pprof.AllocProfile` during integration tests; budget allocs/sample.
The sample-processing loop in `profile/profiler.go` and
`offcpu/profiler.go` churns `Frame{}` structs hard — every kernel +
user frame is one alloc, with the `Inlined` chain adding more.
Pool-reuse opportunity once the budget is in place.

## Capture quality

### 8. System-wide userspace MMAP2

The Bug 3 fix is single-PID only. For `-a` (system-wide), `perf
record` does a `/proc/*/maps` walk at start. Needs
`perfdata.Writer.AddAllUserspaceMmaps` that scoops every executable
mapping on the host, gated by a `--snapshot-all-mmaps` flag so the
perf.data overhead is opt-in. Out of scope for the prod-hardening PR
but the natural follow-up.

### 9. Lazy MMAP2 on first new-PID sample

For `-a`, instead of pre-walking thousands of PIDs (expensive on
busy hosts), emit MMAP2 lazily when a sample carries a PID we
haven't seen. Tracks `exec()` correctly too — pre-walking misses
processes that fork after capture starts. Complements #8.

### 10. kthread MMAP / COMM attribution

In the blog flames that motivated this work, KVM kthreads
(`kvm-pit/*`, `vhost-*`) showed real CPU but with truncated kernel
stacks. Their kernel stacks go through the `[kernel.kallsyms]_text`
MMAP fine, but `comm` records may not be emitted for kthreads —
worth checking that `AddComm` runs for kernel-only PIDs.

## Triage / ordering

| ID | Effort | Priority | Notes |
|----|--------|----------|-------|
| 1  | 1d     | High     | Catches future lockdown-class bugs at PR time |
| 2  | 0.5d   | High     | Foundation for everything else |
| 3  | 0.5d   | Med      | Easy wins once #2 ships |
| 4  | 0.5d   | Med      | Depends on #2 |
| 5  | 5min   | High     | One-line CI matrix entry |
| 6  | 0.5d   | Med      | Bench infrastructure already exists |
| 7  | 1d     | Low      | Needs #2 first |
| 8  | 1d     | Med      | The `-a` user pain point |
| 9  | 1d     | Med      | Combines with #8 |
| 10 | 0.5d   | Low      | Probably already works; verify with #5 |

Recommended order: **5 → 2 → 1 → 4 → 3 → 6 → 8 → 9 → 7 → 10**. The
first three together would have prevented the v1.2.0 ship-and-pray
incident this PR cleaned up.
