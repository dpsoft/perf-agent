package gpu

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteJSONSnapshot(t *testing.T) {
	var buf bytes.Buffer
	snap := Snapshot{
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
	}
	if err := WriteJSONSnapshot(&buf, snap); err != nil {
		t.Fatalf("WriteJSONSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), "flash_attn_fwd") {
		t.Fatalf("missing kernel name in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"events\"") {
		t.Fatalf("missing events field in %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"submit\"") {
		t.Fatalf("missing timeline event in %q", buf.String())
	}
}
