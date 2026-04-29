package gpu_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type fakeBackend struct {
	startErr      error
	eventBackends []gpu.GPUBackendID
}

func (f fakeBackend) ID() gpu.GPUBackendID                     { return "fake" }
func (f fakeBackend) EventBackends() []gpu.GPUBackendID       { return append([]gpu.GPUBackendID(nil), f.eventBackends...) }
func (f fakeBackend) Capabilities() []gpu.GPUCapability       { return nil }
func (f fakeBackend) Start(context.Context, gpu.EventSink) error {
	return f.startErr
}
func (f fakeBackend) Stop(context.Context) error { return nil }
func (f fakeBackend) Close() error               { return nil }

func TestManagerStartPropagatesCause(t *testing.T) {
	want := errors.New("boom")
	m := gpu.NewManager([]gpu.Backend{fakeBackend{startErr: want}}, nil)
	err := m.Start(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Start error = %v, want %v", err, want)
	}
}

func TestManagerEventBackendsDeduplicatesAndSorts(t *testing.T) {
	m := gpu.NewManager([]gpu.Backend{
		fakeBackend{eventBackends: []gpu.GPUBackendID{gpu.BackendLinuxKFD, gpu.BackendLinuxDRM}},
		fakeBackend{eventBackends: []gpu.GPUBackendID{gpu.BackendLinuxDRM}},
	}, nil)

	got := m.EventBackends()
	want := []gpu.GPUBackendID{gpu.BackendLinuxDRM, gpu.BackendLinuxKFD}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("eventBackends[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
