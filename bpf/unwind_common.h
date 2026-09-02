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

// The record, the persisted walk state, the three maps both sides of the
// handoff touch, and the two frame pushers. That header is the ENTIRE
// contract between this walker and an unwinder for a language whose frames
// are not on the machine stack -- see bpf/interp/ for what one looks like.
#include "unwind_record.h"

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

// walk_ctx holds the per-ENTRY state of one native walk: the registers the
// walk is standing on, the cursor into the record, and the record itself.
// Lives on the BPF entry function's stack, which is where bpf_loop wants its
// callback context.
//
// Everything that must survive a tail call lives in struct walk_persist
// instead (unwind_record.h); walk_load/walk_save below move between the two.
// The split is forced: a map value may not hold a pointer, and `rec` is one.
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
    // the _Static_assert on MAX_FRAMES <= 255 in unwind_record.h is what
    // proves that cast cannot truncate.
    //
    // The width is NOT what makes a bounds check safe here. The wrap bug CI
    // caught (see record_push_interp) came from doing arithmetic on the
    // checked value; a __u8 field would have hidden that one instance behind
    // integer promotion while leaving the pattern in place to bite the next
    // helper that needs a two-slot bound. Check against a compile-time
    // constant instead.
    __u32 n_pcs;
    struct sample_record *rec;
    // Set by walk_step when another unwinder claims the frame it stopped on;
    // read by the driver after bpf_loop returns. UNWINDER_NATIVE means the
    // walk ended for one of the ordinary reasons.
    __u32 pending_unwinder;
    // One bit per unwinder id, mirroring walk_persist.interp_done: an
    // unwinder that has declared itself finished with this sample is not
    // offered another frame. Read by walk_step, written only by a module.
    __u32 interp_done;
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

// BINARY_SEARCH_MAX_ITERS bounds the binary search over the CFI and
// classification tables. A search over n sorted rows needs ceil(log2(n))
// halvings, so this bound is a CEILING ON HOW BIG A BINARY CAN BE UNWOUND --
// and when a binary exceeds it the failure is silent and total.
//
// It was 20, on the reasoning that log2(1,000,000) is about 20 and that
// "suffices for any realistically sized binary". PyTorch is a realistically
// sized binary and it does not:
//
//	libtorch_cpu.so    2,359,137 CFI rows   needs 22   HAD 20
//	libtorch_cuda.so     947,971            needs 20   had 20  (exactly at it)
//	libtorch_python.so   283,904            needs 19
//	libcuda.so           135,805            needs 18
//	libcupti.so           46,422            needs 16
//
// With 20 iterations a 2.36M-row table narrows to a range of two or three and
// then the loop ends, so the lookup fails for almost every PC. BOTH failure
// paths then lie: cfi_lookup returns NULL, which walk_step reads as a CFI miss
// and STOPS the walk; classify_rel_pc returns MODE_FP_SAFE, which sends a
// frame-pointer-less frame down the frame-pointer path. Measured on a real
// GPU + PyTorch capture: 4,299 launch stacks, every one abandoned, none
// reaching root, all of them stopping inside libtorch's dispatcher
// (at::_ops::mm::redispatch, structured_clamp_min_out::impl, add_kernel) --
// the first library on the way up whose table is too big to search.
//
// 24 covers 16.7M rows, a little over 7x the largest table anyone has put in
// front of this walker. The cost is linear and was measured rather than
// assumed, on perf_dwarf: 20 -> 160,530 processed, 22 -> 186,978,
// 24 -> 206,062, 26 -> 223,810. About 9,600 per iteration, two searches per
// frame, inside the loop callback where everything is expensive.
//
// ehmaps refuses to install a table this search cannot reach, and says so:
// silently installing one is indistinguishable from having none, except that
// it also stops the walk. See ehmaps.MaxSearchableRows.
#define BINARY_SEARCH_MAX_ITERS 24

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
// WALKER_FLAG_FRAME_PUSH_REFUSED (0x80) is defined in unwind_record.h, beside
// the two pushers that raise it, so an interpreter module compiled against
// that header alone still has the name.

