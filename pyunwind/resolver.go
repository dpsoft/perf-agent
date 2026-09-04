package pyunwind

import (
	"fmt"
	"sync"
)

// Resolver turns code-object addresses into names for one process, and
// remembers what it found.
//
// WHY IT CACHES, which is the whole design
// ----------------------------------------
// Reading a name means reading the target's memory, so it only works while the
// target is alive. A profiler builds its output after the workload has exited
// -- the GPU tools do it explicitly, and even the CPU ones resolve at collect
// time -- so a name looked up at render time is a name that cannot be looked
// up at all. Every read therefore happens during the capture, and the cache is
// what carries the answer past the process's death.
//
// Code objects are immortal for the life of the interpreter and are reused for
// every call of the same function, so the cache is small and its hit rate is
// high: a PyTorch capture with 213 Python frames touches far fewer distinct
// code objects than that.
type Resolver struct {
	reader *ProcReader
	off    CodeOffsets

	mu    sync.RWMutex
	cache map[uint64]CodeInfo
	// misses records addresses that could not be read, so a broken one is
	// attempted once rather than once per sample.
	misses map[uint64]struct{}

	stats ResolverStats
}

// ResolverStats reports what resolution achieved. Hits and Misses are kept
// apart from Declined because "we looked and failed" and "this build cannot
// look at this interpreter" are different facts and only the first is a defect.
type ResolverStats struct {
	Resolved uint64
	Cached   uint64
	Failed   uint64
	Declined uint64
}

// NewResolver returns a resolver for pid, or nil when this build has no
// measured code offsets for the interpreter. A nil *Resolver is usable: its
// Resolve declines, and the frame keeps the python:0x… form.
func NewResolver(pid int, off CodeOffsets) *Resolver {
	if !off.Measured() {
		return nil
	}
	return &Resolver{
		reader: NewProcReader(pid, pid),
		off:    off,
		cache:  make(map[uint64]CodeInfo, 64),
		misses: make(map[uint64]struct{}, 8),
	}
}

// Resolve names one code object. The second return is false when nothing could
// be read, and the caller must then keep whatever it had.
func (r *Resolver) Resolve(codeObject uint64) (CodeInfo, bool) {
	if r == nil {
		return CodeInfo{}, false
	}
	r.mu.RLock()
	if info, ok := r.cache[codeObject]; ok {
		r.mu.RUnlock()
		r.stats.Cached++
		return info, true
	}
	_, bad := r.misses[codeObject]
	r.mu.RUnlock()
	if bad {
		return CodeInfo{}, false
	}

	info, err := ReadCodeInfo(r.reader, r.off, codeObject)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.misses[codeObject] = struct{}{}
		r.stats.Failed++
		return CodeInfo{}, false
	}
	r.cache[codeObject] = info
	r.stats.Resolved++
	return info, true
}

// Name renders a resolved frame the way a flame graph should read it:
// qualified function, file basename, first line. Falls back to the address
// form when nothing could be read, so a partially resolvable process still
// produces a legible stack rather than a mixture of styles.
func (r *Resolver) Name(codeObject uint64) string {
	info, ok := r.Resolve(codeObject)
	if !ok || info.Qualname == "" {
		return FrameName(codeObject)
	}
	if info.Filename == "" {
		return info.Qualname
	}
	return fmt.Sprintf("%s (%s:%d)", info.Qualname, baseName(info.Filename), info.FirstLine)
}

// Stats returns a snapshot.
func (r *Resolver) Stats() ResolverStats {
	if r == nil {
		return ResolverStats{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stats
}

// baseName is filepath.Base without the import: a co_filename is always a
// slash-separated path as CPython stores it, whatever the host separator.
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// resolvers holds one Resolver per attached process.
//
// Package-level because the namer registered with unwind/interp is
// package-level: the interp registry is keyed by unwinder id, not by module
// instance, and a process is attached by whichever module instance happens to
// own that pid.
var (
	resolversMu sync.RWMutex
	resolvers   = map[uint32]*Resolver{}
)

// InstallResolver records the resolver to use for pid. Called at attach, where
// the interpreter's version -- and therefore whether its code offsets are
// measured at all -- has just been established.
func InstallResolver(pid uint32, r *Resolver) {
	resolversMu.Lock()
	defer resolversMu.Unlock()
	if r == nil {
		delete(resolvers, pid)
		return
	}
	resolvers[pid] = r
}

// RemoveResolver forgets pid. The cached names go with it, which is correct:
// they described that process, and a pid is reused.
func RemoveResolver(pid uint32) {
	resolversMu.Lock()
	defer resolversMu.Unlock()
	delete(resolvers, pid)
}

func resolverFor(pid uint32) *Resolver {
	resolversMu.RLock()
	defer resolversMu.RUnlock()
	return resolvers[pid]
}
