package metrics

import "context"

// Exporter defines the interface for metrics exporters.
type Exporter interface {
	// Export sends the metrics snapshot to the destination.
	Export(ctx context.Context, snapshot *MetricsSnapshot) error

	// Name returns the name of the exporter.
	Name() string
}
