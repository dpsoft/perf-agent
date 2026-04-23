package ehmaps

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

func TestTableIDForBuildIDKnownValue(t *testing.T) {
	// FNV-1a 64-bit of 20 bytes of 0xAA. Known-value anchor; if the
	// calculation drifts, the test catches it.
	buildID := make([]byte, 20)
	for i := range buildID {
		buildID[i] = 0xAA
	}
	const want uint64 = 0x88ebb801b154ad85
	if got := TableIDForBuildID(buildID); got != want {
		t.Fatalf("TableIDForBuildID(0xAA*20) = %#x, want %#x", got, want)
	}
}

func TestTableIDForBuildIDDiffersByInput(t *testing.T) {
	a := TableIDForBuildID([]byte{1, 2, 3})
	b := TableIDForBuildID([]byte{1, 2, 4})
	if a == b {
		t.Fatalf("distinct inputs produced same table_id %#x", a)
	}
}

func TestTableIDForBuildIDEmpty(t *testing.T) {
	// FNV-1a offset basis for an empty input.
	const want uint64 = 0xcbf29ce484222325
	if got := TableIDForBuildID(nil); got != want {
		t.Fatalf("empty buildID = %#x, want %#x", got, want)
	}
}

func TestMarshalCFIEntryMatchesBPFLayout(t *testing.T) {
	e := ehcompile.CFIEntry{
		PCStart:    0x1234_5678_9abc_def0,
		PCEndDelta: 0x0102_0304,
		CFAType:    ehcompile.CFATypeSP,
		FPType:     ehcompile.FPTypeOffsetCFA,
		CFAOffset:  -16,
		FPOffset:   -32,
		RAOffset:   -8,
		RAType:     ehcompile.RATypeOffsetCFA,
	}
	got := MarshalCFIEntry(e)
	want := make([]byte, 24)
	binary.LittleEndian.PutUint64(want[0:8], 0x1234_5678_9abc_def0)
	binary.LittleEndian.PutUint32(want[8:12], 0x0102_0304)
	want[12] = 1 // cfa_type = SP
	want[13] = 1 // fp_type = OffsetCFA
	cfa := int16(-16)
	binary.LittleEndian.PutUint16(want[14:16], uint16(cfa))
	fp := int16(-32)
	binary.LittleEndian.PutUint16(want[16:18], uint16(fp))
	ra := int16(-8)
	binary.LittleEndian.PutUint16(want[18:20], uint16(ra))
	want[20] = 1 // ra_type = OffsetCFA
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalCFIEntry:\n got %x\nwant %x", got, want)
	}
}

func TestMarshalClassificationMatchesBPFLayout(t *testing.T) {
	c := ehcompile.Classification{
		PCStart:    0xdeadbeef_cafef00d,
		PCEndDelta: 42,
		Mode:       ehcompile.ModeFPLess,
	}
	got := MarshalClassification(c)
	want := make([]byte, 16)
	binary.LittleEndian.PutUint64(want[0:8], 0xdeadbeef_cafef00d)
	binary.LittleEndian.PutUint32(want[8:12], 42)
	want[12] = 1 // mode = FPLess
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalClassification:\n got %x\nwant %x", got, want)
	}
}

func TestMarshalPIDMapping(t *testing.T) {
	m := PIDMapping{VMAStart: 0x400000, VMAEnd: 0x500000, LoadBias: 0x400000, TableID: 0x12345}
	got := MarshalPIDMapping(m)
	want := make([]byte, 32)
	binary.LittleEndian.PutUint64(want[0:8], 0x400000)
	binary.LittleEndian.PutUint64(want[8:16], 0x500000)
	binary.LittleEndian.PutUint64(want[16:24], 0x400000)
	binary.LittleEndian.PutUint64(want[24:32], 0x12345)
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalPIDMapping:\n got %x\nwant %x", got, want)
	}
}
