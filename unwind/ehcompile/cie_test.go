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

// FDE for the sample CIE:
//
//	length = 0x10 (16 bytes body follow)
//	CIE_pointer = 0x1C (backward offset from this field to CIE start)
//	initial_location = 0x100 (sdata4 pcrel)
//	address_range = 0x20
//	augmentation length = 0
//	instructions = DW_CFA_nop * 3
func sampleFDE(ciePos uint64, fdePos uint64) []byte {
	return []byte{
		0x10, 0x00, 0x00, 0x00,
		0x1c, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00,
		0x20, 0x00, 0x00, 0x00,
		0x00,
		0x00, 0x00, 0x00,
	}
}

func TestParseFDE_Basic(t *testing.T) {
	cieRaw := sampleCIEx86()
	c, err := parseCIE(cieRaw, 0)
	require.NoError(t, err)

	fdeRaw := sampleFDE(0, uint64(len(cieRaw)))
	f, err := parseFDE(fdeRaw, uint64(len(cieRaw)), c)
	require.NoError(t, err)

	wantPC := uint64(len(cieRaw)) + 8 + 0x100
	assert.Equal(t, wantPC, f.initialLocation)
	assert.Equal(t, uint64(0x20), f.addressRange)
	assert.NotEmpty(t, f.instructions)
}
