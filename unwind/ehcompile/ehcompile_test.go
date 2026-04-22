package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompile_NotImplemented(t *testing.T) {
	_, _, err := Compile("/dev/null")
	require.Error(t, err)
}
