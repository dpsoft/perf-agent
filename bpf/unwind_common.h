// unwind_common.h — shared types and BPF maps for the DWARF-unwind CPU and
// off-CPU profilers (perf_dwarf.bpf.c, offcpu_dwarf.bpf.c).
//
// The existing FP-only programs (perf.bpf.c, offcpu.bpf.c) are untouched;
// users opt into DWARF unwinding via --unwind dwarf / auto, which causes
// userspace to load these new programs instead.
//
// Scope: the sample-record shape, per-CPU walker scratch, ringbuf for
// emitted samples, PID filter, CFI tables, and per-instruction classification
// + pid_mappings HASH_OF_MAPS tables that the hybrid walker consults
// per frame. The walker itself lives in perf_dwarf.bpf.c (CPU) /
// offcpu_dwarf.bpf.c (off-CPU).
//
// See docs/dwarf-unwinding-design.md for architecture.
#ifndef PERF_AGENT_UNWIND_COMMON_H
#define PERF_AGENT_UNWIND_COMMON_H

// Callers should include the arch-specific vmlinux header (vmlinux.h on x86,
// vmlinux_arm64.h on arm64) BEFORE including this file. We guard on
// __VMLINUX_H__ so the two headers don't both get pulled in accidentally.
#ifndef __VMLINUX_H__
#include "vmlinux.h"
#endif
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
// CFIEntry / Classification in types.go and vice versa.

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
    __u8  walker_flags; // bitmask of WALKER_FLAG_* (defined near walk_step)
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

// ----- Lazy CFI: miss-notify ringbuf (Option A2).
//
// Walker writes a cfi_miss_event here when cfi_lookup misses on a frame
// classified MODE_FP_LESS. Userspace drains and compiles the missed
// binary on demand. Rate-limited per (pid, table_id) pair via
// cfi_miss_ratelimit below — without it, a single FP-less binary would
// flood ~99 events/sec/CPU until the userspace compile completes.
//
// 64 KB sized to hold ~2000 in-flight events while the drainer processes
// them; larger than realistic miss rates × compile latency.

struct cfi_miss_event {
    __u32 pid;
    __u64 table_id;
    __u64 rel_pc;        // diagnostic; userspace compiles whole binary
    __u64 ktime_ns;       // BPF emit time (for userspace latency telemetry)
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 64 * 1024);
} cfi_miss_events SEC(".maps");

// Per-(pid, table_id) rate-limit. LRU caps memory at ~96 KB regardless
// of fork-storms or long uptime.
struct cfi_miss_ratelimit_key {
    __u32 pid;
    __u64 table_id;
} __attribute__((packed));

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct cfi_miss_ratelimit_key);
    __type(value, __u64);  // last-emit ktime_get_ns()
} cfi_miss_ratelimit SEC(".maps");

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

// ----- Lazy CFI: miss emit helper.
//
// Called from walk_step when MODE_FP_LESS + cfi_lookup miss occur.
// Rate-limits to 1 event per (pid, table_id) per second; drops on
// ringbuf full (next sample after the rate-limit window will retry).
static __always_inline void emit_cfi_miss(__u32 pid, __u64 table_id, __u64 rel_pc) {
    struct cfi_miss_ratelimit_key key = {.pid = pid, .table_id = table_id};
    __u64 now = bpf_ktime_get_ns();
    __u64 *last = bpf_map_lookup_elem(&cfi_miss_ratelimit, &key);
    if (last && now - *last < 1000000000ULL /* 1 sec */) {
        return;
    }
    bpf_map_update_elem(&cfi_miss_ratelimit, &key, &now, BPF_ANY);

    struct cfi_miss_event *ev = bpf_ringbuf_reserve(&cfi_miss_events, sizeof(*ev), 0);
    if (!ev) return;
    ev->pid = pid;
    ev->table_id = table_id;
    ev->rel_pc = rel_pc;
    ev->ktime_ns = now;
    bpf_ringbuf_submit(ev, 0);
}

