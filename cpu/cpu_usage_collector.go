package cpu

import (
	"errors"
	"fmt"
	"log"
	"time"
	"unsafe"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/cilium/ebpf/ringbuf"
)

type CPUUsageCollector struct {
	objs         *cpuObjects
	reader       *ringbuf.Reader
	metrics      map[uint32]*PidMetrics
	lastPollTime time.Time
}

type CpuStat struct {
	CPU       uint32
	Busy      uint64
	Total     uint64
	Timestamp uint64
}

type PidStat struct {
	TGID          uint32
	_pad0         uint32 // Alignment padding
	DeltaNs       uint64 // On-CPU time
	RunqLatencyNs uint64 // Runqueue wait time (wakeup to switch)
	Cycles        uint64
	Instructions  uint64
	CacheMisses   uint64
	Timestamp     uint64
	PrevState     uint8 // Why switched out: 0=running, 1=sleep, 2=io
	Preempt       uint8 // Was preempted?
	_pad1         [6]uint8
}

// Task state constants (must match BPF)
const (
	StateRunning         = 0 // Was running, got preempted
	StateInterruptible   = 1 // Voluntary sleep (mutex, sleep())
	StateUninterruptible = 2 // I/O wait (D state)
)

// PidMetrics holds accumulated metrics for a PID
type PidMetrics struct {
	OnCPUHist         *hdrhistogram.Histogram // Time on CPU per switch
	RunqLatencyHist   *hdrhistogram.Histogram // Time waiting in runqueue
	TotalCycles       uint64
	TotalInstructions uint64
	TotalCacheMisses  uint64
	SampleCount       uint64
	// Task state counters
	PreemptedCount uint64 // Switched out while running (preempted)
	VoluntaryCount uint64 // Voluntary sleep (interruptible)
	IOWaitCount    uint64 // I/O wait (uninterruptible/D state)
}

func NewCPUUsageCollector(objs *cpuObjects) (*CPUUsageCollector, error) {
	reader, err := ringbuf.NewReader(objs.Rb)
	if err != nil {
		return nil, fmt.Errorf("opening ring buffer reader: %w", err)
	}

	return &CPUUsageCollector{
		objs:         objs,
		reader:       reader,
		metrics:      make(map[uint32]*PidMetrics),
		lastPollTime: time.Now(),
	}, nil
}

func (c *CPUUsageCollector) ReadCPUUsage() error {
	// Read all available events from the ring buffer
	for {
		rec, err := c.reader.Read()
		if err != nil {
			// No more events available (EAGAIN) or context cancelled
			if errors.Is(err, ringbuf.ErrClosed) {
				return err
			}
			// No events available, return without error
			return nil
		}

		size := len(rec.RawSample)

		switch size {
		case int(unsafe.Sizeof(PidStat{})):
			ps := (*PidStat)(unsafe.Pointer(&rec.RawSample[0]))

			// Get or create metrics for this PID
			m, exists := c.metrics[ps.TGID]
			if !exists {
				m = &PidMetrics{
					OnCPUHist:       hdrhistogram.New(0, 1000000000000000, 3), // 0-1 hour in ns
					RunqLatencyHist: hdrhistogram.New(0, 1000000000000, 3),    // 0-1000 seconds in ns
				}
				c.metrics[ps.TGID] = m
			}

			// Record on-CPU time
			if err := m.OnCPUHist.RecordValue(int64(ps.DeltaNs)); err != nil {
				log.Printf("[PID %d] Error recording on-CPU time: %v (value=%d ns = %.0f sec)",
					ps.TGID, err, ps.DeltaNs, float64(ps.DeltaNs)/1e9)
			}

			// Record runqueue latency (if available)
			if ps.RunqLatencyNs > 0 {
				if err := m.RunqLatencyHist.RecordValue(int64(ps.RunqLatencyNs)); err != nil {
					log.Printf("[PID %d] Error recording runq latency: %v (value=%d ns = %.3f ms)",
						ps.TGID, err, ps.RunqLatencyNs, float64(ps.RunqLatencyNs)/1e6)
				}
			}

			// Accumulate hardware counter values
			m.TotalCycles += ps.Cycles
			m.TotalInstructions += ps.Instructions
			m.TotalCacheMisses += ps.CacheMisses
			m.SampleCount++

			// Track task state when switched out
			switch ps.PrevState {
			case StateRunning:
				m.PreemptedCount++
			case StateInterruptible:
				m.VoluntaryCount++
			case StateUninterruptible:
				m.IOWaitCount++
			}

		case int(unsafe.Sizeof(CpuStat{})):
			cs := (*CpuStat)(unsafe.Pointer(&rec.RawSample[0]))

			if cs.Total > 0 {
				pct := float64(cs.Busy) / float64(cs.Total) * 100
				log.Printf("[CPU %d] %.2f%% busy (busy=%d, total=%d) at %s",
					cs.CPU, pct, cs.Busy, cs.Total,
					time.Unix(0, int64(cs.Timestamp)).Format(time.RFC3339Nano))
			}

		default:
			log.Printf("Unexpected ringbuf size: %d bytes (PidStat=%d, CpuStat=%d)",
				size, unsafe.Sizeof(PidStat{}), unsafe.Sizeof(CpuStat{}))
		}
	}
}

