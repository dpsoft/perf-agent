# Issue #67 — a sampled stack is lost when its launch fills the batch

Branch `fix/sampled-stack-join-race`, one commit, closes #67.

## Cannot verify

**I have `CapEff: 0`. I did not run the privileged gate, and I have no GPU.**
Nothing below that describes `TestStubDrivesThePipelineToPprofWithoutAGPU`
running, or the CUDA path running, is an observation. The numbers I predict
for both are derived, and they are stated so they can be falsified by one run.

What I *did* run is in "Verification" at the end: the whole unprivileged
suite, plus a new test that reproduces the defect's mechanism with no
capabilities at all and fails against unfixed code.

## The fix

Both producers now fire the unbatched `gpu_launch_sampled_v1` probe **before**
the batched `add()`, instead of after it.

- `shim/stub/stub.cc` — `gpu_launch_sampled_v1_emit(...)` moved above
  `lb.add(l)`. The `gpu_launch_v1` record is still fully built first, so the
  sampled record still copies `l.kernel_id` from it; only the two probe fires
  swapped places. `sampler.should_sample()` remains the left operand of the
  `&&`, so the schedule is still advanced on every launch whether or not a
  consumer is attached.
- `shim/nvidia/cupti_adapter.cc` — the same move above `g_lb->add(l)`, after
  `l.correlation = correlate_launch(...)` has run, since the sampled record
  reuses that correlation.

No ABI change: `sample_period`, `launch_seq` and every record layout are
untouched, the probe set is untouched, and the consumer is untouched.

### Why this closes it rather than narrowing it

The two records are twins — same correlation, one carrying the launch, the
other carrying only the CPU stack the consumer staples onto it. The consumer
joins them in either order, but the orders are not equally safe:

- **sampled first** — the stack parks in `pendingStacks`, where nothing but
  the twin can claim it, and any number of unrelated batches may pass.
- **batched first** — the launch waits in `deferredLaunches`, and the first
  batch of any other kind releases it stackless. The stack then parks with
  nothing to join.

Batched-first was reachable because the launch record entered the batch before
the sampled probe fired: a launch that both *filled* the batch and was
*sampled* put its own batched record on the wire inside that `add()`, with the
exec batch of the same loop iteration landing between the twins.

With the probe first, batched-first is **unreachable, not rare**. A record
cannot be in a batch before `add()` puts it there, so no flush — on the
launching thread, on the drain thread, or on a CUPTI worker — can carry the
twin past a probe that has already fired. The guarantee holds without any
assumption about threading or about flush timing, which is what distinguishes
it from a narrowing.

### The four questions asked before implementing

1. **Does anything depend on the batched record being queued first?** No.
   `Consumer.applyBatch` registers a PID with the walker on *any* batch
   (`c.unwind.note(b.PID)`), sequence numbers are per-probe and unaffected by
   cross-probe order, and `attachSampledStackLocked` reaches nothing the
   launch path establishes. In practice the first record either producer emits
   was already the unbatched `gpu_kernel_name_v1`, and the launch batch does
   not flush until 32 records — so the sampled record was already ahead of the
   first launch batch on every run. This changes only which of the two twins
   leads, and only at a batch boundary.
2. **Can the sampled record reference state the batched path establishes?**
   Only within the same call: the stub's `sl.kernel_id` reads `l.kernel_id`,
   and the adapter's `s.correlation` reads `l.correlation` from
   `correlate_launch`. Both are *struct fills*, not `add()`. Both stay above
   the probe; `add()` is what moved.
3. **Sampled probe fires, process dies before the batch flushes.** Same
   exposure as before, in exactly the same set of cases: the stack parks and
   is counted in `PendingStacks`, the launch is lost with the unflushed
   partial batch, and the producer's `launch_dropped` counts it. That was
   already true for the ~31 launches that can sit in a partial batch behind
   any sampled record — the reorder does not widen the window, because the
   twin it concerns is emitted in the same `add()` that would have flushed it.
   Both producers flush on the normal exit path (`perfagent_stub_run` before
   lingering, `at_exit_handler` for the adapter); only a `SIGKILL` loses the
   partial batch, before and after.
