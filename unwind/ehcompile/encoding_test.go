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

func TestDecodeEHPointer_udata4(t *testing.T) {
	b := []byte{0x34, 0x12, 0x00, 0x00}
	v, n, err := decodeEHPointer(b, 0x03, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x1234), v)
	assert.Equal(t, 4, n)
}

func TestDecodeEHPointer_sdata4_pcrel(t *testing.T) {
	b := []byte{0xf0, 0xff, 0xff, 0xff}
	v, n, err := decodeEHPointer(b, 0x1B, 0x1000, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x0FF0), v)
	assert.Equal(t, 4, n)
}

func TestDecodeEHPointer_sdata4_datarel(t *testing.T) {
	b := []byte{0x10, 0x00, 0x00, 0x00}
	v, n, err := decodeEHPointer(b, 0x3B, 0, 0x2000)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x2010), v)
	assert.Equal(t, 4, n)
}

func TestDecodeEHPointer_omit(t *testing.T) {
	v, n, err := decodeEHPointer(nil, 0xff, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), v)
	assert.Equal(t, 0, n)
}
