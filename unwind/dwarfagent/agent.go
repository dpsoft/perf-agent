package dwarfagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"

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
// the embedded *session — Profiler only adds per-CPU perf_event +
// RawLink slices.
type Profiler struct {
	*session
	sampleRate int
	perfFDs    []int
	perfLinks  []link.Link
}

// NewProfilerWithMode is the variant of NewProfiler that accepts both
// an optional Hooks struct and a Mode. Pass ModeEager + nil hooks for
// the same behavior as NewProfiler.
func NewProfilerWithMode(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks, mode Mode) (*Profiler, error) {
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

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent", hooks, mode)
	if err != nil {
		_ = objs.Close()
		return nil, err
	}

	p := &Profiler{session: sess, sampleRate: sampleRate}
	if err := p.attachPerfEvents(objs.Program(), cpus, sampleRate); err != nil {
		_ = p.close()
		return nil, err
	}

	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateCPUSample)

	if mode == ModeLazy {
		sess.drainerWG.Add(1)
		go sess.consumeCFIMisses()
	}

	return p, nil
}

// NewProfilerWithHooks is the legacy variant that defaults to ModeEager.
// Existing callers (perfagent.Agent's --unwind dwarf path) work unchanged.
func NewProfilerWithHooks(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks) (*Profiler, error) {
	return NewProfilerWithMode(pid, systemWide, cpus, tags, sampleRate, hooks, ModeEager)
}

// NewProfiler loads the perf_dwarf BPF program, wires ehmaps via
// newSession, opens per-CPU perf events at sampleRate Hz, attaches
// the BPF program to each, and starts the ringbuf reader + tracker
// goroutines.
//
// On error, every resource created is closed before returning.
// Callers should NOT call Close on a Profiler they received as (nil, err).
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	return NewProfilerWithHooks(pid, systemWide, cpus, tags, sampleRate, nil)
}

// attachPerfEvents opens one perf_event_open per CPU (pid=-1, cpu=N,
// BPF pids-map filter) and link.AttachRawLinks the BPF program to each.
// Populates p.perfFDs + p.perfLinks for Close to tear down.
func (p *Profiler) attachPerfEvents(prog *ebpf.Program, cpus []uint, sampleRate int) error {
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: uint64(sampleRate),
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	cleanup := func() {
		for _, l := range p.perfLinks {
			_ = l.Close()
		}
		for _, fd := range p.perfFDs {
			_ = unix.Close(fd)
		}
		p.perfLinks = nil
		p.perfFDs = nil
	}
	for _, cpu := range cpus {
		fd, err := unix.PerfEventOpen(attr, -1, int(cpu), -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			cleanup()
			return fmt.Errorf("perf_event_open cpu=%d: %w", cpu, err)
		}
		p.perfFDs = append(p.perfFDs, fd)
		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target:  fd,
			Program: prog,
			Attach:  ebpf.AttachPerfEvent,
		})
		if err != nil {
			cleanup()
			return fmt.Errorf("attach perf event cpu=%d: %w", cpu, err)
		}
		p.perfLinks = append(p.perfLinks, rl)
		if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
			cleanup()
			return fmt.Errorf("enable perf event cpu=%d: %w", cpu, err)
		}
	}
	if len(p.perfFDs) == 0 {
		return fmt.Errorf("no perf events attached (pid=%d, cpus=%d)", p.pid, len(cpus))
	}
	return nil
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

// Close closes perf-event links + fds, then delegates the rest to
// session.close (ringbuf reader, watcher, symbolizer, BPF handle).
// Idempotent at the stop-channel level.
func (p *Profiler) Close() error {
	for _, l := range p.perfLinks {
		_ = l.Close()
	}
	for _, fd := range p.perfFDs {
		_ = unix.Close(fd)
	}
	p.perfLinks = nil
	p.perfFDs = nil
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
