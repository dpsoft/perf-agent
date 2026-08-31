// python_walk.h — reaching the current thread's PyThreadState from BPF and
// walking the CPython frame chain it names.
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
//
// The chain walk (py_push_frames, at the bottom) is driven from walk_step in
// unwind_common.h, which is why this header is included from there AFTER
// struct walk_ctx and frame_push_python exist: it consumes both.
#ifndef PERF_AGENT_PYTHON_WALK_H
#define PERF_AGENT_PYTHON_WALK_H

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
                                       // 4 on 3.14). py_push_frames refuses
                                       // an owner byte above it.
    __u8  frame_owner_cstack;         // this version's FRAME_OWNED_BY_CSTACK
                                       // (3 on 3.12/3.13, 4 on 3.14).
                                       // Carried for completeness of the
                                       // mirror; the stop condition uses
                                       // frame_owner_boundary, NOT this --
                                       // see py_push_frames.
    __u8  frame_owner_boundary;       // lowest owner value that means "this
                                       // frame is the interpreter's C
                                       // boundary, not Python code": 3 on
                                       // every supported version, but
                                       // measured from a DIFFERENT
                                       // enumerator per version. See
                                       // pyunwind.Offsets.FrameOwnerBoundary
                                       // and py_push_frames.
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

// ----- The interpreter's eval-loop text range.
//
// Keyed by BINARY (table_id), not by pid: every process running the same
// libpython shares one entry. table_id is what mapping_for_pc() already
// computed for the frame walk_step is standing on, so the lookup costs one
// hash on a value we already hold. lo/hi are RELATIVE to the mapping's load
// bias, the same space mapping_for_pc reports rel_pc in.
//
// Nothing in this header populates it; userspace does, once per libpython it
// recognises (pyunwind.InstallEvalRange, from AttachProcess). Until a range
// is installed for a binary, the arm is verified but never taken for it.
struct py_eval_range { __u64 lo, hi; };

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u64);              // table_id
    __type(value, struct py_eval_range);
    __uint(max_entries, 64);
} py_eval_ranges SEC(".maps");

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
//                           convention: every one of those paths leaves
//                           walk_ctx.py_state == PY_CHAIN_DONE, and both
//                           py_push_frames and walk_step's interpreter arm
//                           do nothing at all once it is DONE. So a deep
//                           native stack that lands on the eval loop twenty
//                           times still contributes at most one.
//   slot 9 (CHAIN_          counts SAMPLES too, by a different mechanism: the
//   ABANDONED)              drivers ask the question exactly once, after
//                           bpf_loop returns.
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
#define PY_CNT_CHAIN_TRUNCATED   6  // the OUTER walk's iteration budget ran
                                    // out with a segment still in progress:
                                    // the record filled up mid-chain
#define PY_CNT_PUSH_REFUSED      7  // frame_push_python had no room left
#define PY_CNT_NONE_EXECUTABLE   8  // f_executable was NULL or Py_None on a
                                    // frame the owner test did not already
                                    // stop at: a torn-read backstop, expected
                                    // to read zero (see py_push_frames)
#define PY_CNT_CHAIN_ABANDONED   9  // the NATIVE walk stopped while a Python
                                    // cursor was still live; see below
#define PY_CNT_MAX               10

