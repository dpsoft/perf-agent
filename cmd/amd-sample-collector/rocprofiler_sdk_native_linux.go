//go:build linux && cgo

package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef int32_t perf_agent_rocprofiler_status_t;
typedef uint32_t perf_agent_rocprofiler_agent_version_t;

typedef struct {
	uint32_t major;
	uint32_t minor;
	uint32_t patch;
} perf_agent_rocprofiler_version_triplet_t;

typedef struct {
	uint64_t handle;
} perf_agent_rocprofiler_agent_id_t;

typedef struct {
	uint32_t x;
	uint32_t y;
	uint32_t z;
} perf_agent_rocprofiler_dim3_t;

typedef struct {
	uint8_t bytes[16];
} perf_agent_rocprofiler_uuid_t;

typedef struct {
	uint32_t hsa : 1;
	uint32_t hip : 1;
	uint32_t rccl : 1;
	uint32_t rocdecode : 1;
	uint32_t reserved : 28;
} perf_agent_rocprofiler_agent_runtime_visibility_t;

typedef union {
	uint32_t Value;
} perf_agent_hsa_engine_version_t;

typedef union {
	uint32_t Value;
} perf_agent_hsa_engine_id_t;

typedef union {
	uint32_t Value;
} perf_agent_hsa_capability_t;

typedef struct {
	uint64_t size;
	perf_agent_rocprofiler_agent_id_t id;
	int32_t type;
	uint32_t cpu_cores_count;
	uint32_t simd_count;
	uint32_t mem_banks_count;
	uint32_t caches_count;
	uint32_t io_links_count;
	uint32_t cpu_core_id_base;
	uint32_t simd_id_base;
	uint32_t max_waves_per_simd;
	uint32_t lds_size_in_kb;
	uint32_t gds_size_in_kb;
	uint32_t num_gws;
	uint32_t wave_front_size;
	uint32_t num_xcc;
	uint32_t cu_count;
	uint32_t array_count;
	uint32_t num_shader_banks;
	uint32_t simd_arrays_per_engine;
	uint32_t cu_per_simd_array;
	uint32_t simd_per_cu;
	uint32_t max_slots_scratch_cu;
	uint32_t gfx_target_version;
	uint16_t vendor_id;
	uint16_t device_id;
	uint32_t location_id;
	uint32_t domain;
	uint32_t drm_render_minor;
	uint32_t num_sdma_engines;
	uint32_t num_sdma_xgmi_engines;
	uint32_t num_sdma_queues_per_engine;
	uint32_t num_cp_queues;
	uint32_t max_engine_clk_ccompute;
	uint32_t max_engine_clk_fcompute;
	perf_agent_hsa_engine_version_t sdma_fw_version;
	perf_agent_hsa_engine_id_t fw_version;
	perf_agent_hsa_capability_t capability;
	uint32_t cu_per_engine;
	uint32_t max_waves_per_cu;
	uint32_t family_id;
	uint32_t workgroup_max_size;
	uint32_t grid_max_size;
	uint64_t local_mem_size;
	uint64_t hive_id;
	uint64_t gpu_id;
	perf_agent_rocprofiler_dim3_t workgroup_max_dim;
	perf_agent_rocprofiler_dim3_t grid_max_dim;
	const void* mem_banks;
	const void* caches;
	const void* io_links;
	const char* name;
	const char* vendor_name;
	const char* product_name;
	const char* model_name;
	uint32_t node_id;
	int32_t logical_node_id;
	int32_t logical_node_type_id;
	perf_agent_rocprofiler_agent_runtime_visibility_t runtime_visibility;
	perf_agent_rocprofiler_uuid_t uuid;
} perf_agent_rocprofiler_agent_v0_t;

typedef perf_agent_rocprofiler_status_t (*perf_agent_rocprofiler_get_version_triplet_fn_t)(perf_agent_rocprofiler_version_triplet_t*);
typedef perf_agent_rocprofiler_status_t (*perf_agent_rocprofiler_query_available_agents_cb_t)(perf_agent_rocprofiler_agent_version_t, const void**, size_t, void*);
typedef perf_agent_rocprofiler_status_t (*perf_agent_rocprofiler_query_available_agents_fn_t)(perf_agent_rocprofiler_agent_version_t, perf_agent_rocprofiler_query_available_agents_cb_t, size_t, void*);

