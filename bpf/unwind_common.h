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
// per frame. The walker itself — walk_step, the bpf_loop callback — lives
// HERE, in this header; the drivers only fill a walk_ctx and call bpf_loop.
//
// Drivers: perf_dwarf.bpf.c (perf_event), offcpu_dwarf.bpf.c (tp_btf) and
// gpu_usdt.bpf.c (uprobe.multi). The first two emit sample_records through
// the stack_events ringbuf; the third stages its PCs into a map of its own
// and defines UNWIND_NO_SAMPLE_RINGBUF to skip the emit-side maps, which
// would otherwise cost it ~16 MB of preallocated kern_stackmap it never
// reads.
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
// Fixed-size layout (~1.15 KB): header + MAX_FRAMES u64 PCs + MAX_FRAMES u8
// tags, with n_pcs telling consumers how many pcs[]/tags[] slots are valid.
// A variable-length layout would save bandwidth but fights the verifier; we
// pay the constant-size cost and optimize later if needed.
// sample_header is 40 bytes; explicit tail padding makes the `pcs` array
// that follows it naturally 8-byte aligned on both archs. kern_stack carries
// the BPF stack-ID produced by bpf_get_stackid on kern_stackmap (or -1 when
// kernel-stack capture is disabled). Userspace reads it to look the kernel
// IPs back out of kern_stackmap, symbolizes via the kernel symbolizer, and
// merges leaf-first with user frames.
//
// pcs[] is no longer a flat, uniformly-native PC array: a Python frame
// occupies two consecutive slots (code object address, then an encoded
// fingerprint/f_lasti word), so tags[] carries one FRAME_TAG_* byte per
// pcs[] slot telling consumers how to read it. Issue #83. tags[] trails
// pcs[] (rather than interleaving) so the existing fixed pcs[] offset is
// unchanged for readers that only care about native frames.
struct sample_header {
    __u32 pid;
    __u32 tid;
    __u64 time_ns;
    __u64 value;       // sample weight: 1 for CPU, blocking-ns for off-CPU
    __u8  mode;        // dominant classification for the sample (telemetry)
    __u8  n_pcs;       // number of valid slots in the pcs[]/tags[] arrays
    __u8  walker_flags; // bitmask of WALKER_FLAG_* (defined near walk_step)
    __u8  _pad;
    __u32 _pad2;
    __s64 kern_stack;  // bpf_get_stackid(&kern_stackmap,…) result, or -1 if disabled
};

// Frame tags. Each pcs[] slot carries its kind. FRAME_TAG_NATIVE is a single
// slot holding one PC; FRAME_TAG_PYTHON is the first of a pair of slots
// (code object, then encoded fingerprint/f_lasti) — see frame_push_python.
#define FRAME_TAG_NATIVE 0
#define FRAME_TAG_PYTHON 1

struct sample_record {
    struct sample_header hdr;
    __u64 pcs[MAX_FRAMES];
    // One tag per pcs[] slot. A u8 array rather than bits packed into the PC
    // because a PC is a full 64 bits and stealing from it would break the
    // day someone maps something high.
    __u8 tags[MAX_FRAMES];
};

// ----- Per-CPU scratch map.
//
// Used to build the sample_record before copying into the ringbuf slot.
// 1184 bytes per record exceeds the 512-byte BPF stack limit, so staging
// through a per-CPU map is mandatory.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct sample_record);
    __uint(max_entries, 1);
} walker_scratch SEC(".maps");

// ----- Ringbuf for emitted sample records.
//
// Emit-side only: a driver that stages its walk elsewhere (see
// UNWIND_NO_SAMPLE_RINGBUF above) never touches this or kern_stackmap, and
// creating them anyway would preallocate ~16 MB per attach for nothing.
#ifndef UNWIND_NO_SAMPLE_RINGBUF
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, RINGBUF_BYTES);
} stack_events SEC(".maps");
#endif

