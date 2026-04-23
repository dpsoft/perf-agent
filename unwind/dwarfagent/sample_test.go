package dwarfagent

import (
	"encoding/binary"
	"testing"
)

func TestParseSampleHeader(t *testing.T) {
	const sampleSize = 32 + 127*8
	buf := make([]byte, sampleSize)
	binary.LittleEndian.PutUint32(buf[0:4], 0x1234)       // pid
	binary.LittleEndian.PutUint32(buf[4:8], 0x5678)       // tid
	binary.LittleEndian.PutUint64(buf[8:16], 0x9abc_def0) // time_ns
	binary.LittleEndian.PutUint64(buf[16:24], 1)          // value
	buf[24] = 1                                           // mode = MODE_FP_LESS
	buf[25] = 3                                           // n_pcs = 3
	buf[26] = 0x2                                         // walker_flags = DWARF_USED
	binary.LittleEndian.PutUint64(buf[32:40], 0xaaaa)
	binary.LittleEndian.PutUint64(buf[40:48], 0xbbbb)
	binary.LittleEndian.PutUint64(buf[48:56], 0xcccc)

	s, err := parseSample(buf)
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if s.PID != 0x1234 {
		t.Errorf("PID = %#x, want 0x1234", s.PID)
	}
	if s.TID != 0x5678 {
		t.Errorf("TID = %#x, want 0x5678", s.TID)
	}
	if s.Mode != 1 {
		t.Errorf("Mode = %d, want 1", s.Mode)
	}
	if len(s.PCs) != 3 {
		t.Fatalf("len(PCs) = %d, want 3", len(s.PCs))
	}
	if s.PCs[0] != 0xaaaa || s.PCs[1] != 0xbbbb || s.PCs[2] != 0xcccc {
		t.Errorf("PCs = %v, want [0xaaaa 0xbbbb 0xcccc]", s.PCs)
	}
	if s.WalkerFlags != 0x2 {
		t.Errorf("WalkerFlags = %#x, want 0x2", s.WalkerFlags)
	}
}

func TestParseSampleTruncatedHeader(t *testing.T) {
	buf := make([]byte, 16) // smaller than 32-byte header
	if _, err := parseSample(buf); err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestParseSampleNPCsClamped(t *testing.T) {
	const sampleSize = 32 + 127*8
	buf := make([]byte, sampleSize)
	binary.LittleEndian.PutUint32(buf[0:4], 42)
	buf[25] = 200 // n_pcs > 127, should clamp
	s, err := parseSample(buf)
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if len(s.PCs) != 127 {
		t.Errorf("clamped len(PCs) = %d, want 127", len(s.PCs))
	}
}
