package main

import (
	"bytes"
	"ebpfExecutor/blazesym"
	"ebpfExecutor/pprof"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/google/pprof/profile"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
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

const pid int = 197936

func main() {
	// Grant CAP_SYS_ADMIN for perf event access
	caps := cap.GetProc()
	err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN, cap.PERFMON)
	if err != nil {
		log.Fatalf("Failed to apply capabilities: %v", err)
	}

	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("Failed to remove memlock: %v", err)
	}

	objs := PerfObjects{}
	if err := LoadPerfObjects(&objs, nil); err != nil {
		log.Fatal(err)
	}
	defer objs.Close()

	config := PerfPidConfig{
		Type:          0,
		CollectUser:   1,
		CollectKernel: 0,
	}

	err = objs.Pids.Update(uint32(pid), &config, ebpf.UpdateAny)
	if err != nil {
		log.Fatalf("failed to update pid map: %v", err)
	}

	onlineCPUIDs, err := cpuonline.Get()
	if err != nil {
		log.Fatalf("failed to get online CPUs: %v", err)
	}

	for _, id := range onlineCPUIDs {
		pe, err := newPerfEvent(int(id), 10000)
		if err != nil {
			log.Fatalf("failed to create perf event on CPU %d: %v", id, err)
		}

		err = pe.attachPerfEvent(objs.Profile)
		if err != nil {
			log.Fatalf("failed to attach eBPF program to perf event on CPU %d: %v", id, err)
		}
	}

	//symbolizer
	symbolizer, err := blazesym.NewSymbolizer()
	defer symbolizer.Close()

	if err != nil {
		log.Fatalf("Failed to create symbolizer: %v", err)
	}

	//go func() {
	//	for {
	fmt.Println("Waiting...")
	time.Sleep(10 * time.Second)

	var (
		m       = objs.PerfMaps.Counts
		mapSize = m.MaxEntries()
	)

	keys := make([]PerfSampleKey, mapSize)
	values := make([]uint32, mapSize)

	opts := &ebpf.BatchOptions{}
	cursor := new(ebpf.MapBatchCursor)

	n, err := m.BatchLookupAndDelete(cursor, keys, values, opts)
	if n > 0 {
		log.Printf("BatchLookupAndDelete: %d", n)

	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		log.Printf("BatchLookupAndDelete: %s", err)
	}

	log.Println(
		"msg", "getCountsMapValues iter",
		"count", len(keys),
	)

	for _, k := range keys {
		//if config.CollectUser > 0 {
		//var userStack uint8
		//var kernelStack uint64
		stack, err := objs.Stackmap.LookupBytes(uint32(k.UserStack))
		if err != nil {
			log.Printf("Failed to lookup user stack: %v", err)
		}

		if len(stack) == 0 {
			return
		}

		var fullStack []uint64
		for i := 0; i < 127; i++ {
			instructionPointerBytes := stack[i*8 : i*8+8]
			instructionPointer := binary.LittleEndian.Uint64(instructionPointerBytes)
			if instructionPointer == 0 {
				break
			}
			fullStack = append(fullStack, instructionPointer)
		}

		//var results []string
		symbols, err := symbolizer.Symbolize(uint32(pid), fullStack)
		if err != nil {
			log.Println("Failed to symbolize: %v", err)
		}

		sb := new(stackBuilder)
		begin := len(sb.stack)

		for _, symbol := range symbols {
			//results = append(results, fmt.Sprintf("%s:%d", symbol.Name, symbol.Line))
			sb.append(symbol.Name)
		}
		end := len(sb.stack)
		Reverse(sb.stack[begin:end])

		builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
			SampleRate:    int64(97),
			PerPIDProfile: false,
		})

		caca := pprof.ProfileSample{
			Pid:         uint32(pid),
			Aggregation: pprof.SampleAggregated,
			SampleType:  pprof.SampleTypeCpu,
			Stack:       sb.stack,
			Value:       uint64(k.UserStack),
		}

		builders.AddSample(&caca)

		builder := builders.BuilderForSample(&caca)

		buf := bytes.NewBuffer(nil)
		_, err = builder.Write(buf)
		if err != nil {
			log.Fatalf("Failed to write profile: %v", err)
		}
		rawProfile := buf.Bytes()

		parsed, err := profile.Parse(bytes.NewBuffer(rawProfile))
		if err != nil {
			log.Fatalf("Failed to write profile: %v", err)
		}
		log.Println(parsed)

		//err = objs.Stackmap.Lookup(uint32(k.KernStack), kernelStack)
		//if err != nil {
		//	log.Printf("Failed to lookup kernel stack: %v", err)
		//}
		//log.Println(symbolizer.Symbolize(uint32(pid), userStack))
		//log.Println(symbolizer.Symbolize(uint32(pid), kernelStack))
		//}
	}

	//results := []string{}
	//
	//symbols, err := symbolizer.Symbolize(uint32(pid), []uint64{})
	//if err != nil {
	//	log.Println("Failed to symbolize: %v", err)
	//}
	////
	//for _, symbol := range symbols {
	//	results = append(results, fmt.Sprintf("%s:%d", symbol.Name, symbol.Line))
	//}

	log.Println("keys:", keys)
	log.Println("values:", values)

	log.Println("eBPF program successfully loaded and attached")

	//log.Println("Results:", results)
	// Create a channel to receive OS signals.
	sigChan := make(chan os.Signal, 1)

	// Notify the channel on interrupt and terminate signals.
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Block until a signal is received.
	sig := <-sigChan

	fmt.Printf("Received signal: %s\n", sig)

	// Perform cleanup here if necessary.

	fmt.Println("Exiting program.")
}

func Reverse[S ~[]E, E any](s S) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
