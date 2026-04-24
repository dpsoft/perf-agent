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

// NewOffCPUProfiler loads the offcpu_dwarf BPF program, wires ehmaps
// via newSession, attaches the tp_btf program via link.AttachTracing,
// and starts the ringbuf reader + tracker goroutines.
//
// On error, every resource created is closed before returning.
// Callers should NOT call Close on an OffCPUProfiler they received
// as (nil, err).
func NewOffCPUProfiler(pid int, systemWide bool, cpus []uint, tags []string) (*OffCPUProfiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadOffCPUDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent (offcpu)")
	if err != nil {
		objs.Close()
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

	return p, nil
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
	return p.session.collect(w, pprof.SampleTypeOffCpu, 0)
}

// CollectAndWrite is a file-path convenience wrapper.
func (p *OffCPUProfiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close closes the tp_btf link, then delegates the rest to
// session.close. Idempotent.
func (p *OffCPUProfiler) Close() error {
	if p.link != nil {
		_ = p.link.Close()
		p.link = nil
	}
	return p.session.close()
}
