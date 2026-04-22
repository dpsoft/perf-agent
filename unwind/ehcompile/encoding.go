package ehcompile

import (
	"errors"
)

var errTruncated = errors.New("ehcompile: truncated input")

// decodeULEB128 reads one ULEB128-encoded unsigned integer from b.
// Returns the value and the number of bytes consumed.
func decodeULEB128(b []byte) (uint64, int, error) {
	var result uint64
	var shift uint
	for i, by := range b {
		if i >= 10 {
			return 0, 0, errors.New("ehcompile: ULEB128 too long")
		}
		result |= uint64(by&0x7f) << shift
		if by&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, errTruncated
}

// decodeSLEB128 reads one SLEB128-encoded signed integer from b.
func decodeSLEB128(b []byte) (int64, int, error) {
	var result int64
	var shift uint
	for i, by := range b {
		if i >= 10 {
			return 0, 0, errors.New("ehcompile: SLEB128 too long")
		}
		result |= int64(by&0x7f) << shift
		shift += 7
		if by&0x80 == 0 {
			if shift < 64 && by&0x40 != 0 {
				result |= -(int64(1) << shift)
			}
			return result, i + 1, nil
		}
	}
	return 0, 0, errTruncated
}
