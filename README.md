# perf-agent

*eBPF-based Linux profiler — CPU, off-CPU, and PMU, system-wide or per-PID, pprof output.*

[![CI](https://github.com/dpsoft/perf-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/dpsoft/perf-agent/actions/workflows/ci.yml)
[![Tests](https://github.com/dpsoft/perf-agent/actions/workflows/tests.yml/badge.svg)](https://github.com/dpsoft/perf-agent/actions/workflows/tests.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dpsoft/perf-agent.svg)](https://pkg.go.dev/github.com/dpsoft/perf-agent)
[![Go Version](https://img.shields.io/github/go-mod/go-version/dpsoft/perf-agent)](go.mod)
[![License](https://img.shields.io/github/license/dpsoft/perf-agent)](LICENSE)

A single binary that samples on-CPU stack traces, off-CPU blocking time, and hardware PMU counters — and emits production-ready pprof. Hybrid FP+DWARF unwinder handles release-built C++/Rust binaries that omit frame pointers; built-in symbolization for native code (DWARF + ELF), Python (`-X perf` perf-maps), Node.js (`--perf-basic-prof`), and Go.

Runs entirely local. No backend, no telemetry, no scrape config.

> 🚧 **GPU profiling support is in active development** as an experimental track ([design spec](docs/superpowers/specs/2026-04-25-gpu-profiling-design.md)). CPU, off-CPU, and PMU profiling are stable today.

---

## Contents

- [Quickstart](#quickstart)
- [What you can do with perf-agent](#what-you-can-do-with-perf-agent)
- [Requirements](#requirements)
- [Usage](#usage)
- [Flags](#flags)
- [Output](#output)
- [Library usage](#library-usage)
- [Architecture](#architecture)
- [Building](#building)
- [Testing](#testing)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

---

## Quickstart

```bash
# Build (one-time, see BUILDING.md for full toolchain setup)
make build

# Grant capabilities once so subsequent runs don't need sudo
sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent

# Capture a 30-second CPU profile of one process — output is pprof
./perf-agent --profile --pid <PID> --duration 30s

# Inspect
go tool pprof <output>.pb.gz
```

---

## What you can do with perf-agent

Real workflows perf-agent is built for. Each maps to one or more of the modes documented under [Flags](#flags).

### 🔥 On-demand production profiling

Hot-attach to a running pod or process — no restart. For Python 3.12+, `--inject-python` activates the perf trampoline at profile start and deactivates it at exit, so the per-call overhead does not persist past the profiling window. Drop in as a sidecar (`shareProcessNamespace: true`), capture for 30s, exit. Output is pprof; ship it home with `kubectl cp` or pipe it into your store.

### 💤 Off-CPU stalls and blocking analysis

Find why a service is "slow but not CPU-busy." `--offcpu` hooks `sched_switch` and accumulates blocking time per call site — lock waits, syscall blocks, channel reads, mutex contention. Output is pprof, viewable in `go tool pprof` or any flame-graph tool that consumes it.

### 🐍 Cross-language flame graphs

One profile, multiple runtimes. Native (DWARF + ELF) symbolizes alongside Python (`-X perf` perf-maps, optionally activated on demand), Node.js (`--perf-basic-prof`), and Go — all inlined frames expanded by blazesym. The hybrid FP+DWARF unwinder handles release-built C++/Rust without `-fno-omit-frame-pointer`.

### 📊 Hardware-counter performance investigations

`--pmu` summarizes IPC, cache miss rate, runqueue latency (P50/P99), and context-switch reasons (preempted vs voluntary vs I/O wait) without the `perf stat` parsing tax. Combine with `--per-pid` in system-wide mode to see which processes dominate the node's wait time.

### 🧪 Differential profiling and sample-based PGO

High-fidelity pprof is the foundation. Each `Mapping` carries the absolute file path, GNU build-id, and file offsets; each `Location` is keyed by `(mapping_id, file_offset)` rather than symbol name, so two PCs that symbolize to the same `(file, line, func)` stay distinguishable. Feeds `go tool pprof -diff_base`, LLVM SamplePGO converters, and any cross-run analysis that depends on stable address-level identity.

### 🐳 Sidecar profiling inside Kubernetes pods

`--pid <N>` is namespace-aware: a PID visible from inside a pod (with `shareProcessNamespace: true`) is translated to the host kernel PID via `/proc/<N>/status`'s `NSpid` line. Output samples carry k8s identity labels (`pod_uid`, `container_id`, `cgroup_path`) parsed from the cgroup, plus best-effort `pod_name` / `namespace` / `container_name` from the downward API. **No kubelet API calls, no client-go dependency.**

---

## Requirements

- Linux kernel 5.8+ (BTF + CO-RE).
- Root, OR `setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent`.

---

## Usage

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

# System-wide
./perf-agent --profile -a --duration 30s

# All features with metadata tags
./perf-agent --profile --offcpu --pmu --pid <PID> --duration 30s \
    --tag env=production \
    --tag version=1.2.3 \
    --tag service=api
```

### Profiling running Python processes

For Python 3.12+ processes, perf-agent can activate the perf trampoline at profile start without restarting the target — no need for `python -X perf`:

```bash
sudo perf-agent --profile --pid $(pgrep -f myapp.py) \
                --duration 30s --inject-python
```

The trampoline emits Python qualnames to `/tmp/perf-<PID>.map`, which perf-agent reads via blazesym to attach human-readable names to JIT'd frames. perf-agent automatically deactivates the trampoline at end of profile, so the per-call overhead does not persist past the profiling window.

For system-wide injection (`-a`), perf-agent activates every detected Python 3.12+ process and tolerates per-process failures (e.g., processes built without `--enable-perf-trampoline`):

```bash
sudo perf-agent --profile -a --duration 30s --inject-python
```

Requires `CAP_SYS_PTRACE` (already in the standard cap set). See [docs/python-profiling.md](docs/python-profiling.md) for details.

### Profiling inside a Kubernetes pod

When perf-agent runs as a sidecar with `shareProcessNamespace: true`, `--pid <N>` is namespace-aware: the user-visible PID is translated to the host kernel PID via `/proc/<N>/status`'s `NSpid` line. Output pprof samples carry k8s identity labels (`pod_uid`, `container_id`, `cgroup_path`) parsed from the target's cgroup, plus best-effort `pod_name` / `namespace` / `container_name` from the downward API env vars. No kubelet API calls, no client-go dependency.

---

## Flags

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
| `--inject-python` | Activate Python 3.12+ perf trampoline on the target before profiling | `false` |
| `--tag key=value` | Add tag to profile (repeatable) | - |

Either `--pid` or `-a/--all` is required. At least one of `--profile`, `--offcpu`, or `--pmu` must be specified.

---

## Output

### Output file naming

Output files are auto-named by process name + timestamp + profile type:

| Mode | Per-PID example | System-wide example |
|------|----------------|---------------------|
| `--profile` | `myapp-202604021430-on-cpu.pb.gz` | `202604021430-on-cpu.pb.gz` |
| `--offcpu` | `myapp-202604021430-off-cpu.pb.gz` | `202604021430-off-cpu.pb.gz` |
| `--pmu-output auto` | `myapp-202604021430-pmu.txt` | `202604021430-pmu.txt` |

Process name comes from `/proc/<pid>/comm`. Override with `--profile-output` / `--offcpu-output`.

### pprof fidelity

CPU and off-CPU profiles are pprof. Each `Mapping` carries:

- `File` — absolute binary path (`/usr/bin/myapp`, `/lib/x86_64-linux-gnu/libc.so.6`).
- `BuildID` — ELF GNU build-id (hex).
- `Start`, `Limit`, `Offset` — VA range and file offset for the mapping.
- `HasFunctions` / `HasFilenames` / `HasLineNumbers` — flags indicating what symbolization could resolve.

Each `Location` carries:

- `Address` — file-relative offset (`Address - MapStart + MapOff`), portable across runs.
- One `Line` per inlined frame (blazesym expands inline chains).
- pprof labels (`pod_uid`, `container_id`, etc.) when running in a k8s pod.

Sentinel mappings handle the special cases: `[kernel]` for kernel frames (one shared mapping across all PIDs in a profile) and `[jit]` for Python/Node JIT frames where address has no file-offset meaning.

Tags (`--tag key=value`) are stored as profile-level comments.

```bash
go tool pprof myapp-202604021430-on-cpu.pb.gz
```

### PMU output

On-CPU time, runqueue latency, context-switch reasons, hardware counters (cycles, instructions, cache misses), and derived metrics (IPC, cache miss rate).

Example:
```
=== PMU Metrics (PID: 84228) ===
Samples: 26358

On-CPU Time (time slice per context switch):
  Min:    0.003 ms
  P50:    0.071 ms
  P99:    9.183 ms

Runqueue Latency (time waiting for CPU):
  Min:    0.001 ms
  P50:    0.012 ms
  P99:    0.850 ms

Context Switch Reasons:
  Preempted (running):     45.2%  (11912 times)
  Voluntary (sleep/mutex): 42.1%  (11095 times)
  I/O Wait (D state):      12.7%  (3351 times)

Hardware Counters:
  IPC (Instr/Cycle):  2.342
  Cache Misses/1K:    0.022
```

---

## Library usage

`perf-agent` is also a Go library via the `perfagent` package:

```go
package main

import (
    "context"
    "log"
    "time"
    "github.com/dpsoft/perf-agent/perfagent"
)

func main() {
    agent, err := perfagent.New(
        perfagent.WithPID(12345),
        perfagent.WithCPUProfile("profile.pb.gz"),
        perfagent.WithPMU(),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer agent.Close()

    ctx := context.Background()
    agent.Start(ctx)
    time.Sleep(10 * time.Second)
    agent.Stop(ctx)
}
```

### In-memory collection

```go
var buf bytes.Buffer
agent, _ := perfagent.New(
    perfagent.WithCPUProfileWriter(&buf), // gzip-compressed pprof
)
// After Stop(), buf contains ready-to-use .pb.gz data
```

### Custom labels and metrics export

```go
agent, _ := perfagent.New(
    perfagent.WithPID(12345),
    perfagent.WithLabels(map[string]string{
        "service": "api",
        "version": "1.2.3",
    }),
    // override the default k8s label enricher
    perfagent.WithLabelEnricher(myEnricher),
    perfagent.WithMetricsExporter(&MyExporter{}),
)
```

See the [`perfagent` package documentation](perfagent/) for all available options.

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│                            USER SPACE (Go)                               │
│                                                                          │
│                            ┌──────────┐                                  │
│                            │ main.go  │                                  │
│                            └────┬─────┘                                  │
│                                 ▼                                        │
│                       ┌──────────────────┐                               │
│                       │ perfagent.Agent  │  lifecycle + --unwind dispatch│
│                       └─────┬────────────┘                               │
│       ┌─────────────────────┼─────────────────────────┐                  │
│       ▼                     ▼                         ▼                  │
│ ┌───────────────┐  ┌──────────────────────┐  ┌──────────────┐            │
│ │  CPU Profiler │  │  DWARF CPU/Off-CPU   │  │ PMU Monitor  │            │
│ │   (FP path)   │  │      Profiler        │  │              │            │
│ │   profile/    │  │  unwind/dwarfagent/  │  │   cpu/       │            │
│ │   offcpu/     │  │   (hybrid walker)    │  │              │            │
│ └───────┬───────┘  └──────────┬───────────┘  └──────┬───────┘            │
│         │                     │                     │                    │
│         │     ┌───────────────┴───────────────┐     │                    │
│         │     ▼                               ▼     │                    │
│         │   ┌─────────────────┐    ┌──────────────────────┐              │
│         │   │ unwind/ehcompile│    │  unwind/ehmaps       │              │
│         │   │ .eh_frame → CFI │    │  per-PID map lifecyle│              │
│         │   └─────────────────┘    │  + MMAP2 watcher     │              │
│         │                          └──────────┬───────────┘              │
│         │                                     │                          │
│         ▼                                     ▼                          │
│   ┌──────────────────────────────────────────────────────────────┐       │
│   │              unwind/procmap (Resolver)                       │       │
│   │   /proc/<pid>/maps + .note.gnu.build-id, lazy per-PID cache  │       │
│   └────────────────────┬─────────────────────────────────────────┘       │
│                        ▼                                                 │
│   ┌──────────────────────────────────────────────────────────────┐       │
│   │            pprof/ ProfileBuilder                             │       │
│   │  address-keyed Locations + per-binary Mapping (build-id,     │       │
│   │  file offsets) + kernel/[jit] sentinels + name-based         │       │
│   │  fallback when resolver misses                               │       │
│   └──────────────────────────────────────────────────────────────┘       │
│                                                                          │
│   Symbolization: blazesym (DWARF + ELF + perf-maps for JIT runtimes)     │
└─────────────┬──────────────────┬──────────────────┬──────────────────────┘
              │                  │                  │
══════════════╪══════════════════╪══════════════════╪═══════════════════════
              │  eBPF load       │                  │
              ▼                  ▼                  ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                          KERNEL SPACE (eBPF)                             │
│                                                                          │
│  ┌──────────────┐  ┌────────────────┐  ┌────────────────┐  ┌──────────┐  │
│  │ perf.bpf.c   │  │ perf_dwarf.bpf │  │ offcpu.bpf.c   │  │ cpu.bpf.c│  │
│  │ (FP only)    │  │ (hybrid: FP    │  │ + offcpu_dwarf │  │ HW ctrs  │  │
│  │ stackmap     │  │  fast path,    │  │ sched_switch   │  │ rq lat   │  │
│  │ aggregated   │  │  DWARF for     │  │ blocking-ns    │  │ ctx swch │  │
│  │ counts       │  │  FP-less PCs)  │  │                │  │          │  │
│  └──────┬───────┘  └────────┬───────┘  └────────┬───────┘  └────┬─────┘  │
│         │                   │                   │               │        │
│         │             CFI tables, classification, pid_mappings  │        │
│         │             via HASH_OF_MAPS keyed by build-id        │        │
│         │                   │                                   │        │
│         └────────┬──────────┴──────────────┬────────────────────┘        │
│                  ▼                         ▼                             │
│           ┌─────────────┐          ┌─────────────────┐                   │
│           │ stack ringbuf│         │ aggregated maps │                   │
│           │ (DWARF path) │         │ (FP path)       │                   │
│           └─────────────┘          └─────────────────┘                   │
└──────────────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
                    ┌──────────────────────────────────────┐
                    │              OUTPUT                  │
                    │                                      │
                    │  *-on-cpu.pb.gz   *-off-cpu.pb.gz    │
                    │  PMU: console / file                 │
                    └──────────────────────────────────────┘
```

Two stack-walker paths share a single user-space pipeline:

- **FP path** (`--unwind fp`): cheap, kernel-side stackmap aggregation. Truncates on FP-less code (release C++/Rust without `-fno-omit-frame-pointer`).
- **DWARF/hybrid path** (`--unwind dwarf` or `auto`, the default): pure-FP for FP-safe code, falls through to `.eh_frame`-derived CFI rules for FP-less PCs. Userspace pre-compiles per-binary CFI from `.eh_frame` (`unwind/ehcompile`) and installs it into BPF maps (`unwind/ehmaps`); the BPF walker reads CFI per-frame. MMAP2 events keep CFI fresh as processes `dlopen`/`exec`. Eager-compile failures (Go binaries lack `.eh_frame`) are tolerated — the walker's FP path covers those.

The `procmap.Resolver` sits between the walkers and pprof. It lazily reads `/proc/<pid>/maps` and ELF `.note.gnu.build-id`, caches per-PID, and gives the pprof builder real `Mapping` identity (path, start/limit, file offset, build-id). Each `Location` is keyed by `(mapping_id, file_offset)` rather than by symbol name, so two PCs that symbolize to the same `(file, line, func)` stay distinguishable — the data downstream tools need for sample-based PGO and cross-run diffing.

## Requirements

- Linux kernel 5.8+ (BTF + CO-RE).
- Root, OR `setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent`.

## Usage

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

# System-wide
./perf-agent --profile -a --duration 30s

# All features with metadata tags
./perf-agent --profile --offcpu --pmu --pid <PID> --duration 30s \
    --tag env=production \
    --tag version=1.2.3 \
    --tag service=api
```

## Flags

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
| `--tag key=value` | Add tag to profile (repeatable) | - |

Either `--pid` or `-a/--all` is required. At least one of `--profile`, `--offcpu`, or `--pmu` must be specified.

### Profiling running Python processes

For Python 3.12+ processes, perf-agent can activate the perf trampoline at
profile start without restarting the target — no need for `python -X perf`:

```bash
sudo perf-agent --profile --pid $(pgrep -f myapp.py) \
                --duration 30s --inject-python
```

The trampoline emits Python qualnames to `/tmp/perf-<PID>.map`, which
perf-agent reads via blazesym to attach human-readable names to JIT'd
frames. perf-agent automatically deactivates the trampoline at end of
profile, so the per-call overhead does not persist past the profiling
window.

For system-wide injection (`-a`), perf-agent activates every detected
Python 3.12+ process and tolerates per-process failures (e.g., processes
built without `--enable-perf-trampoline`):

```bash
sudo perf-agent --profile -a --duration 30s --inject-python
```

Requires `CAP_SYS_PTRACE` (already in the standard cap set).
See [docs/python-profiling.md](docs/python-profiling.md) for details.

## Output

### Output File Naming

Output files are auto-named by process name + timestamp + profile type:

| Mode | Per-PID example | System-wide example |
|------|----------------|---------------------|
| `--profile` | `myapp-202604021430-on-cpu.pb.gz` | `202604021430-on-cpu.pb.gz` |
| `--offcpu` | `myapp-202604021430-off-cpu.pb.gz` | `202604021430-off-cpu.pb.gz` |
| `--pmu-output auto` | `myapp-202604021430-pmu.txt` | `202604021430-pmu.txt` |

Process name comes from `/proc/<pid>/comm`. Override with `--profile-output` / `--offcpu-output`.

### pprof fidelity

CPU and off-CPU profiles are pprof. Each `Mapping` carries:

- `File` — absolute binary path (`/usr/bin/myapp`, `/lib/x86_64-linux-gnu/libc.so.6`).
- `BuildID` — ELF GNU build-id (hex).
- `Start`, `Limit`, `Offset` — VA range and file offset for the mapping.
- `HasFunctions` / `HasFilenames` / `HasLineNumbers` — flags indicating what symbolization could resolve.

Each `Location` carries:

- `Address` — file-relative offset (`Address - MapStart + MapOff`), portable across runs.
- One `Line` per inlined frame (blazesym expands inline chains).

Sentinel mappings handle the special cases: `[kernel]` for kernel frames (one shared mapping across all PIDs in a profile) and `[jit]` for Python/Node JIT frames where address has no file-offset meaning.

Tags (`--tag key=value`) are stored as profile-level comments.

```bash
go tool pprof myapp-202604021430-on-cpu.pb.gz
```

### Experimental GPU replay pipeline

There is an experimental contract-validation path for the planned GPU profiling architecture. It does **not** talk to a real vendor runtime yet. Instead, it replays normalized GPU events from a JSON fixture, exports the normalized snapshot as JSON, and projects a mixed CPU+GPU profile using synthetic GPU frames.

Replay fixtures are versioned envelopes:

```json
{
  "version": 1,
  "events": [
    { "kind": "launch", "...": "..." }
  ]
}
```

Replay clock-domain contract:

- `clock_domain` is an optional field on normalized replay events
- omitted `clock_domain` defaults to `cpu-monotonic`
- replay currently accepts only `cpu-monotonic`; non-CPU domains are rejected early
- all timestamp fields in replay fixtures are therefore comparable directly to CPU launch timestamps today

```bash
go run . \
  --gpu-replay-input gpu/testdata/replay/flash_attn.json \
  --gpu-raw-output /tmp/gpu-raw.json \
  --gpu-profile-output /tmp/gpu.pb.gz \
  --duration 1ms

go tool pprof /tmp/gpu.pb.gz
```

This path is intended to validate:

- the vendor-agnostic GPU event contract
- timeline correlation and raw JSON export
- `pprof` projection with synthetic GPU frames

It is not yet a real NVIDIA / Intel / AMD backend.

### Experimental live GPU stream pipeline

There is also an experimental live ingestion path for normalized GPU NDJSON events. It keeps the same vendor-agnostic event contract as replay mode, but reads one event per line from stdin and drives the existing JSON snapshot plus synthetic-frame `pprof` projection.

Stream contract:

- one UTF-8 JSON object per line
- `kind` must be one of `launch`, `exec`, `counter`, `sample`, `event`
- `clock_domain` is optional and defaults to `cpu-monotonic`
- timestamps (`time_ns`, `start_ns`, `end_ns`, `duration_ns`) are in the CPU monotonic clock domain
- stream ingestion currently accepts only `cpu-monotonic`
- external collectors must convert device-local/GPU clocks before emitting into this pipe

```bash
cat <<'EOF' | go run . \
  --gpu-stream-stdin \
  --gpu-raw-output /tmp/gpu-live.json \
  --gpu-profile-output /tmp/gpu-live.pb.gz \
  --duration 1ms
{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":100}
{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}
{"kind":"sample","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":150,"stall_reason":"memory_throttle","weight":7}
EOF

go tool pprof /tmp/gpu-live.pb.gz
```

This is still a bridge layer, not a vendor runtime integration. It is meant to validate:

- live event ingestion
- NDJSON decode and validation
- reuse of the existing GPU manager, JSON export, and `pprof` projection

### Experimental host replay plus GPU stream pipeline

There is also an experimental host-correlation validation path. It replays CPU-side launch attribution from a fixture, combines that with a live GPU NDJSON execution stream, and validates that the final JSON snapshot and synthetic-frame `pprof` output include CPU launch frames joined to GPU execution.

```bash
cat <<'EOF' | go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/flash_attn_launches.json \
  --gpu-stream-stdin \
  --gpu-raw-output /tmp/gpu-host-raw.json \
  --gpu-profile-output /tmp/gpu-host.pb.gz \
  --duration 1ms
{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}
{"kind":"sample","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":150,"stall_reason":"memory_throttle","weight":7}
EOF

go tool pprof /tmp/gpu-host.pb.gz
```

This path is still not a real `uprobes` collector or vendor callback backend. It is intended to validate:

- canonical host launch normalization
- host launch to GPU execution correlation
- reuse of the existing mixed CPU+GPU `pprof` projection

### Offline and live helper script

The current branch also includes a small checked-in helper for the MVP workflows:

```bash
scripts/gpu-offline-demo.sh [--dry-run] <mode> <outdir>
```

Current modes are:

- `host-exec`
- `hip-amd-sample`
- `hip-amd-sample-rich`
- `hip-rocprofv2-rich`
- `hip-rocprofv2-command-rich`
- `hip-rocprofv3-command-rich`
- `host-driver`
- `multi-exec`
- `multi-driver`
- `live-hip-amdsample`
- `live-hip-linuxdrm`
- `live-hip-linuxkfd`

For example, the checked-in host-to-execution path can now be run as:

```bash
bash scripts/gpu-offline-demo.sh host-exec /tmp/gpu-demo
```

And the checked-in HIP host + AMD execution/sample path can be run as:

```bash
bash scripts/gpu-offline-demo.sh hip-amd-sample /tmp/gpu-amd-demo
```

And the current live entrypoint can be previewed safely with:

```bash
bash scripts/gpu-offline-demo.sh --dry-run live-hip-linuxdrm /tmp/gpu-live \
  --pid 4242 \
  --hip-library /opt/rocm/lib/libamdhip64.so
```

For the AMD compute-side KFD path instead of the DRM path:

```bash
bash scripts/gpu-offline-demo.sh --dry-run live-hip-linuxkfd /tmp/gpu-live \
  --pid 4242 \
  --hip-library /opt/rocm/lib/libamdhip64.so
```

For a future real AMD execution/sample producer instead of lifecycle-only DRM/KFD events:

```bash
bash scripts/gpu-offline-demo.sh --dry-run live-hip-amdsample /tmp/gpu-live \
  --pid 4242 \
  --hip-library /opt/rocm/lib/libamdhip64.so
```

The live path also accepts:

- `--join-window <dur>` to tune HIP launch -> `linuxdrm` fallback joins
- `PERF_AGENT_HIP_LIBRARY=/path/to/libamdhip64.so` instead of repeating `--hip-library`

That prints the exact `go run . ...` command it would execute, including:

- raw snapshot output
- standalone attribution JSON
- folded flamegraph input
- synthetic-frame `pprof`

If `--hip-library` is omitted, the helper will first honor `PERF_AGENT_HIP_LIBRARY`, then try a small set of common local ROCm library paths.

There is also a dedicated wrapper for the live AMD path that avoids long `sudo /usr/bin/env ...` commands entirely:

```bash
bash scripts/gpu-live-hip-linuxdrm.sh --outdir /tmp/gpu-live --pid 4242
```

For the KFD-only AMD compute path:

```bash
bash scripts/gpu-live-hip-linuxkfd.sh --outdir /tmp/gpu-live --pid 4242
```

For an external AMD execution/sample producer that writes NDJSON on stdout:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242
```

The `rocprofiler-sdk` real source currently defaults to `external` mode, meaning the collector consumes an external producer through command/path/output-file contracts. The `native` mode is now a real in-process seam: it validates the library path, rejects external producer knobs, and probes the shared library before stopping at the not-yet-implemented capture path.

When exercising that native seam today, use `--rocprofiler-sdk-mode native --rocprofiler-sdk-library /path/to/librocprofiler-sdk.so`. The branch validates the native contract, rejects mixing it with the external command/path/output knobs, and fails clearly if the shared library cannot be loaded. On this host shape, `rocm-runtime` alone does not provide `librocprofiler-sdk.so`; the native seam needs an actual ROCprofiler-SDK install.

If you are building ROCprofiler-SDK from source instead of installing it under `/opt/rocm`, point the native seam at the build artifact directly:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofiler-sdk \
  --rocprofiler-sdk-mode native \
  --rocprofiler-sdk-library ~/github/rocm-systems/rocprofiler-sdk-build/lib/librocprofiler-sdk.so
```

If `--sample-command` is omitted, the wrapper now defaults to that checked-in
adapter script automatically. The adapter can then:
- exec `--sample-collector-path` / `PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH` directly
- run `PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND`
- otherwise prefer the checked-in Go collector binary via `go run ./cmd/amd-sample-collector`
- and only fall back to the shell synthetic producer as a last resort

There is now a checked-in collector executable you can build and pass through
`--sample-collector-path`:

```bash
go build -o /tmp/amd-sample-collector ./cmd/amd-sample-collector

bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-collector-path /tmp/amd-sample-collector
```

The collector path now has an explicit mode boundary too:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode synthetic
```

`synthetic` is still the default. `real` is now an explicit opt-in whose
preferred source is `rocprofiler-sdk`, while `rocm-smi` remains available as a
coarse hardware-backed fallback using the same collector contract and output
shape. It is still not true GPU PC sampling yet, but it no longer silently
pretends that `real` mode exists without any live signal behind it.

If the real collector needs an explicit `rocm-smi` location, the wrapper now
passes that through too. `rocm-smi` is now explicit fallback behavior:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocm-smi \
  --rocm-smi-path /opt/rocm/bin/rocm-smi
```

The preferred modern path is now `rocprofiler-sdk`, with `rocprofv3` kept as
the richer CLI-shaped compatibility surface and `rocprofv2` retained only as
an older compatibility source. These hooks expect a collector-style executable
behind the selected path and adapt simple native JSON records such as
`dispatch` and `sample` into the same `amdsample` contract, including
alternate timing aliases and nested source-location metadata:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofiler-sdk \
  --rocprofiler-sdk-command 'rocprofiler-sdk --hip-trace --output /tmp/rocprofiler-sdk-out'
```

CLI-shaped compatibility path with `rocprofv3`:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofv3 \
  --rocprofv3-command 'rocprofv3 --hip-trace --output /tmp/rocprofv3-out'
```

Compatibility path with `rocprofv2`:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofv2 \
  --rocprofv2-path /opt/rocm/bin/rocprofv2
```

If the profiler needs a full command line instead of a bare executable path, pass that directly:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofv2 \
  --rocprofv2-command 'rocprofv2 --hip-trace --output /tmp/rocprofv2-out'
```

If that source writes native records to a file instead of stdout, pass the output path too:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofv2 \
  --rocprofv2-path /opt/rocm/bin/rocprofv2 \
  --rocprofv2-output-path /tmp/rocprofv2.jsonl
```

If it writes multiple traces into a directory, pass the directory and the collector will pick the newest file after the profiler exits:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-source rocprofv2 \
  --rocprofv2-path /opt/rocm/bin/rocprofv2 \
  --rocprofv2-output-dir /tmp/rocprofv2-out
```

The real collector poll interval is also tunable when you want denser or
sparser coarse hardware samples:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-mode real \
  --real-poll-interval 25ms
```

If the live target kernel name is known, pass it explicitly so the producer /
collector contract does not stay tied to the local shim default:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --kernel-name flash_attn_fwd
```

For synthetic or adapted producers that also need explicit queue / device
identity, the wrapper now exposes those too:

```bash
bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --device-id gfx942:0 \
  --device-name MI300X \
  --queue-id compute:7
```

Or preview the wrapped command shape without a real PID yet:

```bash
bash scripts/gpu-live-hip-linuxdrm.sh --dry-run --outdir /tmp/gpu-live
```

The wrapper:

- sets the required Go / CGO / blazesym environment internally
- runs the existing `live-hip-linuxdrm` helper under `sudo`
- accepts the same live knobs such as `--join-window`, `--duration`, and `--hip-library`
- requires `--pid` for a real run, because the target must already be a HIP process
- prints `join_stats` again after the helper completes

After a real run, the helper also prints the fastest inspection steps for the current MVP:

```bash
jq '.join_stats' /tmp/gpu-live/live_hip_linuxdrm.raw.json
jq '.' /tmp/gpu-live/live_hip_linuxdrm.attributions.json
```

And the KFD-only path writes the parallel `live_hip_linuxkfd.*` outputs.

The AMD execution/sample wrapper writes the parallel `live_hip_amdsample.*` outputs.

If `jq` is installed, it also prints:

- the `join_stats` block directly
- a short `join summary`
- a first-pass `tuning hint`

For the local HIP shim harness, the same script can now target either Linux surface:

```bash
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface drm
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface kfd
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-mode real
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-mode real --real-source rocm-smi --rocm-smi-path /opt/rocm/bin/rocm-smi
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-mode real --real-source rocprofiler-sdk --rocprofiler-sdk-command 'rocprofiler-sdk --hip-trace --output /tmp/rocprofiler-sdk-out'
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-mode real --real-source rocprofv2 --rocprofv2-path /opt/rocm/bin/rocprofv2
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-mode real --real-poll-interval 25ms
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --kernel-name flash_attn_fwd
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --device-id gfx942:0 --device-name MI300X --queue-id compute:7
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-collector-path /opt/rocm/bin/amd-sample-collector
```

`drm` remains the default. `kfd` switches the shim demo to the KFD-only live wrapper path. `amdsample` switches it to the execution/sample wrapper and defaults the sample producer to `bash scripts/amd-sample-adapter.sh`.

There is also a small checked-in AMD sample producer for live-shaped demos:

```bash
bash scripts/amd-sample-producer.sh --kernel-name hip_launch_shim_kernel
```

The checked-in adapter now prefers the Go collector binary and only falls back
to the shell producer if `go` or the collector package path is unavailable.
Both honor the same `PERF_AGENT_AMD_SAMPLE_MODE` / kernel / device / queue
contract, along with `PERF_AGENT_AMD_SAMPLE_REAL_SOURCE` for real mode, and
emit the same live-shaped `amdsample` execution/sample NDJSON with boot-relative
timestamps, so the path-based collector hook and the default adapter fallback
stay aligned.

There is also a fully offline host-to-execution path backed by checked-in fixtures. It replays the same canonical host launch plus a correlated execution/sample stream, then writes the folded flame input, raw snapshot, and a rendered SVG/HTML flamegraph:

```bash
bash scripts/gpu-offline-demo.sh host-exec /tmp/gpu-host-exec
xdg-open /tmp/gpu-host-exec/host_exec_sample.html 2>/dev/null || open /tmp/gpu-host-exec/host_exec_sample.html
```

The resulting folded line is expected to look like:

```text
train_step;cudaLaunchKernel;[gpu:cgroup:9876];[gpu:pod:pod-abc];[gpu:container:ctr-123];[gpu:launch];[gpu:kernel:flash_attn_fwd];[gpu:stall:memory_throttle] 7
```

There is also a checked-in HIP host + AMD execution/sample stdin path using the new `amdsample` source mode. This is the branch’s clearest CPU+GPU end-to-end flamegraph example right now:

```bash
bash scripts/gpu-offline-demo.sh hip-amd-sample /tmp/gpu-amd-exec
xdg-open /tmp/gpu-amd-exec/amd_sample_exec.html 2>/dev/null || open /tmp/gpu-amd-exec/amd_sample_exec.html
```

The generated folded lines are expected to look like:

```text
train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:memory_wait] 11
train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:wave_barrier] 5
```

The rendered HTML/SVG flamegraph now includes both:

- the CPU launch side: `train_step -> hipLaunchKernel`
- the GPU sample side: queue, kernel, stall, and richer sample frames such as function/source/PC when present

This is still not a true device-internal flame graph, but it is the current branch’s clearest CPU-to-GPU execution artifact. It proves:

- host launch replay through the canonical launch model
- execution/sample replay through the canonical execution model
- synthetic flame output for a correlated kernel sample path
- workload-level attribution for execution time and sample weight

There is also a richer checked-in variant that makes the rendered flamegraph more Brendan-like by projecting function, source, and PC frames into the GPU leaves:

```bash
bash scripts/gpu-offline-demo.sh hip-amd-sample-rich /tmp/gpu-amd-rich
xdg-open /tmp/gpu-amd-rich/amd_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-amd-rich/amd_sample_exec_rich.html
```

That view still starts with the CPU launch stack, but the GPU side now includes frames like:

```text
[gpu:function:flash_attn_fwd]
[gpu:source:flash_attn.hip:77]
[gpu:pc:0xabc]
```

Preferred CLI-shaped compatibility variant with `rocprofv3`:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofv3-rich /tmp/gpu-rocprofv3-rich
xdg-open /tmp/gpu-rocprofv3-rich/rocprofv3_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprofv3-rich/rocprofv3_sample_exec_rich.html
```

That path runs:

```text
rocprofv3 native records -> cmd/amd-sample-collector --mode real --real-source rocprofv3 -> perf-agent -> folded/svg/html
```

The checked-in demo currently exercises the file-output flavor of that contract.

Legacy compatibility variant with `rocprofv2`:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofv2-rich /tmp/gpu-rocprof-rich
xdg-open /tmp/gpu-rocprof-rich/rocprofv2_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprof-rich/rocprofv2_sample_exec_rich.html
```

There is also a legacy command-shaped variant that exercises the same adapter through the `rocprofv2` full-command hook and file-output discovery:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofv2-command-rich /tmp/gpu-rocprof-command-rich
xdg-open /tmp/gpu-rocprof-command-rich/rocprofv2_command_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprof-command-rich/rocprofv2_command_sample_exec_rich.html
```

Preferred modern SDK-shaped variant:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofiler-sdk-rich /tmp/gpu-rocprofiler-sdk-rich
xdg-open /tmp/gpu-rocprofiler-sdk-rich/rocprofiler_sdk_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprofiler-sdk-rich/rocprofiler_sdk_sample_exec_rich.html
```

This renders a mixed `CPU + GPU Flame Graph: rocprofiler_sdk_sample_exec_rich` artifact, keeping the CPU launch side (`train_step -> hipLaunchKernel`) visible above the richer GPU `function/source/pc` frames.

For a more realistic inference-style example with a deeper CPU stack, use:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofiler-sdk-llm-rich /tmp/gpu-rocprofiler-sdk-llm-rich
xdg-open /tmp/gpu-rocprofiler-sdk-llm-rich/rocprofiler_sdk_llm_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprofiler-sdk-llm-rich/rocprofiler_sdk_llm_sample_exec_rich.html
```

This renders a mixed `CPU + GPU Flame Graph: rocprofiler_sdk_llm_sample_exec_rich` artifact with a model-like CPU path:

```text
serve_request -> generate_token -> model_forward -> transformer_block_17 -> flash_attention -> hipLaunchKernel
```

and richer algorithm GPU leaves such as:

```text
[gpu:function:flash_attn_fwd]
[gpu:function:paged_kv_gather]
[gpu:function:flash_attn_epilogue]
```

This canonical SDK demo currently exercises the `rocprofiler-sdk` `external` mode rather than an in-process native SDK collector.

If you have a real `librocprofiler-sdk.so` available, there is also a native probe variant that uses the in-process loader path instead of the external adapter:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofiler-sdk-native-probe /tmp/gpu-rocprofiler-sdk-native-probe
xdg-open /tmp/gpu-rocprofiler-sdk-native-probe/rocprofiler_sdk_native_probe.html 2>/dev/null || open /tmp/gpu-rocprofiler-sdk-native-probe/rocprofiler_sdk_native_probe.html
```

That path currently emits a mixed CPU+GPU artifact from live SDK metadata probes:
- CPU side still comes from the checked-in HIP launch replay
- GPU side carries native SDK probe leaves such as `native_sdk_version` and `native_sdk_available_agents`

Recorder-envelope variant of the same modern SDK path:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofiler-sdk-recorder-rich /tmp/gpu-rocprofiler-sdk-recorder-rich
xdg-open /tmp/gpu-rocprofiler-sdk-recorder-rich/rocprofiler_sdk_recorder_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprofiler-sdk-recorder-rich/rocprofiler_sdk_recorder_sample_exec_rich.html
```

And the `rocprofv3` command-shaped compatibility variant:

```bash
bash scripts/gpu-offline-demo.sh hip-rocprofv3-command-rich /tmp/gpu-rocprofv3-command-rich
xdg-open /tmp/gpu-rocprofv3-command-rich/rocprofv3_command_sample_exec_rich.html 2>/dev/null || open /tmp/gpu-rocprofv3-command-rich/rocprofv3_command_sample_exec_rich.html
```

There is also a checked-in multi-workload execution path that proves exact correlation stays separated by workload:

```bash
bash scripts/gpu-offline-demo.sh multi-exec /tmp/gpu-multi-exec
xdg-open /tmp/gpu-multi-exec/multi_workload_exec.html 2>/dev/null || open /tmp/gpu-multi-exec/multi_workload_exec.html
```

The checked-in folded output currently looks like:

```text
train_step_a;cudaLaunchKernel;[gpu:cgroup:1000];[gpu:pod:pod-a];[gpu:launch];[gpu:kernel:alpha_kernel];[gpu:stall:memory_throttle] 11
train_step_b;cudaLaunchKernel;[gpu:cgroup:2000];[gpu:pod:pod-b];[gpu:launch];[gpu:kernel:beta_kernel];[gpu:stall:wait] 13
```

And the attribution rollup is expected to stay split cleanly across the two workloads:

```json
[
  {
    "cgroup_id": "1000",
    "pod_uid": "pod-a",
    "first_seen_ns": 10,
    "last_seen_ns": 80,
    "backends": ["stream"],
    "kernel_names": ["alpha_kernel"],
    "launch_count": 1,
    "exact_join_count": 1,
    "execution_count": 1,
    "execution_duration_ns": 60,
    "sample_weight": 11
  },
  {
    "cgroup_id": "2000",
    "pod_uid": "pod-b",
    "first_seen_ns": 40,
    "last_seen_ns": 100,
    "backends": ["stream"],
    "kernel_names": ["beta_kernel"],
    "launch_count": 1,
    "exact_join_count": 1,
    "execution_count": 1,
    "execution_duration_ns": 40,
    "sample_weight": 13
  }
]
```

The same run emits a workload rollup like:

```json
[
  {
    "cgroup_id": "9876",
    "pod_uid": "pod-abc",
    "container_id": "ctr-123",
    "container_runtime": "containerd",
    "first_seen_ns": 100,
    "last_seen_ns": 200,
    "backends": ["stream"],
    "kernel_names": ["flash_attn_fwd"],
    "launch_count": 1,
    "exact_join_count": 1,
    "execution_count": 1,
    "execution_duration_ns": 80,
    "sample_weight": 7
  }
]
```

There is also an offline host-to-driver flame path for the current MVP. It uses checked-in fixtures for a canonical host launch plus a normalized Linux DRM submit event, then writes folded stacks that you can render with Brendan Gregg’s FlameGraph tools:

```bash
go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/flash_attn_launches.json \
  --gpu-replay-input gpu/testdata/replay/host_driver_submit.json \
  --gpu-raw-output /tmp/gpu-host-driver.raw.json \
  --gpu-attribution-output /tmp/gpu-host-driver.attributions.json \
  --gpu-folded-output /tmp/gpu-host-driver.folded \
  --duration 1ms

flamegraph.pl /tmp/gpu-host-driver.folded > /tmp/gpu-host-driver.svg
cat /tmp/gpu-host-driver.attributions.json
```

The resulting folded line is expected to look like:

```text
train_step;cudaLaunchKernel;[gpu:cgroup:9876];[gpu:pod:pod-abc];[gpu:container:ctr-123];[gpu:launch];[gpu:event:submit:amdgpu-cs] 13
```

This is still a host-to-driver correlation flame, not a true GPU-internal flame graph. It proves:

- host launch replay through the canonical launch model
- lifecycle event replay through the canonical event model
- heuristic launch-to-submit attribution
- tenancy-aware folded output suitable for later `flamegraph.pl` rendering

For the live AMD path, `join_stats` in the raw snapshot is the quickest tuning signal:

- `launch_count`: total host launches observed
- `matched_launch_count`: launches used by at least one join
- `unmatched_launch_count`: launches that never matched
- `exact_execution_join_count`: execution joins by correlation ID
- `heuristic_event_join_count`: submit/wait joins by PID/TID plus time window
- `unmatched_candidate_event_count`: submit/wait events that did not match any launch

That gives a practical tuning loop for `--join-window`:

1. Run `live-hip-linuxdrm` against a real PID.
2. Inspect `jq '.join_stats' ...raw.json`.
3. If `unmatched_candidate_event_count` is high, widen the window.
4. If almost every launch matches but the associations look suspicious, narrow the window.

There is also a checked-in multi-workload lifecycle path that proves heuristic host-to-driver attribution stays workload-scoped:

```bash
go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/multi_workload_launches.json \
  --gpu-replay-input gpu/testdata/replay/multi_workload_submit.json \
  --gpu-raw-output /tmp/gpu-multi-driver.raw.json \
  --gpu-attribution-output /tmp/gpu-multi-driver.attributions.json \
  --gpu-folded-output /tmp/gpu-multi-driver.folded \
  --gpu-profile-output /tmp/gpu-multi-driver.pb.gz \
  --duration 1ms

flamegraph.pl /tmp/gpu-multi-driver.folded > /tmp/gpu-multi-driver.svg
cat /tmp/gpu-multi-driver.attributions.json
```

The checked-in folded output currently looks like:

```text
train_step_b;cudaLaunchKernel;[gpu:cgroup:2000];[gpu:pod:pod-b];[gpu:launch];[gpu:event:submit:submit-b] 5
train_step_a;cudaLaunchKernel;[gpu:cgroup:1000];[gpu:pod:pod-a];[gpu:launch];[gpu:event:submit:submit-a1] 3
train_step_a;cudaLaunchKernel;[gpu:cgroup:1000];[gpu:pod:pod-a];[gpu:launch];[gpu:event:wait:wait-a2] 4
```

And the attribution rollup is expected to show the heuristic join counts per workload:

```json
[
  {
    "cgroup_id": "1000",
    "pod_uid": "pod-a",
    "first_seen_ns": 10,
    "last_seen_ns": 29,
    "backends": ["linuxdrm", "stream"],
    "kernel_names": ["alpha_kernel"],
    "launch_count": 1,
    "heuristic_join_count": 2,
    "event_count": 2,
    "event_duration_ns": 7
  },
  {
    "cgroup_id": "2000",
    "pod_uid": "pod-b",
    "first_seen_ns": 40,
    "last_seen_ns": 55,
    "backends": ["linuxdrm", "stream"],
    "kernel_names": ["beta_kernel"],
    "launch_count": 1,
    "heuristic_join_count": 1,
    "event_count": 1,
    "event_duration_ns": 5
  }
]
```

The same run now also emits a workload-level rollup in the raw snapshot:

```json
[
  {
    "cgroup_id": "9876",
    "pod_uid": "pod-abc",
    "container_id": "ctr-123",
    "container_runtime": "containerd",
    "first_seen_ns": 100,
    "last_seen_ns": 143,
    "backends": ["linuxdrm", "stream"],
    "kernel_names": ["flash_attn_fwd"],
    "launch_count": 1,
    "heuristic_join_count": 1,
    "event_count": 1,
    "event_duration_ns": 13
  }
]
```

Those attribution summaries are meant to be the bridge from profiling artifacts to workload-oriented reporting:

- `cgroup_id`, `pod_uid`, `container_id`, `container_runtime` identify the workload
- `first_seen_ns` and `last_seen_ns` bound the observed activity window
- `backends` shows which collection paths contributed data
- `kernel_names` lists the unique kernels currently associated with that workload in the snapshot
- `exact_join_count` and `heuristic_join_count` show how much of the rollup came from exact correlation versus fallback matching
- `launch_count`, `event_count`, and the duration counters provide a first rollup surface for per-workload GPU usage

If you want just the workload rollup without the full snapshot, use `--gpu-attribution-output <path>`. It writes the same `attributions` array as standalone JSON.

### Experimental Linux DRM lifecycle backend

There is also an experimental Linux-first GPU lifecycle backend. It traces `ioctl` activity for a single target PID, emits normalized lifecycle events into the GPU timeline, and writes the raw JSON snapshot through the existing GPU export path.

```bash
go run . \
  --pid 12345 \
  --gpu-linux-drm \
  --gpu-raw-output /tmp/gpu-linuxdrm.json \
  --duration 5s
```

What this currently provides:

- PID-scoped Linux DRM boundary telemetry
- normalized DRM `ioctl` lifecycle events in the GPU JSON snapshot
- conservative semantic naming for recognizable DRM core calls such as sync waits, PRIME imports/exports, and generic driver-command buckets
- scheduler wakeup and runqueue-latency events for the same target PID
- stable device attrs, with open-driver enrichment when `/sys/dev/char` exposes a DRM node and bound kernel driver
- a real eBPF + ringbuf collector path behind the existing `gpu` manager

Current limits:

- `--pid` is required
- `-a/--all` is not supported for this backend
- raw JSON is the primary output artifact for this mode
- it needs a Linux host with BPF attach capability and a real `/dev/dri/renderD*` workload to observe
- it does not yet decode vendor-specific submit/wait semantics, queue/context identities, device counters, or vendor runtime correlation

On an AMDGPU host, the backend now adds a small amount of optional driver-specific naming when `/sys/dev/char` resolves the render node to `amdgpu`. The first mapped commands are focused on high-signal operations such as:

- `amdgpu-cs`
- `amdgpu-wait-cs`
- `amdgpu-wait-fences`
- `amdgpu-gem-create`
- `amdgpu-gem-mmap`
- `amdgpu-info`

One practical local validation loop is:

```bash
rocminfo >/tmp/rocminfo.out 2>/tmp/rocminfo.err &
pid=$!
sudo timeout 5s go run . \
  --pid "$pid" \
  --gpu-linux-drm \
  --gpu-raw-output /tmp/amdgpu.json \
  --duration 3s
wait "$pid" || true
```

Then inspect `/tmp/amdgpu.json` for `amdgpu-*` event names and `command_family=amdgpu`.

There is also a capability-gated integration test for real AMDGPU observation:

```bash
sudo -E go test ./gpu/backend/linuxdrm -run '^TestLinuxDRMAMDGPUObservation$' -v
```

If you want to save the observed normalized snapshot as a fixture for offline iteration, set:

```bash
export PERF_AGENT_WRITE_AMDGPU_FIXTURE=/tmp/amdgpu-observation.json
sudo -E go test ./gpu/backend/linuxdrm -run '^TestLinuxDRMAMDGPUObservation$' -v
```

The test will write the normalized GPU snapshot JSON to the requested path after it sees a real `amdgpu-*` event.

### PMU output

On-CPU time, runqueue latency, context-switch reasons, hardware counters (cycles, instructions, cache misses), and derived metrics (IPC, cache miss rate).

Example:
```
=== PMU Metrics (PID: 84228) ===
Samples: 26358

On-CPU Time (time slice per context switch):
  Min:    0.003 ms
  P50:    0.071 ms
  P99:    9.183 ms

Runqueue Latency (time waiting for CPU):
  Min:    0.001 ms
  P50:    0.012 ms
  P99:    0.850 ms

Context Switch Reasons:
  Preempted (running):     45.2%  (11912 times)
  Voluntary (sleep/mutex): 42.1%  (11095 times)
  I/O Wait (D state):      12.7%  (3351 times)

Hardware Counters:
  IPC (Instr/Cycle):  2.342
  Cache Misses/1K:    0.022
```

## Library Usage

`perf-agent` is also a Go library via the `perfagent` package:

```go
package main

import (
    "context"
    "log"
    "time"
    "github.com/dpsoft/perf-agent/perfagent"
)

func main() {
    agent, err := perfagent.New(
        perfagent.WithPID(12345),
        perfagent.WithCPUProfile("profile.pb.gz"),
        perfagent.WithPMU(),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer agent.Close()

    ctx := context.Background()
    agent.Start(ctx)
    time.Sleep(10 * time.Second)
    agent.Stop(ctx)
}
```

### In-Memory Collection

```go
var buf bytes.Buffer
agent, _ := perfagent.New(
    perfagent.WithCPUProfileWriter(&buf), // gzip-compressed pprof
)
// After Stop(), buf contains ready-to-use .pb.gz data
```

### Custom Metrics Export

```go
agent, _ := perfagent.New(
    perfagent.WithPMU(),
    perfagent.WithMetricsExporter(&MyExporter{}),
)
```

See [perfagent package documentation](perfagent/) for all available options.

## Building

Requires Go 1.26+, Clang/LLVM, Linux headers, and [blazesym](https://github.com/libbpf/blazesym) (Rust C library for symbolization).

```bash
make build
```

The Makefile defaults to `GOTOOLCHAIN=auto`, so Go fetches the pinned toolchain automatically if your system Go is older. Override with `GOTOOLCHAIN=local make build` to enforce the locally-installed toolchain.

See [BUILDING.md](BUILDING.md) for the full toolchain setup.

---

## Testing

Unit tests run without root; integration tests require root or a setcap'd binary.

```bash
# Build + cap the binary once, then run tests as a normal user
make build
sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent

# Unit tests (no root)
make test-unit

# Integration tests — auto-skip when neither root nor caps are available
make test-integration
```

Test gates honor file capabilities on the `perf-agent` binary: a setcap'd `perf-agent` lets the test runner exec it without sudo. For tests that load BPF in-process (library tests), the test binary itself needs caps — `setcap` it after `go test -c`.

For detailed testing documentation see [TESTING.md](TESTING.md).

---

## Contributing

PRs welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening one — it covers build/test conventions, the commit-message style, and what's in-scope vs. deferred. By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

---

## Security

If you find a security issue, please do **not** open a public issue. See [SECURITY.md](SECURITY.md) for the reporting channel and threat model. perf-agent runs with elevated kernel capabilities; we take privilege-escalation and kernel-DoS reports seriously.

---

## License

Apache License 2.0 — see [LICENSE](LICENSE).
