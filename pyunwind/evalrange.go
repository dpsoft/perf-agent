package pyunwind

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ErrEvalLoopNotLocatable means the interpreter's bytecode dispatch loop
// could not be located in the binary's symbols. Without it there is no
// eval-loop text range to install, and py_eval_ranges is the interpreter
// arm's on-switch: no range, no Python frames, for any process running that
// libpython.
//
// It is a refusal rather than a best guess for the usual reason: the range
// decides WHERE in the native stack the Python frames are spliced. A range
// that covers the wrong text (all of libpython, say) fires on the first
// libpython frame the walk reaches -- typically a leaf like PyLong_FromLong
// -- and hangs the whole Python stack above it. That is a plausible call
// path that never happened, which is worse than no Python frames at all.
var ErrEvalLoopNotLocatable = errors.New("pyunwind: cannot locate the interpreter's eval loop")

// evalLoopSymbol is the function the bytecode dispatch loop lives in on
// every supported version.
const evalLoopSymbol = "_PyEval_EvalFrameDefault"

// minEvalLoopBytes is the size below which a candidate cannot be the
// dispatch loop.
//
// This is not a taste threshold; it separates two MEASURED populations. The
// dispatch loop compiles the ~200 handlers of Python/bytecodes.c into one
// function, and it measures:
//
//	50905 bytes  python:3.12.14-slim
//	53657 bytes  python:3.13.15-slim
//	62290 bytes  python:3.14.3-slim
//	56311 bytes  Ubuntu 24.04's libpython3.12
//	51431 bytes  actions/setup-python 3.12.14 -- as
//	             _PyEval_EvalFrameDefault.cold, a LOCAL symbol; the
//	             exported one on that build is a 350-byte entry stub
//	             (GCC partitions the function under PGO)
//
// against the one case where the loop is NOT locatable at all:
//
//	994 bytes  Fedora 44's libpython3.14 -- stripped, so only .dynsym is
//	           readable, and the partition holding the loop has no
//	           symbol there at all
//
// 8 KiB sits an order of magnitude below every real loop and an order of
// magnitude above the stub, so neither population can drift across it
// without something having changed fundamentally.
const minEvalLoopBytes = 8 * 1024

// EvalRangesForFile returns EVERY text fragment of the interpreter's eval loop
// in a libpython (or a statically linked python executable), largest first, in
// the load-bias-relative space bpf/unwind_common.h's mapping_for_pc reports
// rel_pc in -- which is the ELF's own virtual-address space, since rel_pc is
// pc - load_bias.
//
// ALL OF THEM, AND THAT IS THE WHOLE POINT OF THIS FUNCTION. It used to return
// one range, the largest fragment, and that cost a day of debugging on the
// first real target. GCC and Clang partition _PyEval_EvalFrameDefault under
// PGO, and which partition is largest is not which partition runs. Measured on
// uv's cpython-3.12.14, the interpreter a PyTorch venv actually uses:
//
//	_PyEval_EvalFrameDefault         66,065 bytes   <- the hot dispatch loop
//	_PyEval_EvalFrameDefault.warm    28,019
//	_PyEval_EvalFrameDefault.cold   135,934         <- the LARGEST, and cold
//	_PyEval_EvalFrameDefault.org.0        5
//
// "Largest" therefore selected the partition the compiler had marked rarely
// executed. On a workload with 86% of samples sitting in the eval loop, not
// one of them fell inside the installed range: the handoff never fired, and
// because nothing had gone wrong, every counter read zero.
//
// The heuristic was not arbitrary -- it was measured, on actions/setup-python
// 3.12.14, where the exported symbol is a 350-byte stub and the loop really is
// in .cold. Both builds exist. Only the union satisfies both.
//
// NOT the min-to-max span, which would be worse than the bug: those fragments
// are 3 MB apart, so a single covering range swallows unrelated CPython
// functions and would hang a Python stack off a native frame that is not the
// eval loop. That is the plausible-stack-that-never-happened failure this
// package refuses everywhere else.
//
// Sorted largest first so a caller that can carry fewer spans than there are
// fragments drops the ones fewest samples land in -- and interp.MaxSpans is a
// measured verifier ceiling, so that caller exists.
//
// The SIZE FLOOR applies to the total, not to each fragment: a 5-byte
// trampoline is a real part of the function and costs nothing to include, but
// a binary whose only match is a 994-byte stub carries no dispatch loop and
// must still be refused (ErrEvalLoopNotLocatable).
func EvalRangesForFile(path string) ([]EvalRange, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	return evalFragments(allFuncSymbols(f), path)
}

