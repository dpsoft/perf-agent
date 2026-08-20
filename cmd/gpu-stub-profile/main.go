// Attaches to the stub, runs it, and writes gpu-stub.pb.gz through the
// existing pprof builder — the Phase 3 gate as an artifact a human can open.
package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/pprof"
)

func main() {
	const stub = "./shim/perfagent-gpu-stub"

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: stub,
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     timeline,
	})
	if err != nil {
		log.Fatalf("attach: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := c.Run(ctx); err != nil {
			log.Printf("consumer: %v", err)
		}
	}()

	if out, err := exec.Command(stub, "2000", "200").CombinedOutput(); err != nil {
		log.Fatalf("stub: %v: %s", err, out)
	}
	// The stub flushes both batches synchronously before it exits, and
	// CombinedOutput above already blocked until then, so every record is
	// in the ringbuf by this point. The sleep is for c.Run's goroutine to
	// drain it, not for the stub.
	time.Sleep(500 * time.Millisecond)
	cancel()

	snap := timeline.Snapshot()
	samples := gpu.ProjectExecutions(snap)
	if len(samples) == 0 {
		log.Fatal("no samples projected; the pipeline produced nothing")
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1})
	for i := range samples {
		builders.AddSample(&samples[i])
	}

	f, err := os.Create("gpu-stub.pb.gz")
	if err != nil {
		log.Fatal(err)
	}
	for _, b := range builders.Builders {
		if _, err := b.Write(f); err != nil {
			log.Fatalf("write profile: %v", err)
		}
		break
	}
	if err := f.Close(); err != nil {
		log.Fatalf("close gpu-stub.pb.gz: %v", err)
	}
	log.Printf("wrote gpu-stub.pb.gz: %d samples, stats=%+v", len(samples), c.Stats())
}
