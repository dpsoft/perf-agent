# Namespace-aware `--pid` + k8s pprof labels — Design Spec

> **Status:** drafted, awaiting user review.
> **Scope:** small. CLI behaviour unchanged at the surface; library gains two
> new options. No new BPF, no watchers, no k8s API integration.

## Problem

Today's perf-agent works for two targeting models — `--pid N` and `-a/--all` —
both of which assume the agent runs in the host PID namespace. When deployed
as a sidecar in a Kubernetes pod (the common production way to profile a
specific application), two things break:

1. **PID number means nothing across namespaces.** A user who knows the PID
   from inside their pod (e.g. PID `5` of their Java app) passes `--pid 5`,
   but the agent's BPF filter operates on **host kernel PIDs**, which differ.
   Result: the BPF map is keyed on PID 5 of the wrong process (or none at
   all), no samples are captured, no error is surfaced.

2. **Output pprof has no k8s identity.** A pprof captured against host PID
   12345 has no way for downstream consumers (Grafana, Pyroscope-compatible
   stores, custom dashboards) to know which pod/container it came from.
   Today the only join key is the PID itself, which is meaningless after the
   pod restarts.

Both problems block the production use case the user explicitly called out:
*"in a cluster we need to profile a pod but also a container inside the pod
— always the same one if possible."*

## Goals

1. `--pid <N>` works correctly when the agent runs in a non-host PID
   namespace. The user passes the PID they see; perf-agent translates
   internally via `/proc/<N>/status`'s `NSpid:` line. Zero CLI changes.
2. Output pprof samples carry **k8s identity labels** parsed from the
   target's cgroup path and (optionally) downward-API environment
   variables. No external API calls, no kubelet integration, no k8s client
   library.
3. Same behaviour available via the library API (`perfagent.New(...)`)
   through composable `Option`s. Library callers can override the defaults.
4. Behaviour degrades cleanly outside k8s: if `/proc/<pid>/cgroup` doesn't
   match the kubepods pattern, no k8s labels are added; perf-agent works
   exactly as today on bare metal / docker / podman.

## Non-goals

- **BPF-side cgroup-id allowlist filtering.** Today's BPF PID filter is
  sufficient when paired with namespace translation. Cgroup-id allowlists
  require map sync, watchers, and add complexity for marginal benefit on a
  single-PID target. Out of scope for v1.
- **Full k8s API / kubelet / CRI integration.** This is the OTel /
  Pyroscope / Parca path the project is explicitly rejecting. We do not
  ship `client-go`, do not open a watch on the API server, do not talk to
  `containerd.sock`. Everything is derived from `/proc` and process env.
- **DaemonSet-style "label every sample on the node by cgroup-id."** That's
  a separate, larger feature (BPF-side per-sample cgroup-id lookup, target
  inventory map, etc.) and a different operational model. Today's spec
  labels only the **single targeted PID's** samples.
- **Cgroup v1 support.** The kubepods cgroup v1 layout differs and
  `bpf_get_current_cgroup_id()` doesn't work on v1; modern clusters are v2
  by default since k8s 1.25 (Aug 2022). Modern distros (Ubuntu 22.04+,
  Fedora 31+, RHEL 9+) are v2-only. Documenting v2-as-required is one less
  code path.
- **`--tid` flag.** Per-thread targeting is exotic in production. The
  existing BPF filter keys on TGID, so all threads of the targeted process
  are already captured.
- **Watcher / always-on / scrape-server modes.** Single-shot CLI per
  existing semantics; library callers compose long-running behaviour
  themselves.

## Architecture

### Component map (where the new code lives)

```
internal/nspid/         ← NEW: namespace-aware PID translation.
  nspid.go              ← Translate(pidInOurView) -> (hostPID, error)
  nspid_test.go         ← /proc fixtures: single-ns, shared-pidns, missing-ns

internal/k8slabels/     ← NEW: cgroup parse + env-var read for k8s labels.
  k8slabels.go          ← FromPID(hostPID) -> map[string]string
  cgroup_parse.go       ← path → {pod_uid, container_id} extractors
  env.go                ← downward-API env-var reader
  *_test.go             ← table-tests for containerd/crio/docker variants

perfagent/options.go    ← TOUCHED: WithLabels, WithLabelEnricher.
perfagent/agent.go      ← TOUCHED: wire NSpid translation + label collection.
pprof/pprof.go          ← TOUCHED: merge static labels onto every emitted sample.
```

