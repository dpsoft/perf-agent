package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type sink struct {
	launches int
	execs    int
	samples  int
	events   int
}

func (s *sink) EmitLaunch(gpu.GPUKernelLaunch)   { s.launches++ }
func (s *sink) EmitExec(gpu.GPUKernelExec)       { s.execs++ }
func (s *sink) EmitCounter(gpu.GPUCounterSample) {}
func (s *sink) EmitSample(gpu.GPUSample)         { s.samples++ }
func (s *sink) EmitEvent(gpu.GPUTimelineEvent)   { s.events++ }

func TestReplayBackendEmitsFixture(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "replay", "flash_attn.json")
	b, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.ID(); got != gpu.BackendReplay {
		t.Fatalf("ID=%q", got)
	}
	if got := b.EventBackends(); len(got) != 0 {
		t.Fatalf("EventBackends()=%v", got)
	}
	var s sink
	if err := b.Start(context.Background(), &s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.launches != 1 || s.execs != 1 || s.samples != 1 {
		t.Fatalf("counts: %+v", s)
	}
}

func TestReplayBackendEmitsTimelineEventFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.json")
	data := []byte(`[
  {
    "kind": "event",
    "event": {
      "backend": "linuxdrm",
      "kind": "submit",
      "name": "amdgpu-cs",
      "time_ns": 130,
      "duration_ns": 13,
      "pid": 4242,
      "tid": 4243,
      "source": "replay"
    }
  }
]`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	b, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var s sink
	if err := b.Start(context.Background(), &s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.events != 1 {
		t.Fatalf("events=%d", s.events)
	}
}
