package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
)

func writeGPUConsole(w io.Writer, gpu *GPUMetrics) error {
	if gpu.Error != "" {
		_, err := fmt.Fprintf(w, "GPU telemetry unavailable: %s\n", gpu.Error)
		return err
	}
	if gpu.Kind != "summary" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\nGPU telemetry (device scope, %s, missed ticks: %d)\n", gpu.EndedAt.Sub(gpu.StartedAt), gpu.MissedTicks); err != nil {
		return err
	}
	for _, d := range gpu.Devices {
		if _, err := fmt.Fprintf(w, "%s (%s), driver %s, memory API %s\n", d.UUID, d.Name, d.DriverVersion, d.MemoryAPIVersion); err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(d.Summary)) {
			s := d.Summary[name]
			if _, err := fmt.Fprintf(w, "  %s: min=%.6g max=%.6g mean=%.6g last=%.6g samples=%d\n", name, s.Min, s.Max, s.Mean, s.Last, s.Count); err != nil {
				return err
			}
		}
		for _, name := range slices.Sorted(maps.Keys(d.FailedReadings)) {
			if _, err := fmt.Fprintf(w, "  %s: unavailable readings=%d (last error: %s)\n", name, d.FailedReadings[name], d.Unavailable[name]); err != nil {
				return err
			}
		}
	}
	return nil
}
