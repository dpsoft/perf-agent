package ehmaps

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// RefcountTable tracks which (tableID, PID) pairs currently reference a
// CFI table. A table stays in the BPF maps until the last PID releases it.
// Zero-value is not usable; construct via NewRefcountTable.
//
// Operations are safe for concurrent use. Acquire and Release return the
// post-operation refcount so callers can decide whether to install or
// evict the actual BPF-side table.
type RefcountTable struct {
	mu   sync.Mutex
	byID map[uint64]map[uint32]struct{} // tableID → set of PIDs
}

// NewRefcountTable creates an empty RefcountTable.
func NewRefcountTable() *RefcountTable {
	return &RefcountTable{byID: map[uint64]map[uint32]struct{}{}}
}

// Acquire records that `pid` now references `tableID`. Idempotent — a
// repeat acquire for the same (tid, pid) does NOT double-count. Returns
// the resulting refcount (number of distinct PIDs holding this tableID).
func (r *RefcountTable) Acquire(tableID uint64, pid uint32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	pids, ok := r.byID[tableID]
	if !ok {
		pids = map[uint32]struct{}{}
		r.byID[tableID] = pids
	}
	pids[pid] = struct{}{}
	return len(pids)
}

// Release records that `pid` no longer references `tableID`. Returns the
// resulting refcount. Releasing an untracked (tid, pid) is a no-op
// (returns 0).
func (r *RefcountTable) Release(tableID uint64, pid uint32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	pids, ok := r.byID[tableID]
	if !ok {
		return 0
	}
	delete(pids, pid)
	if len(pids) == 0 {
		delete(r.byID, tableID)
		return 0
	}
	return len(pids)
}

// TableStore owns the BPF-side cfi_* outer maps and composes refcount
// tracking with actual map population. Wraps Populate{CFI,Classification} with refcounting so callers don't
// hand-manage table lifetimes.
type TableStore struct {
	CFIRules          *ebpf.Map
	CFILengths        *ebpf.Map
	CFIClassification *ebpf.Map
	CFIClassLengths   *ebpf.Map

	rc *RefcountTable

	// The refcount says who HOLDS a table. It does not say the table is in
	// the BPF maps yet, and those are different facts.
	//
	// AcquireBinary used to return "already installed" the moment the
	// refcount exceeded one - which is true from the instant the FIRST caller
	// bumped it, before that caller had compiled anything or written a single
	// row. With two concurrent registrations of two processes that map the
	// same binary (the gpuprobe startup rendezvous running alongside the lazy
	// worker, for instance) the second one returned instantly, its PID was
	// marked ready, and its stack walks consulted a half-populated table.
	//
	// That failure is invisible in the obvious place: those walks have
	// mappings, so they never count as "no tables". They surface as CFI
	// misses or as an FP-only walk with tables, which is a different symptom
	// with a different suspect.
	//
	// installed/inflight close it. A caller that finds an install in flight
	// waits for it; a caller that finds one finished returns without
	// recompiling; and only the caller that claims `inflight` compiles.
	// Guarded by instMu, with instCond to wake waiters.
	instMu    sync.Mutex
	instCond  *sync.Cond
	installed map[uint64]struct{}
	inflight  map[uint64]struct{}

	// onCompile, if non-nil, is invoked after each successful first-time
	// compile in AcquireBinary. Nil means no observation. Set via
	// SetOnCompile after construction.
	//
	// It runs INSIDE the install claim for that table (see beginInstall), so
	// a hook that called back into this store for the same binary would wait
	// on itself forever. Hooks are for observation - timing, logging,
	// metrics - and must not re-enter.
	onCompile func(path, buildID string, ehFrameBytes int, dur time.Duration)
}

// NewTableStore wires up a TableStore around already-loaded BPF maps
// (typically from the agent's perf_dwarf program load). The caller owns
// the maps; TableStore does not close them.
func NewTableStore(cfi, cfiLen, cls, clsLen *ebpf.Map) *TableStore {
	s := &TableStore{
		CFIRules:          cfi,
		CFILengths:        cfiLen,
		CFIClassification: cls,
		CFIClassLengths:   clsLen,
		rc:                NewRefcountTable(),
		installed:         map[uint64]struct{}{},
		inflight:          map[uint64]struct{}{},
	}
	s.instCond = sync.NewCond(&s.instMu)
	return s
}

// beginInstall claims the right to compile and populate tableID, waiting out
// any install already in flight for it. Returns false when the table is
// already in the maps and the caller has nothing to do.
//
// Broadcast rather than a per-table channel: an install finishing is rare
// (once per distinct binary) and the waiters re-check their own key, so the
// spurious wakeups cost a map lookup and the store keeps no per-table state
// it would have to garbage-collect.
func (s *TableStore) beginInstall(tableID uint64) bool {
	s.instMu.Lock()
	defer s.instMu.Unlock()
	for {
		if _, ok := s.installed[tableID]; ok {
			return false
		}
		if _, ok := s.inflight[tableID]; !ok {
			s.inflight[tableID] = struct{}{}
			return true
		}
		s.instCond.Wait()
	}
}

// endInstall releases the claim. installed is set only when the table is
// actually in the maps, so a failed compile leaves the next caller free to
// try again rather than inheriting a table that was never written.
func (s *TableStore) endInstall(tableID uint64, ok bool) {
	s.instMu.Lock()
	delete(s.inflight, tableID)
	if ok {
		s.installed[tableID] = struct{}{}
	}
	s.instMu.Unlock()
	s.instCond.Broadcast()
}

