package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"perf-agent/cpu"
	"perf-agent/offcpu"
	"perf-agent/profile"
)

var (
	flagProfile  = flag.Bool("profile", false, "Enable CPU profiling with stack traces")
	flagOffCpu   = flag.Bool("offcpu", false, "Enable off-CPU profiling with stack traces")
	flagPMU      = flag.Bool("pmu", false, "Enable PMU hardware counters (cycles, instructions, cache misses)")
	flagPID      = flag.Int("pid", 0, "Target process ID to monitor")
	flagAll      = flag.Bool("a", false, "System-wide profiling (all processes)")
	flagPerPID   = flag.Bool("per-pid", false, "Show per-PID breakdown (only with -a --pmu)")
	flagDuration = flag.Duration("duration", 10*time.Second, "Collection duration")
	flagTags     tagFlags
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

	// Validate flags
	if !*flagProfile && !*flagPMU && !*flagOffCpu {
		log.Fatal("At least one of --profile, --offcpu, or --pmu must be specified")
	}

	// Validate PID / system-wide flags
	if *flagPID != 0 && *flagAll {
		log.Fatal("--pid and -a/--all are mutually exclusive")
	}
	if *flagPID == 0 && !*flagAll {
		log.Fatal("Either --pid or -a/--all is required")
	}

	// Validate --per-pid flag
	if *flagPerPID {
		if !*flagAll {
			log.Fatal("--per-pid requires -a/--all")
		}
		if !*flagPMU {
			log.Fatal("--per-pid is only valid with --pmu")
		}
	}

	pid := *flagPID
	systemWide := *flagAll

	// Common setup: Set capabilities
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN, cap.PERFMON); err != nil {
		log.Fatalf("Failed to apply capabilities: %v", err)
	}

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("Failed to remove memlock: %v", err)
	}

	// Get online CPUs
	onlineCPUIDs, err := cpuonline.Get()
	if err != nil {
		log.Fatalf("failed to get online CPUs: %v", err)
	}

	var cpuProfiler *profile.Profiler
	var offcpuProfiler *offcpu.Profiler
	var pmuMonitor *cpu.PMUMonitor

	// Setup profiler if enabled
	if *flagProfile {
		cpuProfiler, err = profile.NewProfiler(pid, systemWide, onlineCPUIDs, flagTags)
		if err != nil {
			log.Fatalf("Failed to setup profiler: %v", err)
		}
		defer cpuProfiler.Close()
		if systemWide {
			log.Println("Profiler enabled (system-wide)")
		} else {
			log.Printf("Profiler enabled (PID: %d)", pid)
		}
	}

	// Setup off-CPU profiler if enabled
	if *flagOffCpu {
		offcpuProfiler, err = offcpu.NewProfiler(pid, systemWide, flagTags)
		if err != nil {
			log.Fatalf("Failed to setup off-CPU profiler: %v", err)
		}
		defer offcpuProfiler.Close()
		if systemWide {
			log.Println("Off-CPU profiler enabled (system-wide)")
		} else {
			log.Printf("Off-CPU profiler enabled (PID: %d)", pid)
		}
	}

	// Setup PMU monitor if enabled
	if *flagPMU {
		pmuMonitor, err = cpu.NewPMUMonitor(pid, systemWide, onlineCPUIDs)
		if err != nil {
			log.Fatalf("Failed to setup PMU monitor: %v", err)
		}
		defer pmuMonitor.Close()
		if systemWide {
			log.Println("PMU monitor enabled (system-wide)")
		} else {
			log.Printf("PMU monitor enabled (PID: %d)", pid)
		}
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

	// Collect and print results
	if *flagPMU && pmuMonitor != nil {
		pmuMonitor.PrintMetrics(systemWide, *flagPerPID)
	}

	if *flagProfile && cpuProfiler != nil {
		if err := cpuProfiler.CollectAndWrite("profile.pb.gz"); err != nil {
			log.Printf("Failed to write profile: %v", err)
		}
	}

	if *flagOffCpu && offcpuProfiler != nil {
		if err := offcpuProfiler.CollectAndWrite("offcpu.pb.gz"); err != nil {
			log.Printf("Failed to write off-CPU profile: %v", err)
		}
	}

	fmt.Println("Exiting program.")
}
