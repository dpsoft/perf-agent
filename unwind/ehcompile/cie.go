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

type fde struct {
	cie             *cie
	initialLocation uint64
	addressRange    uint64
	instructions    []byte
}

// parseFDE reads an FDE starting at raw[0]. filePos is raw[0]'s absolute
// file offset. Caller is responsible for locating the matching CIE.
func parseFDE(raw []byte, filePos uint64, c *cie) (*fde, error) {
	if len(raw) < 8 {
		return nil, errTruncated
	}
	length := binary.LittleEndian.Uint32(raw[:4])
	if length == 0xFFFFFFFF || length == 0 {
		return nil, errors.New("ehcompile: unsupported FDE length")
	}
	body := raw[4 : 4+length]
	pos := 4 // skip CIE pointer (caller already resolved it)

	encPos := filePos + 8 // position of initial_location in absolute terms
	initLoc, n, err := decodeEHPointer(body[pos:], c.fdePointerEnc, encPos, 0)
	if err != nil {
		return nil, err
	}
	pos += n

	// address_range uses the same data format as initial_location but no
	// relativity.
	rangeEnc := c.fdePointerEnc & 0x0f
	addrRange, n, err := decodeEHPointer(body[pos:], rangeEnc, 0, 0)
	if err != nil {
		return nil, err
	}
	pos += n

	if len(c.augmentation) > 0 && c.augmentation[0] == 'z' {
		augLen, n, err := decodeULEB128(body[pos:])
		if err != nil {
			return nil, err
		}
		pos += n + int(augLen)
	}

	return &fde{
		cie:             c,
		initialLocation: initLoc,
		addressRange:    addrRange,
		instructions:    body[pos:],
	}, nil
}

// walkEHFrame iterates CIE/FDE records in an .eh_frame section.
// sectionPos is section[0]'s absolute file offset. The callback receives
// exactly one of (c, f) non-nil per invocation; c is passed BEFORE any
// of its FDEs.
func walkEHFrame(section []byte, sectionPos uint64, cb func(off uint64, c *cie, f *fde) error) error {
	cies := make(map[uint64]*cie)

	var pos uint64
	for int(pos)+4 <= len(section) {
		length := binary.LittleEndian.Uint32(section[pos : pos+4])
		if length == 0 {
			return nil // EOF sentinel
		}
		if length == 0xFFFFFFFF {
			return errors.New("ehcompile: 64-bit .eh_frame not supported")
		}
		recordEnd := pos + 4 + uint64(length)
		if recordEnd > uint64(len(section)) {
			return errTruncated
		}
		secondWord := binary.LittleEndian.Uint32(section[pos+4 : pos+8])

		if secondWord == 0 {
			c, err := parseCIE(section[pos:recordEnd], sectionPos+pos)
			if err != nil {
				return fmt.Errorf("CIE at +%#x: %w", pos, err)
			}
			cies[pos] = c
			if err := cb(pos, c, nil); err != nil {
				return err
			}
		} else {
			cieOff := pos + 4 - uint64(secondWord)
			c, ok := cies[cieOff]
			if !ok {
				return fmt.Errorf("FDE at +%#x references unknown CIE at +%#x", pos, cieOff)
			}
			f, err := parseFDE(section[pos:recordEnd], sectionPos+pos, c)
			if err != nil {
				return fmt.Errorf("FDE at +%#x: %w", pos, err)
			}
			if err := cb(pos, nil, f); err != nil {
				return err
			}
		}
		pos = recordEnd
	}
	return nil
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
