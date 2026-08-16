package debuginfod

import (
	"debug/elf"
	"encoding/binary"
	"os"
)

// alignUp rounds n up to the nearest multiple of a (a must be a power of 2).
func alignUp(n, a uint32) uint32 { return (n + a - 1) &^ (a - 1) }

// writeELFWithNoteGnuBuildID writes a minimal ELF64 LE file at path with a
// PT_NOTE segment + SHT_NOTE section carrying NT_GNU_BUILD_ID. It has no
// .debug_info, no .gnu_debuglink, no .symtab.
//
// Mirrors unwind/procmap/elfhelpers_test.go::writeELFWithBuildID, which sits
// in a different package and so isn't importable from these _test.go files.
func writeELFWithNoteGnuBuildID(path string, buildID []byte) error {
	const (
		ehsz = 64 // ELF header
		phsz = 56 // program header entry (Prog64)
		shsz = 64 // section header entry (Section64)
	)

	bo := binary.LittleEndian

	// NT_GNU_BUILD_ID note payload.
	const gnuNameSz = 4 // "GNU\0"
	descSz := uint32(len(buildID))
	noteSz := 12 + int(alignUp(gnuNameSz, 4)) + int(alignUp(descSz, 4))
	note := make([]byte, noteSz)
	bo.PutUint32(note[0:4], gnuNameSz)
	bo.PutUint32(note[4:8], descSz)
	bo.PutUint32(note[8:12], 3) // NT_GNU_BUILD_ID
	copy(note[12:], "GNU\x00")
	copy(note[12+alignUp(gnuNameSz, 4):], buildID)

	// shstrtab: \0  .note.gnu.build-id\0  .shstrtab\0
	shstrtab := []byte{0x00}
	noteNameOff := uint32(len(shstrtab))
	shstrtab = append(shstrtab, ".note.gnu.build-id\x00"...)
	shstrNameOff := uint32(len(shstrtab))
	shstrtab = append(shstrtab, ".shstrtab\x00"...)

	// File layout:
	//   [0..ehsz)              ELF header
	//   [ehsz..ehsz+phsz)     PT_NOTE phdr
	//   [noteOff..shdrsOff)   note payload
	//   [shdrsOff..)          3 × 64-byte section headers (null, note, shstrtab)
	//   [shstrDataOff..)      shstrtab bytes
	noteOff := uint64(ehsz + phsz)
	shdrsOff := noteOff + uint64(noteSz)
	shstrDataOff := shdrsOff + 3*shsz
	total := shstrDataOff + uint64(len(shstrtab))

	buf := make([]byte, total)

	// ── ELF header ────────────────────────────────────────────────────────────
	copy(buf[0:4], elf.ELFMAG)
	buf[4] = byte(elf.ELFCLASS64)
	buf[5] = byte(elf.ELFDATA2LSB)
	buf[6] = byte(elf.EV_CURRENT)
	buf[7] = byte(elf.ELFOSABI_NONE)
	bo.PutUint16(buf[16:18], uint16(elf.ET_EXEC))
	bo.PutUint16(buf[18:20], uint16(elf.EM_X86_64))
	bo.PutUint32(buf[20:24], uint32(elf.EV_CURRENT))
	// e_entry [24:32] = 0
	bo.PutUint64(buf[32:40], uint64(ehsz)) // e_phoff
	bo.PutUint64(buf[40:48], shdrsOff)     // e_shoff
	// e_flags [48:52] = 0
	bo.PutUint16(buf[52:54], ehsz) // e_ehsize
	bo.PutUint16(buf[54:56], phsz) // e_phentsize
	bo.PutUint16(buf[56:58], 1)    // e_phnum
	bo.PutUint16(buf[58:60], shsz) // e_shentsize
	bo.PutUint16(buf[60:62], 3)    // e_shnum: null, note, shstrtab
	bo.PutUint16(buf[62:64], 2)    // e_shstrndx: section 2 = .shstrtab

	// ── PT_NOTE phdr ──────────────────────────────────────────────────────────
	ph := buf[ehsz : ehsz+phsz]
	bo.PutUint32(ph[0:4], uint32(elf.PT_NOTE))
	bo.PutUint32(ph[4:8], uint32(elf.PF_R))
	bo.PutUint64(ph[8:16], noteOff)         // p_offset
	bo.PutUint64(ph[16:24], noteOff)        // p_vaddr
	bo.PutUint64(ph[24:32], noteOff)        // p_paddr
	bo.PutUint64(ph[32:40], uint64(noteSz)) // p_filesz
	bo.PutUint64(ph[40:48], uint64(noteSz)) // p_memsz
	bo.PutUint64(ph[48:56], 4)              // p_align

	// ── note payload ──────────────────────────────────────────────────────────
	copy(buf[noteOff:], note)

	// ── Section headers ───────────────────────────────────────────────────────
	// Section 0: null — all zeros (already).

	// Section 1: .note.gnu.build-id
	s1 := buf[shdrsOff+shsz : shdrsOff+2*shsz]
	bo.PutUint32(s1[0:4], noteNameOff)          // sh_name
	bo.PutUint32(s1[4:8], uint32(elf.SHT_NOTE)) // sh_type
	bo.PutUint64(s1[24:32], noteOff)            // sh_offset
	bo.PutUint64(s1[32:40], uint64(noteSz))     // sh_size
	bo.PutUint64(s1[48:56], 4)                  // sh_addralign

	// Section 2: .shstrtab
	s2 := buf[shdrsOff+2*shsz : shdrsOff+3*shsz]
	bo.PutUint32(s2[0:4], shstrNameOff)            // sh_name
	bo.PutUint32(s2[4:8], uint32(elf.SHT_STRTAB))  // sh_type
	bo.PutUint64(s2[24:32], shstrDataOff)          // sh_offset
	bo.PutUint64(s2[32:40], uint64(len(shstrtab))) // sh_size
	bo.PutUint64(s2[48:56], 1)                     // sh_addralign

	// ── shstrtab data ─────────────────────────────────────────────────────────
	copy(buf[shstrDataOff:], shstrtab)

	return os.WriteFile(path, buf, 0o644)
}

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
