// python_walk.h — reaching the current thread's PyThreadState from BPF and
// walking the CPython frame chain it names.
//
// THIS FILE IS NOT PART OF THE CORE WALKER AND IS NOT REACHABLE FROM IT.
// It is compiled into python_walk.bpf.c, its own BPF object, whose programs
// sit in the drivers' interp_progs tail-call table. The core walks native
// frames, finds a PC inside a range something else claimed (handoff_ranges),
// stops there and tail-calls the claimant. Everything below -- the maps, the
// per-version offsets, the glibc TSD reimplementation, the counters -- belongs
// here and appears in perf_dwarf.bpf.c, offcpu_dwarf.bpf.c, gpu_usdt.bpf.c and
// unwind_common.h nowhere at all.
//
// The only thing shared with the core is bpf/unwind_record.h: the record both
// sides append to, the persisted walk state, and the three maps that carry
// them across the tail call.
//
// CPython stores the PyThreadState in a pthread TSD slot, not in a global.
// The spike (see docs/superpowers/plans/2026-08-29-python-walker-slice1-2.md)
// found no shared-library CPython build in the supported range carries a
// static TLS offset that can be recovered by disassembly -- 3.12 and 3.13
// call __tls_get_addr, 3.14 uses TLSDESC -- so there is nothing to extract
// that way. Instead we take the TSS *key*, whose offset comes out of
// PyGILState_GetThisThreadState (see pyunwind's autoTSSkey parser), and do
// the pthread lookup here, in BPF, against the target's own thread pointer.
//
// A second measurement (the "spike before Task 5" in the plan) confirmed the
// TSS slot is actually POPULATED -- not just extractable -- on 3.12.14,
// 3.13.15 and 3.14.3: PyGILState_GetThisThreadState() and the authoritative
// PyThreadState_Get() returned identical non-NULL pointers on all three. So
// this is the mechanism for the whole 3.12-3.14 range; there is no
// static-TLS fallback to implement.
//
// This is Pyroscope's mechanism (bpf/pthread_amd64.h). It depends on glibc's
// internal struct layout, which is why pthread_specific1stblock and its
// neighbors below are supplied per-process from userspace rather than
// hardcoded here -- this program cannot tell which libc it is looking at.
#ifndef PERF_AGENT_INTERP_PYTHON_WALK_H
#define PERF_AGENT_INTERP_PYTHON_WALK_H

#ifndef PERF_AGENT_UNWIND_RECORD_H
#include "unwind_record.h"
#endif

// INTERP_ID_PYTHON is this module's unwinder id: the value userspace writes
// into handoff_ranges for a libpython, the interp_progs slot the driver
// tail-calls, and the tags[] byte every Python frame pair carries.
//
// It is 1 because FRAME_TAG_PYTHON was 1 before the walker moved out here
// (issue #83), and the wire format does not change just because the code did.
// Mirrored by pyunwind.UnwinderID on the Go side.
#define INTERP_ID_PYTHON 1

// struct py_proc_info is the per-PID record userspace writes after
// validating a target's CPython build. It carries two unrelated things
// under one map value because both are needed before a single Python frame
// can be read, and both come from the same one-time per-process setup:
//
//   1. What py_tss_get needs to turn a TSS key into a PyThreadState*
//      (tss_key and the four pthread_* glibc offsets).
//   2. The _PyInterpreterFrame / PyThreadState / PyCodeObject layout for
//      this process's CPython minor version -- carries the SAME SET of
//      fields as pyunwind.Offsets (pyunwind/offsets.go), at the SAME
//      WIDTHS, mapped by NAME, not by position. The declaration order
//      below is deliberately NOT pyunwind.Offsets's order: fields are
//      regrouped by width (u64 block, then u32 block, then u16 block,
//      then u8 block) so the struct packs to 56 bytes with zero implicit
//      padding beyond the one trailing block that rounds the struct up to
//      its own 8-byte alignment (forced by none_addr below) -- see the
//      _Static_assert below. Do not "restore" positional order to match
//      the Go side; that would reintroduce padding and trip the assert.
//      A mismatch in the SET or a WIDTH between this struct and its Go
//      counterpart writes garbage offsets into the map, which produces a
//      plausible-looking stack of wrong frames rather than a loud failure
//      -- the exact failure this whole design exists to prevent. If a
//      field is added on one side, add the same-named, same-width field
//      on the other side (position is free; grouped by width here).
struct py_proc_info {
    // ----- CPython 3.13+ sentinel. Needs 8-byte alignment, so it gets its
    // own block ahead of the u32 block (the same "regroup by width, widest
    // first" rule the rest of this struct already follows).
    //
    // From 3.13 onward the top C-entered frame's f_executable is Py_None
    // rather than a code object, so the walker must recognise it and stop
    // there instead of masking/dereferencing it as a code pointer. This is
    // the absolute address of that process's _Py_NoneStruct, resolved from
    // its dynsym entry at attach. Zero on 3.12, where it is not needed and
    // never read.
    __u64 none_addr;

