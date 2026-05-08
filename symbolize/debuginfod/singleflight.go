package debuginfod

import (
	"context"
	"io"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"golang.org/x/sync/singleflight"
)

// storer is the slice of cache.Cache that the singleflight fetcher needs.
// The interface lets tests substitute a stub.
type storer interface {
	WriteAtomic(buildID string, kind cache.Kind, body io.Reader) (string, error)
	Evict() error
}

type singleflightFetcher struct {
	upstream *fetcher
	cache    storer
	sf       singleflight.Group
}

func newSingleflightFetcher(upstream *fetcher, store storer) *singleflightFetcher {
	return &singleflightFetcher{upstream: upstream, cache: store}
}

// fetchAndStore collapses concurrent fetches keyed by (kind, buildID).
// On success the response body is streamed into the cache and the absolute
// final path is returned.
func (s *singleflightFetcher) fetchAndStore(ctx context.Context, kindStr, buildID string) (string, error) {
	key := kindStr + ":" + buildID
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
		return "", err
	}
	return res.(string), nil
}
