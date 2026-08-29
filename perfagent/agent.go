package perfagent

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/cpu"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/k8slabels"
	"github.com/dpsoft/perf-agent/internal/nspid"
	"github.com/dpsoft/perf-agent/internal/perfdata"
	"github.com/dpsoft/perf-agent/internal/perfevent"
	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/offcpu"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/debuginfod"
	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
	"github.com/dpsoft/perf-agent/unwind/procmap"
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
	perfDataWriter *perfdata.Writer // nil when --perf-data-output not set

	// symbolizer is the agent-owned shared symbol resolver. Selected at
	// Start() time based on whether DebuginfodURLs is non-empty.
	symbolizer symbolize.Symbolizer

	// kernelSymbolizer resolves kernel-space addresses. Initialized in
	// Start() when cfg.KernelStacks is true; otherwise NoopKernelSymbolizer.
	kernelSymbolizer symbolize.KernelSymbolizer

	// metricsSrv is the optional HTTP server hosting /metrics and
	// /debug/pprof. nil when WithMetricsListen wasn't used.
	metricsSrv *metricsServer

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

	return nil
}

// resolveTarget translates the configured PID to its host-namespace
// counterpart and computes the final per-sample label set by running the
// configured enricher (default: internal/k8slabels.FromPID) and merging
// the static labels from WithLabels on top.
//
// If config.PID is 0 (system-wide -a mode), no translation runs and
// labels come solely from WithLabels and from any user-supplied enricher
// invoked with hostPID=0.
func (a *Agent) resolveTarget() (hostPID int, labels map[string]string, err error) {
	hostPID = a.config.PID
	if hostPID > 0 {
		hostPID, err = nspid.Translate(a.config.PID)
		if err != nil {
			return 0, nil, fmt.Errorf("resolve target pid: %w", err)
		}
	}

	labels = make(map[string]string, 8)

	// Default enricher unless the caller explicitly disabled or replaced.
	enricher := a.config.LabelEnricher
	if !a.config.LabelEnricherSet {
		enricher = func(pid int) map[string]string {
			if pid <= 0 {
				return nil
			}
			out, err := k8slabels.FromPID("/proc", pid)
			if err != nil {
				log.Printf("k8slabels.FromPID(%d): %v (continuing without k8s labels)", pid, err)
				return nil
			}
			return out
		}
	}
	if enricher != nil {
		maps.Copy(labels, enricher(hostPID))
	}
	maps.Copy(labels, a.config.Labels) // WithLabels wins on key collision
	return hostPID, labels, nil
}

// pidLogStr returns "N" when hostPID equals the user-visible PID,
// otherwise "N (host: M)" so sidecar deployments see both.
func (a *Agent) pidLogStr(hostPID int) string {
	if hostPID == a.config.PID {
		return fmt.Sprintf("%d", hostPID)
	}
	return fmt.Sprintf("%d (host: %d)", a.config.PID, hostPID)
}

// chooseSymbolizer constructs the agent-owned symbolizer. If DebuginfodURLs
// is non-empty (or the DEBUGINFOD_URLS env var is set), a Debuginfod
// symbolizer is returned; otherwise the local blazesym symbolizer is used.
// debuginfodURLsFromEnv extracts the server URLs from a DEBUGINFOD_URLS value.
//
// The variable is whitespace-separated, but it is NOT only URLs. elfutils'
// debuginfod-client also accepts policy tokens in the same list, and Fedora
// ships exactly that by default:
//
//	DEBUGINFOD_URLS="ima:enforcing https://debuginfod.fedoraproject.org/ ima:ignore"
//
// Taking every field as a server, which is what this did, hands two
// non-servers to the fetcher and asks it to resolve build-ids against them.
// Only http and https are servers here; anything else is a token this agent
// does not implement and must ignore rather than dial. Issue #109.
func debuginfodURLsFromEnv(env string) []string {
	var urls []string
	for f := range strings.FieldsSeq(env) {
		u, err := url.Parse(f)
		if err != nil {
			continue
		}
		// Scheme AND host: "https://" alone parses cleanly and is not a
		// server, and a bare path picks up no scheme at all.
		if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			urls = append(urls, f)
		}
	}
	return urls
}

