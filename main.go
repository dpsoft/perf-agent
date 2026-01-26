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
	flagProfile    = flag.Bool("profile", false, "Enable CPU profiling with stack traces")
	flagOffCpu     = flag.Bool("offcpu", false, "Enable off-CPU profiling with stack traces")
	flagPMU        = flag.Bool("pmu", false, "Enable PMU hardware counters (cycles, instructions, cache misses)")
	flagPID        = flag.Int("pid", 0, "Target process ID to monitor")
	flagAll        = flag.Bool("a", false, "System-wide profiling (all processes)")
	flagPerPID     = flag.Bool("per-pid", false, "Show per-PID breakdown (only with -a --pmu)")
	flagDuration   = flag.Duration("duration", 10*time.Second, "Collection duration")
	flagSampleRate = flag.Int("sample-rate", 99, "CPU profiling sample rate in Hz")
	flagTags       tagFlags
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

	fmt.Println("Exiting program.")
}

func buildOptions() []perfagent.Option {
	var opts []perfagent.Option

	// Target selection
	if *flagAll {
		opts = append(opts, perfagent.WithSystemWide())
	} else if *flagPID != 0 {
		opts = append(opts, perfagent.WithPID(*flagPID))
	} else {
		log.Fatal("Either --pid or -a/--all is required")
	}

	// Validate mutually exclusive
	if *flagPID != 0 && *flagAll {
		log.Fatal("--pid and -a/--all are mutually exclusive")
	}

	// Profiling modes
	if !*flagProfile && !*flagPMU && !*flagOffCpu {
		log.Fatal("At least one of --profile, --offcpu, or --pmu must be specified")
	}

	if *flagProfile {
		opts = append(opts, perfagent.WithCPUProfile("profile.pb.gz"))
	}

	if *flagOffCpu {
		opts = append(opts, perfagent.WithOffCPUProfile("offcpu.pb.gz"))
	}

	if *flagPMU {
		opts = append(opts, perfagent.WithPMU())
		// Use console exporter for backward compatibility
		opts = append(opts, perfagent.WithMetricsExporter(metrics.NewConsoleExporter(*flagPerPID)))
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

	return opts
}
