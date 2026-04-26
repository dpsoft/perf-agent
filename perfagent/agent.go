package perfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/cpu"
	"github.com/dpsoft/perf-agent/gpu"
	linuxdrm "github.com/dpsoft/perf-agent/gpu/backend/linuxdrm"
	"github.com/dpsoft/perf-agent/gpu/backend/replay"
	"github.com/dpsoft/perf-agent/gpu/backend/stream"
	hostsource "github.com/dpsoft/perf-agent/gpu/host"
	hostreplay "github.com/dpsoft/perf-agent/gpu/host/replay"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/offcpu"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

// cpuProfiler is the narrow shape both profile.Profiler and
// dwarfagent.Profiler satisfy, letting Agent dispatch on --unwind.
type cpuProfiler interface {
	Collect(w io.Writer) error
	CollectAndWrite(path string) error
	Close()
}

// dwarfProfilerAdapter wraps dwarfagent.Profiler so its Close() matches
// the void Close() the FP profiler exposes (see cpuProfiler interface).
type dwarfProfilerAdapter struct{ *dwarfagent.Profiler }

func (a dwarfProfilerAdapter) Close() {
	if err := a.Profiler.Close(); err != nil {
		log.Printf("dwarfagent.Close: %v", err)
	}
}

// offcpuProfiler is the narrow shape both offcpu.Profiler and
// dwarfagent.OffCPUProfiler satisfy, letting Agent dispatch on --unwind
// for the off-CPU path.
type offcpuProfiler interface {
	Collect(w io.Writer) error
	CollectAndWrite(path string) error
	Close()
}

// dwarfOffCPUProfilerAdapter wraps dwarfagent.OffCPUProfiler so its
// Close() matches the void Close() the FP offcpu profiler exposes.
type dwarfOffCPUProfilerAdapter struct{ *dwarfagent.OffCPUProfiler }

func (a dwarfOffCPUProfilerAdapter) Close() {
	if err := a.OffCPUProfiler.Close(); err != nil {
		log.Printf("dwarfagent.OffCPUProfiler.Close: %v", err)
	}
}

// Agent is the main performance monitoring agent.
type Agent struct {
	config *Config

	cpuProfiler    cpuProfiler
	offcpuProfiler offcpuProfiler
	pmuMonitor     *cpu.PMUMonitor
	gpuManager     *gpu.Manager
	hostSource     hostsource.HostSource

	mu      sync.Mutex
	started bool
}

// New creates a new Agent with the given options.
func New(opts ...Option) (*Agent, error) {
	config := DefaultConfig()
	for _, opt := range opts {
		opt(config)
	}

	// Validate configuration
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &Agent{
		config: config,
	}, nil
}

// validate checks the configuration for errors.
func (c *Config) validate() error {
	if c.gpuSourceCount() > 1 {
		return errors.New("gpu source options are mutually exclusive")
	}
	if c.hostSourceCount() > 1 {
		return errors.New("gpu host source options are mutually exclusive")
	}

	if !c.EnableCPUProfile && !c.EnableOffCPUProfile && !c.EnablePMU && c.gpuSourceCount() == 0 && c.hostSourceCount() == 0 {
		return errors.New("at least one of CPU profile, off-CPU profile, PMU, a GPU source, or a GPU host source must be enabled")
	}
	if c.hostSourceCount() > 0 && c.gpuSourceCount() == 0 {
		return errors.New("gpu host source requires a gpu source")
	}

	if c.GPULinuxDRM {
		if c.SystemWide {
			return errors.New("linuxdrm backend does not support system-wide mode")
		}
		if c.PID == 0 {
			return errors.New("linuxdrm backend requires pid")
		}
	}

	if c.gpuSourceCount() == 0 && c.hostSourceCount() == 0 {
		if c.PID == 0 && !c.SystemWide {
			return errors.New("either PID or system-wide is required")
		}
	} else if c.PID == 0 && !c.SystemWide && !c.EnableCPUProfile && !c.EnableOffCPUProfile && !c.EnablePMU {
		return nil
	}

	if c.PID != 0 && c.SystemWide {
		return errors.New("PID and system-wide are mutually exclusive")
	}

	if c.PerPID && !c.SystemWide {
		return errors.New("per-PID requires system-wide mode")
	}

	if c.PerPID && !c.EnablePMU {
		return errors.New("per-PID is only valid with PMU enabled")
	}

	return nil
}

