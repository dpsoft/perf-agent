package main

//
//import (
//	"fmt"
//	"log"
//	"runtime"
//
//	"github.com/elastic/go-perf"
//	"kernel.org/pub/linux/libs/security/libcap/cap"
//)
//
//func main() {
//	// Grant CAP_SYS_ADMIN for perf event access
//
//	caps := cap.GetProc()
//
//	//err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN)
//	err := caps.SetFlag(cap.Effective, true, cap.SYS_ADMIN, cap.PERFMON)
//
//	if err != nil {
//		log.Fatalf("Failed to apply capabilities: %v", err)
//	}
//
//	// Create a performance event group
//	perfEvent := perf.Group{
//		CountFormat: perf.CountFormat{
//			Running: true,
//		},
//	}
//	perfEvent.Add(perf.Instructions, perf.CPUCycles)
//
//	// Lock to a specific thrsudoi ead
//	runtime.LockOSThread()
//	defer runtime.UnlockOSThread()
//
//	ipc, err := perfEvent.Open(perf.CallingThread, perf.AnyCPU)
//	if err != nil {
//		log.Fatalf("Failed to open perf group: %v", err)
//	}
//	//ipc.SetBPF()
//	defer ipc.Close()
//
//	// Perform a task and measure performance
//	sum := 0
//	metrics, err := ipc.MeasureGroup(func() {
//		for i := 0; i < 2e6; i++ {
//			sum += i
//		}
//	})
//	if err != nil {
//		log.Fatalf("Failed to measure group: %v", err)
//	}
//
//	insns, cycles := metrics.Values[0].Value, metrics.Values[1].Value
//	fmt.Printf("Sum: %d, Instructions: %d, CPU Cycles: %d, IPC: %.2f\n",
//		sum, insns, cycles, float64(insns)/float64(cycles))
//}