func chooseSymbolizer(cfg *Config, res *procmap.Resolver, logger *slog.Logger) (symbolize.Symbolizer, error) {
	urls := cfg.DebuginfodURLs
	if len(urls) == 0 {
		urls = debuginfodURLsFromEnv(os.Getenv("DEBUGINFOD_URLS"))
	}
	if len(urls) == 0 {
		// res names the module behind an address blazesym could not resolve
		// to a symbol, so an unresolved frame reads "libcuda.so.1+0x1b71c6"
		// instead of an ASLR'd address. It is the same Resolver the
		// debuginfod branch below takes, so exactly one maps cache exists
		// per agent either way.
		//
		// It is never invalidated, which it shares with the Resolvers
		// profile/ and offcpu/ hand to the pprof builders: a PID that exits
		// and is reused within one run would keep the first process's
		// mappings. Adding module attribution here does not widen that
		// window - the builders already attribute every user frame through
		// the same kind of cache - but it does not narrow it either.
		return symbolize.NewLocalSymbolizer(symbolize.WithModuleIndex(res))
	}
	// Announced, because it is the single biggest thing that can change how
	// long a capture takes and it was previously silent. Measured on a
	// workstation: the same 5s system-wide capture took 6.4s with the local
	// symbolizer and 87s with debuginfod reachable, with nothing in the
	// output to say why (issue #109). DEBUGINFOD_URLS is set by default on
	// several distributions, so this is on for many users who never asked.
	log.Printf("perf-agent: debuginfod symbolization ENABLED (%d server(s): %s). "+
		"Debug info is fetched over the network on first contact with each "+
		"unknown build-id, which can take far longer than the capture itself; "+
		"unset DEBUGINFOD_URLS or pass --debuginfod-urls='' to use local "+
		"symbols only",
		len(urls), strings.Join(urls, " "))
	cacheDir := cmp.Or(cfg.SymbolCacheDir, "/tmp/perf-agent-debuginfod")
	cacheMax := cmp.Or(cfg.SymbolCacheMaxBytes, int64(2<<30))
	// Left to the package default (5s) unless configured; see its rationale
	// in debuginfod.Options.validate. Raising it trades capture latency for
	// source detail on slow servers.
	timeout := cfg.SymbolFetchTimeout
	return debuginfod.New(debuginfod.Options{
		URLs:          urls,
		CacheDir:      cacheDir,
		CacheMaxBytes: cacheMax,
		FetchTimeout:  timeout,
		FailClosed:    cfg.SymbolFailClosed,
		Resolver:      res,
		Logger:        logger,
		Demangle:      true,
		InlinedFns:    true,
		CodeInfo:      true,
	})
}

// chooseKernelSymbolizer returns LocalKernelSymbolizer when cfg.KernelStacks
// is true and /proc/kallsyms is readable; otherwise NoopKernelSymbolizer (and
// a one-time warning if the user opted in but kallsyms is locked down). When
// cfg.KernelStacks is false, returns NoopKernelSymbolizer silently — the user
// did not opt in.
func chooseKernelSymbolizer(cfg *Config, logger *slog.Logger) symbolize.KernelSymbolizer {
	if !cfg.KernelStacks {
		return symbolize.NoopKernelSymbolizer{}
	}
	s, err := symbolize.NewLocalKernelSymbolizer()
	if err != nil {
		if logger != nil {
			logger.Warn("kernel symbols unavailable; kernel frames will be raw addresses",
				"error", err,
				"hint", "sysctl kernel.kptr_restrict=0 (and ensure perf_event_paranoid <= 2)")
		}
		return symbolize.NoopKernelSymbolizer{}
	}
	return s
}

