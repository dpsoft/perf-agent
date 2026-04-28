package host

import (
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestNormalizeLaunchRecord(t *testing.T) {
	rec := LaunchRecord{
		Backend:       "stream",
		PID:           123,
		TID:           456,
		TimeNs:        100,
		KernelName:    "flash_attn_fwd",
		QueueID:       "q7",
		CorrelationID: "c1",
		CPUStack: []pp.Frame{
			pp.FrameFromName("train_step"),
			pp.FrameFromName("cudaLaunchKernel"),
		},
		Tags: map[string]string{"env": "test"},
	}

	launch, err := NormalizeLaunch(rec)
	if err != nil {
		t.Fatalf("NormalizeLaunch: %v", err)
	}
	if launch.Correlation != (gpu.CorrelationID{Backend: "stream", Value: "c1"}) {
		t.Fatalf("correlation=%+v", launch.Correlation)
	}
	if launch.Queue.QueueID != "q7" {
		t.Fatalf("queue=%q", launch.Queue.QueueID)
	}
	if got := len(launch.Launch.CPUStack); got != 2 {
		t.Fatalf("cpu stack len=%d", got)
	}
	if launch.Launch.Tags["env"] != "test" {
		t.Fatalf("tags=%v", launch.Launch.Tags)
	}
}

func TestNormalizeLaunchRejectsMissingCorrelation(t *testing.T) {
	_, err := NormalizeLaunch(LaunchRecord{
		Backend:    "stream",
		KernelName: "flash_attn_fwd",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

type captureEventSink struct {
	launches []gpu.GPUKernelLaunch
}

func (s *captureEventSink) EmitLaunch(event gpu.GPUKernelLaunch) {
	s.launches = append(s.launches, event)
}

func (s *captureEventSink) EmitExec(gpu.GPUKernelExec) {}

func (s *captureEventSink) EmitCounter(gpu.GPUCounterSample) {}

func (s *captureEventSink) EmitSample(gpu.GPUSample) {}

func (s *captureEventSink) EmitEvent(gpu.GPUTimelineEvent) {}

func TestLaunchSinkEmitsCanonicalLaunch(t *testing.T) {
	var sink captureEventSink
	hostSink := NewLaunchSink(&sink)

	err := hostSink.EmitLaunchRecord(LaunchRecord{
		Backend:       "stream",
		KernelName:    "flash_attn_fwd",
		CorrelationID: "c1",
	})
	if err != nil {
		t.Fatalf("EmitLaunchRecord: %v", err)
	}
	if len(sink.launches) != 1 {
		t.Fatalf("launches=%d", len(sink.launches))
	}
	if sink.launches[0].Correlation.Value != "c1" {
		t.Fatalf("correlation=%+v", sink.launches[0].Correlation)
	}
}

func TestLaunchSinkEnrichesCgroupTags(t *testing.T) {
	var sink captureEventSink
	hostSink := newLaunchSinkWithLookup(&sink, func(pid uint32) (string, bool) {
		if pid != 4242 {
			return "", false
		}
		return "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod2af2f6f1_1111_2222_3333_444444444444.slice/cri-containerd-0123456789abcdef.scope", true
	})

	err := hostSink.EmitLaunchRecord(LaunchRecord{
		Backend:       "hip",
		PID:           4242,
		KernelName:    "flash_attn_fwd",
		CorrelationID: "c1",
		Tags: map[string]string{
			"cgroup_id": "9876",
		},
	})
	if err != nil {
		t.Fatalf("EmitLaunchRecord: %v", err)
	}
	tags := sink.launches[0].Launch.Tags
	if got := tags["cgroup_path"]; got == "" {
		t.Fatalf("missing cgroup_path in %v", tags)
	}
	if got := tags["pod_uid"]; got != "2af2f6f1-1111-2222-3333-444444444444" {
		t.Fatalf("pod_uid=%q", got)
	}
	if got := tags["container_runtime"]; got != "containerd" {
		t.Fatalf("container_runtime=%q", got)
	}
	if got := tags["container_id"]; got != "0123456789abcdef" {
		t.Fatalf("container_id=%q", got)
	}
}
