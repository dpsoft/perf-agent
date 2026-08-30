package pyunwind

import (
	"bytes"
	"os"
	"testing"
)

// The offsets the spike measured by disassembly. If a parser change makes
// these move, it is wrong: these are facts about real shipped binaries.
func TestParseAutoTSSKeyOffset(t *testing.T) {
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
		got, err := ParseAutoTSSKeyOffset(code)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got != tc.want {
			t.Fatalf("%s: offset = %#x, want %#x", tc.file, got, tc.want)
		}
	}
}

// A function that is not the shape we expect must be refused, not guessed
// at. Feeding it a wrong offset would produce a plausible stack of garbage.
func TestParseAutoTSSKeyOffsetRefusesUnknownShape(t *testing.T) {
	if _, err := ParseAutoTSSKeyOffset([]byte{0xf3, 0x0f, 0x1e, 0xfa, 0xc3}); err == nil {
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
func TestParseAutoTSSKeyOffsetRefusesBareLeaOnlyBody(t *testing.T) {
	body := []byte{
		0xf3, 0x0f, 0x1e, 0xfa, // endbr64
		0x48, 0x8d, 0xb8, 0x08, 0x06, 0x00, 0x00, // lea 0x608(%rax),%rdi -- no preceding cmpl
		0xc3, // ret
	}
	if _, err := ParseAutoTSSKeyOffset(body); err == nil {
		t.Fatal("expected refusal for a short body containing only a bare lea")
	}
}

func TestParseAutoTSSKeyOffsetRefusesBareLeaOnlyBodyZeroOffset(t *testing.T) {
	body := []byte{
		0xf3, 0x0f, 0x1e, 0xfa, // endbr64
		0x48, 0x8d, 0xb8, 0x00, 0x00, 0x00, 0x00, // lea 0x0(%rax),%rdi -- no preceding cmpl
		0xc3, // ret
	}
	if off, err := ParseAutoTSSKeyOffset(body); err == nil {
		t.Fatalf("expected refusal for a short body containing only a bare lea, got offset %#x", off)
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
func TestParseAutoTSSKeyOffsetRefusesReorderedInstructions(t *testing.T) {
	body := append([]byte{}, bytes.Repeat([]byte{0x90}, 8)...)
	body = append(body, 0x48, 0x8d, 0xb8, 0x08, 0x06, 0x00, 0x00) // lea 0x608(%rax),%rdi -- first, wrong position
	body = append(body, bytes.Repeat([]byte{0x90}, 10)...)
	body = append(body, 0x83, 0xb8, 0x08, 0x06, 0x00, 0x00, 0x00) // cmpl $0x0,0x608(%rax) -- second, wrong position
	body = append(body, bytes.Repeat([]byte{0x90}, 4)...)

	if off, err := ParseAutoTSSKeyOffset(body); err == nil {
		t.Fatalf("expected refusal for reordered/non-adjacent instructions, got offset %#x", off)
	}
}

// A body that is one byte short of, or one byte longer than, the exact
// 35-byte shape must be refused even when every byte it does carry is
// taken verbatim from a real, valid function body.
func TestParseAutoTSSKeyOffsetRefusesWrongLength(t *testing.T) {
	if len(validBody312) != 35 {
		t.Fatalf("test fixture itself is wrong: len(validBody312) = %d, want 35", len(validBody312))
	}

	t.Run("34 bytes", func(t *testing.T) {
		short := validBody312[:34]
		if off, err := ParseAutoTSSKeyOffset(short); err == nil {
			t.Fatalf("expected refusal for a 34-byte body, got offset %#x", off)
		}
	})

	t.Run("36 bytes", func(t *testing.T) {
		long := append([]byte{}, validBody312...)
		long = append(long, 0x90)
		if off, err := ParseAutoTSSKeyOffset(long); err == nil {
			t.Fatalf("expected refusal for a 36-byte body, got offset %#x", off)
		}
	})
}

// The two shapes the spike never saw, both measured from real shipped
// libpythons of the SAME CPython version (3.12) as the 35-byte fixture
// above. Their existence is the point: the 35-byte body is a property of a
// toolchain, not of CPython, and the 44-byte one is what CI's interpreter
// (actions/setup-python 3.12.14 for ubuntu-24.04) actually ships. Before it
// was handled, Attach refused every interpreter on the integration runners.
//
// Both must decode to 0x608 -- the same autoTSSkey offset the 35-byte
// Debian 3.12 body yields, because it is the same struct in the same
// version. That agreement across three independently compiled bodies is
// what says the parser read the offset rather than some other u32.
func TestParseAutoTSSKeyOffsetAcceptsOtherToolchainShapes(t *testing.T) {
	cases := []struct {
		file    string
		wantLen int
		want    uint32
	}{
		{"testdata/gilstate_312_gcc13_pgo.bin", 44, 0x608},
		{"testdata/gilstate_312_ubuntu_fp.bin", 64, 0x608},
	}
	for _, tc := range cases {
		code, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if len(code) != tc.wantLen {
			t.Fatalf("%s: fixture is %d bytes, expected the measured %d", tc.file, len(code), tc.wantLen)
		}
		got, err := ParseAutoTSSKeyOffset(code)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got != tc.want {
			t.Fatalf("%s: offset = %#x, want %#x", tc.file, got, tc.want)
		}
	}
}

// The 35-byte fixture and the two new ones are three separate compilations
// of CPython 3.12's PyGILState_GetThisThreadState. A parser that read the
// wrong u32 out of any one of them would disagree with the other two, so
// pinning the agreement is a stronger statement than pinning each literal
// on its own.
func TestAutoTSSKeyOffsetAgreesAcrossToolchainsForOneVersion(t *testing.T) {
	files := []string{
		"testdata/gilstate_312.bin",
		"testdata/gilstate_312_gcc13_pgo.bin",
		"testdata/gilstate_312_ubuntu_fp.bin",
	}
	var first uint32
	for i, f := range files {
		code, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		got, err := ParseAutoTSSKeyOffset(code)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("%s: offset %#x disagrees with %s's %#x; three builds of one CPython version must report one struct offset",
				f, got, files[0], first)
		}
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
func TestParseAutoTSSKeyOffsetRefusesEverySingleByteCorruption(t *testing.T) {
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
	}
	for _, tc := range cases {
		good, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if _, err := ParseAutoTSSKeyOffset(good); err != nil {
			t.Fatalf("%s: fixture must parse before corruption: %v", tc.file, err)
		}
		for i := range good {
			mutant := append([]byte{}, good...)
			mutant[i] ^= 0xff
			_, err := ParseAutoTSSKeyOffset(mutant)
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
