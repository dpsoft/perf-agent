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
	c := newClassifier(nil /* cache unused for skip-path tests */, nil /* fetcher unused */)
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

	c := newClassifier(nil, nil)
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

	c := newClassifier(cc, nil) // no fetcher needed — cache hit
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

	c := newClassifier(cc, nil)
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