// evalFragments is EvalRangesForFile's decision, separated from the file I/O
// so every property it has can be pinned against a symbol table written by
// hand -- including the builds this machine does not have.
func evalFragments(syms []elf.Symbol, path string) ([]EvalRange, error) {
	// DEDUPED BY (address, size). allFuncSymbols reads .symtab AND .dynsym,
	// and an exported fragment appears in both -- so without this the hot
	// dispatch loop is counted twice and, since the walker can only scan
	// interp.MaxSpans of them, its duplicate evicts a REAL fragment. Measured
	// on uv's cpython-3.12.14: the top three came back as
	// {.cold, main, main} and .warm was dropped, which is the original bug
	// wearing the fix's clothes.
	seen := map[[2]uint64]bool{}
	var frags []elf.Symbol
	for _, s := range syms {
		if s.Size == 0 || !isEvalFragment(s.Name) {
			continue
		}
		k := [2]uint64{s.Value, s.Size}
		if seen[k] {
			continue
		}
		seen[k] = true
		frags = append(frags, s)
	}
	if len(frags) == 0 {
		return nil, fmt.Errorf("%w: no %s symbol in %s", ErrEvalLoopNotLocatable, evalLoopSymbol, path)
	}
	sort.Slice(frags, func(i, j int) bool {
		if frags[i].Size != frags[j].Size {
			return frags[i].Size > frags[j].Size
		}
		return frags[i].Value < frags[j].Value
	})

	var total uint64
	out := make([]EvalRange, 0, len(frags))
	for _, s := range frags {
		total += s.Size
		out = append(out, EvalRange{Lo: s.Value, Hi: s.Value + s.Size})
	}
	if total < minEvalLoopBytes {
		return nil, fmt.Errorf(
			"%w: %s in %s totals %d bytes across %d fragments, below the %d-byte floor for a dispatch loop",
			ErrEvalLoopNotLocatable, evalLoopSymbol, path, total, len(frags), minEvalLoopBytes)
	}
	return out, nil
}

// isEvalFragment reports whether a symbol name is the eval loop or one of the
// partitions a compiler split it into.
//
// Matched as "the name, or the name followed by a '.' suffix" rather than by a
// bare prefix: a bare prefix would also swallow a hypothetical
// _PyEval_EvalFrameDefaultSomethingElse, and claiming a function that is not
// the eval loop puts the Python stack under the wrong native frame.
func isEvalFragment(name string) bool {
	return name == evalLoopSymbol || strings.HasPrefix(name, evalLoopSymbol+".")
}

// allFuncSymbols returns every STT_FUNC symbol in both symbol tables.
// .symtab is where a PGO-partitioned build's local `.cold` fragment lives,
// so reading only .dynsym would miss exactly the case EvalRangeForFile
// exists to handle. A binary with no .symtab (the common stripped case)
// yields elf.ErrNoSymbols, which is not an error here -- .dynsym alone is
// what such a binary has.
func allFuncSymbols(f *elf.File) []elf.Symbol {
	var out []elf.Symbol
	for _, get := range []func() ([]elf.Symbol, error){f.Symbols, f.DynamicSymbols} {
		syms, err := get()
		if err != nil {
			continue
		}
		for _, s := range syms {
			if elf.ST_TYPE(s.Info) == elf.STT_FUNC {
				out = append(out, s)
			}
		}
	}
	return out
}

// gilStateSymbol is the function whose prologue carries the autoTSSkey
// reference. Named once: the bytes are read from it here and its link-time
// address is looked up again in prepareInfo, for the RIP-relative shape,
// and the two must be the same symbol.
const gilStateSymbol = "PyGILState_GetThisThreadState"

// GILStateCode reads the machine-code body of PyGILState_GetThisThreadState
// out of a libpython, for ParseAutoTSSKeyRef.
//
// The bytes come from the FILE, not from the running process: the function
// is in a read-only text mapping, so the two are identical, and reading the
// file needs no access to the target at all. Its length is the symbol's
// size -- the parser requires an exact length and refuses anything else, so
// a truncated or padded read cannot be mistaken for a shape it knows.
func GILStateCode(path string) ([]byte, error) {
	osf, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: open %s: %w", path, err)
	}
	defer func() { _ = osf.Close() }()
	f, err := elf.NewFile(osf)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: parse %s: %w", path, err)
	}

	var sym elf.Symbol
	var found bool
	for _, s := range allFuncSymbols(f) {
		if s.Name == gilStateSymbol && s.Size > 0 {
			sym, found = s, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s in %s", ErrSymbolNotFound, gilStateSymbol, path)
	}

	off, err := fileOffsetFor(f, sym.Value)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: %s in %s: %w", gilStateSymbol, path, err)
	}
	buf := make([]byte, sym.Size)
	if _, err := osf.ReadAt(buf, int64(off)); err != nil {
		return nil, fmt.Errorf("pyunwind: read %s body from %s: %w", gilStateSymbol, path, err)
	}
	return buf, nil
}

// fileOffsetFor maps a virtual address to a file offset through the PT_LOAD
// segment that covers it. Doing it through the program headers rather than
// through a section's Addr/Offset pair keeps this correct for binaries whose
// segments are not laid out with p_vaddr == p_offset.
func fileOffsetFor(f *elf.File, vaddr uint64) (uint64, error) {
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if vaddr < p.Vaddr || vaddr >= p.Vaddr+p.Filesz {
			continue
		}
		return p.Off + (vaddr - p.Vaddr), nil
	}
	return 0, fmt.Errorf("no PT_LOAD segment covers vaddr %#x", vaddr)
}
