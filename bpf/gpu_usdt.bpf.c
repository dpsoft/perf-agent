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

char LICENSE[] SEC("license") = "GPL";

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
#define KIND_MAX            8

// The wire record sizes, frozen in shim/core/usdt_abi.h and mirrored by
// internal/gpuabi. They live here as named constants because the payload
// budget below is derived from them at compile time.
#define REC_LAUNCH         48
#define REC_EXEC           48
#define REC_MODULE         40
#define REC_PC             40
#define REC_LAUNCH_SAMPLED 56
#define REC_KERNEL_NAME    272

#define MAX_RECORDS_PER_BATCH 64

// MAX_RECORD_BYTES is the largest record on the wire: gpu_kernel_name_v1 at
// 272 bytes. It deliberately does NOT size the reservation. Sizing the
// reservation for the worst case of every kind would cost
// 40 + 64*272 = 17448 bytes per batch, i.e. ~240 batches in a 4MB ring —
// too few to absorb a burst, and paid on every launch batch to serve two
// probes that never batch at all.
#define MAX_RECORD_BYTES 272

// MAX_BATCHED_RECORD_BYTES is the largest record that ever arrives with
// count > 1: gpu_launch_v1 / gpu_exec_v1, both 48 bytes. gpu_launch_sampled_v1
// (56) rides alone because its stack must belong to exactly one launch, and
// gpu_kernel_name_v1 (272) is emitted one record per interned name.
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
// stack_id is the BPF stackmap key for the launching thread's user stack,
// or negative when the capture failed. It is per-batch, not per-record,
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

// stacks_missing[kind] counts probe fires whose bpf_get_stackid() failed —
// a stack deeper than PERF_MAX_STACK_DEPTH, a full stackmap, or a binary
// built without frame pointers. It is deliberately NOT the `dropped` map:
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

#define PERF_MAX_STACK_DEPTH 127
#define STACK_MAP_SIZE       16384

// The launching thread's user stack, keyed by the id bpf_get_stackid returns
// and read back by gpuprobe (Task 5) exactly as profile/ does.
struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(max_entries, STACK_MAP_SIZE);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, PERF_MAX_STACK_DEPTH * sizeof(__u64));
} stackmap SEC(".maps");

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
    return 0;
}

// max_records is record_size's companion: how many records of this kind one
// reservation can carry. Same if-chain shape and same reason — every arm is
// a compile-time constant, so the verifier sees a bounded scalar and clang
// emits no division.
//
// The byte-budget caps are 48B -> 64, 40B -> 64 (capped), 56B -> 54,
// 272B -> 11. They exist so the clamp is sound for any count a producer
// could pass, not just the counts this shim passes.
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
    return 0;
}

_Static_assert(1 <= BATCH_CAP(REC_LAUNCH_SAMPLED),
               "the sampled-launch cap of 1 must still fit the payload budget");

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
        // only installs cookies 1-6, but this is the one return that would
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

    // Only the sampled-launch probe carries a stack: it is the only one that
    // fires on the launching thread, once per launch, unbatched. Captured
    // after the reservation succeeded so a batch that will never be
    // submitted does not consume a stackmap slot.
    //
    // A negative return is a real outcome — a stack deeper than
    // PERF_MAX_STACK_DEPTH, a full stackmap, or a frame-pointer-less
    // binary — not an error to bail on. It is counted, and the record still
    // flows with a negative stack_id, so a launch is never lost merely
    // because its stack was.
    if (kind == KIND_LAUNCH_SAMPLED) {
        stack_id = (__s32)bpf_get_stackid(ctx, &stackmap, BPF_F_USER_STACK);
        if (stack_id < 0)
            count_stack_missing(KIND_LAUNCH_SAMPLED);
    }

    id = bpf_get_current_pid_tgid();
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
