// Package pyunwind recovers the offsets needed to walk CPython's
// PyThreadState chain from machine code and data structures, without any
// per-version offset table where that can be avoided.
package pyunwind

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrTSSPatternUnrecognised means the function body did not match the shape
// this parser knows. It is deliberately an error and not a fallback: the
// alternative is guessing an offset, and a wrong offset yields a plausible
// stack of garbage frames rather than no frames.
var ErrTSSPatternUnrecognised = errors.New("pyunwind: PyGILState_GetThisThreadState has an unrecognised shape")

// ParseAutoTSSKeyOffset recovers the offset of _PyRuntime's autoTSSkey from
// the machine code of PyGILState_GetThisThreadState.
//
// The function is byte-identical in shape across CPython 3.12, 3.13 and
// 3.14 -- 35 bytes, eight instructions -- with only the RIP displacement
// and this offset varying:
//
//	[0:4]    f3 0f 1e fa       endbr64
//	[4:7]    48 8b 05 <disp32> mov  disp32(%rip),%rax     disp32 at [7:11]
//	[11:13]  83 b8 <off32> 00  cmpl $0x0,off32(%rax)      off32  at [13:17]
//	[18:20]  74 0c             je   +0x0c
//	[20:23]  48 8d b8 <off32>  lea  off32(%rax),%rdi       off32  at [23:27]
//	[27]     e9 <rel32>        jmp  PyThread_tss_get       rel32  at [28:32]
//	[32:34]  31 c0             xor  %eax,%eax
//	[34]     c3                ret
//
// This is decoded positionally, not scanned for: every fixed opcode byte
// is required at its exact offset, in order, adjacent, for the whole
// 35-byte body -- not merely present somewhere in it. An earlier version
// of this parser scanned independently for the cmpl and the lea anywhere
// in the input and cross-checked only that the two off32 values agreed
// numerically; that scan-and-cross-check approach does NOT stop a
// coincidental byte sequence elsewhere in the body from being read as an
// offset; it accepted a 36-byte body containing a lea and a cmpl with
// matching offsets in reversed order, separated by filler, with no
// endbr64/mov/je/jmp at all. Positional decoding of the full instruction
// sequence is what actually closes that off: nothing but the real
// function body can satisfy every fixed byte at every fixed offset.
//
// The two off32 fields (the cmpl's and the lea's) are still required to
// agree, but that is no longer the only thing standing between this
// parser and a false accept -- it is redundant with, not a substitute
// for, the structural match above.
//
// 3.11 is deliberately not handled: it passes the key VALUE to
// pthread_getspecific@plt rather than a pointer to PyThread_tss_get, which
// is a different shape and a different parser. See the spec's non-goals.
func ParseAutoTSSKeyOffset(code []byte) (uint32, error) {
	const wantLen = 35
	if len(code) != wantLen {
		return 0, fmt.Errorf("%w: expected %d bytes, got %d", ErrTSSPatternUnrecognised, wantLen, len(code))
	}

	if !bytes.Equal(code[0:4], []byte{0xf3, 0x0f, 0x1e, 0xfa}) {
		return 0, fmt.Errorf("%w: byte 0: expected endbr64 (f3 0f 1e fa), got % x", ErrTSSPatternUnrecognised, code[0:4])
	}
	if !bytes.Equal(code[4:7], []byte{0x48, 0x8b, 0x05}) {
		return 0, fmt.Errorf("%w: byte 4: expected mov opcode (48 8b 05), got % x", ErrTSSPatternUnrecognised, code[4:7])
	}
	// code[7:11] is the mov's RIP-relative disp32 -- it varies per binary
	// (it points at the &_PyRuntime.autoTSSkey-holding relocation) and is
	// not otherwise validated.
	if !bytes.Equal(code[11:13], []byte{0x83, 0xb8}) {
		return 0, fmt.Errorf("%w: byte 11: expected cmpl opcode (83 b8), got % x", ErrTSSPatternUnrecognised, code[11:13])
	}
	cmpOff := binary.LittleEndian.Uint32(code[13:17])
	if code[17] != 0x00 {
		return 0, fmt.Errorf("%w: byte 17: expected cmpl immediate 0x00, got %#x", ErrTSSPatternUnrecognised, code[17])
	}
	if !bytes.Equal(code[18:20], []byte{0x74, 0x0c}) {
		return 0, fmt.Errorf("%w: byte 18: expected je +0x0c (74 0c), got % x", ErrTSSPatternUnrecognised, code[18:20])
	}
	if !bytes.Equal(code[20:23], []byte{0x48, 0x8d, 0xb8}) {
		return 0, fmt.Errorf("%w: byte 20: expected lea opcode (48 8d b8), got % x", ErrTSSPatternUnrecognised, code[20:23])
	}
	leaOff := binary.LittleEndian.Uint32(code[23:27])
	if code[27] != 0xe9 {
		return 0, fmt.Errorf("%w: byte 27: expected jmp opcode (e9), got %#x", ErrTSSPatternUnrecognised, code[27])
	}
	// code[28:32] is the jmp's rel32 to PyThread_tss_get -- varies per
	// binary and is not otherwise validated.
	if !bytes.Equal(code[32:34], []byte{0x31, 0xc0}) {
		return 0, fmt.Errorf("%w: byte 32: expected xor opcode (31 c0), got % x", ErrTSSPatternUnrecognised, code[32:34])
	}
	if code[34] != 0xc3 {
		return 0, fmt.Errorf("%w: byte 34: expected ret (c3), got %#x", ErrTSSPatternUnrecognised, code[34])
	}

	if cmpOff != leaOff {
		return 0, fmt.Errorf("%w: cmp offset %#x disagrees with lea offset %#x",
			ErrTSSPatternUnrecognised, cmpOff, leaOff)
	}
	return leaOff, nil
}
