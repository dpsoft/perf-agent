package linuxdrm

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/dpsoft/perf-agent/gpu"
)

var (
	errEventSinkRequired = errors.New("event sink is required")
	errBackendStarted    = errors.New("linuxdrm backend already started")
	baseCapabilities     = []gpu.GPUCapability{
		gpu.CapabilityLifecycleTimeline,
	}
)

type Backend struct {
	cfg Config

	mu      sync.Mutex
	started bool
	done    chan struct{}
	cancel  context.CancelCauseFunc
	runErr  error
}

func New(cfg Config) (*Backend, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Backend{cfg: cfg}, nil
}

func (b *Backend) ID() gpu.GPUBackendID {
	return "linuxdrm"
}

func (b *Backend) Capabilities() []gpu.GPUCapability {
	return slices.Clone(baseCapabilities)
}

func (b *Backend) Start(ctx context.Context, sink gpu.EventSink) error {
	if sink == nil {
		return errEventSinkRequired
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return errBackendStarted
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	b.cancel = cancel
	b.done = make(chan struct{})
	b.runErr = nil
	b.started = true

	go b.run(runCtx, sink)
	return nil
}

func (b *Backend) run(ctx context.Context, sink gpu.EventSink) {
	defer close(b.done)

	if b.cfg.testRun != nil {
		if err := b.cfg.testRun(ctx, sink); err != nil {
			b.setRunErr(err)
		}
		return
	}

	<-ctx.Done()
}

func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	done := b.done
	cancel := b.cancel
	b.mu.Unlock()

	if done == nil {
		return nil
	}
	if cancel != nil {
		cancel(context.Canceled)
	}

	select {
	case <-done:
		return b.err()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (b *Backend) Close() error {
	return nil
}

func (b *Backend) setRunErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runErr = err
}

func (b *Backend) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runErr
}
