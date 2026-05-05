package linuxdrm

import (
	"context"
	"errors"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type sink struct{}

func (s *sink) EmitLaunch(gpu.GPUKernelLaunch)   {}
func (s *sink) EmitExec(gpu.GPUKernelExec)       {}
func (s *sink) EmitCounter(gpu.GPUCounterSample) {}
func (s *sink) EmitSample(gpu.GPUSample)         {}
func (s *sink) EmitEvent(gpu.GPUTimelineEvent)   {}

type eventSink struct {
	events []gpu.GPUTimelineEvent
}

func (s *eventSink) EmitLaunch(gpu.GPUKernelLaunch)   {}
func (s *eventSink) EmitExec(gpu.GPUKernelExec)       {}
func (s *eventSink) EmitCounter(gpu.GPUCounterSample) {}
func (s *eventSink) EmitSample(gpu.GPUSample)         {}
func (s *eventSink) EmitEvent(event gpu.GPUTimelineEvent) {
	s.events = append(s.events, event)
}

func TestNewRejectsMissingPID(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNewAcceptsMinimalConfig(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("expected backend")
	}
}

func TestBackendIDAndCapabilities(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.ID(); got != gpu.BackendLinuxDRM {
		t.Fatalf("ID=%q", got)
	}
	if got := b.EventBackends(); len(got) != 2 || got[0] != gpu.BackendLinuxDRM || got[1] != gpu.BackendLinuxKFD {
		t.Fatalf("EventBackends()=%v", got)
	}
	got := b.Capabilities()
	want := []gpu.GPUCapability{gpu.CapabilityLifecycleTimeline}
	if len(got) != len(want) {
		t.Fatalf("len(capabilities)=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cap[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestBackendEventBackendsCanBeScopedToLinuxKFD(t *testing.T) {
	b, err := New(Config{
		PID:           123,
		EventBackends: []gpu.GPUBackendID{gpu.BackendLinuxKFD},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.EventBackends(); len(got) != 1 || got[0] != gpu.BackendLinuxKFD {
		t.Fatalf("EventBackends()=%v", got)
	}
}

func TestStartRejectsNilSink(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Start(t.Context(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestStopPropagatesRunError(t *testing.T) {
	want := errors.New("boom")
	b, err := New(Config{
		PID: 123,
		testRun: func(context.Context, gpu.EventSink) error {
			return want
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Start(context.Background(), &sink{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); !errors.Is(err, want) {
		t.Fatalf("Stop error=%v want %v", err, want)
	}
}

func TestStartEmitsNormalizedEventsFromTestRecords(t *testing.T) {
	b, err := New(Config{
		PID: 123,
		testRecords: []rawRecord{
			{
				Kind:        recordKindIOCtl,
				PID:         123,
				TID:         124,
				FD:          9,
				Command:     0xc04064,
				ResultCode:  0,
				StartNs:     1000,
				EndNs:       1200,
				DeviceMajor: 226,
				DeviceMinor: 128,
				Inode:       77,
				CgroupID:    12345,
			},
			{
				Kind:    recordKindSchedWakeup,
				PID:     123,
				TID:     124,
				StartNs: 2000,
				CPU:     5,
			},
			{
				Kind:    recordKindSchedRunq,
				PID:     123,
				TID:     124,
				StartNs: 2000,
				EndNs:   2200,
				CPU:     5,
				AuxNs:   200,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var sink eventSink
	if err := b.Start(context.Background(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(sink.events) != 3 {
		t.Fatalf("events=%d", len(sink.events))
	}
	if sink.events[0].Kind != gpu.TimelineEventIOCtl {
		t.Fatalf("kind=%q", sink.events[0].Kind)
	}
	if got := sink.events[0].Attributes["cgroup_id"]; got != "12345" {
		t.Fatalf("event[0].cgroup_id=%q", got)
	}
	if sink.events[1].Name != "sched-wakeup" {
		t.Fatalf("event[1].name=%q", sink.events[1].Name)
	}
	if sink.events[2].Name != "sched-runq-latency" {
		t.Fatalf("event[2].name=%q", sink.events[2].Name)
	}
}

func TestStartFiltersEventsToConfiguredEventBackends(t *testing.T) {
	b, err := New(Config{
		PID:           123,
		EventBackends: []gpu.GPUBackendID{gpu.BackendLinuxKFD},
		testRecords: []rawRecord{
			{
				Kind:        recordKindIOCtl,
				PID:         123,
				TID:         124,
				FD:          9,
				Command:     0xc04064,
				ResultCode:  0,
				StartNs:     1000,
				EndNs:       1200,
				DeviceMajor: 226,
				DeviceMinor: 128,
				Inode:       77,
			},
			{
				Kind:        recordKindIOCtl,
				PID:         123,
				TID:         124,
				FD:          3,
				Command:     encodeTestIOCtl(1, 8, 'K', 0x17),
				ResultCode:  0,
				StartNs:     1300,
				EndNs:       1500,
				DeviceMajor: 235,
				DeviceMinor: 0,
				Inode:       527,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var sink eventSink
	if err := b.Start(context.Background(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(sink.events) != 1 {
		t.Fatalf("events=%d want 1", len(sink.events))
	}
	if got := sink.events[0].Backend; got != gpu.BackendLinuxKFD {
		t.Fatalf("event backend=%q want %q", got, gpu.BackendLinuxKFD)
	}
	if got := sink.events[0].Name; got != "kfd-free-memory-of-gpu" {
		t.Fatalf("event name=%q", got)
	}
}
