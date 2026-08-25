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

// gpu_config_v1.vendor. Zero means the producer did not say, which is a
// different fact from "not NVIDIA" and must stay tellable apart.
#define GPU_VENDOR_UNKNOWN 0
#define GPU_VENDOR_NVIDIA  1
#define GPU_VENDOR_AMD     2

struct gpu_dropped_v1 {
    uint64_t count;
    uint8_t  klass;
    uint8_t  _pad[7];
};

// The classes gpu_dropped_v1.klass may carry.
//
// The record has been in this header since Phase 3 with no enum beside it and
// no probe on the wire; these are the first classes ever defined for it. The
// values are part of the wire contract: append only, never renumber. Zero is
// deliberately not a class -- a producer that memsets a record and forgets to
// set klass must not land on a real class and have its loss filed under
// somebody else's heading.
//
// The first three are PC sampling's losses, and they are three rather than
// one because the operator's action differs for each. HW backpressure says
// lower the sampling frequency; a full hardware buffer says raise
// HARDWARE_BUFFER_SIZE or drain more often; non-user-kernel samples say
// nothing is wrong at all and that much of the device's time simply cannot be
// attributed by this mechanism. Folding them together would produce one
// number that no action follows from.
//
// GPU_DROP_CLASS_PC_NON_USER_KERNEL is not loss the way the other two are --
// nothing was dropped, CUPTI never produced the records. It rides here anyway
// because it is the SIZE of a structural omission (see the header comment on
// gpu_pc_sample_batch_v1's emitter in the adapter), and the alternative was a
// new record for a diagnostic.
#define GPU_DROP_CLASS_INVALID            0
#define GPU_DROP_CLASS_PC_DROPPED_HW      1   // CUpti_PCSamplingData.droppedSamples
#define GPU_DROP_CLASS_PC_BUFFER_FULL     2   // ...hardwareBufferFull observations
#define GPU_DROP_CLASS_PC_NON_USER_KERNEL 3   // ...nonUsrKernelsTotalSamples
#define GPU_DROP_CLASS_GRAPH_EXEC         4   // executions launched from a CUDA graph
#define GPU_DROP_CLASS_MAX                4

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

// Longest stall-reason name carried inline, mirroring CUPTI's
// CUPTI_STALL_REASON_STRING_SIZE. GA102 reports 38 reasons whose names are
// far shorter, so truncation is not expected here — but name_len is
// authoritative and `truncated` is flagged per record rather than hidden,
// exactly as gpu_kernel_name_v1 does it.
#define GPU_STALL_NAME_MAX 128

// index -> name for the device's stall reasons. One-shot, emitted when
// sampling is first enabled and replayed on late attach, so a consumer that
// attaches mid-run still learns the table. The index is the vendor's own and
// is NOT stable across devices or driver versions, which is precisely why it
// must never escape into a label value un-resolved: the name is the only
// portable identity a stall reason has.
//
// Fixed-size by the ABI's rules (spec §6.3): the wire record does not shrink
// to fit a short name.
struct gpu_stall_reason_map_v1 {
    uint32_t index;
    uint16_t name_len;
    uint8_t  truncated;
    uint8_t  _pad;
    char     name[GPU_STALL_NAME_MAX];
};

// mode values for gpu_sampling_window_v1. They mirror the two collection
// tiers and are part of the wire contract, not an internal enum.
#define GPU_SAMPLING_MODE_CONTINUOUS        1
#define GPU_SAMPLING_MODE_KERNEL_SERIALIZED 2

// One PC-sampling burst: the interval over which PC sampling was enabled.
//
// This is Tier A's disclosure mechanism. In KERNEL_SERIALIZED mode kernels
// serialize while sampling is on, so every execution overlapping a window is
// perturbed and the profile must be able to say so rather than presenting a
// perturbed duration as an ordinary one.
//
// end_ns == 0 means the window was still OPEN when the producer stopped
// reporting — a hard exit mid-burst. It is not "zero length" and must never
// be read as one: an execution at or after such a window's start_ns is
// "unknown", never "not serialized". The ordinary teardown path closes the
// window with the exit timestamp, so a zero here is specifically the hard
// case.
struct gpu_sampling_window_v1 {
    uint64_t start_ns;
    uint64_t end_ns;
    uint8_t  mode;
    uint8_t  _pad[7];
};

