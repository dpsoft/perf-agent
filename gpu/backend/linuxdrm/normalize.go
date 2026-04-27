package linuxdrm

import (
	"fmt"
	"strconv"

	"github.com/dpsoft/perf-agent/gpu"
)

func normalizeRecord(record rawRecord) (gpu.GPUTimelineEvent, error) {
	return normalizeRecordWithLookup(record, lookupDRMDeviceInfo)
}

func normalizeRecordWithLookup(record rawRecord, lookup func(uint32, uint32) (drmDeviceInfo, bool)) (gpu.GPUTimelineEvent, error) {
	switch record.Kind {
	case recordKindIOCtl:
		device, attrs := classifyFileIdentityWithLookup(record, lookup)
		for key, value := range ioctlAttributes(record.Command) {
			attrs[key] = value
		}
		name := "ioctl"
		switch attrs["node_class"] {
		case "render":
			name = "drm-render-ioctl"
		case "card":
			name = "drm-card-ioctl"
		}
		driver := attrs["driver"]
		if classification, ok := classifyIOCtlForDriver(record.Command, driver); ok {
			name = classification.Name
			for key, value := range classification.Attributes {
				attrs[key] = value
			}
		}
		event := gpu.GPUTimelineEvent{
			Backend:    "linuxdrm",
			Kind:       gpu.TimelineEventIOCtl,
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
		return gpu.GPUTimelineEvent{
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
		}, nil
	case recordKindSchedRunq:
		return gpu.GPUTimelineEvent{
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
		}, nil
	default:
		return gpu.GPUTimelineEvent{}, fmt.Errorf("unsupported record kind %d", record.Kind)
	}
}

func duration(start, end uint64) uint64 {
	if end < start {
		return 0
	}
	return end - start
}
