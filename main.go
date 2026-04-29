package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/metrics"
	"github.com/dpsoft/perf-agent/perfagent"
)

var (
	flagProfile            = flag.Bool("profile", false, "Enable CPU profiling with stack traces")
	flagOffCpu             = flag.Bool("offcpu", false, "Enable off-CPU profiling with stack traces")
	flagPMU                = flag.Bool("pmu", false, "Enable PMU hardware counters (cycles, instructions, cache misses)")
	flagPID                = flag.Int("pid", 0, "Target process ID to monitor")
	flagAll                = flag.Bool("a", false, "System-wide profiling (all processes)")
	flagPerPID             = flag.Bool("per-pid", false, "Show per-PID breakdown (only with -a --pmu)")
	flagDuration           = flag.Duration("duration", 10*time.Second, "Collection duration")
	flagSampleRate         = flag.Int("sample-rate", 99, "CPU profiling sample rate in Hz")
	flagProfileOutput      = flag.String("profile-output", "", "Output path for CPU profile (default: auto-generated)")
	flagOffcpuOutput       = flag.String("offcpu-output", "", "Output path for off-CPU profile (default: auto-generated)")
	flagPMUOutput          = flag.String("pmu-output", "", "Output path for PMU metrics (default: stdout)")
	flagUnwind             = flag.String("unwind", "auto", "Stack unwinding strategy: fp | dwarf | auto (auto → dwarf)")
	flagGPUHostReplayInput = flag.String("gpu-host-replay-input", "", "Experimental: replay host launch attribution from a JSON fixture")
	flagGPUHostHIPLibrary  = flag.String("gpu-host-hip-library", "", "Experimental: attach HIP host launch attribution to this shared library path")
	flagGPUHostHIPSymbol   = flag.String("gpu-host-hip-symbol", "hipLaunchKernel", "Experimental: HIP launch symbol name for --gpu-host-hip-library")
	flagGPUHIPLinuxDRMJoin = flag.Duration("gpu-hip-linuxdrm-join-window", 5*time.Millisecond, "Experimental: fallback join window for HIP host launches to linuxdrm lifecycle events")
	flagGPUReplayInput     = flag.String("gpu-replay-input", "", "Experimental: replay normalized GPU events from a JSON fixture")
	flagGPUStreamStdin     = flag.Bool("gpu-stream-stdin", false, "Experimental: read normalized GPU NDJSON events from stdin")
	flagGPUAMDSampleStdin  = flag.Bool("gpu-amd-sample-stdin", false, "Experimental: read AMD execution/sample NDJSON events from stdin")
	flagGPULinuxDRM        = flag.Bool("gpu-linux-drm", false, "Experimental: collect Linux DRM GPU lifecycle telemetry for the target PID")
	flagGPULinuxKFD        = flag.Bool("gpu-linux-kfd", false, "Experimental: collect Linux KFD GPU compute lifecycle telemetry for the target PID")
	flagGPURawOutput       = flag.String("gpu-raw-output", "", "Experimental: write normalized GPU snapshot JSON to this path")
	flagGPUAttributionOut  = flag.String("gpu-attribution-output", "", "Experimental: write workload attribution rollups as JSON to this path")
	flagGPUProfileOutput   = flag.String("gpu-profile-output", "", "Experimental: write synthetic-frame GPU pprof output to this path")
	flagGPUFoldedOutput    = flag.String("gpu-folded-output", "", "Experimental: write folded GPU flamegraph input to this path")
	flagTags               tagFlags
)

// tagFlags is a custom flag type for collecting multiple --tag key=value arguments
type tagFlags []string

func (t *tagFlags) String() string {
	return strings.Join(*t, ",")
}

func (t *tagFlags) Set(value string) error {
	if !strings.Contains(value, "=") {
		return fmt.Errorf("tag must be in key=value format")
	}
	*t = append(*t, value)
	return nil
}

