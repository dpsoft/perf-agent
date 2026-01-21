package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"perf-agent/cpu"
	"perf-agent/offcpu"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"perf-agent/pprof"
	"perf-agent/profile"

	blazesym "github.com/libbpf/blazesym/go"

	p "github.com/google/pprof/profile"
)

var (
	flagProfile  = flag.Bool("profile", false, "Enable CPU profiling with stack traces")
	flagOffCpu   = flag.Bool("offcpu", false, "Enable off-CPU profiling with stack traces")
	flagPMU      = flag.Bool("pmu", false, "Enable PMU hardware counters (cycles, instructions, cache misses)")
	flagPID      = flag.Int("pid", 0, "Target process ID to monitor")
	flagAll      = flag.Bool("a", false, "System-wide profiling (all processes)")
	flagPerPID   = flag.Bool("per-pid", false, "Show per-PID breakdown (only with -a --pmu)")
	flagDuration = flag.Duration("duration", 10*time.Second, "Collection duration")
)

func init() {
	// Register long form for -a flag
	flag.BoolVar(flagAll, "all", false, "System-wide profiling (all processes)")
}

type stackBuilder struct {
	stack []string
}

// Commented out unused function
// func (s *stackBuilder) reset() {
// 	s.stack = s.stack[:0]
// }

func (s *stackBuilder) append(sym string) {
	s.stack = append(s.stack, sym)
}

// Profiler state for cleanup and profile collection
type profilerState struct {
	objs       *profile.PerfObjects
	symbolizer *blazesym.Symbolizer
	perfEvents []*perfEvent
}

