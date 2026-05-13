package procmap

import (
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

