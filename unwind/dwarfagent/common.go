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

	"github.com/dpsoft/perf-agent/internal/bpfstack"
	"github.com/dpsoft/perf-agent/internal/perfdata"
	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
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
	KernStackmap() *ebpf.Map
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
	pid    int
	tags   []string
	labels map[string]string

	objs             sessionObjs
	store            *ehmaps.TableStore
	tracker          *ehmaps.PIDTracker
	watcher          mmapEventSourceCloser
	ringReader       *ringbuf.Reader
	symbolizer       symbolize.Symbolizer
	kernelSymbolizer symbolize.KernelSymbolizer
	resolver         *procmap.Resolver

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu         sync.Mutex
	samples    map[sampleKey]uint64
	stacks     map[sampleKey][]uint64
	kernStacks map[sampleKey][]uint64 // kernel IPs per sample key; nil when kernel-stacks disabled

	attachStats attachStats

	// Lazy CFI (mode == ModeLazy) state. Zero-valued in eager modes.
	// missReader is nil when not in lazy mode; the consumeCFIMisses
	// goroutine is only spawned when missReader != nil.
	missReader   *ringbuf.Reader
	drainerWG    sync.WaitGroup
	missCounters missCounters

	perfData *perfdata.Writer // optional, nil when --perf-data-output not set
}

// attachStats records the (pidCount, binaryCount) returned by the
// initial AttachAll{Processes,Mappings} call in newSession. Exposed
// via Profiler.AttachStats / OffCPUProfiler.AttachStats. Zero on
// attach failure (the agent still ran in FP-only mode for unattached
// binaries).
type attachStats struct {
	pidCount    int
	binaryCount int
}