func init() {
	// Register long form for -a flag
	flag.BoolVar(flagAll, "all", false, "System-wide profiling (all processes)")
	// Register --tag flag for profile metadata
	flag.Var(&flagTags, "tag", "Add tag to profile (repeatable, format: key=value)")
}

var pmuFile *os.File

func debugGPULivef(format string, args ...any) {
	if os.Getenv("PERF_AGENT_DEBUG_GPU_LIVE") == "" {
		return
	}
	log.Printf("gpu-live-debug: "+format, args...)
}

func gpuEventBackendLine(agent *perfagent.Agent) (string, error) {
	backends, err := agent.GPUEventBackends()
	if err != nil {
		return "", err
	}
	if len(backends) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(backends))
	for _, backend := range backends {
		parts = append(parts, string(backend))
	}
	return fmt.Sprintf("GPU event backends: %s", strings.Join(parts, ", ")), nil
}

func main() {
	flag.Parse()

	// Build options from flags
	opts := buildOptions()

	// Create agent
	agent, err := perfagent.New(opts...)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}
	defer func() {
		if err := agent.Close(); err != nil {
			log.Printf("Error closing agent: %v", err)
		}
	}()

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the agent
	if err := agent.Start(ctx); err != nil {
		log.Fatalf("Failed to start agent: %v", err)
	}
	debugGPULivef("agent started")
	if line, err := gpuEventBackendLine(agent); err != nil {
		log.Printf("Failed to report GPU event backends: %v", err)
	} else if line != "" {
		fmt.Println(line)
	}

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for duration or signal
	fmt.Printf("Collecting data for %v (Ctrl+C to stop early)...\n", *flagDuration)

	select {
	case <-time.After(*flagDuration):
		debugGPULivef("duration completed after %v", *flagDuration)
		fmt.Println("Collection duration completed")
	case sig := <-sigChan:
		debugGPULivef("received signal %s", sig)
		fmt.Printf("\nReceived signal: %s\n", sig)
	}

	// Stop and collect results
	debugGPULivef("calling agent.Stop")
	if err := agent.Stop(ctx); err != nil {
		debugGPULivef("agent.Stop returned error: %v", err)
		log.Printf("Error stopping agent: %v", err)
	} else {
		debugGPULivef("agent.Stop completed successfully")
	}

	if pmuFile != nil {
		_ = pmuFile.Close()
	}

	fmt.Println("Exiting program.")
	debugGPULivef("program exit")
}

