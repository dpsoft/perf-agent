package gputelemetry

import (
	"errors"
	"fmt"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/dpsoft/perf-agent/metrics"
)

type NVML struct {
	lib     nvml.Interface
	handles map[string]nvml.Device
}

func NewNVML() *NVML { return &NVML{lib: nvml.New()} }

func (s *NVML) Open() (devices []Device, err error) {
	if ret := s.lib.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("NVML initialization: %s; libnvidia-ml.so.1 and NVIDIA device access are required; with file capabilities the library must be in ld.so.cache or standard loader paths (LD_LIBRARY_PATH is ignored)", nvml.ErrorString(ret))
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	driver, ret := s.lib.SystemGetDriverVersion()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("NVML driver version: %s", nvml.ErrorString(ret))
	}
	count, ret := s.lib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("NVML device count: %s", nvml.ErrorString(ret))
	}
	s.handles = make(map[string]nvml.Device, count)
	for i := range count {
		h, ret := s.lib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("NVML device %d: %s", i, nvml.ErrorString(ret))
		}
		uuid, ret := h.GetUUID()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("NVML device %d UUID: %s", i, nvml.ErrorString(ret))
		}
		name, ret := h.GetName()
		if ret != nvml.SUCCESS {
			return nil, fmt.Errorf("NVML device %s name: %s", uuid, nvml.ErrorString(ret))
		}
		s.handles[uuid] = h
		devices = append(devices, Device{UUID: uuid, Name: name, DriverVersion: driver})
	}
	return devices, nil
}

func (s *NVML) Close() error {
	if ret := s.lib.Shutdown(); ret != nvml.SUCCESS {
		return fmt.Errorf("NVML shutdown: %s", nvml.ErrorString(ret))
	}
	return nil
}

var clockReasons = []struct {
	name string
	mask uint64
}{
	{"idle", nvml.ClocksEventReasonGpuIdle},
	{"application_clocks", nvml.ClocksEventReasonApplicationsClocksSetting},
	{"power_cap", nvml.ClocksEventReasonSwPowerCap},
	{"hardware_slowdown", nvml.ClocksThrottleReasonHwSlowdown},
	{"sync_boost", nvml.ClocksEventReasonSyncBoost},
	{"software_thermal", nvml.ClocksEventReasonSwThermalSlowdown},
	{"hardware_thermal", nvml.ClocksThrottleReasonHwThermalSlowdown},
	{"hardware_power_brake", nvml.ClocksThrottleReasonHwPowerBrakeSlowdown},
	{"display_clock", nvml.ClocksEventReasonDisplayClockSetting},
	{"board_limit", nvml.ClocksEventReasonBoardLimit},
	{"reliability", nvml.ClocksEventReasonReliability},
}

func (s *NVML) Read(device Device) metrics.GPUDeviceMetrics {
	started := time.Now()
	d := metrics.GPUDeviceMetrics{
		UUID: device.UUID, Name: device.Name, DriverVersion: device.DriverVersion,
		MemoryAPIVersion: "nvmlDeviceGetMemoryInfo_v2", Timestamp: started,
		Values: make(map[string]float64), Unavailable: make(map[string]string),
	}
	h := s.handles[device.UUID]
	put := func(name string, value float64, ret nvml.Return) {
		if ret != nvml.SUCCESS {
			d.Unavailable[name] = nvml.ErrorString(ret)
			return
		}
		d.Values[name] = value
	}
	temp, ret := h.GetTemperature(nvml.TEMPERATURE_GPU)
	put("gpu.temperature_celsius", float64(temp), ret)
	power, ret := h.GetPowerUsage()
	put("gpu.power_watts", float64(power)/1000, ret)
	limit, ret := h.GetPowerManagementLimit()
	put("gpu.power_limit_watts", float64(limit)/1000, ret)
	var memory nvml.Memory_v2
	ret = nvml.ERROR_FUNCTION_NOT_FOUND
	if s.has("nvmlDeviceGetMemoryInfo_v2") {
		memory, ret = h.GetMemoryInfo_v2()
	}
	// Preserve raw v2 fields: driver accounting has differed from the API's
	// prose. Neither infer used=total-free nor normalize with reserved.
	put("gpu.memory.total_bytes", float64(memory.Total), ret)
	put("gpu.memory.used_bytes", float64(memory.Used), ret)
	put("gpu.memory.free_bytes", float64(memory.Free), ret)
	put("gpu.memory.reserved_bytes", float64(memory.Reserved), ret)
	util, ret := h.GetUtilizationRates()
	put("gpu.utilization_ratio", float64(util.Gpu)/100, ret)
	put("gpu.memory_activity_ratio", float64(util.Memory)/100, ret)
	sm, ret := h.GetClockInfo(nvml.CLOCK_SM)
	put("gpu.sm_clock_hertz", float64(sm)*1e6, ret)
	mem, ret := h.GetClockInfo(nvml.CLOCK_MEM)
	put("gpu.memory_clock_hertz", float64(mem)*1e6, ret)
	active, supported, ret := s.clockEvents(h)
	for _, reason := range clockReasons {
		state, status := 0.0, ret
		if ret == nvml.SUCCESS {
			if supported&reason.mask == 0 {
				status = nvml.ERROR_NOT_SUPPORTED
			} else if active&reason.mask != 0 {
				state = 1
			}
		}
		put("gpu.clock_event_active."+reason.name, state, status)
	}
	d.ReadDuration = time.Since(started)
	return d
}

func (s *NVML) has(symbol string) bool { return s.lib.Extensions().LookupSymbol(symbol) == nil }

func (s *NVML) clockEvents(h nvml.Device) (uint64, uint64, nvml.Return) {
	var supported, active uint64
	var ret nvml.Return
	switch {
	case s.has("nvmlDeviceGetSupportedClocksEventReasons"):
		supported, ret = h.GetSupportedClocksEventReasons()
	case s.has("nvmlDeviceGetSupportedClocksThrottleReasons"):
		supported, ret = h.GetSupportedClocksThrottleReasons()
	default:
		return 0, 0, nvml.ERROR_FUNCTION_NOT_FOUND
	}
	if ret != nvml.SUCCESS {
		return 0, 0, ret
	}
	switch {
	case s.has("nvmlDeviceGetCurrentClocksEventReasons"):
		active, ret = h.GetCurrentClocksEventReasons()
	case s.has("nvmlDeviceGetCurrentClocksThrottleReasons"):
		active, ret = h.GetCurrentClocksThrottleReasons()
	default:
		return 0, 0, nvml.ERROR_FUNCTION_NOT_FOUND
	}
	return active, supported, ret
}