typedef struct {
	size_t num_agents;
	uint16_t first_gpu_device_id;
	char first_gpu_name[128];
} perf_agent_native_agent_probe_t;

static perf_agent_rocprofiler_status_t
perf_agent_capture_agent_probe_cb(perf_agent_rocprofiler_agent_version_t version, const void** agents, size_t num_agents, void* user_data) {
	(void) version;
	perf_agent_native_agent_probe_t* out = (perf_agent_native_agent_probe_t*) user_data;
	if(!out) return -1;
	out->num_agents = num_agents;
	if(!agents) return 0;
	for(size_t i = 0; i < num_agents; ++i) {
		const perf_agent_rocprofiler_agent_v0_t* agent = (const perf_agent_rocprofiler_agent_v0_t*) agents[i];
		if(!agent) continue;
		if(agent->type != 2) continue;
		out->first_gpu_device_id = agent->device_id;
		const char* label = agent->product_name;
		if(!label || label[0] == '\0') label = agent->name;
		if(label) {
			strncpy(out->first_gpu_name, label, sizeof(out->first_gpu_name) - 1);
			out->first_gpu_name[sizeof(out->first_gpu_name) - 1] = '\0';
		}
		break;
	}
	return 0;
}

static perf_agent_rocprofiler_status_t
perf_agent_capture_agent_probe(perf_agent_rocprofiler_query_available_agents_fn_t fn, perf_agent_native_agent_probe_t* out) {
	if(!fn || !out) return -1;
	memset(out, 0, sizeof(*out));
	return fn(1u, perf_agent_capture_agent_probe_cb, sizeof(perf_agent_rocprofiler_agent_v0_t), out);
}

static perf_agent_rocprofiler_status_t
perf_agent_call_get_version_triplet(perf_agent_rocprofiler_get_version_triplet_fn_t fn, perf_agent_rocprofiler_version_triplet_t* out) {
	if(!fn || !out) return -1;
	return fn(out);
}
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

type rocprofilerSDKNativeProbe struct {
	Major            uint32
	Minor            uint32
	Patch            uint32
	NumAgents        int
	FirstGPUDeviceID uint16
	FirstGPUName     string
}

func runRocprofilerSDKNative(cfg collectorConfig) error {
	probe, err := probeRocprofilerSDKNative(cfg.rocprofilerSDKLibrary)
	if err != nil {
		return err
	}
	return emitRocprofilerSDKNativeProbe(cfg, probe)
}

func probeRocprofilerSDKNative(path string) (rocprofilerSDKNativeProbe, error) {
	handle, err := openSharedLibrary(path)
	if err != nil {
		return rocprofilerSDKNativeProbe{}, fmt.Errorf("load rocprofiler-sdk native library: %w; install ROCprofiler-SDK (rocm-runtime alone is not enough)", err)
	}
	defer C.dlclose(handle)

	versionSym, err := lookupSharedLibrarySymbol(handle, "rocprofiler_get_version_triplet")
	if err != nil {
		return rocprofilerSDKNativeProbe{}, fmt.Errorf("resolve rocprofiler-sdk native symbol rocprofiler_get_version_triplet: %w", err)
	}
	agentsSym, err := lookupSharedLibrarySymbol(handle, "rocprofiler_query_available_agents")
	if err != nil {
		return rocprofilerSDKNativeProbe{}, fmt.Errorf("resolve rocprofiler-sdk native symbol rocprofiler_query_available_agents: %w", err)
	}

	versionFn := C.perf_agent_rocprofiler_get_version_triplet_fn_t(versionSym)
	queryAgentsFn := C.perf_agent_rocprofiler_query_available_agents_fn_t(agentsSym)

	var version C.perf_agent_rocprofiler_version_triplet_t
	if status := C.perf_agent_rocprofiler_status_t(C.perf_agent_call_get_version_triplet(versionFn, &version)); status != 0 {
		return rocprofilerSDKNativeProbe{}, fmt.Errorf("rocprofiler_get_version_triplet failed with status %d", int(status))
	}

	var agents C.perf_agent_native_agent_probe_t
	if status := C.perf_agent_rocprofiler_status_t(C.perf_agent_capture_agent_probe(queryAgentsFn, &agents)); status != 0 {
		return rocprofilerSDKNativeProbe{}, fmt.Errorf("rocprofiler_query_available_agents failed with status %d", int(status))
	}

	return rocprofilerSDKNativeProbe{
		Major:            uint32(version.major),
		Minor:            uint32(version.minor),
		Patch:            uint32(version.patch),
		NumAgents:        int(agents.num_agents),
		FirstGPUDeviceID: uint16(agents.first_gpu_device_id),
		FirstGPUName:     C.GoString(&agents.first_gpu_name[0]),
	}, nil
}