func buildOptions() []perfagent.Option {
	var opts []perfagent.Option
	gpuHostReplayMode := *flagGPUHostReplayInput != ""
	gpuHostHIPMode := *flagGPUHostHIPLibrary != ""
	gpuReplayMode := *flagGPUReplayInput != ""
	gpuStreamMode := *flagGPUStreamStdin
	gpuAMDsampleMode := *flagGPUAMDSampleStdin
	gpuLinuxDRMMode := *flagGPULinuxDRM
	gpuLinuxKFDMode := *flagGPULinuxKFD

	gpuSourceCount := 0
	for _, enabled := range []bool{gpuReplayMode, gpuStreamMode, gpuAMDsampleMode, gpuLinuxDRMMode, gpuLinuxKFDMode} {
		if enabled {
			gpuSourceCount++
		}
	}
	if gpuSourceCount > 1 {
		log.Fatal("--gpu-replay-input, --gpu-stream-stdin, --gpu-amd-sample-stdin, --gpu-linux-drm, and --gpu-linux-kfd are mutually exclusive")
	}
	if gpuLinuxDRMMode && gpuLinuxKFDMode {
		log.Fatal("--gpu-linux-drm and --gpu-linux-kfd are mutually exclusive")
	}
	if gpuHostReplayMode && gpuHostHIPMode {
		log.Fatal("--gpu-host-replay-input and --gpu-host-hip-library are mutually exclusive")
	}
	if gpuLinuxDRMMode && *flagAll {
		log.Fatal("--gpu-linux-drm does not support -a/--all")
	}
	if gpuLinuxKFDMode && *flagAll {
		log.Fatal("--gpu-linux-kfd does not support -a/--all")
	}

	// Target selection
	if *flagAll {
		opts = append(opts, perfagent.WithSystemWide())
	} else if *flagPID != 0 {
		opts = append(opts, perfagent.WithPID(*flagPID))
	} else if !gpuReplayMode && !gpuStreamMode && !gpuAMDsampleMode {
		log.Fatal("Either --pid or -a/--all is required")
	}

	// Validate mutually exclusive
	if *flagPID != 0 && *flagAll {
		log.Fatal("--pid and -a/--all are mutually exclusive")
	}

	// Profiling modes
	if !*flagProfile && !*flagPMU && !*flagOffCpu && !gpuReplayMode && !gpuStreamMode && !gpuAMDsampleMode && !gpuLinuxDRMMode && !gpuLinuxKFDMode {
		log.Fatal("At least one of --profile, --offcpu, or --pmu must be specified")
	}

	if *flagProfile {
		outputPath := *flagProfileOutput
		if outputPath == "" {
			outputPath = generateOutputName(*flagPID, *flagAll, "on-cpu", "pb.gz")
		}
		opts = append(opts, perfagent.WithCPUProfile(outputPath))
	}

	if *flagOffCpu {
		outputPath := *flagOffcpuOutput
		if outputPath == "" {
			outputPath = generateOutputName(*flagPID, *flagAll, "off-cpu", "pb.gz")
		}
		opts = append(opts, perfagent.WithOffCPUProfile(outputPath))
	}

	if *flagPMU {
		opts = append(opts, perfagent.WithPMU())
		exporter := metrics.NewConsoleExporter(*flagPerPID)
		if *flagPMUOutput != "" {
			pmuPath := *flagPMUOutput
			if pmuPath == "auto" {
				pmuPath = generateOutputName(*flagPID, *flagAll, "pmu", "txt")
			}
			f, err := os.Create(pmuPath)
			if err != nil {
				log.Fatalf("Failed to create PMU output file: %v", err)
			}
			pmuFile = f
			exporter.Writer = f
			fmt.Printf("PMU metrics will be written to %s\n", pmuPath)
		}
		opts = append(opts, perfagent.WithMetricsExporter(exporter))
	}

	if gpuReplayMode {
		opts = append(opts, perfagent.WithGPUReplayInput(*flagGPUReplayInput))
		if *flagGPURawOutput != "" {
			opts = append(opts, perfagent.WithGPURawOutputPath(*flagGPURawOutput))
		}
		if *flagGPUAttributionOut != "" {
			opts = append(opts, perfagent.WithGPUAttributionOutputPath(*flagGPUAttributionOut))
		}
		if *flagGPUProfileOutput != "" {
			opts = append(opts, perfagent.WithGPUProfileOutputPath(*flagGPUProfileOutput))
		}
		if *flagGPUFoldedOutput != "" {
			opts = append(opts, perfagent.WithGPUFoldedOutputPath(*flagGPUFoldedOutput))
		}
	}
	if gpuHostReplayMode {
		opts = append(opts, perfagent.WithGPUHostReplayInput(*flagGPUHostReplayInput))
	}
	if gpuHostHIPMode {
		opts = append(opts, perfagent.WithGPUHostHIP(*flagGPUHostHIPLibrary, *flagGPUHostHIPSymbol))
	}
	if gpuStreamMode {
		opts = append(opts, perfagent.WithGPUStreamInput(os.Stdin))
		if *flagGPURawOutput != "" {
			opts = append(opts, perfagent.WithGPURawOutputPath(*flagGPURawOutput))
		}
		if *flagGPUAttributionOut != "" {
			opts = append(opts, perfagent.WithGPUAttributionOutputPath(*flagGPUAttributionOut))
		}
		if *flagGPUProfileOutput != "" {
			opts = append(opts, perfagent.WithGPUProfileOutputPath(*flagGPUProfileOutput))
		}
		if *flagGPUFoldedOutput != "" {
			opts = append(opts, perfagent.WithGPUFoldedOutputPath(*flagGPUFoldedOutput))
		}
	}
	if gpuAMDsampleMode {
		opts = append(opts, perfagent.WithGPUAMDSampleInput(os.Stdin))
		if *flagGPURawOutput != "" {
			opts = append(opts, perfagent.WithGPURawOutputPath(*flagGPURawOutput))
		}
		if *flagGPUAttributionOut != "" {
			opts = append(opts, perfagent.WithGPUAttributionOutputPath(*flagGPUAttributionOut))
		}
		if *flagGPUProfileOutput != "" {
			opts = append(opts, perfagent.WithGPUProfileOutputPath(*flagGPUProfileOutput))
		}
		if *flagGPUFoldedOutput != "" {
			opts = append(opts, perfagent.WithGPUFoldedOutputPath(*flagGPUFoldedOutput))
		}
	}
	if gpuLinuxDRMMode {
		opts = append(opts, perfagent.WithGPULinuxDRM())
		if gpuHostHIPMode {
			opts = append(opts, perfagent.WithGPUHIPLinuxDRMJoinWindow(*flagGPUHIPLinuxDRMJoin))
		}
		if *flagGPURawOutput != "" {
			opts = append(opts, perfagent.WithGPURawOutputPath(*flagGPURawOutput))
		}
		if *flagGPUAttributionOut != "" {
			opts = append(opts, perfagent.WithGPUAttributionOutputPath(*flagGPUAttributionOut))
		}
		if *flagGPUFoldedOutput != "" {
			opts = append(opts, perfagent.WithGPUFoldedOutputPath(*flagGPUFoldedOutput))
		}
	}
	if gpuLinuxKFDMode {
		opts = append(opts, perfagent.WithGPULinuxKFD())
		if gpuHostHIPMode {
			opts = append(opts, perfagent.WithGPUHIPLinuxDRMJoinWindow(*flagGPUHIPLinuxDRMJoin))
		}
		if *flagGPURawOutput != "" {
			opts = append(opts, perfagent.WithGPURawOutputPath(*flagGPURawOutput))
		}
		if *flagGPUAttributionOut != "" {
			opts = append(opts, perfagent.WithGPUAttributionOutputPath(*flagGPUAttributionOut))
		}
		if *flagGPUFoldedOutput != "" {
			opts = append(opts, perfagent.WithGPUFoldedOutputPath(*flagGPUFoldedOutput))
		}
	}

	// Per-PID validation
	if *flagPerPID {
		if !*flagAll {
			log.Fatal("--per-pid requires -a/--all")
		}
		if !*flagPMU {
			log.Fatal("--per-pid is only valid with --pmu")
		}
		opts = append(opts, perfagent.WithPerPID())
	}

	// Sample rate
	opts = append(opts, perfagent.WithSampleRate(*flagSampleRate))

	// Tags
	if len(flagTags) > 0 {
		opts = append(opts, perfagent.WithTags(flagTags...))
	}

	// Unwinding strategy
	if *flagUnwind != "" {
		opts = append(opts, perfagent.WithUnwind(*flagUnwind))
	}

	return opts
}

func generateOutputName(pid int, systemWide bool, suffix, ext string) string {
	timestamp := time.Now().Format("200601021504")
	if systemWide {
		return fmt.Sprintf("%s-%s.%s", timestamp, suffix, ext)
	}
	procName := readProcessName(pid)
	return fmt.Sprintf("%s-%s-%s.%s", procName, timestamp, suffix, ext)
}

func readProcessName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return fmt.Sprintf("pid%d", pid)
	}
	return strings.TrimSpace(string(data))
}
