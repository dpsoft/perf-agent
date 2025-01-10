package main

import (
	"errors"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/iovisor/gobpf/pkg/cpuonline"
	"kernel.org/pub/linux/libs/security/libcap/cap"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const pid int = 12714

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

	// Load the eBPF program from an ELF file.

	//spec, err := ebpf.LoadCollectionSpec("/home/diego/github/otrotest/perf.bpf.o")
	//if err != nil {
	//	log.Fatalf("Failed to load eBPF program: %v", err)
	//}
	//
	//// Load the eBPF program from an ELF file.
	//coll, err := ebpf.NewCollection(spec)
	//if err != nil {
	//	log.Fatalf("Failed to load eBPF program: %v", err)
	//}
	//defer coll.Close()

	//prog := coll.Programs["profile"]
	//log.Println(prog.Type())
	//if prog == nil {
	//	log.Fatalf("Program 'kprobe_execve' not found")
	//}

	//symbolizer, err := blazesym.NewSymbolizer();
	//if err != nil {
	//	log.Fatalf("Failed to create symbolizer: %v", err)
	//}

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
		pe, err := newPerfEvent(int(id), 100)
		if err != nil {
			log.Fatalf("failed to create perf event on CPU %d: %v", id, err)
		}

		err = pe.attachPerfEvent(objs.Profile)
		if err != nil {
			log.Fatalf("failed to attach eBPF program to perf event on CPU %d: %v", id, err)
		}
	}

	// read the events from the ring buffer
	//rd, err := ringbuf.NewReader(coll.Maps["counts"])
	////rd, err := perf.NewReader(coll.Maps["counts"], os.Getpagesize())
	//if err != nil {
	//	log.Fatalf("Creating event reader: %s", err)
	//}
	//defer rd.Close()
	//
	////symbolizer
	//symbolizer, err := blazesym.NewSymbolizer()
	//defer symbolizer.Close()
	//
	//if err != nil {
	//	log.Fatalf("Failed to create symbolizer: %v", err)
	//}
	//
	//results := []string{}
	//

	//go func() {
	//	for {
	fmt.Println("Waiting...")
	time.Sleep(10 * time.Second)
	//var event interface{}
	//currPid := uint32(pid)
	//log.Printf("current pid: %d", currPid)
	//kp := unsafe.Pointer(&currPid)
	////for objs.Counts.Iterate()

	//if err := objs.Counts.Lookup(kp, &event); err != nil {
	//	log.Printf("get event failed: %s", err)
	//}

	//counts := coll.Maps["counts"]
	//if counts == nil {
	//	log.Fatalf("Map 'counts' not found")
	//}

	var (
		m       = objs.PerfMaps.Counts
		mapSize = m.MaxEntries()
	)
	//
	keys := make([]PerfSampleKey, mapSize)
	values := make([]uint32, mapSize)
	//
	opts := &ebpf.BatchOptions{}
	cursor := new(ebpf.MapBatchCursor)

	//it := m.Iterate()
	//for {
	//	ok := it.Next(&keys, &values)
	//	k := PerfSampleKey{}
	//	v := uint32(0)
	//	if err := i.MapRead(&k, &v); err != nil {
	//		log.Printf("MapRead failed: %s", err)
	//	}
	//	keys = append(keys, k)
	//	values = append(values, v)
	//}
	n, err := m.BatchLookupAndDelete(cursor, keys, values, opts)
	if n > 0 {
		log.Printf("BatchLookupAndDelete: %d", n)

	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		log.Printf("BatchLookupAndDelete: %s", err)
	}

	//resultKeys := keys[:0]
	//resultValues := values[:0]
	//it := counts.Iterate()
	//
	//var k PerfSampleKey
	//var v uint32
	//for it.Next(&k, &v) {
	//	resultKeys = append(resultKeys, k)
	//	resultValues = append(resultValues, v)
	//}
	//if err := it.Err(); err != nil {
	//	log.Fatalf("iteration error: %v", err)
	//}
	log.Println(
		"msg", "getCountsMapValues iter",
		"count", len(keys),
	)
	//record, err := rd.Read()
	//if err != nil {
	//	if errors.Is(err, perf.ErrClosed) {
	//		log.Println("Received signal, exiting..")
	//		return
	//	}
	//	log.Printf("Reading from reader: %s", err)
	//	continue
	//}

	//symbols, err := symbolizer.Symbolize(pid, []uint64{})
	//if err != nil {
	//	log.Println("Failed to symbolize: %v", err)
	//}
	////
	//for _, symbol := range symbols {
	//	results = append(results, fmt.Sprintf("%s:%d", symbol.Name, symbol.Line))
	//}

	log.Println("keys:", keys)
	log.Println("values:", keys)

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
