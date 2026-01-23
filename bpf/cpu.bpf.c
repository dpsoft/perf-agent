//go:build ignore
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

// Task state flags from linux/sched.h -> // https://github.com/torvalds/linux/blob/v6.1/include/linux/sched.h#L84
#define TASK_INTERRUPTIBLE   0x00000001
#define TASK_UNINTERRUPTIBLE 0x00000002

// Simplified task state classification
#define STATE_RUNNING         0  // Was running, got preempted
#define STATE_INTERRUPTIBLE   1  // Voluntary sleep (mutex, sleep())
#define STATE_UNINTERRUPTIBLE 2  // I/O wait (D state)

struct cpu_stat_s {
    u32 cpu;
    u64 busy;
    u64 total;
    u64 timestamp;
};

struct pid_stat_s {
    u32 tgid;
    u32 _pad0;           // Alignment padding
    u64 delta_ns;        // On-CPU time
    u64 runq_latency_ns; // Runqueue wait time (wakeup to switch)
    u64 cycles;
    u64 instructions;
    u64 cache_misses;
    u64 timestamp;
    u8 prev_state;       // Why switched out: 0=running, 1=sleep, 2=io
    u8 preempt;          // Was preempted?
    u8 _pad1[6];         // Alignment padding to 8-byte boundary
};

// Hardware perf counter readers (userspace attaches perf FDs to these maps)
struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __type(key, u32);
    __type(value, u32);
    __uint(max_entries, 128);
} cpu_cycles_reader SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __type(key, u32);
    __type(value, u32);
    __uint(max_entries, 128);
} cpu_instructions_reader SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __type(key, u32);
    __type(value, u32);
    __uint(max_entries, 128);
} cache_misses_reader SEC(".maps");

// Per-CPU arrays to store previous counter values for delta calculation
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, u32);
    __type(value, u64);
    __uint(max_entries, 1);
} prev_cycles SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, u32);
    __type(value, u64);
    __uint(max_entries, 1);
} prev_instructions SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, u32);
    __type(value, u64);
    __uint(max_entries, 1);
} prev_cache_misses SEC(".maps");

// Config flag: enable hardware counters (set from userspace)
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __type(key, u32);
    __type(value, u32);
    __uint(max_entries, 1);
} hw_counters_enabled SEC(".maps");

// Helper: read counter and calculate delta since last read
static __always_inline u64 read_counter_delta(void *reader_map, void *prev_map, u32 cpu) {
    u32 key = 0;
    struct bpf_perf_event_value val = {};

    long err = bpf_perf_event_read_value(reader_map, cpu, &val, sizeof(val));
    if (err)
        return 0;

    u64 *prev = bpf_map_lookup_elem(prev_map, &key);
    u64 delta = 0;

    if (prev && val.counter > *prev)
        delta = val.counter - *prev;

    u64 current = val.counter;
    bpf_map_update_elem(prev_map, &key, &current, BPF_ANY);

    return delta;
}

// System-wide mode: when true, profile all processes; when false, use PID filter
const volatile bool system_wide = false;

// Ring buffer for host-wide CPU stats
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 256 * 1024);
} rb SEC(".maps");

// Filter: tracked TGIDs (used when system_wide=false)
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

// Wakeup timestamps for runqueue latency calculation
// Key: pid, Value: timestamp when sched_wakeup occurred
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 10240);
    __type(key, u32);
    __type(value, u64);
} wakeup_ts SEC(".maps");

// Per-PID runqueue latency (stored when task starts running, used when it stops)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 10240);
    __type(key, u32);
    __type(value, u64);
} pid_runq_latency SEC(".maps");

#define PF_KTHREAD 0x00200000 /* I am a kernel thread */

// Helper: classify task state
static __always_inline u8 classify_task_state(unsigned int state)
{
    if (state & TASK_INTERRUPTIBLE) {
        return STATE_INTERRUPTIBLE;
    } else if (state & TASK_UNINTERRUPTIBLE) {
        return STATE_UNINTERRUPTIBLE;
    }
    return STATE_RUNNING;
}

// Track when a task becomes runnable (for runqueue latency calculation)
SEC("tp_btf/sched_wakeup")
int BPF_PROG(handle_wakeup, struct task_struct *p)
{
    u32 tgid = BPF_CORE_READ(p, tgid);
    
    // Check if we should track this process
    if (!system_wide && !bpf_map_lookup_elem(&pid_filter, &tgid))
        return 0;
    
    // Skip kernel threads
    u32 flags = BPF_CORE_READ(p, flags);
    if (flags & PF_KTHREAD)
        return 0;
    
    u32 pid = BPF_CORE_READ(p, pid);
    u64 ts = bpf_ktime_get_ns();
    
    // Store wakeup timestamp for this pid
    bpf_map_update_elem(&wakeup_ts, &pid, &ts, BPF_ANY);
    
    return 0;
}