GPU_STATIC_ASSERT(sizeof(struct gpu_launch_v1) == 48, "gpu_launch_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_exec_v1) == 48, "gpu_exec_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_module_load_v1) == 40, "gpu_module_load_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_pc_sample_batch_v1) == 40, "gpu_pc_sample_batch_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_config_v1) == 24, "gpu_config_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_dropped_v1) == 16, "gpu_dropped_v1 layout");
// Pinned for the first time now that the record is actually emitted and
// internal/gpuabi decodes it by hard-coded offset.
GPU_STATIC_ASSERT(offsetof(struct gpu_dropped_v1, count) == 0, "dropped count leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_dropped_v1, klass) == 8, "dropped klass position");
GPU_STATIC_ASSERT(GPU_DROP_CLASS_INVALID == 0, "class 0 is reserved for an unset klass");
GPU_STATIC_ASSERT(offsetof(struct gpu_pc_sample_batch_v1, cubin_crc) == 0, "cubin_crc leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_module_load_v1, cubin_crc) == 0, "cubin_crc leads");
GPU_STATIC_ASSERT(sizeof(struct gpu_launch_sampled_v1) == 56, "gpu_launch_sampled_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_kernel_name_v1) == 272, "gpu_kernel_name_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_launch_sampled_v1, sample_period) == 44, "sample_period position");

// The two probes added for PC sampling. Sizes AND offsets are asserted: a
// field that moved while the size held would decode as garbage at every
// consumer without any of them erroring, which is the failure mode the
// hard-coded offsets in internal/gpuabi make possible.
GPU_STATIC_ASSERT(sizeof(struct gpu_stall_reason_map_v1) == 136, "gpu_stall_reason_map_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_stall_reason_map_v1, index) == 0, "stall map index leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_stall_reason_map_v1, name_len) == 4, "stall map name_len position");
GPU_STATIC_ASSERT(offsetof(struct gpu_stall_reason_map_v1, truncated) == 6, "stall map truncated position");
GPU_STATIC_ASSERT(offsetof(struct gpu_stall_reason_map_v1, name) == 8, "stall map name position");
GPU_STATIC_ASSERT(sizeof(((struct gpu_stall_reason_map_v1 *)0)->name) == GPU_STALL_NAME_MAX,
                  "stall map name is a fixed GPU_STALL_NAME_MAX buffer");

GPU_STATIC_ASSERT(sizeof(struct gpu_sampling_window_v1) == 24, "gpu_sampling_window_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_sampling_window_v1, start_ns) == 0, "sampling window start_ns leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_sampling_window_v1, end_ns) == 8, "sampling window end_ns position");
GPU_STATIC_ASSERT(offsetof(struct gpu_sampling_window_v1, mode) == 16, "sampling window mode position");

// gpu_config_v1 has been in this header since Phase 3 with no consumer. Its
// offsets are pinned here for the first time now that internal/gpuabi decodes
// it by hard-coded offset.
GPU_STATIC_ASSERT(offsetof(struct gpu_config_v1, clock_hz) == 0, "config clock_hz leads");
GPU_STATIC_ASSERT(offsetof(struct gpu_config_v1, sampling_factor) == 8, "config sampling_factor position");
GPU_STATIC_ASSERT(offsetof(struct gpu_config_v1, sm_count) == 12, "config sm_count position");
GPU_STATIC_ASSERT(offsetof(struct gpu_config_v1, vendor) == 16, "config vendor position");

// Neither new record may exceed the largest record the BPF consumer sizes its
// reservation against. 136 <= 272, so MAX_RECORD_BYTES is unchanged and
// bpf/gpu_usdt.bpf.c's _Static_assert(MAX_RECORD_BYTES <= PAYLOAD_BYTES) still
// holds untouched.
GPU_STATIC_ASSERT(sizeof(struct gpu_stall_reason_map_v1) <= sizeof(struct gpu_kernel_name_v1),
                  "a record larger than gpu_kernel_name_v1 would raise MAX_RECORD_BYTES in bpf/gpu_usdt.bpf.c");

#endif
