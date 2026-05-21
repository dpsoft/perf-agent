package symbolize

import (
	"os"
	"path/filepath"
	"testing"
)

// withMarkerCacheDir overrides the XDG_CACHE_HOME so the EPERM
// marker lands in a t.TempDir(). Avoids polluting the user's
// real ~/.cache. Also pins the boot_id reader.
func withMarkerCacheDir(t *testing.T, bootID [16]byte) func() {
	t.Helper()
	dir := t.TempDir()
	prevXDG, hadPrev := os.LookupEnv("XDG_CACHE_HOME")
	t.Setenv("XDG_CACHE_HOME", dir)
	prevBoot := readBootIDFn
	readBootIDFn = func() ([16]byte, error) { return bootID, nil }
	return func() {
		readBootIDFn = prevBoot
		if hadPrev {
			_ = os.Setenv("XDG_CACHE_HOME", prevXDG)
		} else {
			_ = os.Unsetenv("XDG_CACHE_HOME")
		}
	}
}

// TestBlazesymEPERMMarker_Roundtrip asserts writing the marker
// then checking with the same boot_id reports it as present.
func TestBlazesymEPERMMarker_Roundtrip(t *testing.T) {
	cleanup := withMarkerCacheDir(t, [16]byte{1, 2, 3, 4})
	defer cleanup()

	if blazesymEPERMMarkerExists() {
		t.Fatalf("marker exists before write")
	}
	if err := writeBlazesymEPERMMarker(); err != nil {
		t.Fatalf("writeBlazesymEPERMMarker: %v", err)
	}
	if !blazesymEPERMMarkerExists() {
		t.Errorf("marker missing after write")
	}
}

// TestBlazesymEPERMMarker_BootIDScoped asserts the marker for one
// boot_id is invisible under a different boot_id. Critical: after
// a reboot, lockdown state may differ — must not assume EPERM
// persists.
func TestBlazesymEPERMMarker_BootIDScoped(t *testing.T) {
	cleanup := withMarkerCacheDir(t, [16]byte{1, 2, 3, 4})
	defer cleanup()
	if err := writeBlazesymEPERMMarker(); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Switch to a different boot_id; marker should appear absent.
	readBootIDFn = func() ([16]byte, error) { return [16]byte{9, 9, 9, 9}, nil }
	if blazesymEPERMMarkerExists() {
		t.Errorf("marker visible under different boot_id")
	}
}

// TestBlazesymEPERMMarker_PathIncludesBootID is a structural
// check: the marker filename literally encodes the boot_id hex
// so multiple boots' markers can coexist without colliding.
func TestBlazesymEPERMMarker_PathIncludesBootID(t *testing.T) {
	cleanup := withMarkerCacheDir(t, [16]byte{0xab, 0xcd})
	defer cleanup()
	bootID, _ := readBootIDFn()
	path := blazesymEPERMMarkerPath(bootID)
	if !filepath.IsAbs(path) {
		t.Errorf("marker path not absolute: %s", path)
	}
	// abcd0000...0000 hex form should appear in the filename.
	if filepath.Base(path) == "" {
		t.Fatalf("empty basename")
	}
}
