package pyunwind

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
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

// EvalRangeForFile returns the eval-loop text range of a libpython (or a
// statically linked python executable), in the load-bias-relative space
// bpf/unwind_common.h's mapping_for_pc reports rel_pc in -- which is the
// ELF's own virtual-address space, since rel_pc is pc - load_bias.
//
// WHY THE LARGEST FRAGMENT, not the exported symbol. GCC's PGO builds
// partition _PyEval_EvalFrameDefault: the exported symbol keeps the entry
// block and the dispatch loop moves to a local `.cold` (or `.part`,
// `.constprop`) sibling. On actions/setup-python's 3.12.14 -- the
// interpreter this project's CI runs -- the exported symbol is 350 bytes
// and _PyEval_EvalFrameDefault.cold is 51431, and a profile of that build
// shows every eval-loop return address landing inside the .cold fragment
// and none inside the exported one. Taking the exported symbol's range
// there installs a range no sample ever falls in: the arm is on, and
// nothing happens.
//
// Only ONE range is installed per binary because py_eval_ranges holds one
// per table_id. Where the loop is split across several fragments this
// covers the largest; samples landing in a smaller sibling are missed, and
// that shows up as PyCntChainAbandoned rather than as a wrong stack.
func EvalRangeForFile(path string) (EvalRange, error) {
	f, err := elf.Open(path)
	if err != nil {
		return EvalRange{}, fmt.Errorf("pyunwind: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	best, err := pickEvalFragment(allFuncSymbols(f))
	if err != nil {
		return EvalRange{}, fmt.Errorf("%w in %s", err, path)
	}
	return EvalRange{Lo: best.Value, Hi: best.Value + best.Size}, nil
}

// pickEvalFragment chooses the eval-loop fragment from a symbol list. Split
// out from EvalRangeForFile so the choice can be tested without an ELF.
func pickEvalFragment(syms []elf.Symbol) (elf.Symbol, error) {
	var best elf.Symbol
	var found bool
	var names []string
	for _, s := range syms {
		if s.Name != evalLoopSymbol && !strings.HasPrefix(s.Name, evalLoopSymbol+".") {
			continue
		}
		names = append(names, fmt.Sprintf("%s (%d bytes)", s.Name, s.Size))
		if !found || s.Size > best.Size {
			best, found = s, true
		}
	}
	if !found {
		return elf.Symbol{}, fmt.Errorf("%w: no %s symbol", ErrEvalLoopNotLocatable, evalLoopSymbol)
	}
	if best.Size < minEvalLoopBytes {
		return elf.Symbol{}, fmt.Errorf(
			"%w: the largest %s fragment is only %d bytes, below the %d-byte floor for a bytecode dispatch loop; "+
				"this looks like a stripped, PGO-partitioned build whose loop has no symbol in .dynsym (candidates: %s). "+
				"Installing the debug symbols for this interpreter makes the fragment visible",
			ErrEvalLoopNotLocatable, evalLoopSymbol, best.Size, minEvalLoopBytes, strings.Join(names, ", "))
	}
	return best, nil
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

// GILStateCode reads the machine-code body of PyGILState_GetThisThreadState
// out of a libpython, for ParseAutoTSSKeyOffset.
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

	const want = "PyGILState_GetThisThreadState"
	var sym elf.Symbol
	var found bool
	for _, s := range allFuncSymbols(f) {
		if s.Name == want && s.Size > 0 {
			sym, found = s, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s in %s", ErrSymbolNotFound, want, path)
	}

	off, err := fileOffsetFor(f, sym.Value)
	if err != nil {
		return nil, fmt.Errorf("pyunwind: %s in %s: %w", want, path, err)
	}
	buf := make([]byte, sym.Size)
	if _, err := osf.ReadAt(buf, int64(off)); err != nil {
		return nil, fmt.Errorf("pyunwind: read %s body from %s: %w", want, path, err)
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
