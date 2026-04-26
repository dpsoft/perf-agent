//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

const volatile __u32 target_pid = 0;

#define MINORBITS 20
#define MINORMASK ((1U << MINORBITS) - 1)

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
    __u32 device_major;
    __u64 command;
    __s64 result_code;
    __u64 start_ns;
    __u64 end_ns;
    __u32 device_minor;
    __u32 _pad1;
    __u64 inode;
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

static __always_inline __u32 dev_major(__u32 dev)
{
    return dev >> MINORBITS;
}

static __always_inline __u32 dev_minor(__u32 dev)
{
    return dev & MINORMASK;
}

static __always_inline void capture_file_identity(__s32 fd, __u32 *major, __u32 *minor, __u64 *inode_num)
{
    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    struct files_struct *files = BPF_CORE_READ(task, files);
    if (!files) {
        return;
    }

    struct fdtable *fdt = BPF_CORE_READ(files, fdt);
    if (!fdt) {
        return;
    }

    unsigned int max_fds = BPF_CORE_READ(fdt, max_fds);
    if (fd < 0 || (__u32)fd >= max_fds) {
        return;
    }

    struct file **fd_array = BPF_CORE_READ(fdt, fd);
    if (!fd_array) {
        return;
    }

    struct file *file = NULL;
    bpf_probe_read_kernel(&file, sizeof(file), &fd_array[fd]);
    if (!file) {
        return;
    }

    struct inode *inode = BPF_CORE_READ(file, f_inode);
    if (!inode) {
        return;
    }

    __u32 rdev = BPF_CORE_READ(inode, i_rdev);
    *major = dev_major(rdev);
    *minor = dev_minor(rdev);
    *inode_num = BPF_CORE_READ(inode, i_ino);
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
    capture_file_identity(start->fd, &record->device_major, &record->device_minor, &record->inode);

    bpf_ringbuf_submit(record, 0);
    bpf_map_delete_elem(&inflight, &tid);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
