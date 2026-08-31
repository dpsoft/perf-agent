package pyunwind

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// The offsets the spike measured by disassembly. If a parser change makes
// these move, it is wrong: these are facts about real shipped binaries.
func TestParseAutoTSSKeyRef(t *testing.T) {
	cases := []struct {
		file string
		want uint32
	}{
		{"testdata/gilstate_312.bin", 0x608},
		{"testdata/gilstate_313.bin", 0x870},
		{"testdata/gilstate_314.bin", 0x920},
	}
	for _, tc := range cases {
		code, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		got, err := ParseAutoTSSKeyRef(code)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got.Kind != KeyRefRuntimeOffset {
			t.Fatalf("%s: decoded as kind %d; these shapes encode an offset from _PyRuntime", tc.file, got.Kind)
		}
		if got.Value != uint64(tc.want) {
			t.Fatalf("%s: offset = %#x, want %#x", tc.file, got.Value, tc.want)
		}
	}
}

// A function that is not the shape we expect must be refused, not guessed
// at. Feeding it a wrong offset would produce a plausible stack of garbage.
func TestParseAutoTSSKeyRefRefusesUnknownShape(t *testing.T) {
	if _, err := ParseAutoTSSKeyRef([]byte{0xf3, 0x0f, 0x1e, 0xfa, 0xc3}); err == nil {
		t.Fatal("expected refusal for an unrecognised function body")
	}
}

// validBody312 is the real 3.12 fixture bytes inline, so the length tests
// below can slice/pad a genuinely valid body rather than invent one.
var validBody312 = []byte{
	0xf3, 0x0f, 0x1e, 0xfa, // endbr64
	0x48, 0x8b, 0x05, 0xfc, 0xe1, 0x34, 0x00, // mov disp32(%rip),%rax
	0x83, 0xb8, 0x08, 0x06, 0x00, 0x00, 0x00, // cmpl $0x0,0x608(%rax)
	0x74, 0x0c, // je +0x0c
	0x48, 0x8d, 0xb8, 0x08, 0x06, 0x00, 0x00, // lea 0x608(%rax),%rdi
	0xe9, 0xc7, 0x27, 0x0d, 0x00, // jmp PyThread_tss_get
	0x31, 0xc0, // xor %eax,%eax
	0xc3, // ret
}

// Below are two bodies that, under the earlier scan-and-cross-check
// parser, existed specifically to isolate the "haveCmp" guard from the
// "cmpOff != leaOff" guard as two independent checks. The parser is now
// positional: it decodes the full 35-byte, 8-instruction shape at fixed
// offsets, so there is no longer a separate haveCmp/haveLea state to
// isolate -- a body this short (12 bytes) is refused outright on length
// before any instruction byte is even inspected. They are kept as
// regression fixtures (a body containing only a bare lea, with or without
// a same/zero valued would-be cmpl offset, must still be refused) but no
// longer prove anything about a specific internal guard.
func TestParseAutoTSSKeyRefRefusesBareLeaOnlyBody(t *testing.T) {
	body := []byte{
		0xf3, 0x0f, 0x1e, 0xfa, // endbr64
		0x48, 0x8d, 0xb8, 0x08, 0x06, 0x00, 0x00, // lea 0x608(%rax),%rdi -- no preceding cmpl
		0xc3, // ret
	}
	if _, err := ParseAutoTSSKeyRef(body); err == nil {
		t.Fatal("expected refusal for a short body containing only a bare lea")
	}
}

func TestParseAutoTSSKeyRefRefusesBareLeaOnlyBodyZeroOffset(t *testing.T) {
	body := []byte{
		0xf3, 0x0f, 0x1e, 0xfa, // endbr64
		0x48, 0x8d, 0xb8, 0x00, 0x00, 0x00, 0x00, // lea 0x0(%rax),%rdi -- no preceding cmpl
		0xc3, // ret
	}
	if ref, err := ParseAutoTSSKeyRef(body); err == nil {
		t.Fatalf("expected refusal for a short body containing only a bare lea, got %#x", ref.Value)
	}
}