// Start initializes and starts all enabled profilers.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.started {
		return errors.New("agent already started")
	}

	// Capabilities raised to Effective. The set is deliberately the historical
	// superset so that pre-5.8 kernels keep working:
	//
	//   CAP_BPF                - load eBPF programs and create maps
	//   CAP_PERFMON            - perf_event_open, stack traces, tracing attachment
	//   CAP_SYS_PTRACE         - read /proc/<pid>/maps and /proc/<pid>/mem of other processes
	//   CAP_CHECKPOINT_RESTORE - follow /proc/<pid>/map_files/ symlinks (blazesym symbolization)
	//   CAP_SYS_ADMIN          - only needed on kernels older than 5.8/5.9; see below
	//
	// CAP_SYS_ADMIN is retained for backward compatibility, not because current
	// kernels need it. Two capabilities were introduced to carve out its roles,
	// but they did not arrive together:
	//
	//   - CAP_PERFMON (5.8) covers perf_event_open, including pid=-1 for
	//     system-wide profiling.
	//   - CAP_CHECKPOINT_RESTORE (5.9) covers /proc/<pid>/map_files.
	//
	// That one-version gap is the whole reason this is still here. The project
	// documents a floor of kernel 5.8, and on exactly 5.8 CAP_CHECKPOINT_RESTORE
	// does not exist, so dropping CAP_SYS_ADMIN would break symbolization there.
	//
	// On kernel >= 5.9 the minimal working set is:
	//
	//   cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep
	//
	// Dropping CAP_SYS_ADMIN matters for per-pod and sidecar deployments, where
	// it is the near-root capability that gets a workload rejected. The condition
	// is not "wait for newer kernels" - deployments already target 6.x. It is:
	// raise the documented floor from 5.8 to 5.9+, then verify the minimal set
	// for both --pid and -a runs (-a is the one that exercises
	// perf_event_open(pid=-1)), then drop it here.
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE); err != nil {
		return fmt.Errorf("set capabilities: %w", err)
	}

	// Remove memlock limit
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock: %w", err)
	}

	// Translate PID to host namespace and collect labels.
	hostPID, labels, err := a.resolveTarget()
	if err != nil {
		return err
	}
	// hostPID replaces config.PID for downstream BPF setup;
	// labels are passed to every profiler constructor.

	// Get CPUs to monitor
	cpus := a.config.CPUs
	if len(cpus) == 0 {
		var err error
		cpus, err = cpuonline.Get()
		if err != nil {
			return fmt.Errorf("get online CPUs: %w", err)
		}
	}

	// Open perf.data writer if --perf-data-output set. Probe HW cycles up
	// front so the writer's attr matches the perf events we'll actually
	// open in the profiler — and pass the same spec into the profiler's
	// constructor below so perf_event_open and the perf.data attr stay
	// in sync. Otherwise consumers see HW/cycles in the header but real
	// SW/cpu-clock samples, and weight attribution silently drifts.
	var profilerEventSpec *perfevent.EventSpec
	if a.config.PerfDataOutput != "" {
		spec, err := perfevent.ProbeHardwareCycles(uint64(a.config.SampleRate))
		if err != nil {
			return fmt.Errorf("probe perf event for perf.data: %w", err)
		}
		log.Printf("perf-agent: perf.data event = %s", spec)
		profilerEventSpec = &spec

		hostname, _ := os.Hostname()
		w, err := perfdata.Open(a.config.PerfDataOutput, perfdata.EventSpec{
			Type:         spec.Type,
			Config:       spec.Config,
			SamplePeriod: spec.SamplePeriod,
			Frequency:    spec.Frequency,
		}, perfdata.MetaInfo{
			Hostname:  hostname,
			OSRelease: readOSRelease(),
			NumCPUs:   uint32(len(cpus)),
		})
		if err != nil {
			return fmt.Errorf("open perf.data: %w", err)
		}
		a.perfDataWriter = w
	}

	if a.perfDataWriter != nil && a.config.KernelStacks {
		if err := a.perfDataWriter.AddKernelMmap(); err != nil {
			log.Printf("perfdata: AddKernelMmap: %v (continuing; kernel symbol resolution may be limited)", err)
		}
	}

	if a.perfDataWriter != nil {
		// Synthesize userspace MMAP2 records from /proc/<pid>/maps so
		// `perf script` / `perf report` can resolve user-space IPs
		// against on-disk binaries. Without these, every userspace
		// frame in the resulting perf.data shows up as [unknown].
		r := procmap.NewResolver()
		if hostPID != 0 {
			emitCommForPID(a.perfDataWriter, hostPID)
			emitUserspaceMmapsForPID(a.perfDataWriter, r, hostPID)
		} else {
			// System-wide: emit COMM + MMAP2 lazily, on the first
			// sample observed per PID. The previous "walk every
			// /proc PID at writer init" pass cost ~30% of
			// perf-agent CPU on a busy host (dogfood iter 9 found
			// kernel /proc/<pid>/maps rendering — show_map_vma,
			// mangle_path, lock_next_vma — dominating the
			// profile). Lazy emission also automatically covers
			// PIDs that exec after capture starts; the eager
			// walk could never see those.
			a.perfDataWriter.OnNewPID = func(pid uint32) {
				emitCommForPID(a.perfDataWriter, int(pid))
				emitUserspaceMmapsForPID(a.perfDataWriter, r, int(pid))
			}
		}
	}

	// Construct the shared symbolizer once. All profilers below share the
	// same instance. chooseSymbolizer picks LocalSymbolizer when no
	// debuginfod URLs are configured, or DebuginfodSymbolizer otherwise.
	sym, err := chooseSymbolizer(a.config, procmap.NewResolver(), slog.Default())
	if err != nil {
		return fmt.Errorf("symbolizer: %w", err)
	}
	a.symbolizer = sym
	a.kernelSymbolizer = chooseKernelSymbolizer(a.config, slog.Default())

	// Optional /metrics + /debug/pprof endpoint. Started after the
	// kernelSymbolizer is constructed so the handler closure can
	// snapshot live counters; stopped in cleanup() after the
	// symbolizer is closed so a late scrape can't race.
	if a.config.MetricsListen != "" {
		srv, err := startMetricsListener(a.config.MetricsListen, func() symbolize.CountersSnapshot {
			if lks, ok := a.kernelSymbolizer.(*symbolize.LocalKernelSymbolizer); ok {
				return lks.Stats()
			}
			return symbolize.CountersSnapshot{}
		})
		if err != nil {
			return fmt.Errorf("start metrics listener: %w", err)
		}
		a.metricsSrv = srv
		log.Printf("perf-agent: metrics endpoint listening on http://%s/metrics (and /debug/pprof)", srv.addr)
	}

	// Start CPU profiler if enabled
	if a.config.EnableCPUProfile {
		switch a.config.Unwind {
		case "dwarf":
			p, err := dwarfagent.NewProfilerWithMode(
				hostPID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				nil, // dwarfagent.Hooks: no observers wired
				dwarfagent.ModeEager,
				labels,
				a.perfDataWriter,
				profilerEventSpec,
				a.symbolizer,
				a.kernelSymbolizer,
				a.config.KernelStacks,
			)
			if err != nil {
				return fmt.Errorf("create DWARF CPU profiler: %w", err)
			}
			a.cpuProfiler = dwarfProfilerAdapter{p}
			if a.config.SystemWide {
				log.Printf("CPU profiler enabled (system-wide, %d Hz, DWARF)", a.config.SampleRate)
			} else {
				log.Printf("CPU profiler enabled (PID: %s, %d Hz, DWARF)", a.pidLogStr(hostPID), a.config.SampleRate)
			}
		case "auto":
			p, err := dwarfagent.NewProfilerWithMode(
				hostPID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				nil, // dwarfagent.Hooks: no observers wired
				dwarfagent.ModeLazy,
				labels,
				a.perfDataWriter,
				profilerEventSpec,
				a.symbolizer,
				a.kernelSymbolizer,
				a.config.KernelStacks,
			)
			if err != nil {
				return fmt.Errorf("create DWARF CPU profiler: %w", err)
			}
			a.cpuProfiler = dwarfProfilerAdapter{p}
			if a.config.SystemWide {
				log.Printf("CPU profiler enabled (system-wide, %d Hz, DWARF)", a.config.SampleRate)
			} else {
				log.Printf("CPU profiler enabled (PID: %s, %d Hz, DWARF)", a.pidLogStr(hostPID), a.config.SampleRate)
			}
		default:
			profiler, err := profile.NewProfiler(
				hostPID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				a.config.SampleRate,
				labels,
				a.perfDataWriter,
				profilerEventSpec,
				a.symbolizer,
				a.kernelSymbolizer,
				a.config.KernelStacks,
			)
			if err != nil {
				return fmt.Errorf("create CPU profiler: %w", err)
			}
			a.cpuProfiler = profiler
			if a.config.SystemWide {
				log.Printf("CPU profiler enabled (system-wide, %d Hz)", a.config.SampleRate)
			} else {
				log.Printf("CPU profiler enabled (PID: %s, %d Hz)", a.pidLogStr(hostPID), a.config.SampleRate)
			}
		}
	}

	// Start off-CPU profiler if enabled
	if a.config.EnableOffCPUProfile {
		switch a.config.Unwind {
		case "dwarf", "auto":
			p, err := dwarfagent.NewOffCPUProfiler(
				hostPID,
				a.config.SystemWide,
				cpus,
				a.config.Tags,
				labels,
				a.symbolizer,
				a.kernelSymbolizer,
				a.config.KernelStacks,
			)
			if err != nil {
				a.cleanup()
				return fmt.Errorf("create DWARF off-CPU profiler: %w", err)
			}
			a.offcpuProfiler = dwarfOffCPUProfilerAdapter{p}
			if a.config.SystemWide {
				log.Println("Off-CPU profiler enabled (system-wide, DWARF)")
			} else {
				log.Printf("Off-CPU profiler enabled (PID: %s, DWARF)", a.pidLogStr(hostPID))
			}
		default:
			profiler, err := offcpu.NewProfiler(
				hostPID,
				a.config.SystemWide,
				a.config.Tags,
				labels,
				a.symbolizer,
				a.kernelSymbolizer,
				a.config.KernelStacks,
			)
			if err != nil {
				a.cleanup()
				return fmt.Errorf("create off-CPU profiler: %w", err)
			}
			a.offcpuProfiler = profiler
			if a.config.SystemWide {
				log.Println("Off-CPU profiler enabled (system-wide)")
			} else {
				log.Printf("Off-CPU profiler enabled (PID: %s)", a.pidLogStr(hostPID))
			}
		}
	}

	// Start PMU monitor if enabled
	if a.config.EnablePMU {
		monitor, err := cpu.NewPMUMonitor(
			hostPID,
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
			log.Printf("PMU monitor enabled (PID: %s)", a.pidLogStr(hostPID))
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
			a.warnFlamegraphNeedsPath(a.config.CPUFlamegraphPath, "CPU")
		} else {
			if err := a.cpuProfiler.CollectAndWrite(a.config.CPUProfilePath); err != nil {
				log.Printf("Failed to write CPU profile: %v", err)
				lastErr = err
			} else if err := a.writeFlamegraph(a.config.CPUProfilePath, a.config.CPUFlamegraphPath, "on-CPU"); err != nil {
				log.Printf("Failed to write CPU flame graph: %v", err)
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
			a.warnFlamegraphNeedsPath(a.config.OffCPUFlamegraphPath, "off-CPU")
		} else {
			if err := a.offcpuProfiler.CollectAndWrite(a.config.OffCPUProfilePath); err != nil {
				log.Printf("Failed to write off-CPU profile: %v", err)
				lastErr = err
			} else if err := a.writeFlamegraph(a.config.OffCPUProfilePath, a.config.OffCPUFlamegraphPath, "off-CPU"); err != nil {
				log.Printf("Failed to write off-CPU flame graph: %v", err)
				lastErr = err
			}
		}
	}

	return lastErr
}