// ----- Kernel-stack capture (gated by kernel_stacks_enabled).
//
// Mirrors the FP-side stackmap in perf.bpf.c: BPF_MAP_TYPE_STACK_TRACE
// indexed by stack ID, each slot holding up to PERF_MAX_STACK_DEPTH u64
// kernel IPs. Populated per-sample via
// bpf_get_stackid(&kern_stackmap, KERN_STACKID_FLAGS) when the gate is on;
// userspace later does kern_stackmap.LookupBytes(stack_id) to retrieve the
// raw IPs and symbolizes them through the kernel symbolizer. When the gate
// is off, sample_header.kern_stack stays -1 and userspace skips the lookup.
//
// Named distinctly from the FP-side "stackmap" so both BPF programs can
// coexist in the same address space without symbol collision.
#define PERF_MAX_STACK_DEPTH 127
#define PROFILE_MAPS_SIZE    16384
#define KERN_STACKID_FLAGS   (0 | BPF_F_FAST_STACK_CMP)

#ifndef UNWIND_NO_SAMPLE_RINGBUF
struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, PERF_MAX_STACK_DEPTH * sizeof(__u64));
    __uint(max_entries, PROFILE_MAPS_SIZE);
} kern_stackmap SEC(".maps");
#endif

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
//
// py_frame/py_state are the CPython chain walk's resume cursor, zero for a
// process with no interpreter and untouched by every native path. They live
// here, not in a map, because they are per-SAMPLE state and this struct is
// the only per-sample thing the walker has: _PyInterpreterFrame.previous
// links run through the interpreter's C boundary frames, so a native stack
// that crosses the eval loop twice must RESUME the one chain rather than
// restart it. See PY_CHAIN_* and py_push_frames in python_walk.h.
//
// The three drivers build this on the BPF stack with designated
// initialisers, so the two added fields cost them nothing but zeroes.
// Forward declaration: struct walk_ctx carries a pointer to one, and
// python_walk.h (which defines it) is included further down, after the
// helpers it needs exist.
struct py_proc_info;

