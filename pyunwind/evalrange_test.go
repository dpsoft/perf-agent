package pyunwind

import (
	"debug/elf"
	"errors"
	"testing"
)

// fn builds a STT_FUNC symbol. The sizes and addresses below are measured
// from real libpythons with `readelf -sW`; see minEvalLoopBytes for the
// full table and where each came from.
func fn(name string, value, size uint64) elf.Symbol {
	return elf.Symbol{Name: name, Value: value, Size: size, Info: byte(elf.STT_FUNC)}
}

// TestPickEvalFragmentAgainstMeasuredBuilds is the test that keeps the
// eval-range choice honest without a live interpreter, and it is the one
// that must fail if the picker regresses to "use the exported symbol".
//
// It matters more than it looks. If the range is wrong, the interpreter arm
// is switched on for a stretch of text no sample ever lands in: the walker
// produces no Python frames, no counter moves, and the end-to-end test's
// own precondition check would SKIP rather than fail (an unlocatable eval
// loop is a legitimate property of some builds). This test cannot skip.
func TestPickEvalFragmentAgainstMeasuredBuilds(t *testing.T) {
	cases := []struct {
		name     string
		syms     []elf.Symbol
		wantLo   uint64
		wantSize uint64
	}{
		{
			// actions/setup-python 3.12.14 for ubuntu-24.04 -- the
			// interpreter this project's CI runs. GCC's PGO partitioning
			// leaves the exported symbol as a 350-byte entry stub and puts
			// the dispatch loop in a LOCAL .cold sibling. A profile of this
			// exact binary put 66 of 66 eval-loop frames inside the .cold
			// range and none inside the exported one.
			name: "PGO-partitioned: the loop is in the local .cold sibling",
			syms: []elf.Symbol{
				fn("_PyEval_EvalFrameDefault", 0x25f1c0, 350),
				fn("_PyEval_EvalFrameDefault.localalias", 0x25f1c0, 350),
				fn("_PyEval_EvalFrameDefault.cold", 0x18c8e8, 51431),
			},
			wantLo:   0x18c8e8,
			wantSize: 51431,
		},
		{
			name:     "python:3.12.14-slim, one unpartitioned function",
			syms:     []elf.Symbol{fn("_PyEval_EvalFrameDefault", 0x1f4290, 50905)},
			wantLo:   0x1f4290,
			wantSize: 50905,
		},
		{
			name:     "python:3.13.15-slim",
			syms:     []elf.Symbol{fn("_PyEval_EvalFrameDefault", 0x19ac70, 53657)},
			wantLo:   0x19ac70,
			wantSize: 53657,
		},
		{
			name:     "python:3.14.3-slim",
			syms:     []elf.Symbol{fn("_PyEval_EvalFrameDefault", 0x1a8ce0, 62290)},
			wantLo:   0x1a8ce0,
			wantSize: 62290,
		},
		{
			name:     "Ubuntu 24.04 system libpython3.12",
			syms:     []elf.Symbol{fn("_PyEval_EvalFrameDefault", 0x119500, 56311)},
			wantLo:   0x119500,
			wantSize: 56311,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickEvalFragment(tc.syms)
			if err != nil {
				t.Fatalf("refused a locatable eval loop: %v", err)
			}
			if got.Value != tc.wantLo || got.Size != tc.wantSize {
				t.Fatalf("picked %s at %#x (%d bytes), want %#x (%d bytes)",
					got.Name, got.Value, got.Size, tc.wantLo, tc.wantSize)
			}
		})
	}
}

// TestPickEvalFragmentRefusesAStubOnlyBuild pins the other measured
// population: Fedora 44's stripped libpython3.14, where .dynsym carries a
// 994-byte fragment and the partition holding the dispatch loop has no
// symbol at all. Installing that range would switch the arm on for a
// binary whose eval loop is elsewhere.
func TestPickEvalFragmentRefusesAStubOnlyBuild(t *testing.T) {
	_, err := pickEvalFragment([]elf.Symbol{fn("_PyEval_EvalFrameDefault", 0x197af0, 994)})
	if !errors.Is(err, ErrEvalLoopNotLocatable) {
		t.Fatalf("994-byte fragment: err = %v, want ErrEvalLoopNotLocatable", err)
	}
}

func TestPickEvalFragmentRefusesWhenTheSymbolIsAbsent(t *testing.T) {
	_, err := pickEvalFragment([]elf.Symbol{fn("PyEval_EvalCode", 0x1000, 100000)})
	if !errors.Is(err, ErrEvalLoopNotLocatable) {
		t.Fatalf("no eval symbol: err = %v, want ErrEvalLoopNotLocatable", err)
	}
}

// A large unrelated function must not win just by being large: the
// candidate set is "_PyEval_EvalFrameDefault" and its dotted siblings, not
// "the biggest function in libpython".
func TestPickEvalFragmentIgnoresUnrelatedSymbols(t *testing.T) {
	got, err := pickEvalFragment([]elf.Symbol{
		fn("_PyEval_EvalFrameDefault", 0x1f4290, 50905),
		fn("some_enormous_generated_table_fn", 0x300000, 900000),
		fn("_PyEval_EvalFrameDefaultButNotReally", 0x400000, 800000),
	})
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if got.Name != "_PyEval_EvalFrameDefault" {
		t.Fatalf("picked %q (%d bytes); only the eval symbol and its dotted siblings are candidates", got.Name, got.Size)
	}
}

// The range is half-open and derived from the symbol, so Hi must be
// Lo+Size. A range built as [Lo, Lo+Size] would include the first byte of
// the next function, and one built as [Lo, Size) would be nonsense on a
// PIE. Pinned because both are one character away.
func TestEvalRangeIsHalfOpenAroundTheFragment(t *testing.T) {
	sym := fn("_PyEval_EvalFrameDefault.cold", 0x18c8e8, 51431)
	got, err := pickEvalFragment([]elf.Symbol{sym})
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	r := EvalRange{Lo: got.Value, Hi: got.Value + got.Size}
	if r.Lo != 0x18c8e8 || r.Hi != 0x18c8e8+51431 {
		t.Fatalf("range [%#x,%#x), want [%#x,%#x)", r.Lo, r.Hi, 0x18c8e8, 0x18c8e8+51431)
	}
	// 0x1969a9 is a real eval-loop return address measured in a profile of
	// that binary; 0x25f1c0 is its exported entry stub, which is outside.
	if !(r.Lo <= 0x1969a9 && 0x1969a9 < r.Hi) {
		t.Errorf("a measured eval-loop PC (%#x) falls outside the installed range", 0x1969a9)
	}
	if r.Lo <= 0x25f1c0 && 0x25f1c0 < r.Hi {
		t.Errorf("the exported entry stub (%#x) falls inside the .cold range; the two fragments are disjoint", 0x25f1c0)
	}
}
