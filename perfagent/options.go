// Package perfagent provides a library interface for the performance monitoring agent.
package perfagent

import (
	"io"
	"time"

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

	// PerfDataOutput is the path for a kernel-format perf.data file. Empty
	// disables emission. Set via WithPerfDataOutput. Only on-CPU samples
	// are written; off-CPU and PMU modes ignore this option.
	PerfDataOutput string

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

	// InjectPython enables Python perf-trampoline injection during profiling.
	// Only valid with EnableCPUProfile. Requires CAP_SYS_PTRACE.
	InjectPython bool

	// Labels are static per-sample pprof labels. Merged on top of the enricher
	// output (Labels wins on key collision). Set via WithLabels.
	Labels map[string]string

	// LabelEnricher computes additional per-sample labels at agent startup
	// from the resolved host PID. Default is internal/k8slabels.FromPID.
	// Override via WithLabelEnricher; pass nil to disable defaults entirely.
	// LabelEnricherSet records whether the user explicitly called
	// WithLabelEnricher (so passing nil to disable is distinguishable from
	// not calling it at all).
	LabelEnricher    func(hostPID int) map[string]string
	LabelEnricherSet bool

	// DebuginfodURLs is the ordered list of debuginfod servers to consult for
	// off-box DWARF/executable fetching. If empty (and DEBUGINFOD_URLS env is
	// also empty), the agent uses the local symbolizer.
	DebuginfodURLs []string

	// SymbolCacheDir overrides the debuginfod cache directory.
	// Default: /tmp/perf-agent-debuginfod.
	SymbolCacheDir string

	// SymbolCacheMaxBytes overrides the debuginfod cache size cap. Default: 2 GiB.
	SymbolCacheMaxBytes int64

	// SymbolFetchTimeout overrides per-artifact fetch timeout. Default: 30s.
	SymbolFetchTimeout time.Duration

	// SymbolFailClosed makes the agent refuse to symbolize a mapping whose
	// debuginfod fetch failed (vs. fall back to local). Default: false.
	// Note: M1 ships the option but FailClosed semantics are M2.
	SymbolFailClosed bool
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

// WithLabels attaches static per-sample labels to every emitted pprof
// sample. Merged with WithLabelEnricher output; static labels win on key
// collision.
func WithLabels(labels map[string]string) Option {
	return func(c *Config) {
		if c.Labels == nil {
			c.Labels = make(map[string]string, len(labels))
		}
		for k, v := range labels {
			c.Labels[k] = v
		}
	}
}

// WithLabelEnricher overrides the default label enricher (which derives
// k8s identity labels from /proc/<hostPID>/cgroup and downward-API env
// vars). Pass nil to disable all enricher-sourced labels — only labels
// from WithLabels will be attached.
func WithLabelEnricher(fn func(hostPID int) map[string]string) Option {
	return func(c *Config) {
		c.LabelEnricher = fn
		c.LabelEnricherSet = true
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

// WithPerfDataOutput enables writing a Linux perf.data file alongside the
// pprof output. Only on-CPU samples are emitted (off-CPU and PMU modes
// ignore this). The output is consumable by perf script, perf report,
// create_llvm_prof (AutoFDO PGO), FlameGraph, hotspot, etc.
func WithPerfDataOutput(path string) Option {
	return func(c *Config) { c.PerfDataOutput = path }
}

// WithDebuginfodURL appends a debuginfod server URL. Repeatable.
func WithDebuginfodURL(url string) Option {
	return func(c *Config) {
		c.DebuginfodURLs = append(c.DebuginfodURLs, url)
	}
}

// WithSymbolCacheDir overrides the debuginfod cache directory.
func WithSymbolCacheDir(dir string) Option {
	return func(c *Config) { c.SymbolCacheDir = dir }
}

// WithSymbolCacheMaxBytes overrides the debuginfod cache cap.
func WithSymbolCacheMaxBytes(n int64) Option {
	return func(c *Config) { c.SymbolCacheMaxBytes = n }
}

// WithSymbolFetchTimeout overrides per-artifact fetch timeout.
func WithSymbolFetchTimeout(d time.Duration) Option {
	return func(c *Config) { c.SymbolFetchTimeout = d }
}

// WithSymbolFailClosed enables fail-closed behavior on debuginfod errors.
func WithSymbolFailClosed() Option {
	return func(c *Config) { c.SymbolFailClosed = true }
}
