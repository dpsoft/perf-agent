//go:build ignore
//
// gpu_usdt.bpf.c — consumer for the perfagent GPU USDT ABI.
//
// One program serves every probe site; bpf_get_attach_cookie() says which
// record kind fired. The shim pins the probe arguments to the first three
// integer-argument registers (ptr, count, seq), so PT_REGS_PARM1..3 read
// them portably on both supported arches.
//
// The program is attached with a single uprobe_multi BPF link (see
// gpuprobe.Attach): the perf_uprobe PMU path needs CAP_SYS_ADMIN, the BPF
// link path does not. The section name must therefore be "uprobe.multi" and
// not "uprobe" — the kernel rejects LINK_CREATE with BPF_TRACE_UPROBE_MULTI
// unless the program was loaded with that expected_attach_type, and
// cilium/ebpf derives expected_attach_type from the section name.

#if defined(__TARGET_ARCH_arm64)
#include "vmlinux_arm64.h"
#else
#include "vmlinux.h"
#endif
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

// The hybrid DWARF/FP walker, shared with perf_dwarf.bpf.c and
// offcpu_dwarf.bpf.c: walk_step, the CFI and mapping tables it consults, and
// the per-CPU walker_scratch it writes into. UNWIND_NO_SAMPLE_RINGBUF drops
// the emit-side maps (stack_events, kern_stackmap) this driver never touches
// — it stages its PCs into gpu_stacks below instead, and creating a 16384-
// entry kernel stackmap for nothing would cost ~16 MB per attach.
#define UNWIND_NO_SAMPLE_RINGBUF 1
#include "unwind_common.h"

char LICENSE[] SEC("license") = "GPL";

// vmlinux.h is generated from BTF and carries no errno definitions, so the
// one value this program has to tell apart is spelled out here. E2BIG is
// what a full BPF_MAP_TYPE_HASH returns from bpf_map_update_elem, and it is
// ABI-stable across every Linux architecture.
#define GPU_E2BIG 7

// Slot 0 is not a record kind: it collects drops for kinds this program does
// not know, so the "no loss is silent" contract has no hole even on a cookie
// we never installed.
#define KIND_UNKNOWN        0
#define KIND_LAUNCH         1
#define KIND_EXEC           2
#define KIND_MODULE         3
#define KIND_PC             4
#define KIND_LAUNCH_SAMPLED 5
#define KIND_KERNEL_NAME    6
#define KIND_STALL_MAP      7
#define KIND_SAMPLING_WINDOW 8
#define KIND_CONFIG         9
// Producer-side loss, by class. gpu_dropped_v1 has been in the ABI header
// since Phase 3 with no kind, no cookie and no probe, so it has never reached
// the wire -- which meant every drop class the shim could define was a
// counter that could not go non-zero. Tier B defines the first four classes
// and this is the kind that carries them.
#define KIND_DROPPED        10
// KIND_MAX sizes the `dropped` and `stacks_missing` arrays below and bounds
// count_drop / count_stack_missing. It is deliberately larger than the
// highest kind in use: an array resize is an unavoidable map-layout change,
// and doing it once with headroom is better than doing it on every probe
// added. Go's kindMax (gpuprobe/consumer.go) mirrors this and MUST move in
// the same commit — a mismatch mis-sizes the drop accounting silently, so a
// drop storm reads as zero drops. TestKindMaxPinsTheBPFDropAccountingArrays
// pins the pair against this compiled object.
#define KIND_MAX            16

// The wire record sizes, frozen in shim/core/usdt_abi.h and mirrored by
// internal/gpuabi. They live here as named constants because the payload
// budget below is derived from them at compile time.
#define REC_LAUNCH         48
#define REC_EXEC           48
#define REC_MODULE         40
#define REC_PC             40
#define REC_LAUNCH_SAMPLED 56
#define REC_KERNEL_NAME    272
#define REC_STALL_MAP      136
#define REC_SAMPLING_WINDOW 24
#define REC_CONFIG         24
#define REC_DROPPED        16