// PY_CNT_CHAIN_ABANDONED is a NATIVE-WALK OUTCOME WITH A PYTHON COST, not a
// Python-side failure. Nothing in the Python walk went wrong; the walk simply
// never got another turn.
//
// The cursor goes live when py_push_frames reaches an interpreter entry frame
// whose `previous` is non-NULL -- meaning there is an OUTER Python segment
// waiting below the C code -- and it is consumed the next time the native walk
// lands on an eval-loop PC. If the native walk ends first, that segment is
// absent from the sample and every other counter here reads clean.
//
// It cannot be bumped from py_push_frames or from walk_step: neither knows it
// is running for the last time. The drivers bump it after bpf_loop returns,
// which is the only place the question can be asked.
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
//     eval-loop PC. That points at py_eval_ranges: a libpython whose range was
//     never installed, or an eval loop entered through a PC outside the
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
// branch, and every branch inside walk_step forks the verifier's exploration
// -- which is then re-run for each of the outer bpf_loop's 127 iterations and
// each state of the mapping scan nested inside it. Four reads meant four
// forks per Python frame. Measured: with the interpreter arm present the
// program peaked at 116,553 verifier states against 16,462 without it, and
// removing a nested bpf_loop changed that by exactly zero -- the forks in the
// body were the cost, not the loop structure. This is the technique OTel uses
// for the same reason (python_tracer.ebpf.c:60-76): one bulk read into a
// fixed-size buffer, one bounds guard hoisted out of the field accesses, then
// plain offset arithmetic.
//
// The buffer lives in a per-CPU map rather than on the BPF stack: a
// variable-offset read from a map value is what the verifier handles best,
// and walk_step's frame is already carrying the walk's own state.
#define PY_FRAME_WINDOW 96

struct py_frame_window { __u8 b[PY_FRAME_WINDOW]; };

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct py_frame_window);
} py_frame_scratch SEC(".maps");

// ----- The frame chain.

// There is no per-segment frame bound any more. The chain is walked one
// frame per walk_step iteration, so it shares the record's MAX_FRAMES budget
// with the native frames instead of carrying a second, independent one --
// see py_step_one for why the loop that bound used to govern had to go.

// walk_ctx.py_state: where this sample's Python chain walk stands.
//
// The chain has to be resumed rather than restarted. _PyInterpreterFrame's
// `previous` links run THROUGH the C boundary: an entry frame's previous is
// the frame that was current when C code re-entered the interpreter, so one
// linked list spans every Python segment of the stack, segmented by entry
// frames. A native stack with two eval-loop frames on it (Python -> C
// extension -> Python, which is what a callback or a numpy/torch dispatch
// looks like) therefore hits walk_step's Python arm twice; restarting from
// tstate->current_frame the second time would push the innermost segment a
// second time and claim it belonged to the outer one.
#define PY_CHAIN_UNSTARTED 0  // no TSS lookup done for this sample yet
#define PY_CHAIN_ACTIVE    1  // walk_ctx.py_frame names where to resume, at
                              // the next eval-loop PC the native walk lands on
#define PY_CHAIN_DONE      2  // chain exhausted, or refused; do not retry
#define PY_CHAIN_WALKING   3  // a segment is in progress: walk_ctx.py_iter
                              // names the next frame, and walk_step spends
                              // its own loop iterations on it rather than
                              // advancing the native walk. See py_step_one.

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

