# Issue #99 — Tier A deadlocks the profiled process inside CUPTI

Branch `fix/cupti-call-serialization`, one commit on top of `origin/main` (`93cccd69`, the Task 12
merge).

The adapter called CUPTI from two independent background threads with nothing serialising them.
It now calls CUPTI from **one** background thread, and every call it makes — from that thread or
from an application thread inside one of our callbacks — goes through **one guard**. The raw CUPTI
entry points are poisoned in the adapter's translation unit, so a future call site that skips the
guard does not compile.

---

## 1. What the headers say about concurrent use

This was the first thing to establish, because it decides whether the fix is mandatory or merely
defensive. CUDA 13.3, `/usr/local/cuda-13.3/targets/x86_64-linux/include/`.

**CUPTI documents thread safety per function — and does it in three of its headers:**

| header | `\note \b Thread-safety:` occurrences |
| --- | --- |
| `cupti_events.h` | 32 |
| `cupti_callbacks.h` | 8 |
| `cupti_result.h` | 2 |
| **`cupti_activity.h`** | **0** |
| **`cupti_pcsampling.h`** | **0** |

`cupti_callbacks.h` shows the vendor is precise about it when it matters, in both directions:

> `\note \b Thread-safety: a subscriber must serialize access to cuptiGetCallbackState,
> cuptiEnableCallback, cuptiEnableDomain, and cuptiEnableAllDomains. For example, if
> cuptiGetCallbackState(sub, d, c) and cuptiEnableCallback(sub, d, c) are called concurrently,
> the results are undefined.`

So: **neither `cuptiActivityFlushAll` nor any PC-sampling entry point carries a thread-safety
guarantee of any kind.** Not "documented unsafe" — *unstated*, in a library that states it
elsewhere. Calling two of them concurrently was never sanctioned; it was untested, and on an
RTX 3090 it deadlocked.

**That makes the fix mandatory rather than defensive**, and it changes the design: there is no
documented lock ordering inside CUPTI to reason about and no subset of pairs that is known safe, so
the correct target is *never overlap*, not *overlap carefully*. That is why the fix is a single
lock plus a single thread rather than an ordering rule.

Two further things the headers say that the design uses:

- `cuptiActivityFlushAll` is **"a blocking call"** which returns buffers "using the callback
  registered in `cuptiActivityRegisterCallbacks`", and "Default flush can be done at a regular
  interval in a separate thread." A separate thread is sanctioned; a *second* separate thread doing
  something else is not mentioned either way.
- The buffer-completed callback: "After this call CUPTI relinquished ownership of the buffer and
  will not use it anymore."

**Callback-context restriction.** CUPTI does not publish a general "these entry points must not be
called from within a callback" list in these headers. The only positive statement is on
`cuptiGetDeviceId`, which is documented as behaving like `cuCtxGetDevice` "but **may** be called
from within callback functions" — phrasing that only means anything if the general case is not
assured. The adapter's own timers are not callbacks, so this does not bear on them directly; the
application thread that deadlocked *was* inside a launch callback, but that is CUPTI's own frame,
not a call we made. What the adapter does do from inside callbacks is call the PC-sampling API and
`cuptiGetCubinCrc` on `CONTEXT_CREATED` / `CONTEXT_DESTROY_STARTING` / `MODULE_UNLOAD_STARTING` /
`MODULE_LOADED` — that predates this issue, is unchanged, and is now covered by the same guard.

Nothing here is a hardware measurement. It is a reading of the shipped headers, which is a
different kind of evidence and a stronger one for this particular question.

---

## 2. The option chosen, and why

The issue offers two: give the activity flush the same mutex, or put drain and burst work on one
thread. **Both are implemented, because neither alone is sufficient, and the reasons differ.**

**One thread alone is not sufficient.** It removes the *reported* deadlock — the two threads in the
backtrace merge into one — but the adapter also calls CUPTI from the application's threads, inside
`CONTEXT_CREATED`, `CONTEXT_DESTROY_STARTING`, `MODULE_UNLOAD_STARTING` and `MODULE_LOADED`. Those
were serialised against the timers only for the PC-sampling family (`g_pc_mu`) and not at all for
`cuptiGetCubinCrc`. A single timer thread leaves an app thread and the timer thread able to be
inside CUPTI at once.

