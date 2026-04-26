package linuxdrm

import (
	"context"
	"errors"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type sink struct{}

func (s *sink) EmitLaunch(gpu.GPUKernelLaunch)   {}
func (s *sink) EmitExec(gpu.GPUKernelExec)       {}
func (s *sink) EmitCounter(gpu.GPUCounterSample) {}
func (s *sink) EmitSample(gpu.GPUSample)         {}
func (s *sink) EmitEvent(gpu.GPUTimelineEvent)   {}

func TestNewRejectsMissingPID(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNewAcceptsMinimalConfig(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("expected backend")
	}
}

func TestBackendIDAndCapabilities(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.ID(); got != "linuxdrm" {
		t.Fatalf("ID=%q", got)
	}
	got := b.Capabilities()
	want := []gpu.GPUCapability{gpu.CapabilityLifecycleTimeline}
	if len(got) != len(want) {
		t.Fatalf("len(capabilities)=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cap[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestStartRejectsNilSink(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Start(t.Context(), nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestStopPropagatesRunError(t *testing.T) {
	want := errors.New("boom")
	b, err := New(Config{
		PID: 123,
		testRun: func(context.Context, gpu.EventSink) error {
			return want
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := b.Start(context.Background(), &sink{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); !errors.Is(err, want) {
		t.Fatalf("Stop error=%v want %v", err, want)
	}
}
