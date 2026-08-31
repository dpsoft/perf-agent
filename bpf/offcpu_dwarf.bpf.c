//go:build ignore
//
// offcpu_dwarf.bpf.c — DWARF-capable off-CPU sampler.
//
// Loaded only when --offcpu --unwind dwarf is selected. Walks the user
// stack of tasks going off-CPU using the hybrid walker (walk_step in
// unwind_common.h). Emits one ringbuf record per off-CPU interval with
// value = blocking-ns.
//
// Two-step flow:
//   - switch-OUT (prev going off-CPU): walk prev's user stack now,
//     stash the full sample_record in offcpu_start keyed by (pid,tgid).
//     Timestamp is parked in hdr.value; overwritten on switch-IN.
//   - switch-IN (prev coming back on): delta = now - stashed_timestamp;
//     overwrite hdr.value with delta, emit via ringbuf, delete entry.

#if defined(__TARGET_ARCH_arm64)
#include "vmlinux_arm64.h"
#else
#include "vmlinux.h"
#endif
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include "unwind_common.h"

// Note: unwind_common.h declares cfi_miss_events ringbuf and cfi_miss_ratelimit
// LRU map for Option A2 (lazy CFI compile, --unwind auto). Off-CPU is always
// eager in v1 per the A2 spec's non-goals — userspace's off-CPU dispatch
// hardcodes ModeEager, so the lazy-mode emit path in walk_step (probing
// cfi_classification_lengths) finds the length entry present and skips the
// ringbuf write. The fallback FP_LESS+miss emit can still fire on rare
// edge cases (samples inside code without a CFI rule), but no userspace
// drainer reads the ringbuf in off-CPU mode. Bounded ~96 KB of pinned BPF
// memory per off-CPU profiler instance is the cost; A2 v2 may extend the
// drainer to off-CPU if data justifies it.

// System-wide mode toggle set by userspace at load time. When true, the
// PID filter below is skipped — the walker emits a sample for every
// non-kernel task's off-CPU interval.
const volatile bool system_wide = false;

// Set by userspace at load time (cfg.KernelStacks). When false, kernel
// stack capture is fully bypassed — zero per-sample cost. When true, the
// existing per-mode gate (system_wide hard-true OR pid_config.collect_kernel)
// decides which samples capture kernel stacks. Task 3 wires the kernel
// stack ID capture path; this declaration lets userspace flip the gate
// at load time without another bpf2go regeneration.
const volatile bool kernel_stacks_enabled = false;

// offcpu_start keys the stashed sample by (pid, tgid). Value is the
// sample_record captured on switch-OUT. To avoid blowing the 512-byte
// BPF stack, we do NOT wrap it in a struct with a timestamp — instead
// we stash the switch-OUT timestamp in hdr.value (the "sample weight"
// slot, which is u64 and currently unused during the off-CPU interval),
// and overwrite it with the elapsed blocking-ns on switch-IN before
// emission. No consumer sees hdr.value as "timestamp" because emission
// only happens on switch-IN, after the overwrite.
struct offcpu_start_key {
    __u32 pid;
    __u32 tgid;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 2048);
    __type(key, struct offcpu_start_key);
    __type(value, struct sample_record);
} offcpu_start SEC(".maps");

BTF_MATERIALIZE(offcpu_start_key)

