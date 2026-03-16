//go:build ignore
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

//#include "perf.h"

#define PERF_MAX_STACK_DEPTH    127
#define PROFILE_MAPS_SIZE       16384

struct sample_key {
    __u32 pid;
    __u32 flags;
    __s64 kern_stack;
    __s64 user_stack;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct sample_key);
    __type(value, u64);
    __uint(max_entries, PROFILE_MAPS_SIZE);
} counts SEC(".maps");


struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(key_size, sizeof(u32));
    __uint(value_size, PERF_MAX_STACK_DEPTH * sizeof(u64));
    __uint(max_entries, PROFILE_MAPS_SIZE);
} stackmap SEC(".maps");


#define KERN_STACKID_FLAGS (0 | BPF_F_FAST_STACK_CMP)
#define USER_STACKID_FLAGS (0 | BPF_F_FAST_STACK_CMP | BPF_F_USER_STACK)

////////// PERF CONFIG //////////

// System-wide mode: when true, profile all processes; when false, use PID filter
const volatile bool system_wide = false;

// PID namespace device and inode for namespace-aware PID resolution.
// When both are non-zero, bpf_get_ns_current_pid_tgid() is used instead of
// bpf_get_current_pid_tgid(), allowing correct PID matching inside containers.
const volatile u64 pidns_dev = 0;
const volatile u64 pidns_ino = 0;

struct pid_config {
    uint8_t type;
    uint8_t collect_user;
    uint8_t collect_kernel;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, u32);
    __type(value, struct pid_config);
    __uint(max_entries, 2048);
} pids SEC(".maps");

#define PF_KTHREAD 0x00200000 /* I am a kernel thread */

////////// PERF CONFIG //////////

SEC("perf_event")
int profile(struct bpf_perf_event_data *ctx) {
    char kv_fmt[] = "key %llx, value %lle\n";
    char counter_fmt[] = "increment counter %llx\n";

    int tgid;

    // Use namespace-aware PID resolution when pidns info is available.
    // This is needed when running inside containers (e.g. K8s sidecars)
    // where the PID namespace differs from the host namespace.
    if (pidns_dev != 0 && pidns_ino != 0) {
        struct bpf_pidns_info nsdata = {};
        if (bpf_get_ns_current_pid_tgid(pidns_dev, pidns_ino, &nsdata, sizeof(nsdata)) == 0) {
            tgid = nsdata.tgid;
        } else {
            tgid = bpf_get_current_pid_tgid() >> 32;
        }
    } else {
        tgid = bpf_get_current_pid_tgid() >> 32;
    }

    u64 *val, one = 1;

    //
    struct task_struct *task = (struct task_struct *)bpf_get_current_task();
    if (tgid == 0 || task == 0) {
        return 0;
    }

    // skip kernel threads
    if (BPF_CORE_READ(task, flags) & PF_KTHREAD) {
        return 0;
    }

    // In system-wide mode, collect from all processes
    // In targeted mode, check if config exists for this pid
    struct pid_config *config = NULL;
    if (!system_wide) {
        config = bpf_map_lookup_elem(&pids, &tgid);
        if (config == NULL) {
            char kernel_fmt[] = "config is null for pid %d\n";
            bpf_trace_printk(kernel_fmt, sizeof(kernel_fmt), tgid);
            return 0;
        }
    }

    // struct bpf_perf_event_value value_buf;
    struct sample_key key = {};

    key.pid = tgid;
    key.kern_stack = -1;
    key.user_stack = -1;

    // In system-wide mode, always collect user stacks (default behavior)
    // In targeted mode, use config settings
    bool collect_kernel = system_wide ? false : (config && config->collect_kernel);
    bool collect_user = system_wide ? true : (config && config->collect_user);

    if (collect_kernel) {
        key.kern_stack = bpf_get_stackid(ctx, &stackmap, KERN_STACKID_FLAGS);
    }

    if (collect_user) {
        key.user_stack = bpf_get_stackid(ctx, &stackmap, USER_STACKID_FLAGS);
    }

    char key_fmt[] = "pid %d, kern_stack %d, user_stack %d\n";
    bpf_trace_printk(key_fmt, sizeof(key_fmt), key.pid, key.kern_stack, key.user_stack);

    val = bpf_map_lookup_elem(&counts, &key);
    if (val) {
        (*val)++;
        bpf_trace_printk(counter_fmt, sizeof(counter_fmt), *val);
    } else {
        bpf_map_update_elem(&counts, &key, &one, BPF_NOEXIST);
        bpf_trace_printk(kv_fmt, sizeof(kv_fmt), &key, &one);
    }
    return 0;

}

char LICENSE[] SEC("license") = "GPL";