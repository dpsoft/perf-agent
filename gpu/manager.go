package gpu

import (
	"context"
	"log"
	"os"
)

type Manager struct {
	backends []Backend
	timeline *Timeline
}

type ManagerConfig struct {
	LaunchEventJoinWindowNs uint64
}

func debugGPULivef(format string, args ...any) {
	if os.Getenv("PERF_AGENT_DEBUG_GPU_LIVE") == "" {
		return
	}
	log.Printf("gpu-live-debug: "+format, args...)
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
		debugGPULivef("stopping gpu backend %d (%s)", i, m.backends[i].ID())
		if err := m.backends[i].Stop(ctx); err != nil {
			debugGPULivef("gpu backend %d (%s) stop error: %v", i, m.backends[i].ID(), err)
			return err
		}
		debugGPULivef("gpu backend %d (%s) stopped", i, m.backends[i].ID())
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
