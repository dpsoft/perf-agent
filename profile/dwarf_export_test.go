package profile

import (
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/pyunwind"
)

// taggedFrame describes one frame to stage into a test sampleRecord: a
// native frame is one word (a PC), a Python frame is two words (code
// object address, then the encoded fingerprint/f_lasti word) — mirroring
// bpf/unwind_common.h's frame_push_native / frame_push_python.
type taggedFrame struct {
	tag   uint8
	words []uint64
}

// makeSampleRecord lays frames out into a sampleRecord's pcs[]/tags[]
// arrays back to back, in order, and sets NPcs to the total word count.
// Fails the test outright if the frames don't fit in 127 slots — every
// test that hits this path is a test bug, not a runtime condition.
func makeSampleRecord(t *testing.T, frames []taggedFrame) *sampleRecord {
	t.Helper()
	rec := &sampleRecord{}
	n := 0
	for _, f := range frames {
		for _, w := range f.words {
			if n >= len(rec.Pcs) {
				t.Fatalf("makeSampleRecord: frames overflow %d slots", len(rec.Pcs))
			}
			rec.Tags[n] = f.tag
			rec.Pcs[n] = w
			n++
		}
	}
	rec.NPcs = uint8(n)
	return rec
}

func TestTaggedFramesDecodeNativeOnly(t *testing.T) {
	// A record holding three native frames must decode to three PCs,
	// unchanged, with no Python frames.
	rec := makeSampleRecord(t, []taggedFrame{
		{tag: frameTagNative, words: []uint64{0x1000}},
		{tag: frameTagNative, words: []uint64{0x2000}},
		{tag: frameTagNative, words: []uint64{0x3000}},
	})
	pcs, py := decodeFrames(rec)
	require.Equal(t, []uint64{0x1000, 0x2000, 0x3000}, pcs)
	require.Empty(t, py, "no python frames were pushed")
}

func TestTaggedFramesDecodeMixed(t *testing.T) {
	// Native and Python frames interleaved: Python frames consume two
	// slots each and must not be mistaken for a pair of native PCs.
	rec := makeSampleRecord(t, []taggedFrame{
		{tag: frameTagNative, words: []uint64{0x1000}},
		{tag: frameTagPython, words: []uint64{0xc0de, 0x2a}},
		{tag: frameTagNative, words: []uint64{0x3000}},
	})
	pcs, py := decodeFrames(rec)
	require.Equal(t, []uint64{0x1000, 0x3000}, pcs)
	require.Equal(t, []PythonFrame{{CodeObject: 0xc0de, Encoded: 0x2a}}, py)
}

func TestTaggedFramesDecodeTruncatedPythonPair(t *testing.T) {
	// A Python tag on the last valid slot with no partner word is a
	// truncated pair (e.g. MAX_FRAMES cut the record mid-frame). It must
	// be dropped, not half-read as a bogus native PC or an out-of-bounds
	// access.
	rec := makeSampleRecord(t, []taggedFrame{
		{tag: frameTagNative, words: []uint64{0x1000}},
	})
	rec.Tags[rec.NPcs] = frameTagPython
	rec.Pcs[rec.NPcs] = 0xc0de
	rec.NPcs++

	before := FrameDecodeCounters().TruncatedPythonPairs
	pcs, py := decodeFrames(rec)
	require.Equal(t, []uint64{0x1000}, pcs)
	require.Empty(t, py, "truncated python pair must be dropped, not half-read")

	after := FrameDecodeCounters().TruncatedPythonPairs
	require.Equal(t, before+1, after,
		"a dropped truncated pair must increment TruncatedPythonPairs, never be silent")
}

// ----- Issue #83: the interpreter arm rides on the shared walk_step.
//
// perf_dwarf, offcpu_dwarf and gpu_usdt all drive the same bpf_loop callback,
// so the Python walk reaches all three by construction rather than by three
// integrations. That is only true if the maps it needs actually came along
// with the header into each embedded object — assert it here rather than
// discover it on a machine with capabilities.
func TestEmbeddedDWARFProgramsCarryThePythonMaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		load func() (*ebpf.CollectionSpec, error)
	}{
		{"perf_dwarf", loadPerf_dwarf},
		{"offcpu_dwarf", loadOffcpu_dwarf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := tc.load()
			if err != nil {
				t.Fatalf("load spec: %v", err)
			}
			for _, m := range []string{"py_procs", "py_eval_ranges", "py_walk_counters"} {
				if _, ok := spec.Maps[m]; !ok {
					t.Errorf("%s is missing: the Python walk cannot run in this program", m)
				}
			}
			if got := spec.Maps["py_walk_counters"].MaxEntries; got != pyunwind.PyCntMax {
				t.Errorf("py_walk_counters has %d slots, pyunwind expects %d", got, pyunwind.PyCntMax)
			}
			if got := spec.Maps["py_walk_counters"].Type; got != ebpf.PerCPUArray {
				t.Errorf("py_walk_counters is %v, want PerCPUArray", got)
			}
			if got := spec.Maps["py_eval_ranges"].KeySize; got != 8 {
				t.Errorf("py_eval_ranges key is %d bytes, want 8 (table_id, not pid)", got)
			}
		})
	}
}

// Adding the Python resume cursor to struct walk_ctx must not have moved the
// sample record: walk_ctx lives on the BPF stack, the record lives in a map,
// and unwind/dwarfagent parses the record by byte offset.
func TestThePythonCursorDidNotResizeTheSampleRecord(t *testing.T) {
	if PerfDwarfSampleRecordSize != 1184 {
		t.Fatalf("sample_record is %d bytes, want 1184: every offset in unwind/dwarfagent's parser moved",
			PerfDwarfSampleRecordSize)
	}
}