**One lock alone is not sufficient either** — or rather, it is sufficient for correctness but weak
as an argument. It excludes the bad state by discipline: every call site must remember. The bug
being fixed *is* a call site that did not remember. A lock alone would leave the same failure mode
one careless patch away.

So:

**(a) Structural half — one background thread.** The second `Drainer` is gone. One timer runs at the
shorter of the two periods (the burst tick when Tier A is on, the drain period otherwise) and
dispatches the burst poll and then, rate-limited, the activity drain (`core/tickplan.h`,
`on_timer_tick`). *The adapter now has exactly one thread of its own from which it can ever call
CUPTI.* "Two of our background threads inside CUPTI" is not a state the process can represent,
whatever a future call site does.

**(b) Discipline half — one guard over the whole vendor surface.** `g_pc_mu` is replaced by
`perfagent::cupti::guard()` (`shim/nvidia/cupti_guard.h`, built on `shim/core/callguard.h`), taken by
every CUPTI call the adapter initiates — PC sampling, activity, subscription, timestamp, cubin CRC,
result strings. One lock rather than one per subsystem, so there is **no ordering between our own
locks left to get wrong**.

**(c) The bit that makes (b) not depend on memory.** `cupti_guard.h` defines a guarded wrapper for
every CUPTI entry point the adapter uses, and then `#define`s the raw names to identifiers that
exist nowhere. From that include onward, `cuptiActivityFlushAll(0)` is a compile error naming the
wrapper to use. Since each wrapper *takes the guard itself*, there is no way to name a CUPTI
function in this translation unit without acquiring the lock.

### The claim, stated exactly

> **At most one adapter-initiated CUPTI call is in flight at any instant, process-wide.**

with one named exception (§5). This is a structural claim, not a probabilistic one: it follows from
(i) every call site acquiring a single mutual-exclusion primitive, and (ii) the impossibility of
writing a call site that does not. It is not "the window is now very small".

The reported deadlock is specifically unreachable: it required `cuptiPCSamplingStop` and
`cuptiActivityFlushAll` to be in flight simultaneously, and both halves of the fix independently
prevent that.

### What it does *not* claim

It does not claim CUPTI cannot deadlock. CUPTI invokes its own callbacks on its own worker threads
concurrently with whatever we are doing, and the adapter cannot serialise CUPTI against itself. It
claims only that the adapter no longer *creates* the concurrency, which is what issue #99 says the
cause was.

---

## 3. The lock ordering, in the code

Written at the top of `shim/nvidia/cupti_guard.h`:

> `perfagent::cupti::guard()` is the **outermost** lock in this adapter. Every other mutex it owns —
> the clock fit, the device list, `Batch`, `KernelNameTable`, `ReplayLog`, `CubinQueue`,
> `BurstController`, `PCDrainSchedule` — is a **leaf**: each is taken and released without calling
> CUPTI and without acquiring the guard, so they may all be taken while holding it and none may be
> held while acquiring it.

Checked path by path against the current source:

| path | locks, in order |
| --- | --- |
| `resample_clock` | guard (inside `GetTimestamp`), released; then `g_clock_mu` |
| `on_module_loaded` → `CubinQueue::capture` → `cupti_cubin_crc` | guard; `CubinQueue::mu_` is taken *after* the CRC returns, never across it |
| `pc_drain_ctx_locked` | guard → `Batch::mu_` (leaf) |
| `pc_query_stall_reasons` | guard → `ReplayLog::mu_` (leaf) |
| `burst_close` | `BurstController::mu_` taken and released by `poll()` first, **then** guard → `PCDrainSchedule::mu_`; guard released before `emit_window` and `g_burst->closed()` |
| `on_finalize` | `BurstController::mu_` (released), then guard via `TryCallScope` |
| `buffer_completed` | no guard; `g_clock_mu`, `g_dev_mu`, `Batch::mu_` |
| `at_exit_handler` | no guard held across `g_timer->stop()` |

