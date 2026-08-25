package cubin

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insnBytes is the SASS instruction size on sm_70 and later, where every
// instruction is a 128-bit bundle with no separate control word. The library
// never decodes SASS; this stride is used only by the tests, to enumerate the
// candidate PCs a sampler could report inside a function. TestInstructionStride
// asserts the assumption against the fixtures rather than leaving it implicit.
const insnBytes = 16

func loadFixture(t *testing.T, name string) *Cubin {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".cubin"))
	require.NoError(t, err, "fixture %s missing; see testdata/README.md to regenerate", name)
	c, err := Parse(b)
	require.NoError(t, err)
	return c
}

// TestSingleLineinfoExactPCToLine pins the whole line table of the
// single-kernel fixture: every row boundary, and an interior PC inside each
// row's span to confirm a row's location holds until the next row.
func TestSingleLineinfoExactPCToLine(t *testing.T) {
	c := loadFixture(t, "single_lineinfo")
	require.True(t, c.HasLineInfo())
	require.NoError(t, c.LineInfoErr())

	// Row boundaries, exactly as .debug_line records them for addOne.
	rows := []struct {
		pc   uint64
		line uint32
	}{
		{0x00, 5}, {0x10, 6}, {0x40, 7}, {0x60, 8},
		{0xa0, 10}, {0xb0, 9}, {0xc0, 10}, {0xd0, 12},
	}
	for _, r := range rows {
		file, line, ok := c.Resolve("addOne", r.pc)
		require.True(t, ok, "pcOffset %#x should resolve", r.pc)
		assert.Equal(t, r.line, line, "pcOffset %#x", r.pc)
		// The directory component of the recorded path is the build working
		// directory, so only the base name is stable across regeneration.
		assert.Equal(t, "single.cu", filepath.Base(file), "pcOffset %#x", r.pc)
	}

	// Interior PCs: each takes the line of the row at or below it.
	interior := []struct {
		pc   uint64
		line uint32
	}{
		{0x08, 5}, {0x18, 6}, {0x30, 6}, {0x50, 7},
		{0x70, 8}, {0x90, 8}, {0xa8, 10}, {0xb8, 9}, {0xe0, 12}, {0x170, 12},
	}
	for _, r := range interior {
		_, line, ok := c.Resolve("addOne", r.pc)
		require.True(t, ok, "pcOffset %#x should resolve", r.pc)
		assert.Equal(t, r.line, line, "pcOffset %#x", r.pc)
	}

	// The end_sequence address equals the function's size, which is what makes
	// the table's addresses function-relative. At and past it nothing resolves.
	fns := c.Functions()
	require.Len(t, fns, 1)
	assert.Equal(t, uint64(0x180), fns[0].Size)
	for _, pc := range []uint64{0x180, 0x190, 1 << 20} {
		_, _, ok := c.Resolve("addOne", pc)
		assert.False(t, ok, "pcOffset %#x is outside the function and must not resolve", pc)
	}
}

// TestNoLineinfo asserts the -lineinfo signal: the same source built without
// it has no .debug_line, HasLineInfo is false, and nothing resolves anywhere in
// the function.
func TestNoLineinfo(t *testing.T) {
	c := loadFixture(t, "single_nolineinfo")

	assert.False(t, c.HasLineInfo(), "a cubin built without -lineinfo has no .debug_line")
	assert.NoError(t, c.LineInfoErr(), "an absent .debug_line is a build-flag choice, not a damaged table")

	fns := c.Functions()
	require.Len(t, fns, 1)
	assert.Equal(t, "addOne", fns[0].Name)
	assert.False(t, fns[0].HasLineInfo)
	// The function list survives, which is what lets a caller still name the
	// kernel a PC sample landed in.
	assert.Equal(t, uint64(384), fns[0].Size)
	assert.Equal(t, ".text.addOne", fns[0].Section)

	for pc := uint64(0); pc < fns[0].Size+64; pc += insnBytes {
		file, line, ok := c.Resolve("addOne", pc)
		require.False(t, ok, "pcOffset %#x must not resolve without a line table", pc)
		assert.Empty(t, file)
		assert.Zero(t, line)
	}
}

