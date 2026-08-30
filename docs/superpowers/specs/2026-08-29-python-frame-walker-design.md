# Python frame-chain walking from BPF — design

Issue: #83. Supersedes the removed perf-trampoline injector (`0d3f00e5`).

## Purpose

Put Python frames in the same stack as the native frames that ran them, so a
PyTorch profile reads as one tower: `torch.matmul` above the C code that called
into CUDA, joined to the launch that followed.

That last clause is the whole reason this is worth building. A Python profiler
that cannot reach the CUDA launch site is a commodity; one that can is the thing
this project already has the GPU half of.

## Non-goals

- **Any CPython below 3.12.** Nothing older is detected, walked or guessed at.
  3.11 introduced `_PyInterpreterFrame` and the `owner` field this design
  depends on, so it is structurally in range — but it is excluded because it is
  the odd one out for thread-state discovery (measured below): it has no
  `_PyThreadState_GetCurrent` at all, and its `PyGILState_GetThisThreadState`
  passes the TSS key *value* to `pthread_getspecific@plt` rather than a pointer
  to `PyThread_tss_get`. Supporting 3.11 means a second parser for a second
  instruction shape; 3.12+ needs exactly one.
- **Profiling an interpreter we have no offset table for.** Refuse and count.
- **PyPy, MicroPython, or CPython embedded in a differently-named binary.**
- **Removing `CAP_SYS_PTRACE` from the symbolization path.** See "Capabilities",
  which is a deliberate reversal of one of #83's four stated reasons.

## What was surveyed, and what it settled

Three implementations were read before writing this, because the offset story
and the version story *are* the design and guessing at them is how this class of
profiler produces confidently wrong frames.

| | OTel / Parca | Pyroscope `pyperf` | py-spy |
|---|---|---|---|
| Names read in | userspace | BPF | userspace |
| Per-sample process reads | `process_vm_readv` | none | all |
| `CAP_SYS_PTRACE` | per sample | attach only | yes, plus real attach |
| Line numbers | yes | **no** | yes |
| Native + Python in one stack | **yes** | **no — mutually exclusive** | no |
| Status | active | **deleted 2025-07-29** | active |

Three findings decided this design:

1. **Parca is OTel.** `parca-agent/go.mod:191` replaces
   `go.opentelemetry.io/ebpf-profiler` with a parca-dev fork and carries no
   Python unwinder of its own. There are two independent designs in the world,
   not three.
2. **Pyroscope cannot express the stack we need.** Profiling type is chosen per
   process by executable name (`session.go:694`); the Python path never calls
   `bpf_get_stackid` for the user stack. You get Python frames or native frames,
   never both — no numpy, no libc, no CUDA. That is disqualifying here whatever
   its other merits.
3. **Grafana abandoned their own design for OTel's.** Alloy's `pyroscope.ebpf`
   now imports `go.opentelemetry.io/ebpf-profiler/interpreter/python`. The
   removal commit states no rationale, so none is claimed here; the fact is two
   lines of `go.mod` and it is the strongest behavioural evidence available.

A fourth finding corrected an assumption made while designing: **reading strings
in BPF does not avoid ptrace.** Pyroscope reads names in BPF and still opens
`/proc/<pid>/mem` at attach (`ebpf/python/tss.go:11`) for the pthread TSS key,
because that key lives in `_PyRuntime`'s writable data and is assigned at
interpreter init, so it cannot come from the ELF on disk. Interning moves ptrace
off the sampling path; it does not remove it.

## Architecture

### The walk lives inside `walk_step`

`bpf/unwind_common.h`'s `walk_step` is already the per-frame `bpf_loop` callback
for the hybrid native walker, and it already calls `mapping_for_pc()` to
classify every PC. Recognising "this PC is inside the interpreter's eval loop"
is one more arm of that same lookup.

This placement is the design's cheapest and most consequential decision.
`perf_dwarf.bpf.c:88` and `gpu_usdt.bpf.c:487` both drive the walker with
`bpf_loop(MAX_FRAMES, walk_step, &walker, 0)`. Putting the hook inside
`walk_step` means the **CUDA launch probe inherits Python frames with no second
integration** — which is exactly what #83 warns is "nearly free now and awkward
later".

### Switching native → interpreter

A global BPF hash holding the text range of `_PyEval_EvalFrameDefault`, keyed
by the **binary**, not by PID. Our walker already resolves a PC to a
`(table_id, rel_pc)` pair through `mapping_for_pc()` for CFI lookup, and
`table_id` is per-binary — so the interpreter range keys off the same identifier
the walker has already computed for that frame, and every process running the
same libpython shares one entry. Userspace resolves the symbol once per distinct
interpreter and installs it.

Two details from OTel that are not optional:

- **The `.cold` split range.** Compilers emit `_PyEval_EvalFrameDefault.cold`
  outside the main body. OTel recovers it by following a relative jump
  (`python.go:1030 findColdRange`). Without it the switch is silently missed on
  some builds, which presents as "Python frames sometimes just do not appear".
- **A done-bit per unwind.** Once the Python chain for a thread is exhausted,
  re-entering the eval loop deeper in the same stack must not restart it
  (OTel: `unwinder_is_done`, `tracemgmt.h:432`).

