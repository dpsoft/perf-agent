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
// keyAt lists the byte positions of the little-endian u32 fields that
// encode where autoTSSkey is. Where a shape has more than one they must all
// agree numerically; where it has one, the structural match is what stands
// between this parser and a false accept -- see the note on shape 2 below.
//
// WHAT THOSE u32s MEAN DEPENDS ON `absolute`, and the two forms are not
// interchangeable. A PIC build loads &_PyRuntime through the GOT and adds a
// displacement, so the field is an OFFSET from _PyRuntime. A non-PIE build
// materialises the address itself as an immediate (`mov $0xb379c8,%edi`),
// so the field is the LINK-TIME ADDRESS of the Py_tss_t. Reading one as the
// other yields either a wild address or an offset of several megabytes; see
// AutoTSSKeyRef, which keeps the distinction all the way to the read.
type tssShape struct {
	// name says which build this shape was measured from, so a future
	// mismatch can be reproduced rather than guessed at.
	name     string
	size     int
	fixed    []fixedRun
	keyAt    []int
	absolute bool
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
//	37 bytes  Ubuntu 24.04's /usr/bin/python3.12 -- the statically linked
//	          interpreter executable, non-PIE, so it names autoTSSkey's
//	          address outright instead of offsetting a GOT-loaded base.
//	44 bytes  actions/setup-python 3.12.14 for ubuntu-24.04 (gcc 13.3.0,
//	          PGO). PyThread_tss_is_created is a real call through the PLT.
//	64 bytes  Ubuntu 24.04's own libpython3.12.so.1.0 (gcc 13, frame
//	          pointers kept -- Ubuntu builds -fno-omit-frame-pointer since
//	          24.04). Same call, plus a frame prologue and epilogue.
//
// WHICH ONE CI RUNS, measured rather than assumed: the 37-byte one. Both CI
// jobs resolve `python3` to /usr/bin/python3.12 -- the integration job runs
// its tests under sudo, whose secure_path drops actions/setup-python's
// tool-cache directory from PATH -- so the 44-byte body, which is what that
// tool-cache interpreter ships, is not the one the tests reach. Handling
// only the 35-byte shape meant Attach refused every interpreter on the
// runners; that is how the 44-byte one was found (by reading setup-python's
// binary) and then how the 37-byte one was found (by CI failing on the
// interpreter the tests actually reach).
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
		name:  "35-byte inlined-tss_is_created (Debian/Fedora)",
		size:  35,
		keyAt: []int{13, 23},
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
		name:  "44-byte called-tss_is_created (setup-python gcc 13 PGO)",
		size:  44,
		keyAt: []int{15},
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
		name:  "64-byte frame-pointer build (Ubuntu 24.04 system python3.12)",
		size:  64,
		keyAt: []int{23},
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
	{
		// 37 bytes, measured from Ubuntu 24.04's /usr/bin/python3.12 at
		// 0x608550 -- the STATICALLY LINKED interpreter executable, which
		// Debian and Ubuntu build separately from the shared
		// libpython3.12.so.1.0 the 64-byte shape above came from. It is
		// non-PIE, so the compiler materialises the address of
		// _PyRuntime.autoTSSkey as a 32-bit immediate instead of adding a
		// displacement to a GOT-loaded base:
		//
		//	[0:4]    f3 0f 1e fa       endbr64
		//	[4]      55                push %rbp
		//	[5]      bf <imm32>        mov  $&autoTSSkey,%edi   imm32 at [6:10]
		//	[10:13]  48 89 e5          mov  %rsp,%rbp
		//	[13]     e8 <rel32>        call PyThread_tss_is_created  rel32 at [14:18]
		//	[18:20]  85 c0             test %eax,%eax
		//	[20:22]  0f 84 <rel32>     je   <outlined return 0>  rel32 at [22:26]
		//	[26]     bf <imm32>        mov  $&autoTSSkey,%edi   imm32 at [27:31]
		//	[31]     5d                pop  %rbp
		//	[32]     e9 <rel32>        jmp  PyThread_tss_get     rel32 at [33:37]
		//
		// TWO imm32 fields that must agree, so this shape carries the same
		// numeric cross-check the 35-byte one does. `absolute` is what
		// makes the difference legible downstream: the number here is
		// 0xb379c8, an ADDRESS, not the 0x608 offset the other three shapes
		// of this same CPython version encode. AutoTSSKeyRef.Resolve
		// converts it, and refuses if it does not land inside _PyRuntime.
		//
		// This shape is why the live-interpreter tests are worth running in
		// CI: it was found by them failing on a runner's own
		// /usr/bin/python3.12, an interpreter no machine here has.
		name:     "37-byte non-PIE absolute-immediate (Ubuntu 24.04 /usr/bin/python3.12)",
		size:     37,
		keyAt:    []int{6, 27},
		absolute: true,
		fixed: []fixedRun{
			{0, []byte{0xf3, 0x0f, 0x1e, 0xfa}}, // endbr64
			{4, []byte{0x55}},                   // push %rbp
			{5, []byte{0xbf}},                   // mov $imm32,%edi
			{10, []byte{0x48, 0x89, 0xe5}},      // mov %rsp,%rbp
			{13, []byte{0xe8}},                  // call PyThread_tss_is_created
			{18, []byte{0x85, 0xc0}},            // test %eax,%eax
			{20, []byte{0x0f, 0x84}},            // je <outlined return 0>
			{26, []byte{0xbf}},                  // mov $imm32,%edi
			{31, []byte{0x5d}},                  // pop %rbp
			{32, []byte{0xe9}},                  // jmp PyThread_tss_get
		},
	},
}

