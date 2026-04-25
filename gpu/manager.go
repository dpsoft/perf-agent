package gpu

import (
	"context"
)

type Manager struct {
	backends []Backend
	timeline *Timeline
}

func NewManager(backends []Backend, _ any) *Manager {
	return &Manager{
		backends: backends,
		timeline: NewTimeline(),
	}
}

func (m *Manager) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	for _, b := range m.backends {
		if err := b.Start(runCtx, m); err != nil {
			cancel(err)
			return context.Cause(runCtx)
		}
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context) error {
	for i := len(m.backends) - 1; i >= 0; i-- {
		if err := m.backends[i].Stop(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Close() error {
	for i := len(m.backends) - 1; i >= 0; i-- {
		if err := m.backends[i].Close(); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) EmitLaunch(launch GPUKernelLaunch) {
	m.timeline.RecordLaunch(launch)
}

func (m *Manager) EmitExec(exec GPUKernelExec) {
	m.timeline.RecordExec(exec)
}

func (m *Manager) EmitCounter(counter GPUCounterSample) {
	m.timeline.RecordCounter(counter)
}

func (m *Manager) EmitSample(sample GPUSample) {
	m.timeline.RecordSample(sample)
}
