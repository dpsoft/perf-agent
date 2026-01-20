package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

var (
	duration = flag.Duration("duration", 30*time.Second, "Run duration")
	threads  = flag.Int("threads", 4, "Number of I/O threads")
)

func main() {
	flag.Parse()
	fmt.Printf("Starting I/O-bound workload: %d threads for %v\n", *threads, *duration)
	fmt.Printf("PID: %d\n", os.Getpid())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < *threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tmpFile := fmt.Sprintf("/tmp/perf-agent-test-%d.dat", id)
			defer os.Remove(tmpFile)

			for {
				select {
				case <-stop:
					return
				default:
					// Write 1MB
					f, _ := os.Create(tmpFile)
					data := make([]byte, 1024*1024)
					f.Write(data)
					f.Sync()
					f.Close()

					// Read it back
					f, _ = os.Open(tmpFile)
					io.ReadAll(f)
					f.Close()

					time.Sleep(100 * time.Millisecond)
				}
			}
		}(i)
	}

	time.Sleep(*duration)
	close(stop)
	wg.Wait()
	fmt.Println("Workload completed")
}
