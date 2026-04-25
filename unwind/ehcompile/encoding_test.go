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

// --- Additional encoding edge cases ---------------------------------------

func TestDecodeSLEB128_Truncated(t *testing.T) {
	_, _, err := decodeSLEB128([]byte{0x80, 0x80, 0x80})
	require.Error(t, err)
}

func TestDecodeSLEB128_MaxInt64(t *testing.T) {
	// 0x7fffffffffffffff = 10-byte sleb128 encoding.
	b := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00}
	got, n, err := decodeSLEB128(b)
	require.NoError(t, err)
	assert.Equal(t, int64(0x7fffffffffffffff), got)
	assert.Equal(t, 10, n)
}

func TestDecodeSLEB128_MinInt64(t *testing.T) {
	// -2^63 = 0x8000000000000000 as two's complement = 10-byte sleb128.
	b := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x7f}
	got, n, err := decodeSLEB128(b)
	require.NoError(t, err)
	assert.Equal(t, int64(-1)<<63, got)
	assert.Equal(t, 10, n)
}

func TestDecodeULEB128_EmptyInput(t *testing.T) {
	_, _, err := decodeULEB128(nil)
	require.Error(t, err)
}

func TestDecodeULEB128_TooLong(t *testing.T) {
	// 11 bytes all continuation — exceeds 10-byte max for uint64.
	b := bytes11()
	_, _, err := decodeULEB128(b)
	require.Error(t, err)
}

// bytes11 returns 11 continuation bytes, to trigger "too long" errors.
func bytes11() []byte {
	b := make([]byte, 11)
	for i := range b {
		b[i] = 0x80
	}
	b[10] = 0x01 // technically terminated, but already past 10-byte limit
	return b
}

func TestDecodeEHPointer_udata8(t *testing.T) {
	b := []byte{0x78, 0x56, 0x34, 0x12, 0xef, 0xcd, 0xab, 0x89}
	v, n, err := decodeEHPointer(b, 0x04, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x89abcdef12345678), v)
	assert.Equal(t, 8, n)
}

func TestDecodeEHPointer_sdata2_negative(t *testing.T) {
	// sdata2 with absolute relativity. -1 = 0xFFFF.
	b := []byte{0xff, 0xff}
	v, n, err := decodeEHPointer(b, 0x0a, 0, 0)
	require.NoError(t, err)
	// Cast int16(-1) to int64 → -1 → uint64 → max uint64.
	assert.Equal(t, ^uint64(0), v)
	assert.Equal(t, 2, n)
}

func TestDecodeEHPointer_uleb128(t *testing.T) {
	// uleb128 absptr. Encoded 128 = {0x80, 0x01}.
	b := []byte{0x80, 0x01}
	v, n, err := decodeEHPointer(b, 0x01, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(128), v)
	assert.Equal(t, 2, n)
}

func TestDecodeEHPointer_Truncated(t *testing.T) {
	// sdata4 with only 3 bytes of input.
	_, _, err := decodeEHPointer([]byte{0x01, 0x02, 0x03}, 0x0b, 0, 0)
	require.Error(t, err)
}

func TestDecodeEHPointer_UnsupportedRelativity(t *testing.T) {
	// textrel (0x20) isn't supported.
	_, _, err := decodeEHPointer([]byte{0x10, 0x00, 0x00, 0x00}, 0x2b, 0, 0)
	require.Error(t, err)
}

func TestDecodeEHPointer_UnknownFormat(t *testing.T) {
	// Low nibble 0x0e isn't a defined encoding.
	_, _, err := decodeEHPointer([]byte{0x00, 0x00, 0x00, 0x00}, 0x0e, 0, 0)
	require.Error(t, err)
}

// --- Micro-benchmarks -----------------------------------------------------

func BenchmarkDecodeULEB128_1byte(b *testing.B) {
	in := []byte{0x7f}
	for b.Loop() {
		_, _, _ = decodeULEB128(in)
	}
}

func BenchmarkDecodeULEB128_3bytes(b *testing.B) {
	in := []byte{0xe5, 0x8e, 0x26} // 624485
	for b.Loop() {
		_, _, _ = decodeULEB128(in)
	}
}

func BenchmarkDecodeULEB128_10bytes(b *testing.B) {
	in := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	for b.Loop() {
		_, _, _ = decodeULEB128(in)
	}
}

func BenchmarkDecodeSLEB128_Negative(b *testing.B) {
	in := []byte{0xff, 0x7e} // -129
	for b.Loop() {
		_, _, _ = decodeSLEB128(in)
	}
}

func BenchmarkDecodeEHPointer_udata4(b *testing.B) {
	in := []byte{0x34, 0x12, 0x00, 0x00}
	for b.Loop() {
		_, _, _ = decodeEHPointer(in, 0x03, 0, 0)
	}
}

func BenchmarkDecodeEHPointer_sdata4_pcrel(b *testing.B) {
	in := []byte{0xf0, 0xff, 0xff, 0xff}
	for b.Loop() {
		_, _, _ = decodeEHPointer(in, 0x1B, 0x1000, 0)
	}
}
