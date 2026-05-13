package procmap

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestParseMapsFile(t *testing.T) {
	path := filepath.Join("testdata", "proc", "4242", "maps")
	got, err := parseMapsFile(path)
	if err != nil {
		t.Fatalf("parseMapsFile: %v", err)
	}

	want := []Mapping{
		{Path: "/usr/bin/target", Start: 0x00400000, Limit: 0x00420000, Offset: 0x1000, IsExec: true},
		{Path: "/lib/x86_64-linux-gnu/libc.so.6", Start: 0x7f0000001000, Limit: 0x7f0000100000, Offset: 0x2000, IsExec: true},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d mappings, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mapping %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestReadBuildID(t *testing.T) {
	// /bin/ls on any modern distro has a GNU build-id. We don't assert
	// the exact value (it varies) — only that it parses to a non-empty
	// lowercase hex string.
	id, err := ReadBuildID("/bin/ls")
	if err != nil {
		t.Fatalf("ReadBuildID(/bin/ls): %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty build-id, got empty")
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("build-id %q contains non-hex char %q", id, r)
		}
	}
}

func TestReadBuildIDMissing(t *testing.T) {
	id, err := ReadBuildID("/nonexistent/path/to/nothing")
	if err == nil {
		t.Fatalf("expected error, got id=%q", id)
	}
}

func TestResolverLookupHitMiss(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	m, ok := r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("expected lookup hit in /usr/bin/target range")
	}
	if m.Path != "/usr/bin/target" {
		t.Errorf("got Path=%q, want /usr/bin/target", m.Path)
	}

	_, ok = r.Lookup(4242, 0xdeadbeef)
	if ok {
		t.Fatal("expected lookup miss outside any mapping")
	}
}

func TestResolverMissingPID(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, ok := r.Lookup(9999999, 0x00401234)
	if ok {
		t.Fatal("expected miss for non-existent PID")
	}
	// Second call should hit the cached empty entry, not re-read /proc.
	_, ok = r.Lookup(9999999, 0x00401234)
	if ok {
		t.Fatal("second lookup should also miss")
	}
}

func TestResolverInvalidate(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, ok := r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("first lookup should hit")
	}
	r.Invalidate(4242)
	// Still hits because the fixture file is unchanged, but the path
	// re-populated. Just ensures Invalidate doesn't panic and Lookup
	// keeps working afterward.
	_, ok = r.Lookup(4242, 0x00401234)
	if !ok {
		t.Fatal("lookup after Invalidate should still hit")
	}
}

