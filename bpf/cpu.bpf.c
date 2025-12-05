//go:build ignore
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

// #define MAX_PID 4194304
// #define MAX_CPUS 1024

// #define CPU_USAGE_GROUP_WIDTH 8
// #define SOFTIRQ_GROUP_WIDTH 16



// // the offsets to use in the `counters` group
// #define USER_OFFSET 0
// #define SYSTEM_OFFSET 1


// // tracking per-task user time
// struct {
//     __uint(type, BPF_MAP_TYPE_ARRAY);
//     __uint(map_flags, BPF_F_MMAPABLE);
//     __type(key, u32);
//     __type(value, u64);
//     __uint(max_entries, MAX_PID);
// } task_utime SEC(".maps");

// // tracking per-task system time
// struct {
//     __uint(type, BPF_MAP_TYPE_ARRAY);
//     __uint(map_flags, BPF_F_MMAPABLE);
//     __type(key, u32);
//     __type(value, u64);
//     __uint(max_entries, MAX_PID);
// } task_stime SEC(".maps");

// // per-cpu cpu usage tracking in nanoseconds by category
// // 0 - USER
// // 1 - SYSTEM
// struct {
//     __uint(type, BPF_MAP_TYPE_ARRAY);
//     __uint(map_flags, BPF_F_MMAPABLE);
//     __type(key, u32);
//     __type(value, u64);
//     __uint(max_entries, MAX_CPUS* CPU_USAGE_GROUP_WIDTH);
// } cpu_usage SEC(".maps");

// // struct {
// //     __uint(type, BPF_MAP_TYPE_HASH);
// //     __type(key, u32);  // PID
// //     __type(value, u64);
// //     __uint(max_entries, MAX_PIDS);
// // } task_stime_prev SEC(".maps");
// //
// // Per-PID CPU usage (nanoseconds)
// struct {
//     __uint(type, BPF_MAP_TYPE_HASH);
//     __type(key, u32);  // PID
//     __type(value, u64);
//     __uint(max_entries, MAX_PID);
// } pid_cpu_user_time SEC(".maps");


// struct {
//     __uint(type, BPF_MAP_TYPE_HASH);
//     __type(key, u32);  // PID
//     __type(value, u64);
//     __uint(max_entries, MAX_PID);
// } pid_cpu_system_time SEC(".maps");


// // track the start time of softirq
// struct {
//     __uint(type, BPF_MAP_TYPE_ARRAY);
//     __uint(max_entries, MAX_CPUS * 8);
//     __type(key, u32);
//     __type(value, u64);
// } softirq_start SEC(".maps");

// // per-cpu softirq counts by category
// // 0 - HI
// // 1 - TIMER
// // 2 - NET_TX
// // 3 - NET_RX
// // 4 - BLOCK
// // 5 - IRQ_POLL
// // 6 - TASKLET
// // 7 - SCHED
// // 8 - HRTIMER
// // 9 - RCU
// struct {
//     __uint(type, BPF_MAP_TYPE_ARRAY);
//     __uint(map_flags, BPF_F_MMAPABLE);
//     __type(key, u32);
//     __type(value, u64);
//     __uint(max_entries, MAX_CPUS* SOFTIRQ_GROUP_WIDTH);
// } softirq SEC(".maps");

// // Config: which PIDs to track
// struct {
//     __uint(type, BPF_MAP_TYPE_HASH);
//     __type(key, u32);
//     __type(value, u8);
//     __uint(max_entries, MAX_PID);
// } tracked_pids SEC(".maps");

// // per-cpu softirq time in nanoseconds by category
// // 0 - HI
// // 1 - TIMER
// // 2 - NET_TX
// // 3 - NET_RX
// // 4 - BLOCK
// // 5 - IRQ_POLL
// // 6 - TASKLET
// // 7 - SCHED
// // 8 - HRTIMER
// // 9 - RCU
// struct {
//     __uint(type, BPF_MAP_TYPE_ARRAY);
//     __uint(map_flags, BPF_F_MMAPABLE);
//     __type(key, u32);
//     __type(value, u64);
//     __uint(max_entries, MAX_CPUS* SOFTIRQ_GROUP_WIDTH);
// } softirq_time SEC(".maps");


// // Add this map at the top with other maps
// struct {
//     __uint(type, BPF_MAP_TYPE_HASH);
//     __type(key, u32);
//     __type(value, u64);
//     __uint(max_entries, 100);
// } pid_call_count SEC(".maps");


// // Helper: atomic add
// static __always_inline void map_add(void* map, u32 key, u64 value) {
//     u64* elem = bpf_map_lookup_elem(map, &key);
//     if (elem) {
//         // Use legacy atomics which LLVM lowers to BPF XADD
//         __sync_fetch_and_add(elem, value);
//     }
// }