4. **Is the same reorder correct for the CUPTI adapter?** Yes, and it is
   slightly *more* necessary there than the issue implies. In the stub the
   collision is same-thread and deterministic. In the adapter the exec batch
   is flushed by the CUPTI worker thread and by the drain timer, never by
   `on_launch`, so the interleave was a genuine thread race: `g_lb->add(l)`
   flushes a batch ending in launch N, and before the sampled probe two
   instructions later another thread flushes `g_eb`. Small window, real
   window, nothing making it impossible — which is why "unaffected in every
   run measured" is the right description of the CUDA path and "structurally
   safe" was not. Post-reorder it is structurally safe.

## What I rejected

- **Hold the deferred queue across a non-launch batch.** This is the one the
  comment in `sampledstacks.go` calls deliberate, and it is right to. In the
  stub the exec batch for correlations 1..32 follows the launch batch for
  1..32 immediately; holding launches past it means every exec is handed to
  the sink before its own launch, for 32 launches per batch, for the whole
  run. It also needs a number — hold for one batch? two? — and a kernel-name
  batch or a second exec batch arriving first defeats whatever number is
  chosen. It buys a rare attribution with a systematic delay and a heuristic.
- **Attach late stacks via `LaunchCache`.** The consumer talks to a
  `gpu.EventSink` interface and does not own a `LaunchCache` — `Timeline` does.
  Reaching it means either widening the sink interface with a mutate-in-place
  method, or re-emitting the launch (`Timeline.EmitLaunch` → `cache.Put`
  replaces on correlation, so it would technically work). Both break "one
  launch in, one launch out", which is asserted in
  `TestSampledStackArrivingFirstAttachesToTheBatchedLaunch` and relied on by
  every counting sink; re-emission also races `Snapshot`. The parent called
  this "reaching back into already-emitted state" and that is exactly what it
  is.
- **A consumer-side fix in general.** I could not find one that is lossless
  without either delaying launches or re-emitting them, and I now think none
  exists: once the launch has gone to the sink there is nothing left to attach
  to. This is why the deliverable does **not** contain the consumer-hostile-
  order test the brief asked for — such a test can only pass if the consumer
  changes, and I am arguing the consumer should not. See "Testing without
  privilege" for what I wrote instead, which tests the same mechanism and
  fails against unfixed code just as required.

`deferredLaunches` stays exactly as it is. The ABI is public, and an older or
third-party producer may still emit batched-first; for those the queue is the
difference between "usually joins" and "never joins". What it cannot be is
lossless, and `TestStackParksUnattachedWhenAnotherBatchSplitsTheTwins` now
documents that as a statement about foreign producers rather than about ours.

## Testing without privilege

`shim/stub/probe_order_test.cc` — new, wired into `make -C shim test`.

It uses the trick `core/probe_args_test.cc` already established: read the
binary's own `.note.stapsdt` to find the probe sites, patch their one-byte
nops with `int3` (which is all a uprobe does), and read `%rdi`/`%rsi` out of
the trapped context in the `SIGTRAP` handler — the same bytes
`bpf_probe_read_user` copies in `bpf/gpu_usdt.bpf.c`. No CAP_BPF, no consumer,
no GPU.

It links the real `stub/stub.cc` (with `PERFAGENT_STUB_NO_MAIN`), arms all
four semaphores, calls `perfagent_stub_run`, stamps every probe fire with a
global tick, and asserts: **for every sampled launch, the sampled record's
tick is strictly less than the tick of the launch batch carrying that same
correlation.** Two passes:

