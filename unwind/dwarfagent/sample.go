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

// SampleRecordBytes is the full record size: header + MaxFrames × u64.
const SampleRecordBytes = SampleHeaderBytes + MaxFrames*8

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
	PCs         []uint64
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
//	[40:..] PCs
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
	return s, nil
}
