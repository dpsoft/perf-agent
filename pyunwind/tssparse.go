// Package pyunwind recovers the offsets needed to walk CPython's
// PyThreadState chain from machine code and data structures, without any
// per-version offset table where that can be avoided.
package pyunwind

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// ErrTSSPatternUnrecognised means the function body did not match any shape
// this parser knows. It is deliberately an error and not a fallback: the
// alternative is guessing an offset, and a wrong offset yields a plausible
// stack of garbage frames rather than no frames.
var ErrTSSPatternUnrecognised = errors.New("pyunwind: PyGILState_GetThisThreadState has an unrecognised shape")

// fixedRun is a run of machine-code bytes that must appear verbatim at a
// fixed offset in the function body. Everything a shape does NOT cover with
// a fixedRun or an off32 field is a per-binary displacement (a RIP-relative
// disp32, or a call/jmp rel32) that carries no information this parser uses
// and is therefore not checked.
type fixedRun struct {
	at    int
	bytes []byte
}

// tssShape is one MEASURED machine-code shape of
// PyGILState_GetThisThreadState. Every shape here was read out of a real
// shipped libpython with objdump; none is hypothesised. `size` is the exact
// symbol size, and a body of any other length is refused before a single
// instruction byte is inspected.
//
// off32At lists the byte positions of the little-endian u32 fields that
// encode the offset of _PyRuntime's autoTSSkey. Where a shape has more than
// one they must all agree numerically; where it has one, the structural
// match is what stands between this parser and a false accept -- see the
// note on shape 2 below.
type tssShape struct {
	// name says which build this shape was measured from, so a future
	// mismatch can be reproduced rather than guessed at.
	name    string
	size    int
	fixed   []fixedRun
	off32At []int
}

