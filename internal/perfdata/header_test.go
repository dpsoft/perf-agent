package perfdata

import (
	"bytes"
	"testing"
)

func TestEncodeFileHeader_Empty(t *testing.T) {
	var buf bytes.Buffer
	hdr := fileHeader{
		attrs:        section{offset: 104, size: 136},
		data:         section{offset: 240, size: 0},
		eventTypes:   section{offset: 0, size: 0},
		addsFeatures: 0,
	}
	encodeFileHeader(&buf, hdr)

	want := []byte{
		// magic = "PERFILE2" little-endian
		0x32, 0x45, 0x4c, 0x49, 0x46, 0x52, 0x45, 0x50,
		// size = 104
		0x68, 0, 0, 0, 0, 0, 0, 0,
		// attr_size = 136
		0x88, 0, 0, 0, 0, 0, 0, 0,
		// attrs.offset = 104
		0x68, 0, 0, 0, 0, 0, 0, 0,
		// attrs.size = 136
		0x88, 0, 0, 0, 0, 0, 0, 0,
		// data.offset = 240
		0xf0, 0, 0, 0, 0, 0, 0, 0,
		// data.size = 0
		0, 0, 0, 0, 0, 0, 0, 0,
		// event_types.offset = 0
		0, 0, 0, 0, 0, 0, 0, 0,
		// event_types.size = 0
		0, 0, 0, 0, 0, 0, 0, 0,
		// adds_features bitmap (4 × u64 = 32 bytes), all zero
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("file header bytes mismatch:\n got: % x\nwant: % x", buf.Bytes(), want)
	}
	if buf.Len() != fileHeaderSize {
		t.Errorf("file header size = %d, want %d", buf.Len(), fileHeaderSize)
	}
}

func TestEncodeFileHeader_FeatureBitsSet(t *testing.T) {
	var buf bytes.Buffer
	hdr := fileHeader{
		attrs:      section{offset: 104, size: 136},
		data:       section{offset: 240, size: 1024},
		eventTypes: section{offset: 0, size: 0},
		// HEADER_BUILD_ID = 2, HEADER_HOSTNAME = 4 → mask = (1<<2) | (1<<4) = 0x14
		addsFeatures: (1 << featBuildID) | (1 << featHostname),
	}
	encodeFileHeader(&buf, hdr)

	got := buf.Bytes()
	if buf.Len() != fileHeaderSize {
		t.Fatalf("size = %d, want %d", buf.Len(), fileHeaderSize)
	}
	// adds_features starts at offset 72 (8+8+8+16+16+16 = 72)
	if got[72] != 0x14 {
		t.Errorf("adds_features[0] = 0x%02x, want 0x14", got[72])
	}
}
