package replay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type sink struct {
	launches int
	execs    int
	samples  int
}

func (s *sink) EmitLaunch(gpu.GPUKernelLaunch)   { s.launches++ }
func (s *sink) EmitExec(gpu.GPUKernelExec)       { s.execs++ }
func (s *sink) EmitCounter(gpu.GPUCounterSample) {}
func (s *sink) EmitSample(gpu.GPUSample)         { s.samples++ }
func (s *sink) EmitEvent(gpu.GPUTimelineEvent)   {}

func TestReplayBackendEmitsFixture(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "replay", "flash_attn.json")
	b, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var s sink
	if err := b.Start(context.Background(), &s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.launches != 1 || s.execs != 1 || s.samples != 1 {
		t.Fatalf("counts: %+v", s)
	}
}