// Off-CPU profiler state for cleanup and profile collection
type offcpuState struct {
	objs       *offcpu.OffcpuObjects
	symbolizer *blazesym.Symbolizer
	link       link.Link
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
	err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN, cap.PERFMON)
	if err != nil {
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

	var profilerCleanup func()
	var profiler *profilerState

	var offcpuCleanup func()
	var offcpuProfiler *offcpuState

	var pmuCleanup func()
	var collector *cpu.CPUUsageCollector

	// Setup profiler if enabled
	if *flagProfile {
		profiler, profilerCleanup, err = setupProfiler(pid, systemWide, onlineCPUIDs)
		if err != nil {
			log.Fatalf("Failed to setup profiler: %v", err)
		}
		defer profilerCleanup()
		if systemWide {
			log.Println("Profiler enabled (system-wide)")
		} else {
			log.Printf("Profiler enabled (PID: %d)", pid)
		}
	}

	// Setup off-CPU profiler if enabled
	if *flagOffCpu {
		offcpuProfiler, offcpuCleanup, err = setupOffcpuProfiler(pid, systemWide)
		if err != nil {
			log.Fatalf("Failed to setup off-CPU profiler: %v", err)
		}
		defer offcpuCleanup()
		if systemWide {
			log.Println("Off-CPU profiler enabled (system-wide)")
		} else {
			log.Printf("Off-CPU profiler enabled (PID: %d)", pid)
		}
	}

	// Setup PMU monitor if enabled
	if *flagPMU {
		collector, pmuCleanup, err = setupPMUMonitor(pid, systemWide, onlineCPUIDs)
		if err != nil {
			log.Fatalf("Failed to setup PMU monitor: %v", err)
		}
		defer pmuCleanup()
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
	if *flagPMU && collector != nil {
		collector.PrintMetrics(systemWide, *flagPerPID)
	}

	if *flagProfile && profiler != nil {
		collectAndWriteProfile(profiler, systemWide)
	}

	if *flagOffCpu && offcpuProfiler != nil {
		collectAndWriteOffcpuProfile(offcpuProfiler, systemWide)
	}

	fmt.Println("Exiting program.")
}

func setupProfiler(pid int, systemWide bool, cpus []uint) (*profilerState, func(), error) {
	spec, err := profile.LoadPerf()
	if err != nil {
		return nil, nil, fmt.Errorf("load profile spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.RewriteConstants(map[string]interface{}{
		"system_wide": systemWide,
	}); err != nil {
		return nil, nil, fmt.Errorf("rewrite constants: %w", err)
	}

	objs := &profile.PerfObjects{}
	if err := spec.LoadAndAssign(objs, nil); err != nil {
		return nil, nil, fmt.Errorf("load profile objects: %w", err)
	}

	// Only configure PID filter for targeted mode
	if !systemWide {
		config := profile.PerfPidConfig{
			Type:          0,
			CollectUser:   1,
			CollectKernel: 0,
		}

		if err := objs.Pids.Update(uint32(pid), &config, ebpf.UpdateAny); err != nil {
			_ = objs.Close()
			return nil, nil, fmt.Errorf("update pid map: %w", err)
		}
	}

	var perfEvents []*perfEvent
	for _, id := range cpus {
		pe, err := newPerfEvent(int(id), 10000)
		if err != nil {
			// Cleanup already created perf events
			for _, pe := range perfEvents {
				_ = pe.Close()
			}
			_ = objs.Close()
			return nil, nil, fmt.Errorf("create perf event on CPU %d: %w", id, err)
		}

		if err := pe.attachPerfEvent(objs.Profile); err != nil {
			_ = pe.Close()
			for _, pe := range perfEvents {
				_ = pe.Close()
			}
			_ = objs.Close()
			return nil, nil, fmt.Errorf("attach eBPF to perf event on CPU %d: %w", id, err)
		}

		perfEvents = append(perfEvents, pe)
	}

	symbolizer, err := blazesym.NewSymbolizer(blazesym.SymbolizerWithCodeInfo(true))
	if err != nil {
		for _, pe := range perfEvents {
			_ = pe.Close()
		}
		_ = objs.Close()
		return nil, nil, fmt.Errorf("create symbolizer: %w", err)
	}

	state := &profilerState{
		objs:       objs,
		symbolizer: symbolizer,
		perfEvents: perfEvents,
	}

	cleanup := func() {
		symbolizer.Close()
		for _, pe := range perfEvents {
			_ = pe.Close()
		}
		_ = objs.Close()
	}

	return state, cleanup, nil
}

func setupPMUMonitor(pid int, systemWide bool, cpus []uint) (*cpu.CPUUsageCollector, func(), error) {
	spec, err := cpu.LoadCPU()
	if err != nil {
		return nil, nil, fmt.Errorf("load CPU spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.RewriteConstants(map[string]interface{}{
		"system_wide": systemWide,
	}); err != nil {
		return nil, nil, fmt.Errorf("rewrite constants: %w", err)
	}

	cpuObjs := &cpu.CPUObjects{}
	if err := spec.LoadAndAssign(cpuObjs, nil); err != nil {
		return nil, nil, fmt.Errorf("load CPU objects: %w", err)
	}

	// Initialize hardware perf counters
	cpuList := make([]int, len(cpus))
	for i, id := range cpus {
		cpuList[i] = int(id)
	}

	var hwPerf *cpu.HardwarePerfEvents
	hwPerf, err = cpu.NewHardwarePerfEvents(cpuList)
	if err != nil {
		log.Printf("Hardware perf counters unavailable (running in VM?): %v", err)
	} else {
		err = hwPerf.AttachToMaps(
			cpuObjs.CpuCyclesReader,
			cpuObjs.CpuInstructionsReader,
			cpuObjs.CacheMissesReader,
		)
		if err != nil {
			log.Printf("Failed to attach HW counters to maps: %v", err)
			_ = hwPerf.Close()
			hwPerf = nil
		} else {
			if err := hwPerf.EnableInBPF(cpuObjs.HwCountersEnabled); err != nil {
				log.Printf("Failed to enable HW counters in eBPF: %v", err)
			} else {
				log.Println("Hardware perf counters enabled (cycles, instructions, cache misses)")
			}
		}
	}

	tp, err := link.AttachTracing(link.TracingOptions{
		Program: cpuObjs.HandleSwitch,
	})
	if err != nil {
		if hwPerf != nil {
			_ = hwPerf.Close()
		}
		_ = cpuObjs.Close()
		return nil, nil, fmt.Errorf("attach tp_btf sched_switch: %w", err)
	}

	collector, err := cpu.NewCPUUsageCollector(cpuObjs)
	if err != nil {
		_ = tp.Close()
		if hwPerf != nil {
			_ = hwPerf.Close()
		}
		_ = cpuObjs.Close()
		return nil, nil, fmt.Errorf("create CPU usage collector: %w", err)
	}

	// Configure PID filter only for targeted mode
	if !systemWide {
		trackValue := uint8(1)
		if err := cpuObjs.PidFilter.Update(uint32(pid), &trackValue, ebpf.UpdateAny); err != nil {
			_ = tp.Close()
			if hwPerf != nil {
				_ = hwPerf.Close()
			}
			_ = cpuObjs.Close()
			return nil, nil, fmt.Errorf("configure PID filter: %w", err)
		}
	}

	// Start polling goroutine
	ticker := time.NewTicker(1 * time.Second)
	stopPolling := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				if err := collector.ReadCPUUsage(); err != nil {
					log.Printf("Error reading CPU usage: %v", err)
				}
			case <-stopPolling:
				return
			}
		}
	}()

	cleanup := func() {
		close(stopPolling)
		ticker.Stop()
		_ = tp.Close()
		if hwPerf != nil {
			_ = hwPerf.Close()
		}
		_ = cpuObjs.Close()
	}

	return collector, cleanup, nil
}

