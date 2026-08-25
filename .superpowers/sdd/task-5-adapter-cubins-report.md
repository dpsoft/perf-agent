# Task 5 — The adapter captures cubins

Branch `feat/adapter-captures-cubins`, one commit, rebased onto `origin/main`
(which now carries Task 1's `internal/cubin` and Task 2's ABI additions) with
Task 3's transport commit beneath it. Verified offline: `CapEff: 0`, no GPU.
Everything Task 5 can prove without hardware is proven; the three things the
plan defers to the RTX 3090 are stated as outstanding at the end, and a fourth
that this implementation introduces is stated with them.

## What was built

| file | what |
| --- | --- |
| `shim/core/cubinqueue.h` | `CubinView` — the non-owning view whose pointer cannot escape — and `CubinQueue`, the bounded copy-now/send-later queue |
| `shim/core/cubinqueue.cc` | the copy, the CRC-over-the-copy, the intern, the bounded push, the lock-free-of-the-offer drain |
| `shim/core/cubinqueue_test.cc` | 13 runtime assertions, including the one that fails if the copy is deferred |
| `shim/core/cubin_defer_test.cc` | the compile-time assertion: five deferrals that must not compile, plus a control that must |
| `shim/nvidia/cupti_adapter.cc` | `CUPTI_CBID_RESOURCE_MODULE_LOADED` is read; `cuptiGetCubinCrc()`; `gpu_module_load_v1`; the drain-tick offer; nine counters in the stats dump |
| `shim/stub/stub.cc` | the fake module-load path: `PERFAGENT_STUB_CUBINS`, the same `CubinQueue`, the same probe, the real cubin channel |
| `gpuprobe/cubin_test.go` | four Go tests driving the stub's module path over the real Task 3 listener |
| `shim/Makefile` | `core/cubinqueue.cc` into `CORE_SRC`; `cubinqueue_test` into `test` and `test-tsan`; the new `check-cubin-defer` target, which `test` depends on |

**Task 3's transport and the enrolment path are unchanged in substance.**
`shim/core/cubin.{h,cc}`, `gpuprobe/cubin.go`, `gpuprobe/enroll.go` and
`shim/core/enroll.{h,cc}` have no production edits. What Task 5 uses, it uses by
calling: `cubin_offer_to_consumer`, `cubin_timeout_ms`, `cubin_self_name`,
`cubins_offered()`, `cubins_send_failed()`. `CubinOfferFn` is deliberately
signature-compatible with `cubin_offer_to_consumer`, so the production wiring is
a name and the tests are a substitute.

One change to a Task 3 **test helper** is flagged in its own section below.

## The capture path

`cubin.h` states the rule as a contract — *"Nothing in this header may be called
from a CUPTI callback. The MODULE_LOADED callback's job is the memcpy … the
offer belongs to the drain thread"* — and this task implements exactly that
split.

**In the callback** (`on_module_loaded`, on the application's `cuModuleLoad`
thread):

1. reject a descriptor with no bytes → `g_module_no_bytes`
2. gate: is a consumer believed present → `g_module_unattached`
3. reject a cubin over the per-cubin ceiling **before the memcpy** → `cubin_too_large`
4. `malloc` + **`memcpy`** — the copy, with nothing deferred ahead of it
5. `cuptiGetCubinCrc()` **over the copy** → zero means "could not" → `cubin_crc_failed`
6. intern by CRC; a re-load is skipped → `module_reload_skipped`
7. **fire `gpu_module_load_v1`**, `bytes_ptr` = the copy
8. push to the bounded queue, or drop the *offer* → `cubin_queue_full`

**On the drain thread** (the 100 ms `on_tick` the adapter already owns, plus one
bounded pass in the `atexit` handler): pop, `cubin_offer_to_consumer`, free.
`drain()` never holds the queue mutex across an offer — doing so would hand the
offer's whole 2 s timeout to the application's next module load, which is the
cost the split exists to avoid. `test_a_slow_offer_does_not_block_a_capture`
asserts a capture completes while an offer is wedged, and TSan proves the
absence of the race rather than the counters agreeing by luck.

**Step 7's position is load-bearing, not stylistic.** The record is emitted
*after* the CRC and *before* the push, which is the only instant at which the
copy is owned by nobody else: after the push the drain thread may offer and free
it, and `bytes_ptr` would name freed memory. `bytes_ptr` stays in the ABI and
stays accurate; it is still not a transport, because reading it from the agent
needs `CAP_SYS_PTRACE`.

