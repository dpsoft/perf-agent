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

	w.AddComm(commRecord{pid: 1234, tid: 1234, comm: "myapp"})
	w.AddMmap2(mmap2Record{
		pid: 1234, tid: 1234,
		addr: 0x400000, len: 0x1000, pgoff: 0,
		filename: "/usr/bin/myapp",
	})
	w.AddSample(sampleRecord{
		ip: 0x400500, pid: 1234, tid: 1234,
		time: 1000, cpu: 0, period: 1,
		callchain: []uint64{0x400500},
	})
	w.AddBuildID(buildIDEntry{
		pid:      -1,
		buildID:  [20]byte{0xde, 0xad, 0xbe, 0xef},
		filename: "/usr/bin/myapp",
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
