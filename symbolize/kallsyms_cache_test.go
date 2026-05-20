package symbolize

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withCacheDir overrides the cache path to a test-local temp dir
// and the boot-id reader to a deterministic value. Returns a
// cleanup func.
func withCacheDir(t *testing.T, bootID [16]byte) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kallsyms.cache")
	prevPath := cachePathFn
	prevBoot := readBootIDFn
	cachePathFn = func() string { return path }
	readBootIDFn = func() ([16]byte, error) { return bootID, nil }
	return path, func() {
		cachePathFn = prevPath
		readBootIDFn = prevBoot
	}
}

// TestKallsymsCache_Roundtrip writes a synthetic symbolizer to the
// cache, then loads it back, and verifies the loaded copy resolves
// addresses to the same symbols. Sanity check for the binary
// format: addr → name/module pairs preserved exactly, including
// the module intern map's identity invariant (same module text
// resolves to the same Go string).
func TestKallsymsCache_Roundtrip(t *testing.T) {
	_, cleanup := withCacheDir(t, [16]byte{1, 2, 3, 4})
	defer cleanup()

	want := &kallsymsSymbolizer{
		addrs:   []uint64{0xffffffff80001000, 0xffffffff80002000, 0xffffffffc0001000},
		names:   []string{"do_sys_openat2", "vfs_open", "kvm_vcpu_ioctl"},
		modules: []string{"", "", "[kvm]"},
	}

	if err := writeKallsymsCache(want); err != nil {
		t.Fatalf("writeKallsymsCache: %v", err)
	}

	got, err := loadCachedKallsyms()
	if err != nil {
		t.Fatalf("loadCachedKallsyms: %v", err)
	}
	if len(got.addrs) != len(want.addrs) {
		t.Fatalf("addrs len = %d, want %d", len(got.addrs), len(want.addrs))
	}
	for i := range want.addrs {
		if got.addrs[i] != want.addrs[i] {
			t.Errorf("addrs[%d] = %#x, want %#x", i, got.addrs[i], want.addrs[i])
		}
		if got.names[i] != want.names[i] {
			t.Errorf("names[%d] = %q, want %q", i, got.names[i], want.names[i])
		}
		if got.modules[i] != want.modules[i] {
			t.Errorf("modules[%d] = %q, want %q", i, got.modules[i], want.modules[i])
		}
	}

	// Resolution still works: pick the kvm address + 0x42, expect
	// the kvm module marker on the resolved frame.
	frames := got.Resolve([]uint64{0xffffffffc0001042})
	if frames[0].Module != "[kvm]" {
		t.Errorf("Module = %q, want [kvm]", frames[0].Module)
	}
}

// TestKallsymsCache_StaleBootID asserts a cache produced under one
// boot_id is rejected when the current boot_id has changed (reboot).
// Critical for correctness: kernel module addresses change on
// reboot and a stale cache would mis-attribute every kernel frame.
func TestKallsymsCache_StaleBootID(t *testing.T) {
	path, cleanup := withCacheDir(t, [16]byte{1, 2, 3, 4})
	defer cleanup()

	if err := writeKallsymsCache(&kallsymsSymbolizer{
		addrs:   []uint64{0xffffffff80001000},
		names:   []string{"sym"},
		modules: []string{""},
	}); err != nil {
		t.Fatalf("writeKallsymsCache: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not written: %v", err)
	}

	// Simulate reboot: change the boot_id the loader sees.
	readBootIDFn = func() ([16]byte, error) { return [16]byte{9, 9, 9, 9}, nil }

	_, err := loadCachedKallsyms()
	if !errors.Is(err, errKallsymsCacheStale) {
		t.Errorf("loadCachedKallsyms err = %v, want errKallsymsCacheStale", err)
	}
}

// TestKallsymsCache_Missing covers the "cold cache" case — first
// run on a host. Loader returns an error (not panic), caller falls
// back to a fresh parse.
func TestKallsymsCache_Missing(t *testing.T) {
	_, cleanup := withCacheDir(t, [16]byte{1, 2, 3, 4})
	defer cleanup()

	if _, err := loadCachedKallsyms(); err == nil {
		t.Fatalf("loadCachedKallsyms returned nil err on missing cache")
	}
}

// TestKallsymsCache_CorruptIsNonFatal asserts a corrupt file is
// detected (wrong magic) and reported as an error rather than
// causing a panic or hang. Hosts with partial writes from killed
// agents would otherwise re-hit the issue on every startup.
func TestKallsymsCache_CorruptIsNonFatal(t *testing.T) {
	path, cleanup := withCacheDir(t, [16]byte{1, 2, 3, 4})
	defer cleanup()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("garbage-not-a-cache"), 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := loadCachedKallsyms(); err == nil {
		t.Errorf("loadCachedKallsyms returned nil on corrupt file")
	}
}

// TestReadBootIDLive: smoke test against the real
// /proc/sys/kernel/random/boot_id. Skips if unreadable (e.g.,
// CI sandbox).
func TestReadBootIDLive(t *testing.T) {
	if _, err := os.Stat("/proc/sys/kernel/random/boot_id"); err != nil {
		t.Skip("no /proc/sys/kernel/random/boot_id on this host")
	}
	b, err := readBootID()
	if err != nil {
		t.Fatalf("readBootID: %v", err)
	}
	var zero [16]byte
	if b == zero {
		t.Errorf("boot_id read as all-zero — parser likely failed")
	}
}
