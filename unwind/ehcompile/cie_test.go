package ehcompile

import (
	"encoding/binary"
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
//	CIE_pointer = backward offset from CIE_pointer field to CIE record start
//	initial_location = 0x100 (sdata4 pcrel)
//	address_range = 0x20
//	augmentation length = 0
//	instructions = DW_CFA_nop * 3
func sampleFDE(ciePos uint64, fdePos uint64) []byte {
	// CIE_pointer field is at fdePos+4; value = distance back to CIE record start.
	ciePtrFieldPos := fdePos + 4
	ciePtr := uint32(ciePtrFieldPos - ciePos)
	b := make([]byte, 20)
	binary.LittleEndian.PutUint32(b[0:], 0x10)
	binary.LittleEndian.PutUint32(b[4:], ciePtr)
	// initial_location: 0x100 (sdata4 pcrel, little-endian)
	binary.LittleEndian.PutUint32(b[8:], 0x100)
	// address_range: 0x20
	binary.LittleEndian.PutUint32(b[12:], 0x20)
	// augmentation length = 0, three DW_CFA_nop bytes
	b[16] = 0x00
	b[17] = 0x00
	b[18] = 0x00
	b[19] = 0x00
	return b
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

func TestWalkEHFrame_CIEAndFDE(t *testing.T) {
	cieRaw := sampleCIEx86()
	fdeRaw := sampleFDE(0, uint64(len(cieRaw)))
	section := append(append([]byte{}, cieRaw...), fdeRaw...)

	var cies, fdes int
	err := walkEHFrame(section, 0, func(off uint64, c *cie, f *fde) error {
		if c != nil {
			cies++
		}
		if f != nil {
			fdes++
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, cies)
	assert.Equal(t, 1, fdes)
}

// --- Additional CIE / FDE / walker edge cases -----------------------------

func TestParseCIE_TooShort(t *testing.T) {
	_, err := parseCIE([]byte{0x01, 0x02}, 0)
	require.Error(t, err)
}

func TestParseCIE_ZeroLength(t *testing.T) {
	// length=0 is the EOF sentinel, not a valid CIE.
	_, err := parseCIE([]byte{0x00, 0x00, 0x00, 0x00}, 0)
	require.Error(t, err)
}

func TestParseCIE_NonZeroCIE_id(t *testing.T) {
	// CIE_id must be 0 for .eh_frame. Corrupt byte 4 to trigger error.
	b := sampleCIEx86()
	b[4] = 0x01
	_, err := parseCIE(b, 0)
	require.Error(t, err)
}

func TestParseCIE_UnsupportedVersion(t *testing.T) {
	b := sampleCIEx86()
	b[8] = 0x05 // not 1 or 3
	_, err := parseCIE(b, 0)
	require.Error(t, err)
}

func TestParseCIE_LegacyEHAugmentation(t *testing.T) {
	// Build a CIE with augmentation string "eh" (legacy gcc) — we reject.
	// Start from sampleCIEx86 and rewrite the augmentation field bytes.
	// sampleCIEx86 has "zR\0" at offset 9..11; replace with "eh\0".
	b := sampleCIEx86()
	b[9] = 'e'
	b[10] = 'h'
	// b[11] stays as 0 (null terminator)
	_, err := parseCIE(b, 0)
	require.Error(t, err)
}

func TestParseCIE_EmptyAugmentation(t *testing.T) {
	// Build a minimal CIE with no augmentation. Single-byte augmentation
	// string "" (just a null terminator), followed by code/data align
	// and RA column, no augmentation data, just nops for initial insts.
	//   length body = 4 (CIE_id) + 1 (version) + 1 (null aug) + 1 (codeAlign=1)
	//                 + 1 (dataAlign=-8 sleb128 = 0x78) + 1 (raCol=16) + 3 nops
	//                 = 12 bytes
	b := []byte{
		0x0c, 0x00, 0x00, 0x00, // length = 12
		0x00, 0x00, 0x00, 0x00, // CIE_id
		0x01,             // version
		0x00,             // augmentation = ""
		0x01,             // codeAlign
		0x78,             // dataAlign = -8
		0x10,             // raColumn = 16
		0x00, 0x00, 0x00, // nops
	}
	c, err := parseCIE(b, 0)
	require.NoError(t, err)
	assert.Equal(t, "", c.augmentation)
	assert.Len(t, c.initialInstructions, 3)
}

func TestWalkEHFrame_EmptyBuffer(t *testing.T) {
	// Zero-byte section is trivially valid (immediate EOF).
	err := walkEHFrame(nil, 0, func(off uint64, c *cie, f *fde) error {
		t.Errorf("callback invoked on empty buffer")
		return nil
	})
	require.NoError(t, err)
}

func TestWalkEHFrame_ZeroSentinel(t *testing.T) {
	// Length=0 record terminates the section. Nothing parsed.
	section := []byte{0x00, 0x00, 0x00, 0x00}
	var called bool
	err := walkEHFrame(section, 0, func(off uint64, c *cie, f *fde) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.False(t, called)
}

func TestWalkEHFrame_Truncated(t *testing.T) {
	// Record claims 0x10 bytes but buffer is only 8.
	section := []byte{0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	err := walkEHFrame(section, 0, func(off uint64, c *cie, f *fde) error { return nil })
	require.Error(t, err)
}

// --- CIE/FDE parse benchmarks ---------------------------------------------

func BenchmarkParseCIE(b *testing.B) {
	raw := sampleCIEx86()
	for b.Loop() {
		_, _ = parseCIE(raw, 0)
	}
}

func BenchmarkParseFDE(b *testing.B) {
	cieRaw := sampleCIEx86()
	c, _ := parseCIE(cieRaw, 0)
	fdeRaw := sampleFDE(0, uint64(len(cieRaw)))
	for b.Loop() {
		_, _ = parseFDE(fdeRaw, uint64(len(cieRaw)), c)
	}
}

func BenchmarkWalkEHFrame(b *testing.B) {
	cieRaw := sampleCIEx86()
	fdeRaw := sampleFDE(0, uint64(len(cieRaw)))
	section := append(append([]byte{}, cieRaw...), fdeRaw...)
	for b.Loop() {
		_ = walkEHFrame(section, 0, func(off uint64, c *cie, f *fde) error { return nil })
	}
}
