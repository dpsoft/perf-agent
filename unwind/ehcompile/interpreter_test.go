package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCIE() *cie {
	return &cie{
		version:   1,
		codeAlign: 1,
		dataAlign: -8,
		raColumn:  16, // x86_64 convention
	}
}

func TestInterpret_AdvanceLocOnly(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		0x40 | 5,
		cfaAdvanceLoc1,
		10,
		cfaNop,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x100F, program)
	require.NoError(t, err)
	assert.Empty(t, s.entries)
	assert.Empty(t, s.classifications)
}

func TestInterpret_DefCFAEmitsRow(t *testing.T) {
	// def_cfa(rsp,8); advance(4); def_cfa_offset(16); advance(8).
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		0x40 | 4,
		cfaDefCFAOffset, 16,
		0x40 | 8,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x100C, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 2)

	assert.Equal(t, uint64(0x1000), s.entries[0].PCStart)
	assert.Equal(t, uint32(4), s.entries[0].PCEndDelta)
	assert.Equal(t, CFATypeSP, s.entries[0].CFAType)
	assert.Equal(t, int16(8), s.entries[0].CFAOffset)

	assert.Equal(t, uint64(0x1004), s.entries[1].PCStart)
	assert.Equal(t, uint32(8), s.entries[1].PCEndDelta)
	assert.Equal(t, CFATypeSP, s.entries[1].CFAType)
	assert.Equal(t, int16(16), s.entries[1].CFAOffset)
}

func TestInterpret_DefCFAWithFPOnARM64(t *testing.T) {
	// arm64 CIE: raColumn=30; def_cfa(x29, 16) → FP-based CFA.
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64X29, 16,
		0x40 | 4,
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x2000, 0x2004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	assert.Equal(t, CFATypeFP, s.entries[0].CFAType)
	assert.Equal(t, int16(16), s.entries[0].CFAOffset)
}

func TestInterpret_OffsetFPAndRA_x86(t *testing.T) {
	// def_cfa(rsp,16); offset(rbp,2); offset(RIP,1); advance(4).
	// data_align = -8, factor 2 → -16 for RBP; factor 1 → -8 for RA.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 16,
		0x80 | x86RBP, 2,
		0x80 | x86RIP, 1,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x3000, 0x3004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	e := s.entries[0]
	assert.Equal(t, CFATypeSP, e.CFAType)
	assert.Equal(t, int16(16), e.CFAOffset)
	assert.Equal(t, FPTypeOffsetCFA, e.FPType)
	assert.Equal(t, int16(-16), e.FPOffset)
	assert.Equal(t, RATypeOffsetCFA, e.RAType)
	assert.Equal(t, int16(-8), e.RAOffset)
}

func TestInterpret_OffsetFPAndRA_arm64(t *testing.T) {
	// arm64 CIE: raColumn=30. def_cfa(sp,16); offset(x29,2); offset(x30,1); advance(4).
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64SP, 16,
		0x80 | arm64X29, 2,
		0x80 | arm64X30, 1,
		0x40 | 4,
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x4000, 0x4004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	e := s.entries[0]
	assert.Equal(t, int16(-16), e.FPOffset)
	assert.Equal(t, int16(-8), e.RAOffset)
}

func TestInterpret_RememberRestoreState(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		0x40 | 2,
		cfaRememberState,
		cfaDefCFAOffset, 64,
		0x40 | 3,
		cfaRestoreState,
		0x40 | 5,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x5000, 0x500A, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 3)
	assert.Equal(t, int16(8), s.entries[0].CFAOffset)
	assert.Equal(t, int16(64), s.entries[1].CFAOffset)
	assert.Equal(t, int16(8), s.entries[2].CFAOffset)
}

func TestInterpret_ExpressionProducesFallback(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFAExpression, 1, 0x90,
		0x40 | 16,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x6000, 0x6010, program)
	require.NoError(t, err)
	assert.Empty(t, s.entries)
	require.Len(t, s.classifications, 1)
	assert.Equal(t, ModeFallback, s.classifications[0].Mode)
}