func (c *Config) gpuSourceCount() int {
	count := 0
	if c.GPUReplayInput != "" {
		count++
	}
	if c.GPUStreamInput != nil {
		count++
	}
	if c.GPULinuxDRM {
		count++
	}
	return count
}

func (c *Config) hostSourceCount() int {
	count := 0
	if c.GPUHostReplayInput != "" {
		count++
	}
	return count
}

// Start initializes and starts all enabled profilers.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.started {
		return errors.New("agent already started")
	}

	gpuMode := a.config.gpuSourceCount() == 1 && !a.config.EnableCPUProfile && !a.config.EnableOffCPUProfile && !a.config.EnablePMU
	if gpuMode {
		gpuBackend, err := a.newGPUBackend()
		if err != nil {
			return fmt.Errorf("create GPU backend: %w", err)
		}
		a.gpuManager = gpu.NewManager([]gpu.Backend{gpuBackend}, nil)
		if err := a.gpuManager.Start(ctx); err != nil {
			a.cleanup()
			return fmt.Errorf("start GPU manager: %w", err)
		}
		if a.config.hostSourceCount() == 1 {
			hostSource, err := a.newHostSource()
			if err != nil {
				a.cleanup()
				return fmt.Errorf("create host source: %w", err)
			}
			a.hostSource = hostSource
			if err := a.hostSource.Start(ctx, hostsource.NewLaunchSink(a.gpuManager)); err != nil {
				a.cleanup()
				return fmt.Errorf("start host source: %w", err)
			}
		}
		a.started = true
		return nil
	}

	// Set capabilities:
	//   CAP_SYS_ADMIN          - perf_event_open with pid=-1 (system-wide perf events)
	//   CAP_BPF                - load eBPF programs and create maps
	//   CAP_PERFMON            - perf_event_open, stack traces, tracing attachment
	//   CAP_SYS_PTRACE         - read /proc/<pid>/maps and /proc/<pid>/mem of other processes
	//   CAP_CHECKPOINT_RESTORE - follow /proc/<pid>/map_files/ symlinks (blazesym symbolization)
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE); err != nil {
		return fmt.Errorf("set capabilities: %w", err)
	}

	// Remove memlock limit
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock: %w", err)
	}

	// Get CPUs to monitor
	cpus := a.config.CPUs
	if len(cpus) == 0 {
		var err error
		cpus, err = cpuonline.Get()
		if err != nil {
			return fmt.Errorf("get online CPUs: %w", err)
		}
	}

	// Start CPU profiler if enabled
	if a.config.EnableCPUProfile {
		switch a.config.Unwind {
		case "dwarf", "auto":
			p, err := dwarfagent.NewProfiler(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
			)
			if err != nil {
				return fmt.Errorf("create DWARF CPU profiler: %w", err)
			}
			a.cpuProfiler = dwarfProfilerAdapter{p}
			if a.config.SystemWide {
				log.Printf("CPU profiler enabled (system-wide, %d Hz, DWARF)", a.config.SampleRate)
			} else {
				log.Printf("CPU profiler enabled (PID: %d, %d Hz, DWARF)", a.config.PID, a.config.SampleRate)
			}
		default:
			profiler, err := profile.NewProfiler(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
			)
			if err != nil {
				return fmt.Errorf("create CPU profiler: %w", err)
			}
			a.cpuProfiler = profiler
			if a.config.SystemWide {
				log.Printf("CPU profiler enabled (system-wide, %d Hz)", a.config.SampleRate)
			} else {
				log.Printf("CPU profiler enabled (PID: %d, %d Hz)", a.config.PID, a.config.SampleRate)
			}
		}
	}

	// Start off-CPU profiler if enabled
	if a.config.EnableOffCPUProfile {
		switch a.config.Unwind {
		case "dwarf", "auto":
			p, err := dwarfagent.NewOffCPUProfiler(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
			)
			if err != nil {
				a.cleanup()
				return fmt.Errorf("create DWARF off-CPU profiler: %w", err)
			}
			a.offcpuProfiler = dwarfOffCPUProfilerAdapter{p}
			if a.config.SystemWide {
				log.Println("Off-CPU profiler enabled (system-wide, DWARF)")
			} else {
				log.Printf("Off-CPU profiler enabled (PID: %d, DWARF)", a.config.PID)
			}
		default:
			profiler, err := offcpu.NewProfiler(
				a.config.PID,
				a.config.SystemWide,
				a.config.Tags,
			)
			if err != nil {
				a.cleanup()
				return fmt.Errorf("create off-CPU profiler: %w", err)
			}
			a.offcpuProfiler = profiler
			if a.config.SystemWide {
				log.Println("Off-CPU profiler enabled (system-wide)")
			} else {
				log.Printf("Off-CPU profiler enabled (PID: %d)", a.config.PID)
			}
		}
	}

	// Start PMU monitor if enabled
	if a.config.EnablePMU {
		monitor, err := cpu.NewPMUMonitor(
			a.config.PID,
			a.config.SystemWide,
			cpus,
		)
		if err != nil {
			a.cleanup()
			return fmt.Errorf("create PMU monitor: %w", err)
		}
		a.pmuMonitor = monitor
		if a.config.SystemWide {
			log.Println("PMU monitor enabled (system-wide)")
		} else {
			log.Printf("PMU monitor enabled (PID: %d)", a.config.PID)
		}
	}

	a.started = true
	return nil
}

