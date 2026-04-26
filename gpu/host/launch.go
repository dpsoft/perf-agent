package host

import (
	"fmt"
	"maps"
	"slices"

	"github.com/dpsoft/perf-agent/gpu"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type LaunchRecord struct {
	Backend       gpu.GPUBackendID   `json:"backend"`
	PID           uint32             `json:"pid"`
	TID           uint32             `json:"tid"`
	TimeNs        uint64             `json:"time_ns"`
	CPUStack      []pp.Frame         `json:"cpu_stack"`
	KernelName    string             `json:"kernel_name"`
	QueueID       string             `json:"queue_id"`
	ContextID     string             `json:"context_id"`
	CorrelationID string             `json:"correlation_id"`
	Tags          map[string]string  `json:"tags"`
	Source        string             `json:"source"`
}

type launchSink struct {
	sink gpu.EventSink
}

func NormalizeLaunch(record LaunchRecord) (gpu.GPUKernelLaunch, error) {
	if record.Backend == "" {
		return gpu.GPUKernelLaunch{}, fmt.Errorf("launch record missing backend")
	}
	if record.CorrelationID == "" {
		return gpu.GPUKernelLaunch{}, fmt.Errorf("launch record missing correlation_id")
	}
	if record.KernelName == "" {
		return gpu.GPUKernelLaunch{}, fmt.Errorf("launch record missing kernel_name")
	}

	return gpu.GPUKernelLaunch{
		Correlation: gpu.CorrelationID{
			Backend: record.Backend,
			Value:   record.CorrelationID,
		},
		Queue: gpu.GPUQueueRef{
			Backend: record.Backend,
			QueueID: record.QueueID,
		},
		KernelName: record.KernelName,
		TimeNs:     record.TimeNs,
		Launch: gpu.LaunchContext{
			PID:      record.PID,
			TID:      record.TID,
			TimeNs:   record.TimeNs,
			CPUStack: slices.Clone(record.CPUStack),
			Tags:     maps.Clone(record.Tags),
		},
	}, nil
}

func NewLaunchSink(sink gpu.EventSink) HostSink {
	return launchSink{sink: sink}
}

func (s launchSink) EmitLaunchRecord(record LaunchRecord) error {
	launch, err := NormalizeLaunch(record)
	if err != nil {
		return err
	}
	s.sink.EmitLaunch(launch)
	return nil
}
