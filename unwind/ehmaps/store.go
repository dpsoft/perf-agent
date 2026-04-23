package ehmaps

import (
	"fmt"
	"sync"

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
// tracking with actual map population. It is the S4 replacement for the
// hand-wired calls to PopulateCFI/PopulateClassification in S3's tests.
type TableStore struct {
	CFIRules          *ebpf.Map
	CFILengths        *ebpf.Map
	CFIClassification *ebpf.Map
	CFIClassLengths   *ebpf.Map

	rc *RefcountTable
}

// NewTableStore wires up a TableStore around already-loaded BPF maps
// (typically from the agent's perf_dwarf program load). The caller owns
// the maps; TableStore does not close them.
func NewTableStore(cfi, cfiLen, cls, clsLen *ebpf.Map) *TableStore {
	return &TableStore{
		CFIRules:          cfi,
		CFILengths:        cfiLen,
		CFIClassification: cls,
		CFIClassLengths:   clsLen,
		rc:                NewRefcountTable(),
	}
}

// AcquireBinary ensures CFI for `binPath` is installed and references
// it on behalf of `pid`. Returns the tableID plus a boolean indicating
// whether a fresh compile happened (false means the refcount was
// simply incremented on an existing table).
func (s *TableStore) AcquireBinary(binPath string, pid uint32) (tableID uint64, compiled bool, err error) {
	buildID, err := ReadBuildID(binPath)
	if err != nil {
		return 0, false, fmt.Errorf("build-id %s: %w", binPath, err)
	}
	tableID = TableIDForBuildID(buildID)
	if rc := s.rc.Acquire(tableID, pid); rc > 1 {
		return tableID, false, nil // already installed
	}
	// First reference for this tableID — compile + install.
	entries, classifications, err := ehcompile.Compile(binPath)
	if err != nil {
		s.rc.Release(tableID, pid)
		return 0, false, fmt.Errorf("ehcompile %s: %w", binPath, err)
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
	return tableID, true, nil
}

// ReleaseBinary drops `pid`'s reference to `tableID`. If the refcount
// hits zero, evicts the inner maps (best-effort — eviction errors are
// returned but the refcount is still decremented).
func (s *TableStore) ReleaseBinary(tableID uint64, pid uint32) error {
	if rc := s.rc.Release(tableID, pid); rc > 0 {
		return nil
	}
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