// ----- CFI maps.
//
// cfi_rules is a HASH_OF_MAPS: outer key is table_id (FNV-1a of build-id),
// inner is a variable-size ARRAY of cfi_entry sorted by pc_start.
// cfi_lengths holds the valid length of each inner array (BPF can't read
// inner max_entries at runtime).
//
// cfi_classification mirrors the structure for classification rows.
//
// pid_mappings: outer key is pid, inner is a fixed-size ARRAY of pid_mapping
// entries (most processes need < 256 mappings). pid_mapping_lengths holds
// the valid length per pid.

#define MAX_PID_MAPPINGS 256

// Clang emits only a BTF forward declaration for a struct referenced solely
// inside a HASH_OF_MAPS' __type(value, ...) annotation — the outer map's
// BTF records the inner value type as BTF_KIND_FWD rather than the full
// layout. cilium/ebpf's loader needs the full layout to generate Go structs
// and validate types, so we anchor each struct with an (otherwise unused)
// global so clang emits BTF_KIND_STRUCT with complete field info.
#define BTF_MATERIALIZE(T) struct T _btf_anchor_##T __attribute__((unused));
BTF_MATERIALIZE(cfi_entry)
BTF_MATERIALIZE(classification)
BTF_MATERIALIZE(pid_mapping)

// Named inner-map types for HASH_OF_MAPS.
struct cfi_inner {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1); // template only; actual inner maps are sized per binary at populate time
    __uint(map_flags, BPF_F_INNER_MAP);
    __type(key, __u32);
    __type(value, struct cfi_entry);
};

struct classification_inner {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1); // template only; actual inner maps are sized per binary at populate time
    __uint(map_flags, BPF_F_INNER_MAP);
    __type(key, __u32);
    __type(value, struct classification);
};

struct pid_mapping_inner {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, MAX_PID_MAPPINGS);
    __uint(map_flags, BPF_F_INNER_MAP);
    __type(key, __u32);
    __type(value, struct pid_mapping);
};

// Outer maps.

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __array(values, struct cfi_inner);
} cfi_rules SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __type(value, __u32);
} cfi_lengths SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __array(values, struct classification_inner);
} cfi_classification SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __type(value, __u32);
} cfi_classification_lengths SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __array(values, struct pid_mapping_inner);
} pid_mappings SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __type(value, __u32);
} pid_mapping_lengths SEC(".maps");

// ----- Lookup helpers.
//
// These helpers are called per-frame by the hybrid walker. They
// encapsulate the map-of-maps dance so the walker stays readable.

// mapping_lookup_result carries what mapping_for_pc returns.
struct mapping_lookup_result {
    __u64 table_id;
    __u64 rel_pc;     // pc - load_bias
    __u8  found;      // 1 if pc falls inside some mapping of this pid
    __u8  _pad[7];
};

// mapping_scan_ctx is the bpf_loop callback's state; it also serves as
// the return channel via ctx->out.
struct mapping_scan_ctx {
    __u32 pid;
    __u64 pc;
    struct mapping_lookup_result out;
    void *inner;
    __u32 len;
};

// mapping_scan_step checks one mapping slot; stops the loop when we find
// a hit or when we pass the end of valid entries.
static long mapping_scan_step(__u32 idx, void *arg) {
    struct mapping_scan_ctx *ctx = (struct mapping_scan_ctx *)arg;
    if (idx >= ctx->len) return 1;
    struct pid_mapping *m = bpf_map_lookup_elem(ctx->inner, &idx);
    if (!m) return 1;
    if (ctx->pc >= m->vma_start && ctx->pc < m->vma_end) {
        ctx->out.table_id = m->table_id;
        ctx->out.rel_pc = ctx->pc - m->load_bias;
        ctx->out.found = 1;
        return 1;
    }
    return 0;
}