#define MAX_RECORDS_PER_BATCH 64

// MAX_RECORD_BYTES is the largest record on the wire: gpu_kernel_name_v1 at
// 272 bytes. gpu_stall_reason_map_v1 (136) is the second largest and does not
// move it. It deliberately does NOT size the reservation. Sizing the
// reservation for the worst case of every kind would cost
// 40 + 64*272 = 17448 bytes per batch, i.e. ~240 batches in a 4MB ring —
// too few to absorb a burst, and paid on every launch batch to serve two
// probes that never batch at all.
#define MAX_RECORD_BYTES 272

// MAX_BATCHED_RECORD_BYTES is the largest record that ever arrives with
// count > 1: gpu_launch_v1 / gpu_exec_v1, both 48 bytes. gpu_launch_sampled_v1
// (56) rides alone because its stack must belong to exactly one launch, and
// gpu_kernel_name_v1 (272) is emitted one record per interned name.
//
// gpu_stall_reason_map_v1 is 136 and deliberately does NOT raise this. It
// fires a few dozen times per process — once per stall reason the device
// reports — so sizing it in would enlarge EVERY launch batch's reservation
// from 3072 to 8704 bytes and cut the 4MB ring from ~1300 batches to ~480,
// paid on the hot path to serve a one-shot table. BATCH_CAP gives it its own
// cap of 22 records instead, and the excess is counted as a drop rather than
// truncated silently.
#define MAX_BATCHED_RECORD_BYTES 48

// The ringbuf reservation is fixed-size: bpf_ringbuf_reserve takes a
// constant, so every batch costs the worst case (64 records * 48 bytes)
// regardless of how many records it actually carries. Userspace reads the
// real length from batch_hdr.bytes.
//
// Kinds whose record is larger than MAX_BATCHED_RECORD_BYTES are not
// unbounded here: BATCH_CAP gives each kind its own record cap, so the bytes
// copied never exceed PAYLOAD_BYTES whatever count the producer passes, and
// the records that do not fit are counted as drops rather than truncated
// silently.
#define PAYLOAD_BYTES (MAX_RECORDS_PER_BATCH * MAX_BATCHED_RECORD_BYTES)

// BATCH_CAP(sz) is how many sz-byte records one reservation holds, never
// more than MAX_RECORDS_PER_BATCH. Entirely compile-time: no BPF_DIV reaches
// the verifier.
#define BATCH_CAP(sz)                                                    \
    ((PAYLOAD_BYTES / (sz)) < MAX_RECORDS_PER_BATCH                      \
         ? (PAYLOAD_BYTES / (sz))                                        \
         : MAX_RECORDS_PER_BATCH)

// Every kind must fit at least one record, or that kind could never be
// delivered at all. 272 <= 3072, with room to spare.
_Static_assert(MAX_RECORD_BYTES <= PAYLOAD_BYTES,
               "a record kind larger than one reservation could never be delivered");

// batch_hdr is 40 bytes. stack_id and _pad were APPENDED in Phase 4a: the
// Go decoder (gpuprobe/consumer.go) reads every field by hard-coded offset,
// so reordering would decode garbage without erroring anywhere.
//
//   0 kind  4 count  8 seq  16 pid  20 tid  24 bytes  32 stack_id  36 _pad
//
// stack_id is this program's own handle into gpu_stacks for the launching
// thread's user stack, or negative when the capture failed. Phase 4b changed
// what it MEANS — a key we mint rather than one bpf_get_stackid returns —
// without changing its size or position. It is per-batch, not per-record,
// which is sound only because the one kind that carries it
// (KIND_LAUNCH_SAMPLED) always arrives with count == 1 — enforced by
// max_records, not merely assumed of the producer.
struct batch_hdr {
    __u32 kind;
    __u32 count;
    __u64 seq;
    __u32 pid;
    __u32 tid;
    __u64 bytes;
    __s32 stack_id;
    __u32 _pad;
};

