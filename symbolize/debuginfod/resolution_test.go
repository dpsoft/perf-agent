package debuginfod

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasDwarfTrue(t *testing.T) {
	// /usr/bin/grep is typically built with debug info on Debian-style systems;
	// fall back to skipping if we can't find a binary that has DWARF.
	for _, p := range []string{"/usr/bin/grep", "/usr/bin/ls", "/bin/ls"} {
		if _, err := os.Stat(p); err == nil {
			if hasDwarf(p) {
				return // PASS
			}
		}
	}
	t.Skip("no system binary with DWARF found")
}

func TestHasDwarfMissingFile(t *testing.T) {
	if hasDwarf("/nonexistent/path") {
		t.Fatalf("hasDwarf on missing path returned true")
	}
}

func TestBinaryReadable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !binaryReadable(good) {
		t.Fatalf("binaryReadable(%s) = false", good)
	}
	if binaryReadable(filepath.Join(dir, "missing")) {
		t.Fatalf("binaryReadable(missing) = true")
	}
}

func TestHasResolvableDebuglinkMissing(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "x")
	if err := os.WriteFile(bin, []byte("not an elf"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if hasResolvableDebuglink(bin, nil) {
		t.Fatalf("non-ELF file returned true")
	}
}