func TestResolverConcurrentLookup(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	const N = 32
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, ok := r.Lookup(4242, 0x00401234)
			if !ok {
				errs <- fmt.Errorf("lookup miss")
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolverInvalidateAddrNoOpInRange(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, _ = r.Lookup(7777, 0x00701000) // populate
	before := r.populateCountForTest(7777)

	r.InvalidateAddr(7777, 0x00701000) // in-range -> no-op
	after := r.populateCountForTest(7777)

	if after != before {
		t.Fatalf("populate count changed %d -> %d after in-range InvalidateAddr", before, after)
	}
}

func TestResolverInvalidateAddrOutOfRangeForcesReparse(t *testing.T) {
	r := NewResolver(WithProcRoot("testdata/proc"))
	defer r.Close()

	_, _ = r.Lookup(7777, 0x00701000) // populate
	before := r.populateCountForTest(7777)

	r.InvalidateAddr(7777, 0xdeadbeef) // out-of-range -> evict
	_, _ = r.Lookup(7777, 0x00701000)  // re-populate
	after := r.populateCountForTest(7777)

	if after != before+1 {
		t.Fatalf("expected 1 re-populate, got %d -> %d", before, after)
	}
}

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
	bo.PutUint64(buf[32:40], uint64(ehsz))  // e_phoff
	bo.PutUint64(buf[40:48], shdrsOff)       // e_shoff
	// e_flags [48:52] = 0
	bo.PutUint16(buf[52:54], ehsz)           // e_ehsize
	bo.PutUint16(buf[54:56], phsz)           // e_phentsize
	bo.PutUint16(buf[56:58], 1)              // e_phnum
	bo.PutUint16(buf[58:60], shsz)           // e_shentsize
	bo.PutUint16(buf[60:62], 3)              // e_shnum: null, note, shstrtab
	bo.PutUint16(buf[62:64], 2)              // e_shstrndx: section 2 = .shstrtab

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
	bo.PutUint32(s1[0:4], noteNameOff)         // sh_name
	bo.PutUint32(s1[4:8], uint32(elf.SHT_NOTE)) // sh_type
	// sh_flags [8:16] = 0
	// sh_addr  [16:24] = 0
	bo.PutUint64(s1[24:32], noteOff)            // sh_offset
	bo.PutUint64(s1[32:40], uint64(noteSz))     // sh_size
	// sh_link [40:44] = 0, sh_info [44:48] = 0
	bo.PutUint64(s1[48:56], 4)                  // sh_addralign

	// Section 2: .shstrtab
	s2 := buf[shdrsOff+2*shsz : shdrsOff+3*shsz]
	bo.PutUint32(s2[0:4], shstrNameOff)            // sh_name
	bo.PutUint32(s2[4:8], uint32(elf.SHT_STRTAB))  // sh_type
	// sh_flags, sh_addr = 0
	bo.PutUint64(s2[24:32], shstrDataOff)           // sh_offset
	bo.PutUint64(s2[32:40], uint64(len(shstrtab)))  // sh_size
	bo.PutUint64(s2[48:56], 1)                      // sh_addralign

	// ── shstrtab data ─────────────────────────────────────────────────────────
	copy(buf[shstrDataOff:], shstrtab)

	p := filepath.Join(dir, "fake.elf")
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("writeELFWithBuildID: %v", err)
	}
	return p
}

func TestResolverPopulateBuildIDViaMapFiles(t *testing.T) {
	// Simulate the sidecar case: build-id is only readable through the
	// MapFiles symlink because the symbolic Path is unreachable.
	tmp := t.TempDir()
	binWithBuildID := writeELFWithBuildID(t, tmp, []byte{0xab, 0xcd, 0xef, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11})

	m := Mapping{
		Path:     "/sidecar/unreachable/bin", // doesn't exist on host
		MapFiles: binWithBuildID,             // does exist
		Start:    0x400000,
		Limit:    0x401000,
		IsExec:   true,
	}

	r := &Resolver{} // bare resolver; we only need the build-id attachment path
	mappings := []Mapping{m}
	r.attachBuildIDs(mappings)

	want := "abcdef0102030405060708090a0b0c0d0e0f1011"
	if mappings[0].BuildID != want {
		t.Errorf("BuildID via MapFiles = %q, want %q", mappings[0].BuildID, want)
	}
}

func TestMappingOpenablePath(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "exe")
	if err := os.WriteFile(binPath, []byte("dummy"), 0o755); err != nil {
		t.Fatal(err)
	}
	binPath2 := filepath.Join(tmp, "exe2")
	if err := os.WriteFile(binPath2, []byte("dummy2"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		mapFiles string
		path     string
		want     string
	}{
		// binPath is in MapFiles; binPath2 is the symbolic Path.
		// The result must be binPath, proving MapFiles is checked first.
		{"map_files preferred when both readable", binPath, binPath2, binPath},
		{"falls back to symbolic when map_files empty", "", binPath, binPath},
		{"falls back to symbolic when map_files missing", "/nope/missing", binPath, binPath},
		{"map_files wins when symbolic deleted", binPath, "/deleted/path", binPath},
		{"empty when neither works", "/nope/a", "/nope/b", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Mapping{MapFiles: tc.mapFiles, Path: tc.path}
			if got := m.OpenablePath(); got != tc.want {
				t.Errorf("OpenablePath() = %q, want %q", got, tc.want)
			}
		})
	}
}
