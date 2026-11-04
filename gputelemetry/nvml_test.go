package gputelemetry

import (
	"errors"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
)

type fakeNVMLDevice struct {
	nvml.Device
	memoryStatus      nvml.Return
	temperatureStatus nvml.Return
	memoryReads       int
}

func (d *fakeNVMLDevice) GetTemperature(nvml.TemperatureSensors) (uint32, nvml.Return) {
	return 0, d.temperatureStatus
}
func (d *fakeNVMLDevice) GetPowerUsage() (uint32, nvml.Return)           { return 35196, nvml.SUCCESS }
func (d *fakeNVMLDevice) GetPowerManagementLimit() (uint32, nvml.Return) { return 370000, nvml.SUCCESS }
func (d *fakeNVMLDevice) GetMemoryInfo_v2() (nvml.Memory_v2, nvml.Return) {
	d.memoryReads++
	return nvml.Memory_v2{Total: 25769803776, Reserved: 474480640, Free: 25295192064, Used: 131072}, d.memoryStatus
}
func (d *fakeNVMLDevice) GetUtilizationRates() (nvml.Utilization, nvml.Return) {
	return nvml.Utilization{Gpu: 42, Memory: 7}, nvml.SUCCESS
}
func (d *fakeNVMLDevice) GetClockInfo(clock nvml.ClockType) (uint32, nvml.Return) {
	if clock == nvml.CLOCK_SM {
		return 210, nvml.SUCCESS
	}
	return 405, nvml.SUCCESS
}
func (d *fakeNVMLDevice) GetCurrentClocksEventReasons() (uint64, nvml.Return) {
	return nvml.ClocksEventReasonGpuIdle, nvml.SUCCESS
}
func (d *fakeNVMLDevice) GetSupportedClocksEventReasons() (uint64, nvml.Return) {
	return nvml.ClocksEventReasonGpuIdle | nvml.ClocksEventReasonSwThermalSlowdown, nvml.SUCCESS
}

type fakeNVMLLibrary struct {
	nvml.Interface
	missingMemory bool
}

func (l *fakeNVMLLibrary) Extensions() nvml.ExtendedInterface { return l }
func (l *fakeNVMLLibrary) LookupSymbol(symbol string) error {
	if l.missingMemory && symbol == "nvmlDeviceGetMemoryInfo_v2" {
		return errors.New("missing")
	}
	return nil
}

func TestNVMLUnitsRawMemoryAndIdle(t *testing.T) {
	h := &fakeNVMLDevice{}
	s := NVML{lib: &fakeNVMLLibrary{}, handles: map[string]nvml.Device{"GPU-a": h}}
	s.loadSymbols()
	d := s.Read(Device{UUID: "GPU-a", DriverVersion: "610"})
	require.Equal(t, 35.196, d.Values["gpu.power_watts"])
	require.Equal(t, 370.0, d.Values["gpu.power_limit_watts"])
	require.Equal(t, 210e6, d.Values["gpu.sm_clock_hertz"])
	require.Equal(t, 405e6, d.Values["gpu.memory_clock_hertz"])
	require.Equal(t, 0.42, d.Values["gpu.utilization_ratio"])
	require.Equal(t, 0.07, d.Values["gpu.memory_activity_ratio"])
	require.Equal(t, float64(131072), d.Values["gpu.memory.used_bytes"])
	require.Equal(t, float64(474480640), d.Values["gpu.memory.reserved_bytes"])
	require.Equal(t, 1.0, d.Values["gpu.clock_event_active.idle"])
	require.Equal(t, 0.0, d.Values["gpu.clock_event_active.software_thermal"])
	require.NotContains(t, d.Values, "gpu.clock_event_active.board_limit")
	require.Contains(t, d.Unsupported, "gpu.clock_event_active.board_limit")
	require.NotZero(t, d.Timestamp)
	require.Positive(t, d.ReadDuration)
}

func TestNVMLMissingFieldsNeverBecomeZero(t *testing.T) {
	for _, missingSymbol := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "missing_symbol"}[missingSymbol], func(t *testing.T) {
			h := &fakeNVMLDevice{memoryStatus: nvml.ERROR_NOT_SUPPORTED, temperatureStatus: nvml.ERROR_GPU_IS_LOST}
			s := NVML{lib: &fakeNVMLLibrary{missingMemory: missingSymbol}, handles: map[string]nvml.Device{"GPU-a": h}}
			s.loadSymbols()
			d := s.Read(Device{UUID: "GPU-a"})
			for _, field := range []string{"gpu.temperature_celsius", "gpu.memory.total_bytes", "gpu.memory.used_bytes", "gpu.memory.free_bytes", "gpu.memory.reserved_bytes"} {
				require.NotContains(t, d.Values, field)
				if field == "gpu.temperature_celsius" {
					require.NotEmpty(t, d.Unavailable[field])
				} else {
					require.NotEmpty(t, d.Unsupported[field])
				}
			}
			require.Contains(t, d.Values, "gpu.power_watts")
			if missingSymbol {
				require.Zero(t, h.memoryReads)
			}
		})
	}
}

func TestNVMLMissingLibrary(t *testing.T) {
	s := NVML{lib: nvml.New(nvml.WithLibraryPath(t.TempDir() + "/missing.so"))}
	_, err := s.Open()
	require.ErrorContains(t, err, "NVML initialization")
	require.ErrorContains(t, err, "ld.so.cache")
}