// py_push_frames walks _PyInterpreterFrame.previous from where this sample
// left off, pushing one two-slot Python frame per Python-executing frame,
// and stops at the interpreter's C boundary so native unwinding can carry on
// beneath it.
//
// Stopping at the boundary is not an optimisation. Running the chain to NULL
// consumes the entire Python stack in one go and then terminates the trace,
// losing every native frame beneath the interpreter -- which is exactly what
// the reference implementation's pre-3.11 path does, and its own fixtures
// show the native stack simply missing.
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
// -- and so does this walk, against pi->frame_owner_boundary, which
// pyunwind's per-version table measures from FRAME_OWNED_BY_CSTACK on
// 3.12/3.13 and from FRAME_OWNED_BY_INTERPRETER on 3.14. The literal 3 lives
// in that table, next to every other per-version fact, and not here.
//
// Every refusal, fault and truncation bumps a named py_walk_counters slot.
// Returns nothing: a Python failure never stops the NATIVE walk, which is
// the walk that must survive.
// py_step_one advances the chain by ONE _PyInterpreterFrame: read this
// frame's owner, stop at the interpreter's C boundary, otherwise push it and
// move the cursor to `previous`.
//
// WHY THERE IS NO LOOP HERE AT ALL. This started as a `#pragma unroll` over
// 32 frames, became a bpf_loop callback, and neither loaded on Linux 6.19:
// both were rejected with "BPF program is too large. Processed 1000001 insn",
// the verifier's PATH-EXPLORATION ceiling rather than its size limit -- 3826
// instructions failed and so did 870.
//
// The measurement that explains it: the PRE-BRANCH program carries a nested
// bpf_loop (mapping_scan_step) with a bound of 256 inside the same
// 127-iteration outer walk, and it verifies on that exact kernel. What broke
// it was adding a SECOND nested fixpoint-iterated callback site, not the
// bound (32 < 256) and not the body. Each bpf_loop call site is a
// push_stack() fork the verifier re-enters until it converges, and
// func_states_equal() refuses to prune across differing callback_depth, so
// the levels multiply (kernel/bpf/verifier.c, 6.19: :10719, :11671, :11129,
// :19456; 6.19.4 backported f597664454bd, which makes every bpf_loop site an
// SCC so precision marks propagate over its implicit backedge).
//
// So the chain does not get a loop of its own. walk_step is already driven by
// a bpf_loop over MAX_FRAMES, and a live Python segment now consumes those
// iterations one frame at a time -- the same shape OTel converged on, whose
// unwinder runs one loop with a switch over "step a Python frame" and "step a
// native frame" rather than a loop per language.
//
// Two things fall out of that, both improvements:
//   * There is no PY_MAX_FRAMES_PER_ENTRY any more. Python and native frames
//     share the record's MAX_FRAMES budget, which is the honest resource; a
//     40-frame Python segment completes as long as there is room, where the
//     per-segment bound used to truncate it at 32.
//   * The walk is bounded by the outer loop, so nothing here can run away.
//
// Returns 1 only when the record is full -- the caller stops the whole walk,
// as it would for a native push refusal. Otherwise 0, with ctx->py_state
// saying whether the segment continues (PY_CHAIN_WALKING) or has ended.
static __always_inline int py_step_one(struct walk_ctx *ctx) {
    struct py_proc_info *pi = ctx->py_pi;
    // Defensive: py_begin_chain sets this before entering PY_CHAIN_WALKING,
    // and the verifier requires the null test to be reachable from every
    // dereference below regardless.
    if (!pi) {
        ctx->py_state = PY_CHAIN_DONE;
        return 0;
    }

    __u64 frame = ctx->py_iter;
    if (frame == 0) {
        ctx->py_state = PY_CHAIN_DONE;  // the chain ran out; not a failure
        return 0;
    }

    // One guard for every field, then one read. See PY_FRAME_WINDOW.
    //
    // The guard is on the OFFSETS, which come from the per-version table, so
    // it fires only for a table that does not fit the window -- never for a
    // frame. It is expressed against compile-time constants rather than by
    // adding to the checked value, for the reason frame_push_python's bound
    // documents: arithmetic on a checked value is how the earlier wrap bug
    // got past both the author and the verifier.
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
    // discarded at the second load. This is the same discipline
    // frame_push_native documents for MAX_FRAMES: bind the value once, then
    // check and use that binding.
    __u16 off_owner = pi->frame_owner;
    __u16 off_prev = pi->frame_previous;
    __u16 off_exec = pi->frame_executable;
    __u16 off_instr = pi->frame_instr_ptr;
    if (off_owner > PY_FRAME_WINDOW - 1 ||
        off_prev > PY_FRAME_WINDOW - 8 ||
        off_exec > PY_FRAME_WINDOW - 8 ||
        off_instr > PY_FRAME_WINDOW - 8) {
        py_count(PY_CNT_FRAME_READ_FAIL);
        ctx->py_state = PY_CHAIN_DONE;
        return 0;
    }
    __u32 wkey = 0;
    struct py_frame_window *w = bpf_map_lookup_elem(&py_frame_scratch, &wkey);
    if (!w) {
        py_count(PY_CNT_FRAME_READ_FAIL);
        ctx->py_state = PY_CHAIN_DONE;
        return 0;
    }
    // All-or-nothing, and that is the honest shape: bpf_probe_read_user
    // either copies the whole window or copies nothing and returns an error.
    // A partial read cannot leave a zeroed owner byte looking like a valid
    // frame, which is what per-field reads made possible.
    if (bpf_probe_read_user(w->b, sizeof(w->b), (void *)frame)) {
        py_count(PY_CNT_FRAME_READ_FAIL);
        ctx->py_state = PY_CHAIN_DONE;
        return 0;
    }

    __u8 owner = w->b[off_owner];
    // The same plausibility screen pyunwind.Offsets.Validate applies at
    // attach, re-applied per frame: an owner byte outside this version's enum
    // means the offsets are being read against a layout they do not describe,
    // and every frame after it would be invented.
    if (owner > pi->frame_owner_max) {
        py_count(PY_CNT_OWNER_IMPLAUSIBLE);
        ctx->py_state = PY_CHAIN_DONE;
        return 0;
    }

    __u64 prev = *(__u64 *)&w->b[off_prev];

    if (owner >= pi->frame_owner_boundary) {
        // The interpreter's own entry frame: it executes no Python code, so
        // it is not pushed. Hand back to native unwinding, and leave the
        // cursor on what lies beyond it so the next eval-loop PC the native
        // walk lands on resumes the same chain instead of restarting it.
        if (prev != 0) {
            ctx->py_frame = prev;
            ctx->py_state = PY_CHAIN_ACTIVE;
        } else {
            ctx->py_state = PY_CHAIN_DONE;
        }
        return 0;
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
        ctx->py_state = PY_CHAIN_DONE;
        return 0;
    }

    if (frame_push_python(ctx, exec, instr)) {
        // The record is full. frame_push_python has already raised
        // WALKER_FLAG_FRAME_PUSH_REFUSED; this says which of the two pushers
        // it was. The whole walk stops: a native push would refuse too.
        py_count(PY_CNT_PUSH_REFUSED);
        ctx->py_state = PY_CHAIN_DONE;
        return 1;
    }
    py_count(PY_CNT_FRAMES_PUSHED);

    ctx->py_iter = prev;
    return 0;  // still PY_CHAIN_WALKING: the next outer iteration continues
}