_Static_assert(sizeof(struct batch_hdr) == 40, "batch_hdr must stay 40 bytes; see batchHdrSize in gpuprobe/consumer.go");

struct batch_msg {
    struct batch_hdr hdr;
    __u8 payload[PAYLOAD_BYTES];
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 22);   // 4MB
} events SEC(".maps");

// dropped[kind] counts *records* this program could not deliver: a batch
// larger than one reservation can hold, a ringbuf that had no room, a
// user-memory read that faulted, or (in slot KIND_UNKNOWN) a probe fire whose
// attach cookie names no kind this program can size. Spec §6.1 admits no
// silent loss, so userspace reads this map in Consumer.Stats().
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, KIND_MAX);
    __type(key, __u32);
    __type(value, __u64);
} dropped SEC(".maps");

// stacks_missing[kind] counts probe fires that produced no usable stack —
// the walk returned no frames, gpu_stacks was full, or the insert failed.
// walk_errors below says WHICH; this stays a per-kind count so the userspace
// cross-check against the stack_id values it actually saw still works.
// It is deliberately NOT the `dropped` map:
// the batch is still delivered, with batch_hdr.stack_id negative, so no
// record is lost and folding this into Stats.KernelDropped would report
// phantom record loss. Userspace surfaces it as Stats.KernelStacksMissing
// and cross-checks it against the stack_id values it actually saw.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, KIND_MAX);
    __type(key, __u32);
    __type(value, __u64);
} stacks_missing SEC(".maps");

// ----- The walker's output map.
//
// bpf_get_stackid used to fill a kernel BPF_MAP_TYPE_STACK_TRACE here. It
// walked frame pointers and nothing else, so on a real CUDA process it
// stopped at the first frame it could not follow — which is the profiler's
// own CUPTI callback, one frame above the probe. The chain that matters
// (callback -> libcupti -> libcudart -> the application) was never reached,
// and the GPU time it explains was attributed to the profiler.
//
// A hand-rolled walk cannot populate a STACK_TRACE map, so the walk stages
// its PCs in walker_scratch and this program copies them here under an id it
// mints itself. gpuprobe.Consumer.resolveStackLocked reads the entry and
// deletes it; nothing else reclaims a slot.
//
// One value is 4 + 4 + 127*8 + 127 (+1 pad) = 1152 bytes, so GPU_STACKS_SIZE
// entries preallocate 4.5 MB — the same order as the `events` ringbuf above,
// and deliberately so: occupancy is proportional to how many captures are in
// flight between the probe and the consumer's drain, not to the length of
// the run. 4096 is roughly two full ringbufs' worth of sampled launches, so
// the map cannot fill before the ringbuf does unless the consumer has
// stopped draining entirely — in which case the loss is already counted.
#define GPU_STACKS_SIZE 4096

struct gpu_stack {
    __u32 n_pcs;
    // walker_flags is the walk's own WALKER_FLAG_* bitmask, copied out of
    // the scratch record. Nothing reads it yet; it is here because the walk
    // that produced these PCs is the only place that knows whether DWARF
    // fired, whether the FP chain terminated naturally, or whether a CFI
    // lookup cut the walk short — and re-deriving that later is impossible.
    __u32 walker_flags;
    __u64 pcs[MAX_FRAMES];
    // One FRAME_TAG_* byte per pcs[] slot, mirroring struct sample_record's
    // tags[] (bpf/unwind_common.h) and laid out the same way: trailing
    // pcs[], not interleaved, so pcs[]'s byte offset is unchanged.
    //
    // Issue #83: without this, a Python frame — two consecutive slots
    // holding a code-object address and an encoded instruction word —
    // arrives here indistinguishable from two native PCs, and the consumer
    // symbolizes both against the process's mappings. That produces two
    // plausible, wrong native frames on the ONE path this whole feature
    // exists to serve, and nothing downstream could tell. The 128 bytes are
    // the price of that not happening.
    __u8 tags[MAX_FRAMES];
};

