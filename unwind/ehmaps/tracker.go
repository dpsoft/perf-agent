package ehmaps

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
)

// PIDTracker holds per-PID state for the hybrid unwinder. Each Attach
// populates pid_mappings for that PID and takes a TableStore reference
// for every unique binary in the process's address space. Detach
// reverses both.
//
// Attach is called once per binary in the target's address space.
// Subsequent calls for the same PID with a different binPath append
// to the pid_mappings array. The integration test exercises
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

// EnrollWithoutCompile populates pid_mappings for binPath under pid
// WITHOUT compiling CFI. Used by the lazy mode (Option A2) to give the
// walker enough mapping info to classify per-frame, deferring the
// expensive ehcompile.Compile call to the first sample miss.
//
// buildIDCache is shared across calls so each unique binary's build-id
// is read exactly once across all PIDs. Caller owns the cache.
//
// Does NOT increment the TableStore refcount — compile-time refcounting
// is handled by AttachCompileOnly when the drainer compiles on demand.
func (t *PIDTracker) EnrollWithoutCompile(pid uint32, binPath string, buildIDCache map[string][]byte) error {
	buildID, ok := buildIDCache[binPath]
	if !ok {
		var err error
		buildID, err = ReadBuildID(binPath)
		if err != nil {
			return fmt.Errorf("build-id %s: %w", binPath, err)
		}
		buildIDCache[binPath] = buildID
	}
	tableID := TableIDForBuildID(buildID)

	newMappings, err := LoadProcessMappings(int(pid), binPath, tableID)
	if err != nil {
		return fmt.Errorf("load mappings pid=%d: %w", pid, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	st, exists := t.perPID[pid]
	if !exists {
		st = &pidState{tableIDs: map[uint64]struct{}{}}
		t.perPID[pid] = st
	}
	st.mappings = append(st.mappings, newMappings...)
	// NOTE: do NOT add tableID to st.tableIDs — no refcount taken.
	// AttachCompileOnly will record it when the drainer compiles on demand.
	return PopulatePIDMappings(PopulatePIDMappingsArgs{
		PID: pid, Mappings: st.mappings,
		OuterMap: t.pidMappings, LengthMap: t.pidMapLens,
	})
}

// AttachCompileOnly compiles CFI for binPath and registers the tableID
// for refcount-tracked release. Assumes pid_mappings already has an
// entry for this binary (set by a prior EnrollWithoutCompile). Used by
// the lazy CFI miss drainer.
//
// Skips LoadProcessMappings + PopulatePIDMappings — the enrolled state
// already covers them. Calling this for a binary that was NOT enrolled
// would leave pid_mappings empty for it; the walker would still hit
// MAPPING_NOT_FOUND and fall through to FP-only.
func (t *PIDTracker) AttachCompileOnly(pid uint32, binPath string) error {
	tableID, _, err := t.store.AcquireBinary(binPath, pid)
	if err != nil {
		return fmt.Errorf("acquire %s: %w", binPath, err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.perPID[pid]
	if !ok {
		st = &pidState{tableIDs: map[uint64]struct{}{}}
		t.perPID[pid] = st
	}
	st.tableIDs[tableID] = struct{}{}
	return nil
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
	if t.pidMappings != nil {
		if err := t.pidMappings.Delete(pid); err != nil {
			firstErr = fmt.Errorf("delete pid_mappings[%d]: %w", pid, err)
		}
	}
	if t.pidMapLens != nil {
		if err := t.pidMapLens.Delete(pid); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete pid_mapping_lengths[%d]: %w", pid, err)
		}
	}
	if t.store != nil {
		for tid := range st.tableIDs {
			if err := t.store.ReleaseBinary(tid, pid); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// mmapEventSource is the shape both *MmapWatcher and
// *MultiCPUMmapWatcher satisfy — Events() returns a read-only channel
// of event records.
type mmapEventSource interface {
	Events() <-chan MmapEventRecord
}

// Run blocks consuming events from the watcher until ctx is canceled or
// the watcher's event channel closes. Call from a goroutine. On MmapEvent
// with an executable filename, auto-attaches the PID if we haven't seen
// that (pid, path) already. On ExitEvent (group-leader only), detaches.
//
// Observers (if any) run BEFORE the tracker's own dispatch for each
// event — they see every event including those the tracker itself
// would filter out. Used by dwarfagent.session to keep a procmap
// Resolver's cache in sync with MMAP2/EXIT events.
func (t *PIDTracker) Run(ctx context.Context, w mmapEventSource, observers ...func(MmapEventRecord)) {
	seen := map[uint32]map[string]struct{}{} // pid → set of paths already attached
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			for _, obs := range observers {
				obs(ev)
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
			case ForkEvent:
				// Only act on group-leader fork (whole new process).
				// Per-thread FORKs fire for every clone inside an
				// existing tracked TGID — no-op those.
				if ev.TID != ev.PID {
					continue
				}
				n, err := AttachAllMappings(t, ev.PID)
				if err != nil || n == 0 {
					slog.Debug("ehmaps: fork Attach failed", "pid", ev.PID, "err", err)
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

// AttachAllMappings walks /proc/<pid>/maps, finds every file-backed
// executable mapping, and calls tracker.Attach once per unique binary
// path. Returns the count of distinct binaries attached.
//
// Call this once at agent startup to cover the main binary plus every
// shared library present at that moment. Subsequent mmaps (dlopen,
// runtime-loaded plugins) are handled by MmapWatcher driving
// PIDTracker.Run.
//
// Attach failures for individual binaries are logged at Debug and
// skipped rather than fatal — a process may have exotic mappings
// (ehcompile-rejectable formats, ELFs without .eh_frame) that we
// shouldn't fail the whole attach on. The first failure is returned
// only if NO binary was successfully attached; otherwise we report
// whatever we managed.
func AttachAllMappings(t *PIDTracker, pid uint32) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/maps: %w", pid, err)
	}
	seen := map[string]struct{}{}
	var firstErr error
	n := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		path := fields[5]
		if path == "" || strings.HasPrefix(path, "[") || strings.HasPrefix(path, "//anon") {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := t.Attach(pid, path); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("attach %s: %w", path, err)
			} else {
				slog.Debug("ehmaps: AttachAllMappings: skip", "path", path, "err", err)
			}
			continue
		}
		n++
	}
	if n == 0 && firstErr != nil {
		return 0, firstErr
	}
	return n, nil
}

// AttachAllProcesses walks /proc/* and calls AttachAllMappings for every
// numeric PID directory that still has a live /proc/<pid>/maps.
// Returns (pidCount, distinctBinaryCount, err). The distinct-binary
// count comes from observing the TableStore's CFIRules outer map before
// and after the scan; the walker tolerates individual PID failures
// (process vanished between listdir and open).
//
// Intended for system-wide startup. After this returns, follow-up
// tracking relies on per-CPU MmapWatchers + FORK events.
func AttachAllProcesses(t *PIDTracker) (pids, tables int, err error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc: %w", err)
	}
	beforeCFI := countCFIRules(t.store)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		if pid == 0 {
			continue
		}
		// Skip self so the agent doesn't track itself — it has no
		// actionable samples from its own code in practice.
		if int(pid) == os.Getpid() {
			continue
		}
		n, err := AttachAllMappings(t, uint32(pid))
		if err != nil || n == 0 {
			slog.Debug("ehmaps: AttachAllProcesses: skip", "pid", pid, "err", err)
			continue
		}
		pids++
	}
	afterCFI := countCFIRules(t.store)
	return pids, afterCFI - beforeCFI, nil
}

// countCFIRules returns the number of distinct table_ids currently
// present in the TableStore's CFIRules outer map.
func countCFIRules(s *TableStore) int {
	if s == nil || s.CFIRules == nil {
		return 0
	}
	it := s.CFIRules.Iterate()
	var k uint64
	var v uint32
	n := 0
	for it.Next(&k, &v) {
		n++
	}
	return n
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
