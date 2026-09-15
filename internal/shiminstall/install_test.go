package shiminstall

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func statIno(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func TestInstallCopiesWhenAbsent(t *testing.T) {
	src := filepath.Join(t.TempDir(), Name)
	dst := t.TempDir()
	write(t, src, "shim-v1")

	got, replaced, err := Install(src, dst)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !replaced {
		t.Error("replaced = false on first install; want true")
	}
	if want := filepath.Join(dst, Name); got != want {
		t.Errorf("dest = %q, want %q", got, want)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "shim-v1" {
		t.Errorf("content = %q, want shim-v1", b)
	}
}

func TestInstallSkipsWhenIdentical(t *testing.T) {
	src := filepath.Join(t.TempDir(), Name)
	dst := t.TempDir()
	write(t, src, "shim-v1")

	first, _, err := Install(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	inoBefore := statIno(t, first)

	_, replaced, err := Install(src, dst)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if replaced {
		t.Error("replaced = true for identical content; a restart must not churn the inode")
	}
	if statIno(t, first) != inoBefore {
		t.Error("inode changed on a no-op install; every live mapper would be orphaned")
	}
}

func TestInstallReplacesAtomicallyAndLeavesTheOldInodeIntact(t *testing.T) {
	src := filepath.Join(t.TempDir(), Name)
	dst := t.TempDir()
	write(t, src, "shim-v1")
	first, _, err := Install(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the old file open, standing in for a process that mapped it.
	held, err := os.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	oldIno := statIno(t, first)

	write(t, src, "shim-v2")
	second, replaced, err := Install(src, dst)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !replaced {
		t.Fatal("replaced = false for changed content")
	}
	if statIno(t, second) == oldIno {
		t.Error("inode unchanged after replace; the file was written IN PLACE, which " +
			"corrupts every live mapping rather than versioning it")
	}
	// The holder still reads the old bytes: that is what makes rename safe.
	buf := make([]byte, len("shim-v1"))
	if _, err := held.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "shim-v1" {
		t.Errorf("old fd sees %q, want shim-v1 -- the replace was not atomic", buf)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "shim-v2" {
		t.Errorf("new content = %q, want shim-v2", b)
	}
}

func TestInstallLeavesNoTempFileBehind(t *testing.T) {
	src := filepath.Join(t.TempDir(), Name)
	dst := t.TempDir()
	write(t, src, "shim-v1")
	if _, _, err := Install(src, dst); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dest holds %v; want only the shim. A stray temp file is a second "+
			"inode a target could be pointed at by accident", names)
	}
}

func TestInstallLeavesNoTempFileAfterAFailedCopy(t *testing.T) {
	// The failure path matters as much as the success one: a temp file
	// left in the shim directory is a second inode with the right-looking
	// bytes, and CUDA_INJECTION64_PATH is a plain string an operator can
	// point at it by accident.
	dst := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist.so")

	if _, _, err := Install(missing, dst); err == nil {
		t.Fatal("Install of a missing source returned nil error")
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dest holds %d entries after a failed install; want none", len(entries))
	}
}