func collectAndWriteProfile(profiler *profilerState, systemWide bool) {
	m := profiler.objs.Counts
	mapSize := m.MaxEntries()

	keys := make([]profile.PerfSampleKey, mapSize)
	values := make([]uint64, mapSize)

	opts := &ebpf.BatchOptions{}
	cursor := new(ebpf.MapBatchCursor)

	n, err := m.BatchLookupAndDelete(cursor, keys, values, opts)
	if n > 0 {
		log.Printf("BatchLookupAndDelete: %d samples", n)
	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		// Expected when map is empty or all entries processed
	} else if err != nil {
		log.Printf("BatchLookupAndDelete error: %v", err)
	}

	if n == 0 {
		log.Println("No profile samples collected")
		return
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    int64(97),
		PerPIDProfile: false,
	})

	for i := 0; i < n; i++ {
		key := keys[i]
		value := values[i]

		// Use PID from sample key for symbolization
		samplePid := key.Pid

		stack, err := profiler.objs.Stackmap.LookupBytes(uint32(key.UserStack))
		if err != nil {
			log.Printf("Failed to lookup user stack: %v", err)
			continue
		}

		if len(stack) == 0 {
			continue
		}

		sb := new(stackBuilder)
		begin := len(sb.stack)

		for j := 0; j < 127; j++ {
			instructionPointerBytes := stack[j*8 : j*8+8]
			instructionPointer := binary.LittleEndian.Uint64(instructionPointerBytes)
			if instructionPointer == 0 {
				break
			}

			symbol, err := profiler.symbolizer.SymbolizeProcessAbsAddrs(
				[]uint64{instructionPointer},
				samplePid,
				blazesym.ProcessSourceWithPerfMap(true),
				blazesym.ProcessSourceWithDebugSyms(true),
			)
			if err != nil {
				log.Printf("Failed to symbolize: %v", err)
				break
			}

			sb.append(symbol[0].Name)
		}

		end := len(sb.stack)
		Reverse(sb.stack[begin:end])

		sample := createSample(sb, value, int(samplePid))
		builders.AddSample(&sample)
	}

	// Get builder and write profile
	var buf bytes.Buffer
	for _, builder := range builders.Builders {
		_, err = builder.Write(&buf)
		if err != nil {
			log.Printf("Failed to write profile: %v", err)
			return
		}
		break // Only need first builder for non-per-PID profile
	}

	if buf.Len() == 0 {
		log.Println("No profile data to write")
		return
	}

	parsed, err := p.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Printf("Failed to parse profile: %v", err)
		return
	}

	file, err := os.Create("profile.pb.gz")
	if err != nil {
		log.Printf("Failed to create profile file: %v", err)
		return
	}
	defer func() { _ = file.Close() }()

	if err := parsed.Write(file); err != nil {
		log.Printf("Failed to write profile to file: %v", err)
		return
	}

	log.Println("Profile written to profile.pb.gz")
}

func createSample(sb *stackBuilder, value uint64, pid int) pprof.ProfileSample {
	return pprof.ProfileSample{
		Pid:         uint32(pid),
		Aggregation: pprof.SampleAggregated,
		SampleType:  pprof.SampleTypeCpu,
		Stack:       sb.stack,
		Value:       value,
	}
}