// static __always_inline void array_add(void* array, u32 idx, u64 value) {
//     u64* elem;

//     elem = bpf_map_lookup_elem(array, &idx);

//     if (elem) {
//         __atomic_fetch_add(elem, value, __ATOMIC_RELAXED);
//     }
// }

// static __always_inline void array_incr(void* array, u32 idx) {
//     array_add(array, idx, 1);
// }

// SEC("kprobe/cpuacct_account_field")
// int BPF_KPROBE(cpuacct_account_field_kprobe,
//                struct task_struct* task,
//                u32 index,
//                u64 delta) {
//     u32 cpu, idx;
//     u32 pid;
//     u64 curr_utime, curr_stime;
//     u64 *last_utime, *last_stime;




//     if (!task) {
//         return 0;
//     }

//    // pid = BPF_CORE_READ(task, pid);
//     u32 tgid = bpf_get_current_pid_tgid() >> 32;  // TGID is upper 32 bits
//     pid = tgid;  // Use TG

//     if (pid == 0 || pid >= MAX_PID) {
//         return 0;
//     }

// //         // Then in the kprobe function, right after the PID filter check:
// // // Debug: count how many times we see this PID
// // u64 *count = bpf_map_lookup_elem(&pid_call_count, &pid);
// // if (count) {
// //     __sync_fetch_and_add(count, 1);
// // } else {
// //     u64 one = 1;
// //     bpf_map_update_elem(&pid_call_count, &pid, &one, BPF_NOEXIST);
// // }


//     // Check if we should track this PID - only track if explicitly enabled
//     u8 *should_track = bpf_map_lookup_elem(&tracked_pids, &pid);
//     if (!should_track || *should_track == 0) {
//         // PID not in map or explicitly disabled, skip
//         return 0;
//     }

//     // Read current values from kernel
//     curr_utime = BPF_CORE_READ(task, utime);
//     curr_stime = BPF_CORE_READ(task, stime);

//     // Get previous values
//     last_utime = bpf_map_lookup_elem(&task_utime, &pid);
//     last_stime = bpf_map_lookup_elem(&task_stime, &pid);

//     if (!last_utime || !last_stime)
//         return 0;

//     // Calculate deltas with overflow protection
//     u64 delta_utime = 0;
//     u64 delta_stime = 0;

//     // Only calculate delta if we have valid previous values
//     // AND if the current value is reasonably close (within 1 hour of CPU time)
//     if (*last_utime != 0 && curr_utime >= *last_utime) {
//         delta_utime = curr_utime - *last_utime;
//     }

//     if (*last_stime != 0 && curr_stime >= *last_stime) {
//         delta_stime = curr_stime - *last_stime;
//     }

//     // Update previous values
//     *last_utime = curr_utime;
//     *last_stime = curr_stime;

//     // // // Skip if no change
//     // if (delta_utime == 0 && delta_stime == 0)
//     //     return 0;

//     // Initialize maps on first access (even if delta is 0) so PID appears immediately
//     if (delta_utime == 0 && delta_stime == 0) {
//         // First time seeing this PID with non-zero utime/stime, initialize maps
//         // This ensures the PID appears in the map for tracking
//         u64 zero = 0;
//         u64* existing_user = bpf_map_lookup_elem(&pid_cpu_user_time, &pid);
//         u64* existing_system = bpf_map_lookup_elem(&pid_cpu_system_time, &pid);

//         if (!existing_user) {
//             bpf_map_update_elem(&pid_cpu_user_time, &pid, &zero, BPF_NOEXIST);
//         }
//         if (!existing_system) {
//             bpf_map_update_elem(&pid_cpu_system_time, &pid, &zero, BPF_NOEXIST);
//         }
//         return 0;
//     }

//     // Get CPU index
//     cpu = bpf_get_smp_processor_id();
//     if (cpu >= MAX_CPUS)
//         return 0;

//     // Update per-CPU user time
//     if (delta_utime > 0) {
//         idx = CPU_USAGE_GROUP_WIDTH * cpu + USER_OFFSET;
//         if (idx < MAX_CPUS * CPU_USAGE_GROUP_WIDTH) {
//             array_add(&cpu_usage, idx, delta_utime);
//         }
//     }

//     // Update per-CPU system time
//     if (delta_stime > 0) {
//         idx = CPU_USAGE_GROUP_WIDTH * cpu + SYSTEM_OFFSET;
//         if (idx < MAX_CPUS * CPU_USAGE_GROUP_WIDTH) {
//             array_add(&cpu_usage, idx, delta_stime);
//         }
//     }


