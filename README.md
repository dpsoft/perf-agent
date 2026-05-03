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

---

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
