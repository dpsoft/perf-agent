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

// Set by userspace at load time (cfg.KernelStacks). When false, kernel
// stack capture is fully bypassed — zero per-sample cost. When true, the
// existing per-mode gate (system_wide hard-true OR pid_config.collect_kernel)
// decides which samples capture kernel stacks. Task 3 wires the kernel
// stack ID capture path; this declaration lets userspace flip the gate
// at load time without another bpf2go regeneration.
const volatile bool kernel_stacks_enabled = false;

// perf_dwarf_walk walks (or resumes the walk of) one sample and emits it.
//
// Two programs call it: the perf_event entry point below, once it has
// initialised the state, and interp_resume, once an interpreter module has
// appended its frames and tail-called back. A sample that went through an
// interpreter is emitted exactly like one that did not, so both share this.
//
// IT LOOKS THE TWO PER-CPU MAPS UP ITSELF rather than taking them as
// arguments, and that is a measured decision, not an oversight: holding those
// two map-value pointers live across unwind_walk costs the verifier 146,027
// processed instructions on this kernel (333,403 against 187,376), because
// both are then part of the state at every point inside the walk loop and
// pruning gets worse. The entry point below looks them up again for its own
// initialisation; two lookups of a per-CPU array are cheaper at runtime than
// one of them is to verify.
static __always_inline void perf_dwarf_walk(void *ctx, bool resumed) {
    struct walk_persist *st = walk_state_get();
    struct sample_record *rec = walk_record_get();
    if (!st || !rec) return;

    // MAY NOT RETURN: when the walk stops on a frame another unwinder claims,
    // unwind_walk tail-calls that unwinder and this program ceases to exist.
    // interp_resume comes back here to finish the sample.
    __u32 n_pcs = unwind_walk(ctx, st, rec, resumed);

    // Kernel-stack capture. Default to -1 so userspace can cheaply detect
    // "no kernel stack" without branching on the gate. bpf_get_stackid is
    // the kernel-side counterpart of the FP-walk path's stackmap insert in
    // perf.bpf.c — see internal/bpfstack.ExtractIPs for the userspace decode.
    rec->hdr.kern_stack = -1;
    if (kernel_stacks_enabled) {
        rec->hdr.kern_stack = bpf_get_stackid(ctx, &kern_stackmap, KERN_STACKID_FLAGS);
    }

    // Fill the header AFTER the walk so we know n_pcs.
    rec->hdr.pid          = st->pid;
    rec->hdr.tid          = st->tid;
    rec->hdr.time_ns      = bpf_ktime_get_ns();
    rec->hdr.value        = 1; // CPU sample count; weight is applied at sampling rate
    rec->hdr.n_pcs        = (__u8)n_pcs;
    // Dominant mode for telemetry: FP_LESS if DWARF fired at least once,
    // else FP_SAFE. walker_flags carries the per-bit breakdown.
    rec->hdr.mode = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;
    // walker_flags already populated by the walk — do not reset here.

    // Copy the full fixed-size record into the ringbuf. The wasted bytes
    // past n_pcs are acceptable; see unwind_common.h for the design note.
    bpf_ringbuf_output(&stack_events, rec, sizeof(*rec), 0);
}

INTERP_DEFINE_PROGRAMS("perf_event", perf_dwarf_walk)

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

    // The record is 1184 bytes and the BPF stack is 512, so it is built in a
    // per-CPU map. The walk cursor lives in a second per-CPU map for a
    // different reason: a tail call to an interpreter would take this
    // program's stack with it.
    struct sample_record *rec = walk_record_get();
    struct walk_persist *st = walk_state_get();
    if (!rec || !st) return 0;

    // User registers. PT_REGS_* macros handle the arch-specific field names;
    // &ctx->regs points at bpf_user_pt_regs_t, which the macros cast and read.
    unwind_walk_begin(st, rec, tgid, tid,
                      (__u64)PT_REGS_IP(&ctx->regs),
                      (__u64)PT_REGS_FP(&ctx->regs),
                      (__u64)PT_REGS_SP(&ctx->regs));

    perf_dwarf_walk(ctx, false);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
