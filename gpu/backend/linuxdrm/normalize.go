package linuxdrm

import (
	"fmt"
	"strconv"

	"github.com/dpsoft/perf-agent/gpu"
)

func normalizeRecord(record rawRecord) (gpu.GPUTimelineEvent, error) {
	switch record.Kind {
	case recordKindIOCtl:
		device, attrs := classifyFileIdentity(record)
		attrs["command"] = strconv.FormatUint(record.Command, 10)
		event := gpu.GPUTimelineEvent{
			Backend:    "linuxdrm",
			Kind:       gpu.TimelineEventIOCtl,
			Name:       "ioctl",
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