    // ----- pthread TSD lookup (py_tss_get). Filled once at attach, from
    // the target's own libc.
    __u32 tss_key;                    // value read from the target at attach
    __u32 pthread_specific1stblock;   // glibc struct offset
    __u32 pthread_key_data_size;      // sizeof(struct pthread_key_data)
    __u32 pthread_key_data_off;       // offsetof(struct pthread_key_data, data)
    __u32 pthread_size;               // arm64 only; 0 on x86

    // ----- _PyInterpreterFrame layout. See pyunwind.Offsets for provenance
    // and the per-version literals.
    __u16 frame_previous;             // struct _PyInterpreterFrame *previous
    __u16 frame_executable;           // f_code (3.12) / f_executable (3.13+)
    __u16 frame_instr_ptr;            // prev_instr (3.12) / instr_ptr (3.13+)
    __u16 frame_owner;                // char owner

    // ----- PyThreadState layout.
    __u16 threadstate_frame;          // current_frame, or `cframe` when
                                       // threadstate_frame_indirect is set

    // ----- PyCodeObject layout.
    __u16 code_argcount;
    __u16 code_kwonlyargcount;
    __u16 code_flags;
    __u16 code_firstlineno;

    // ----- Layout facts a byte offset alone can't express.
    __u8  frame_owner_max;            // highest valid _frameowner raw value
                                       // for this version (3 on 3.12/3.13,
                                       // 4 on 3.14). py_seg_step refuses
                                       // an owner byte above it.
    __u8  frame_owner_cstack;         // this version's FRAME_OWNED_BY_CSTACK
                                       // (3 on 3.12/3.13, 4 on 3.14).
                                       // Carried for completeness of the
                                       // mirror; the stop condition uses
                                       // frame_owner_boundary, NOT this --
                                       // see py_seg_step.
    __u8  frame_owner_boundary;       // lowest owner value that means "this
                                       // frame is the interpreter's C
                                       // boundary, not Python code": 3 on
                                       // every supported version, but
                                       // measured from a DIFFERENT
                                       // enumerator per version. See
                                       // pyunwind.Offsets.FrameOwnerBoundary
                                       // and py_seg_step.
    __u8  frame_executable_tagged;    // 1 if frame_executable holds a tagged
                                       // _PyStackRef (3.14) needing masking
                                       // before use as a PyObject*
    __u8  threadstate_frame_indirect; // 1 if threadstate_frame is the offset
                                       // of a `cframe` pointer whose own
                                       // offset-0 field is current_frame
                                       // (3.12), rather than current_frame
                                       // itself (3.13+)

    __u8  enabled;                    // 0 until validation passed
    __u8  _pad[4];                    // explicit trailing padding: none_addr's
                                       // u64 alignment forces sizeof() up to a
                                       // multiple of 8; spelled out rather than
                                       // left to the compiler, same as every
                                       // other byte in this struct.
};

// Pinned so a change to either side of the Go/C mirror (see the struct
// comment above) fails the build loudly instead of writing offsets at the
// wrong byte position. Update alongside pyProcInfo in the Go pyunwind
// package -- keep both edits in the same commit.
_Static_assert(sizeof(struct py_proc_info) == 56,
               "py_proc_info must mirror pyProcInfo in the Go pyunwind package field-for-field; update both together");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u32);               // pid
    __type(value, struct py_proc_info);
    __uint(max_entries, 1024);
} py_procs SEC(".maps");

#define PY_TSS_KEYS_PER_BLOCK 32