- `period=1 launches=64` — every launch is sampled, so the launch that fills
  the 32-record batch is sampled *by construction*. This pass fails on
  unfixed code on every machine, every run; it does not depend on the
  sampler's schedule.
- `period=8 launches=2048` — the shipped configuration, with enough sample
  points for the jittered schedule to land on a batch boundary the way the
  gate did. One-directional: it can detect a violation, never invent one.

Both passes refuse to pass vacuously (a full batch must have flushed, and at
least one sampled launch must have been seen).

### Pre-fix failure output (verbatim, against `origin/main`'s producer)

```
stub: launches=64 observed=64 sampled=64 period=1 seed=0x9e3779b97f4a7c15 launch_dropped=0 exec_dropped=0 enroll=disabled
period=1 launches=64: 2 of 64 sampled launches had their BATCHED record emitted before their sampled twin (first: correlation 32, batched at tick 32, sampled at tick 33).
  The consumer holds that launch in deferredLaunches, the exec batch of the same loop iteration releases it stackless, and the stack parks in pendingStacks with nothing to join (issue #67).
  Fire the sampled probe BEFORE the batched add().
stub: launches=2048 observed=2048 sampled=253 period=8 seed=0x9e3779b97f4a7c15 launch_dropped=0 exec_dropped=0 enroll=disabled
period=8 launches=2048: 7 of 253 sampled launches had their BATCHED record emitted before their sampled twin (first: correlation 32, batched at tick 4, sampled at tick 5).
  The consumer holds that launch in deferredLaunches, the exec batch of the same loop iteration releases it stackless, and the stack parks in pendingStacks with nothing to join (issue #67).
  Fire the sampled probe BEFORE the batched add().
EXIT=1
```

7 of 253 at 2048 launches is the same order as the demo run's 24 of 250 at
2000 that `sampledstacks.go` recorded, reproduced unprivileged. Note that
correlation 32 — ordinal 31, the first batch boundary — *is* in the jittered
schedule at period 8, which is precisely what the fixed stride made
impossible.

### Post-fix

```
probe_order_test: period=1 launches=64 ok - 64 sampled launches, all emitted before their batched twin; 64 launch records in 2 full batches
probe_order_test: period=8 launches=2048 ok - 253 sampled launches, all emitted before their batched twin; 2048 launch records in 64 full batches
```

### Two more guards

- `TestBothShimsFireTheSampledProbeBeforeTheBatchedAdd`
  (`gpuprobe/batch_size_test.go`) pins the emit-before-add order against the
  *source* of both shims. It is a regex over C++ and it is a weak instrument;
  it is there because the CUPTI adapter cannot be driven without a CUDA
  process and a GPU, so for that file it is the only unprivileged guard there
  is. Pre-fix output:

  ```
  --- FAIL: TestBothShimsFireTheSampledProbeBeforeTheBatchedAdd (0.00s)
      Error: "5176" is not less than "4285"
      Messages: ../shim/stub/stub.cc: the launch is added to its batch before the
      sampled probe fires. A launch that fills the batch then reaches the consumer
      ahead of its own stack, is released stackless by the next exec batch, and the
      stack parks in PendingStacks forever (issue #67)
  ```

- `TestStackSurvivesASplittingBatchWhenTheSampledRecordLeads`
  (`gpuprobe/consumer_test.go`) is the positive counterpart of
  `TestStackParksUnattachedWhenAnotherBatchSplitsTheTwins`: the same batch
  boundary and the same splitting exec batch, in the order the fixed producers
  emit. It asserts the stack attaches and `PendingStacks == 0`, in the
  consumer's own vocabulary.

## Gate changes

`gpuprobe/gate_test.go`, in `TestStubDrivesThePipelineToPprofWithoutAGPU`:

- `assert.Zero(t, stats.PendingStacks, ...)` — the assertion #67 asks for.
  Checked at rest, after `Run` has returned; `PendingStacks` is not drained by
  `Flush`, so this is exactly "a resolved stack with no launch left to join".
