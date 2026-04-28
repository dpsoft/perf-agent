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
	flagGPUReplayInput     = flag.String("gpu-replay-input", "", "Experimental: replay normalized GPU events from a JSON fixture")
	flagGPUStreamStdin     = flag.Bool("gpu-stream-stdin", false, "Experimental: read normalized GPU NDJSON events from stdin")
	flagGPULinuxDRM        = flag.Bool("gpu-linux-drm", false, "Experimental: collect Linux DRM GPU lifecycle telemetry for the target PID")
	flagGPURawOutput       = flag.String("gpu-raw-output", "", "Experimental: write normalized GPU snapshot JSON to this path")
	flagGPUProfileOutput   = flag.String("gpu-profile-output", "", "Experimental: write synthetic-frame GPU pprof output to this path")
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

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for duration or signal
	fmt.Printf("Collecting data for %v (Ctrl+C to stop early)...\n", *flagDuration)

	select {
	case <-time.After(*flagDuration):
		fmt.Println("Collection duration completed")
	case sig := <-sigChan:
		fmt.Printf("\nReceived signal: %s\n", sig)
	}

	// Stop and collect results
	if err := agent.Stop(ctx); err != nil {
		log.Printf("Error stopping agent: %v", err)
	}

	if pmuFile != nil {
		_ = pmuFile.Close()
	}

	fmt.Println("Exiting program.")
}

func buildOptions() []perfagent.Option {
	var opts []perfagent.Option
	gpuHostReplayMode := *flagGPUHostReplayInput != ""
	gpuHostHIPMode := *flagGPUHostHIPLibrary != ""
	gpuReplayMode := *flagGPUReplayInput != ""
	gpuStreamMode := *flagGPUStreamStdin
	gpuLinuxDRMMode := *flagGPULinuxDRM

	if gpuReplayMode && gpuStreamMode {
		log.Fatal("--gpu-replay-input and --gpu-stream-stdin are mutually exclusive")
	}
	if gpuHostReplayMode && gpuHostHIPMode {
		log.Fatal("--gpu-host-replay-input and --gpu-host-hip-library are mutually exclusive")
	}
	if gpuLinuxDRMMode && *flagAll {
		log.Fatal("--gpu-linux-drm does not support -a/--all")
	}

	// Target selection
	if *flagAll {
		opts = append(opts, perfagent.WithSystemWide())
	} else if *flagPID != 0 {
		opts = append(opts, perfagent.WithPID(*flagPID))
	} else if !gpuReplayMode && !gpuStreamMode {
		log.Fatal("Either --pid or -a/--all is required")
	}

	// Validate mutually exclusive
	if *flagPID != 0 && *flagAll {
		log.Fatal("--pid and -a/--all are mutually exclusive")
	}

	// Profiling modes
	if !*flagProfile && !*flagPMU && !*flagOffCpu && !gpuReplayMode && !gpuStreamMode && !gpuLinuxDRMMode {
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
		if *flagGPUProfileOutput != "" {
			opts = append(opts, perfagent.WithGPUProfileOutputPath(*flagGPUProfileOutput))
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
		if *flagGPUProfileOutput != "" {
			opts = append(opts, perfagent.WithGPUProfileOutputPath(*flagGPUProfileOutput))
		}
	}
	if gpuLinuxDRMMode {
		opts = append(opts, perfagent.WithGPULinuxDRM())
		if *flagGPURawOutput != "" {
			opts = append(opts, perfagent.WithGPURawOutputPath(*flagGPURawOutput))
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
