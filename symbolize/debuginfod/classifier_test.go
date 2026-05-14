package debuginfod

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestClassifierSkipPaths(t *testing.T) {
	c := newClassifier(nil /* cache unused for skip-path tests */, nil /* fetcher unused */, &atomicStats{})
	skipPaths := []string{"", "[vdso]", "[stack]", "[vsyscall]", "[heap]"}
	for _, p := range skipPaths {
		t.Run(p, func(t *testing.T) {
			m := procmap.Mapping{Path: p}
			got := c.classify(t.Context(), m)
			if got.route != routeSkip {
				t.Errorf("classify(%q) route = %v, want routeSkip", p, got.route)
			}
		})
	}
}

func TestClassifierTier2ProcessMode(t *testing.T) {
	tmp := t.TempDir()

	dwarfPresent := filepath.Join(tmp, "dwarf-bin")
	if err := writeELFWithSection(dwarfPresent, ".debug_info", []byte("placeholder dwarf payload")); err != nil {
		t.Fatal(err)
	}

	debugLinkBin := filepath.Join(tmp, "linked-bin")
	linkeeName := "linked-bin.debug"
	linkeePath := filepath.Join(tmp, linkeeName)
	if err := os.WriteFile(linkeePath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	// Payload layout: NUL-terminated filename, padded to 4 bytes, then 4-byte CRC32.
	payload := append([]byte(linkeeName), 0)
	for len(payload)%4 != 0 {
		payload = append(payload, 0)
	}
	payload = append(payload, 0xde, 0xad, 0xbe, 0xef) // dummy CRC32
	if err := writeELFWithSection(debugLinkBin, ".gnu_debuglink", payload); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(nil, nil, &atomicStats{})
	cases := []struct {
		name string
		m    procmap.Mapping
		want routeKind
	}{
		{"dwarf in binary", procmap.Mapping{Path: dwarfPresent}, routeProcessMode},
		{"resolvable debug-link", procmap.Mapping{Path: debugLinkBin}, routeProcessMode},
		{"unreadable file-like path", procmap.Mapping{Path: "/nope/missing"}, routeProcessMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.classify(t.Context(), tc.m)
			if got.route != tc.want {
				t.Errorf("route = %v, want %v", got.route, tc.want)
			}
		})
	}
}

