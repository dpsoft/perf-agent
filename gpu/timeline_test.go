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
	if len(snapshot.Launches) != 1 {
		t.Fatalf("got %d launches", len(snapshot.Launches))
	}
	if snapshot.Executions[0].Launch == nil {
		t.Fatalf("expected correlated launch")
	}
	if snapshot.Executions[0].Join != JoinExact {
		t.Fatalf("join=%q", snapshot.Executions[0].Join)
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
	if snapshot.Executions[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.Executions[0].Join)
	}
}

func TestTimelineHeuristicallyAttachesInWindowSamples(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Queue:      GPUQueueRef{Backend: BackendAMDSample, QueueID: "compute:0"},
		KernelName: "hip_launch_shim_kernel",
		TimeNs:     100,
	})
	tl.RecordExec(GPUKernelExec{
		Execution:   GPUExecutionRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", QueueID: "compute:0", ExecID: "dispatch-1"},
		Correlation: CorrelationID{Backend: BackendAMDSample, Value: "dispatch-1"},
		Queue: GPUQueueRef{
			Backend: BackendAMDSample,
			Device:  GPUDeviceRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", Name: "AMD Radeon 780M Graphics"},
			QueueID: "compute:0",
		},
		KernelName: "hip_launch_shim_kernel",
		StartNs:    120,
		EndNs:      260,
	})
	tl.RecordSample(GPUSample{
		Correlation: CorrelationID{Backend: BackendAMDSample, Value: "sample-stream-1"},
		Device:      GPUDeviceRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", Name: "AMD Radeon 780M Graphics"},
		TimeNs:      150,
		KernelName:  "hip_launch_shim_kernel",
		StallReason: "memory_wait",
		Weight:      11,
	})
	tl.RecordSample(GPUSample{
		Correlation: CorrelationID{Backend: BackendAMDSample, Value: "sample-stream-2"},
		Device:      GPUDeviceRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", Name: "AMD Radeon 780M Graphics"},
		TimeNs:      300,
		KernelName:  "hip_launch_shim_kernel",
		StallReason: "wave_barrier",
		Weight:      5,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 {
		t.Fatalf("got %d executions", len(snapshot.Executions))
	}
	if snapshot.Executions[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.Executions[0].Join)
	}
	if len(snapshot.Executions[0].Samples) != 1 {
		t.Fatalf("got %d samples", len(snapshot.Executions[0].Samples))
	}
	got := snapshot.Executions[0].Samples[0]
	if got.StallReason != "memory_wait" || got.Weight != 11 {
		t.Fatalf("sample=%+v", got)
	}
}

func TestTimelineHeuristicallyJoinsAMDSampleExecAcrossQueueBoundary(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendHIP, Value: "hip:555:555:100"},
		Queue:       GPUQueueRef{Backend: BackendHIP, QueueID: "stream:0"},
		KernelName:  "hip_launch_shim_kernel",
		TimeNs:      100,
		Launch:      LaunchContext{PID: 555, TID: 555},
	})
	tl.RecordExec(GPUKernelExec{
		Execution:   GPUExecutionRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", QueueID: "compute:0", ExecID: "dispatch-1"},
		Correlation: CorrelationID{Backend: BackendAMDSample, Value: "dispatch-1"},
		Queue: GPUQueueRef{
			Backend: BackendAMDSample,
			Device:  GPUDeviceRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", Name: "AMD Radeon 780M Graphics"},
			QueueID: "compute:0",
		},
		KernelName: "hip_launch_shim_kernel",
		StartNs:    120,
		EndNs:      260,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 {
		t.Fatalf("got %d executions", len(snapshot.Executions))
	}
	if snapshot.Executions[0].Launch == nil {
		t.Fatal("expected attached launch")
	}
	if snapshot.Executions[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.Executions[0].Join)
	}
	if !snapshot.Executions[0].Heuristic {
		t.Fatalf("expected heuristic execution join: %+v", snapshot.Executions[0])
	}
}