func TestInterpret_GnuArgsSizeIsNoop(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		cfaGnuArgsSize, 0x10,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x7000, 0x7004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
}

func TestInterpret_ARM64_TypicalPrologue(t *testing.T) {
	// Models a typical arm64 function prologue:
	//   stp x29, x30, [sp, #-16]!   (push FP + LR, decrement SP by 16)
	//   mov x29, sp                  (FP = SP)
	// CFI emitted by gcc for this typically looks like:
	//   def_cfa(sp, 0)
	//   advance_loc(4)
	//   def_cfa_offset(16)             CFA = SP + 16
	//   offset(x29, 2)                 x29 saved at CFA-16
	//   offset(x30, 1)                 x30 saved at CFA-8
	//   advance_loc(4)
	//   def_cfa_register(x29)          CFA = x29 + 16
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64SP, 0,
		0x40 | 4,
		cfaDefCFAOffset, 16,
		0x80 | arm64X29, 2,
		0x80 | arm64X30, 1,
		0x40 | 4,
		cfaDefCFARegister, arm64X29,
		0x40 | 16,
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x5000, 0x5018, program)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(s.entries), 2)

	// One row should have CFA=FP (x29)+16 — the main body after the prologue.
	var found bool
	for _, e := range s.entries {
		if e.CFAType == CFATypeFP && e.CFAOffset == 16 {
			found = true
			assert.Equal(t, FPTypeOffsetCFA, e.FPType)
			assert.Equal(t, int16(-16), e.FPOffset)
			assert.Equal(t, RATypeOffsetCFA, e.RAType)
			assert.Equal(t, int16(-8), e.RAOffset)
		}
	}
	assert.True(t, found, "expected a row with CFA=FP+16")
}

func TestInterpret_ARM64_NegateRAStateIsNoop(t *testing.T) {
	// DW_CFA_AArch64_negate_ra_state (0x2d) takes no operand and must
	// not affect CFA tracking. Verify it parses cleanly and the row
	// before/after has identical CFA/FP/RA rules.
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64SP, 16,
		0x80 | arm64X29, 2,
		0x80 | arm64X30, 1,
		0x40 | 4,
		cfaAArch64NegateRAState, // no operand
		0x40 | 4,
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x6000, 0x6008, program)
	require.NoError(t, err)
	// Coalescing should merge the two ranges since state is identical.
	require.Len(t, s.entries, 1)
	assert.Equal(t, uint32(8), s.entries[0].PCEndDelta)
}

// --- Additional interpreter edge cases ------------------------------------

func TestInterpret_CodeAlignScaling(t *testing.T) {
	// Non-1 code_alignment_factor — advance_loc deltas must be multiplied.
	// codeAlign=4 with advance_loc(3) should advance 12 PC units.
	c := &cie{version: 1, codeAlign: 4, dataAlign: -8, raColumn: 16}
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		0x40 | 3, // advance_loc(3) * 4 = +12
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x100C, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	assert.Equal(t, uint32(12), s.entries[0].PCEndDelta)
}

func TestInterpret_NonSPNonFPCFARegisterFallback(t *testing.T) {
	// def_cfa(RDX, 32) — RDX isn't SP or FP on x86_64 → FALLBACK.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RDX, 32,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.NoError(t, err)
	assert.Empty(t, s.entries)
	require.Len(t, s.classifications, 1)
	assert.Equal(t, ModeFallback, s.classifications[0].Mode)
}

func TestInterpret_RememberStateOverflow(t *testing.T) {
	// Push 17 states — exceeds the 16-deep stack.
	c := newTestCIE()
	program := []byte{cfaDefCFA, x86RSP, 8}
	for i := 0; i < 17; i++ {
		program = append(program, cfaRememberState)
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1010, program)
	require.Error(t, err)
}

func TestInterpret_RestoreStateUnderflow(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		cfaRestoreState, // no prior remember_state
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.Error(t, err)
}

func TestInterpret_UnhandledOpcode(t *testing.T) {
	// 0x17 is reserved and not handled — must error cleanly.
	c := newTestCIE()
	program := []byte{0x17}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unhandled opcode 0x17")
}