// writeFlamegraph renders htmlPath from the pprof file just written to
// profilePath. It is a no-op when no flame graph was requested.
//
// Every honest number the page carries is also echoed to the terminal. A
// profile that folded to nothing still produces a page - one that says so -
// and still logs why, because a silently empty flame graph is exactly the
// failure this feature must not add.
func (a *Agent) writeFlamegraph(profilePath, htmlPath, mode string) error {
	if htmlPath == "" {
		return nil
	}
	res, err := flamegraph.FromProfileFile(profilePath, htmlPath, flamegraph.Options{
		Title: fmt.Sprintf("%s flame graph - %s", mode, filepath.Base(profilePath)),
		Meta: []flamegraph.MetaItem{
			{Label: "mode", Value: mode},
			{Label: "target", Value: a.targetDescription()},
		},
	})
	if err != nil {
		return err
	}
	log.Printf("Flame graph written to %s (%s across %d samples in %d distinct stacks)",
		htmlPath, flamegraph.FormatValue(res.Total, res.Unit), res.Samples, len(res.Stacks))
	for _, w := range res.Warnings {
		log.Printf("Flame graph note: %s", w)
	}
	return nil
}

// warnFlamegraphNeedsPath says out loud why no flame graph appeared. The
// writer-based options hand the profile to an io.Writer the agent cannot
// re-read, so there is nothing to render from; staying quiet about it would
// look identical to the flag having worked.
func (a *Agent) warnFlamegraphNeedsPath(htmlPath, mode string) {
	if htmlPath == "" {
		return
	}
	log.Printf("No %s flame graph written to %s: this run streams the profile to a writer rather than a file, and the renderer reads the written profile back. Use the path-based profile option to get one.", mode, htmlPath)
}