- `assert.Equal(t, uint64(wantSampled), stats.StacksAttached, ...)` — the same
  fact from the counter side. The existing timeline-side assertion
  (`sampledLaunches == wantSampled`, the one that fails today) is **unchanged**
  and stays.
- A `t.Logf("stack attach: ...")` printing the full identity line, so
  `58 = 57 + 0 + 0 + 1` is visible whether or not the assertions pass.

Nothing was weakened. `wantSampled` is still 58, the sampler is untouched, and
`#50` is untouched.

## Predicted gate output — derived, not observed

`sudo -E ... go test -run TestStubDrivesThePipelineToPprofWithoutAGPU ./gpuprobe/`
should print and assert:

```
stack attach: sampled=58 resolved=58 attached=58 evicted=0 profiler-only=0 pending=0 missing=0 uncorrelated=0
```

- `Stats.SampledLaunches` = **58** — unchanged; the sampler, its seed and its
  schedule are untouched, and the stub's own `sampled=58` still agrees.
- `Stats.StacksAttached` = **58** (was 57).
- `Stats.PendingStacks` = **0** (was 1).
- `sampledLaunches` counted over `snap.Executions` = **58**, so the assertion
  that fails today (`Not equal: expected 58, actual 57`) passes.

Derivation: every sampled record now precedes its batched twin, so every
resolved stack parks and every parked stack is taken by the launch batch that
follows. At most one batch-window's worth of stacks (~32/8 ≈ 4) is parked at
any instant, far under `SampledStackCapacity`, so `StacksEvicted` stays 0. The
stub flushes both batches before it lingers and the gate sleeps 500ms after
the producer exits, so every launch batch is consumed before `Stats()` is
read — nothing is legitimately still in flight.

Everything else on the gate is unchanged and should read as it does today:
`500 executions, all exact; 500 launches, all matched; cache 500 live`,
`dwarf=58 fp-only=0 reached-root=58`, both kernels represented in
`stacks per kernel`, `launch_dropped=0 exec_dropped=0 enroll=confirmed`.

**If `PendingStacks` reads non-zero after this**, the cause is not the twin
order — `probe_order_test` rules that out unprivileged — and the log line
names the remaining suspects directly.

## Predicted CUDA-path numbers — derived, not observed

The adapter change is the mirror of the stub's and can only remove a join
failure, never add one. So the nine measured runs' numbers must not move:

- `PendingStacks` = **0**, as before.
- `StacksAttached == SampledLaunches` = **505 == 505** at 4000 launches,
  period 8 (the sampler pin in `shim/core/sampler_test.cc` prints exactly
  `4000 launches at period 8 -> 505 sampled`).
- `launch_batch_dropped`, `exec_batch_dropped`, `exec_no_clock`,
  `exec_no_time`, `cupti_dropped` — unchanged; nothing on those paths was
  touched.
- Join quality, clock fit, kernel names, correlation epochs — unchanged.

The one thing that could move is timing: the uprobe trap for a sampled launch
now happens a few instructions earlier inside `on_launch`, before the batch
mutex rather than after it. That shortens the interval during which
`g_lb`'s mutex is held on the sampling thread, if anything; it does not add
work.

`make -C shim nvidia` builds clean against CUDA 13.3 here. That is a compile,
not a run.

## Verification (all run, all green)

```
make -C shim                     OK
make -C shim test                OK  (includes the new probe_order_test)
make -C shim check-fpless        OK
make -C shim nvidia              OK  (CUDA 13.3; compile only)
go build ./... && go vet ./...   OK
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1     all ok
go test ./gpu/ ./gpuprobe/ -race -count=4              all ok
golangci-lint run --timeout=5m   0 issues
```

`TestStubDrivesThePipelineToPprofWithoutAGPU` **skipped** in every one of
those runs, for want of `cap_bpf`/`cap_perfmon`/`cap_checkpoint_restore`.