//     // // Update per-PID counters
//     if (delta_utime > 0) {
//         map_add(&pid_cpu_user_time, pid, delta_utime);
//     }
    
//     if (delta_stime > 0) {
//         map_add(&pid_cpu_system_time, pid, delta_stime);
//     }

//     return 0;
// }


// SEC("tracepoint/irq/softirq_entry")
// int softirq_enter(struct trace_event_raw_softirq* args) {
//     u32 cpu = bpf_get_smp_processor_id();
//     u64 ts = bpf_ktime_get_ns();

//     u32 idx = cpu * SOFTIRQ_GROUP_WIDTH + args->vec;
//     u32 start_idx = cpu * 8;

//     bpf_map_update_elem(&softirq_start, &start_idx, &ts, 0);
//     array_incr(&softirq, idx);

//     return 0;
// }

// SEC("tracepoint/irq/softirq_exit")
// int softirq_exit(struct trace_event_raw_softirq* args) {
//     u32 cpu = bpf_get_smp_processor_id();
//     u64 *start_ts, dur = 0;
//     u32 idx, cpuusage_idx;
//     u32 start_idx = cpu * 8;

//     // lookup the start time
//     start_ts = bpf_map_lookup_elem(&softirq_start, &start_idx);

//     // possible we missed the start
//     if (!start_ts || *start_ts == 0) {
//         return 0;
//     }

//     struct task_struct* current = (struct task_struct*)bpf_get_current_task();
//     int pid = BPF_CORE_READ(current, pid);

//     // calculate the duration
//     dur = bpf_ktime_get_ns() - *start_ts;

//     // update the softirq time
//     idx = SOFTIRQ_GROUP_WIDTH * cpu + args->vec;
//     array_add(&softirq_time, idx, dur);
//     if (pid == 0) {
//         cpuusage_idx = CPU_USAGE_GROUP_WIDTH * cpu + SYSTEM_OFFSET;
//         array_add(&cpu_usage, cpuusage_idx, dur);
//     }

//     // clear the start timestamp
//     *start_ts = 0;

//     return 0;
// }


struct cpu_stat_s {
    u32 cpu;
    u64 busy;
    u64 total;
    u64 timestamp;
};

struct pid_stat_s {
    u32 tgid;
    u64 delta_ns;
    u64 timestamp;
};

// Ring buffer for host-wide CPU stats
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 256 * 1024);
} rb SEC(".maps");

// Filter: tracked TGIDs
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, u32);
    __type(value, u8);
} pid_filter SEC(".maps");

// Last seen timestamp per CPU
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 128);
    __type(key, u32);
    __type(value, u64);
} last_seen SEC(".maps");

SEC("tracepoint/sched/sched_switch")
int handle_switch(struct trace_event_raw_sched_switch *ctx) {
    u64 now = bpf_ktime_get_ns();
    u32 cpu = bpf_get_smp_processor_id();

    u64 *last_ts = bpf_map_lookup_elem(&last_seen, &cpu);
    u64 delta = last_ts ? now - *last_ts : 0;

    if (ctx->prev_pid != 0) {
        u32 tgid = bpf_get_current_pid_tgid() >> 32;
        u8 *track = bpf_map_lookup_elem(&pid_filter, &tgid);
        if (track) {
            struct pid_stat_s *ps = bpf_ringbuf_reserve(&rb, sizeof(*ps), 0);
            if (ps) {
                ps->tgid = tgid;
                ps->delta_ns = delta;
                ps->timestamp = now;
                bpf_ringbuf_submit(ps, 0);
            }
        }
    }

    bpf_map_update_elem(&last_seen, &cpu, &now, BPF_ANY);
    return 0;
}


SEC("fexit/kcpustat_cpu_fetch")
int BPF_PROG(on_kcpustat_fetch, struct kernel_cpustat *kcpustat, int cpu) {
    struct cpu_stat_s *stat = bpf_ringbuf_reserve(&rb, sizeof(*stat), 0);
    if (!stat) return 0;

    stat->cpu = cpu;
    stat->busy = BPF_CORE_READ(kcpustat, cpustat[CPUTIME_USER]) +
                 BPF_CORE_READ(kcpustat, cpustat[CPUTIME_SYSTEM]) +
                 BPF_CORE_READ(kcpustat, cpustat[CPUTIME_IRQ]) +
                 BPF_CORE_READ(kcpustat, cpustat[CPUTIME_SOFTIRQ]);
    stat->total = stat->busy + BPF_CORE_READ(kcpustat, cpustat[CPUTIME_IDLE]);
    stat->timestamp = bpf_ktime_get_ns();
    bpf_ringbuf_submit(stat, 0);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";