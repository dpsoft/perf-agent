package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchX86_64(t *testing.T) {
	a := archX86_64()
	assert.Equal(t, uint8(x86RSP), a.spReg)
	assert.Equal(t, uint8(x86RBP), a.fpReg)
	assert.Equal(t, uint8(x86RIP), a.raReg)
	assert.Equal(t, "x86_64", a.name)
}

func TestArchARM64(t *testing.T) {
	a := archARM64()
	assert.Equal(t, uint8(arm64SP), a.spReg)
	assert.Equal(t, uint8(arm64X29), a.fpReg)
	assert.Equal(t, uint8(arm64X30), a.raReg)
	assert.Equal(t, "arm64", a.name)
}

func TestCFATypeFromReg(t *testing.T) {
	a := archX86_64()
	assert.Equal(t, CFATypeSP, a.cfaTypeFor(x86RSP))
	assert.Equal(t, CFATypeFP, a.cfaTypeFor(x86RBP))
	assert.Equal(t, CFATypeUndefined, a.cfaTypeFor(x86R8))
}

func TestArchFromELFMachine(t *testing.T) {
	a, err := archFromELFMachine(62) // EM_X86_64
	require.NoError(t, err)
	assert.Equal(t, "x86_64", a.name)

	a, err = archFromELFMachine(183) // EM_AARCH64
	require.NoError(t, err)
	assert.Equal(t, "arm64", a.name)

	_, err = archFromELFMachine(8) // EM_MIPS
	require.Error(t, err)
}