func TestClassifierTier3FileModeCacheHit(t *testing.T) {
	tmp := t.TempDir()
	buildIDHex := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	binPath := writeStrippedELFWithBuildID(t, filepath.Join(tmp, "exe"), buildIDHex)

	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.NewSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cc := &cache.Cache{Dir: cacheDir, Index: idx}

	// Stage a fake .debug at the cache's .build-id layout.
	debugDir := filepath.Join(cacheDir, ".build-id", buildIDHex[:2])
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(debugDir, buildIDHex[2:]+".debug")
	if err := os.WriteFile(debugPath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Touch(buildIDHex, cache.KindDebuginfo, 4); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(cc, nil, &atomicStats{}) // no fetcher needed — cache hit
	got := c.classify(t.Context(), procmap.Mapping{Path: binPath})
	if got.route != routeFileMode {
		t.Errorf("route = %v, want routeFileMode", got.route)
	}
	if got.debugPath != debugPath {
		t.Errorf("debugPath = %q, want %q", got.debugPath, debugPath)
	}
}

func TestClassifierBadDebugFiltersCacheCopy(t *testing.T) {
	tmp := t.TempDir()
	buildIDHex := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	binPath := writeStrippedELFWithBuildID(t, filepath.Join(tmp, "exe"), buildIDHex)

	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.NewSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cc := &cache.Cache{Dir: cacheDir, Index: idx}

	debugDir := filepath.Join(cacheDir, ".build-id", buildIDHex[:2])
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(debugDir, buildIDHex[2:]+".debug")
	if err := os.WriteFile(debugPath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Touch(buildIDHex, cache.KindDebuginfo, 4); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(cc, nil, &atomicStats{})
	// Mark the cached file as bad via its signature.
	sig, err := statSig(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	c.markBadDebug(sig)

	got := c.classify(t.Context(), procmap.Mapping{Path: binPath})
	if got.route != routeProcessMode {
		t.Errorf("route with badDebug-blocked cache and no other candidate = %v, want routeProcessMode", got.route)
	}
}

// TestClassifierBadDebugSkipsToSiblingCandidate proves the core Tier 3
// invariant: badDebug is keyed by per-path signature, so a corrupt
// candidate does NOT block a valid sibling for the same build-id.
// Without the loop continuing past the bad entry, this test fails.
func TestClassifierBadDebugSkipsToSiblingCandidate(t *testing.T) {
	tmp := t.TempDir()
	buildIDHex := "cccccccccccccccccccccccccccccccccccccccc"
	binPath := writeStrippedELFWithBuildID(t, filepath.Join(tmp, "exe"), buildIDHex)

	// Set up a fake "system" debug root in a temp dir and place a valid
	// .debug there. This is candidate #1 (preferred).
	sysRoot := t.TempDir()
	sysDir := filepath.Join(sysRoot, buildIDHex[:2])
	if err := os.MkdirAll(sysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sysDebug := filepath.Join(sysDir, buildIDHex[2:]+".debug")
	if err := os.WriteFile(sysDebug, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}

	// Set up the cache with a SECOND .debug. This is candidate #2.
	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.NewSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cc := &cache.Cache{Dir: cacheDir, Index: idx}

	debugDir := filepath.Join(cacheDir, ".build-id", buildIDHex[:2])
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheDebug := filepath.Join(debugDir, buildIDHex[2:]+".debug")
	if err := os.WriteFile(cacheDebug, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Touch(buildIDHex, cache.KindDebuginfo, 4); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(cc, nil, &atomicStats{})
	c.systemDebugRoot = sysRoot // override the hardcoded path

	// Mark the FIRST candidate (sysDebug) as bad. Loop must skip it and
	// return the SECOND candidate (cacheDebug).
	sig, err := statSig(sysDebug)
	if err != nil {
		t.Fatal(err)
	}
	c.markBadDebug(sig)

	got := c.classify(t.Context(), procmap.Mapping{Path: binPath})
	if got.route != routeFileMode {
		t.Fatalf("route = %v, want routeFileMode", got.route)
	}
	if got.debugPath != cacheDebug {
		t.Errorf("debugPath = %q, want %q (the cache sibling — first candidate was badDebug)", got.debugPath, cacheDebug)
	}
}

// TestSymbolizerSplitsBatchesByRoute exercises the integration glue: an
// IP batch with two distinct mappings — one routed to process-mode,
// one to file-mode — should produce frames in the correct order with
// the correct sources. This is a unit-level guard for the routing logic
// in SymbolizeProcess(); the full end-to-end story lives in the
// integration tests in test/.
func TestSymbolizerSplitsBatchesByRoute(t *testing.T) {
	t.Skip("integration-style test; uses real /proc/<pid>/maps from this very test process")
	// Implementation hint: pick a non-trivial Go function in the current
	// process (a runtime symbol), call SymbolizeProcess(os.Getpid(),
	// [its_PC]), assert one frame with that name. Then also pass an
	// address inside a deliberately-stripped fixture mapped via
	// syscall.Mmap. Verify both frames resolve. This test is gated by
	// t.Skip because it's fragile across runners; the integration tests
	// in test/ cover the same path with real workloads. Left in as a
	// documented placeholder in case someone wants to expand unit
	// coverage of the router.
}

// TestClassifierUsesMapFilesWhenSymbolicPathDeleted verifies that the
// classifier routes correctly when a mapping's symbolic path is unreachable
// (deleted binary, sidecar / mount-namespace case) but MapFiles still points
// at the live inode via /proc/<pid>/map_files/<va>-<va>.
//
// The DWARF unwinder in unwind/dwarfagent opens the binary by symbolic path at
// attach time; that path may also be absent. Fixing that is a separate PR.
// This test verifies the classifier's own concern: it reads the build-id via
// MapFiles and routes the mapping to file-mode when a cached .debug exists.
func TestClassifierUsesMapFilesWhenSymbolicPathDeleted(t *testing.T) {
	tmp := t.TempDir()
	buildIDHex := "dddddddddddddddddddddddddddddddddddddddd"

	// Write a stripped ELF with a build-id to act as the "live binary" that
	// would be reachable via /proc/<pid>/map_files/<range>.
	mapFilesTarget := writeStrippedELFWithBuildID(t, filepath.Join(tmp, "live-binary"), buildIDHex)

	// Set up a cache with a .debug staged for the build-id.
	cacheDir := t.TempDir()
	cacheDB := filepath.Join(cacheDir, "index.db")
	idx, err := cache.NewSQLiteIndex(cacheDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	cc := &cache.Cache{Dir: cacheDir, Index: idx}

	debugDir := filepath.Join(cacheDir, ".build-id", buildIDHex[:2])
	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	debugPath := filepath.Join(debugDir, buildIDHex[2:]+".debug")
	if err := os.WriteFile(debugPath, []byte{0x7f, 'E', 'L', 'F'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Touch(buildIDHex, cache.KindDebuginfo, 4); err != nil {
		t.Fatal(err)
	}

	c := newClassifier(cc, nil, &atomicStats{})

	// Construct a Mapping whose symbolic Path is unreachable (deleted binary)
	// but whose MapFiles field points at the live inode — exactly what
	// procmap.Parse produces when it sees " (deleted)" in /proc/<pid>/maps
	// and resolves the map_files link successfully.
	m := procmap.Mapping{
		Path:     "/deleted/path/rust-workload",
		MapFiles: mapFilesTarget, // the live file the kernel still has open
	}

	got := c.classify(t.Context(), m)
	if got.route != routeFileMode {
		t.Errorf("classify() route = %v, want routeFileMode (MapFiles provides build-id even though Path is deleted)", got.route)
	}
	if got.debugPath != debugPath {
		t.Errorf("classify() debugPath = %q, want %q", got.debugPath, debugPath)
	}
}

// writeStrippedELFWithBuildID writes an ELF with .note.gnu.build-id (decoded
// from hex) but no .debug_info, no .gnu_debuglink, no .symtab.
func writeStrippedELFWithBuildID(t *testing.T, path, buildIDHex string) string {
	t.Helper()
	buildID, err := hex.DecodeString(buildIDHex)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeELFWithNoteGnuBuildID(path, buildID); err != nil {
		t.Fatal(err)
	}
	return path
}
