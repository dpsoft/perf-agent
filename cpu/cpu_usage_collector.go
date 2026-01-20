package cpu

import (
	"fmt"
	"log"
	"time"
	"unsafe"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/cilium/ebpf/ringbuf"
)

type CPUUsageCollector struct {
	objs         *CPUObjects
	reader       *ringbuf.Reader
	histograms   map[uint32]*hdrhistogram.Histogram // deprecated, use metrics
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
	TGID        uint32
	DeltaNs     uint64
	Cycles      uint64
	Instructions uint64
	CacheMisses uint64
	Timestamp   uint64
}

// PidMetrics holds accumulated metrics for a PID
type PidMetrics struct {
	TimeHist        *hdrhistogram.Histogram
	TotalCycles     uint64
	TotalInstructions uint64
	TotalCacheMisses  uint64
	SampleCount     uint64
}

func NewCPUUsageCollector(objs *CPUObjects) (*CPUUsageCollector, error) {
	reader, err := ringbuf.NewReader(objs.Rb)
	if err != nil {
		return nil, fmt.Errorf("opening ring buffer reader: %w", err)
	}

	return &CPUUsageCollector{
		objs:         objs,
		reader:       reader,
		histograms:   make(map[uint32]*hdrhistogram.Histogram),
		metrics:      make(map[uint32]*PidMetrics),
		lastPollTime: time.Now(),
	}, nil
}

func (c *CPUUsageCollector) ReadCPUUsage() error {
	// Read all available events from ring buffer
	for {
		rec, err := c.reader.Read()
		if err != nil {
			// No more events available (EAGAIN) or context cancelled
			if err == ringbuf.ErrClosed {
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
				// Keep histograms map in sync for backward compatibility
				c.histograms[ps.TGID] = m.TimeHist
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

func (c *CPUUsageCollector) GetHistogram(pid uint32) *hdrhistogram.Histogram {
	return c.histograms[pid]
}

func (c *CPUUsageCollector) GetAllHistograms() map[uint32]*hdrhistogram.Histogram {
	return c.histograms
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
