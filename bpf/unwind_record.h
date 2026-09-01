// unwind_record.h — the ABI between the native stack walker and any
// unwinder for a language whose frames are not on the machine stack.
//
// This header is the WHOLE contract. The core walker (unwind_common.h,
// compiled into perf_dwarf / offcpu_dwarf / gpu_usdt) includes it, and so
// does every interpreter module — which is compiled as its own BPF object,
// in its own directory, and shares these three maps with the driver at LOAD
// time through cilium/ebpf's MapReplacements. Nothing else crosses.
//
// What is here, and why each piece has to be:
//
//   struct sample_record  the frame array both sides append to, and the
//                         cursor into it (walk_persist.n_pcs).
//   struct walk_persist   the walk state that has to survive a tail call,
//                         because a tail call replaces the running program
//                         and the driver's stack goes with it.
//   walker_scratch        where the record lives (it is 1184 bytes; the BPF
//                         stack is 512).
//   walk_states           where walk_persist lives, for the same reason.
//   interp_progs          the tail-call table: slot 0 is the driver's resume
//                         program, slot N the unwinder with id N.
//   frame_push_native /   the two ways a slot is written, with the bounds
//   frame_push_interp     discipline in ONE place rather than re-derived at
//                         each call site.
//
// What is deliberately NOT here: anything about a particular language.
// The core knows a range of text and an opaque id (see handoff_ranges in
// unwind_common.h); a module knows its own runtime. Neither knows the other.
#ifndef PERF_AGENT_UNWIND_RECORD_H
#define PERF_AGENT_UNWIND_RECORD_H

#ifndef __VMLINUX_H__
#include "vmlinux.h"
#endif
#include <bpf/bpf_helpers.h>

// MAX_FRAMES: the unwind walker's per-sample loop bound. Matches the
// BPF_MAP_TYPE_STACK_TRACE convention; deeper stacks truncate.
#ifndef MAX_FRAMES
// Overridable with -D only so a verifier experiment can measure how the cost
// scales with the outer loop's bound (see verifier-plan.md's ladder); the
// build never sets it, and changing it changes struct sample_record, which
// unwind/dwarfagent parses by byte offset.
#define MAX_FRAMES 127
#endif

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
// pcs[] is no longer a flat, uniformly-native PC array: an interpreter frame
// occupies two consecutive slots (see frame_push_interp), so tags[] carries
// one FRAME_TAG_* byte per pcs[] slot telling consumers how to read it.
// Issue #83. tags[] trails pcs[] (rather than interleaving) so the existing
// fixed pcs[] offset is unchanged for readers that only care about native
// frames.
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

// Frame tags. Each pcs[] slot carries its kind.
//
// FRAME_TAG_NATIVE is a single slot holding one PC. Any other value is an
// UNWINDER ID — the same opaque id handoff_ranges carries and interp_progs
// is indexed by — and marks the first of a PAIR of slots written by that
// unwinder (see frame_push_interp). The core assigns no meaning to a
// non-zero tag beyond "two slots, not one, and not an instruction pointer";
// userspace routes the pair to the module that owns that id.
//
// Tag 1 has been the CPython walker's since issue #83 and stays there, which
// is what keeps the wire format unchanged.
#define FRAME_TAG_NATIVE 0

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
//
// Shared with every interpreter module: the module appends its frames to the
// SAME record the native walk is building, at the position the walk stopped
// at, which is what makes the interleave a kernel guarantee rather than a
// userspace reconstruction.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct sample_record);
    __uint(max_entries, 1);
} walker_scratch SEC(".maps");

// ----- The tail-call table.
//
// Slots 0 and 1 are the DRIVER's two resume programs (INTERP_DEFINE_PROGRAMS
// defines both); slot INTERP_SLOT_UNWINDER(id) is the unwinder with that id.
// Userspace populates the table -- unwind/interp -- and an empty slot simply
// means the tail call fails and the walk finishes natively, which is the
// correct degradation.
//
// A prog array's entries must all share the driver's program type, so each
// driver gets its own instance of this map and each interpreter module
// supplies one program per program type it can be dispatched from.
#define INTERP_SLOT_RESUME_STEP 0
#define INTERP_SLOT_RESUME_WALK 1
#define INTERP_SLOT_UNWINDER(id) ((id) + 1)
#define INTERP_MAX_UNWINDERS 8

