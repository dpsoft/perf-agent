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
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpu/execlog"
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

	// EventRingBytes sizes the BPF event ringbuf. Zero keeps the
	// compiled-in default, which is sized for a default-period capture.
	EventRingBytes int

	// PerExecutionSamples keeps one pprof sample per projected execution,
	// each labelled with its gpu_correlation, so a profile can be filtered to
	// a single kernel execution (pprof -tagfocus). It costs memory per kernel.
	// By default samples with the same stack and labels are merged instead,
	// so memory grows with distinct stacks; ExecutionsPath keeps the
	// per-execution detail outside the profile.
	PerExecutionSamples bool

	// ExecutionsPath, when set, streams the per-execution detail of the
	// aggregated profile to this file (see gpu/execlog): which aggregated
	// sample each execution was summed into, its correlation id and value.
	// It is written incrementally, so it costs no memory per execution.
	// Incompatible with PerExecutionSamples.
	ExecutionsPath string

	// CaptureID identifies the capture in the executions log header.
	CaptureID string

	// MaxDistinctPCLabels caps the distinct gpu_pc values in the whole
	// profile; zero keeps the gpu package's default. PC samples past it are
	// kept without gpu_pc and counted in Counts.
	MaxDistinctPCLabels int

	// ProtectedPaths are the capture's other outputs. The executions log is
	// never published over any of them.
	ProtectedPaths []string
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
	// executions counts the executions the timeline handed over, before
	// projection; samples counts projected rows. With PC sampling one
	// execution projects to several rows, and by default rows are merged into
	// fewer aggregated samples, so the three numbers differ.
	executions uint64
	execLog    *execlog.Writer
	// execLogErr is why the executions log was abandoned, if it was. The
	// profile itself is unaffected.
	execLogErr error

	// pcLabels is the gpu_pc budget for the whole capture: the profile
	// accumulates across drains, so a budget per drain would let it carry
	// one ceiling of distinct gpu_pc values per drain.
	pcLabels *gpu.PCLabelBudget
	// pcSuppressed counts PC samples written without gpu_pc because the
	// budget was full. Their time and other labels are kept.
	pcSuppressed uint64
	distinctPCs  uint64
	pcLabelCap   uint64

	// dropped is the timeline's loss record as of the latest drain. The
	// timeline's counters are cumulative, so this is replaced, never summed.
	dropped gpu.TimelineDropStats
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

	if cfg.PerExecutionSamples && cfg.ExecutionsPath != "" {
		return nil, errors.New("gpuprofile: an executions log describes an aggregated profile; it cannot be combined with per-execution samples")
	}
	// Created before attaching: a requested log that cannot be written should
	// fail the capture up front, not lose its detail at the end.
	var execLog *execlog.Writer
	if cfg.ExecutionsPath != "" {
		w, err := execlog.Create(cfg.ExecutionsPath, execlog.Header{
			CaptureID:  cfg.CaptureID,
			Backend:    string(gpu.BackendCUPTI),
			SampleType: "gpu/nanoseconds",
		})
		if err != nil {
			return nil, fmt.Errorf("gpuprofile: executions log: %w", err)
		}
		w.Protect(cfg.ProtectedPaths...)
		execLog = w
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
		EventRingBytes:            cfg.EventRingBytes,
	})
	if err != nil {
		if execLog != nil {
			execLog.Abort()
		}
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
		execLog:  execLog,
		pcLabels: gpu.NewPCLabelBudget(cfg.MaxDistinctPCLabels),
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
	p.drainLoop(ctx, consumerDone)
}

// drainLoop drains the timeline until ctx ends, then waits for the consumer
// and drains once more. Split from run so a test can drive it with no BPF
// consumer behind the timeline.
func (p *Profiler) drainLoop(ctx context.Context, consumerDone <-chan struct{}) {
	// The profile accumulates ACROSS drains, which is what lets an attached
	// run empty the timeline while it is still collecting. Snapshot drains:
	// it swaps in fresh rings and leaves anything already projected behind.
	//
	// It drains on the ticker and whenever a ring passes half full. The
	// ticker alone lost every execution past 65536 per interval -- about 33k
	// kernels/s at the 2s default, which a fine-tuning loop of small kernels
	// exceeds -- and said nothing about it.
	pressure := p.timeline.Pressure()
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
		case <-pressure:
			p.drain()
		}
	}
}

func (p *Profiler) drain() {
	snap := p.timeline.Snapshot()
	pcfg := gpu.ProjectionConfig{Modules: p.store, PCLabels: p.pcLabels}
	var execOf []int
	if !p.cfg.PerExecutionSamples {
		pcfg.SampleExecutions = &execOf
		pcfg.OmitCorrelationLabel = true
	}
	samples, stats := gpu.ProjectExecutionsWith(snap, pcfg)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pcSuppressed += stats.PCLabelsSuppressed
	p.distinctPCs, p.pcLabelCap = stats.DistinctPCLabels, stats.PCLabelCap
	p.dropped = snap.Dropped
	// Ordinals continue across drains: this snapshot's executions are
	// numbered from the count already seen.
	base := p.executions
	p.executions += uint64(len(snap.Executions))
	p.samples += len(samples)
	if p.cfg.PerExecutionSamples {
		for i := range samples {
			p.builders.AddSample(&samples[i])
		}
		return
	}
	for i := range samples {
		s := &samples[i]
		// gpu_correlation, the one label unique per execution, was left off
		// (OmitCorrelationLabel), so samples with the same stack and remaining
		// labels merge. With PC sampling, gpu_pc is capped for the whole
		// capture by p.pcLabels; gpu_src_* follow the PCs and are bounded by
		// the device code's source lines, not by that cap.
		s.Aggregation = pprof.SampleAggregation(false)
		idx := p.builders.AddSampleIndexed(s)
		if p.execLog != nil {
			corr := snap.Executions[execOf[i]].Exec.Correlation
			p.logRowLocked(idx, base+uint64(execOf[i]), corr, s.Value)
		}
	}
}

