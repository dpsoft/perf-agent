# perf-agent

*eBPF-based Linux profiler — CPU, off-CPU, and PMU, system-wide or per-PID, pprof output.*

[![CI](https://github.com/dpsoft/perf-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/dpsoft/perf-agent/actions/workflows/ci.yml)
[![Tests](https://github.com/dpsoft/perf-agent/actions/workflows/tests.yml/badge.svg)](https://github.com/dpsoft/perf-agent/actions/workflows/tests.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dpsoft/perf-agent.svg)](https://pkg.go.dev/github.com/dpsoft/perf-agent)
[![Go Version](https://img.shields.io/github/go-mod/go-version/dpsoft/perf-agent)](go.mod)
[![License](https://img.shields.io/github/license/dpsoft/perf-agent)](LICENSE)

One binary, runs locally, no backend or telemetry.

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

### 🔥 On-demand production profiling

Hot-attach to a running process — no restart, no preinstalled agent. For Python 3.12+, `--inject-python` enables the perf trampoline only for the capture window, so there's no persistent overhead.

### 💤 Off-CPU stalls and blocking analysis

Find why a service is "slow but not CPU-busy." `--offcpu` hooks `sched_switch` and accumulates blocking time per call site — lock waits, syscall blocks, channel reads, mutex contention.

### 🐍 Cross-language flame graphs

One profile, multiple runtimes. Native (DWARF + ELF) symbolizes alongside Python (`-X perf` perf-maps, optionally activated on demand), Node.js (`--perf-basic-prof`), and Go. The hybrid FP+DWARF unwinder handles release-built C++/Rust without `-fno-omit-frame-pointer`.

### 📊 Hardware-counter performance investigations

`--pmu` summarizes IPC, cache miss rate, runqueue latency (P50/P99), and context-switch reasons (preempted vs voluntary vs I/O wait). Combine with `--per-pid` in system-wide mode to see which processes dominate the node's wait time.

### 🧪 Differential profiling and sample-based PGO

High-fidelity pprof: every `Mapping` carries the absolute path, GNU build-id, and file offsets; every `Location` is address-stable across runs. Feeds `go tool pprof -diff_base`, LLVM SamplePGO converters, and any cross-run analysis that depends on stable address-level identity.

### 🐳 Sidecar profiling inside Kubernetes pods

`--pid <N>` is namespace-aware (with `shareProcessNamespace: true` on the pod), so the in-pod PID just works. Output samples carry k8s identity labels (`pod_uid`, `container_id`, `cgroup_path`) parsed from the cgroup, plus best-effort `pod_name` / `namespace` / `container_name` from the downward API. **No kubelet API calls, no client-go dependency.**

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

Two specific deployment shapes — Python via `--inject-python`, and sidecar inside a Kubernetes pod — work as documented in the use cases above. Python details: [docs/python-profiling.md](docs/python-profiling.md).

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

CPU and off-CPU profiles are full-fidelity pprof: every `Mapping` carries the absolute path, GNU build-id, and file offsets; every `Location` is keyed by file offset (not symbol name) so cross-run diffing and sample-PGO converters work. `[kernel]` and `[jit]` sentinels handle the special cases. Tags from `--tag key=value` land as profile-level comments; k8s identity labels (when running in a pod) attach per-sample.

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
agent, _ := perfagent.New(
    perfagent.WithPID(12345),
    perfagent.WithCPUProfile("profile.pb.gz"),
    perfagent.WithPMU(),
)
defer agent.Close()
agent.Start(ctx); time.Sleep(10*time.Second); agent.Stop(ctx)
```

See the [`perfagent` package docs](perfagent/) for in-memory output, custom label enrichers, and metrics exporters.

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

Two stack-walker paths: **`--unwind fp`** (cheap, kernel-side aggregation; truncates on FP-less code) and **`--unwind dwarf`** / **`auto`** (default — FP fast path with `.eh_frame`-derived CFI fallback for release C++/Rust without frame pointers).

Sample addresses resolve through `procmap.Resolver` (lazy `/proc/<pid>/maps` + build-id), so each pprof `Mapping` carries real per-binary identity and each `Location` is keyed by `(mapping_id, file_offset)` — what `go tool pprof -diff_base` and sample-based PGO converters need to round-trip.

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