The two new packages live under `internal/` so the API surface is private to
perf-agent. Library callers consume them indirectly through the existing
`perfagent.Option` pattern; nothing leaks.

### Namespace translation

When the agent receives a PID (CLI `--pid N` or library `WithPID(N)`):

1. Read `/proc/<N>/status` from the agent's own /proc view.
2. Parse the `NSpid:` line: a tab-separated list of PIDs across namespaces,
   ordered **outermost (host) first → innermost last**.
3. Use the **first** column as the host kernel PID for all BPF setup
   (`profile.NewProfiler.Pids`, `dwarfagent.AddPID`, etc.).

Edge cases:

- `NSpid: 12345` (one column) → agent is in host PID namespace; N is already
  the host PID. No translation, no error.
- `/proc/<N>/status` missing → process exited (or never visible from this
  namespace). Hard error: `pid <N> not visible from agent namespace
  (process exited, or shareProcessNamespace not enabled?)`.
- `NSpid:` line missing entirely → kernel too old (pre-4.1) or non-Linux.
  Hard error with explicit "kernel too old or no PID namespace support".
  No silent fallback.

The translation runs once at startup. The hostPID is stored on the agent
struct and used everywhere downstream.

### k8s labels — what gets attached

Two layers of labels, both opt-in via the library hook system:

**Layer 1 — derived from the target's cgroup (default).**
Read once at startup from `/proc/<hostPID>/cgroup`. The file may have one
line (pure v2) or multiple (legacy hybrid mode). The parser scans for the
**v2 line specifically** — the one starting `0::` — and ignores all
others. If no `0::` line is found, the system is cgroup-v1-only; no k8s
labels are added (this is one of the documented non-goal cases). From the
v2 path:

| Label         | Source                                             | Always set?              |
|---------------|----------------------------------------------------|--------------------------|
| `cgroup_path` | full path string from `/proc/<hostPID>/cgroup`     | yes (when /proc readable) |
| `pod_uid`     | `pod<UID>` segment, normalized                     | only if kubepods matched |
| `container_id`| leaf `cri-containerd-<id>` / `crio-<id>` / `docker-<id>`, suffix stripped | only if a known runtime prefix matched |

If the cgroup path doesn't contain `kubepods` anywhere, **no k8s labels
beyond `cgroup_path` are added**. perf-agent works as today on hosts that
aren't k8s nodes.

`cgroup_id` (the inode) is *available* through the parser but **not** added
to pprof labels by default — it's locally meaningful (matches
`bpf_get_current_cgroup_id()`) but not portable across nodes. Library
callers who want it can pull it themselves via `WithLabelEnricher`.

**Layer 2 — downward-API env vars (default, best-effort).**
If the agent's process environment contains the canonical k8s downward-API
names, attach them:

| Label            | Env var          | Set when                |
|------------------|------------------|-------------------------|
| `pod_name`       | `POD_NAME`       | env var present + non-empty |
| `namespace`      | `POD_NAMESPACE`  | env var present + non-empty |
| `container_name` | `CONTAINER_NAME` | env var present + non-empty |

These cost three `os.Getenv` calls and zero error paths. If absent (host CLI
use, library calls without those vars set), they're silently skipped — no
warning, no error.

**No k8s API call**, **no kubelet read**, **no container runtime socket**.
Pod_name/namespace come from the env or they don't come at all. Consumers
who need richer metadata (pod labels, owner refs, deployment name) can join
later in their analysis pipeline using `pod_uid` as the foreign key.

### Library API

Two new options on `perfagent`:

```go
// WithLabels attaches static labels to every emitted sample. Merged on top
// of any built-in defaults. Useful for service identity (e.g. service=foo,
// version=1.2.3) that the caller controls.
func WithLabels(labels map[string]string) Option

// WithLabelEnricher overrides the built-in label derivation. Called once
// at agent startup with the resolved host PID; the returned map is merged
// with any WithLabels values (WithLabels wins on key collision). Pass nil
// (or a function returning nil) to disable all default labels including
// the cgroup-derived ones.
func WithLabelEnricher(fn func(hostPID int) map[string]string) Option
```

Defaults (when neither option is provided):

- Built-in enricher = `internal/k8slabels.FromPID`. Returns layer-1
  + layer-2 labels described above.

Override semantics:

- `WithLabelEnricher(custom)` replaces the default enricher entirely.
  Caller is responsible for everything they want labelled.