struct {
    __uint(type, BPF_MAP_TYPE_PROG_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, sizeof(__u32));
    __uint(max_entries, INTERP_MAX_UNWINDERS);
} interp_progs SEC(".maps");

// ----- Why a handoff did or did not happen.
//
// A per-CPU counter array, generic, owned by the core: it counts the CORE's
// decisions about dispatching, not anything a module does. A module's own
// refusals are its own counters' business.
//
// IT EXISTS BECAUSE A FAILED HANDOFF WAS INVISIBLE. Every counter on the
// module side reading zero has two completely different causes -- "the
// unwinder ran and refused" and "the unwinder was never reached" -- and from
// outside the kernel they looked identical. An hour was spent unable to tell
// them apart on a target where the native walk was landing on the eval loop in
// 86% of samples. These six slots make the difference readable, and read
// together they name WHICH step failed:
//
//   RANGE_HIT 0                 handoff_ranges has no entry under the
//                               table_id the walker computes: either nothing
//                               was installed, or the install and the lookup
//                               disagree about the key.
//   RANGE_HIT > 0, IN_RANGE 0   the entry is found and the PC never falls
//                               inside it: the install and the lookup
//                               disagree about the ADDRESS SPACE (a range in
//                               file offsets against a load-bias-relative pc,
//                               say).
//   IN_RANGE > 0, CLAIMED 0     the unwinder had already declared itself
//                               finished for that sample. Not a fault.
//   CLAIMED > 0, DISPATCHED 0   walk_step claimed and the driver did not act,
//                               which can only be the budget: see BUDGET.
//   DISPATCHED > 0, FAILED > 0  the tail call did not happen. The slot is
//                               empty, or the kernel's own tail-call limit
//                               was reached. This is the one that used to be
//                               completely silent.
//   RESUMED < DISPATCHED        the module was entered and did not hand
//                               control back.
//
// All six are cheap enough to leave in unconditionally: measured at 25% of the
// verifier budget before they were added and 25% after (see the commit).
#define INTERP_STAT_RANGE_HIT   0  // a claim exists for this frame's binary
#define INTERP_STAT_IN_RANGE    1  // and this frame's PC falls inside it
#define INTERP_STAT_CLAIMED     2  // and the unwinder still wants frames
#define INTERP_STAT_DISPATCHED  3  // the driver tail-called the unwinder
#define INTERP_STAT_TAILCALL_FAILED 4  // and the tail call did not happen
#define INTERP_STAT_BUDGET      5  // a claim was dropped: round-trip budget
#define INTERP_STAT_RESUMED     6  // an unwinder handed control back
#define INTERP_STAT_MAX         7

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, INTERP_STAT_MAX);
    __type(key, __u32);
    __type(value, __u64);
} interp_stats SEC(".maps");

// interp_count bumps one named slot. Per-CPU, so the increment needs no
// atomic; userspace sums across CPUs.
static __always_inline void interp_count(__u32 slot) {
    if (slot >= INTERP_STAT_MAX) return;
    __u64 *d = bpf_map_lookup_elem(&interp_stats, &slot);
    if (d) *d += 1;
}

