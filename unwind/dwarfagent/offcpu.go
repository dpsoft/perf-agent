package dwarfagent

import (
	"fmt"
	"io"
	"os"

	"github.com/cilium/ebpf/link"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
)

// OffCPUProfiler is the DWARF-capable off-CPU profiler. Same public
// shape as offcpu.Profiler (Collect / CollectAndWrite / Close). Most
// lifecycle lives in the embedded *session; OffCPUProfiler only adds
// the single tp_btf link.
type OffCPUProfiler struct {
	*session
	link link.Link
}

// NewOffCPUProfilerWithMode is the variant of NewOffCPUProfiler that accepts both
// an optional Hooks struct and a Mode. Pass ModeEager + nil hooks for
// the same behavior as NewOffCPUProfiler.
func NewOffCPUProfilerWithMode(pid int, systemWide bool, cpus []uint, tags []string, hooks *Hooks, mode Mode) (*OffCPUProfiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	// Per-PID always eager regardless of caller-passed mode (per spec).
	if !systemWide {
		mode = ModeEager
	}
	objs, err := profile.LoadOffCPUDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			_ = objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent (offcpu)", hooks, mode)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	tpLink, err := link.AttachTracing(link.TracingOptions{Program: objs.Program()})
	if err != nil {
		_ = sess.close()
		return nil, fmt.Errorf("attach tp_btf: %w", err)
	}

	p := &OffCPUProfiler{session: sess, link: tpLink}
	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateOffCPUSample)

	if mode == ModeLazy {
		sess.drainerWG.Go(sess.consumeCFIMisses)
	}

	return p, nil
}

// NewOffCPUProfilerWithHooks is the variant of NewOffCPUProfiler that
// accepts an optional observation surface. Pass nil hooks for the same
// behavior as NewOffCPUProfiler.
func NewOffCPUProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, hooks *Hooks) (*OffCPUProfiler, error) {
	return NewOffCPUProfilerWithMode(pid, systemWide, cpus, tags, hooks, ModeEager)
}

// NewOffCPUProfiler loads the offcpu_dwarf BPF program, wires ehmaps
// via newSession, attaches the tp_btf program via link.AttachTracing,
// and starts the ringbuf reader + tracker goroutines.
//
// On error, every resource created is closed before returning.
// Callers should NOT call Close on an OffCPUProfiler they received
// as (nil, err).
func NewOffCPUProfiler(pid int, systemWide bool, cpus []uint, tags []string) (*OffCPUProfiler, error) {
	return NewOffCPUProfilerWithHooks(pid, systemWide, cpus, tags, nil)
}

// aggregateOffCPUSample is the off-CPU-specific ringbuf aggregator:
// samples that never saw a switch-IN (Value == 0) are skipped; valid
// samples add their blocking-ns into the accumulator.
func aggregateOffCPUSample(s *session, sample Sample) {
	if sample.Value == 0 {
		return
	}
	key := sampleKey{pid: sample.PID, hash: hashPCs(sample.PCs)}
	s.mu.Lock()
	s.samples[key] += sample.Value
	s.stashStack(key, sample.PCs)
	s.mu.Unlock()
}

// Collect writes a gzipped pprof to w. SampleType is off-CPU; sample
// values are accumulated blocking-ns.
func (p *OffCPUProfiler) Collect(w io.Writer) error {
	return p.collect(w, pprof.SampleTypeOffCpu, 0)
}

// CollectAndWrite is a file-path convenience wrapper.
func (p *OffCPUProfiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return p.Collect(f)
}

// Close closes the tp_btf link, then delegates the rest to
// session.close. Idempotent.
func (p *OffCPUProfiler) Close() error {
	if p.link != nil {
		_ = p.link.Close()
		p.link = nil
	}
	return p.close()
}

// AttachStats returns the (pidCount, binaryCount) recorded by newSession's
// initial AttachAllProcesses/AttachAllMappings call. See
// (*Profiler).AttachStats for full semantics.
func (p *OffCPUProfiler) AttachStats() (pidCount, binaryCount int) {
	return p.attachStats.pidCount, p.attachStats.binaryCount
}
