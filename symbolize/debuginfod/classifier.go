package debuginfod

import (
	"context"
	"errors"
	"fmt"
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

	// systemDebugRoot is the directory blazesym would walk for build-id-
	// keyed debug files. Defaults to "/usr/lib/debug/.build-id" (the
	// elfutils standard). Tests override it via newClassifierForTest.
	systemDebugRoot string

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
		cache:           c,
		fetcher:         f,
		systemDebugRoot: "/usr/lib/debug/.build-id",
		mappers:         make(map[mapperKey]*procmap.AddressMapper),
		negFetch:        make(map[string]time.Time),
		badDebug:        make(map[pathSig]time.Time),
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
	if sys := c.systemDebugPath(buildID); sys != "" {
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

// mapperFor returns an AddressMapper for the mapping, content-addressed
// by (dev, ino) of mapping.OpenablePath(). Mappers are cached across
// classify() calls: two mappings backed by the same inode (e.g. a
// shared library mapped into multiple processes) reuse a single parse.
//
// The lock is held only around map access; the (potentially slow)
// NewAddressMapper call happens unlocked. A benign race may cause two
// goroutines to parse the same file concurrently; the loser's mapper
// is discarded.
func (c *classifier) mapperFor(m procmap.Mapping) (*procmap.AddressMapper, error) {
	openPath := m.OpenablePath()
	if openPath == "" {
		return nil, errors.New("classifier: mapping not openable")
	}
	fi, err := os.Stat(openPath)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("classifier: stat %s: not a *syscall.Stat_t", openPath)
	}
	key := mapperKey{dev: uint64(st.Dev), ino: st.Ino}

	c.mu.Lock()
	if existing, ok := c.mappers[key]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.mu.Unlock()

	mapper, err := procmap.NewAddressMapper(openPath)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if existing, ok := c.mappers[key]; ok {
		// A concurrent caller won the race; prefer the cached entry
		// to keep identity stable for downstream consumers.
		mapper = existing
	} else {
		c.mappers[key] = mapper
	}
	c.mu.Unlock()
	return mapper, nil
}

// systemDebugPath returns the elfutils-standard split-debug path for
// buildID under c.systemDebugRoot, or "" if absent.
func (c *classifier) systemDebugPath(buildID string) string {
	if len(buildID) < 4 {
		return ""
	}
	candidate := filepath.Join(c.systemDebugRoot, buildID[:2], buildID[2:]+".debug")
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
		return pathSig{}, fmt.Errorf("classifier: stat %s: not a *syscall.Stat_t", path)
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
