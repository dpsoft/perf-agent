package dwarfagent

import (
	"encoding/binary"
	"testing"
)

func TestParseSampleHeader(t *testing.T) {
	const sampleSize = 40 + 127*8
	buf := make([]byte, sampleSize)
	binary.LittleEndian.PutUint32(buf[0:4], 0x1234)       // pid
	binary.LittleEndian.PutUint32(buf[4:8], 0x5678)       // tid
	binary.LittleEndian.PutUint64(buf[8:16], 0x9abc_def0) // time_ns
	binary.LittleEndian.PutUint64(buf[16:24], 1)          // value
	buf[24] = 1                                           // mode = MODE_FP_LESS
	buf[25] = 3                                           // n_pcs = 3
	buf[26] = 0x2                                         // walker_flags = DWARF_USED
	binary.LittleEndian.PutUint64(buf[32:40], 7)          // kern_stack = 7
	binary.LittleEndian.PutUint64(buf[40:48], 0xaaaa)
	binary.LittleEndian.PutUint64(buf[48:56], 0xbbbb)
	binary.LittleEndian.PutUint64(buf[56:64], 0xcccc)

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
	if s.KernStack != 7 {
		t.Errorf("KernStack = %d, want 7", s.KernStack)
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

// TestParseSampleHeaderKernStackDisabled covers the gate-off case where
// BPF wrote -1 into kern_stack — userspace must read it back as -1 so
// the consumeRingbuf loop knows to skip the kern_stackmap lookup.
func TestParseSampleHeaderKernStackDisabled(t *testing.T) {
	const sampleSize = 40 + 127*8
	buf := make([]byte, sampleSize)
	// -1 in two's complement int64
	binary.LittleEndian.PutUint64(buf[32:40], ^uint64(0))
	s, err := parseSample(buf)
	if err != nil {
		t.Fatalf("parseSample: %v", err)
	}
	if s.KernStack != -1 {
		t.Errorf("KernStack = %d, want -1", s.KernStack)
	}
}

func TestParseSampleTruncatedHeader(t *testing.T) {
	buf := make([]byte, 16) // smaller than 40-byte header
	if _, err := parseSample(buf); err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestParseSampleNPCsClamped(t *testing.T) {
	const sampleSize = 40 + 127*8
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
