package gpu

import "testing"

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