func Reverse[S ~[]E, E any](s S) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func setupOffcpuProfiler(pid int, systemWide bool) (*offcpuState, func(), error) {
	spec, err := offcpu.LoadOffcpu()
	if err != nil {
		return nil, nil, fmt.Errorf("load offcpu spec: %w", err)
	}

	// Set system_wide variable in eBPF program
	if err := spec.RewriteConstants(map[string]interface{}{
		"system_wide": systemWide,
	}); err != nil {
		return nil, nil, fmt.Errorf("rewrite constants: %w", err)
	}

	objs := &offcpu.OffcpuObjects{}
	if err := spec.LoadAndAssign(objs, nil); err != nil {
		return nil, nil, fmt.Errorf("load offcpu objects: %w", err)
	}

	// Configure PID filter only for targeted mode
	if !systemWide {
		trackValue := uint8(1)
		if err := objs.PidFilter.Update(uint32(pid), &trackValue, ebpf.UpdateAny); err != nil {
			_ = objs.Close()
			return nil, nil, fmt.Errorf("configure PID filter: %w", err)
		}
	}

	// Attach to sched_switch tracepoint
	tp, err := link.AttachTracing(link.TracingOptions{
		Program: objs.OffcpuSchedSwitch,
	})
	if err != nil {
		_ = objs.Close()
		return nil, nil, fmt.Errorf("attach tp_btf sched_switch: %w", err)
	}

	symbolizer, err := blazesym.NewSymbolizer(blazesym.SymbolizerWithCodeInfo(true))
	if err != nil {
		_ = tp.Close()
		_ = objs.Close()
		return nil, nil, fmt.Errorf("create symbolizer: %w", err)
	}

	state := &offcpuState{
		objs:       objs,
		symbolizer: symbolizer,
		link:       tp,
	}

	cleanup := func() {
		symbolizer.Close()
		_ = tp.Close()
		_ = objs.Close()
	}

	return state, cleanup, nil
}

func collectAndWriteOffcpuProfile(profiler *offcpuState, systemWide bool) {
	m := profiler.objs.OffcpuCounts
	mapSize := m.MaxEntries()

	keys := make([]offcpu.OffcpuOffcpuKey, mapSize)
	values := make([]uint64, mapSize)

	opts := &ebpf.BatchOptions{}
	cursor := new(ebpf.MapBatchCursor)

	n, err := m.BatchLookupAndDelete(cursor, keys, values, opts)
	if n > 0 {
		log.Printf("Off-CPU BatchLookupAndDelete: %d samples", n)
	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		// Expected when map is empty or all entries processed
	} else if err != nil {
		log.Printf("Off-CPU BatchLookupAndDelete error: %v", err)
	}

	if n == 0 {
		log.Println("No off-CPU samples collected")
		return
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    1, // Not used for off-CPU, but needed for builder
		PerPIDProfile: false,
	})

	for i := 0; i < n; i++ {
		key := keys[i]
		value := values[i]

		// Use PID from sample key for symbolization
		samplePid := key.Pid

		stack, err := profiler.objs.Stackmap.LookupBytes(uint32(key.UserStack))
		if err != nil {
			log.Printf("Failed to lookup user stack: %v", err)
			continue
		}

		if len(stack) == 0 {
			continue
		}

		sb := new(stackBuilder)
		begin := len(sb.stack)

		for j := 0; j < 127; j++ {
			instructionPointerBytes := stack[j*8 : j*8+8]
			instructionPointer := binary.LittleEndian.Uint64(instructionPointerBytes)
			if instructionPointer == 0 {
				break
			}

			symbol, err := profiler.symbolizer.SymbolizeProcessAbsAddrs(
				[]uint64{instructionPointer},
				samplePid,
				blazesym.ProcessSourceWithPerfMap(true),
				blazesym.ProcessSourceWithDebugSyms(true),
			)
			if err != nil {
				log.Printf("Failed to symbolize: %v", err)
				break
			}

			sb.append(symbol[0].Name)
		}

		end := len(sb.stack)
		Reverse(sb.stack[begin:end])

		sample := pprof.ProfileSample{
			Pid:         samplePid,
			Aggregation: pprof.SampleAggregated,
			SampleType:  pprof.SampleTypeOffCpu,
			Stack:       sb.stack,
			Value:       value,
		}
		builders.AddSample(&sample)
	}

	// Get builder and write profile
	var buf bytes.Buffer
	for _, builder := range builders.Builders {
		_, err = builder.Write(&buf)
		if err != nil {
			log.Printf("Failed to write off-CPU profile: %v", err)
			return
		}
		break // Only need first builder for non-per-PID profile
	}

	if buf.Len() == 0 {
		log.Println("No off-CPU profile data to write")
		return
	}

	parsed, err := p.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Printf("Failed to parse off-CPU profile: %v", err)
		return
	}

	file, err := os.Create("offcpu.pb.gz")
	if err != nil {
		log.Printf("Failed to create off-CPU profile file: %v", err)
		return
	}
	defer func() { _ = file.Close() }()

	if err := parsed.Write(file); err != nil {
		log.Printf("Failed to write off-CPU profile to file: %v", err)
		return
	}

	log.Println("Off-CPU profile written to offcpu.pb.gz")
}
