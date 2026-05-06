package debuginfod

import "sync/atomic"

// Stats reports operational counters for a Symbolizer. Read via Stats().
type Stats struct {
	CacheHits, CacheMisses, CacheEvictions             uint64
	FetchSuccessDebuginfo, FetchSuccessExecutable      uint64
	Fetch404s, FetchErrors                             uint64
	FetchBytesTotal                                    uint64
	InFlightFetches                                    int64
	DispatcherCalls, DispatcherSkippedLocal            uint64
	DispatcherPanics                                   uint64
}

type atomicStats struct {
	cacheHits, cacheMisses, cacheEvictions             atomic.Uint64
	fetchSuccessDebuginfo, fetchSuccessExecutable      atomic.Uint64
	fetch404s, fetchErrors                             atomic.Uint64
	fetchBytesTotal                                    atomic.Uint64
	inFlightFetches                                    atomic.Int64
	dispatcherCalls, dispatcherSkippedLocal            atomic.Uint64
	dispatcherPanics                                   atomic.Uint64
}

func (a *atomicStats) snapshot() Stats {
	return Stats{
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
	}
}