// targetDescription is the header chip naming what was profiled.
func (a *Agent) targetDescription() string {
	if a.config.SystemWide {
		return "system-wide"
	}
	return fmt.Sprintf("pid %d", a.config.PID)
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

// logDebuginfodStats prints the fetch counters when debuginfod symbolization
// was in use.
//
// The counters already existed; nothing printed them, which is why a run that
// spent 82 of its 87 seconds fetching produced six lines of output and no clue
// (issue #109). Fetch counts and bytes are what distinguish "the network was
// slow" from "this build-id is not on any server and we asked 199 times".
func (a *Agent) logDebuginfodStats() {
	d, ok := a.symbolizer.(*debuginfod.Symbolizer)
	if !ok {
		return
	}
	st := d.Stats()
	log.Printf("perf-agent: debuginfod: symbolization took %v over %d call(s) "+
		"[maps %v, symbolize %v; slowest call %v, %d call(s) over 50ms]; "+
		"fetches ok=%d 404=%d err=%d bytes=%dMB; "+
		"cache hits=%d misses=%d evictions=%d; dispatcher calls=%d skipped_local=%d; "+
		"file_mode addrs=%d fetch_fails=%d local_hits=%d parse_fails=%d",
		time.Duration(st.TotalNs).Round(time.Millisecond), st.Calls,
		time.Duration(st.MappingsNs).Round(time.Millisecond),
		time.Duration(st.SymbolizeNs).Round(time.Millisecond),
		time.Duration(st.SlowestNs).Round(time.Millisecond), st.CallsOver50ms,
		st.FetchSuccessDebuginfo+st.FetchSuccessExecutable,
		st.Fetch404s, st.FetchErrors, st.FetchBytesTotal/(1<<20),
		st.CacheHits, st.CacheMisses, st.CacheEvictions,
		st.DispatcherCalls, st.DispatcherSkippedLocal,
		st.FileModeAddrs, st.FileModeFetchFails, st.FileModeLocalHits, st.FileModeParseFails)
}

// cleanup releases profiler resources.
func (a *Agent) cleanup() {
	a.logDebuginfodStats()
	if a.cpuProfiler != nil {
		a.cpuProfiler.Close()
		a.cpuProfiler = nil
	}
	// Close perf.data after the CPU profiler so any in-flight ringbuf
	// samples (dwarfagent fan-out) land before we patch the file header.
	if a.perfDataWriter != nil {
		if err := a.perfDataWriter.Close(); err != nil {
			log.Printf("perf-agent: close perf.data: %v", err)
		}
		a.perfDataWriter = nil
	}
	if a.offcpuProfiler != nil {
		a.offcpuProfiler.Close()
		a.offcpuProfiler = nil
	}
	if a.pmuMonitor != nil {
		a.pmuMonitor.Close()
		a.pmuMonitor = nil
	}
	// Close the symbolizer last — profilers above may have been using it
	// up until their own Close() calls completed.
	if a.symbolizer != nil {
		_ = a.symbolizer.Close()
		a.symbolizer = nil
	}
	if a.kernelSymbolizer != nil {
		// Surface kernel-symbolizer counters so operators see
		// fallback engagement and frame drops at end-of-run
		// without having to scrape a /metrics endpoint.
		if lks, ok := a.kernelSymbolizer.(*symbolize.LocalKernelSymbolizer); ok {
			log.Printf("%s", lks.Stats())
		}
		_ = a.kernelSymbolizer.Close()
		a.kernelSymbolizer = nil
	}
	// Stop the metrics endpoint after the symbolizer is gone — a
	// late scrape during shutdown would otherwise see post-Close
	// counter state, which is harmless but adds confusion.
	stopMetricsListener(a.metricsSrv)
	a.metricsSrv = nil
}

// readOSRelease reads the running kernel release from
// /proc/sys/kernel/osrelease (a single-line file). Used to populate the
// HEADER_OSRELEASE feature section in the perf.data header. Returns
// "unknown" if the file can't be read.
func readOSRelease() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

// Config returns a copy of the agent's configuration.
func (a *Agent) Config() Config {
	return *a.config
}

// emitCommForPID reads /proc/<pid>/comm and writes a PERF_RECORD_COMM.
// Best-effort: returns silently if /proc/<pid>/comm is unreadable
// (process exited, restricted) — without COMM `perf script` shows
// the bare numeric pid in place of the process name, but the
// kernel-side samples are still attributed by pid.
//
// Emitted for every PID we walk, including kernel threads:
// kthreads (kvm-pit, vhost-*, kworker/*) have empty cmdline but a
// valid comm; without this record, kernel-stacks samples drawn from
// them appear with no name in `perf script` output.
func emitCommForPID(w *perfdata.Writer, pid int) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return
	}
	comm := strings.TrimSpace(string(body))
	if comm == "" {
		return
	}
	w.AddComm(perfdata.CommRecord{
		Pid:  uint32(pid),
		Tid:  uint32(pid),
		Comm: comm,
	})
}

