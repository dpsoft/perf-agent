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
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"perf-agent/blazesym"
	"perf-agent/pprof"
	"perf-agent/profile"

	p "github.com/google/pprof/profile"
)

var (
	flagProfile  = flag.Bool("profile", false, "Enable CPU profiling with stack traces")
	flagPMU      = flag.Bool("pmu", false, "Enable PMU hardware counters (cycles, instructions, cache misses)")
	flagPID      = flag.Int("pid", 0, "Target process ID to monitor (required)")
	flagDuration = flag.Duration("duration", 10*time.Second, "Collection duration")
)

type stackBuilder struct {
	stack []string
}

func (s *stackBuilder) reset() {
	s.stack = s.stack[:0]
}

func (s *stackBuilder) append(sym string) {
	s.stack = append(s.stack, sym)
}

// Profiler state for cleanup and profile collection
type profilerState struct {
	objs       *profile.PerfObjects
	symbolizer *blazesym.Symbolizer
	perfEvents []*perfEvent
}

func main() {
	flag.Parse()

	// Validate flags
	if !*flagProfile && !*flagPMU {
		log.Fatal("At least one of --profile or --pmu must be specified")
	}
	if *flagPID == 0 {
		log.Fatal("--pid is required")
	}

	pid := *flagPID

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

	var pmuCleanup func()
	var collector *cpu.CPUUsageCollector

	// Setup profiler if enabled
	if *flagProfile {
		profiler, profilerCleanup, err = setupProfiler(pid, onlineCPUIDs)
		if err != nil {
			log.Fatalf("Failed to setup profiler: %v", err)
		}
		defer profilerCleanup()
		log.Println("Profiler enabled")
	}

	// Setup PMU monitor if enabled
	if *flagPMU {
		collector, pmuCleanup, err = setupPMUMonitor(pid, onlineCPUIDs)
		if err != nil {
			log.Fatalf("Failed to setup PMU monitor: %v", err)
		}
		defer pmuCleanup()
		log.Println("PMU monitor enabled")
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
		printPMUMetrics(collector)
	}

	if *flagProfile && profiler != nil {
		collectAndWriteProfile(profiler, pid)
	}

	fmt.Println("Exiting program.")
}

func setupProfiler(pid int, cpus []uint) (*profilerState, func(), error) {
	objs := &profile.PerfObjects{}
	if err := profile.LoadPerfObjects(objs, nil); err != nil {
		return nil, nil, fmt.Errorf("load profile objects: %w", err)
	}

	config := profile.PerfPidConfig{
		Type:          0,
		CollectUser:   1,
		CollectKernel: 0,
	}

	if err := objs.Pids.Update(uint32(pid), &config, ebpf.UpdateAny); err != nil {
		objs.Close()
		return nil, nil, fmt.Errorf("update pid map: %w", err)
	}

	var perfEvents []*perfEvent
	for _, id := range cpus {
		pe, err := newPerfEvent(int(id), 10000)
		if err != nil {
			// Cleanup already created perf events
			for _, pe := range perfEvents {
				pe.Close()
			}
			objs.Close()
			return nil, nil, fmt.Errorf("create perf event on CPU %d: %w", id, err)
		}

		if err := pe.attachPerfEvent(objs.Profile); err != nil {
			pe.Close()
			for _, pe := range perfEvents {
				pe.Close()
			}
			objs.Close()
			return nil, nil, fmt.Errorf("attach eBPF to perf event on CPU %d: %w", id, err)
		}

		perfEvents = append(perfEvents, pe)
	}

	symbolizer, err := blazesym.NewSymbolizer()
	if err != nil {
		for _, pe := range perfEvents {
			pe.Close()
		}
		objs.Close()
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
			pe.Close()
		}
		objs.Close()
	}

	return state, cleanup, nil
}

