//go:build ignore
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#define PERF_MAX_STACK_DEPTH    127
#define OFFCPU_MAPS_SIZE        16384
#define START_MAPS_SIZE         10240

// Key for tracking when a task went off-CPU
struct start_key {
    u32 pid;
    u32 tgid;
};

// Value stored when task goes off-CPU: timestamp and captured stacks
struct start_val {
    u64 timestamp;
    s64 kern_stack;
    s64 user_stack;
};

// Key for aggregating off-CPU time by stack
struct offcpu_key {
    u32 pid;
    s64 kern_stack;
    s64 user_stack;
};

// Map to track when tasks went off-CPU and their stacks
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct start_key);
    __type(value, struct start_val);
    __uint(max_entries, START_MAPS_SIZE);
} start SEC(".maps");

// Map to aggregate off-CPU nanoseconds by stack
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct offcpu_key);
    __type(value, u64);
    __uint(max_entries, OFFCPU_MAPS_SIZE);
} offcpu_counts SEC(".maps");

// Stack trace storage
struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(key_size, sizeof(u32));
    __uint(value_size, PERF_MAX_STACK_DEPTH * sizeof(u64));
    __uint(max_entries, OFFCPU_MAPS_SIZE);
} stackmap SEC(".maps");

// PID filter: only track specified PIDs
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, u32);
    __type(value, u8);
} pid_filter SEC(".maps");

#define KERN_STACKID_FLAGS (0 | BPF_F_FAST_STACK_CMP)
#define USER_STACKID_FLAGS (0 | BPF_F_FAST_STACK_CMP | BPF_F_USER_STACK)

#define PF_KTHREAD 0x00200000 /* I am a kernel thread */

static int handle_sched_switch(void *ctx, bool preempt,
                               struct task_struct *prev, struct task_struct *next)
{
    u64 now = bpf_ktime_get_ns();

    u32 prev_pid = BPF_CORE_READ(prev, pid);
    u32 prev_tgid = BPF_CORE_READ(prev, tgid);
    u32 next_pid = BPF_CORE_READ(next, pid);
    u32 next_tgid = BPF_CORE_READ(next, tgid);

    // Handle prev task going off-CPU
    if (prev_pid != 0) {
        // Check if we're tracking this PID
        u8 *track = bpf_map_lookup_elem(&pid_filter, &prev_tgid);
        if (track) {
            // Skip kernel threads
            u32 flags = BPF_CORE_READ(prev, flags);
            if (!(flags & PF_KTHREAD)) {
                struct start_key sk = {
                    .pid = prev_pid,
                    .tgid = prev_tgid,
                };

                struct start_val sv = {
                    .timestamp = now,
                    .kern_stack = -1,
                    .user_stack = -1,
                };

                // Capture stack trace using ctx (BPF_PROG provides this)
                // At sched_switch, prev is still "current" so ctx captures its stack
                sv.kern_stack = bpf_get_stackid(ctx, &stackmap, KERN_STACKID_FLAGS);
                sv.user_stack = bpf_get_stackid(ctx, &stackmap, USER_STACKID_FLAGS);

                bpf_map_update_elem(&start, &sk, &sv, BPF_ANY);
            }
        }
    }

    // Handle next task returning to CPU
    if (next_pid != 0) {
        u8 *track = bpf_map_lookup_elem(&pid_filter, &next_tgid);
        if (track) {
            struct start_key sk = {
                .pid = next_pid,
                .tgid = next_tgid,
            };

            struct start_val *sv = bpf_map_lookup_elem(&start, &sk);
            if (sv && sv->timestamp > 0) {
                // Calculate how long the task was off-CPU
                u64 delta = now - sv->timestamp;

                // Aggregate by stack
                struct offcpu_key ok = {
                    .pid = next_tgid,  // Use tgid for consistency with on-CPU profiler
                    .kern_stack = sv->kern_stack,
                    .user_stack = sv->user_stack,
                };

                u64 *val = bpf_map_lookup_elem(&offcpu_counts, &ok);
                if (val) {
                    __sync_fetch_and_add(val, delta);
                } else {
                    bpf_map_update_elem(&offcpu_counts, &ok, &delta, BPF_NOEXIST);
                }

                // Clear the start entry
                bpf_map_delete_elem(&start, &sk);
            }
        }
    }

    return 0;
}

SEC("tp_btf/sched_switch")
int BPF_PROG(offcpu_sched_switch, bool preempt, struct task_struct *prev, struct task_struct *next)
{
    return handle_sched_switch(ctx, preempt, prev, next);
}

char LICENSE[] SEC("license") = "GPL";