func TestTimelineHeuristicallyJoinsAMDSampleExecFromHIPModuleLaunchFallbackName(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendHIP, Value: "hip:999:999:100"},
		Queue:       GPUQueueRef{Backend: BackendHIP, QueueID: "stream:1"},
		KernelName:  "hip_kernel@0x1234",
		TimeNs:      100,
		Launch: LaunchContext{
			PID: 999,
			TID: 999,
			CPUStack: []pp.Frame{
				pp.FrameFromName("real_hip_attention_workload::flash_attention"),
				pp.FrameFromName("hipModuleLaunchKernel"),
			},
		},
	})
	tl.RecordExec(GPUKernelExec{
		Execution:   GPUExecutionRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", QueueID: "compute:0", ExecID: "dispatch:1"},
		Correlation: CorrelationID{Backend: BackendAMDSample, Value: "dispatch:1"},
		Queue: GPUQueueRef{
			Backend: BackendAMDSample,
			Device:  GPUDeviceRef{Backend: BackendAMDSample, DeviceID: "gfx1103:0", Name: "AMD Radeon 780M Graphics"},
			QueueID: "compute:0",
		},
		KernelName: "flash_attn_decode_bf16_gfx11.kd",
		StartNs:    120,
		EndNs:      260,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Executions) != 1 {
		t.Fatalf("got %d executions", len(snapshot.Executions))
	}
	if snapshot.Executions[0].Launch == nil {
		t.Fatal("expected attached launch")
	}
	if snapshot.Executions[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.Executions[0].Join)
	}
	if !snapshot.Executions[0].Heuristic {
		t.Fatalf("expected heuristic execution join: %+v", snapshot.Executions[0])
	}
	if got := snapshot.JoinStats.HeuristicExecutionJoinCount; got != 1 {
		t.Fatalf("heuristic_execution_join_count=%d", got)
	}
	if got := snapshot.JoinStats.UnmatchedLaunchCount; got != 0 {
		t.Fatalf("unmatched_launch_count=%d", got)
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
	if snapshot.EventViews[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.EventViews[0].Join)
	}
}

func TestTimelineAttachesLaunchToEventWithinConfiguredWindow(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchEventJoinWindowNs: 50})
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend: "linuxdrm",
		Kind:    TimelineEventSubmit,
		Name:    "amdgpu-cs",
		TimeNs:  140,
		PID:     10,
		TID:     11,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.EventViews) != 1 {
		t.Fatalf("got %d event views", len(snapshot.EventViews))
	}
	if snapshot.EventViews[0].Launch == nil {
		t.Fatal("expected attached launch")
	}
}

func TestTimelineSkipsEventOutsideConfiguredWindow(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchEventJoinWindowNs: 20})
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend: "linuxdrm",
		Kind:    TimelineEventSubmit,
		Name:    "amdgpu-cs",
		TimeNs:  140,
		PID:     10,
		TID:     11,
	})

	snapshot := tl.Snapshot()
	if len(snapshot.EventViews) != 1 {
		t.Fatalf("got %d event views", len(snapshot.EventViews))
	}
	if snapshot.EventViews[0].Launch != nil {
		t.Fatalf("expected no attached launch: %#v", snapshot.EventViews[0])
	}
	if snapshot.EventViews[0].Join != "" {
		t.Fatalf("expected empty join, got %q", snapshot.EventViews[0].Join)
	}
}

func TestTimelineAttachesLaunchHeuristicallyToKFDMemoryEvent(t *testing.T) {
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
		Kind:       TimelineEventMemory,
		Name:       "kfd-free-memory-of-gpu",
		TimeNs:     120,
		DurationNs: 15,
		PID:        10,
		TID:        11,
		Attributes: map[string]string{
			"command_family": "kfd",
			"command_name":   "free_memory_of_gpu",
		},
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
	if snapshot.EventViews[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.EventViews[0].Join)
	}
	if stats := snapshot.JoinStats; stats.LaunchCount != 1 || stats.MatchedLaunchCount != 1 || stats.UnmatchedLaunchCount != 0 || stats.HeuristicEventJoinCount != 1 || stats.UnmatchedCandidateEventCount != 0 {
		t.Fatalf("join stats=%+v", stats)
	}
}

