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

## Two deployments: sidecar and collector

[Polar Signals' `parcagpu`][ps] established the injection pattern both of ours
follow: an init container drops a CUPTI-based `.so` onto a shared volume, and
the application container loads it via `CUDA_INJECTION64_PATH`. That part we
take directly.

There are two manifests here, and the difference is **where the agent runs**,
which decides **how many shim inodes exist**:

| | sidecar (`pytorch-gpu-profile.yaml`) | collector (`gpu-collector-daemonset.yaml`) |
|---|---|---|
| agents | one per profiled pod | one per node |
| shim volume | `emptyDir`, one inode per pod | `hostPath`, one inode per node |
| attach | per-pod, inside the pod | one `UprobeMulti` with `PID: 0` |
| process visibility | `shareProcessNamespace: true` (pod only) | `hostPID: true` (whole node) |
| app pod adds | init container + volume + env | volume + env |
| blast radius | one pod | every process on the node |

The sidecar can open the pod's `emptyDir` inode because it is *in* the pod. A
collector is not, so per-pod copies would mean a link per pod, a watch for pods
appearing and leaving, and the loss of every pod that reached `cuInit` before
we noticed it. One inode per node removes all three — measured on an RTX 3090,
2026-09-15: a workload started *after* the link existed, never discovered and
with rediscovery disabled, was sampled anyway; a byte-identical copy at another
path was not.

Against parcagpu, the difference that survives is privilege, not topology:

| | parcagpu | perf-agent collector |
|---|---|---|
| `hostPID` | yes | yes |
| `privileged` | **yes** | **no** |
| `CAP_SYS_ADMIN` | always | only below kernel 6.6 |

`CAP_SYS_ADMIN` is a preference here rather than a prohibition — see constraint
2 below. Not being `privileged` is the claim worth making.

**Neither deployment is zero-touch.** Both require the application pod to
declare the volume and `CUDA_INJECTION64_PATH`. Parca is in the same position:
their CPU profiling is zero-instrumentation, their GPU profiling is not.

## The three constraints this manifest exists to encode

**1. Same inode, not same bytes.** uprobe attachment keys on `(dev, ino)`. The
agent must open the *identical file* the application loaded. Any packaging
convenience that duplicates it — `go:embed`-and-extract, a per-container copy,
a second init container — yields **zero probe fires and no error message**.
The shared `emptyDir` exists for this reason alone.

**2. `uprobe_multi` where it exists, `perf_uprobe` below it.** Measured on
hardware: attaching the shim's USDT probe through the `perf_uprobe` PMU fails
`EACCES` under `CAP_BPF`+`CAP_PERFMON` and works only once `CAP_SYS_ADMIN` is
added; the `uprobe_multi` BPF link works without it, and costs **Linux ≥ 6.6**.

Prefer the multi link: one link for N probes and a smaller capability set. But
"no `CAP_SYS_ADMIN`" and "6.6+" were the same decision stated twice, and
relaxing the first releases the second — the project floor is **6.2+**, with
the PMU path plus `CAP_SYS_ADMIN` as the fallback on 6.2–6.5.

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
