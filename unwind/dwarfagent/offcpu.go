package dwarfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// OffCPUProfiler is the DWARF-capable off-CPU profiler. Same public
// shape as offcpu.Profiler — Collect / CollectAndWrite / Close — so
// perfagent.Agent can swap between the two on --unwind.
type OffCPUProfiler struct {
	pid  int
	tags []string

	objs       *profile.OffCPUDwarf
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    *ehmaps.MmapWatcher
	link       link.Link
	ringReader *ringbuf.Reader

	symbolizer *blazesym.Symbolizer

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64 // value = accumulated blocking-ns
	stacks  map[sampleKey][]uint64
}

// NewOffCPUProfiler loads the offcpu_dwarf BPF program, primes the
// ehmaps lifecycle (CFI/pid_mappings), attaches the tp_btf program,
// and starts the ringbuf reader + tracker goroutines.
//
// On error, every resource created up to the failure point is closed
// before returning. Callers should NOT call Close on a Profiler they
// received as (nil, err).
func NewOffCPUProfiler(pid int, tags []string) (*OffCPUProfiler, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 (system-wide is S7 scope)")
	}
	objs, err := profile.LoadOffCPUDwarf(false)
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf: %w", err)
	}
	if err := objs.AddPID(uint32(pid)); err != nil {
		objs.Close()
		return nil, fmt.Errorf("add pid to filter: %w", err)
	}

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())

	nAttached, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("attach initial mappings: %w", err)
	}
	log.Printf("dwarfagent (offcpu): attached %d binaries from /proc/%d/maps", nAttached, pid)

	watcher, err := ehmaps.NewMmapWatcher(uint32(pid))
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("mmap watcher: %w", err)
	}

	tpLink, err := link.AttachTracing(link.TracingOptions{
		Program: objs.Program(),
	})
	if err != nil {
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("attach tp_btf: %w", err)
	}

	rd, err := ringbuf.NewReader(objs.RingbufMap())
	if err != nil {
		tpLink.Close()
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
		tpLink.Close()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	p := &OffCPUProfiler{
		pid:        pid,
		tags:       tags,
		objs:       objs,
		store:      store,
		tracker:    tracker,
		watcher:    watcher,
		link:       tpLink,
		ringReader: rd,
		symbolizer: symbolizer,
		stop:       make(chan struct{}),
		samples:    map[sampleKey]uint64{},
	}

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

	p.readerWG.Add(1)
	go p.consume()

	return p, nil
}

// consume is the ringbuf reader goroutine. Adds Sample.Value (blocking-ns)
// into the aggregated total for each (pid, stack) key.
func (p *OffCPUProfiler) consume() {
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
			log.Printf("dwarfagent (offcpu): ringbuf read: %v", err)
			return
		}
		s, err := parseSample(rec.RawSample)
		if err != nil {
			log.Printf("dwarfagent (offcpu): parseSample: %v", err)
			continue
		}
		if len(s.PCs) == 0 || s.Value == 0 {
			continue
		}
		key := sampleKey{pid: s.PID, hash: hashPCs(s.PCs)}
		p.mu.Lock()
		p.samples[key] += s.Value
		if p.stacks == nil {
			p.stacks = map[sampleKey][]uint64{}
		}
		if _, have := p.stacks[key]; !have {
			p.stacks[key] = append([]uint64(nil), s.PCs...)
		}
		p.mu.Unlock()
	}
}

// Collect symbolizes accumulated off-CPU samples and writes a gzipped
// pprof to w. SampleType is off-CPU; Value is accumulated blocking-ns.
func (p *OffCPUProfiler) Collect(w io.Writer) error {
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
		log.Println("dwarfagent (offcpu): no samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		PerPIDProfile: false,
		Comments:      p.tags,
	})

	for key, totalNs := range samples {
		frames := symbolizePID(p.symbolizer, key.pid, stacks[key])
		sample := pprof.ProfileSample{
			Pid:         key.pid,
			SampleType:  pprof.SampleTypeOffCpu,
			Aggregation: pprof.SampleAggregated,
			Stack:       frames,
			Value:       totalNs,
		}
		builders.AddSample(&sample)
	}

	for _, b := range builders.Builders {
		if _, err := b.Write(w); err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break
	}
	return nil
}

// CollectAndWrite is a convenience wrapper for file output.
func (p *OffCPUProfiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close stops goroutines and releases all resources. Idempotent at the
// stop-channel level. Waits for goroutines to exit before unmapping
// so an in-flight drain can't fault on freed memory.
func (p *OffCPUProfiler) Close() error {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.readerWG.Wait()
	p.ringReader.Close()
	p.watcher.Close()
	p.trackerWG.Wait()
	if p.link != nil {
		_ = p.link.Close()
	}
	if p.symbolizer != nil {
		p.symbolizer.Close()
	}
	return p.objs.Close()
}
