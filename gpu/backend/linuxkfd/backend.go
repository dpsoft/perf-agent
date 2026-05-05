package linuxkfd

import (
	"context"

	"github.com/dpsoft/perf-agent/gpu"
	linuxdrm "github.com/dpsoft/perf-agent/gpu/backend/linuxdrm"
)

type Config struct {
	PID int
}

type Backend struct {
	inner *linuxdrm.Backend
}

func New(cfg Config) (*Backend, error) {
	inner, err := linuxdrm.New(linuxdrm.Config{
		PID:           cfg.PID,
		EventBackends: []gpu.GPUBackendID{gpu.BackendLinuxKFD},
	})
	if err != nil {
		return nil, err
	}
	return &Backend{inner: inner}, nil
}

func (b *Backend) ID() gpu.GPUBackendID {
	return gpu.BackendLinuxKFD
}

func (b *Backend) EventBackends() []gpu.GPUBackendID {
	return []gpu.GPUBackendID{gpu.BackendLinuxKFD}
}

func (b *Backend) Capabilities() []gpu.GPUCapability {
	return b.inner.Capabilities()
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
