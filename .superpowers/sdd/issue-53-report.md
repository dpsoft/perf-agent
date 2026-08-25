# Issue #53 — GPU samples carried no process identity, and `gpu_correlation` was ambiguous across processes

Branch `feat/gpu-pid-label`, worktree `.worktrees/gpu-pid-label`, cut from `origin/main`
(`f5764f99`). One commit. Not pushed, no PR.

Scope: what reaches the profile. No join behaviour, no counters, nothing in `gpuprobe/`
changed. The only production file touched is `gpu/projection.go`.

---

## 1. The `gpu_correlation` decision

**Decision: `gpu_correlation` keeps its `backend:value` format unchanged. The process
identity goes into a new, separate `gpu_pid` label.**

### 1.1 The three candidates

| | Existing filters | Cardinality | Ergonomics |
|---|---|---|---|
| (i) redefine as `backend:pid:value` | **break, silently** | +0 strings | one-label filter works |
| (ii) keep format, add `gpu_pid` | **unchanged** | +1 key, +1 value *per process* | two-label filter |
| (iii) keep format, add `gpu_correlation_key=backend:pid:value` | unchanged | +1 value *per execution* | one-label filter |

### 1.2 Why (i) loses

The failure mode is not "the filter breaks", it is **how** it breaks. A reader with
`pprof -tagfocus gpu_correlation=cupti:4294967301` in a runbook or a script does not get
an error under (i) — the label still exists, the value simply never matches. pprof renders
an empty profile, and an empty profile reads as *"there was no such GPU work"*, not
*"your filter is stale"*. That is the same silent-wrong-answer class spec §4 exists to
forbid, exported to the consumer side.

Under (ii) the same filter matches exactly what it matched before. In a system-wide
profile it may match samples from more than one process — but it did that before this
change too, and now every sample it returns carries a `gpu_pid` that says which. The
ambiguity becomes **visible and resolvable** instead of being replaced by a silent empty.

Two smaller arguments against (i):

- The label's own doc comment (added in #33 review, still in the file) is emphatic that
  `gpu_correlation` reports *what was actually observed on the execution* — the vendor's
  id under the vendor's backend — rather than anything this package inferred or assembled.
  A pid is a fact about the observer, not part of the vendor identifier. Folding it in
  turns an observation label into a composite.
- One fact per label is the established shape here. `pod_uid` and `container_id` are two
  labels, not one `pod:container` string, and `gpu_queue`/`gpu_device` are split the same
  way even though a queue is only meaningful within a device.

### 1.3 Why (iii) loses — cardinality

This is the one option that actually costs something measurable. Measured on the real
profile: `gpu_correlation` has **4000 distinct values across 4000 samples** — it is
already, by a wide margin, the highest-cardinality label in a GPU profile, roughly one
distinct string per execution. A parallel fully-qualified copy would add a second
unique-per-execution string and roughly double the string-table contribution of the
correlation dimension, to buy back only single-invocation `-tagfocus` convenience.

`gpu_pid` costs one key string plus **one value string per distinct process** — bounded
by the process count, not by executions or PC samples. Measured: see §4.

### 1.4 What a reader with an existing filter experiences

- **Single-process profile** (`--pid`, the common case): nothing changes. The filter
  matches the same samples; they now additionally carry `gpu_pid=<the one pid>`.
- **System-wide profile**: the filter matches the same samples it always did. Before, the
  reader had no way to see that two processes were in the result. Now `go tool pprof
  -tags` shows the `gpu_pid` breakdown, and `-tagfocus gpu_correlation=cupti:N
  -tagignore gpu_pid=<other>` narrows it in one invocation.
- **Nothing breaks anywhere.** Nothing in the repo parses `gpu_correlation`'s value —
  grep for the string across the whole `origin/main` tree (`gpu/`, `gpuprobe/`, `cmd/`,
  `test/`, `bench/`, `docs/`, `.superpowers/`) turns up exactly four hits: the producer (`gpu/projection.go`), one test asserting the
  literal `"cupti:a"` (`gpu/projection_test.go:99`, an anti-forgery assertion), the Phase 2
  plan's label inventory, and the #36 report's forward reference to this issue. There is
  no in-repo consumer of the format at all; the compatibility surface is entirely external
  (user `-tagfocus` filters and downstream tooling), which is precisely why the silent
  failure mode of (i) is the deciding factor — those consumers cannot be found and fixed.

