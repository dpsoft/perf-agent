// Package pyunwind recovers the offsets needed to walk CPython's
// PyThreadState chain from machine code and data structures, without any
// per-version offset table where that can be avoided.
package pyunwind

import (
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
//	mov  <disp32>(%rip),%rax     48 8b 05 <disp32>
//	cmpl $0x0,<off32>(%rax)      83 b8 <off32> 00
//	je   +0x0c                   74 0c
//	lea  <off32>(%rax),%rdi      48 8d b8 <off32>
//	jmp  PyThread_tss_get        e9 <rel32>
//
// We match on the `lea <off32>(%rax),%rdi` because it is the instruction
// that actually forms the argument, and cross-check it against the `cmpl`
// that guards it. Requiring both to agree is what stops a coincidental
// byte sequence elsewhere in the body from being read as an offset.
//
// 3.11 is deliberately not handled: it passes the key VALUE to
// pthread_getspecific@plt rather than a pointer to PyThread_tss_get, which
// is a different shape and a different parser. See the spec's non-goals.
func ParseAutoTSSKeyOffset(code []byte) (uint32, error) {
	var cmpOff, leaOff uint32
	var haveCmp, haveLea bool

	for i := 0; i+7 <= len(code); i++ {
		// cmpl $0x0, off32(%rax)  ==  83 b8 <off32> 00
		if !haveCmp && code[i] == 0x83 && code[i+1] == 0xb8 && code[i+6] == 0x00 {
			cmpOff = binary.LittleEndian.Uint32(code[i+2 : i+6])
			haveCmp = true
		}
		// lea off32(%rax), %rdi   ==  48 8d b8 <off32>
		if !haveLea && code[i] == 0x48 && code[i+1] == 0x8d && code[i+2] == 0xb8 {
			leaOff = binary.LittleEndian.Uint32(code[i+3 : i+7])
			haveLea = true
		}
	}
	if !haveCmp || !haveLea {
		return 0, ErrTSSPatternUnrecognised
	}
	if cmpOff != leaOff {
		return 0, fmt.Errorf("%w: cmp offset %#x disagrees with lea offset %#x",
			ErrTSSPatternUnrecognised, cmpOff, leaOff)
	}
	return leaOff, nil
}
