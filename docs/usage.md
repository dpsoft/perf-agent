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
vLLM dependency or OTLP exporter. With file capabilities (`setcap`), NVML must be
resolvable through standard loader paths or `ld.so.cache`; `LD_LIBRARY_PATH` is
ignored by secure execution. In containers, expose the NVIDIA devices and driver
libraries with NVIDIA Container Toolkit.

---

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
| `--gpu-telemetry` | Collect NVIDIA device sensors, also usable alone | `false` |
| `--gpu-telemetry-output` | Write timestamped GPU readings and summary to JSONL; enables telemetry | - |
| `--gpu-telemetry-interval` | Sensor polling interval | `1s` |
| `--gpu-telemetry-devices` | Comma-separated physical GPU UUIDs | all visible GPUs |
| `--sample-rate` | CPU profile sample rate (Hz) | `99` |
| `--unwind` | Stack unwinding strategy: `fp` \| `dwarf` \| `auto` (auto routes to dwarf; the hybrid walker covers FP-safe code via the FP path) | `auto` |
| `--profile-output` | Output path for CPU profile | auto-named |
| `--offcpu-output` | Output path for off-CPU profile | auto-named |
| `--pmu-output` | Output path for PMU metrics (`auto` for auto-named) | stdout |
| `--flamegraph-output` | Also write a self-contained interactive HTML flame graph of the profile (`auto` for auto-named). Requires `--profile` or `--offcpu`. | - |
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

Profiling requires `--pid` or `-a/--all` and at least one profiling mode (`--profile`, `--offcpu`, `--gpu`, `--pmu`). GPU telemetry alone requires neither; duration and polling interval must be positive.

---
