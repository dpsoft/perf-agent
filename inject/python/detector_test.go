package python

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// buildSyntheticProc creates a /proc-like tree under tmp for one PID, with a
// maps file containing the given lines and an exe symlink pointing at exeTarget.
// Returns the procRoot path.
func buildSyntheticProc(t *testing.T, pid uint32, mapsLines []string, exeTarget string) string {
	t.Helper()
	root := t.TempDir()
	pidDir := filepath.Join(root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mapsContent := ""
	for _, l := range mapsLines {
		mapsContent += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(pidDir, "maps"), []byte(mapsContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if exeTarget != "" {
		if err := os.Symlink(exeTarget, filepath.Join(pidDir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// findRealLibpython finds a libpython3.12+ on the test host, or skips.
func findRealLibpython(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"/usr/lib/x86_64-linux-gnu/libpython3.12.so.1.0",
		"/usr/lib/x86_64-linux-gnu/libpython3.13.so.1.0",
		"/usr/lib/aarch64-linux-gnu/libpython3.12.so.1.0",
		"/usr/lib/libpython3.12.so.1.0",
		"/usr/lib64/libpython3.12.so.1.0",
		"/usr/lib64/libpython3.13.so.1.0",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	matches, _ := filepath.Glob("/usr/lib*/libpython3.1*.so*")
	for _, m := range matches {
		if _, err := os.Stat(m); err == nil {
			return m
		}
	}
	t.Skip("no libpython3.12+ found on test host")
	return ""
}

func TestDetect_DynamicLinkedPython312(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}
	libpath := findRealLibpython(t)
	pid := uint32(12345)
	mapsLine := fmt.Sprintf("00400000-00500000 r-xp 00000000 00:00 0 %s", libpath)
	root := buildSyntheticProc(t, pid, []string{mapsLine}, "")

	d := NewDetector(root, nil)
	got, err := d.Detect(pid)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.PID != pid {
		t.Errorf("PID = %d, want %d", got.PID, pid)
	}
	if got.LibPythonPath != libpath {
		t.Errorf("LibPythonPath = %q, want %q", got.LibPythonPath, libpath)
	}
	if got.LoadBase != 0x00400000 {
		t.Errorf("LoadBase = 0x%x, want 0x00400000", got.LoadBase)
	}
	if got.PyGILEnsureAddr == 0 || got.PyRunStringAddr == 0 || got.PyGILReleaseAddr == 0 {
		t.Errorf("symbol addrs not populated: %+v", got)
	}
}

// TestDetect_DynamicLinkedPython_SeparateCodeLayout covers the modern
// -Wl,-z,separate-code layout (default in Ubuntu 24.04 + glibc 2.39, used by
// actions/setup-python's CPython 3.12 builds): the first PT_LOAD segment is
// r-- (header + RELRO data) at file offset 0, and a separate r-x segment
// holds .text at a non-zero file offset. Picking the first executable mapping
// in this layout would return load_base + first_segment_size — wrong by that
// delta. Detection must use file offset 0 as the load-base anchor.
func TestDetect_DynamicLinkedPython_SeparateCodeLayout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}
	libpath := findRealLibpython(t)
	pid := uint32(54321)
	root := buildSyntheticProc(t, pid, []string{
		// Leading r-- segment at offset 0 — this is the proper load base.
		fmt.Sprintf("7f0000400000-7f0000401000 r--p 00000000 00:00 0 %s", libpath),
		// Executable .text segment at non-zero offset — old code would
		// have picked this start address as load base, off by 0x1000.
		fmt.Sprintf("7f0000401000-7f0000800000 r-xp 00001000 00:00 0 %s", libpath),
		fmt.Sprintf("7f0000800000-7f0000900000 r--p 00400000 00:00 0 %s", libpath),
		fmt.Sprintf("7f0000900000-7f0000a00000 rw-p 00500000 00:00 0 %s", libpath),
	}, "")

	d := NewDetector(root, nil)
	got, err := d.Detect(pid)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.LoadBase != 0x7f0000400000 {
		t.Errorf("LoadBase = 0x%x, want 0x7f0000400000 (the offset-0 mapping, not the executable one)",
			got.LoadBase)
	}
}

func TestDetect_NonPython(t *testing.T) {
	pid := uint32(22222)
	root := buildSyntheticProc(t, pid, []string{
		"00400000-00500000 r-xp 00000000 00:00 0 /usr/bin/cat",
	}, "/usr/bin/cat")

	d := NewDetector(root, nil)
	_, err := d.Detect(pid)
	if err == nil {
		t.Fatal("expected error for non-python; got nil")
	}
	if !errors.Is(err, ErrNotPython) {
		t.Fatalf("expected ErrNotPython; got %v", err)
	}
}

func TestDetect_PythonTooOld(t *testing.T) {
	pid := uint32(33333)
	mapsLine := "00400000-00500000 r-xp 00000000 00:00 0 /usr/lib/libpython3.11.so.1.0"
	root := buildSyntheticProc(t, pid, []string{mapsLine}, "")

	d := NewDetector(root, nil)
	_, err := d.Detect(pid)
	if err == nil {
		t.Fatal("expected ErrPythonTooOld")
	}
	if !errors.Is(err, ErrPythonTooOld) {
		t.Fatalf("expected ErrPythonTooOld; got %v", err)
	}
}

func TestDetect_ProcessGone(t *testing.T) {
	pid := uint32(99999)
	// Don't create any /proc/<pid> entry — simulates process exit.
	root := t.TempDir()
	d := NewDetector(root, nil)
	_, err := d.Detect(pid)
	if err == nil {
		t.Fatal("expected error for missing /proc/<pid>")
	}
	if !errors.Is(err, ErrNotPython) {
		t.Fatalf("expected ErrNotPython (wrapping process-gone); got %v", err)
	}
}
