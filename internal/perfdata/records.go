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
