package linuxdrm

import (
	"fmt"

	"github.com/dpsoft/perf-agent/gpu"
)

func normalizeRecord(record rawRecord) (gpu.GPUTimelineEvent, error) {
	switch record.Kind {
	case recordKindIOCtl:
		device, attrs := classifyFileIdentity(record)
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