// mapping_for_pc finds the first mapping in this pid's list whose vma range
// contains `pc`. Linear scan over MAX_PID_MAPPINGS; terminates early at the
// valid length. Returns .found == 0 if nothing matched (e.g. the PC is in a
// binary we never compiled CFI for, like the kernel's vsyscall or an anon
// JIT page).
static __always_inline struct mapping_lookup_result mapping_for_pc(__u32 pid, __u64 pc) {
    struct mapping_scan_ctx ctx = { .pid = pid, .pc = pc, };
    ctx.inner = bpf_map_lookup_elem(&pid_mappings, &pid);
    if (!ctx.inner) return ctx.out;
    __u32 *lenp = bpf_map_lookup_elem(&pid_mapping_lengths, &pid);
    if (!lenp || *lenp == 0) return ctx.out;
    ctx.len = *lenp > MAX_PID_MAPPINGS ? MAX_PID_MAPPINGS : *lenp;
    bpf_loop(MAX_PID_MAPPINGS, mapping_scan_step, &ctx, 0);
    return ctx.out;
}

// BINARY_SEARCH_MAX_ITERS bounds binary search over CFI / classification
// tables. log2(1_000_000) ≈ 20, so 20 iters suffices for any realistically
// sized binary.
#define BINARY_SEARCH_MAX_ITERS 20

// classify_rel_pc returns MODE_FP_SAFE / MODE_FP_LESS / MODE_FALLBACK for the
// given (table_id, rel_pc). If the table is absent or no row covers rel_pc,
// returns MODE_FP_SAFE — the walker treats FP-safe and "unknown" identically
// (spec: "FALLBACK behaves exactly like FP_SAFE").
static __always_inline __u8 classify_rel_pc(__u64 table_id, __u64 rel_pc) {
    void *inner = bpf_map_lookup_elem(&cfi_classification, &table_id);
    if (!inner) return MODE_FP_SAFE;
    __u32 *lenp = bpf_map_lookup_elem(&cfi_classification_lengths, &table_id);
    if (!lenp || *lenp == 0) return MODE_FP_SAFE;
    __u32 lo = 0, hi = *lenp;
    for (int i = 0; i < BINARY_SEARCH_MAX_ITERS; i++) {
        if (lo >= hi) break;
        __u32 mid = lo + (hi - lo) / 2;
        struct classification *c = bpf_map_lookup_elem(inner, &mid);
        if (!c) break;
        if (rel_pc < c->pc_start) {
            hi = mid;
        } else if (rel_pc >= c->pc_start + (__u64)c->pc_end_delta) {
            lo = mid + 1;
        } else {
            return c->mode;
        }
    }
    return MODE_FP_SAFE;
}

// cfi_lookup returns a pointer to the cfi_entry whose PC range contains
// rel_pc, or NULL if not found. Pointer is into the inner map — safe to
// read but not to retain across helper calls.
static __always_inline struct cfi_entry *cfi_lookup(__u64 table_id, __u64 rel_pc) {
    void *inner = bpf_map_lookup_elem(&cfi_rules, &table_id);
    if (!inner) return NULL;
    __u32 *lenp = bpf_map_lookup_elem(&cfi_lengths, &table_id);
    if (!lenp || *lenp == 0) return NULL;
    __u32 lo = 0, hi = *lenp;
    for (int i = 0; i < BINARY_SEARCH_MAX_ITERS; i++) {
        if (lo >= hi) break;
        __u32 mid = lo + (hi - lo) / 2;
        struct cfi_entry *e = bpf_map_lookup_elem(inner, &mid);
        if (!e) return NULL;
        if (rel_pc < e->pc_start) {
            hi = mid;
        } else if (rel_pc >= e->pc_start + (__u64)e->pc_end_delta) {
            lo = mid + 1;
        } else {
            return e;
        }
    }
    return NULL;
}

// walker_flags bits, exposed via sample_header.walker_flags:
//
//   bit 0 — FP walk reached a natural terminator (saved_fp == 0). Clear
//           means the walk was cut short by a read failure or MAX_FRAMES.
//   bit 1 — at least one frame used the DWARF path.
//   bit 2 — at least one frame's CFI lookup missed while classified FP_LESS
//           (walk truncated at that frame).
#define WALKER_FLAG_FP_TERMINATED  0x01
#define WALKER_FLAG_DWARF_USED     0x02
#define WALKER_FLAG_CFI_MISS       0x04

