# GPU collector — DaemonSet deployment — design

Issue: #124. Companion to
`2026-09-10-pid-namespace-translation-design.md`, which settled the
namespace and capability questions this design depends on.

## Purpose

Run **one agent per node instead of one per profiled pod**, and let it
profile every GPU pod on that node.

An earlier draft of this document claimed the collector profiles
"workloads nobody edited". **That was wrong and is corrected here.** CUDA
injection happens because `CUDA_INJECTION64_PATH` is in the target's
environment at `cuInit`, and nothing in either shim layout puts it there
on its own — the application's pod spec still declares it. A `hostPath`
shim changes which inode the variable points at; it does not remove the
variable. Zero-touch needs a third party to mutate the pod, which is a
separate design (see Non-goals).

Polar Signals is in the same position and it is worth being explicit about
it: parca-agent's **CPU** profiling is zero-instrumentation, its **GPU**
profiling is not — their documented layout has the application pod mount a
volume and set `CUDA_INJECTION64_PATH`, exactly as ours does.

So the collector's v1 value is narrower than "zero-touch", and still real:

- **One agent process per node**, not one sidecar per profiled pod — the
  per-pod container cost in #124's table, which is the cost that scales
  with the number of workloads.
- **A smaller edit to the application pod.** With `hostPath` the pod adds
  one volume mount and one environment variable. With the sidecar's
  `emptyDir` it also adds an init container. Less to get wrong, and
  nothing in the app pod that has to be version-matched to the agent.
- **Node-level lifecycle.** The agent is upgraded, restarted and
  configured once per node rather than once per workload.

## Settled before this design

These were decided on 2026-09-10 and are inputs, not open questions:

- **The agent runs in the init PID namespace** (`hostPID: true`). No PID
  translation, in either direction. See the companion design for why
  `/proc` cannot supply one and why we decline to build a BPF one.
- **`CAP_SYS_ADMIN` is acceptable** where a mechanism needs it; explicit
  capabilities are still preferred over blanket `privileged`.
- **Kernel floor 6.2+**, with `uprobe_multi` on 6.6+ and `perf_uprobe`
  below it.
- **Shim delivery differs by deployment, one per deployment:**
  - **sidecar → `emptyDir`** (already shipped;
    `examples/kubernetes/pytorch-gpu-profile.yaml`)
  - **collector → `hostPath`** (this design)

## The inode is the design

Uprobe attachment keys on `(dev, ino)`. The existing example says it
plainly, and it is the constraint everything here follows from:

> the agent must open the identical inode, not a copy of the same bytes.
> Anything that duplicates the file produces zero probe fires and no error.

A sidecar satisfies that trivially: it lives in the pod and opens the same
`emptyDir` file the app loaded. Polar Signals' parcagpu uses the same
per-pod shared-volume shape, and our sidecar is modelled on it.

**A collector cannot.** It is not in the pod. Reaching each pod's copy
means `/var/lib/kubelet/pods/<uid>/volumes/…` or `/proc/<pid>/root/…`, and
every copy is a distinct inode. That buys three problems at once:

