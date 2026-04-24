//go:build ignore
//
// offcpu_dwarf.bpf.c — DWARF-capable off-CPU sampler (S6).
//
// Loaded only when --offcpu --unwind dwarf is selected. Walks the user
// stack of tasks going off-CPU using the S3 hybrid walker (walk_step in
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

// System-wide mode toggle set by userspace at load time. When true, the
// PID filter below is skipped — the walker emits a sample for every
// non-kernel task's off-CPU interval.
const volatile bool system_wide = false;

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

static __always_inline void handle_switch_out(struct task_struct *prev) {
    __u32 pid = BPF_CORE_READ(prev, pid);
    __u32 tgid = BPF_CORE_READ(prev, tgid);
    if (pid == 0 || tgid == 0) return;
    if (BPF_CORE_READ(prev, flags) & PF_KTHREAD) return;

    // PID filter (skipped in system-wide mode).
    if (!system_wide) {
        if (!bpf_map_lookup_elem(&pids, &tgid)) return;
    }

    // Grab per-CPU scratch to build the sample_record.
    __u32 zero = 0;
    struct sample_record *rec = bpf_map_lookup_elem(&walker_scratch, &zero);
    if (!rec) return;

    // User-space registers of prev.
    struct pt_regs *regs = (struct pt_regs *)bpf_task_pt_regs(prev);
    if (!regs) return;
    __u64 ip = (__u64)PT_REGS_IP(regs);
    __u64 fp = (__u64)PT_REGS_FP(regs);
    __u64 sp = (__u64)PT_REGS_SP(regs);

    struct walk_ctx walker = {
        .pc    = ip,
        .fp    = fp,
        .sp    = sp,
        .pid   = tgid,
        .n_pcs = 0,
        .rec   = rec,
    };
    rec->hdr.walker_flags = 0;
    bpf_loop(MAX_FRAMES, walk_step, &walker, 0);

    __u64 now = bpf_ktime_get_ns();
    rec->hdr.pid     = tgid;
    rec->hdr.tid     = pid;
    rec->hdr.time_ns = now;
    rec->hdr.value   = now; // stash timestamp here; overwritten on switch-IN
    rec->hdr.n_pcs   = (__u8)(walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs);
    rec->hdr.mode    = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;

    struct offcpu_start_key k = { .pid = pid, .tgid = tgid };
    // Pass the per-CPU scratch pointer to avoid a 1KB stack-local copy
    // (BPF stack is 512 bytes; sample_record is 1032).
    bpf_map_update_elem(&offcpu_start, &k, rec, BPF_ANY);
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
    handle_switch_out(prev);
    handle_switch_in(next);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
