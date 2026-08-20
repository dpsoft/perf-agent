# GPU attribution guard: a stack that never left the profiler is not an attribution

Branch `fix/gpu-profiler-only-stacks`, worktree `.worktrees/gpu-attrib-guard`.

## The rule

A resolved launch capture is attached to its launch **only if at least one of
its frames is provably outside the profiler's own injected module** — unless
the shim is itself the program, in which case there is no profiler/application
boundary and every stack is a legitimate attribution.

A capture that fails the test is withheld. The launch still ships, stackless,
and its execution's measured GPU time projects as `[gpu:launch unsampled]` —
the same unattributed population as a launch the sampler never picked. Nothing
is scaled, no execution borrows a sibling's call path, and the attributed and
unattributed populations still sum to the exact measured total. The only thing
lost is a claim the profiler could not support.

This is what the RTX 3090 / CUPTI run produced without the guard: the
frame-pointer walk died inside libcupti, the single surviving frame was
`(anonymous namespace)::on_callback`, and 100% of the attributed GPU time was
nested under a function that has never launched a kernel.

## Telling the two deployment shapes apart

`gpuprobe.shimScope` classifies the shim **once, at consumer construction,
from the shim's own ELF header**:

* `ET_EXEC`, or `ET_DYN` carrying a `PT_INTERP` segment → a file the kernel can
  exec → **self-contained producer** (`shim/stub`, which the phase gate runs).
  Guard OFF: `main → perfagent_stub_run` is entirely "inside the shim" and is a
  perfectly good attribution, because profiler and application are the same
  binary.
* anything else → a shared object → **injected adapter** (the CUDA case).
  Guard ON: application code lives in other modules by construction, so a stack
  confined to the shim provably never reached the application.

### Why this and not `/proc/<pid>/exe`

Comparing `stat("/proc/<pid>/exe")` against the shim by device+inode answers
the same question and is immune to path spelling, but it answers it *later*,
*per pid*, *on the hot capture path*; it needs `PTRACE_MODE_READ` on a process
that may already have exited (the short-lived-CUDA-process case is exactly the
one Phase 4b is chartered to fix); and it needs a bounded per-pid memo table to
stay cheap under a system-wide attach. The ELF answer is a static, deterministic
property of the file the consumer already opened to attach its probes, costs one
`elf.Open` per consumer, and needs no privilege at all. Its one blind spot is
covered below, and it fails towards silence.

### What fools it

* **A static-PIE self-contained producer** (`ET_DYN`, no `PT_INTERP`) is read as
  an injected library, so its legitimate inside-only stacks are rejected.
  Information loss, counted, honest. This is the `/proc/<pid>/exe` route's one
  real advantage, given up deliberately. (`shim/stub` is dynamically linked, so
  the gate is unaffected; `TestShimShapeIsClassifiedFromTheELF` pins the
  classification of a program and of a real mapped `.so`.)
* **Module identity is a path comparison** — the configured spelling, its
  absolute form, its symlink-resolved form, plus a basename fallback. The
  basename fallback exists because a target in another mount namespace reports
  the shim under *its own* rootfs, which matches none of the three spellings;
  without it the shim's own frames would read as "outside" and the guard would
  accept a profiler-only stack, the one direction that must not happen. The cost
  is that an application module sharing the shim's file name reads as "inside",
  which can only cause a rejection. `BuildID` would be a stronger identity, but
  `pprof.Frame.BuildID` is filled by the pprof builder from `/proc/<pid>/maps`
  long after this check runs; the frames the consumer holds carry only what the
  symbolizer set. When Phase 4b snapshots maps, this comparison should move to
  the build ID.
* **A frame with no module proves nothing** and is never read as "outside".
  Reading "unknown" as "outside" would be a false accept. So a stack of nothing
  but unnamed modules is rejected too — and counted apart, see below.
