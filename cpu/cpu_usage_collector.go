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
	TGID         uint32
	DeltaNs      uint64
	Cycles       uint64
	Instructions uint64
	CacheMisses  uint64
	Timestamp    uint64
}

// PidMetrics holds accumulated metrics for a PID
type PidMetrics struct {
	TimeHist          *hdrhistogram.Histogram
	TotalCycles       uint64
	TotalInstructions uint64
	TotalCacheMisses  uint64
	SampleCount       uint64
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
					TimeHist: hdrhistogram.New(0, 1000000000000000, 3), // 0-1 hour in ns
				}
				c.metrics[ps.TGID] = m
			}

			if err := m.TimeHist.RecordValue(int64(ps.DeltaNs)); err != nil {
				log.Printf("[PID %d] Error recording: %v (value=%d ns = %.0f sec)",
					ps.TGID, err, ps.DeltaNs, float64(ps.DeltaNs)/1e9)
			}

			// Accumulate hardware counter values
			m.TotalCycles += ps.Cycles
			m.TotalInstructions += ps.Instructions
			m.TotalCacheMisses += ps.CacheMisses
			m.SampleCount++

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

	// Time histogram stats
	hist := m.TimeHist
	fmt.Printf("\nScheduling Latency (time on CPU per switch):\n")
	fmt.Printf("  Min:    %.3f ms\n", float64(hist.Min())/1e6)
	fmt.Printf("  Max:    %.3f ms\n", float64(hist.Max())/1e6)
	fmt.Printf("  Mean:   %.3f ms\n", hist.Mean()/1e6)
	fmt.Printf("  P50:    %.3f ms\n", float64(hist.ValueAtQuantile(50.0))/1e6)
	fmt.Printf("  P95:    %.3f ms\n", float64(hist.ValueAtQuantile(95.0))/1e6)
	fmt.Printf("  P99:    %.3f ms\n", float64(hist.ValueAtQuantile(99.0))/1e6)
	fmt.Printf("  P99.9:  %.3f ms\n", float64(hist.ValueAtQuantile(99.9))/1e6)

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

	for _, m := range metrics {
		totalSamples += m.SampleCount
		totalCycles += m.TotalCycles
		totalInstructions += m.TotalInstructions
		totalCacheMisses += m.TotalCacheMisses
	}

	fmt.Printf("\nPerformance counter stats for 'system wide':\n\n")
	fmt.Printf("  Processes profiled:     %d\n", len(metrics))
	fmt.Printf("  Total samples:          %d\n", totalSamples)
	fmt.Printf("\n")
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
