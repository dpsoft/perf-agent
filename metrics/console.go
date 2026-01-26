package metrics

import (
	"context"
	"fmt"
	"io"
	"os"
)

// ConsoleExporter outputs metrics to the console (stdout by default).
type ConsoleExporter struct {
	Writer io.Writer
	PerPID bool
}

// NewConsoleExporter creates a new console exporter.
func NewConsoleExporter(perPID bool) *ConsoleExporter {
	return &ConsoleExporter{
		Writer: os.Stdout,
		PerPID: perPID,
	}
}

// Name returns the exporter name.
func (e *ConsoleExporter) Name() string {
	return "console"
}

// Export outputs the metrics snapshot to the console.
func (e *ConsoleExporter) Export(ctx context.Context, snapshot *MetricsSnapshot) error {
	if len(snapshot.Processes) == 0 {
		_, _ = fmt.Fprintln(e.Writer, "\nNo PMU metrics collected")
		return nil
	}

	if !snapshot.SystemWide {
		// Targeted mode (single PID)
		for pid, m := range snapshot.Processes {
			_, _ = fmt.Fprintf(e.Writer, "\n=== PMU Metrics (PID: %d) ===\n", pid)
			e.printSinglePIDMetrics(m)
		}
		return nil
	}

	if e.PerPID {
		// System-wide with per-PID breakdown
		_, _ = fmt.Fprintf(e.Writer, "\n=== PMU Metrics (System-Wide, Per-PID) ===\n")
		_, _ = fmt.Fprintf(e.Writer, "Profiled %d processes\n", len(snapshot.Processes))
		for pid, m := range snapshot.Processes {
			fmt.Fprintf(e.Writer, "\n--- PID %d ---\n", pid)
			e.printSinglePIDMetrics(m)
		}
	} else {
		// System-wide aggregate (default)
		fmt.Fprintf(e.Writer, "\n=== PMU Metrics (System-Wide) ===\n")
		e.printAggregateMetrics(snapshot)
	}

	return nil
}

func (e *ConsoleExporter) printSinglePIDMetrics(m *ProcessMetrics) {
	w := e.Writer
	fmt.Fprintf(w, "Samples: %d\n", m.SampleCount)

	// On-CPU time histogram stats
	fmt.Fprintf(w, "\nOn-CPU Time (time slice per context switch):\n")
	fmt.Fprintf(w, "  Min:    %.3f ms\n", float64(m.OnCPUStats.Min)/1e6)
	fmt.Fprintf(w, "  Max:    %.3f ms\n", float64(m.OnCPUStats.Max)/1e6)
	fmt.Fprintf(w, "  Mean:   %.3f ms\n", m.OnCPUStats.Mean/1e6)
	fmt.Fprintf(w, "  P50:    %.3f ms\n", float64(m.OnCPUStats.P50)/1e6)
	fmt.Fprintf(w, "  P95:    %.3f ms\n", float64(m.OnCPUStats.P95)/1e6)
	fmt.Fprintf(w, "  P99:    %.3f ms\n", float64(m.OnCPUStats.P99)/1e6)
	fmt.Fprintf(w, "  P99.9:  %.3f ms\n", float64(m.OnCPUStats.P999)/1e6)

	// Runqueue latency histogram stats
	if m.RunqueueStats.Count > 0 {
		fmt.Fprintf(w, "\nRunqueue Latency (time waiting for CPU):\n")
		fmt.Fprintf(w, "  Min:    %.3f ms\n", float64(m.RunqueueStats.Min)/1e6)
		fmt.Fprintf(w, "  Max:    %.3f ms\n", float64(m.RunqueueStats.Max)/1e6)
		fmt.Fprintf(w, "  Mean:   %.3f ms\n", m.RunqueueStats.Mean/1e6)
		fmt.Fprintf(w, "  P50:    %.3f ms\n", float64(m.RunqueueStats.P50)/1e6)
		fmt.Fprintf(w, "  P95:    %.3f ms\n", float64(m.RunqueueStats.P95)/1e6)
		fmt.Fprintf(w, "  P99:    %.3f ms\n", float64(m.RunqueueStats.P99)/1e6)
		fmt.Fprintf(w, "  P99.9:  %.3f ms\n", float64(m.RunqueueStats.P999)/1e6)
	}

	// Context switch reasons
	totalSwitches := m.ContextSwitches.PreemptedCount + m.ContextSwitches.VoluntaryCount + m.ContextSwitches.IOWaitCount
	if totalSwitches > 0 {
		fmt.Fprintf(w, "\nContext Switch Reasons:\n")
		fmt.Fprintf(w, "  Preempted (running):     %.1f%%  (%d times)\n",
			float64(m.ContextSwitches.PreemptedCount)/float64(totalSwitches)*100, m.ContextSwitches.PreemptedCount)
		fmt.Fprintf(w, "  Voluntary (sleep/mutex): %.1f%%  (%d times)\n",
			float64(m.ContextSwitches.VoluntaryCount)/float64(totalSwitches)*100, m.ContextSwitches.VoluntaryCount)
		fmt.Fprintf(w, "  I/O Wait (D state):      %.1f%%  (%d times)\n",
			float64(m.ContextSwitches.IOWaitCount)/float64(totalSwitches)*100, m.ContextSwitches.IOWaitCount)
	}

	// Hardware counters
	if m.HardwareCounters.Available {
		fmt.Fprintf(w, "\nHardware Counters:\n")
		fmt.Fprintf(w, "  Total Cycles:       %d\n", m.HardwareCounters.Cycles)
		fmt.Fprintf(w, "  Total Instructions: %d\n", m.HardwareCounters.Instructions)
		fmt.Fprintf(w, "  Total Cache Misses: %d\n", m.HardwareCounters.CacheMisses)
		fmt.Fprintf(w, "  IPC (Instr/Cycle):  %.3f\n", m.HardwareCounters.IPC)
		if m.HardwareCounters.Instructions > 0 {
			fmt.Fprintf(w, "  Cache Misses/1K Instr: %.3f\n", m.HardwareCounters.MissRate)
		}
	} else {
		fmt.Fprintf(w, "\nHardware Counters: not available\n")
	}
}

