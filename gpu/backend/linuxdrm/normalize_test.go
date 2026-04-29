package linuxdrm

import (
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

func TestNormalizeRecord(t *testing.T) {
	event, err := normalizeRecordWithResolvers(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     0xc04064,
		ResultCode:  -11,
		StartNs:     1000,
		EndNs:       1200,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
		CgroupID:    4242,
	}, lookupDRMDeviceInfo, func(uint32) (string, bool) {
		return "/kubepods.slice/pod-abc/container-def", true
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Backend != "linuxdrm" {
		t.Fatalf("backend=%q", event.Backend)
	}
	if event.Kind != "ioctl" {
		t.Fatalf("kind=%q", event.Kind)
	}
	if event.Name != "drm-render-ioctl" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.TimeNs != 1000 || event.DurationNs != 200 {
		t.Fatalf("timing=%#v", event)
	}
	if event.Source != "ebpf" || event.Confidence != "exact" {
		t.Fatalf("provenance=%#v", event)
	}
	if event.ContextID != "" || event.Queue != nil {
		t.Fatalf("expected unavailable queue/context fields: %#v", event)
	}
	if event.Device == nil {
		t.Fatalf("expected classified device: %#v", event)
	}
	if got := event.Attributes["device_id"]; got != "226:128:77" {
		t.Fatalf("device_id attr=%q", got)
	}
	if got := event.Attributes["node_class"]; got != "render" {
		t.Fatalf("node_class=%q", got)
	}
	if got := event.Attributes["command_hex"]; got != "0xc04064" {
		t.Fatalf("command_hex=%q", got)
	}
	if got := event.Attributes["ioctl_type_char"]; got != "@" {
		t.Fatalf("ioctl_type_char=%q", got)
	}
	if got := event.Attributes["cgroup_id"]; got != "4242" {
		t.Fatalf("cgroup_id=%q", got)
	}
	if got := event.Attributes["cgroup_path"]; got != "/kubepods.slice/pod-abc/container-def" {
		t.Fatalf("cgroup_path=%q", got)
	}
}

func TestNormalizeRecordAddsDerivedKubernetesCgroupAttrs(t *testing.T) {
	event, err := normalizeRecordWithResolvers(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     0xc04064,
		ResultCode:  -11,
		StartNs:     1000,
		EndNs:       1200,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
		CgroupID:    4242,
	}, lookupDRMDeviceInfo, func(uint32) (string, bool) {
		return "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod2af2f6f1_1111_2222_3333_444444444444.slice/cri-containerd-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.scope", true
	})
	if err != nil {
		t.Fatalf("normalizeRecordWithResolvers: %v", err)
	}

	if got := event.Attributes["pod_uid"]; got != "2af2f6f1-1111-2222-3333-444444444444" {
		t.Fatalf("pod_uid=%q", got)
	}
	if got := event.Attributes["container_runtime"]; got != "containerd" {
		t.Fatalf("container_runtime=%q", got)
	}
	if got := event.Attributes["container_id"]; got != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("container_id=%q", got)
	}
}

