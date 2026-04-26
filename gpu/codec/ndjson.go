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
)

type DecodedEvent struct {
	Kind    EventKind
	Launch  gpu.GPUKernelLaunch
	Exec    gpu.GPUKernelExec
	Counter gpu.GPUCounterSample
	Sample  gpu.GPUSample
}

type envelope struct {
	Kind EventKind `json:"kind"`
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
		if err := validateLaunch(out.Launch); err != nil {
			return DecodedEvent{}, err
		}
	case KindExec:
		if err := json.Unmarshal(line, &out.Exec); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode exec event: %w", err)
		}
		if err := validateExec(out.Exec); err != nil {
			return DecodedEvent{}, err
		}
	case KindCounter:
		if err := json.Unmarshal(line, &out.Counter); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode counter event: %w", err)
		}
		if err := validateCounter(out.Counter); err != nil {
			return DecodedEvent{}, err
		}
	case KindSample:
		if err := json.Unmarshal(line, &out.Sample); err != nil {
			return DecodedEvent{}, fmt.Errorf("decode sample event: %w", err)
		}
		if err := validateSample(out.Sample); err != nil {
			return DecodedEvent{}, err
		}
	default:
		return DecodedEvent{}, fmt.Errorf("unknown event kind %q", env.Kind)
	}

	return out, nil
}

func validateLaunch(event gpu.GPUKernelLaunch) error {
	if event.Correlation.Backend == "" || event.Correlation.Value == "" {
		return fmt.Errorf("launch event missing correlation")
	}
	if event.KernelName == "" {
		return fmt.Errorf("launch event missing kernel_name")
	}
	return nil
}

func validateExec(event gpu.GPUKernelExec) error {
	if event.Correlation.Backend == "" || event.Correlation.Value == "" {
		return fmt.Errorf("exec event missing correlation")
	}
	if event.KernelName == "" {
		return fmt.Errorf("exec event missing kernel_name")
	}
	return nil
}

func validateCounter(event gpu.GPUCounterSample) error {
	if event.Device.Backend == "" || event.Device.DeviceID == "" {
		return fmt.Errorf("counter event missing device")
	}
	if event.Name == "" {
		return fmt.Errorf("counter event missing name")
	}
	return nil
}

func validateSample(event gpu.GPUSample) error {
	if event.Correlation.Backend == "" || event.Correlation.Value == "" {
		return fmt.Errorf("sample event missing correlation")
	}
	if event.KernelName == "" {
		return fmt.Errorf("sample event missing kernel_name")
	}
	return nil
}
