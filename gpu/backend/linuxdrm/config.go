package linuxdrm

import (
	"context"
	"errors"

	"github.com/dpsoft/perf-agent/gpu"
)

var errPIDRequired = errors.New("pid is required")

type Config struct {
	PID int

	testRun     func(context.Context, gpu.EventSink) error
	testRecords []rawRecord
}

func (c Config) validate() error {
	if c.PID <= 0 {
		return errPIDRequired
	}
	return nil
}
