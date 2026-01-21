//go:build ignore
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

struct cpu_stat_s {
    u32 cpu;
    u64 busy;
    u64 total;
    u64 timestamp;
};

struct pid_stat_s {
    u32 tgid;
    u64 delta_ns;
    u64 cycles;
    u64 instructions;
    u64 cache_misses;
    u64 timestamp;
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

#define PF_KTHREAD 0x00200000 /* I am a kernel thread */

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

    u32 prev_pid = BPF_CORE_READ(prev, pid);
    if (prev_pid != 0) {
        u32 tgid = BPF_CORE_READ(prev, tgid);

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
                ps->cycles = cycles;
                ps->instructions = instrs;
                ps->cache_misses = cache_miss;
                ps->timestamp = now;
                bpf_ringbuf_submit(ps, 0);
            }
        }
    }

update_timestamp:
    bpf_map_update_elem(&last_seen, &cpu, &now, BPF_ANY);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";