// py_tss_get reaches the current thread's PyThreadState* by reimplementing
// glibc's pthread_getspecific against a TSS key, entirely from BPF: read the
// thread pointer out of the current task's arch-specific TLS field, then
// index into pthread's first TSD block at the userspace-supplied offsets in
// pi. Returns 0 (NULL) on any failure to read or on an out-of-range key --
// never a garbage pointer.
static __always_inline __u64 py_tss_get(__u32 key, struct py_proc_info *pi) {
    // pi is normally a bpf_map_lookup_elem result; the verifier requires
    // that be checked before any dereference (pthread_size, ->
    // pthread_specific1stblock, etc. below all go through it), so this
    // helper checks it itself rather than trusting every future caller to.
    if (!pi) return 0;

    // Only the first TSD block is supported. glibc's specific_1stblock holds
    // 32 keys and CPython's autoTSSkey is in practice 0; a key past the block
    // would need the second-level array walk. Counted, not guessed at.
    if (key >= PY_TSS_KEYS_PER_BLOCK) return 0;

    __u64 tls_base = 0;
    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    if (!task) return 0;
#if defined(__TARGET_ARCH_x86)
    if (bpf_probe_read_kernel(&tls_base, sizeof(tls_base), &task->thread.fsbase)) return 0;
#elif defined(__TARGET_ARCH_arm64)
    if (bpf_probe_read_kernel(&tls_base, sizeof(tls_base), &task->thread.uw.tp_value)) return 0;
    tls_base -= pi->pthread_size;
#else
    return 0;
#endif

    // pthread->specific_1stblock[key].data
    __u64 slot = tls_base + pi->pthread_specific1stblock
               + (__u64)key * pi->pthread_key_data_size + pi->pthread_key_data_off;
    __u64 val = 0;
    if (bpf_probe_read_user(&val, sizeof(val), (void *)slot)) return 0;
    return val;
}


// ----- Why the Python arm did or did not produce frames.
//
// A dedicated per-CPU counter array rather than more walker_flags bits: all
// eight bits of sample_header.walker_flags are allocated (see the flag block
// in unwind_common.h), and a bit could only say "something went wrong" once
// per sample anyway, where these count every occurrence and name it.
// Per-CPU so the increment needs no atomic; userspace sums the per-CPU
// values (pyunwind.ReadWalkCounters).
//
// Slot 0 is the SUCCESS count, deliberately: a counter set that can only go
// up when something breaks cannot distinguish "the arm never fired" from
// "the arm fired and worked", and that is the reading an operator needs
// first.
//
// UNITS -- these are not all the same thing, and mixing them silently is its
// own trap:
//
//   slot 0 (FRAMES_PUSHED)  counts FRAMES. One per Python frame that reached
//                           the record, so it grows with stack depth.
//   slots 1..8 (everything  count SAMPLES. Each is bumped at most once per
//   except 9)               sample, and that is a structural property, not a
//                           convention: every one of those paths leaves the
//                           sample in PY_CHAIN_DONE and raises this module's
//                           bit in walk_persist.interp_done, after which the
//                           core stops offering it frames (next_unwinder in
//                           unwind_common.h). So a deep native stack that
//                           lands on the eval loop twenty times still
//                           contributes at most one.
//   slot 9 (CHAIN_          counts SAMPLES too, by a different mechanism --
//   ABANDONED)              a provisional increment that is withdrawn if the
//                           segment is resumed. See its declaration below.
//
// The useful ratios therefore read: FRAMES_PUSHED / (samples with Python) is
// mean Python depth, and every other slot over the sample count is a rate of
// occurrence. Reading slot 0 against the others as if they shared a unit
// would make a deep stack look like a failure storm.
#define PY_CNT_FRAMES_PUSHED     0  // one Python frame pair reached the record
#define PY_CNT_TSS_MISS          1  // the TSD slot held no PyThreadState
#define PY_CNT_NO_PROC_INFO      2  // an eval-loop PC in a process with no
                                    // validated, enabled py_procs entry
#define PY_CNT_TSTATE_READ_FAIL  3  // current_frame (or 3.12's cframe hop)
                                    // could not be read
#define PY_CNT_FRAME_READ_FAIL   4  // an _PyInterpreterFrame field faulted
#define PY_CNT_OWNER_IMPLAUSIBLE 5  // owner byte above this version's enum
#define PY_CNT_CHAIN_TRUNCATED   6  // a segment was still in progress when the
                                    // record filled up mid-chain
#define PY_CNT_PUSH_REFUSED      7  // record_push_interp had no room left
#define PY_CNT_NONE_EXECUTABLE   8  // f_executable was NULL or Py_None on a
                                    // frame the owner test did not already
                                    // stop at: a torn-read backstop, expected
                                    // to read zero (see py_seg_step)
#define PY_CNT_CHAIN_ABANDONED   9  // the NATIVE walk stopped while a Python
                                    // cursor was still live; see below
#define PY_CNT_MAX               10

