package perfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strconv"
	"sync"

	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/cpu"
	"github.com/dpsoft/perf-agent/inject/ptraceop"
	"github.com/dpsoft/perf-agent/inject/python"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/offcpu"
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
	pyInjector     *python.Manager // nil unless --inject-python is set

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

	a := &Agent{config: config}

	if config.InjectPython {
		low := &ptraceopBridge{inj: ptraceop.New(slog.Default())}
		a.pyInjector = python.NewManager(python.Options{
			StrictPerPID: config.PID != 0, // single-PID is strict; -a is lenient
			Logger:       slog.Default(),
			Detector:     python.NewDetector("/proc", slog.Default()),
			Injector:     low,
		})
	}

	return a, nil
}

// validate checks the configuration for errors.
func (c *Config) validate() error {
	if !c.EnableCPUProfile && !c.EnableOffCPUProfile && !c.EnablePMU {
		return errors.New("at least one of CPU profile, off-CPU profile, or PMU must be enabled")
	}

	if c.PID != 0 && c.SystemWide {
		return errors.New("PID and system-wide are mutually exclusive")
	}

	if c.PID == 0 && !c.SystemWide {
		return errors.New("either PID or system-wide is required")
	}

	if c.PerPID && !c.SystemWide {
		return errors.New("per-PID requires system-wide mode")
	}

	if c.PerPID && !c.EnablePMU {
		return errors.New("per-PID is only valid with PMU enabled")
	}

	if c.InjectPython && !c.EnableCPUProfile {
		return errors.New("--inject-python requires --profile (off-cpu and pmu are not supported)")
	}

	if c.InjectPython && !hasCapSysPtrace() {
		return errors.New("--inject-python requires CAP_SYS_PTRACE; use sudo or setcap")
	}

	return nil
}

// hasCapSysPtrace returns true if the current process holds CAP_SYS_PTRACE
// in its effective set. Uses the libcap package already imported by the agent.
func hasCapSysPtrace() bool {
	if os.Geteuid() == 0 {
		return true
	}
	caps := cap.GetProc()
	have, err := caps.GetFlag(cap.Effective, cap.SYS_PTRACE)
	if err != nil {
		return false
	}
	return have
}

// Start initializes and starts all enabled profilers.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.started {
		return errors.New("agent already started")
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

	// Inject Python perf-trampoline before BPF attach so early samples have
	// JIT symbol names. Runs only when --inject-python is set.
	if a.pyInjector != nil {
		pids := a.scanPythonTargets()
		if err := a.pyInjector.ActivateAll(pids); err != nil {
			return fmt.Errorf("python injection: %w", err)
		}
	}

	// Start CPU profiler if enabled
	if a.config.EnableCPUProfile {
		switch a.config.Unwind {
		case "dwarf":
			p, err := dwarfagent.NewProfilerWithMode(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				nil,
				dwarfagent.ModeEager,
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
		case "auto":
			p, err := dwarfagent.NewProfilerWithMode(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				nil,
				dwarfagent.ModeLazy,
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

	// Deactivate Python trampolines after profile finalization but before BPF
	// teardown. Tolerates ESRCH (process gone) and respects the 5s deadline.
	if a.pyInjector != nil {
		a.pyInjector.DeactivateAll(ctx)
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
}

// Config returns a copy of the agent's configuration.
func (a *Agent) Config() Config {
	return *a.config
}

// PythonInjectStats returns counters for the Python injector. Returns a
// zero-value Stats if --inject-python was not enabled.
func (a *Agent) PythonInjectStats() *python.Stats {
	if a.pyInjector == nil {
		return &python.Stats{}
	}
	return a.pyInjector.Stats()
}

// scanPythonTargets returns the PIDs to consider for injection. For --pid
// mode, just [cfg.PID]. For -a mode, walks /proc and returns all numeric PID
// directories (the Manager's Detect call filters down to actual Python
// processes).
func (a *Agent) scanPythonTargets() []uint32 {
	if a.config.PID != 0 {
		return []uint32{uint32(a.config.PID)}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	pids := make([]uint32, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		pids = append(pids, uint32(v))
	}
	return pids
}

// ptraceopBridge adapts ptraceop.Injector to python.LowLevelInjector,
// supplying the activate/deactivate payloads from inject/python.
type ptraceopBridge struct {
	inj *ptraceop.Injector
}

func (b *ptraceopBridge) RemoteActivate(pid uint32, addrs python.SymbolAddrsForTarget) error {
	return b.inj.RemoteActivate(pid, ptraceop.SymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, python.ActivatePayload())
}

func (b *ptraceopBridge) RemoteDeactivate(pid uint32, addrs python.SymbolAddrsForTarget) error {
	return b.inj.RemoteDeactivate(pid, ptraceop.SymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, python.DeactivatePayload())
}
