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
	"github.com/dpsoft/perf-agent/internal/gpuabi"
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

	// sample_period=8 (the stub's default, stated) and a 30s linger. The
	// linger is what makes the CPU frames in this profile readable: a
	// sampled launch's stack is symbolized against /proc/<pid>/maps of the
	// stub, which the kernel destroys the instant the stub exits. Waiting on
	// the process (the previous CombinedOutput) meant every record still in
	// the ringbuf at that moment resolved to bare hex addresses — an artifact
	// that looks like a working profile and names nothing.
	// The sampler jitters each gap around the period (issue #50), so this is
	// not 2000/8; it is the exact length of the deterministic schedule the
	// stub will follow, replayed here from the same seed the stub uses.
	// Rounding it up to 250 would park this loop on its 30s deadline for a
	// count that is never reached.
	wantSampled := gpuabi.SampledCount(2000, 8, gpuabi.DefaultSampleSeed)
	cmd := exec.Command(stub, "2000", "200", "8", "30000")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	release, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("stub stdin: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("stub: %v", err)
	}
	// Hold the stub open until the consumer has counted every sampled
	// launch; closing its stdin is what releases it.
	deadline := time.Now().Add(30 * time.Second)
	for c.Stats().SampledLaunches < wantSampled {
		if time.Now().After(deadline) {
			log.Printf("WARNING: only %d/%d sampled launches observed before the stub was released; "+
				"stacks that arrive after it exits cannot be symbolized",
				c.Stats().SampledLaunches, wantSampled)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := release.Close(); err != nil {
		log.Fatalf("release stub: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		log.Fatalf("stub: %v", err)
	}
	// The stub flushes both batches synchronously before it lingers, so every
	// record is in the ringbuf by this point. The sleep is for c.Run's
	// goroutine to drain the tail of batched launches and executions — none
	// of which carries a stack, so none of it needs the stub alive.
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
	// c.Stats() above is ingestion: what arrived off the ringbuf. This is
	// attribution: what the timeline could join it to, and what it evicted
	// trying. A run can be perfect on the first and quietly useless on the
	// second, so both are printed - one line when the join is clean, one
	// extra line per anomaly when it is not (see gpu.JoinHealth).
	for _, line := range gpu.JoinHealth(snap) {
		log.Print(line)
	}
}
