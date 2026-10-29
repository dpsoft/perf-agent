package debuginfod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// PrefetchOptions configures Prefetch.
type PrefetchOptions struct {
	URLs          []string
	CacheDir      string
	CacheMaxBytes int64

	// PIDs are the processes whose mappings are fetched for. Nil means every
	// process in /proc.
	PIDs []uint32

	// PerFileTimeout bounds one download, transfer included. Default 30m: long
	// enough for the 233MB libgallium debuginfo at the ~1.2MB/s that made a
	// capture's 5s budget hopeless (issue #224), and still finite.
	PerFileTimeout time.Duration

	// Concurrency is how many downloads run at once. Default 2: the cost is
	// bandwidth to one server, and more parallel streams mostly split it.
	Concurrency int

	// Logf reports each file as it finishes. Nil discards.
	Logf func(format string, args ...any)
}

// PrefetchResult counts what Prefetch did.
type PrefetchResult struct {
	Processes, Unreadable int // processes read, and those whose maps could not be
	AlreadyLocal          int // binaries with usable debug info already on disk
	Fetched, NotFound     int
	Failed                int
	Bytes                 int64
}

// Prefetch downloads, with no capture waiting on it, the debuginfo that
// symbolizing pids would otherwise try to fetch, into the cache a capture
// reads from.
//
// It exists because a capture cannot afford a large download and the server
// cannot resume a partial one (see slow.go): on a slow link, a debuginfo file
// of a few hundred MB never arrives within any budget a foreground capture
// can spend. Run once, this fills the cache, and every later capture finds
// the file locally. Issue #224.
//
// It asks the classifier's own question, localRoute, so it fetches exactly
// the build-ids a capture would have tried to -- nothing for a binary that
// already has DWARF, a resolvable debug link, a /usr/lib/debug copy or a
// cached one. It ignores "too slow" markers, since it has the time, and
// clears each one whose file it fetches.
func Prefetch(ctx context.Context, po PrefetchOptions) (PrefetchResult, error) {
	resolver := procmap.NewResolver()
	defer resolver.Close()
	return prefetch(ctx, po, func(pid uint32) ([]procmap.Mapping, error) {
		defer resolver.Invalidate(pid)
		return resolver.Mappings(pid)
	})
}

// prefetch is Prefetch with the source of each process's mappings injected,
// so a test can hand it fixture binaries instead of live processes.
func prefetch(ctx context.Context, po PrefetchOptions, mappingsOf func(pid uint32) ([]procmap.Mapping, error)) (PrefetchResult, error) {
	var res PrefetchResult
	if po.PerFileTimeout <= 0 {
		po.PerFileTimeout = 30 * time.Minute
	}
	if po.Concurrency <= 0 {
		po.Concurrency = 2
	}
	logf := po.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	opts := Options{
		URLs:          po.URLs,
		CacheDir:      po.CacheDir,
		CacheMaxBytes: po.CacheMaxBytes,
		FetchTimeout:  po.PerFileTimeout,
	}
	if err := opts.validate(); err != nil {
		return res, err
	}
	c, err := openCache(opts)
	if err != nil {
		return res, err
	}
	defer func() { _ = c.Close() }()

	pids := po.PIDs
	if pids == nil {
		if pids, err = allPIDs(); err != nil {
			return res, err
		}
	}

	// Which build-ids to fetch, and a path to name each by. Deduplicated by
	// path first because localRoute opens the file, and a shared library is
	// mapped several times into each of hundreds of processes.
	cl := newClassifier(c, nil, nil)
	want := make(map[string]string) // build-id -> path
	local := make(map[string]bool)
	seen := make(map[string]bool)
	for _, pid := range pids {
		mappings, err := mappingsOf(pid)
		if err != nil {
			res.Unreadable++
			continue
		}
		res.Processes++
		for _, m := range mappings {
			key := m.Path
			if key == "" {
				key = m.MapFiles
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			route, buildID, fetch := cl.localRoute(m)
			switch {
			case fetch:
				want[buildID] = m.Path
			case route.route == routeFileMode && !local[buildID]:
				local[buildID] = true
				res.AlreadyLocal++
			}
		}
	}

	sf := newSingleflightFetcher(newFetcher(opts.URLs, opts.HTTPClient), c).withBudget(0, opts.CacheDir)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, po.Concurrency)
	for buildID, path := range want {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			abs, err := fetchRetrying(ctx, sf, buildID, po.PerFileTimeout)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				var n int64
				if fi, serr := os.Stat(abs); serr == nil {
					n = fi.Size()
				}
				res.Fetched++
				res.Bytes += n
				logf("prefetch: %s: %.1fMB in %v", path, float64(n)/1e6, time.Since(start).Round(100*time.Millisecond))
			case errors.Is(err, ErrNotFound):
				res.NotFound++
			default:
				res.Failed++
				logf("prefetch: %s (build-id %s): %v", path, buildID, err)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("prefetch interrupted: %w", err)
	}
	return res, nil
}

// prefetchAttempts bounds retries of a download that failed for a reason
// other than a 404. Measured against debuginfod.fedoraproject.org: the
// server resets long transfers ("connection reset by peer" while streaming
// the 1.38GB libLLVM debuginfo), and Go multiplexes concurrent requests to one
// host over a single HTTP/2 connection, so the reset also killed the small
// downloads sharing it. A second attempt, on a new connection, fetched 5 of
// the 8 the first run lost. Each retry starts from zero: the server ignores
// Range.
const prefetchAttempts = 3

func fetchRetrying(ctx context.Context, sf *singleflightFetcher, buildID string, perFile time.Duration) (string, error) {
	var err error
	for attempt := range prefetchAttempts {
		if attempt > 0 {
			sf.forgetFailure("debuginfo", buildID)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		fctx, cancel := context.WithTimeout(ctx, perFile)
		var abs string
		abs, err = sf.fetchAndStore(fctx, "debuginfo", buildID)
		cancel()
		if err == nil || errors.Is(err, ErrNotFound) || ctx.Err() != nil {
			return abs, err
		}
	}
	return "", err
}

func allPIDs() ([]uint32, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []uint32
	for _, e := range entries {
		if n, err := strconv.ParseUint(e.Name(), 10, 32); err == nil && e.IsDir() {
			pids = append(pids, uint32(n))
		}
	}
	return pids, nil
}