_Static_assert(sizeof(struct gpu_stack) == 1152,
               "gpu_stack must stay 1152 bytes; see gpuStackSize in gpuprobe/consumer.go");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, GPU_STACKS_SIZE);
    __type(key, __u32);
    __type(value, struct gpu_stack);
} gpu_stacks SEC(".maps");

// Staging buffer for one gpu_stack. The value is 1024 bytes and the BPF
// stack is 512, so the copy from walker_scratch has to land in a map.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct gpu_stack);
} gpu_stack_scratch SEC(".maps");

// ----- Stack id generation.
//
// An id must be unique across every CPU that can fire this probe at once,
// and must stay non-negative: batch_hdr.stack_id is __s32 and negative means
// "no stack". The construction is a per-CPU counter in the low bits and the
// CPU index in the high bits, which cannot collide between CPUs by
// construction — no atomics, no shared cursor, no CAS on a hot path.
//
//   bit 30..18  CPU index   (8192 CPUs; masked, see below)
//   bit 17..0   per-CPU seq (262144 ids before it wraps)
//   bit 31      always 0
//
// On wrap the per-CPU sequence returns to 0 and ids repeat. That is safe
// because an id is only in use between the probe and the consumer's read,
// and gpu_stacks holds at most 4096 entries: 262144 captures on one CPU must
// have come and gone before a value repeats. If one somehow has NOT been
// consumed, the insert below uses BPF_NOEXIST, so the live entry is kept and
// the NEW capture is the one that fails — counted in walk_errors, never a
// silent overwrite that would hand one launch another launch's call path.
//
// CPU indices at or above 8192 alias onto low ones. Two such CPUs would also
// have to hold the same per-CPU sequence value at the same moment to collide,
// and Linux's CONFIG_NR_CPUS tops out at 8192 on x86_64, so this is a bound
// worth stating rather than a race worth fearing.
#define GPU_STACK_SEQ_BITS 18
#define GPU_STACK_SEQ_MASK ((1U << GPU_STACK_SEQ_BITS) - 1)
#define GPU_STACK_CPU_MASK 0x1FFFU

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u32);
} stack_id_seq SEC(".maps");

// ----- Why a capture produced no stack.
//
// stacks_missing says a sampled launch lost its stack; this says why. Every
// early return in capture_stack lands in exactly one slot, so no failure in
// the walk path is silent. Read as Stats.StackWalk* / Stats.StackMap* .
#define WALK_ERR_NO_SCRATCH  0  // a per-CPU scratch lookup failed
#define WALK_ERR_EMPTY       1  // the walk produced no frames at all
#define WALK_ERR_MAP_FULL    2  // gpu_stacks is full (-E2BIG)
#define WALK_ERR_UPDATE      3  // the insert failed some other way
#define WALK_ERR_MAX         4

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, WALK_ERR_MAX);
    __type(key, __u32);
    __type(value, __u64);
} walk_errors SEC(".maps");

// record_size is an if-chain rather than a switch so clang cannot lower it
// to a .rodata table load, which would hand the verifier an unbounded
// scalar where it needs a constant.
static __always_inline __u32 record_size(__u32 kind)
{
    if (kind == KIND_LAUNCH)
        return REC_LAUNCH;
    if (kind == KIND_EXEC)
        return REC_EXEC;
    if (kind == KIND_MODULE)
        return REC_MODULE;
    if (kind == KIND_PC)
        return REC_PC;
    if (kind == KIND_LAUNCH_SAMPLED)
        return REC_LAUNCH_SAMPLED;
    if (kind == KIND_KERNEL_NAME)
        return REC_KERNEL_NAME;
    if (kind == KIND_STALL_MAP)
        return REC_STALL_MAP;
    if (kind == KIND_SAMPLING_WINDOW)
        return REC_SAMPLING_WINDOW;
    if (kind == KIND_CONFIG)
        return REC_CONFIG;
    if (kind == KIND_DROPPED)
        return REC_DROPPED;
    return 0;
}

