package cpu

import (
	"testing"
	"unsafe"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStructAlignment(t *testing.T) {
	// Verify Go struct sizes match BPF expectations
	t.Run("PidStat size", func(t *testing.T) {
		// Updated struct with runq_latency_ns and task state fields:
		// u32 tgid (4) + u32 _pad0 (4) + u64 delta_ns (8) + u64 runq_latency_ns (8) +
		// u64 cycles (8) + u64 instructions (8) + u64 cache_misses (8) + u64 timestamp (8) +
		// u8 prev_state (1) + u8 preempt (1) + u8[6] _pad1 (6) = 64 bytes
		size := int(unsafe.Sizeof(PidStat{}))
		t.Logf("PidStat size: %d bytes", size)
		assert.Equal(t, 64, size, "PidStat should be 64 bytes with padding")
	})

	t.Run("CpuStat size", func(t *testing.T) {
		// u32 (4) + padding (4) + 3*u64 (24) = 32 bytes
		size := int(unsafe.Sizeof(CpuStat{}))
		t.Logf("CpuStat size: %d bytes", size)
		assert.Equal(t, 32, size, "CpuStat should be 32 bytes")
	})

	t.Run("PidStat field offsets", func(t *testing.T) {
		var ps PidStat
		tgidOffset := unsafe.Offsetof(ps.TGID)
		deltaNsOffset := unsafe.Offsetof(ps.DeltaNs)
		runqLatencyOffset := unsafe.Offsetof(ps.RunqLatencyNs)
		cyclesOffset := unsafe.Offsetof(ps.Cycles)
		prevStateOffset := unsafe.Offsetof(ps.PrevState)
		preemptOffset := unsafe.Offsetof(ps.Preempt)

		t.Logf("TGID offset: %d", tgidOffset)
		t.Logf("DeltaNs offset: %d", deltaNsOffset)
		t.Logf("RunqLatencyNs offset: %d", runqLatencyOffset)
		t.Logf("Cycles offset: %d", cyclesOffset)
		t.Logf("PrevState offset: %d", prevStateOffset)
		t.Logf("Preempt offset: %d", preemptOffset)

		assert.Equal(t, uintptr(0), tgidOffset)
		assert.Equal(t, uintptr(8), deltaNsOffset, "DeltaNs should be at offset 8")
		assert.Equal(t, uintptr(16), runqLatencyOffset, "RunqLatencyNs should be at offset 16")
		assert.Equal(t, uintptr(24), cyclesOffset, "Cycles should be at offset 24")
		assert.Equal(t, uintptr(56), prevStateOffset, "PrevState should be at offset 56")
		assert.Equal(t, uintptr(57), preemptOffset, "Preempt should be at offset 57")
	})
}

func TestHistogramConfiguration(t *testing.T) {
	t.Run("OnCPU histogram range", func(t *testing.T) {
		// Test histogram can handle expected ranges (0-1 hour in ns)
		hist := hdrhistogram.New(0, 1000000000000000, 3)

		// 1 microsecond
		err := hist.RecordValue(1000)
		assert.NoError(t, err)

		// 1 millisecond
		err = hist.RecordValue(1000000)
		assert.NoError(t, err)

		// 1 second
		err = hist.RecordValue(1000000000)
		assert.NoError(t, err)

		// 1 minute
		err = hist.RecordValue(60000000000)
		assert.NoError(t, err)

		// Verify percentiles make sense
		assert.Greater(t, hist.ValueAtQuantile(50.0), int64(0))
		assert.Greater(t, hist.Max(), hist.Min())
		assert.Greater(t, hist.Mean(), float64(0))

		t.Logf("Min: %d ns", hist.Min())
		t.Logf("Max: %d ns", hist.Max())
		t.Logf("Mean: %.2f ns", hist.Mean())
		t.Logf("P50: %d ns", hist.ValueAtQuantile(50.0))
		t.Logf("P95: %d ns", hist.ValueAtQuantile(95.0))
		t.Logf("P99: %d ns", hist.ValueAtQuantile(99.0))
	})

	t.Run("RunqLatency histogram range", func(t *testing.T) {
		// Runqueue latency histogram (0-1000 seconds in ns)
		hist := hdrhistogram.New(0, 1000000000000, 3)

		// Typical runqueue latencies
		err := hist.RecordValue(10000) // 10µs
		assert.NoError(t, err)
		err = hist.RecordValue(100000) // 100µs
		assert.NoError(t, err)
		err = hist.RecordValue(1000000) // 1ms
		assert.NoError(t, err)
		err = hist.RecordValue(10000000) // 10ms (high latency)
		assert.NoError(t, err)

		t.Logf("RunqLatency P50: %.3f ms", float64(hist.ValueAtQuantile(50.0))/1e6)
		t.Logf("RunqLatency P99: %.3f ms", float64(hist.ValueAtQuantile(99.0))/1e6)
	})
}

func TestPidMetrics(t *testing.T) {
	t.Run("OnCPU time tracking", func(t *testing.T) {
		m := &PidMetrics{
			OnCPUHist:       hdrhistogram.New(0, 1000000000000000, 3),
			RunqLatencyHist: hdrhistogram.New(0, 1000000000000, 3),
		}

		// Simulate some scheduling events (on-CPU time)
		deltas := []int64{
			1000000,  // 1ms
			5000000,  // 5ms
			10000000, // 10ms
			2000000,  // 2ms
			15000000, // 15ms
		}

		for _, delta := range deltas {
			err := m.OnCPUHist.RecordValue(delta)
			assert.NoError(t, err)
			m.SampleCount++
		}

		// Verify metrics
		assert.Equal(t, uint64(5), m.SampleCount)
		assert.Greater(t, m.OnCPUHist.Mean(), float64(0))
		// HDR histogram approximates values, so check within reasonable range
		assert.InDelta(t, 1000000, m.OnCPUHist.Min(), 100000)  // Within 100µs
		assert.InDelta(t, 15000000, m.OnCPUHist.Max(), 100000) // Within 100µs

		t.Logf("Sample count: %d", m.SampleCount)
		t.Logf("Mean on-CPU time: %.3f ms", m.OnCPUHist.Mean()/1e6)
		t.Logf("Min on-CPU time: %.3f ms", float64(m.OnCPUHist.Min())/1e6)
		t.Logf("Max on-CPU time: %.3f ms", float64(m.OnCPUHist.Max())/1e6)
	})

	t.Run("Runqueue latency tracking", func(t *testing.T) {
		m := &PidMetrics{
			OnCPUHist:       hdrhistogram.New(0, 1000000000000000, 3),
			RunqLatencyHist: hdrhistogram.New(0, 1000000000000, 3),
		}

		// Simulate runqueue latencies (typically much smaller than on-CPU time)
		latencies := []int64{
			5000,   // 5µs
			10000,  // 10µs
			50000,  // 50µs
			100000, // 100µs
			500000, // 500µs (high latency)
		}

		for _, lat := range latencies {
			err := m.RunqLatencyHist.RecordValue(lat)
			require.NoError(t, err)
		}

		assert.Equal(t, int64(5), m.RunqLatencyHist.TotalCount())
		t.Logf("Runqueue latency P50: %.3f µs", float64(m.RunqLatencyHist.ValueAtQuantile(50.0))/1e3)
		t.Logf("Runqueue latency P99: %.3f µs", float64(m.RunqLatencyHist.ValueAtQuantile(99.0))/1e3)
	})
}

func TestTaskStateTracking(t *testing.T) {
	m := &PidMetrics{
		OnCPUHist:       hdrhistogram.New(0, 1000000000000000, 3),
		RunqLatencyHist: hdrhistogram.New(0, 1000000000000, 3),
	}

	// Simulate different context switch reasons
	// 50 preempted, 30 voluntary, 20 I/O wait
	m.PreemptedCount = 50
	m.VoluntaryCount = 30
	m.IOWaitCount = 20
	m.SampleCount = 100

	total := m.PreemptedCount + m.VoluntaryCount + m.IOWaitCount
	assert.Equal(t, uint64(100), total)

	preemptPct := float64(m.PreemptedCount) / float64(total) * 100
	voluntaryPct := float64(m.VoluntaryCount) / float64(total) * 100
	ioWaitPct := float64(m.IOWaitCount) / float64(total) * 100

	assert.InDelta(t, 50.0, preemptPct, 0.1)
	assert.InDelta(t, 30.0, voluntaryPct, 0.1)
	assert.InDelta(t, 20.0, ioWaitPct, 0.1)

	t.Logf("Preempted: %.1f%%", preemptPct)
	t.Logf("Voluntary: %.1f%%", voluntaryPct)
	t.Logf("I/O Wait: %.1f%%", ioWaitPct)
}

func TestTaskStateConstants(t *testing.T) {
	// Verify state constants match expected values
	assert.Equal(t, uint8(0), uint8(StateRunning))
	assert.Equal(t, uint8(1), uint8(StateInterruptible))
	assert.Equal(t, uint8(2), uint8(StateUninterruptible))
}

func TestHardwareCounters(t *testing.T) {
	m := &PidMetrics{
		OnCPUHist:         hdrhistogram.New(0, 1000000000000000, 3),
		RunqLatencyHist:   hdrhistogram.New(0, 1000000000000, 3),
		TotalCycles:       100000000,
		TotalInstructions: 200000000,
		TotalCacheMisses:  50000,
		SampleCount:       1000,
	}

	// Calculate IPC
	ipc := float64(m.TotalInstructions) / float64(m.TotalCycles)
	assert.InDelta(t, 2.0, ipc, 0.01)

	// Calculate cache miss rate per 1K instructions
	missRate := float64(m.TotalCacheMisses) / float64(m.TotalInstructions) * 1000
	assert.InDelta(t, 0.25, missRate, 0.01)

	t.Logf("IPC: %.3f", ipc)
	t.Logf("Cache misses per 1K instructions: %.3f", missRate)
}

func TestPidStatParsing(t *testing.T) {
	// Simulate parsing a raw PidStat from ring buffer
	ps := PidStat{
		TGID:          12345,
		DeltaNs:       5000000, // 5ms on-CPU
		RunqLatencyNs: 100000,  // 100µs runqueue latency
		Cycles:        10000000,
		Instructions:  20000000,
		CacheMisses:   500,
		Timestamp:     1000000000000,
		PrevState:     StateInterruptible,
		Preempt:       0,
	}

	assert.Equal(t, uint32(12345), ps.TGID)
	assert.Equal(t, uint64(5000000), ps.DeltaNs)
	assert.Equal(t, uint64(100000), ps.RunqLatencyNs)
	assert.Equal(t, uint8(StateInterruptible), ps.PrevState)
	assert.Equal(t, uint8(0), ps.Preempt)

	t.Logf("PidStat TGID: %d", ps.TGID)
	t.Logf("PidStat OnCPU time: %.3f ms", float64(ps.DeltaNs)/1e6)
	t.Logf("PidStat Runqueue latency: %.3f µs", float64(ps.RunqLatencyNs)/1e3)
	t.Logf("PidStat State: %d (interruptible)", ps.PrevState)
}

func TestMetricsAccumulation(t *testing.T) {
	m := &PidMetrics{
		OnCPUHist:       hdrhistogram.New(0, 1000000000000000, 3),
		RunqLatencyHist: hdrhistogram.New(0, 1000000000000, 3),
	}

	// Simulate processing multiple events
	events := []struct {
		deltaNs   uint64
		runqLat   uint64
		prevState uint8
		cycles    uint64
		instrs    uint64
	}{
		{1000000, 10000, StateRunning, 1000000, 2000000},
		{2000000, 20000, StateInterruptible, 2000000, 4000000},
		{3000000, 30000, StateUninterruptible, 3000000, 6000000},
		{4000000, 15000, StateRunning, 4000000, 8000000},
		{5000000, 25000, StateInterruptible, 5000000, 10000000},
	}

	for _, e := range events {
		_ = m.OnCPUHist.RecordValue(int64(e.deltaNs))
		if e.runqLat > 0 {
			_ = m.RunqLatencyHist.RecordValue(int64(e.runqLat))
		}
		m.TotalCycles += e.cycles
		m.TotalInstructions += e.instrs
		m.SampleCount++

		switch e.prevState {
		case StateRunning:
			m.PreemptedCount++
		case StateInterruptible:
			m.VoluntaryCount++
		case StateUninterruptible:
			m.IOWaitCount++
		}
	}

	assert.Equal(t, uint64(5), m.SampleCount)
	assert.Equal(t, uint64(2), m.PreemptedCount)
	assert.Equal(t, uint64(2), m.VoluntaryCount)
	assert.Equal(t, uint64(1), m.IOWaitCount)
	assert.Equal(t, uint64(15000000), m.TotalCycles)
	assert.Equal(t, uint64(30000000), m.TotalInstructions)
	assert.Equal(t, int64(5), m.OnCPUHist.TotalCount())
	assert.Equal(t, int64(5), m.RunqLatencyHist.TotalCount())

	t.Logf("Total samples: %d", m.SampleCount)
	t.Logf("On-CPU P50: %.3f ms", float64(m.OnCPUHist.ValueAtQuantile(50.0))/1e6)
	t.Logf("Runqueue P50: %.3f µs", float64(m.RunqLatencyHist.ValueAtQuantile(50.0))/1e3)
}