// ----- The walk state that survives a tail call.
//
// It is deliberately NOT struct walk_ctx. A tail call replaces the running
// program, so the driver's stack is gone and the cursor has to live in a map
// -- and a map value may not contain pointers. bpf2go refuses outright:
//
//   generating perf_dwarfWalkState: Struct:"walk_state": field 0:
//   Struct:"walk_ctx": field 5: type *btf.Pointer: not supported
//
// which is the tool telling us something the verifier would have told us
// later: a pointer stored in a map value comes back as a plain scalar, so
// `rec` could not be trusted across the boundary anyway. It is re-derived
// from walker_scratch on every entry. So walk_ctx keeps its pointers and
// stays on the stack -- which also keeps bpf_loop's callback context a stack
// pointer, the shape these programs already verify -- and only the scalars
// persist.
struct walk_persist {
    __u64 pc;
    __u64 fp;
    __u64 sp;
    // interp_scratch is an OPAQUE SAVE AREA. The core never reads it, never
    // writes it except to zero it at the start of a sample, and has no
    // opinion about what is in it: it belongs to whichever unwinder the walk
    // handed off to, and is how that unwinder resumes its own chain when the
    // native walk lands on a second frame it claims.
    //
    // It lives here rather than in a map of the module's own because the
    // lifetime is exactly this struct's -- one sample -- and a second per-CPU
    // map would need a sample counter to know when to reset, which is the
    // core learning something about the module's lifecycle in order to avoid
    // learning about its contents. Zeroing four words in the driver is free
    // (a store in this path costs nothing measurable; see rd-report.md) and
    // says the same thing with less machinery.
    //
    // Two interpreters live in ONE sample would share it. No supported pair
    // can be: a PC belongs to one handoff range. The day that stops being
    // true this becomes an array indexed by unwinder id, and the modules do
    // not change.
    __u64 interp_scratch[4];
    __u32 pid;
    __u32 tid;
    __u32 n_pcs;
    // Set by walk_step when another unwinder claims the frame it stopped on;
    // read by the driver after bpf_loop returns. UNWINDER_NATIVE means the
    // walk ended for one of the ordinary reasons.
    __u32 pending_unwinder;
    // One bit per unwinder id: "this unwinder is finished with this sample,
    // stop handing it frames". Set by the module, read by next_unwinder.
    //
    // Without it a deep stack that crosses the same interpreter's text many
    // times pays a full round trip per crossing to be told nothing, and burns
    // the tail-call budget doing it. This is OTel's `unwindersDone`
    // (tracemgmt.h:432) and it is here for the same reason.
    __u32 interp_done;
    // Set when the one resume step a resumed walk begins with could not unwind
    // past the frame it was given: the cursor never moved, so re-entering the
    // loop would push the same frame twice. Read in unwind_walk, under a
    // compile-time-constant guard so only the resume side carries the test --
    // reading it inside the loop callback instead costs 187k more processed
    // instructions, measured.
    __u8  stopped;
    __u8  _pad2[3];
    // tail_calls bounds the native<->interpreter round trips in one sample.
    // One round trip is THREE tail calls, so the budget is well under the
    // kernel's MAX_TAIL_CALL_CNT of 33 -- see INTERP_TAIL_CALL_BUDGET.
    __u32 tail_calls;
    __u32 _pad;
};

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct walk_persist);
} walk_states SEC(".maps");

static __always_inline struct walk_persist *walk_state_get(void) {
    __u32 zero = 0;
    return bpf_map_lookup_elem(&walk_states, &zero);
}

static __always_inline struct sample_record *walk_record_get(void) {
    __u32 zero = 0;
    return bpf_map_lookup_elem(&walker_scratch, &zero);
}

// ----- Writing a frame.
//
// A TRAP THAT HAS NOW BITTEN TWICE, WRITTEN DOWN WHERE IT BITES.
//
// A bound the verifier proved about a value is attached to the REGISTER it
// proved it about, not to the memory the value came from. So this is rejected:
//
//     if (pi->frame_owner > LIMIT) return;      // proves a bound on a load
//     x = buf[pi->frame_owner];                 // a SECOND load: no bound
//
//   invalid access to map value, value_size=96 off=65535 size=1
//   R2 max value is outside of the allowed memory range
//
// 65535 is the full __u16 range -- the bound from three lines earlier, gone.
// It is not the verifier being obtuse: pi points into a map value, and another
// CPU may write it between the two loads, so the second load genuinely has no
// bound. Bind the value ONCE and use that binding for both the check and the
// access:
//
//     __u16 off = pi->frame_owner;
//     if (off > LIMIT) return;
//     x = buf[off];
//
// The same class of defect, in the other direction, produced the earlier
// wrap bug: `if (i + 2 > MAX_FRAMES)` did arithmetic ON the checked value, so
// the check and the store disagreed about what had been proved. Both rules are
// one rule -- check and use the same register, and do nothing to it in
// between -- and both were found by CI rather than by review.
//
// Both pushers take the cursor by POINTER rather than reading it out of a
// context struct, so the one place a record can overflow stays one place for
// the core walker and for every interpreter module alike.
//
// A refusal is never silent: WALKER_FLAG_FRAME_PUSH_REFUSED (bit 7, declared
// with the rest of the flags in unwind_common.h) says a frame was dropped for
// lack of room, distinct from every other reason a walk can stop short.
#define WALKER_FLAG_FRAME_PUSH_REFUSED 0x80