func TestNormalizeRecordRejectsUnknownKind(t *testing.T) {
	if _, err := normalizeRecord(rawRecord{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeSchedRunqRecord(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:     recordKindSchedRunq,
		PID:      123,
		TID:      124,
		StartNs:  1000,
		EndNs:    1150,
		CPU:      7,
		AuxNs:    150,
		CgroupID: 99,
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Kind != gpu.TimelineEventWait {
		t.Fatalf("kind=%q", event.Kind)
	}
	if event.Name != "sched-runq-latency" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.TimeNs != 1000 || event.DurationNs != 150 {
		t.Fatalf("timing=%#v", event)
	}
	if got := event.Attributes["cpu"]; got != "7" {
		t.Fatalf("cpu=%q", got)
	}
	if got := event.Attributes["cgroup_id"]; got != "99" {
		t.Fatalf("cgroup_id=%q", got)
	}
}

func TestNormalizeSchedWakeupRecord(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:     recordKindSchedWakeup,
		PID:      123,
		TID:      124,
		StartNs:  1000,
		CPU:      5,
		CgroupID: 7,
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Kind != gpu.TimelineEventWait {
		t.Fatalf("kind=%q", event.Kind)
	}
	if event.Name != "sched-wakeup" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.DurationNs != 0 {
		t.Fatalf("duration=%d", event.DurationNs)
	}
	if got := event.Attributes["cpu"]; got != "5" {
		t.Fatalf("cpu=%q", got)
	}
	if got := event.Attributes["cgroup_id"]; got != "7" {
		t.Fatalf("cgroup_id=%q", got)
	}
}

func TestNormalizeRecordClassifiesDRMSyncobjWait(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 32, 'd', 0xc3),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Name != "drm-syncobj-wait" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventWait {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_family"]; got != "drm-core" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["command_name"]; got != "syncobj_wait" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "sync-wait" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordClassifiesDRMPrimeImport(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 32, 'd', 0x2e),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Name != "drm-prime-fd-to-handle" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventMemory {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["semantic"]; got != "prime-import" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordClassifiesDRMGEMCloseAsMemory(t *testing.T) {
	event, err := normalizeRecord(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 32, 'd', 0x09),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	})
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Name != "drm-gem-close" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventMemory {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["semantic"]; got != "memory-release" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordBucketsDRMDriverCommands(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 64, 'd', 0x40),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeRecord: %v", err)
	}

	if event.Name != "drm-driver-ioctl" {
		t.Fatalf("name=%q", event.Name)
	}
	if got := event.Attributes["command_family"]; got != "drm-driver" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "driver-command" {
		t.Fatalf("semantic=%q", got)
	}
	if got := event.Attributes["drm_command_index"]; got != "0" {
		t.Fatalf("drm_command_index=%q", got)
	}
}

func TestNormalizeRecordClassifiesAMDGPUCommandSubmission(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 64, 'd', 0x44),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	}, func(uint32, uint32) (drmDeviceInfo, bool) {
		return drmDeviceInfo{Driver: "amdgpu", Node: "renderD128"}, true
	})
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "amdgpu-cs" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventSubmit {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_family"]; got != "amdgpu" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["command_name"]; got != "cs" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "command-submit" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordClassifiesAMDGPUWaitFences(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 64, 'd', 0x52),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	}, func(uint32, uint32) (drmDeviceInfo, bool) {
		return drmDeviceInfo{Driver: "amdgpu", Node: "renderD128"}, true
	})
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "amdgpu-wait-fences" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventWait {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_name"]; got != "wait_fences" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "sync-wait" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordClassifiesAMDGPUBufferCreationAsMemory(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 64, 'd', 0x40),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	}, func(uint32, uint32) (drmDeviceInfo, bool) {
		return drmDeviceInfo{Driver: "amdgpu", Node: "renderD128"}, true
	})
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "amdgpu-gem-create" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventMemory {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_name"]; got != "gem_create" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "memory-create" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordLeavesUnknownAMDGPUDriverCommandBucketed(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          9,
		Command:     encodeTestIOCtl(3, 64, 'd', 0x59),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 226,
		DeviceMinor: 128,
		Inode:       77,
	}, func(uint32, uint32) (drmDeviceInfo, bool) {
		return drmDeviceInfo{Driver: "amdgpu", Node: "renderD128"}, true
	})
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "drm-driver-ioctl" {
		t.Fatalf("name=%q", event.Name)
	}
	if got := event.Attributes["command_family"]; got != "drm-driver" {
		t.Fatalf("command_family=%q", got)
	}
}

func TestNormalizeRecordClassifiesKFDFreeMemoryAsMemory(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          3,
		Command:     encodeTestIOCtl(1, 8, 'K', 0x17),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 235,
		DeviceMinor: 0,
		Inode:       527,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "kfd-free-memory-of-gpu" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventMemory {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_family"]; got != "kfd" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["command_name"]; got != "free_memory_of_gpu" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "memory-release" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordClassifiesKFDUnmapMemoryAsMemory(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          3,
		Command:     encodeTestIOCtl(3, 24, 'K', 0x19),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 235,
		DeviceMinor: 0,
		Inode:       527,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "kfd-unmap-memory-from-gpu" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventMemory {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_family"]; got != "kfd" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["command_name"]; got != "unmap_memory_from_gpu" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "memory-unmap" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordClassifiesKFDWaitEventsAsWait(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          3,
		Command:     encodeTestIOCtl(3, 16, 'K', 0x0c),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 235,
		DeviceMinor: 0,
		Inode:       527,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "kfd-wait-events" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventWait {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_family"]; got != "kfd" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["command_name"]; got != "wait_events" {
		t.Fatalf("command_name=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "sync-wait" {
		t.Fatalf("semantic=%q", got)
	}
}

func TestNormalizeRecordBucketsUnknownKFDCommand(t *testing.T) {
	event, err := normalizeRecordWithLookup(rawRecord{
		Kind:        recordKindIOCtl,
		PID:         123,
		TID:         124,
		FD:          3,
		Command:     encodeTestIOCtl(3, 8, 'K', 0x26),
		ResultCode:  0,
		StartNs:     1000,
		EndNs:       1300,
		DeviceMajor: 235,
		DeviceMinor: 0,
		Inode:       527,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeRecordWithLookup: %v", err)
	}

	if event.Name != "kfd-ioctl" {
		t.Fatalf("name=%q", event.Name)
	}
	if event.Kind != gpu.TimelineEventIOCtl {
		t.Fatalf("kind=%q", event.Kind)
	}
	if got := event.Attributes["command_family"]; got != "kfd" {
		t.Fatalf("command_family=%q", got)
	}
	if got := event.Attributes["semantic"]; got != "compute-ioctl" {
		t.Fatalf("semantic=%q", got)
	}
}

func encodeTestIOCtl(dir, size, typ, nr uint64) uint64 {
	return (dir << iocDirShift) | (size << iocSizeShift) | (typ << iocTypeShift) | (nr << iocNRShift)
}