// logRowLocked appends one row to the executions log, or abandons the log
// with a reason if the row cannot be represented exactly. The caller holds
// p.mu.
func (p *Profiler) logRowLocked(sample int, execution uint64, corr gpu.CorrelationID, value uint64) {
	row := execlog.Row{Execution: execution, Value: value}
	if sample < 0 || uint64(sample) > uint64(^uint32(0)) {
		p.abandonExecLogLocked(fmt.Errorf("aggregated sample index %d out of range", sample))
		return
	}
	row.Sample = uint32(sample)
	if corr.Present() {
		v, err := strconv.ParseUint(corr.Value, 10, 64)
		if corr.Backend != gpu.BackendCUPTI || err != nil {
			p.abandonExecLogLocked(fmt.Errorf("correlation %s:%s is not a %s:<uint64> id", corr.Backend, corr.Value, gpu.BackendCUPTI))
			return
		}
		row.HasCorrelation, row.Correlation = true, v
	}
	p.execLog.Add(row)
	if err := p.execLog.Err(); err != nil {
		p.abandonExecLogLocked(err)
	}
}

func (p *Profiler) abandonExecLogLocked(err error) {
	logf("gpuprofile: executions log abandoned, the profile is unaffected: %v", err)
	p.execLog.Abort()
	p.execLog = nil
	p.execLogErr = err
}

// Collect stops the pipeline and writes the accumulated profile.
func (p *Profiler) Collect(w io.Writer) error {
	p.stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.builders.Builders {
		// The executions log binds to these exact bytes.
		digest := sha256.New()
		if _, err := b.Write(io.MultiWriter(w, digest)); err != nil {
			if p.execLog != nil {
				p.abandonExecLogLocked(fmt.Errorf("profile write failed: %w", err))
			}
			return fmt.Errorf("gpuprofile: write profile: %w", err)
		}
		if p.execLog != nil {
			var sum [32]byte
			copy(sum[:], digest.Sum(nil))
			if err := p.execLog.Finish(uint64(len(b.Profile.Sample)), sum); err != nil {
				logf("gpuprofile: executions log not written, the profile is unaffected: %v", err)
				p.execLogErr = err
			}
			p.execLog = nil
		}
		// One builder: the GPU pipeline does not split per-PID profiles.
		return nil
	}
	if p.execLog != nil {
		p.abandonExecLogLocked(errors.New("no GPU samples were collected"))
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

// Counts are the three numbers a GPU profile's size has to be read against:
// executions the timeline handed over, rows they projected to (more than
// executions with PC sampling), and aggregated samples in the profile.
type Counts struct {
	Executions        uint64
	ProjectedRows     uint64
	AggregatedSamples uint64
	// DistinctPCLabels is how many distinct gpu_pc values the profile
	// carries, against PCLabelCap; PCLabelsSuppressed counts PC samples
	// written without one because the cap was reached.
	DistinctPCLabels   uint64
	PCLabelCap         uint64
	PCLabelsSuppressed uint64
	// ExecutionsLogErr is why a requested executions log was not written.
	ExecutionsLogErr error
}

// Counts reports Counts; final after Stop.
func (p *Profiler) Counts() Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := Counts{
		Executions: p.executions, ProjectedRows: uint64(p.samples), ExecutionsLogErr: p.execLogErr,
		DistinctPCLabels: p.distinctPCs, PCLabelCap: p.pcLabelCap, PCLabelsSuppressed: p.pcSuppressed,
	}
	for _, b := range p.builders.Builders {
		c.AggregatedSamples += uint64(len(b.Profile.Sample))
	}
	return c
}

// Samples reports how many GPU samples have been projected so far.
func (p *Profiler) Samples() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.samples
}

// Dropped reports what the timeline evicted before a drain reached it: GPU
// work that arrived and is not in the profile. Stats does not include it; the
// two are lost at different stages. Final only after Stop.
func (p *Profiler) Dropped() gpu.TimelineDropStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// Stop ends collection and runs the final drain, so Samples and Dropped
// cover the whole capture. Collect and Close call it too; it runs once.
func (p *Profiler) Stop() { p.stop() }

// Stats reports the consumer's ingestion counters: what arrived off the
// ringbuf and what was lost on the way. The profile alone cannot show this --
// a capture that dropped most of its launches still writes a well-formed
// profile, just a smaller one -- so the caller needs the counters to tell a
// quiet GPU from a lossy capture.
func (p *Profiler) Stats() gpuprobe.Stats {
	return p.consumer.Stats()
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
	p.mu.Lock()
	if p.execLog != nil {
		// Never collected: nothing to bind the log to.
		p.execLog.Abort()
		p.execLog = nil
	}
	p.mu.Unlock()
	if err := p.consumer.Close(); err != nil {
		logf("gpuprofile: close consumer: %v", err)
	}
}

// SetCapture supplies the capture lifecycle metadata for serialization.
func (p *Profiler) SetCapture(c pprof.CaptureWindow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.builders.SetCapture(c)
}