// py_begin_chain arms the cursor for a segment, doing the one-time work of
// finding this thread's PyThreadState and its current frame. The stepping is
// py_step_one, above, driven by walk_step's own loop.
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
// -- and so does py_step_one, against pi->frame_owner_boundary, which
// pyunwind's per-version table measures from FRAME_OWNED_BY_CSTACK on
// 3.12/3.13 and from FRAME_OWNED_BY_INTERPRETER on 3.14. The literal 3 lives
// in that table, next to every other per-version fact, and not here.
//
// Every refusal and fault bumps a named py_walk_counters slot. Returns
// nothing: a Python failure never stops the NATIVE walk, which is the walk
// that must survive.
static __always_inline void py_begin_chain(struct walk_ctx *ctx, struct py_proc_info *pi) {
    // Defensive only: walk_step checks pi and owns PY_CNT_NO_PROC_INFO, so
    // this arm counts nothing (counting here too would attribute one miss
    // twice). It stays because pi is a map-lookup result and the verifier
    // requires the null test to be reachable from every dereference below,
    // not just from the caller.
    if (!pi) return;
    if (ctx->py_state == PY_CHAIN_DONE || ctx->py_state == PY_CHAIN_WALKING) return;

    __u64 frame = ctx->py_frame;
    if (ctx->py_state == PY_CHAIN_UNSTARTED) {
        // Marked DONE before anything can fail, so every early return below
        // costs this sample one TSS lookup rather than one per native frame
        // inside the eval loop.
        ctx->py_state = PY_CHAIN_DONE;

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
    }

    // The resume cursor is consumed: from here the segment is live and
    // ctx->py_iter carries it. py_step_one re-arms py_frame if it reaches
    // another boundary with chain still behind it.
    ctx->py_frame = 0;
    ctx->py_pi = pi;
    ctx->py_iter = frame;
    ctx->py_state = PY_CHAIN_WALKING;
}

#endif // PERF_AGENT_PYTHON_WALK_H
