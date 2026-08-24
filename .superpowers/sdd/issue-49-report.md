# Issue #49 — the CFI tables now exist before the probe fires

Branch `fix/gpu-eager-cfi`. One commit. **Not verified on hardware:** this machine has
`CapEff: 0`, no BPF attach and no GPU, so `TestStubDrivesThePipelineToPprofWithoutAGPU`
skips and no CUDA run was possible. Every number in §5 is a prediction derived from the
mechanism, stated before any run.

---

## 1. The premise, checked

The issue's framing is correct and the measurement is where it says it is.

`gpuprobe/consumer.go` registers eagerly and synchronously only when `Config.PID != 0`.
For `Config.PID == 0` — which `cmd/gpu-cuda-profile` passes, because the CUDA process
does not exist at `Attach` time — nothing was registered until `applyBatch` saw the first
batch from a PID and queued a compile on the registry's worker goroutine.

The reason that is unfixable in userspace-observation terms: **the stack walk happens in
the kernel, inside `walk_step` (`bpf/unwind_common.h`), at the instant the uprobe traps.**
`walk_step` unwinds by CFI only for a PC it can find in `pid_mappings`; a miss is silent
and falls through to the frame-pointer chain, which in a CUDA process dies in the vendor
libraries. So any trigger a consumer can observe — a first batch, an mmap notification,
an exec notification — is by construction late: the sample that triggered it was already
walked without tables, and so is everything arriving during the compile. libcuda alone is
135 805 CFI entries at ~73 ms against a ~540 ms workload, which is why the loss is ~38%
and not a rounding error.

Two arithmetic checks on the reported numbers, both of which hold and both of which
constrain the prediction in §5:

- `StacksWalkedDWARF + StacksWalkedNoTables == 500` in every run (308 + 192, 313 + 187).
  Combined with the existing invariant `DWARF + FPOnly == sampled`, that means
  `FPOnly == NoTables` exactly: **there were no FP-only walks that had tables.** Every
  walk that had tables used them.
- `StacksProfilerOnly == StacksWalkedNoTables` in every run: the tableless walks are
  precisely the ones the #39 guard refused. So the lost population and the tableless
  population are the same set, and removing one removes the other.

---

## 2. The mechanism: the producer waits

The producer has a point in its life that provably precedes every launch, and the
consumer does not. So the producer is the one that waits.

**`shim/core/enroll.{h,cc}` — the producer half.** At initialisation, and only when the
sampled-launch probe's semaphore says a consumer is attached, the shim connects to an
abstract `AF_UNIX` socket and blocks for one status byte.

**`gpuprobe/enroll.go` — the consumer half.** `Attach` binds that socket *before* it
creates the `uprobe_multi` link. It accepts one producer at a time, takes the peer's PID
from `SO_PEERCRED`, checks that PID actually maps the shim inode it attached to,
registers its CFI tables **synchronously**, and only then writes the byte.

The address is derived independently on both sides from the shim's own device and inode:

```
\0perfagent-gpu-enroll.v1.<dev_major>.<dev_minor>.<inode>
```

