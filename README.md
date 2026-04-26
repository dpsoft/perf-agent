# perf-agent

eBPF-based performance monitoring agent for Linux. CPU profiling, off-CPU profiling, and PMU hardware counters in a single binary, with a hybrid FP+DWARF stack walker that handles release-built C++/Rust binaries that omit frame pointers.

## What you get

- **On-CPU profiles** with full stack traces → pprof.
- **Off-CPU profiles** (blocking/sleep time, with stacks) → pprof.
- **PMU metrics** — hardware counters, scheduling latency, context-switch breakdown.
- **High-fidelity pprof output**: real per-binary `Mapping` entries with build-id and file offsets, address-keyed `Location` entries — feeds tools like `go tool pprof`, differential profiling, and downstream LLVM sample-PGO converters.
- **Multi-runtime symbolization**: native (DWARF + ELF), Python (`-X perf` perf-maps), Node.js (`--perf-basic-prof`), Go.

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

See [BUILDING.md](BUILDING.md) for detailed setup instructions.

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

For detailed testing documentation, see [TESTING.md](TESTING.md).
