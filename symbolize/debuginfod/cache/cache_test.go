package cache

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPathDebuginfo(t *testing.T) {
	got := pathFor("aabbccddeeff", KindDebuginfo)
	want := filepath.Join(".build-id", "aa", "bbccddeeff.debug")
	if got != want {
		t.Fatalf("pathFor debuginfo = %q, want %q", got, want)
	}
}

func TestPathExecutable(t *testing.T) {
	got := pathFor("aabbccddeeff", KindExecutable)
	want := filepath.Join(".build-id", "aa", "bbccddeeff")
	if got != want {
		t.Fatalf("pathFor executable = %q, want %q", got, want)
	}
}

func TestPathTooShortBuildID(t *testing.T) {
	got := pathFor("a", KindDebuginfo)
	if got != "" {
		t.Fatalf("pathFor(short) = %q, want empty", got)
	}
}

func TestWriteAtomicCreatesFile(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	body := strings.NewReader("hello world")
	abs, err := c.WriteAtomic("deadbeef0011223344", KindDebuginfo, body)
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("file contents = %q", got)
	}
	if !strings.HasSuffix(abs, "/.build-id/de/adbeef0011223344.debug") {
		t.Fatalf("path layout wrong: %q", abs)
	}
}

func TestWriteAtomicNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	// Make the destination dir non-writable so CreateTemp fails before rename
	// is reached. Verifies the no-leftover-tmp-file invariant on the early
	// (CreateTemp) failure path.
	bad := filepath.Join(dir, ".build-id", "de")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(bad, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(bad, 0o755) //nolint:errcheck
	_, err := c.WriteAtomic("deadbeef0011223344", KindDebuginfo, strings.NewReader("x"))
	if err == nil {
		t.Skip("rename succeeded despite chmod (likely root)")
	}
	// No .debug file should exist.
	final := filepath.Join(bad, "adbeef0011223344.debug")
	if _, err := os.Stat(final); err == nil {
		t.Fatalf("partial file present at %q", final)
	}
	// And no leftover tmp file in dir.
	entries, _ := os.ReadDir(bad)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fetch-") {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
}

// Sanity: io.Copy produces the same bytes through WriteAtomic.
func TestWriteAtomicLargeBody(t *testing.T) {
	dir := t.TempDir()
	c := &Cache{Dir: dir}
	const N = 1 << 20
	want := bytes.Repeat([]byte("A"), N)
	abs, err := c.WriteAtomic("abcdef0123456789aa", KindExecutable, bytes.NewReader(want))
	if err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body mismatch (got %d bytes, want %d)", len(got), N)
	}
}

func newCacheWithIndex(t *testing.T) *Cache {
	t.Helper()
	dir := t.TempDir()
	idx, err := NewSQLiteIndex(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("NewSQLiteIndex: %v", err)
	}
	c := &Cache{Dir: dir, Index: idx, MaxBytes: 250}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCacheWriteRecordsInIndex(t *testing.T) {
	c := newCacheWithIndex(t)
	if _, err := c.WriteAtomic("aabbccddee0011", KindDebuginfo, strings.NewReader("xyz")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	total, err := c.Index.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	if total != 3 {
		t.Fatalf("TotalBytes = %d, want 3", total)
	}
}

func TestCacheEvictionDeletesFiles(t *testing.T) {
	c := newCacheWithIndex(t) // MaxBytes = 250

	for i := range 5 {
		buildID := strings.Repeat(strconv.Itoa(i), 16)
		body := strings.Repeat("X", 100)
		if _, err := c.WriteAtomic(buildID, KindDebuginfo, strings.NewReader(body)); err != nil {
			t.Fatalf("WriteAtomic: %v", err)
		}
	}

	if err := c.Evict(); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	total, _ := c.Index.TotalBytes()
	if total > 250 {
		t.Fatalf("TotalBytes after evict = %d, want ≤ 250", total)
	}

	var remainingFiles int
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".debug") {
			remainingFiles++
		}
		return nil
	})
	if int64(remainingFiles)*100 != total {
		t.Fatalf("file count %d * 100 != index total %d (drift)", remainingFiles, total)
	}
}

func TestCachePrewarmRebuildsIndex(t *testing.T) {
	dir := t.TempDir()
	idx, _ := NewSQLiteIndex(filepath.Join(dir, "index.db"))
	c := &Cache{Dir: dir, Index: idx, MaxBytes: 1024}
	if _, err := c.WriteAtomic("ddee0011223344", KindDebuginfo, strings.NewReader("hello")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	_ = c.Close()

	idx2, _ := NewSQLiteIndex(filepath.Join(dir, "index2.db"))
	c2 := &Cache{Dir: dir, Index: idx2, MaxBytes: 1024}
	defer c2.Close()
	if err := c2.Prewarm(); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	total, _ := c2.Index.TotalBytes()
	if total != 5 {
		t.Fatalf("TotalBytes after Prewarm = %d, want 5", total)
	}
}
