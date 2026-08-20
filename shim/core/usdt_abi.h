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

GPU_STATIC_ASSERT(sizeof(struct gpu_launch_v1) == 48, "gpu_launch_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_exec_v1) == 48, "gpu_exec_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_module_load_v1) == 40, "gpu_module_load_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_pc_sample_batch_v1) == 40, "gpu_pc_sample_batch_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_config_v1) == 24, "gpu_config_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_dropped_v1) == 16, "gpu_dropped_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_pc_sample_batch_v1, cubin_crc) == 0, "cubin_crc leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_module_load_v1, cubin_crc) == 0, "cubin_crc leads");

#endif
