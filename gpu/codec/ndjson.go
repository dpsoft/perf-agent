package codec

import (
	"encoding/json"
	"fmt"

	"github.com/dpsoft/perf-agent/gpu"
)

type EventKind string

const (
	KindLaunch  EventKind = "launch"
	KindExec    EventKind = "exec"
	KindCounter EventKind = "counter"
	KindSample  EventKind = "sample"
	KindEvent   EventKind = "event"
)

type DecodedEvent struct {
	Kind    EventKind
	Launch  gpu.GPUKernelLaunch
	Exec    gpu.GPUKernelExec
	Counter gpu.GPUCounterSample
	Sample  gpu.GPUSample
	Event   gpu.GPUTimelineEvent
}

type envelope struct {
	Kind EventKind `json:"kind"`
}

type timelineEventEnvelope struct {
	Kind  EventKind            `json:"kind"`
	Event gpu.GPUTimelineEvent `json:"event"`
}

func DecodeLine(line []byte) (DecodedEvent, error) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return DecodedEvent{}, fmt.Errorf("decode ndjson envelope: %w", err)
	}
	if env.Kind == "" {
		return DecodedEvent{}, fmt.Errorf("missing event kind")
	}

	out := DecodedEvent{Kind: env.Kind}
	switch env.Kind {
	case KindLaunch:
		if err := json.Unmarshal(line, &out.Launch); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode launch event: %w", err)
		}
		out.Launch.ClockDomain = gpu.NormalizeClockDomain(out.Launch.ClockDomain)
		if err := validateLaunch(out.Launch); err != nil {
			return DecodedEvent{}, err
		}
	case KindExec:
		if err := json.Unmarshal(line, &out.Exec); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode exec event: %w", err)
		}
		out.Exec.ClockDomain = gpu.NormalizeClockDomain(out.Exec.ClockDomain)
		if err := validateExec(out.Exec); err != nil {
			return DecodedEvent{}, err
		}
	case KindCounter:
		if err := json.Unmarshal(line, &out.Counter); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode counter event: %w", err)
		}
		out.Counter.ClockDomain = gpu.NormalizeClockDomain(out.Counter.ClockDomain)
		if err := validateCounter(out.Counter); err != nil {
			return DecodedEvent{}, err
		}
	case KindSample:
		if err := json.Unmarshal(line, &out.Sample); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode sample event: %w", err)
		}
		out.Sample.ClockDomain = gpu.NormalizeClockDomain(out.Sample.ClockDomain)
		if err := validateSample(out.Sample); err != nil {
			return DecodedEvent{}, err
		}
	case KindEvent:
		var eventLine timelineEventEnvelope
		if err := json.Unmarshal(line, &eventLine); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode timeline event: %w", err)
		}
		out.Event = eventLine.Event
		out.Event.ClockDomain = gpu.NormalizeClockDomain(out.Event.ClockDomain)
		if err := validateEvent(out.Event); err != nil {
			return DecodedEvent{}, err
		}
	default:
		return DecodedEvent{}, fmt.Errorf("unknown event kind %q", env.Kind)
	}

	return out, nil
}

func validateLaunch(event gpu.GPUKernelLaunch) error {
	if err := gpu.ValidateSupportedClockDomain(event.ClockDomain); err != nil {
		return fmt.Errorf("launch event %w", err)
	}
	if event.Correlation.Backend == "" || event.Correlation.Value == "" {
		return fmt.Errorf("launch event missing correlation")
	}
	if event.KernelName == "" {
		return fmt.Errorf("launch event missing kernel_name")
	}
	return nil
}

func validateExec(event gpu.GPUKernelExec) error {
	if err := gpu.ValidateSupportedClockDomain(event.ClockDomain); err != nil {
		return fmt.Errorf("exec event %w", err)
	}
	if event.Correlation.Backend == "" || event.Correlation.Value == "" {
		return fmt.Errorf("exec event missing correlation")
	}
	if event.KernelName == "" {
		return fmt.Errorf("exec event missing kernel_name")
	}
	return nil
}

func validateCounter(event gpu.GPUCounterSample) error {
	if err := gpu.ValidateSupportedClockDomain(event.ClockDomain); err != nil {
		return fmt.Errorf("counter event %w", err)
	}
	if event.Device.Backend == "" || event.Device.DeviceID == "" {
		return fmt.Errorf("counter event missing device")
	}
	if event.Name == "" {
		return fmt.Errorf("counter event missing name")
	}
	return nil
}

func validateSample(event gpu.GPUSample) error {
	if err := gpu.ValidateSupportedClockDomain(event.ClockDomain); err != nil {
		return fmt.Errorf("sample event %w", err)
	}
	if event.Correlation.Backend == "" || event.Correlation.Value == "" {
		return fmt.Errorf("sample event missing correlation")
	}
	if event.KernelName == "" {
		return fmt.Errorf("sample event missing kernel_name")
	}
	return nil
}

func validateEvent(event gpu.GPUTimelineEvent) error {
	if err := gpu.ValidateSupportedClockDomain(event.ClockDomain); err != nil {
		return fmt.Errorf("timeline event %w", err)
	}
	if event.Kind == "" {
		return fmt.Errorf("timeline event missing kind")
	}
	if event.Name == "" {
		return fmt.Errorf("timeline event missing name")
	}
	return nil
}