// offcpu_dwarf_walk walks (or resumes the walk of) prev's user stack and
// stashes the sample for switch-IN to emit.
//
// Two programs call it: handle_switch_out below, once it has initialised the
// state, and interp_resume_walk, once an interpreter module has appended its
// frames and tail-called back.
//
// IT LOOKS THE TWO PER-CPU MAPS UP ITSELF rather than taking them as
// arguments, and that is measured, not incidental: holding those map-value
// pointers live across unwind_walk costs the verifier 146,027 processed
// instructions (333,403 against 187,376 on perf_dwarf, same shape), because
// both are then part of the state at every point inside the walk loop.
static __always_inline void offcpu_dwarf_walk(void *ctx, bool resumed) {
    struct walk_persist *st = walk_state_get();
    struct sample_record *rec = walk_record_get();
    if (!st || !rec) return;

    // MAY NOT RETURN: when the walk stops on a frame another unwinder claims,
    // unwind_walk tail-calls that unwinder and this program ceases to exist.
    // The resume programs come back here to finish the sample.
    __u32 n_pcs = unwind_walk(ctx, st, rec, resumed);

    // Kernel-stack capture for prev. At sched_switch, prev is still
    // "current" so bpf_get_stackid(ctx, …) records prev's kernel stack.
    // Default -1 so userspace can skip the lookup without branching on
    // the gate. Mirror of the FP off-CPU's behavior in offcpu.bpf.c.
    rec->hdr.kern_stack = -1;
    if (kernel_stacks_enabled) {
        rec->hdr.kern_stack = bpf_get_stackid(ctx, &kern_stackmap, KERN_STACKID_FLAGS);
    }

    __u64 now = bpf_ktime_get_ns();
    rec->hdr.pid     = st->pid;
    rec->hdr.tid     = st->tid;
    rec->hdr.time_ns = now;
    rec->hdr.value   = now; // stash timestamp here; overwritten on switch-IN
    rec->hdr.n_pcs   = (__u8)n_pcs;
    rec->hdr.mode    = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;

    struct offcpu_start_key k = { .pid = st->tid, .tgid = st->pid };
    // Pass the per-CPU scratch pointer to avoid a 1KB stack-local copy
    // (BPF stack is 512 bytes; sample_record is 1184).
    bpf_map_update_elem(&offcpu_start, &k, rec, BPF_ANY);
}

INTERP_DEFINE_PROGRAMS("tp_btf/sched_switch", offcpu_dwarf_walk)

static __always_inline void handle_switch_out(void *ctx, struct task_struct *prev) {
    __u32 pid = BPF_CORE_READ(prev, pid);
    __u32 tgid = BPF_CORE_READ(prev, tgid);
    if (pid == 0 || tgid == 0) return;
    if (BPF_CORE_READ(prev, flags) & PF_KTHREAD) return;

    // PID filter (skipped in system-wide mode).
    if (!system_wide) {
        if (!bpf_map_lookup_elem(&pids, &tgid)) return;
    }

    struct sample_record *rec = walk_record_get();
    struct walk_persist *st = walk_state_get();
    if (!rec || !st) return;

    // User-space registers of prev.
    struct pt_regs *regs = (struct pt_regs *)bpf_task_pt_regs(prev);
    if (!regs) return;

    unwind_walk_begin(st, rec, tgid, pid,
                      (__u64)PT_REGS_IP(regs),
                      (__u64)PT_REGS_FP(regs),
                      (__u64)PT_REGS_SP(regs));

    offcpu_dwarf_walk(ctx, false);
}

static __always_inline void handle_switch_in(struct task_struct *next) {
    __u32 pid = BPF_CORE_READ(next, pid);
    __u32 tgid = BPF_CORE_READ(next, tgid);
    if (pid == 0) return;

    struct offcpu_start_key k = { .pid = pid, .tgid = tgid };
    struct sample_record *saved = bpf_map_lookup_elem(&offcpu_start, &k);
    if (!saved || saved->hdr.value == 0) return;

    __u64 now = bpf_ktime_get_ns();
    __u64 delta = now - saved->hdr.value;
    saved->hdr.value = delta; // overwrite stashed timestamp with blocking-ns

    bpf_ringbuf_output(&stack_events, saved, sizeof(*saved), 0);
    bpf_map_delete_elem(&offcpu_start, &k);
}

SEC("tp_btf/sched_switch")
int BPF_PROG(offcpu_dwarf_sched_switch, bool preempt,
             struct task_struct *prev, struct task_struct *next) {
    // SWITCH-IN FIRST, and the order is load-bearing. handle_switch_out ends
    // in a walk that may TAIL-CALL an interpreter, and a tail call replaces
    // the running program: anything after it in this function would never run,
    // so every off-CPU interval of a Python target would be started and never
    // emitted. The two halves are independent -- switch-in reads
    // offcpu_start[next], switch-out writes offcpu_start[prev], and prev is
    // never next -- so running them the other way round changes nothing else.
    handle_switch_in(next);
    handle_switch_out(ctx, prev);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
