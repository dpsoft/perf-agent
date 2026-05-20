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

**Shipped in this PR.** `symbolize.Counters` carries:

- Counters: KernelBatches, KernelInputIPs, KernelBatchFailures,
  KernelFallbackEngaged, KernelRawAddrFrames, KernelLockdownEPERM
  (#4), KernelOtherErr (#4)
- LatencyHist: KernelBatchHist — sliding-window (1024 samples)
  ring-buffer histogram of per-SymbolizeKernel-call wall-clock
  duration, with min/max/mean/p50/p99 on snapshot

Exposed via end-of-run log line and `/metrics` HTTP (#3) as
`*_p50_microseconds`, `*_p99_microseconds`,
`*_max_microseconds`. Snapshot is mutex-guarded but the Record
path is zero-alloc (TestAllocsBudget_LatencyHistRecord).

Still to ship in a follow-up:
- `samples.drops` (BPF ring-buffer overruns — currently silent)
- `proc.maps.parses_per_sec`
- User-side equivalents on `LocalSymbolizer` / `DebuginfodSymbolizer`

### 3. `--metrics-listen` HTTP endpoint

**Shipped in this PR.** `--metrics-listen <addr>` (e.g.
`127.0.0.1:7777`) starts an HTTP server hosting:

- `/metrics` — Prometheus text format with all
  `symbolize.Counters` fields, including the reason buckets
  added in #4 (`KernelLockdownEPERM`, `KernelOtherErr`)
- `/debug/pprof/` — full Go runtime self-pprof, so
  `go tool pprof http://host:7777/debug/pprof/profile` works
  live (vs the offline bench-self path)

Default off — no port opened when the flag isn't set. Lifecycle
is wired into `Agent.Start` / `Agent.cleanup` with a 2-second
graceful shutdown on Close. Tests cover Prometheus format, live
scrape against a `:0` listener, post-Stop liveness, and the
pprof mount.

### 4. Symbolizer error counter by reason

**Shipped in this PR.** `symbolize.Counters` gained
`KernelLockdownEPERM` and `KernelOtherErr` (atomic, bumped at
the cgoSymbolize error classification site). `CountersSnapshot`
+ String() now include `eperm=` and `other_err=` fields so the
end-of-run log line distinguishes:

  - `eperm=N` matching `fallback_engaged=1` → canonical
    lockdown signature
  - `other_err > 0` → unexpected blazesym failure, deserves a
    look at log lines around the failure

Remaining (user-side reason buckets — debuginfod NoBuildID etc.):
left for a follow-up PR that adds a sibling Counters struct on
the user symbolizer types.

## Regression gates (cheap to add)

### 5. CI lockdown lane

**Shipped in this PR.** `make test-integration-lockdown` runs the
kernel-stacks integration tests with
`PERFAGENT_FORCE_KERNEL_FALLBACK=1`. Wired into the default `make
test` target so every CI run exercises the kallsyms fallback path
regardless of the host's lockdown state.

### 6. `go test -bench` for the symbolize hot path

**Shipped in this PR.** `symbolize/kallsyms_bench_test.go`
covers: full fresh parse, cache load, per-IP resolve, per-line
parser. `make bench-symbolize` is the canonical entry point.
Reference numbers on a Ryzen 9 7940HS + Fedora 44 (~225k
filtered kallsyms symbols):

  BenchmarkParseKallsymsFresh   204 ms/op  365k allocs   81 MB
  BenchmarkLoadCachedKallsyms    10 ms/op  365k allocs   32 MB  (20x)
  BenchmarkResolveKernelIPs     ~35 ns/IP   1 alloc/call
  BenchmarkParseKallsymsLine   17.5 ns/op   0 allocs

The 20x cache speedup is the headline metric — PRs that
regress it will surface in CI when the bench-symbolize lane
runs (currently manual).

### 7. Allocations budget

**Partially shipped in this PR.** `symbolize/allocs_budget_test.go`
uses `testing.AllocsPerRun` to assert hot-path functions stay
within their budget:

- `parseKallsymsLine`: 0 allocs/op
- `(*kallsymsSymbolizer).Resolve`: 1 alloc/op (the return slice)
- `LatencyHist.Record`: 0 allocs/op

PRs that regress these will fail the test immediately, not just
show worse numbers in benchstat.

Still to ship in a follow-up: extend the budget gate to the
sample-processing loop in `profile/profiler.go` and
`offcpu/profiler.go`. Those allocate `Frame{}` structs per
captured frame plus the `Inlined` chain; pool reuse is the
natural follow-up.

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

**Shipped in this PR.** `perfdata.Writer.OnNewPID` fires the
first time a unique pid arrives in `AddSample`. Wired in
`agent.go` system-wide mode to emit COMM + MMAP2 just-in-time
for each sampled PID; the eager `/proc/*/maps` walk at writer
init is gone. Sentinel filter (pid != 0 && pid != 0xffffffff)
keeps kernel-only samples from triggering.

Discovered via bench-self iter 9: on a busy host (9000+ PIDs),
the eager walk burned ~30% of perf-agent CPU on kernel
`/proc/<pid>/maps` rendering (`show_map_vma`, `mangle_path`,
`lock_next_vma`). iter 11 (lazy) emitted records for only the
~200 actually-sampled PIDs; perf.data dropped to ~3 MB (vs
40-50 MB the eager walk would have written), and the per-PID
walk cost is now bounded by activity rather than host PID
count. Bonus: lazy mode also covers PIDs that exec AFTER capture
starts — the eager walk could never see those.

Remaining kernel `/proc/maps` cost in iter 11 (~15% cum) comes
from procmap.Resolver's per-batch re-snapshot (spec invariant —
see docs/specs/2026-05-12-debuginfod-cache-layout-design.md)
and dwarfagent's lazy attach (also bounded by sampled-PID
count). Both reduce when the workload has fewer active PIDs.

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
| 2  | 0.5d   | High     | **Shipped** in this PR — counters + p50/p99 batch histograms |
| 3  | 0.5d   | Med      | **Shipped** in this PR — `--metrics-listen` flag |
| 4  | 0.5d   | Med      | **Shipped** in this PR — `KernelLockdownEPERM` + `KernelOtherErr` |
| 5  | 5min   | High     | **Shipped** in this PR |
| 6  | 0.5d   | Med      | **Shipped** in this PR — `make bench-symbolize` |
| 7  | 1d     | Low      | **Partial** — symbolize hot-path budget gates shipped |
| 8  | 1d     | Med      | **Shipped** in this PR |
| 9  | 1d     | Med      | **Shipped** in this PR — `Writer.OnNewPID` |
| 10 | 0.5d   | Low      | **Shipped** in this PR (AddComm was entirely unwired, not just for kthreads) |

