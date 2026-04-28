package hip

import (
	"context"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/host"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type captureSink struct {
	records []host.LaunchRecord
}

func (s *captureSink) EmitLaunchRecord(record host.LaunchRecord) error {
	s.records = append(s.records, record)
	return nil
}

func TestNewRejectsMissingPID(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestStartEmitsNormalizedLaunchRecordFromTestRecord(t *testing.T) {
	src, err := New(Config{
		PID: 123,
		testRecords: []rawRecord{{
			PID:          123,
			TID:          456,
			TimeNs:       789,
			FunctionAddr: 0x1234,
			UserStackID:  7,
			Stream:       0xbeef,
		}},
		testDecode: func(record rawRecord) (launchRecord, error) {
			return launchRecord{
				Backend:       "hip",
				PID:           record.PID,
				TID:           record.TID,
				TimeNs:        record.TimeNs,
				KernelName:    "hip_kernel",
				QueueID:       "stream:beef",
				CorrelationID: "hip:123:456:789",
				CPUStack: []pp.Frame{
					pp.FrameFromName("train_step"),
					pp.FrameFromName("hipLaunchKernel"),
				},
				Source: "ebpf",
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var sink captureSink
	if err := src.Start(context.Background(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(sink.records); got != 1 {
		t.Fatalf("records=%d", got)
	}
	if sink.records[0].KernelName != "hip_kernel" {
		t.Fatalf("kernel=%q", sink.records[0].KernelName)
	}
	if got := len(sink.records[0].CPUStack); got != 2 {
		t.Fatalf("cpu stack len=%d", got)
	}
}
