package perfdata

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAddKernelMmap asserts that the on-disk perf.data carries a
// PERF_RECORD_MMAP2 with filename [kernel.kallsyms]_text and pid=-1.
// Address/Length depend on the host (real /proc/kallsyms vs catch-all
// fallback); we just check the record shape, not the numeric range.
func TestAddKernelMmap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type:         1, // PERF_TYPE_SOFTWARE
		Config:       0, // PERF_COUNT_SW_CPU_CLOCK
		SamplePeriod: 99,
		Frequency:    true,
	}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.AddKernelMmap(); err != nil {
		t.Fatalf("AddKernelMmap: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(body, []byte("[kernel.kallsyms]_text")) {
		t.Fatalf("perf.data missing kernel MMAP2 filename")
	}
	// pid=-1 = 0xffffffff little-endian. We don't pin its byte offset
	// (depends on header), but the bytes must appear in the file.
	if !bytes.Contains(body, []byte{0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("perf.data missing pid=-1 marker")
	}
}

// TestKernelCatchallRange asserts the fallback constants. Pinned because
// these are the values perf report attributes module text to when
// /proc/kallsyms is unreadable.
func TestKernelCatchallRange(t *testing.T) {
	addr, length := kernelCatchallRange()
	if addr != 0xffffffff80000000 {
		t.Errorf("addr = %#x, want 0xffffffff80000000", addr)
	}
	if length != 0x80000000 {
		t.Errorf("length = %#x, want 0x80000000", length)
	}
}