**Why the gate is not the probe semaphore alone.** `capture_enabled()` is
`g_consumer_enrolled || gpu_module_load_v1_enabled()`. Issue #49 measured that
the semaphore reads **zero** at `InitializeInjection` on the RTX 3090 across
three runs while four thousand probes fired later in the same process — it
answers "has the kernel told this process yet", not "is a consumer attached".
CUDA's lazy loading can put the first `MODULE_LOADED` very close to that moment,
and a module missed there is missed *permanently*: there is no "copy it later".
So the gate is the one #49's second fix settled on — the rendezvous `connect()`
succeeded — with the semaphore as a second chance. An unprofiled process reads
false on both and copies nothing.

## The compile error that proves the deferral is impossible

`CubinView` has no copy constructor, no move constructor, no assignment, no
default constructor, a deleted `operator new`, and **no accessor for its
pointer at all**. `copy_to(dst, cap)` is the only member that can reach
`bytes_`, and it writes into a buffer the caller already owns. The consequence
is that the vendor's pointer has exactly one consumer — the memcpy — and cannot
reach any structure that outlives the callback.

`make -C shim check-cubin-defer` runs as a prerequisite of `make -C shim test`.
It compiles `core/cubin_defer_test.cc` six times and fails the build if any of
the five violations compiles, or if the control does not:

```
check-cubin-defer: OK - the compliant capture compiles, all 5 deferrals do not
```

The five, with the errors GCC 16 actually produced:

| mode | the deferral attempted | `c++ -std=c++17 -Wall -Werror -I core -fsyntax-only` |
| --- | --- | --- |
| 1 | store the view in a struct that outlives the callback | `error: use of deleted function ‘perfagent::CubinView::CubinView(const perfagent::CubinView&)’` |
| 2 | heap-allocate the view and keep the address | `error: use of deleted function ‘static void* perfagent::CubinView::operator new(size_t)’` |
| 3 | move the view into a `std::vector` | `error: use of deleted function ‘perfagent::CubinView::CubinView(perfagent::CubinView&&)’` |
| 4 | read the borrowed pointer straight out | `error: ‘const class perfagent::CubinView’ has no member named ‘bytes’; did you mean ‘const void* perfagent::CubinView::bytes_’? (not accessible from this context)` |
| 5 | keep a long-lived view slot and re-point it | `error: use of deleted function ‘perfagent::CubinView& perfagent::CubinView::operator=(perfagent::CubinView&&)’` |

**Mode 0 is the control and it must compile.** Without it the whole check would
pass just as happily on a file broken for an unrelated reason — a typo, a
renamed header — and would be proving nothing at all, which is the exact shape
of the eleven counters this project has shipped reading green at the worst
possible moment. The Makefile prints a distinct diagnostic for a failing control
and for each compiling violation, naming spec §6.3 finding 2 as the reason.

The runtime companion is `test_the_copy_is_taken_inside_the_callback`: it
captures 4,096 bytes, then **overwrites the source buffer** exactly as CUPTI's
does after `cuModuleUnload`, then drains — and asserts the offered bytes are the
original. A deferred copy passes every other assertion in that file and fails
this one.

## The stub's fake module-load path

`PERFAGENT_STUB_CUBINS` is a `:`-separated list of paths, in the shape `PATH`
itself uses. For each, the stub reads the file, builds a `CubinView` over it,
and runs it through **the same `perfagent::CubinQueue` the CUPTI adapter runs**
— same capture, same crc-over-the-copy, same `gpu_module_load_v1` fired from
inside `capture()`, same offer on the drain thread. Modules are captured before
the launch loop, as they are in a CUDA process, and the stub waits (bounded at
5 s) for the queue to empty so a gate can assert on a module the consumer
provably already holds.

The fixtures are Task 1's, reused rather than reinvented, so the same bytes are
exercised by the reader's tests, the module store's, and the transport's:

```
$ PERFAGENT_STUB_CUBINS=…/single_lineinfo.cubin:…/single_nolineinfo.cubin \
    shim/perfagent-gpu-stub 0 0 8 0
stub: module id=1 path=…/single_lineinfo.cubin size=4840 crc=0x9d57accad01046eb captured=yes
stub: module id=2 path=…/single_nolineinfo.cubin size=3240 crc=0x4db8f7580a182c2e captured=yes
stub: cubins requested=2 captured=2 reload_skipped=0 queue_full=0 too_large=0 crc_failed=0
      alloc_failed=0 sent=0 send_failed=2 pending=0 offered=2 transport_send_failed=0
      timeout_ms=2000 cubin_addr=@perfagent-gpu-cubin.v1.0.34.2089417
```

(`send_failed=2` above is a run with no listener bound — the honest reading of
"a consumer will not have these bytes". With the Go listener up it is 0 and
`sent=2`.)

