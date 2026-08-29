package debuginfod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// Symbolizer resolves abs addresses against a process while consulting a
// debuginfod-protocol server for missing debug info. Implements
// symbolize.Symbolizer.
type Symbolizer struct {
	opts       Options
	cache      *cache.Cache
	fetcher    *fetcher
	sf         sfFetcher
	resolver   *procmap.Resolver
	classifier *classifier
	cgo        *cgoState
	stats      atomicStats
	closed     atomic.Bool
	inflight   sync.WaitGroup
}

// New constructs a Symbolizer from opts. opts.URLs must be non-empty.
func New(opts Options) (*Symbolizer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	idx, err := openIndex(filepath.Join(opts.CacheDir, indexFilename))
	if err != nil {
		return nil, err
	}
	c := &cache.Cache{
		Dir:      opts.CacheDir,
		Index:    idx,
		MaxBytes: opts.CacheMaxBytes,
	}
	if err := c.Prewarm(); err != nil {
		_ = c.Close()
		return nil, err
	}
	f := newFetcher(opts.URLs, opts.HTTPClient)
	sfConcrete := newSingleflightFetcher(f, c)

	s := &Symbolizer{
		opts:     opts,
		cache:    c,
		fetcher:  f,
		sf:       sfConcrete,
		resolver: opts.Resolver,
	}
	s.classifier = newClassifier(c, sfConcrete, &s.stats)
	st, err := newCgoState(s)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	// LocalSymbolizer fallback for the NULL-return case (locally-built
	// binaries with build-ids absent upstream, or targets whose
	// /proc/<pid>/exe is restricted). Construct lazily-tolerant:
	// failure to build the fallback is non-fatal — the dispatcher
	// just runs without it.
	// opts.Resolver doubles as the fallback's module index, so a frame the
	// fallback cannot name still says which library it is in. It may be nil,
	// in which case those frames stay bare addresses.
	if lf, lfErr := symbolize.NewLocalSymbolizer(symbolize.WithModuleIndex(moduleIndex(opts.Resolver))); lfErr == nil {
		st.localFallback = lf
	}
	s.cgo = st
	return s, nil
}

// moduleIndex adapts a possibly-nil *procmap.Resolver to symbolize.ModuleIndex
// without handing over a non-nil interface wrapping a nil pointer, which would
// panic on the first Lookup instead of being skipped.
func moduleIndex(r *procmap.Resolver) symbolize.ModuleIndex {
	if r == nil {
		return nil
	}
	return r
}