The producer reads its own `dev:inode` out of `/proc/self/maps` (the mapping containing
`enroll_with_consumer`'s own address, which is linked into the shim); the consumer reads
the same two numbers from `stat(2)` on `Config.ShimPath`. The inode is the right key
because it is exactly what a uprobe attaches to: a producer whose probes this consumer
armed maps that inode by construction. No environment variable, no path agreement, no
cooperation from whoever launched the process, and two consumers watching two copies of a
shim (the gate makes a private copy per run precisely so concurrent runs cannot collide)
get two different addresses for free.

### Why this closes the window rather than narrowing it

For a CUDA process the call sequence is fixed by the driver:

```
cuInit
  └── dlopen(CUDA_INJECTION64_PATH)      libcuda, libcupti, libcudart, the app: all mapped
  └── InitializeInjection()
        ├── open_log()
        ├── enroll_with_consumer()  <-- blocks here until the tables are in
        ├── cuptiSubscribe(on_callback)
        └── cuptiActivityEnable(...)
  ...
cudaLaunchKernel  ->  on_callback  ->  gpu_launch_sampled_v1 probe  ->  walk_step
```

The rendezvous sits **before `cuptiSubscribe`**, not after. Subscribing is what makes
`on_callback` reachable, and `on_callback` is the only thing in the adapter that fires a
probe. So at the moment the reply is written, this process has fired no probe of any
kind — not even from another thread already inside a CUDA call — and cannot until
`InitializeInjection` returns.

There is therefore no launch, so no probe, so no walk, until `AttachAllMappings` has
returned. That is a synchronisation point, not a shorter race: making the compile faster
would not change the argument and slowing it down would not break it.

The stub takes the identical shape — the call is the first statement of
`perfagent_stub_run`, before its launch loop.

### It does not touch the event path

The compile runs on the `enrollListener`'s own goroutine. The ringbuf drain goroutine is
unchanged, and `Consumer.applyBatch` still does only O(1) map and list work. No batch can
even arrive from the enrolling PID during its compile, because that process is blocked in
its own initialisation; other PIDs drain normally throughout. `SequenceGaps` and
`KernelDropped` have no new exposure.

The accept loop is serial on purpose. N producers starting at once interleave into the
same total I/O either way, and serial makes the ordering property trivially true: the
reply for a producer goes out after that producer's tables are installed, with nothing
else half-installed at the time.

---

## 3. What it does not cover

Stated plainly, because the fix is a mechanism with a scope, not a blanket.

1. **A process that was already running when the consumer attached.** Its
   `InitializeInjection` ran with the semaphore at zero; it never reaches the rendezvous.
   Its first batch triggers lazy registration exactly as before, and everything sampled
   during that compile is walked without tables. **This is the genuine residual of #49**,
   and it is visible as `StacksWalkedNoTables > 0` with `StacksNoTablesAfterEnroll == 0`.
   Closing it needs the process stopped or its launches suppressed from outside, which
   nothing here does.
2. **A producer in a different network namespace.** Abstract sockets are netns-scoped. A
   containerised CUDA process whose netns differs from the profiler's cannot reach the
   address and falls back to lazy. Host-network and same-netns cases are fine. A
   filesystem-path socket would trade this for a mount-namespace and cleanup problem;
   neither is free, and the abstract one has no stale-file failure mode.
3. **Libraries mapped after `cuInit`.** The tables cover what was mapped when the
   rendezvous ran. Something dlopened later and landing on the launch path would produce a
   per-frame mapping or CFI miss, not a "no tables for this PID" — a different counter
   (`StacksWalkedCFIMiss`, `StackWalkAbandoned`) and a different fix (an mmap watcher),
   out of scope here. The measured launch path — app → libcudart → libcupti → the adapter
   → the probe — is entirely mapped before `InitializeInjection` runs.
4. **A shim built before this change**, or one where `PERFAGENT_GPU_ENROLL_TIMEOUT_MS=0`:
   no rendezvous, lazy path, pre-#49 behaviour.
5. **A second consumer on the same shim inode.** Only one can bind; the second reports
   `UnwindEnrollListening == false` and registers lazily for everything.
6. **Non-CUPTI backends.** The rendezvous lives in `shim/core`, so any future vendor
   adapter gets it by calling one function at init — but each has to call it.
7. **A producer refused by the rate limiter.** More than 32 enrolments per second from one
   uid (or 96 across the listener) fall back to the lazy path. That is far past any real
   workload, and `UnwindEnrollThrottled` says when it happens, but it is a documented
   ceiling rather than an unbounded promise.

It is *not* limited to the case where the profiler spawns the target. A genuine
system-wide attach covers every CUDA process that starts afterwards in the same netns,
which is the deployment shape `cmd/gpu-cuda-profile` and the gate both use.

---

## 4. Failure and fallback

Every path falls through to today's behaviour. Registration remains best-effort and
nothing here can fail an attach or fail a run.

| what fails | producer | consumer |
|---|---|---|
| no consumer attached (semaphore 0) | never connects; zero cost | — |
| address unbindable (already held, no abstract sockets) | `no-listener`, runs immediately | `Attach` continues, `UnwindEnrollListening=false`, reason in `UnwindEnrollLastError` |
| peer credentials unreadable / peer does not map the shim / wrong PID on a per-PID attach | reads `'X'`, runs | `UnwindEnrollRefused++`, no compile at all |
| registration installs nothing | reads `'X'`, runs | `UnwindEnrollFailed++`, PID marked enrolled so its walks are counted as the contradiction they are |
| consumer closes mid-rendezvous | reads EOF, runs | `close()` releases the producer; it is never left parked |
| consumer too slow | `PERFAGENT_GPU_ENROLL_TIMEOUT_MS` (default 2000 ms) expires, runs | reply write has its own 5 s deadline so a slow compile still gets its answer out |
| listener saturated (rate limit) | reads `'X'`, runs | `UnwindEnrollThrottled++`, no /proc read and no compile |
| `enroll_with_consumer` cannot find its own mapping | `no-address`, runs | — |
| connect interrupted by a signal after it completed | `EISCONN` is treated as connected, not as an error, so a rendezvous that really connected is not thrown away | — |

**The budget is one deadline for the whole rendezvous**, connect and reply together, taken
from `CLOCK_MONOTONIC` on entry and re-applied as *remaining* time before every blocking
call. Two things made the first draft of this wrong, and both were measured on a harness
against the real `enroll.cc`:

- A per-syscall `SO_RCVTIMEO` is re-armed from zero on every `EINTR`, so a signal arriving
  faster than the budget makes the wait never expire — 500 ms budget, `SIGALRM` every
  100 ms, still blocked past **25 s** with no upper bound. That is not exotic: a Go runtime
  sending `SIGURG`/`SIGPROF` in a cgo CUDA host, a JVM, `setitimer`, or a second profiler
  all produce it, and the result was the profiled application hanging inside `cuInit`
  forever. In a design whose premise is blocking the profiled process, an unbounded stall
  is the one outcome that must be impossible.
- `SO_SNDTIMEO` on the connect *plus* `SO_RCVTIMEO` on the read is two budgets, so the real
  worst case was 2× the documented one.

The signal-storm case in `shim/core/enroll_test.cc` pins both: it fires `SIGALRM` every
50 ms (with `SA_RESTART` off, so the read really returns `EINTR`) against a 300 ms budget
and asserts the call returns timed-out in under a second, and a companion case asserts a
200 ms budget is not spent twice. Reverting `arm()` to a fixed per-call timeout makes the
first one fail — verified by building that variant.

The 2 s default is ~10× the measured need (libcuda 73 ms plus libcupti, libcudart, libc and
the workload). A process mapping something libLLVM-sized could exceed it and fall back;
`PERFAGENT_GPU_ENROLL_TIMEOUT_MS` is the dial, and `0` turns the rendezvous off.

**Abuse.** The address is reachable by anything in the netns. Authorisation is the shim
mapping, checked in order: `SO_PEERCRED` → per-PID filter → `procMapsHaveInode` → *then*
enrol. An arbitrary PID cannot be registered.

`Ucred.Uid` is **not** a blanket authorisation check. A root profiler legitimately profiles
other users' processes, so requiring `uid == euid` unconditionally would refuse the real
producer in the commonest production shape and silently restore the pre-#49 loss.

There is a strictly additive version, and it is now in: when the consumer's euid is non-zero
**and** it holds no `CAP_SYS_PTRACE`, the listener serves only its own uid. Such a consumer
could not read another user's `/proc/<pid>/maps` anyway, so this refuses nothing it could
have done — and it closes the rendezvous to every other local user on a multi-user box.
Privileged consumers keep the inode check alone. `enrollRequiredUID` checks Permitted as
well as Effective, for the same reason `perfagent.hasCapSysPtrace` does: a setcap'd binary
has not promoted Permitted yet and would otherwise be misread as unprivileged.

Otherwise the uid is the accounting key for the rate limiter and a name in the error string.

What remains is resource exhaustion, and `enrollAdmission` bounds it: a token bucket per
peer uid (32 burst, 32/s) and one for the listener as a whole (96/96), with the per-uid
table itself capped at 64 entries so the defence cannot become the exhaustion. Entries that
have refilled completely — i.e. uids idle for a full window — are reclaimed when the table
is full, so a spray of one-shot uids cannot pin it and push real producers onto the shared
bucket alone. Refusal is
instantaneous, happens before any `/proc` read, and is counted in `UnwindEnrollThrottled`.
A throttled producer is released and takes the lazy path — a worse profile, never a
stalled application. (The compile is largely amortised anyway: `TableStore` keys on
build-id, so a thousand processes mapping the same libLLVM pay for one.)

**TOCTOU, accepted and documented.** `SO_PEERCRED` is stamped at connect but
`/proc/<pid>/maps` is read afterwards, so a peer that disconnects immediately could in
principle allow a PID recycle. The peer is normally blocked on the socket for the whole
window, and losing the race is benign: the maps read and the registration use the *same*
later `/proc` read, so the tables installed are correct for whoever holds that PID now, not
stale ones for the process that connected — and that new process must itself map the shim
inode or the check refuses it, which makes it a legitimate producer. The cost is a wasted
compile and an LRU slot, never a wrong stack. A pidfd dance would not be atomic against
exec either.

---

## 4b. A shared-table race this change would otherwise have introduced

Before this commit the `PID == 0` path had exactly one caller of
`pidRegistrar.Register`: the registry's worker goroutine. The rendezvous adds a second (the
listener goroutine), and that made a latent bug in `unwind/ehmaps` reachable.

