package dwarfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// mmapEventSourceCloser is the local-to-dwarfagent interface that both
// *ehmaps.MmapWatcher and *ehmaps.MultiCPUMmapWatcher satisfy, letting
// us store either concrete type in the Profiler struct and close it
// uniformly.
type mmapEventSourceCloser interface {
	Events() <-chan ehmaps.MmapEventRecord
	Close() error
}

// Profiler is the DWARF-capable CPU profiler. It has the same public
// shape as profile.Profiler — Collect / CollectAndWrite / Close —
// so perfagent.Agent can swap between the two on --unwind.
type Profiler struct {
	pid        int
	sampleRate int
	tags       []string

	objs       *profile.PerfDwarf
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    mmapEventSourceCloser
	perfFDs    []int
	perfLinks  []link.Link
	ringReader *ringbuf.Reader

	symbolizer *blazesym.Symbolizer

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64
	stacks  map[sampleKey][]uint64 // stashed PC chain per key — lazy-init in stash()
}

// NewProfiler loads the BPF program, walks /proc/<pid>/maps to prime
// the ehmaps lifecycle, opens per-CPU perf events at sampleRate Hz,
// and starts the ringbuf reader + tracker goroutines.
//
// On error, every resource created up to the failure point is closed
// before returning. Callers should NOT call Close on a Profiler they
// received as (nil, err).
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadPerfDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())

	if systemWide {
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach all processes: %w", err)
		}
		log.Printf("dwarfagent: attached %d distinct binaries across %d PIDs", nTables, nPIDs)
	} else {
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach initial mappings: %w", err)
		}
		log.Printf("dwarfagent: attached %d binaries from /proc/%d/maps", n, pid)
	}

	var watcher mmapEventSourceCloser
	if systemWide {
		cpuInts := make([]int, 0, len(cpus))
		for _, c := range cpus {
			cpuInts = append(cpuInts, int(c))
		}
		mw, err := ehmaps.NewMultiCPUMmapWatcher(cpuInts)
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("multi-cpu mmap watcher: %w", err)
		}
		watcher = mw
	} else {
		w, err := ehmaps.NewMmapWatcher(uint32(pid))
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("mmap watcher: %w", err)
		}
		watcher = w
	}

	// Per-CPU perf events. pid=-1 + BPF-side pids filter = same pattern
	// as profile.Profiler in --unwind fp.
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: uint64(sampleRate),
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	perfFDs := make([]int, 0, len(cpus))
	perfLinks := make([]link.Link, 0, len(cpus))
	cleanupPerf := func() {
		for _, l := range perfLinks {
			_ = l.Close()
		}
		for _, fd := range perfFDs {
			_ = unix.Close(fd)
		}
	}
	for _, cpu := range cpus {
		fd, err := unix.PerfEventOpen(attr, -1, int(cpu), -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			cleanupPerf()
			watcher.Close()
			objs.Close()
			return nil, fmt.Errorf("perf_event_open cpu=%d: %w", cpu, err)
		}
		perfFDs = append(perfFDs, fd)
		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target:  fd,
			Program: objs.Program(),
			Attach:  ebpf.AttachPerfEvent,
		})
		if err != nil {
			cleanupPerf()
			watcher.Close()
			objs.Close()
			return nil, fmt.Errorf("attach perf event cpu=%d: %w", cpu, err)
		}
		perfLinks = append(perfLinks, rl)
		if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
			cleanupPerf()
			watcher.Close()
			objs.Close()
			return nil, fmt.Errorf("enable perf event cpu=%d: %w", cpu, err)
		}
	}
	if len(perfFDs) == 0 {
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("no perf events attached — pid %d may have exited", pid)
	}

	rd, err := ringbuf.NewReader(objs.RingbufMap())
	if err != nil {
		cleanupPerf()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("ringbuf reader: %w", err)
	}

	symbolizer, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		rd.Close()
		cleanupPerf()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	p := &Profiler{
		pid:        pid,
		sampleRate: sampleRate,
		tags:       tags,
		objs:       objs,
		store:      store,
		tracker:    tracker,
		watcher:    watcher,
		perfFDs:    perfFDs,
		perfLinks:  perfLinks,
		ringReader: rd,
		symbolizer: symbolizer,
		stop:       make(chan struct{}),
		samples:    map[sampleKey]uint64{},
	}

	// PIDTracker.Run consumes mmap events.
	p.trackerWG.Add(1)
	go func() {
		defer p.trackerWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-p.stop
			cancel()
		}()
		p.tracker.Run(ctx, p.watcher)
	}()

	// Ringbuf consumer.
	p.readerWG.Add(1)
	go p.consume()

	return p, nil
}