struct walk_ctx {
    __u64 pc;
    __u64 fp;
    __u64 sp;
    __u32 pid;
    // n_pcs is __u32 while sample_header.n_pcs is __u8, and that is
    // deliberate: this is the walker's live cursor, read into a register and
    // compared against MAX_FRAMES in the two hottest helpers in the program,
    // where a narrower field only buys the compiler a mask on every load and
    // store. The narrowing to __u8 happens once, in each driver, after the
    // walk (`rec->hdr.n_pcs = (__u8)(walker.n_pcs > MAX_FRAMES ? ...)`), and
    // the _Static_assert on MAX_FRAMES <= 255 near frame_push_python is what
    // proves that cast cannot truncate.
    //
    // The width is NOT what makes a bounds check safe here. The wrap bug CI
    // caught (see frame_push_python) came from doing arithmetic on the
    // checked value; a __u8 field would have hidden that one instance behind
    // integer promotion while leaving the pattern in place to bite the next
    // helper that needs a two-slot bound. Check against a compile-time
    // constant instead.
    __u32 n_pcs;
    struct sample_record *rec;
    __u64 py_frame;      // next _PyInterpreterFrame to push, 0 if none
    // The chain walk's per-CALL state, distinct from the resume cursor
    // above. py_push_frames drives its loop through bpf_loop rather than an
    // unrolled body (see python_walk.h for why), and a bpf_loop callback
    // takes exactly one context pointer -- so the callback's inputs live
    // here, where the callback already has them, instead of in a second
    // struct whose pointer would have to be carried through the callback
    // boundary as well.
    //
    // py_pi is a py_procs map value, the same kind of pointer `rec` above
    // already is, and is null-checked in the callback like any other.
    struct py_proc_info *py_pi;
    __u64 py_iter;       // the _PyInterpreterFrame this iteration stands on
    __u8  py_state;      // PY_CHAIN_UNSTARTED / _ACTIVE / _DONE
    // py_iter_stopped distinguishes "the walk ended for a reason" from "the
    // per-entry budget ran out", which is the difference between silence and
    // PY_CNT_CHAIN_TRUNCATED. bpf_loop's return value cannot answer it: a
    // callback that stops on its last permitted iteration returns the same
    // count as one that never stopped at all.
    __u8  py_iter_stopped;
    __u8  _pad[6];
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
//   bit 0 — the frame-pointer chain reached its natural terminator: a frame
//           whose saved-FP slot holds zero, which is the x86-64 psABI's
//           marker for the outermost frame (_start does `xorl %ebp, %ebp`
//           before calling __libc_start_main, and the clone child does the
//           same before calling the thread entry point). Clear means the FP
//           path did not end the walk at the root. The walk does not stop
//           here: the return address stored beside that zero is a real
//           caller PC (`_start`'s), so it is carried forward for exactly one
//           more step with fp == 0 so the unwind tables can confirm the root
//           (bit 3) instead of the walk merely assuming it. This bit is also
//           the walker's own record that it has taken that step: see
//           fp_chain_ended().
//   bit 1 — at least one frame used the DWARF path.
//   bit 2 — at least one frame's CFI lookup missed while classified FP_LESS
//           (walk truncated at that frame).
//   bit 3 — a frame's CFI gives the return address as UNDEFINED, the DWARF
//           marker for an outermost frame (glibc emits it for _start and for
//           thread entry points). The unwind information itself says the
//           chain ends here: a SUCCESS, the DWARF-side counterpart of bit 0.
//   bit 4 — the walker arrived at an FP_SAFE frame with ctx->fp == 0 and
//           could not continue. A FAILURE to make progress, not an end of
//           chain: the frame pointer was lost one step earlier, because the
//           DWARF rules of the FP_LESS frame below gave no location for it
//           (fp_type UNDEFINED / REGISTER → new_fp = 0). Whatever called the
//           FP_SAFE frame is missing from the stack, and nothing about the
//           unwind information says that frame was outermost.
//           NOT set when the frame pointer is zero because the chain ended
//           legitimately one step earlier (bit 0) — see fp_chain_ended().
//   bit 5 — the saved-FP slot of a frame held a value that is neither zero
//           nor above the current frame pointer, so the chain cannot be
//           followed further. The return address read out of the SAME frame
//           is still recorded (it is a separate slot and does not depend on
//           the saved FP being sane), then the walk stops. Like bit 4 this
//           is a FAILURE; it gets its own bit because the cause is different
//           and, unlike bit 4, it points at a corrupt or hand-rolled frame
//           rather than at unwind tables that dropped the register.
//   bit 6 — the step taken past the end of the frame-pointer chain (bit 0)
//           landed on a frame whose CFI says a caller EXISTS
//           (ra_type OFFSET_CFA) rather than declaring it outermost. The
//           frame-pointer chain and the unwind tables disagree about where
//           the stack ends; the walker believes the chain and stops. Always
//           accompanied by bit 0, never by bit 3, and it is what keeps that
//           pair from reading as an unqualified success: consumers must
//           treat a walk carrying it as one whose ending is UNCONFIRMED.
//   bit 7 — frame_push_native or frame_push_python refused to write because
//           too few pcs[]/tags[] slots remained (MAX_FRAMES for a native
//           push, fewer than 2 remaining for a Python pair). The record
//           itself is not corrupted - the refused frame simply never
//           landed - but the walk is one frame shorter than it otherwise
//           would be. Reached from the FP-nonmonotonic arm's second push
//           in the same iteration as the one that fills the record (see
//           frame_push_native's call there), and from frame_push_python,
//           whose two-slot push can be refused with one slot still free --
//           so it fires on shallower stacks than a native push alone would.
//           Distinct from bit 2 (a CFI table gap): this is the record
//           running out of room, not a classification failure.
//
// Bits 3 and 4 were one bit (WALKER_FLAG_UNWIND_TERMINATED) until issue #44.
// Merging them was wrong, and wrong in the direction that reads green: on the
// RTX 3090 validation run for #43 every DWARF walk ended via what is now bit
// 4 — stopped at main, missing __libc_start_main_impl and _start — and every
// one was classified a complete walk. Harmless there only because the lost
// frames were uninteresting. When the frame pointer is lost inside a vendor
// library with an FP_SAFE APPLICATION frame above it, the same bit means the
// walk stopped mid-application-stack. The root cause of bit 4 firing at all
// is issue #45 (ehcompile reads "no rule for %rbp" as UNDEFINED where the
// x86-64 psABI says unchanged); until that is fixed, bit 4 vs bit 3 is the
// measurement of how often it bites.
//
// Bits 0 and 3 are the two "the walk finished" bits; a walk with neither
// stopped for a reason it could not do anything about (a user-memory read
// fault, a lost frame pointer (bit 4), a non-monotonic one (bit 5), a CFI
// miss, an RA/FP location this walker does not track, or MAX_FRAMES).
// Consumers must treat bits 0 and 3 as a pair — see gpuprobe/consumer.go
// Stats.StackWalkAbandoned, which counts exactly the walks that set neither
// and did not hit MAX_FRAMES, and Stats.StackWalkFPExhausted /
// Stats.StackWalkFPNonMonotonic, which name bits 4 and 5 as the subsets of
// those with a known cause.
//
// Bits 0 and 3 are NOT mutually exclusive since issue #45: a walk that runs
// off the end of the frame-pointer chain (bit 0) takes one further step with
// fp == 0, and if the unwind tables classify that last frame FP_LESS and
// give its return address as UNDEFINED it sets bit 3 as well. That pair is
// the strongest possible ending — the FP chain and the CFI agreeing on where
// the stack stops — and it is the normal shape for a hybrid walk on glibc.
// When they disagree instead, bit 6 says so, and bit 0 alone then means
// "the frame pointer ran out" rather than "the stack ended".
//
// perf_dwarf.bpf.c and offcpu_dwarf.bpf.c read walker_flags only for
// WALKER_FLAG_DWARF_USED when computing sample_header.mode, and no Go code
// reads the field they emit (unwind/dwarfagent/sample.go decodes it and
// nothing consults it), so the bits themselves change nothing for them. The
// FRAMES they capture do change: issue #45's fix stops the DWARF path
// zeroing the frame pointer when the CFI carries no rule for it, and the
// step past the FP-chain root adds the outermost frame both used to drop.
#define WALKER_FLAG_FP_TERMINATED    0x01
#define WALKER_FLAG_DWARF_USED       0x02
#define WALKER_FLAG_CFI_MISS         0x04
#define WALKER_FLAG_RA_UNDEFINED     0x08
#define WALKER_FLAG_FP_EXHAUSTED     0x10
#define WALKER_FLAG_FP_NONMONOTONIC  0x20
#define WALKER_FLAG_ROOT_DISAGREEMENT 0x40
#define WALKER_FLAG_FRAME_PUSH_REFUSED 0x80

// frame_push_native appends one native-PC slot, tagging it FRAME_TAG_NATIVE.
// Returns 0 on success, 1 if the record is already full (MAX_FRAMES
// reached) — the caller must stop walking in that case. The refusal itself
// is never silent: WALKER_FLAG_FRAME_PUSH_REFUSED says a frame was dropped
// for lack of room, distinct from every other reason a walk can stop short.
//
// The bound is re-asserted into a local (`i`) immediately before the
// stores, rather than checked on ctx->n_pcs and indexed with ctx->n_pcs
// again: the verifier tracks a register's proven range across a branch,
// not a struct field re-read through a pointer, so `if (ctx->n_pcs >=
// MAX_FRAMES) ... ctx->rec->pcs[ctx->n_pcs++]` verified the check but not
// the store, rejecting perf_dwarf and offcpu_dwarf as an unbounded R4
// access at the pcs[] write. Binding `i` once and using it for both the
// check and every array access keeps the proof in one register.
static __always_inline int frame_push_native(struct walk_ctx *ctx, __u64 pc) {
    __u32 i = ctx->n_pcs;
    if (i >= MAX_FRAMES) {
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_FRAME_PUSH_REFUSED;
        return 1;
    }
    ctx->rec->tags[i] = FRAME_TAG_NATIVE;
    ctx->rec->pcs[i] = pc;
    ctx->n_pcs = i + 1;
    return 0;
}

// frame_push_python appends a two-slot Python frame (code object address,
// then an encoded fingerprint/f_lasti word), both tagged FRAME_TAG_PYTHON.
// Returns 0 on success, 1 if fewer than two slots remain — the caller must
// stop walking in that case rather than push a half-pair. Its caller is
// py_push_frames, below, which drives the CPython frame-chain walk.
// Keeping the MAX_FRAMES bounds check here, alongside frame_push_native's,
// means every place this record can overflow is checked in exactly one
// spot rather than re-derived at each call site. Like frame_push_native,
// a refusal raises WALKER_FLAG_FRAME_PUSH_REFUSED rather than dropping the
// frame silently.
//
// The check does NO ARITHMETIC ON THE CHECKED VALUE. It used to read
// `if (i + 2 > MAX_FRAMES)`, which looks like one comparison covering both
// slots and is in fact a wrap: `i` is __u32, `i + 2` is u32 arithmetic, and
// at i == 0xFFFFFFFE it evaluates to 0, which is not > 127. The check passes
// and both stores run with a wild index. The verifier is right to refuse to
// derive `i <= 125` from `i + 2 <= 127`, and it did — CI rejected both
// perf_dwarf and offcpu_dwarf with
//
//   (73) *(u8 *)(r1 +1056) = r0: R1 unbounded memory access
//
// at the tags[] store (1056 == offsetof(struct sample_record, tags)).
// frame_push_native was accepted through all of this because `i >= MAX_FRAMES`
// does no arithmetic and cannot wrap; that asymmetry was the tell.
//
// The bound is therefore expressed as a comparison against a COMPILE-TIME
// CONSTANT: this push writes slots `i` and `i + 1`, both of which must be
// < MAX_FRAMES, so the accept set is exactly i <= MAX_FRAMES - 2 (125 at
// MAX_FRAMES 127) and the refusal is `i > MAX_FRAMES - 2`. `MAX_FRAMES - 2`
// folds at compile time, so nothing the verifier has to track is ever added
// to. The _Static_assert below keeps that subtraction from underflowing into
// a huge unsigned bound if MAX_FRAMES is ever made tiny.
//
// Both slots are still written from that same checked local `i`, so the
// two-slot push stays atomic (refuse before writing either, never write one
// and fail the second) and, per frame_push_native's comment above, the
// verifier's proof for both `i` and `i + 1` stays tied to the one checked
// register instead of a re-read of ctx->n_pcs.
_Static_assert(MAX_FRAMES >= 2,
               "frame_push_python's bound is MAX_FRAMES - 2; below 2 that underflows to a huge unsigned and accepts everything");
_Static_assert(MAX_FRAMES <= 255,
               "walk_ctx.n_pcs is __u32 but narrows to sample_header.n_pcs (__u8) in every driver; MAX_FRAMES must fit");

static __always_inline int frame_push_python(struct walk_ctx *ctx, __u64 code, __u64 instr) {
    __u32 i = ctx->n_pcs;
    if (i > MAX_FRAMES - 2) {
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_FRAME_PUSH_REFUSED;
        return 1;
    }
    ctx->rec->tags[i] = FRAME_TAG_PYTHON;
    ctx->rec->pcs[i] = code;
    ctx->rec->tags[i + 1] = FRAME_TAG_PYTHON;
    ctx->rec->pcs[i + 1] = instr;
    ctx->n_pcs = i + 2;
    return 0;
}

// ----- Python frame walking.
//
// Included HERE, below frame_push_python and above walk_step, because
// py_push_frames consumes both struct walk_ctx (declared far above) and
// frame_push_python (just above): a C macro or function is not visible
// before its definition, so the include cannot sit with the other maps the
// way it used to. python_walk.h supplies py_proc_info/py_procs, the
// pthread-TSD reimplementation that reaches a target thread's
// PyThreadState, the eval-loop text ranges walk_step switches on, and the
// per-CPU counters that make every Python-side refusal visible.
#include "python_walk.h"

// fp_chain_ended reports whether this walk has already stepped PAST the end
// of the frame-pointer chain: it left a frame whose saved-FP slot held zero,
// carried that frame's return address forward, and is now standing on the
// caller the slot named with ctx->fp == 0.
//
// It reads the reported bit rather than carrying separate state because
// WALKER_FLAG_FP_TERMINATED is set at exactly one site in walk_step — the
// saved_fp == 0 arm — and that site is the only one that continues with a
// zeroed frame pointer. Keeping it out of struct walk_ctx also keeps that
// struct as small as it can be, so the three drivers' entry code (which
// builds one on the BPF stack) is untouched by this change.
//
// Every arm that can be reached with it set stops the walk, so the step past
// the root is bounded to exactly one.
static __always_inline int fp_chain_ended(struct walk_ctx *ctx) {
    return (ctx->rec->hdr.walker_flags & WALKER_FLAG_FP_TERMINATED) != 0;
}

// walk_step is the per-frame bpf_loop callback for the hybrid walker.
// Classifies ctx->pc, picks FP or DWARF path, and advances the walk
// state. Returns 0 to continue, 1 to stop.
static long walk_step(__u32 idx, void *arg) {
    struct walk_ctx *ctx = (struct walk_ctx *)arg;
    if (frame_push_native(ctx, ctx->pc)) return 1;

    // Per-frame classification. Miss = treat as FP_SAFE (spec: FALLBACK
    // behaves the same as FP_SAFE at runtime).
    struct mapping_lookup_result m = mapping_for_pc(ctx->pid, ctx->pc);
    __u8 mode = MODE_FP_SAFE;
    if (m.found) {
        // ----- The interpreter arm.
        //
        // The switch is a text-range test on the frame we are already
        // standing on: if this PC is inside the eval loop of a libpython we
        // know, the native frame just pushed is _PyEval_EvalFrameDefault and
        // the Python frames it is running live in the interpreter's own
        // chain, not on this stack. Keyed by table_id, which mapping_for_pc
        // has already computed, so a non-Python process pays one hash on a
        // value it holds and nothing else.
        //
        // It lives INSIDE walk_step rather than beside it so perf_dwarf,
        // offcpu_dwarf and gpu_usdt all inherit it from the bpf_loop they
        // already share -- the CUDA launch probe gets Python frames with no
        // second integration.
        //
        // A Python failure never stops the native walk: py_push_frames
        // returns void and counts, and control falls through to the
        // classification below exactly as it did before.
        struct py_eval_range *er = bpf_map_lookup_elem(&py_eval_ranges, &m.table_id);
        // ctx->py_state == PY_CHAIN_DONE means this sample is finished with
        // Python -- the chain ran out, or something refused and was counted.
        // Skipping the py_procs hash in that case is the same economy
        // py_push_frames applies to the TSS lookup: a deep native stack can
        // land on the eval loop many times per sample, and none of those
        // repeats can learn anything new.
        if (er && ctx->py_state != PY_CHAIN_DONE &&
            m.rel_pc >= er->lo && m.rel_pc < er->hi) {
            __u32 py_pid = ctx->pid;
            struct py_proc_info *pi = bpf_map_lookup_elem(&py_procs, &py_pid);
            if (pi && pi->enabled) {
                py_push_frames(ctx, pi);
            } else {
                // An eval-loop PC in a process userspace never validated (or
                // refused: wrong version, free-threaded build, unreadable
                // offsets). Named rather than silent -- this is the counter
                // that separates "no Python frames because attach refused"
                // from "no Python frames because the walk failed".
                //
                // Counted on the FIRST such frame only, then the sample is
                // marked done. Without the gate this bumps once per
                // eval-loop native frame per sample, forever, for every
                // refused interpreter on the box -- which makes the number
                // scale with stack depth rather than with occurrences and
                // breaks the unit rule the whole counter set is read under
                // (see PY_CNT_* in python_walk.h: slot 0 counts frames,
                // every other slot counts samples).
                if (ctx->py_state == PY_CHAIN_UNSTARTED) {
                    py_count(PY_CNT_NO_PROC_INFO);
                }
                ctx->py_state = PY_CHAIN_DONE;
            }
        }

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
            if (fp_chain_ended(ctx)) {
                // This frame is the one step taken past the end of the
                // frame-pointer chain, and its CFI had the chance to say the
                // chain ends here (RA_TYPE_UNDEFINED, below) and did not.
                // The two sources DISAGREE: the frame pointer read out of
                // the running stack says this is the root, the unwind tables
                // say a caller exists. One of them is wrong and this walker
                // cannot tell which, so it believes the FP chain — following
                // the CFI would mean reading a caller slot at a CFA derived
                // from a frame the psABI has already called outermost.
                //
                // But it does NOT stop quietly. WALKER_FLAG_FP_TERMINATED is
                // already set from one frame below, so without a bit of its
                // own this walk would be filed as a clean termination and no
                // counter would move — a counter reading green in a case
                // that may well be a failure, which is precisely the defect
                // issue #44 exists to remove. Flag it.
                ctx->rec->hdr.walker_flags |= WALKER_FLAG_ROOT_DISAGREEMENT;
                return 1;
            }
            if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr),
                                    (void *)(cfa + (__s64)e.ra_offset)) != 0) return 1;
        } else if (e.ra_type == RA_TYPE_UNDEFINED) {
            // The CFI says this frame has no return address: it is the
            // outermost frame of the chain (glibc marks _start and thread
            // entry points this way). The walk is COMPLETE, not stuck.
            ctx->rec->hdr.walker_flags |= WALKER_FLAG_RA_UNDEFINED;
            return 1;
        } else {
            // SAME_VALUE (leaf on arm64) or REGISTER — the return address
            // lives in a register we do not track, so we cannot proceed
            // even though a caller exists. A genuine stop, left unflagged.
            return 1;
        }

        __u64 new_fp = ctx->fp;
        if (e.fp_type == FP_TYPE_OFFSET_CFA) {
            if (bpf_probe_read_user(&new_fp, sizeof(new_fp),
                                    (void *)(cfa + (__s64)e.fp_offset)) != 0) return 1;
        } else if (e.fp_type == FP_TYPE_SAME_VALUE) {
            // The callee-saved register was never touched in this frame, so
            // the caller's value is still live in it: new_fp unchanged.
            //
            // Since issue #45 this is also what "the CFI carries no rule for
            // the frame-pointer register" compiles to — the x86-64 psABI and
            // AAPCS64 both make an unmentioned callee-saved register
            // unchanged, and ehcompile used to emit UNDEFINED for it. That
            // one-value change is why an FP_SAFE frame above an FP-less one
            // now has a frame pointer to walk. See unwind/ehcompile's
            // archDefaultFPRule.
            //
            // KNOWN LIMITATION, and it is a real one rather than a
            // theoretical one. "No rule" is an inference about the producer,
            // not a statement by it, and hand-written assembly can break the
            // inference: a routine that does `push %rbp` and then uses %rbp
            // as a scratch register under an FDE that is entirely
            // DW_CFA_nop tells the unwinder nothing, and this rule then
            // propagates a %rbp that holds arithmetic, not a frame base.
            // Three such functions exist in Fedora's glibc — __mpn_addmul_1,
            // __mpn_submul_1 (scratch use) and __swapcontext (reloads %rbp
            // from the target ucontext) — out of 20976 no-rule FDEs across
            // six binaries surveyed for #45, and none at all in
            // libcuda.so.1. See the report for the scan.
            //
            // What changed is the FAILURE MODE, not the outcome: in those
            // ranges the walk was already lost (for the two __mpn_ routines
            // the CFA rule is wrong too — it stays rsp+8 after two pushes),
            // but it used to be lost LOUDLY, as new_fp = 0 and a counted
            // WALKER_FLAG_FP_EXHAUSTED one frame later. Now the garbage
            // propagates: most often it faults on the next read (an
            // unflagged stop), sometimes it lands in
            // WALKER_FLAG_FP_NONMONOTONIC, and with small probability it
            // yields a plausible bogus frame. That last case is the one
            // nothing catches, and it is the price of the 12-27% of every
            // shipped library's code that this rule un-truncates.
        } else {
            // UNDEFINED / REGISTER — the CFI positively says the register
            // does not hold the caller's value and does not say where it
            // does, so the FP is lost. Continuing via DWARF is still fine;
            // FP-based frames further up will hit the ctx->fp == 0 arm.
            new_fp = 0;
        }

        ctx->pc = ret_addr;
        ctx->fp = new_fp;
        ctx->sp = cfa;
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_DWARF_USED;
        return 0;
    }

    // FP_SAFE or FALLBACK — same path: FP walk.
    if (ctx->fp == 0) {
        if (fp_chain_ended(ctx)) {
            // The frame pointer is zero because the chain ended one step
            // below, at a frame whose saved-FP slot held the psABI's
            // outermost-frame marker, and this frame is the caller that slot
            // named. It has been recorded. Without unwind tables classifying
            // it FP_LESS there is nothing further to read — but the walk
            // FINISHED, it did not fail, and bit 0 already says so. Falling
            // into the WALKER_FLAG_FP_EXHAUSTED arm below would relabel
            // every complete frame-pointer walk a failure.
            return 1;
        }
        // No caller frame pointer to follow, so this walk stops HERE — and
        // this is a FAILURE, not an end of chain. Nothing in the unwind
        // information said this frame was outermost. Almost always the frame
        // pointer was lost one step earlier, when the DWARF rules of the
        // FP_LESS frame below gave no location for it (fp_type UNDEFINED /
        // REGISTER sets new_fp = 0 above); the remaining case is a thread
        // whose FP register was already zero when the sample was taken,
        // which for a probe firing deep inside application code means the
        // register was in use for something else, not that the stack ended.
        // Either way whatever called this frame is real and is missing.
        //
        // This flag used to be WALKER_FLAG_UNWIND_TERMINATED, shared with
        // the ra_type == UNDEFINED arm above, on the claim that it recorded
        // "the same fact saved_fp == 0 records one step later". That claim
        // was false (issue #44): saved_fp == 0 is a real chain root read out
        // of live memory, while ctx->fp == 0 here is the walker's own DWARF
        // step having zeroed the register. Sharing one bit made every such
        // truncation read as a clean termination.
        //
        // Reading user memory at address 0 to rediscover this would fault,
        // which before this check existed made such a walk indistinguishable
        // from one killed by a genuine read fault — hence a flag rather than
        // a bare `return 1`. The distinction it now carries is which KIND of
        // stop it was, and this one belongs with the failures.
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_FP_EXHAUSTED;
        return 1;
    }
    __u64 saved_fp = 0, ret_addr = 0;
    if (bpf_probe_read_user(&saved_fp, sizeof(saved_fp), (void *)ctx->fp) != 0) return 1;
    if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr), (void *)(ctx->fp + 8)) != 0) return 1;
    if (saved_fp <= ctx->fp) {
        // The saved-FP slot does not name a caller frame. Two causes, and
        // they are reported apart because they mean different things:
        //
        //   saved_fp == 0  the psABI's outermost-frame marker. _start does
        //                  `xorl %ebp, %ebp` before calling
        //                  __libc_start_main, and the clone child does the
        //                  same before calling a thread entry point, so the
        //                  frame that reads zero here is the LAST one with a
        //                  frame pointer, not a broken one. A SUCCESS.
        //   otherwise      a frame pointer that does not increase: a corrupt
        //                  or hand-rolled frame, or a %rbp holding something
        //                  that is not a frame base at all. A FAILURE.
        //
        // Either way ret_addr was already read, out of a DIFFERENT slot of
        // the same frame, and it is the caller's PC regardless of what the
        // saved-FP slot holds. Both arms used to `return 1` and throw it
        // away, which cost the outermost frame of every stack the walker
        // produced — `_start` on a main-thread stack (issue #45).
        if (saved_fp == 0) {
            ctx->rec->hdr.walker_flags |= WALKER_FLAG_FP_TERMINATED;
            if (ret_addr == 0) return 1;
            // Hand the caller's PC to the next iteration rather than
            // appending it here: the loop head records it and then lets the
            // unwind tables classify it, which is the only way the walk can
            // reach a frame whose CFI marks it outermost (glibc's `_start`
            // and clone child) and end with WALKER_FLAG_RA_UNDEFINED. Both
            // guards above (fp_chain_ended) bound this to a SINGLE extra
            // step, so a walk cannot wander past the root on a bad read.
            ctx->sp = ctx->fp + 16;
            ctx->pc = ret_addr;
            ctx->fp = 0;
            return 0;
        }
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_FP_NONMONOTONIC;
        // Not carried forward the way the zero case is: the frame this came
        // out of is already suspect, so record the one value that is still
        // meaningful and stop rather than classify a PC on its say-so.
        //
        // Routed through frame_push_native (rather than a bare bounds
        // check + direct write) so this slot's tags[] byte is set too.
        // walker_scratch is a REUSED per-CPU buffer: an untagged slot here
        // would carry forward whatever FRAME_TAG_* byte a previous sample
        // on this CPU last left in it, and once frame_push_python has a
        // caller that stale byte can read back as FRAME_TAG_PYTHON — a
        // native frame silently decoding as half of a Python pair.
        if (ret_addr != 0) frame_push_native(ctx, ret_addr);
        return 1;
    }

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
