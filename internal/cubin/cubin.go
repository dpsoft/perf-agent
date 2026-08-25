// Package cubin reads an NVIDIA cubin (a CUDA device ELF) and exposes the two
// things GPU PC-sample attribution needs: the module's device functions, and a
// line table that turns a function-relative PC offset into a source location.
//
// It is deliberately pure Go and cgo-free. The agent runs in containers that
// have no CUDA toolkit, so nothing here shells out to nvdisasm or cuobjdump or
// links libcupti. Only debug/elf and debug/dwarf are used.
//
// SASS is never decoded. The line table's addresses are what a PC offset is
// looked up against, so instruction decoding would buy nothing.
//
// # How a cubin carries line info
//
// A cubin built with "nvcc -lineinfo" carries a .debug_line section in
// standard DWARF v2 and no .debug_info at all. Go's debug/dwarf refuses to
// construct without .debug_info, so a minimal synthetic one is supplied
// (see synthesizeInfo) purely to obtain a *dwarf.LineReader.
//
// Presence of a non-empty .debug_line is exactly the "-lineinfo" signal: the
// same source built without it has .debug_frame and no .debug_line.
//
// # Why relocations are read rather than assumed
//
// .debug_line holds one line-program sequence per device function, and every
// sequence begins at address 0 — the addresses are function-relative, which is
// what a CUPTI PC offset is. Nothing inside the line program says which
// function a sequence describes. The binding lives entirely in .rel.debug_line,
// which carries one relocation per sequence, against that sequence's own
// function symbol, pointed at the 8-byte operand of the sequence's opening
// DW_LNE_set_address.
//
// Because every device function symbol has value 0, applying those relocations
// literally is the identity and would leave all sequences overlapping at 0 and
// therefore indistinguishable. So instead of the symbol's value, each sequence
// is relocated to a distinct synthetic base address (see relocSequences). The
// line table then reads back with each sequence in its own address window,
// which recovers the sequence-to-function binding from the relocations rather
// than from an assumption about sequence ordering.
package cubin