With one vendor lock and every other mutex a leaf, a cycle would need two adapter locks with a
CUPTI call between them, and there is none.

The guard is **re-entrant on the owning thread**, and that is a requirement rather than a
convenience — see §4 and §6.

---

## 4. How a future call site is prevented from bypassing it

Three layers, weakest to strongest.

1. **Naming.** The only spelling of a CUPTI call in the adapter is
   `perfagent::cupti::PCSamplingStop(...)`, and the wrappers do nothing except take the guard and
   forward.

2. **The poison.** After the wrappers, `cupti_guard.h` contains 19 lines of the form

   ```c
   #define cuptiActivityFlushAll PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityFlushAll
   ```

   A call site that reaches for the raw name gets `error: use of undeclared identifier
   'PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityFlushAll'`, which names the fix.
   Only function names are poisoned; `CUpti_*` types and enumerators are untouched.

3. **`make -C shim check-cupti-guard`**, a build step and a prerequisite of `make -C shim nvidia`,
   built to the same shape as the existing `check-cubin-defer`: `nvidia/cupti_guard_test.cc`
   compiled six times.

   - **Mode 0 is the control and MUST compile.** Without it the whole check would pass just as
     happily on a file that fails to build for an unrelated reason — a typo, a CUDA version bump —
     and would then be proving nothing, which is the exact shape of the defects this project has
     shipped reading green at the worst moment.
   - Modes 1–5 must all be rejected: `cuptiActivityFlushAll` from a timer (**literally the bug**),
     `cuptiPCSamplingStop` (**the other thread in the same backtrace**), `cuptiPCSamplingGetData`,
     taking the raw function's address to call later, and a `::`-qualified raw call.

   ```
   check-cupti-guard: OK - the guarded call compiles, all 5 unguarded do not
   ```

The guard primitive itself has a unit test with no GPU: `shim/core/callguard_test.cc`, run by
`make -C shim test` and again under ThreadSanitizer by `make -C shim test-tsan`. It asserts mutual
exclusion across 8 threads × 2000 iterations (an unguarded `long` increment plus an explicit
overlap detector), three-deep re-entrancy, the two `try` refusals, and that a re-entrantly released
guard is genuinely free afterwards. **It is not vacuous:** with the exclusion removed from
`CallGuard::enter`/`leave`, the test fails.

`shim/core/tickplan_test.cc` covers the merged schedule against a fake clock: the tick period with
Tier A on and off, that the drain period is never lengthened by more than one tick over a simulated
minute, that a late tick leaves no debt firing catch-up drains, and — the case that matters most —
that the **default** configuration gets no rate limit at all (see §7).

---

## 5. The one exemption, and its boundary

`cuptiActivityGetNextRecord` and `cuptiActivityGetNumDroppedRecords`, called from inside CUPTI's
`buffer_completed` callback, are **not** guarded.

This is a reasoned boundary, not an oversight, and the reason is that guarding them would rebuild
the very deadlock: `cuptiActivityFlushAll` is documented as a blocking call that returns buffers
through that callback. If CUPTI delivers them on a worker thread while our timer thread holds the
guard across the flush, a guarded callback blocks the worker on a lock the flusher is waiting for
it to release.

The boundary is: **the guard covers calls the adapter initiates on a thread of its own choosing.
It does not cover the documented way to consume a buffer CUPTI has just handed us and, in its own
words, "relinquished ownership of."**

Because "we only call these two from that callback" is exactly the kind of claim this project has
been wrong about before, it is not left as a comment:

- the wrappers are named `ActivityGetNextRecord_InBufferCompletedOnly` and
  `ActivityGetNumDroppedRecords_InBufferCompletedOnly`;
- `buffer_completed` establishes a `BufferCompletedScope` (a thread-local flag) on entry, and both
  wrappers **count** a call made outside one;
- the count is reported as `cupti_calls_misplaced` and **must be 0**.

---

## 6. What the two preserved properties cost under the new scheme

### Property 1 — `on_finalize` must not block (and must not proceed under an in-flight call)

