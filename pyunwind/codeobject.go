package pyunwind

import (
	"encoding/binary"
	"fmt"
)

// CodeOffsets locates the fields of a PyCodeObject that name a frame.
//
// Zero means "not measured for this interpreter", and a zero table makes the
// resolver decline rather than read whatever happens to be at offset 0. That
// is the same discipline the frame offsets follow: a version this build has
// not been measured against renders python:0x… exactly as before, which is
// honest, instead of a plausible-looking name read from the wrong field.
type CodeOffsets struct {
	Qualname  uint16 // PyObject *co_qualname  ("Widget.method_here")
	Filename  uint16 // PyObject *co_filename
	FirstLine uint16 // int co_firstlineno
}

// Measured returns whether this build knows where to look.
func (c CodeOffsets) Measured() bool { return c.Qualname != 0 && c.Filename != 0 }

// PyASCIIObject's layout, which carries every co_qualname and co_filename in
// practice: they are compact ASCII, being identifiers and paths.
//
// Not per-version: this header has been stable across the versions this
// package supports, and the state bits below are checked on every read, so a
// layout that moved is caught as "not compact ascii" and declined rather than
// decoded into nonsense.
const (
	asciiLengthOff = 16 // Py_ssize_t length
	asciiStateOff  = 32 // struct { unsigned int interned:2; kind:3; compact:1; ascii:1; ... }
	asciiDataOff   = 40 // sizeof(PyASCIIObject): the payload follows the header
)

// maxPyStringBytes bounds a single decoded string. A qualname or a path beyond
// this is not something to render; it is a sign the address was not a string
// at all, and reading it would be an unbounded read into the target.
const maxPyStringBytes = 4096

// CodeInfo is what a code object says about a frame.
type CodeInfo struct {
	Qualname  string // "Widget.method_here" -- qualified, not the bare co_name
	Filename  string
	FirstLine uint32
}

// readPyASCII decodes a compact-ASCII PyUnicodeObject at addr.
//
// The compact and ascii state bits are verified rather than assumed. A
// non-compact or non-ASCII string (a path with non-ASCII characters, a
// legacy-layout object) is declined: its payload is not where this reads, and
// a wrong answer here becomes a wrong function name in a flame graph, which is
// worse than no name.
func readPyASCII(r *ProcReader, addr uint64) (string, error) {
	if addr == 0 {
		return "", fmt.Errorf("pyunwind: nil string")
	}
	lenB, err := r.read(addr+asciiLengthOff, 8)
	if err != nil {
		return "", err
	}
	n := binary.LittleEndian.Uint64(lenB)
	if n == 0 {
		return "", nil
	}
	if n > maxPyStringBytes {
		return "", fmt.Errorf("pyunwind: implausible string length %d", n)
	}
	stateB, err := r.read(addr+asciiStateOff, 4)
	if err != nil {
		return "", err
	}
	state := binary.LittleEndian.Uint32(stateB)
	const compactBit, asciiBit = 1 << 5, 1 << 6
	if state&compactBit == 0 || state&asciiBit == 0 {
		return "", fmt.Errorf("pyunwind: not compact-ascii (state=%#x)", state)
	}
	b, err := r.read(addr+asciiDataOff, int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadCodeInfo reads the naming fields out of one PyCodeObject.
//
// The target must be ALIVE: this reads its memory. Callers on a profiler's
// collect path have already lost that race -- see the resolver's cache, which
// exists so the reads happen during the capture and the names survive the
// process.
func ReadCodeInfo(r *ProcReader, off CodeOffsets, codeObject uint64) (CodeInfo, error) {
	var out CodeInfo
	if !off.Measured() {
		return out, fmt.Errorf("pyunwind: code offsets not measured for this interpreter")
	}
	if codeObject == 0 {
		return out, fmt.Errorf("pyunwind: nil code object")
	}
	qnPtr, err := r.ReadU64(codeObject + uint64(off.Qualname))
	if err != nil {
		return out, err
	}
	fnPtr, err := r.ReadU64(codeObject + uint64(off.Filename))
	if err != nil {
		return out, err
	}
	if out.Qualname, err = readPyASCII(r, qnPtr); err != nil {
		return out, err
	}
	if out.Filename, err = readPyASCII(r, fnPtr); err != nil {
		return out, err
	}
	if off.FirstLine != 0 {
		lb, lerr := r.read(codeObject+uint64(off.FirstLine), 4)
		if lerr == nil {
			out.FirstLine = binary.LittleEndian.Uint32(lb)
		}
	}
	return out, nil
}