// newSession wires up everything shared after BPF is loaded: ehmaps,
// initial attach, MmapWatcher, ringbuf reader, blazesym symbolizer.
// Does NOT start any goroutines, attach the BPF program, or call
// AddPID — those are caller-specific (CPU vs off-CPU differ).
//
// kernelSym resolves kernel-mode addresses for samples that carry a
// valid BPF kernel stack-ID. Callers pass symbolize.NoopKernelSymbolizer{}
// to disable kernel symbolization (the BPF gate
// kernel_stacks_enabled=false makes the userspace lookup a dead branch
// anyway). The session does NOT close the kernel symbolizer — its
// lifecycle is owned by the Agent, same as the user-mode Symbolizer.
//
// On error, every resource newSession allocated is closed. Caller's
// BPF-handle `objs` is NOT closed on error — caller remains responsible
// for it, so its defer-close pattern still works.
func newSession(objs sessionObjs, pid int, systemWide bool, cpus []uint, tags []string, logPrefix string, hooks *Hooks, mode Mode, labels map[string]string, perfData *perfdata.Writer, sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer) (*session, error) {
	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	if hooks != nil && hooks.OnCompile != nil {
		store.SetOnCompile(hooks.onCompileFunc())
	}
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())
	if hooks != nil && hooks.OnNewExec != nil {
		tracker.SetOnNewExec(hooks.OnNewExec)
	}

	// Attach strategy depends on mode:
	//   ModeLazy + systemWide: ScanAndEnroll (pid_mappings only, no CFI compile).
	//   ModeEager + systemWide: AttachAllProcesses (compile CFI eagerly).
	//   per-PID (always eager): AttachAllMappings.
	// All paths are best-effort; failures fall through to FP-only mode.
	var stats attachStats
	switch {
	case mode == ModeLazy && systemWide:
		nPIDs, nTables, err := ehmaps.ScanAndEnroll(tracker)
		if err != nil {
			log.Printf("%s: ScanAndEnroll: %v (falling back to eager)", logPrefix, err)
			// Fallback: today's eager path
			nPIDs, nTables, err = ehmaps.AttachAllProcesses(tracker)
			if err != nil {
				log.Printf("%s: AttachAllProcesses fallback: %v (continuing FP-only)", logPrefix, err)
			}
		}
		log.Printf("%s: enrolled %d binaries across %d PIDs (lazy)", logPrefix, nTables, nPIDs)
		stats.pidCount = nPIDs
		stats.binaryCount = nTables

	case systemWide:
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			log.Printf("%s: AttachAllProcesses: %v (continuing; walker uses FP path for unattached binaries)", logPrefix, err)
		} else {
			log.Printf("%s: attached %d distinct binaries across %d PIDs", logPrefix, nTables, nPIDs)
			stats.pidCount = nPIDs
			stats.binaryCount = nTables
		}

	default:
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			log.Printf("%s: AttachAllMappings(pid=%d): %v (continuing; walker uses FP path for unattached binaries)", logPrefix, pid, err)
		} else {
			log.Printf("%s: attached %d binaries from /proc/%d/maps", logPrefix, n, pid)
			stats.pidCount = 1
			stats.binaryCount = n
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

	var missReader *ringbuf.Reader
	if mode == ModeLazy {
		// Type-assert sessionObjs to access the CFIMissRingbuf accessor
		// added in Task 2. Only PerfDwarf implements it; OffCPUDwarf does not.
		type missMapAccessor interface {
			CFIMissRingbuf() *ebpf.Map
		}
		if a, ok := objs.(missMapAccessor); ok && a.CFIMissRingbuf() != nil {
			mr, err := ringbuf.NewReader(a.CFIMissRingbuf())
			if err != nil {
				_ = rd.Close()
				_ = watcher.Close()
				return nil, fmt.Errorf("cfi miss ringbuf reader: %w", err)
			}
			missReader = mr
		}
	}

	return &session{
		pid:              pid,
		tags:             tags,
		labels:           labels,
		objs:             objs,
		store:            store,
		tracker:          tracker,
		watcher:          watcher,
		ringReader:       rd,
		missReader:       missReader,
		symbolizer:       sym,
		kernelSymbolizer: kernelSym,
		resolver:         procmap.NewResolver(),
		stop:             make(chan struct{}),
		samples:          map[sampleKey]uint64{},
		attachStats:      stats,
		perfData:         perfData,
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
// function that does `s.samples[key] += sample.Value`. kernelIPs holds
// the resolved kernel-mode IPs (may be empty if --kernel-stacks is off
// or the stack-ID went stale); the aggregator stashes them via
// stashKernelStack so collect() can symbolize them later.
type aggregator func(s *session, sample Sample, kernelIPs []uint64)

// consumeRingbuf runs the ringbuf reader loop until s.stop fires or
// the reader returns ErrClosed. Must be called exactly once in a
// goroutine after newSession + runTracker. Caller is responsible for
// s.readerWG.Add(1) BEFORE spawning the goroutine.
//
// For each sample with a valid BPF kernel stack-ID (sample.KernStack >= 0),
// the loop reads the IPs out of the per-program kern_stackmap and stashes
// them in s.kernStacks keyed by the same sampleKey as the user PCs. The
// stash happens BEFORE the per-mode aggregator (CPU vs off-CPU) runs so
// it sees the kernel IPs alongside the user PC chain. When the BPF gate
// is off the field is -1 and we skip the lookup entirely.
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
		// Kernel-stack lookup gated on the BPF-set stack-ID. The
		// kern_stackmap is shared across all CPUs / samples; the
		// stack-ID identifies a unique IP chain inside the kernel
		// FNV-hashed buffer. Lookup failures (stale ID, map evict)
		// are logged at debug level and fall through to no-kernel.
		var kernelIPs []uint64
		if sample.KernStack >= 0 {
			if kernBytes, err := s.objs.KernStackmap().LookupBytes(uint32(sample.KernStack)); err == nil {
				kernelIPs = bpfstack.ExtractIPs(kernBytes)
			}
		}
		agg(s, sample, kernelIPs)
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

// stashKernelStack stores the kernel IP chain for key if not already
// stashed. No-op when kernelIPs is empty (the common case when kernel
// stacks are disabled or the BPF gate didn't fire). Must be called
// under s.mu.
func (s *session) stashKernelStack(key sampleKey, kernelIPs []uint64) {
	if len(kernelIPs) == 0 {
		return
	}
	if s.kernStacks == nil {
		s.kernStacks = map[sampleKey][]uint64{}
	}
	if _, have := s.kernStacks[key]; !have {
		s.kernStacks[key] = append([]uint64(nil), kernelIPs...)
	}
}

// collect drains accumulated samples, symbolizes them, and writes a
// gzipped pprof to w. sampleType distinguishes CPU vs off-CPU in the
// output. sampleRate is passed through to pprof builders (off-CPU can
// pass 0).
//
// Kernel frames (if --kernel-stacks is on and the stack-ID was valid
// when consumeRingbuf ran) are symbolized via s.kernelSymbolizer and
// merged leaf-first with the user frames — matching the FP profiler's
// shape so pprof Reverse() yields root→kernel→user.
func (s *session) collect(w io.Writer, sampleType pprof.SampleType, sampleRate int) error {
	s.mu.Lock()
	samples := make(map[sampleKey]uint64, len(s.samples))
	stacks := make(map[sampleKey][]uint64, len(s.stacks))
	kernStacks := make(map[sampleKey][]uint64, len(s.kernStacks))
	for k, v := range s.samples {
		samples[k] = v
	}
	for k, v := range s.stacks {
		stacks[k] = v
	}
	for k, v := range s.kernStacks {
		kernStacks[k] = v
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
		Labels:        s.labels,
	})
	for key, val := range samples {
		frames := symbolizePIDWithKernel(s.symbolizer, s.kernelSymbolizer, key.pid, stacks[key], kernStacks[key])
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
	s.drainerWG.Wait()
	_ = s.ringReader.Close()
	if s.missReader != nil {
		_ = s.missReader.Close()
	}
	_ = s.watcher.Close()
	s.trackerWG.Wait()
	if s.resolver != nil {
		s.resolver.Close()
	}
	// Symbolizer is owned by the Agent; do not close it here.
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
