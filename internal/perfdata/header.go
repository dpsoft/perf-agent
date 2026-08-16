package perfdata

import "io"

// fileHeaderSize is the on-disk size of struct perf_file_header in bytes.
// 104 = 8 (magic) + 8 (size) + 8 (attr_size) + 16 (attrs section) +
//       16 (data section) + 16 (event_types section) + 32 (adds_features bitmap).
const fileHeaderSize = 104

// magicPERFILE2 is the uint64 value that, when serialized little-endian via
// writeUint64LE, lands the ASCII bytes 'P','E','R','F','I','L','E','2' on
// disk in that order. Decoded high-to-low: 0x32 ('2'), 0x45 ('E'), 0x4c
// ('L'), 0x49 ('I'), 0x46 ('F'), 0x52 ('R'), 0x45 ('E'), 0x50 ('P') — i.e.
// "PERFILE2" reversed. The kernel matches the on-disk literal, not the
// big-endian value.
const magicPERFILE2 uint64 = 0x32454c4946524550

// attrV8Size is the on-disk size of struct perf_event_attr at version 8 of
// the format (the canonical modern size).
const attrV8Size = 136

// section is a {offset, size} pointer into the file. Used both inside
// the file header and inside feature-section index entries.
type section struct {
	offset uint64
	size   uint64
}

// Feature bit indices. Subset of HEADER_* in tools/perf/util/header.h.
// Names match the kernel constants minus the HEADER_ prefix.
const (
	featTracingData  = 1
	featBuildID      = 2
	featHostname     = 3
	featOSRelease    = 4
	featVersion      = 5
	featArch         = 6
	featNRCPUS       = 7
	featCPUDesc      = 8
	featCPUID        = 9
	featTotalMem     = 10
	featCmdLine      = 11
	featEventDesc    = 12
	featCPUTopology  = 13
	featNUMATopology = 14
	featBranchStack  = 15
	// ... up to HEADER_LAST_FEATURE around 31; we only emit a small subset.
)

// fileHeader is the in-memory representation of the on-disk perf_file_header.
// All fields are filled by the Writer; encodeFileHeader serializes them.
type fileHeader struct {
	attrs        section
	data         section
	eventTypes   section
	addsFeatures uint64 // bitmap, lower 64 bits only (we use no features above bit 31)
}

// encodeFileHeader writes the 104-byte file header.
func encodeFileHeader(w io.Writer, h fileHeader) {
	writeUint64LE(w, magicPERFILE2)
	writeUint64LE(w, fileHeaderSize)
	writeUint64LE(w, attrV8Size)
	writeUint64LE(w, h.attrs.offset)
	writeUint64LE(w, h.attrs.size)
	writeUint64LE(w, h.data.offset)
	writeUint64LE(w, h.data.size)
	writeUint64LE(w, h.eventTypes.offset)
	writeUint64LE(w, h.eventTypes.size)
	// adds_features is a 4×u64 bitmap. We only need the first u64.
	writeUint64LE(w, h.addsFeatures)
	writeUint64LE(w, 0)
	writeUint64LE(w, 0)
	writeUint64LE(w, 0)
}