// PY_CNT_CHAIN_ABANDONED is a NATIVE-WALK OUTCOME WITH A PYTHON COST, not a
// Python-side failure. Nothing in the Python walk went wrong; the walk simply
// never got another turn.
//
// The cursor goes live when a segment ends at an interpreter entry frame whose
// `previous` is non-NULL -- meaning there is an OUTER Python segment waiting
// below the C code -- and it is consumed the next time the native walk lands
// on a PC inside the eval loop. If the native walk ends first, that segment is
// absent from the sample and every other counter here reads clean.
//
// HOW IT IS COUNTED, AND WHY LIKE THAT. Nothing in this module knows the
// native walk has finished: the module is a tail-call target that appends a
// segment and hands control straight back. Asking the question from the driver
// would mean the driver knowing this module exists, which is the coupling the
// whole separation removes. So it is counted PROVISIONALLY -- incremented when
// the cursor is left live, and withdrawn (py_count_undo) when a later dispatch
// in the same sample consumes it. What remains at read time is exactly the
// number of samples whose outer segment was never reached.
//
// The withdrawal is sound because both the increment and its withdrawal happen
// inside ONE program invocation chain, on ONE CPU, and these are per-CPU
// counters: a tail call does not migrate, so the decrement can only ever
// cancel an increment this CPU made moments earlier and can never underflow.
//
// THE JOIN A READER NEEDS. This never arrives alone -- the record's
// walker_flags always say why the native walk stopped, and the flags separate
// two quite different causes:
//
//   * With an early-stop bit -- WALKER_FLAG_FP_EXHAUSTED (0x10),
//     WALKER_FLAG_FP_NONMONOTONIC (0x20), WALKER_FLAG_CFI_MISS (0x04) or
//     WALKER_FLAG_FRAME_PUSH_REFUSED (0x80) -- the native unwinder gave out
//     or ran out of room. The Python frames are collateral damage and the fix
//     is on the native side (unwind tables, MAX_FRAMES).
//
//   * With a clean ending instead -- WALKER_FLAG_FP_TERMINATED (0x01) and/or
//     WALKER_FLAG_RA_UNDEFINED (0x08), no early-stop bit -- the native walk
//     reached the root and simply never recognised the outer segment's
//     eval-loop PC. That points at the handoff range: a libpython whose range
//     was never installed, or an eval loop entered through a PC outside the
//     installed range. This is the reading that matters, because everything
//     else about such a sample looks perfect.
//
// Being a native outcome, it is expected to be non-zero on any real workload
// with deep stacks. It is a cost, not an error rate: read it against
// PY_CNT_FRAMES_PUSHED to see what fraction of the Python stack is being lost.

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, PY_CNT_MAX);
    __type(key, __u32);
    __type(value, __u64);
} py_walk_counters SEC(".maps");

// py_count bumps one named slot. Takes no ctx and needs none -- see
// count_walk_error in gpu_usdt.bpf.c, the same idiom.
static __always_inline void py_count(__u32 slot) {
    if (slot >= PY_CNT_MAX) return;
    __u64 *d = bpf_map_lookup_elem(&py_walk_counters, &slot);
    // Per-CPU value: this CPU is the only writer, so a plain increment is
    // correct and cheaper than __sync_fetch_and_add.
    if (d) *d += 1;
}

// py_count_undo withdraws one provisional increment. Its ONLY caller is the
// resume path of PY_CNT_CHAIN_ABANDONED, whose whole mechanism is documented
// at that constant; the guard against a decrement below zero is here because a
// counter that can wrap to 18 quintillion is not a counter.
static __always_inline void py_count_undo(__u32 slot) {
    if (slot >= PY_CNT_MAX) return;
    __u64 *d = bpf_map_lookup_elem(&py_walk_counters, &slot);
    if (d && *d > 0) *d -= 1;
}

// ----- One read per frame, not four.
//
// PY_FRAME_WINDOW is the prefix of _PyInterpreterFrame that carries every
// field this walk reads. The largest offset in any supported version is
// FrameOwner at 74 (3.14; it is 70 on 3.12/3.13) and the widest field is an
// 8-byte pointer at 56 (instr_ptr), so 96 covers all three with room, and
// TestFrameWindowCoversEveryTable pins that against the Go tables rather
// than leaving it to a comment.
//
// WHY ONE READ. Each bpf_probe_read_user is a helper call with an error
// branch, and every branch forks the verifier's exploration. Four reads meant
// four forks per Python frame. Measured, back when this walk lived inside the
// core's bpf_loop callback: the program peaked at 116,553 verifier states
// against 16,462 without it, and removing a nested bpf_loop changed that by
// exactly zero -- the forks in the body were the cost, not the loop structure.
// That measurement is why the walk is a separate program now; the one-read
// technique is kept because it is still the cheap shape, and it is what OTel
// does for the same reason (python_tracer.ebpf.c:60-76).
//
// The buffer lives in a per-CPU map rather than on the BPF stack: a
// variable-offset read from a map value is what the verifier handles best.
#define PY_FRAME_WINDOW 96

struct py_frame_window { __u8 b[PY_FRAME_WINDOW]; };

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct py_frame_window);
} py_frame_scratch SEC(".maps");


// ----- Where a sample's chain walk stands, and where that is kept.
//
// walk_persist.interp_scratch is four opaque words the core zeroes at the
// start of every sample and never otherwise touches (see unwind_record.h).
// This module uses two of them: the resume cursor and the state. That is the
// whole of its per-sample memory.
//
// THE CHAIN HAS TO BE RESUMED RATHER THAN RESTARTED.
// _PyInterpreterFrame's `previous` links run THROUGH the C boundary: an entry
// frame's previous is the frame that was current when C code re-entered the
// interpreter, so one linked list spans every Python segment of the stack,
// segmented by entry frames. A native stack with two eval-loop frames on it
// (Python -> C extension -> Python, which is what a callback or a numpy/torch
// dispatch looks like) therefore dispatches here twice; restarting from
// tstate->current_frame the second time would push the innermost segment a
// second time and claim it belonged to the outer one. That is the defect
// test/python_walk_defects_test.go's first case exists to catch.
#define PY_SCRATCH_FRAME 0    // next _PyInterpreterFrame to push, 0 if none
#define PY_SCRATCH_STATE 1    // PY_CHAIN_*

#define PY_CHAIN_UNSTARTED 0  // no TSS lookup done for this sample yet. Zero
                              // on purpose: it is what the core's zeroing of
                              // interp_scratch leaves behind.
#define PY_CHAIN_ACTIVE    1  // PY_SCRATCH_FRAME names where to resume, at the
                              // next eval-loop PC the native walk lands on
#define PY_CHAIN_DONE      2  // chain exhausted, or refused; do not retry

// CPython 3.14 makes _PyInterpreterFrame.f_executable a tagged _PyStackRef
// rather than a plain PyObject*, so the low bits must be cleared before the
// value is a pointer.
//
// The mask is CPython's own, not a guess: Modules/_remote_debugging_module.c
// (v3.14.3) is CPython's in-tree OUT-OF-PROCESS frame reader, and it does
//
//   line 45:  #define CLEAR_PTR_TAG(ptr) (((uintptr_t)(ptr) & ~Py_TAG_BITS))
//   line 46:  #define GET_MEMBER_NO_TAG(type, obj, offset) \
//                        (type)(CLEAR_PTR_TAG(*(type*)((char*)(obj) + (offset))))
//   line 2186: code_object = GET_MEMBER_NO_TAG(uintptr_t, frame,
//                                ...interpreter_frame.executable);
//
// and Py_TAG_BITS is 3 for the ordinary GIL build --
// Include/internal/pycore_stackref.h:446, inside the `#else // Py_GIL_DISABLED
// // With GIL` arm, measured by reading that header out of
// python:3.14.3-slim. (The free-threaded arm's Py_TAG_BITS is 1, but a
// free-threaded build is refused at attach; see pyunwind.ErrFreeThreaded.)
// Two low bits, not one: an immortal object's reference carries Py_TAG_REFCNT
// and an int can be tagged with Py_INT_TAG == 3.
#define PY_STACKREF_TAG_BITS 3ULL

// py_seg is one segment walk: the state the bpf_loop callback below needs,
// on the stack, which is the shape bpf_loop's callback context verifies best.
struct py_seg {
    struct sample_record *rec;
    struct py_proc_info *pi;
    __u64 frame;    // the _PyInterpreterFrame this iteration stands on
    __u32 n_pcs;    // the record cursor, copied back into walk_persist after
    __u8  state;    // the PY_CHAIN_* this segment ends in
    __u8  stopped;  // the callback ended the segment for a REASON, as opposed
                    // to the loop bound running out. bpf_loop's return value
                    // cannot answer that: a callback that stops on its last
                    // permitted iteration returns the same count as one that
                    // never stopped at all.
};

