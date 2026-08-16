// Probes the kernel for the most accurate sample event available, falling
// back gracefully when running in environments (cloud VMs, k8s pods on
// virtualized hosts) where hardware PMU events aren't exposed.
package perfevent

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	PerfTypeHardware = 0
	PerfTypeSoftware = 1

	PerfCountHWCPUCycles = 0
	PerfCountSWCPUClock  = 0
)

// EventSpec is the perf-event configuration used to open per-CPU events
// AND to populate the perf.data attr section. SamplePeriod is interpreted
// as a frequency (Hz) when Frequency is true.
type EventSpec struct {
	Type         uint32
	Config       uint64
	SamplePeriod uint64
	Frequency    bool
}

func (s EventSpec) String() string {
	switch {
	case s.Type == PerfTypeHardware && s.Config == PerfCountHWCPUCycles:
		return "hardware/cpu-cycles"
	case s.Type == PerfTypeSoftware && s.Config == PerfCountSWCPUClock:
		return "software/cpu-clock"
	default:
		return fmt.Sprintf("type=%d/config=%d", s.Type, s.Config)
	}
}

// ProbeHardwareCycles tries to open a PERF_TYPE_HARDWARE / PERF_COUNT_HW_CPU_CYCLES
// event; on success returns that EventSpec. On the typical virtualized-host
// failures (EOPNOTSUPP, ENOENT, ENODEV, EINVAL, EACCES) it returns the
// software cpu-clock fallback. Any other error propagates — those usually
// mean broken kernel state we shouldn't paper over.
//
// sampleHz is the desired sample rate; threaded through into the returned
// EventSpec.SamplePeriod with Frequency=true.
func ProbeHardwareCycles(sampleHz uint64) (EventSpec, error) {
	attr := unix.PerfEventAttr{
		Type:   PerfTypeHardware,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config: PerfCountHWCPUCycles,
		Sample: sampleHz,
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	fd, err := unix.PerfEventOpen(&attr, -1, 0, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err == nil {
		_ = unix.Close(fd)
		return EventSpec{
			Type:         PerfTypeHardware,
			Config:       PerfCountHWCPUCycles,
			SamplePeriod: sampleHz,
			Frequency:    true,
		}, nil
	}
	// Common virt / restricted-env failures — fall back silently.
	if errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOENT) ||
		errors.Is(err, unix.ENODEV) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EACCES) {
		return EventSpec{
			Type:         PerfTypeSoftware,
			Config:       PerfCountSWCPUClock,
			SamplePeriod: sampleHz,
			Frequency:    true,
		}, nil
	}
	return EventSpec{}, fmt.Errorf("perfevent: probe HW cycles: %w", err)
}
