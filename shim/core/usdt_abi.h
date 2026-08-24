// The USDT ABI. Frozen in Phase 3; every change bumps the probe's version
// suffix rather than mutating a record. Field order is chosen so every field
// is naturally aligned with no compiler-inserted padding (spec §6.3).
#ifndef PERFAGENT_GPU_USDT_ABI_H
#define PERFAGENT_GPU_USDT_ABI_H

#include <stdint.h>
#include <stddef.h>

#define GPU_ABI_VERSION 1

#ifdef __cplusplus
#define GPU_STATIC_ASSERT(cond, msg) static_assert(cond, msg)
#else
#define GPU_STATIC_ASSERT(cond, msg) _Static_assert(cond, msg)
#endif

// Emitted when a kernel is submitted. queue_id is optional: zero means
// unknown. The launch side cannot always derive it (spec §6.3 finding 1).
struct gpu_launch_v1 {
    uint64_t correlation;
    uint64_t kernel_id;
    uint64_t queue_id;
    uint64_t context_id;
    uint64_t time_ns;
    uint32_t tid;
    uint32_t _pad;
};

// Emitted when a kernel's on-device window is known. Authoritative for
// queue and device identity.
struct gpu_exec_v1 {
    uint64_t correlation;
    uint64_t kernel_id;
    uint64_t queue_id;
    uint64_t device_id;
    uint64_t start_ns;
    uint64_t end_ns;
};

// cubin_crc is first because it, not module_id, is what PC samples join on.
struct gpu_module_load_v1 {
    uint64_t cubin_crc;
    uint64_t module_id;
    uint64_t size_bytes;
    uint64_t load_ns;
    uint64_t bytes_ptr;   // adapter-owned copy; never the vendor's buffer
};

// One record per (PC, stall reason) pair. correlation is zero in continuous
// collection, which is the mode we ship (spec §6.3 finding 3).
struct gpu_pc_sample_batch_v1 {
    uint64_t cubin_crc;
    uint64_t correlation;
    uint64_t pc_offset;
    uint32_t function_index;
    uint32_t stall_index;
    uint32_t count;
    uint32_t _pad;
};

struct gpu_config_v1 {
    uint64_t clock_hz;
    uint32_t sampling_factor;
    uint32_t sm_count;
    uint8_t  vendor;
    uint8_t  _pad[7];
};

struct gpu_dropped_v1 {
    uint64_t count;
    uint8_t  klass;
    uint8_t  _pad[7];
};

// Longest kernel name carried inline. CUDA names are mangled C++ and can
// exceed this; truncation is flagged per record rather than hidden.
#define GPU_KERNEL_NAME_MAX 256

// A launch selected for CPU-stack capture. Fires UNBATCHED, one record per
// probe, so the consumer's bpf_get_stackid captures the stack of the thread
// that made THIS launch. gpu_launch_v1 stays batched and unchanged.
struct gpu_launch_sampled_v1 {
    uint64_t correlation;
    uint64_t kernel_id;
    uint64_t queue_id;
    uint64_t context_id;
    uint64_t time_ns;
    uint32_t tid;
    // The N in one-in-N, at capture time; never 0. The MEAN stride, not an
    // exact one: the sampler jitters each gap around N so it cannot lock
    // phase against a periodic launch pattern (shim/core/sampler.h, issue
    // #50). The rate it names -- one launch in N carries a stack -- is
    // unchanged, and it is still not a scale factor: durations are never
    // scaled by it.
    uint32_t sample_period;
    uint64_t launch_seq;      // ordinal among ALL launches, sampled or not
};

// kernel_id -> name, emitted once on first sight and replayed on late
// attach. Fixed-size by the ABI's rules; name_len is authoritative.
struct gpu_kernel_name_v1 {
    uint64_t kernel_id;
    uint16_t name_len;
    uint8_t  truncated;
    uint8_t  _pad[5];
    char     name[GPU_KERNEL_NAME_MAX];
};

GPU_STATIC_ASSERT(sizeof(struct gpu_launch_v1) == 48, "gpu_launch_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_exec_v1) == 48, "gpu_exec_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_module_load_v1) == 40, "gpu_module_load_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_pc_sample_batch_v1) == 40, "gpu_pc_sample_batch_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_config_v1) == 24, "gpu_config_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_dropped_v1) == 16, "gpu_dropped_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_pc_sample_batch_v1, cubin_crc) == 0, "cubin_crc leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_module_load_v1, cubin_crc) == 0, "cubin_crc leads");
GPU_STATIC_ASSERT(sizeof(struct gpu_launch_sampled_v1) == 56, "gpu_launch_sampled_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_kernel_name_v1) == 272, "gpu_kernel_name_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_launch_sampled_v1, sample_period) == 44, "sample_period position");

#endif