// GetMetrics returns the metrics for a specific PID
func (c *CPUUsageCollector) GetMetrics(pid uint32) *PidMetrics {
	return c.metrics[pid]
}

// GetAllMetrics returns all PID metrics
func (c *CPUUsageCollector) GetAllMetrics() map[uint32]*PidMetrics {
	return c.metrics
}

func (c *CPUUsageCollector) Close() error {
	if c.reader != nil {
		return c.reader.Close()
	}
	return nil
}

// PrintMetrics prints PMU metrics
// systemWide: true for system-wide mode, false for targeted PID mode
// perPID: true to show per-PID breakdown in system-wide mode
func (c *CPUUsageCollector) PrintMetrics(systemWide bool, perPID bool) {
	metrics := c.GetAllMetrics()

	if len(metrics) == 0 {
		fmt.Println("\nNo PMU metrics collected")
		return
	}

	if !systemWide {
		// Targeted mode (single PID)
		for pid, m := range metrics {
			fmt.Printf("\n=== PMU Metrics (PID: %d) ===\n", pid)
			printSinglePIDMetrics(m)
		}
		return
	}

	if perPID {
		// System-wide with per-PID breakdown
		fmt.Printf("\n=== PMU Metrics (System-Wide, Per-PID) ===\n")
		fmt.Printf("Profiled %d processes\n", len(metrics))
		for pid, m := range metrics {
			fmt.Printf("\n--- PID %d ---\n", pid)
			printSinglePIDMetrics(m)
		}
	} else {
		// System-wide aggregate (default)
		fmt.Printf("\n=== PMU Metrics (System-Wide) ===\n")
		printAggregateMetrics(metrics)
	}
}

