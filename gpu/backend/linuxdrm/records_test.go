package linuxdrm

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDecodeRecord(t *testing.T) {
	in := rawRecord{
		Kind:       recordKindIOCtl,
		PID:        123,
		TID:        124,
		FD:         9,
		Command:    0xc04064,
		ResultCode: -11,
		StartNs:    1000,
		EndNs:      1200,
		DeviceID:   77,
	}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, in); err != nil {
		t.Fatalf("binary.Write: %v", err)
	}

	got, err := decodeRecord(buf.Bytes())
	if err != nil {
		t.Fatalf("decodeRecord: %v", err)
	}
	if got != in {
		t.Fatalf("record mismatch: got %#v want %#v", got, in)
	}
}

func TestDecodeRecordRejectsTruncatedBytes(t *testing.T) {
	if _, err := decodeRecord([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error")
	}
}
