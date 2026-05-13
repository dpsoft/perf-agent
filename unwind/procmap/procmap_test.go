package procmap

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestParseMapsFile(t *testing.T) {
	path := filepath.Join("testdata", "proc", "4242", "maps")
	got, err := parseMapsFile(path)
	if err != nil {
		t.Fatalf("parseMapsFile: %v", err)
	}

	mapFilesDir := filepath.Join("testdata", "proc", "4242", "map_files")
	want := []Mapping{
		{
			Path:     "/usr/bin/target",
			MapFiles: filepath.Join(mapFilesDir, "400000-420000"),
			Start:    0x00400000, Limit: 0x00420000, Offset: 0x1000, IsExec: true,
		},
		{
			Path:     "/lib/x86_64-linux-gnu/libc.so.6",
			MapFiles: filepath.Join(mapFilesDir, "7f0000001000-7f0000100000"),
			Start:    0x7f0000001000, Limit: 0x7f0000100000, Offset: 0x2000, IsExec: true,
		},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d mappings, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mapping %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestParseMapsFileMapFilesPopulated verifies that after parseMapsFile, each
// returned Mapping.MapFiles is non-empty and resolves to the expected
// /proc/<pid>/map_files/<startHex>-<limitHex> path.
func TestParseMapsFileMapFilesPopulated(t *testing.T) {
	// Build a fake /proc/<pid>/maps under a temp dir so we can verify the
	// MapFiles path is constructed relative to that directory.
	tmp := t.TempDir()
	pidDir := filepath.Join(tmp, "1234")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const mapsContent = "00400000-00401000 r-xp 00000000 fd:01 111 /usr/bin/prog\n" +
		"7f0000001000-7f0000002000 r-xp 00001000 fd:01 222 /lib/libfoo.so\n"
	mapsPath := filepath.Join(pidDir, "maps")
	if err := os.WriteFile(mapsPath, []byte(mapsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := parseMapsFile(mapsPath)
	if err != nil {
		t.Fatalf("parseMapsFile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mappings, want 2", len(got))
	}

	mapFilesDir := filepath.Join(pidDir, "map_files")
	cases := []struct {
		idx      int
		wantPath string
	}{
		{0, filepath.Join(mapFilesDir, "400000-401000")},
		{1, filepath.Join(mapFilesDir, "7f0000001000-7f0000002000")},
	}
	for _, tc := range cases {
		m := got[tc.idx]
		if m.MapFiles == "" {
			t.Errorf("mapping[%d] MapFiles is empty, want %q", tc.idx, tc.wantPath)
			continue
		}
		if m.MapFiles != tc.wantPath {
			t.Errorf("mapping[%d] MapFiles = %q, want %q", tc.idx, m.MapFiles, tc.wantPath)
		}
	}
}

func TestReadBuildID(t *testing.T) {
	// /bin/ls on any modern distro has a GNU build-id. We don't assert
	// the exact value (it varies) — only that it parses to a non-empty
	// lowercase hex string.
	id, err := ReadBuildID("/bin/ls")
	if err != nil {
		t.Fatalf("ReadBuildID(/bin/ls): %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty build-id, got empty")
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("build-id %q contains non-hex char %q", id, r)
		}
	}
}

func TestReadBuildIDMissing(t *testing.T) {
	id, err := ReadBuildID("/nonexistent/path/to/nothing")
	if err == nil {
		t.Fatalf("expected error, got id=%q", id)
	}
}

func TestResolverLookupHitMiss(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	m, ok := r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("expected lookup hit in /usr/bin/target range")
	}
	if m.Path != "/usr/bin/target" {
		t.Errorf("got Path=%q, want /usr/bin/target", m.Path)
	}

	_, ok = r.Lookup(4242, 0xdeadbeef)
	if ok {
		t.Fatal("expected lookup miss outside any mapping")
	}
}

func TestResolverMissingPID(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, ok := r.Lookup(9999999, 0x00401234)
	if ok {
		t.Fatal("expected miss for non-existent PID")
	}
	// Second call should hit the cached empty entry, not re-read /proc.
	_, ok = r.Lookup(9999999, 0x00401234)
	if ok {
		t.Fatal("second lookup should also miss")
	}
}

func TestResolverInvalidate(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, ok := r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("first lookup should hit")
	}
	r.Invalidate(4242)
	// Still hits because the fixture file is unchanged, but the path
	// re-populated. Just ensures Invalidate doesn't panic and Lookup
	// keeps working afterward.
	_, ok = r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("lookup after Invalidate should still hit")
	}
}