`TableStore.AcquireBinary` returned "already installed" as soon as the refcount exceeded
one — which is true from the instant the **first** caller bumped it, before that caller had
compiled anything or written a single row. With two CUDA processes mapping the same binary:
A's enrolment is compiling libc, B's registration gets `rc = 2` and returns immediately,
`recordLocked` marks B `pidReady`, and B's walks consult a half-populated table.

The symptom is the worst kind: those walks *have* mappings, so they never count as
`NoTables`. They surface as CFI misses or FP-only-with-tables — a different symptom with a
different suspect, and invisible to the counter meant to detect exactly this.

Fixed in `unwind/ehmaps/store.go` by separating "who holds this table" (the refcount) from
"this table is in the maps" (`installed`), with an `inflight` set and a `sync.Cond` so a
second caller **waits for** the install rather than assuming it finished. `installed` is set
only after both populates succeed, so a failed compile leaves the next caller free to retry
instead of inheriting rows that were never written. `ReleaseBinary` holds `instMu` across
both the forget *and* the map deletions, so eviction is atomic with respect to
`beginInstall`: an acquirer either sees the table before the eviction starts, or compiles it
again — it is never told "installed" for rows that are about to be deleted. The barrier is
per-table, so one slow libcuda compile does not serialise every other binary.