// SymbolizeProcess resolves abs IPs into Frames. Each address is routed
// per-mapping:
//
//   - skip (only triggered when classify() returns routeSkip — currently
//     rare in practice because procmap.Resolver's parser already filters
//     out pseudo-files ([vdso], [stack], [vsyscall], [heap]) and anonymous
//     ranges upstream; IPs in those regions hit "no mapping found" and fall
//     through to process-mode instead) → empty Frame with the original
//     address so the pprof location's mapping fallback can name it
//   - process-mode → blazesym's default abs-addr API, with the dispatcher
//     hook handling on-demand fetches of missing executables
//   - file-mode → AddressMapper translates each IP into a file-VA against
//     the cached .debug, then blazesym's elf-virt API resolves the file-VA
//     and the result's Address is rewritten back to the original IP
//
// Mappings come from procmap.Resolver, whose snapshot excludes pseudo-files
// such as [vdso]/[stack] and anonymous ranges. IPs in those regions
// therefore do not hit a separate skip route here; when no mapping is
// found, they fall back to process-mode.
//
// Failure modes are graceful: a missing AddressMapper translation demotes
// the individual IP to process-mode; a parse failure of the whole .debug
// marks the file in badDebug and routes the bucket through process-mode;
// resolver failure (or no Resolver configured) falls back to pure
// process-mode for the entire batch. Stack shape is preserved — no frames
// are dropped — so pprof's mapping resolver can still attach mapping
// names to every location.
func (s *Symbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if len(ips) == 0 {
		return nil, nil
	}

	callStart := time.Now()
	defer func() {
		d := time.Since(callStart)
		ns := uint64(d.Nanoseconds())
		s.stats.calls.Add(1)
		s.stats.totalNs.Add(ns)
		if d > 50*time.Millisecond {
			s.stats.callsOver50ms.Add(1)
		}
		for {
			prev := s.stats.slowestNs.Load()
			if ns <= prev || s.stats.slowestNs.CompareAndSwap(prev, ns) {
				break
			}
		}
	}()

	ctx := context.Background()
	if s.opts.FetchTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.FetchTimeout)
		defer cancel()
	}

	// No resolver configured (or it failed): fall back to pure
	// process-mode for the entire batch. The dispatcher hook still
	// handles on-demand fetches for build-id-only mappings.
	if s.resolver == nil {
		s.stats.classifyProcessMode.Add(uint64(len(ips)))
		t := time.Now()
		defer func() { s.stats.symbolizeNs.Add(uint64(time.Since(t).Nanoseconds())) }()
		return s.cgo.symbolizeProcess(pid, ips)
	}

	// Spec invariant: each Symbolize call must re-snapshot /proc/<pid>/maps
	// (docs/specs/2026-05-12-debuginfod-cache-layout-design.md — "No
	// persistent per-PID state"). Without this Invalidate, Mappings is
	// sync.Once-cached: a dlopen/mmap/exec that extends the address space
	// after the first call would leave new mappings invisible, causing IPs
	// in those regions to fall through findMapping with no hit and be
	// silently misrouted to process-mode against the wrong (or no) mapping.
	mapStart := time.Now()
	s.resolver.Invalidate(pid)
	mappings, err := s.resolver.Mappings(pid)
	s.stats.mappingsNs.Add(uint64(time.Since(mapStart).Nanoseconds()))
	if err != nil || len(mappings) == 0 {
		s.stats.classifyProcessMode.Add(uint64(len(ips)))
		return s.cgo.symbolizeProcess(pid, ips)
	}

	symStart := time.Now()
	frames, rerr := s.routeAndSymbolize(ctx, pid, ips, mappings)
	s.stats.symbolizeNs.Add(uint64(time.Since(symStart).Nanoseconds()))
	return frames, rerr
}

