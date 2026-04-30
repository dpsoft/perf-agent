package gpu

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
						Tags: map[string]string{
							"cgroup_id": "9876",
							"pod_uid":   "pod-abc",
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
	want := "train_step;hipLaunchKernel;[gpu:cgroup:9876];[gpu:pod:pod-abc];[gpu:launch];[gpu:queue:q7];[gpu:kernel:hip_kernel];[gpu:stall:memory_throttle] 7"
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

func TestWriteFoldedStacksIncludesAttributedSubmitEvent(t *testing.T) {
	snap := Snapshot{
		EventViews: []EventView{
			{
				Launch: &GPUKernelLaunch{
					Launch: LaunchContext{
						PID: 1,
						CPUStack: []pp.Frame{
							pp.FrameFromName("train_step"),
							pp.FrameFromName("hipLaunchKernel"),
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
					TimeNs:     100,
					DurationNs: 13,
				},
				Heuristic: true,
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteFoldedStacks(&buf, snap); err != nil {
		t.Fatalf("WriteFoldedStacks: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "train_step;hipLaunchKernel;[gpu:cgroup:9876];[gpu:pod:pod-abc];[gpu:launch];[gpu:event:submit:amdgpu-cs] 13"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWriteFoldedStacksIncludesAttributedKFDMemoryEvents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("backend", "linuxdrm", "testdata", "hip_kfd_observation.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	var buf bytes.Buffer
	if err := WriteFoldedStacks(&buf, snap); err != nil {
		t.Fatalf("WriteFoldedStacks: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "[gpu:event:memory:kfd-unmap-memory-from-gpu] 179787") {
		t.Fatalf("missing unmap line in %q", lines[0])
	}
	if !strings.Contains(lines[1], "[gpu:event:memory:kfd-free-memory-of-gpu] 26770") {
		t.Fatalf("missing free line in %q", lines[1])
	}
}

func TestWriteFoldedStacksIncludesRichAMDSampleFrames(t *testing.T) {
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
						Tags: map[string]string{
							"cgroup_id": "9876",
						},
					},
				},
				Exec: GPUKernelExec{
					Queue:      GPUQueueRef{Backend: "amdsample", QueueID: "compute:3"},
					KernelName: "attention_kernel",
					StartNs:    10,
					EndNs:      50,
				},
				Samples: []GPUSample{{
					StallReason: "memory_wait",
					Function:    "attention_epilogue",
					File:        "attention.hip",
					Line:        44,
					PC:          0x1234,
					Weight:      7,
				}},
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteFoldedStacks(&buf, snap); err != nil {
		t.Fatalf("WriteFoldedStacks: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "train_step;hipLaunchKernel;[gpu:cgroup:9876];[gpu:launch];[gpu:queue:compute:3];[gpu:kernel:attention_kernel];[gpu:stall:memory_wait];[gpu:function:attention_epilogue];[gpu:source:attention.hip:44];[gpu:pc:0x1234] 7"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