// py_seg_step advances the chain by ONE _PyInterpreterFrame: read this frame's
// owner, stop at the interpreter's C boundary, otherwise push it and move the
// cursor to `previous`. Returns 1 to end the segment.
//
// WHERE THE BOUNDARY IS is per-version and is NOT "owner == CSTACK". The
// _frameowner enum is renumbered in 3.14: FRAME_OWNED_BY_INTERPRETER is
// inserted at 3, pushing FRAME_OWNED_BY_CSTACK to 4 -- and CPython moved the
// C-boundary frame onto the new enumerator at the same time, so the value
// that marks the boundary is 3 on every supported version:
//
//   3.12.14 Python/ceval.c:702   entry_frame.owner = FRAME_OWNED_BY_CSTACK;      (3)
//   3.13.15 Python/ceval.c:719   entry_frame.owner = FRAME_OWNED_BY_CSTACK;      (3)
//   3.14.3  Python/ceval.c:1162  entry.frame.owner = FRAME_OWNED_BY_INTERPRETER; (3)
//
// FRAME_OWNED_BY_CSTACK == 4 is assigned nowhere in the 3.14 tree, so testing
// equality with frame_owner_cstack would walk straight past the boundary
// there and consume the native stack. CPython's own readers test the
// half-open range instead --
//
//   3.14.3 Python/ceval.c:260                     frame->owner >= FRAME_OWNED_BY_INTERPRETER
//   3.14.3 Modules/_remote_debugging_module.c:2148-2149
//                                                 owner == FRAME_OWNED_BY_CSTACK ||
//                                                 owner == FRAME_OWNED_BY_INTERPRETER
//
// -- and so does this walk, against pi->frame_owner_boundary, which pyunwind's
// per-version table measures from FRAME_OWNED_BY_CSTACK on 3.12/3.13 and from
// FRAME_OWNED_BY_INTERPRETER on 3.14. The literal 3 lives in that table, next
// to every other per-version fact, and not here.
//
// Stopping at the boundary is not an optimisation. Running the chain to NULL
// consumes the entire Python stack in one go and then terminates the trace,
// losing every native frame beneath the interpreter -- which is exactly what
// the reference implementation's pre-3.11 path does, and its own fixtures
// show the native stack simply missing.
static long py_seg_step(__u32 idx, void *arg) {
    struct py_seg *s = (struct py_seg *)arg;
    struct py_proc_info *pi = s->pi;
    // Defensive: py_walk_segment sets this before entering the loop, and the
    // verifier requires the null test to be reachable from every dereference
    // below regardless.
    if (!pi) {
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }

    __u64 frame = s->frame;
    if (frame == 0) {
        s->state = PY_CHAIN_DONE;  // the chain ran out; not a failure
        s->stopped = 1;
        return 1;
    }

    // One guard for every field, then one read. See PY_FRAME_WINDOW.
    //
    // The guard is on the OFFSETS, which come from the per-version table, so
    // it fires only for a table that does not fit the window -- never for a
    // frame. It is expressed against compile-time constants rather than by
    // adding to the checked value, for the reason record_push_interp's bound
    // documents: arithmetic on a checked value is how the earlier wrap bug
    // got past both the author and the verifier.
    //
    // The offsets are read into REGISTERS first, and the bounds check and the
    // indexing both use those registers. Checking pi->frame_owner and then
    // indexing with pi->frame_owner is two separate loads from a map value,
    // and the verifier will not carry a bound from one to the other --
    // another CPU could write the map between them. It says so plainly:
    //
    //   invalid access to map value, value_size=96 off=65535 size=1
    //   R2 max value is outside of the allowed memory range
    //
    // 65535 is the full __u16 range, i.e. the bound the guard proved was
    // discarded at the second load.
    __u16 off_owner = pi->frame_owner;
    __u16 off_prev = pi->frame_previous;
    __u16 off_exec = pi->frame_executable;
    __u16 off_instr = pi->frame_instr_ptr;
    if (off_owner > PY_FRAME_WINDOW - 1 ||
        off_prev > PY_FRAME_WINDOW - 8 ||
        off_exec > PY_FRAME_WINDOW - 8 ||
        off_instr > PY_FRAME_WINDOW - 8) {
        py_count(PY_CNT_FRAME_READ_FAIL);
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }
    __u32 wkey = 0;
    struct py_frame_window *w = bpf_map_lookup_elem(&py_frame_scratch, &wkey);
    if (!w) {
        py_count(PY_CNT_FRAME_READ_FAIL);
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }
    // All-or-nothing, and that is the honest shape: bpf_probe_read_user
    // either copies the whole window or copies nothing and returns an error.
    // A partial read cannot leave a zeroed owner byte looking like a valid
    // frame, which is what per-field reads made possible.
    if (bpf_probe_read_user(w->b, sizeof(w->b), (void *)frame)) {
        py_count(PY_CNT_FRAME_READ_FAIL);
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }

    __u8 owner = w->b[off_owner];
    // The same plausibility screen pyunwind.Offsets.Validate applies at
    // attach, re-applied per frame: an owner byte outside this version's enum
    // means the offsets are being read against a layout they do not describe,
    // and every frame after it would be invented.
    if (owner > pi->frame_owner_max) {
        py_count(PY_CNT_OWNER_IMPLAUSIBLE);
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }

    __u64 prev = *(__u64 *)&w->b[off_prev];

    if (owner >= pi->frame_owner_boundary) {
        // The interpreter's own entry frame: it executes no Python code, so
        // it is not pushed. Hand back to native unwinding, and leave the
        // cursor on what lies beyond it so the next eval-loop PC the native
        // walk lands on resumes the same chain instead of restarting it.
        if (prev != 0) {
            s->frame = prev;
            s->state = PY_CHAIN_ACTIVE;
        } else {
            s->state = PY_CHAIN_DONE;
        }
        s->stopped = 1;
        return 1;
    }

    __u64 exec = *(__u64 *)&w->b[off_exec];
    __u64 instr = *(__u64 *)&w->b[off_instr];
    if (pi->frame_executable_tagged) exec &= ~PY_STACKREF_TAG_BITS;

    // A TORN-READ BACKSTOP, not the mechanism for the C-entered frame.
    //
    // CPython >= 3.13 does put Py_None in f_executable rather than a code
    // object, but only on the ENTRY frame -- 3.13.15 ceval.c:716 and 3.14.3
    // ceval.c:1159 set it on the same frame whose owner they set to the
    // boundary value -- and the owner test above has already returned by
    // then. So this can only fire on a frame with owner < boundary whose
    // executable is nonetheless NULL or None: a read torn by a chain being
    // mutated underneath us, or offsets that are wrong in a way the owner
    // screen did not catch.
    //
    // It is kept because that is exactly the screen CPython's own
    // out-of-process reader applies (Modules/_remote_debugging_module.c:
    // 2142-2144, is_frame_valid, which refuses a NULL code object before
    // reading anything else) and because emitting either value as a code
    // object puts one garbage frame on the stack. EXPECT
    // PY_CNT_NONE_EXECUTABLE TO READ ZERO in production.
    if (exec == 0 || (pi->none_addr != 0 && exec == pi->none_addr)) {
        py_count(PY_CNT_NONE_EXECUTABLE);
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }

    if (record_push_interp(s->rec, &s->n_pcs, INTERP_ID_PYTHON, exec, instr)) {
        // The record is full. record_push_interp has already raised
        // WALKER_FLAG_FRAME_PUSH_REFUSED; these two say which pusher it was
        // and that a chain was in progress when it happened. Both were
        // counted for this case before the walker moved out of walk_step --
        // PUSH_REFUSED from the pusher, CHAIN_TRUNCATED from the driver
        // epilogue that saw a live cursor -- and both still are, so the
        // numbers an operator has been reading do not change meaning.
        py_count(PY_CNT_PUSH_REFUSED);
        py_count(PY_CNT_CHAIN_TRUNCATED);
        s->state = PY_CHAIN_DONE;
        s->stopped = 1;
        return 1;
    }
    py_count(PY_CNT_FRAMES_PUSHED);

    s->frame = prev;
    return 0;  // the segment continues on the next iteration
}

