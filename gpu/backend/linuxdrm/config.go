package linuxdrm

import (
	"context"
	"errors"

	"github.com/dpsoft/perf-agent/gpu"
)

var errPIDRequired = errors.New("pid is required")

type Config struct {
	PID int

	EventBackends []gpu.GPUBackendID

	testRun     func(context.Context, gpu.EventSink) error
	testRecords []rawRecord
}

func (c Config) validate() error {
	if c.PID <= 0 {
		return errPIDRequired
	}
	for _, backend := range c.EventBackends {
		switch backend {
		case gpu.BackendLinuxDRM, gpu.BackendLinuxKFD:
		default:
			return errors.New("unsupported linux event backend")
		}
	}
	return nil
}

func (c Config) configuredEventBackends() []gpu.GPUBackendID {
	if len(c.EventBackends) != 0 {
		return append([]gpu.GPUBackendID(nil), c.EventBackends...)
	}
	return []gpu.GPUBackendID{gpu.BackendLinuxDRM, gpu.BackendLinuxKFD}
}
