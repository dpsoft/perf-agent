package gpu_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

type fakeBackend struct{ startErr error }

func (f fakeBackend) ID() gpu.GPUBackendID                     { return "fake" }
func (f fakeBackend) Capabilities() []gpu.GPUCapability { return nil }
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
