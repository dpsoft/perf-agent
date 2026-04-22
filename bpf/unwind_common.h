// unwind_common.h — shared types and BPF maps for the DWARF-unwind CPU and
// off-CPU profilers (perf_dwarf.bpf.c, offcpu_dwarf.bpf.c).
//
// The existing FP-only programs (perf.bpf.c, offcpu.bpf.c) are untouched;
// users opt into DWARF unwinding via --unwind dwarf / auto, which causes
// userspace to load these new programs instead.
//
// S2 scope: this header defines the sample-record shape, the per-CPU
// walker scratch, and the ringbuf for emitted samples. The CFI + pc-
// classification + pid_mappings maps land in S3/S4 — for S2 the struct
// types are defined here so layouts are locked in, but the maps
// themselves aren't declared yet (no point; they'd just be dead code).
//
// See docs/dwarf-unwinding-design.md for architecture.
#ifndef PERF_AGENT_UNWIND_COMMON_H
#define PERF_AGENT_UNWIND_COMMON_H

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>

// MAX_FRAMES: the unwind walker's per-sample loop bound. Matches the
// BPF_MAP_TYPE_STACK_TRACE convention; deeper stacks truncate.
#define MAX_FRAMES 127

// RINGBUF_BYTES: size of the stack_events ringbuf. Must be a power of two
// and >= PAGE_SIZE. 256 KB absorbs bursts at 99 Hz × 16 CPUs; higher
// sample rates want bigger.
#define RINGBUF_BYTES (256 * 1024)

#define PF_KTHREAD 0x00200000

// ----- Type layouts mirrored from unwind/ehcompile/types.go.
//
// Kept in lockstep with the Go side — any change here requires updating
// CFIEntry / Classification in types.go and vice versa. S2 declares the
// types so future stages' maps can reference them; the maps themselves
// are added in S3/S4.

enum cfa_type {
    CFA_TYPE_UNDEFINED = 0,
    CFA_TYPE_SP        = 1,
    CFA_TYPE_FP        = 2,
};

enum fp_rule_type {
    FP_TYPE_UNDEFINED  = 0,
    FP_TYPE_OFFSET_CFA = 1,
    FP_TYPE_SAME_VALUE = 2,
    FP_TYPE_REGISTER   = 3,
};

enum ra_rule_type {
    RA_TYPE_UNDEFINED  = 0,
    RA_TYPE_OFFSET_CFA = 1,
    RA_TYPE_SAME_VALUE = 2,
    RA_TYPE_REGISTER   = 3,
};

enum classification_mode {
    MODE_FP_SAFE  = 0,
    MODE_FP_LESS  = 1,
    MODE_FALLBACK = 2,
};

struct cfi_entry {
    __u64 pc_start;
    __u32 pc_end_delta;
    __u8  cfa_type;
    __u8  fp_type;
    __s16 cfa_offset;
    __s16 fp_offset;
    __s16 ra_offset;
    __u8  ra_type;
    __u8  _pad[5];
};

struct classification {
    __u64 pc_start;
    __u32 pc_end_delta;
    __u8  mode;
    __u8  _pad[3];
};

struct pid_mapping {
    __u64 vma_start;
    __u64 vma_end;
    __u64 load_bias;
    __u64 table_id;
};

// ----- Sample record emitted via ringbuf per sample.
//
// Fixed-size layout (~1 KB): header + MAX_FRAMES u64 PCs, with n_pcs
// telling consumers how many slots are valid. A variable-length layout
// would save bandwidth but fights the verifier; we pay the constant-size
// cost and optimize later if needed.
// sample_header is 32 bytes; explicit tail padding makes the `pcs` array
// that follows it naturally 8-byte aligned on both archs.
struct sample_header {
    __u32 pid;
    __u32 tid;
    __u64 time_ns;
    __u64 value;       // sample weight: 1 for CPU, blocking-ns for off-CPU
    __u8  mode;        // dominant classification for the sample (telemetry)
    __u8  n_pcs;       // number of valid PCs in the pcs[] array
    __u8  walker_flags; // bit 0: FP walk reached terminator; 0 = truncated mid-read
    __u8  _pad;
    __u32 _pad2;
};

struct sample_record {
    struct sample_header hdr;
    __u64 pcs[MAX_FRAMES];
};

// ----- Per-CPU scratch map.
//
// Used to build the sample_record before copying into the ringbuf slot.
// 1032 bytes per record exceeds the 512-byte BPF stack limit, so staging
// through a per-CPU map is mandatory.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct sample_record);
    __uint(max_entries, 1);
} walker_scratch SEC(".maps");

// ----- Ringbuf for emitted sample records.
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_BYTES);
} stack_events SEC(".maps");

// ----- PID filter (same shape as perf.bpf.c).
struct pid_config {
    __u8 type;
    __u8 collect_user;
    __u8 collect_kernel;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u32);
    __type(value, struct pid_config);
    __uint(max_entries, 2048);
} pids SEC(".maps");

// ----- Walker helpers.

// walk_ctx holds per-sample unwinder state. Lives on the BPF entry
// function's stack; the pcs array lives in walker_scratch.
struct walk_ctx {
    __u64 pc;
    __u64 fp;
    __u64 sp;
    __u32 pid;
    __u32 n_pcs;
    struct sample_record *rec;
};

// walk_step is the per-frame callback invoked by bpf_loop(). Returns 0 to
// continue walking, 1 to stop. Reads saved-FP and return-address off the
// user stack; any read failure terminates the walk.
static long walk_step(__u32 idx, void *arg) {
    struct walk_ctx *ctx = (struct walk_ctx *)arg;
    if (ctx->n_pcs >= MAX_FRAMES) return 1;

    ctx->rec->pcs[ctx->n_pcs++] = ctx->pc;

    __u64 saved_fp = 0, ret_addr = 0;
    if (bpf_probe_read_user(&saved_fp, sizeof(saved_fp), (void *)ctx->fp) != 0) return 1;
    if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr), (void *)(ctx->fp + 8)) != 0) return 1;
    if (saved_fp == 0 || saved_fp <= ctx->fp) return 1;

    ctx->pc = ret_addr;
    ctx->fp = saved_fp;
    return 0;
}

#endif // PERF_AGENT_UNWIND_COMMON_H