import (
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// ErrPanic wraps a panic escaping debug/elf or debug/dwarf on hostile input.
//
// Parse converts such a panic into an error so that a malformed cubin from a
// profiled process is counted rather than fatal. It is a distinct sentinel so
// that the fuzz target can still fail on it: a recover that made panics
// indistinguishable from ordinary parse errors would hide from the fuzzer
// exactly the bugs it exists to find.
var ErrPanic = errors.New("cubin: panic while parsing")

// maxFunctions bounds how many device functions and line-program sequences are
// admitted from one cubin. Cubin bytes arrive from a profiled process and are
// not trusted; this caps work and allocation independently of what the section
// headers claim.
const maxFunctions = 1 << 16

// seqShift sizes the synthetic address window given to each line-program
// sequence. Windows of 1 TiB are far larger than any device function, so a
// sequence can never advance out of its own window, and the window index
// recovers which sequence an address belongs to.
const seqShift = 40

// Function is one device function in the cubin: a FUNC symbol together with the
// .text section that holds its SASS.
type Function struct {
	// Name is the symbol name, as mangled by the CUDA compiler. Kernels
	// declared extern "C" appear unmangled.
	Name string

	// SymIndex is the function's index in the cubin's .symtab, counting the
	// null symbol at index 0. CUPTI documents CUpti_PCSamplingPCData's
	// functionIndex as "the function's unique symbol index in the module";
	// whether that is this index cannot be determined without hardware, so it
	// is exposed here for the task that measures it rather than assumed.
	SymIndex int

	// Section is the name of the section holding the function's code, which
	// for a cubin is ".text.<Name>". Empty if the symbol is not in a section.
	Section string

	// Size is the function's code size in bytes, from the symbol.
	Size uint64

	// HasLineInfo reports whether a line-program sequence in .debug_line is
	// bound to this function. It can be false while Cubin.HasLineInfo is true:
	// a module may carry line info for some of its functions and not others.
	HasLineInfo bool
}

// row is one line-table entry, with an address made function-relative.
type row struct {
	pcOffset uint64
	line     uint32
	file     string
	end      bool // DWARF end_sequence: coverage stops here
}

// Cubin is a parsed cubin. It is read-only after Parse and safe for concurrent
// use.
type Cubin struct {
	funcs []Function

	// rows holds each function's line table, sorted by pcOffset. Keyed by
	// function name.
	rows map[string][]row

	// size is each function's code size, keyed by name, so Resolve does not
	// scan funcs on what is a per-PC-sample hot path.
	size map[string]uint64

	// hasDebugLine records that a non-empty .debug_line section was present,
	// which is the "-lineinfo" signal. It is deliberately independent of
	// whether the table could be used, so that a build-flag choice is never
	// confused with a damaged table.
	hasDebugLine bool

	// lineErr is non-nil when .debug_line was present but could not be turned
	// into a usable table. See LineInfoErr.
	lineErr error
}

// Parse reads a cubin from b.
//
// It returns an error only for failures that make the module unusable as a
// whole: not an ELF, not a CUDA ELF, an unsupported encoding, or no symbol
// table. A .debug_line section that is absent, or present but unusable, is not
// an error — the function list is still valuable on its own, and the two cases
// are reported separately by HasLineInfo and LineInfoErr.
//
// b is not retained.
func Parse(b []byte) (c *Cubin, err error) {
	// Cubin bytes arrive over a socket from a profiled process and are not
	// trusted. debug/elf and debug/dwarf are careful but are not contractually
	// panic-free on hostile input, and the caller's guarantee here is that a
	// bad cubin is counted, never fatal. Converting a panic into an error
	// keeps that guarantee without pretending the parsers cannot fail.
	defer func() {
		if r := recover(); r != nil {
			c, err = nil, fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()

	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("cubin: not an ELF: %w", err)
	}
	if f.Machine != elf.EM_CUDA {
		return nil, fmt.Errorf("cubin: not a CUDA ELF: machine is %v", f.Machine)
	}
	// Every cubin NVIDIA emits is 64-bit little-endian. Refusing anything else
	// keeps the relocation and DWARF decoding below on one well-tested path
	// instead of guessing at a shape that has never been observed.
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return nil, fmt.Errorf("cubin: unsupported encoding: class %v, data %v", f.Class, f.Data)
	}

	syms, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("cubin: reading .symtab: %w", err)
	}

	c = &Cubin{rows: make(map[string][]row), size: make(map[string]uint64)}

	// f.Symbols omits the null symbol at index 0, so a symbol's .symtab index
	// is its slice index plus one. Relocations below index the real table.
	funcBySym := make(map[int]string)
	for i, s := range syms {
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC || s.Name == "" {
			continue
		}
		if len(c.funcs) >= maxFunctions {
			return nil, fmt.Errorf("cubin: more than %d functions", maxFunctions)
		}
		// Resolve is keyed by name, so two FUNC symbols sharing one would make
		// a lookup ambiguous and silently answer with whichever function's rows
		// landed in the map. Refuse rather than pick: returning one kernel's
		// source lines for another kernel's PC is the failure this package's
		// relocation handling exists to avoid, and it would be no better
		// arriving by this route.
		if _, dup := c.size[s.Name]; dup {
			return nil, fmt.Errorf("cubin: duplicate FUNC symbol %q", s.Name)
		}
		sec := ""
		if int(s.Section) < len(f.Sections) {
			sec = f.Sections[s.Section].Name
		}
		c.funcs = append(c.funcs, Function{
			Name:     s.Name,
			SymIndex: i + 1,
			Section:  sec,
			Size:     s.Size,
		})
		funcBySym[i+1] = s.Name
		c.size[s.Name] = s.Size
	}

	line, err := readSection(f, b, ".debug_line")
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		// No .debug_line: the module was built without -lineinfo. Not an
		// error, and explicitly not the same fact as a damaged table.
		return c, nil
	}
	c.hasDebugLine = true

	if err := c.buildLineTable(f, b, line, funcBySym); err != nil {
		// The module is still usable for its function list; only source
		// resolution is lost, and LineInfoErr says why.
		c.lineErr = err
		c.rows = make(map[string][]row)
	}

	for i := range c.funcs {
		c.funcs[i].HasLineInfo = len(c.rows[c.funcs[i].Name]) > 0
	}
	return c, nil
}