* **An empty `ShimPath` disables the guard.** With no idea which module is the
  profiler's there is no evidence in either direction, and rejecting everything
  on no evidence is destruction rather than honesty. `Attach` always sets it (it
  parses that file's USDT notes and opens it as an executable), so this is a
  unit-test shape, not a live one.
* **An unreadable/unparseable shim ELF** leaves the shape unknown; the guard
  stays ON and treats it as injected — the lossy direction.

Every failure mode above errs towards rejecting a genuine attribution
(information loss, counted) rather than accepting a profiler-only one (the bug).

## Where the check lives, and why

In `gpuprobe`, in `Consumer.attachSampledStackLocked`, immediately after
`resolveStackLocked` succeeds and before the capture can be parked or attached —
so it covers **both** arrival orders (stack-first parks nothing; batch-first
releases the held launch without lending it the refused stack).

`gpu/` is unchanged in behaviour, deliberately. The judgement needs two things
`gpu/` does not have and should not acquire: which module is the profiler's, and
which deployment shape it is running in — vendor/deployment knowledge that this
codebase has kept out of `gpu/` on purpose. `gpu/projection.go`'s `sampledStack`
already treats "launch carried no stack" as the single definition of
unattributed; the consumer withholding a stack simply lands there. The only
`gpu/` change is a comment on `sampledStack` recording that "carried no stack"
now also covers a refused capture, and why the decision is made elsewhere.

## Accounting

Two new fields on `gpuprobe.Stats`:

* **`StacksProfilerOnly`** — resolved captures refused because the walk never
  left the profiler's own injected shim. Attribution loss, not record loss.
* **`StacksProfilerOnlyUncertain`** — the *subset* of the above refused without
  proof (no frame provably outside, but at least one frame's module unknown).
  Deliberately a subset, not an additive bucket, so the reconciliation stays a
  partition. It separates the two ways the guard can be wrong: proven refusals
  are the bug it was built for; a rising uncertain count means symbolization is
  failing to name modules and the guard is paying for it in lost attribution.

The documented at-rest identity becomes:

    StacksResolved = StacksAttached + StacksEvicted + StacksProfilerOnly + PendingStacks

`TestSampledStackAccountingReconciles` asserts it, plus
`StacksProfilerOnlyUncertain <= StacksProfilerOnly`. `cmd/gpu-stub-profile`
prints `Stats` with `%+v`, so both counters surface with no change there.

## Phase gate

Not weakened, and strengthened by one assertion: `gate_test.go` now also asserts
`stats.StacksProfilerOnly == 0`, on the grounds that the stub IS the shim, so a
refusal there means the injected-adapter guard misread a self-contained
producer. The existing "exactly 63 sampled launches carry a non-empty CPUStack
containing `perfagent_stub_run`" assertion is untouched and is unaffected by the
guard, which is off for a program-shaped shim.

## Tests added (`gpuprobe/shimscope_test.go`)

* `TestShimShapeIsClassifiedFromTheELF` — `/proc/self/exe` classifies as a
  program (guard off); a real shared object mapped into this very process
  (found by parsing `/proc/self/maps`) classifies as a library (guard on).
* `TestNoShimPathLeavesTheGuardOff`.
* `TestVerdictNeedsPositiveEvidenceOfLeavingTheShim` — table over the injected
  shape: the exact RTX 3090 single-callback stack; a stack that reached the
  application; the same shim under another mount namespace's path; unknown
  modules alone and mixed with real evidence.
* `TestInjectedShimOnlyStackIsWithheldAndCounted` — end to end through
  `applyBatch`: launch ships, `CPUStack` empty, `SamplePeriod` zero,
  `StacksProfilerOnly == 1`, `StacksResolved == 1`, nothing parked.
* `TestInjectedShimStackThatReachesTheApplicationIsKept` — same shim, one
  application frame: attached untouched, counter zero.
* `TestSelfContainedShimStackIsAnAttribution` — the stub shape, synthesized:
  every frame in the shim, and it is attached.
* `TestUnprovenRejectionIsCountedSeparately`.
* `TestRefusedStackIsNeverAttachedInEitherArrivalOrder` — batch-first order.

None of them skips on this machine (verified with `-v`).

## Verification

```
$ go build ./... && go vet ./...
(no output: clean)

$ go test ./gpu/ ./gpuprobe/ ./internal/... -count=1
ok  	github.com/dpsoft/perf-agent/gpu	3.484s
ok  	github.com/dpsoft/perf-agent/gpuprobe	0.050s
ok  	github.com/dpsoft/perf-agent/internal/bpfstack	0.003s
ok  	github.com/dpsoft/perf-agent/internal/gpuabi	0.003s
ok  	github.com/dpsoft/perf-agent/internal/k8slabels	0.003s
ok  	github.com/dpsoft/perf-agent/internal/nspid	0.003s
ok  	github.com/dpsoft/perf-agent/internal/perfdata	0.227s
ok  	github.com/dpsoft/perf-agent/internal/perfevent	0.002s
ok  	github.com/dpsoft/perf-agent/internal/usdt	0.114s

$ go test ./gpuprobe/ -race -count=1
ok  	github.com/dpsoft/perf-agent/gpuprobe	1.178s

$ gofmt -l gpu gpuprobe
(no output: clean)

$ ~/go/bin/golangci-lint run --timeout=5m
0 issues.
```

## What I could not verify

* `CapEff: 0` on this machine, so **the phase gate itself was not run**
  (`TestStubDrivesThePipelineToPprofWithoutAGPU` needs `CAP_BPF` + `CAP_PERFMON`
  and skips). The argument that it still passes is: `shim/perfagent-gpu-stub` is
  a dynamically linked program, so `newShimScope` returns an unguarded scope and
  `verdict` returns `stackAttributable` unconditionally — the guard cannot touch
  a single stub stack. `TestSelfContainedShimStackIsAnAttribution` exercises that
  path end to end with `/proc/self/exe` standing in for the stub.
* **No real CUPTI/GPU hardware here**, so the injected-adapter shape is exercised
  against synthesized frames and a real system `.so` rather than against
  `libperfagent_cupti.so` on an RTX 3090. What a live run will confirm is the one
  assumption behind the guard: that blazesym populates `Frame.Module` with the
  adapter's path for the callback frame. If it does not, the run lands in
  `StacksProfilerOnlyUncertain` instead of `StacksProfilerOnly` — still refused,
  still honest, still counted, just without proof.
* The privileged gate binary was rebuilt once at the end
  (`/home/diego/gpuprobe.test`, blazesym statically linked, `readelf -d | grep -c
  blazesym` = 0). **Rebuilding stripped its file capabilities; a human must
  `setcap` it again** before the gate can run.
