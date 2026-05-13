package procmap

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeELFWithBuildID writes a minimal ELF64 LE file carrying the given
// build-id in a PT_NOTE segment + .note.gnu.build-id section.
// Returns the absolute path of the created file.
func writeELFWithBuildID(t *testing.T, dir string, buildID []byte) string {
	t.Helper()

	// ELF64 Section header byte offsets (each field in the 64-byte record):
	//   [0:4]   sh_name
	//   [4:8]   sh_type
	//   [8:16]  sh_flags
	//   [16:24] sh_addr
	//   [24:32] sh_offset (file offset of section data)
	//   [32:40] sh_size
	//   [40:44] sh_link
	//   [44:48] sh_info
	//   [48:56] sh_addralign
	//   [56:64] sh_entsize

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
	// sh_flags [8:16] = 0
	// sh_addr  [16:24] = 0
	bo.PutUint64(s1[24:32], noteOff)        // sh_offset
	bo.PutUint64(s1[32:40], uint64(noteSz)) // sh_size
	// sh_link [40:44] = 0, sh_info [44:48] = 0
	bo.PutUint64(s1[48:56], 4) // sh_addralign

	// Section 2: .shstrtab
	s2 := buf[shdrsOff+2*shsz : shdrsOff+3*shsz]
	bo.PutUint32(s2[0:4], shstrNameOff)           // sh_name
	bo.PutUint32(s2[4:8], uint32(elf.SHT_STRTAB)) // sh_type
	// sh_flags, sh_addr = 0
	bo.PutUint64(s2[24:32], shstrDataOff)          // sh_offset
	bo.PutUint64(s2[32:40], uint64(len(shstrtab))) // sh_size
	bo.PutUint64(s2[48:56], 1)                     // sh_addralign

	// ── shstrtab data ─────────────────────────────────────────────────────────
	copy(buf[shstrDataOff:], shstrtab)

	p := filepath.Join(dir, "fake.elf")
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("writeELFWithBuildID: %v", err)
	}
	return p
}