// frame_push_native appends one native-PC slot to the record this walk is
// building. Returns 0 on success, 1 if the record is full -- the caller must
// stop walking in that case.
//
// The bounds discipline that makes this safe, and the two CI rejections that
// taught it, are documented once at record_push_native in unwind_record.h,
// where the check actually lives. It lives THERE and not here because an
// interpreter module -- compiled into its own object, sharing only that
// header -- appends to the same record, and a second copy of a bounds check
// is how the two drift.
static __always_inline int frame_push_native(struct walk_ctx *ctx, __u64 pc) {
    return record_push_native(ctx->rec, &ctx->n_pcs, pc);
}

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

// ----- Handing a frame to another unwinder.
//
// The core walks native frames. When a PC belongs to something that needs its
// own walker -- a language runtime whose frames are not on the machine stack --
// the walk stops there and hands off. This is the ONLY thing the core knows
// about that: a range of text, and an opaque id saying who claims it.
//
// Keyed by table_id, which mapping_for_pc has already computed, so a process
// with no registered unwinder pays one hash on a value it is holding anyway.
// lo/hi are in the same load-bias-relative space mapping_for_pc reports
// rel_pc in.
#define UNWINDER_NATIVE 0

// HANDOFF_MAX_RANGES is how many disjoint text spans one claim may cover.
//
// IT IS NOT ONE, AND THAT COST US A DAY. A compiler is free to split a
// function across several partitions, and CPython's eval loop is exactly the
// function it does that to. Measured on uv's cpython-3.12.14, the build a
// PyTorch venv actually runs:
//
//   _PyEval_EvalFrameDefault        66,065 bytes   <- the hot dispatch loop
//   _PyEval_EvalFrameDefault.warm   28,019
//   _PyEval_EvalFrameDefault.cold  135,934        <- the LARGEST, and cold
//   _PyEval_EvalFrameDefault.org.0       5
//
// With one span per binary the installer had to choose, chose the largest, and
// therefore claimed the partition the compiler had marked RARELY EXECUTED.
// Samples land in the first two; none ever fell inside the claim; the handoff
// never fired on a workload where 86% of samples were sitting in the eval
// loop. Every counter read zero because nothing had gone wrong -- nothing had
// happened.
//
// Spanning min..max instead would have been worse than the bug: those
// fragments are 3 MB apart, so the claim would swallow unrelated CPython
// functions and hang a Python stack off a native frame that is not the eval
// loop at all. That is the plausible-stack-that-never-happened failure this
// design refuses everywhere else. The union has to be the actual union.
//
// THREE IS A MEASURED CEILING, not a guess. The scan is unrolled into the
// walk callback, which the verifier re-explores per frame, and the cost is a
// cliff rather than a slope. perf_dwarf, processed instructions:
//
//   1 span  176,701      2 spans 158,942      3 spans 160,530
//   4 spans 468,718      8 spans REJECTED at the 1,000,001 ceiling
//
// (Two and three come out BELOW one because the value is copied out of the
// map once instead of being dereferenced twice; see next_unwinder.)
//
// Three covers uv's three real fragments exactly and every other build
// surveyed with room. A binary with more gets its SMALLEST fragments dropped
// by the installer, which says so in the log rather than silently covering
// less than it claims.
#define HANDOFF_MAX_RANGES 3

struct handoff_span { __u64 lo, hi; };

struct handoff_range {
    // Unused spans are zeroed, and zero never matches: `rel_pc >= 0 &&
    // rel_pc < 0` is false for every rel_pc. So the scan below needs no
    // count, no bound check and no early exit -- which is the whole reason it
    // is affordable in the loop callback.
    struct handoff_span spans[HANDOFF_MAX_RANGES];
    __u32 unwinder_id;
    __u32 _pad;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u64);              // table_id
    __type(value, struct handoff_range);
    __uint(max_entries, 64);
} handoff_ranges SEC(".maps");