The trade-off I am accepting: a reader who wants "this one execution in this one process"
needs two labels rather than one, and pprof takes a single `-tagfocus`. I judge that a
smaller cost than silently invalidating every existing filter, especially since
`gpu_correlation` is near-unique per execution anyway — grouping *across* processes by it
was never a meaningful operation, only accidental collision.

---

## 2. Spec §8 grounding

I checked §8 rather than assuming. Its ruling is a two-sided list:

> - **Frames** — what should nest in the flame graph: real CPU stack, then
>   `[gpu:launch]`, then the kernel name.
> - **Labels** — what must not fragment aggregation: stall reason, PC, queue, device,
>   correlation ID, cgroup, pod UID, container ID.

Two things follow directly:

1. The frames side is **exhaustively enumerated** as three items. A `[gpu:pid:1234]` frame
   is not among them, and §8's whole argument is that context put in frames fragments
   stack identity — a per-process frame would split one call path into one leaf per
   process for a fact no flame graph nests on.
2. The labels side already contains **cgroup, pod UID and container ID** — process/workload
   identity of exactly the same kind. Process identity is metadata *about* a sample, and
   §8 puts that class of metadata in labels. `gpu_pid` is the narrowest possible member of
   a set §8 already places there.

So §8 does rule on this, by category rather than by name, and the ruling is: label.

---

## 3. What was built

### 3.1 `gpu_pid`

Set in `projectionLabels`, **after** the `maps.Copy(labels, Launch.Tags)` — the same
reserved-name protection every other `gpu_*` label has, so a producer-supplied tag named
`gpu_pid` (from `--tag` or cgroup attribution) cannot forge the process. `gpu/projection_test.go`'s
existing anti-forgery test was extended to cover it.

### 3.2 Which pid, and the existing mechanism question

I looked at whether `Launch.Tags` — which carries `pod_uid`/`container_id` in other paths —
was the right vehicle. It is not, for two reasons:

- `Tags` is **producer-supplied and never populated on the GPU path today**.
  `gpuprobe/consumer.go` builds `LaunchContext` with `PID`/`TID`/`TimeNs` and leaves `Tags`
  nil; `internal/k8slabels` feeds `perfagent`'s builder-level label map, which is a
  different (profile-wide, constant) mechanism. Routing a profiler-derived fact through the
  producer-supplied map would put it on the wrong side of the exact trust boundary
  `projectionLabels`' comment describes.
- The established way to attach *per-sample* metadata is `pp.ProfileSample.Labels`, which
  `projectionLabels` already builds. `gpu_pid` uses it. No parallel mechanism was added.

The pid itself comes from a new `labelPID`, not from `projectionPID`, because the two
answer different questions:

- `projectionPID` (→ `ProfileSample.Pid`) answers *"whose address space were these frames
  symbolized in"*. For an unmatched execution the honest answer is "none" — no launch, no
  stack, no address space — and it stays 0. **Unchanged by this commit.**
- `labelPID` (→ `gpu_pid`) answers *"which process produced this GPU work"*. It prefers the
  launch's pid so the label can never contradict the address space the frames came from,
  and falls back to `view.Exec.Correlation.PID` when there is no launch.

That fallback matters: an execution whose correlation **missed** the cache (its launch aged
out) keeps its `gpu_correlation` label and takes the `gpu_join="unmatched"` path with
`Launch == nil`. Without the fallback, exactly the samples that keep the ambiguous label
would keep the ambiguity. Since #36 put the pid inside `CorrelationID`, that pid is
probe-observed truth, not an inference, so using it fabricates nothing.

For an **exact** join the two sources are equal by construction — `(backend, pid, value)`
is the join key. For a **heuristic** join the execution supplied no correlation at all
(that is the only way it reaches the heuristic), so `Correlation.PID` is zero and the
launch's pid is the only one in play; it is an inference, and `gpu_join="heuristic"` on the
same sample already says so.

### 3.3 When the label is omitted