// TestParseAutoTSSKeyOffsetRefusesReorderedInstructions is the case that
// regressed under the earlier scan-and-cross-check parser: it independently
// scanned the whole input for a cmpl anywhere and a lea anywhere, and
// accepted them as long as their off32 values numerically agreed --
// regardless of order, adjacency, or whether any of the other six
// instructions (endbr64, mov, je, jmp, xor, ret) were present at all. Fed
// this body -- a lea and a cmpl with matching offsets, in reversed order,
// separated by filler, with no endbr64/mov/je/jmp anywhere -- the old
// parser returned offset 0x608 with a nil error: a wrong-shape function
// silently treated as trustworthy. The positional parser must refuse it.
func TestParseAutoTSSKeyRefRefusesReorderedInstructions(t *testing.T) {
	body := append([]byte{}, bytes.Repeat([]byte{0x90}, 8)...)
	body = append(body, 0x48, 0x8d, 0xb8, 0x08, 0x06, 0x00, 0x00) // lea 0x608(%rax),%rdi -- first, wrong position
	body = append(body, bytes.Repeat([]byte{0x90}, 10)...)
	body = append(body, 0x83, 0xb8, 0x08, 0x06, 0x00, 0x00, 0x00) // cmpl $0x0,0x608(%rax) -- second, wrong position
	body = append(body, bytes.Repeat([]byte{0x90}, 4)...)

	if ref, err := ParseAutoTSSKeyRef(body); err == nil {
		t.Fatalf("expected refusal for reordered/non-adjacent instructions, got %#x", ref.Value)
	}
}

// A body that is one byte short of, or one byte longer than, the exact
// 35-byte shape must be refused even when every byte it does carry is
// taken verbatim from a real, valid function body.
func TestParseAutoTSSKeyRefRefusesWrongLength(t *testing.T) {
	if len(validBody312) != 35 {
		t.Fatalf("test fixture itself is wrong: len(validBody312) = %d, want 35", len(validBody312))
	}

	t.Run("34 bytes", func(t *testing.T) {
		short := validBody312[:34]
		if ref, err := ParseAutoTSSKeyRef(short); err == nil {
			t.Fatalf("expected refusal for a 34-byte body, got %#x", ref.Value)
		}
	})

	t.Run("36 bytes", func(t *testing.T) {
		long := append([]byte{}, validBody312...)
		long = append(long, 0x90)
		if ref, err := ParseAutoTSSKeyRef(long); err == nil {
			t.Fatalf("expected refusal for a 36-byte body, got %#x", ref.Value)
		}
	})
}

// The three shapes the spike never saw, all measured from real shipped
// libpythons and interpreters of the SAME CPython version (3.12) as the
// 35-byte fixture above. Their existence is the point: the 35-byte body is
// a property of a toolchain, not of CPython.
//
// Two of them were found by CI. The 44-byte one is what
// actions/setup-python 3.12.14 ships, and until it was handled Attach
// refused every interpreter on the integration runners. The 37-byte one is
// Ubuntu noble's own /usr/bin/python3.12 -- a non-PIE build that names the
// address of autoTSSkey outright instead of adding an offset to a
// GOT-loaded base -- and it was found by the live tests in this package
// failing on a runner.
func TestParseAutoTSSKeyRefAcceptsOtherToolchainShapes(t *testing.T) {
	cases := []struct {
		file    string
		wantLen int
		kind    AutoTSSKeyRefKind
		want    uint64
	}{
		{"testdata/gilstate_312_gcc13_pgo.bin", 44, KeyRefRuntimeOffset, 0x608},
		{"testdata/gilstate_312_ubuntu_fp.bin", 64, KeyRefRuntimeOffset, 0x608},
		{"testdata/gilstate_312_ubuntu_nonpie.bin", 37, KeyRefAbsoluteAddress, 0xb379c8},
		// Clang: the value is an offset from the body's own start, so it
		// means nothing until Resolve is given where the body was linked.
		// 0x10c3c08 = the cmpl's displacement (0x10c3bfd) plus the 11 bytes
		// from the body's start to the end of that instruction.
		{"testdata/gilstate_312_clang_pbs.bin", 29, KeyRefBodyRelative, 0x10c3c08},
	}
	for _, tc := range cases {
		code, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if len(code) != tc.wantLen {
			t.Fatalf("%s: fixture is %d bytes, expected the measured %d", tc.file, len(code), tc.wantLen)
		}
		got, err := ParseAutoTSSKeyRef(code)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got.Kind != tc.kind {
			t.Fatalf("%s: Kind = %d, want %d -- an address read as an offset (or the reverse) is a wild read, not a wrong number",
				tc.file, got.Kind, tc.kind)
		}
		if got.Value != tc.want {
			t.Fatalf("%s: value = %#x, want %#x", tc.file, got.Value, tc.want)
		}
	}
}

