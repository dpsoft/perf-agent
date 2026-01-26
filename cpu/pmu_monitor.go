package cpu

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/dpsoft/perf-agent/metrics"
)

// PMUMonitor handles PMU hardware counter monitoring
type PMUMonitor struct {
	objs        *cpuObjects
	hwPerf      *HardwarePerfEvents
	tp          link.Link
	collector   *CPUUsageCollector
	ticker      *time.Ticker
	stopPolling chan struct{}
}

// NewPMUMonitor creates a new PMU monitor
func NewPMUMonitor(pid int, systemWide bool, cpus []uint) (*PMUMonitor, error) {
	spec, err := loadCpu()
	if err != nil {
		return nil, fmt.Errorf("load CPU spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.RewriteConstants(map[string]interface{}{
		"system_wide": systemWide,
	}); err != nil {
		return nil, fmt.Errorf("rewrite constants: %w", err)
	}

	cpuObjs := &cpuObjects{}
	if err := spec.LoadAndAssign(cpuObjs, nil); err != nil {
		return nil, fmt.Errorf("load CPU objects: %w", err)
	}

	// Initialize hardware perf counters
	cpuList := make([]int, len(cpus))
	for i, id := range cpus {
		cpuList[i] = int(id)
	}

	var hwPerf *HardwarePerfEvents
	hwPerf, err = NewHardwarePerfEvents(cpuList)
	if err != nil {
		log.Printf("Hardware perf counters unavailable (running in VM?): %v", err)
	} else {
		err = hwPerf.AttachToMaps(
			cpuObjs.CpuCyclesReader,
			cpuObjs.CpuInstructionsReader,
			cpuObjs.CacheMissesReader,
		)
		if err != nil {
			log.Printf("Failed to attach HW counters to maps: %v", err)
			_ = hwPerf.Close()
			hwPerf = nil
		} else {
			if err := hwPerf.EnableInBPF(cpuObjs.HwCountersEnabled); err != nil {
				log.Printf("Failed to enable HW counters in eBPF: %v", err)
			} else {
				log.Println("Hardware perf counters enabled (cycles, instructions, cache misses)")
			}
		}
	}

	tp, err := link.AttachTracing(link.TracingOptions{
		Program: cpuObjs.HandleSwitch,
	})
	if err != nil {
		if hwPerf != nil {
			_ = hwPerf.Close()
		}
		_ = cpuObjs.Close()
		return nil, fmt.Errorf("attach tp_btf sched_switch: %w", err)
	}

	collector, err := NewCPUUsageCollector(cpuObjs)
	if err != nil {
		_ = tp.Close()
		if hwPerf != nil {
			_ = hwPerf.Close()
		}
		_ = cpuObjs.Close()
		return nil, fmt.Errorf("create CPU usage collector: %w", err)
	}

	// Configure PID filter only for targeted mode
	if !systemWide {
		trackValue := uint8(1)
		if err := cpuObjs.PidFilter.Update(uint32(pid), &trackValue, ebpf.UpdateAny); err != nil {
			_ = tp.Close()
			if hwPerf != nil {
				_ = hwPerf.Close()
			}
			_ = cpuObjs.Close()
			return nil, fmt.Errorf("configure PID filter: %w", err)
		}
	}

	// Start polling goroutine
	ticker := time.NewTicker(1 * time.Second)
	stopPolling := make(chan struct{})

	monitor := &PMUMonitor{
		objs:        cpuObjs,
		hwPerf:      hwPerf,
		tp:          tp,
		collector:   collector,
		ticker:      ticker,
		stopPolling: stopPolling,
	}

	go monitor.pollLoop()

	return monitor, nil
}

func (m *PMUMonitor) pollLoop() {
	for {
		select {
		case <-m.ticker.C:
			if err := m.collector.ReadCPUUsage(); err != nil {
				log.Printf("Error reading CPU usage: %v", err)
			}
		case <-m.stopPolling:
			return
		}
	}
}

// PrintMetrics prints collected metrics
func (m *PMUMonitor) PrintMetrics(systemWide, perPID bool) {
	m.collector.PrintMetrics(systemWide, perPID)
}

// GetMetricsSnapshot returns a snapshot of the current metrics.
func (m *PMUMonitor) GetMetricsSnapshot(systemWide bool) *metrics.MetricsSnapshot {
	return m.collector.GetSnapshot(systemWide)
}

// ExportMetrics exports metrics using the provided exporters.
func (m *PMUMonitor) ExportMetrics(ctx context.Context, systemWide bool, exporters ...metrics.Exporter) error {
	return m.collector.ExportMetrics(ctx, systemWide, exporters...)
}

// Close releases all resources associated with the monitor
func (m *PMUMonitor) Close() {
	close(m.stopPolling)
	m.ticker.Stop()
	_ = m.tp.Close()
	if m.hwPerf != nil {
		_ = m.hwPerf.Close()
	}
	_ = m.objs.Close()
}
