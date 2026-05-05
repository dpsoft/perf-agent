package amdsample

import (
	"context"
	"fmt"
	"io"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpu/backend/stream"
)

type Config struct {
	Reader io.Reader
}

type Backend struct {
	inner *stream.Backend
}

func New(cfg Config) (*Backend, error) {
	if cfg.Reader == nil {
		return nil, fmt.Errorf("amd sample reader is required")
	}
	return &Backend{inner: stream.New(cfg.Reader)}, nil
}

func (b *Backend) ID() gpu.GPUBackendID {
	return gpu.BackendAMDSample
}

func (b *Backend) EventBackends() []gpu.GPUBackendID { return nil }

func (b *Backend) Capabilities() []gpu.GPUCapability {
	return []gpu.GPUCapability{
		gpu.CapabilityExecTimeline,
		gpu.CapabilityPCSampling,
		gpu.CapabilityStallReasons,
		gpu.CapabilitySourceMap,
	}
}

func (b *Backend) Start(ctx context.Context, sink gpu.EventSink) error {
	return b.inner.Start(ctx, sink)
}

func (b *Backend) Stop(ctx context.Context) error {
	return b.inner.Stop(ctx)
}

func (b *Backend) Close() error {
	return b.inner.Close()
}
