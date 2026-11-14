# Usage

```bash
# CPU profiling — DWARF/hybrid walker is the default
./perf-agent --profile --pid <PID>

# Force frame-pointer-only walker (cheaper startup, may truncate on FP-less binaries)
./perf-agent --profile --unwind fp --pid <PID>

# Force DWARF walker (eager CFI compile + per-frame hybrid)
./perf-agent --profile --unwind dwarf --pid <PID>

# Off-CPU profiling
./perf-agent --offcpu --pid <PID>

# Combined on-CPU + off-CPU
./perf-agent --profile --offcpu --pid <PID>

# PMU only (hardware counters)
./perf-agent --pmu --pid <PID>

# CPU + off-CPU + GPU (the target must already have loaded the adapter; see GPU below)
./perf-agent --profile --offcpu --gpu --gpu-shim <adapter.so> --pid <PID>

# System-wide
./perf-agent --profile -a --duration 30s

# All features with metadata tags
./perf-agent --profile --offcpu --pmu --pid <PID> --duration 30s \
    --tag env=production \
    --tag version=1.2.3 \
    --tag service=api
```

Python frames need no extra flag — a capture walks the interpreter's frame
chain on CPython 3.12-3.14, with `--pid` or system-wide. A system-wide `-a`
capture enrols each Python process the first time it samples it, so that
process's first samples are native-only and the rest carry Python frames.
Processes that are never sampled are never stopped.

## GPU

CUDA kernels land under the code that queued them, joined on CUPTI's
correlation id. It takes two steps: `--gpu` writes its own profile alongside
`--profile`/`--offcpu`, and `cmd/flamegraph -fuse` joins them into one tree.

```bash
# The workload must already have loaded the adapter:
#   CUDA_INJECTION64_PATH=/path/to/libperfagent-gpu-nvidia.so python train.py
./perf-agent --profile --offcpu --gpu \
    --gpu-shim /path/to/libperfagent-gpu-nvidia.so --pid <PID>

# One profile per collector; fuse them
go run ./cmd/flamegraph -fuse -o fused.html \
    -in 'cpu.pb.gz;;[cpu] perf-agent 99 Hz' \
    -in 'gpu.pb.gz;;[gpu] per sampled launch'
```

Injection happens during `cuInit` and cannot be added to a live process, so
`CUDA_INJECTION64_PATH` must be set before the workload starts — Parca and
OpenTelemetry's profilers require the same. `--gpu-shim` must be the *same
file* the target mapped: the uprobe attaches by inode, so identical bytes at
another path capture nothing.

`PERFAGENT_GPU_SAMPLE_PERIOD` (default 8) controls how many launches carry a
CPU stack. Lowering it raises the event rate; see
[GPU injection](gpu-injection.md) for what that costs.

At shutdown, GPU diagnostics distinguish several kinds of incomplete data:

- `kernel_dropped` counts **wire records** lost in BPF transport, broken down
  by kind. `exec` records carry measured GPU durations; `launch` and
  `sampled_launch` records carry CPU attribution. The total is not a count of
  missing kernels. Unreadable BPF counter slots are reported explicitly.
- `sequence_gaps` counts **batches that never arrived**, whose record counts
  are unknown. Concurrent producer threads deliver batches out of order; a batch
  that arrives late, up to 1024 positions behind the newest one of its stream,
  is counted in `reordered` and not as a gap. `stale` counts duplicates and
  batches later than that window, and is the most `sequence_gaps` can overstate
  real loss by. Gaps can overlap the BPF losses and must not be added to them.
- `CPU attribution incomplete` describes missing, evicted or unresolved CPU
  stacks. GPU executions may still be present with measured durations and no
  caller attribution; this is not evidence of missing GPU time by itself.
- `sink_rejected` counts events rejected after transport. Timeline eviction
  has a separate message for executions, events and modules evicted before
  profile projection.