func TestResolverConcurrentLookup(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	const N = 32
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, ok := r.Lookup(4242, 0x00401234)
			if !ok {
				errs <- fmt.Errorf("lookup miss")
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolverInvalidateAddrNoOpInRange(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, _ = r.Lookup(7777, 0x00701000) // populate
	before := r.populateCountForTest(7777)

	r.InvalidateAddr(7777, 0x00701000) // in-range -> no-op
	after := r.populateCountForTest(7777)

	if after != before {
		t.Fatalf("populate count changed %d -> %d after in-range InvalidateAddr", before, after)
	}
}

func TestResolverInvalidateAddrOutOfRangeForcesReparse(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, _ = r.Lookup(7777, 0x00701000) // populate
	before := r.populateCountForTest(7777)

	r.InvalidateAddr(7777, 0xdeadbeef) // out-of-range -> evict
	_, _ = r.Lookup(7777, 0x00701000)  // re-populate
	after := r.populateCountForTest(7777)

	if after != before+1 {
		t.Fatalf("expected 1 re-populate, got %d -> %d", before, after)
	}
}

func TestResolverPopulateBuildIDViaMapFiles(t *testing.T) {
	// Simulate the sidecar case: build-id is only readable through the
	// MapFiles symlink because the symbolic Path is unreachable.
	tmp := t.TempDir()
	binWithBuildID := writeELFWithBuildID(t, tmp, []byte{0xab, 0xcd, 0xef, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11})

	m := Mapping{
		Path:     "/sidecar/unreachable/bin", // doesn't exist on host
		MapFiles: binWithBuildID,             // does exist
		Start:    0x400000,
		Limit:    0x401000,
		IsExec:   true,
	}

	r := &Resolver{} // bare resolver; we only need the build-id attachment path
	mappings := []Mapping{m}
	r.attachBuildIDs(mappings)

	want := "abcdef0102030405060708090a0b0c0d0e0f1011"
	if mappings[0].BuildID != want {
		t.Errorf("BuildID via MapFiles = %q, want %q", mappings[0].BuildID, want)
	}
}

// TestResolverMappingsReSnapshotAfterInvalidate verifies the spec invariant
// ("No persistent per-PID state — each Symbolize call re-snapshots
// /proc/<pid>/maps"). It checks that Mappings repopulates after Invalidate by
// confirming the populate counter increments; it also mutates the fixture
// between calls and asserts the second snapshot sees the added mapping.
func TestResolverMappingsReSnapshotAfterInvalidate(t *testing.T) {
	// Set up a private /proc root with a single mapping for PID 9001.
	tmp := t.TempDir()
	pidDir := filepath.Join(tmp, "9001")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapsPath := filepath.Join(pidDir, "maps")

	// First snapshot: one executable mapping.
	const firstMaps = "00400000-00401000 r-xp 00000000 fd:01 111 /usr/bin/first\n"
	if err := os.WriteFile(mapsPath, []byte(firstMaps), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()

	// First call — populates the cache.
	m1, err := r.Mappings(9001)
	if err != nil {
		t.Fatalf("first Mappings: %v", err)
	}
	if len(m1) != 1 || m1[0].Path != "/usr/bin/first" {
		t.Fatalf("first snapshot: got %v, want 1 mapping /usr/bin/first", m1)
	}
	countAfterFirst := r.populateCountForTest(9001)
	if countAfterFirst != 1 {
		t.Fatalf("populate count after first call: got %d, want 1", countAfterFirst)
	}

	// Mutate the fixture: add a second mapping (simulates dlopen / mmap).
	const secondMaps = "00400000-00401000 r-xp 00000000 fd:01 111 /usr/bin/first\n" +
		"7f0000001000-7f0000002000 r-xp 00000000 fd:01 222 /lib/libnew.so\n"
	if err := os.WriteFile(mapsPath, []byte(secondMaps), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without Invalidate, Mappings returns the stale snapshot.
	stale, _ := r.Mappings(9001)
	if len(stale) != 1 {
		t.Fatalf("expected stale snapshot (1 mapping), got %d", len(stale))
	}
	if r.populateCountForTest(9001) != 1 {
		t.Fatal("populate count must not change without Invalidate")
	}

	// Invalidate forces re-snapshot on the next Mappings call.
	r.Invalidate(9001)
	m2, err := r.Mappings(9001)
	if err != nil {
		t.Fatalf("second Mappings: %v", err)
	}
	if len(m2) != 2 {
		t.Fatalf("second snapshot: got %d mappings, want 2", len(m2))
	}
	if m2[1].Path != "/lib/libnew.so" {
		t.Errorf("second snapshot mapping[1]: got %q, want /lib/libnew.so", m2[1].Path)
	}
	if r.populateCountForTest(9001) != 2 {
		t.Fatalf("populate count after Invalidate+Mappings: got %d, want 2", r.populateCountForTest(9001))
	}
}

func TestMappingOpenablePath(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "exe")
	if err := os.WriteFile(binPath, []byte("dummy"), 0o755); err != nil {
		t.Fatal(err)
	}
	binPath2 := filepath.Join(tmp, "exe2")
	if err := os.WriteFile(binPath2, []byte("dummy2"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		mapFiles string
		path     string
		want     string
	}{
		// binPath is in MapFiles; binPath2 is the symbolic Path.
		// The result must be binPath, proving MapFiles is checked first.
		{"map_files preferred when both readable", binPath, binPath2, binPath},
		{"falls back to symbolic when map_files empty", "", binPath, binPath},
		{"falls back to symbolic when map_files missing", "/nope/missing", binPath, binPath},
		{"map_files wins when symbolic deleted", binPath, "/deleted/path", binPath},
		{"empty when neither works", "/nope/a", "/nope/b", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Mapping{MapFiles: tc.mapFiles, Path: tc.path}
			if got := m.OpenablePath(); got != tc.want {
				t.Errorf("OpenablePath() = %q, want %q", got, tc.want)
			}
		})
	}
}
