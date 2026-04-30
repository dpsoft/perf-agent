// Package perfagent provides a library interface for the performance monitoring agent.
package perfagent

import (
	"io"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
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

	// Unwind selects the stack unwinding strategy for --profile and
	// --offcpu modes. Valid values: "fp" (frame pointer),
	// "dwarf" (DWARF CFI), "auto" (default; aliases to "dwarf",
	// and the DWARF walker already takes the FP path for FP-safe
	// frames). After options parsing, an empty string is treated
	// as "auto".
	Unwind string

	// CPUs is the list of CPUs to monitor. If nil, all online CPUs are used.
	CPUs []uint

	// GPUReplayInput is a fixture path for the experimental replay backend.
	GPUReplayInput string

	// GPUHostReplayInput is a fixture path for the experimental host replay source.
	GPUHostReplayInput string

	// GPUHostHIPLibrary is the shared object path for the experimental HIP host source.
	GPUHostHIPLibrary string

	// GPUHostHIPSymbol is the HIP launch symbol name to trace.
	GPUHostHIPSymbol string

	// GPUStreamInput is a live normalized GPU NDJSON stream.
	GPUStreamInput io.Reader

	// GPUAMDSampleInput is a live AMD execution/sample NDJSON stream.
	GPUAMDSampleInput io.Reader

	// GPULinuxDRM enables the experimental Linux DRM lifecycle backend.
	GPULinuxDRM bool

	// GPULinuxKFD enables the experimental Linux KFD compute lifecycle backend.
	GPULinuxKFD bool

	// GPUHIPLinuxDRMJoinWindow bounds heuristic HIP launch -> linuxdrm event joins.
	GPUHIPLinuxDRMJoinWindow time.Duration

	// GPURawOutputPath writes the normalized GPU snapshot as JSON when set.
	GPURawOutputPath string

	// GPURawOutputWriter receives JSON GPU snapshot output when set.
	GPURawOutputWriter io.Writer

	// GPUAttributionOutputPath writes workload attribution rollups as JSON when set.
	GPUAttributionOutputPath string

	// GPUAttributionOutputWriter receives workload attribution rollups as JSON when set.
	GPUAttributionOutputWriter io.Writer

	// GPUProfileOutputPath writes synthetic-frame GPU pprof output when set.
	GPUProfileOutputPath string

	// GPUProfileOutputWriter receives synthetic-frame GPU pprof output when set.
	GPUProfileOutputWriter io.Writer

	// GPUFoldedOutputPath writes folded-stack GPU flamegraph input when set.
	GPUFoldedOutputPath string

	// GPUFoldedOutputWriter receives folded-stack GPU flamegraph input when set.
	GPUFoldedOutputWriter io.Writer

	// InjectPython enables Python perf-trampoline injection during profiling.
	// Only valid with EnableCPUProfile. Requires CAP_SYS_PTRACE.
	InjectPython bool
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

// WithUnwind selects the stack-unwinding strategy. See Config.Unwind.
func WithUnwind(mode string) Option {
	return func(c *Config) {
		c.Unwind = mode
	}
}

// WithInjectPython enables Python perf-trampoline injection during profiling.
// Caller must hold CAP_SYS_PTRACE. With --pid N, any per-target failure exits
// non-zero (strict). With -a, failures are logged and the profile continues
// (lenient). Only valid when CPU profiling is enabled.
func WithInjectPython(enabled bool) Option {
	return func(c *Config) { c.InjectPython = enabled }
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

func WithGPUReplayInput(path string) Option {
	return func(c *Config) {
		c.GPUReplayInput = path
	}
}

func WithGPUHostReplayInput(path string) Option {
	return func(c *Config) {
		c.GPUHostReplayInput = path
	}
}

func WithGPUHostHIP(libraryPath, symbol string) Option {
	return func(c *Config) {
		if symbol == "" {
			symbol = "hipLaunchKernel"
		}
		c.GPUHostHIPLibrary = libraryPath
		c.GPUHostHIPSymbol = symbol
	}
}

func WithGPUStreamInput(r io.Reader) Option {
	return func(c *Config) {
		c.GPUStreamInput = r
	}
}

func WithGPUAMDSampleInput(r io.Reader) Option {
	return func(c *Config) {
		c.GPUAMDSampleInput = r
	}
}

func WithGPULinuxDRM() Option {
	return func(c *Config) {
		c.GPULinuxDRM = true
	}
}

func WithGPULinuxKFD() Option {
	return func(c *Config) {
		c.GPULinuxKFD = true
	}
}

func WithGPUHIPLinuxDRMJoinWindow(window time.Duration) Option {
	return func(c *Config) {
		c.GPUHIPLinuxDRMJoinWindow = window
	}
}

func WithGPURawOutput(w io.Writer) Option {
	return func(c *Config) {
		c.GPURawOutputWriter = w
	}
}

func WithGPUProfileOutput(w io.Writer) Option {
	return func(c *Config) {
		c.GPUProfileOutputWriter = w
	}
}

func WithGPUAttributionOutput(w io.Writer) Option {
	return func(c *Config) {
		c.GPUAttributionOutputWriter = w
	}
}

func WithGPUFoldedOutput(w io.Writer) Option {
	return func(c *Config) {
		c.GPUFoldedOutputWriter = w
	}
}

func WithGPURawOutputPath(path string) Option {
	return func(c *Config) {
		c.GPURawOutputPath = path
	}
}

func WithGPUProfileOutputPath(path string) Option {
	return func(c *Config) {
		c.GPUProfileOutputPath = path
	}
}

func WithGPUAttributionOutputPath(path string) Option {
	return func(c *Config) {
		c.GPUAttributionOutputPath = path
	}
}

func WithGPUFoldedOutputPath(path string) Option {
	return func(c *Config) {
		c.GPUFoldedOutputPath = path
	}
}

func newGPUManager(backends []gpu.Backend) *gpu.Manager {
	return gpu.NewManager(backends, nil)
}
