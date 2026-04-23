package ehmaps

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// Run blocks consuming events from the watcher until ctx is canceled or
// the watcher's event channel closes. Call from a goroutine. On MmapEvent
// with an executable filename, auto-attaches the PID if we haven't seen
// that (pid, path) already. On ExitEvent (group-leader only), detaches.
func (t *PIDTracker) Run(ctx context.Context, w *MmapWatcher) {
	seen := map[uint32]map[string]struct{}{} // pid → set of paths already attached
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			switch ev.Kind {
			case MmapEvent:
				if !looksExecutable(ev) {
					continue
				}
				bucket, present := seen[ev.PID]
				if !present {
					bucket = map[string]struct{}{}
					seen[ev.PID] = bucket
				}
				if _, already := bucket[ev.Filename]; already {
					continue
				}
				bucket[ev.Filename] = struct{}{}
				if err := t.Attach(ev.PID, ev.Filename); err != nil {
					slog.Debug("ehmaps: Attach failed", "pid", ev.PID, "path", ev.Filename, "err", err)
				}
			case ExitEvent:
				// Only act on group-leader exit (whole process gone).
				// Per-thread exits still fire PERF_RECORD_EXIT but leave
				// the process alive; detaching on those would break
				// tracking mid-process.
				if ev.TID != ev.PID {
					continue
				}
				delete(seen, ev.PID)
				if err := t.Detach(ev.PID); err != nil {
					slog.Debug("ehmaps: Detach failed", "pid", ev.PID, "err", err)
				}
			}
		}
	}
}

// looksExecutable filters MMAP2 events down to those worth attaching to.
// Must be an executable mapping (PROT_EXEC), have a real filename
// (non-empty, not an anonymous or special kernel path), and the file
// must exist and be a regular file.
func looksExecutable(ev MmapEventRecord) bool {
	const protExec = 0x4
	if ev.Prot&protExec == 0 {
		return false
	}
	if ev.Filename == "" {
		return false
	}
	if strings.HasPrefix(ev.Filename, "[") || strings.HasPrefix(ev.Filename, "//anon") {
		return false
	}
	clean := filepath.Clean(ev.Filename)
	info, err := os.Stat(clean)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return true
}
