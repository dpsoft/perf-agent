package gpu

import (
	"testing"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestProjectionAppendsSyntheticGPUFrames(t *testing.T) {
	snap := Snapshot{
		Executions: []ExecutionView{
			{
				Launch: &GPUKernelLaunch{
					Queue:      GPUQueueRef{Backend: "replay", QueueID: "q7"},
					KernelName: "flash_attn_fwd",
					Launch: LaunchContext{
						PID: 1,
						CPUStack: []pp.Frame{
							pp.FrameFromName("train_step"),
							pp.FrameFromName("cudaLaunchKernel"),
						},
						Tags: map[string]string{
							"cgroup_id": "9876",
							"pod_uid":   "pod-abc",
						},
					},
				},
				Exec: GPUKernelExec{
					Queue:      GPUQueueRef{Backend: "replay", QueueID: "q7"},
					KernelName: "flash_attn_fwd",
					StartNs:    10,
					EndNs:      50,
				},
				Samples: []GPUSample{{StallReason: "memory_throttle", Weight: 7}},
			},
		},
	}
	samples := ProjectExecutionSamples(snap)
	if len(samples) != 1 {
		t.Fatalf("got %d samples", len(samples))
	}
	got := samples[0].Stack
	wantNames := []string{
		"train_step",
		"cudaLaunchKernel",
		"[gpu:cgroup:9876]",
		"[gpu:pod:pod-abc]",
		"[gpu:launch]",
		"[gpu:queue:q7]",
		"[gpu:kernel:flash_attn_fwd]",
		"[gpu:stall:memory_throttle]",
	}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d frames, want %d", len(got), len(wantNames))
	}
	for i, want := range wantNames {
		if got[i].Name != want {
			t.Fatalf("frame %d = %q want %q", i, got[i].Name, want)
		}
	}
}

func TestProjectionIncludesAttributedSubmitEvent(t *testing.T) {
	snap := Snapshot{
		EventViews: []EventView{
			{
				Launch: &GPUKernelLaunch{
					Launch: LaunchContext{
						PID: 1,
						CPUStack: []pp.Frame{
							pp.FrameFromName("train_step"),
							pp.FrameFromName("cudaLaunchKernel"),
						},
						Tags: map[string]string{
							"cgroup_id": "9876",
							"pod_uid":   "pod-abc",
						},
					},
				},
				Event: GPUTimelineEvent{
					Backend:    "linuxdrm",
					Kind:       TimelineEventSubmit,
					Name:       "amdgpu-cs",
					TimeNs:     130,
					DurationNs: 13,
				},
				Heuristic: true,
			},
		},
	}

	samples := ProjectExecutionSamples(snap)
	if len(samples) != 1 {
		t.Fatalf("got %d samples", len(samples))
	}
	wantNames := []string{
		"train_step",
		"cudaLaunchKernel",
		"[gpu:cgroup:9876]",
		"[gpu:pod:pod-abc]",
		"[gpu:launch]",
		"[gpu:event:submit:amdgpu-cs]",
	}
	got := samples[0].Stack
	if len(got) != len(wantNames) {
		t.Fatalf("got %d frames, want %d", len(got), len(wantNames))
	}
	for i, want := range wantNames {
		if got[i].Name != want {
			t.Fatalf("frame %d = %q want %q", i, got[i].Name, want)
		}
	}
	if samples[0].Value != 13 {
		t.Fatalf("value=%d", samples[0].Value)
	}
}