// emitUserspaceMmapsForPID writes PERF_RECORD_MMAP2 entries to w for
// every executable mapping in /proc/<pid>/maps, sourced via the given
// procmap.Resolver (which also attaches build-ids from
// .note.gnu.build-id sections). Best-effort: errors are logged and
// the perf.data writer continues; symbol resolution will be partial
// for any pid we couldn't enumerate.
//
// Does NOT emit COMM — callers that want both should call
// emitCommForPID first (perf convention: COMM before MMAP2 for the
// same pid).
func emitUserspaceMmapsForPID(w *perfdata.Writer, r *procmap.Resolver, pid int) {
	mappings, err := r.Mappings(uint32(pid))
	if err != nil {
		log.Printf("perfdata: enumerate userspace mappings for pid %d: %v (continuing; perf.data userspace symbols may be [unknown])", pid, err)
		return
	}
	user := make([]perfdata.UserspaceMapping, 0, len(mappings))
	for _, m := range mappings {
		if !m.IsExec {
			continue
		}
		var bid []byte
		if m.BuildID != "" {
			if b, err := hex.DecodeString(m.BuildID); err == nil {
				bid = b
			}
		}
		user = append(user, perfdata.UserspaceMapping{
			Start:   m.Start,
			Len:     m.Limit - m.Start,
			Pgoff:   m.Offset,
			Path:    m.Path,
			BuildID: bid,
		})
	}
	w.AddUserspaceMmaps(pid, user)
}