// TestTwoKernelsResolveIndependently is the fixture the plan requires to settle
// whether .debug_line holds one sequence per function or one relocated
// sequence. Both kernels have identical PC ranges (each is 384 bytes starting
// at 0), so the only thing that can tell their rows apart is the relocation
// binding, and a resolver that got that wrong would return the other kernel's
// lines here rather than failing.
func TestTwoKernelsResolveIndependently(t *testing.T) {
	c := loadFixture(t, "two_kernels_lineinfo")
	require.True(t, c.HasLineInfo())
	require.NoError(t, c.LineInfoErr())

	fns := c.Functions()
	require.Len(t, fns, 2)
	byName := map[string]Function{}
	for _, f := range fns {
		byName[f.Name] = f
		assert.True(t, f.HasLineInfo, "%s should have a bound sequence", f.Name)
		assert.Equal(t, uint64(384), f.Size)
	}
	require.Contains(t, byName, "scale")
	require.Contains(t, byName, "offset")

	// scale is declared first in the source and its rows are lines 5..11.
	scale := []struct {
		pc   uint64
		line uint32
	}{{0x00, 5}, {0x10, 6}, {0x40, 7}, {0x60, 8}, {0xa0, 9}, {0xd0, 11}}
	// offset is declared second and its rows are lines 13..21. Its PC range is
	// the same as scale's.
	offset := []struct {
		pc   uint64
		line uint32
	}{{0x00, 13}, {0x10, 14}, {0x40, 15}, {0x60, 16}, {0xa0, 19}, {0xb0, 17}, {0xc0, 19}, {0xe0, 21}}

	for _, r := range scale {
		file, line, ok := c.Resolve("scale", r.pc)
		require.True(t, ok, "scale pcOffset %#x", r.pc)
		assert.Equal(t, r.line, line, "scale pcOffset %#x", r.pc)
		assert.Equal(t, "two_kernels.cu", filepath.Base(file))
	}
	for _, r := range offset {
		file, line, ok := c.Resolve("offset", r.pc)
		require.True(t, ok, "offset pcOffset %#x", r.pc)
		assert.Equal(t, r.line, line, "offset pcOffset %#x", r.pc)
		assert.Equal(t, "two_kernels.cu", filepath.Base(file))
	}

	// The two kernels' line ranges are disjoint, so no PC of one can be read as
	// a line of the other.
	for pc := uint64(0); pc < 384; pc += insnBytes {
		_, sl, ok := c.Resolve("scale", pc)
		require.True(t, ok)
		_, ol, ok := c.Resolve("offset", pc)
		require.True(t, ok)
		assert.LessOrEqual(t, sl, uint32(11), "scale pcOffset %#x resolved to line %d, which belongs to offset", pc, sl)
		assert.GreaterOrEqual(t, ol, uint32(13), "offset pcOffset %#x resolved to line %d, which belongs to scale", pc, ol)
	}

	_, _, ok := c.Resolve("noSuchKernel", 0)
	assert.False(t, ok)
}