**The CRC is a documented stand-in and is not `cuptiGetCubinCrc()`.** CUPTI's
polynomial is unpublished and there is no CUDA toolkit on the consumer's path.
What the join actually requires is that one number names one set of bytes and
that the *same* number reaches both ends; the stub declares FNV-1a, spelled once
in `stub.cc` and independently a second time in Go (`stubCubinCRC`), so "the key
the consumer stored is the key the producer meant" is an assertion rather than a
read-back.

**What this now drives end to end with no GPU:**

- **Task 3** — the real sealed-memfd offer over the real abstract socket, from a
  real producer binary, with the address derived independently at both ends;
- **Task 4** — a real `-lineinfo` cubin and a real no-lineinfo cubin arrive at
  the store, which is what makes more than one `gpu_src_status` value reachable
  from one producer run;
- **Task 7** — `gpu_module_load_v1` on the wire from a producer, so `kindModule`
  has something to decode;
- **Task 9** — a module whose line table resolves, reached from a producer
  rather than from a hand-built fixture.

Four Go tests, all passing without capabilities:

```
--- PASS: TestTheStubsFakeModuleLoadDeliversACheckedInCubin
--- PASS: TestTheStubDeliversSeveralModulesInOneRun
--- PASS: TestTheStubDoesNotReofferAReloadedModule
--- PASS: TestAStubRunWithNoModulesTouchesTheCubinChannelAtAll
```

The last one is the zero-direction assertion: a run that asked for no modules
must read zero on every producer counter *and* leave `mapped`, `received` and
`bytes` at zero on the consumer.

## The counters

The plan names four. Four are not enough, because three further paths drop a
module and one path drops it before the queue exists — and a drop with no
counter is the failure this project has shipped eleven times. All nine are
printed by the adapter's `report()` and by the stub's summary line.

| counter | where | drops what | healthy run |
| --- | --- | --- | --- |
| **`g_modules_captured`** | `CubinQueue` | — (the success count) | = distinct modules |
| **`g_module_reload_skipped`** | `CubinQueue` | a re-offer of a CRC already sent | 0 (non-zero is normal under lazy loading) |
| **`g_cubin_queue_full`** | `CubinQueue` | the OFFER, past `max_entries` or `max_queued_bytes` | **0** |
| **`g_cubin_send_failed`** | `CubinQueue` | an offer that did not end `accepted` | **0** |
| `g_cubin_too_large` | `CubinQueue` | a cubin over the per-cubin ceiling, **before the memcpy** | **0** |
| `g_cubin_crc_failed` | `CubinQueue` | a cubin whose CRC came back zero | **0** |
| `g_cubin_alloc_failed` | `CubinQueue` | the copy itself could not be allocated | **0** |
| `g_module_unattached` | adapter | a `MODULE_LOADED` declined because no consumer was believed present | **0** while attached |
| `g_module_no_bytes` | adapter | a `MODULE_LOADED` whose descriptor carried no cubin | **0** |

Plus `cubins_sent` (accepted offers) and `cubin_queue_depth`, and Task 3's own
`cubins_offered` / `cubins_send_failed` reprinted beside them.

`g_cubin_send_failed` is deliberately **broader** than Task 3's
`cubins_send_failed()`: that one excludes "nobody was listening", because an
unprofiled process must not accumulate failures. The queue only ever holds bytes
when a consumer was believed present, so here a no-listener result *is* a module
the consumer will not have, and it is counted.

`test_a_healthy_run_reads_zero_on_every_drop_counter` asserts all seven queue
counters at zero on a clean run, and each of them at an exact non-zero value in
its own case. A counter that cannot go non-zero in a test is not a counter.

Two deliberate readings, on the record rather than buried:

- **The queued-bytes ceiling counts as `cubin_queue_full`, not as a tenth
  counter.** A queue full by bytes and a queue full by entries are the same
  drop with the same consequence and the same operator action; both are tested
  separately.
- **A queue-full drop does not intern the CRC.** A later re-load of that module
  gets another chance at a queue that may since have drained. Interning it would
  make one moment of backpressure permanent for that module.

## The one change outside this task

`gpuprobe/cubin_test.go`'s `offerCubinFDs` helper now tolerates `EPIPE` /
`ECONNRESET` on its `WriteMsgUnix` instead of `require.NoError`-ing on it.

This is **pre-existing and not caused by anything here**:

```
$ git worktree add --detach /tmp/pa-base origin/feat/cubin-transport
$ cd /tmp/pa-base && go test ./gpuprobe/ -run TestAPerPIDConsumerRefusesCubinsFromEveryOtherPID -count=20
--- FAIL: TestAPerPIDConsumerRefusesCubinsFromEveryOtherPID
    Error: Received unexpected error: write unix @: sendmsg: broken pipe
FAIL
```

