package procmap

import (
	"path/filepath"
	"testing"
)

func TestParseMapsFile(t *testing.T) {
	path := filepath.Join("testdata", "proc", "4242", "maps")
	got, err := parseMapsFile(path)
	if err != nil {
		t.Fatalf("parseMapsFile: %v", err)
	}

	want := []Mapping{
		{Path: "/usr/bin/target", Start: 0x00400000, Limit: 0x00420000, Offset: 0x1000, IsExec: true},
		{Path: "/lib/x86_64-linux-gnu/libc.so.6", Start: 0x7f0000001000, Limit: 0x7f0000100000, Offset: 0x2000, IsExec: true},
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

func TestReadBuildID(t *testing.T) {
	// /bin/ls on any modern distro has a GNU build-id. We don't assert
	// the exact value (it varies) — only that it parses to a non-empty
	// lowercase hex string.
	id, err := readBuildID("/bin/ls")
	if err != nil {
		t.Fatalf("readBuildID(/bin/ls): %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty build-id, got empty")
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("build-id %q contains non-hex char %q", id, r)
		}
	}
}

func TestReadBuildIDMissing(t *testing.T) {
	id, err := readBuildID("/nonexistent/path/to/nothing")
	if err == nil {
		t.Fatalf("expected error, got id=%q", id)
	}
}
