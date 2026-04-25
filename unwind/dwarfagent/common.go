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

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// sampleKey is "(pid, stack hash)" — we dedupe identical stacks
// userspace-side to avoid re-symbolizing the same N-PC chain N times.
// The hash collides at the theoretical FNV rate (not cryptographic);
// collisions conflate counts but don't miss samples.
type sampleKey struct {
	pid  uint32
	hash uint64
}

// sessionObjs is the narrow shape both profile.PerfDwarf and
// profile.OffCPUDwarf satisfy. Lets session work generically without
// importing both concrete types.
type sessionObjs interface {
	Close() error
	RingbufMap() *ebpf.Map
	CFIRulesMap() *ebpf.Map
	CFILengthsMap() *ebpf.Map
	CFIClassificationMap() *ebpf.Map
	CFIClassificationLengthsMap() *ebpf.Map
	PIDMappingsMap() *ebpf.Map
	PIDMappingLengthsMap() *ebpf.Map
}

// session owns everything both dwarfagent.Profiler and
// dwarfagent.OffCPUProfiler share: BPF handle, ehmaps lifecycle
// (TableStore/PIDTracker/MmapWatcher), ringbuf reader, symbolizer,
// shutdown coordination, and the sample aggregation maps.
//
// Each profiler type embeds *session and adds its own attach-specific
// state (per-CPU perf_event fds + links for CPU; one tp_btf link for
// off-CPU) plus a consume-callback that controls how Sample.Value
// aggregates (count++ vs sum+=).
type session struct {
	pid  int
	tags []string

	objs       sessionObjs
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    mmapEventSourceCloser
	ringReader *ringbuf.Reader
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64
	stacks  map[sampleKey][]uint64
}

// newSession wires up everything shared after BPF is loaded: ehmaps,
// initial attach, MmapWatcher, ringbuf reader, blazesym symbolizer.
// Does NOT start any goroutines, attach the BPF program, or call
// AddPID — those are caller-specific (CPU vs off-CPU differ).
//
// On error, every resource newSession allocated is closed. Caller's
// BPF-handle `objs` is NOT closed on error — caller remains responsible
// for it, so its defer-close pattern still works.
func newSession(objs sessionObjs, pid int, systemWide bool, cpus []uint, tags []string, logPrefix string) (*session, error) {
	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())

	// Eager-compile is best-effort. Binaries without .eh_frame (Go,
	// stripped musl statics) intentionally fail here; the hybrid walker
	// still handles FP-safe code at runtime, and MmapWatcher will retry
	// per-binary on dlopen. Log the error and continue rather than refusing
	// to start.
	if systemWide {
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			log.Printf("%s: AttachAllProcesses: %v (continuing; walker uses FP path for unattached binaries)", logPrefix, err)
		} else {
			log.Printf("%s: attached %d distinct binaries across %d PIDs", logPrefix, nTables, nPIDs)
		}
	} else {
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			log.Printf("%s: AttachAllMappings(pid=%d): %v (continuing; walker uses FP path for unattached binaries)", logPrefix, pid, err)
		} else {
			log.Printf("%s: attached %d binaries from /proc/%d/maps", logPrefix, n, pid)
		}
	}

	var watcher mmapEventSourceCloser
	if systemWide {
		cpuInts := make([]int, 0, len(cpus))
		for _, c := range cpus {
			cpuInts = append(cpuInts, int(c))
		}
		mw, err := ehmaps.NewMultiCPUMmapWatcher(cpuInts)
		if err != nil {
			return nil, fmt.Errorf("multi-cpu mmap watcher: %w", err)
		}
		watcher = mw
	} else {
		w, err := ehmaps.NewMmapWatcher(uint32(pid))
		if err != nil {
			return nil, fmt.Errorf("mmap watcher: %w", err)
		}
		watcher = w
	}

	rd, err := ringbuf.NewReader(objs.RingbufMap())
	if err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("ringbuf reader: %w", err)
	}

	symbolizer, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		_ = rd.Close()
		_ = watcher.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	return &session{
		pid:        pid,
		tags:       tags,
		objs:       objs,
		store:      store,
		tracker:    tracker,
		watcher:    watcher,
		ringReader: rd,
		symbolizer: symbolizer,
		resolver:   procmap.NewResolver(),
		stop:       make(chan struct{}),
		samples:    map[sampleKey]uint64{},
	}, nil
}

