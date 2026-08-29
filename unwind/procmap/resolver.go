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

	buildIDs       sync.Map // path string -> build-id hex string
	populateCounts sync.Map // uint32 (pid) -> *int64 (populate count)
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

// Mappings returns a snapshot of pid's executable mappings, populating
// the per-PID cache on first call. The returned slice aliases the cached
// state — callers MUST NOT mutate it.
//
// Return contract:
//   - (nil, nil)  — PID has no mappings: process gone, access restricted,
//     or the maps file contained no executable regions.
//   - (nil, err)  — /proc parse failed (I/O error, unexpected format, etc.)
//   - (mappings, nil) — success; slice may be empty if no executable regions
//     were found (same observable result as the nil-nil case above).
func (r *Resolver) Mappings(pid uint32) ([]Mapping, error) {
	entry := r.entryFor(pid)
	entry.once.Do(func() { r.populate(entry, pid) })
	if entry.err != nil {
		return nil, entry.err
	}
	return entry.mappings, nil
}

// Warm populates pid's cache now, while the process is presumably still
// alive, and reports how many executable mappings it holds afterwards.
//
// This exists because of WHEN the profilers resolve. Lookup populates lazily,
// and in the profilers the first Lookup for a PID happens at collect time —
// after the whole capture window has closed. A process sampled during the
// window but gone by the end has no /proc/<pid>/maps left to read, so every
// frame it contributed resolves to nothing. System-wide is where that bites:
// short-lived processes are exactly what a `-a` capture exists to catch, and
// exactly the ones guaranteed to be gone by the time anything asks. Issue #56.
//
// Idempotent and cheap to repeat: the per-PID sync.Once means a second call
// for the same PID costs a map lookup and nothing else, which is what lets a
// sweep run on a timer without re-reading /proc for every process each time.
//
// It deliberately does NOT freeze the answer. A process still alive at collect
// time is Refreshed there, so warming only ever supplies a fallback for
// processes that are gone — it never makes a live process's mappings staler
// than they would have been.
func (r *Resolver) Warm(pid uint32) int {
	m, err := r.Mappings(pid)
	if err != nil {
		return 0
	}
	return len(m)
}

// Refresh re-reads pid's mappings, KEEPING the existing cache entry if the
// re-read yields nothing.
//
// That fallback is the whole point, and it is why this is not Invalidate
// followed by Lookup. Invalidate drops the entry unconditionally, so a process
// that exits between the drop and the re-read loses mappings that had been
// warmed while it was alive — turning the fix for #56 back into the bug, in a
// window narrow enough to be rare and therefore to be mistaken for something
// else. Here the old entry survives any failure.
//
// Returns true when fresh mappings replaced the cached ones.
func (r *Resolver) Refresh(pid uint32) bool {
	fresh := &pidEntry{}
	fresh.once.Do(func() { r.populate(fresh, pid) })
	if fresh.err != nil || len(fresh.mappings) == 0 {
		return false // process gone or unreadable; keep whatever we warmed
	}
	r.mu.Lock()
	r.cache[pid] = fresh
	r.mu.Unlock()
	return true
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

// isCached reports whether pid has an entry already, without creating one.
// The Warmer uses it to count first reads separately from repeats; creating an
// entry here would make every sweep look like a first read.
func (r *Resolver) isCached(pid uint32) bool {
	r.mu.RLock()
	_, ok := r.cache[pid]
	r.mu.RUnlock()
	return ok
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
	defer r.bumpPopulateCount(pid)
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

	r.attachBuildIDs(mappings)
	entry.mappings = mappings
}

// attachBuildIDs sets BuildID on each mapping that doesn't already have one.
// It tries MapFiles first (kernel-resolved symlink, works across mount
// namespaces and survives unlinked-but-mapped binaries), then falls back to
// the symbolic Path.
//
// When the build-id is read via MapFiles, the result is cached under the
// symbolic Path — not under the MapFiles key — so that two PIDs mapping the
// same binary share a single cache entry. Keying by MapFiles
// (/proc/<pid>/map_files/<range>) would create a unique entry per PID+VA,
// causing unbounded growth of r.buildIDs in long-running agents.
func (r *Resolver) attachBuildIDs(mappings []Mapping) {
	for i := range mappings {
		if mappings[i].BuildID != "" {
			continue // already attached
		}
		if mp := mappings[i].MapFiles; mp != "" {
			// Read directly via MapFiles; bypass buildIDFor's cache because
			// the MapFiles key is /proc/<pid>/map_files/<range> — unique per
			// PID + VA range — so caching by it defeats cross-PID sharing.
			if id, _ := ReadBuildID(mp); id != "" {
				mappings[i].BuildID = id
				// Backfill the cache under the stable symbolic path so future
				// lookups for the same binary (possibly a different PID) get a
				// cache hit without re-reading the ELF.
				if p := mappings[i].Path; p != "" {
					r.buildIDs.LoadOrStore(p, id)
				}
				continue
			}
		}
		mappings[i].BuildID = r.buildIDFor(mappings[i].Path)
	}
}

// BuildID returns a cached hex build-id for path, reading the ELF on
// first call. Read failures produce an empty string (cached) —
// a missing build-id is not a Lookup failure. Safe for concurrent use.
func (r *Resolver) BuildID(path string) string { return r.buildIDFor(path) }

// buildIDFor is the internal implementation. The exported BuildID method
// delegates here.
func (r *Resolver) buildIDFor(path string) string {
	if v, ok := r.buildIDs.Load(path); ok {
		return v.(string)
	}
	id, _ := ReadBuildID(path)
	actual, _ := r.buildIDs.LoadOrStore(path, id)
	return actual.(string)
}

// bumpPopulateCount increments the per-PID populate counter. Test-only
// observability: lets tests assert whether a cache miss forced a
// re-parse.
func (r *Resolver) bumpPopulateCount(pid uint32) {
	v, _ := r.populateCounts.LoadOrStore(pid, new(int64))
	*v.(*int64)++
}

// populateCountForTest returns the number of times populate ran for
// pid. Exported name ends with "ForTest" to mark it as a test hook;
// no external callers should rely on it.
func (r *Resolver) populateCountForTest(pid uint32) int64 {
	v, ok := r.populateCounts.Load(pid)
	if !ok {
		return 0
	}
	return *v.(*int64)
}
