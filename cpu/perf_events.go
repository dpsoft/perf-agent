package cpu

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

// HardwarePerfEvents manages hardware performance counter events for all CPUs
type HardwarePerfEvents struct {
	cyclesFDs       []int
	instructionsFDs []int
	cacheMissesFDs  []int
	cpus            []int
}

// NewHardwarePerfEvents opens hardware perf events on all specified CPUs.
// Returns nil (not error) if hardware counters are unavailable.
func NewHardwarePerfEvents(cpus []int) (*HardwarePerfEvents, error) {
	h := &HardwarePerfEvents{
		cpus:            cpus,
		cyclesFDs:       make([]int, 0, len(cpus)),
		instructionsFDs: make([]int, 0, len(cpus)),
		cacheMissesFDs:  make([]int, 0, len(cpus)),
	}

	// Try to open cycles counter first to check if HW counters are available
	testFD, err := openHWCounter(cpus[0], unix.PERF_COUNT_HW_CPU_CYCLES)
	if err != nil {
		return nil, fmt.Errorf("hardware counters unavailable: %w", err)
	}
	_ = unix.Close(testFD)

	// Open events for each CPU
	for _, cpu := range cpus {
		// CPU Cycles
		cyclesFD, err := openHWCounter(cpu, unix.PERF_COUNT_HW_CPU_CYCLES)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("open cycles counter on CPU %d: %w", cpu, err)
		}
		h.cyclesFDs = append(h.cyclesFDs, cyclesFD)

		// Instructions
		instrFD, err := openHWCounter(cpu, unix.PERF_COUNT_HW_INSTRUCTIONS)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("open instructions counter on CPU %d: %w", cpu, err)
		}
		h.instructionsFDs = append(h.instructionsFDs, instrFD)

		// Cache Misses
		cacheFD, err := openHWCounter(cpu, unix.PERF_COUNT_HW_CACHE_MISSES)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("open cache misses counter on CPU %d: %w", cpu, err)
		}
		h.cacheMissesFDs = append(h.cacheMissesFDs, cacheFD)
	}

	return h, nil
}

// openHWCounter opens a hardware perf counter for a specific CPU using unix.PerfEventOpen directly
func openHWCounter(cpu int, config uint64) (int, error) {
	attr := unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_HARDWARE,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config: config,
	}
	fd, err := unix.PerfEventOpen(&attr, -1, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return -1, os.NewSyscallError("perf_event_open", err)
	}
	return fd, nil
}

// AttachToMaps attaches the perf event file descriptors to the eBPF PERF_EVENT_ARRAY maps
func (h *HardwarePerfEvents) AttachToMaps(
	cyclesMap *ebpf.Map,
	instructionsMap *ebpf.Map,
	cacheMissesMap *ebpf.Map,
) error {
	for i, cpu := range h.cpus {
		// Attach cycles FD
		if err := cyclesMap.Update(uint32(cpu), uint32(h.cyclesFDs[i]), ebpf.UpdateAny); err != nil {
			return fmt.Errorf("attach cycles to map for CPU %d: %w", cpu, err)
		}

		// Attach instructions FD
		if err := instructionsMap.Update(uint32(cpu), uint32(h.instructionsFDs[i]), ebpf.UpdateAny); err != nil {
			return fmt.Errorf("attach instructions to map for CPU %d: %w", cpu, err)
		}

		// Attach cache misses FD
		if err := cacheMissesMap.Update(uint32(cpu), uint32(h.cacheMissesFDs[i]), ebpf.UpdateAny); err != nil {
			return fmt.Errorf("attach cache misses to map for CPU %d: %w", cpu, err)
		}
	}

	return nil
}

// EnableInBPF sets the hw_counters_enabled flag in the eBPF program
func (h *HardwarePerfEvents) EnableInBPF(enabledMap *ebpf.Map) error {
	key := uint32(0)
	value := uint32(1)
	return enabledMap.Update(key, value, ebpf.UpdateAny)
}

// Close closes all perf event file descriptors
func (h *HardwarePerfEvents) Close() error {
	var lastErr error

	for _, fd := range h.cyclesFDs {
		if fd >= 0 {
			if err := unix.Close(fd); err != nil {
				lastErr = err
			}
		}
	}
	for _, fd := range h.instructionsFDs {
		if fd >= 0 {
			if err := unix.Close(fd); err != nil {
				lastErr = err
			}
		}
	}
	for _, fd := range h.cacheMissesFDs {
		if fd >= 0 {
			if err := unix.Close(fd); err != nil {
				lastErr = err
			}
		}
	}

	return lastErr
}
