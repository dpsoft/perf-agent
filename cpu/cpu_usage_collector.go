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
	histograms   map[uint32]*hdrhistogram.Histogram
	lastPollTime time.Time
}

// Matches cpu_stat_s in BPF (must match exact layout)
type CpuStat struct {
	CPU       uint32
	Busy      uint64
	Total     uint64
	Timestamp uint64
}

// Matches pid_stat_s in BPF (must match exact layout)
type PidStat struct {
	TGID      uint32
	DeltaNs   uint64
	Timestamp uint64
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

			// Get or create histogram for this PID
			hist, exists := c.histograms[ps.TGID]
			if !exists {
				hist = hdrhistogram.New(0, 1000000000, 3) // 0-1sec in nanoseconds
				c.histograms[ps.TGID] = hist
			}

			// Record the delta_ns
			if err := hist.RecordValue(int64(ps.DeltaNs)); err != nil {
				log.Printf("[PID %d] Error recording: %v", ps.TGID, err)
			} else {
				log.Printf("[PID %d] used %.2f ms at %s",
					ps.TGID, float64(ps.DeltaNs)/1e6,
					time.Unix(0, int64(ps.Timestamp)).Format(time.RFC3339Nano))
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

func (c *CPUUsageCollector) GetHistogram(pid uint32) *hdrhistogram.Histogram {
	return c.histograms[pid]
}

func (c *CPUUsageCollector) GetAllHistograms() map[uint32]*hdrhistogram.Histogram {
	return c.histograms
}

func (c *CPUUsageCollector) Close() error {
	if c.reader != nil {
		return c.reader.Close()
	}
	return nil
}
