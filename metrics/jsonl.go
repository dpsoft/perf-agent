package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// GPUJSONLExporter writes one device record per sample or summary.
// CPU/PMU snapshots are ignored so their existing output contract is preserved.
type GPUJSONLExporter struct {
	Writer io.Writer
}

func (e *GPUJSONLExporter) Name() string { return "gpu-jsonl" }

func (e *GPUJSONLExporter) Export(ctx context.Context, snapshot *MetricsSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot.GPU == nil {
		return nil
	}
	if e.Writer == nil {
		return fmt.Errorf("gpu-jsonl: writer is nil")
	}
	gpu := snapshot.GPU
	type record struct {
		SchemaVersion int    `json:"schema_version"`
		Source        string `json:"source"`
		Scope         string `json:"scope"`
		*GPUMetrics
		Device *GPUDeviceMetrics `json:"device,omitempty"`
	}
	metadata := *gpu
	metadata.Devices = nil
	enc := json.NewEncoder(e.Writer)
	row := record{SchemaVersion: 1, Source: "nvml", Scope: "device", GPUMetrics: &metadata}
	if len(gpu.Devices) == 0 {
		return enc.Encode(row)
	}
	for i := range gpu.Devices {
		row.Device = &gpu.Devices[i]
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}
