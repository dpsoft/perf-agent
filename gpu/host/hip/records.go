package hip

import (
	"fmt"

	"github.com/dpsoft/perf-agent/gpu/host"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type rawRecord struct {
	PID          uint32
	TID          uint32
	TimeNs       uint64
	FunctionAddr uint64
	UserStackID  int32
	Stream       uint64
}

type recordDecoder struct {
	resolveKernel func(pid uint32, addr uint64) (string, bool)
	resolveStack  func(pid uint32, stackID int32) []pp.Frame
}

type launchRecord struct {
	Backend       string
	PID           uint32
	TID           uint32
	TimeNs        uint64
	CPUStack      []pp.Frame
	KernelName    string
	QueueID       string
	CorrelationID string
	Source        string
}

func (r launchRecord) toHostRecord() host.LaunchRecord {
	return host.LaunchRecord{
		Backend:       "hip",
		PID:           r.PID,
		TID:           r.TID,
		TimeNs:        r.TimeNs,
		CPUStack:      r.CPUStack,
		KernelName:    r.KernelName,
		QueueID:       r.QueueID,
		CorrelationID: r.CorrelationID,
		Source:        r.Source,
	}
}

func (d recordDecoder) decode(record rawRecord) (launchRecord, error) {
	if d.resolveKernel == nil {
		return launchRecord{}, fmt.Errorf("kernel resolver is required")
	}
	kernelName, ok := d.resolveKernel(record.PID, record.FunctionAddr)
	if !ok || kernelName == "" {
		return launchRecord{}, fmt.Errorf("resolve kernel %#x for pid %d", record.FunctionAddr, record.PID)
	}

	var stack []pp.Frame
	if d.resolveStack != nil && record.UserStackID >= 0 {
		stack = d.resolveStack(record.PID, record.UserStackID)
	}

	return launchRecord{
		Backend:       "hip",
		PID:           record.PID,
		TID:           record.TID,
		TimeNs:        record.TimeNs,
		CPUStack:      stack,
		KernelName:    kernelName,
		QueueID:       fmt.Sprintf("stream:%x", record.Stream),
		CorrelationID: fmt.Sprintf("hip:%d:%d:%d", record.PID, record.TID, record.TimeNs),
		Source:        "ebpf",
	}, nil
}
