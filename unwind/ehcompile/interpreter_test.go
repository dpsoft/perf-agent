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
