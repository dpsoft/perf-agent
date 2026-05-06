package debuginfod

import (
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

// Symbolizer resolves abs addresses against a process while consulting a
// debuginfod-protocol server for missing debug info. Implements
// symbolize.Symbolizer.
type Symbolizer struct {
	opts     Options
	cache    *cache.Cache
	fetcher  *fetcher
	sf       *singleflightFetcher
	cgo      *cgoState
	stats    atomicStats
	closed   atomic.Bool
	inflight sync.WaitGroup
}

// New constructs a Symbolizer from opts. opts.URLs must be non-empty.
func New(opts Options) (*Symbolizer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
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
	sf := newSingleflightFetcher(f, c)

	s := &Symbolizer{
		opts:    opts,
		cache:   c,
		fetcher: f,
		sf:      sf,
	}
	st, err := newCgoState(s)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	s.cgo = st
	return s, nil
}

// SymbolizeProcess resolves abs IPs into Frames via blazesym, with the
// debuginfod dispatcher inserted as a process_dispatch hook so missing
// debug info is fetched on demand.
func (s *Symbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	return s.cgo.symbolizeProcess(pid, ips)
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
