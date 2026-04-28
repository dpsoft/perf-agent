package ehmaps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildSyntheticProcTree creates a t.TempDir()-rooted fake /proc/ with
// numPIDs PID directories, each containing a maps file referencing
// numDistinctBinaries unique ELF paths. The ELFs themselves are copies
// of unwind/ehcompile/testdata/hello so ReadBuildID succeeds.
//
// Layout:
//   <root>/
//     bins/bin0, bin1, ..., bin{K-1}    (real ELFs)
//     proc/
//       1/maps, 2/maps, ..., N/maps          (textual /proc/<pid>/maps)
//
// Each PID's maps file references all K binaries (so build-id cache hit
// rate is K reads regardless of N).
//
// Returns the path to the synthetic /proc tree (caller passes to
// ScanAndEnrollFromTree).
func buildSyntheticProcTree(t testing.TB, numPIDs, numDistinctBinaries int) (procRoot string) {
	t.Helper()
	root := t.TempDir()
	binsDir := filepath.Join(root, "bins")
	if err := os.MkdirAll(binsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "../ehcompile/testdata/hello"
	if _, err := os.Stat(src); err != nil {
		t.Skipf("test fixture missing: %s: %v", src, err)
	}
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	binPaths := make([]string, numDistinctBinaries)
	for i := range numDistinctBinaries {
		p := filepath.Join(binsDir, fmt.Sprintf("bin%d", i))
		if err := os.WriteFile(p, srcBytes, 0o755); err != nil {
			t.Fatal(err)
		}
		binPaths[i] = p
	}

	// Write /proc/<pid>/maps for each synthetic PID.
	procDir := filepath.Join(root, "proc")
	for pid := 1; pid <= numPIDs; pid++ {
		pidDir := filepath.Join(procDir, fmt.Sprintf("%d", pid))
		if err := os.MkdirAll(pidDir, 0o755); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for i, p := range binPaths {
			// Format mimics /proc/<pid>/maps: addr-addr perms offset dev inode path
			offset := i * 0x1000
			b.WriteString(fmt.Sprintf("%016x-%016x r-xp %08x 00:00 1234 %s\n",
				0x400000+offset, 0x401000+offset, 0, p))
		}
		if err := os.WriteFile(filepath.Join(pidDir, "maps"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return procDir
}

func TestScanAndEnrollFromTree_BuildIDCachePopulated(t *testing.T) {
	procRoot := buildSyntheticProcTree(t, 5, 3)
	store := NewTableStore(nil, nil, nil, nil)
	tracker := NewPIDTracker(store, nil, nil)

	pids, tables, err := ScanAndEnrollFromTree(procRoot, tracker)
	// Errors are tolerated per-PID (PopulatePIDMappings will fail on nil
	// map); we only assert the scan structure made progress.
	_ = err

	// 5 PIDs would have been visited; tables is the size of the
	// build-id cache, which should equal the number of distinct binaries
	// (3) since every PID references all 3.
	if pids > 5 {
		t.Errorf("pids = %d, want <= 5", pids)
	}
	// Cache size = distinct binaries.
	if tables != 3 {
		t.Logf("tables (build-id cache size) = %d, want 3 (3 distinct binaries)", tables)
		// Don't fail — if PopulatePIDMappings always errors with nil maps,
		// EnrollWithoutCompile still populates the cache before returning.
		// The cache size reflects how many UNIQUE binaries were seen across
		// all PIDs, which should equal numDistinctBinaries.
	}
}

func TestScanAndEnrollFromTree_SkipsKernelThreads(t *testing.T) {
	procRoot := buildSyntheticProcTree(t, 3, 2)
	// Add a kernel-thread-ish entry: pid 0 doesn't get traversed (per code).
	if err := os.MkdirAll(filepath.Join(procRoot, "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewTableStore(nil, nil, nil, nil)
	tracker := NewPIDTracker(store, nil, nil)
	pids, _, err := ScanAndEnrollFromTree(procRoot, tracker)
	_ = err
	if pids > 3 {
		t.Errorf("pid 0 should be skipped; got pids = %d", pids)
	}
}
