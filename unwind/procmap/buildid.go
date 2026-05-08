package procmap

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const (
	nt_GNU_BUILD_ID = 3
	gnu_name        = "GNU\x00"
)

// ReadBuildID returns the GNU build-id of the ELF at path as a
// lowercase hex string. Returns an empty string (with nil error) when
// the ELF is valid but has no .note.gnu.build-id note, and an error
// when the file can't be opened or isn't ELF.
func ReadBuildID(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	sec := f.Section(".note.gnu.build-id")
	if sec == nil {
		return "", nil
	}

	data, err := sec.Data()
	if err != nil {
		return "", fmt.Errorf("read section: %w", err)
	}

	id, err := parseBuildIDNote(data, f.ByteOrder)
	if err != nil {
		return "", err
	}
	return id, nil
}

// parseBuildIDNote walks the ELF note records in data and returns the
// desc of the first NT_GNU_BUILD_ID note whose name is "GNU\0".
// Empty string with nil error means no matching note was found.
func parseBuildIDNote(data []byte, bo binary.ByteOrder) (string, error) {
	for len(data) > 0 {
		if len(data) < 12 {
			return "", fmt.Errorf("note header truncated")
		}
		namesz := bo.Uint32(data[0:4])
		descsz := bo.Uint32(data[4:8])
		typ := bo.Uint32(data[8:12])
		data = data[12:]

		nameEnd := int(alignUp(namesz, 4))
		if nameEnd > len(data) {
			return "", fmt.Errorf("note name truncated")
		}
		name := data[:namesz]
		data = data[nameEnd:]

		descEnd := int(alignUp(descsz, 4))
		if descEnd > len(data) {
			return "", fmt.Errorf("note desc truncated")
		}
		desc := data[:descsz]
		data = data[descEnd:]

		if typ == nt_GNU_BUILD_ID && bytes.Equal(name, []byte(gnu_name)) {
			return hex.EncodeToString(desc), nil
		}
	}
	return "", nil
}

func alignUp(n, a uint32) uint32 { return (n + a - 1) &^ (a - 1) }