// printSinglePIDMetrics prints metrics for a single PID
func printSinglePIDMetrics(m *PidMetrics) {
	fmt.Printf("Samples: %d\n", m.SampleCount)

	// On-CPU time histogram stats
	hist := m.OnCPUHist
	fmt.Printf("\nOn-CPU Time (time slice per context switch):\n")
	fmt.Printf("  Min:    %.3f ms\n", float64(hist.Min())/1e6)
	fmt.Printf("  Max:    %.3f ms\n", float64(hist.Max())/1e6)
	fmt.Printf("  Mean:   %.3f ms\n", hist.Mean()/1e6)
	fmt.Printf("  P50:    %.3f ms\n", float64(hist.ValueAtQuantile(50.0))/1e6)
	fmt.Printf("  P95:    %.3f ms\n", float64(hist.ValueAtQuantile(95.0))/1e6)
	fmt.Printf("  P99:    %.3f ms\n", float64(hist.ValueAtQuantile(99.0))/1e6)
	fmt.Printf("  P99.9:  %.3f ms\n", float64(hist.ValueAtQuantile(99.9))/1e6)

	// Runqueue latency histogram stats
	runqHist := m.RunqLatencyHist
	if runqHist.TotalCount() > 0 {
		fmt.Printf("\nRunqueue Latency (time waiting for CPU):\n")
		fmt.Printf("  Min:    %.3f ms\n", float64(runqHist.Min())/1e6)
		fmt.Printf("  Max:    %.3f ms\n", float64(runqHist.Max())/1e6)
		fmt.Printf("  Mean:   %.3f ms\n", runqHist.Mean()/1e6)
		fmt.Printf("  P50:    %.3f ms\n", float64(runqHist.ValueAtQuantile(50.0))/1e6)
		fmt.Printf("  P95:    %.3f ms\n", float64(runqHist.ValueAtQuantile(95.0))/1e6)
		fmt.Printf("  P99:    %.3f ms\n", float64(runqHist.ValueAtQuantile(99.0))/1e6)
		fmt.Printf("  P99.9:  %.3f ms\n", float64(runqHist.ValueAtQuantile(99.9))/1e6)
	}

	// Context switch reasons
	totalSwitches := m.PreemptedCount + m.VoluntaryCount + m.IOWaitCount
	if totalSwitches > 0 {
		fmt.Printf("\nContext Switch Reasons:\n")
		fmt.Printf("  Preempted (running):     %.1f%%  (%d times)\n",
			float64(m.PreemptedCount)/float64(totalSwitches)*100, m.PreemptedCount)
		fmt.Printf("  Voluntary (sleep/mutex): %.1f%%  (%d times)\n",
			float64(m.VoluntaryCount)/float64(totalSwitches)*100, m.VoluntaryCount)
		fmt.Printf("  I/O Wait (D state):      %.1f%%  (%d times)\n",
			float64(m.IOWaitCount)/float64(totalSwitches)*100, m.IOWaitCount)
	}

	// Hardware counters
	if m.TotalCycles > 0 || m.TotalInstructions > 0 {
		fmt.Printf("\nHardware Counters:\n")
		fmt.Printf("  Total Cycles:       %d\n", m.TotalCycles)
		fmt.Printf("  Total Instructions: %d\n", m.TotalInstructions)
		fmt.Printf("  Total Cache Misses: %d\n", m.TotalCacheMisses)

		if m.TotalCycles > 0 {
			ipc := float64(m.TotalInstructions) / float64(m.TotalCycles)
			fmt.Printf("  IPC (Instr/Cycle):  %.3f\n", ipc)
		}
		if m.TotalInstructions > 0 {
			missRate := float64(m.TotalCacheMisses) / float64(m.TotalInstructions) * 1000
			fmt.Printf("  Cache Misses/1K Instr: %.3f\n", missRate)
		}
	} else {
		fmt.Printf("\nHardware Counters: not available\n")
	}
}