func emitRocprofilerSDKNativeProbe(cfg collectorConfig, probe rocprofilerSDKNativeProbe) error {
	startNS, sample1NS, sample2NS, endNS, _, err := collectionWindow()
	if err != nil {
		return err
	}

	if probe.FirstGPUName != "" && cfg.deviceName == defaultDeviceName {
		cfg.deviceName = probe.FirstGPUName
	}
	if probe.FirstGPUDeviceID != 0 && cfg.deviceID == defaultDeviceID {
		cfg.deviceID = fmt.Sprintf("amd:0x%04x", probe.FirstGPUDeviceID)
	}

	contextID := "ctx0"
	execID := fmt.Sprintf("rocprofiler-sdk-native:%d", startNS)
	if hipPID := envOrDefault("PERF_AGENT_HIP_PID", ""); hipPID != "" {
		contextID = fmt.Sprintf("pid-%s", hipPID)
		execID = fmt.Sprintf("rocprofiler-sdk-native:%s:%d", hipPID, startNS)
	}

	dev := device{
		Backend:  "amdsample",
		DeviceID: cfg.deviceID,
		Name:     cfg.deviceName,
	}
	q := queue{
		Backend: "amdsample",
		Device:  dev,
		QueueID: cfg.queueID,
	}

	if err := writeJSONLine(execRecord{
		Kind: "exec",
		Execution: execution{
			Backend:   "amdsample",
			DeviceID:  cfg.deviceID,
			QueueID:   cfg.queueID,
			ContextID: contextID,
			ExecID:    execID,
		},
		Correlation: correlation{Backend: "amdsample", Value: execID},
		Queue:       q,
		KernelName:  cfg.kernelName,
		StartNS:     startNS,
		EndNS:       endNS,
	}); err != nil {
		return fmt.Errorf("write native exec record: %w", err)
	}

	versionValue := int(probe.Major)*10000 + int(probe.Minor)*100 + int(probe.Patch)
	if err := writeJSONLine(sampleRecord{
		Kind:         "sample",
		Correlation:  correlation{Backend: "amdsample", Value: fmt.Sprintf("native-sdk-version:%d", sample1NS)},
		Device:       dev,
		TimeNS:       sample1NS,
		KernelName:   cfg.kernelName,
		StallReason:  "native_sdk_version",
		SampleWeight: versionValue,
	}); err != nil {
		return fmt.Errorf("write native version sample: %w", err)
	}

	if err := writeJSONLine(sampleRecord{
		Kind:         "sample",
		Correlation:  correlation{Backend: "amdsample", Value: fmt.Sprintf("native-sdk-agents:%d", sample2NS)},
		Device:       dev,
		TimeNS:       sample2NS,
		KernelName:   cfg.kernelName,
		StallReason:  "native_sdk_available_agents",
		SampleWeight: probe.NumAgents,
	}); err != nil {
		return fmt.Errorf("write native agent sample: %w", err)
	}

	return nil
}

func openSharedLibrary(path string) (unsafe.Pointer, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	handle := C.dlopen(cpath, C.RTLD_NOW|C.RTLD_LOCAL)
	if handle == nil {
		if msg := C.dlerror(); msg != nil {
			return nil, fmt.Errorf("%s", C.GoString(msg))
		}
		return nil, fmt.Errorf("unknown dlopen failure")
	}
	return handle, nil
}

func lookupSharedLibrarySymbol(handle unsafe.Pointer, symbol string) (unsafe.Pointer, error) {
	csym := C.CString(symbol)
	defer C.free(unsafe.Pointer(csym))

	C.dlerror()
	ptr := C.dlsym(handle, csym)
	if ptr == nil {
		if msg := C.dlerror(); msg != nil {
			return nil, fmt.Errorf("%s", C.GoString(msg))
		}
		return nil, fmt.Errorf("symbol not found")
	}
	return ptr, nil
}

func _() {
	_ = time.Nanosecond
}