- `stop_unread_bytes` counts **ring bytes** that were still unread when the
  final drain timed out (5 s). The capture ends when the profiler stops: the
  GPU probes are detached at that moment, so these counters do not change
  afterwards. Records that BPF had already accepted are then drained into the
  profile. Anything the drain could not reach in time is reported here, and it
  is in no drop counter.

- `why stacks were missing` follows `stacks_missing` and says what the BPF side
  ran into. `stack_map_full` means the consumer was not freeing `gpu_stacks`
  entries fast enough; the other reasons are walk failures.

Symbolizing a sampled launch's CPU stack is the consumer's most expensive step.
A launch whose call path was symbolized in the last 2 s reuses that result
(`stack memo: hits=... misses=...` at shutdown). GPU workloads launch from a
few call paths millions of times, so the memo takes symbolization almost
entirely off the drain path. `--gpu-stack-memo N` sets how many call paths it
holds (default 4096); `--gpu-stack-memo -1` turns it off. If a library is
unloaded and another mapped at the same address, a stack can carry the old
names for up to 2 s. A stack with no frame resolved is not reused, so the next
launch from that call path is symbolized again.

`--gpu-ring-bytes` can absorb larger bursts, at the cost of memory, but cannot
make a continuously overloaded consumer keep up. Increasing the target's
`PERFAGENT_GPU_SAMPLE_PERIOD` reduces CPU stack sampling and attribution detail;
it does not intentionally sample GPU execution records. The setting must be
present when the target starts. Sampled launches use a compact BPF message, but
symbolization still runs in the consumer and high event rates can exceed its
throughput. Do not infer complete execution capture solely from a nonempty
profile or from a missing attribution warning.

### Aggregated GPU samples and the executions log

The GPU profile merges kernel executions that share a stack and labels into
one sample, summing their durations. Totals, flame graphs and the share of GPU
time per call path are the same as with one sample per execution. What changes
is memory: the agent keeps one sample per distinct stack and label set instead
of one per kernel. A fine-tuning capture of 243,387 kernels over 247 stacks
held 150 MB of samples per execution; aggregated, it holds about 3 MB. The
`GPU profile written` line reports both numbers, aggregated samples and
executions, plus the projected rows (more than executions when PC sampling
splits one execution across several program counters).

Labels that vary per program counter (`gpu_pc`, `gpu_stall`, `gpu_src_*`) stay
on the samples, so with PC sampling the number of samples grows with the
distinct PCs seen, up to a cap: at most 20,000 distinct `gpu_pc` values for the
whole capture. Past it, new program counters are written without `gpu_pc`.
Their time, `gpu_stall` and `gpu_src_*` source location are kept, and the
agent logs how many samples lost the label. `gpu_src_*` are bounded by the
device code's source lines. Without PC sampling the number of samples is
bounded by stacks and labels.

To keep the per-execution detail without paying memory for it, add
`--gpu-executions-output`:

```bash
./perf-agent --pid <PID> --profile --gpu --gpu-shim /path/libperfagent-gpu-nvidia.so \
    --gpu-output gpu.pb.gz --gpu-executions-output gpu.exec
go run ./cmd/gpuexec -profile gpu.pb.gz -executions gpu.exec        # summary, verified
go run ./cmd/gpuexec -profile gpu.pb.gz -executions gpu.exec -csv   # one row per sample
```

The file is written to disk as the capture runs, so it costs a few bytes per
execution on disk and nothing per execution in memory. Each row names the
aggregated sample it was summed into, the execution it came from (rows of one
execution share an ordinal), its CUPTI correlation id and its value. The file
is bound to the exact bytes of the GPU profile it was written with: `gpuexec`
rejects it against any other profile, and rejects a truncated or corrupt file.
It does not record per-execution timestamps.