// FIVE separate compilations of CPython 3.12's
// PyGILState_GetThisThreadState, from four different builds, must all say
// autoTSSkey is at offset 0x608 of _PyRuntime -- because it is the same
// struct in the same version. Three encode that offset directly; the
// non-PIE one encodes an absolute address, and the Clang one a displacement
// from its own instruction stream, both of which only become 0x608 once
// resolved against that binary's own symbols.
//
// This is a stronger statement than four separately pinned literals: a
// parser reading the wrong u32 out of any one body, or resolving a
// reference against the wrong base, disagrees with the other four. The _PyRuntime coordinates below are read with `readelf -sW` from
// the same binaries the fixtures came from.
func TestAutoTSSKeyOffsetAgreesAcrossToolchainsForOneVersion(t *testing.T) {
	const wantOffset = 0x608
	cases := []struct {
		file string
		// _PyRuntime and PyGILState_GetThisThreadState as that binary's own
		// ELF declares them. The non-PIE fixture needs the first to reach
		// an offset at all and the Clang one needs the second; supplying
		// both everywhere means Resolve's bounds check runs on every case.
		runtimeVaddr uint64
		runtimeSize  uint64
		bodyVaddr    uint64
	}{
		{"testdata/gilstate_312.bin", 0x5e42a0, 0x70210, 0x197b39},               // python:3.12.14-slim libpython3.12.so.1.0
		{"testdata/gilstate_312_gcc13_pgo.bin", 0x61c600, 0x70210, 0x1b831e},     // actions/setup-python 3.12.14 libpython3.12.so.1.0
		{"testdata/gilstate_312_ubuntu_fp.bin", 0x8342e0, 0x704a8, 0x2f9580},     // Ubuntu 24.04 libpython3.12.so.1.0
		{"testdata/gilstate_312_ubuntu_nonpie.bin", 0xb373c0, 0x704a8, 0x608550}, // Ubuntu 24.04 /usr/bin/python3.12
		{"testdata/gilstate_312_clang_pbs.bin", 0x1547cf0, 0x70210, 0x4846f0},    // python-build-standalone 3.12.14 (Clang)
	}
	for _, tc := range cases {
		code, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		ref, err := ParseAutoTSSKeyRef(code)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		// bias 0: these are link-time coordinates, and Resolve adds the
		// bias its caller measured from the live mapping.
		addr, err := ref.Resolve(SymbolPlacement{
			RuntimeVaddr: tc.runtimeVaddr, RuntimeSize: tc.runtimeSize, BodyVaddr: tc.bodyVaddr,
		})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", tc.file, err)
		}
		if got := addr - tc.runtimeVaddr; got != wantOffset {
			t.Fatalf("%s: autoTSSkey at offset %#x of _PyRuntime, want %#x; four builds of one CPython version must agree",
				tc.file, got, wantOffset)
		}
	}
}

// Resolve is the only bounds check there is on a number that came out of an
// instruction stream, so each way it can refuse gets a case. The happy
// paths are covered by the agreement test above.
func TestAutoTSSKeyRefResolveRefusesImplausibleReferences(t *testing.T) {
	const rtVaddr, rtSize = 0xb373c0, 0x704a8
	cases := []struct {
		name string
		ref  AutoTSSKeyRef
		size uint64
	}{
		{
			// A rel32 misread as an immediate: the classic way a wrong
			// positional decode produces a number at all.
			name: "absolute address below _PyRuntime",
			ref:  AutoTSSKeyRef{Value: 0x4b2592, Kind: KeyRefAbsoluteAddress},
			size: rtSize,
		},
		{
			name: "absolute address past the end of _PyRuntime",
			ref:  AutoTSSKeyRef{Value: rtVaddr + rtSize, Kind: KeyRefAbsoluteAddress},
			size: rtSize,
		},
		{
			name: "offset past the end of _PyRuntime",
			ref:  AutoTSSKeyRef{Value: rtSize - 4},
			size: rtSize,
		},
		{
			// The last 8 bytes are the last Py_tss_t that could fit; one
			// byte later there is no room for the whole struct.
			name: "offset that leaves less than a Py_tss_t",
			ref:  AutoTSSKeyRef{Value: rtSize - 7},
			size: rtSize,
		},
		{
			name: "_PyRuntime with no size to check against",
			ref:  AutoTSSKeyRef{Value: 0x608},
			size: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := tc.ref.Resolve(SymbolPlacement{RuntimeVaddr: rtVaddr, RuntimeSize: tc.size})
			if err == nil {
				t.Fatalf("resolved to %#x; expected a refusal", addr)
			}
			if !errors.Is(err, ErrOffsetsImplausible) {
				t.Fatalf("err = %v, want it to wrap ErrOffsetsImplausible so callers do not retry it per thread", err)
			}
		})
	}
	// The boundary case that must be ACCEPTED, so the check above is not
	// simply refusing everything: a Py_tss_t in the last 8 bytes.
	if _, err := (AutoTSSKeyRef{Value: rtSize - 8}).Resolve(SymbolPlacement{RuntimeVaddr: rtVaddr, RuntimeSize: rtSize}); err != nil {
		t.Fatalf("a Py_tss_t in the final 8 bytes of _PyRuntime must resolve: %v", err)
	}
}

