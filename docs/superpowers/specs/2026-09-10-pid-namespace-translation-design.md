# The profiler runs in the init PID namespace — design

Issue: #124. Supersedes the "PID namespace translation" design of the same
date, which is retained below as the record of a disproved approach.

**Decision: perf-agent requires the init PID namespace. It does not
translate PIDs, and it will not grow machinery to avoid `hostPID`.**

## Why this is the whole design

Every eBPF program reports init-namespace PIDs; that is what the kernel
gives it. A profiler that resolves those PIDs against `/proc` therefore
has to be in the namespace where they mean something. There are exactly
two ways to arrange that, and only one of them is simple:

1. **Be in the init PID namespace.** `hostPID: true` on the DaemonSet, or
   run on the host. Nothing to translate, nothing to detect, no new code.
2. **Translate.** Requires the kernel to tell us the mapping, because
   `/proc` cannot (see the correction below), which means a BPF change at
   every PID capture site plus a fallback path at each one.

**Every comparable profiler chose (1).** Parca runs as a node DaemonSet
with `hostPID` and `privileged`; the OpenTelemetry eBPF profiler runs as a
host-level agent with `hostPID: true` and the host `/proc` mounted, and
resolves *container attribution* — which pod owns this PID — by parsing
cgroups, which is a different question. Neither maps host PIDs to
namespace-local ones, because neither is ever nested.

This repository already said so, in §11 of the GPU V2 design:

> Parca does not solve the sidecar problem; it avoids it by running as a
> node DaemonSet with `hostPID` and `privileged`.

We adopt the same constraint. Not because translation is impossible — it
is implementable via `bpf_get_ns_current_pid_tgid` — but because it is
accidental complexity bought to avoid a pod-spec field that the collector
deployment in #124 already assumes. Six BPF call sites, four regenerated
objects and a per-site fallback is a large price for declining one line of
YAML that the entire industry writes.

## Capability posture

The same reasoning applies one level down. `CAP_SYS_ADMIN` is **acceptable
where it is genuinely required**, which is the posture Parca and the OTel
profiler take: the OTel deployment guidance asks for `hostPID: true` plus
"either privileged mode or explicit capabilities such as SYS_ADMIN,
PERFMON, and BPF".

The preference order stays, because it costs nothing to keep:

1. **Explicit capabilities over blanket `privileged`.** Naming
   `CAP_BPF`, `CAP_PERFMON`, `CAP_SYS_PTRACE`, `CAP_CHECKPOINT_RESTORE`
   (and `CAP_SYSLOG` for kernel stacks) is strictly more auditable than
   `privileged: true`, and the agent already runs that way.
2. **`CAP_SYS_ADMIN` only where a mechanism needs it** — not as a
   blanket, and named in the docs where it applies.

What changes is that its presence is no longer disqualifying. Phase 1
treated "no `CAP_SYS_ADMIN`" as an absolute, and that absolute is what
pinned the GPU path to `uprobe_multi` and therefore to kernel 6.6. Relaxed
to a preference, the floor question reopens — see below.

`hostPID` is a genuine privilege increase over a sidecar and is documented
as a requirement rather than absorbed quietly.

## What we build instead

Nothing that translates. Two things that make the constraint legible:

1. **A startup check that names the situation.** The failure mode being
   removed is silence: a nested agent resolves nothing and emits an empty
   profile indistinguishable from a workload that never ran. The agent
   must say which namespace it is in, once, at startup.
2. **A diagnostic when `/proc` reads fail systematically.** If BPF reports
   a PID as live and `/proc/<pid>` does not exist, the agent is in the
   wrong namespace. That is already counted — `UnwindPIDsFailed`,
   `UnwindLastError` — and it is what made the failed containerized run
   legible. It gets promoted from a stats field to an actionable message
   naming `hostPID: true`.

Detection note: the namespace cannot be identified from `NSpid` (below),
so the check is symptomatic rather than declarative — repeated `ENOENT` on
PIDs BPF just reported. That is the honest signal, and it is the one that
actually fired.

## Kernel floor

**One floor: 6.2+**, for every path.

6.6 was never a kernel constraint on the GPU path; it was a consequence of
refusing `CAP_SYS_ADMIN`. `uprobe_multi` landed in 6.6 and is the only
USDT attach that avoids that capability — so "no `CAP_SYS_ADMIN`" and
"6.6+" were the same decision stated twice. Relaxing the first releases
the second:

