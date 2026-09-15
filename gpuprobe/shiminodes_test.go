package gpuprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// stageShimMapping writes procRoot/<pid>/maps naming shimPath with the
// device and inode a uprobe would key on.
func stageShimMapping(t *testing.T, procRoot string, pid uint32, shimPath string) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(shimPath, &st); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	maps := fmt.Sprintf("7f0000000000-7f0000001000 r-xp 00000000 %02x:%02x %d %s\n",
		(st.Dev>>8)&0xfff, st.Dev&0xff, st.Ino, shimPath)
	if err := os.WriteFile(filepath.Join(dir, "maps"), []byte(maps), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShimInodesInUseGroupsByInodeNotPath(t *testing.T) {
	dir := t.TempDir()
	// Two shim files in one directory, identical bytes, different inodes
	// -- exactly what a rename(2) upgrade leaves behind.
	oldShim := filepath.Join(dir, "libperfagent-gpu-nvidia.so.old")
	newShim := filepath.Join(dir, "libperfagent-gpu-nvidia.so")
	for _, p := range []string{oldShim, newShim} {
		if err := os.WriteFile(p, []byte("\x7fELF fake"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	procRoot := t.TempDir()
	stageShimMapping(t, procRoot, 11, oldShim)
	stageShimMapping(t, procRoot, 22, newShim)
	stageShimMapping(t, procRoot, 33, newShim)

	got, err := shimInodesInUseIn(procRoot, dir)
	if err != nil {
		t.Fatalf("shimInodesInUseIn: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d inodes, want 2 (one per file)", len(got))
	}
	for f, pids := range got {
		switch f.Path {
		case oldShim:
			if len(pids) != 1 || pids[0] != 11 {
				t.Errorf("old inode pids = %v, want [11]", pids)
			}
		case newShim:
			if len(pids) != 2 {
				t.Errorf("new inode pids = %v, want two", pids)
			}
		default:
			t.Errorf("unexpected path %q", f.Path)
		}
	}
}

func TestShimInodesInUseSkipsFilesNothingMapped(t *testing.T) {
	dir := t.TempDir()
	unused := filepath.Join(dir, "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(unused, []byte("\x7fELF fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := shimInodesInUseIn(t.TempDir(), dir)
	if err != nil {
		t.Fatalf("shimInodesInUseIn: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v; a file with no mappers needs no link", got)
	}
}
