package linuxdrm

import (
	"fmt"
	"strconv"

	"github.com/dpsoft/perf-agent/gpu"
)

func normalizeRecord(record rawRecord) (gpu.GPUTimelineEvent, error) {
	return normalizeRecordWithResolvers(record, lookupDRMDeviceInfo, lookupCgroupPath)
}

func normalizeRecordWithLookup(record rawRecord, lookup func(uint32, uint32) (drmDeviceInfo, bool)) (gpu.GPUTimelineEvent, error) {
	return normalizeRecordWithResolvers(record, lookup, lookupCgroupPath)
}

func normalizeRecordWithResolvers(
	record rawRecord,
	lookupDevice func(uint32, uint32) (drmDeviceInfo, bool),
	lookupCgroup func(uint32) (string, bool),
) (gpu.GPUTimelineEvent, error) {
	switch record.Kind {
	case recordKindIOCtl:
		device, attrs := classifyFileIdentityWithLookup(record, lookupDevice)
		addAttributionAttrs(attrs, record, lookupCgroup)
		for key, value := range ioctlAttributes(record.Command) {
			attrs[key] = value
		}
		name := "ioctl"
		kind := gpu.TimelineEventIOCtl
		switch attrs["node_class"] {
		case "render":
			name = "drm-render-ioctl"
		case "card":
			name = "drm-card-ioctl"
		}
		driver := attrs["driver"]
		if classification, ok := classifyIOCtlForDriver(record.Command, driver); ok {
			name = classification.Name
			if classification.Kind != "" {
				kind = classification.Kind
			}
			for key, value := range classification.Attributes {
				attrs[key] = value
			}
		}
		event := gpu.GPUTimelineEvent{
			Backend:    "linuxdrm",
			Kind:       kind,
			Name:       name,
			TimeNs:     record.StartNs,
			DurationNs: duration(record.StartNs, record.EndNs),
			PID:        record.PID,
			TID:        record.TID,
			FD:         record.FD,
			ResultCode: record.ResultCode,
			Device:     device,
			Source:     "ebpf",
			Confidence: "exact",
			Attributes: attrs,
		}
		return event, nil
	case recordKindSchedWakeup:
		event := gpu.GPUTimelineEvent{
			Backend:    "linuxdrm",
			Kind:       gpu.TimelineEventWait,
			Name:       "sched-wakeup",
			TimeNs:     record.StartNs,
			PID:        record.PID,
			TID:        record.TID,
			Source:     "ebpf",
			Confidence: "exact",
			Attributes: map[string]string{
				"cpu": strconv.FormatUint(uint64(record.CPU), 10),
			},
		}
		addAttributionAttrs(event.Attributes, record, lookupCgroup)
		return event, nil
	case recordKindSchedRunq:
		event := gpu.GPUTimelineEvent{
			Backend:    "linuxdrm",
			Kind:       gpu.TimelineEventWait,
			Name:       "sched-runq-latency",
			TimeNs:     record.StartNs,
			DurationNs: record.AuxNs,
			PID:        record.PID,
			TID:        record.TID,
			Source:     "ebpf",
			Confidence: "exact",
			Attributes: map[string]string{
				"cpu": strconv.FormatUint(uint64(record.CPU), 10),
			},
		}
		addAttributionAttrs(event.Attributes, record, lookupCgroup)
		return event, nil
	default:
		return gpu.GPUTimelineEvent{}, fmt.Errorf("unsupported record kind %d", record.Kind)
	}
}

func addAttributionAttrs(attrs map[string]string, record rawRecord, lookupCgroup func(uint32) (string, bool)) {
	if record.CgroupID != 0 {
		attrs["cgroup_id"] = strconv.FormatUint(record.CgroupID, 10)
	}
	if lookupCgroup != nil {
		if path, ok := lookupCgroup(record.PID); ok {
			attrs["cgroup_path"] = path
		}
	}
}

func duration(start, end uint64) uint64 {
	if end < start {
		return 0
	}
	return end - start
}
