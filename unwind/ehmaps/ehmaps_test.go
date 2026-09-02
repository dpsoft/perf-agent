package ehmaps

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"regexp"
	"strconv"
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
	want := make([]byte, 32)
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
	// want[21:32] is tail padding (already zeroed by make)
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalCFIEntry:\n got %x\nwant %x", got, want)
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

// The Go mirror of the walker's search bound must match the header, because
// the whole point of MaxSearchableRows is to refuse exactly the tables the BPF
// side cannot search. A drift here re-opens the silent failure: tables get
// installed that every lookup misses.
func TestSearchBoundMirrorsTheBPFHeader(t *testing.T) {
	src, err := os.ReadFile("../../bpf/unwind_common.h")
	if err != nil {
		t.Fatalf("read unwind_common.h: %v", err)
	}
	m := regexp.MustCompile(`(?m)^#define\s+BINARY_SEARCH_MAX_ITERS\s+(\d+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("BINARY_SEARCH_MAX_ITERS not found in bpf/unwind_common.h")
	}
	want, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if binarySearchMaxIters != want {
		t.Fatalf("Go mirror is %d, bpf/unwind_common.h says %d: tables the walker cannot search "+
			"would be installed and every lookup in them would miss", binarySearchMaxIters, want)
	}
}

// ceil(log2(n)) -- the property MaxSearchableRows is derived from, and the
// number the refusal message quotes.
func TestBitsNeeded(t *testing.T) {
	for _, tc := range []struct{ n, want int }{
		{1, 0}, {2, 1}, {3, 2}, {4, 2}, {5, 3},
		{1 << 20, 20}, {(1 << 20) + 1, 21},
		{2359137, 22}, // libtorch_cpu.so, the binary that exposed this
		{947971, 20},  // libtorch_cuda.so, which sat exactly on the old bound
	} {
		if got := bitsNeeded(tc.n); got != tc.want {
			t.Errorf("bitsNeeded(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// A table the search cannot reach is refused BY NAME rather than installed.
// Installing one is worse than installing nothing: with no table the walker
// frame-pointer-walks, while with an unsearchable table cfi_lookup also
// returns NULL, which walk_step reads as a CFI miss and stops on.
func TestAnUnsearchableTableIsRefused(t *testing.T) {
	err := PopulateCFI(PopulateCFIArgs{
		TableID: 0x1234,
		Entries: make([]ehcompile.CFIEntry, MaxSearchableRows+1),
	})
	if !errors.Is(err, ErrTableTooLarge) {
		t.Fatalf("err = %v, want ErrTableTooLarge", err)
	}
}
