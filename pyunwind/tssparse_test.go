package pyunwind

import (
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

// A body carrying a bare `lea off32(%rax),%rdi` with no matching `cmpl`
// must still be refused. Without the haveCmp check this instruction alone
// is enough to yield an (unverified) offset -- exactly the "plausible
// garbage" failure mode this parser exists to avoid. The 5-byte junk body
// in TestParseAutoTSSKeyOffsetRefusesUnknownShape does not exercise this:
// it is too short to contain any 4-byte `48 8d b8 <off32>` sequence at
// all, so a mutation that drops the haveCmp requirement passes that test
// unnoticed.
//
// Note this case (offset 0x608, matching a real build) still refuses even
// with the haveCmp requirement deleted, because the zero-valued cmpOff
// then disagrees with the nonzero leaOff and the separate cmpOff!=leaOff
// check catches it -- coincidentally, not by design. It is the *next*
// test, with a lea offset of zero, that actually isolates the haveCmp
// mutation: see its comment.
func TestParseAutoTSSKeyOffsetRefusesLeaWithoutCmp(t *testing.T) {
	body := []byte{
		0xf3, 0x0f, 0x1e, 0xfa, // endbr64
		0x48, 0x8d, 0xb8, 0x08, 0x06, 0x00, 0x00, // lea 0x608(%rax),%rdi -- no preceding cmpl
		0xc3, // ret
	}
	if _, err := ParseAutoTSSKeyOffset(body); err == nil {
		t.Fatal("expected refusal for a lea with no matching cmpl")
	}
}

// A bare lea with offset zero and no cmpl is the case that actually pins
// the haveCmp requirement. If haveCmp is dropped from the final gate (as
// in the mutation above), cmpOff keeps its zero value, which then equals
// a zero leaOff -- the cmpOff!=leaOff cross-check has nothing to catch,
// and the parser would silently return offset 0 with a nil error. That is
// precisely the failure mode this package exists to prevent: a wrong
// offset returned as if it were trustworthy. This case is what makes
// mutating away the haveCmp check observable; the 0x608 case above does
// not, because it happens to be caught by the disagreement check instead.
func TestParseAutoTSSKeyOffsetRefusesLeaWithoutCmpZeroOffset(t *testing.T) {
	body := []byte{
		0xf3, 0x0f, 0x1e, 0xfa, // endbr64
		0x48, 0x8d, 0xb8, 0x00, 0x00, 0x00, 0x00, // lea 0x0(%rax),%rdi -- no preceding cmpl
		0xc3, // ret
	}
	if off, err := ParseAutoTSSKeyOffset(body); err == nil {
		t.Fatalf("expected refusal for a lea with no matching cmpl, got offset %#x", off)
	}
}
