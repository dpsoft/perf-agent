package stream

import (
	"context"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type sink struct {
	launches []gpu.GPUKernelLaunch
	execs    []gpu.GPUKernelExec
	counters []gpu.GPUCounterSample
	samples  []gpu.GPUSample
	events   []gpu.GPUTimelineEvent
}

func (s *sink) EmitLaunch(event gpu.GPUKernelLaunch)   { s.launches = append(s.launches, event) }
func (s *sink) EmitExec(event gpu.GPUKernelExec)       { s.execs = append(s.execs, event) }
func (s *sink) EmitCounter(event gpu.GPUCounterSample) { s.counters = append(s.counters, event) }
func (s *sink) EmitSample(event gpu.GPUSample)         { s.samples = append(s.samples, event) }
func (s *sink) EmitEvent(event gpu.GPUTimelineEvent)   { s.events = append(s.events, event) }

func TestStreamBackendEmitsEventsFromReader(t *testing.T) {
	src := strings.NewReader(
		"{\"kind\":\"launch\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":100}\n" +
			"{\"kind\":\"exec\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"start_ns\":120,\"end_ns\":200}\n" +
			"{\"kind\":\"sample\",\"correlation\":{\"backend\":\"stream\",\"value\":\"c1\"},\"kernel_name\":\"flash_attn_fwd\",\"time_ns\":150,\"stall_reason\":\"memory_throttle\",\"weight\":7}\n" +
			"{\"kind\":\"event\",\"event\":{\"backend\":\"linuxdrm\",\"kind\":\"submit\",\"name\":\"amdgpu-cs\",\"time_ns\":130,\"duration_ns\":13,\"pid\":4242,\"tid\":4243,\"source\":\"replay\"}}\n",
	)
	b := New(src)
	if got := b.ID(); got != gpu.BackendStream {
		t.Fatalf("ID=%q", got)
	}
	if got := b.EventBackends(); len(got) != 0 {
		t.Fatalf("EventBackends()=%v", got)
	}
	var s sink

	if err := b.Start(t.Context(), &s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := len(s.launches); got != 1 {
		t.Fatalf("launches=%d", got)
	}
	if got := len(s.execs); got != 1 {
		t.Fatalf("execs=%d", got)
	}
	if got := len(s.samples); got != 1 {
		t.Fatalf("samples=%d", got)
	}
	if got := len(s.events); got != 1 {
		t.Fatalf("events=%d", got)
	}
}

func TestStreamBackendEOF(t *testing.T) {
	b := New(strings.NewReader(""))

	if err := b.Start(t.Context(), &sink{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStreamBackendPropagatesDecodeError(t *testing.T) {
	b := New(strings.NewReader("{\"kind\":\"launch\",\"time_ns\":100}\n"))

	if err := b.Start(context.Background(), &sink{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Stop(t.Context()); err == nil {
		t.Fatal("expected error")
	}
}
