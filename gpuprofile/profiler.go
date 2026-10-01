// Package gpuprofile runs the GPU pipeline as one more collector inside
// perf-agent, alongside the CPU, off-CPU and PMU ones.
//
// It exists because measuring CPU and GPU on the same process used to take
// two binaries and a join done by hand. Everything needed to avoid that was
// already here: gpuprobe is importable, both sides stamp from the same
// monotonic clock, and the agent already runs three independent BPF-using
// collectors with one lifecycle. What was missing was a type shaped like
// the others. See issue #154.
//
// The injection step is NOT handled here, and that is deliberate. CUPTI
// loads the adapter from CUDA_INJECTION64_PATH during cuInit and nothing
// can add it to a live process, so whoever sets it must start the workload.
// This profiler attaches, exactly as Parca's and OpenTelemetry's do; the
// operator sets the variable in a pod spec or a shell. Teaching the agent
// to launch workloads would diverge from every comparable profiler to save
// one documented line.
package gpuprofile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

// Config is what the agent supplies. The pipeline's many capacity dials are
// deliberately not surfaced: they have defaults chosen against measured
// workloads, and re-exporting them here would put a second copy of the
// sizing where drift is invisible.
type Config struct {
	// ShimPath is the CUPTI adapter the target loaded through
	// CUDA_INJECTION64_PATH. The uprobe attaches to this file's inode, so it
	// must be the same FILE the target mapped -- a rebuilt or copied shim
	// with identical bytes is a different inode and therefore a different
	// target.
	ShimPath string

	// PID is the process to profile. Non-zero narrows the uprobe link to it
	// AND takes the eager CFI-registration path, which is what makes a late
	// attach viable: a process already past cuInit cannot use the startup
	// rendezvous, and without its tables installed before the link exists
	// its first stacks are walked without them.
	PID int

	// Symbolizer resolves the instruction pointers of a sampled launch's
	// captured stack. The agent passes its own, so the GPU stacks resolve
	// through the same module index and caches as the CPU ones. Nil disables
	// resolution: launches still arrive and still reach the profile, without
	// a stack.
	Symbolizer symbolize.Symbolizer

	// PCSampling selects the GPU PC-sampling tier.
	PCSampling gpu.PCSamplingTier

	// KeepInstrumentationFrames leaves the profiler's own delivery path in
	// every sampled stack. See gpuprobe.Config; the elision is irreversible
	// once the profile is written.
	KeepInstrumentationFrames bool

	// DrainEvery is how often the timeline is drained into the profile. The
	// timeline's rings hold ONE interval, not the whole run, so a long attach
	// to a busy process overruns them and loses GPU time outright. Zero means
	// DefaultDrainEvery.
	DrainEvery time.Duration
}

// DefaultDrainEvery matches cmd/gpu-cuda-profile's default.
const DefaultDrainEvery = 2 * time.Second

// Profiler is the GPU collector. Its method set matches the off-CPU
// profiler's so the agent can treat them alike.
type Profiler struct {
	cfg Config

	store    *gpu.ModuleStore
	timeline *gpu.Timeline
	consumer *gpuprobe.Consumer
	builders *pprof.ProfileBuilders

	cancel context.CancelFunc
	done   chan struct{}

	// mu guards the accumulating profile. drain() runs on a ticker while
	// Collect may be called from the agent's shutdown path.
	mu      sync.Mutex
	samples int
	stopped bool
}

