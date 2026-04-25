package dwarfagent

import (
	"testing"

	blazesym "github.com/libbpf/blazesym/go"
)

func TestDwarfBlazeSymToFramesAddress(t *testing.T) {
	s := blazesym.Sym{Name: "bar", Module: "/lib/x.so"}
	frames := blazeSymToFrames(s, 0x1000)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Address != 0x1000 {
		t.Fatalf("expected Address=0x1000, got %#x", frames[0].Address)
	}
}
