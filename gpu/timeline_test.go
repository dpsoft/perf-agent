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