func (a *Agent) newGPUBackend() (gpu.Backend, error) {
	switch {
	case a.config.GPUReplayInput != "":
		return replay.New(a.config.GPUReplayInput)
	case a.config.GPUStreamInput != nil:
		return stream.New(a.config.GPUStreamInput), nil
	case a.config.GPULinuxDRM:
		return linuxdrm.New(linuxdrm.Config{PID: a.config.PID})
	default:
		return nil, errors.New("no gpu source configured")
	}
}

func (a *Agent) newHostSource() (hostsource.HostSource, error) {
	switch {
	case a.config.GPUHostReplayInput != "":
		return hostreplay.New(a.config.GPUHostReplayInput)
	default:
		return nil, errors.New("no gpu host source configured")
	}
}

// Stop stops data collection and writes profiles.
func (a *Agent) Stop(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.started {
		return errors.New("agent not started")
	}

	var lastErr error

	// Export PMU metrics
	if a.pmuMonitor != nil {
		if len(a.config.MetricsExporters) > 0 {
			if err := a.pmuMonitor.ExportMetrics(ctx, a.config.SystemWide, a.config.MetricsExporters...); err != nil {
				log.Printf("Failed to export metrics: %v", err)
				lastErr = err
			}
		} else {
			// Fall back to console output for backward compatibility
			a.pmuMonitor.PrintMetrics(a.config.SystemWide, a.config.PerPID)
		}
	}

	// Write CPU profile
	if a.cpuProfiler != nil {
		if a.config.CPUProfileWriter != nil {
			if err := a.cpuProfiler.Collect(a.config.CPUProfileWriter); err != nil {
				log.Printf("Failed to write CPU profile: %v", err)
				lastErr = err
			}
		} else {
			if err := a.cpuProfiler.CollectAndWrite(a.config.CPUProfilePath); err != nil {
				log.Printf("Failed to write CPU profile: %v", err)
				lastErr = err
			}
		}
	}

	// Write off-CPU profile
	if a.offcpuProfiler != nil {
		if a.config.OffCPUProfileWriter != nil {
			if err := a.offcpuProfiler.Collect(a.config.OffCPUProfileWriter); err != nil {
				log.Printf("Failed to write off-CPU profile: %v", err)
				lastErr = err
			}
		} else {
			if err := a.offcpuProfiler.CollectAndWrite(a.config.OffCPUProfilePath); err != nil {
				log.Printf("Failed to write off-CPU profile: %v", err)
				lastErr = err
			}
		}
	}

	if a.gpuManager != nil {
		if a.hostSource != nil {
			if err := a.hostSource.Stop(ctx); err != nil {
				log.Printf("Failed to stop GPU host source: %v", err)
				lastErr = err
			}
		}
		if err := a.gpuManager.Stop(ctx); err != nil {
			log.Printf("Failed to stop GPU manager: %v", err)
			lastErr = err
		}
		snapshot := a.gpuManager.Snapshot()
		if err := a.writeGPURaw(snapshot); err != nil {
			log.Printf("Failed to write GPU raw output: %v", err)
			lastErr = err
		}
		if err := a.writeGPUProfile(snapshot); err != nil {
			log.Printf("Failed to write GPU profile output: %v", err)
			lastErr = err
		}
	}

	return lastErr
}

