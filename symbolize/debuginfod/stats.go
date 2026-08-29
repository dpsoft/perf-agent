package debuginfod

import "sync/atomic"

// Stats reports operational counters for a Symbolizer. Read via Stats().
type Stats struct {
	CacheHits, CacheMisses, CacheEvictions        uint64
	FetchSuccessDebuginfo, FetchSuccessExecutable uint64
	Fetch404s, FetchErrors                        uint64
	FetchBytesTotal                               uint64
	InFlightFetches                               int64
	DispatcherCalls, DispatcherSkippedLocal       uint64
	DispatcherPanics                              uint64
	// Per-mapping routing (Symbolize-time).
	ClassifyProcessMode, ClassifyFileMode, ClassifySkipped uint64
	// File-mode outcomes.
	// FileModeAddrs is the total number of addresses (IPs) resolved through
	// the file-mode path (one per IP, not one per symbolizeFileBucket call).
	FileModeAddrs, FileModeParseFails     uint64
	FileModeFetchFails, FileModeLocalHits uint64
	// AddressMapper miss for an individual IP.
	NormalizationFails uint64

	// Where SymbolizeProcess actually spends its time (issue #109). Calls is
	// how many times it ran; the three Ns figures are cumulative wall time in
	// its parts. They exist because the cost of this path was invisible: a
	// run could spend 82 of its 87 seconds here having made two network
	// requests, and nothing said which part was slow.
	Calls       uint64
	TotalNs     uint64
	MappingsNs  uint64 // resolver Invalidate + Mappings, per call
	SymbolizeNs uint64 // the blazesym calls themselves
	// SlowestNs is the most expensive single call and CallsOver50ms how many
	// exceeded 50ms. Together they separate the two explanations a large
	// TotalNs allows: a handful of cold parses of large binaries (few slow
	// calls, most fast) versus work being redone on every call (nearly all
	// slow). A total alone cannot tell those apart, and guessing which one it
	// was cost several wrong theories on issue #109.
	SlowestNs     uint64
	CallsOver50ms uint64
}

type atomicStats struct {
	calls         atomic.Uint64
	totalNs       atomic.Uint64
	mappingsNs    atomic.Uint64
	symbolizeNs   atomic.Uint64
	slowestNs     atomic.Uint64
	callsOver50ms atomic.Uint64

	cacheHits, cacheMisses, cacheEvictions        atomic.Uint64
	fetchSuccessDebuginfo, fetchSuccessExecutable atomic.Uint64
	fetch404s, fetchErrors                        atomic.Uint64
	fetchBytesTotal                               atomic.Uint64
	inFlightFetches                               atomic.Int64
	dispatcherCalls, dispatcherSkippedLocal       atomic.Uint64
	dispatcherPanics                              atomic.Uint64
	// Classifier routing (Symbolize-time).
	classifyProcessMode, classifyFileMode, classifySkipped atomic.Uint64
	// File-mode outcomes.
	// fileModeAddrs counts the total number of addresses (IPs) resolved
	// through the file-mode path; incremented by len(virt) per bucket call.
	fileModeAddrs, fileModeParseFails     atomic.Uint64
	fileModeFetchFails, fileModeLocalHits atomic.Uint64
	// AddressMapper miss for an individual IP.
	normalizationFails atomic.Uint64
}

func (a *atomicStats) snapshot() Stats {
	return Stats{
		Calls:                  a.calls.Load(),
		TotalNs:                a.totalNs.Load(),
		MappingsNs:             a.mappingsNs.Load(),
		SymbolizeNs:            a.symbolizeNs.Load(),
		SlowestNs:              a.slowestNs.Load(),
		CallsOver50ms:          a.callsOver50ms.Load(),
		CacheHits:              a.cacheHits.Load(),
		CacheMisses:            a.cacheMisses.Load(),
		CacheEvictions:         a.cacheEvictions.Load(),
		FetchSuccessDebuginfo:  a.fetchSuccessDebuginfo.Load(),
		FetchSuccessExecutable: a.fetchSuccessExecutable.Load(),
		Fetch404s:              a.fetch404s.Load(),
		FetchErrors:            a.fetchErrors.Load(),
		FetchBytesTotal:        a.fetchBytesTotal.Load(),
		InFlightFetches:        a.inFlightFetches.Load(),
		DispatcherCalls:        a.dispatcherCalls.Load(),
		DispatcherSkippedLocal: a.dispatcherSkippedLocal.Load(),
		DispatcherPanics:       a.dispatcherPanics.Load(),
		ClassifyProcessMode:    a.classifyProcessMode.Load(),
		ClassifyFileMode:       a.classifyFileMode.Load(),
		ClassifySkipped:        a.classifySkipped.Load(),
		FileModeAddrs:          a.fileModeAddrs.Load(),
		FileModeParseFails:     a.fileModeParseFails.Load(),
		FileModeFetchFails:     a.fileModeFetchFails.Load(),
		FileModeLocalHits:      a.fileModeLocalHits.Load(),
		NormalizationFails:     a.normalizationFails.Load(),
	}
}