`--gpu-per-execution-samples` restores one sample per execution, each labelled
`gpu_correlation`, for workflows that filter a profile to a single kernel
execution (`pprof -tagfocus gpu_correlation=cupti:<id>`). Memory then grows
with the number of kernels. It cannot be combined with
`--gpu-executions-output`, which describes the aggregated profile.

## GPU device telemetry

Collect hardware sensors without starting a profiler:

```bash
./perf-agent --gpu-telemetry --duration 30s
./perf-agent --gpu-telemetry-output gpu.jsonl --duration 30s \
    --gpu-telemetry-interval 1s --gpu-telemetry-devices GPU-<uuid>
```

Or collect sensors alongside a profile:

```bash
./perf-agent --profile --pid <PID> --duration 30s \
    --gpu-telemetry-output gpu.jsonl
```

Telemetry reads NVML directly. It needs the NVIDIA driver, device access and
`libnvidia-ml.so.1`, but no CUDA injection, application changes, PID or BPF
capabilities when used alone. Physical GPU UUIDs select devices; the default is
all visible physical GPUs. Even alongside a per-PID profile, sensors describe the
**whole device**, including other workloads. MIG instance metrics are not collected.

An initial reading, periodic readings and a final reading are followed by a
console summary. `--gpu-telemetry-output` also enables collection and writes one
JSON object per device per reading, plus one summary record per device. Each
record has `schema_version: 1`, `source: "nvml"`, `scope: "device"`, and a `kind`
of `initial`, `sample`, `final`, `summary` or `unavailable`. Device records include
UUID, driver version, memory API, actual read timestamp, read duration and values.
The configured interval is a polling interval, not the sensor's internal refresh
period. `missed_ticks` counts periodic polls skipped when reads or output are slow.

| Value key | Unit and meaning |
|---|---|
| `gpu.temperature_celsius` | GPU core temperature, °C |
| `gpu.power_watts` | Driver-reported power, W; may average an internal window |
| `gpu.power_limit_watts` | Configured power limit, W |
| `gpu.memory.{total,used,free,reserved}_bytes` | Raw memory-info v2 fields, bytes |
| `gpu.utilization_ratio` | Fraction of the driver's sample period with kernels executing, 0–1 |
| `gpu.memory_activity_ratio` | Fraction with device memory reads/writes, 0–1 |
| `gpu.sm_clock_hertz` | SM clock frequency, Hz |
| `gpu.memory_clock_hertz` | Memory clock frequency, Hz |
| `gpu.clock_event_active.<reason>` | Supported clock-event state, 0 or 1 |

Clock reasons are `idle`, `application_clocks`, `power_cap`, `hardware_slowdown`,
`sync_boost`, `software_thermal`, `hardware_thermal`, `hardware_power_brake`,
`display_clock`, `board_limit`, and `reliability`. Idle is not thermal throttling.
Utilization is not SM occupancy; memory activity is neither occupied capacity nor
bandwidth. Memory fields are preserved exactly as reported, without deriving
`used = total - free` or adjusting for reserved memory. Memory-info v1 is not
substituted when v2 is unavailable.

Unsupported fields are absent from `values` and listed once per device in
`unsupported`; transient read errors have a reason in `unavailable`.
A real zero stays present. The summary reports min/max/last and the arithmetic
mean of **successful samples**, plus counts of successful and failed readings.
It does not estimate energy or interpolate missing samples. Sampling and export
are serial with bounded memory. A driver call or local writer must return before
telemetry can finish; profiling stops independently at its duration boundary.
A source initialization or output failure is reported and produces a nonzero exit
status. In a combined capture, the profile still completes if telemetry fails.

`/metrics` remains internal agent diagnostics. These measurements go through the
metrics exporter interface; they are not re-exported through HTTP. There is no
vLLM dependency. The optional OTLP exporter is described below. With file capabilities (`setcap`), NVML must be
resolvable through standard loader paths or `ld.so.cache`; `LD_LIBRARY_PATH` is
ignored by secure execution. In containers, expose the NVIDIA devices and driver
libraries with NVIDIA Container Toolkit.

