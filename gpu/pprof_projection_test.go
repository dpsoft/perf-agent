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
