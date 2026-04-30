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

### Offline and live helper script

The current branch also includes a small checked-in helper for the MVP workflows:

```bash
scripts/gpu-offline-demo.sh [--dry-run] <mode> <outdir>
```

Current modes are:

- `host-exec`
- `hip-amd-sample`
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

If `--sample-command` is omitted, the wrapper now defaults to that checked-in
adapter script automatically. The adapter can then:
- exec `--sample-collector-path` / `PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH` directly
- run `PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND`
- or fall back to the checked-in synthetic producer

There is now a checked-in collector executable you can build and pass through
`--sample-collector-path`:

```bash
go build -o /tmp/amd-sample-collector ./cmd/amd-sample-collector

bash scripts/gpu-live-hip-amdsample.sh \
  --outdir /tmp/gpu-live \
  --pid 4242 \
  --sample-collector-path /tmp/amd-sample-collector
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
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --kernel-name flash_attn_fwd
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --device-id gfx942:0 --device-name MI300X --queue-id compute:7
bash scripts/gpu-live-hip-shim-demo.sh --dry-run --linux-surface amdsample --sample-collector-path /opt/rocm/bin/amd-sample-collector
```

`drm` remains the default. `kfd` switches the shim demo to the KFD-only live wrapper path. `amdsample` switches it to the execution/sample wrapper and defaults the sample producer to `bash scripts/amd-sample-adapter.sh`.

There is also a small checked-in AMD sample producer for live-shaped demos:

```bash
bash scripts/amd-sample-producer.sh --kernel-name hip_launch_shim_kernel
```

The checked-in adapter defaults to the synthetic producer and emits producer-native `amdsample` execution/sample NDJSON with boot-relative
timestamps, which is a closer stand-in for a real live producer than replaying a
static checked-in file.

The checked-in Go collector binary emits the same live-shaped NDJSON contract as
the shell producer, but through the real `--sample-collector-path` executable
hook rather than a shell command string.

There is also a fully offline host-to-execution path backed by checked-in fixtures. It replays the same canonical host launch plus a correlated execution/sample stream, then writes the folded flame input and raw snapshot:

```bash
go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/flash_attn_launches.json \
  --gpu-replay-input gpu/testdata/replay/host_exec_sample.json \
  --gpu-raw-output /tmp/gpu-host-exec.raw.json \
  --gpu-attribution-output /tmp/gpu-host-exec.attributions.json \
  --gpu-folded-output /tmp/gpu-host-exec.folded \
  --duration 1ms

flamegraph.pl /tmp/gpu-host-exec.folded > /tmp/gpu-host-exec.svg
cat /tmp/gpu-host-exec.attributions.json
```

The resulting folded line is expected to look like:

```text
train_step;cudaLaunchKernel;[gpu:cgroup:9876];[gpu:pod:pod-abc];[gpu:container:ctr-123];[gpu:launch];[gpu:kernel:flash_attn_fwd];[gpu:stall:memory_throttle] 7
```

There is also a checked-in HIP host + AMD execution/sample stdin path using the new `amdsample` source mode:

```bash
go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/hip_kfd_launches.json \
  --gpu-amd-sample-stdin \
  --gpu-raw-output /tmp/gpu-amd-exec.raw.json \
  --gpu-attribution-output /tmp/gpu-amd-exec.attributions.json \
  --gpu-folded-output /tmp/gpu-amd-exec.folded \
  --gpu-profile-output /tmp/gpu-amd-exec.pb.gz \
  --duration 1ms < gpu/testdata/replay/amd_sample_exec.ndjson

flamegraph.pl /tmp/gpu-amd-exec.folded > /tmp/gpu-amd-exec.svg
cat /tmp/gpu-amd-exec.attributions.json
```

The resulting folded lines are expected to look like:

```text
train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:memory_wait] 11
train_step;hipLaunchKernel;[gpu:cgroup:138970];[gpu:launch];[gpu:queue:compute:0];[gpu:kernel:hip_launch_shim_kernel];[gpu:stall:wave_barrier] 5
```

This is still not a true device-internal flame graph, but it is the current branch’s clearest CPU-to-GPU execution artifact. It proves:

- host launch replay through the canonical launch model
- execution/sample replay through the canonical execution model
- synthetic flame output for a correlated kernel sample path
- workload-level attribution for execution time and sample weight

There is also a checked-in multi-workload execution path that proves exact correlation stays separated by workload:

```bash
go run . \
  --gpu-host-replay-input gpu/testdata/host/replay/multi_workload_launches.json \
  --gpu-replay-input gpu/testdata/replay/multi_workload_exec.json \
  --gpu-raw-output /tmp/gpu-multi-exec.raw.json \
  --gpu-attribution-output /tmp/gpu-multi-exec.attributions.json \
  --gpu-folded-output /tmp/gpu-multi-exec.folded \
  --gpu-profile-output /tmp/gpu-multi-exec.pb.gz \
  --duration 1ms

flamegraph.pl /tmp/gpu-multi-exec.folded > /tmp/gpu-multi-exec.svg
cat /tmp/gpu-multi-exec.attributions.json
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
