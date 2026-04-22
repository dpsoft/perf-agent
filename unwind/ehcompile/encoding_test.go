package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeULEB128(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		want     uint64
		consumed int
	}{
		{"single byte zero", []byte{0x00}, 0, 1},
		{"single byte 0x7f", []byte{0x7f}, 127, 1},
		{"two bytes 128", []byte{0x80, 0x01}, 128, 2},
		{"three bytes 16384", []byte{0x80, 0x80, 0x01}, 16384, 3},
		{"DWARF spec example 624485", []byte{0xe5, 0x8e, 0x26}, 624485, 3},
		{"max uint64", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, 0xFFFFFFFFFFFFFFFF, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n, err := decodeULEB128(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.consumed, n)
		})
	}
}

func TestDecodeULEB128_Truncated(t *testing.T) {
	_, _, err := decodeULEB128([]byte{0x80, 0x80, 0x80})
	require.Error(t, err)
}

func TestDecodeSLEB128(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		want     int64
		consumed int
	}{
		{"zero", []byte{0x00}, 0, 1},
		{"positive small", []byte{0x02}, 2, 1},
		{"negative small (-2)", []byte{0x7e}, -2, 1},
		{"positive 127", []byte{0xff, 0x00}, 127, 2},
		{"negative 128", []byte{0x80, 0x7f}, -128, 2},
		{"DWARF spec 129", []byte{0x81, 0x01}, 129, 2},
		{"DWARF spec -129", []byte{0xff, 0x7e}, -129, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n, err := decodeSLEB128(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.consumed, n)
		})
	}
}