- `WithLabelEnricher(nil)` disables the enricher; only `WithLabels(...)`
  static labels are attached.
- `WithLabels(...)` adds caller-controlled static labels and **wins** on key
  collision with enricher output. Lets callers override (e.g. force
  `namespace=other-ns` for testing).

Final label set is computed once at agent startup and stored on the agent
struct. There's no per-sample callback in v1 — the labels are static for the
life of the run since the target is one PID.

### CLI surface

**No new CLI flag.** Behaviour is automatic:

- `perf-agent --profile --pid 5 --duration 30s` from inside a pod → NSpid
  translation runs, k8s labels derived, profile written. Looks identical to
  today's invocation; produces a richer pprof.
- `perf-agent --profile --pid 12345 --duration 30s` on a bare-metal host →
  `NSpid` has one column (no translation), cgroup path doesn't match
  kubepods, no k8s labels added. Behaviour identical to today.
- `perf-agent --profile -a --duration 30s` (system-wide) → no PID,
  translation skipped, no per-target k8s labels. Behaviour unchanged.

The CLI uses the library defaults — the library/CLI parity is automatic.

## Data flow

```
CLI flag --pid 5         OR        Library: perfagent.New(WithPID(5))
       ↓                                    ↓
       └──────────────┬─────────────────────┘
                      ↓
         internal/nspid.Translate(5)
                      ↓
              hostPID = 12345
                      ↓
              ┌───────┴───────┐
              ↓               ↓
   profile.NewProfiler   internal/k8slabels.FromPID(12345)
   (BPF PID filter set            ↓
    to host PID 12345)    {pod_uid, container_id, cgroup_path, ...}
                                  ↓
                       merge with WithLabels static map
                                  ↓
                            staticLabels
                                  ↓
                    each pprof.Sample emitted →
                    sample.Label = staticLabels + per-sample dynamic labels
```

## Error handling

| Failure mode                          | Behaviour                                                  |
|---------------------------------------|------------------------------------------------------------|
| `/proc/<N>/status` missing            | Hard error at startup: "pid not visible".                 |
| `NSpid:` line missing in status       | Hard error: "kernel doesn't expose PID namespaces".        |
| `/proc/<hostPID>/cgroup` unreadable   | Warn at info level; continue with no k8s labels.           |
| No `0::` line in cgroup file (v1-only host) | Warn at info level; no k8s labels added.            |
| Cgroup path is not a kubepods layout  | Silently skip k8s labels; `cgroup_path` still set.         |
| Env vars empty / unset                | Silently skip; no labels for those keys.                   |
| `WithLabelEnricher` returns nil/error | Treat as "no labels"; only `WithLabels` static set applied. |

## Testing strategy

- **`internal/nspid` unit tests** (no root needed): synthetic /proc/<pid>/status
  fixtures covering host-ns, shared pidns, missing process, and missing
  NSpid line.
- **`internal/k8slabels` unit tests** (no root needed): table-driven on
  cgroup path strings — containerd, criO, docker, kubelet-direct, non-k8s
  paths. Cover the regex extractors fully.
- **Label-merging unit tests** in `perfagent`: verify enricher ↔ WithLabels
  precedence, nil-enricher disabling, override semantics.
- **Integration test** (in existing `test/` module, root-gated as today):
  launch a Python workload, run `perf-agent --pid <PID> --duration 5s`,
  parse the resulting pprof and assert at least one sample has the expected
  `cgroup_path` label. Skip in non-k8s environments — guard with a check
  that `/proc/self/cgroup` contains `kubepods`. (CI runners are not in
  pods, so this test will skip on GitHub Actions; gates correctly under
  `kind` or any kubelet-managed env.)

## Open questions

1. **Numeric labels via `pprof.NumLabel`?** The `pprof.Sample.NumLabel` field
   carries int64 maps for numeric grouping (e.g. `cgroup_id` as a number,
   not a hex string). For v1 we use only string labels; revisit if a
   consumer surfaces a need for numeric grouping. Decision: **strings
   only**, document the convention in the package comment.

2. **What if multiple `--pid` are eventually allowed?** Today only one is
   accepted. If we extend to a list later, the label-derivation has to
   become per-PID. The architecture in this spec accommodates that (the
   enricher is a function of hostPID), so the only change later would be
   storing a `map[hostPID]labelSet` and looking up at sample emission. Out
   of scope for v1; flagging it so we don't paint into a corner.
