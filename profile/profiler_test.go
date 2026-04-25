package profile

import (
	"testing"

	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestBlazeSymToFramesAddress(t *testing.T) {
	s := blazesym.Sym{
		Name:   "foo",
		Module: "/usr/bin/target",
	}
	frames := blazeSymToFrames(s, 0xdeadbeef)

	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	if frames[0].Address != 0xdeadbeef {
		t.Fatalf("expected Address=0xdeadbeef, got %#x", frames[0].Address)
	}
	if frames[0].Name != "foo" {
		t.Errorf("Name mismatch: %q", frames[0].Name)
	}
}

func TestBlazeSymToFramesInlineSharesAddress(t *testing.T) {
	s := blazesym.Sym{
		Name:    "outer",
		Module:  "/usr/bin/target",
		Inlined: []blazesym.InlinedFn{{Name: "inner"}},
	}
	frames := blazeSymToFrames(s, 0x4000)

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames (1 inline + 1 outer), got %d", len(frames))
	}
	for i, f := range frames {
		if f.Address != 0x4000 {
			t.Errorf("frame %d Address=%#x, want 0x4000", i, f.Address)
		}
	}
}

func TestProfilerHasResolver(t *testing.T) {
	// Compile-time check: Profiler has a resolver field with the
	// expected procmap.Resolver type. Behavioral tests live in the
	// integration suite.
	var p Profiler
	var _ *procmap.Resolver = p.resolver
}