---

## GPU telemetry in the HTML profile

Add `--flamegraph-output auto` to a capture with GPU telemetry. Each generated
HTML contains a collapsible device summary and a **Compare GPUs** tab. Selecting
**Use in summary** in that tab returns to the flamegraph with the chosen physical
GPU. Zoom and the collapsed state are preserved while switching views.

For an existing CUDA process launched with the profiling adapter:

```bash
# Use the same adapter file as the target's CUDA_INJECTION64_PATH.
./perf-agent --pid <PID> --profile --gpu --gpu-shim /absolute/path/libperfagent-gpu-nvidia.so \
    --duration 30s --profile-output cpu.pb.gz --gpu-output gpu.pb.gz \
    --gpu-telemetry-output gpu.jsonl --flamegraph-output auto
```

The command prints the generated HTML paths (one per profile mode). It collects
NVML sensors and renders the views in the same run; no telemetry server or OTel
collector is needed for the HTML. `--gpu-telemetry` also works without a JSONL
output when only the automatic HTML is wanted. Profiling retains its existing
BPF/perf permission requirements; NVML-only collection does not require them.

Render the same capture offline, or combine its CPU and GPU profiles:

```bash
go run ./cmd/flamegraph -gpu-telemetry gpu.jsonl -o gpu.html gpu.pb.gz

go run ./cmd/flamegraph -fuse -gpu-telemetry gpu.jsonl -o combined.html \
    -in 'cpu.pb.gz;leaf-first;CPU' -in 'gpu.pb.gz;leaf-first;GPU'
```

### Inspecting and comparing profiles

`cmd/flamegraph` can also answer questions about one profile or compare two,
over the same profile model, without drawing the full graph. These flags read
leaf-first pprof only, never find an executions sidecar on their own, and exit
2 when combined with `-folded`, `-fuse` or `-stack-order root-first`.
`-sample-index` applies to all of them.

```bash
# The 20 widest nodes, as JSON lines {"id","path","cum"}
go run ./cmd/flamegraph -list-nodes 20 profile.pb.gz

# Facts for one node: its ID, or a root-first name path
go run ./cmd/flamegraph -facts 'main;work;inner' -execlog profile.executions profile.pb.gz

# Diff against a base profile: writes profile.pb.gz.diff.html and .diff.json
go run ./cmd/flamegraph -diff base.pb.gz -execlog-base base.executions \
    -execlog profile.executions profile.pb.gz
```

A name path that matches several nodes (the same name in different modules)
exits 1 and lists the candidate IDs on stderr; pass one of them instead.
`-diff` writes the page to `-o` (default `<profile>.diff.html`) and the diff
JSON next to it with `.json` replacing `.html`; it refuses an `-o` that
names an input profile or sidecar.

For continuous JSONL rotation, preserve the relevant `gpu.jsonl.2`,
`gpu.jsonl.1` and `gpu.jsonl` files. The offline command reads the supplied file
only; concatenate retained segments oldest first into a separate input when the
profile spans a rotation. Missing earlier segments can only produce a partial
report, with an explicit observation-range warning; they cannot be reconstructed
from a later summary.

The JSONL carries `capture_id` and `interval_ns`. All pprof files from the same
agent run carry `perf-agent.capture_id=<id>` in their comments, `TimeNanos` and
`DurationNanos`. Offline rendering requires the same identity and a valid window;
fused inputs must share both. Legacy/unrelated files are rejected rather than
paired by their names or approximate timestamps. Rendering without a telemetry
sidecar continues to accept ordinary pprof files. If a wall-clock change makes
the capture window invalid, profiles are still written with a warning, without
telemetry linkage; the automatic HTML omits the sensor panel.