// TestRelDebugLineBindsSequencesToFunctions asserts, by reading the bytes, the
// two structural facts the resolver depends on:
//
//   - .debug_line holds one sequence per function, each starting at
//     pcOffset 0, so the addresses are function-relative and the relocation is
//     the identity for a function-relative lookup;
//   - .rel.debug_line is the only record of which sequence is which function,
//     and its entry order is not the sequences' byte order in .debug_line, so
//     the binding must come from the relocations and not from position.
func TestRelDebugLineBindsSequencesToFunctions(t *testing.T) {
	for _, name := range []string{"single_lineinfo", "two_kernels_lineinfo", "unrolled_lineinfo"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name+".cubin"))
			require.NoError(t, err)
			f, err := elf.NewFile(bytes.NewReader(b))
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })

			line, err := f.Section(".debug_line").Data()
			require.NoError(t, err)
			rel, err := f.Section(".rel.debug_line").Data()
			require.NoError(t, err)
			syms, err := f.Symbols()
			require.NoError(t, err)

			c := loadFixture(t, name)
			nFuncs := len(c.Functions())
			require.Equal(t, nFuncs, len(rel)/16, "one relocation per function")

			var relOffsets []uint64
			for off := 0; off+16 <= len(rel); off += 16 {
				rOff := binary.LittleEndian.Uint64(rel[off:])
				rInfo := binary.LittleEndian.Uint64(rel[off+8:])
				sym := syms[int(rInfo>>32)-1]

				// The relocation targets the 8-byte operand of a sequence's
				// opening DW_LNE_set_address...
				require.Equal(t, setAddrPrologue, line[rOff-3:rOff],
					"relocation at %#x must follow DW_LNE_set_address", rOff)
				// ...and that operand is zero, i.e. every sequence starts at
				// address 0. Applying the relocation would write the symbol's
				// value, which is also 0 — the identity for our purposes,
				// asserted here rather than assumed.
				require.Zero(t, binary.LittleEndian.Uint64(line[rOff:]),
					"sequence at %#x must start at address 0", rOff)
				require.Zero(t, sym.Value, "function symbol %s must have value 0", sym.Name)
				require.Equal(t, elf.STT_FUNC, elf.ST_TYPE(sym.Info))

				relOffsets = append(relOffsets, rOff)

				// The bound function's own table starts at pcOffset 0.
				rows := c.rows[sym.Name]
				require.NotEmpty(t, rows, "no rows bound to %s", sym.Name)
				assert.Zero(t, rows[0].pcOffset, "%s's sequence must start at pcOffset 0", sym.Name)
				assert.True(t, rows[len(rows)-1].end, "%s's sequence must end with end_sequence", sym.Name)
				assert.Equal(t, sym.Size, rows[len(rows)-1].pcOffset,
					"%s's end_sequence address must equal its symbol size, which is what makes the addresses function-relative", sym.Name)
			}

			if name == "two_kernels_lineinfo" {
				// The load-bearing negative: relocation order is the reverse of
				// the sequences' byte order here, so zipping sequence i to
				// relocation i would bind each kernel's rows to the other one.
				require.Len(t, relOffsets, 2)
				assert.Greater(t, relOffsets[0], relOffsets[1],
					"relocation order is expected to differ from .debug_line byte order in this fixture")
			}
		})
	}
}

// TestInstructionStride asserts the assumption the collapse measurement rests
// on: on sm_86 every instruction is 16 bytes, so every line-table address and
// every function size is a multiple of 16. If a future fixture broke this the
// collapse ratio would be measured over PCs that cannot occur.
func TestInstructionStride(t *testing.T) {
	for _, name := range []string{"single_lineinfo", "two_kernels_lineinfo", "unrolled_lineinfo"} {
		c := loadFixture(t, name)
		for _, f := range c.Functions() {
			assert.Zero(t, f.Size%insnBytes, "%s/%s size %d", name, f.Name, f.Size)
			for _, r := range c.rows[f.Name] {
				assert.Zero(t, r.pcOffset%insnBytes, "%s/%s row %#x", name, f.Name, r.pcOffset)
			}
		}
	}
}

// TestUnrolledCollapseRatio makes the plan's PC-to-line collapse claim
// assertable rather than asserted. gpu_pc carries one value per distinct
// sampled instruction, so distinct PCs are counted at instruction granularity
// across the function, not at line-table row granularity: a fully unrolled loop
// emits one row spanning every copy of the body, and all of those instructions
// are distinct sample PCs that share one source line.
func TestUnrolledCollapseRatio(t *testing.T) {
	c := loadFixture(t, "unrolled_lineinfo")
	require.True(t, c.HasLineInfo())

	fns := c.Functions()
	require.Len(t, fns, 1)
	fn := fns[0]
	require.Equal(t, "unrolledSum", fn.Name)

	distinctPCs := 0
	distinctLines := map[uint32]bool{}
	for pc := uint64(0); pc < fn.Size; pc += insnBytes {
		_, line, ok := c.Resolve(fn.Name, pc)
		if !ok {
			continue
		}
		distinctPCs++
		distinctLines[line] = true
	}

	require.NotEmpty(t, distinctLines)
	t.Logf("collapse: %d distinct instruction PCs -> %d distinct source lines (%.1fx)",
		distinctPCs, len(distinctLines), float64(distinctPCs)/float64(len(distinctLines)))

	assert.GreaterOrEqual(t, distinctPCs, 4*len(distinctLines),
		"distinct PCs (%d) must outnumber distinct lines (%d) by at least 4x",
		distinctPCs, len(distinctLines))
}

