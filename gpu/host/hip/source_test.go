package hip

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestNewRejectsMissingLibraryInLiveMode(t *testing.T) {
	if _, err := New(Config{PID: 123}); err == nil {
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

func TestNewAcceptsLiveConfig(t *testing.T) {
	src, err := New(Config{
		PID:         123,
		LibraryPath: "/lib64/libamdhip64.so",
		Symbol:      "hipExtLaunchKernel",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if src == nil {
		t.Fatal("expected source")
	}
}

func TestStartLiveModeDelegatesToLiveRunner(t *testing.T) {
	src, err := New(Config{
		PID:         123,
		LibraryPath: "/lib64/libamdhip64.so",
		Symbol:      "hipLaunchKernel",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	started := make(chan struct{})
	released := make(chan struct{})
	src.startLive = func(ctx context.Context, sink host.HostSink) error {
		close(started)
		if err := sink.EmitLaunchRecord(host.LaunchRecord{
			Backend:       "hip",
			PID:           123,
			TID:           456,
			TimeNs:        789,
			KernelName:    "hip_kernel",
			QueueID:       "stream:beef",
			CorrelationID: "hip:123:456:789",
		}); err != nil {
			return err
		}
		close(released)
		<-ctx.Done()
		return context.Cause(ctx)
	}

	var sink captureSink
	if err := src.Start(context.Background(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected live runner")
	}
	if got := len(sink.records); got != 1 {
		t.Fatalf("records=%d", got)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("expected launch emission")
	}
	if err := src.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopLiveModePropagatesRunError(t *testing.T) {
	src, err := New(Config{
		PID:         123,
		LibraryPath: "/lib64/libamdhip64.so",
		Symbol:      "hipLaunchKernel",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := errors.New("boom")
	started := make(chan struct{})
	src.startLive = func(context.Context, host.HostSink) error {
		close(started)
		return want
	}

	var sink captureSink
	if err := src.Start(context.Background(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected live runner")
	}
	if err := src.Stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopBeforeStartIsNoop(t *testing.T) {
	src, err := New(Config{
		PID:         123,
		LibraryPath: "/lib64/libamdhip64.so",
		Symbol:      "hipLaunchKernel",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := src.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartRejectsSecondLiveStart(t *testing.T) {
	src, err := New(Config{
		PID:         123,
		LibraryPath: "/lib64/libamdhip64.so",
		Symbol:      "hipLaunchKernel",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	block := make(chan struct{})
	src.startLive = func(ctx context.Context, sink host.HostSink) error {
		<-ctx.Done()
		close(block)
		return context.Cause(ctx)
	}

	var sink captureSink
	if err := src.Start(context.Background(), &sink); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := src.Start(context.Background(), &sink); err == nil {
		t.Fatal("expected second Start error")
	}
	if err := src.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-block:
	case <-time.After(time.Second):
		t.Fatal("expected live runner release")
	}
}
