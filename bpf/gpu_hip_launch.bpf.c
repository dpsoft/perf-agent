//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

const volatile __u32 target_pid = 0;

#define MAX_STACK_DEPTH 127

struct raw_record {
    __u32 pid;
    __u32 tid;
    __u64 time_ns;
    __u64 function_addr;
    __s32 user_stack_id;
    __u32 _pad0;
    __u64 stream;
    __u64 cgroup_id;
};

struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __type(value, __u64[MAX_STACK_DEPTH]);
} stackmap SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 256 * 1024);
} events SEC(".maps");

static __always_inline bool should_trace(__u32 pid)
{
    if (target_pid == 0) {
        return false;
    }
    return pid == target_pid;
}

SEC("uprobe/hipLaunchKernel")
int handle_hip_launch(struct pt_regs *ctx)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;

    if (!should_trace(pid)) {
        return 0;
    }

    struct raw_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
    if (!record) {
        return 0;
    }

    record->pid = pid;
    record->tid = (__u32)pid_tgid;
    record->time_ns = bpf_ktime_get_ns();
    record->function_addr = (__u64)PT_REGS_PARM1(ctx);
    record->user_stack_id = bpf_get_stackid(ctx, &stackmap, BPF_F_USER_STACK);
    record->stream = (__u64)PT_REGS_PARM6(ctx);
    record->cgroup_id = bpf_get_current_cgroup_id();

    bpf_ringbuf_submit(record, 0);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
