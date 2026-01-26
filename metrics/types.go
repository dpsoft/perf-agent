// Package metrics provides types and interfaces for exporting performance metrics.
package metrics

import "time"

// MetricsSnapshot represents a point-in-time capture of performance metrics.
type MetricsSnapshot struct {
	Timestamp  time.Time
	SystemWide bool
	Processes  map[uint32]*ProcessMetrics
}

// ProcessMetrics holds all metrics for a single process.
type ProcessMetrics struct {
	PID              uint32
	SampleCount      uint64
	OnCPUStats       LatencyStats
	RunqueueStats    LatencyStats
	ContextSwitches  ContextSwitchStats
	HardwareCounters HardwareCounterStats
}

// LatencyStats contains statistical summary of latency measurements.
type LatencyStats struct {
	Min   int64
	Max   int64
	P50   int64
	P95   int64
	P99   int64
	P999  int64
	Mean  float64
	Count int64
}

// ContextSwitchStats tracks reasons for context switches.
type ContextSwitchStats struct {
	PreemptedCount  uint64 // Switched out while running (preempted)
	VoluntaryCount  uint64 // Voluntary sleep (interruptible)
	IOWaitCount     uint64 // I/O wait (uninterruptible/D state)
}

// HardwareCounterStats contains hardware PMU counter values.
type HardwareCounterStats struct {
	Available    bool
	Cycles       uint64
	Instructions uint64
	CacheMisses  uint64
	IPC          float64 // Instructions per cycle
	MissRate     float64 // Cache misses per 1K instructions
}

// NewMetricsSnapshot creates a new empty metrics snapshot.
func NewMetricsSnapshot(systemWide bool) *MetricsSnapshot {
	return &MetricsSnapshot{
		Timestamp:  time.Now(),
		SystemWide: systemWide,
		Processes:  make(map[uint32]*ProcessMetrics),
	}
}

// AddProcess adds or updates metrics for a process.
func (m *MetricsSnapshot) AddProcess(pid uint32, pm *ProcessMetrics) {
	m.Processes[pid] = pm
}

// TotalSamples returns the total number of samples across all processes.
func (m *MetricsSnapshot) TotalSamples() uint64 {
	var total uint64
	for _, pm := range m.Processes {
		total += pm.SampleCount
	}
	return total
}

// AggregateHardwareCounters returns aggregated hardware counter stats.
func (m *MetricsSnapshot) AggregateHardwareCounters() HardwareCounterStats {
	var agg HardwareCounterStats
	for _, pm := range m.Processes {
		if pm.HardwareCounters.Available {
			agg.Available = true
			agg.Cycles += pm.HardwareCounters.Cycles
			agg.Instructions += pm.HardwareCounters.Instructions
			agg.CacheMisses += pm.HardwareCounters.CacheMisses
		}
	}

	if agg.Cycles > 0 {
		agg.IPC = float64(agg.Instructions) / float64(agg.Cycles)
	}
	if agg.Instructions > 0 {
		agg.MissRate = float64(agg.CacheMisses) / float64(agg.Instructions) * 1000
	}

	return agg
}

// AggregateContextSwitches returns aggregated context switch stats.
func (m *MetricsSnapshot) AggregateContextSwitches() ContextSwitchStats {
	var agg ContextSwitchStats
	for _, pm := range m.Processes {
		agg.PreemptedCount += pm.ContextSwitches.PreemptedCount
		agg.VoluntaryCount += pm.ContextSwitches.VoluntaryCount
		agg.IOWaitCount += pm.ContextSwitches.IOWaitCount
	}
	return agg
}