// interp_enabled gates the handoff at LOAD time, and its placement is part of
// the design rather than a wrapper around it.
//
// The question it answers is not "can we turn one language off" but "what does
// a deployment with no interpreters pay". Asking it costs real verifier
// budget, and that is a cost every user would carry, including those who never
// profile a runtime. A `const volatile` is a constant to the verifier, so with
// this false the lookup, the span scan AND the early-return branch are pruned
// before verification and the program is exactly the native walker again.
//
// Default true; userspace turns it off on a kernel that refuses the program
// (profile.loadWithInterpGate).
#ifndef INTERP_DEFAULT
#define INTERP_DEFAULT true
#endif
const volatile bool interp_enabled = INTERP_DEFAULT;

// next_unwinder answers "does something other than the native walker claim
// this PC, and is it still interested". UNWINDER_NATIVE means carry on.
//
// The span scan is UNROLLED WITH NO EARLY EXIT, deliberately. Every measured
// regression in this walker came from adding a branch that leaves the body:
// the comparisons below all merge into one boolean and there is exactly one
// exit, so the verifier explores the body once. Breaking out of the loop on a
// hit would be the cheap-looking version that costs.
//
// `done` is walk_ctx.interp_done: one bit per unwinder id, set by a module
// that has finished with this sample. Honouring it here rather than in the
// module is what keeps a deep stack cheap -- a Python process's native walk
// can cross the eval loop a dozen times in one sample, and without this every
// crossing after the first costs a full round trip to be told nothing.
static __always_inline __u32 next_unwinder(__u64 table_id, __u64 rel_pc, __u32 done) {
    struct handoff_range *h = bpf_map_lookup_elem(&handoff_ranges, &table_id);
    if (!h) return UNWINDER_NATIVE;
    interp_count(INTERP_STAT_RANGE_HIT);

    // BRANCHLESS ON PURPOSE, and this is the difference between the scan
    // fitting and not fitting. BPF has no conditional move, so every `<`
    // compiles to a jump; written the obvious way as
    // `inside |= (rel_pc >= lo && rel_pc < hi)`, eight spans are sixteen
    // jumps whose paths multiply inside a callback the verifier already
    // re-explores per frame. Measured on perf_dwarf, processed instructions:
    //
    //   1 span   176,701      2 spans  177,281      (a branch pair is nearly free)
    //   4 spans  497,588      8 spans  REJECTED at 1,000,001
    //
    // The sign-bit form below has no jumps at all: eight spans cost eight
    // times a handful of ALU ops and the verifier walks one path.
    //
    // It is exact rather than clever. Every address here is a link-time
    // vaddr well under 2^63, so `(__s64)rel_pc - (__s64)lo` cannot overflow,
    // and its sign bit IS the predicate: clear means rel_pc >= lo, set means
    // rel_pc < hi for the second term. An unused span is zeroed, and for
    // lo == hi == 0 both sign bits come out clear, so it contributes nothing
    // -- which is why the scan needs no count and no bound check.
    struct handoff_range hr = *h;
    __u32 inside = 0;
#pragma unroll
    for (int i = 0; i < HANDOFF_MAX_RANGES; i++) {
        if (rel_pc >= hr.spans[i].lo && rel_pc < hr.spans[i].hi) inside = 1;
    }
    if (!inside) return UNWINDER_NATIVE;
    interp_count(INTERP_STAT_IN_RANGE);

    __u32 id = hr.unwinder_id;
    // The mask is on the id read out of the map value, not on a re-read: see
    // the bounds discipline at record_push_native. 31 rather than
    // INTERP_MAX_UNWINDERS-1 so a shift is always defined even if a map entry
    // carries an id this build has no slot for; such an id simply finds an
    // empty interp_progs slot and the tail call fails, which the dispatch
    // counters now name.
    if (done & (1u << (id & 31))) return UNWINDER_NATIVE;
    return id;
}

// walk_load rebuilds a stack walk_ctx from the persisted scalars, pointing it
// at this entry's freshly looked-up record. See struct walk_persist in
// unwind_record.h for why the two structs are not one.
static __always_inline void walk_load(struct walk_ctx *w, struct walk_persist *st,
                                      struct sample_record *rec) {
    w->pc = st->pc;
    w->fp = st->fp;
    w->sp = st->sp;
    w->pid = st->pid;
    w->n_pcs = st->n_pcs;
    w->rec = rec;
    w->pending_unwinder = st->pending_unwinder;
    w->interp_done = st->interp_done;
}