### The claim leak that first version shipped, and how it got through

`AcquireBinary` uses named return values. Every failure path after the claim returns
`0, false, err`, which **assigns** the named `tableID` before the deferred `endInstall`
runs — so the defer released the claim on table 0 and left the real tableID marked in flight
**forever**, with no timeout and no cancellation for the next caller.

The blast radius was the serious part, and it had nothing to do with GPUs.
`ehcompile.Compile` returns `ErrNoEHFrame` for any ELF without `.eh_frame` — an *expected*
case that `AttachAllMappings` already logs and skips. So the first process mapping such a
library poisoned its tableID and the second wedged in `beginInstall`. In `perf_dwarf` and
`offcpu_dwarf` the wedged caller is the `PIDTracker.Run` goroutine, which also services exit
events and `Detach`, so mmap tracking and PID teardown would stop with it; `pidRegistry.close`'s
`wg.Wait()` would hang `Consumer.Close`. Reachable on any system-wide DWARF run. The
pre-existing code had no such path, because the old `rc > 1` early return took no claim.

The fix is one line (`claimed := tableID` before the defer), but **the test gap is the real
finding**. This passed five new tests and `-race -count=4` because
`store_install_test.go` exercised `beginInstall`/`endInstall` directly and never went
through `AcquireBinary` — testing the primitives while the bug lived in the caller.
`TestASecondAcquireAfterAFailedInstallDoesNotWedge` now drives the real entry point through
both failure arms: an `objcopy`-stripped ELF that has a build-id (so the claim *is* taken)
but no `.eh_frame`, and a successful compile whose populate then fails. It asserts both that
`inflight` is empty and that a second `AcquireBinary` returns within 5 s. Reverting the
defer to close over the named `tableID` fails it — verified.

