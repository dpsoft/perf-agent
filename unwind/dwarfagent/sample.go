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
// bpf/unwind_common.h (32 bytes including padding).
const SampleHeaderBytes = 32

// SampleRecordBytes is the full record size: header + MaxFrames × u64.
const SampleRecordBytes = SampleHeaderBytes + MaxFrames*8

// Sample is the userspace parse of one ringbuf stack_events record.
type Sample struct {
	PID         uint32
	TID         uint32
	TimeNs      uint64
	Value       uint64
	Mode        uint8
	WalkerFlags uint8
	PCs         []uint64
}

// parseSample decodes one stack_events record. nPCs is clamped to
// MaxFrames. Returns an error if buf is smaller than the 32-byte
// header; a short PC array (buf truncated) is silently clamped rather
// than errored, matching the resilience posture of the ringbuf
// consumer pattern.
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
	}
	nPCs := int(buf[25])
	if nPCs > MaxFrames {
		nPCs = MaxFrames
	}
	pcEnd := SampleHeaderBytes + nPCs*8
	if pcEnd > len(buf) {
		nPCs = (len(buf) - SampleHeaderBytes) / 8
		pcEnd = SampleHeaderBytes + nPCs*8
	}
	s.PCs = make([]uint64, nPCs)
	for i := range nPCs {
		off := SampleHeaderBytes + i*8
		s.PCs[i] = binary.LittleEndian.Uint64(buf[off : off+8])
	}
	return s, nil
}