// TestParseRejectsNonCubin covers the inputs Parse must refuse outright, as
// opposed to the ones it degrades on.
func TestParseRejectsNonCubin(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"short", []byte{0x7f, 'E'}},
		{"not elf", bytes.Repeat([]byte{0xab}, 4096)},
		{"elf magic only", append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 128)...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(tc.in)
			assert.Error(t, err)
			assert.Nil(t, c)
		})
	}

	// A well-formed ELF that is simply not a CUDA one is refused by machine,
	// so a host object can never be mistaken for a module's cubin.
	host, err := os.ReadFile("/proc/self/exe")
	if err == nil && len(host) > 0 {
		c, err := Parse(host)
		assert.Error(t, err)
		assert.Nil(t, c)
	}
}

// TestParseDoesNotRetainInput asserts Parse does not alias the caller's bytes,
// which matters because the transport hands over an mmap that may be unmapped
// once Parse returns.
func TestParseDoesNotRetainInput(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "single_lineinfo.cubin"))
	require.NoError(t, err)
	c, err := Parse(b)
	require.NoError(t, err)

	for i := range b {
		b[i] = 0
	}
	file, line, ok := c.Resolve("addOne", 0x10)
	require.True(t, ok)
	assert.Equal(t, uint32(6), line)
	assert.Equal(t, "single.cu", filepath.Base(file))
	assert.Equal(t, "addOne", c.Functions()[0].Name)
}

// TestFunctionsIsACopy asserts a caller cannot mutate the parsed cubin through
// the returned slice.
func TestFunctionsIsACopy(t *testing.T) {
	c := loadFixture(t, "single_lineinfo")
	got := c.Functions()
	require.Len(t, got, 1)
	got[0].Name = "clobbered"
	assert.Equal(t, "addOne", c.Functions()[0].Name)
}

// TestDamagedLineTableIsNotReportedAsAbsent covers the third state of the
// line-info triple. An absent .debug_line means "built without -lineinfo",
// which the reader fixes by rebuilding; a present but unusable one means the
// bytes are damaged, which rebuilding will not fix. Reporting the second as the
// first would send the reader after the wrong problem, so HasLineInfo reports
// the section's presence and LineInfoErr reports its usability.
func TestDamagedLineTableIsNotReportedAsAbsent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "single_lineinfo.cubin"))
	require.NoError(t, err)

	// Locate the DW_LNE_set_address opcode that .rel.debug_line points at, and
	// corrupt it, so the relocation can no longer be verified to land on a
	// sequence start and the table is refused.
	f, err := elf.NewFile(bytes.NewReader(b))
	require.NoError(t, err)
	lineSec := f.Section(".debug_line")
	require.NotNil(t, lineSec)
	rel, err := f.Section(".rel.debug_line").Data()
	require.NoError(t, err)
	require.NotEmpty(t, rel)
	require.NoError(t, f.Close())

	relOff := binary.LittleEndian.Uint64(rel[0:])
	opcodeAt := lineSec.Offset + relOff - 1 // the DW_LNE_set_address byte
	require.Less(t, opcodeAt, uint64(len(b)))

	damaged := append([]byte(nil), b...)
	require.Equal(t, byte(0x02), damaged[opcodeAt], "expected DW_LNE_set_address here")
	damaged[opcodeAt] = 0x03

	c, err := Parse(damaged)
	require.NoError(t, err, "a damaged line table must not lose the function list")

	assert.True(t, c.HasLineInfo(), ".debug_line is still present, so this is not a -lineinfo problem")
	assert.Error(t, c.LineInfoErr(), "the table is unusable and must say so")

	fns := c.Functions()
	require.Len(t, fns, 1)
	assert.Equal(t, "addOne", fns[0].Name, "the kernel is still nameable")
	assert.False(t, fns[0].HasLineInfo)

	for pc := uint64(0); pc < fns[0].Size; pc += insnBytes {
		_, _, ok := c.Resolve("addOne", pc)
		require.False(t, ok, "nothing may resolve from a refused table (pcOffset %#x)", pc)
	}
}