Zero pid → no label. That covers a correlation-less execution that matched nothing, and a
device-global producer that names no process (`CorrelationID.PID`'s documented zero case).
Emitting `gpu_pid="0"` would name pid 0 — the kernel's — as the producer of GPU work. This
is the opposite of the `gpu_join` rule (always emitted, never omitted) and deliberately so:
an absent `gpu_join` could be misread as `"exact"`, whereas an absent `gpu_pid` is not
readable as any process, and `gpu_join` on the same sample says why it is absent.

### 3.4 Single-process mode: emitted anyway

With `Config.PID != 0` every sample carries the same value. I still emit it:

- **The consumer cannot tell absence apart from absence.** A tool reading a profile cannot
  distinguish "single-process run, so the label was skipped" from "this profile has no GPU
  labels at all" or "produced by an older build". A label that is always present is
  strictly easier to consume than one that vanishes in a mode the reader cannot detect.
  This is the same argument the file already makes for `gpu_join` being unconditional.
- **The package does not know the mode.** `gpu.ProjectExecutions` takes a `Snapshot` and
  nothing else — no `Config`. Making the label conditional would mean plumbing agent mode
  into the projection layer to *remove* one bounded string.
- **The cost is one string.** Measured below.

---

## 4. Before / after label output

Both halves below are real command output, not reconstructions.

### 4.1 The real profile, as it stands today (before)

`/home/diego/gpu-cuda-45.pb.gz` — 4000 samples, RTX 3090, produced on `main`:

```
=== REAL profile /home/diego/gpu-cuda-45.pb.gz (produced on main, before) ===
samples=4000 label keys=3
  gpu_correlation:   distinct=4000   e.g. [cupti:4294967301 cupti:4294967302 cupti:4294967303 cupti:4294967304]
  gpu_join:          distinct=1      e.g. [exact]
  gpu_sample_period: distinct=1      e.g. [8]
```

No `gpu_pid`, and 4000 distinct correlation strings for 4000 samples — the cardinality
fact §1.3 rests on. The correlation values also confirm the premise: CUPTI's counter
starts at `4294967301` (`0x100000005`) in *this* process and will start there in the next
one too.

Adding `gpu_pid` to all 4000 samples of that exact profile costs **+1 key and +1 value in
the string table**, and on the wire:

```
REAL profile with gpu_pid added to all 4000 samples: 24748B -> 25144B (+396B)
```

396 gzipped bytes on 24.7 KB — 1.6%, for a profile with 4000 label values already in it.
That is the single-process worst case for "is the label worth it".

### 4.2 A two-process snapshot, before and after

The real profile is single-process, so it cannot show the collision. This is a synthetic
snapshot in the same shape — two pids whose CUPTI counters both start at `4294967301`,
same kernel, same queue — projected through the real `Timeline`, the real
`ProjectExecutions` and the real `pprof` builder. "Before" is the identical projection with
`gpu_pid` stripped.

```
=== SYNTHETIC two-process snapshot, BEFORE (gpu_pid removed) ===
samples=6 label keys=7
  gpu_correlation:   distinct=3      e.g. [cupti:4294967301 cupti:4294967302 cupti:4294967303]
  gpu_device:        distinct=1      e.g. [0]
  gpu_join:          distinct=1      e.g. [exact]
  gpu_pc:            distinct=1      e.g. [0x1a40]
  gpu_queue:         distinct=1      e.g. [s1]
  gpu_sample_period: distinct=1      e.g. [45]
  gpu_stall:         distinct=1      e.g. [long_scoreboard]

=== SYNTHETIC two-process snapshot, AFTER (this change) ===
samples=6 label keys=8
  gpu_correlation:   distinct=3      e.g. [cupti:4294967301 cupti:4294967302 cupti:4294967303]
  gpu_device:        distinct=1      e.g. [0]
  gpu_join:          distinct=1      e.g. [exact]
  gpu_pc:            distinct=1      e.g. [0x1a40]
  gpu_pid:           distinct=2      e.g. [4242 5353]
  gpu_queue:         distinct=1      e.g. [s1]
  gpu_sample_period: distinct=1      e.g. [45]
  gpu_stall:         distinct=1      e.g. [long_scoreboard]

encoded size: before=388B after=414B delta=+26B over 6 samples
```

Six samples, **three** distinct `gpu_correlation` values — that is the over-grouping #53
describes, and it is deliberately still there after the change. What changed is that the
result is now separable:

```
-- samples matching gpu_correlation=cupti:4294967301 (BEFORE) --
  value=1570ns gpu_pid=[] gpu_join=[exact] root=[gpu:kernel:vecAdd]
  value=1570ns gpu_pid=[] gpu_join=[exact] root=[gpu:kernel:vecAdd]

-- samples matching gpu_correlation=cupti:4294967301 (AFTER) --
  value=1570ns gpu_pid=[4242] gpu_join=[exact] root=[gpu:kernel:vecAdd]
  value=1570ns gpu_pid=[5353] gpu_join=[exact] root=[gpu:kernel:vecAdd]
```

Before, the two rows are indistinguishable in the output — the exact complaint in the
issue. After, they are two named processes, and the samples' *stacks* (`worker_4242` /
`worker_5353`, verified in `TestProjectionLabelsNameTheProducingProcess`) agree with the
label.

