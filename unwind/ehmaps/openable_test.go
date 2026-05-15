package ehmaps

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOpenableBinary_SymbolicPathWorks: when the symbolic path exists and is
// openable, openableBinary returns it. (The map_files probe for an unmapped
// va range fails, so we hit the fallback.)
func TestOpenableBinary_SymbolicPathWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Use a va range that almost certainly has no mapping in our PID.
	got := openableBinary(uint32(os.Getpid()), 0xdeadbeef00000000, 0xdeadbeef00001000, path)
	if got != path {
		t.Errorf("openableBinary = %q; want symbolic path %q (map_files fallback expected)", got, path)
	}
}

// TestOpenableBinary_MapFilesFallback: when the symbolic path is gone but
// /proc/self/map_files/<start>-<limit> exists (for one of our own mappings),
// openableBinary returns that map_files path.
//
// Reading /proc/<pid>/map_files requires CAP_SYS_ADMIN (or CAP_CHECKPOINT_RESTORE
// on newer kernels). When the test binary has neither, we can't exercise the
// fallback path — skip rather than report a meaningless failure.
func TestOpenableBinary_MapFilesFallback(t *testing.T) {
	start, limit, ok := pickLiveExecMapping(t)
	if !ok {
		t.Skip("no executable file-backed mapping in /proc/self/maps")
	}
	probe := fmt.Sprintf("/proc/%d/map_files/%x-%x", os.Getpid(), start, limit)
	if f, err := os.Open(probe); err != nil {
		t.Skipf("map_files unreadable without CAP_SYS_ADMIN/CAP_CHECKPOINT_RESTORE: %v", err)
	} else {
		_ = f.Close()
	}

	bogus := filepath.Join(t.TempDir(), "definitely-not-here")
	got := openableBinary(uint32(os.Getpid()), start, limit, bogus)
	if got != probe {
		t.Errorf("openableBinary = %q; want map_files %q (symbolic %q was missing)", got, probe, bogus)
	}
}

// TestOpenableBinary_BothMissing: returns "" when neither resolves.
func TestOpenableBinary_BothMissing(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "not-here")
	got := openableBinary(uint32(os.Getpid()), 0xdeadbeef00000000, 0xdeadbeef00001000, bogus)
	if got != "" {
		t.Errorf("openableBinary = %q; want \"\"", got)
	}
}

// pickLiveExecMapping scans /proc/self/maps for an executable file-backed
// mapping whose symbolic path is a regular file, and returns its va range.
func pickLiveExecMapping(t *testing.T) (start, limit uint64, ok bool) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("read maps: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		path := fields[5]
		if path == "" || strings.HasPrefix(path, "[") {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		s, err1 := strconv.ParseUint(fields[0][:dash], 16, 64)
		l, err2 := strconv.ParseUint(fields[0][dash+1:], 16, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return s, l, true
	}
	return 0, 0, false
}
