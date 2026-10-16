# Container images

v1.3.0 publishes two images, multi-arch (amd64 and arm64), built natively on
each architecture rather than under emulation:

```
ghcr.io/dpsoft/perf-agent:v1.3.0        # the profiler
ghcr.io/dpsoft/perf-agent-shim:v1.3.0   # the CUPTI adapter, as a carrier image
```

Both carry the same tag on purpose. The shim's USDT record layouts are frozen
per release and the agent decodes them, so the pair is meant to match —
though nothing at runtime refuses a mismatched one, so pin the version rather
than tracking `:latest` in anything you keep.

## The shim image is a carrier, not a program

It is a busybox layer holding one file. Running it prints that and exits
non-zero, which is deliberate: a pod spec that names it as a normal container
should fail loudly rather than idle.

```
/usr/local/lib/libperfagent-gpu-nvidia.so
```

Two ways to use it.

**As an init container**, copying the library onto a volume the application
container also mounts. That is what `examples/kubernetes/` does.

**By taking the file out**, for anything that is not Kubernetes:

```bash
id=$(podman create ghcr.io/dpsoft/perf-agent-shim:v1.3.0)
podman cp "$id":/usr/local/lib/libperfagent-gpu-nvidia.so ./shim.so
podman rm "$id"
```

Then point the workload at it. The variable has to be set **before** the
process starts — injection happens inside `cuInit` and cannot be added to a
running process:

```bash
podman run --rm \
  -v "$PWD/shim.so":/opt/pa/shim.so:ro \
  -e CUDA_INJECTION64_PATH=/opt/pa/shim.so \
  your-cuda-image
```

## The agent image

It runs `gpu-cuda-profile` as uid 65532, which works because the binary
carries file capabilities. The kernel refuses to exec such a file unless
those capabilities are in the container's **bounding set**, and it refuses
with a bare `EPERM` before any of the program's code runs — so a container
started without them reports `exec ... Operation not permitted`, naming
neither the capability nor the setting that should have granted it. The
entrypoint checks the bounding set first and says which one is missing.

Four capabilities, and no `privileged`:

| | |
|---|---|
| `BPF` | load programs, create maps and links |
| `PERFMON` | `perf_event_open`, including system-wide |
| `SYS_PTRACE` | read another process's memory |
| `CHECKPOINT_RESTORE` | `/proc/<pid>/map_files`, for symbolization |

Attaching to a process outside the container also needs its pid namespace,
which is `--pid=host` here and `hostPID: true` in a pod spec. That is a real
privilege increase over a sidecar sharing one pod's namespace, and is worth
stating rather than absorbing quietly.

```bash
podman run --rm \
  --pid=host \
  --cap-drop=ALL \
  --cap-add=BPF --cap-add=PERFMON \
  --cap-add=SYS_PTRACE --cap-add=CHECKPOINT_RESTORE \
  -v /sys/kernel/btf:/sys/kernel/btf:ro \
  -v "$PWD":/profiles \
  ghcr.io/dpsoft/perf-agent:v1.3.0 \
    -pid <PID> -out /profiles/gpu.pb.gz -duration 60s
```

`/sys/kernel/btf` is mounted read-only because the BPF programs are loaded
against the running kernel's BTF.

For the node-wide shape, `-mode=collector -shim-dir=<dir>` installs the
carried shim into a directory and attaches to every process that maps that
inode, including ones that start later. See
[`examples/kubernetes/gpu-collector-daemonset.yaml`](../examples/kubernetes/gpu-collector-daemonset.yaml).

## What the images do not remove

The application still opts in. A `hostPath` shim changes which inode
`CUDA_INJECTION64_PATH` names; it does not remove the variable, and the
workload still has to be started with it set. Injecting it without editing
the pod needs a mutating admission webhook, which is not built.
