package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
	"time"
)

var (
	duration = flag.Duration("duration", 30*time.Second, "Run duration")
	threads  = flag.Int("threads", runtime.NumCPU(), "Number of threads")
)

func main() {
	flag.Parse()
	fmt.Printf("Starting CPU-bound workload: %d threads for %v\n", *threads, *duration)
	fmt.Printf("PID: %d\n", os.Getpid())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// CPU-intensive computation
	for i := 0; i < *threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sum := 0.0
			for {
				select {
				case <-stop:
					return
				default:
					// Compute-heavy operations
					for j := 0; j < 10000; j++ {
						sum += math.Sqrt(float64(j)) * math.Sin(float64(j))
					}
				}
			}
		}(i)
	}

	time.Sleep(*duration)
	close(stop)
	wg.Wait()
	fmt.Println("Workload completed")
}