### 4.3 What `go tool pprof -tags` prints

On the current real profile it prints three blocks — `gpu_correlation` (4000 lines),
`gpu_join`, `gpu_sample_period`. After this change a fourth block appears, and in the
single-process case it is one line:

```
 gpu_pid: Total 5.99ms of 5.99ms (  100%)
          5.99ms (  100%): <the profiled pid>
```

In a two-process system-wide profile it becomes the per-process breakdown, which is the
one thing a reader currently cannot get out of a GPU profile at all:

```
 gpu_pid: Total 9.42ms of 9.42ms (  100%)
          4.71ms (50.00%): 4242
          4.71ms (50.00%): 5353
```

(The two blocks above are the shape `-tags` emits for a one-value and a two-value label,
written out from the measured totals — the `-tags` renderer itself was run only against
the real single-process profile, which is the first block minus the `gpu_pid` line.)

---

## 5. Tests

New, in `gpu/crossprocess_test.go` (where the #36 cross-process cases live):

- `TestProjectionLabelsNameTheProducingProcess` — two processes, same correlation value.
  Asserts both samples carry the **identical** `gpu_correlation` (pinning the format
  decision, so a future "fix" that folds the pid in fails loudly here), distinct `gpu_pid`,
  and that each `gpu_pid` agrees with the sample's `Pid` and with the call path in its
  frames. This is #53's projection-half regression test.
- `TestProjectionNamesTheProcessOfAnUnmatchedExecution` — correlated execution, no launch:
  `gpu_join="unmatched"`, `gpu_correlation` present, `gpu_pid="4242"` from the execution's
  own correlation, and `ProfileSample.Pid == 0` (unchanged).
- `TestProjectionOmitsPidWhenNoProcessIsKnown` — correlation-less orphan execution: no
  `gpu_pid` key at all.
- `TestProjectionEmitsPidInSingleProcessMode` — pins the deliberate choice in §3.4.

Extended: `TestProjectionReservedLabelsWinOverTags` now also feeds a `gpu_pid: "HIJACKED"`
tag and asserts the real pid wins.

Verification run in this worktree:

```
go build ./...                                        ok
go vet ./...                                          ok
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1     all ok (8 packages)
golangci-lint run --timeout=5m                        0 issues
gofmt -l gpu/                                         (empty)
```

---

## 6. Cannot verify

- **No live GPU run.** `CapEff: 0` in this environment — no BPF load, no `uprobe_multi`
  attach, no ringbuf, no CUPTI shim injection. Everything above went through the real
  `Timeline` and the real `ProjectExecutions`/`pprof` builder, but the *input* was either a
  synthetic snapshot or an already-rendered profile from an earlier privileged run.
- **The two-process RTX 3090 case has never been run.** §4.2 is the shape of it, not the
  thing itself. The specific thing to check on a real two-process run: that `gpu_pid`'s
  `-tags` breakdown has exactly as many entries as there were profiled processes, and that
  no sample carries a `gpu_pid` whose value disagrees with the process its frames name.
- **The `-tags` output in §4.3 is partly written out, not captured.** The real profile has
  no `gpu_pid` (it was produced on `main`), so I could not re-render it through the changed
  code — only add the label to the decoded profile to measure cost. The single-process
  block's *total* is the measured `5.99ms` from the real profile's own `-tags` output; the
  two-process block is from the synthetic snapshot's totals. The renderer was not run
  against a post-change profile.
- **External consumers.** The claim "no existing filter breaks" is verified for everything
  in this repository. Any out-of-tree tooling that parses `gpu_correlation` is
  unverifiable from here — which is exactly the asymmetry that decided §1: (ii) cannot
  break such a consumer, (i) could, and neither of us would find out.
