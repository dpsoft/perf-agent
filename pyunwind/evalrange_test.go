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

// TestEvalFragmentsAgainstMeasuredBuilds is the test that keeps the
// eval-range choice honest without a live interpreter, and it is the one
// that must fail if the picker regresses to "use the exported symbol".
//
// It matters more than it looks. If the range is wrong, the interpreter arm
// is switched on for a stretch of text no sample ever lands in: the walker
// produces no Python frames, no counter moves, and the end-to-end test's
// own precondition check would SKIP rather than fail (an unlocatable eval
// loop is a legitimate property of some builds). This test cannot skip.
func TestEvalFragmentsAgainstMeasuredBuilds(t *testing.T) {
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
			got, err := evalFragments(tc.syms, "test")
			if err != nil {
				t.Fatalf("refused a locatable eval loop: %v", err)
			}
			if got[0].Lo != tc.wantLo || got[0].Hi-got[0].Lo != tc.wantSize {
				t.Fatalf("largest fragment is [%#x,%#x) (%d bytes), want %#x (%d bytes)",
					got[0].Lo, got[0].Hi, got[0].Hi-got[0].Lo, tc.wantLo, tc.wantSize)
			}
		})
	}
}

// TestEvalFragmentsRefuseAStubOnlyBuild pins the other measured
// population: Fedora 44's stripped libpython3.14, where .dynsym carries a
// 994-byte fragment and the partition holding the dispatch loop has no
// symbol at all. Installing that range would switch the arm on for a
// binary whose eval loop is elsewhere.
func TestEvalFragmentsRefuseAStubOnlyBuild(t *testing.T) {
	_, err := evalFragments([]elf.Symbol{fn("_PyEval_EvalFrameDefault", 0x197af0, 994)}, "test")
	if !errors.Is(err, ErrEvalLoopNotLocatable) {
		t.Fatalf("994-byte fragment: err = %v, want ErrEvalLoopNotLocatable", err)
	}
}

func TestEvalFragmentsRefuseWhenTheSymbolIsAbsent(t *testing.T) {
	_, err := evalFragments([]elf.Symbol{fn("PyEval_EvalCode", 0x1000, 100000)}, "test")
	if !errors.Is(err, ErrEvalLoopNotLocatable) {
		t.Fatalf("no eval symbol: err = %v, want ErrEvalLoopNotLocatable", err)
	}
}

// A large unrelated function must not win just by being large: the
// candidate set is "_PyEval_EvalFrameDefault" and its dotted siblings, not
// "the biggest function in libpython".
func TestEvalFragmentsIgnoreUnrelatedSymbols(t *testing.T) {
	got, err := evalFragments([]elf.Symbol{
		fn("_PyEval_EvalFrameDefault", 0x1f4290, 50905),
		fn("some_enormous_generated_table_fn", 0x300000, 900000),
		fn("_PyEval_EvalFrameDefaultButNotReally", 0x400000, 800000),
	}, "test")
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if len(got) != 1 || got[0].Lo != 0x1f4290 {
		t.Fatalf("claimed %v; only the eval symbol and its DOTTED siblings are candidates -- "+
			"a bare prefix match would swallow _PyEval_EvalFrameDefaultButNotReally and hang the "+
			"Python stack off a native frame that is not the eval loop", got)
	}
}

// The range is half-open and derived from the symbol, so Hi must be
// Lo+Size. A range built as [Lo, Lo+Size] would include the first byte of
// the next function, and one built as [Lo, Size) would be nonsense on a
// PIE. Pinned because both are one character away.
func TestEvalRangeIsHalfOpenAroundTheFragment(t *testing.T) {
	sym := fn("_PyEval_EvalFrameDefault.cold", 0x18c8e8, 51431)
	got, err := evalFragments([]elf.Symbol{sym}, "test")
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	r := got[0]
	if r.Lo != 0x18c8e8 || r.Hi != 0x18c8e8+51431 {
		t.Fatalf("range [%#x,%#x), want [%#x,%#x)", r.Lo, r.Hi, 0x18c8e8, 0x18c8e8+51431)
	}
	// 0x1969a9 is a real eval-loop return address measured in a profile of
	// that binary; 0x25f1c0 is its exported entry stub, which is outside.
	if r.Lo > 0x1969a9 || 0x1969a9 >= r.Hi {
		t.Errorf("a measured eval-loop PC (%#x) falls outside the installed range", 0x1969a9)
	}
	if r.Lo <= 0x25f1c0 && 0x25f1c0 < r.Hi {
		t.Errorf("the exported entry stub (%#x) falls inside the .cold range; the two fragments are disjoint", 0x25f1c0)
	}
}

// ----- The partitioned-build defect, and why one range was never enough.
//
// uv's cpython-3.12.14 -- the interpreter a PyTorch venv actually runs -- has
// the eval loop split across four fragments, and the LARGEST is the one the
// compiler marked cold. Claiming only that one meant that on a workload with
// 86% of samples sitting in the eval loop, not one fell inside the claim: the
// handoff never fired, and because nothing had gone wrong, every counter read
// zero.
//
// Measured from the real binary with llvm-readelf.
var uvCPython312Fragments = []elf.Symbol{
	fn("_PyEval_EvalFrameDefault", 0x1812300, 66065),
	fn("_PyEval_EvalFrameDefault.warm", 0x1a74300, 28019),
	fn("_PyEval_EvalFrameDefault.cold", 0x1af2910, 135934),
	fn("_PyEval_EvalFrameDefault.org.0", 0x5bc470, 5),
}

