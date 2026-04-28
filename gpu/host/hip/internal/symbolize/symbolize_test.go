package symbolize

import (
	"testing"

	blazesym "github.com/libbpf/blazesym/go"
)

func TestBlazeSymToFramesLeafFirst(t *testing.T) {
	frames := blazeSymToFrames(blazesym.Sym{
		Name:   "outer",
		Module: "/opt/libhip.so",
		Inlined: []blazesym.InlinedFn{
			{Name: "inline_outer"},
			{Name: "inline_inner"},
		},
	}, 0x1234)

	if len(frames) != 3 {
		t.Fatalf("frames=%d", len(frames))
	}
	if frames[0].Name != "inline_inner" {
		t.Fatalf("frame[0]=%q", frames[0].Name)
	}
	if frames[1].Name != "inline_outer" {
		t.Fatalf("frame[1]=%q", frames[1].Name)
	}
	if frames[2].Name != "outer" {
		t.Fatalf("frame[2]=%q", frames[2].Name)
	}
}
