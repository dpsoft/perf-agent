package gpu

import (
	"context"
)

type Manager struct {
	backends []Backend
	timeline *Timeline
}

type ManagerConfig struct {
	LaunchEventJoinWindowNs uint64
}

func NewManager(backends []Backend, cfg *ManagerConfig) *Manager {
	timelineCfg := TimelineConfig{}
	if cfg != nil {
		timelineCfg.LaunchEventJoinWindowNs = cfg.LaunchEventJoinWindowNs
	}
	return &Manager{
		backends: backends,
		timeline: NewTimeline(timelineCfg),
	}
}

func (m *Manager) Start(ctx context.Context) error {
	for _, b := range m.backends {
		if err := b.Start(ctx, m); err != nil {
			return err
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

func (m *Manager) EmitEvent(event GPUTimelineEvent) {
	m.timeline.RecordEvent(event)
}

func (m *Manager) Snapshot() Snapshot {
	return m.timeline.Snapshot()
}
