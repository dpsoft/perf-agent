package procmap

import (
	"fmt"
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
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
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