// consume is the ringbuf reader goroutine. Stops when p.stop fires
// (Close closes it) or when the reader returns ErrClosed.
func (p *Profiler) consume() {
	defer p.readerWG.Done()
	for {
		select {
		case <-p.stop:
			return
		default:
		}
		p.ringReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		rec, err := p.ringReader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			log.Printf("dwarfagent: ringbuf read: %v", err)
			return
		}
		s, err := parseSample(rec.RawSample)
		if err != nil {
			log.Printf("dwarfagent: parseSample: %v", err)
			continue
		}
		if len(s.PCs) == 0 {
			continue
		}
		key := sampleKey{pid: s.PID, hash: hashPCs(s.PCs)}
		p.mu.Lock()
		p.samples[key]++
		p.stash(key, s.PCs)
		p.mu.Unlock()
	}
}

// stash stores the PC chain for a given key if not already stashed.
// Called under p.mu.
func (p *Profiler) stash(key sampleKey, pcs []uint64) {
	if p.stacks == nil {
		p.stacks = map[sampleKey][]uint64{}
	}
	if _, have := p.stacks[key]; !have {
		p.stacks[key] = append([]uint64(nil), pcs...)
	}
}

// Collect drains accumulated samples, symbolizes them, and writes a
// gzipped pprof to w. Does NOT clear accumulated state — follow with
// Close to release BPF resources.
func (p *Profiler) Collect(w io.Writer) error {
	p.mu.Lock()
	samples := make(map[sampleKey]uint64, len(p.samples))
	stacks := make(map[sampleKey][]uint64, len(p.stacks))
	for k, v := range p.samples {
		samples[k] = v
	}
	for k, v := range p.stacks {
		stacks[k] = v
	}
	p.mu.Unlock()

	if len(samples) == 0 {
		log.Println("dwarfagent: no samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    int64(p.sampleRate),
		PerPIDProfile: false,
		Comments:      p.tags,
	})

	for key, count := range samples {
		pcs := stacks[key]
		frames := symbolizePID(p.symbolizer, key.pid, pcs)
		sample := pprof.ProfileSample{
			Pid:         key.pid,
			SampleType:  pprof.SampleTypeCpu,
			Aggregation: pprof.SampleAggregated,
			Stack:       frames,
			Value:       count,
		}
		builders.AddSample(&sample)
	}

	for _, b := range builders.Builders {
		if _, err := b.Write(w); err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break // single non-per-PID profile
	}
	return nil
}

// CollectAndWrite is a convenience wrapper for file output.
func (p *Profiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close stops all goroutines and releases BPF / perf / symbolizer
// resources. Idempotent at the channel level (a second Close is a no-op
// w.r.t. the stop channel but still safe to call).
func (p *Profiler) Close() error {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.readerWG.Wait()
	p.ringReader.Close()
	p.watcher.Close()
	p.trackerWG.Wait()
	for _, l := range p.perfLinks {
		_ = l.Close()
	}
	for _, fd := range p.perfFDs {
		_ = unix.Close(fd)
	}
	if p.symbolizer != nil {
		p.symbolizer.Close()
	}
	return p.objs.Close()
}