// readSection returns a section's bytes, refusing any section whose header
// claims a range outside the file. debug/elf caps allocation against the
// reader's size, but checking here makes the refusal explicit and keeps a
// malformed header from being read as a short section.
func readSection(f *elf.File, b []byte, name string) ([]byte, error) {
	s := f.Section(name)
	if s == nil {
		return nil, nil
	}
	if s.Type == elf.SHT_NOBITS {
		return nil, nil
	}
	end := s.Offset + s.FileSize
	if end < s.Offset || end > uint64(len(b)) {
		return nil, fmt.Errorf("cubin: section %s runs past end of file", name)
	}
	d, err := s.Data()
	if err != nil {
		return nil, fmt.Errorf("cubin: reading %s: %w", name, err)
	}
	return d, nil
}

// setAddrPrologue is the byte sequence that introduces DW_LNE_set_address in a
// DWARF line program: extended-opcode escape 0x00, ULEB length 9, then the
// DW_LNE_set_address opcode 0x02. Each .rel.debug_line relocation must point at
// the 8-byte operand that follows, and asserting that is what makes relocating
// to a synthetic base safe rather than a guess about section layout.
var setAddrPrologue = []byte{0x00, 0x09, 0x02}

// sequence is one line-program sequence, bound to a function by a relocation.
type sequence struct {
	fn   string
	base uint64
}

// relocSequences reads .rel.debug_line and rewrites, in a copy of the line
// section, each sequence's opening DW_LNE_set_address operand to a distinct
// synthetic base address. It returns the sequences in base order.
//
// The relocations are the only record of which function a sequence describes,
// so they are read rather than assumed; in particular the order of sequences
// within .debug_line is not assumed to match symbol or section order.
func relocSequences(f *elf.File, b, line []byte, funcBySym map[int]string) ([]sequence, error) {
	rs := f.Section(".rel.debug_line")
	if rs == nil {
		return nil, errors.New(".rel.debug_line is absent, so no sequence can be bound to a function")
	}
	if rs.Type != elf.SHT_REL {
		return nil, fmt.Errorf(".rel.debug_line has type %v, want SHT_REL", rs.Type)
	}
	rel, err := readSection(f, b, ".rel.debug_line")
	if err != nil {
		return nil, err
	}
	const relSize = 16 // Elf64_Rel: r_offset uint64, r_info uint64
	if len(rel)%relSize != 0 {
		return nil, fmt.Errorf(".rel.debug_line is %d bytes, not a multiple of %d", len(rel), relSize)
	}
	if n := len(rel) / relSize; n > maxFunctions {
		return nil, fmt.Errorf(".rel.debug_line has %d relocations, over the %d cap", n, maxFunctions)
	}

	var seqs []sequence
	for off := 0; off+relSize <= len(rel); off += relSize {
		rOffset := binary.LittleEndian.Uint64(rel[off:])
		rInfo := binary.LittleEndian.Uint64(rel[off+8:])
		symIdx := int(rInfo >> 32)

		fn, ok := funcBySym[symIdx]
		if !ok {
			// A relocation against something that is not one of our device
			// functions. Nothing to bind; skip it rather than guess.
			continue
		}
		// Assert the relocation lands on a DW_LNE_set_address operand. If it
		// does not, the layout is not what this reader understands and the
		// whole table is refused rather than partially rewritten.
		if rOffset < uint64(len(setAddrPrologue)) || rOffset+8 > uint64(len(line)) {
			return nil, fmt.Errorf("relocation at %#x is outside .debug_line", rOffset)
		}
		if !bytes.Equal(line[rOffset-uint64(len(setAddrPrologue)):rOffset], setAddrPrologue) {
			return nil, fmt.Errorf("relocation at %#x does not follow DW_LNE_set_address", rOffset)
		}
		if got := binary.LittleEndian.Uint64(line[rOffset:]); got != 0 {
			return nil, fmt.Errorf("relocation at %#x targets a non-zero address %#x", rOffset, got)
		}

		base := uint64(len(seqs)+1) << seqShift
		binary.LittleEndian.PutUint64(line[rOffset:], base)
		seqs = append(seqs, sequence{fn: fn, base: base})
	}
	if len(seqs) == 0 {
		return nil, errors.New(".rel.debug_line binds no sequence to a device function")
	}
	return seqs, nil
}

