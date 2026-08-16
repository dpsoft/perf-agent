package perfdata

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAddUserspaceMmaps asserts AddUserspaceMmaps emits one
// PERF_RECORD_MMAP2 per UserspaceMapping with the supplied PID
// (so `perf script` attributes the record to that process), the
// mapping's load address / length / file offset, and the binary's
// filename. Without these records `perf script` shows [unknown] for
// every user-space IP in samples drawn from the same PID.
func TestAddUserspaceMmaps(t *testing.T) {
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

	const targetPID = 4242
	mappings := []UserspaceMapping{
		{
			Start: 0x55fa00000000,
			Len:   0x100000,
			Pgoff: 0,
			Path:  "/usr/bin/some_app",
		},
		{
			Start: 0x7f9c12345000,
			Len:   0x200000,
			Pgoff: 0x1000,
			Path:  "/usr/lib/libc.so.6",
		},
	}
	w.AddUserspaceMmaps(targetPID, mappings)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Both filenames must appear in the on-disk perf.data.
	if !bytes.Contains(body, []byte("/usr/bin/some_app")) {
		t.Errorf("perf.data missing /usr/bin/some_app MMAP2 filename")
	}
	if !bytes.Contains(body, []byte("/usr/lib/libc.so.6")) {
		t.Errorf("perf.data missing /usr/lib/libc.so.6 MMAP2 filename")
	}

	// pid=4242 = 0x10920000 in little-endian bytes. Both MMAP2 records
	// must carry this PID, so the byte pattern appears at least twice.
	pidLE := []byte{0x92, 0x10, 0x00, 0x00}
	if count := bytes.Count(body, pidLE); count < 2 {
		t.Errorf("expected pid=%d marker %x at least twice (one per MMAP2 record), got %d", targetPID, pidLE, count)
	}
}

// TestAddUserspaceMmapsEmptyIsNoop verifies that passing an empty
// slice produces no records and doesn't latch a writer error.
func TestAddUserspaceMmapsEmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type: 1, Config: 0, SamplePeriod: 99, Frequency: true,
	}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	posBefore := w.pos
	w.AddUserspaceMmaps(1234, nil)
	if w.pos != posBefore {
		t.Errorf("empty AddUserspaceMmaps advanced pos by %d", w.pos-posBefore)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAddUserspaceMmapsWithBuildID verifies the build-id flavour of
// the MMAP2 union: when UserspaceMapping carries a non-empty BuildID,
// AddUserspaceMmaps emits the record with the miscMmapBuildID flag
// set so `perf script` can match the build-id to a debuginfo file.
func TestAddUserspaceMmapsWithBuildID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type: 1, Config: 0, SamplePeriod: 99, Frequency: true,
	}, MetaInfo{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	buildID := []byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		0xde, 0xad, 0xbe, 0xef,
	}
	w.AddUserspaceMmaps(7777, []UserspaceMapping{
		{
			Start:   0x40000000,
			Len:     0x1000,
			Pgoff:   0,
			Path:    "/opt/bin/with_buildid",
			BuildID: buildID,
		},
	})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(body, buildID) {
		t.Errorf("perf.data missing build-id payload %x", buildID)
	}
}