// TestLineTableRefusesRowsPastTheFunction pins the invariant a fuzz finding
// added: a line program that walks past the end of the function it is bound to
// is not describing that function, and the table is refused rather than
// filtered at lookup time.
func TestLineTableRefusesRowsPastTheFunction(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "single_lineinfo.cubin"))
	require.NoError(t, err)

	f, err := elf.NewFile(bytes.NewReader(b))
	require.NoError(t, err)
	lineSec := f.Section(".debug_line")
	require.NotNil(t, lineSec)
	line, err := lineSec.Data()
	require.NoError(t, err)
	rel, err := f.Section(".rel.debug_line").Data()
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Scan the line program past the opening DW_LNE_set_address, whose operand
	// the relocation owns, for a DW_LNS_advance_pc (0x02) with a single-byte
	// ULEB128 operand.
	const advancePC = 0x02
	scanFrom := int(binary.LittleEndian.Uint64(rel[0:])) + 8
	idx := -1
	for i := scanFrom; i+1 < len(line); i++ {
		if line[i] == advancePC && line[i+1] < 0x80 {
			idx = i
			break
		}
	}
	require.Greater(t, idx, 0, "no single-byte DW_LNS_advance_pc found")

	// 0x7f is the largest single-byte ULEB128, so replacing the operand keeps
	// the program's length and every following opcode intact while walking the
	// address past the function's 384 bytes.
	require.Less(t, line[idx+1], byte(0x7f), "operand is already maximal")
	damaged := append([]byte(nil), b...)
	damaged[lineSec.Offset+uint64(idx)+1] = 0x7f

	c, err := Parse(damaged)
	require.NoError(t, err, "a damaged line table must not lose the function list")
	assert.True(t, c.HasLineInfo())
	if assert.Error(t, c.LineInfoErr()) {
		assert.Contains(t, c.LineInfoErr().Error(), "past addOne's 384 bytes")
	}
	assert.False(t, c.Functions()[0].HasLineInfo)
	_, _, ok := c.Resolve("addOne", 0)
	assert.False(t, ok, "nothing may resolve from a refused table")
}

// TestDuplicateFunctionNamesAreRefused covers the one way a name-keyed Resolve
// could answer with the wrong function's lines without any relocation being
// misread.
func TestDuplicateFunctionNamesAreRefused(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "two_kernels_lineinfo.cubin"))
	require.NoError(t, err)

	f, err := elf.NewFile(bytes.NewReader(b))
	require.NoError(t, err)
	symtab := f.Section(".symtab")
	require.NotNil(t, symtab)
	syms, err := f.Symbols()
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Find the two kernels' .symtab indices, then point the first symbol's
	// st_name at the second's string so both read as the same name.
	idx := map[string]uint64{}
	for i, sym := range syms {
		if elf.ST_TYPE(sym.Info) == elf.STT_FUNC {
			idx[sym.Name] = uint64(i) + 1 // f.Symbols omits the null symbol
		}
	}
	require.Contains(t, idx, "scale")
	require.Contains(t, idx, "offset")

	const symSize = 24 // Elf64_Sym; st_name is its first 4 bytes
	dup := append([]byte(nil), b...)
	scaleName := binary.LittleEndian.Uint32(dup[symtab.Offset+idx["scale"]*symSize:])
	binary.LittleEndian.PutUint32(dup[symtab.Offset+idx["offset"]*symSize:], scaleName)

	c, err := Parse(dup)
	assert.Nil(t, c)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), `duplicate FUNC symbol "scale"`)
	}
}