// GetMetrics returns the current PMU metrics snapshot.
func (a *Agent) GetMetrics() (*metrics.MetricsSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.started {
		return nil, errors.New("agent not started")
	}

	if a.pmuMonitor == nil {
		return nil, errors.New("PMU monitor not enabled")
	}

	return a.pmuMonitor.GetMetricsSnapshot(a.config.SystemWide), nil
}

// Close releases all resources associated with the agent.
func (a *Agent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.cleanup()
	a.started = false
	return nil
}

// cleanup releases profiler resources.
func (a *Agent) cleanup() {
	if a.cpuProfiler != nil {
		a.cpuProfiler.Close()
		a.cpuProfiler = nil
	}
	if a.offcpuProfiler != nil {
		a.offcpuProfiler.Close()
		a.offcpuProfiler = nil
	}
	if a.pmuMonitor != nil {
		a.pmuMonitor.Close()
		a.pmuMonitor = nil
	}
	if a.hostSource != nil {
		_ = a.hostSource.Close()
		a.hostSource = nil
	}
	if a.gpuManager != nil {
		_ = a.gpuManager.Close()
		a.gpuManager = nil
	}
}

// Config returns a copy of the agent's configuration.
func (a *Agent) Config() Config {
	return *a.config
}

func (a *Agent) writeGPURaw(snapshot gpu.Snapshot) error {
	switch {
	case a.config.GPURawOutputWriter != nil:
		return gpu.WriteJSONSnapshot(a.config.GPURawOutputWriter, snapshot)
	case a.config.GPURawOutputPath != "":
		f, err := os.Create(a.config.GPURawOutputPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return gpu.WriteJSONSnapshot(f, snapshot)
	default:
		return nil
	}
}

func (a *Agent) writeGPUProfile(snapshot gpu.Snapshot) error {
	samples := gpu.ProjectExecutionSamples(snapshot)
	if len(samples) == 0 {
		return nil
	}

	var w io.Writer
	switch {
	case a.config.GPUProfileOutputWriter != nil:
		w = a.config.GPUProfileOutputWriter
	case a.config.GPUProfileOutputPath != "":
		f, err := os.Create(a.config.GPUProfileOutputPath)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	default:
		return nil
	}

	builders := pp.NewProfileBuilders(pp.BuildersOptions{
		SampleRate:    int64(max(1, a.config.SampleRate)),
		PerPIDProfile: false,
		Comments:      a.config.Tags,
	})
	for i := range samples {
		builders.AddSample(&samples[i])
	}
	for _, builder := range builders.Builders {
		_, err := builder.Write(w)
		return err
	}
	return nil
}
