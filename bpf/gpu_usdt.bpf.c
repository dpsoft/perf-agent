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

#define KIND_LAUNCH 1
#define KIND_EXEC   2
#define KIND_MODULE 3
#define KIND_PC     4
#define KIND_MAX    8

#define MAX_RECORDS_PER_BATCH 64
#define MAX_RECORD_BYTES      48

// The ringbuf reservation is fixed-size: bpf_ringbuf_reserve takes a
// constant, so every batch costs the worst case (64 records * 48 bytes)
// regardless of how many records it actually carries. Userspace reads the
// real length from batch_hdr.bytes.
#define PAYLOAD_BYTES (MAX_RECORDS_PER_BATCH * MAX_RECORD_BYTES)

struct batch_hdr {
    __u32 kind;
    __u32 count;
    __u64 seq;
    __u32 pid;
    __u32 tid;
    __u64 bytes;
};

struct batch_msg {
    struct batch_hdr hdr;
    __u8 payload[PAYLOAD_BYTES];
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 22);   // 4MB
} events SEC(".maps");

// dropped[kind] counts *records* this program could not deliver: a batch
// larger than one reservation can hold, a ringbuf that had no room, or a
// user-memory read that faulted. Spec §6.1 admits no silent loss, so
// userspace reads this map in Consumer.Stats().
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, KIND_MAX);
    __type(key, __u32);
    __type(value, __u64);
} dropped SEC(".maps");

// record_size is an if-chain rather than a switch so clang cannot lower it
// to a .rodata table load, which would hand the verifier an unbounded
// scalar where it needs a constant.
static __always_inline __u32 record_size(__u32 kind)
{
    if (kind == KIND_LAUNCH)
        return 48;
    if (kind == KIND_EXEC)
        return 48;
    if (kind == KIND_MODULE)
        return 40;
    if (kind == KIND_PC)
        return 40;
    return 0;
}

static __always_inline void count_drop(__u32 kind, __u64 records)
{
    __u64 *d;

    if (kind >= KIND_MAX)
        return;
    d = bpf_map_lookup_elem(&dropped, &kind);
    if (d)
        __sync_fetch_and_add(d, records);
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
    __u32 bytes;
    __u64 id;
    struct batch_msg *msg;

    if (rsz == 0 || count == 0)
        return 0;
    if (count > MAX_RECORDS_PER_BATCH) {
        // Truncation is loss. Count the records that will never be copied.
        count_drop(kind, count - MAX_RECORDS_PER_BATCH);
        count = MAX_RECORDS_PER_BATCH;
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
    msg->hdr.kind = kind;
    msg->hdr.count = (__u32)count;
    msg->hdr.seq = seq;
    msg->hdr.pid = (__u32)(id >> 32);
    msg->hdr.tid = (__u32)id;
    msg->hdr.bytes = bytes;

    bpf_ringbuf_submit(msg, 0);
    return 0;
}