// The shapes, in the order they are tried (which is irrelevant: sizes are
// distinct, so at most one can match a given body).
//
// WHY THERE IS MORE THAN ONE. The spike measured a single 35-byte shape and
// found it byte-identical across CPython 3.12, 3.13 and 3.14 -- which it is,
// across every interpreter built the way those were. It is not a property of
// CPython: this function's body is whatever the toolchain emitted, and the
// three shapes below were measured across four builds of the SAME 3.12
// series:
//
//	35 bytes  python:3.12.14-slim, python:3.13.15-slim, python:3.14.3-slim
//	          (Debian, gcc 12) and Fedora 44's python3.14 (gcc 16).
//	          PyThread_tss_is_created is inlined to a cmpl.
//	44 bytes  actions/setup-python 3.12.14 for ubuntu-24.04 (gcc 13.3.0,
//	          PGO). PyThread_tss_is_created is a real call through the PLT.
//	64 bytes  Ubuntu 24.04's own libpython3.12 (gcc 13, frame pointers
//	          kept -- Ubuntu builds -fno-omit-frame-pointer since 24.04).
//	          Same call, plus a frame prologue and epilogue.
//
// The 44-byte shape is the one CI actually runs: without it, Attach refuses
// every interpreter on the integration runners and no Python frame is ever
// walked there. That is how it was found.
//
// A body that matches none of these is REFUSED, which is the designed
// behaviour: a new toolchain shape shows up as a named refusal
// (ErrTSSPatternUnrecognised, carried on Result.Reason) with the body's
// length in the message, not as a wrong offset. Adding a fourth shape is a
// data change plus a testdata body measured from the binary that motivated
// it.
var tssShapes = []tssShape{
	{
		// 35 bytes, eight instructions:
		//
		//	[0:4]    f3 0f 1e fa       endbr64
		//	[4:7]    48 8b 05 <disp32> mov  disp32(%rip),%rax   disp32 at [7:11]
		//	[11:13]  83 b8 <off32> 00  cmpl $0x0,off32(%rax)    off32  at [13:17]
		//	[18:20]  74 0c             je   +0x0c
		//	[20:23]  48 8d b8 <off32>  lea  off32(%rax),%rdi     off32  at [23:27]
		//	[27]     e9 <rel32>        jmp  PyThread_tss_get     rel32  at [28:32]
		//	[32:34]  31 c0             xor  %eax,%eax
		//	[34]     c3                ret
		//
		// The cmpl tests autoTSSkey._is_initialized at +0 and the lea
		// computes &autoTSSkey._key, also at +0 of the same struct, which
		// is why both off32 fields hold the offset of the Py_tss_t STRUCT
		// and must agree.
		name:    "35-byte inlined-tss_is_created (Debian/Fedora)",
		size:    35,
		off32At: []int{13, 23},
		fixed: []fixedRun{
			{0, []byte{0xf3, 0x0f, 0x1e, 0xfa}}, // endbr64
			{4, []byte{0x48, 0x8b, 0x05}},       // mov disp32(%rip),%rax
			{11, []byte{0x83, 0xb8}},            // cmpl $0x0,off32(%rax)
			{17, []byte{0x00}},                  //   ... its immediate
			{18, []byte{0x74, 0x0c}},            // je +0x0c
			{20, []byte{0x48, 0x8d, 0xb8}},      // lea off32(%rax),%rdi
			{27, []byte{0xe9}},                  // jmp PyThread_tss_get
			{32, []byte{0x31, 0xc0}},            // xor %eax,%eax
			{34, []byte{0xc3}},                  // ret
		},
	},
	{
		// 44 bytes, measured from actions/setup-python's
		// python-3.12.14-linux-24.04-x64 libpython3.12.so.1.0 at 0x1b831e:
		//
		//	[0:4]    f3 0f 1e fa       endbr64
		//	[4:7]    48 8b 05 <disp32> mov  disp32(%rip),%rax   disp32 at [7:11]
		//	[11]     53                push %rbx
		//	[12:15]  48 8d 98 <off32>  lea  off32(%rax),%rbx    off32  at [15:19]
		//	[19:22]  48 89 df          mov  %rbx,%rdi
		//	[22]     e8 <rel32>        call PyThread_tss_is_created  rel32 at [23:27]
		//	[27:29]  85 c0             test %eax,%eax
		//	[29:31]  74 09             je   +0x09
		//	[31:34]  48 89 df          mov  %rbx,%rdi
		//	[34]     5b                pop  %rbx
		//	[35]     e9 <rel32>        jmp  PyThread_tss_get     rel32  at [36:40]
		//	[40:42]  31 c0             xor  %eax,%eax
		//	[42]     5b                pop  %rbx
		//	[43]     c3                ret
		//
		// ONE off32, so there is no numeric cross-check here -- unlike the
		// 35-byte shape, where the cmpl's and the lea's offsets must agree.
		// The structural match carries it: the lea is required at exactly
		// [12], between a push %rbx at [11] and a mov %rbx,%rdi at [19],
		// with the whole 44-byte instruction sequence around it. The
		// cross-check was never the load-bearing part anyway (see the
		// reordered-instructions regression test): positional decoding is.
		name:    "44-byte called-tss_is_created (setup-python gcc 13 PGO)",
		size:    44,
		off32At: []int{15},
		fixed: []fixedRun{
			{0, []byte{0xf3, 0x0f, 0x1e, 0xfa}}, // endbr64
			{4, []byte{0x48, 0x8b, 0x05}},       // mov disp32(%rip),%rax
			{11, []byte{0x53}},                  // push %rbx
			{12, []byte{0x48, 0x8d, 0x98}},      // lea off32(%rax),%rbx
			{19, []byte{0x48, 0x89, 0xdf}},      // mov %rbx,%rdi
			{22, []byte{0xe8}},                  // call PyThread_tss_is_created
			{27, []byte{0x85, 0xc0}},            // test %eax,%eax
			{29, []byte{0x74, 0x09}},            // je +0x09
			{31, []byte{0x48, 0x89, 0xdf}},      // mov %rbx,%rdi
			{34, []byte{0x5b}},                  // pop %rbx
			{35, []byte{0xe9}},                  // jmp PyThread_tss_get
			{40, []byte{0x31, 0xc0}},            // xor %eax,%eax
			{42, []byte{0x5b}},                  // pop %rbx
			{43, []byte{0xc3}},                  // ret
		},
	},
	{
		// 64 bytes, measured from Ubuntu 24.04's libpython3.12.so.1.0 at
		// 0x2f9580 (same call as the 44-byte shape, wrapped in the frame
		// prologue/epilogue Ubuntu's -fno-omit-frame-pointer build keeps,
		// and with the two returns laid out around a 4-byte nop pad):
		//
		//	[0:4]    f3 0f 1e fa       endbr64
		//	[4]      55                push %rbp
		//	[5:8]    48 89 e5          mov  %rsp,%rbp
		//	[8]      53                push %rbx
		//	[9:13]   48 83 ec 08       sub  $0x8,%rsp
		//	[13:16]  48 8b 05 <disp32> mov  disp32(%rip),%rax   disp32 at [16:20]
		//	[20:23]  48 8d 98 <off32>  lea  off32(%rax),%rbx    off32  at [23:27]
		//	[27:30]  48 89 df          mov  %rbx,%rdi
		//	[30]     e8 <rel32>        call PyThread_tss_is_created  rel32 at [31:35]
		//	[35:37]  85 c0             test %eax,%eax
		//	[37:39]  74 11             je   +0x11
		//	[39:42]  48 89 df          mov  %rbx,%rdi
		//	[42:46]  48 8b 5d f8       mov  -0x8(%rbp),%rbx
		//	[46]     c9                leave
		//	[47]     e9 <rel32>        jmp  PyThread_tss_get     rel32  at [48:52]
		//	[52:56]  0f 1f 40 00       nopl 0x0(%rax)
		//	[56:60]  48 8b 5d f8       mov  -0x8(%rbp),%rbx
		//	[60:62]  31 c0             xor  %eax,%eax
		//	[62]     c9                leave
		//	[63]     c3                ret
		//
		// One off32, for the same reason and with the same standing as the
		// 44-byte shape above.
		name:    "64-byte frame-pointer build (Ubuntu 24.04 system python3.12)",
		size:    64,
		off32At: []int{23},
		fixed: []fixedRun{
			{0, []byte{0xf3, 0x0f, 0x1e, 0xfa}},  // endbr64
			{4, []byte{0x55}},                    // push %rbp
			{5, []byte{0x48, 0x89, 0xe5}},        // mov %rsp,%rbp
			{8, []byte{0x53}},                    // push %rbx
			{9, []byte{0x48, 0x83, 0xec, 0x08}},  // sub $0x8,%rsp
			{13, []byte{0x48, 0x8b, 0x05}},       // mov disp32(%rip),%rax
			{20, []byte{0x48, 0x8d, 0x98}},       // lea off32(%rax),%rbx
			{27, []byte{0x48, 0x89, 0xdf}},       // mov %rbx,%rdi
			{30, []byte{0xe8}},                   // call PyThread_tss_is_created
			{35, []byte{0x85, 0xc0}},             // test %eax,%eax
			{37, []byte{0x74, 0x11}},             // je +0x11
			{39, []byte{0x48, 0x89, 0xdf}},       // mov %rbx,%rdi
			{42, []byte{0x48, 0x8b, 0x5d, 0xf8}}, // mov -0x8(%rbp),%rbx
			{46, []byte{0xc9}},                   // leave
			{47, []byte{0xe9}},                   // jmp PyThread_tss_get
			{52, []byte{0x0f, 0x1f, 0x40, 0x00}}, // nopl 0x0(%rax)
			{56, []byte{0x48, 0x8b, 0x5d, 0xf8}}, // mov -0x8(%rbp),%rbx
			{60, []byte{0x31, 0xc0}},             // xor %eax,%eax
			{62, []byte{0xc9}},                   // leave
			{63, []byte{0xc3}},                   // ret
		},
	},
}