// max_records is record_size's companion: how many records of this kind one
// reservation can carry. Same if-chain shape and same reason — every arm is
// a compile-time constant, so the verifier sees a bounded scalar and clang
// emits no division.
//
// The byte-budget caps are 48B -> 64, 40B -> 64 (capped), 56B -> 54,
// 272B -> 11, 136B -> 22, 24B -> 64 (capped). They exist so the clamp is
// sound for any count a producer could pass, not just the counts this shim
// passes.
//
// KIND_LAUNCH_SAMPLED is capped at 1 for a stronger reason than bytes. Its
// stack id lives in the batch header, one per batch, so a batch of N sampled
// launches would attribute one captured stack to N unrelated launches — the
// exact misattribution this feature exists to avoid, and silent, because
// every record would still decode. The cap makes a batching producer lose
// records loudly in the `dropped` map instead. gpuprobe's decodeBatch
// rejects count != 1 on this kind for the same reason: neither end trusts
// the other to hold the invariant.
static __always_inline __u32 max_records(__u32 kind)
{
    if (kind == KIND_LAUNCH)
        return BATCH_CAP(REC_LAUNCH);
    if (kind == KIND_EXEC)
        return BATCH_CAP(REC_EXEC);
    if (kind == KIND_MODULE)
        return BATCH_CAP(REC_MODULE);
    if (kind == KIND_PC)
        return BATCH_CAP(REC_PC);
    if (kind == KIND_LAUNCH_SAMPLED)
        return 1;   // one stack per batch => one record per batch
    if (kind == KIND_KERNEL_NAME)
        return BATCH_CAP(REC_KERNEL_NAME);
    if (kind == KIND_STALL_MAP)
        return BATCH_CAP(REC_STALL_MAP);
    if (kind == KIND_SAMPLING_WINDOW)
        return BATCH_CAP(REC_SAMPLING_WINDOW);
    if (kind == KIND_CONFIG)
        return BATCH_CAP(REC_CONFIG);
    if (kind == KIND_DROPPED)
        return BATCH_CAP(REC_DROPPED);
    return 0;
}

_Static_assert(1 <= BATCH_CAP(REC_LAUNCH_SAMPLED),
               "the sampled-launch cap of 1 must still fit the payload budget");

// Every kind in use must be addressable in the drop arrays. A kind at or past
// KIND_MAX is discarded by count_drop without being counted anywhere, which
// is loss with no counter at all.
_Static_assert(KIND_DROPPED < KIND_MAX,
               "the highest kind must fit the dropped/stacks_missing arrays");

// The two 24-byte records saturate MAX_RECORDS_PER_BATCH rather than the byte
// budget, and the 136-byte one must still fit at least one record.
_Static_assert(BATCH_CAP(REC_STALL_MAP) * REC_STALL_MAP <= PAYLOAD_BYTES,
               "the stall-map cap must not overrun the payload");
_Static_assert(BATCH_CAP(REC_STALL_MAP) >= 1,
               "a stall-map record must fit one reservation");
_Static_assert(BATCH_CAP(REC_SAMPLING_WINDOW) == MAX_RECORDS_PER_BATCH,
               "a 24-byte record is capped by the record count, not by bytes");

static __always_inline void count_drop(__u32 kind, __u64 records)
{
    __u64 *d;

    if (kind >= KIND_MAX)
        return;
    d = bpf_map_lookup_elem(&dropped, &kind);
    if (d)
        __sync_fetch_and_add(d, records);
}

// count_stack_missing records a capture that failed. The batch it belongs to
// is still submitted; see the stacks_missing map comment.
static __always_inline void count_stack_missing(__u32 kind)
{
    __u64 *d;

    if (kind >= KIND_MAX)
        return;
    d = bpf_map_lookup_elem(&stacks_missing, &kind);
    if (d)
        __sync_fetch_and_add(d, 1);
}

// count_walk_error records WHY a capture failed. Companion to
// count_stack_missing, which records THAT it failed.
static __always_inline void count_walk_error(__u32 slot)
{
    __u64 *d;

    if (slot >= WALK_ERR_MAX)
        return;
    d = bpf_map_lookup_elem(&walk_errors, &slot);
    if (d)
        __sync_fetch_and_add(d, 1);
}

