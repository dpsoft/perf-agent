package ehcompile

import (
	"encoding/binary"
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

// DWARF exception-handling pointer encoding byte.
// Low nibble = data format; bits 4-6 = relativity; bit 7 = indirect.
// (Bit 7 we don't support — real binaries don't use it for FDE pointers.)
const (
	dwEhPEAbsptr = 0x00
	dwEhPEOmit   = 0xff

	dwEhPEUleb128 = 0x01
	dwEhPEUdata2  = 0x02
	dwEhPEUdata4  = 0x03
	dwEhPEUdata8  = 0x04
	dwEhPESleb128 = 0x09
	dwEhPESdata2  = 0x0a
	dwEhPESdata4  = 0x0b
	dwEhPESdata8  = 0x0c

	dwEhPEPcrel   = 0x10
	dwEhPETextrel = 0x20
	dwEhPEDatarel = 0x30
	dwEhPEFuncrel = 0x40
	dwEhPEAligned = 0x50
)

// decodeEHPointer reads a DWARF EH pointer from b using encoding byte enc.
// pcPos = absolute address of b[0] (for DW_EH_PE_pcrel).
// dataBase = base for DW_EH_PE_datarel (typically address of .eh_frame_hdr).
// Returns the resolved address and bytes consumed.
//
// DW_EH_PE_omit (0xff) returns (0, 0, nil).
func decodeEHPointer(b []byte, enc byte, pcPos uint64, dataBase uint64) (uint64, int, error) {
	if enc == dwEhPEOmit {
		return 0, 0, nil
	}
	format := enc & 0x0f
	rel := enc & 0x70

	var raw int64
	var n int
	switch format {
	case dwEhPEUleb128:
		u, cn, err := decodeULEB128(b)
		if err != nil {
			return 0, 0, err
		}
		raw = int64(u)
		n = cn
	case dwEhPEUdata2:
		if len(b) < 2 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint16(b))
		n = 2
	case dwEhPEUdata4:
		if len(b) < 4 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint32(b))
		n = 4
	case dwEhPEUdata8:
		if len(b) < 8 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint64(b))
		n = 8
	case dwEhPESleb128:
		s, cn, err := decodeSLEB128(b)
		if err != nil {
			return 0, 0, err
		}
		raw = s
		n = cn
	case dwEhPESdata2:
		if len(b) < 2 {
			return 0, 0, errTruncated
		}
		raw = int64(int16(binary.LittleEndian.Uint16(b)))
		n = 2
	case dwEhPESdata4:
		if len(b) < 4 {
			return 0, 0, errTruncated
		}
		raw = int64(int32(binary.LittleEndian.Uint32(b)))
		n = 4
	case dwEhPESdata8:
		if len(b) < 8 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint64(b))
		n = 8
	default:
		return 0, 0, errors.New("ehcompile: unknown DW_EH_PE format")
	}

	var base int64
	switch rel {
	case dwEhPEAbsptr:
		base = 0
	case dwEhPEPcrel:
		base = int64(pcPos)
	case dwEhPEDatarel:
		base = int64(dataBase)
	default:
		return 0, 0, errors.New("ehcompile: unsupported DW_EH_PE relativity")
	}

	return uint64(base + raw), n, nil
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
