package symbolize

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// fakeIndex is a fixture ModuleIndex: a flat list of ranges, no /proc.
type fakeIndex struct {
	pid  uint32
	maps []procmap.Mapping
	hits int
}

func (f *fakeIndex) Lookup(pid uint32, addr uint64) (procmap.Mapping, bool) {
	f.hits++
	if pid != f.pid {
		return procmap.Mapping{}, false
	}
	for _, m := range f.maps {
		if addr >= m.Start && addr < m.Limit {
			return m, true
		}
	}
	return procmap.Mapping{}, false
}

func libcuda() procmap.Mapping {
	return procmap.Mapping{
		Path:    "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		Start:   0x7f2c94700000,
		Limit:   0x7f2c96000000,
		Offset:  0x1000,
		BuildID: "cafe",
		IsExec:  true,
	}
}

func TestAttachModules_NamesTheModuleOfAnUnresolvedFrame(t *testing.T) {
	idx := &fakeIndex{pid: 77, maps: []procmap.Mapping{libcuda()}}
	frames := []Frame{
		{Address: 0x7f2c958b71c6, Name: "0x7f2c958b71c6", Reason: FailureMissingSymbols},
	}

	attached, bare := attachModules(idx, 77, frames)
	if attached != 1 || bare != 0 {
		t.Fatalf("attached=%d bare=%d, want 1/0", attached, bare)
	}
	f := frames[0]
	if f.Module != "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
		t.Errorf("Module = %q", f.Module)
	}
	if f.BuildID != "cafe" {
		t.Errorf("BuildID = %q", f.BuildID)
	}
	off, ok := f.ModuleOffset()
	if !ok {
		t.Fatal("ModuleOffset() not ok")
	}
	// 0x7f2c958b71c6 - 0x7f2c94700000 + 0x1000
	if want := uint64(0x11b81c6); off != want {
		t.Errorf("ModuleOffset() = %#x, want %#x", off, want)
	}
}

func TestAttachModules_LeavesResolvedFramesAlone(t *testing.T) {
	idx := &fakeIndex{pid: 77, maps: []procmap.Mapping{libcuda()}}
	frames := []Frame{
		{Address: 0x7f2c958b71c6, Name: "cuLaunchKernel", Reason: FailureNone},
	}

	attached, bare := attachModules(idx, 77, frames)
	if attached != 0 || bare != 0 {
		t.Fatalf("attached=%d bare=%d, want 0/0", attached, bare)
	}
	if idx.hits != 0 {
		t.Errorf("looked up %d addresses for a fully resolved stack; want 0", idx.hits)
	}
	if frames[0].Name != "cuLaunchKernel" || frames[0].Module != "" {
		t.Errorf("resolved frame mutated: %+v", frames[0])
	}
}

func TestAttachModules_KeepsAModuleTheSymbolizerAlreadyKnew(t *testing.T) {
	idx := &fakeIndex{pid: 77, maps: []procmap.Mapping{libcuda()}}
	frames := []Frame{{
		Address: 0x7f2c958b71c6,
		Name:    "0x7f2c958b71c6",
		Module:  "/somewhere/else.so",
		Reason:  FailureMissingSymbols,
	}}

	attachModules(idx, 77, frames)
	if frames[0].Module != "/somewhere/else.so" {
		t.Errorf("overwrote the symbolizer's own module: %q", frames[0].Module)
	}
}

// An address in no mapping must stay a bare address. This is the case the
// whole design has to keep visible: naming a module we do not know would be
// worse than the hex.
func TestAttachModules_NoMappingStaysBare(t *testing.T) {
	idx := &fakeIndex{pid: 77, maps: []procmap.Mapping{libcuda()}}
	frames := []Frame{
		{Address: 0x400000, Name: "0x400000", Reason: FailureMissingSymbols},
	}

	attached, bare := attachModules(idx, 77, frames)
	if attached != 0 || bare != 1 {
		t.Fatalf("attached=%d bare=%d, want 0/1", attached, bare)
	}
	f := frames[0]
	if f.Module != "" || f.MapStart != 0 || f.MapLimit != 0 || f.MapOff != 0 {
		t.Errorf("invented a mapping for an unmapped address: %+v", f)
	}
	if _, ok := f.ModuleOffset(); ok {
		t.Error("ModuleOffset() ok for a frame with no mapping")
	}
}

// No index configured is not the same as an index that found nothing, but
// both must leave the frame bare and both must be counted.
func TestAttachModules_NilIndexCountsBare(t *testing.T) {
	frames := []Frame{
		{Address: 0x400000, Name: "0x400000", Reason: FailureMissingSymbols},
		{Address: 0x401000, Name: "main", Reason: FailureNone},
	}
	attached, bare := attachModules(nil, 77, frames)
	if attached != 0 || bare != 1 {
		t.Fatalf("attached=%d bare=%d, want 0/1", attached, bare)
	}
	if frames[0].Module != "" {
		t.Errorf("nil index produced a module: %q", frames[0].Module)
	}
}

func TestModuleOffset_RejectsAddressOutsideItsOwnMapping(t *testing.T) {
	f := Frame{
		Address:  0x10,
		Module:   "/lib/x.so",
		MapStart: 0x1000,
		MapLimit: 0x2000,
	}
	if _, ok := f.ModuleOffset(); ok {
		t.Error("ModuleOffset() ok for an address below its mapping")
	}
}

func TestToProfFrames_CarriesModuleAndUnresolvedBit(t *testing.T) {
	frames := []Frame{
		{
			Address: 0x7f2c958b71c6, Name: "0x7f2c958b71c6", Reason: FailureMissingSymbols,
			Module: "/usr/lib/libcuda.so.1", BuildID: "cafe",
			MapStart: 0x7f2c94700000, MapLimit: 0x7f2c96000000, MapOff: 0x1000,
		},
		{Address: 0x401000, Name: "main", Reason: FailureNone},
	}
	out := ToProfFrames(frames)
	if len(out) != 2 {
		t.Fatalf("got %d frames", len(out))
	}
	if !out[0].Unresolved {
		t.Error("unresolved frame did not carry Unresolved")
	}
	if out[0].MapStart != 0x7f2c94700000 || out[0].MapLimit != 0x7f2c96000000 || out[0].MapOff != 0x1000 {
		t.Errorf("mapping not carried: %+v", out[0])
	}
	if out[0].BuildID != "cafe" {
		t.Errorf("build id not carried: %q", out[0].BuildID)
	}
	if out[1].Unresolved {
		t.Error("resolved frame marked Unresolved")
	}
}

// An inline chain only exists where resolution succeeded, so no frame in one
// may be marked unresolved - that would drag real function names through the
// module+offset rename.
func TestToProfFrames_InlinedFramesAreNeverUnresolved(t *testing.T) {
	frames := []Frame{{
		Address: 0x401000, Name: "outer", Reason: FailureNone,
		Module:  "/bin/app",
		Inlined: []Frame{{Name: "inner"}},
	}}
	for _, f := range ToProfFrames(frames) {
		if f.Unresolved {
			t.Errorf("frame %q marked Unresolved", f.Name)
		}
	}
}