func TestEveryEvalFragmentIsClaimed(t *testing.T) {
	got, err := evalFragments(uvCPython312Fragments, "uv/python3.12")
	if err != nil {
		t.Fatalf("refused a partitioned build: %v", err)
	}
	if len(got) != len(uvCPython312Fragments) {
		t.Fatalf("claimed %d of %d fragments; a sample in an unclaimed one carries no Python frames",
			len(got), len(uvCPython312Fragments))
	}

	// The hot dispatch loop -- the one blazesym names in the profile, and the
	// one the old "largest fragment" rule dropped -- must be covered.
	const hotPC = 0x1812300 + 0x1000
	if !covered(got, hotPC) {
		t.Errorf("a PC in the hot fragment (%#x) is not claimed; this is the exact miss that "+
			"produced zero Python frames on a stack that was sitting in the eval loop", hotPC)
	}
	if !covered(got, 0x1a74300+0x100) {
		t.Error("a PC in the .warm fragment is not claimed")
	}
	if !covered(got, 0x1af2910+0x100) {
		t.Error("a PC in the .cold fragment is not claimed")
	}
	// And nothing between the fragments, which is 3 MB of unrelated CPython.
	// Spanning min..max would be worse than the bug it fixes: it would hang a
	// Python stack off a native frame that is not the eval loop at all.
	if covered(got, 0x1900000) {
		t.Error("a PC BETWEEN fragments is claimed; the union has been replaced by a covering span")
	}
}

// The largest-first order is what makes the drop safe when a build has more
// fragments than the walker can scan: the ones dropped are the ones fewest
// samples land in.
func TestEvalFragmentsAreLargestFirst(t *testing.T) {
	got, err := evalFragments(uvCPython312Fragments, "uv/python3.12")
	if err != nil {
		t.Fatalf("evalFragments: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Hi-got[i].Lo > got[i-1].Hi-got[i-1].Lo {
			t.Fatalf("fragment %d (%d bytes) is larger than fragment %d (%d bytes); "+
				"a caller that can carry fewer spans would drop the wrong ones",
				i, got[i].Hi-got[i].Lo, i-1, got[i-1].Hi-got[i-1].Lo)
		}
	}
}

// The size floor is on the TOTAL, not per fragment: a 5-byte trampoline is a
// real part of the function, while a lone 994-byte stub is not a dispatch loop
// however many fragments it is split into.
func TestTheSizeFloorAppliesToTheWholeFunction(t *testing.T) {
	if _, err := evalFragments([]elf.Symbol{
		fn("_PyEval_EvalFrameDefault", 0x1000, 900),
		fn("_PyEval_EvalFrameDefault.cold", 0x9000, 94),
	}, "stub"); !errors.Is(err, ErrEvalLoopNotLocatable) {
		t.Fatalf("994 bytes across two fragments: err = %v, want ErrEvalLoopNotLocatable", err)
	}
	// Just over the floor, split, must be accepted.
	if _, err := evalFragments([]elf.Symbol{
		fn("_PyEval_EvalFrameDefault", 0x1000, 4096),
		fn("_PyEval_EvalFrameDefault.cold", 0x9000, 4097),
	}, "split"); err != nil {
		t.Fatalf("a loop split evenly across two fragments was refused: %v", err)
	}
}

func covered(rs []EvalRange, pc uint64) bool {
	for _, r := range rs {
		if pc >= r.Lo && pc < r.Hi {
			return true
		}
	}
	return false
}

// An exported fragment appears in BOTH .symtab and .dynsym, and allFuncSymbols
// reads both. Counted twice, the hot dispatch loop's duplicate evicts a real
// fragment from the claim -- because the walker can only scan interp.MaxSpans
// of them -- and that is the original bug wearing the fix's clothes.
//
// Measured on uv's cpython-3.12.14 before the dedupe: the largest three came
// back {.cold, main, main} and .warm was dropped.
func TestDuplicateSymbolTableEntriesDoNotEvictAFragment(t *testing.T) {
	syms := []elf.Symbol{
		fn("_PyEval_EvalFrameDefault", 0x1812300, 66065), // .symtab
		fn("_PyEval_EvalFrameDefault", 0x1812300, 66065), // .dynsym, same fragment
		fn("_PyEval_EvalFrameDefault.warm", 0x1a74300, 28019),
		fn("_PyEval_EvalFrameDefault.cold", 0x1af2910, 135934),
	}
	got, err := evalFragments(syms, "uv/python3.12")
	if err != nil {
		t.Fatalf("evalFragments: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d fragments from 4 symbols covering 3 distinct ranges: %v", len(got), got)
	}
	// The claim the walker actually installs is the first MaxSpans of these.
	// All three real fragments must survive that cut.
	for _, pc := range []uint64{0x1812300 + 0x10, 0x1a74300 + 0x10, 0x1af2910 + 0x10} {
		if !covered(got[:3], pc) {
			t.Errorf("PC %#x is not claimed; a duplicate symbol has evicted a real fragment", pc)
		}
	}
}