// next_stack_id mints a handle for one capture. See the GPU_STACK_SEQ_BITS
// comment for the uniqueness argument and the wrap behaviour. Returns a
// negative value only if the per-CPU counter cannot be reached, which the
// caller counts as a scratch failure.
static __always_inline __s32 next_stack_id(void)
{
    __u32 zero = 0;
    __u32 *seq = bpf_map_lookup_elem(&stack_id_seq, &zero);
    if (!seq)
        return -1;
    // A per-CPU array value: this CPU is the only writer, and a BPF program
    // cannot migrate mid-run, so a plain increment is correct and an atomic
    // would only cost a locked instruction on the probe's hot path.
    __u32 n = ++(*seq);
    __u32 cpu = bpf_get_smp_processor_id() & GPU_STACK_CPU_MASK;
    return (__s32)((cpu << GPU_STACK_SEQ_BITS) | (n & GPU_STACK_SEQ_MASK));
}

// capture_stack walks the launching thread's user stack with the hybrid
// DWARF/FP walker and parks the result in gpu_stacks. Returns the handle, or
// -1 if there is nothing to hand the consumer. Every -1 is counted twice: in
// walk_errors (why) and, by the caller, in stacks_missing (that it happened).
//
// The walk is driven exactly as perf_dwarf.bpf.c and offcpu_dwarf.bpf.c
// drive it — walker_flags zeroed BEFORE bpf_loop, because walk_step ORs bits
// into it as it classifies frames.
static __always_inline __s32 capture_stack(struct pt_regs *ctx, __u32 tgid)
{
    __u32 zero = 0;
    __u32 n;
    __s32 id;
    long ret;

    struct sample_record *rec = bpf_map_lookup_elem(&walker_scratch, &zero);
    if (!rec) {
        count_walk_error(WALK_ERR_NO_SCRATCH);
        return -1;
    }
    struct gpu_stack *out = bpf_map_lookup_elem(&gpu_stack_scratch, &zero);
    if (!out) {
        count_walk_error(WALK_ERR_NO_SCRATCH);
        return -1;
    }

    // At a USDT probe the register file is the application's own: ip is the
    // probe site, fp/sp the launching thread's frame. PT_REGS_* expand to
    // ip/bp/sp on x86_64 and pc/regs[29]/sp on arm64.
    struct walk_ctx walker = {
        .pc    = (__u64)PT_REGS_IP(ctx),
        .fp    = (__u64)PT_REGS_FP(ctx),
        .sp    = (__u64)PT_REGS_SP(ctx),
        .pid   = tgid,
        .n_pcs = 0,
        .rec   = rec,
    };
    rec->hdr.walker_flags = 0;
    bpf_loop(MAX_FRAMES, walk_step, &walker, 0);

    n = walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs;
    if (n == 0) {
        // Not even the probe's own PC came back. Nothing to attribute, and
        // an entry holding zero frames would only be refused on read.
        count_walk_error(WALK_ERR_EMPTY);
        return -1;
    }

    id = next_stack_id();
    if (id < 0) {
        count_walk_error(WALK_ERR_NO_SCRATCH);
        return -1;
    }

    out->n_pcs = n;
    out->walker_flags = rec->hdr.walker_flags;
    // A constant-size copy of the whole array, not n_pcs elements: the size
    // has to be a compile-time constant for the verifier, and the slots past
    // n_pcs are never read — the consumer honours n_pcs rather than scanning
    // for a zero terminator, precisely because this scratch is per-CPU and
    // its tail still holds the previous capture's PCs.
    //
    //
    // tags[] rides along for the same reason and under the same rule: it is
    // one byte per pcs[] slot and the consumer reads only the first n_pcs of
    // them. Without it a Python frame's two slots would reach userspace
    // looking exactly like two native PCs (issue #83).
    __builtin_memcpy(out->pcs, rec->pcs, sizeof(out->pcs));
    __builtin_memcpy(out->tags, rec->tags, sizeof(out->tags));

    // BPF_NOEXIST, never BPF_ANY. If this id is somehow still live — a
    // per-CPU sequence that wrapped past an entry the consumer never read —
    // the live entry wins and this capture is the one that fails. An
    // overwrite would hand one launch another launch's call path, and
    // nothing downstream could tell.
    ret = bpf_map_update_elem(&gpu_stacks, &id, out, BPF_NOEXIST);
    if (ret != 0) {
        count_walk_error(ret == -GPU_E2BIG ? WALK_ERR_MAP_FULL : WALK_ERR_UPDATE);
        return -1;
    }
    return id;
}

