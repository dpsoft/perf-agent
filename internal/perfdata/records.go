package perfdata

import "io"

// PERF_RECORD_* constants (uapi/linux/perf_event.h).
const (
	recordMmap          = 1
	recordComm          = 3
	recordExit          = 4
	recordSample        = 9
	recordMmap2         = 10
	recordFinishedRound = 12
)

// recordHeaderSize is the size of struct perf_event_header in bytes.
// 8 = u32 type + u16 misc + u16 size.
const recordHeaderSize = 8

// commRecord is the in-memory image of a PERF_RECORD_COMM payload.
type commRecord struct {
	pid  uint32
	tid  uint32
	comm string
}

// encodeComm writes a PERF_RECORD_COMM record (type 3). Layout:
//
//	struct perf_event_header header;  // 8 bytes
//	u32 pid;
//	u32 tid;
//	char comm[];                       // NUL-terminated, 8-byte padded
func encodeComm(w io.Writer, r commRecord) {
	commBytes := align8(len(r.comm) + 1) // NUL + padding
	size := recordHeaderSize + 4 + 4 + commBytes
	writeUint32LE(w, recordComm)
	writeUint16LE(w, 0) // misc
	writeUint16LE(w, uint16(size))
	writeUint32LE(w, r.pid)
	writeUint32LE(w, r.tid)
	writeCStringPadded8(w, r.comm)
}

// encodeFinishedRound writes a PERF_RECORD_FINISHED_ROUND record (type 12).
// No payload; consumers use it as a sync point. Total size = 8 bytes.
func encodeFinishedRound(w io.Writer) {
	writeUint32LE(w, recordFinishedRound)
	writeUint16LE(w, 0) // misc
	writeUint16LE(w, recordHeaderSize)
}

// miscMmapBuildID is set in perf_event_header.misc when the MMAP2 record
// uses the build-id flavour of its union (post-5.12 kernels).
const miscMmapBuildID = 1 << 14

// mmap2Record is the in-memory image of a PERF_RECORD_MMAP2 payload.
// Set hasBuildID when emitting the build-id flavour of the union;
// otherwise the maj/min/ino path is used (and all four fields stay zero).
type mmap2Record struct {
	pid, tid uint32
	addr     uint64
	len      uint64
	pgoff    uint64

	// union: build-id flavour
	hasBuildID  bool
	buildIDSize uint8
	buildID     [20]byte // padded to 20 bytes; SHA-1 build-ids are exactly that

	// (maj, min, ino, inoGen would go here for the file-id flavour;
	// we always emit zeros — consumers fall back to filename matching.)

	prot     uint32
	flags    uint32
	filename string
}

// encodeMmap2 writes a PERF_RECORD_MMAP2 record (type 10). The record carries
// either {maj, min, ino, ino_generation} (24 bytes) OR
// {build_id_size: u8, __reserved_1: u8[3], build_id: u8[20]} (24 bytes) in the
// same slot — selected by miscMmapBuildID in the header's misc field.
//
// Layout:
//
//	struct perf_event_header header;  // 8 bytes
//	u32 pid, u32 tid;                 // 8
//	u64 addr;                         // 8
//	u64 len;                          // 8
//	u64 pgoff;                        // 8
//	union { ino flavour | build-id flavour } // 24
//	u32 prot, u32 flags;              // 8
//	char filename[];                  // NUL-terminated, 8-byte padded
func encodeMmap2(w io.Writer, r mmap2Record) {
	filenameBytes := align8(len(r.filename) + 1)
	bodySize := 4 + 4 + 8 + 8 + 8 + 24 + 4 + 4 + filenameBytes
	size := recordHeaderSize + bodySize
	misc := uint16(0)
	if r.hasBuildID {
		misc |= miscMmapBuildID
	}

	writeUint32LE(w, recordMmap2)
	writeUint16LE(w, misc)
	writeUint16LE(w, uint16(size))

	writeUint32LE(w, r.pid)
	writeUint32LE(w, r.tid)
	writeUint64LE(w, r.addr)
	writeUint64LE(w, r.len)
	writeUint64LE(w, r.pgoff)

	// union (24 bytes)
	if r.hasBuildID {
		_, _ = w.Write([]byte{r.buildIDSize, 0, 0, 0}) // u8 + 3 reserved
		_, _ = w.Write(r.buildID[:])                   // 20 bytes
	} else {
		// maj=0, min=0, ino=0, ino_generation=0 — 24 bytes of zeros
		_, _ = w.Write(make([]byte, 24))
	}

	writeUint32LE(w, r.prot)
	writeUint32LE(w, r.flags)
	writeCStringPadded8(w, r.filename)
}