// ParseAutoTSSKeyOffset recovers the offset of _PyRuntime's autoTSSkey from
// the machine code of PyGILState_GetThisThreadState.
//
// Each known shape is decoded POSITIONALLY, not scanned for: every fixed
// opcode byte is required at its exact offset, in order, adjacent, for the
// whole body -- not merely present somewhere in it. An earlier version of
// this parser scanned independently for the cmpl and the lea anywhere in
// the input and cross-checked only that the two off32 values agreed
// numerically; that scan-and-cross-check approach does NOT stop a
// coincidental byte sequence elsewhere in the body from being read as an
// offset; it accepted a 36-byte body containing a lea and a cmpl with
// matching offsets in reversed order, separated by filler, with no
// endbr64/mov/je/jmp at all. Positional decoding of the full instruction
// sequence is what actually closes that off: nothing but the real function
// body can satisfy every fixed byte at every fixed offset.
//
// 3.11 is deliberately not handled: it passes the key VALUE to
// pthread_getspecific@plt rather than a pointer to PyThread_tss_get, which
// is a different shape and a different parser. See the spec's non-goals.
func ParseAutoTSSKeyOffset(code []byte) (uint32, error) {
	for _, s := range tssShapes {
		if len(code) != s.size {
			continue
		}
		off, err := s.decode(code)
		if err != nil {
			return 0, err
		}
		return off, nil
	}
	return 0, fmt.Errorf("%w: %d-byte body matches none of the known shapes (%s)",
		ErrTSSPatternUnrecognised, len(code), shapeSizes())
}

// decode checks one shape against a body already known to be its length.
func (s tssShape) decode(code []byte) (uint32, error) {
	for _, f := range s.fixed {
		got := code[f.at : f.at+len(f.bytes)]
		if !bytes.Equal(got, f.bytes) {
			return 0, fmt.Errorf("%w: %s: byte %d: expected % x, got % x",
				ErrTSSPatternUnrecognised, s.name, f.at, f.bytes, got)
		}
	}
	first := binary.LittleEndian.Uint32(code[s.off32At[0] : s.off32At[0]+4])
	for _, at := range s.off32At[1:] {
		other := binary.LittleEndian.Uint32(code[at : at+4])
		if other != first {
			return 0, fmt.Errorf("%w: %s: offset at byte %d (%#x) disagrees with offset at byte %d (%#x)",
				ErrTSSPatternUnrecognised, s.name, s.off32At[0], first, at, other)
		}
	}
	return first, nil
}

// shapeSizes renders the accepted body lengths for the refusal message, so
// an operator hitting a new toolchain shape can see at a glance that the
// length itself is what did not match.
func shapeSizes() string {
	var b strings.Builder
	b.WriteString("known sizes:")
	for i, s := range tssShapes {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " %d", s.size)
	}
	return b.String()
}
