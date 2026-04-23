package ehmaps

import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadBuildID reads the GNU build-id from an ELF's .note.gnu.build-id.
// Returns the raw bytes (typically 20 for sha1), or an error if absent.
func ReadBuildID(path string) ([]byte, error) {
	ef, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer ef.Close()
	for _, sec := range ef.Sections {
		if sec.Type != elf.SHT_NOTE {
			continue
		}
		data, err := sec.Data()
		if err != nil {
			continue
		}
		if id := extractGNUBuildID(data); id != nil {
			return id, nil
		}
	}
	return nil, errors.New("no .note.gnu.build-id section")
}

// extractGNUBuildID walks an ELF .note section payload looking for the
// type-3 "GNU"-named note. Format: u32 name_size, u32 desc_size, u32 type,
// name (padded to 4), desc (padded to 4).
func extractGNUBuildID(notes []byte) []byte {
	for len(notes) >= 12 {
		nameSize := binary.LittleEndian.Uint32(notes[0:4])
		descSize := binary.LittleEndian.Uint32(notes[4:8])
		noteType := binary.LittleEndian.Uint32(notes[8:12])
		p := 12
		nameEnd := p + int(nameSize)
		namePadded := (nameEnd + 3) &^ 3
		if namePadded > len(notes) {
			return nil
		}
		descEnd := namePadded + int(descSize)
		descPadded := (descEnd + 3) &^ 3
		if descPadded > len(notes) {
			return nil
		}
		if noteType == 3 && nameSize == 4 && string(notes[p:p+3]) == "GNU" {
			return notes[namePadded:descEnd]
		}
		notes = notes[descPadded:]
	}
	return nil
}

// LoadProcessMappings reads /proc/<pid>/maps and returns one PIDMapping
// per executable-mapped range of binPath. The load bias is computed from
// the ELF's executable PT_LOAD — "vma_start - file_offset" is wrong for
// PIE binaries where PT_LOAD vaddr differs from file offset (Rust's
// release output has a 0x1000 hole).
func LoadProcessMappings(pid int, binPath string, tableID uint64) ([]PIDMapping, error) {
	ef, err := elf.Open(binPath)
	if err != nil {
		return nil, err
	}
	defer ef.Close()
	var execProg *elf.Prog
	for _, p := range ef.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
			execProg = p
			break
		}
	}
	if execProg == nil {
		return nil, errors.New("no executable PT_LOAD in ELF")
	}
	const pageMask uint64 = ^uint64(0xfff)
	execVaddrAligned := execProg.Vaddr & pageMask
	execOffsetAligned := execProg.Off & pageMask

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil, err
	}
	base := binPath[strings.LastIndex(binPath, "/")+1:]

	var loadBias uint64
	var haveBias bool
	var out []PIDMapping
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if !strings.Contains(fields[1], "x") {
			continue
		}
		if !strings.HasSuffix(fields[5], base) {
			continue
		}
		addrs := strings.SplitN(fields[0], "-", 2)
		if len(addrs) != 2 {
			continue
		}
		start, err := strconv.ParseUint(addrs[0], 16, 64)
		if err != nil {
			continue
		}
		end, err := strconv.ParseUint(addrs[1], 16, 64)
		if err != nil {
			continue
		}
		offset, err := strconv.ParseUint(fields[2], 16, 64)
		if err != nil {
			continue
		}
		if !haveBias && offset == execOffsetAligned {
			loadBias = start - execVaddrAligned
			haveBias = true
		}
		out = append(out, PIDMapping{
			VMAStart: start,
			VMAEnd:   end,
			TableID:  tableID,
		})
	}
	if !haveBias {
		return nil, errors.New("executable PT_LOAD has no matching /proc/<pid>/maps entry")
	}
	for i := range out {
		out[i].LoadBias = loadBias
	}
	return out, nil
}
