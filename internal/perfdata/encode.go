// Package perfdata writes Linux kernel perf.data files. The on-disk format
// is documented in tools/perf/Documentation/perf.data-file-format.txt in
// the Linux kernel tree. perf.data is little-endian on every supported
// architecture, so we hardcode that here.
package perfdata

import (
	"encoding/binary"
	"io"
)

// align8 rounds n up to the next multiple of 8. perf.data uses 8-byte
// alignment for record bodies and string fields.
func align8(n int) int {
	return (n + 7) &^ 7
}

// writeUint32LE writes a little-endian 32-bit unsigned integer.
func writeUint32LE(w io.Writer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, _ = w.Write(b[:])
}

// writeUint64LE writes a little-endian 64-bit unsigned integer.
func writeUint64LE(w io.Writer, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, _ = w.Write(b[:])
}

// writeUint16LE writes a little-endian 16-bit unsigned integer.
func writeUint16LE(w io.Writer, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	_, _ = w.Write(b[:])
}

// writeCStringPadded8 writes s as a NUL-terminated string, then pads to the
// next 8-byte boundary. perf.data uses this layout for filenames inside
// MMAP2 records, comm names, and feature-section strings.
func writeCStringPadded8(w io.Writer, s string) {
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte{0})
	pad := align8(len(s)+1) - (len(s) + 1)
	if pad > 0 {
		_, _ = w.Write(make([]byte, pad))
	}
}
