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
	}
	if err := WriteJSONSnapshot(&buf, snap); err != nil {
		t.Fatalf("WriteJSONSnapshot: %v", err)
	}
	if !strings.Contains(buf.String(), "flash_attn_fwd") {
		t.Fatalf("missing kernel name in %q", buf.String())
	}
}
