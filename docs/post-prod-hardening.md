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

## Findings from running the self scenario

Running `make bench-self` on this codebase, then `addr2line`-ing
the raw stacks from perf-agent #2's profile of perf-agent #1
(workload: `cpu_bound -threads=2`, capture 25s @ 99Hz), surfaced
the following hot categories. Frequencies are from a single 25s
capture — illustrative, not statistically rigorous.

| Category | Samples | What it is |
|---|---|---|
| `net.*` (DNS / sockets) | 5 | DEBUGINFOD_URLS resolution + socket setup on critical path |
| `cilium/ebpf/btf.*` | 5 | BTF parse + CO-RE relocation at BPF load |
| `cilium/ebpf/asm.*` | 4 | BPF instruction encoding |
| `cilium/ebpf.*` | 2 | `newProgramWithOptions` (BPF program load) |
| `modernc.org/sqlite` | 2 | debuginfod cache SQLite init |
| `pprof.(*Line).encode` | 1 | pprof output building |
| `perfdata.encodeMmap2` | 1 | system-wide MMAP2 emission (item #8) |

85 of 119 unique addresses were unresolved by `addr2line` — those
are the statically-linked Rust blazesym + some Go addresses
addr2line can't follow. Symbol fidelity matches what an operator
running the bench would actually see.

### Bugs found while running the bench (worth their own follow-ups)

**A. `DebuginfodSymbolizer` doesn't fall back to local on NULL.**
With `DEBUGINFOD_URLS=https://debuginfod.fedoraproject.org/` set,
profiling a locally-built binary (build-id not present upstream)
makes blazesym's process-symbolize return NULL, and perf-agent
gives up — every userspace frame appears as `<unknown>`. Should
fall back to `LocalSymbolizer.SymbolizeProcess` and cache the
per-build-id decision so we don't keep retrying upstream.

**B. Userspace symbolize EPERM when the target has file caps.**
Profiling a setcap'd binary (e.g., a second perf-agent) hits
`permission denied` from blazesym when it tries `/proc/<pid>/exe`.
The kernel restricts the symlink for privileged targets unless
`PTRACE_MODE_READ_REALCREDS` is granted. perf-agent already parses
`/proc/<pid>/maps` (via `procmap.Resolver`) — should pass those
paths to blazesym instead of letting it follow `/proc/<pid>/exe`.

### Proposed improvements (data-driven, not yet shipped)

In rough priority order based on hot-path share and ease:

1. **Lazy `modernc.org/sqlite` init (roadmap addition).** debuginfod
   cache opens its SQLite DB unconditionally at agent startup, even
   when DEBUGINFOD_URLS isn't set or upstream is unreachable. The
   `_btreeInitPage` / `_removeFromSharingList` samples come from
   that init. Make it lazy — first symbolize miss that would
   benefit from upstream fetch triggers the open. ~0.5d.

2. **Async DEBUGINFOD_URLS resolution.** DNS + connect for
   debuginfod runs on the critical path of agent startup. Move
   behind a `sync.Once` triggered on first off-box need. ~0.5d.

3. **Fix bug A** (debuginfod NULL → local fallback). Concrete fix:
   in `symbolize/debuginfod/dispatcher.go`, on
   `blaze_symbolize_process_abs_addrs == NULL`, retry through
   `LocalSymbolizer`. Add a "fell-back-to-local" counter
   (extending #2's `symbolize.Counters`). ~3h.

4. **Fix bug B** (target-has-caps EPERM). Concrete fix: pass
   per-mapping `Mapping.Path` (from `procmap.Resolver`) into
   blazesym instead of relying on `/proc/<pid>/exe`. ~half-day.

5. **Cache cilium/ebpf BTF parse across in-process invocations.**
   The BTF samples are dominated by `parseBTFHeader` /
   `LoadSplitSpecFromReader`. cilium/ebpf has package-level state
   for some of this; verify whether we re-parse vmlinux BTF more
   often than needed. ~half-day investigation, then varies.

6. **Roadmap #9 (lazy MMAP2 on first new-PID sample) gets sharper
   motivation from this profile**: `encodeMmap2` shows up in
   the hot list because system-wide mode emits MMAP2 for ~9000
   PIDs at writer init. Lazy emission would cut that to the
   PIDs we actually sample (likely 10s, not 1000s).

### Dogfood iteration log

The self scenario was actually run against this branch four times,
each iteration finding a new bottleneck and the next iteration
confirming the fix and revealing what was hidden underneath:

| # | Visible bottleneck | Fix landed | Hidden underneath |
|---|---|---|---|
| 1 | 100% `<unknown>` in user pprof | (this PR's earlier symbolize fallback chain) | per-IP empty-name failures rendered as `<unknown>` |
| 2 | `<unknown>` still 76% of samples | `frameFromCSym` / `fromBlazesymSym` fill empty Name with hex (symmetric to kernel side) | 40% of "user" CPU was kernel addresses leaked from BPF user-stack walker |
| 3 | `module_get_kallsym` + `seq_read_iter` 40% (kernel-side) | `bpfstack.SplitUserKernelIPs` routes the leak to the kernel symbolizer; 256 KiB read buffer on `/proc/kallsyms` | the kallsyms read of ~3M lines was forcing the kernel through `vsnprintf` per small read() |
| 4 | `do_syscall_64` 42%, `finish_task_switch` 21%, `ep_poll` 10%, ring-buffer read 1% | (none — intrinsic) | diminishing returns: clock / futex / ring-buffer driven by sample rate |

After iteration 4 the top user-side functions resolve to:
`Syscall6`, `nanotime1`, `time.now`, `futex`, `ringbuf.readRecord`,
`dwarfagent.aggregateCPUSample`. Genuine steady-state cost of an
eBPF-based profiler.

Bugs surfaced and fixed along the way (in addition to the lockdown
fix that motivated the PR):

- **A**. `DebuginfodSymbolizer` swallowed NULL returns. Fix:
  `LocalSymbolizer` fallback inside the dispatcher.
- **B**. Userspace symbolize EPERM when target has caps (e.g.,
  a setcap'd perf-agent profiling another setcap'd perf-agent).
  Fix: `rawUserAddrFrames` synthesis preserves stack shape and
  addresses (matches the kernel-side raw-hex behavior in this PR).
- **B'**. Per-IP empty Name in successful blazesym calls (the
  blazesym successfully ran but couldn't resolve some IPs)
  rendered as `<unknown>`. Fix: hex-name fallback in
  `frameFromCSym` + `fromBlazesymSym`.
- **C**. BPF user-stack walker leaks kernel IPs into the user
  buffer when the sampled task is in syscall/irq/fault context.
  Fix: `bpfstack.SplitUserKernelIPs` partitions and routes.
- **D**. `/proc/kallsyms` parser used the default 4 KiB read
  buffer, forcing the kernel through `vsnprintf` on each small
  read. Fix: 256 KiB `bufio.NewReaderSize` wrap.

### Limitations of the analysis

- Single 25s capture, no statistical replication. Use the JSON
  output from #1 (`make bench-self`) across N runs for trend.
- The workload (`cpu_bound`) is mild; perf-agent is dominated by
  startup. A noisier workload (multi-PID system-wide) would shift
  the distribution toward sample-processing hot paths
  (`profile/profiler.go`, `pprof.AddSample`, blazesym
  `process_abs_addrs` per-batch overhead).
- 71% of addresses were unresolved (Rust blazesym statically
  linked without debug info linkage). Address that to read the
  full perf-agent profile cleanly: build blazesym with
  `RUSTFLAGS="-C debuginfo=2"` and ensure `addr2line` picks up
  the static archive's debug info.
