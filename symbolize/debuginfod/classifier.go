package debuginfod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// routeKind selects the symbolization path for a single mapping.
type routeKind int

const (
	routeSkip routeKind = iota
	routeProcessMode
	routeFileMode
)

func (r routeKind) String() string {
	switch r {
	case routeSkip:
		return "skip"
	case routeProcessMode:
		return "process-mode"
	case routeFileMode:
		return "file-mode"
	default:
		return "unknown"
	}
}

// classifyResult is the per-mapping decision.
type classifyResult struct {
	route routeKind
	// debugPath is set when route == routeFileMode: the absolute path of
	// the .debug file blazesym should symbolize against.
	debugPath string
}

// classifier picks a route per mapping. It owns the negFetch and badDebug
// state — both content-addressed, both bounded LRU.
type classifier struct {
	cache   *cache.Cache
	fetcher *singleflightFetcher

	mu       sync.Mutex
	mappers  map[mapperKey]*procmap.AddressMapper
	negFetch map[string]time.Time  // build-id → deadline (don't re-fetch)
	badDebug map[pathSig]time.Time // path signature → deadline (don't re-open)
}

type mapperKey struct {
	dev uint64
	ino uint64
}

// pathSig identifies a debug file by (dev, ino, mtime). Keying badDebug by
// signature — rather than by build-id — ensures one corrupt cache copy
// never blocks a valid /usr/lib/debug sibling for the same build-id.
type pathSig struct {
	dev   uint64
	ino   uint64
	mtime int64 // unix nanos
}

const (
	negFetchTTL = 5 * time.Minute
	badDebugTTL = 1 * time.Hour
)

// nonSymbolizablePaths are virtual/anonymous mapping names that have no
// backing ELF; we drop them at Tier 1.
var nonSymbolizablePaths = []string{"", "[vdso]", "[stack]", "[vsyscall]", "[heap]"}

func newClassifier(c *cache.Cache, f *singleflightFetcher) *classifier {
	return &classifier{
		cache:    c,
		fetcher:  f,
		mappers:  make(map[mapperKey]*procmap.AddressMapper),
		negFetch: make(map[string]time.Time),
		badDebug: make(map[pathSig]time.Time),
	}
}

// classify returns the route for one mapping.
func (c *classifier) classify(ctx context.Context, m procmap.Mapping) classifyResult {
	// Tier 1: inherent non-symbolizable.
	if slices.Contains(nonSymbolizablePaths, m.Path) {
		return classifyResult{route: routeSkip}
	}

	// Tier 2: try to read the ELF; on any failure fall through to
	// process-mode so blazesym's defaults can attempt resolution.
	openPath := m.OpenablePath()
	if openPath == "" {
		// Unreachable from agent namespace. Process-mode is defensive —
		// blazesym will emit [binary]:offset which still preserves
		// stack shape vs skipping (which would drop frames).
		return classifyResult{route: routeProcessMode}
	}
	if hasDwarf(openPath) {
		return classifyResult{route: routeProcessMode}
	}
	// Debug-link search is filesystem-relative; only meaningful with the
	// symbolic path. Skip if it isn't readable.
	if m.Path != "" && hasResolvableDebuglink(m.Path, nil) {
		return classifyResult{route: routeProcessMode}
	}

	// Tier 3: file-mode routing. Read build-id; without one we can't
	// look up any debug file remotely, so fall through to process-mode.
	buildID := readBuildID(m.MapFiles, m.Path)
	if buildID == "" {
		return classifyResult{route: routeProcessMode}
	}

	// 3a: collect local candidate paths (no network).
	candidates := make([]string, 0, 2)
	if sys := systemDebugPath(buildID); sys != "" {
		candidates = append(candidates, sys)
	}
	if c.cache != nil && c.cache.Has(buildID, cache.KindDebuginfo) {
		candidates = append(candidates, c.cache.AbsPath(buildID, cache.KindDebuginfo))
	}

	// Try each candidate; skip ones in badDebug. badDebug is keyed by the
	// per-file (dev, ino, mtime) signature, so a corrupt cache copy does
	// NOT block a valid system-debug sibling that shares the same build-id.
	for _, p := range candidates {
		sig, err := statSig(p)
		if err != nil {
			continue // file vanished between exists() check and stat
		}
		if c.isBadDebug(sig) {
			continue
		}
		return classifyResult{route: routeFileMode, debugPath: p}
	}

	// 3b: local candidates exhausted (or all bad). Try remote fetch unless
	// a recent fetch failed or there is no fetcher.
	if c.isNegFetch(buildID) || c.fetcher == nil {
		return classifyResult{route: routeProcessMode}
	}

	abs, err := c.fetcher.fetchAndStore(ctx, "debuginfo", buildID)
	if err != nil {
		c.markNegFetch(buildID)
		return classifyResult{route: routeProcessMode}
	}

	// Newly fetched. Check badDebug in case the same signature was
	// previously marked (rare: same dev/ino/mtime as a known-bad file).
	sig, err := statSig(abs)
	if err != nil || c.isBadDebug(sig) {
		return classifyResult{route: routeProcessMode}
	}
	return classifyResult{route: routeFileMode, debugPath: abs}
}

// systemDebugPath returns the elfutils-standard /usr/lib/debug location for
// a given build-id. Returns "" if buildID is too short to split or the
// path doesn't exist.
func systemDebugPath(buildID string) string {
	if len(buildID) < 4 {
		return ""
	}
	candidate := filepath.Join("/usr/lib/debug", ".build-id", buildID[:2], buildID[2:]+".debug")
	if _, err := os.Stat(candidate); err != nil {
		return ""
	}
	return candidate
}

// statSig captures the (dev, ino, mtime) of path for badDebug keying.
func statSig(path string) (pathSig, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return pathSig{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return pathSig{}, errors.New("classifier: stat: not a *syscall.Stat_t")
	}
	return pathSig{
		dev:   uint64(st.Dev),
		ino:   st.Ino,
		mtime: fi.ModTime().UnixNano(),
	}, nil
}

func (c *classifier) markBadDebug(sig pathSig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.badDebug[sig] = time.Now().Add(badDebugTTL)
}

func (c *classifier) isBadDebug(sig pathSig) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, ok := c.badDebug[sig]
	if !ok {
		return false
	}
	if time.Now().After(deadline) {
		delete(c.badDebug, sig)
		return false
	}
	return true
}

func (c *classifier) markNegFetch(buildID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.negFetch[buildID] = time.Now().Add(negFetchTTL)
}

func (c *classifier) isNegFetch(buildID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, ok := c.negFetch[buildID]
	if !ok {
		return false
	}
	if time.Now().After(deadline) {
		delete(c.negFetch, buildID)
		return false
	}
	return true
}
