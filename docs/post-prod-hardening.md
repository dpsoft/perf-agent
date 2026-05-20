# Post-Prod-Hardening — Improvement Track

Captures follow-up work surfaced while fixing the v1.2.0 kernel-stacks
production gaps (lockdown=integrity symbolization + perf.data
userspace MMAP2 records).

**Status:** Items #1, #2, #5, #8, #10 shipped in the same PR as the
bug fixes. Items #3, #4, #6, #7, #9 remain as follow-ups.

## Observability of perf-agent itself

perf-agent is invisible to its own users. The lockdown bug shipped in
M1 because nothing measured "did kernel symbolization actually
succeed?" — operators just saw partial flames and moved on. The
improvements below close that gap.

### 1. Self-profile lane

**Shipped in this PR.** New `self` scenario in
`bench/cmd/scenario/`: perf-agent #1 profiles a CPU workload while
perf-agent #2 profiles perf-agent #1. Outputs JSON with:

- `cpu_overhead_ratio` — agent samples / workload samples (a
  straight comparison at the same sample rate over the same window)
- `kernel_resolution_rate` — fraction of kernel-side Locations
  whose Function.Name is non-hex (i.e., resolved by blazesym or
  the kallsyms fallback rather than dropped to "0x<addr>")

Budget gates: `--cpu-budget` (max overhead) and
`--resolution-budget` (min resolution rate) on the scenario binary
fail the run with a non-zero exit when breached. `make bench-self`
runs the canonical 3×10s configuration with 10% CPU / 50% kernel
resolution gates.

Future: wire `make bench-self` into a CI lane that runs on every
PR touching `profile/`, `offcpu/`, `symbolize/`.

### 2. `metrics.Exporter` histograms

**Partially shipped in this PR.** `symbolize.Counters` (atomic-based)
covers: KernelBatches, KernelInputIPs, KernelBatchFailures,
KernelFallbackEngaged, KernelRawAddrFrames. Logged at agent shutdown
via `agent.cleanup`.

Still to do:

- Histograms (p50/p99) for blazesym + kallsyms batch durations.
  Requires reservoir sampling or HDR-histogram; deferred.
- `samples.drops` (BPF ring-buffer overruns — currently silent).
- `proc.maps.parses_per_sec`.
- Integration with the existing `metrics.Exporter` snapshot model so
  ConsoleExporter / future PrometheusExporter can pick them up.

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

**Shipped in this PR.** `make test-integration-lockdown` runs the
kernel-stacks integration tests with
`PERFAGENT_FORCE_KERNEL_FALLBACK=1`. Wired into the default `make
test` target so every CI run exercises the kallsyms fallback path
regardless of the host's lockdown state.

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

**Shipped in this PR.** `perfagent.emitCommAndMmapsForAllPIDs` walks
/proc at writer init and emits PERF_RECORD_MMAP2 (plus COMM, see
#10) for every visible PID. Integration test
`TestPerfDataUserspaceMmap2_SystemWide` verifies the walk covers
multiple distinct PIDs.

Remaining gap: snapshot semantics miss processes that exec after the
walk. Roadmap #9 below.

### 9. Lazy MMAP2 on first new-PID sample

For `-a`, instead of pre-walking thousands of PIDs (expensive on
busy hosts), emit MMAP2 lazily when a sample carries a PID we
haven't seen. Tracks `exec()` correctly too — pre-walking misses
processes that fork after capture starts. Complements #8.

### 10. kthread MMAP / COMM attribution

**Shipped in this PR.** Audit found that `perfdata.Writer.AddComm`
was defined but never called from anywhere in perf-agent — every
perf.data emitted had zero COMM records, not just for kthreads.
`perfagent.emitCommForPID` now reads `/proc/<pid>/comm` and emits
PERF_RECORD_COMM for every PID enumerated (kthreads included, since
they have a valid comm even when their userspace maps are empty).
`TestPerfDataUserspaceMmap2` asserts the COMM record appears
alongside the MMAP2 record for the workload pid.

## Triage / ordering

| ID | Effort | Priority | Status |
|----|--------|----------|--------|
| 1  | 1d     | High     | **Shipped** in this PR — `make bench-self` |
| 2  | 0.5d   | High     | **Partial** — counters shipped, histograms pending |
| 3  | 0.5d   | Med      | Pending — easy wins once #2 fully ships |
| 4  | 0.5d   | Med      | Pending — depends on #2 |
| 5  | 5min   | High     | **Shipped** in this PR |
| 6  | 0.5d   | Med      | Pending — bench infrastructure already exists |
| 7  | 1d     | Low      | Pending — needs #2 first |
| 8  | 1d     | Med      | **Shipped** in this PR |
| 9  | 1d     | Med      | Pending — natural follow-up to #8 |
| 10 | 0.5d   | Low      | **Shipped** in this PR (AddComm was entirely unwired, not just for kthreads) |

Recommended next: **9 → 4 → 3 → 6 → 7**. With #1 in place,
catching overhead and lockdown regressions is now mechanical; the
remaining items broaden observability and capture quality.