The window describes the **capture lifecycle, including attachment/setup**; it
is not the exact interval with every perf event enabled. GPU collection stops
before CPU symbolization, and CPU/off-CPU can still drain briefly at the boundary.
The renderer includes only device readings whose timestamps lie inside that
window. It recalculates aggregates from raw readings and never substitutes a
continuous summary that covers a longer period. With no matching observations,
the page says so. In continuous mode the automatic report is frozen at profile
completion while JSONL/OTLP collection continues; the HTML is not live.

The summary shows temperature, power, used/total memory, GPU utilization, SM
clock and memory activity. Details include min/mean/max/last valid/count for each
metric, clocks and clock-event observations, read errors, unsupported reasons,
and the actual first/last reading times. Means use successful observations, not
time weighting. Reading counts do not claim uninterrupted coverage. Idle is
separate from thermal limits; a missing value is never converted to zero.
Sensors cover the whole physical GPU, including other processes, and do not
change with frame zoom. GiB and MHz are display conversions of the original
bytes and hertz; utilization/activity ratios display as percentages.

To verify a capture, check its matching capture IDs, UTC window, physical UUIDs
and first/last observation times in the HTML. Compare the per-field valid counts
and missing-value reasons with the JSONL. The in-memory report is bounded to 64
GPUs and 128 fields per GPU, and reports truncation; offline input is limited to
256 MiB and 1 MiB per JSONL record. No new series or capture ID labels are added
to OTLP, and the internal `/metrics` endpoint is unchanged.

---

## Continuous GPU telemetry and OTLP

Run telemetry until SIGINT/SIGTERM, with optional OTLP/HTTP protobuf delivery:

```bash
./perf-agent --continuous --otlp --otlp-endpoint http://localhost:4318

# Local rolling storage, with or without OTLP:
./perf-agent --continuous --gpu-telemetry-output gpu.jsonl \
    --gpu-telemetry-window 1m --gpu-telemetry-max-bytes 67108864

# One finite profile while device telemetry continues:
./perf-agent --continuous --otlp --profile --pid <PID> --duration 30s \
    --otlp-endpoint http://localhost:4318
```

`--continuous` requires GPU telemetry (`--gpu-telemetry`, a JSONL output, or
`--otlp`). With profiling, `--duration` controls that single profile: its outputs
are written and BPF/PMU resources closed at the boundary while telemetry continues.
Without profiling, explicitly passing `--duration` with `--continuous` is an error.
This does not schedule repeated profiles or add telemetry to the HTML view.

Continuous collection uses independent summary windows (default one minute).
Each window has initial/periodic/final samples and resets its min/max/mean/count.
NVML is reopened at each window boundary to rediscover GPUs and recover driver
handles. Failed initialization/discovery retries after 1, 2, 4, 8, 16, then 30 seconds;
identical console errors are suppressed until the state changes. JSONL preserves
unavailable records. Device read failures stay explicit and the next boundary
reopens the source. Native driver calls and local writes still must return; the
remote HTTP deadline cannot interrupt a blocked NVML call or filesystem write.

Continuous JSONL appends and rotates at the configured byte limit, keeping only
`gpu.jsonl`, `gpu.jsonl.1`, and `gpu.jsonl.2` (64 MiB each by default). Records are
never split. Oversized records or existing files/backups, non-regular paths, and
write/rotation errors are rejected. A local output error terminates continuous
collection and closes an active profile immediately. Use a separate output path
per process: the JSONL writer does not coordinate concurrent processes.

OTLP requires explicit `--otlp`; setting `OTEL_*` variables alone never enables
network export. It also enables NVML telemetry. The official Go HTTP/protobuf
exporter handles `OTEL_EXPORTER_OTLP_*` and signal-specific
`OTEL_EXPORTER_OTLP_METRICS_*` endpoints, headers, TLS, compression and timeout.
The metrics-specific setting wins. Other protocols are rejected when using environment-based endpoint configuration. An explicit
`--otlp-endpoint` selects HTTP/protobuf, overrides the endpoint environment, and warns if an incompatible protocol environment setting is ignored; a bare host URL gets
`/v1/metrics`, while an explicit path is preserved. Standard environment endpoint
path rules still apply: a general endpoint gets `/v1/metrics` appended; a
metrics-specific endpoint is already the complete URL.