// walk_save writes back everything a resumed walk needs.
static __always_inline void walk_save(struct walk_persist *st, struct walk_ctx *w) {
    st->pc = w->pc;
    st->fp = w->fp;
    st->sp = w->sp;
    st->pid = w->pid;
    st->n_pcs = w->n_pcs;
    st->pending_unwinder = w->pending_unwinder;
    st->interp_done = w->interp_done;
}

// unwind_frame does ONE frame: record it, offer it to another unwinder, then
// advance pc/fp/sp to its caller. Returns 0 to continue, 1 to stop.
//
// `lead` is a COMPILE-TIME constant at both call sites and that is the whole
// point of it being a parameter.
//
//   true  — the ordinary case. The frame has not been seen: push it, and ask
//           whether something else claims it.
//   false — the ONE frame a resumed walk starts on. It was pushed before the
//           handoff and the unwinder that claimed it has already appended its
//           frames after it; all that is left is to unwind past it. Asking
//           again would hand the same PC to the same unwinder forever.
//
// WHY A CONSTANT AND NOT A FLAG IN walk_ctx. Measured, on this kernel with
// this walker: reading the "am I resuming" bit out of walk_ctx inside the
// bpf_loop callback took perf_dwarf from 187,342 processed instructions to
// 519,965 — a 2.8x tax on every sample to describe a condition that holds for
// at most one frame of one sample. The bit makes the two paths differ in
// n_pcs, so the verifier explores the entire body twice from the top and
// keeps both states alive to the end. As a constant, each expansion has one
// path: the loop callback below never carries the resume case at all, and the
// resume case is one straight-line inlining in the driver's resume program.
static __always_inline long unwind_frame(struct walk_ctx *ctx, bool lead) {

    if (lead && frame_push_native(ctx, ctx->pc)) goto stop;

    // Per-frame mapping lookup: which binary this PC is in, and where in it.
    struct mapping_lookup_result m = mapping_for_pc(ctx->pid, ctx->pc);
    __u8 have_tables = 0;
    if (m.found) {
        // ----- The handoff.
        //
        // walk_step DECIDES; the driver dispatches. A bpf_tail_call from
        // inside a bpf_loop callback does not load (measured: "bad address"),
        // so this records who claims the frame and stops the loop HERE, with
        // the cursor intact. The driver acts on it after bpf_loop returns, and
        // because the walk stopped AT this frame with nothing pushed since,
        // the frames that unwinder appends land immediately after it -- the
        // ordering ruling T7-R7 asks for, kept in the kernel, with no anchor
        // and no userspace splice.
        if (interp_enabled && lead) {
            __u32 u = next_unwinder(m.table_id, m.rel_pc, ctx->interp_done);
            if (u != UNWINDER_NATIVE) {
                interp_count(INTERP_STAT_CLAIMED);
                ctx->pending_unwinder = u;
                return 1;
            }
        }

        // Lazy mode (Option A2): pid_mappings is populated but the CFI may
        // not be compiled yet. Detect by probing cfi_classification_lengths;
        // if missing, the binary was enrolled but not yet compiled -- emit a
        // miss event so the userspace drainer compiles on demand. This frame
        // takes the frame-pointer path; the next sample after the compile
        // completes unwinds properly.
        //
        // A single hash lookup, NOT a binary search: what used to sit here was
        // classify_rel_pc, a second 24-iteration search over the
        // classification table for every frame. Its answer chose between the
        // DWARF path and the frame-pointer path, and that choice is now made
        // by whether a CFI ROW EXISTS -- which the DWARF path has to look up
        // anyway. Removing it paid for using the tables on every frame: two
        // searches per frame cost 580,876 processed instructions, one costs
        // less than the single search did before.
        have_tables = bpf_map_lookup_elem(&cfi_classification_lengths, &m.table_id) != NULL;
        if (!have_tables) {
            emit_cfi_miss(ctx->pid, m.table_id, m.rel_pc);
        }
    }

    // ----- The unwind tables are used WHENEVER THEY EXIST, not only for
    // frames whose CFA is SP-based.
    //
    // This used to read `if (mode == MODE_FP_LESS)`, so a frame classified
    // FP_SAFE was walked by following the frame-pointer chain even though a
    // CFI row for it was sitting in the map. That is correct only while the
    // WHOLE chain has frame pointers, and it is not a shortcut that fails
    // safely: the FP step reads the saved-FP slot and refuses a value that
    // does not look like a frame pointer (WALKER_FLAG_FP_NONMONOTONIC), so
    // the walk STOPS at the first frame whose caller was compiled without
    // one -- which is all of Rust, all of CUDA, and most optimised C++.
    //
    // MEASURED, replaying this walker in userspace against a live
    // rust_cuda_rt at a kernel launch (unwind/ehcompile's walk replica):
    //
    //   frame 13  __device_stub__Z8rs_scalePffi   FP_SAFE
    //     the CFI says      cfa = fp+16, ra = 0x56021ae2b621   <- correct
    //     the FP step reads saved_fp = [fp] = 0x2c1332         <- not a pointer
    //     -> FP NON-MONOTONIC, walk abandoned at 14 frames
    //
    // The return address was in the SAME SLOT both ways; only the saved-FP
    // sanity check differed, and it rejected a frame the tables could
    // describe perfectly. Preferring the tables takes that walk from 14
    // frames to 22 and ends it at _start with RA_UNDEFINED -- a walk that
    // reaches the root instead of one that is abandoned.
    //
    // The frame-pointer path is what it always should have been: the fallback
    // for a frame with NO row, which is the FALLBACK classification (a CFI
    // expression this compiler declines) and the gaps between FDEs.
    struct cfi_entry *ep = m.found ? cfi_lookup(m.table_id, m.rel_pc) : NULL;
    if (!ep && have_tables) {
        // The binary's tables are compiled and no row covers this PC: a
        // genuine gap in the unwind information, not a binary we have yet to
        // compile. Flagged, but the walk CONTINUES down the frame-pointer
        // path -- a gap is exactly the case that path exists for, and
        // stopping here would lose every frame beneath a single uncovered
        // one.
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_CFI_MISS;
    }
    if (ep) {
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
                goto stop;
            }
            if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr),
                                    (void *)(cfa + (__s64)e.ra_offset)) != 0) goto stop;
        } else if (e.ra_type == RA_TYPE_UNDEFINED) {
            // The CFI says this frame has no return address: it is the
            // outermost frame of the chain (glibc marks _start and thread
            // entry points this way). The walk is COMPLETE, not stuck.
            ctx->rec->hdr.walker_flags |= WALKER_FLAG_RA_UNDEFINED;
            goto stop;
        } else {
            // SAME_VALUE (leaf on arm64) or REGISTER — the return address
            // lives in a register we do not track, so we cannot proceed
            // even though a caller exists. A genuine stop, left unflagged.
            goto stop;
        }

        __u64 new_fp = ctx->fp;
        if (e.fp_type == FP_TYPE_OFFSET_CFA) {
            if (bpf_probe_read_user(&new_fp, sizeof(new_fp),
                                    (void *)(cfa + (__s64)e.fp_offset)) != 0) goto stop;
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
            goto stop;
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
        goto stop;
    }
    __u64 saved_fp = 0, ret_addr = 0;
    if (bpf_probe_read_user(&saved_fp, sizeof(saved_fp), (void *)ctx->fp) != 0) goto stop;
    if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr), (void *)(ctx->fp + 8)) != 0) goto stop;
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
            if (ret_addr == 0) goto stop;
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
        goto stop;
    }

    // Caller's resume SP: after a standard prologue (push FP; move FP=SP
    // on x86_64; equivalent stp x29, x30 on arm64), the caller's SP at
    // the return instruction is current FP + 16 (saved FP + saved RA).
    // Matters when the next frame up is FP_LESS with CFA rooted at SP.
    ctx->sp = ctx->fp + 16;
    ctx->pc = ret_addr;
    ctx->fp = saved_fp;
    return 0;

stop:
    return 1;
}

// walk_step is the bpf_loop callback: one leading frame per iteration.
//
// NOTHING RESUME-SHAPED MAY BE READ IN HERE. Measured on this kernel: adding a
// single "am I resuming" bit read out of the callback context took perf_dwarf
// from 187,918 processed instructions to 519,965, and a stop-only bit read
// here took it to 374,355. A bit read inside a bpf_loop callback is paid at
// every state of the walk, which is why the whole resume protocol lives
// outside the loop -- in a program of its own (INTERP_DEFINE_PROGRAMS) and in
// one compile-time-constant branch in unwind_walk.
static long walk_step(__u32 idx, void *arg) {
    return unwind_frame((struct walk_ctx *)arg, true);
}

// ----- Driving the walk, and handing off in the middle of it.
//
// INTERP_TAIL_CALL_BUDGET bounds the native<->interpreter round trips in one
// sample, and the number is arithmetic against the kernel's own ceiling
// rather than a taste.
//
// ONE ROUND TRIP IS THREE TAIL CALLS: out to the unwinder, from there to the
// driver's resume-step program, and from there to its resume-walk program.
// The kernel's MAX_TAIL_CALL_CNT is 33 and is enforced silently -- the 34th
// bpf_tail_call simply returns, which here would end the walk mid-stack with
// the record still holding everything collected so far. That is a truncated
// sample rather than a wrong one, but it is a truncation nothing counts, so
// the budget is set to stop first, where the stop is deliberate.
//
// 10 round trips is 30 tail calls, three under the ceiling. A stack that
// crosses an interpreter's text more than ten times in one sample is deeper
// than MAX_FRAMES can hold anyway.
#define INTERP_TAIL_CALL_BUDGET 10

