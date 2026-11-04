package metrics

import "time"

// GPUMetrics is independent of the CPU process scope of MetricsSnapshot.
type GPUMetrics struct {
	Kind        string             `json:"kind"`
	StartedAt   time.Time          `json:"started_at"`
	EndedAt     time.Time          `json:"ended_at,omitzero"`
	MissedTicks uint64             `json:"missed_ticks"`
	Devices     []GPUDeviceMetrics `json:"devices,omitempty"`
	Error       string             `json:"error,omitempty"`
}

type GPUDeviceMetrics struct {
	UUID             string                  `json:"uuid"`
	Name             string                  `json:"name"`
	DriverVersion    string                  `json:"driver_version"`
	MemoryAPIVersion string                  `json:"memory_api_version"`
	Timestamp        time.Time               `json:"timestamp"`
	ReadDuration     time.Duration           `json:"read_duration_ns"`
	Values           map[string]float64      `json:"values,omitempty"`
	Unavailable      map[string]string       `json:"unavailable,omitempty"`
	Summary          map[string]GaugeSummary `json:"summary,omitempty"`
	FailedReadings   map[string]uint64       `json:"failed_readings,omitempty"`
}

type GaugeSummary struct {
	Count uint64  `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
	Last  float64 `json:"last"`
}

func (s *GaugeSummary) Observe(value float64) {
	if s.Count == 0 {
		s.Min, s.Max = value, value
	}
	s.Count++
	s.Min = min(s.Min, value)
	s.Max = max(s.Max, value)
	s.Mean += (value - s.Mean) / float64(s.Count)
	s.Last = value
}