func TestTimelineDoesNotAttachLaunchToNonKFDMemoryEvent(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventMemory,
		Name:       "drm-gem-close",
		TimeNs:     120,
		DurationNs: 15,
		PID:        10,
		TID:        11,
		Attributes: map[string]string{
			"command_family": "drm-core",
			"command_name":   "gem_close",
		},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.EventViews) != 1 {
		t.Fatalf("got %d event views", len(snapshot.EventViews))
	}
	if snapshot.EventViews[0].Launch != nil {
		t.Fatalf("expected no attached launch: %#v", snapshot.EventViews[0])
	}
	if snapshot.EventViews[0].Join != "" {
		t.Fatalf("expected empty join, got %q", snapshot.EventViews[0].Join)
	}
	if stats := snapshot.JoinStats; stats.LaunchCount != 1 || stats.MatchedLaunchCount != 0 || stats.UnmatchedLaunchCount != 1 || stats.HeuristicEventJoinCount != 0 || stats.UnmatchedCandidateEventCount != 0 {
		t.Fatalf("join stats=%+v", stats)
	}
}

func TestTimelineAttachesLaunchToKFDMemoryEventByExplicitFamily(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventMemory,
		Family:     "kfd",
		Name:       "kfd-map-memory-to-gpu",
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
	if snapshot.EventViews[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.EventViews[0].Join)
	}
}

func TestTimelineAttachesLaunchToKFDMemoryEventByExplicitBackend(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    BackendLinuxKFD,
		Kind:       TimelineEventMemory,
		Name:       "kfd-map-memory-to-gpu",
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
	if snapshot.EventViews[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.EventViews[0].Join)
	}
}

func TestTimelineExplicitFamilyOverridesConflictingAttributes(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventMemory,
		Family:     "drm-core",
		Name:       "drm-gem-close",
		TimeNs:     120,
		DurationNs: 15,
		PID:        10,
		TID:        11,
		Attributes: map[string]string{
			"command_family": "kfd",
		},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.EventViews) != 1 {
		t.Fatalf("got %d event views", len(snapshot.EventViews))
	}
	if snapshot.EventViews[0].Launch != nil {
		t.Fatalf("expected no attached launch: %#v", snapshot.EventViews[0])
	}
}

func TestTimelineExplicitBackendOverridesConflictingFamily(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		KernelName: "hip_kernel",
		TimeNs:     100,
		Launch:     LaunchContext{PID: 10, TID: 11},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    BackendLinuxKFD,
		Kind:       TimelineEventMemory,
		Family:     "drm-core",
		Name:       "kfd-map-memory-to-gpu",
		TimeNs:     120,
		DurationNs: 15,
		PID:        10,
		TID:        11,
		Attributes: map[string]string{
			"command_family": "drm-core",
		},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.EventViews) != 1 {
		t.Fatalf("got %d event views", len(snapshot.EventViews))
	}
	if snapshot.EventViews[0].Launch == nil {
		t.Fatal("expected attached launch")
	}
	if snapshot.EventViews[0].Join != JoinHeuristic {
		t.Fatalf("join=%q", snapshot.EventViews[0].Join)
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
				"cgroup_id":         "9876",
				"pod_uid":           "pod-abc",
				"container_id":      "ctr-123",
				"container_runtime": "containerd",
			},
		},
	})
	tl.RecordExec(GPUKernelExec{
		Execution:   GPUExecutionRef{Backend: "stream"},
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
	if got.CgroupID != "9876" || got.PodUID != "pod-abc" || got.ContainerID != "ctr-123" || got.ContainerRuntime != "containerd" {
		t.Fatalf("attribution=%+v", got)
	}
	if got.LaunchCount != 1 {
		t.Fatalf("launch count=%d", got.LaunchCount)
	}
	if len(got.KernelNames) != 1 || got.KernelNames[0] != "hip_kernel" {
		t.Fatalf("kernel names=%v", got.KernelNames)
	}
	if got.ExactJoinCount != 1 || got.HeuristicJoinCount != 1 {
		t.Fatalf("join counts=%+v", got)
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
	if got.FirstSeenNs != 100 || got.LastSeenNs != 200 {
		t.Fatalf("seen window=%+v", got)
	}
	if len(got.Backends) != 2 || got.Backends[0] != "linuxdrm" || got.Backends[1] != "stream" {
		t.Fatalf("backends=%v", got.Backends)
	}
	if stats := snapshot.JoinStats; stats.LaunchCount != 1 || stats.MatchedLaunchCount != 1 || stats.UnmatchedLaunchCount != 0 || stats.ExactExecutionJoinCount != 1 || stats.HeuristicEventJoinCount != 1 || stats.UnmatchedExecutionCount != 0 || stats.UnmatchedCandidateEventCount != 0 {
		t.Fatalf("join stats=%+v", stats)
	}
}

func TestTimelineBuildsWorkloadAttributionsForKFDMemoryEvent(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "hip", Value: "c1"},
		KernelName:  "hip_kernel",
		TimeNs:      100,
		Launch: LaunchContext{
			PID: 10,
			TID: 11,
			Tags: map[string]string{
				"cgroup_id":         "9876",
				"pod_uid":           "pod-abc",
				"container_id":      "ctr-123",
				"container_runtime": "containerd",
			},
		},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxkfd",
		Kind:       TimelineEventMemory,
		Name:       "kfd-unmap-memory-from-gpu",
		TimeNs:     130,
		DurationNs: 13,
		PID:        10,
		TID:        11,
		Attributes: map[string]string{
			"command_family": "kfd",
			"command_name":   "unmap_memory_from_gpu",
		},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Attributions) != 1 {
		t.Fatalf("got %d attributions", len(snapshot.Attributions))
	}
	got := snapshot.Attributions[0]
	if got.LaunchCount != 1 || got.EventCount != 1 || got.EventDurationNs != 13 {
		t.Fatalf("event aggregation=%+v", got)
	}
	if got.HeuristicJoinCount != 1 || got.ExactJoinCount != 0 {
		t.Fatalf("join counts=%+v", got)
	}
	if len(got.Backends) != 2 || got.Backends[0] != "hip" || got.Backends[1] != "linuxkfd" {
		t.Fatalf("backends=%v", got.Backends)
	}
	if len(got.EventFamilies) != 1 || got.EventFamilies[0] != "kfd" {
		t.Fatalf("event families=%v", got.EventFamilies)
	}
	if got.FirstSeenNs != 100 || got.LastSeenNs != 143 {
		t.Fatalf("seen window=%+v", got)
	}
}