**Preserved, and strengthened.** It was `std::unique_lock<std::mutex>(g_pc_mu, std::try_to_lock)`;
it is now `perfagent::cupti::TryCallScope`, whose `owns()` is false when the guard is held by
**anyone, the calling thread included**.

The distinction is load-bearing now that the guard is re-entrant: a plain try from a thread that
already holds it would *succeed*, and `on_finalize` would then call `cuptiPCSamplingDisable` out
from under the CUPTI call its own outer frame is standing in. `CallGuard::try_enter_uncontended`
refuses that case and counts it separately (`cupti_try_failed_self`) from losing the race to another
thread (`cupti_try_failed_other`). Both feed `finalize_contended`, which must still be 0 on a
healthy run.

**A latent bug fixed on the way:** `std::mutex::try_lock()` from a thread that already owns the
mutex is *undefined behaviour*, and the old line was on precisely the path where that can happen —
a fatal-error callback arriving on a thread already holding `g_pc_mu`. It was UB in the code
written to avoid a deadlock on that exact path.

**Cost:** the guard is now held for more of the run than `g_pc_mu` was — it also covers the activity
flush — so the probability that a fatal-error callback finds it held is *higher* than before. The
outcome of that is unchanged (teardown skipped, tail of the profile lost, counted, logged) and it
was always the designed-for outcome. Nothing that was previously guaranteed to succeed can now fail:
the teardown was always best-effort on this path.

### Property 2 — the `MODULE_UNLOAD_STARTING` drain must block

**Preserved.** `on_resource` still calls `pc_drain_all` for `MODULE_UNLOAD_STARTING`, and
`pc_drain_all` still takes the guard **blocking**. A skipped flush there makes two instructions
share a PC identity, silently; that is still the worst outcome available and it is still not
reachable by design.

**Cost, precisely:** the guard is wider than `g_pc_mu`, so this path can now additionally wait
behind an activity flush. The application's `cuModuleUnload` therefore blocks for at most one
in-flight CUPTI call rather than at most one in-flight *PC-sampling* call. The wait was never
bounded by a small constant — it already covered a full PC drain across every context, which is
the larger of the two — so this widens a stall the design already accepted rather than introducing
one. Module unloads are tens per process.

**A cost that is genuinely new:** `cuptiGetCubinCrc` on the `MODULE_LOADED` path now takes the guard,
so the application's `cuModuleLoad` can wait behind one of the timer thread's CUPTI calls. That path
previously took no lock at all. It is accepted deliberately: the alternative is an exemption, and an
exemption is discipline — the thing that failed here. It is consistent with what the adapter already
does to `cuModuleUnload` and `cuCtxCreate`, and it is bounded by one CUPTI call.

**A hazard that gets better, not worse.** Both the Task 6 report (item 13) and the Task 10 report
(item 11) list as unverified whether `cuptiPCSamplingGetData` / `cuptiPCSamplingStop` can re-enter
our resource callback — with `g_pc_mu` that was a guaranteed self-deadlock, and widening a
non-re-entrant mutex over the activity flush would have widened it. Because `CallGuard` is
re-entrant on the owning thread, such a nested callback now proceeds instead of hanging, and every
re-entrant acquisition is counted (`cupti_reentrant`). The open question becomes a number in the
adapter's report on the next hardware run rather than a hang. *Re-entrancy here is not for
convenience — it is what keeps a vendor-delivered nested callback from being fatal, and it is
counted so it can never be invisible.*

`burst_close` also spends it deliberately: the stop and its mandatory range-end flush are now **one**
critical section, which they were not before (the old code released `g_pc_mu` between them, so
another thread's CUPTI call could land in the gap between a range ending and the flush the header
requires after it).

### Everything else preserved

The two-record window protocol (open at start with `end_ns = 0`, closed at stop with the same
`start_ns`), the CUDA-graph refusal at both moments, tier selection through one environment
variable, the cubin capture and its `CubinView` guard (`check-cubin-defer`: still OK, all five
deferrals still refuse to compile), enable-then-configure ordering, per-attribute `attributeStatus`
checking, the drop classes, the replay log, and every existing counter. No ABI change, no BPF
change, no `.o` churn, ten probes, one exported symbol.