func setupPMUMonitor(pid int, cpus []uint) (*cpu.CPUUsageCollector, func(), error) {
	cpuObjs := &cpu.CPUObjects{}
	if err := cpu.LoadCPUObjects(cpuObjs, nil); err != nil {
		return nil, nil, fmt.Errorf("load CPU objects: %w", err)
	}

	// Initialize hardware perf counters
	cpuList := make([]int, len(cpus))
	for i, id := range cpus {
		cpuList[i] = int(id)
	}

	var hwPerf *cpu.HardwarePerfEvents
	hwPerf, err := cpu.NewHardwarePerfEvents(cpuList)
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
			hwPerf.Close()
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
		Program: cpuObjs.CPUPrograms.HandleSwitch,
	})
	if err != nil {
		if hwPerf != nil {
			hwPerf.Close()
		}
		cpuObjs.Close()
		return nil, nil, fmt.Errorf("attach tp_btf sched_switch: %w", err)
	}

	collector, err := cpu.NewCPUUsageCollector(cpuObjs)
	if err != nil {
		tp.Close()
		if hwPerf != nil {
			hwPerf.Close()
		}
		cpuObjs.Close()
		return nil, nil, fmt.Errorf("create CPU usage collector: %w", err)
	}

	// Configure PID filter
	trackValue := uint8(1)
	if err := cpuObjs.PidFilter.Update(uint32(pid), &trackValue, ebpf.UpdateAny); err != nil {
		tp.Close()
		if hwPerf != nil {
			hwPerf.Close()
		}
		cpuObjs.Close()
		return nil, nil, fmt.Errorf("configure PID filter: %w", err)
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
		tp.Close()
		if hwPerf != nil {
			hwPerf.Close()
		}
		cpuObjs.Close()
	}

	return collector, cleanup, nil
}

func printPMUMetrics(collector *cpu.CPUUsageCollector) {
	for pid, m := range collector.GetAllMetrics() {
		fmt.Printf("\n=== PID %d Metrics ===\n", pid)
		fmt.Printf("Samples: %d\n", m.SampleCount)

		// Time histogram stats
		hist := m.TimeHist
		fmt.Printf("\nScheduling Latency (time on CPU per switch):\n")
		fmt.Printf("  Min:    %.3f ms\n", float64(hist.Min())/1e6)
		fmt.Printf("  Max:    %.3f ms\n", float64(hist.Max())/1e6)
		fmt.Printf("  Mean:   %.3f ms\n", hist.Mean()/1e6)
		fmt.Printf("  P50:    %.3f ms\n", float64(hist.ValueAtQuantile(50.0))/1e6)
		fmt.Printf("  P95:    %.3f ms\n", float64(hist.ValueAtQuantile(95.0))/1e6)
		fmt.Printf("  P99:    %.3f ms\n", float64(hist.ValueAtQuantile(99.0))/1e6)
		fmt.Printf("  P99.9:  %.3f ms\n", float64(hist.ValueAtQuantile(99.9))/1e6)

		// Hardware counters
		if m.TotalCycles > 0 || m.TotalInstructions > 0 {
			fmt.Printf("\nHardware Counters:\n")
			fmt.Printf("  Total Cycles:       %d\n", m.TotalCycles)
			fmt.Printf("  Total Instructions: %d\n", m.TotalInstructions)
			fmt.Printf("  Total Cache Misses: %d\n", m.TotalCacheMisses)

			if m.TotalCycles > 0 {
				ipc := float64(m.TotalInstructions) / float64(m.TotalCycles)
				fmt.Printf("  IPC (Instr/Cycle):  %.3f\n", ipc)
			}
			if m.TotalInstructions > 0 {
				missRate := float64(m.TotalCacheMisses) / float64(m.TotalInstructions) * 1000
				fmt.Printf("  Cache Misses/1K Instr: %.3f\n", missRate)
			}
		} else {
			fmt.Printf("\nHardware Counters: not available\n")
		}
	}
}

func collectAndWriteProfile(profiler *profilerState, pid int) {
	m := profiler.objs.PerfMaps.Counts
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

			symbol, err := profiler.symbolizer.Symbolize(uint32(pid), []uint64{instructionPointer})
			if err != nil {
				log.Printf("Failed to symbolize: %v", err)
				break
			}

			sb.append(symbol[0].Name)
		}

		end := len(sb.stack)
		Reverse(sb.stack[begin:end])

		sample := createSample(sb, value, pid)
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
	defer file.Close()

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