// printAggregateMetrics prints aggregated metrics for system-wide mode
func printAggregateMetrics(metrics map[uint32]*PidMetrics) {
	var totalSamples uint64
	var totalCycles, totalInstructions, totalCacheMisses uint64
	var totalPreempted, totalVoluntary, totalIOWait uint64

	// Aggregate histograms for latency metrics
	aggOnCPUHist := hdrhistogram.New(0, 1000000000000000, 3)
	aggRunqHist := hdrhistogram.New(0, 1000000000000, 3)

	for _, m := range metrics {
		totalSamples += m.SampleCount
		totalCycles += m.TotalCycles
		totalInstructions += m.TotalInstructions
		totalCacheMisses += m.TotalCacheMisses
		totalPreempted += m.PreemptedCount
		totalVoluntary += m.VoluntaryCount
		totalIOWait += m.IOWaitCount

		// Merge histograms
		aggOnCPUHist.Merge(m.OnCPUHist)
		aggRunqHist.Merge(m.RunqLatencyHist)
	}

	fmt.Printf("\nPerformance counter stats for 'system wide':\n\n")
	fmt.Printf("  Processes profiled:     %d\n", len(metrics))
	fmt.Printf("  Total samples:          %d\n", totalSamples)

	// On-CPU time histogram stats
	if aggOnCPUHist.TotalCount() > 0 {
		fmt.Printf("\nOn-CPU Time (time slice per context switch):\n")
		fmt.Printf("  Min:    %.3f ms\n", float64(aggOnCPUHist.Min())/1e6)
		fmt.Printf("  Max:    %.3f ms\n", float64(aggOnCPUHist.Max())/1e6)
		fmt.Printf("  Mean:   %.3f ms\n", aggOnCPUHist.Mean()/1e6)
		fmt.Printf("  P50:    %.3f ms\n", float64(aggOnCPUHist.ValueAtQuantile(50.0))/1e6)
		fmt.Printf("  P95:    %.3f ms\n", float64(aggOnCPUHist.ValueAtQuantile(95.0))/1e6)
		fmt.Printf("  P99:    %.3f ms\n", float64(aggOnCPUHist.ValueAtQuantile(99.0))/1e6)
		fmt.Printf("  P99.9:  %.3f ms\n", float64(aggOnCPUHist.ValueAtQuantile(99.9))/1e6)
	}

	// Runqueue latency histogram stats
	if aggRunqHist.TotalCount() > 0 {
		fmt.Printf("\nRunqueue Latency (time waiting for CPU):\n")
		fmt.Printf("  Min:    %.3f ms\n", float64(aggRunqHist.Min())/1e6)
		fmt.Printf("  Max:    %.3f ms\n", float64(aggRunqHist.Max())/1e6)
		fmt.Printf("  Mean:   %.3f ms\n", aggRunqHist.Mean()/1e6)
		fmt.Printf("  P50:    %.3f ms\n", float64(aggRunqHist.ValueAtQuantile(50.0))/1e6)
		fmt.Printf("  P95:    %.3f ms\n", float64(aggRunqHist.ValueAtQuantile(95.0))/1e6)
		fmt.Printf("  P99:    %.3f ms\n", float64(aggRunqHist.ValueAtQuantile(99.0))/1e6)
		fmt.Printf("  P99.9:  %.3f ms\n", float64(aggRunqHist.ValueAtQuantile(99.9))/1e6)
	}

	// Context switch reasons
	totalSwitches := totalPreempted + totalVoluntary + totalIOWait
	if totalSwitches > 0 {
		fmt.Printf("\nContext Switch Reasons (aggregate):\n")
		fmt.Printf("  Preempted (running):     %.1f%%  (%d times)\n",
			float64(totalPreempted)/float64(totalSwitches)*100, totalPreempted)
		fmt.Printf("  Voluntary (sleep/mutex): %.1f%%  (%d times)\n",
			float64(totalVoluntary)/float64(totalSwitches)*100, totalVoluntary)
		fmt.Printf("  I/O Wait (D state):      %.1f%%  (%d times)\n",
			float64(totalIOWait)/float64(totalSwitches)*100, totalIOWait)
	}

	fmt.Printf("\nHardware Counters:\n")
	fmt.Printf("  Total Cycles:           %d\n", totalCycles)
	fmt.Printf("  Total Instructions:     %d\n", totalInstructions)
	fmt.Printf("  Total Cache Misses:     %d\n", totalCacheMisses)

	if totalCycles > 0 {
		ipc := float64(totalInstructions) / float64(totalCycles)
		fmt.Printf("  IPC (Instr/Cycle):      %.2f\n", ipc)
	}
	if totalInstructions > 0 {
		missRate := float64(totalCacheMisses) / float64(totalInstructions) * 1000
		fmt.Printf("  Cache Misses/1K Instr:  %.2f\n", missRate)
	}
}