---

## 7. Two further defects of the same family, found and fixed

**`at_exit_handler` left the drain timer running.** It stopped `g_burst_timer` and never stopped
`g_drainer`, so the drain thread's `cuptiActivityFlushAll` ran concurrently with `on_finalize`'s
`cuptiPCSamplingDisable` — *the same two-threads-in-CUPTI shape as #99, on the exit path, present in
Tier B too*. There is one timer now and it is stopped and **joined** first, so from that line on the
only thread of ours that can reach CUPTI is the exiting one.

**A regression the merge would have introduced in the default arm, caught before it shipped.** With
Tier A off, the merged timer runs at exactly the drain period. A naive "drain at most once per
period" limit on top of it would turn any tick that arrived a hair early — which sleeping timers do
routinely — into a skipped drain, pushing the next one out a whole period and roughly halving
delivery, *in the configuration that has nothing to do with this bug*. `drain_limit_ns` returns 0
("drain on every tick") whenever the tick is not shorter than the drain period, and
`tickplan_test.cc` pins it with early-arriving ticks.

`atexit` is now armed **before** the timer starts. The two-timer version got that property from its
ordering by accident; a burst opening before the exit handler exists could not be closed by it, and
an unclosed window reports a hard exit that did not happen.

---

## 8. New counters, and what each reads on a healthy run

One line in the adapter's report, emitted **whatever the tier** — the serialisation is a property of
the process, not of PC sampling:

| counter | healthy |
| --- | --- |
| `cupti_calls` | > 0 on any run that touched CUPTI |
| `cupti_reentrant` | **not** expected to be 0 — nearly every wrapper call is made inside an outer `CallScope`, and `burst_close` nests on purpose. `cupti_max_depth` is what discriminates |
| `cupti_waits` | > 0 is healthy on a busy process: the application's callbacks meeting the timer. **0 on a Tier A run would mean the guard is never contended and therefore proving nothing** |
| `cupti_try_failed_self` / `cupti_try_failed_other` | **0** — the two halves of `finalize_contended` |
| `cupti_max_depth` | **3** on Tier A (`burst_close` → `pc_drain_all` → the `GetData` wrapper), **2** in Tier B and on the context callbacks, 1 with PC sampling off. **Anything above 3 means CUPTI re-entered us through a callback** — the Task 6 (item 13) / Task 10 (item 11) open question, now a number instead of a self-deadlock |
| `cupti_calls_misplaced` | **0** — nobody widened the one unguarded hole |
| `timer_ticks` / `timer_drains` / `timer_skipped` | `drains ≈ ticks × tick_ms / drain_ms`; ticks moving with drains at 0 is a drain that silently stopped |

---

## 9. Verification actually run

```
make -C shim                    exit 0
make -C shim test               exit 0  (callguard_test OK, tickplan_test OK, burst_test OK,
                                         pcdrain_test OK, check-cubin-defer OK, and all the rest)
make -C shim check-fpless       OK
make -C shim check-cubin-defer  OK - the compliant capture compiles, all 5 deferrals do not
make -C shim check-cupti-guard  OK - the guarded call compiles, all 5 unguarded do not
make -C shim nvidia             exit 0 (CUDA 13.3, real libcupti; runs check-cupti-guard first)
make -C shim nvidia-concurrent  exit 0
make -C shim test-tsan          exit 0 (callguard_test clean under ThreadSanitizer)
go build ./... && go vet ./...  clean
go test ./gpu/ ./gpuprobe/ ./internal/... -count=1   all pass
go test ./gpu/ ./gpuprobe/ -race -count=4            ok gpu 23.1s, gpuprobe 19.8s
~/go/bin/golangci-lint run --timeout=5m              0 issues
```

Ten probes in the built adapter and one exported symbol, unchanged:

