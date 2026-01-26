// Package perfagent provides a library interface for the performance monitoring agent.
package perfagent

import (
	"io"

	"github.com/dpsoft/perf-agent/metrics"
)

// Config holds the configuration for the performance agent.
type Config struct {
	// PID is the target process ID to monitor (0 for system-wide).
	PID int

	// SystemWide enables system-wide profiling of all processes.
	SystemWide bool

	// EnableCPUProfile enables CPU profiling with stack traces.
	EnableCPUProfile bool

	// CPUProfilePath is the output path for CPU profiles.
	CPUProfilePath string

	// CPUProfileWriter is an optional writer for CPU profile output.
	// If set, profile data is written here instead of to CPUProfilePath.
	CPUProfileWriter io.Writer

	// EnableOffCPUProfile enables off-CPU profiling with stack traces.
	EnableOffCPUProfile bool

	// OffCPUProfilePath is the output path for off-CPU profiles.
	OffCPUProfilePath string

	// OffCPUProfileWriter is an optional writer for off-CPU profile output.
	// If set, profile data is written here instead of to OffCPUProfilePath.
	OffCPUProfileWriter io.Writer

	// EnablePMU enables PMU hardware counter monitoring.
	EnablePMU bool

	// PerPID shows per-PID breakdown in system-wide mode.
	PerPID bool

	// SampleRate is the CPU profiling sample rate in Hz.
	SampleRate int

	// Tags are key=value pairs to add to profiles.
	Tags []string

	// MetricsExporters are the exporters to use for metrics output.
	MetricsExporters []metrics.Exporter

	// CPUs is the list of CPUs to monitor. If nil, all online CPUs are used.
	CPUs []uint
}

// Option is a functional option for configuring the Agent.
type Option func(*Config)

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		SampleRate:        99,
		CPUProfilePath:    "profile.pb.gz",
		OffCPUProfilePath: "offcpu.pb.gz",
	}
}

// WithPID sets the target process ID to monitor.
func WithPID(pid int) Option {
	return func(c *Config) {
		c.PID = pid
		c.SystemWide = false
	}
}

// WithSystemWide enables system-wide profiling.
func WithSystemWide() Option {
	return func(c *Config) {
		c.SystemWide = true
		c.PID = 0
	}
}

// WithCPUProfile enables CPU profiling and sets the output path.
func WithCPUProfile(outputPath string) Option {
	return func(c *Config) {
		c.EnableCPUProfile = true
		if outputPath != "" {
			c.CPUProfilePath = outputPath
		}
	}
}

// WithOffCPUProfile enables off-CPU profiling and sets the output path.
func WithOffCPUProfile(outputPath string) Option {
	return func(c *Config) {
		c.EnableOffCPUProfile = true
		if outputPath != "" {
			c.OffCPUProfilePath = outputPath
		}
	}
}

// WithPMU enables PMU hardware counter monitoring.
func WithPMU() Option {
	return func(c *Config) {
		c.EnablePMU = true
	}
}

// WithPerPID enables per-PID breakdown in system-wide mode.
func WithPerPID() Option {
	return func(c *Config) {
		c.PerPID = true
	}
}

// WithSampleRate sets the CPU profiling sample rate in Hz.
func WithSampleRate(hz int) Option {
	return func(c *Config) {
		c.SampleRate = hz
	}
}

// WithTags adds tags to profiles.
func WithTags(tags ...string) Option {
	return func(c *Config) {
		c.Tags = append(c.Tags, tags...)
	}
}

// WithMetricsExporter adds a metrics exporter.
func WithMetricsExporter(exp metrics.Exporter) Option {
	return func(c *Config) {
		c.MetricsExporters = append(c.MetricsExporters, exp)
	}
}

// WithCPUs sets the list of CPUs to monitor.
func WithCPUs(cpus []uint) Option {
	return func(c *Config) {
		c.CPUs = cpus
	}
}

// WithCPUProfileWriter enables CPU profiling and sets a writer for output.
// The writer receives gzip-compressed pprof data.
// This takes precedence over CPUProfilePath if both are set.
func WithCPUProfileWriter(w io.Writer) Option {
	return func(c *Config) {
		c.EnableCPUProfile = true
		c.CPUProfileWriter = w
	}
}

// WithOffCPUProfileWriter enables off-CPU profiling and sets a writer for output.
// The writer receives gzip-compressed pprof data.
// This takes precedence over OffCPUProfilePath if both are set.
func WithOffCPUProfileWriter(w io.Writer) Option {
	return func(c *Config) {
		c.EnableOffCPUProfile = true
		c.OffCPUProfileWriter = w
	}
}
