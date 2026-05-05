package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeBuildIDFeature(t *testing.T) {
	var buf bytes.Buffer
	encodeBuildIDFeature(&buf, []BuildIDEntry{
		{
			Pid:      -1,
			BuildID:  [20]byte{0xab, 0xcd, 0xef},
			Filename: "/usr/bin/ls",
		},
	})
	got := buf.Bytes()
	// One entry: header(40) + filename ("ls" terminator? no — full path) padded.
	// build_id_event = perf_event_header(8) + pid(4) + build_id[24]+ filename[]
	// header.type = PERF_RECORD_HEADER_BUILD_ID = 67 (kernel HEADER_BUILD_ID record-type wrapper)
	// Don't hardcode size; just check the record type and presence of filename.
	if len(got) < 40 {
		t.Fatalf("build_id record too small: %d bytes", len(got))
	}
	// type at offset 0 = 67
	if got[0] != 67 {
		t.Errorf("type = %d, want 67 (HEADER_BUILD_ID record type)", got[0])
	}
	// pid at offset 8, signed s32 LE = -1 = 0xFFFFFFFF
	if got[8] != 0xFF || got[9] != 0xFF || got[10] != 0xFF || got[11] != 0xFF {
		t.Errorf("pid = % x, want ff ff ff ff", got[8:12])
	}
}

func TestEncodeStringFeature(t *testing.T) {
	var buf bytes.Buffer
	encodeStringFeature(&buf, "linux-host-1")
	got := buf.Bytes()
	// Layout: u32 len, char str[len], pad to 8.
	// "linux-host-1" is 12 bytes + NUL = 13, len field stores 13.
	// Actually perf header strings store: u32 len; char str[len]; with len
	// being the padded length (including NUL).
	// len at offset 0 (u32 LE)
	wantLen := uint32(align8(len("linux-host-1") + 1)) // 16
	if got[0] != byte(wantLen) {
		t.Errorf("len = %d, want %d", got[0], wantLen)
	}
	// "linux-host-1" should appear starting at offset 4
	if !bytes.HasPrefix(got[4:], []byte("linux-host-1")) {
		t.Errorf("string body wrong: %q", got[4:])
	}
}

func TestEncodeNRCPUSFeature(t *testing.T) {
	var buf bytes.Buffer
	encodeNRCPUSFeature(&buf, 16, 16) // online=16, available=16
	got := buf.Bytes()
	if len(got) != 8 {
		t.Fatalf("NRCPUS size = %d, want 8", len(got))
	}
	// online at offset 0 (u32 LE) = 16
	if got[0] != 16 {
		t.Errorf("online = %d, want 16", got[0])
	}
	// available at offset 4 (u32 LE) = 16
	if got[4] != 16 {
		t.Errorf("available = %d, want 16", got[4])
	}
}
