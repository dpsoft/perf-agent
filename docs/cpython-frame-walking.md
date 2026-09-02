# Reading CPython's frame chain from outside the process

Notes gathered while building the BPF-side Python unwinder (issue #83).
Everything here was checked against CPython source and, where a number is
quoted, measured against real interpreter binaries on this host. It is
written down because most of it is not in any CPython document, and three
of the items below contradict what the widely-copied out-of-process
profilers do.

Supported range is CPython 3.12, 3.13 and 3.14, amd64 only.

---

## The authority is CPython's own out-of-process reader

`Modules/_remote_debugging_module.c` landed in 3.14 and is the backing
implementation for `sys.remote_exec` / PEP 768. It does exactly what a
profiler does — walk `PyThreadState` and `_PyInterpreterFrame` from
another process, over a memory-read primitive — except it is *in the
tree*, compiled against the very structs it decodes, and updated in the
same commit whenever those structs change.

That makes it a better oracle than any third-party profiler, including
the ones this design took inspiration from. Where an external project and
`_remote_debugging_module.c` disagree, we follow the module and cite the
line. Two of the corrections below came from doing that.

It is also the reason the supported floor is 3.12 rather than 3.11: the
module gives us a checkable reference for 3.14, and the 3.12/3.13 layouts
are close enough to it to reason about. Below 3.12 we would be guessing.

---

## The frame owner enum is renumbered in 3.14, and the fix is not the obvious one

`_PyInterpreterFrame` carries a one-byte `owner` field drawn from
`enum _frameowner`. The walk has to know when it has reached the frame
that represents the boundary back into C, because that is where the
Python chain ends and native unwinding resumes.

Through 3.13 that frame's owner is `FRAME_OWNED_BY_CSTACK`, which is 3.

In 3.14 the enum gains a new member, `FRAME_OWNED_BY_INTERPRETER`,
inserted *before* `CSTACK`. The natural conclusion — and the one we
initially wrote into the plan — is that the sentinel became 4 and the
walker needs a per-version constant.

That is wrong. CPython moved the boundary frame onto the new enumerator
in the same change: 3.14's `ceval.c` sets the entry frame's owner to
`FRAME_OWNED_BY_INTERPRETER`, which is *also* 3. `FRAME_OWNED_BY_CSTACK`
at 4 is assigned nowhere in the 3.14 tree — it is vestigial.

So the sentinel value never moved. What changed is the name attached to
it. CPython's own code does not test either name for equality; both
`Python/ceval.c` and `_remote_debugging_module.c` test

```c
owner >= FRAME_OWNED_BY_INTERPRETER   /* i.e. owner >= 3 */
```

which is correct on every version in range and needs no version
dispatch at all. We do the same.

The cost of getting this wrong is not a crash. A walker testing
`owner == 4` on 3.14 never sees its stop condition, walks off the top of
the chain into whatever the entry frame's `previous` pointer holds, and
either truncates on a failed read or emits garbage frames — silently, and
only on the newest interpreter.

---

## `f_executable` is a tagged pointer on 3.14

3.14 converts the frame's code-object reference to `_PyStackRef`, a tagged
pointer: low bits carry deferred-reference-counting state and must be
cleared before the value is dereferenced. On 3.12 and 3.13 the field is a
plain `PyCodeObject *` and must not be masked. We carry a per-process
boolean rather than inferring it.

The mask is `& ~3`, matching `CLEAR_PTR_TAG` in
`Modules/_remote_debugging_module.c:45`, which clears `Py_TAG_BITS`.

The subtlety worth writing down is that `Py_TAG_BITS` is **not one value**.
In `Include/internal/pycore_stackref.h` (3.14.3) it is:

| Build | `Py_TAG_BITS` | Where |
|---|---|---|
| default (with GIL) | `3` | line 446 |
| `Py_GIL_DISABLED` (free-threaded) | `1` | line 271 |
| `Py_STACKREF_DEBUG` | `0` | line 56 |

Two independent readings of CPython disagreed here — `& ~1` versus `& ~3` —
and both were quoting the source correctly; they were quoting different
`#if` arms. This is the kind of disagreement that looks like one party
being sloppy and is actually the source being conditional.

`& ~3` is correct for all three builds, because a `PyCodeObject *` is
8-aligned: bits 0-2 are zero in any valid pointer, so clearing bits 0-1
removes the tag where there is one and changes nothing where there is not.
We use the widest of the three deliberately, rather than switching on the
build, and that is a decision rather than an accident.

One consequence that is easy to miss: `PyStackRef_None` is
`((uintptr_t)&_Py_NoneStruct) | Py_TAG_REFCNT` (`pycore_stackref.h:460`).
The `Py_None` comparison described below therefore has to happen *after*
the mask, or it never matches on 3.14.

## 3.12 has an extra indirection that 3.13 removed

Getting from a `PyThreadState` to its current frame is:

- **3.12** — `tstate->cframe->current_frame`. `_PyCFrame` is a separate
  structure and the thread state holds a pointer to it.
- **3.13, 3.14** — `tstate->current_frame`, read directly.

`_PyCFrame` was removed in 3.13. This is a real pointer hop, not an
offset difference, so it cannot be folded into an offset table — the
walker branches on a per-process flag.

---

## `Py_None` in `f_executable` is a backstop, not the stop condition

From 3.13 onward the frame at the C boundary holds `Py_None` in
`f_executable` rather than a `PyCodeObject *`, and it is tempting — we did
this at first — to describe that as how a walker recognises the boundary.

It is not, and the distinction matters for anyone reading the counter.
The owner test above (`owner >= 3`) already stopped at that frame, before
`f_executable` is ever read. So a `Py_None` executable can only be observed
on a frame whose owner is *below* the boundary — which means a frame caught
mid-push, i.e. a torn read of a live interpreter.

That makes the check a genuinely useful defensive screen — it is what
CPython's own `is_frame_valid` does at
`Modules/_remote_debugging_module.c:2142-2144` — but its counter should
read approximately zero in production. Do not let a zero there be
interpreted as "the check never runs, so it can be removed". It runs; it is
supposed to find nothing.

On 3.12 there is no such frame at all: the entry frame carries
`interp->interpreter_trampoline`, a real code object, so the recorded
`Py_None` address is zero and the comparison is dead by construction.

Note also that on 3.14 `PyStackRef_None` carries a tag bit
(`pycore_stackref.h:460`), so this comparison must be made *after* the mask
described above or it never matches.

## The thread-state TSS key is the *second* field of `Py_tss_t`

To find the `PyThreadState` for the thread we sampled, we resolve
CPython's autoTSS key and read the corresponding glibc TSD slot.

`Py_tss_t` is

```c
typedef struct {
    int _is_initialized;
    pthread_key_t _key;
} Py_tss_t;
```

Both members are 4 bytes and `pthread_key_t` is `unsigned int`, so on
amd64 the struct is 8 bytes with the *flag* in the low half and the
*key* in the high half. Reading the 8-byte word and truncating to 32 bits
yields `_is_initialized`, not the key.

This bug is nearly invisible in testing. An initialized key has
`_is_initialized == 1`, so on a process whose autoTSS key happens to be
allocated as 1 — which is common, since CPython allocates it early — the
wrong read returns the right answer. It was caught by dumping
`/proc/<pid>/mem` at `_PyRuntime + <offset>` on a live interpreter and
seeing `0x0000000100000001`: the two halves are only distinguishable when
they differ.

The corresponding trap in the *test* is worse: a fixture that writes
`{0, 0}` for the key encodes `Py_tss_NEEDS_INIT`, which is
self-consistent with the buggy read and passes either way. A regression
test here has to use a key value that differs from 1 and from the flag.

---

## We do not parse the offsets out of DWARF

Offsets come from hand-maintained tables keyed by interpreter version,
validated at attach time by plausibility checks against the live process.
The alternative — parsing debug info at attach — costs a DWARF dependency
in the hot path and is unavailable exactly when it matters most, on the
stripped interpreters that ship in production images.

The autoTSS key's *address* is the exception. It is not a struct offset
but a location inside `_PyRuntime`, and it is recovered by decoding the
fixed 35-byte body of `PyGILState_GetThisThreadState` positionally —
a technique that is stable across the supported range because the
function is small enough to compile identically. The decoder matches
opcodes at fixed positions rather than scanning, so a reordered or
differently-compiled body is rejected rather than misread.

---

## Where this diverges from the profilers we read

The design took its overall shape from OpenTelemetry's eBPF profiler,
which is the most complete BPF-side Python unwinder in the open. Four
places where we ended up somewhere different:

1. **The owner sentinel.** Described above. Testing `>= 3` rather than
   equality removes the version dispatch entirely.
2. **The tagged-pointer mask.** Settled against
   `_remote_debugging_module.c` rather than against another profiler.
3. **The chain is resumed, not restarted.** A frame's `previous` link
   runs *through* the C boundary — 3.14's `ceval.c:1169-1171` sets
   `entry.frame.previous = tstate->current_frame` before installing the
   new frame — so a Python → C-extension → Python stack enters the eval
   loop twice in one sample. A walker that restarts from
   `tstate->current_frame` each time it recognises an eval-loop PC pushes
   the innermost segment again beneath the outer one's native position.
   The result is duplicated frames in a plausible-looking order, which is
   worse than an obvious failure. We keep a per-sample cursor and resume
   from where the previous segment stopped.
4. **Failure accounting.** Every refusal, miss and truncation on the
   Python path increments a named per-CPU counter that is readable from
   userspace. A Python unwinder that quietly declines to walk looks
   identical, in a flame graph, to a program that was not running Python
   — which is the failure mode most likely to waste a user's afternoon.

Whether any of this is worth reporting upstream is an open question to
settle once the implementation has run against real workloads. The
renumbered-enum trap is the strongest candidate: it is a genuine
correctness hazard for every out-of-process Python profiler, it is not
documented anywhere we found, and the safe formulation is already what
CPython itself uses.
