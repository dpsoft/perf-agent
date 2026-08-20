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
	"github.com/dpsoft/perf-agent/symbolize"
)

func main() {
	const stub = "./shim/perfagent-gpu-stub"

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	// Without a symbolizer the sampled launch stacks still arrive and are
	// still accounted for, but every one of them degrades to no stack — the
	// profile would then be honest and useless, all GPU time unattributed.
	sym, err := symbolize.NewLocalSymbolizer()
	if err != nil {
		log.Fatalf("symbolizer: %v", err)
	}
	defer func() { _ = sym.Close() }()

	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath:   stub,
		Backend:    gpu.GPUBackendID("stub"),
		Sink:       timeline,
		Symbolizer: sym,
	})
	if err != nil {
		log.Fatalf("attach: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	// Wait for Run to actually return before flushing or snapshotting:
	// cancelling only asks the reader to stop, it does not block until it
	// has. Flushing (or reading the timeline) while Run is still applying a
	// batch races the consumer's own goroutine and can snapshot mid-batch -
	// c.Flush() and Run's own deferred Flush would then interleave with
	// applyBatch under the same mutex but in an order this caller cannot
	// predict, and the snapshot below could be taken before the last batch
	// landed.
	<-done
	// Release any launch still being held for a sampled twin before the
	// snapshot: held launches are not lost, but a snapshot taken without
	// this would be missing the most recent ones and their executions would
	// read as unmatched. Run's own deferred Flush already did this once Run
	// returned; calling it again here is idempotent (there is nothing left
	// to release) and keeps this call site correct even if Run's internals
	// ever change.
	c.Flush()

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
