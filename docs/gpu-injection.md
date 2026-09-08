<!-- docs/gpu-injection.md -->
# Installing the GPU adapter with CUDA injection

perf-agent profiles GPU work by getting a small shared library -- the *shim* --
loaded into the application's own process, where it subscribes to CUPTI and
emits USDT probes the agent consumes. The application is not rebuilt, not
relinked, and not otherwise modified.

This document is how to deliver that library, and how to tell whether it
actually arrived.

## The mechanism

The CUDA driver loads whatever `CUDA_INJECTION64_PATH` names, during `cuInit`,
and calls its `InitializeInjection`:

```
CUDA_INJECTION64_PATH=/path/to/libperfagent-gpu-nvidia.so ./your-cuda-app
```

Two properties of this mechanism drive everything below.

**It fails open and silent.** If the driver cannot load the library -- wrong
path, wrong architecture, a glibc the process does not have -- it carries on
exactly as though the variable were never set. Nothing is logged by the driver,
the application runs normally, and the profile comes out empty. *"The shim is
broken"* and *"this workload launched no kernels"* produce identical output.
Verifying injection is therefore not optional; see **Did it work?** below.

**`CUDA_INJECTION64_PATH` is absent from NVIDIA's public CUPTI documentation**
(only `NVTX_INJECTION64_PATH` is). It is the mechanism NVIDIA's own tools use
and that other profilers depend on, but treat it as semi-official surface.

## Which shim to use

Build the **portable** one for anything that leaves your machine:

```bash
make -C shim nvidia-portable      # needs podman/docker and CUPTI headers
```

`make -C shim nvidia` is for local work on the machine that built it. The
difference is not cosmetic: the shim is loaded into *someone else's* process, so
its requirements are requirements on the **target**.

| | `nvidia` (local) | `nvidia-portable` |
|---|---|---|
| glibc floor | your build machine's | **2.34** |
| libstdc++ | dynamic | statically linked in |
| RUNPATH | your CUDA path | none |
| loads on ubuntu 22.04 / 24.04 / 25.04 | not necessarily | **yes, asserted in CI** |

Both resolve CUPTI at **runtime**, so neither is tied to a CUDA major version
and neither needs the toolkit present in order to build.

`shim/elfgate.sh` checks a shim against those requirements and names any it
fails:

```bash
./shim/elfgate.sh shim/libperfagent-gpu-nvidia.portable.so 2.34
```

CI runs it on every change and additionally `dlopen`s the artifact inside
Ubuntu 22.04, 24.04 and 25.04 -- because the ELF version headers said an
unloadable shim was fine, and only `dlopen` disagreed.

## CUPTI is yours to supply

CUPTI ships with the **CUDA toolkit**, not the driver, and
`nvidia-container-toolkit` never injects it. The shim `dlopen`s
`libcupti.so.13`, `.12`, `.11`, then the bare soname; if none is present it says
so and lets the process run unprofiled rather than taking it down.

In a PyTorch image the pip-installed CUPTI is usually already present, under
`site-packages/nvidia/*/lib`. Put that directory on `LD_LIBRARY_PATH` if the
loader does not find it.

## Delivering it

### Bare metal

```bash
CUDA_INJECTION64_PATH=/opt/perf-agent/libperfagent-gpu-nvidia.so ./your-app
```

### A container

The library must be inside the container and readable by the process:

```bash
podman run --rm \
  -v /opt/perf-agent/libperfagent-gpu-nvidia.so:/opt/pa/shim.so:ro \
  -e CUDA_INJECTION64_PATH=/opt/pa/shim.so \
  your-cuda-image
```

### Kubernetes

An init container copies the shim onto a shared volume, the application
container mounts it and sets the environment variable. See
[`examples/kubernetes/`](../examples/kubernetes/) for a full manifest and for
what does and does not work today.

**The agent must open the same inode**, not a copy of the same bytes: uprobe
attachment keys on `(dev, ino)`. Two copies of an identical file produce zero
probe fires and no error message.

## Did it work?

The single most useful check, because the failure is silent:

