package linuxdrm

import "testing"

func TestDecodeIOCtl(t *testing.T) {
	meta := decodeIOCtl(0xc04064)
	if meta.Number != 0x64 {
		t.Fatalf("number=%d", meta.Number)
	}
	if meta.Type != 0x40 {
		t.Fatalf("type=%d", meta.Type)
	}
	if meta.Size != 0xc0 {
		t.Fatalf("size=%d", meta.Size)
	}
	if meta.Dir != 0 {
		t.Fatalf("dir=%d", meta.Dir)
	}
	if meta.TypeChar != "@" {
		t.Fatalf("type_char=%q", meta.TypeChar)
	}
	if meta.DirName != "none" {
		t.Fatalf("dir_name=%q", meta.DirName)
	}
}
