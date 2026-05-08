package symbolize

import (
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/pprof"
)

func TestToProfFramesLeafFirst(t *testing.T) {
	in := []Frame{
		{
			Address: 0x401000,
			Name:    "outer",
			Module:  "/bin/x",
			File:    "x.c", Line: 100,
			Inlined: []Frame{
				{Address: 0x401000, Name: "caller_inline", Module: "/bin/x", File: "x.c", Line: 50},
				{Address: 0x401000, Name: "callee_inline", Module: "/bin/x", File: "x.c", Line: 60},
			},
		},
	}
	got := ToProfFrames(in)
	want := []pprof.Frame{
		// Inline chain expands leaf-first (reverse of caller→callee order).
		{Name: "callee_inline", Module: "/bin/x", File: "x.c", Line: uint32(60), Address: 0x401000},
		{Name: "caller_inline", Module: "/bin/x", File: "x.c", Line: uint32(50), Address: 0x401000},
		{Name: "outer", Module: "/bin/x", File: "x.c", Line: uint32(100), Address: 0x401000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToProfFrames mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestToProfFramesEmpty(t *testing.T) {
	if got := ToProfFrames(nil); got != nil {
		t.Fatalf("ToProfFrames(nil) = %+v, want nil", got)
	}
}
