package ehcompile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

type cie struct {
	version             byte
	augmentation        string
	codeAlign           uint64
	dataAlign           int64
	raColumn            uint64
	fdePointerEnc       byte
	hasSignalFrame      bool
	initialInstructions []byte
}

// parseCIE reads a CIE from raw[0:]. filePos is raw[0]'s absolute file
// offset, used to resolve augmentation-data pcrel pointers (which we
// skip — we just need to advance past them).
func parseCIE(raw []byte, filePos uint64) (*cie, error) {
	if len(raw) < 4 {
		return nil, errTruncated
	}
	length := binary.LittleEndian.Uint32(raw[:4])
	if length == 0xFFFFFFFF {
		return nil, errors.New("ehcompile: 64-bit .eh_frame not supported")
	}
	if length == 0 {
		return nil, errors.New("ehcompile: zero-length CIE (EOF sentinel)")
	}
	body := raw[4 : 4+length]
	if len(body) < 9 {
		return nil, errTruncated
	}
	if binary.LittleEndian.Uint32(body[:4]) != 0 {
		return nil, errors.New("ehcompile: non-zero CIE_id")
	}
	pos := 4
	version := body[pos]
	pos++
	if version != 1 && version != 3 {
		return nil, fmt.Errorf("ehcompile: unsupported CIE version %d", version)
	}

	nul := bytes.IndexByte(body[pos:], 0)
	if nul < 0 {
		return nil, errTruncated
	}
	augmentation := string(body[pos : pos+nul])
	pos += nul + 1

	codeAlign, n, err := decodeULEB128(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n

	dataAlign, n, err := decodeSLEB128(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n

	raCol, n, err := decodeULEB128(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n

	c := &cie{
		version:      version,
		augmentation: augmentation,
		codeAlign:    codeAlign,
		dataAlign:    dataAlign,
		raColumn:     raCol,
	}

	if len(augmentation) > 0 && augmentation[0] == 'z' {
		augLen, n, err := decodeULEB128(body[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		augData := body[pos : pos+int(augLen)]
		pos += int(augLen)
		if err := c.parseAugmentationData(augmentation[1:], augData); err != nil {
			return nil, err
		}
	} else if augmentation == "" {
		// no augmentation data
	} else if augmentation == "eh" {
		return nil, errors.New("ehcompile: legacy 'eh' augmentation not supported")
	} else {
		return nil, fmt.Errorf("ehcompile: unknown augmentation %q", augmentation)
	}

	c.initialInstructions = body[pos:]
	return c, nil
}

func (c *cie) parseAugmentationData(augChars string, data []byte) error {
	pos := 0
	for _, ch := range augChars {
		switch ch {
		case 'R':
			if pos >= len(data) {
				return errTruncated
			}
			c.fdePointerEnc = data[pos]
			pos++
		case 'S':
			c.hasSignalFrame = true
		case 'P':
			if pos >= len(data) {
				return errTruncated
			}
			enc := data[pos]
			pos++
			_, n, err := decodeEHPointer(data[pos:], enc, 0, 0)
			if err != nil {
				return err
			}
			pos += n
		case 'L':
			if pos >= len(data) {
				return errTruncated
			}
			pos++
		case 'B':
			// arm64 pointer auth — no operand in CIE aug data.
		default:
			return fmt.Errorf("ehcompile: unknown augmentation char %c", ch)
		}
	}
	return nil
}