1. **A link per pod**, bounded by whatever the kernel allows.
2. **A pod watch**, to add links as pods appear and drop them as they go.
3. **A late-attach race on every new pod.** The adapter captures module
   bytes only while a consumer is present (#121), so a pod that reaches
   `cuInit` before the collector notices it loses its modules — and that
   loss is silent.

`hostPath` removes all three, and does so while making the application's
pod spec smaller rather than larger. One shim file on the node, mounted
read-only into every GPU pod at the same path, means every CUDA process on
that node maps **one inode**:

```go
ex, _ := link.OpenExecutable(shimPath)
l, _ := ex.UprobeMulti(nil, objs.GpuUsdtBatch, &link.UprobeMultiOptions{
        Addresses: addrs, RefCtrOffsets: refCtrs, Cookies: cookies,
        PID: 0, // every process mapping this file — including ones not yet started
})
```

`PID: 0` is already the API's default behaviour; the single-PID case is
the restriction (`gpuprobe/consumer.go:1760`). The probe lives on the
inode, not on a process, so it is armed **before any pod exists** and the
`cuInit` race cannot occur. Attachment needs no pod lifecycle at all.

### Verified on hardware, 2026-09-15

This is the load-bearing claim of the whole design, so it was measured
before the plan was written rather than after. Two processes on one host
stand in for two pods; RTX 3090, rescans disabled for the run
(`-rediscover-every 1h`) so that a rescan cannot be mistaken for the
uprobe.

| process | agent discovered it | samples in profile |
|---|---|---|
| A, started **before** attach | yes (`173742`) | 296,364 |
| B, started **after** attach | **no** | **120,000** |

B was never discovered and never rescanned for, and its launches are in
the profile. `PID: 0` covers processes that did not exist when the link
was created.

**The negative control, which is what makes that interpretable.** Two
files, identical bytes (`md5` equal), different inodes. The agent attached
to `shimcopy` (inode 5960732); a decoy loaded `shimdir` (inode 5960731):

| process | inode | samples in profile |
|---|---|---|
| C, on the attached inode | 5960732 | 298,844 |
| D, identical bytes, other inode | 5960731 | **none** |

D is absent entirely, so the test was capable of failing and the
`(dev, ino)` rule holds as the layout assumes.

A second signal agrees without being designed for: the shim's own
rendezvous reports `enroll=confirmed` for B and `enroll=no-listener` for
D. Two independent mechanisms, same answer.

Reproduction: `~/perf-agent-spike/spike-positive.sh` and
`spike-negative.sh` — throwaway, not part of the tree.

Cost, stated rather than designed around: one shim version per node, so no
per-workload canary; and a node running both deployments has two shim
versions live. Neither is worth a link-per-pod to avoid.

## What already exists

Three pieces landed before this design and are not rebuilt:

| piece | where | note |
|---|---|---|
| target discovery by mapping | `gpuprobe.ProcessesMappingShim` (#138) | scans `/proc` for processes mapping the shim inode |
| late registration | `Consumer.RegisterTarget` | installs CFI for a process found after attach; idempotent |
| pod identity without an API server | `internal/k8slabels.FromPID` | cgroup v2 parsing, no kubelet or CRI socket |

With `hostPath` + `PID: 0`, discovery stops being load-bearing for
*attachment* and becomes load-bearing only for *unwind-table
registration*: a process that enrolled itself during `cuInit` needs
nothing, and the rescan covers the ones already past it when we arrived.
`-rediscover-every` keeps its meaning and its default.

## Mode selection

**Explicit `agent` / `collector`, not inferred.** #124 proposes this and
it is right here for a reason beyond log readability: the two deployments
now differ in shim delivery (`emptyDir` vs `hostPath`) and in pod-spec
requirements (`hostPID`). A binary that guessed would guess about the
operator's manifest, and would guess silently.

Each mode asserts its own preconditions at startup and refuses rather than
degrading:

- `collector` requires the init PID namespace. If `/proc` reads fail for
  PIDs BPF reports as live, say so and name `hostPID: true` — the failure
  that produced an empty, silent profile in the 2026-09-10 containerized
  run.
- `agent` requires the shim path to be openable and to be the same inode
  the target mapped.

## Per-pod attribution

**One profile, labelled per pod** — not one profile per pod.

`k8slabels.FromPID` already derives pod identity from cgroups and the
agent already merges it into per-sample labels. pprof labels are the
existing mechanism, the storage side already groups by them, and N output
streams would need a lifecycle, a naming scheme and a flush policy that
nothing currently asks for.

## Installation: the agent carries the shim and copies it on start

The agent image ships `libperfagent-gpu-nvidia.so` and writes it to the
`hostPath` directory at startup. No separate init container, no
out-of-band node provisioning step.

The reason to prefer it over node provisioning is version coupling: the
shim and the consumer that decodes its USDT payload are one artifact, so
they cannot drift. Record layouts are frozen per version (see the GPU V2
Phase 3 work); a node-provisioned shim that is older or newer than the
agent is a decode failure waiting to happen, and nothing in the pod spec
would reveal it.

The directory is therefore mounted **read-write** for the agent and
**read-only** for application pods.

### Replace atomically, and only when it changed

Write to a temporary file in the same directory and `rename(2)` over the
target. `rename` within one filesystem is atomic, so the path a pod
resolves at `cuInit` is never a partially written file. Never write in
place: the file is mapped executable by live processes, and truncating it
under them is a crash, not a version skew.

Compare build-ids first and skip the write when they match. A restarting
agent on an unchanged node should cost nothing, and — more importantly —
should not create a new inode (below).

### An upgrade leaves two live inodes, briefly

This is the consequence of `rename` and it has to be designed for, because
it partially reintroduces the problem `hostPath` was chosen to remove.

After a replace, processes that already mapped the old file keep **the old
inode** — that is what makes the replace safe. New processes map the new
one. So a node mid-upgrade has two shim inodes with live mappers, and a
single `UprobeMulti` on the new path would silently stop covering every
workload that was already running.

The rule: **attach to the current file, plus one link per distinct older
inode that still has a mapper.** That set is discovered the way targets
already are — `/proc/*/maps` — and is bounded by the number of live shim
versions, normally one and transiently two, rather than by the number of
pods. Drop a link when its inode has no mappers left.

Note this does not resurrect the `cuInit` race: each link is created
before the version it covers can be mapped by anything new, because the
new inode does not exist until the agent creates it.

### The ordering hazard, stated

A GPU pod that starts before the agent has written the file resolves
`CUDA_INJECTION64_PATH` to a missing path. **CUDA injection fails open and
silent** (`docs/gpu-injection.md`), so that workload runs unprofiled with
no error anywhere — the same shape as every other silent failure this
project has hit.

No layout removes this entirely; node provisioning only moves it earlier.
What the agent can do is report it: on startup, count GPU processes
already running that have **not** mapped any shim inode, and say so. That
number is also the answer to "pods that never opted in", so one diagnostic
covers both.

## The pod-spec contract

What an application pod must declare to be profilable by the collector.
Stated explicitly because it is the thing v1 does *not* remove:

```yaml
    volumeMounts:
      - name: perfagent-shim
        mountPath: /var/lib/perf-agent/gpu
        readOnly: true
    env:
      - name: CUDA_INJECTION64_PATH
        value: /var/lib/perf-agent/gpu/libperfagent-gpu-nvidia.so
  volumes:
    - name: perfagent-shim
      hostPath:
        path: /var/lib/perf-agent/gpu
        type: Directory
```

Two lines of intent, no init container, and no image the pod has to keep
in step with the agent. A pod that omits this is simply not profiled, and
the collector should be able to say how many GPU processes it saw that
never mapped the shim — otherwise "not profiled" and "nothing ran" look
identical, which is the failure mode this project keeps meeting.

## Open scope questions

Deliberately not settled here; they need answers before the plan:

1. ~~Does the collector own shim installation?~~ **Settled: the agent
   image carries the shim and copies it on start.** See Installation.
2. ~~Pods that do not mount the shim.~~ **Settled above:** they are not
   profiled, and the collector reports how many GPU processes it saw that
   never mapped the shim. Silence there would make "not opted in" and
   "nothing ran" identical.
3. **`-discover` cannot attach with zero targets.** `waitForTargets`
   fatals after `-wait-for-shim` when nothing maps the shim, so today's
   CLI cannot start before the workloads it will profile. A collector
   always starts first. This is a required change, not a question: attach
   with `PID: 0` and wait, treating an empty node as normal rather than
   fatal. Found while setting up the spike above, which had to start a
   workload first to work around it.
4. **Eviction.** `pidRegistry` evicts on process exit; whether a pod
   ending should also drop its cached labels and module bytes is
   unexamined.
5. **`examples/kubernetes/`**: a sibling `collector-daemonset.yaml`
   alongside the sidecar example, or its own directory.
6. **Spec §11** is written sidecar-first ("perf-agent runs per-pod rather
   than as a DaemonSet"). Amend in place or add a collector section.


## Non-goals

- **Zero-touch injection, in v1.** Making a pod profilable without editing
  it requires something to inject the mount and the environment variable
  at admission — a mutating webhook (the Istio sidecar-injection pattern),
  or a hook in the NVIDIA GPU operator / container toolkit, which already
  injects libraries and environment into GPU containers. Either is a
  design of its own, and both become *easier* once this one exists,
  because the webhook then only has to add a mount and a variable rather
  than an init container. Deferred, not rejected.
- **`privileged`.** The four-capability set plus `hostPID` is the target.
  `CAP_SYS_ADMIN` where a mechanism needs it is acceptable; a blanket
  `privileged: true` is not.
- **Replacing the sidecar.** Both ship. The sidecar remains the
  finer-grained option and the one that needs no node-level mount.
- **A cluster-wide watcher or scrape config.** `CONTRIBUTING.md:69` puts
  that outside this project: perf-agent is the engine.

## Testing

- **The single-link claim is proven (above) and now needs a regression
  test**, not an investigation. Two workloads, the second started after
  attach, rescans disabled, asserting both appear. A test with one
  workload cannot distinguish `PID: 0` from a lucky single-target attach,
  and a test with rescans on cannot distinguish it from rediscovery.
- **The inode rule needs its negative test in the tree.** Point the agent
  at a *copy* of the shim and assert zero probe fires — the failure
  produces no error, so only an assertion catches it.
- **`hostPID` refusal.** Run the collector without it and assert it names
  the requirement instead of emitting an empty profile.
- **The upgrade case needs its own test, and it is the one most likely to
  be skipped.** Start a workload, replace the shim, start a second
  workload, and assert BOTH are still sampled. A test that replaces the
  file with nothing running passes while covering none of the risk.
- Rootful podman with GPU passthrough remains the harness
  (`run-nspid-task9.sh` is the starting point); two concurrent containers
  stand in for two pods.

## Risks

- **One shim version per node** is a rollout constraint: updating it
  affects every GPU workload on that node at once.
- **`hostPath` is a node-level write surface** if the collector installs
  the shim itself. See open question 1.
- **`PID: 0` attaches to every process mapping the inode**, including ones
  outside Kubernetes entirely. On a node that also runs the sidecar, both
  may see the same process; the join is per-consumer so this is believed
  benign, and is worth asserting rather than assuming.