```
$ readelf -n shim/libperfagent-gpu-nvidia.so | grep -o 'gpu_[a-z_0-9]*' | sort -u
gpu_config_v1  gpu_dropped_v1  gpu_exec_v1  gpu_kernel_name_v1  gpu_launch_sampled_v1
gpu_launch_v1  gpu_module_load_v1  gpu_pc_sample_batch_v1  gpu_sampling_window_v1
gpu_stall_reason_map_v1
$ nm -D --defined-only shim/libperfagent-gpu-nvidia.so
00000000000068a0 T InitializeInjection
```

No Go code changed. No ABI, BPF program or `.o` file changed.

### Proven / argued / unproven — the honest split

**Proven, here, without a GPU:**

- CUPTI does not document `cuptiActivityFlushAll` or any PC-sampling entry point as thread safe,
  while documenting 42 other functions as such (a fact about the shipped headers — reproducible with
  `grep -c "Thread-safety" cupti_*.h`).
- The guard excludes, is re-entrant, and refuses the two `try` cases — with a negative control
  showing the test detects the exclusion being removed, and under ThreadSanitizer.
- An unguarded CUPTI call site does not compile, in five spellings, with a compliant control that
  does.
- The merged schedule keeps the burst tick's granularity and the drain's period, leaves no catch-up
  debt after a late tick, and imposes no rate limit in the default configuration.

**Argued, from structure, not measured:**

- That at most one adapter-initiated CUPTI call is in flight at any instant. This follows from one
  mutual-exclusion primitive plus the impossibility of writing a call site that skips it — a
  structural argument, and the reason the fix is a lock *and* a thread merge rather than either
  alone.
- That the lock ordering is acyclic: one vendor lock, every other adapter mutex a leaf, verified
  path by path in §3. It is not enforced by a tool.
- That the buffer-completed exemption cannot deadlock, which rests on the header's description of
  the flush as blocking and on that callback taking no adapter lock that the flusher holds.

### Cannot verify — needs the RTX 3090

**Nothing below has been executed. `CapEff: 0`, no GPU on this machine, and no `CUpti_` code path
touched by this commit has ever run.**

**The fix is unproven on hardware. Nobody has observed the deadlock not happening.** What has been
established is that the two calls the backtrace named can no longer be in flight together, and that
the mechanism which guarantees that cannot be bypassed by a later edit without failing the build.
Whether CUPTI has another deadlock of its own that this does not touch is unknown and unknowable
from here.

Specifically unverified:

1. **That Tier A completes a run at all.** The only real test is Task 12's overhead run on the
   3090 — the one that hung.
2. **`cupti_waits` moving.** If it stays 0 on a Tier A run, the guard was never contended and this
   fix has demonstrated nothing on hardware even if the run passes.
3. **`cupti_max_depth` staying at 3.** Above 3 is CUPTI re-entering us through a callback — the
   hazard both prior reports listed as open, now measurable rather than fatal. If it moves, the
   per-context `CUpti_PCSamplingData` buffer is being re-entered by a nested drain, and that needs a
   per-call buffer, not a lock.
4. **`cupti_calls_misplaced == 0`**, and `cupti_try_failed_self`/`_other == 0`.
5. **The real duty figure under the merged timer.** `duty=` is measured, not assumed, and the merge
   means a slow activity flush can delay a burst stop by its own duration and lengthen the burst
   that actually ran. The size of that is the flush duration on a busy GPU, which is unmeasured.
   The 10 % ceiling bounds what the controller may *ask* for, never what the OS and the flush
   deliver; this makes the gap between the two slightly larger than it was.
6. **The cost of `cuptiGetCubinCrc` now taking the guard**, i.e. how long the application's
   `cuModuleLoad` waits. Bounded by one CUPTI call, unmeasured.
7. **Whether CUPTI delivers `buffer_completed` on the flushing thread or on a worker.** The design
   is correct either way — that is why the exemption exists — but which it is decides whether
   `cupti_reentrant` is inflated by that path.
8. **Whether stopping and joining the single timer inside `atexit` can wedge process exit.** It
   waits for at most one tick's work, which includes one activity flush; a flush that never returns
   would hang exit where the old code would have exited with the timer still running. That trade is
   deliberate — an exit that races CUPTI teardown is the shape of this whole issue — but it is a
   new way for exit to be slow and it has not been observed.