func (e *ConsoleExporter) printAggregateMetrics(snapshot *MetricsSnapshot) {
	w := e.Writer
	totalSamples := snapshot.TotalSamples()
	hwCounters := snapshot.AggregateHardwareCounters()
	ctxSwitches := snapshot.AggregateContextSwitches()

	fmt.Fprintf(w, "\nPerformance counter stats for 'system wide':\n\n")
	fmt.Fprintf(w, "  Processes profiled:     %d\n", len(snapshot.Processes))
	fmt.Fprintf(w, "  Total samples:          %d\n", totalSamples)

	// For aggregate metrics, we need to merge histograms from all processes
	// This is a simplified version - for full histogram merging, see cpu_usage_collector.go
	if len(snapshot.Processes) > 0 {
		// Find first process with data to show sample stats
		for _, m := range snapshot.Processes {
			if m.OnCPUStats.Count > 0 {
				fmt.Fprintf(w, "\nOn-CPU Time (time slice per context switch):\n")
				fmt.Fprintf(w, "  Min:    %.3f ms\n", float64(m.OnCPUStats.Min)/1e6)
				fmt.Fprintf(w, "  Max:    %.3f ms\n", float64(m.OnCPUStats.Max)/1e6)
				fmt.Fprintf(w, "  Mean:   %.3f ms\n", m.OnCPUStats.Mean/1e6)
				fmt.Fprintf(w, "  P50:    %.3f ms\n", float64(m.OnCPUStats.P50)/1e6)
				fmt.Fprintf(w, "  P95:    %.3f ms\n", float64(m.OnCPUStats.P95)/1e6)
				fmt.Fprintf(w, "  P99:    %.3f ms\n", float64(m.OnCPUStats.P99)/1e6)
				fmt.Fprintf(w, "  P99.9:  %.3f ms\n", float64(m.OnCPUStats.P999)/1e6)
				break
			}
		}

		for _, m := range snapshot.Processes {
			if m.RunqueueStats.Count > 0 {
				fmt.Fprintf(w, "\nRunqueue Latency (time waiting for CPU):\n")
				fmt.Fprintf(w, "  Min:    %.3f ms\n", float64(m.RunqueueStats.Min)/1e6)
				fmt.Fprintf(w, "  Max:    %.3f ms\n", float64(m.RunqueueStats.Max)/1e6)
				fmt.Fprintf(w, "  Mean:   %.3f ms\n", m.RunqueueStats.Mean/1e6)
				fmt.Fprintf(w, "  P50:    %.3f ms\n", float64(m.RunqueueStats.P50)/1e6)
				fmt.Fprintf(w, "  P95:    %.3f ms\n", float64(m.RunqueueStats.P95)/1e6)
				fmt.Fprintf(w, "  P99:    %.3f ms\n", float64(m.RunqueueStats.P99)/1e6)
				fmt.Fprintf(w, "  P99.9:  %.3f ms\n", float64(m.RunqueueStats.P999)/1e6)
				break
			}
		}
	}

	// Context switch reasons
	totalSwitches := ctxSwitches.PreemptedCount + ctxSwitches.VoluntaryCount + ctxSwitches.IOWaitCount
	if totalSwitches > 0 {
		fmt.Fprintf(w, "\nContext Switch Reasons (aggregate):\n")
		fmt.Fprintf(w, "  Preempted (running):     %.1f%%  (%d times)\n",
			float64(ctxSwitches.PreemptedCount)/float64(totalSwitches)*100, ctxSwitches.PreemptedCount)
		fmt.Fprintf(w, "  Voluntary (sleep/mutex): %.1f%%  (%d times)\n",
			float64(ctxSwitches.VoluntaryCount)/float64(totalSwitches)*100, ctxSwitches.VoluntaryCount)
		fmt.Fprintf(w, "  I/O Wait (D state):      %.1f%%  (%d times)\n",
			float64(ctxSwitches.IOWaitCount)/float64(totalSwitches)*100, ctxSwitches.IOWaitCount)
	}

	fmt.Fprintf(w, "\nHardware Counters:\n")
	fmt.Fprintf(w, "  Total Cycles:           %d\n", hwCounters.Cycles)
	fmt.Fprintf(w, "  Total Instructions:     %d\n", hwCounters.Instructions)
	fmt.Fprintf(w, "  Total Cache Misses:     %d\n", hwCounters.CacheMisses)

	if hwCounters.Cycles > 0 {
		fmt.Fprintf(w, "  IPC (Instr/Cycle):      %.2f\n", hwCounters.IPC)
	}
	if hwCounters.Instructions > 0 {
		fmt.Fprintf(w, "  Cache Misses/1K Instr:  %.2f\n", hwCounters.MissRate)
	}
}
