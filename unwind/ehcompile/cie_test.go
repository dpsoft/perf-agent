package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Canonical x86_64 CIE:
//
//	length = 0x14
//	CIE_id = 0
//	version = 1
//	augmentation = "zR\0"
//	code_alignment_factor = 1 (uleb128)
//	data_alignment_factor = -8 (sleb128 = 0x78)
//	return_address_column = 16 (uleb128)
//	z augmentation length = 1
//	R augmentation data = 0x1B (DW_EH_PE_pcrel|sdata4)
//	initial instructions:
//	  DW_CFA_def_cfa(7, 8)
//	  DW_CFA_offset(16, 1)   // RA at CFA + 1 * -8 = -8
//	  DW_CFA_nop
func sampleCIEx86() []byte {
	return []byte{
		0x13, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x01,
		'z', 'R', 0x00,
		0x01,
		0x78,
		0x10,
		0x01,
		0x1b,
		0x0c, 0x07, 0x08,
		0x90, 0x01,
		0x00,
	}
}

func TestParseCIE_Basic(t *testing.T) {
	c, err := parseCIE(sampleCIEx86(), 0)
	require.NoError(t, err)
	assert.Equal(t, byte(1), c.version)
	assert.Equal(t, "zR", c.augmentation)
	assert.Equal(t, uint64(1), c.codeAlign)
	assert.Equal(t, int64(-8), c.dataAlign)
	assert.Equal(t, uint64(16), c.raColumn)
	assert.Equal(t, byte(0x1b), c.fdePointerEnc)
	assert.NotEmpty(t, c.initialInstructions)
}

func TestParseCIE_UnknownAugmentation(t *testing.T) {
	b := sampleCIEx86()
	b[10] = 'X'
	_, err := parseCIE(b, 0)
	require.Error(t, err)
}