// walk_step is the per-frame bpf_loop callback for the hybrid walker.
// Classifies ctx->pc, picks FP or DWARF path, and advances the walk
// state. Returns 0 to continue, 1 to stop.
static long walk_step(__u32 idx, void *arg) {
    struct walk_ctx *ctx = (struct walk_ctx *)arg;
    if (ctx->n_pcs >= MAX_FRAMES) return 1;

    ctx->rec->pcs[ctx->n_pcs++] = ctx->pc;

    // Per-frame classification. Miss = treat as FP_SAFE (spec: FALLBACK
    // behaves the same as FP_SAFE at runtime).
    struct mapping_lookup_result m = mapping_for_pc(ctx->pid, ctx->pc);
    __u8 mode = MODE_FP_SAFE;
    if (m.found) {
        // Lazy mode (Option A2): pid_mappings is populated but
        // cfi_classification may not be. Detect by probing
        // cfi_classification_lengths[table_id]. If missing, the binary
        // was enrolled but not yet compiled — emit a miss event so the
        // userspace drainer compiles on demand. Fall through to FP path
        // for this sample; the next sample after compile completes will
        // classify and unwind normally.
        __u32 *cls_len = bpf_map_lookup_elem(&cfi_classification_lengths, &m.table_id);
        if (!cls_len) {
            emit_cfi_miss(ctx->pid, m.table_id, m.rel_pc);
            // mode stays MODE_FP_SAFE (default); continue via FP path.
        } else {
            mode = classify_rel_pc(m.table_id, m.rel_pc);
        }
    }

    if (mode == MODE_FP_LESS) {
        struct cfi_entry *ep = cfi_lookup(m.table_id, m.rel_pc);
        if (!ep) {
            ctx->rec->hdr.walker_flags |= WALKER_FLAG_CFI_MISS;
            emit_cfi_miss(ctx->pid, m.table_id, m.rel_pc);
            return 1;
        }
        // Copy out of the inner map immediately — the pointer's lifetime
        // is bounded by the next BPF helper call. Defensive-copying keeps
        // reasoning simple and avoids any verifier fuss.
        struct cfi_entry e = *ep;

        __u64 base = (e.cfa_type == CFA_TYPE_FP) ? ctx->fp : ctx->sp;
        __u64 cfa = base + (__s64)e.cfa_offset;

        __u64 ret_addr = 0;
        if (e.ra_type == RA_TYPE_OFFSET_CFA) {
            if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr),
                                    (void *)(cfa + (__s64)e.ra_offset)) != 0) return 1;
        } else {
            // SAME_VALUE (leaf on arm64) or REGISTER — we don't track
            // non-FP registers, so stop.
            return 1;
        }

        __u64 new_fp = ctx->fp;
        if (e.fp_type == FP_TYPE_OFFSET_CFA) {
            if (bpf_probe_read_user(&new_fp, sizeof(new_fp),
                                    (void *)(cfa + (__s64)e.fp_offset)) != 0) return 1;
        } else if (e.fp_type == FP_TYPE_SAME_VALUE) {
            // new_fp unchanged
        } else {
            // UNDEFINED / REGISTER — FP is lost; continuing via DWARF
            // further is fine but FP-based frames further up will fail.
            new_fp = 0;
        }

        ctx->pc = ret_addr;
        ctx->fp = new_fp;
        ctx->sp = cfa;
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_DWARF_USED;
        return 0;
    }

    // FP_SAFE or FALLBACK — same path: FP walk.
    __u64 saved_fp = 0, ret_addr = 0;
    if (bpf_probe_read_user(&saved_fp, sizeof(saved_fp), (void *)ctx->fp) != 0) return 1;
    if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr), (void *)(ctx->fp + 8)) != 0) return 1;
    if (saved_fp == 0) {
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_FP_TERMINATED;
        return 1;
    }
    if (saved_fp <= ctx->fp) return 1;

    // Caller's resume SP: after a standard prologue (push FP; move FP=SP
    // on x86_64; equivalent stp x29, x30 on arm64), the caller's SP at
    // the return instruction is current FP + 16 (saved FP + saved RA).
    // Matters when the next frame up is FP_LESS with CFA rooted at SP.
    ctx->sp = ctx->fp + 16;
    ctx->pc = ret_addr;
    ctx->fp = saved_fp;
    return 0;
}

#endif // PERF_AGENT_UNWIND_COMMON_H
