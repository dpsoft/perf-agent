package debuginfod

import (
	"encoding/binary"
	"os"
)

// writeELFWithSection writes an ELF64 file at path containing a single
// named SHT_PROGBITS section with the given data plus a .shstrtab.
// Used to build small fixtures for classifier/resolution tests.
//
// Minimal layout:
//
//	0..63    ELF64 header
//	64..     <data> (the named section)
//	...      <.shstrtab> (NUL + section name + NUL + ".shstrtab" + NUL)
//	...      section header table: NULL section, named section, .shstrtab
func writeELFWithSection(path, sectionName string, data []byte) error {
	const (
		ehSize = 64
		shSize = 64
	)

	// Layout:
	//   ELF header
	//   data section bytes
	//   shstrtab bytes (NUL + sectionName + NUL + ".shstrtab" + NUL)
	//   section header table (3 entries: NULL, named, shstrtab)

	dataOff := uint64(ehSize)
	dataEnd := dataOff + uint64(len(data))

	shstrtab := []byte{0}
	shstrtab = append(shstrtab, []byte(sectionName)...)
	shstrtab = append(shstrtab, 0)
	nameOffShstrtab := uint32(len(shstrtab))
	shstrtab = append(shstrtab, []byte(".shstrtab")...)
	shstrtab = append(shstrtab, 0)

	shstrtabOff := dataEnd
	shstrtabEnd := shstrtabOff + uint64(len(shstrtab))

	shOff := shstrtabEnd
	totalSize := shOff + uint64(3*shSize)

	buf := make([]byte, totalSize)

	// ELF header
	copy(buf[:4], []byte{0x7f, 'E', 'L', 'F'})
	buf[4] = 2 // ELFCLASS64
	buf[5] = 1 // ELFDATA2LSB
	buf[6] = 1 // EV_CURRENT
	binary.LittleEndian.PutUint16(buf[16:], 2)                       // e_type = ET_EXEC
	binary.LittleEndian.PutUint16(buf[18:], 62)                      // e_machine = EM_X86_64
	binary.LittleEndian.PutUint32(buf[20:], 1)                       // e_version
	binary.LittleEndian.PutUint64(buf[40:], shOff)                   // e_shoff
	binary.LittleEndian.PutUint16(buf[52:], ehSize)                  // e_ehsize
	binary.LittleEndian.PutUint16(buf[58:], shSize)                  // e_shentsize
	binary.LittleEndian.PutUint16(buf[60:], 3)                       // e_shnum (NULL + named + shstrtab)
	binary.LittleEndian.PutUint16(buf[62:], 2)                       // e_shstrndx (.shstrtab is index 2)

	// Data section payload
	copy(buf[dataOff:], data)

	// .shstrtab payload
	copy(buf[shstrtabOff:], shstrtab)

	// SHT layout per elf(5) Section64:
	//   [0:4]   sh_name
	//   [4:8]   sh_type
	//   [8:16]  sh_flags
	//   [16:24] sh_addr
	//   [24:32] sh_offset
	//   [32:40] sh_size
	//   [40:44] sh_link
	//   [44:48] sh_info
	//   [48:56] sh_addralign
	//   [56:64] sh_entsize

	// Section 0: NULL (all zero)

	// Section 1: named SHT_PROGBITS
	sh1 := buf[shOff+shSize:]
	binary.LittleEndian.PutUint32(sh1[0:], 1)                    // sh_name = offset 1 in shstrtab (after leading NUL)
	binary.LittleEndian.PutUint32(sh1[4:], 1)                    // sh_type = SHT_PROGBITS
	binary.LittleEndian.PutUint64(sh1[24:], dataOff)             // sh_offset
	binary.LittleEndian.PutUint64(sh1[32:], uint64(len(data)))   // sh_size

	// Section 2: .shstrtab
	sh2 := buf[shOff+2*shSize:]
	binary.LittleEndian.PutUint32(sh2[0:], nameOffShstrtab)             // sh_name = offset of ".shstrtab" in shstrtab
	binary.LittleEndian.PutUint32(sh2[4:], 3)                           // sh_type = SHT_STRTAB
	binary.LittleEndian.PutUint64(sh2[24:], shstrtabOff)                // sh_offset
	binary.LittleEndian.PutUint64(sh2[32:], uint64(len(shstrtab)))      // sh_size

	return os.WriteFile(path, buf, 0o644)
}