The listener decides unauthorized and throttled offers **from the peer's
credentials alone, without ever reading** — which is Task 3's design and is
correct — so it can reply `'X'` and close before the test's write lands. The
status byte is still queued on the test's end and the read below returns it, so
failing on the write makes every reject-before-read case a coin toss on how the
two ends interleave. On this machine (6.19.10) it loses reproducibly. The
transport is untouched; only the test's write-error handling changed, and the
assertion on the `'X'` reply is unchanged. Flagged here so Task 3's owner sees
it rather than discovering it as a conflict.

## Verification run

```
make -C shim                        exit 0
make -C shim test                   exit 0   (check-cubin-defer OK, cubinqueue_test OK)
make -C shim test-tsan              exit 0   (cubinqueue_test under ThreadSanitizer)
make -C shim check-fpless           exit 0
make -C shim nvidia                 exit 0   (CUDA 13.3; the adapter's compile check)
go build ./... && go vet ./...      clean
go test ./gpuprobe/ ./gpu/ ./internal/... -count=1
                                    ok gpuprobe 4.4s, gpu 3.7s, 9 internal pkgs
go test ./gpuprobe/ -race -count=4  ok 18.2s
~/go/bin/golangci-lint run          0 issues
```

`readelf -n libperfagent-gpu-nvidia.so` and `libperfagent-gpu-fpless.so` both
carry the `gpu_module_load_v1` note, so the probe the consumer's cookie expects
exists in both producers.

`shim/core/cubinqueue_test.cc`, all green:

```
  copy-in-the-callback: 4096 bytes survive the vendor overwriting its buffer
  gpu_module_load_v1 fires inside capture, over the owned copy
  the CRC is computed over the copy, not over the borrowed buffer
  a zero CRC is refused and counted: crc_failed=1
  a re-load of one CRC offers once: captured=1 reload_skipped=1
  a full queue drops 3 offers, counts them, and never blocks
  the queued-bytes ceiling is the same drop with the same counter
  a cubin over the per-cubin ceiling is refused before the memcpy
  a refused offer counts send_failed=1 and is not retried
  drain empties the queue, is bounded, and passes the timeout through
  a capture completes while a slow offer is in flight (no lock held across it)
  healthy run: captured=4 sent=4 and every drop counter at zero
  destroying a non-empty queue releases its copies
cubinqueue_test: OK
```

The existing gate is unaffected: it runs the stub **without**
`PERFAGENT_STUB_CUBINS`, so no module record fires and its
`assert.Zero(t, stats.Undecoded)` still holds. Task 7 decodes `kindModule`
before the gate starts emitting them.

## Cannot verify — outstanding on the RTX 3090

The implementer has `CapEff: 0` and no GPU. The plan defers three things from
this task to hardware; a fourth is introduced by this implementation's gating
choice and belongs on the same list.

1. **That `MODULE_LOADED` fires at all for the test workload, and how often.**
   Nothing here has seen a real CUPTI resource callback. CUDA's lazy module
   loading may put the first one much later than expected — possibly after the
   first launch — and `g_resource_events[6]` beside `g_modules_captured` is what
   answers it on the first run.

2. **That a `cuModuleUnload` after capture leaves our copy's recomputed CRC
   unchanged** — the direct test of §6.3 finding 2. The offline test simulates
   the hazard by overwriting the source buffer; only hardware shows that CUPTI's
   buffer really does change under us and that the copy really is taken early
   enough to miss it.

3. **That `cuptiGetCubinCrc()` over the copy matches the PC records'
   `cubinCrc`.** This is the most consequential unknown in the phase and it is
   load-bearing beyond this task. Task 3's report already notes that the agent
   **cannot recompute the CRC** — no CUDA toolkit — and therefore trusts the
   offer header's value as the join key. If the two numbers disagree on
   hardware, **every Tier B PC sample resolves `no-module` with every counter on
   both sides reading green.** There is no offline experiment that can move this
   one; it must be the first thing the first GPU run prints.

4. **That `capture_enabled()` reads true by the time the first `MODULE_LOADED`
   arrives.** This is new with Task 5. The gate is `enroll == confirmed ||
   semaphore != 0`, chosen precisely because issue #49 measured the semaphore
   reading zero at `InitializeInjection`. If the rendezvous is *also* not
   confirmed at that moment in some configuration, the first module is declined
   and — because there is no "copy it later" — permanently unresolvable.
   `g_module_unattached` is the counter that makes it visible; it must read
   zero on a profiled run, and a non-zero value there is the signal to widen
   the gate rather than to widen the tolerance.

Also untested here, by design: the CUPTI adapter has **only** a compile check
and the escape-analysis assertion, exactly as the plan says it can. No `CUpti_`
code path in this commit has ever executed.
