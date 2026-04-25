package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpu/backend"
)

type Backend struct {
	path string
}

func New(path string) (*Backend, error) {
	if path == "" {
		return nil, fmt.Errorf("replay path is required")
	}
	return &Backend{path: path}, nil
}

func (b *Backend) ID() backend.GPUBackendID {
	return "replay"
}

func (b *Backend) Capabilities() []backend.GPUCapability {
	return []backend.GPUCapability{
		gpu.CapabilityLaunchTrace,
		gpu.CapabilityExecTimeline,
		gpu.CapabilityStallReasons,
	}
}

func (b *Backend) Start(_ context.Context, sink backend.EventSink) error {
	data, err := os.ReadFile(b.path)
	if err != nil {
		return fmt.Errorf("read replay fixture: %w", err)
	}
	var events []rawEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return fmt.Errorf("decode replay fixture: %w", err)
	}
	for _, event := range events {
		if err := emitEvent(event, sink); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Stop(context.Context) error { return nil }

func (b *Backend) Close() error { return nil }

type rawEvent struct {
	Kind        string            `json:"kind"`
	Correlation gpu.CorrelationID `json:"correlation"`
	Queue       gpu.GPUQueueRef   `json:"queue"`
	Launch      gpu.LaunchContext `json:"launch"`
	Device      gpu.GPUDeviceRef  `json:"device"`
	KernelName  string            `json:"kernel_name"`
	TimeNs      uint64            `json:"time_ns"`
	StartNs     uint64            `json:"start_ns"`
	EndNs       uint64            `json:"end_ns"`
	StallReason string            `json:"stall_reason"`
	Weight      uint64            `json:"weight"`
}

func emitEvent(event rawEvent, sink backend.EventSink) error {
	switch event.Kind {
	case "launch":
		sink.EmitLaunch(gpu.GPUKernelLaunch{
			Correlation: event.Correlation,
			Queue:       event.Queue,
			KernelName:  event.KernelName,
			TimeNs:      event.TimeNs,
			Launch:      event.Launch,
		})
	case "exec":
		sink.EmitExec(gpu.GPUKernelExec{
			Correlation: event.Correlation,
			Queue:       event.Queue,
			KernelName:  event.KernelName,
			StartNs:     event.StartNs,
			EndNs:       event.EndNs,
		})
	case "sample":
		sink.EmitSample(gpu.GPUSample{
			Correlation: event.Correlation,
			Device:      event.Device,
			TimeNs:      event.TimeNs,
			KernelName:  event.KernelName,
			StallReason: event.StallReason,
			Weight:      max(1, event.Weight),
		})
	default:
		return fmt.Errorf("unsupported replay event kind %q", event.Kind)
	}
	return nil
}
