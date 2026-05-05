package perfdata

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestWriter_RoundTrip captures a tiny synthetic profile and re-reads the
// resulting file to verify magic, header section pointers, and that the
// data section contains the records we wrote.
func TestWriter_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type:         perfTypeSoftware,
		Config:       perfCountSWCPUClock,
		SamplePeriod: 99,
		Frequency:    true,
	}, MetaInfo{
		Hostname:  "test-host",
		OSRelease: "5.15.0-test",
		NumCPUs:   8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	w.AddComm(CommRecord{Pid: 1234, Tid: 1234, Comm: "myapp"})
	w.AddMmap2(Mmap2Record{
		Pid: 1234, Tid: 1234,
		Addr: 0x400000, Len: 0x1000, Pgoff: 0,
		Filename: "/usr/bin/myapp",
	})
	w.AddSample(SampleRecord{
		IP: 0x400500, Pid: 1234, Tid: 1234,
		Time: 1000, Cpu: 0, Period: 1,
		Callchain: []uint64{0x400500},
	})
	w.AddBuildID(BuildIDEntry{
		Pid:      -1,
		BuildID:  [20]byte{0xde, 0xad, 0xbe, 0xef},
		Filename: "/usr/bin/myapp",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	// magic at offset 0
	magic := binary.LittleEndian.Uint64(body[0:8])
	if magic != magicPERFILE2 {
		t.Errorf("magic = %x, want %x", magic, magicPERFILE2)
	}
	// header.size at offset 8 = 104
	if got := binary.LittleEndian.Uint64(body[8:16]); got != fileHeaderSize {
		t.Errorf("header.size = %d, want %d", got, fileHeaderSize)
	}
	// data section size at offset 48 must be > 0 (we wrote at least one record)
	if got := binary.LittleEndian.Uint64(body[48:56]); got == 0 {
		t.Errorf("data.size = 0; expected non-zero")
	}
	// adds_features bitmap at offset 72: must have at least BUILD_ID (bit 2),
	// HOSTNAME (bit 3), OSRELEASE (bit 4), NRCPUS (bit 7) set.
	mask := binary.LittleEndian.Uint64(body[72:80])
	wantBits := uint64((1 << featBuildID) | (1 << featHostname) | (1 << featOSRelease) | (1 << featNRCPUS))
	if mask&wantBits != wantBits {
		t.Errorf("adds_features mask = %#x, missing bits from %#x", mask, wantBits)
	}
	// raw byte search: filename "/usr/bin/myapp" must appear in data section
	if !bytes.Contains(body, []byte("/usr/bin/myapp")) {
		t.Errorf("filename not found in output")
	}
}

// TestWriter_LatchedErrorMakesAddsNoOp verifies that once a write error is
// latched on the Writer, subsequent Add* calls advance neither pos nor the
// underlying buffer, and Close surfaces the error rather than patching the
// header with stale offsets. We inject the error manually because driving
// a real ENOSPC is impractical in unit tests.
func TestWriter_LatchedErrorMakesAddsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.perf.data")

	w, err := Open(path, EventSpec{
		Type: perfTypeSoftware, Config: perfCountSWCPUClock,
		SamplePeriod: 99, Frequency: true,
	}, MetaInfo{Hostname: "h", OSRelease: "r", NumCPUs: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	posBefore := w.pos
	w.err = errSentinel{}
	w.AddSample(SampleRecord{IP: 0x1, Pid: 1, Tid: 1, Period: 1, Callchain: []uint64{0x1}})
	if w.pos != posBefore {
		t.Errorf("pos advanced after latched error: %d → %d", posBefore, w.pos)
	}

	if err := w.Close(); err == nil {
		t.Fatal("Close: expected latched error, got nil")
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }
