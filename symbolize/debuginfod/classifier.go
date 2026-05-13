package debuginfod

import (
	"context"
	"slices"
	"sync"
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

	// Tier 3 (file-mode) lands in the next task.
	return classifyResult{route: routeProcessMode}
}