| kernel | USDT attach | capability |
|---|---|---|
| **6.6+** | `link.UprobeMulti` | four-cap set, no `CAP_SYS_ADMIN` |
| **6.2 – 6.5** | `link.Uprobe` (`perf_uprobe` PMU) | adds `CAP_SYS_ADMIN` |

Prefer the multi link wherever it exists: one link for N probes, and it
keeps the capability set smaller on any reasonably current node. Fall back
below 6.6 and say which path was taken and why, at startup — a silent
fallback that quietly needs a capability the operator did not grant is the
same class of defect as the silent empty profile above.

`README.md:148` still says "5.8+", which predates all of this and should
state the real floor and the ladder.

## Correction: why `/proc` cannot supply the mapping

Retained because it is the evidence for the decision above.

The superseded design assumed every visible process states its namespace
chain in `/proc/<local>/status`, so one pass yields `host → local`:

```
NSpid:  44321   17        # outermost (host) first, innermost last
```

**That form only exists when read downward.** `NSpid` reports PIDs with
respect to the *reading* process's namespace and its descendants. Measured
on this machine, same process, two readers:

| reader | `NSpid` |
|---|---|
| host (an ancestor namespace) | `NSpid:  64393   1` |
| inside the container | `NSpid:  1` |

So a nested agent cannot see its own host PID, let alone anyone else's,
and no scan of its `/proc` can build the mapping. `nspid.NestedAt` counted
those columns and therefore returned false on the host **and** inside a
container — a detector that cannot succeed. Every resolver kept
`IdentityMapper`; the containerized run failed exactly there:

```
UnwindLastError: read /proc/63926/maps: no such file or directory
StackWalkAbandoned:8000   StackWalkReachedRoot:0
```

`63926` is the host PID; the workload was PID `27` inside the container.

**How it survived to a live run.** The unit tests staged
`/proc/self/status` containing `NSpid: 44000 9` — a shape the kernel never
produces for a self-read from inside. The fixture encoded the assumption
under test, so the tests confirmed the assumption instead of checking the
kernel. Nothing cheaper than a container could have caught it: on the host
and in CI, host PID == local PID.

**A pre-existing bug falls out of the same fact.** `internal/nspid.Translate`
returns `fields[0]` as the host PID. Read from inside a sidecar that is the
*local* PID, returned unchanged — so `perfagent.resolveTarget` has been
translating `-p` to itself in the one deployment the function was written
for, since before this work. Under this design the function has no
remaining purpose and should be removed with its callers, not fixed.

## What is removed

All of it, except the parts that were never about namespaces:

- `nspid.Index`, `BuildAt`, `Cache`, `Shared`, `Nested`, `NestedAt`,
  `Confirm`, `Translate` — the whole package.
- `procmap.Mapper`, `IdentityMapper`, `WithMapper`, `SetMapper`,
  `TranslationStats`, the namespace-aware default mapper, and the Warmer's
  `LocalToHost` sweep translation.
- The read-PID / enrol-PID split in `ehmaps`. Under this design the two
  numbers are always equal, and a split maintained for a case that cannot
  arise is exactly the accidental complexity this decision rejects.
- The k8slabels and perf.data translation wrappers.

## What is kept

Two changes that are testability improvements independent of namespaces,
and were only discovered because this work went looking:

- **`openableBinary` takes a `procRoot` and returns its resolved source.**
  It previously hardcoded `/proc`, so no fixture could stage a `map_files`
  entry and no test could tell whether the fallback ran. That
  unobservability is why a `procRoot != "/proc"` path-sniff stood in for a
  flag. The sniff is gone and the behaviour is asserted.
- **The scan takes an `enroller` interface.** The `PIDTracker` in unit
  tests has nil BPF maps, so `EnrollWithoutCompile` fails before recording
  anything — which is why the existing tests there assert almost nothing
  and tolerate every error. A test double makes the enrolment decision
  visible.

Both should be preserved on their own merits when the rest is unwound.

## Testing

- The disproof belongs in a test: read the real `/proc/self/status` and
  assert `NSpid` has exactly one column. That is a fact about the kernel,
  it holds on the host and in a container, and it prevents anyone
  rebuilding the superseded design.
- The startup check and the `ENOENT` diagnostic are assertable without
  privileges.
- The containerized run (`run-nspid-task9.sh`) becomes a *negative*
  acceptance test: without `hostPID`, the agent must say so clearly rather
  than emit an empty profile. Add `--pid host` and it must behave exactly
  as it does on the host.
