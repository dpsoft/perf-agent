package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dpsoft/perf-agent/gpu"
)

type Backend struct {
	path string
}

type fixture struct {
	Version int        `json:"version"`
	Events  []rawEvent `json:"events"`
}

func New(path string) (*Backend, error) {
	if path == "" {
		return nil, fmt.Errorf("replay path is required")
	}
	return &Backend{path: path}, nil
}

func (b *Backend) ID() gpu.GPUBackendID {
	return gpu.BackendReplay
}

func (b *Backend) EventBackends() []gpu.GPUBackendID { return nil }

func (b *Backend) Capabilities() []gpu.GPUCapability {
	return []gpu.GPUCapability{
		gpu.CapabilityLaunchTrace,
		gpu.CapabilityExecTimeline,
		gpu.CapabilityStallReasons,
	}
}

func (b *Backend) Start(_ context.Context, sink gpu.EventSink) error {
	data, err := os.ReadFile(b.path)
	if err != nil {
		return fmt.Errorf("read replay fixture: %w", err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("decode versioned replay fixture: %w", err)
	}
	if f.Version == 0 {
		return fmt.Errorf("decode versioned replay fixture: missing versioned replay fixture envelope")
	}
	if f.Version != 1 {
		return fmt.Errorf("decode versioned replay fixture: unsupported replay fixture version %d", f.Version)
	}
	for _, event := range f.Events {
		if err := emitEvent(event, sink); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Stop(context.Context) error { return nil }

func (b *Backend) Close() error { return nil }

type rawEvent struct {
	Kind        string               `json:"kind"`
	Event       gpu.GPUTimelineEvent `json:"event"`
	Correlation gpu.CorrelationID    `json:"correlation"`
	Execution   gpu.GPUExecutionRef  `json:"execution"`
	Queue       gpu.GPUQueueRef      `json:"queue"`
	Launch      gpu.LaunchContext    `json:"launch"`
	Device      gpu.GPUDeviceRef     `json:"device"`
	KernelName  string               `json:"kernel_name"`
	TimeNs      uint64               `json:"time_ns"`
	StartNs     uint64               `json:"start_ns"`
	EndNs       uint64               `json:"end_ns"`
	StallReason string               `json:"stall_reason"`
	Weight      uint64               `json:"weight"`
	ClockDomain gpu.ClockDomain      `json:"clock_domain,omitempty"`
}

func emitEvent(event rawEvent, sink gpu.EventSink) error {
	switch event.Kind {
	case "launch":
		domain := gpu.NormalizeClockDomain(event.ClockDomain)
		if err := gpu.ValidateSupportedClockDomain(domain); err != nil {
			return fmt.Errorf("replay launch event %w", err)
		}
		sink.EmitLaunch(gpu.GPUKernelLaunch{
			Correlation: event.Correlation,
			Queue:       event.Queue,
			KernelName:  event.KernelName,
			ClockDomain: domain,
			TimeNs:      event.TimeNs,
			Launch:      event.Launch,
		})
	case "exec":
		domain := gpu.NormalizeClockDomain(event.ClockDomain)
		if err := gpu.ValidateSupportedClockDomain(domain); err != nil {
			return fmt.Errorf("replay exec event %w", err)
		}
		sink.EmitExec(gpu.GPUKernelExec{
			Execution:   event.Execution,
			Correlation: event.Correlation,
			Queue:       event.Queue,
			KernelName:  event.KernelName,
			ClockDomain: domain,
			StartNs:     event.StartNs,
			EndNs:       event.EndNs,
		})
	case "sample":
		domain := gpu.NormalizeClockDomain(event.ClockDomain)
		if err := gpu.ValidateSupportedClockDomain(domain); err != nil {
			return fmt.Errorf("replay sample event %w", err)
		}
		sink.EmitSample(gpu.GPUSample{
			Correlation: event.Correlation,
			Device:      event.Device,
			ClockDomain: domain,
			TimeNs:      event.TimeNs,
			KernelName:  event.KernelName,
			StallReason: event.StallReason,
			Weight:      max(1, event.Weight),
		})
	case "event":
		event.Event.ClockDomain = gpu.NormalizeClockDomain(event.Event.ClockDomain)
		if err := gpu.ValidateSupportedClockDomain(event.Event.ClockDomain); err != nil {
			return fmt.Errorf("replay timeline event %w", err)
		}
		sink.EmitEvent(event.Event)
	default:
		return fmt.Errorf("unsupported replay event kind %q", event.Kind)
	}
	return nil
}
