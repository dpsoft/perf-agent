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

// CommRecord is the in-memory image of a PERF_RECORD_COMM payload.
type CommRecord struct {
	Pid  uint32
	Tid  uint32
	Comm string
}

// encodeComm writes a PERF_RECORD_COMM record (type 3). Layout:
//
//	struct perf_event_header header;  // 8 bytes
//	u32 pid;
//	u32 tid;
//	char comm[];                       // NUL-terminated, 8-byte padded
func encodeComm(w io.Writer, r CommRecord) {
	commBytes := align8(len(r.Comm) + 1) // NUL + padding
	size := recordHeaderSize + 4 + 4 + commBytes
	writeUint32LE(w, recordComm)
	writeUint16LE(w, 0) // misc
	writeUint16LE(w, uint16(size))
	writeUint32LE(w, r.Pid)
	writeUint32LE(w, r.Tid)
	writeCStringPadded8(w, r.Comm)
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

// Mmap2Record is the in-memory image of a PERF_RECORD_MMAP2 payload.
// Set HasBuildID when emitting the build-id flavour of the union;
// otherwise the maj/min/ino path is used (and all four fields stay zero).
type Mmap2Record struct {
	Pid, Tid uint32
	Addr     uint64
	Len      uint64
	Pgoff    uint64

	// union: build-id flavour
	HasBuildID  bool
	BuildIDSize uint8
	BuildID     [20]byte // padded to 20 bytes; SHA-1 build-ids are exactly that

	// (maj, min, ino, inoGen would go here for the file-id flavour;
	// we always emit zeros — consumers fall back to filename matching.)

	Prot     uint32
	Flags    uint32
	Filename string
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
func encodeMmap2(w io.Writer, r Mmap2Record) {
	filenameBytes := align8(len(r.Filename) + 1)
	bodySize := 4 + 4 + 8 + 8 + 8 + 24 + 4 + 4 + filenameBytes
	size := recordHeaderSize + bodySize
	misc := uint16(0)
	if r.HasBuildID {
		misc |= miscMmapBuildID
	}

	writeUint32LE(w, recordMmap2)
	writeUint16LE(w, misc)
	writeUint16LE(w, uint16(size))

	writeUint32LE(w, r.Pid)
	writeUint32LE(w, r.Tid)
	writeUint64LE(w, r.Addr)
	writeUint64LE(w, r.Len)
	writeUint64LE(w, r.Pgoff)

	// union (24 bytes)
	if r.HasBuildID {
		_, _ = w.Write([]byte{r.BuildIDSize, 0, 0, 0}) // u8 + 3 reserved
		_, _ = w.Write(r.BuildID[:])                   // 20 bytes
	} else {
		// maj=0, min=0, ino=0, ino_generation=0 — 24 bytes of zeros
		_, _ = w.Write(make([]byte, 24))
	}

	writeUint32LE(w, r.Prot)
	writeUint32LE(w, r.Flags)
	writeCStringPadded8(w, r.Filename)
}

// SampleRecord is the in-memory image of a PERF_RECORD_SAMPLE payload, for
// the fixed sample_type we emit:
//
//	IP | TID | TIME | CPU | PERIOD | CALLCHAIN
//
// (No ADDR, ID, STREAM_ID, READ, RAW, BRANCH_STACK, REGS_USER, STACK_USER,
// WEIGHT, DATA_SRC, TRANSACTION.)
type SampleRecord struct {
	IP        uint64   // PERF_SAMPLE_IP
	Pid, Tid  uint32   // PERF_SAMPLE_TID
	Time      uint64   // PERF_SAMPLE_TIME (ns since clock origin)
	Cpu       uint32   // PERF_SAMPLE_CPU (low 32 bits)
	Period    uint64   // PERF_SAMPLE_PERIOD
	Callchain []uint64 // PERF_SAMPLE_CALLCHAIN (leaf first, ips array)
}

// encodeSample writes a PERF_RECORD_SAMPLE record (type 9). Field order
// follows the sample_type bit order in uapi/linux/perf_event.h:
//
//	{ u64 ip; }                            // PERF_SAMPLE_IP
//	{ u32 pid, tid; }                      // PERF_SAMPLE_TID
//	{ u64 time; }                          // PERF_SAMPLE_TIME
//	{ u32 cpu, res; }                      // PERF_SAMPLE_CPU
//	{ u64 period; }                        // PERF_SAMPLE_PERIOD
//	{ u64 nr; u64 ips[nr]; }               // PERF_SAMPLE_CALLCHAIN
func encodeSample(w io.Writer, r SampleRecord) {
	bodySize := 8 + 8 + 8 + 8 + 8 + 8 + 8*len(r.Callchain)
	size := recordHeaderSize + bodySize
	writeUint32LE(w, recordSample)
	writeUint16LE(w, 0) // misc — could carry CPUMODE_USER etc. but blazesym handles that downstream
	writeUint16LE(w, uint16(size))

	writeUint64LE(w, r.IP)
	writeUint32LE(w, r.Pid)
	writeUint32LE(w, r.Tid)
	writeUint64LE(w, r.Time)
	writeUint32LE(w, r.Cpu)
	writeUint32LE(w, 0) // res
	writeUint64LE(w, r.Period)
	writeUint64LE(w, uint64(len(r.Callchain)))
	for _, ip := range r.Callchain {
		writeUint64LE(w, ip)
	}
}