// synthesizeInfo returns the minimal .debug_abbrev and .debug_info that let
// debug/dwarf construct over a cubin's .debug_line. A cubin has no .debug_info
// of its own and dwarf.New refuses without one; these 8 and 16 bytes declare a
// single compile unit whose only attribute is DW_AT_stmt_list = 0, which is all
// a LineReader needs.
func synthesizeInfo() (abbrev, info []byte) {
	// code 1, DW_TAG_compile_unit (0x11), no children,
	// DW_AT_stmt_list (0x10) DW_FORM_data4 (0x06), attribute and abbrev
	// terminators.
	abbrev = []byte{0x01, 0x11, 0x00, 0x10, 0x06, 0x00, 0x00, 0x00}
	// version 2, abbrev_off 0, addr_size 8, then abbrev code 1 and
	// stmt_list 0.
	body := []byte{2, 0, 0, 0, 0, 0, 8, 0x01, 0, 0, 0, 0}
	info = make([]byte, 0, 4+len(body))
	info = append(info, byte(len(body)), 0, 0, 0) // unit_length
	info = append(info, body...)
	return abbrev, info
}

// buildLineTable relocates the line section onto synthetic per-sequence bases,
// walks it with debug/dwarf, and files each row under the function its sequence
// was bound to, with the address made function-relative again.
func (c *Cubin) buildLineTable(f *elf.File, b, line []byte, funcBySym map[int]string) error {
	// Rewriting happens on our own copy; the caller's bytes are never touched.
	patched := make([]byte, len(line))
	copy(patched, line)

	seqs, err := relocSequences(f, b, patched, funcBySym)
	if err != nil {
		return err
	}

	abbrev, info := synthesizeInfo()
	d, err := dwarf.New(abbrev, nil, nil, info, patched, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("constructing DWARF over .debug_line: %w", err)
	}
	cu, err := d.Reader().Next()
	if err != nil {
		return fmt.Errorf("reading synthetic compile unit: %w", err)
	}
	if cu == nil {
		return errors.New("synthetic compile unit is empty")
	}
	lr, err := d.LineReader(cu)
	if err != nil {
		return fmt.Errorf("opening line reader: %w", err)
	}
	if lr == nil {
		return errors.New("compile unit has no line table")
	}

	rows := make(map[string][]row)
	var ent dwarf.LineEntry
	for {
		err := lr.Next(&ent)
		if errors.Is(err, dwarf.ErrUnknownPC) {
			continue
		}
		if err != nil {
			break // io.EOF ends a well-formed table.
		}
		// Which synthetic window the address landed in names the sequence, and
		// therefore the function.
		idx := int(ent.Address>>seqShift) - 1
		if idx < 0 || idx >= len(seqs) {
			// An address outside every window means the table did not advance
			// the way this reader assumes. Refuse the table rather than file a
			// row under a function it may not belong to.
			return fmt.Errorf("line entry at %#x falls outside every relocated sequence", ent.Address)
		}
		seq := seqs[idx]
		pcOffset := ent.Address - seq.base
		// A sequence must stay inside the function it describes: every fixture
		// ends its sequence exactly at the function's symbol size, which is
		// what makes the addresses function-relative. A row beyond that means
		// the line program is not describing this function, so the table is
		// refused rather than half-trusted. Resolve's own range check would
		// hide such a row, and a table that has to be defended against at
		// lookup time is a table that should not have been accepted.
		if sz := c.size[seq.fn]; sz > 0 && pcOffset > sz {
			return fmt.Errorf("line entry at pcOffset %#x is past %s's %d bytes", pcOffset, seq.fn, sz)
		}
		file := ""
		if ent.File != nil {
			file = ent.File.Name
		}
		rows[seq.fn] = append(rows[seq.fn], row{
			pcOffset: pcOffset,
			line:     uint32(ent.Line), //nolint:gosec // DWARF line numbers are small positive integers.
			file:     file,
			end:      ent.EndSequence,
		})
	}

	for fn := range rows {
		r := rows[fn]
		sort.SliceStable(r, func(i, j int) bool { return r[i].pcOffset < r[j].pcOffset })
		rows[fn] = r
	}
	c.rows = rows
	return nil
}

