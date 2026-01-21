package cpu

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/elastic/go-perf"
	"golang.org/x/sys/unix"
)

// HardwarePerfEvents manages hardware performance counter events for all CPUs
type HardwarePerfEvents struct {
	cyclesEvents       []*perf.Event
	instructionsEvents []*perf.Event
	cacheMissesEvents  []*perf.Event
	cpus               []int
}

// NewHardwarePerfEvents opens hardware perf events on all specified CPUs.
// Returns nil (not error) if hardware counters are unavailable.
func NewHardwarePerfEvents(cpus []int) (*HardwarePerfEvents, error) {
	h := &HardwarePerfEvents{
		cpus:               cpus,
		cyclesEvents:       make([]*perf.Event, 0, len(cpus)),
		instructionsEvents: make([]*perf.Event, 0, len(cpus)),
		cacheMissesEvents:  make([]*perf.Event, 0, len(cpus)),
	}

	// Try to open cycles counter first to check if HW counters are available
	testAttr := &perf.Attr{
		Type:   unix.PERF_TYPE_HARDWARE,
		Config: unix.PERF_COUNT_HW_CPU_CYCLES,
	}
	testEvent, err := perf.Open(testAttr, -1, cpus[0], nil)
	if err != nil {
		return nil, fmt.Errorf("hardware counters unavailable: %w", err)
	}
	_ = testEvent.Close()

	// Open events for each CPU
	for _, cpu := range cpus {
		// CPU Cycles
		cyclesEvent, err := openHWCounter(cpu, unix.PERF_COUNT_HW_CPU_CYCLES)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("open cycles counter on CPU %d: %w", cpu, err)
		}
		h.cyclesEvents = append(h.cyclesEvents, cyclesEvent)

		// Instructions
		instrEvent, err := openHWCounter(cpu, unix.PERF_COUNT_HW_INSTRUCTIONS)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("open instructions counter on CPU %d: %w", cpu, err)
		}
		h.instructionsEvents = append(h.instructionsEvents, instrEvent)

		// Cache Misses
		cacheEvent, err := openHWCounter(cpu, unix.PERF_COUNT_HW_CACHE_MISSES)
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("open cache misses counter on CPU %d: %w", cpu, err)
		}
		h.cacheMissesEvents = append(h.cacheMissesEvents, cacheEvent)
	}

	return h, nil
}

// openHWCounter opens a hardware perf counter for a specific CPU
func openHWCounter(cpu int, config uint64) (*perf.Event, error) {
	attr := &perf.Attr{
		Type:   unix.PERF_TYPE_HARDWARE,
		Config: config,
	}
	// -1 for pid means all processes on this CPU
	return perf.Open(attr, -1, cpu, nil)
}

// AttachToMaps attaches the perf event file descriptors to the eBPF PERF_EVENT_ARRAY maps
func (h *HardwarePerfEvents) AttachToMaps(
	cyclesMap *ebpf.Map,
	instructionsMap *ebpf.Map,
	cacheMissesMap *ebpf.Map,
) error {
	for i, cpu := range h.cpus {
		// Attach cycles FD
		fd, err := h.cyclesEvents[i].FD()
		if err != nil {
			return fmt.Errorf("get cycles FD for CPU %d: %w", cpu, err)
		}
		if err := cyclesMap.Update(uint32(cpu), uint32(fd), ebpf.UpdateAny); err != nil {
			return fmt.Errorf("attach cycles to map for CPU %d: %w", cpu, err)
		}

		// Attach instructions FD
		fd, err = h.instructionsEvents[i].FD()
		if err != nil {
			return fmt.Errorf("get instructions FD for CPU %d: %w", cpu, err)
		}
		if err := instructionsMap.Update(uint32(cpu), uint32(fd), ebpf.UpdateAny); err != nil {
			return fmt.Errorf("attach instructions to map for CPU %d: %w", cpu, err)
		}

		// Attach cache misses FD
		fd, err = h.cacheMissesEvents[i].FD()
		if err != nil {
			return fmt.Errorf("get cache misses FD for CPU %d: %w", cpu, err)
		}
		if err := cacheMissesMap.Update(uint32(cpu), uint32(fd), ebpf.UpdateAny); err != nil {
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

	for _, e := range h.cyclesEvents {
		if e != nil {
			if err := e.Close(); err != nil {
				lastErr = err
			}
		}
	}
	for _, e := range h.instructionsEvents {
		if e != nil {
			if err := e.Close(); err != nil {
				lastErr = err
			}
		}
	}
	for _, e := range h.cacheMissesEvents {
		if e != nil {
			if err := e.Close(); err != nil {
				lastErr = err
			}
		}
	}

	return lastErr
}
