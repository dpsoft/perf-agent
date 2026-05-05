package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	dir := t.TempDir()
	path := filepath.Join(dir, "flash_attn.json")
	data := []byte(`{
  "version": 1,
  "events": [
    {
      "kind": "launch",
      "correlation": { "backend": "replay", "value": "corr-1" },
      "queue": { "backend": "replay", "queue_id": "q7" },
      "kernel_name": "flash_attn_fwd",
      "time_ns": 100,
      "launch": {
        "pid": 101,
        "tid": 202,
        "time_ns": 100,
        "cpu_stack": [
          { "Name": "train_step" },
          { "Name": "cudaLaunchKernel" }
        ]
      }
    },
    {
      "kind": "exec",
      "correlation": { "backend": "replay", "value": "corr-1" },
      "queue": { "backend": "replay", "queue_id": "q7" },
      "kernel_name": "flash_attn_fwd",
      "start_ns": 120,
      "end_ns": 200
    },
    {
      "kind": "sample",
      "correlation": { "backend": "replay", "value": "corr-1" },
      "device": { "backend": "replay", "device_id": "0", "name": "replay-gpu" },
      "time_ns": 140,
      "kernel_name": "flash_attn_fwd",
      "stall_reason": "memory_throttle",
      "weight": 7
    }
  ]
}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
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
	data := []byte(`{
  "version": 1,
  "events": [
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
  ]
}`)
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

func TestReplayBackendRejectsUnversionedFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	data := []byte(`[
  {
    "kind": "exec",
    "correlation": { "backend": "replay", "value": "corr-1" },
    "kernel_name": "flash_attn_fwd",
    "start_ns": 120,
    "end_ns": 200
  }
]`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	b, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = b.Start(context.Background(), &sink{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "versioned replay fixture") {
		t.Fatalf("err=%v", err)
	}
}

func TestReplayBackendRejectsUnsupportedClockDomain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-domain.json")
	data := []byte(`{
  "version": 1,
  "events": [
    {
      "kind": "exec",
      "correlation": { "backend": "replay", "value": "corr-1" },
      "kernel_name": "flash_attn_fwd",
      "clock_domain": "gpu-device",
      "start_ns": 120,
      "end_ns": 200
    }
  ]
}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	b, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = b.Start(context.Background(), &sink{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unsupported clock domain") {
		t.Fatalf("err=%v", err)
	}
}