// routeAndSymbolize is the core per-mapping router. Split out from
// SymbolizeProcess so the prelude (closed check, ctx setup, resolver
// fallback) stays compact and the routing logic is testable in
// isolation if a future test wants to inject mappings.
func (s *Symbolizer) routeAndSymbolize(
	ctx context.Context, pid uint32, ips []uint64, mappings []procmap.Mapping,
) ([]symbolize.Frame, error) {
	// Classify LAZILY: only the mappings an address actually falls into.
	//
	// This used to classify every mapping in the process, up front, on every
	// call — and classify is not cheap. It opens the ELF to test for DWARF,
	// searches for a debug link, and reads the build-id, so its cost is
	// per-mapping file I/O. A process maps hundreds of objects while a stack
	// touches a handful, and SymbolizeProcess runs once per SAMPLE, so the
	// work was O(samples x mappings) to serve O(samples x stack depth).
	//
	// Measured on a workstation before this change: a 5s system-wide capture
	// spent 1m9.88s in symbolization over 178 calls, of which 1m9.67s was
	// here — against 996ms for the same capture through the local symbolizer.
	// Two network requests were made in that time, both 404, zero bytes: the
	// cost was never the fetching. Issue #109.
	//
	// Keyed by mapping.Start, which is unique within one /proc/<pid>/maps
	// snapshot, and memoized for this call only — classification depends on
	// the debuginfod cache's contents, which a fetch during this same call
	// can change, so it must not be carried across calls without splitting
	// the immutable ELF inspection from the mutable cache lookup.
	routesByMapping := make(map[uint64]classifyResult, 8)
	classifyFor := func(m procmap.Mapping) classifyResult {
		if r, ok := routesByMapping[m.Start]; ok {
			return r
		}
		r := s.classifier.classify(ctx, m)
		routesByMapping[m.Start] = r
		return r
	}

	type fileBucket struct {
		mapping     procmap.Mapping
		debugPath   string
		originalIPs []uint64
		indices     []int
	}
	var processBatch struct {
		ips     []uint64
		indices []int
	}
	fileBuckets := map[uint64]*fileBucket{}
	skipped := map[int]bool{}

	for i, ip := range ips {
		m, ok := findMapping(mappings, ip)
		if !ok {
			// No mapping for this address. Hand it to process-mode so
			// blazesym can still emit [unknown]; that preserves the
			// stack slot and lets pprof attribute the sample to its
			// containing mapping if one shows up later.
			s.stats.classifyProcessMode.Add(1)
			processBatch.ips = append(processBatch.ips, ip)
			processBatch.indices = append(processBatch.indices, i)
			continue
		}
		r := classifyFor(m)
		switch r.route {
		case routeSkip:
			s.stats.classifySkipped.Add(1)
			skipped[i] = true
		case routeProcessMode:
			s.stats.classifyProcessMode.Add(1)
			processBatch.ips = append(processBatch.ips, ip)
			processBatch.indices = append(processBatch.indices, i)
		case routeFileMode:
			s.stats.classifyFileMode.Add(1)
			b, ok := fileBuckets[m.Start]
			if !ok {
				b = &fileBucket{mapping: m, debugPath: r.debugPath}
				fileBuckets[m.Start] = b
			}
			b.originalIPs = append(b.originalIPs, ip)
			b.indices = append(b.indices, i)
		}
	}

	out := make([]symbolize.Frame, len(ips))

	// Process-mode batch — single call covers every process-routed IP.
	if len(processBatch.ips) > 0 {
		frames, err := s.cgo.symbolizeProcess(pid, processBatch.ips)
		if err != nil {
			return nil, err
		}
		// blazesym occasionally returns fewer frames than IPs (the
		// abs-addrs API is documented as 1:1, but defensive: if the
		// shorter prefix is what we got, fill the rest with empty
		// frames keyed to the original address so stack shape stays
		// intact).
		for j, idx := range processBatch.indices {
			if j < len(frames) {
				out[idx] = frames[j]
			} else {
				out[idx] = symbolize.Frame{Address: processBatch.ips[j]}
			}
		}
	}

	// File-mode buckets, one per cached .debug.
	for _, b := range fileBuckets {
		if err := s.symbolizeFileBucket(pid, b.mapping, b.debugPath, b.originalIPs, b.indices, out); err != nil {
			return nil, err
		}
	}

	// Skipped frames keep their original address but no name — pprof's
	// mapping resolver will tag them with the mapping's basename.
	for i := range out {
		if skipped[i] {
			out[i] = symbolize.Frame{Address: ips[i]}
		}
	}

	return out, nil
}