func TestInterpret_DefCFASF_Scaled(t *testing.T) {
	// def_cfa_sf with sleb128 offset -2; data_align=-8 → offset = 16.
	c := newTestCIE()
	program := []byte{
		cfaDefCFASF, x86RSP, 0x7e, // sleb128(-2)
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	assert.Equal(t, int16(16), s.entries[0].CFAOffset)
}

func TestInterpret_DefCFAOffsetSF(t *testing.T) {
	// Establish CFA then update with sleb128-scaled offset.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		cfaDefCFAOffsetSF, 0x7e, // sleb128(-2) * -8 = 16
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	assert.Equal(t, int16(16), s.entries[0].CFAOffset)
}

func TestInterpret_RegisterOpcode(t *testing.T) {
	// register(RBP, RDX): RBP is saved in RDX. We track this for FP.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 16,
		cfaRegister, x86RBP, x86RDX,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	assert.Equal(t, FPTypeRegister, s.entries[0].FPType)
}

func TestInterpret_SameValueOpcode(t *testing.T) {
	// same_value(RBP): caller's RBP is unchanged.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 16,
		0x80 | x86RBP, 2, // First save RBP
		0x40 | 2,
		cfaSameValue, x86RBP, // Then switch to same_value
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1006, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 2)
	assert.Equal(t, FPTypeOffsetCFA, s.entries[0].FPType)
	assert.Equal(t, FPTypeSameValue, s.entries[1].FPType)
}

func TestInterpret_CompressedRestore(t *testing.T) {
	// Compressed DW_CFA_restore(RBP) resets to undefined.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 16,
		0x80 | x86RBP, 2, // save RBP
		0x40 | 2,
		0xc0 | x86RBP, // restore RBP
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1006, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 2)
	assert.Equal(t, FPTypeOffsetCFA, s.entries[0].FPType)
	assert.Equal(t, FPTypeUndefined, s.entries[1].FPType)
}

func TestInterpret_ValOffsetProducesFallback(t *testing.T) {
	// val_offset is not tracked — marks state as expression → FALLBACK.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		cfaValOffset, x86RBP, 2,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.NoError(t, err)
	// After val_offset the state is in ruleExpression → classification fallback.
	require.Len(t, s.classifications, 1)
	assert.Equal(t, ModeFallback, s.classifications[0].Mode)
}

func TestInterpret_AdvanceLocTruncated(t *testing.T) {
	// advance_loc1 expects 1 operand byte — program runs out.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		cfaAdvanceLoc1, // no operand byte follows
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x1004, program)
	require.Error(t, err)
}

// --- Interpreter benchmarks -----------------------------------------------

func BenchmarkInterpret_SimplePrologue(b *testing.B) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		0x40 | 4,
		cfaDefCFAOffset, 16,
		0x80 | x86RBP, 2,
		0x80 | x86RIP, 1,
		0x40 | 8,
		cfaDefCFARegister, x86RBP,
		0x40 | 0x20,
	}
	for b.Loop() {
		s := newInterpreter(c, archX86_64())
		_ = s.run(0x1000, 0x1040, program)
	}
}

func BenchmarkInterpret_ARM64Prologue(b *testing.B) {
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64SP, 0,
		0x40 | 4,
		cfaDefCFAOffset, 16,
		0x80 | arm64X29, 2,
		0x80 | arm64X30, 1,
		0x40 | 4,
		cfaDefCFARegister, arm64X29,
		0x40 | 16,
	}
	for b.Loop() {
		s := newInterpreter(c, archARM64())
		_ = s.run(0x2000, 0x2018, program)
	}
}

func BenchmarkInterpret_DeepAdvance(b *testing.B) {
	// 100 advance_loc updates with no rule changes — should coalesce
	// aggressively. Tests the coalescing fast path.
	c := newTestCIE()
	program := []byte{cfaDefCFA, x86RSP, 8}
	for i := 0; i < 100; i++ {
		program = append(program, 0x40|1)
	}
	for b.Loop() {
		s := newInterpreter(c, archX86_64())
		_ = s.run(0x1000, 0x1000+100, program)
	}
}
