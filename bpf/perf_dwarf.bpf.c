//go:build ignore
//
// perf_dwarf.bpf.c — DWARF-capable CPU sampler (S2).
//
// Loaded only when --unwind dwarf is selected. Mirrors perf.bpf.c's PID-
// filter + kernel-thread skip, but:
//
//   1. Uses a custom FP walker (bpf_loop + bpf_probe_read_user) instead of
//      bpf_get_stackid, so we can control per-frame classification.
//   2. Emits per-sample PC chains via BPF_MAP_TYPE_RINGBUF instead of
//      aggregating in a counts map — userspace aggregates post-symbolize.
//
// S2 stubs the classification lookup: every sample is tagged MODE_FP_SAFE
// and FP walking is the only path. S3 adds DWARF unwinding for FP-less
// ranges; S4 wires the per-PID mappings that make the classification
// lookup meaningful.
//
// See docs/dwarf-unwinding-design.md.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include "unwind_common.h"

// x86_64-only for S2. We access pt_regs fields directly (ip/bp/sp); arm64
// uses different field names (pc/regs[29]/sp) and a different struct type
// (user_pt_regs) that isn't in our x86-dumped vmlinux.h. Arm64 BPF support
// lands in a later stage with its own vmlinux and a separate source file.

// System-wide mode toggle set by userspace at load time (const volatile so
// the verifier dead-code-eliminates the wrong branch).
const volatile bool system_wide = false;

SEC("perf_event")
int perf_dwarf(struct bpf_perf_event_data *ctx) {
    __u64 tgid_tid = bpf_get_current_pid_tgid();
    __u32 tgid = tgid_tid >> 32;
    __u32 tid  = (__u32)tgid_tid;

    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    if (tgid == 0 || task == 0) return 0;
    if (BPF_CORE_READ(task, flags) & PF_KTHREAD) return 0;

    // Per-PID filter. In system-wide mode every non-kernel task passes.
    if (!system_wide) {
        if (!bpf_map_lookup_elem(&pids, &tgid)) return 0;
    }

    // Grab the per-CPU scratch slot. A sample_record is 1032 bytes — far
    // past the 512-byte BPF stack limit — so we build it here and copy to
    // the ringbuf at the end.
    __u32 zero = 0;
    struct sample_record *rec = bpf_map_lookup_elem(&walker_scratch, &zero);
    if (!rec) return 0;

    // User registers. x86_64-only; see comment atop this file.
    __u64 ip = ctx->regs.ip;
    __u64 fp = ctx->regs.bp;
    __u64 sp = ctx->regs.sp;

    struct walk_ctx walker = {
        .pc    = ip,
        .fp    = fp,
        .sp    = sp,
        .pid   = tgid,
        .n_pcs = 0,
        .rec   = rec,
    };

    // Walk at most MAX_FRAMES frames. walk_step breaks early on read
    // failure or natural terminator (saved_fp == 0, saved_fp <= fp).
    bpf_loop(MAX_FRAMES, walk_step, &walker, 0);

    // Fill the header AFTER the walk so we know n_pcs.
    rec->hdr.pid          = tgid;
    rec->hdr.tid          = tid;
    rec->hdr.time_ns      = bpf_ktime_get_ns();
    rec->hdr.value        = 1; // CPU sample count; weight is applied at sampling rate
    rec->hdr.mode         = MODE_FP_SAFE; // S2 stub — S3 varies this per-range
    rec->hdr.n_pcs        = (__u8)(walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs);
    rec->hdr.walker_flags = 0; // unused in S2; S3+ marks DWARF transitions

    // Copy the full fixed-size record into the ringbuf. The wasted bytes
    // past n_pcs are acceptable; see unwind_common.h for the design note.
    bpf_ringbuf_output(&stack_events, rec, sizeof(*rec), 0);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
