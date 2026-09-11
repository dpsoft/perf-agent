# GPU collector — DaemonSet deployment — design

Issue: #124. Companion to
`2026-09-10-pid-namespace-translation-design.md`, which settled the
namespace and capability questions this design depends on.

## Purpose

Profile every GPU pod on a node **without editing any application's pod
spec**. That is the collector's whole reason to exist: the sidecar works,
and it requires touching the workload's manifest, which is the thing
platform teams cannot do at scale.

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

`hostPath` removes all three. One shim file on the node, bind-mounted
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

## Open scope questions

Deliberately not settled here; they need answers before the plan:

1. **Does the collector own shim installation?** A DaemonSet init
   container writing the shim to the `hostPath` is the obvious shape, but
   it means the collector writes to the node filesystem. The alternative
   is that node provisioning places it and the collector only reads.
2. **Pods whose containers do not mount the `hostPath`.** The collector
   can only see workloads that loaded the shim. Is a pod that did not
   mount it silently unprofiled, or reported as a known gap?
3. **Eviction.** `pidRegistry` evicts on process exit; whether a pod
   ending should also drop its cached labels and module bytes is
   unexamined.
4. **`examples/kubernetes/`**: a sibling `collector-daemonset.yaml`
   alongside the sidecar example, or its own directory.
5. **Spec §11** is written sidecar-first ("perf-agent runs per-pod rather
   than as a DaemonSet"). Amend in place or add a collector section.

## Non-goals

- **`privileged`.** The four-capability set plus `hostPID` is the target.
  `CAP_SYS_ADMIN` where a mechanism needs it is acceptable; a blanket
  `privileged: true` is not.
- **Replacing the sidecar.** Both ship. The sidecar remains the
  finer-grained option and the one that needs no node-level mount.
- **A cluster-wide watcher or scrape config.** `CONTRIBUTING.md:69` puts
  that outside this project: perf-agent is the engine.

## Testing

- **The single-link claim is the thing to prove, and it must be proven
  with two pods.** One `UprobeMulti` with `PID: 0` against a shared
  `hostPath` inode, with a second GPU workload started **after** attach,
  asserting the second one's launches are sampled. A test with one pod
  cannot distinguish `PID: 0` from a lucky single-target attach.
- **The inode rule needs a negative test.** Point the collector at a
  *copy* of the shim and assert zero probe fires — the failure the example
  warns about produces no error, so only an assertion catches it.
- **`hostPID` refusal.** Run the collector without it and assert it names
  the requirement instead of emitting an empty profile.
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
