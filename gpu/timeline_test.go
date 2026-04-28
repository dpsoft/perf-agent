package gpu

import (
	"testing"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestTimelineCorrelatesByCorrelationID(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "replay", Value: "corr-1"},
		KernelName:  "flash_attn_fwd",
		Launch:      LaunchContext{PID: 101, TID: 202},
	})
	tl.RecordExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: "replay", Value: "corr-1"},
		KernelName:  "flash_attn_fwd",
		StartNs:     100,
		EndNs:       250,
	})
	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 {
		t.Fatalf("got %d executions", len(snapshot.Executions))
	}
	if snapshot.Executions[0].Launch == nil {
		t.Fatalf("expected correlated launch")
	}
}

func TestTimelineMarksHeuristicJoin(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Queue:      GPUQueueRef{Backend: "replay", QueueID: "q0"},
		KernelName: "flash_attn_fwd",
		TimeNs:     100,
	})
	tl.RecordExec(GPUKernelExec{
		Queue:      GPUQueueRef{Backend: "replay", QueueID: "q0"},
		KernelName: "flash_attn_fwd",
		StartNs:    120,
		EndNs:      200,
	})
	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 || !snapshot.Executions[0].Heuristic {
		t.Fatalf("expected heuristic join: %#v", snapshot.Executions)
	}
}

func TestTimelinePreservesLifecycleEventOrder(t *testing.T) {
	tl := NewTimeline()
	tl.RecordEvent(GPUTimelineEvent{
		Backend: "linuxdrm",
		Kind:    TimelineEventIOCtl,
		Name:    "submit-begin",
		TimeNs:  100,
		PID:     10,
		TID:     11,
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend: "linuxdrm",
		Kind:    TimelineEventWait,
		Name:    "wait-end",
		TimeNs:  200,
		PID:     10,
		TID:     11,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Events) != 2 {
		t.Fatalf("got %d events", len(snapshot.Events))
	}
	if snapshot.Events[0].Name != "submit-begin" || snapshot.Events[1].Name != "wait-end" {
		t.Fatalf("unexpected event order: %#v", snapshot.Events)
	}
}

func TestTimelineSnapshotClonesLifecycleEventAttributes(t *testing.T) {
	tl := NewTimeline()
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventIOCtl,
		Name:       "submit",
		TimeNs:     100,
		Attributes: map[string]string{"cmd": "0xc04064"},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Events) != 1 {
		t.Fatalf("got %d events", len(snapshot.Events))
	}

	snapshot.Events[0].Attributes["cmd"] = "mutated"
	again := tl.Snapshot()
	if got := again.Events[0].Attributes["cmd"]; got != "0xc04064" {
		t.Fatalf("attributes mutated through snapshot copy: %q", got)
	}
}

func TestTimelineAttachesLaunchHeuristicallyToSubmitEvent(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch: LaunchContext{
			PID: 10,
			TID: 11,
			CPUStack: []pp.Frame{
				pp.FrameFromName("train_step"),
				pp.FrameFromName("hipLaunchKernel"),
			},
		},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventSubmit,
		Name:       "amdgpu-cs",
		TimeNs:     120,
		DurationNs: 15,
		PID:        10,
		TID:        11,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.EventViews) != 1 {
		t.Fatalf("got %d event views", len(snapshot.EventViews))
	}
	if snapshot.EventViews[0].Launch == nil {
		t.Fatal("expected attached launch")
	}
	if !snapshot.EventViews[0].Heuristic {
		t.Fatal("expected heuristic attribution")
	}
}

func TestTimelineBuildsWorkloadAttributions(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "stream", Value: "c1"},
		KernelName:  "hip_kernel",
		TimeNs:      100,
		Launch: LaunchContext{
			PID: 10,
			TID: 11,
			Tags: map[string]string{
				"cgroup_id": "9876",
				"pod_uid":   "pod-abc",
			},
		},
	})
	tl.RecordExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: "stream", Value: "c1"},
		KernelName:  "hip_kernel",
		StartNs:     120,
		EndNs:       200,
	})
	tl.RecordSample(GPUSample{
		Correlation: CorrelationID{Backend: "stream", Value: "c1"},
		KernelName:  "hip_kernel",
		Weight:      7,
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventSubmit,
		Name:       "amdgpu-cs",
		TimeNs:     130,
		DurationNs: 13,
		PID:        10,
		TID:        11,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Attributions) != 1 {
		t.Fatalf("got %d attributions", len(snapshot.Attributions))
	}
	got := snapshot.Attributions[0]
	if got.CgroupID != "9876" || got.PodUID != "pod-abc" {
		t.Fatalf("attribution=%+v", got)
	}
	if got.ExecutionCount != 1 || got.ExecutionDurationNs != 80 {
		t.Fatalf("execution aggregation=%+v", got)
	}
	if got.SampleWeight != 7 {
		t.Fatalf("sample weight=%d", got.SampleWeight)
	}
	if got.EventCount != 1 || got.EventDurationNs != 13 {
		t.Fatalf("event aggregation=%+v", got)
	}
}
