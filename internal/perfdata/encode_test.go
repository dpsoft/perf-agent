package perfdata

import (
	"bytes"
	"testing"
)

func TestAlign8(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 0},
		{1, 8},
		{7, 8},
		{8, 8},
		{9, 16},
		{16, 16},
		{17, 24},
	}
	for _, c := range cases {
		if got := align8(c.in); got != c.want {
			t.Errorf("align8(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWriteCStringPadded8(t *testing.T) {
	cases := []struct {
		s    string
		want []byte
	}{
		{"", []byte{0, 0, 0, 0, 0, 0, 0, 0}}, // 8-byte zero pad
		{"ls", []byte{'l', 's', 0, 0, 0, 0, 0, 0}},
		{"abcdefg", []byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 0}},        // 8 bytes incl NUL
		{"abcdefgh", []byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 0, 0, 0, 0, 0, 0, 0, 0}}, // pads to 16
	}
	for _, c := range cases {
		var buf bytes.Buffer
		writeCStringPadded8(&buf, c.s)
		if !bytes.Equal(buf.Bytes(), c.want) {
			t.Errorf("writeCStringPadded8(%q) = %v, want %v", c.s, buf.Bytes(), c.want)
		}
	}
}

func TestWriteUint64LE(t *testing.T) {
	var buf bytes.Buffer
	writeUint64LE(&buf, 0x0123456789abcdef)
	want := []byte{0xef, 0xcd, 0xab, 0x89, 0x67, 0x45, 0x23, 0x01}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("writeUint64LE = % x, want % x", buf.Bytes(), want)
	}
}

func TestWriteUint32LE(t *testing.T) {
	var buf bytes.Buffer
	writeUint32LE(&buf, 0x01020304)
	want := []byte{0x04, 0x03, 0x02, 0x01}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("writeUint32LE = % x, want % x", buf.Bytes(), want)
	}
}

func TestWriteUint16LE(t *testing.T) {
	var buf bytes.Buffer
	writeUint16LE(&buf, 0x0102)
	want := []byte{0x02, 0x01}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("writeUint16LE = % x, want % x", buf.Bytes(), want)
	}
}
