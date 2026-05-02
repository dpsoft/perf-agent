package perfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/cpu"
	"github.com/dpsoft/perf-agent/gpu"
	amdsample "github.com/dpsoft/perf-agent/gpu/backend/amdsample"
	linuxdrm "github.com/dpsoft/perf-agent/gpu/backend/linuxdrm"
	linuxkfd "github.com/dpsoft/perf-agent/gpu/backend/linuxkfd"
	"github.com/dpsoft/perf-agent/gpu/backend/replay"
	"github.com/dpsoft/perf-agent/gpu/backend/stream"
	hostsource "github.com/dpsoft/perf-agent/gpu/host"
	hiphost "github.com/dpsoft/perf-agent/gpu/host/hip"
	hostreplay "github.com/dpsoft/perf-agent/gpu/host/replay"
	"github.com/dpsoft/perf-agent/inject/ptraceop"
	"github.com/dpsoft/perf-agent/inject/python"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/offcpu"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

const defaultGPUHIPLinuxDRMJoinWindow = 5 * time.Millisecond

func debugGPULivef(format string, args ...any) {
	if os.Getenv("PERF_AGENT_DEBUG_GPU_LIVE") == "" {
		return
	}
	log.Printf("gpu-live-debug: "+format, args...)
}

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
	if c.GPULinuxKFD {
		if c.SystemWide {
			return errors.New("linuxkfd backend does not support system-wide mode")
		}
		if c.PID == 0 {
			return errors.New("linuxkfd backend requires pid")
		}
	}
	if c.GPUHostHIPLibrary != "" && c.PID == 0 {
		return errors.New("hip host source requires pid")
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

	if c.InjectPython && !c.EnableCPUProfile {
		return errors.New("--inject-python requires --profile (off-cpu and pmu are not supported)")
	}

	if c.InjectPython && !hasCapSysPtrace() {
		return errors.New("--inject-python requires CAP_SYS_PTRACE; use sudo or setcap")
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
	if c.GPUAMDSampleInput != nil {
		count++
	}
	if c.GPULinuxDRM {
		count++
	}
	if c.GPULinuxKFD {
		count++
	}
	return count
}

func (c *Config) hostSourceCount() int {
	count := 0
	if c.GPUHostReplayInput != "" {
		count++
	}
	if c.GPUHostHIPLibrary != "" {
		count++
	}
	return count
}

// hasCapSysPtrace reports whether the current process holds CAP_SYS_PTRACE
// in either the Permitted or Effective set. validate() runs before Start()
// promotes Permitted → Effective via SetFlag, so checking Effective alone
// would falsely reject runs where the cap was granted via setcap or
// inherited but not yet promoted. Mirrors test/integration_test.go's
// nil-safe probing against libcap.
func hasCapSysPtrace() bool {
	if os.Geteuid() == 0 {
		return true
	}
	caps := cap.GetProc()
	if caps == nil {
		return false
	}
	for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
		have, err := caps.GetFlag(flag, cap.SYS_PTRACE)
		if err == nil && have {
			return true
		}
	}
	return false
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
		if err := a.startGPUPipeline(ctx); err != nil {
			a.cleanup()
			return err
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
			hooks := dwarfHooksForAgent(a)
			p, err := dwarfagent.NewProfilerWithMode(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				hooks,
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
			hooks := dwarfHooksForAgent(a)
			p, err := dwarfagent.NewProfilerWithMode(
				a.config.PID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				hooks,
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

	if a.config.gpuSourceCount() == 1 {
		if err := a.startGPUPipeline(ctx); err != nil {
			a.cleanup()
			return err
		}
	}

	a.started = true
	return nil
}

func (a *Agent) startGPUPipeline(ctx context.Context) error {
	gpuBackend, err := a.newGPUBackend()
	if err != nil {
		return fmt.Errorf("create GPU backend: %w", err)
	}
	a.gpuManager = gpu.NewManager([]gpu.Backend{gpuBackend}, a.gpuManagerConfig())
	if err := a.gpuManager.Start(ctx); err != nil {
		return fmt.Errorf("start GPU manager: %w", err)
	}
	if a.config.hostSourceCount() == 1 {
		hostSource, err := a.newHostSource()
		if err != nil {
			return fmt.Errorf("create host source: %w", err)
		}
		a.hostSource = hostSource
		if err := a.hostSource.Start(ctx, hostsource.NewLaunchSink(a.gpuManager)); err != nil {
			return fmt.Errorf("start host source: %w", err)
		}
	}
	return nil
}

func (a *Agent) gpuManagerConfig() *gpu.ManagerConfig {
	if (!a.config.GPULinuxDRM && !a.config.GPULinuxKFD) || a.config.GPUHostHIPLibrary == "" {
		return nil
	}

	window := a.config.GPUHIPLinuxDRMJoinWindow
	if window <= 0 {
		window = defaultGPUHIPLinuxDRMJoinWindow
	}
	return &gpu.ManagerConfig{
		LaunchEventJoinWindowNs: uint64(window),
	}
}

func (a *Agent) newGPUBackend() (gpu.Backend, error) {
	switch {
	case a.config.GPUReplayInput != "":
		return replay.New(a.config.GPUReplayInput)
	case a.config.GPUStreamInput != nil:
		return stream.New(a.config.GPUStreamInput), nil
	case a.config.GPUAMDSampleInput != nil:
		return amdsample.New(amdsample.Config{Reader: a.config.GPUAMDSampleInput})
	case a.config.GPULinuxDRM:
		return linuxdrm.New(linuxdrm.Config{PID: a.config.PID})
	case a.config.GPULinuxKFD:
		return linuxkfd.New(linuxkfd.Config{PID: a.config.PID})
	default:
		return nil, errors.New("no gpu source configured")
	}
}

func (a *Agent) newHostSource() (hostsource.HostSource, error) {
	switch {
	case a.config.GPUHostReplayInput != "":
		return hostreplay.New(a.config.GPUHostReplayInput)
	case a.config.GPUHostHIPLibrary != "":
		return hiphost.New(hiphost.Config{
			PID:         a.config.PID,
			LibraryPath: a.config.GPUHostHIPLibrary,
			Symbol:      a.config.GPUHostHIPSymbol,
		})
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
		// Deactivate Python trampolines after profile finalization but before BPF
		// teardown.
		if a.pyInjector != nil {
			a.pyInjector.DeactivateAll(ctx)
		}
		if a.hostSource != nil {
			debugGPULivef("stopping host source")
			if err := a.hostSource.Stop(ctx); err != nil {
				debugGPULivef("host source stop error: %v", err)
				log.Printf("Failed to stop GPU host source: %v", err)
				lastErr = err
			} else {
				debugGPULivef("host source stopped")
			}
		}
		debugGPULivef("stopping gpu manager")
		if err := a.gpuManager.Stop(ctx); err != nil {
			debugGPULivef("gpu manager stop error: %v", err)
			log.Printf("Failed to stop GPU manager: %v", err)
			lastErr = err
		} else {
			debugGPULivef("gpu manager stopped")
		}
		debugGPULivef("building gpu snapshot")
		snapshot := a.gpuManager.Snapshot()
		if err := a.writeGPURaw(snapshot); err != nil {
			debugGPULivef("gpu raw write error: %v", err)
			log.Printf("Failed to write GPU raw output: %v", err)
			lastErr = err
		}
		if err := a.writeGPUAttributions(snapshot); err != nil {
			debugGPULivef("gpu attribution write error: %v", err)
			log.Printf("Failed to write GPU attribution output: %v", err)
			lastErr = err
		}
		if err := a.writeGPUProfile(snapshot); err != nil {
			debugGPULivef("gpu profile write error: %v", err)
			log.Printf("Failed to write GPU profile output: %v", err)
			lastErr = err
		}
		if err := a.writeGPUFolded(snapshot); err != nil {
			debugGPULivef("gpu folded write error: %v", err)
			log.Printf("Failed to write GPU folded output: %v", err)
			lastErr = err
		}
		debugGPULivef("gpu outputs written")
	}

	if a.gpuManager == nil && a.pyInjector != nil {
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

func (a *Agent) GPUEventBackends() ([]gpu.GPUBackendID, error) {
	if a.gpuManager != nil {
		return a.gpuManager.EventBackends(), nil
	}
	if a.config.gpuSourceCount() == 0 {
		return nil, nil
	}

	backend, err := a.newGPUBackend()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = backend.Close()
	}()
	return backend.EventBackends(), nil
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

func (a *Agent) writeGPUAttributions(snapshot gpu.Snapshot) error {
	switch {
	case a.config.GPUAttributionOutputWriter != nil:
		return gpu.WriteJSONAttributions(a.config.GPUAttributionOutputWriter, snapshot)
	case a.config.GPUAttributionOutputPath != "":
		f, err := os.Create(a.config.GPUAttributionOutputPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return gpu.WriteJSONAttributions(f, snapshot)
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

func (a *Agent) writeGPUFolded(snapshot gpu.Snapshot) error {
	switch {
	case a.config.GPUFoldedOutputWriter != nil:
		return gpu.WriteFoldedStacks(a.config.GPUFoldedOutputWriter, snapshot)
	case a.config.GPUFoldedOutputPath != "":
		f, err := os.Create(a.config.GPUFoldedOutputPath)
		if err != nil {
			return err
		}
		defer f.Close()
		return gpu.WriteFoldedStacks(f, snapshot)
	default:
		return nil
	}
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
// supplying the activate/deactivate payloads from inject/python and
// translating ptraceop's language-agnostic typed errors into Python-specific
// sentinels the manager can classify (e.g. PyRun_SimpleString returning -1
// → python.ErrNoPerfTrampoline so the manager records SkippedNoTramp instead
// of an opaque ActivateFailed).
type ptraceopBridge struct {
	inj *ptraceop.Injector
}

func (b *ptraceopBridge) RemoteActivate(pid uint32, addrs python.SymbolAddrsForTarget) error {
	err := b.inj.RemoteActivate(pid, ptraceop.SymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, python.ActivatePayload())
	return mapPtraceopErrToPython(err)
}

func (b *ptraceopBridge) RemoteDeactivate(pid uint32, addrs python.SymbolAddrsForTarget) error {
	err := b.inj.RemoteDeactivate(pid, ptraceop.SymbolAddrs{
		PyGILEnsure:  addrs.PyGILEnsure,
		PyGILRelease: addrs.PyGILRelease,
		PyRunString:  addrs.PyRunString,
	}, python.DeactivatePayload())
	return mapPtraceopErrToPython(err)
}

// mapPtraceopErrToPython translates a ptraceop typed error into a
// python-domain sentinel-wrapped error when the result code corresponds to
// a Python-level failure. PyRun_SimpleString returns -1 on any Python error;
// in the activate/deactivate payload context this is overwhelmingly the
// "perf trampoline not supported" path (the test gate already runs the
// payload from a normal interpreter, so structurally-different errors at
// inject time are rare and worth surfacing as ActivateFailed).
func mapPtraceopErrToPython(err error) error {
	if err == nil {
		return nil
	}
	var nonZero *ptraceop.ErrRemoteCallNonZero
	if errors.As(err, &nonZero) && int32(nonZero.Result) == -1 {
		return fmt.Errorf("activation refused (PyRun_SimpleString returned -1): %w",
			python.ErrNoPerfTrampoline)
	}
	return err
}

// dwarfHooksForAgent builds a *dwarfagent.Hooks for this agent. When
// --inject-python is enabled and the target is system-wide, OnNewExec is
// wired to pyInjector.ActivateLate so late-arriving Python processes are
// injected without a polling loop. When --inject-python is off (default),
// hooks is nil — the PIDTracker performs a single nil check per fork event
// and does zero additional work.
func dwarfHooksForAgent(a *Agent) *dwarfagent.Hooks {
	if a.pyInjector == nil || a.config.PID != 0 {
		// No injector, or per-PID mode: late subscription is a no-op
		// (single-PID already handled at startup).
		return nil
	}
	return &dwarfagent.Hooks{
		OnNewExec: a.pyInjector.ActivateLate,
	}
}
