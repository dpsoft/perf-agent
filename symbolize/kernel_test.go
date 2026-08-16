package symbolize

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/pprof"
)

func TestNoopKernelSymbolizer(t *testing.T) {
	var s NoopKernelSymbolizer
	frames, err := s.SymbolizeKernel([]uint64{0xffffffff8100abcd, 0xffffffff8100ef01})
	if err != nil {
		t.Fatalf("SymbolizeKernel: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Name != "0xffffffff8100abcd" {
		t.Errorf("frame[0].Name = %q, want hex form", frames[0].Name)
	}
	if frames[0].Address != 0xffffffff8100abcd {
		t.Errorf("frame[0].Address = %#x, want input IP", frames[0].Address)
	}
	if frames[0].Reason != FailureMissingSymbols {
		t.Errorf("frame[0].Reason = %s, want FailureMissingSymbols", frames[0].Reason)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNoopKernelSymbolizerEmpty(t *testing.T) {
	var s NoopKernelSymbolizer
	frames, err := s.SymbolizeKernel(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if frames != nil {
		t.Fatalf("frames = %+v, want nil", frames)
	}
}

func TestMergeKernelFirst(t *testing.T) {
	kernel := []Frame{{Name: "kfn1"}, {Name: "kfn2"}}
	user := []Frame{{Name: "ufn1"}, {Name: "ufn2"}}

	got := MergeKernelFirst(kernel, user)
	want := []Frame{{Name: "kfn1"}, {Name: "kfn2"}, {Name: "ufn1"}, {Name: "ufn2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeKernelFirst: got %+v, want %+v", got, want)
	}
}

func TestMergeKernelFirstEmptyKernel(t *testing.T) {
	user := []Frame{{Name: "ufn1"}}
	got := MergeKernelFirst(nil, user)
	if !reflect.DeepEqual(got, user) {
		t.Fatalf("got %+v, want %+v", got, user)
	}
}

func TestMergeKernelFirstEmptyUser(t *testing.T) {
	kernel := []Frame{{Name: "kfn1"}}
	got := MergeKernelFirst(kernel, nil)
	if !reflect.DeepEqual(got, kernel) {
		t.Fatalf("got %+v, want %+v", got, kernel)
	}
}

func TestToProfFramesKernelSetsIsKernel(t *testing.T) {
	in := []Frame{{Address: 0xffff800000001000, Name: "do_sys_openat2", Module: "[kernel.kallsyms]"}}
	got := ToProfFramesKernel(in)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if !got[0].IsKernel {
		t.Errorf("IsKernel = false, want true")
	}
	if got[0].Name != "do_sys_openat2" {
		t.Errorf("Name = %q", got[0].Name)
	}
	// Sanity: ToProfFrames-without-kernel shouldn't set the flag.
	plain := ToProfFrames(in)
	if plain[0].IsKernel {
		t.Errorf("ToProfFrames set IsKernel; should only be set by ToProfFramesKernel")
	}
}

func TestErrKernelSymbolsUnavailable(t *testing.T) {
	if !errors.Is(ErrKernelSymbolsUnavailable, ErrKernelSymbolsUnavailable) {
		t.Fatal("ErrKernelSymbolsUnavailable should be matchable via errors.Is")
	}
	if ErrKernelSymbolsUnavailable.Error() == "" {
		t.Fatal("ErrKernelSymbolsUnavailable.Error() must be non-empty")
	}
	// Type-only smoke-check — the symbol type must satisfy the interface.
	var _ KernelSymbolizer = NoopKernelSymbolizer{}
	_ = pprof.Frame{} // keep import used
}