SEC("uprobe.multi")
int gpu_usdt_batch(struct pt_regs *ctx)
{
    // The ABI pins its arguments: ptr, count, seq.
    __u64 ptr   = (__u64)PT_REGS_PARM1(ctx);
    __u64 count = (__u64)PT_REGS_PARM2(ctx);
    __u64 seq   = (__u64)PT_REGS_PARM3(ctx);

    __u32 kind = (__u32)bpf_get_attach_cookie(ctx);
    __u32 rsz = record_size(kind);
    __u32 cap = max_records(kind);
    __s32 stack_id = -1;
    __u32 bytes;
    __u64 id;
    struct batch_msg *msg;

    if (rsz == 0) {
        // An attach cookie this program cannot size. Unreachable while Go
        // only installs cookies 1-9, but this is the one return that would
        // otherwise discard a batch without counting it. The producer's own
        // record count is the best size estimate available here.
        count_drop(KIND_UNKNOWN, count);
        return 0;
    }
    if (count == 0)
        return 0;   // nothing to lose
    if (count > cap) {
        // Truncation is loss. Count the records that will never be copied.
        // The cap is per-kind (max_records), not a flat 64: a flat 64 with a
        // 272-byte record would ask for 17408 bytes out of a 3072-byte
        // payload, and the clamp below would then silently copy 3072 bytes
        // while the header still claimed 64 records.
        count_drop(kind, count - cap);
        count = cap;
    }

    bytes = (__u32)count * rsz;
    // barrier_var stops clang from proving the clamp below redundant and
    // deleting it. Without the clamp the verifier has to re-derive the
    // bound from the multiply; with it, the bound is explicit.
    barrier_var(bytes);
    if (bytes > PAYLOAD_BYTES)
        bytes = PAYLOAD_BYTES;

    msg = bpf_ringbuf_reserve(&events, sizeof(*msg), 0);
    if (!msg) {
        count_drop(kind, count);
        return 0;
    }

    if (bpf_probe_read_user(msg->payload, bytes, (const void *)ptr) != 0) {
        bpf_ringbuf_discard(msg, 0);
        count_drop(kind, count);
        return 0;
    }

    id = bpf_get_current_pid_tgid();

    // Only the sampled-launch probe carries a stack: it is the only one that
    // fires on the launching thread, once per launch, unbatched. Captured
    // after the reservation succeeded so a batch that will never be
    // submitted does not consume a gpu_stacks slot.
    //
    // A negative return is a real outcome — an empty walk, a full map, a
    // refused insert — not an error to bail on. It is counted, and the
    // record still flows with a negative stack_id, so a launch is never lost
    // merely because its stack was.
    if (kind == KIND_LAUNCH_SAMPLED) {
        stack_id = capture_stack(ctx, (__u32)(id >> 32));
        if (stack_id < 0)
            count_stack_missing(KIND_LAUNCH_SAMPLED);
    }

    msg->hdr.kind = kind;
    msg->hdr.count = (__u32)count;
    msg->hdr.seq = seq;
    msg->hdr.pid = (__u32)(id >> 32);
    msg->hdr.tid = (__u32)id;
    msg->hdr.bytes = bytes;
    msg->hdr.stack_id = stack_id;
    msg->hdr._pad = 0;

    bpf_ringbuf_submit(msg, 0);
    return 0;
}
