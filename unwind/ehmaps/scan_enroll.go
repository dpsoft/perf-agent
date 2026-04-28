package ehmaps

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ScanAndEnroll walks /proc/* and populates pid_mappings entries for
// every executable mapping of every PID, WITHOUT compiling CFI. Returns
// (pidCount, distinctBinaryCount, err).
//
// Used by --unwind auto -a (Option A2 lazy mode). The deferred compile
// happens via AttachCompileOnly when the BPF walker emits a miss event
// for a sampled (pid, table_id) pair.
//
// Build-id reads are cached so each unique binary's build-id is parsed
// exactly once across all PIDs. ~30,000× cheaper than AttachAllProcesses
// on typical desktops (100s of µs vs tens of seconds).
func ScanAndEnroll(t *PIDTracker) (pids, tables int, err error) {
	return ScanAndEnrollFromTree("/proc", t)
}

// ScanAndEnrollFromTree is the testable variant of ScanAndEnroll: takes
// the proc-tree root as a parameter so unit tests can run against a
// synthetic tree built in t.TempDir().
func ScanAndEnrollFromTree(procRoot string, t *PIDTracker) (pids, tables int, err error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", procRoot, err)
	}
	buildIDCache := map[string][]byte{}
	self := os.Getpid()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil || pid == 0 || int(pid) == self {
			continue
		}
		n, err := enrollPIDFromTree(procRoot, t, uint32(pid), buildIDCache)
		if err != nil || n == 0 {
			slog.Debug("ehmaps: ScanAndEnrollFromTree: skip", "pid", pid, "err", err)
			continue
		}
		pids++
	}
	return pids, len(buildIDCache), nil
}

// enrollPIDFromTree reads procRoot/<pid>/maps and calls
// EnrollWithoutCompile once per unique binary path.
func enrollPIDFromTree(procRoot string, t *PIDTracker, pid uint32, cache map[string][]byte) (int, error) {
	mapsPath := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10), "maps")
	data, err := os.ReadFile(mapsPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", mapsPath, err)
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
		if err := t.EnrollWithoutCompile(pid, path, cache); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("enroll %s: %w", path, err)
			} else {
				slog.Debug("ehmaps: enrollPIDFromTree: skip", "path", path, "err", err)
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