Resources contain host/service/version metadata. `OTEL_SERVICE_NAME` and
`OTEL_RESOURCE_ATTRIBUTES` configure the resource. No PID or capture ID is added
by default. Sensor gauges use the original reading timestamp and physical UUID
attribute `hw.id`; summaries are never re-exported as sensor readings.

| OTLP gauge | Unit |
|---|---|
| `perf_agent.gpu.temperature` | `Cel` |
| `perf_agent.gpu.power`, `perf_agent.gpu.power.limit` | `W` |
| `perf_agent.gpu.memory.{total,used,free,reserved}` | `By` |
| `perf_agent.gpu.utilization`, `perf_agent.gpu.memory.activity` | `1` |
| `perf_agent.gpu.clock.sm`, `perf_agent.gpu.clock.memory` | `Hz` |
| `perf_agent.gpu.clock_event_active.<reason>` | `1` |
| `perf_agent.gpu.info` | `1`, with GPU name, driver version and memory API attributes |
| `perf_agent.gpu.telemetry.available` | `1`, whether any supported sensor is readable |
| `perf_agent.gpu.telemetry.read_errors` | `{reading}`, transient failures in the current poll |
| `perf_agent.gpu.telemetry.state.<state>` | `1`, one-hot state gauges |

States are `ok`, `unsupported`, `read_failed`, `source_unavailable`, and
`not_present`. Each state is sent as 0 or 1 to avoid leaving an old state active.
Available readings are exported even if some other sensors failed; inspect
`read_errors` for partial failures. Initialization failure emits availability=0
for known or explicitly selected UUIDs. Before any UUID can be discovered, no
GPU identity is invented: local unavailable diagnostics remain the evidence.
Absent sensor fields are not converted to zero. The custom metric namespace
preserves NVML meanings; memory activity is not occupied memory capacity.

One HTTP worker retains one in-flight batch and one pending snapshot per GPU,
with a lifetime limit of 128 device identities. A new reading replaces an older
pending reading for that device; unavailable data also replaces stale pending
values. This bounds memory when the receiver is slow. HTTP attempts, including
retries, have a three-second upper bound (a shorter configured timeout is honored).
After the final sample, the CLI allows five seconds total for remote shutdown.
There is no durable network spool: failed, replaced and dropped points are counted.
Statistics are printed every 30 seconds and at shutdown. A failed receiver does
not fail a local profile/JSONL result; if OTLP is the only requested output and
no sensor point was successfully delivered, the command exits nonzero.

By default, cooperative per-UUID `flock` ownership prevents duplicate writers
among participating processes using the same private lock directory and identity.
Locks survive summary windows and are released after the last request. A process
that encounters contention skips that GPU's remote export for its entire run;
local telemetry continues. There is no automatic takeover. Files are never
unlinked on release. The default directory is `$XDG_RUNTIME_DIR/perf-agent-otlp-<uid>` when set,
otherwise `$TMPDIR/perf-agent-otlp-<uid>` (`/tmp` if TMPDIR is unset); `--otlp-lock-dir` selects another private directory.
This is not coordination across users, systemd services with isolated `PrivateTmp`
fallback directories, isolated containers or unrelated agents. File capabilities
can cause secure execution to discard TMPDIR; use `--otlp-lock-dir` explicitly
when producers must share a directory.
Prefer one exporter per node; containers must share the directory to cooperate.
For intentional distinct writers, configure a distinct `--otlp-instance-id`
(or `service.instance.id` resource attribute); it changes both the lock identity
and remote metric identity, increasing cardinality. Never reuse an instance ID
for concurrent writers with separate lock directories.

# Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--profile` | Enable CPU profiling with stack traces | `false` |
| `--offcpu` | Enable off-CPU profiling with stack traces | `false` |
| `--pmu` | Enable PMU hardware counters | `false` |
| `--pid <PID>` | Target process ID | - |
| `-a, --all` | System-wide (all processes) | `false` |
| `--per-pid` | Per-PID breakdown (only with `-a --pmu`) | `false` |
| `--duration` | Collection duration | `10s` |
| `--gpu-stack-memo` | Symbolized GPU launch stacks to remember for 2 s; `-1` disables | `4096` |
| `--gpu-telemetry` | Collect NVIDIA device sensors, also usable alone | `false` |
| `--gpu-telemetry-output` | Write timestamped GPU readings and summary to JSONL; enables telemetry | - |
| `--gpu-telemetry-interval` | Sensor polling interval | `1s` |
| `--continuous` | GPU telemetry until signal; profiling keeps its finite duration | `false` |
| `--gpu-telemetry-window` | Continuous summary and NVML rediscovery window | `1m` |
| `--gpu-telemetry-max-bytes` | Continuous JSONL bytes per file; two backups retained | `67108864` |
| `--otlp` | Explicitly enable GPU OTLP/HTTP protobuf export | `false` |
| `--otlp-endpoint` | Metrics endpoint URL; overrides environment | environment/default |
| `--otlp-instance-id` | Intentional distinct producer identity | unset |
| `--otlp-lock-dir` | Shared private directory for cooperative writer locks | per-user temporary directory |
| `--gpu-telemetry-devices` | Comma-separated physical GPU UUIDs | all visible GPUs |
| `--sample-rate` | CPU profile sample rate (Hz) | `99` |
| `--unwind` | Stack unwinding strategy: `fp` \| `dwarf` \| `auto` (auto routes to dwarf; the hybrid walker covers FP-safe code via the FP path) | `auto` |
| `--profile-output` | Output path for CPU profile | auto-named |
| `--offcpu-output` | Output path for off-CPU profile | auto-named |
| `--pmu-output` | Output path for PMU metrics (`auto` for auto-named) | stdout |
| `--flamegraph-output` | Also write a self-contained interactive HTML flame graph of the profile (`auto` for auto-named). Requires `--profile`, `--offcpu` or `--gpu`. | - |
| `--perf-data-output` | Also emit a Linux kernel-format `perf.data` (consumable by `perf script`, FlameGraph, hotspot, AutoFDO `create_llvm_prof`, …). Requires `--profile`. | - |
| `--tag key=value` | Add tag to profile (repeatable) | - |
| `--debuginfod-url=URL` | Add a `debuginfod`-protocol server (repeatable); turns network symbolization on. | - |
| `--debuginfod` | Fetch from the servers in `DEBUGINFOD_URLS`, which is otherwise ignored. | `false` |
| `--no-debuginfod` | Never fetch debug info over the network; an error alongside the two above. | `false` |
| `--debuginfod-prefetch` | Download debuginfo for `--pid`/`-a` into the symbol cache with no time budget, then exit; for files too large for a capture to fetch. | `false` |
| `--symbol-cache-dir=DIR` | Local directory for fetched artifacts. | `/tmp/perf-agent-debuginfod` |
| `--symbol-cache-max=BYTES` | LRU cap for the symbol cache. | `2147483648` (2 GiB) |
| `--symbol-fetch-timeout=DUR` | Per-artifact HTTP fetch timeout, transfer included. | `5s` |
| `--symbol-fail-closed` | (M2 stub) Refuse to symbolize a mapping whose fetch failed. | `false` |

Profiling requires `--pid` or `-a/--all` and at least one profiling mode (`--profile`, `--offcpu`, `--gpu`, `--pmu`). GPU telemetry alone requires neither. Finite captures require a positive duration; polling intervals must always be positive. See continuous mode above.

---