// unwind_walk_begin initialises the persisted state for a FRESH sample. Every
// driver calls it once, before the first unwind_walk; the resume program does
// not, which is the whole difference between the two entry points.
static __always_inline void unwind_walk_begin(struct walk_persist *st,
                                              struct sample_record *rec,
                                              __u32 pid, __u32 tid,
                                              __u64 pc, __u64 fp, __u64 sp) {
    st->pc = pc;
    st->fp = fp;
    st->sp = sp;
    st->pid = pid;
    st->tid = tid;
    st->n_pcs = 0;
    st->pending_unwinder = UNWINDER_NATIVE;
    st->interp_done = 0;
    st->tail_calls = 0;
    st->stopped = 0;
    // The opaque save area belongs to whichever unwinder claims a frame in
    // this sample; zeroing it here is how a module knows it is starting a
    // fresh chain rather than resuming the previous sample's. Four stores in
    // the driver body, which the anchor measurement (rd-report.md) showed
    // costs the verifier nothing.
    st->interp_scratch[0] = 0;
    st->interp_scratch[1] = 0;
    st->interp_scratch[2] = 0;
    st->interp_scratch[3] = 0;
    rec->hdr.walker_flags = 0;
}

// unwind_walk runs the native walk to its end, dispatching to interpreter
// unwinders as it goes, and returns the number of pcs[] slots written.
//
// IT MAY NOT RETURN AT ALL. When walk_step stops on a frame another unwinder
// claims, this tail-calls that unwinder -- which replaces the running program.
// The module appends its frames and tail-calls the driver's resume program
// (INTERP_DEFINE_PROGRAMS), which calls this again and finishes the sample.
// A driver therefore may not hold anything that must be released -- a reserved
// ringbuf record above all -- across this call.
//
// The failure path is deliberately quiet: if nothing is installed in the
// requested slot, or the kernel's tail-call limit is reached, bpf_tail_call
// returns and the walk ends here with the record as far as it got. That is a
// SHORT sample, not a wrong one; the frames already in it are in the right
// order and the missing ones are simply absent.
static __always_inline __u32 unwind_walk(void *ctx, struct walk_persist *st,
                                         struct sample_record *rec, bool resumed) {
    struct walk_ctx w;
    walk_load(&w, st, rec);

    // `resumed` is a compile-time constant at both call sites, so the entry
    // program does not carry this test at all. On the resume side, stopped
    // means the one step the resume-step program took could not unwind past
    // the frame it was given: the cursor never moved, and running the loop
    // would push that frame a second time.
    if (!resumed || !st->stopped) {
        // Walk at most MAX_FRAMES frames. walk_step breaks early on read
        // failure, on a natural terminator (saved_fp == 0, saved_fp <= fp), or
        // on a frame another unwinder claims.
        bpf_loop(MAX_FRAMES, walk_step, &w, 0);

            if (interp_enabled && w.pending_unwinder != UNWINDER_NATIVE &&
            st->tail_calls >= INTERP_TAIL_CALL_BUDGET) {
            // Claimed, and refused for want of round trips. Counted here
            // rather than left as a silent truncation: it is the one ending
            // that looks exactly like a stack that simply had no more
            // interpreter frames in it.
            interp_count(INTERP_STAT_BUDGET);
        }
        if (interp_enabled && w.pending_unwinder != UNWINDER_NATIVE &&
            st->tail_calls < INTERP_TAIL_CALL_BUDGET) {
            __u32 slot = w.pending_unwinder;
            // Cleared before the save so the module and the resumed walk both
            // see a walk with no pending handoff; walk_step sets it again if
            // the resumed walk lands on another claimed frame.
            w.pending_unwinder = UNWINDER_NATIVE;
            walk_save(st, &w);
            st->tail_calls += 1;
            interp_count(INTERP_STAT_DISPATCHED);
            bpf_tail_call(ctx, &interp_progs, INTERP_SLOT_UNWINDER(slot));
            // REACHED ONLY WHEN THE TAIL CALL DID NOT HAPPEN -- nothing is
            // installed in that slot, or the kernel's own limit was reached.
            // The walk ends here with the record as far as it got, which is a
            // short sample rather than a wrong one, and this is the counter
            // that stops it being a silent one.
            interp_count(INTERP_STAT_TAILCALL_FAILED);
        }
    }

    walk_save(st, &w);
    return w.n_pcs > MAX_FRAMES ? MAX_FRAMES : w.n_pcs;
}