### The frame chain

Per eval-loop entry, walk `_PyInterpreterFrame.previous` and **stop at the entry
frame** — `owner == FRAME_OWNED_BY_CSTACK` — then hand back to native unwinding.

Walking to `NULL` instead is the single most expensive mistake available here:
it consumes the entire Python chain in one go and then terminates the trace,
losing every native frame below the interpreter. OTel's ≤3.10 path does exactly
that, and their own 3.10 coredump fixture ends at `<module>` with no `main` and
no libc beneath it.

### Thread state comes from TLS, not from `PyRuntimeState`

**#83's stated traversal is wrong for a sampling profiler.** It proposes
`PyRuntimeState → PyThreadState → _PyInterpreterFrame`. Walking the
runtime-global thread list yields *a* thread, not the one that was sampled.

Both reference implementations instead read the current thread's
`PyThreadState` out of thread-local storage, and both end up at pthread TSD.
They differ in how the offsets are found, and **a spike settled which mechanism
we use**.

- **OTel** disassembles `_PyThreadState_GetCurrent` to recover a TLS offset.
- **Pyroscope** recovers the TSS *key* and reimplements `pthread_getspecific` in
  BPF against glibc/musl struct offsets (`bpf/pthread_amd64.h:21`), reading the
  TLS base from `task_struct.thread.fsbase`.

**We take Pyroscope's, on measured evidence.** Disassembling four real builds:

| build | `_PyThreadState_GetCurrent` | TLS model |
|---|---|---|
| 3.11 Debian | absent | — |
| 3.12 Debian | `call __tls_get_addr@plt` | general dynamic |
| 3.13 Debian | `call __tls_get_addr@plt` | general dynamic |
| 3.14 Fedora | `call *(%rax)`; `mov %fs:(%rax)` | TLSDESC |

**No shared-library build in the range carries a static fs-relative TLS offset
to extract.** The offset is produced by a runtime call on every one of them.
Distro and container images all ship shared libpython, so this is the common
case, not the exotic one. (This is not a claim that OTel is broken — they may
decode the `__tls_get_addr` argument pattern — only that the simple offset
extraction is not available here.)

The TSS-key route, by contrast, is trivially extractable and **identical in
shape across the whole supported range**:

```
3.12:  mov 0x34e1fc(%rip),%rax ; cmpl $0x0,0x608(%rax) ; lea 0x608(%rax),%rdi ; jmp PyThread_tss_get
3.13:  mov 0x349c12(%rip),%rax ; cmpl $0x0,0x870(%rax) ; lea 0x870(%rax),%rdi ; jmp PyThread_tss_get
3.14:  mov 0x4714e3(%rip),%rax ; cmpl $0x0,0x920(%rax) ; lea 0x920(%rax),%rdi ; jmp PyThread_tss_get
```

Same 35-byte function, same eight instructions, same encodings. Only the
displacement and the `autoTSSkey` offset move. That offset is therefore
**parsed from the binary, not tabled** — one parser, no per-version entry, and
it survives distro patching for free.

This remains the design's largest implementation risk, but the spike has moved
it from "unknown mechanism" to "known mechanism, unknown edge cases" — musl and
statically linked CPython are both untested.

### Frame records

Following OTel, BPF emits **one ordered stack containing both kinds**, because
the kernel is the only party that knows the interleaving. Per Python frame, two
words cross the boundary:

- the `PyCodeObject` address, and
- `fingerprint | (f_lasti << 32)`, where the fingerprint is a cheap 32-bit hash
  of `co_argcount`, `co_kwonlyargcount`, `co_flags` and `co_firstlineno`.

No names and no line numbers cross from the kernel. Line numbers **cannot** be
computed in BPF — the location-table walk is unbounded.

This requires the one genuinely invasive change in this design: `sample_record.pcs[]`
is a flat `__u64[MAX_FRAMES]` today and must become tagged so a Python frame can
occupy two words. Everything that reads `sample_record` is affected, including
the GPU path.

### Symbolization, and the fingerprint's honest status

Userspace resolves `(address, fingerprint)` to `co_qualname` / `co_filename` /
line by reading the live process, caching by address, and **accepting a cache
entry only when the fingerprint matches**.

The fingerprint is a collision heuristic, not an identity. Upstream's own
comment says the fields are chosen "in the *hope* that no collisions occur". A
freed-and-reused code object whose four fields happen to match will be
mis-symbolized. This is inherited deliberately, and it is recorded here so it is
a known residual risk rather than a surprise.

## Capabilities — a deliberate reversal

#83 gives four reasons to replace the trampoline. This design keeps three and
**abandons the third**: symbolization reads target memory with
`process_vm_readv`, which needs `PTRACE_MODE_ATTACH_REALCREDS`, i.e.
`CAP_SYS_PTRACE` for cross-uid targets and — under `yama/ptrace_scope >= 1` —
even for same-uid non-descendants.

The three surviving reasons are sufficient on their own: it stops mutating the
process under measurement, it works below 3.12, and it can profile what was
already running.

Consequences that must be built, not assumed:

