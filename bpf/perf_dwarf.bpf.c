//go:build ignore
//
// perf_dwarf.bpf.c — DWARF-capable CPU sampler.
//
// Loaded only when --unwind dwarf is selected. Mirrors perf.bpf.c's PID-
// filter + kernel-thread skip, but:
//
//   1. Uses a custom hybrid walker (bpf_loop + bpf_probe_read_user) that
//      classifies each frame's PC and picks FP-walk or DWARF-based unwind
//      per frame. Classification and CFI rules come from unwind/ehcompile.
//   2. Emits per-sample PC chains via BPF_MAP_TYPE_RINGBUF instead of
//      aggregating in a counts map — userspace aggregates post-symbolize.
//
// Hybrid walker + MMAP2-driven mapping ingestion so
// the userspace side can track new binaries at runtime without a restart.
//
// See docs/dwarf-unwinding-design.md.

#if defined(__TARGET_ARCH_arm64)
#include "vmlinux_arm64.h"
#else
#include "vmlinux.h"
#endif
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include "unwind_common.h"

// Register access uses the PT_REGS_* macros from bpf_tracing.h, which expand
// to the right fields per arch: ip/bp/sp on x86_64, pc/regs[29]/sp on arm64.
// The vmlinux header include above is gated on bpf2go's __TARGET_ARCH_* define
// so each build sees the correct bpf_user_pt_regs_t typedef.

// System-wide mode toggle set by userspace at load time.
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

    // User registers. PT_REGS_* macros handle the arch-specific field names;
    // &ctx->regs points at bpf_user_pt_regs_t, which the macros cast and read.
    __u64 ip = (__u64)PT_REGS_IP(&ctx->regs);
    __u64 fp = (__u64)PT_REGS_FP(&ctx->regs);
    __u64 sp = (__u64)PT_REGS_SP(&ctx->regs);

    struct walk_ctx walker = {
        .pc    = ip,
        .fp    = fp,
        .sp    = sp,
        .pid   = tgid,
        .n_pcs = 0,
        .rec   = rec,
    };

    // Zero walker_flags BEFORE bpf_loop so walk_step can OR bits in as it
    // classifies / switches modes / hits terminators.
    rec->hdr.walker_flags = 0;

    // Walk at most MAX_FRAMES frames. walk_step breaks early on read
    // failure or natural terminator (saved_fp == 0, saved_fp <= fp).
    bpf_loop(MAX_FRAMES, walk_step, &walker, 0);

    // Fill the header AFTER the walk so we know n_pcs.
    rec->hdr.pid          = tgid;
    rec->hdr.tid          = tid;
    rec->hdr.time_ns      = bpf_ktime_get_ns();
    rec->hdr.value        = 1; // CPU sample count; weight is applied at sampling rate
    rec->hdr.n_pcs        = (__u8)(walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs);
    // Dominant mode for telemetry: FP_LESS if DWARF fired at least once,
    // else FP_SAFE. walker_flags carries the per-bit breakdown.
    rec->hdr.mode = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;
    // walker_flags already populated by walk_step during the walk — do not reset here.

    // Copy the full fixed-size record into the ringbuf. The wasted bytes
    // past n_pcs are acceptable; see unwind_common.h for the design note.
    bpf_ringbuf_output(&stack_events, rec, sizeof(*rec), 0);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
