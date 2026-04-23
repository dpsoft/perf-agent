package ehmaps

import (
	"fmt"
	"sync"

	"github.com/cilium/ebpf"
)

// PIDTracker holds per-PID state for the hybrid unwinder. Each Attach
// populates pid_mappings for that PID and takes a TableStore reference
// for every unique binary in the process's address space. Detach
// reverses both.
//
// S4 scope: Attach is called once per binary in the target's address
// space. Subsequent calls for the same PID with a different binPath
// append to the pid_mappings array. The S4 integration test exercises
// the full flow via MmapWatcher events driving Attach automatically.
type PIDTracker struct {
	store       *TableStore
	pidMappings *ebpf.Map
	pidMapLens  *ebpf.Map

	mu     sync.Mutex
	perPID map[uint32]*pidState
}

type pidState struct {
	mappings []PIDMapping
	tableIDs map[uint64]struct{}
}

// NewPIDTracker wires a tracker around an already-loaded set of BPF maps.
// Caller owns the maps; the tracker does not close them.
func NewPIDTracker(store *TableStore, pidMappings, pidMapLengths *ebpf.Map) *PIDTracker {
	return &PIDTracker{
		store:       store,
		pidMappings: pidMappings,
		pidMapLens:  pidMapLengths,
		perPID:      map[uint32]*pidState{},
	}
}

// Attach walks /proc/<pid>/maps for binPath, acquires CFI via the store,
// and installs a pid_mappings row. Safe to call multiple times with
// different binPaths for the same PID — mappings accumulate.
func (t *PIDTracker) Attach(pid uint32, binPath string) error {
	tableID, _, err := t.store.AcquireBinary(binPath, pid)
	if err != nil {
		return fmt.Errorf("acquire %s: %w", binPath, err)
	}
	newMappings, err := LoadProcessMappings(int(pid), binPath, tableID)
	if err != nil {
		_ = t.store.ReleaseBinary(tableID, pid)
		return fmt.Errorf("load mappings pid=%d: %w", pid, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.perPID[pid]
	if !ok {
		st = &pidState{tableIDs: map[uint64]struct{}{}}
		t.perPID[pid] = st
	}
	st.mappings = append(st.mappings, newMappings...)
	st.tableIDs[tableID] = struct{}{}
	return PopulatePIDMappings(PopulatePIDMappingsArgs{
		PID: pid, Mappings: st.mappings,
		OuterMap: t.pidMappings, LengthMap: t.pidMapLens,
	})
}

// Detach removes the PID from the pid_mappings map and releases all
// binaries it held. Safe to call for an unknown PID (no-op).
func (t *PIDTracker) Detach(pid uint32) error {
	t.mu.Lock()
	st, ok := t.perPID[pid]
	if !ok {
		t.mu.Unlock()
		return nil
	}
	delete(t.perPID, pid)
	t.mu.Unlock()

	var firstErr error
	if err := t.pidMappings.Delete(pid); err != nil {
		firstErr = fmt.Errorf("delete pid_mappings[%d]: %w", pid, err)
	}
	if err := t.pidMapLens.Delete(pid); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("delete pid_mapping_lengths[%d]: %w", pid, err)
	}
	for tid := range st.tableIDs {
		if err := t.store.ReleaseBinary(tid, pid); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
