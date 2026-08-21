// Attaches to the NVIDIA CUPTI adapter, runs a real CUDA workload under it,
// and writes gpu-cuda.pb.gz through the existing pprof builder.
//
// This is cmd/gpu-stub-profile with the synthetic producer replaced by an
// actual GPU. The adapter is not executed: it is a shared object the CUDA
// driver loads into the workload through CUDA_INJECTION64_PATH, so this
// command attaches its uprobes to the .so by path (system-wide, since the
// process it will be mapped into does not exist yet) and then starts the
// workload with that environment variable set.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

func main() {
	var (
		shim     = flag.String("shim", "./shim/libperfagent-gpu-nvidia.so", "the CUPTI adapter .so carrying the perfagent USDT probes")
		workload = flag.String("workload", "./shim/nvidia/testdata/cuda_workload", "CUDA program to run under the adapter")
		iters    = flag.Int("iters", 2000, "workload iterations; it launches two kernels per iteration")
		sleepUs  = flag.Int("sleep-us", 200, "workload sleep between iterations, in microseconds")
		period   = flag.Int("period", 8, "one-in-N launch sampling period (PERFAGENT_GPU_SAMPLE_PERIOD)")
		linger   = flag.Int("linger-ms", 30000, "how long the workload may wait to be released after it finishes")
		out      = flag.String("out", "gpu-cuda.pb.gz", "output pprof profile")
	)
	flag.Parse()

	shimPath, err := filepath.Abs(*shim)
	if err != nil {
		log.Fatalf("shim path: %v", err)
	}
	// The driver dlopens this by the exact string in CUDA_INJECTION64_PATH,
	// and the uprobes are attached to the path resolved here. If the two ever
	// disagreed the probes would sit on a file nothing maps and the run would
	// silently produce an empty profile, so both come from one variable.
	if _, err := os.Stat(shimPath); err != nil {
		log.Fatalf("adapter %s: %v (build it with: make -C shim nvidia)", shimPath, err)
	}

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
		ShimPath: shimPath,
		// PID 0: the process that will map the adapter has not been started
		// yet, so the attachment has to be system-wide. The semaphore the
		// uprobe refcount maintains is what arms the probes in it once the
		// driver maps the .so.
		PID:        0,
		Backend:    gpu.BackendCUPTI,
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

	// The sampler is deterministic one-in-N over every launch, and the
	// workload launches exactly two kernels per iteration, so the expected
	// count is exact rather than approximate.
	launches := *iters * 2
	wantSampled := (launches + *period - 1) / *period

	cmd := exec.Command(*workload,
		fmt.Sprint(*iters), fmt.Sprint(*sleepUs), fmt.Sprint(*linger))
	cmd.Env = append(os.Environ(),
		"CUDA_INJECTION64_PATH="+shimPath,
		fmt.Sprintf("PERFAGENT_GPU_SAMPLE_PERIOD=%d", *period),
		"PERFAGENT_GPU_LOG=stderr",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Same release protocol as the stub: the workload's CPU stacks are
	// symbolized against /proc/<pid>/maps, which the kernel destroys the
	// instant it exits, so we hold its stdin open until the consumer has
	// counted what it needs.
	release, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("workload stdin: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("workload: %v", err)
	}

	deadline := time.Now().Add(time.Duration(*linger) * time.Millisecond)
	for c.Stats().SampledLaunches < uint64(wantSampled) {
		if time.Now().After(deadline) {
			log.Printf("WARNING: only %d/%d sampled launches observed before the workload was released; "+
				"stacks that arrive after it exits cannot be symbolized",
				c.Stats().SampledLaunches, wantSampled)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := release.Close(); err != nil {
		log.Fatalf("release workload: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		log.Fatalf("workload: %v", err)
	}
	// The adapter's atexit handler runs cuptiActivityFlushAll and flushes
	// both batches before the process leaves, so everything is in the ringbuf
	// by now. This sleep is for the consumer goroutine to drain the tail of
	// batched launches and executions — none of which carries a stack, so
	// none of it needs the workload alive.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done
	// Release any launch still held for a sampled twin before the snapshot.
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

	f, err := os.Create(*out)
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
		log.Fatalf("close %s: %v", *out, err)
	}
	st := c.Stats()
	log.Printf("wrote %s: %d samples, launches=%d expected_sampled=%d stats=%+v",
		*out, len(samples), launches, wantSampled, st)
}