// runTracker starts the background goroutine that consumes mmap events
// from the watcher and feeds them to the PIDTracker. Call exactly once
// after newSession returns.
func (s *session) runTracker() {
	s.trackerWG.Add(1)
	go func() {
		defer s.trackerWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-s.stop
			cancel()
		}()

		observer := func(ev ehmaps.MmapEventRecord) {
			switch ev.Kind {
			case ehmaps.MmapEvent:
				s.resolver.InvalidateAddr(ev.PID, ev.Addr)
			case ehmaps.ExitEvent:
				if ev.TID == ev.PID {
					s.resolver.Invalidate(ev.PID)
				}
			}
		}
		s.tracker.Run(ctx, s.watcher, observer)
	}()
}

// aggregator is the per-sample callback used by consumeRingbuf.
// CPU passes a function that does `s.samples[key]++`; off-CPU passes a
// function that does `s.samples[key] += sample.Value`.
type aggregator func(s *session, sample Sample)

// consumeRingbuf runs the ringbuf reader loop until s.stop fires or
// the reader returns ErrClosed. Must be called exactly once in a
// goroutine after newSession + runTracker. Caller is responsible for
// s.readerWG.Add(1) BEFORE spawning the goroutine.
func (s *session) consumeRingbuf(agg aggregator) {
	defer s.readerWG.Done()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.ringReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		rec, err := s.ringReader.Read()
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
		sample, err := parseSample(rec.RawSample)
		if err != nil {
			log.Printf("dwarfagent: parseSample: %v", err)
			continue
		}
		if len(sample.PCs) == 0 {
			continue
		}
		agg(s, sample)
	}
}

// stashStack stores the PC chain for key if not already stashed.
// Must be called under s.mu.
func (s *session) stashStack(key sampleKey, pcs []uint64) {
	if s.stacks == nil {
		s.stacks = map[sampleKey][]uint64{}
	}
	if _, have := s.stacks[key]; !have {
		s.stacks[key] = append([]uint64(nil), pcs...)
	}
}

// collect drains accumulated samples, symbolizes them, and writes a
// gzipped pprof to w. sampleType distinguishes CPU vs off-CPU in the
// output. sampleRate is passed through to pprof builders (off-CPU can
// pass 0).
func (s *session) collect(w io.Writer, sampleType pprof.SampleType, sampleRate int) error {
	s.mu.Lock()
	samples := make(map[sampleKey]uint64, len(s.samples))
	stacks := make(map[sampleKey][]uint64, len(s.stacks))
	for k, v := range s.samples {
		samples[k] = v
	}
	for k, v := range s.stacks {
		stacks[k] = v
	}
	s.mu.Unlock()

	if len(samples) == 0 {
		log.Println("dwarfagent: no samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    int64(sampleRate),
		PerPIDProfile: false,
		Comments:      s.tags,
		Resolver:      s.resolver,
	})
	for key, val := range samples {
		frames := symbolizePID(s.symbolizer, key.pid, stacks[key])
		sample := pprof.ProfileSample{
			Pid:         key.pid,
			SampleType:  sampleType,
			Aggregation: pprof.SampleAggregated,
			Stack:       frames,
			Value:       val,
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

// close tears down shared resources in the correct order: signal stop,
// wait reader goroutine, close reader, close watcher (which closes the
// tracker's event feed), wait tracker goroutine, close symbolizer, close
// BPF objects. Each profiler type wraps this with its own attach-link
// cleanup before delegating here.
//
// Idempotent at the stop-channel level. Returns the first non-nil error
// encountered; subsequent close calls still execute.
func (s *session) close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.readerWG.Wait()
	_ = s.ringReader.Close()
	_ = s.watcher.Close()
	s.trackerWG.Wait()
	if s.resolver != nil {
		s.resolver.Close()
	}
	if s.symbolizer != nil {
		s.symbolizer.Close()
	}
	return s.objs.Close()
}

// hashPCs is a stable, fast, collision-rare hash over a PC chain —
// FNV-1a byte-wise over all u64s. Used to dedupe identical stacks
// userspace-side so we don't re-symbolize the same chain on every
// sample. Collisions conflate counts but don't miss samples.
func hashPCs(pcs []uint64) uint64 {
	const (
		offset uint64 = 0xcbf29ce484222325
		prime  uint64 = 0x100000001b3
	)
	h := offset
	for _, pc := range pcs {
		for shift := uint(0); shift < 64; shift += 8 {
			h ^= (pc >> shift) & 0xff
			h *= prime
		}
	}
	return h
}
