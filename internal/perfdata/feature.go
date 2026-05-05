package perfdata

import "io"

// recordHeaderBuildID is the in-feature record type that wraps each build-id
// entry inside the HEADER_BUILD_ID feature section. Distinct from
// PERF_RECORD_* in the data section.
const recordHeaderBuildID = 67

// BuildIDEntry is one entry in the HEADER_BUILD_ID feature section.
// Pid = -1 means "kernel or any process" (kernel mappings); other values are
// actual host PIDs.
type BuildIDEntry struct {
	Pid      int32
	BuildID  [20]byte
	Filename string
}

// encodeBuildIDFeature writes a HEADER_BUILD_ID feature section payload —
// a sequence of build-id records back-to-back. Each record:
//
//	struct perf_event_header header;  // type=67, misc=0, size=record total
//	s32 pid;
//	u8  build_id[24];                  // 20 hash bytes + 4 padding
//	char filename[];                   // NUL-terminated, 8-byte padded
func encodeBuildIDFeature(w io.Writer, entries []BuildIDEntry) {
	for _, e := range entries {
		filenameBytes := align8(len(e.Filename) + 1)
		bodySize := 4 + 24 + filenameBytes
		size := recordHeaderSize + bodySize
		writeUint32LE(w, recordHeaderBuildID)
		writeUint16LE(w, 0)
		writeUint16LE(w, uint16(size))
		// pid (s32) — write the bit pattern as u32
		writeUint32LE(w, uint32(e.Pid))
		// build_id[24] = 20 hash bytes + 4 padding
		_, _ = w.Write(e.BuildID[:])
		_, _ = w.Write([]byte{0, 0, 0, 0})
		writeCStringPadded8(w, e.Filename)
	}
}

// encodeStringFeature writes a perf_header_string: u32 len (padded length
// including NUL), char str[len]. Used for HEADER_HOSTNAME and HEADER_OSRELEASE.
func encodeStringFeature(w io.Writer, s string) {
	padded := align8(len(s) + 1)
	writeUint32LE(w, uint32(padded))
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte{0})
	if pad := padded - (len(s) + 1); pad > 0 {
		_, _ = w.Write(make([]byte, pad))
	}
}

// encodeNRCPUSFeature writes the HEADER_NRCPUS feature section: two u32s,
// nr_cpus_online followed by nr_cpus_available.
func encodeNRCPUSFeature(w io.Writer, online, available uint32) {
	writeUint32LE(w, online)
	writeUint32LE(w, available)
}

// encodeFeatureIndexTable writes the perf_file_section table that follows
// the data section. Each entry is {offset: u64, size: u64} pointing at one
// feature's payload. Entries appear in feature-bit-number order.
func encodeFeatureIndexTable(w io.Writer, entries []section) {
	for _, e := range entries {
		writeUint64LE(w, e.offset)
		writeUint64LE(w, e.size)
	}
}