// AutoTSSKeyRef is what a decoded function body says about where
// _PyRuntime's autoTSSkey lives. It is deliberately NOT a bare number: the
// four measured shapes encode two different things in that field, and a
// caller handed the wrong one reads a wild address rather than failing.
//
//	Absolute == false  Value is a byte OFFSET from &_PyRuntime (0x608 on
//	                   every 3.12 build measured). PIC builds, which load
//	                   &_PyRuntime through the GOT and add a displacement.
//	Absolute == true   Value is the LINK-TIME ADDRESS of the Py_tss_t
//	                   (0xb379c8 on Ubuntu's non-PIE python3.12). Non-PIE
//	                   builds, which can name the address outright.
//
// Resolve turns either into an address in the target.
type AutoTSSKeyRef struct {
	Value    uint64
	Absolute bool
}

// Resolve returns the address of the Py_tss_t in the TARGET's address
// space, given _PyRuntime as the target's own ELF declares it (link-time
// address and size) plus that mapping's load bias.
//
// The size is not decoration. It is the one independent check available on
// a number that came out of an instruction stream: autoTSSkey is a field of
// _PyRuntime, so the address must land inside that object with room for the
// whole 8-byte Py_tss_t. A misparse -- a rel32 read as an immediate, say --
// lands far outside and is refused here instead of being read as somebody
// else's memory. It is also what catches an absolute reference resolved
// against the wrong binary.
func (r AutoTSSKeyRef) Resolve(runtimeVaddr, runtimeSize, bias uint64) (uint64, error) {
	const tssSize = 8 // sizeof(Py_tss_t): int _is_initialized; pthread_key_t _key
	if runtimeSize == 0 {
		return 0, fmt.Errorf("%w: _PyRuntime has no size in the target's symbol table, so an autoTSSkey reference cannot be bounds-checked",
			ErrOffsetsImplausible)
	}
	var off uint64
	if r.Absolute {
		if r.Value < runtimeVaddr {
			return 0, fmt.Errorf("%w: autoTSSkey address %#x is below _PyRuntime at %#x",
				ErrOffsetsImplausible, r.Value, runtimeVaddr)
		}
		off = r.Value - runtimeVaddr
	} else {
		off = r.Value
	}
	if off+tssSize > runtimeSize {
		return 0, fmt.Errorf("%w: autoTSSkey at offset %#x is outside _PyRuntime (%#x bytes)",
			ErrOffsetsImplausible, off, runtimeSize)
	}
	return runtimeVaddr + bias + off, nil
}

// ParseAutoTSSKeyRef recovers _PyRuntime's autoTSSkey reference from the
// machine code of PyGILState_GetThisThreadState.
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
func ParseAutoTSSKeyRef(code []byte) (AutoTSSKeyRef, error) {
	for _, s := range tssShapes {
		if len(code) != s.size {
			continue
		}
		return s.decode(code)
	}
	return AutoTSSKeyRef{}, fmt.Errorf("%w: %d-byte body matches none of the known shapes (%s)",
		ErrTSSPatternUnrecognised, len(code), shapeSizes())
}

// decode checks one shape against a body already known to be its length.
func (s tssShape) decode(code []byte) (AutoTSSKeyRef, error) {
	for _, f := range s.fixed {
		got := code[f.at : f.at+len(f.bytes)]
		if !bytes.Equal(got, f.bytes) {
			return AutoTSSKeyRef{}, fmt.Errorf("%w: %s: byte %d: expected % x, got % x",
				ErrTSSPatternUnrecognised, s.name, f.at, f.bytes, got)
		}
	}
	first := binary.LittleEndian.Uint32(code[s.keyAt[0] : s.keyAt[0]+4])
	for _, at := range s.keyAt[1:] {
		other := binary.LittleEndian.Uint32(code[at : at+4])
		if other != first {
			return AutoTSSKeyRef{}, fmt.Errorf("%w: %s: value at byte %d (%#x) disagrees with value at byte %d (%#x)",
				ErrTSSPatternUnrecognised, s.name, s.keyAt[0], first, at, other)
		}
	}
	return AutoTSSKeyRef{Value: uint64(first), Absolute: s.absolute}, nil
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