// INTERP_DEFINE_PROGRAMS is the ONE interpreter-shaped line in a driver, and
// it names no interpreter.
//
// It defines the TWO programs an interpreter module hands control back
// through, both with the driver's own section string because every entry of a
// prog array must share the type of the program that tail-calls into it.
// `walk_fn` is the driver's own walk-and-emit function -- the same one its
// entry program calls once the state is initialised -- because resuming a
// sample means finishing it, and only the driver knows how it emits.
//
// WHY TWO AND NOT ONE. A resumed walk begins on the frame the handoff stopped
// at: already pushed, already served, and still needing to be unwound past.
// That one step is a whole frame's worth of code including the mapping scan's
// own bpf_loop, and putting it in the same program as the main walk gives that
// program two bpf_loop call sites -- which the verifier refuses outright, at
// the 1,000,001 ceiling, measured. Sequential sites are no better than nested
// ones here. So the step gets a program of its own, and tail-calls the walk.
//
// The cost is one more tail call per handoff, against a budget of
// INTERP_TAIL_CALL_BUDGET. If the step cannot unwind past its frame the walk
// is over -- the cursor never moved, and re-entering the loop would push the
// same frame again -- so it says so through walk_persist.stopped, which
// walk_step reads at the top and nowhere else.
//
// A driver expands this once, at file scope, after including unwind_common.h.
#define INTERP_DEFINE_PROGRAMS(sec, walk_fn)                                   \
    SEC(sec) int interp_resume_step(void *ctx) {                                \
        struct walk_persist *st = walk_state_get();                             \
        struct sample_record *rec = walk_record_get();                          \
        if (st && rec) {                                                        \
            struct walk_ctx w;                                                  \
            interp_count(INTERP_STAT_RESUMED);                                  \
            walk_load(&w, st, rec);                                             \
            if (unwind_frame(&w, false)) st->stopped = 1;                       \
            walk_save(st, &w);                                                  \
        }                                                                       \
        bpf_tail_call(ctx, &interp_progs, INTERP_SLOT_RESUME_WALK);             \
        return 0;                                                               \
    }                                                                           \
    SEC(sec) int interp_resume_walk(void *ctx) {                                \
        walk_fn(ctx, true);                                                     \
        return 0;                                                               \
    }

#endif // PERF_AGENT_UNWIND_COMMON_H