// Functions returns the cubin's device functions, in symbol-table order.
//
// The returned slice is a copy; the caller may retain and modify it.
func (c *Cubin) Functions() []Function {
	out := make([]Function, len(c.funcs))
	copy(out, c.funcs)
	return out
}

// HasLineInfo reports whether the cubin carries a line table, which is exactly
// whether it has a non-empty .debug_line section and so whether it was built
// with -lineinfo.
//
// This is the fact that distinguishes the two reasons Resolve can return false.
// HasLineInfo false means no line table exists at all, which is a property of
// the module and is fixed by rebuilding with -lineinfo. HasLineInfo true with
// Resolve returning false means the table exists but does not cover that PC,
// which is a property of the PC and is not fixed by anything. A caller must
// never collapse the two.
//
// It reports the section's presence, not the table's usability, so that a
// damaged table is never reported as a build-flag choice. See LineInfoErr.
func (c *Cubin) HasLineInfo() bool { return c.hasDebugLine }

// LineInfoErr returns why a present .debug_line yielded no usable table, or nil
// if there is nothing wrong.
//
// It is nil both when the table parsed and when there was no .debug_line to
// begin with, so it must be read together with HasLineInfo:
//
//	HasLineInfo() == false                   built without -lineinfo
//	HasLineInfo() == true, LineInfoErr() == nil   usable table
//	HasLineInfo() == true, LineInfoErr() != nil   table present but damaged
//
// The third case is not the same fact as the first and a caller that reports it
// as "rebuild with -lineinfo" would be sending the reader after the wrong fix.
func (c *Cubin) LineInfoErr() error { return c.lineErr }

// Resolve maps a function-relative PC offset to its source location.
//
// fn is a function's Name as returned by Functions. pcOffset is relative to the
// start of that function, which is what CUPTI's PC-sample records carry.
//
// ok is false when the cubin has no line table, when fn is not a function of
// this cubin, when this function has no line-table sequence, or when the table
// does not cover pcOffset. Those are different facts: HasLineInfo distinguishes
// the first, Functions the second and third. No line is ever synthesized — no
// nearest-line search and no fall back to the function's first line — because a
// guessed line is indistinguishable from a measured one once it is in a label.
func (c *Cubin) Resolve(fn string, pcOffset uint64) (file string, line uint32, ok bool) {
	rows := c.rows[fn]
	if len(rows) == 0 {
		return "", 0, false
	}
	// The last row at or below pcOffset covers it, since a row's location
	// holds until the next row's address.
	i := sort.Search(len(rows), func(i int) bool { return rows[i].pcOffset > pcOffset })
	if i == 0 {
		return "", 0, false // before the first row
	}
	r := rows[i-1]
	if r.end {
		return "", 0, false // at or past end_sequence: outside the table's coverage
	}
	// A row must not resolve past the end of its own function, even if the
	// table lacks the end_sequence that would normally stop it.
	if sz := c.size[fn]; sz > 0 && pcOffset >= sz {
		return "", 0, false
	}
	return r.file, r.line, true
}