// Also handle newly created tasks
SEC("tp_btf/sched_wakeup_new")
int BPF_PROG(handle_wakeup_new, struct task_struct *p)
{
    u32 tgid = BPF_CORE_READ(p, tgid);
    
    // Check if we should track this process
    if (!system_wide && !bpf_map_lookup_elem(&pid_filter, &tgid))
        return 0;
    
    // Skip kernel threads
    u32 flags = BPF_CORE_READ(p, flags);
    if (flags & PF_KTHREAD)
        return 0;
    
    u32 pid = BPF_CORE_READ(p, pid);
    u64 ts = bpf_ktime_get_ns();
    
    // Store wakeup timestamp for this pid
    bpf_map_update_elem(&wakeup_ts, &pid, &ts, BPF_ANY);
    
    return 0;
}

SEC("tp_btf/sched_switch")
int BPF_PROG(handle_switch, bool preempt, struct task_struct *prev, struct task_struct *next) {
    u64 now = bpf_ktime_get_ns();
    u32 cpu = bpf_get_smp_processor_id();

    // Read hardware counter deltas (if enabled)
    u32 key = 0;
    u32 *hw_enabled = bpf_map_lookup_elem(&hw_counters_enabled, &key);

    u64 cycles = 0, instrs = 0, cache_miss = 0;
    if (hw_enabled && *hw_enabled) {
        cycles = read_counter_delta(&cpu_cycles_reader, &prev_cycles, cpu);
        instrs = read_counter_delta(&cpu_instructions_reader, &prev_instructions, cpu);
        cache_miss = read_counter_delta(&cache_misses_reader, &prev_cache_misses, cpu);
    }

    u64 *last_ts = bpf_map_lookup_elem(&last_seen, &cpu);
    u64 delta = last_ts ? now - *last_ts : 0;

    // --- Handle NEXT task coming ON-CPU ---
    // Calculate runqueue latency (time from wakeup to now)
    u32 next_pid = BPF_CORE_READ(next, pid);
    u32 next_tgid = BPF_CORE_READ(next, tgid);
    if (next_pid != 0) {
        u64 *wake_ts = bpf_map_lookup_elem(&wakeup_ts, &next_pid);
        if (wake_ts) {
            u64 runq_lat = now - *wake_ts;
            // Store runqueue latency for this pid (will be used when it goes off-CPU)
            bpf_map_update_elem(&pid_runq_latency, &next_pid, &runq_lat, BPF_ANY);
            // Clean up wakeup timestamp
            bpf_map_delete_elem(&wakeup_ts, &next_pid);
        }
    }

    // --- Handle PREV task going OFF-CPU ---
    u32 prev_pid = BPF_CORE_READ(prev, pid);
    if (prev_pid != 0) {
        u32 tgid = BPF_CORE_READ(prev, tgid);

        // Read task state early - needed for both event emission and enqueue tracking
        unsigned int prev_state = BPF_CORE_READ(prev, __state);
        u8 prev_state_classified = classify_task_state(prev_state);

        // Skip kernel threads in system-wide mode
        if (system_wide) {
            u32 flags = BPF_CORE_READ(prev, flags);
            if (flags & PF_KTHREAD)
                goto update_timestamp;
        }

        // In system-wide mode, track all processes; otherwise check PID filter
        bool should_track = system_wide || bpf_map_lookup_elem(&pid_filter, &tgid);
        if (should_track) {
            struct pid_stat_s *ps = bpf_ringbuf_reserve(&rb, sizeof(*ps), 0);
            if (ps) {
                ps->tgid = tgid;
                ps->delta_ns = delta;

                // Get runqueue latency that was stored when this task started running
                u64 *runq_lat = bpf_map_lookup_elem(&pid_runq_latency, &prev_pid);
                ps->runq_latency_ns = runq_lat ? *runq_lat : 0;
                if (runq_lat) {
                    bpf_map_delete_elem(&pid_runq_latency, &prev_pid);
                }

                ps->cycles = cycles;
                ps->instructions = instrs;
                ps->cache_misses = cache_miss;
                ps->timestamp = now;

                // Use pre-computed task state classification
                ps->prev_state = prev_state_classified;
                ps->preempt = preempt ? 1 : 0;

                bpf_ringbuf_submit(ps, 0);
            }

            // Track enqueue time for preempted tasks (for runqueue latency measurement)
            // When a task is preempted while still runnable, it goes back to the runqueue
            // without going through sched_wakeup, so we record the timestamp here
            if (preempt || prev_state_classified == STATE_RUNNING) {
                bpf_map_update_elem(&wakeup_ts, &prev_pid, &now, BPF_ANY);
            }
        }
    }

update_timestamp:
    bpf_map_update_elem(&last_seen, &cpu, &now, BPF_ANY);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";