func TestTimelineBuildsLaunchOnlyWorkloadAttribution(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "host-replay", Value: "launch-only"},
		KernelName:  "hip_kernel",
		TimeNs:      55,
		Launch: LaunchContext{
			PID: 10,
			TID: 11,
			Tags: map[string]string{
				"cgroup_id": "1234",
				"pod_uid":   "pod-only",
			},
		},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Attributions) != 1 {
		t.Fatalf("got %d attributions", len(snapshot.Attributions))
	}
	got := snapshot.Attributions[0]
	if got.CgroupID != "1234" || got.PodUID != "pod-only" {
		t.Fatalf("attribution=%+v", got)
	}
	if got.LaunchCount != 1 {
		t.Fatalf("launch count=%d", got.LaunchCount)
	}
	if len(got.KernelNames) != 1 || got.KernelNames[0] != "hip_kernel" {
		t.Fatalf("kernel names=%v", got.KernelNames)
	}
	if got.ExactJoinCount != 0 || got.HeuristicJoinCount != 0 {
		t.Fatalf("join counts=%+v", got)
	}
	if got.FirstSeenNs != 55 || got.LastSeenNs != 55 {
		t.Fatalf("seen window=%+v", got)
	}
	if len(got.Backends) != 1 || got.Backends[0] != "host-replay" {
		t.Fatalf("backends=%v", got.Backends)
	}
}

