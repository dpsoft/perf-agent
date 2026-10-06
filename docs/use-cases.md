# What you can do with perf-agent

## 🎮 GPU kernels under the stack that launched them

CUDA kernels land under the code that queued them, joined on CUPTI's correlation
id — so a slow kernel points at the call path responsible, Python frames
included, not just at itself.

```bash
# The workload must already have loaded the adapter:
#   CUDA_INJECTION64_PATH=/path/to/libperfagent-gpu-nvidia.so python train.py
./perf-agent --profile --offcpu --gpu \
    --gpu-shim /path/to/libperfagent-gpu-nvidia.so --pid <PID>

# One profile per collector; fuse them into the picture below
go run ./cmd/flamegraph -fuse -o fused.html \
    -in 'cpu.pb.gz;;[cpu] perf-agent 99 Hz' \
    -in 'gpu.pb.gz;;[gpu] per sampled launch'
```

![PyTorch training: Python, torch's C++ autograd, and CUDA kernels in one flame graph](flamegraph-pytorch-gpu.png)

*The flame graph those two commands produce: `pa_train_step
(torch_workload.py:76)` under `Thread.run (threading.py:1001)`, down through
`torch::autograd` and `cublasSgemm_v2`, across the `[gpu:launch]` boundary into
the CUDA kernel — Python, C++ and GPU in one tree.
[Interactive version](flamegraph-pytorch-gpu.html).*

Injection happens during `cuInit` and cannot be added to a live process, so
`CUDA_INJECTION64_PATH` must be set before the workload starts — Parca and
OpenTelemetry's profilers require the same. `--gpu-shim` must be the *same file*
the target mapped: the uprobe attaches by inode, so identical bytes at another
path capture nothing.

## 🔥 On-demand production profiling

Hot-attach to a running process — no restart, no preinstalled agent. The agent never modifies the process it measures.

## 💤 Off-CPU stalls and blocking analysis

Find why a service is "slow but not CPU-busy." `--offcpu` hooks `sched_switch` and accumulates blocking time per call site — lock waits, syscall blocks, channel reads, mutex contention.

## 🐍 Cross-language flame graphs

One profile, multiple runtimes. Native (DWARF + ELF) symbolizes alongside Node.js (`--perf-basic-prof`), Go, and any runtime that writes a `/tmp/perf-<pid>.map`. The hybrid FP+DWARF unwinder handles release-built C++/Rust without `-fno-omit-frame-pointer`.

**Python frames come from the interpreter's own frame chain**, walked in BPF — no
injection into the target, no `CAP_SYS_PTRACE`, nothing mutated. Measured on
CPython 3.12–3.14, including distro builds whose eval loop is split by LTO and
reachable only through `.gnu_debugdata`. An interpreter it cannot walk is refused
by name in the log rather than quietly yielding C frames.

> Enrolment is per-PID today: a `--pid` capture walks Python, a system-wide `-a`
> one does not and says so ([#194](https://github.com/dpsoft/perf-agent/issues/194)).

## 📊 Hardware-counter performance investigations

`--pmu` summarizes IPC, cache miss rate, runqueue latency (P50/P99), and context-switch reasons (preempted vs voluntary vs I/O wait). Combine with `--per-pid` in system-wide mode to see which processes dominate the node's wait time.

## 🐳 Kubernetes-aware profile labels

Run as a **DaemonSet on the host PID namespace** (recommended): perf-agent
sees every node process and tags each sample with `pod_uid`,
`container_id`, and `cgroup_path` parsed from `/proc/<pid>/cgroup` — no
kubelet API, no client-go.

For single-tenant pods, sidecar mode also works with
`shareProcessNamespace: true` (which exposes every container's processes
to every other container — fine when the agent and target are co-deployed
by the same operator, a security regression otherwise). Downward-API
env vars then add `pod_name` / `namespace` / `container_name` labels.

`--pid <N>` accepts in-pod PIDs and translates them to host PIDs automatically.

## 🔍 Stripped production binaries via off-box symbols

Production builds usually strip debug info. Point perf-agent at a
`debuginfod`-protocol server with `--debuginfod-url=URL`. A per-mapping
classifier routes each binary in the target:

- Has local DWARF or resolvable `.gnu_debuglink` → blazesym's process-mode
  (system libs from distro debuginfo land here for free).
- Stripped, build-id only (Rust/Go release builds) → file-mode against the
  cached `.debug`, fetched on demand and content-addressed by build-id.
- Deleted-but-still-mapped binary (sidecar / mount-namespace case) →
  same flow, opened via `/proc/<pid>/map_files`.

Cache layout, dispatcher details, and the address-normalization math:
see [docs/debuginfod-symbolization.md](debuginfod-symbolization.md).

## 🧪 PGO and flame graphs

High-fidelity pprof: every `Mapping` carries the absolute path, GNU build-id, and file offsets; every `Location` is address-stable across runs. Feeds `go tool pprof`, `-diff_base`, and Go's native `-pgo=` flag.

```bash
perf-agent --profile --pid <PID> --duration 30s --profile-output cpu.pb.gz
go build -pgo=cpu.pb.gz -o app .
```

`Function.start_line` is populated, which is what Go's PGO matches on
alongside the function name; a profile without it is refused outright.
Inlined frames are the exception and carry no start line — Go keys its hot
nodes on out-of-line functions, so it accepts and applies the profile
regardless.

For toolchains that don't speak pprof, add `--perf-data-output app.perf.data` to emit a kernel-format `perf.data` alongside the pprof output. Same capture, two formats:

- **AutoFDO PGO** for Rust (`rustc -Cllvm-args=-sample-profile-file=...`) and C++ (`clang -fprofile-sample-use=...`) via Google's [`create_llvm_prof`](https://github.com/google/autofdo). End-to-end demo: [`examples/rust-pgo`](../examples/rust-pgo/), [`examples/cpp-pgo`](../examples/cpp-pgo/).
- **[FlameGraph](https://github.com/brendangregg/FlameGraph)** — `perf script | stackcollapse-perf.pl | flamegraph.pl` produces an SVG. Demo: [`examples/flamegraph`](../examples/flamegraph/).

See [`docs/perf-data-output.md`](perf-data-output.md) for the per-tool walkthrough.

### Built-in HTML flame graph

`--flamegraph-output auto` writes an interactive flame graph beside the pprof file — one HTML file, no server, no CDN, no external script or font, correct opened straight off disk. Click a frame to zoom, `/` to search, `Esc` to clear then reset.

```bash
sudo ./perf-agent --pid 1234 --profile --flamegraph-output auto --duration 30s
```

To render a profile written earlier, or one from any other pprof producer:

```bash
go run ./cmd/flamegraph -o profile.html profile.pb.gz
go run ./cmd/flamegraph -folded profile.pb.gz     # the a;b;c 123 text form
```

Colour encodes **domain** — application, libc/startup, GPU runtime, unsymbolized, perf-agent's own shim, the `[gpu:launch]` CPU→GPU boundary, GPU kernel — not a hash of the frame name, and the page carries a legend saying so.

The page also states what it is *not* showing: the count of frames with no symbol, the share of GPU time sitting under `[gpu:launch unsampled]` with no CPU caller, the launch sampling period and what it does to the widths, and every per-sample label that is deliberately kept out of the tree. A profile with no samples renders a page that says so rather than an empty rectangle.

---