Six internal tests in `unwind/ehmaps/store_install_test.go` pin the rest: a second installer
waits, a failed install is retryable, an evicted table is recompiled, distinct tables do not
block each other, and under 16-way contention exactly one caller compiles and **zero**
callers are told the table is ready before it is written.

`SetOnCompile`'s hook runs inside the install claim, so a hook that re-entered the store for
the same binary would wait on itself; that is now documented at both the field and the
setter.

---

## 5. Predicted counters on the RTX 3090, and the derivation

500 sampled launches, `cmd/gpu-cuda-profile` defaults, one workload.

```
StacksWalkedDWARF          500      (was mean 308)
StacksWalkedNoTables         0      (was mean ~192)
StacksWalkedFPOnly           0
StacksProfilerOnly           0      (was == NoTables)
StacksNoTablesAfterEnroll    0
UnwindEnrollListening     true
UnwindEnrollRequests         1
UnwindEnrollConfirmed        1
UnwindEnrollRefused          0
UnwindEnrollFailed           0
UnwindEnrollThrottled        0
UnwindEnrolledPIDsEvicted    0
UnwindEnrolledMarksDropped   0
UnwindPIDsRegistered         1
SequenceGaps                 0      (unchanged)
```

Derivation, step by step:

1. `StacksWalkedNoTables == 0`. It is incremented only for an FP-only walk from a PID the
   registry does not have as `pidReady`. The single producer reaches `pidReady` before
   `InitializeInjection` returns, and `InitializeInjection` returns before `cuptiSubscribe`
   runs, which is before any callback can fire a probe. So no walk can happen from a
   not-ready PID.
2. `StacksWalkedFPOnly == 0`, hence `StacksWalkedDWARF == 500`. The existing invariant
   `DWARF + FPOnly == sampled` holds unconditionally. In the twelve baseline runs
   `FPOnly == NoTables` exactly, i.e. **there was never an FP-only walk that had tables**
   — the walk from the adapter's callback out to the application crosses libcupti and
   libcudart, which are FP-less, so with tables present it always sets `DWARF_USED`.
   With `NoTables == 0` that forces `FPOnly == 0` and `DWARF == 500`.
3. `StacksProfilerOnly == 0`. It equalled `NoTables` in every baseline run: the refused
   stacks were exactly the tableless ones, because those are the walks that die in the
   adapter's own frame. With no tableless walks there is nothing for the #39 guard to
   refuse. This is the number that says the *attribution* was recovered, not just the
   walk — 500 launches now reach the profile with a real application call path.
4. `UnwindEnrollRequests == UnwindEnrollConfirmed == 1`. One workload process, one
   `InitializeInjection`, one connection that passes both identity checks. One connection
   cannot exceed a 32-per-uid-per-second bucket, so `UnwindEnrollThrottled == 0`; one PID
   against a 128-PID bound cannot be evicted, so `UnwindEnrolledPIDsEvicted == 0` and
   `UnwindEnrolledMarksDropped == 0`. The workload runs as the same user as the profiler
   here, and the profiler is privileged, so the uid gate is inert either way.
