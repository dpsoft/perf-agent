package dwarfagent

import (
	"fmt"
	"io"
	"os"

	"github.com/dpsoft/perf-agent/internal/perfdata"
	"github.com/dpsoft/perf-agent/internal/perfevent"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// mmapEventSourceCloser is the local-to-dwarfagent interface that both
// *ehmaps.MmapWatcher and *ehmaps.MultiCPUMmapWatcher satisfy. Used by
// session.watcher as well.
type mmapEventSourceCloser interface {
	Events() <-chan ehmaps.MmapEventRecord
	Close() error
}

// Mode controls the CFI install policy for dwarfagent.Profiler.
type Mode int

const (
	// ModeEager compiles CFI for every binary visible at startup
	// (systemWide: AttachAllProcesses; per-PID: AttachAllMappings).
	// Selected by --unwind dwarf. Today's behavior.
	ModeEager Mode = iota

	// ModeLazy populates pid_mappings only at startup; CFI compile
	// is deferred to the first BPF→userspace miss notification.
	// Selected by --unwind auto with -a (system-wide). For --pid N
	// ModeLazy falls back to ModeEager (compile cost is already
	// negligible per bench data).
	ModeLazy
)

// Profiler is the DWARF-capable CPU profiler. Same public shape as
// profile.Profiler (Collect / CollectAndWrite / Close) so
// perfagent.Agent can swap on --unwind. Most heavy lifting lives in
// the embedded *session — Profiler only adds the per-CPU perf-event Set.
type Profiler struct {
	*session
	sampleRate int
	perfSet    *perfevent.Set
}

// NewProfilerWithMode is the variant of NewProfiler that accepts both
// an optional Hooks struct and a Mode. Pass ModeEager + nil hooks for
// the same behavior as NewProfiler.
//
// eventSpec selects the perf-event source. Pass nil to default to software
// cpu-clock at sampleRate Hz; when non-nil, sampleRate is ignored. Used by
// the agent to keep perf_event_open and perf.data attr in sync.
func NewProfilerWithMode(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks, mode Mode, labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec) (*Profiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	// Per-PID always eager regardless of caller-passed mode (per spec).
	if !systemWide {
		mode = ModeEager
	}
	objs, err := profile.LoadPerfDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent", hooks, mode, labels, perfData)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	spec := perfevent.EventSpec{
		Type:         perfevent.PerfTypeSoftware,
		Config:       perfevent.PerfCountSWCPUClock,
		SamplePeriod: uint64(sampleRate),
		Frequency:    true,
	}
	if eventSpec != nil {
		spec = *eventSpec
	}
	perfSet, err := perfevent.OpenAll(objs.Program(), cpus, spec, perfevent.WithDeferredEnable())
	if err != nil {
		_ = sess.close()
		return nil, err
	}
	p := &Profiler{session: sess, sampleRate: sampleRate, perfSet: perfSet}

	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateCPUSample)

	if mode == ModeLazy {
		sess.drainerWG.Go(sess.consumeCFIMisses)
	}

	return p, nil
}

// NewProfilerWithHooks is the ModeEager variant of NewProfilerWithMode.
// Pass nil for labels when no per-sample static labels are needed.
func NewProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks, labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec) (*Profiler, error) {
	return NewProfilerWithMode(pid, systemWide, cpus, tags, sampleRate, hooks, ModeEager, labels, perfData, eventSpec)
}

// NewProfiler loads the perf_dwarf BPF program, wires ehmaps via
// newSession, opens per-CPU perf events at sampleRate Hz, attaches
// the BPF program to each, and starts the ringbuf reader + tracker
// goroutines.
//
// On error, every resource created is closed before returning.
// Callers should NOT call Close on a Profiler they received as (nil, err).
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, labels map[string]string, perfData *perfdata.Writer, eventSpec *perfevent.EventSpec) (*Profiler, error) {
	return NewProfilerWithHooks(pid, systemWide, cpus, tags, sampleRate, nil, labels, perfData, eventSpec)
}

// aggregateCPUSample is the CPU-specific ringbuf aggregator: each
// sample counts once; blocking-ns isn't meaningful here. Wrapped as a
// free function so consumeRingbuf can pass it generically.
func aggregateCPUSample(s *session, sample Sample) {
	key := sampleKey{pid: sample.PID, hash: hashPCs(sample.PCs)}
	s.mu.Lock()
	s.samples[key]++
	s.stashStack(key, sample.PCs)
	s.mu.Unlock()
	if s.perfData != nil && len(sample.PCs) > 0 {
		s.perfData.AddSample(perfdata.SampleRecord{
			IP:        sample.PCs[0],
			Pid:       sample.PID,
			Tid:       sample.TID,
			Time:      sample.TimeNs,
			Period:    1,
			Callchain: sample.PCs,
		})
	}
}

// Collect writes a gzipped pprof to w. Output is SampleTypeCpu with
// count-weighted samples.
func (p *Profiler) Collect(w io.Writer) error {
	return p.collect(w, pprof.SampleTypeCpu, p.sampleRate)
}

// CollectAndWrite is a file-path convenience wrapper.
func (p *Profiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return p.Collect(f)
}

// Close closes the per-CPU perf-event Set, then delegates the rest to
// session.close (ringbuf reader, watcher, symbolizer, BPF handle).
// Idempotent at the stop-channel level.
func (p *Profiler) Close() error {
	_ = p.perfSet.Close()
	return p.close()
}

// AttachStats returns the (pidCount, binaryCount) recorded by newSession's
// initial AttachAllProcesses/AttachAllMappings call. For per-PID profilers,
// pidCount is always 1. For system-wide, pidCount is the number of distinct
// PIDs successfully scanned. binaryCount is the number of distinct binaries
// (by build-id) compiled into the BPF maps.
//
// Returns (0, 0) if the initial attach failed (the agent still ran in
// FP-only mode for unattached binaries).
func (p *Profiler) AttachStats() (pidCount, binaryCount int) {
	return p.attachStats.pidCount, p.attachStats.binaryCount
}

// MissStats returns a snapshot of the lazy-CFI drainer's lifetime
// counters. Returns the zero value in eager mode (drainer never spawned).
func (p *Profiler) MissStats() MissStats {
	return p.missCounters.snapshot()
}
