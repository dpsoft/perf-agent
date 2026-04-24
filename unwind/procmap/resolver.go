package procmap

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
)

// Resolver caches per-PID /proc/<pid>/maps snapshots and per-path
// build-ids. Safe for concurrent use. Populates lazily on first
// Lookup for a PID.
type Resolver struct {
	procRoot string

	mu    sync.RWMutex
	cache map[uint32]*pidEntry

	buildIDs sync.Map // path string -> build-id hex string
}

type pidEntry struct {
	once     sync.Once
	err      error
	mappings []Mapping // sorted by Start; binary-searched on Lookup
}

// NewResolver returns a Resolver ready for concurrent use.
func NewResolver(opts ...Option) *Resolver {
	cfg := resolverConfig{procRoot: "/proc"}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Resolver{
		procRoot: cfg.procRoot,
		cache:    map[uint32]*pidEntry{},
	}
}

// Lookup returns the Mapping containing addr in pid's address space.
// Returns ok=false when pid has no cached (or resolvable) mappings,
// or when addr falls outside every known executable range.
func (r *Resolver) Lookup(pid uint32, addr uint64) (Mapping, bool) {
	entry := r.entryFor(pid)
	entry.once.Do(func() { r.populate(entry, pid) })
	if entry.err != nil || len(entry.mappings) == 0 {
		return Mapping{}, false
	}

	// Binary search for the largest Start <= addr.
	idx := sort.Search(len(entry.mappings), func(i int) bool {
		return entry.mappings[i].Start > addr
	}) - 1
	if idx < 0 {
		return Mapping{}, false
	}
	m := entry.mappings[idx]
	if addr >= m.Limit {
		return Mapping{}, false
	}
	return m, true
}

// Invalidate drops any cached state for pid. The next Lookup
// re-parses /proc/<pid>/maps. Call on process exit or when the
// agent learns of whole-process churn (e.g., exec).
func (r *Resolver) Invalidate(pid uint32) {
	r.mu.Lock()
	delete(r.cache, pid)
	r.mu.Unlock()
}

// InvalidateAddr invalidates pid's cache only if addr falls outside
// every currently cached mapping — i.e., a new mmap extended the
// process's address space. Cheap no-op otherwise.
func (r *Resolver) InvalidateAddr(pid uint32, addr uint64) {
	if _, ok := r.Lookup(pid, addr); ok {
		return
	}
	r.Invalidate(pid)
}

// Close releases cached state. After Close, the Resolver remains
// usable but behaves as freshly constructed; in-flight Lookups that
// captured a *pidEntry before the call complete normally against
// their captured snapshot.
func (r *Resolver) Close() {
	r.mu.Lock()
	r.cache = map[uint32]*pidEntry{}
	r.mu.Unlock()
}

// entryFor returns the per-PID entry, creating it under the write
// lock if absent. The caller runs the entry's sync.Once to do the
// actual /proc parse; this method is purely the intern step.
func (r *Resolver) entryFor(pid uint32) *pidEntry {
	r.mu.RLock()
	e, ok := r.cache[pid]
	r.mu.RUnlock()
	if ok {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok = r.cache[pid]; ok {
		return e
	}
	e = &pidEntry{}
	r.cache[pid] = e
	return e
}

// populate reads /proc/<pid>/maps, fills entry.mappings, and attaches
// build-ids. Missing PIDs are cached as empty (entry.err==nil,
// entry.mappings==nil) so subsequent Lookups fast-fail.
func (r *Resolver) populate(entry *pidEntry, pid uint32) {
	path := filepath.Join(r.procRoot, fmt.Sprint(pid), "maps")
	mappings, err := parseMapsFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// PID gone — cache empty, no error surfaced to caller.
			return
		}
		entry.err = err
		return
	}

	for i := range mappings {
		mappings[i].BuildID = r.buildIDFor(mappings[i].Path)
	}
	entry.mappings = mappings
}

// buildIDFor returns a cached hex build-id for path, reading the ELF
// on first call. Read failures produce an empty string (cached) —
// a missing build-id is not a Lookup failure.
func (r *Resolver) buildIDFor(path string) string {
	if v, ok := r.buildIDs.Load(path); ok {
		return v.(string)
	}
	id, _ := readBuildID(path)
	actual, _ := r.buildIDs.LoadOrStore(path, id)
	return actual.(string)
}
