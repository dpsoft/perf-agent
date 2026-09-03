<!-- examples/kubernetes/README.md -->
# perf-agent in Kubernetes — CUDA profiling of an unmodified pod

Profile a PyTorch CUDA workload running in Kubernetes, without rebuilding it,
relinking it, or changing a line of its code — then render the result as a
flame graph.

> **Status: not runnable yet. This is a target shape, not a demonstration.**
>
> Every other directory under `examples/` runs end-to-end today. This one does
> not, and the README says so rather than letting you discover it at `kubectl
> apply`. Two things block it:
>
> 1. **[#121] The shim cannot load into these images.** It is built against
>    glibc 2.42 with a hardcoded `RUNPATH` to the build machine's CUDA 13.3,
>    and measurably fails to load on Ubuntu 22.04, 24.04 and 25.04 — which is
>    what essentially every PyTorch image is. There is also no published shim
>    image; `ghcr.io/dpsoft/perf-agent-shim` does not exist yet.
> 2. **The sidecar's cross-namespace inode open is untested.** Injection is
>    confirmed on hardware; the agent opening *the same inode* from a
>    *different mount namespace* is the one part of the design nothing has
>    exercised.
>
> The manifest is here because it is the thing #121 has to make work. It
> encodes the constraints, so the build fix has a target to satisfy.

## Why a sidecar, when Parca uses a DaemonSet

[Polar Signals' `parcagpu`][ps] established the injection pattern this example
follows: an init container drops a CUPTI-based `.so` onto an `emptyDir`, and
the application container loads it via `CUDA_INJECTION64_PATH`. That part we
take directly.

Where we diverge is the consumer. Parca does not solve the sidecar problem — it
avoids it, by running as a **node DaemonSet with `hostPID` and `privileged`**.
perf-agent runs **per-pod**, which is a harder constraint and the reason for
most of the annotation in the manifest:

| | parcagpu | perf-agent |
|---|---|---|
| consumer | node DaemonSet | pod sidecar |
| process visibility | `hostPID: true` | `shareProcessNamespace: true` (pod only) |
| privileges | `privileged: true` | 4 capabilities, no `CAP_SYS_ADMIN` |
| blast radius | every process on the node | one pod |

A per-pod agent that asks for `privileged` gets rejected by admission policy,
so "drop `CAP_SYS_ADMIN`" is a deployment prerequisite rather than hardening
for its own sake.

## The three constraints this manifest exists to encode

**1. Same inode, not same bytes.** uprobe attachment keys on `(dev, ino)`. The
agent must open the *identical file* the application loaded. Any packaging
convenience that duplicates it — `go:embed`-and-extract, a per-container copy,
a second init container — yields **zero probe fires and no error message**.
The shared `emptyDir` exists for this reason alone.

**2. `uprobe_multi`, not `perf_uprobe`.** Measured on hardware: attaching the
shim's USDT probe through the `perf_uprobe` PMU fails `EACCES` under
`CAP_BPF`+`CAP_PERFMON` and works only once `CAP_SYS_ADMIN` is added; the
`uprobe_multi` BPF link works without it. The capability list in the manifest
is only achievable via the second path. Cost: **Linux ≥ 6.6 on the node.**

**3. CUPTI is yours to supply.** It ships with the CUDA *toolkit*, not the
driver, and `nvidia-container-toolkit` never injects it (zero `cupti` hits in
`nvc_info.c`). The pinned image carries the pip CUPTI that PyTorch pulls in; a
slim inference image may not.

## Apply

```bash
kubectl apply -f pytorch-gpu-profile.yaml
kubectl wait --for=condition=Ready pod/pytorch-gpu-profile --timeout=5m
kubectl logs -f pytorch-gpu-profile -c perf-agent
```

**Verify injection actually happened before trusting anything else.**
`CUDA_INJECTION64_PATH` fails **open and silent**: on any error the CUDA loader
carries on as if you had never set it, so a broken shim is indistinguishable
from a workload that launched no kernels. The check is that the app process
mapped the file:

```bash
kubectl exec pytorch-gpu-profile -c perf-agent -- \
  grep libperfagent /proc/1/maps
```

Four file-backed mappings, `r-xp` among them, all sharing one inode. No output
means injection did not happen — do not read the empty profile as "no GPU work".

## Pull the profile out and render it

```bash
kubectl cp pytorch-gpu-profile:/profiles/gpu.pb.gz ./gpu.pb.gz -c perf-agent
go run ./cmd/flamegraph -o gpu.html gpu.pb.gz   # renders an interactive HTML page
```

See [`../flamegraph/`](../flamegraph/) for the CPU-profile equivalent and the
Brendan Gregg toolchain path.

### Reading the result without being misled

A GPU flame graph is not a CPU flame graph with extra frames, and three of its
properties will mislead a reader who assumes otherwise (issue #123):

- **A CPU frame under `[gpu:launch]` is as wide as the GPU time launched *from*
  it** — not the CPU time spent in it. The Python and C++ frames you recognize
  are measuring the device, not themselves.
- **`[gpu:launch unsampled]` is expected, and is not lost data.** Stack capture
  is sampled (`--period`, default 8); durations never are. At the default, most
  of the canvas is GPU time whose launch was not stack-sampled. It is drawn
  hatched and quantified rather than dropped or silently reassigned to a
  sibling. Use `--period 1` for full attribution, at a throughput cost.
- **Do not compare these nanoseconds with a CPU profile's.** One is
  `samples × period`; the other is a measured `EndNs - StartNs` interval. Same
  printed unit, different quantities.

The axis is labelled `gpu/nanoseconds` and the profile contains exactly one
sample type — units are not mixed on the canvas.

## Not covered here

Multi-GPU pods, MIG, multi-node jobs, and continuous profiling (this manifest
is a one-shot `restartPolicy: Never` capture). Profiles land on an `emptyDir`
and die with the pod; a real deployment wants a PVC or an uploader.

[ps]: https://www.polarsignals.com/blog/posts/2025/12/18/profiling-nvidia-cuda-in-kubernetes
