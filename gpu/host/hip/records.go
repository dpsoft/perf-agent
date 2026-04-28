package hip

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/dpsoft/perf-agent/gpu/host"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type rawRecord struct {
	PID          uint32
	TID          uint32
	TimeNs       uint64
	FunctionAddr uint64
	UserStackID  int32
	Pad0         uint32
	Stream       uint64
	CgroupID     uint64
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
	Tags          map[string]string
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
		Tags:          r.Tags,
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
	var tags map[string]string
	if record.CgroupID != 0 {
		tags = map[string]string{
			"cgroup_id": strconv.FormatUint(record.CgroupID, 10),
		}
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
		Tags:          tags,
		Source:        "ebpf",
	}, nil
}

func decodeRecord(raw []byte) (rawRecord, error) {
	var record rawRecord
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &record); err != nil {
		return rawRecord{}, fmt.Errorf("decode hip raw record: %w", err)
	}
	return record, nil
}
