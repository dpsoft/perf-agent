//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>

const volatile __u32 target_pid = 0;

enum record_kind {
    RECORD_KIND_UNKNOWN = 0,
    RECORD_KIND_IOCTL = 1,
};

struct inflight_ioctl {
    __s32 fd;
    __u32 _pad0;
    __u64 command;
    __u64 start_ns;
};

struct raw_record {
    __u8 kind;
    __u8 _pad0[3];
    __u32 pid;
    __u32 tid;
    __s32 fd;
    __u32 _pad1;
    __u64 command;
    __s64 result_code;
    __u64 start_ns;
    __u64 end_ns;
    __u64 device_id;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __type(value, struct inflight_ioctl);
} inflight SEC(".maps");

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

SEC("tracepoint/syscalls/sys_enter_ioctl")
int handle_enter_ioctl(struct trace_event_raw_sys_enter *ctx)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;
    __u32 tid = (__u32)pid_tgid;

    if (!should_trace(pid)) {
        return 0;
    }

    struct inflight_ioctl value = {
        .fd = (__s32)ctx->args[0],
        .command = (__u64)ctx->args[1],
        .start_ns = bpf_ktime_get_ns(),
    };
    bpf_map_update_elem(&inflight, &tid, &value, BPF_ANY);
    return 0;
}

SEC("tracepoint/syscalls/sys_exit_ioctl")
int handle_exit_ioctl(struct trace_event_raw_sys_exit *ctx)
{
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u32 pid = pid_tgid >> 32;
    __u32 tid = (__u32)pid_tgid;

    if (!should_trace(pid)) {
        return 0;
    }

    struct inflight_ioctl *start = bpf_map_lookup_elem(&inflight, &tid);
    if (!start) {
        return 0;
    }

    struct raw_record *record = bpf_ringbuf_reserve(&events, sizeof(*record), 0);
    if (!record) {
        bpf_map_delete_elem(&inflight, &tid);
        return 0;
    }

    record->kind = RECORD_KIND_IOCTL;
    record->pid = pid;
    record->tid = tid;
    record->fd = start->fd;
    record->command = start->command;
    record->result_code = ctx->ret;
    record->start_ns = start->start_ns;
    record->end_ns = bpf_ktime_get_ns();
    record->device_id = 0;

    bpf_ringbuf_submit(record, 0);
    bpf_map_delete_elem(&inflight, &tid);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
