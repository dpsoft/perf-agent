package gpu

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteJSONSnapshot(t *testing.T) {
	var buf bytes.Buffer
	snap := Snapshot{
		Launches: []GPUKernelLaunch{
			{Correlation: CorrelationID{Backend: "stream", Value: "c1"}, KernelName: "flash_attn_fwd"},
		},
		Executions: []ExecutionView{
			{Exec: GPUKernelExec{KernelName: "flash_attn_fwd", StartNs: 1, EndNs: 2}},
		},
		Events: []GPUTimelineEvent{
			{
				Backend: "linuxdrm",
				Kind:    TimelineEventIOCtl,
				Name:    "submit",
				TimeNs:  10,
				PID:     11,
				TID:     12,
			},
		},
		JoinStats: JoinStats{
			LaunchCount:                  1,
			UnmatchedCandidateEventCount: 1,
		},
	}
	if err := WriteJSONSnapshot(&buf, snap); err != nil {
		t.Fatalf("WriteJSONSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), "flash_attn_fwd") {
		t.Fatalf("missing kernel name in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"launches\"") {
		t.Fatalf("missing launches field in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"events\"") {
		t.Fatalf("missing events field in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"join_stats\"") {
		t.Fatalf("missing join_stats field in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"submit\"") {
		t.Fatalf("missing timeline event in %q", buf.String())
	}
}

func TestWriteJSONAttributions(t *testing.T) {
	var buf bytes.Buffer
	snap := Snapshot{
		Attributions: []WorkloadAttribution{
			{
				CgroupID:            "1000",
				PodUID:              "pod-a",
				KernelNames:         []string{"alpha_kernel"},
				LaunchCount:         1,
				ExactJoinCount:      1,
				ExecutionCount:      1,
				ExecutionDurationNs: 60,
				SampleWeight:        11,
			},
		},
	}
	if err := WriteJSONAttributions(&buf, snap); err != nil {
		t.Fatalf("WriteJSONAttributions: %v", err)
	}
	if strings.Contains(buf.String(), "\"executions\"") {
		t.Fatalf("unexpected snapshot fields in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"pod_uid\":\"pod-a\"") {
		t.Fatalf("missing pod uid in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"kernel_names\":[\"alpha_kernel\"]") {
		t.Fatalf("missing kernel names in %q", buf.String())
	}
}
