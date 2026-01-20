package cpu

import (
	"testing"
	"unsafe"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/stretchr/testify/assert"
)

func TestStructAlignment(t *testing.T) {
	// Verify Go struct sizes match BPF expectations
	t.Run("PidStat size", func(t *testing.T) {
		// With Go padding: u32 (4) + padding (4) + 5*u64 (40) = 48 bytes
		size := int(unsafe.Sizeof(PidStat{}))
		t.Logf("PidStat size: %d bytes", size)
		assert.Equal(t, 48, size, "PidStat should be 48 bytes with padding")
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
		cyclesOffset := unsafe.Offsetof(ps.Cycles)

		t.Logf("TGID offset: %d", tgidOffset)
		t.Logf("DeltaNs offset: %d", deltaNsOffset)
		t.Logf("Cycles offset: %d", cyclesOffset)

		assert.Equal(t, uintptr(0), tgidOffset)
		assert.Equal(t, uintptr(8), deltaNsOffset, "DeltaNs should be at offset 8 (after TGID + padding)")
		assert.Equal(t, uintptr(16), cyclesOffset)
	})
}

func TestHistogramConfiguration(t *testing.T) {
	// Test histogram can handle expected ranges
	hist := hdrhistogram.New(0, 1000000000000000, 3) // 0-1 hour in ns

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
}

func TestPidMetrics(t *testing.T) {
	m := &PidMetrics{
		TimeHist: hdrhistogram.New(0, 1000000000000000, 3),
	}

	// Simulate some scheduling events
	deltas := []int64{
		1000000,   // 1ms
		5000000,   // 5ms
		10000000,  // 10ms
		2000000,   // 2ms
		15000000,  // 15ms
	}

	for _, delta := range deltas {
		err := m.TimeHist.RecordValue(delta)
		assert.NoError(t, err)
		m.SampleCount++
	}

	// Verify metrics
	assert.Equal(t, uint64(5), m.SampleCount)
	assert.Greater(t, m.TimeHist.Mean(), float64(0))
	// HDR histogram approximates values, so check within reasonable range
	assert.InDelta(t, 1000000, m.TimeHist.Min(), 100000) // Within 100µs
	assert.InDelta(t, 15000000, m.TimeHist.Max(), 100000) // Within 100µs

	t.Logf("Sample count: %d", m.SampleCount)
	t.Logf("Mean latency: %.3f ms", m.TimeHist.Mean()/1e6)
	t.Logf("Min latency: %.3f ms", float64(m.TimeHist.Min())/1e6)
	t.Logf("Max latency: %.3f ms", float64(m.TimeHist.Max())/1e6)
}