- **Everything except Python symbolization still runs on
  `cap_bpf,cap_perfmon,cap_checkpoint_restore`.** The agent must not require
  ptrace to start.
- **Without ptrace, Python frames degrade to unsymbolized and say so.** A frame
  that could not be resolved for want of a capability must be distinguishable in
  the profile from one that failed for any other reason. Silence here would be
  the same defect this project refuses everywhere else: a profile that looks
  thin rather than one that reports it was not permitted to look.
- README's capability section becomes conditional and must say which capability
  buys what.

## Offsets and versions

**Hand-maintained tables in Go**, one per 3.12 / 3.13 / 3.14, written
into a per-PID BPF hash at attach — the same delivery both references use. The
`pids` map in `unwind_common.h:224` is already a per-PID hash written from Go at
attach; its `pid_config` value is three bytes today and grows, or gains a
sibling map, to carry the offset block.

Two things adopted from the references:

- **Feature flags, not version comparisons in C.** OTel moved to this
  deliberately (`dc766c6`). The BPF program should never contain a version
  number.
- **`tp_members` introspection where CPython offers it.** `PyCodeObject` and
  friends expose member offsets through their type object's `tp_members` array,
  readable from the ELF with no ptrace and no debuginfo. Offsets obtained this
  way survive distro patching and cost nothing, so tables cover only what
  introspection cannot reach.

**Version detection**: the `libpython3.X` soname, falling back to the
`Py_Version` hex constant read from the ELF. An interpreter whose version is
undetected, or detected but untabled, is **refused loudly and counted** — never
walked with the nearest table. Note the `autoTSSkey` offset is deliberately NOT
in these tables: it is parsed per binary (see thread state, above). `_Py_DebugOffsets` (3.12+) is a documented
alternative source rejected for now to keep one mechanism; if the tables become
a burden it is the first thing to revisit.

**Validation before trust.** #83 is explicit that a wrong offset produces
silently wrong frames, which is worse than none. Before a table is used for a
process, walk one frame and check the result is self-consistent: pointers
readable, `owner` within its enum, the chain terminating. A table that fails
validation is refused for that process and counted, not used with a shrug.

## Testing

- **Offset tables are pinned against real interpreters**, not against
  themselves. A test that asserts a Go constant equals the literal it was
  written from proves nothing; this project has shipped that mistake before.
  Fixtures are real CPython binaries per supported minor.
- **Validation is mutation-tested**: corrupt one offset per table and assert the
  validator refuses. A validator that cannot fail is the failure mode here.
- **Interleaving is asserted on a known stack**, PyTorch-shaped: Python frame,
  native frames, Python frame, terminating in a real root.
- **The no-ptrace path is a first-class test**, asserting frames are present,
  unsymbolized, and *labelled* as capability-limited.
- **The GPU path gets its own case**: a Python frame reaching the launch site
  through `gpu_usdt.bpf.c`, since that is the reason for the whole design and it
  would otherwise only be exercised by hand.

## Staging

Three slices, ordered so each de-risks the next rather than by convenience.

1. **Tagged frame records.** Change `sample_record.pcs[]` to tagged frames with
   no Python code anywhere. Existing native and GPU tests must stay green. This
   is the change with real blast radius and it lands alone.
2. **The walk, opaque.** TLS/TSD resolution, the eval-range switch, the frame
   chain, entry-frame termination — emitting addresses, no symbolization.
   Success is a PyTorch flame graph with correctly *placed* Python frames whose
   names are still numbers. This proves the riskiest machinery before anything
   is built on it.
3. **Symbolization.** Userspace resolution, the fingerprint cache, the
   capability degradation and its disclosure.

The TLS offset extraction in slice 2 is the piece most likely to behave
differently on a real distro build than in a design document. It deserves a
spike against a real interpreter before the plan for that slice is written.

## Open risks

- **TLS/TSD extraction is instruction-pattern matching against compiler output**
  and the spike settled the mechanism (TSS key, one shape for 3.12+) but not its
  edges: **musl and statically linked CPython are untested**, and the BPF-side
  `pthread_getspecific` reimplementation depends on glibc struct offsets that
  Pyroscope carried per-libc-version. Still the highest risk item.
- **The fingerprint is a hope, not an identity** (above).
- **`MAX_FRAMES` is 127 and Python frames cost two words.** A deep Python stack
  now consumes the budget twice as fast. Whether 127 stays adequate is a
  measurement, not an assumption.
- **`walk_step` complexity.** It already performs four `bpf_probe_read_user`
  calls per frame inside a `bpf_loop`. The verifier budget after adding the
  interpreter arm is unknown and must be measured early — Pyroscope's unrolled
  equivalent reached 12,754 instructions, which is the wrong end of the scale to
  discover late.

## Worth taking from Pyroscope

**Class-name recovery.** Inspect the first argument (`self` / `cls`) in
`localsplus[0]`, chase `ob_type->tp_name`, with a fallback for a closured `self`
in a cell. OTel does not do this; `Flask.run` reads better than `run`, and for
PyTorch `Module.forward` is far more useful than `forward`. Additive,
independent of everything above, and correctly a later slice.
