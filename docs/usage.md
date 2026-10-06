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

Python frames need no extra flag — a `--pid` capture walks the interpreter's
frame chain on CPython 3.12-3.14. Enrolment is per-PID today: a system-wide `-a`
capture does not walk Python, and says so
([#194](https://github.com/dpsoft/perf-agent/issues/194)).

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
| `--sample-rate` | CPU profile sample rate (Hz) | `99` |
| `--unwind` | Stack unwinding strategy: `fp` \| `dwarf` \| `auto` (auto routes to dwarf; the hybrid walker covers FP-safe code via the FP path) | `auto` |
| `--profile-output` | Output path for CPU profile | auto-named |
| `--offcpu-output` | Output path for off-CPU profile | auto-named |
| `--pmu-output` | Output path for PMU metrics (`auto` for auto-named) | stdout |
| `--flamegraph-output` | Also write a self-contained interactive HTML flame graph of the profile (`auto` for auto-named). Requires `--profile` or `--offcpu`. | - |
| `--perf-data-output` | Also emit a Linux kernel-format `perf.data` (consumable by `perf script`, FlameGraph, hotspot, AutoFDO `create_llvm_prof`, …). Requires `--profile`. | - |
| `--tag key=value` | Add tag to profile (repeatable) | - |
| `--debuginfod-url=URL` | Add a `debuginfod`-protocol server (repeatable). Falls back to `DEBUGINFOD_URLS` env. Unset → off. | - |
| `--symbol-cache-dir=DIR` | Local directory for fetched artifacts. | `/tmp/perf-agent-debuginfod` |
| `--symbol-cache-max=BYTES` | LRU cap for the symbol cache. | `2147483648` (2 GiB) |
| `--symbol-fetch-timeout=DUR` | Per-artifact HTTP fetch timeout. | `30s` |
| `--symbol-fail-closed` | (M2 stub) Refuse to symbolize a mapping whose fetch failed. | `false` |

Either `--pid` or `-a/--all` is required. At least one of `--profile`, `--offcpu`, or `--pmu` must be specified.

---
