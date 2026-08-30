// python_walk.h — reaching the current thread's PyThreadState from BPF.
//
// CPython stores it in a pthread TSD slot, not in a global. The spike (see
// docs/superpowers/plans/2026-08-29-python-walker-slice1-2.md) found no
// shared-library CPython build in the supported range carries a static TLS
// offset that can be recovered by disassembly -- 3.12 and 3.13 call
// __tls_get_addr, 3.14 uses TLSDESC -- so there is nothing to extract that
// way. Instead we take the TSS *key*, whose offset comes out of
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
// Nothing in this file is wired into the walker yet. py_tss_get has no
// caller; py_procs is written by nobody. That lands in the tasks that
// attach to a target process and drive the frame walk.
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
//      this process's CPython minor version -- mirrors pyunwind.Offsets
//      (pyunwind/offsets.go) field-for-field, same order, same widths. A
//      mismatch between this struct and its Go counterpart writes garbage
//      offsets into the map, which produces a plausible-looking stack of
//      wrong frames rather than a loud failure -- the exact failure this
//      whole design exists to prevent. If a field is added on one side, it
//      must be added on the other side in the same position.
//
// frame_owner_max and frame_owner_cstack are carried, not interpreted,
// here: the _frameowner enum is renumbered in 3.14 (it inserts
// FRAME_OWNED_BY_INTERPRETER=3 ahead of FRAME_OWNED_BY_CSTACK, pushing
// CSTACK from 3 to 4), so a single hardcoded sentinel is wrong for at least
// one supported version. The walker that consumes these two fields owns the
// stop-condition logic; this header only transports the per-version facts.
struct py_proc_info {
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
                                       // 4 on 3.14)
    __u8  frame_owner_cstack;         // this version's FRAME_OWNED_BY_CSTACK
    __u8  frame_executable_tagged;    // 1 if frame_executable holds a tagged
                                       // _PyStackRef (3.14) needing masking
                                       // before use as a PyObject*
    __u8  threadstate_frame_indirect; // 1 if threadstate_frame is the offset
                                       // of a `cframe` pointer whose own
                                       // offset-0 field is current_frame
                                       // (3.12), rather than current_frame
                                       // itself (3.13+)

    __u8  enabled;                    // 0 until validation passed
    __u8  _pad[1];
};

// Pinned so a change to either side of the Go/C mirror (see the struct
// comment above) fails the build loudly instead of writing offsets at the
// wrong byte position. Update alongside pyProcInfo in the Go pyunwind
// package -- keep both edits in the same commit.
_Static_assert(sizeof(struct py_proc_info) == 44,
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

#endif // PERF_AGENT_PYTHON_WALK_H