5. `SequenceGaps == 0`. Nothing was added to the drain path.

Also expected, not asserted: the workload's `cuInit` gains roughly 150–250 ms (the
compile of libcuda, libcupti, libcudart, libc and the workload binary) before its first
launch; the adapter logs `enroll=confirmed` on its `PERFAGENT_GPU_LOG` line.

**What would falsify this.** `UnwindEnrollListening == false` means the address could not
be bound and the run is entirely on the old path. `UnwindEnrollRefused > 0` means the
identity check rejected the process the uprobes fired in, which cannot be true and would
mean the `/proc` inode match is wrong. `StacksWalkedNoTables > 0` with
`UnwindEnrollConfirmed == 1` means a walk happened before the reply, i.e. the ordering
argument in §2 is false. `StacksNoTablesAfterEnroll > 0` **while `UnwindEnrollFailed` and
`UnwindEnrolledPIDsEvicted` are both zero** means the rendezvous reported success it did
not deliver. Any of these is a real defect, not a timing transient — which is the
difference from the pre-#49 code, where a non-zero `NoTables` was expected.

---

## 6. Counters

Six defects on this project were counters reading green when things were worst, so:

- **`StacksWalkedNoTables` is untouched and still counts every tableless walk**, enrolled
  or not. Nothing in this change can make it read zero except tables genuinely being
  present. There is a regression test for exactly that
  (`TestATablelessWalkAfterAnEnrollIsCountedSeparately` asserts *both* counters at 1).
- **`StacksNoTablesAfterEnroll`** is the new race counter: the subset of the above whose
  process completed the rendezvous. It has exactly two benign explanations, each with its
  own counter beside it — registration ran and installed nothing (`UnwindEnrollFailed`), or
  the PID was evicted from the bounded set afterwards (`UnwindEnrolledPIDsEvicted`). It is
  **non-zero with both of those at zero** that has no benign reading: it means a walk
  happened before the reply went out. The earlier draft of this report claimed the
  unqualified form, which contradicted the code's own doc; the code doc now enumerates all
  three and this does too.
- **The enrolled mark deliberately outlives eviction.** It used to live only on the
  registry entry, and eviction deletes the entry — so after an eviction the flag was gone,
  `StacksNoTablesAfterEnroll` read zero and `StacksWalkedNoTables` climbed. Green exactly
  when things were worst, inside the counter added to prevent that. `pidRegistry` now keeps
  a bounded FIFO of evicted-but-enrolled PIDs, and `note()`/`registerNow()` carry the mark
  forward when such a PID comes back. The bound means a PID evicted long ago is forgotten
  (a false negative, the safe direction) and a recycled PID can be miscredited (a false
  positive, cross-checked by `UnwindEnrolledPIDsEvicted`).
- **`UnwindEnrolledPIDsEvicted`** names the second benign case on its own: the capacity
  bound biting on exactly the processes the rendezvous exists to protect.
- **`UnwindEnrolledMarksDropped`** closes the last hole in the chain. The mark set is
  bounded, so a mark can age out and `StacksNoTablesAfterEnroll` then under-reports for that
  PID — a silent under-report in the counter that exists to prevent silent under-reports.
  Every dropped mark is now counted, and the set is sized at 4× the PID capacity rather than
  1×: it is fed once per enrolment *and* again when an enrolled PID is evicted, so at 1× it
  churned against the LRU and aged marks out roughly twice as fast as evictions happened.
  `TestTheEnrolledMarkSetOutlivesTheEvictionItMustWitness` pins that, and the bounds test
  asserts the books balance (`dropped + held == enrolments`).
- **`UnwindEnrollThrottled`** makes the rate limiter visible. Non-zero on a machine that is
  not under attack means a legitimate burst exceeded the per-uid bucket and those producers
  fell back to the lazy path — the limiter causing a small version of the loss it protects.
