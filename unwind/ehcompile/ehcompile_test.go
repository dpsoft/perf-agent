package ehcompile

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompile_NotImplemented(t *testing.T) {
	_, _, err := Compile("/dev/null")
	require.Error(t, err)
}

func TestCompile_SystemBinary(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skip("/bin/true not found")
	}
	entries, classes, err := Compile("/bin/true")
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	assert.NotEmpty(t, classes)
	for i := 1; i < len(entries); i++ {
		assert.LessOrEqual(t, entries[i-1].PCStart, entries[i].PCStart,
			"entry %d out of order", i)
	}
}