// py_walk_segment appends one Python segment to the record the native walk is
// building, at the cursor the native walk stopped at.
//
// It is entered because the native walk landed on a PC inside a libpython's
// eval loop -- the frame immediately below whatever this pushes is
// _PyEval_EvalFrameDefault, already in the record -- so the frames appended
// here land in true stack order with nothing in between. That is the whole
// ordering argument, and it is a kernel one: no anchor, no userspace splice.
//
// Every refusal, fault and truncation bumps a named py_walk_counters slot.
// Nothing here can stop the NATIVE walk, which is the walk that must survive:
// the worst case is that this returns having pushed nothing and marked itself
// done, and the native walk resumes exactly where it was.
static __always_inline void py_walk_segment(struct walk_persist *st,
                                            struct sample_record *rec) {
    __u8 state = (__u8)st->interp_scratch[PY_SCRATCH_STATE];
    if (state == PY_CHAIN_DONE) {
        st->interp_done |= 1u << INTERP_ID_PYTHON;
        return;
    }

    __u32 pid = st->pid;
    struct py_proc_info *pi = bpf_map_lookup_elem(&py_procs, &pid);
    if (!pi || !pi->enabled) {
        // An eval-loop PC in a process userspace never validated (or refused:
        // wrong version, free-threaded build, unreadable offsets). Named
        // rather than silent -- this is the counter that separates "no Python
        // frames because attach refused" from "no Python frames because the
        // walk failed".
        //
        // Counted on the FIRST dispatch only; marking the sample done stops
        // the second. Without that it would bump once per eval-loop native
        // frame per sample, forever, for every refused interpreter on the
        // box -- which makes the number scale with stack depth rather than
        // with occurrences and breaks the unit rule the whole counter set is
        // read under (PY_CNT_*: slot 0 counts frames, every other slot counts
        // samples).
        if (state == PY_CHAIN_UNSTARTED) py_count(PY_CNT_NO_PROC_INFO);
        st->interp_scratch[PY_SCRATCH_STATE] = PY_CHAIN_DONE;
        st->interp_done |= 1u << INTERP_ID_PYTHON;
        return;
    }

    __u64 frame;
    if (state == PY_CHAIN_UNSTARTED) {
        // Marked DONE before anything can fail, so every early return below
        // costs this sample one TSS lookup rather than one per eval-loop
        // frame.
        st->interp_scratch[PY_SCRATCH_STATE] = PY_CHAIN_DONE;
        st->interp_done |= 1u << INTERP_ID_PYTHON;

        __u64 tstate = py_tss_get(pi->tss_key, pi);
        if (!tstate) {
            py_count(PY_CNT_TSS_MISS);
            return;
        }
        __u64 p = 0;
        if (bpf_probe_read_user(&p, sizeof(p), (void *)(tstate + pi->threadstate_frame))) {
            py_count(PY_CNT_TSTATE_READ_FAIL);
            return;
        }
        // 3.12 has no current_frame on PyThreadState; the frame pointer is
        // one level deeper, in tstate->cframe->current_frame, and cframe's
        // current_frame is its offset-0 field. See
        // pyunwind.Offsets.ThreadStateFrameIndirect.
        if (pi->threadstate_frame_indirect) {
            if (p == 0 || bpf_probe_read_user(&p, sizeof(p), (void *)p)) {
                py_count(PY_CNT_TSTATE_READ_FAIL);
                return;
            }
        }
        frame = p;
        // The lookup succeeded: take the done bit back off before walking.
        st->interp_done &= ~(1u << INTERP_ID_PYTHON);
    } else {
        // PY_CHAIN_ACTIVE: a previous segment ended at the interpreter's C
        // boundary with chain still behind it, and the native walk has now
        // reached the eval-loop frame that runs it. The provisional
        // CHAIN_ABANDONED that segment recorded is withdrawn -- see
        // py_count_undo at the declaration of PY_CNT_CHAIN_ABANDONED.
        frame = st->interp_scratch[PY_SCRATCH_FRAME];
        py_count_undo(PY_CNT_CHAIN_ABANDONED);
    }

    struct py_seg seg = {
        .rec = rec,
        .pi = pi,
        .frame = frame,
        .n_pcs = st->n_pcs,
        .state = PY_CHAIN_DONE,
        .stopped = 0,
    };
    // MAX_FRAMES, not a bound of its own: the record cannot hold more frames
    // than that whatever produced them, so a Python segment shares the
    // record's budget with the native frames rather than carrying a second,
    // independent one. The per-segment bound this replaces used to truncate a
    // 40-frame segment at 32 while the record still had room.
    bpf_loop(MAX_FRAMES, py_seg_step, &seg, 0);

    st->n_pcs = seg.n_pcs;
    if (!seg.stopped) {
        // The loop bound ran out with the chain still going. Unreachable in
        // practice -- each iteration writes two slots, so the record refuses
        // at iteration 64 of 127 and takes the stopped path -- and counted
        // rather than assumed away.
        py_count(PY_CNT_CHAIN_TRUNCATED);
        seg.state = PY_CHAIN_DONE;
    }

    st->interp_scratch[PY_SCRATCH_STATE] = seg.state;
    if (seg.state == PY_CHAIN_ACTIVE) {
        st->interp_scratch[PY_SCRATCH_FRAME] = seg.frame;
        // Provisional: this segment expects to be resumed at the next
        // eval-loop PC the native walk reaches, and if it is, the entry path
        // above withdraws this. If the native walk ends first, the count
        // stands and names exactly what was lost.
        py_count(PY_CNT_CHAIN_ABANDONED);
    } else {
        st->interp_scratch[PY_SCRATCH_FRAME] = 0;
        st->interp_done |= 1u << INTERP_ID_PYTHON;
    }
}

// py_walk is the module's whole entry point: append a segment, then hand
// control back to the driver's resume program so the native walk continues
// beneath it.
//
// It hands back even on total failure. The alternative -- returning without
// the tail call -- would leave the sample truncated at the eval-loop frame,
// throwing away every native frame beneath the interpreter because the Python
// side could not read a TSD slot. The native walk is the walk that must
// survive.
static __always_inline void py_walk(void *ctx) {
    struct walk_persist *st = walk_state_get();
    struct sample_record *rec = walk_record_get();
    if (!st || !rec) return;
    py_walk_segment(st, rec);
    interp_return_to_native(ctx, st);
}

#endif // PERF_AGENT_INTERP_PYTHON_WALK_H