func TestTimelineBuildsSortedMergedWorkloadAttributions(t *testing.T) {
	tl := NewTimeline()
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "stream", Value: "b"},
		TimeNs:      40,
		Launch: LaunchContext{
			PID: 2,
			TID: 2,
			Tags: map[string]string{
				"cgroup_id": "2000",
				"pod_uid":   "pod-b",
			},
		},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventSubmit,
		Name:       "submit-b",
		TimeNs:     50,
		DurationNs: 5,
		PID:        2,
		TID:        2,
	})
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "stream", Value: "a"},
		TimeNs:      10,
		Launch: LaunchContext{
			PID: 1,
			TID: 1,
			Tags: map[string]string{
				"cgroup_id": "1000",
				"pod_uid":   "pod-a",
			},
		},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventSubmit,
		Name:       "submit-a1",
		TimeNs:     20,
		DurationNs: 3,
		PID:        1,
		TID:        1,
		Attributes: map[string]string{
			"command_family": "amdgpu",
		},
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend:    "linuxdrm",
		Kind:       TimelineEventWait,
		Name:       "wait-a2",
		TimeNs:     25,
		DurationNs: 4,
		PID:        1,
		TID:        1,
		Attributes: map[string]string{
			"command_family": "drm-core",
		},
	})

	snapshot := tl.Snapshot()
	if len(snapshot.Attributions) != 2 {
		t.Fatalf("got %d attributions", len(snapshot.Attributions))
	}

	first := snapshot.Attributions[0]
	second := snapshot.Attributions[1]

	if first.CgroupID != "1000" || first.PodUID != "pod-a" {
		t.Fatalf("first attribution=%+v", first)
	}
	if first.LaunchCount != 1 || first.EventCount != 2 || first.EventDurationNs != 7 {
		t.Fatalf("first totals=%+v", first)
	}
	if len(first.KernelNames) != 0 {
		t.Fatalf("first kernel names=%v", first.KernelNames)
	}
	if first.ExactJoinCount != 0 || first.HeuristicJoinCount != 2 {
		t.Fatalf("first joins=%+v", first)
	}
	if len(first.EventFamilies) != 2 || first.EventFamilies[0] != "amdgpu" || first.EventFamilies[1] != "drm-core" {
		t.Fatalf("first event families=%v", first.EventFamilies)
	}
	if first.FirstSeenNs != 10 || first.LastSeenNs != 29 {
		t.Fatalf("first window=%+v", first)
	}

	if second.CgroupID != "2000" || second.PodUID != "pod-b" {
		t.Fatalf("second attribution=%+v", second)
	}
	if second.LaunchCount != 1 || second.EventCount != 1 || second.EventDurationNs != 5 {
		t.Fatalf("second totals=%+v", second)
	}
	if len(second.KernelNames) != 0 {
		t.Fatalf("second kernel names=%v", second.KernelNames)
	}
	if second.ExactJoinCount != 0 || second.HeuristicJoinCount != 1 {
		t.Fatalf("second joins=%+v", second)
	}
	if len(second.EventFamilies) != 1 || second.EventFamilies[0] != "unknown" {
		t.Fatalf("second event families=%v", second.EventFamilies)
	}
	if second.FirstSeenNs != 40 || second.LastSeenNs != 55 {
		t.Fatalf("second window=%+v", second)
	}
	if stats := snapshot.JoinStats; stats.LaunchCount != 2 || stats.MatchedLaunchCount != 2 || stats.UnmatchedLaunchCount != 0 || stats.HeuristicEventJoinCount != 3 || stats.UnmatchedCandidateEventCount != 0 {
		t.Fatalf("join stats=%+v", stats)
	}
}

func TestTimelineBuildsJoinStatsForUnmatchedRecords(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchEventJoinWindowNs: 5})
	tl.RecordLaunch(GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "stream", Value: "l1"},
		KernelName:  "alpha_kernel",
		TimeNs:      10,
		Launch:      LaunchContext{PID: 1, TID: 1},
	})
	tl.RecordExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: "stream", Value: "missing"},
		KernelName:  "beta_kernel",
		StartNs:     30,
		EndNs:       60,
	})
	tl.RecordEvent(GPUTimelineEvent{
		Backend: "linuxdrm",
		Kind:    TimelineEventSubmit,
		Name:    "submit-late",
		TimeNs:  40,
		PID:     1,
		TID:     1,
	})

	snapshot := tl.Snapshot()
	stats := snapshot.JoinStats
	if stats.LaunchCount != 1 || stats.MatchedLaunchCount != 0 || stats.UnmatchedLaunchCount != 1 {
		t.Fatalf("launch stats=%+v", stats)
	}
	if stats.UnmatchedExecutionCount != 1 || stats.ExactExecutionJoinCount != 0 || stats.HeuristicExecutionJoinCount != 0 {
		t.Fatalf("execution stats=%+v", stats)
	}
	if stats.HeuristicEventJoinCount != 0 || stats.UnmatchedCandidateEventCount != 1 {
		t.Fatalf("event stats=%+v", stats)
	}
}