_Static_assert(MAX_FRAMES >= 2,
               "frame_push_interp's bound is MAX_FRAMES - 2; below 2 that underflows to a huge unsigned and accepts everything");
_Static_assert(MAX_FRAMES <= 255,
               "walk_persist.n_pcs is __u32 but narrows to sample_header.n_pcs (__u8) in every driver; MAX_FRAMES must fit");

// record_push_native appends one native-PC slot, tagging it
// FRAME_TAG_NATIVE. Returns 0 on success, 1 if the record is already full
// (MAX_FRAMES reached) — the caller must stop walking in that case.
//
// The bound is re-asserted into a local (`i`) immediately before the
// stores, rather than checked on *n and indexed with *n again: the verifier
// tracks a register's proven range across a branch, not a memory location
// re-read through a pointer, so `if (*n >= MAX_FRAMES) ... rec->pcs[(*n)++]`
// verified the check but not the store, rejecting perf_dwarf and offcpu_dwarf
// as an unbounded R4 access at the pcs[] write. Binding `i` once and using it
// for both the check and every array access keeps the proof in one register.
static __always_inline int record_push_native(struct sample_record *rec, __u32 *n, __u64 pc) {
    __u32 i = *n;
    if (i >= MAX_FRAMES) {
        rec->hdr.walker_flags |= WALKER_FLAG_FRAME_PUSH_REFUSED;
        return 1;
    }
    rec->tags[i] = FRAME_TAG_NATIVE;
    rec->pcs[i] = pc;
    *n = i + 1;
    return 0;
}

// record_push_interp appends a two-slot interpreter frame — whatever pair of
// words the owning unwinder wants to carry, for CPython the code object's
// address and an encoded fingerprint/f_lasti — both tagged with that
// unwinder's id. Returns 0 on success, 1 if fewer than two slots remain: the
// caller must stop walking rather than push a half-pair.
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
// record_push_native was accepted through all of this because
// `i >= MAX_FRAMES` does no arithmetic and cannot wrap; that asymmetry was
// the tell.
//
// The bound is therefore expressed as a comparison against a COMPILE-TIME
// CONSTANT: this push writes slots `i` and `i + 1`, both of which must be
// < MAX_FRAMES, so the accept set is exactly i <= MAX_FRAMES - 2 (125 at
// MAX_FRAMES 127) and the refusal is `i > MAX_FRAMES - 2`. `MAX_FRAMES - 2`
// folds at compile time, so nothing the verifier has to track is ever added
// to. The _Static_assert above keeps that subtraction from underflowing into
// a huge unsigned bound if MAX_FRAMES is ever made tiny.
//
// Both slots are written from that same checked local `i`, so the two-slot
// push stays atomic (refuse before writing either, never write one and fail
// the second) and the verifier's proof for both `i` and `i + 1` stays tied to
// the one checked register instead of a re-read through the pointer.
static __always_inline int record_push_interp(struct sample_record *rec, __u32 *n,
                                              __u8 tag, __u64 a, __u64 b) {
    __u32 i = *n;
    if (i > MAX_FRAMES - 2) {
        rec->hdr.walker_flags |= WALKER_FLAG_FRAME_PUSH_REFUSED;
        return 1;
    }
    rec->tags[i] = tag;
    rec->pcs[i] = a;
    rec->tags[i + 1] = tag;
    rec->pcs[i + 1] = b;
    *n = i + 2;
    return 0;
}

// ----- Handing control back to the native walk.
//
// An interpreter module calls this when its segment is done. Slot 0 is the
// driver's resume-step program, which takes the frame the walk stopped on --
// already in the record, and now with this module's frames after it -- past
// its caller, and then hands on to the driver's ordinary walk.
//
// If the tail call fails -- nothing installed in slot 0, or the budget is
// exhausted -- control simply returns and the module's program exits. The
// record is already correct as far as the walk got, and the driver that
// dispatched has no way to be re-entered, so the sample is one that stopped
// early rather than one that is wrong. That is the intended degradation.
static __always_inline void interp_return_to_native(void *ctx, struct walk_persist *st) {
    st->pending_unwinder = 0;  // UNWINDER_NATIVE
    bpf_tail_call(ctx, &interp_progs, INTERP_SLOT_RESUME_STEP);
}

#endif // PERF_AGENT_UNWIND_RECORD_H
