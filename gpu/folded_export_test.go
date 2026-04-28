package gpu

import (
	"bytes"
	"strings"
	"testing"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func TestWriteFoldedStacks(t *testing.T) {
	snap := Snapshot{
		Executions: []ExecutionView{
			{
				Launch: &GPUKernelLaunch{
					Launch: LaunchContext{
						PID: 1,
						CPUStack: []pp.Frame{
							pp.FrameFromName("train_step"),
							pp.FrameFromName("hipLaunchKernel"),
						},
					},
				},
				Exec: GPUKernelExec{
					Queue:      GPUQueueRef{Backend: "stream", QueueID: "q7"},
					KernelName: "hip_kernel",
					StartNs:    10,
					EndNs:      50,
				},
				Samples: []GPUSample{{StallReason: "memory_throttle", Weight: 7}},
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteFoldedStacks(&buf, snap); err != nil {
		t.Fatalf("WriteFoldedStacks: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "train_step;hipLaunchKernel;[gpu:launch];[gpu:queue:q7];[gpu:kernel:hip_kernel];[gpu:stall:memory_throttle] 7"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWriteFoldedStacksSkipsEmptySnapshot(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFoldedStacks(&buf, Snapshot{}); err != nil {
		t.Fatalf("WriteFoldedStacks: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("buf len=%d", buf.Len())
	}
}
