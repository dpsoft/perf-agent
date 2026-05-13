package procmap

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeMinimalELF emits an ELF64 with a single executable PT_LOAD segment
// covering [off, off+filesz) in the file and mapping to virtual address vaddr.
func writeMinimalELF(t *testing.T, dir string, off, vaddr, filesz uint64) string {
	t.Helper()
	path := filepath.Join(dir, "exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	const ehSize = 64
	const phSize = 56
	phOff := uint64(ehSize)

	var eh [ehSize]byte
	copy(eh[:4], []byte{0x7f, 'E', 'L', 'F'})
	eh[4] = 2 // ELFCLASS64
	eh[5] = 1 // ELFDATA2LSB
	eh[6] = 1 // EV_CURRENT
	binary.LittleEndian.PutUint16(eh[16:], 2)      // e_type = ET_EXEC
	binary.LittleEndian.PutUint16(eh[18:], 62)     // e_machine = EM_X86_64
	binary.LittleEndian.PutUint32(eh[20:], 1)      // e_version
	binary.LittleEndian.PutUint64(eh[32:], phOff)  // e_phoff
	binary.LittleEndian.PutUint16(eh[52:], ehSize) // e_ehsize
	binary.LittleEndian.PutUint16(eh[54:], phSize) // e_phentsize
	binary.LittleEndian.PutUint16(eh[56:], 1)      // e_phnum
	if _, err := f.Write(eh[:]); err != nil {
		t.Fatal(err)
	}

	var ph [phSize]byte
	binary.LittleEndian.PutUint32(ph[0:], 1)       // p_type = PT_LOAD
	binary.LittleEndian.PutUint32(ph[4:], 5)       // p_flags = PF_R|PF_X
	binary.LittleEndian.PutUint64(ph[8:], off)     // p_offset
	binary.LittleEndian.PutUint64(ph[16:], vaddr)  // p_vaddr
	binary.LittleEndian.PutUint64(ph[24:], vaddr)  // p_paddr
	binary.LittleEndian.PutUint64(ph[32:], filesz) // p_filesz
	binary.LittleEndian.PutUint64(ph[40:], filesz) // p_memsz
	binary.LittleEndian.PutUint64(ph[48:], 0x1000) // p_align
	if _, err := f.Write(ph[:]); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestAddressMapperBasicLookup(t *testing.T) {
	tmp := t.TempDir()
	// PT_LOAD: off=0x1000, vaddr=0x400000, filesz=0x2000
	path := writeMinimalELF(t, tmp, 0x1000, 0x400000, 0x2000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}

	tests := []struct {
		name string
		off  uint64
		want uint64
		ok   bool
	}{
		{"first byte of segment", 0x1000, 0x400000, true},
		{"middle of segment", 0x1500, 0x400500, true},
		{"last byte of segment", 0x1000 + 0x1fff, 0x400000 + 0x1fff, true},
		{"before any segment", 0x0500, 0, false},
		{"past end of segment", 0x1000 + 0x2000, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.FileOffsetToVirtualAddress(tc.off)
			if ok != tc.ok || got != tc.want {
				t.Errorf("FileOffsetToVirtualAddress(%#x) = (%#x, %v), want (%#x, %v)",
					tc.off, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAddressMapperPageAlignment(t *testing.T) {
	tmp := t.TempDir()
	// Kernel mmap aligns p_offset DOWN to page boundary. With p_offset=0x1234
	// and a 4KB page, the segment effectively starts at file-offset 0x1000.
	// Without the page-align trick, a probe at 0x1000 would fall outside the
	// declared range; with the trick, it routes to this segment.
	path := writeMinimalELF(t, tmp, 0x1234, 0x400000, 0x2000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}
	// page-aligned start of the segment is 0x1000 (0x1234 &^ 0xfff)
	got, ok := m.FileOffsetToVirtualAddress(0x1000)
	if !ok {
		t.Fatalf("FileOffsetToVirtualAddress(0x1000) ok=false, want true (page-align should include this offset)")
	}
	// The mapping from off=0x1000 (page-aligned start) follows the same
	// arithmetic OTel uses: vaddr + (off - off_aligned). When the requested
	// offset equals off_aligned, the returned VA equals vaddr.
	if got != 0x400000 {
		t.Errorf("FileOffsetToVirtualAddress(0x1000) = %#x, want %#x", got, 0x400000)
	}
}

func TestAddressMapperZeroOffset(t *testing.T) {
	tmp := t.TempDir()
	// p_offset=0 is the common case for single-segment static binaries and PIE
	// text segments. The page-align math (0 &^ 0xfff == 0) handles it correctly;
	// this test makes that coverage explicit.
	path := writeMinimalELF(t, tmp, 0x0, 0x400000, 0x1000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}

	tests := []struct {
		name string
		off  uint64
		want uint64
		ok   bool
	}{
		{"start of segment", 0x0, 0x400000, true},
		{"middle of segment", 0x500, 0x400500, true},
		{"past end of segment", 0x1000, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.FileOffsetToVirtualAddress(tc.off)
			if ok != tc.ok || got != tc.want {
				t.Errorf("FileOffsetToVirtualAddress(%#x) = (%#x, %v), want (%#x, %v)",
					tc.off, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Multi-PT_LOAD: many real binaries (especially PIE ones) have multiple
// executable segments. The mapper must dispatch each offset to the
// correct one and report "gap" offsets as unmapped.
func TestAddressMapperMultiPTLOAD(t *testing.T) {
	tmp := t.TempDir()
	// Two executable PT_LOADs:
	//   1: off=0x1000, vaddr=0x400000, filesz=0x800
	//   2: off=0x4000, vaddr=0x600000, filesz=0x800
	path := writeELFTwoSegments(t, tmp,
		ptLoad{Off: 0x1000, Vaddr: 0x400000, Filesz: 0x800},
		ptLoad{Off: 0x4000, Vaddr: 0x600000, Filesz: 0x800},
	)
	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper: %v", err)
	}

	cases := []struct {
		off  uint64
		want uint64
		ok   bool
	}{
		{0x1000, 0x400000, true}, // first segment start
		{0x17ff, 0x4007ff, true}, // first segment end
		{0x2000, 0, false},       // in the gap between segments
		{0x4000, 0x600000, true}, // second segment start
		{0x47ff, 0x6007ff, true}, // second segment end
		{0x4800, 0, false},       // past second segment
	}
	for _, tc := range cases {
		got, ok := m.FileOffsetToVirtualAddress(tc.off)
		if ok != tc.ok || got != tc.want {
			t.Errorf("off=%#x: got (%#x, %v), want (%#x, %v)",
				tc.off, got, ok, tc.want, tc.ok)
		}
	}
}

// PIE (ET_DYN) binaries get loaded at random addresses; the mapper itself
// doesn't care — it operates on file→file-VA, the caller computes bias.
// This test confirms ET_DYN ELFs parse identically.
func TestAddressMapperPIE(t *testing.T) {
	tmp := t.TempDir()
	path := writeMinimalELFType(t, tmp, elf.ET_DYN, 0x1000, 0x0, 0x2000)

	m, err := NewAddressMapper(path)
	if err != nil {
		t.Fatalf("NewAddressMapper(ET_DYN): %v", err)
	}
	got, ok := m.FileOffsetToVirtualAddress(0x1500)
	if !ok || got != 0x500 {
		t.Errorf("ET_DYN FileOffsetToVirtualAddress(0x1500) = (%#x, %v), want (0x500, true)",
			got, ok)
	}
}

// writeELFTwoSegments emits an ELF64 with two PT_LOAD segments.
func writeELFTwoSegments(t *testing.T, dir string, p1, p2 ptLoad) string {
	t.Helper()
	path := filepath.Join(dir, "exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	const ehSize = 64
	const phSize = 56
	phOff := uint64(ehSize)

	var eh [ehSize]byte
	copy(eh[:4], []byte{0x7f, 'E', 'L', 'F'})
	eh[4] = 2
	eh[5] = 1
	eh[6] = 1
	binary.LittleEndian.PutUint16(eh[16:], 2)  // ET_EXEC
	binary.LittleEndian.PutUint16(eh[18:], 62) // EM_X86_64
	binary.LittleEndian.PutUint32(eh[20:], 1)
	binary.LittleEndian.PutUint64(eh[32:], phOff)
	binary.LittleEndian.PutUint16(eh[52:], ehSize)
	binary.LittleEndian.PutUint16(eh[54:], phSize)
	binary.LittleEndian.PutUint16(eh[56:], 2)
	if _, err := f.Write(eh[:]); err != nil {
		t.Fatal(err)
	}

	for _, p := range [2]ptLoad{p1, p2} {
		var ph [phSize]byte
		binary.LittleEndian.PutUint32(ph[0:], 1)
		binary.LittleEndian.PutUint32(ph[4:], 5)
		binary.LittleEndian.PutUint64(ph[8:], p.Off)
		binary.LittleEndian.PutUint64(ph[16:], p.Vaddr)
		binary.LittleEndian.PutUint64(ph[24:], p.Vaddr)
		binary.LittleEndian.PutUint64(ph[32:], p.Filesz)
		binary.LittleEndian.PutUint64(ph[40:], p.Filesz)
		binary.LittleEndian.PutUint64(ph[48:], 0x1000)
		if _, err := f.Write(ph[:]); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// writeMinimalELFType emits an ELF64 of the given e_type with one PT_LOAD.
func writeMinimalELFType(t *testing.T, dir string, etype elf.Type, off, vaddr, filesz uint64) string {
	t.Helper()
	path := filepath.Join(dir, "exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	const ehSize = 64
	const phSize = 56
	phOff := uint64(ehSize)

	var eh [ehSize]byte
	copy(eh[:4], []byte{0x7f, 'E', 'L', 'F'})
	eh[4] = 2
	eh[5] = 1
	eh[6] = 1
	binary.LittleEndian.PutUint16(eh[16:], uint16(etype))
	binary.LittleEndian.PutUint16(eh[18:], 62)
	binary.LittleEndian.PutUint32(eh[20:], 1)
	binary.LittleEndian.PutUint64(eh[32:], phOff)
	binary.LittleEndian.PutUint16(eh[52:], ehSize)
	binary.LittleEndian.PutUint16(eh[54:], phSize)
	binary.LittleEndian.PutUint16(eh[56:], 1)
	if _, err := f.Write(eh[:]); err != nil {
		t.Fatal(err)
	}

	var ph [phSize]byte
	binary.LittleEndian.PutUint32(ph[0:], 1)
	binary.LittleEndian.PutUint32(ph[4:], 5)
	binary.LittleEndian.PutUint64(ph[8:], off)
	binary.LittleEndian.PutUint64(ph[16:], vaddr)
	binary.LittleEndian.PutUint64(ph[24:], vaddr)
	binary.LittleEndian.PutUint64(ph[32:], filesz)
	binary.LittleEndian.PutUint64(ph[40:], filesz)
	binary.LittleEndian.PutUint64(ph[48:], 0x1000)
	if _, err := f.Write(ph[:]); err != nil {
		t.Fatal(err)
	}
	return path
}