// symbolizeFileBucket handles one file-mode bucket: builds an
// AddressMapper, normalizes the bucket's IPs to file-VAs, calls
// symbolizeElfVirt, and writes results into `out` at the indices
// supplied. Demotes individual unmappable IPs to process-mode; on a
// whole-bucket parse failure marks the .debug bad and demotes every
// validly-normalized IP back to process-mode.
//
// Pulled out of routeAndSymbolize to keep the bucket-handling control
// flow readable; the parent loop just walks the buckets and forwards
// errors.
func (s *Symbolizer) symbolizeFileBucket(
	pid uint32,
	m procmap.Mapping,
	debugPath string,
	originalIPs []uint64,
	indices []int,
	out []symbolize.Frame,
) error {
	mapper, err := s.classifier.mapperFor(m)
	if err != nil {
		// Mapper construction failed (file vanished, malformed ELF):
		// demote every IP in this bucket to process-mode.
		s.stats.normalizationFails.Add(uint64(len(originalIPs)))
		fallback, perr := s.cgo.symbolizeProcess(pid, originalIPs)
		if perr != nil {
			return perr
		}
		for j, idx := range indices {
			if j < len(fallback) {
				out[idx] = fallback[j]
			} else {
				out[idx] = symbolize.Frame{Address: originalIPs[j]}
			}
		}
		return nil
	}

	// Per-IP normalization: AddressMapper miss demotes that one IP.
	var (
		virt        = make([]uint64, 0, len(originalIPs))
		validIPs    = make([]uint64, 0, len(originalIPs))
		validIdx    = make([]int, 0, len(originalIPs))
		fallbackIPs []uint64
		fallbackIdx []int
	)
	for j, ip := range originalIPs {
		fileOff := ip - m.Start + m.Offset
		va, ok := mapper.FileOffsetToVirtualAddress(fileOff)
		if !ok {
			s.stats.normalizationFails.Add(1)
			fallbackIPs = append(fallbackIPs, ip)
			fallbackIdx = append(fallbackIdx, indices[j])
			continue
		}
		virt = append(virt, va)
		validIPs = append(validIPs, ip)
		validIdx = append(validIdx, indices[j])
	}

	if len(virt) > 0 {
		frames, ferr := s.cgo.symbolizeElfVirt(debugPath, validIPs, virt)
		if ferr != nil {
			// Whole-bucket parse failure: mark the .debug as bad so
			// future classify() calls skip it, then route the
			// validly-normalized IPs through process-mode.
			if sig, sigErr := statSig(debugPath); sigErr == nil {
				s.classifier.markBadDebug(sig)
			}
			s.stats.fileModeParseFails.Add(1)
			fb, perr := s.cgo.symbolizeProcess(pid, validIPs)
			if perr != nil {
				return perr
			}
			for j, idx := range validIdx {
				if j < len(fb) {
					out[idx] = fb[j]
				} else {
					out[idx] = symbolize.Frame{Address: validIPs[j]}
				}
			}
		} else {
			// symbolizeElfVirt asserts cnt == virtOffsets, so we can
			// rely on len(frames) == len(validIdx) here.
			s.stats.fileModeAddrs.Add(uint64(len(virt)))
			for j, idx := range validIdx {
				out[idx] = frames[j]
			}
		}
	}

	if len(fallbackIPs) > 0 {
		fb, perr := s.cgo.symbolizeProcess(pid, fallbackIPs)
		if perr != nil {
			return perr
		}
		for j, idx := range fallbackIdx {
			if j < len(fb) {
				out[idx] = fb[j]
			} else {
				out[idx] = symbolize.Frame{Address: fallbackIPs[j]}
			}
		}
	}
	return nil
}

// findMapping locates the procmap.Mapping that contains ip. Mappings
// are sorted by Start during populate(), so a linear scan over the
// (~few dozen) executable mappings in a process is fine for the batch
// sizes we see; binary search would shave microseconds at best.
func findMapping(mappings []procmap.Mapping, ip uint64) (procmap.Mapping, bool) {
	for _, m := range mappings {
		if ip >= m.Start && ip < m.Limit {
			return m, true
		}
	}
	return procmap.Mapping{}, false
}

// Close drains in-flight dispatcher invocations, frees blazesym, and
// closes the cache index. Idempotent.
//
// Order is critical: inflight.Wait() first, so no callback is mid-flight
// when we tear down the cgo state; cgo.close() next, which frees blazesym
// (releasing the Rust closure that holds the cb/ctx pair) before deleting
// the cgo handle; cache.Close() last.
func (s *Symbolizer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	s.inflight.Wait()
	if s.cgo != nil {
		s.cgo.close()
	}
	if t := s.opts.HTTPClient.Transport; t != nil {
		if cit, ok := t.(interface{ CloseIdleConnections() }); ok {
			cit.CloseIdleConnections()
		}
	}
	return s.cache.Close()
}

// Stats returns a snapshot of operational counters.
func (s *Symbolizer) Stats() Stats { return s.stats.snapshot() }

const indexFilename = "index.db"

// openIndex opens the cache's SQLite index. The indirection is kept so
// future tests can inject a fake Index without changing this site, but
// production calls NewSQLiteIndex directly.
func openIndex(path string) (cache.Index, error) {
	return cache.NewSQLiteIndex(path)
}