- **`UnwindEnrollListening`** exists because a run that lost the rendezvous and a run that
  never needed one are otherwise indistinguishable, and the first is the one that loses
  ~38% of its stacks.
- `UnwindEnrollRequests` / `Confirmed` / `Refused` / `Failed` / `LastError` split the
  outcomes so a shortfall names its own cause.
- The producer reports its own half on stderr (`enroll=confirmed|no-listener|timed-out|…`).
  A producer that never reached the socket is invisible to every consumer-side counter —
  it would simply never appear — so the gate checks both ends.

---

## 7. Tests, and why each is falsifiable

Go (`gpuprobe/enroll_test.go`, `gpuprobe/unwindtables_test.go`) — no caps, no GPU:

| test | fails if |
|---|---|
| `TestTheProducerIsNotReleasedUntilItsTablesAreInstalled` | the reply is written before `Register` returns (the registrar is wedged on a channel and the test asserts no byte arrives) |
| `TestTheCppProducerAndTheGoListenerAgreeOnTheWire` | the two ends spell the socket name differently, or the reply byte changes on one side. Builds a real producer from `shim/core/enroll.cc`, runs it against a real listener, asserts it prints `confirmed` and that the *peer's* PID was registered |
| `TestTheRendezvousAddressIsTheShimsDeviceAndInode` | the address stops being a pure function of the shim's identity; pins the exact spelling |
| `TestASecondRendezvousDoesNotRecompile` | a repeat connection duplicates `pid_mappings` rows and CFI table references |
| `TestABatchArrivingMidEnrollDoesNotQueueASecondRegistration` | `enroll` claims its registry entry after the compile instead of before, letting the lazy path register the same PID again |
| `TestAFailedRegistrationStillReleasesTheProducer` | a failed compile parks the producer instead of releasing it |
| `TestAPeerThatDoesNotMapTheShimIsRefused` / `TestAPerPIDConsumerRefusesEveryOtherPID` | the identity checks stop biting |
| `TestCloseReleasesAWaitingProducer` | teardown leaves a producer parked |
| `TestASecondListenerForTheSameShimIsRefused` | a second consumer silently shadows the first |
| `TestProcMapsInodeMatchIsDeviceAware` | the hex device field in `/proc` is read as decimal, which would refuse every genuine producer while looking like a working check |
| `TestATablelessWalkAfterAnEnrollIsCountedSeparately` / `…WithoutAnEnrollIsNotCountedAsADefect` | the new counter nets out of the honest total, or fires on ordinary startup transients |
| `TestAnEnrolledPIDEvictedFromTheBoundKeepsItsMark` / `…ComesBackLazily` / `TestATablelessWalkAfterAnEnrolledEvictionIsCounted` | the enrolled mark dies with the evicted entry, so a broken promise reads as an ordinary transient |
| `TestTheEnrolledShadowSetIsBounded` | the anti-blindness set grows with a profiled machine's process churn, or drops marks without counting them |
| `TestTheEnrolledMarkSetOutlivesTheEvictionItMustWitness` | the mark set is sized at the PID capacity and ages a mark out before the eviction it exists to witness |
| `TestOnlyAnUnprivilegedConsumerIsPinnedToItsOwnUID` / `TestAPeerOfAnotherUIDIsRefusedByAnUnprivilegedConsumer` | the uid gate becomes unconditional (refusing a root profiler's non-root target) or disappears |
| `TestIdlePerUIDEntriesAreReclaimed` | one-shot uids permanently occupy the rate limiter's table |
| `TestOneUIDCannotMonopoliseTheRendezvous` / `TestManyUIDsTogetherAreStillBounded` / `TestThePerUIDTableIsBounded` / `TestALegitimateBurstOfProducersIsNotThrottled` / `TestAThrottledPeerIsReleasedAndCountedNotServed` | the rate limiter is absent, escapable across uids, unbounded in its own bookkeeping, tight enough to throttle a real workload, or parks the peer it refuses |
| `TestAPeerIsAuthorisedByTheShimItMapsNotByItsUID` | authorisation drifts onto the uid, which would refuse a root profiler's non-root target |

`unwind/ehmaps/store_install_test.go` (internal, no BPF): `TestASecondInstallerWaitsForThe
FirstInsteadOfAssumingItFinished`, `TestAFailedInstallLetsTheNextCallerTryAgain`,
`TestAnEvictedTableIsNoLongerConsideredInstalled`,
`TestInstallsOfDifferentTablesDoNotBlockEachOther`,
`TestExactlyOneCallerInstallsUnderContention`,
`TestASecondAcquireAfterAFailedInstallDoesNotWedge` — see §4b. The last one is the
important one: it drives `AcquireBinary` itself, which is where the bug the other five
could not see actually lived.

C++ (`shim/core/enroll_test.cc`, in `make -C shim test`): name derivation against a maps
fixture (including the anonymous-mapping and no-match cases), `'K'`/`'X'`, no listener, a
listener that accepts and never answers (must time out, not hang), **a signal storm that
must not extend the budget**, **both phases sharing one budget rather than each getting
it**, oversized and empty names, and the `PERFAGENT_GPU_ENROLL_TIMEOUT_MS` parse where an
explicit `0` means off rather than "use the default".

Gate (`TestStubDrivesThePipelineToPprofWithoutAGPU`), which still runs GPU-free with
`PID: 0` and the target not yet running — the shape is unchanged, the producer is still
launched after `Attach`, and the assertions were **tightened, not relaxed**:

- `assert.Less(StacksWalkedNoTables, 63)` → `assert.Zero(StacksWalkedNoTables)`. The old
  comment called zero "the trap in the shape of an obvious assertion" and it was right at
  the time; it is no longer, because the producer no longer races the compile.
- added: `StacksNoTablesAfterEnroll == 0`, `UnwindEnrollListening`,
  `UnwindEnrollConfirmed == 1`, `UnwindEnrollRefused == 0`, `UnwindEnrollFailed == 0`,
  `UnwindEnrollThrottled == 0`, `UnwindEnrolledPIDsEvicted == 0`,
  `StacksWalkedDWARF == 63`, and `stubErr` contains `enroll=confirmed`.

Predicted gate counters, against the §5.2 table of `issue-45-report.md`: everything there
is unchanged except that the "62/1" DWARF/FP-only split — which that report explicitly
left unasserted as timing-dependent — becomes 63/0. `StackWalkReachedRoot ==
StacksWalkedDWARF` still holds, now at 63, and `StackWalkAbandoned`,
`StackWalkFPExhausted`, `StackWalkFPNonMonotonic`, `StackWalkRootDisagreement` and
`StacksWalkedCFIMiss` all stay zero.

---

## 8. Verification actually run

```
go build ./... && go vet ./...                                        pass
go test ./gpu/ ./gpuprobe/ ./internal/... ./unwind/... -count=1        pass
go test ./gpu/ ./gpuprobe/ ./unwind/ehmaps/ -race -count=4             pass
golangci-lint run --timeout=5m                                        0 issues
make -C shim test                                                     pass (incl. enroll_test)
make -C shim perfagent-gpu-stub perfagent-gpu-fpless check-fpless      pass
make -C shim nvidia                                                   builds; exports only InitializeInjection
```

**Cannot verify:** `CapEff: 0` on this machine. No BPF object could be loaded, no uprobe
attached, and no CUDA device is present, so `TestStubDrivesThePipelineToPprofWithoutAGPU`
skips and no RTX 3090 run was made. Nothing in §5 has been observed; it is derived, and
the tightened gate assertions are unproven against real BPF. In particular, the claim that
the uprobe reference-count semaphore is already armed when `InitializeInjection` runs
(kernel `uprobe_mmap` / delayed-uprobe handling at `dlopen`) is read from the kernel's
contract, not measured here — if it were false, the producer would skip the rendezvous
and the run would simply look like the pre-#49 baseline, with `UnwindEnrollRequests == 0`
saying so.
