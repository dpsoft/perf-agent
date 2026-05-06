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
		defer body.Close()
		var k cache.Kind
		switch kindStr {
		case "debuginfo":
			k = cache.KindDebuginfo
		case "executable":
			k = cache.KindExecutable
		}
		return s.cache.WriteAtomic(buildID, k, body)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}
