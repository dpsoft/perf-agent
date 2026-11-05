package gpuprofile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/pprof"
)

// drainOnly is a Profiler with a timeline and no BPF consumer behind it: the
// test is the producer. The ticker is an hour, so any drain before Stop came
// from the timeline's pressure signal.
func drainOnly(t *testing.T, capacity int) (*Profiler, *gpu.Timeline) {
	t.Helper()
	return drainOnlyWith(t, capacity, Config{})
}

func drainOnlyWith(t *testing.T, capacity int, cfg Config) (*Profiler, *gpu.Timeline) {
	t.Helper()
	cfg.DrainEvery = time.Hour
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{})
	tl := gpu.NewTimeline(gpu.TimelineConfig{
		LaunchCache: gpu.LaunchCacheConfig{Capacity: capacity},
		Modules:     store,
	})
	var execLog *execlog.Writer
	if cfg.ExecutionsPath != "" {
		w, err := execlog.Create(cfg.ExecutionsPath, execlog.Header{CaptureID: cfg.CaptureID, Backend: "cupti", SampleType: "gpu/nanoseconds"})
		if err != nil {
			t.Fatal(err)
		}
		execLog = w
	}
	p := &Profiler{
		cfg:      cfg,
		execLog:  execLog,
		store:    store,
		timeline: tl,
		builders: pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1}),
		done:     make(chan struct{}),
		pcLabels: gpu.NewPCLabelBudget(cfg.MaxDistinctPCLabels),
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	consumerDone := make(chan struct{})
	close(consumerDone) // no consumer to wait for
	go func() {
		defer close(p.done)
		p.drainLoop(ctx, consumerDone)
	}()
	t.Cleanup(p.Stop)
	return p, tl
}

var execSeq int

func emitExecs(t *testing.T, tl *gpu.Timeline, n int) {
	t.Helper()
	for range n {
		execSeq++
		if err := tl.EmitExec(gpu.GPUKernelExec{
			Correlation: gpu.CorrelationID{Backend: gpu.BackendCUPTI, Value: fmt.Sprint(execSeq)},
			KernelName:  "k",
			StartNs:     uint64(execSeq) * 10,
			EndNs:       uint64(execSeq)*10 + 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func waitSamples(t *testing.T, p *Profiler, atLeast int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.Samples() < atLeast {
		if time.Now().After(deadline) {
			t.Fatalf("%d samples projected, want at least %d: nothing drained the timeline before its ticker", p.Samples(), atLeast)
		}
		time.Sleep(time.Millisecond)
	}
}

// Executions arriving faster than the drain ticker are drained as the ring
// fills, not evicted. Before, perf-agent --gpu drained only every 2s and lost
// everything past 65536 executions per interval, silently: a fine-tuning
// capture had to be cut to 16-microbatch windows to come out whole.
func TestAFillingTimelineIsDrainedBeforeItEvicts(t *testing.T) {
	const capacity = 64
	p, tl := drainOnly(t, capacity)

	// Five ring-fulls, each paused at just past half so the drainer can run;
	// with only the hour-long ticker, the first wait times out.
	// execSeq is shared by every test in the package; count this test's own.
	start := execSeq
	for range 10 {
		emitExecs(t, tl, capacity/2)
		waitSamples(t, p, execSeq-start)
	}
	p.Stop()

	if d := p.Dropped(); d.EvictedExecutions != 0 {
		t.Errorf("%d executions evicted, want 0", d.EvictedExecutions)
	}
	if got := p.Samples(); got != execSeq-start {
		t.Errorf("%d samples, want one per execution: %d", got, execSeq-start)
	}
}

// Executions the ring does evict are reported. They used to vanish: drain
// discarded the snapshot's loss record.
func TestEvictedExecutionsAreReported(t *testing.T) {
	const capacity = 8
	p, tl := drainOnly(t, capacity)
	// Pressure cannot help a producer that emits a ring-full and more in one
	// burst under the lock-free drainer; whatever it evicts must show.
	p.mu.Lock() // hold the drainer off: drain() blocks on p.mu after its Snapshot
	emitExecs(t, tl, 3*capacity)
	p.mu.Unlock()
	p.Stop()

	d := p.Dropped()
	if d.EvictedExecutions == 0 {
		t.Fatal("no evictions reported for 24 executions into an 8-slot ring")
	}
	if got := uint64(p.Samples()) + d.EvictedExecutions; got != uint64(3*capacity) {
		t.Errorf("samples (%d) + evicted (%d) = %d, want every execution accounted for: %d",
			p.Samples(), d.EvictedExecutions, got, 3*capacity)
	}
}

// A capture shorter than one drain interval is projected by Stop's final
// drain. The agent counted samples before that drain and reported such a
// capture as "no GPU activity", writing nothing.
func TestStopProjectsACaptureShorterThanOneInterval(t *testing.T) {
	p, tl := drainOnly(t, 1024)
	emitExecs(t, tl, 3)
	if got := p.Samples(); got != 0 {
		t.Fatalf("%d samples before Stop; the test needs the ticker not to have run", got)
	}
	p.Stop()
	if got := p.Samples(); got != 3 {
		t.Errorf("%d samples after Stop, want 3", got)
	}
}