```bash
grep libperfagent /proc/<pid>/maps
```

Several file-backed mappings, `r-xp` among them, all sharing one inode. **No
output means injection did not happen** -- do not read the resulting empty
profile as "this workload used no GPU".

`cmd/gpu-cuda-profile` performs this check itself, and when a run samples no
launches it says which of the two causes it was: the shim mapped but the adapter
reported nothing, or the shim never mapped at all. It says nothing on a healthy
run.

The shim also reports its own state on stderr:

```
perfagent-cupti: initialized pid=26 sample_period=8 ... enroll=confirmed
```

and when it cannot find CUPTI:

```
perfagent-cupti: cannot load libcupti (tried libcupti.so.13, .12, .11, libcupti.so): ...
perfagent-cupti: CUPTI ships with the CUDA toolkit, not the driver ...
perfagent-cupti: not attaching; the process runs unprofiled
```

## Capabilities

The agent -- not the shim -- needs `CAP_BPF`, `CAP_PERFMON`, `CAP_SYS_PTRACE`,
`CAP_CHECKPOINT_RESTORE`, and `CAP_SYSLOG` for kernel stacks. It does **not**
need `CAP_SYS_ADMIN`, and that is a deliberate constraint rather than an
accident: the USDT consumer attaches through the `uprobe_multi` BPF link,
because the `perf_uprobe` PMU path requires `CAP_SYS_ADMIN` and that is what
gets a per-pod agent rejected by admission policy. It costs a **Linux 6.6**
floor.

## Profiling a process that is already running

A sidecar or a node collector cannot launch what it profiles: the kubelet
started it. `-pid` attaches to a process that is already running.

```
gpu-cuda-profile -pid 12345 -duration 60s -out gpu.pb.gz
```

The target must have been started with `CUDA_INJECTION64_PATH` already in its
environment — the driver loads the shim during `cuInit` and never again, and
nothing (not `-pid`, not `ptrace`) can add it to a live process afterwards.
That is why the init container sets the variable on the *application* pod
rather than on the profiler. If the shim is not in the target's mappings the
command refuses at startup and says so, rather than collecting nothing for a
full `-duration`.

Three things behave differently from a launched run, and all three are
consequences of not being the parent:

- **Flags that configure the target are refused, not ignored.** `-period` and
  `-gpu-pc-sampling` are read by the adapter out of the target's environment at
  `cuInit`. Accepting them here would change nothing while making the profile
  look as though it had been taken at a rate it was not.
- **The run ends when the target exits.** A sampled launch's stack is
  symbolized against `/proc/<pid>/maps`, which the kernel destroys the instant
  the process leaves; anything collected past that point has stacks that can
  never be resolved.
- **The timeline is drained on an interval** (`-drain-every`, default 2s)
  rather than once at the end. Its rings hold one interval, not a whole run —
  a 20 s attach to a process issuing ~5,500 launches/s overruns a run-length
  ring and loses GPU time outright.

Capabilities are the same set as a launched run; no `privileged`, no
`CAP_SYS_ADMIN`. A node collector additionally needs `hostPID: true` to see
the pids it is attaching to, which is a genuine privilege increase over the
sidecar and should be stated in a pod spec rather than absorbed quietly.

## What this does not yet cover

- **No published container image** for the init-container pattern. CI uploads
  the portable shim as a build artifact; there is no registry image to name in
  a pod spec yet.
- **Retention costs the target memory.** The shim captures every module's
  bytes whether or not a consumer has attached, because the driver offers them
  exactly once and there is no way to ask again — so a process that is never
  profiled can hold up to 512 modules or 64 MiB (the consumer's own module
  store bounds) on a profiler's behalf. Past those bounds captures are dropped
  and counted; nothing grows without limit. `module_retained_unattached` in the
  adapter's exit report is what it is currently holding.
- **The collector (DaemonSet) shape is not built yet** — `-pid` is its
  prerequisite, not the whole of it. One profile per pod versus one profile
  labelled by pod, attaching to pods that start later, and whether the shim is
  one inode per node or one per pod are all open. Tracked in issue #124.
