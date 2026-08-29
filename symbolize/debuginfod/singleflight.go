package debuginfod

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"golang.org/x/sync/singleflight"
)

// storer is the slice of cache.Cache that the singleflight fetcher needs.
// The interface lets tests substitute a stub.
type storer interface {
	WriteAtomic(buildID string, kind cache.Kind, body io.Reader) (string, error)
	Evict() error
}

// sfFetcher is the minimal contract needed by the dispatcher to fetch a
// build-id-keyed artifact. Tests provide an implementation that counts
// calls; production uses *singleflightFetcher.
type sfFetcher interface {
	fetchAndStore(ctx context.Context, kindStr, buildID string) (string, error)
}

// fetchFailTTL is how long a failed (kind, build-id) lookup is remembered.
//
// singleflight collapses CONCURRENT fetches; it does nothing for sequential
// ones. Symbolization runs once per sample, so a build-id no server can serve
// was re-attempted on every sample — and a lookup that hangs rather than 404s
// costs the whole FetchTimeout each time. Measured on a workstation: a 5s
// system-wide capture spent 1m19s in symbolization, of which the slowest
// single call was 30.022s, exactly the default timeout. Only 17 of 176 calls
// were slow; the rest were under 50ms. Issue #109.
//
// Five minutes rather than the run's lifetime: a transient outage or a server
// that was briefly unreachable should not poison symbolization for a
// long-lived agent, and retrying once every five minutes costs at most one
// stall per build-id per five minutes. It matches classifier.negFetchTTL
// deliberately — the two should expire together.
//
// This is NOT a duplicate of classifier.negFetch, though it overlaps it. That
// one guards the classifier's own "debuginfo" fetch and is consulted before
// the call is made, which is cheaper. The dispatcher's "executable" fetch
// (dispatcher.go, case 4) consults nothing, so its failures were repeated
// indefinitely. Putting the memo here, at the chokepoint both paths go
// through, means a fetch site cannot be added later that forgets to check —
// which is how the dispatcher path came to be missing one.
const fetchFailTTL = 5 * time.Minute

// maxFetchFailEntries bounds the memo. A machine can map more distinct
// build-ids than it is worth remembering failures for, and an unbounded map on
// a long-lived agent is a leak.
const maxFetchFailEntries = 4096

type singleflightFetcher struct {
	upstream *fetcher
	cache    storer
	sf       singleflight.Group

	failedMu sync.Mutex
	failed   map[string]fetchFailure
}

type fetchFailure struct {
	err        error
	retryAfter time.Time
}

func newSingleflightFetcher(upstream *fetcher, store storer) *singleflightFetcher {
	return &singleflightFetcher{
		upstream: upstream,
		cache:    store,
		failed:   make(map[string]fetchFailure),
	}
}

// recentFailure reports a remembered failure for key, if one is still within
// its TTL. Expired entries are dropped on the way past.
func (s *singleflightFetcher) recentFailure(key string) (error, bool) {
	s.failedMu.Lock()
	defer s.failedMu.Unlock()
	f, ok := s.failed[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(f.retryAfter) {
		delete(s.failed, key)
		return nil, false
	}
	return f.err, true
}

// noteFailure remembers that key could not be fetched.
func (s *singleflightFetcher) noteFailure(key string, err error) {
	s.failedMu.Lock()
	defer s.failedMu.Unlock()
	if len(s.failed) >= maxFetchFailEntries {
		// Drop an arbitrary entry rather than growing without bound. Which one
		// goes does not matter: every entry is a negative result whose only
		// cost, if dropped early, is one more attempt.
		for k := range s.failed {
			delete(s.failed, k)
			break
		}
	}
	s.failed[key] = fetchFailure{err: err, retryAfter: time.Now().Add(fetchFailTTL)}
}

// fetchAndStore collapses concurrent fetches keyed by (kind, buildID).
// On success the response body is streamed into the cache and the absolute
// final path is returned.
func (s *singleflightFetcher) fetchAndStore(ctx context.Context, kindStr, buildID string) (string, error) {
	key := kindStr + ":" + buildID
	// A lookup that already failed is not retried until its TTL expires.
	// Without this the same unavailable build-id is re-fetched once per
	// sample, and each attempt pays the full fetch timeout when the server
	// hangs rather than answering. See fetchFailTTL.
	if err, ok := s.recentFailure(key); ok {
		return "", err
	}
	res, err, _ := s.sf.Do(key, func() (any, error) {
		body, err := s.upstream.fetch(ctx, kindStr, buildID)
		if err != nil {
			return "", err
		}
		defer func() { _ = body.Close() }()
		var k cache.Kind
		switch kindStr {
		case "debuginfo":
			k = cache.KindDebuginfo
		case "executable":
			k = cache.KindExecutable
		}
		abs, werr := s.cache.WriteAtomic(buildID, k, body)
		if werr != nil {
			return "", werr
		}
		// Best-effort eviction. Errors are surfaced via the cache's own counters
		// in M2; for now we just suppress so a fetch that succeeded isn't
		// retroactively marked as a failure.
		_ = s.cache.Evict()
		return abs, nil
	})
	if err != nil {
		s.noteFailure(key, err)
		return "", err
	}
	return res.(string), nil
}