// New attaches the GPU pipeline to a target and starts draining it.
//
// It returns an error rather than degrading when the shim is not mapped into
// the target: --gpu is a flag the operator passed on purpose, and the CUPTI
// adapter's own failure mode is to fail open and silent. Inheriting that
// would turn "this run measured no GPU" into something indistinguishable
// from "this workload used no GPU".
func New(cfg Config) (*Profiler, error) {
	if cfg.ShimPath == "" {
		return nil, errors.New("gpuprofile: no shim path; GPU profiling needs the adapter the target loaded through CUDA_INJECTION64_PATH")
	}
	if _, err := os.Stat(cfg.ShimPath); err != nil {
		return nil, fmt.Errorf("gpuprofile: shim %s: %w", cfg.ShimPath, err)
	}
	if cfg.DrainEvery <= 0 {
		cfg.DrainEvery = DefaultDrainEvery
	}

	// One store, three readers: the cubin listener writes into it, the
	// timeline's join resolves device function names through it, and the
	// projection resolves source locations against it. A second instance
	// would hold a second copy of every cubin and answer "no-module" for
	// modules the first one holds.
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{})
	timeline := gpu.NewTimeline(gpu.TimelineConfig{PCSampling: cfg.PCSampling, Modules: store})

	var eager []int
	if cfg.PID != 0 {
		eager = []int{cfg.PID}
	}

	consumer, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath:                  cfg.ShimPath,
		PID:                       cfg.PID,
		EagerPIDs:                 eager,
		Backend:                   gpu.BackendCUPTI,
		Sink:                      timeline,
		Symbolizer:                cfg.Symbolizer,
		Modules:                   store,
		KeepInstrumentationFrames: cfg.KeepInstrumentationFrames,
	})
	if err != nil {
		return nil, fmt.Errorf("gpuprofile: attach to %s: %w", cfg.ShimPath, err)
	}

	p := &Profiler{
		cfg:      cfg,
		store:    store,
		timeline: timeline,
		consumer: consumer,
		// SampleRate 1: GPU values are measured durations in nanoseconds and
		// must not be scaled by a CPU sampling period.
		builders: pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1}),
		done:     make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.run(ctx)
	return p, nil
}

// run drives the consumer and drains the timeline on a ticker.
func (p *Profiler) run(ctx context.Context) {
	defer close(p.done)

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		if err := p.consumer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logf("gpuprofile: consumer: %v", err)
		}
	}()

	// The profile accumulates ACROSS drains, which is what lets an attached
	// run empty the timeline while it is still collecting. Snapshot drains:
	// it swaps in fresh rings and leaves anything already projected behind.
	ticker := time.NewTicker(p.cfg.DrainEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			<-consumerDone
			// One last drain after the consumer has stopped, so GPU time
			// that arrived inside the final interval is not discarded.
			p.drain()
			return
		case <-ticker.C:
			p.drain()
		}
	}
}

func (p *Profiler) drain() {
	snap := p.timeline.Snapshot()
	samples, _ := gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{Modules: p.store})
	if len(samples) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range samples {
		p.builders.AddSample(&samples[i])
	}
	p.samples += len(samples)
}

// Collect stops the pipeline and writes the accumulated profile.
func (p *Profiler) Collect(w io.Writer) error {
	p.stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.builders.Builders {
		if _, err := b.Write(w); err != nil {
			return fmt.Errorf("gpuprofile: write profile: %w", err)
		}
		// One builder: the GPU pipeline does not split per-PID profiles.
		return nil
	}
	return errors.New("gpuprofile: no GPU samples were collected")
}

// CollectAndWrite stops the pipeline and writes the profile to path.
func (p *Profiler) CollectAndWrite(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("gpuprofile: create %s: %w", path, err)
	}
	if err := p.Collect(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("gpuprofile: close %s: %w", path, err)
	}
	return nil
}

// Samples reports how many GPU samples have been projected so far.
func (p *Profiler) Samples() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.samples
}

// stop cancels the run loop once and waits for the final drain.
func (p *Profiler) stop() {
	p.mu.Lock()
	already := p.stopped
	p.stopped = true
	p.mu.Unlock()
	if already {
		return
	}
	p.cancel()
	<-p.done
}

// Close stops the pipeline and releases the BPF resources.
func (p *Profiler) Close() {
	p.stop()
	if err := p.consumer.Close(); err != nil {
		logf("gpuprofile: close consumer: %v", err)
	}
}