// Every byte of a shape that is not a per-binary displacement must be
// load-bearing. This flips each byte of each real body in turn and requires
// the parser to refuse -- EXCEPT at the positions the disassembly says
// carry a displacement or the offset itself. Those positions are listed
// here from the objdump output in tssparse.go's shape comments, not read
// back out of the shape table, so a table whose fixed runs quietly stopped
// covering an opcode fails here instead of agreeing with itself.
//
//	35-byte: the mov's disp32 at [7:11] and the jmp's rel32 at [28:32].
//	         Its TWO off32 fields are NOT in the list: corrupting either
//	         makes them disagree, which the parser refuses.
//	44-byte: the mov's disp32 at [7:11], the lone off32 at [15:19] (no
//	         second copy to disagree with, so a corruption there is
//	         accepted and simply reports a different offset), and the
//	         call/jmp rel32s at [23:27] and [36:40].
//	64-byte: the mov's disp32 at [16:20], the lone off32 at [23:27], and
//	         the call/jmp rel32s at [31:35] and [48:52].
//	37-byte: the call/je/jmp rel32s at [14:18], [22:26] and [33:37]. Its
//	         TWO imm32 copies are NOT in the list, for the same reason as
//	         the 35-byte shape's: corrupting either makes them disagree.
//	29-byte: only the jmp's rel32 at [21:25]. Both of its disp32 fields are
//	         cross-checked against each other (they must resolve exactly 4
//	         bytes apart), so corrupting either is refused.
func TestParseAutoTSSKeyRefRefusesEverySingleByteCorruption(t *testing.T) {
	span := func(lo, hi int) []int {
		var out []int
		for i := lo; i < hi; i++ {
			out = append(out, i)
		}
		return out
	}
	join := func(runs ...[]int) map[int]bool {
		m := map[int]bool{}
		for _, r := range runs {
			for _, i := range r {
				m[i] = true
			}
		}
		return m
	}
	cases := []struct {
		file      string
		unchecked map[int]bool
	}{
		{"testdata/gilstate_312.bin", join(span(7, 11), span(28, 32))},
		{"testdata/gilstate_312_gcc13_pgo.bin", join(span(7, 11), span(15, 19), span(23, 27), span(36, 40))},
		{"testdata/gilstate_312_ubuntu_fp.bin", join(span(16, 20), span(23, 27), span(31, 35), span(48, 52))},
		{"testdata/gilstate_312_ubuntu_nonpie.bin", join(span(14, 18), span(22, 26), span(33, 37))},
		{"testdata/gilstate_312_clang_pbs.bin", join(span(21, 25))},
	}
	for _, tc := range cases {
		good, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if _, err := ParseAutoTSSKeyRef(good); err != nil {
			t.Fatalf("%s: fixture must parse before corruption: %v", tc.file, err)
		}
		for i := range good {
			mutant := append([]byte{}, good...)
			mutant[i] ^= 0xff
			_, err := ParseAutoTSSKeyRef(mutant)
			if tc.unchecked[i] {
				if err != nil {
					t.Errorf("%s: byte %d is a displacement the shape does not check, but flipping it was refused: %v", tc.file, i, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s: flipping byte %d was accepted; that byte carries no weight in the shape", tc.file, i)
			}
		}
	}
}
