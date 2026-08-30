// Package dwarfagent wires the perf_dwarf BPF program, the
// ehmaps lifecycle (TableStore / PIDTracker / MmapWatcher), and pprof
// output into a single Profiler with the same Collect/CollectAndWrite
// shape as profile.Profiler. The user-visible entry point is
// `perf-agent --profile --unwind dwarf --pid N`, which in
// perfagent.Start() dispatches to dwarfagent.NewProfiler instead of
// profile.NewProfiler.
package dwarfagent

import (
	"encoding/binary"
	"fmt"
)

// MaxFrames matches bpf/unwind_common.h's MAX_FRAMES (127).
const MaxFrames = 127

// SampleHeaderBytes matches the struct sample_header in
// bpf/unwind_common.h (40 bytes including padding + kern_stack).
const SampleHeaderBytes = 40

// sampleRecordTagsPadBytes is the compiler-inserted padding after
// tags[MAX_FRAMES] that rounds struct sample_record up to its 8-byte
// alignment (driven by the u64 pcs[] field): 40 + 127*8 + 127 = 1183 bytes
// of content, padded to 1184. Getting this wrong is exactly what
// TestSampleRecordBytesMatchesGeneratedStruct in sample_test.go exists to
// catch — SampleRecordBytes disagreed with the real (padded) struct size
// from 1183 to 1184 until that test was added (issue #83 review).
const sampleRecordTagsPadBytes = 1

// SampleRecordBytes is the full record size: header + MaxFrames × u64 pcs
// + MaxFrames × u8 tags + the trailing alignment pad. The tags trailer
// (FRAME_TAG_* in bpf/unwind_common.h, issue #83) IS decoded by parseSample
// below: walk_step's interpreter arm pushes FRAME_TAG_PYTHON pairs into this
// same record, so a PCs-only parse would hand two words of a Python frame to
// the symbolizer as if they were instruction pointers.
const SampleRecordBytes = SampleHeaderBytes + MaxFrames*8 + MaxFrames + sampleRecordTagsPadBytes

// Sample is the userspace parse of one ringbuf stack_events record.
//
// KernStack is the BPF stack-ID produced by bpf_get_stackid against
// kern_stackmap when kernel_stacks_enabled is on, or -1 otherwise.
// Userspace looks the stack back up via the session's KernStackmap
// accessor (see common.consumeRingbuf for the lookup path).
type Sample struct {
	PID         uint32
	TID         uint32
	TimeNs      uint64
	Value       uint64
	Mode        uint8
	WalkerFlags uint8
	KernStack   int64

	// PCs holds the walk's slot words, leaf first — NOT a flat list of
	// instruction pointers. A Python frame occupies two consecutive slots
	// (issue #83), so Tags must be consulted before any of these reaches a
	// symbolizer or a perf.data user-IP stream. splitFrameSlots does that.
	PCs []uint64
	// Tags is one FRAME_TAG_* byte per PCs slot, same length as PCs. Empty
	// only for a record short enough that the tags array was truncated,
	// which splitFrameSlots reads as all-native.
	Tags []uint8
}

// parseSample decodes one stack_events record. nPCs is clamped to
// MaxFrames. Returns an error if buf is smaller than the 40-byte
// header; a short PC array (buf truncated) is silently clamped rather
// than errored, matching the resilience posture of the ringbuf
// consumer pattern.
//
// Layout (matches generated perf_dwarfSampleRecord and
// offcpu_dwarfSampleRecord; both share unwind_common.h's sample_header):
//
//	[0:4]   PID
//	[4:8]   TID
//	[8:16]  TimeNs
//	[16:24] Value
//	[24]    Mode
//	[25]    N_pcs
//	[26]    WalkerFlags
//	[27]    _pad
//	[28:32] _pad2
//	[32:40] KernStack (int64)
//	[40:1056]   PCs (MaxFrames × u64)
//	[1056:1183] Tags (MaxFrames × u8, one FRAME_TAG_* byte per pcs[] slot)
//	[1183:1184] compiler-inserted alignment padding (not meaningful data)
//
// Only the first nPCs tags are read, for the same reason only the first nPCs
// PCs are: the BPF side stages both arrays in a per-CPU scratch buffer and
// copies them whole, so the slots past nPCs still hold the previous sample's
// bytes on that CPU. Reading one stale tag would fold two real native frames
// into an invented Python one.
func parseSample(buf []byte) (Sample, error) {
	if len(buf) < SampleHeaderBytes {
		return Sample{}, fmt.Errorf("sample truncated: %d bytes, need >= %d", len(buf), SampleHeaderBytes)
	}
	s := Sample{
		PID:         binary.LittleEndian.Uint32(buf[0:4]),
		TID:         binary.LittleEndian.Uint32(buf[4:8]),
		TimeNs:      binary.LittleEndian.Uint64(buf[8:16]),
		Value:       binary.LittleEndian.Uint64(buf[16:24]),
		Mode:        buf[24],
		WalkerFlags: buf[26],
		KernStack:   int64(binary.LittleEndian.Uint64(buf[32:40])),
	}
	nPCs := int(buf[25])
	if nPCs > MaxFrames {
		nPCs = MaxFrames
	}
	pcEnd := SampleHeaderBytes + nPCs*8
	if pcEnd > len(buf) {
		nPCs = (len(buf) - SampleHeaderBytes) / 8
	}
	s.PCs = make([]uint64, nPCs)
	for i := range nPCs {
		off := SampleHeaderBytes + i*8
		s.PCs[i] = binary.LittleEndian.Uint64(buf[off : off+8])
	}

	// Tags, clamped the same way the PC array is: a buffer truncated before
	// or inside the tags region yields fewer tags than PCs, and
	// splitFrameSlots treats a missing tag as native — the pre-#83 reading,
	// which is the safe direction. It can only under-report Python frames,
	// never invent one.
	tagsOff := SampleHeaderBytes + MaxFrames*8
	nTags := nPCs
	if tagsOff+nTags > len(buf) {
		nTags = len(buf) - tagsOff
	}
	if nTags > 0 {
		s.Tags = make([]uint8, nTags)
		copy(s.Tags, buf[tagsOff:tagsOff+nTags])
	}
	return s, nil
}