Every numbered item in this roadmap has now landed (with #7 as
a partial — symbolize hot path covered, sample-processing
follow-up still open). The PR closed the loop end-to-end:
profile → observe → fix → re-observe → ship.

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

| 5 | A/B split run: A = profile with --kernel-stacks (full); B = profile without --kernel-stacks (user-only). B revealed `strings.Fields` + `strconv.ParseUint` in the kallsyms parser were dominating user-side allocation (mallocgc, sweepone in top 10). | byte-level `parseKallsymsLine` (16.5 ns/op, 0 allocs) + module-name intern map | kallsyms parse user-side cost gone; kernel-side `module_get_kallsym` + `vsnprintf` remain — those are the kernel synthesizing the file, can't be optimized from userspace |
| 6 | A/B split rerun. user-side allocation pressure gone (no more sweepone / mallocgc in top). kernel-side kallsyms cost is the remaining floor on lockdown hosts: every perf-agent invocation re-parses /proc/kallsyms because blazesym hits EPERM on /proc/kcore. | (none in this PR — next step) | the floor: ~0.8s CPU per agent invocation on this host's ~3M-line kallsyms. Negligible for long captures, noticeable for ≤30s. |

### Remaining bottleneck after iter 6 (proposed follow-up #D-cache)

On hosts where blazesym's kernel source fails (lockdown=integrity,
Secure Boot, missing CAP_SYS_RAWIO), every perf-agent invocation
parses /proc/kallsyms from scratch. The user-side cost is gone
(allocation-free parser), but the kernel still formats the
synthesized file via vsnprintf on each read syscall — no userspace
optimization helps.

**Proposed fix**: disk-cached kallsyms index.

- Cache path: `${XDG_CACHE_HOME:-~/.cache}/perf-agent/kallsyms-${BOOT_ID}.cache`
- Format: addrs (uint64 array) + names (length-prefixed) + modules (intern table + indices)
- Invalidation: `/proc/sys/kernel/random/boot_id` (changes only on reboot)
- Read path: mmap or single read into prealloc'd slices — should drop kallsyms parse to milliseconds
- Write path: best-effort; failure is non-fatal (falls back to fresh parse)

Estimated win: ~0.8s CPU saved per agent invocation on lockdown
hosts. Significant for the short-capture / CI-bench / sidecar
patterns. ~150 LOC + tests. Out of scope for this PR.

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