// forgetInstall drops the "this table is in the maps" fact, for a table that
// has just been evicted from them. ReleaseBinary does the same thing inline,
// under a lock it holds across the map deletions as well; this is the
// standalone spelling the barrier tests drive.
func (s *TableStore) forgetInstall(tableID uint64) {
	s.instMu.Lock()
	delete(s.installed, tableID)
	s.instMu.Unlock()
	s.instCond.Broadcast()
}

// SetOnCompile installs an observer callback that fires after each
// successful CFI compile in AcquireBinary. Pass nil to disable.
// Not safe to call concurrently with AcquireBinary; set once at
// construction time.
//
// The callback runs while this store holds the install claim for the table
// it just compiled, so it MUST NOT call back into the store - AcquireBinary
// for the same binary from inside the hook deadlocks against itself.
func (s *TableStore) SetOnCompile(fn func(path, buildID string, ehFrameBytes int, dur time.Duration)) {
	s.onCompile = fn
}

// AcquireBinary ensures CFI for `binPath` is installed and references
// it on behalf of `pid`. Returns the tableID plus a boolean indicating
// whether a fresh compile happened (false means the refcount was
// simply incremented on an existing table).
//
// binPath is the cache key (the symbolic path, stable across PIDs — two
// processes mapping the same /usr/lib/libc.so.6 share one compile result).
// openPath is the file actually opened for build-id + ehcompile; pass
// "" to use binPath. openPath differs from binPath only when the
// symbolic path is unreachable from the agent's namespace (deleted-but-
// mapped binary, sidecar / mount-namespace cases) and the caller routed
// I/O through /proc/<pid>/map_files.
func (s *TableStore) AcquireBinary(binPath, openPath string, pid uint32) (tableID uint64, compiled bool, err error) {
	if openPath == "" {
		openPath = binPath
	}
	buildID, err := ReadBuildID(openPath)
	if err != nil {
		return 0, false, fmt.Errorf("build-id %s: %w", binPath, err)
	}
	tableID = TableIDForBuildID(buildID)
	s.rc.Acquire(tableID, pid)
	// NOT `if rc > 1 { return }`: the refcount rises before the first holder
	// has written anything, so that test returned "installed" for a table
	// that was still being compiled. See the instMu/installed comment on
	// TableStore. beginInstall waits for the install in flight instead.
	if !s.beginInstall(tableID) {
		return tableID, false, nil // already in the maps, and finished
	}
	// `claimed`, not the named result. Every failure path below returns
	// `0, false, err`, which ASSIGNS the named tableID before this defer
	// runs - so a defer closing over `tableID` would release the claim on
	// table 0 and leave the real one marked in-flight forever. The next
	// AcquireBinary for that build-id would then block in beginInstall with
	// no timeout and no cancellation.
	//
	// That is not a corner case: ehcompile.Compile returns ErrNoEHFrame for
	// any ELF without .eh_frame, which AttachAllMappings expects and skips,
	// so the first process mapping such a library would poison its tableID
	// and the second would wedge - taking the PIDTracker.Run goroutine
	// (mmap tracking AND PID teardown) down with it on any system-wide DWARF
	// run. TestASecondAcquireAfterAFailedInstallDoesNotWedge covers it.
	claimed := tableID
	installOK := false
	defer func() { s.endInstall(claimed, installOK) }()

	// First reference for this tableID — compile + install.
	t0 := time.Now()
	entries, classifications, ehFrameBytes, err := ehcompile.Compile(openPath)
	compileDur := time.Since(t0)
	if err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("ehcompile %s: %w", binPath, err)
	}
	if s.onCompile != nil {
		s.onCompile(binPath, hex.EncodeToString(buildID), ehFrameBytes, compileDur)
	}
	if err := PopulateCFI(PopulateCFIArgs{
		TableID: tableID, Entries: entries,
		OuterMap: s.CFIRules, LengthMap: s.CFILengths,
	}); err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("populate cfi: %w", err)
	}
	if err := PopulateClassification(PopulateClassificationArgs{
		TableID: tableID, Entries: classifications,
		OuterMap: s.CFIClassification, LengthMap: s.CFIClassLengths,
	}); err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("populate classification: %w", err)
	}
	installOK = true
	return tableID, true, nil
}

// ReleaseBinary drops `pid`'s reference to `tableID`. If the refcount
// hits zero, evicts the inner maps (best-effort — eviction errors are
// returned but the refcount is still decremented).
func (s *TableStore) ReleaseBinary(tableID uint64, pid uint32) error {
	if rc := s.rc.Release(tableID, pid); rc > 0 {
		return nil
	}
	// instMu is held across BOTH the forget and the deletes, not just the
	// forget. Dropping it in between leaves a window in which the rows are
	// still in the maps and `installed` no longer says so - harmless - and,
	// worse, a window in which an acquirer that arrived a moment earlier was
	// told "installed" for rows that are about to be deleted. Holding it
	// makes eviction atomic with respect to beginInstall: an acquirer either
	// sees the table before the eviction starts, or compiles it again.
	s.instMu.Lock()
	defer func() {
		s.instMu.Unlock()
		s.instCond.Broadcast()
	}()
	delete(s.installed, tableID)
	// Evict. Deleting from the outer HASH_OF_MAPS drops the kernel's
	// reference to the inner map, which the kernel then frees.
	var firstErr error
	if err := s.CFIRules.Delete(tableID); err != nil {
		firstErr = fmt.Errorf("evict cfi_rules[%#x]: %w", tableID, err)
	}
	if err := s.CFILengths.Delete(tableID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("evict cfi_lengths[%#x]: %w", tableID, err)
	}
	if err := s.CFIClassification.Delete(tableID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("evict cfi_classification[%#x]: %w", tableID, err)
	}
	if err := s.CFIClassLengths.Delete(tableID); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("evict cfi_classification_lengths[%#x]: %w", tableID, err)
	}
	return firstErr
}
