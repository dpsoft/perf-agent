package usdt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"debug/elf"
)

// ---------------------------------------------------------------------
// Black-box tests against the committed testdata fixtures.
//
// Expected offsets below are derived independently of this package: by
// hand from `readelf -n/-S/-l testdata/<fixture>` (virtual addresses,
// section addresses, PT_LOAD Off/Vaddr/Filesz) and cross-checked by
// reading the raw byte at the computed file offset, which must be 0x90
// (a bare `nop`, the instruction every STAP_NOTE/DTRACE_PROBE/STAP_PROBE
// call site compiles to). See testdata/gen.sh for how to reproduce the
// fixtures and rederive these numbers.
// ---------------------------------------------------------------------

func testdataPath(name string) string {
	return filepath.Join("testdata", name)
}

// TestParse_Probe_MultipleProbesSameName exercises testdata/probe, built at
// -O2 specifically so gcc inlines emit_batch into main *and* keeps a
// standalone copy, duplicating the STAP_NOTE asm block. Both notes carry
// identical Provider/Name and differing Location/Arguments.
//
// Catches: any dedup keyed on (Provider, Name) -- e.g. a map[string]Probe
// keyed by name, or a "skip if already seen" guard -- which would silently
// drop one of the two real probes. Also catches wrong vaddr->offset
// arithmetic, since the two expected offsets (0x460, 0x365) are readelf-
// and byte-verified independently, not self-consistent round-trips.
func TestParse_Probe_MultipleProbesSameName(t *testing.T) {
	got, err := ParseFile(testdataPath("probe"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	want := []Probe{
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "8@%rdi", Offset: 0x460},
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "8@%rax", Offset: 0x365},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(probe) =\n%#v\nwant\n%#v", got, want)
	}

	// Independent confirmation that these offsets are real code, not
	// coincidentally-correct arithmetic: both call sites compile to a nop.
	assertByteAt(t, testdataPath("probe"), 0x460, 0x90)
	assertByteAt(t, testdataPath("probe"), 0x365, 0x90)
}

// TestParse_Probe2_NoSemaphore covers a probe with no semaphore
// (DTRACE_PROBE1, no _SDT_HAS_SEMAPHORES). The note's Semaphore field reads
// 0x0.
//
// Catches: code that treats "semaphore field is 0" as "semaphore is at file
// offset 0" instead of "no semaphore". File offset 0 is the ELF header
// itself here -- a legitimate offset -- so if the implementation always set
// HasSemaphore based on the field being merely present (rather than
// nonzero), or always ran the vaddr->offset conversion on a raw 0 address,
// this would either fail outright (0 isn't covered by any PT_LOAD segment
// starting at 0x400000) or silently report SemaphoreOffset=0 as if real.
func TestParse_Probe2_NoSemaphore(t *testing.T) {
	got, err := ParseFile(testdataPath("probe2"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	want := []Probe{
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "8@-8(%rbp)", Offset: 0x44e},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(probe2) =\n%#v\nwant\n%#v", got, want)
	}
	assertByteAt(t, testdataPath("probe2"), 0x44e, 0x90)
}

// TestParse_Probe4_RealSemaphore covers a probe with
// _SDT_HAS_SEMAPHORES: a genuine semaphore variable exists, and the note's
// Semaphore field is a real, nonzero address.
//
// Catches: wrong semaphore address->offset conversion (the expected value
// 0x200c is derived by hand from the RW PT_LOAD segment's Off/Vaddr/Filesz,
// not from running this package's own code), and the PT_LOAD segment
// selection specifically -- the semaphore lives in the RW data segment,
// different from the code segment the probe location lives in, so a bug
// that always uses the first (executable) PT_LOAD would misresolve it.
func TestParse_Probe4_RealSemaphore(t *testing.T) {
	got, err := ParseFile(testdataPath("probe4"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	want := []Probe{
		{
			Provider:        "perfagent",
			Name:            "gpu_launch_v1",
			Args:            "8@-8(%rbp)",
			Offset:          0x47a,
			HasSemaphore:    true,
			SemaphoreOffset: 0x200c,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(probe4) =\n%#v\nwant\n%#v", got, want)
	}
	assertByteAt(t, testdataPath("probe4"), 0x47a, 0x90)

	// The semaphore offset must land in the writable PT_LOAD segment, not
	// (say) the executable one it would land in if the parser reused the
	// probe location's segment by mistake.
	ef, err := elf.Open(testdataPath("probe4"))
	if err != nil {
		t.Fatalf("elf.Open: %v", err)
	}
	defer func() { _ = ef.Close() }()
	seg := progContaining(ef, got[0].SemaphoreOffset)
	if seg == nil {
		t.Fatalf("semaphore offset %#x not contained in any PT_LOAD segment's file range", got[0].SemaphoreOffset)
	}
	if seg.Flags&elf.PF_W == 0 {
		t.Fatalf("semaphore offset %#x resolved into a non-writable segment (flags=%v); expected the RW data segment", got[0].SemaphoreOffset, seg.Flags)
	}
}

// progContaining returns the PT_LOAD segment whose file range contains the
// given file offset, or nil.
func progContaining(ef *elf.File, fileOffset uint64) *elf.Prog {
	for _, p := range ef.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if fileOffset >= p.Off && fileOffset < p.Off+p.Filesz {
			return p
		}
	}
	return nil
}

func assertByteAt(t *testing.T, path string, offset int64, want byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, offset); err != nil {
		t.Fatalf("read %s at %#x: %v", path, offset, err)
	}
	if buf[0] != want {
		t.Fatalf("%s[%#x] = %#02x, want %#02x", path, offset, buf[0], want)
	}
}

// TestParse_ToolchainDrift builds probe2.c fresh with the host's gcc and
// asserts the parse agrees with the committed testdata/probe2 binary's
// parse in every field this package promises to recover, plus an
// independent proof the fresh binary's own offset is real code -- catching
// drift between the toolchain that produced testdata/ and the one running
// the test, without requiring byte-identical binaries (a newer/older gcc
// may legitimately lay out .text differently).
func TestParse_ToolchainDrift(t *testing.T) {
	gccPath, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc not available")
	}
	if _, err := os.Stat("/usr/include/sys/sdt.h"); err != nil {
		t.Skip("sys/sdt.h not available")
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "probe2")
	cmd := exec.Command(gccPath, "-no-pie", "-g", "-O0", "-o", out, testdataPath("probe2.c"))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gcc build failed: %v", err)
	}

	fresh, err := ParseFile(out)
	if err != nil {
		t.Fatalf("ParseFile(fresh build): %v", err)
	}
	committed, err := ParseFile(testdataPath("probe2"))
	if err != nil {
		t.Fatalf("ParseFile(committed): %v", err)
	}

	if len(fresh) != 1 || len(committed) != 1 {
		t.Fatalf("expected exactly one probe from each build: fresh=%d committed=%d", len(fresh), len(committed))
	}
	if fresh[0].Provider != committed[0].Provider ||
		fresh[0].Name != committed[0].Name ||
		fresh[0].Args != committed[0].Args ||
		fresh[0].HasSemaphore != committed[0].HasSemaphore {
		t.Fatalf("fresh build disagrees with committed fixture on structural fields:\nfresh:     %#v\ncommitted: %#v", fresh[0], committed[0])
	}

	// The fresh build's own offset must be real, independent of whatever
	// value the committed fixture happens to have.
	assertByteAt(t, out, int64(fresh[0].Offset), 0x90)
}

// ---------------------------------------------------------------------
// Synthetic-ELF tests.
//
// The compiled fixtures above never exercise a nonzero prelink/base delta
// (this gcc/binutils never emits a note whose base field differs from
// .stapsdt.base's own address -- there is no prelinking happening), a
// malformed note, an absent .note.stapsdt section, or non-amd64 class/byte
// order. Those are constructed by hand below with a minimal ELF builder so
// every documented behavior actually gets a test, not just the ones the
// local toolchain happens to produce.
// ---------------------------------------------------------------------

// TestParse_NoStapsdtSection covers a well-formed ELF with no
// .note.stapsdt section at all.
//
// Catches: treating "section absent" as an error (ef.Section returns nil,
// which must short-circuit to (nil, nil), not be dereferenced or passed to
// code that assumes it exists).
func TestParse_NoStapsdtSection(t *testing.T) {
	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x100, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, nil)

	got, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Parse = %#v, want empty (no .note.stapsdt section present)", got)
	}
}

// TestParse_MalformedNote_TruncatedHeader covers a .note.stapsdt section
// whose last note header is cut short.
//
// Catches: a parser that reads past the end of the buffer (panic) or one
// that silently stops and returns whatever probes it decoded before the
// truncation as if that were the complete, correct list -- the spec
// requires an error here, never a partial result presented as success.
func TestParse_MalformedNote_TruncatedHeader(t *testing.T) {
	note := encodeStapsdtNote(binary.LittleEndian, 8, 0x400010, 0, 0, "perfagent", "gpu_launch_v1", "8@%rdi")
	// Cut off mid note-header (namesz/descsz/type is 12 bytes; leave 5).
	truncated := note[:5]

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x100, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: truncated},
	})

	probes, err := Parse(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("Parse succeeded on a truncated note header; got %#v, want error", probes)
	}
	if probes != nil {
		t.Fatalf("Parse returned %d probes alongside an error; want nil on failure", len(probes))
	}
}

// TestParse_MalformedNote_DescszExceedsSectionBounds covers a note whose
// header promises a descriptor longer than the bytes actually present in
// the section (the raw .note.stapsdt data is cut short of what descsz
// claims). This is caught at the note-container level, by parseNotes'
// `if uint64(len(data)) < descLen` bounds check (usdt.go) -- the same guard
// TestParse_MalformedNote_TruncatedHeader exercises for the header itself.
// It never reaches parseStapsdtDescriptor at all, since parseNotes returns
// an error before slicing out a descriptor to hand it.
//
// Catches: parseNotes trusting descsz without checking it against the
// remaining section buffer (an out-of-range slice would panic; a
// saturating/clamping read would silently hand parseStapsdtDescriptor
// garbage and fabricate a probe from it).
func TestParse_MalformedNote_DescszExceedsSectionBounds(t *testing.T) {
	note := encodeStapsdtNote(binary.LittleEndian, 8, 0x400010, 0, 0, "perfagent", "gpu_launch_v1", "8@%rdi")
	// Keep the 12-byte header (which still claims the full descsz) and the
	// 4-byte-aligned name, but chop the descriptor bytes themselves short
	// -- the section's raw data ends before descsz says it should.
	headerAndName := note[:12+align4Len(len("stapsdt")+1)]
	truncated := append(append([]byte{}, headerAndName...), note[len(headerAndName):len(headerAndName)+3]...)

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x100, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: truncated},
	})

	probes, err := Parse(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("Parse succeeded when the section's raw bytes end before descsz says they should; got %#v, want error", probes)
	}
	if probes != nil {
		t.Fatalf("Parse returned %d probes alongside an error; want nil on failure", len(probes))
	}
}

// TestParse_MalformedNote_DescriptorTooShortForAddresses reaches
// parseStapsdtDescriptor's own bounds check (usdt.go: `need := addrSize*3;
// if len(desc) < need`) -- a distinct guard from the container-level one
// TestParse_MalformedNote_DescszExceedsSectionBounds exercises above. The
// note here is well-formed and *complete* at the container level: descsz is
// accurate, every declared byte is genuinely present, and parseNotes hands
// the full descriptor over without complaint. It only fails once
// parseStapsdtDescriptor tries to read three pointer-sized (8 bytes each,
// on this 64-bit ELF -- 24 bytes total) addresses out of a 5-byte
// descriptor.
//
// Catches: deleting or weakening parseStapsdtDescriptor's own length check.
// Verified live: commenting out that guard turns this test's clean error
// into a panic (slice bounds out of range) inside readAddr instead.
func TestParse_MalformedNote_DescriptorTooShortForAddresses(t *testing.T) {
	// A complete, accurately-declared note (descsz=5, and all 5 bytes are
	// really there) whose descriptor is simply too small to hold
	// location+base+semaphore.
	note := encodeNote(binary.LittleEndian, noteOwnerStapsdt, noteTypeStapsdt, []byte{0x11, 0x22, 0x33, 0x44, 0x55})

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x100, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	probes, err := Parse(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("Parse succeeded on a complete 5-byte descriptor (need 24 bytes for three 8-byte addresses); got %#v, want error", probes)
	}
	if probes != nil {
		t.Fatalf("Parse returned %d probes alongside an error; want nil on failure", len(probes))
	}
}

// TestParse_MalformedNote_UnterminatedString covers a descriptor whose
// three addresses are intact but whose provider/name/args strings never
// hit a NUL byte within the declared descsz.
//
// Catches: string-scanning code that runs off the end of desc (panic) or
// that accepts a non-NUL-terminated tail as the final string silently.
func TestParse_MalformedNote_UnterminatedString(t *testing.T) {
	var desc bytes.Buffer
	var addr [8]byte
	binary.LittleEndian.PutUint64(addr[:], 0x400010)
	desc.Write(addr[:]) // location
	desc.Write(addr[:]) // base (reused value, irrelevant here)
	binary.LittleEndian.PutUint64(addr[:], 0)
	desc.Write(addr[:])           // semaphore = 0
	desc.WriteString("perfagent") // provider, NUL-terminated
	desc.WriteByte(0)
	desc.WriteString("gpu_launch_v1_no_nul") // name, deliberately missing its NUL

	note := encodeNote(binary.LittleEndian, "stapsdt", noteTypeStapsdt, desc.Bytes())

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x100, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	probes, err := Parse(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("Parse succeeded despite an unterminated name string; got %#v, want error", probes)
	}
	if probes != nil {
		t.Fatalf("Parse returned %d probes alongside an error; want nil on failure", len(probes))
	}
}

// TestParse_BaseAdjustment_Applied is the load-bearing test for the
// prelink/base adjustment: it constructs a note whose base field
// deliberately disagrees with the .stapsdt.base section's actual address,
// by a delta chosen so that "forget the adjustment" and "apply it with the
// wrong sign" both land on a *different* PT_LOAD segment than the correct
// answer -- so either mistake fails loudly (wrong segment or ErrNoLoadSegment)
// rather than producing a plausible-but-wrong offset that happens to still
// parse.
//
// Layout: two 0x1000-sized PT_LOAD segments back to back, at 0x400000 and
// 0x401000. The note's raw location field points into the *second*
// segment (0x401010); the note's base field is 0x401000 (matching that
// segment's start, as if the notes were authored against it); but
// .stapsdt.base's actual runtime address is 0x400000 -- one segment
// earlier. The correct delta is 0x400000-0x401000 = -0x1000, so the real
// (adjusted) location is 0x401010-0x1000 = 0x400010, landing in the FIRST
// segment. An implementation that ignores the delta resolves 0x401010
// in the second segment (offset 0x10 there, i.e. file offset 0x1000+0x10);
// one with the sign flipped resolves 0x402010, which is in neither
// segment and must error. Only the correct delta lands on file offset
// 0x10 in the first segment.
func TestParse_BaseAdjustment_Applied(t *testing.T) {
	const (
		seg0Vaddr = 0x400000
		seg1Vaddr = 0x401000
		segSize   = 0x1000

		noteLocation = seg1Vaddr + 0x10 // 0x401010, as authored
		noteBase     = seg1Vaddr        // 0x401000, as authored
		actualBase   = seg0Vaddr        // 0x400000, .stapsdt.base's real runtime address
	)

	note := encodeStapsdtNote(binary.LittleEndian, 8, noteLocation, noteBase, 0, "perfagent", "gpu_launch_v1", "8@%rdi")

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: seg0Vaddr, filesz: segSize, memsz: segSize, flags: elf.PF_R | elf.PF_X},
		{off: 0, vaddr: seg1Vaddr, filesz: segSize, memsz: segSize, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
		{name: stapsdtBaseSection, typ: uint32(elf.SHT_PROGBITS), addr: actualBase, data: []byte{0}},
	})

	got, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Probe{
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "8@%rdi", Offset: 0x10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse with base adjustment =\n%#v\nwant\n%#v\n(delta = actualBase(%#x) - noteBase(%#x); unadjusted or wrong-signed would resolve into the wrong segment or error)", got, want, uint64(actualBase), uint64(noteBase))
	}
}

// TestParse_BaseAdjustment_AbsentSection_NoShift is the companion to the
// above: the note's base field is the same "wrong" value, but this ELF has
// no .stapsdt.base section at all, so no adjustment must be applied -- the
// location is used exactly as recorded.
//
// Catches: an implementation that applies a delta computed some other way
// (e.g. against the ELF's lowest PT_LOAD Vaddr) when .stapsdt.base is
// missing, instead of leaving addresses untouched as the spec requires.
func TestParse_BaseAdjustment_AbsentSection_NoShift(t *testing.T) {
	const (
		segVaddr = 0x400000
		segSize  = 0x1000

		noteLocation = segVaddr + 0x20
		noteBase     = 0x999000 // deliberately nonsensical; must be ignored
	)

	note := encodeStapsdtNote(binary.LittleEndian, 8, noteLocation, noteBase, 0, "perfagent", "gpu_launch_v1", "8@%rdi")

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: segVaddr, filesz: segSize, memsz: segSize, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	got, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Probe{
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "8@%rdi", Offset: 0x20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse without .stapsdt.base =\n%#v\nwant\n%#v", got, want)
	}
}

// TestParse_32Bit covers an ELFCLASS32 file, where note addresses are
// 4-byte fields rather than 8.
//
// Catches: hardcoding 8-byte address reads regardless of class -- which
// would misparse the descriptor layout entirely (reading 4 bytes of the
// provider string as the tail of the semaphore address, for instance) and
// either error or produce garbage strings.
func TestParse_32Bit(t *testing.T) {
	const (
		segVaddr = 0x08048000
		segSize  = 0x1000
		location = segVaddr + 0x30
	)
	note := encodeStapsdtNote(binary.LittleEndian, 4, location, 0, 0, "perfagent", "gpu_launch_v1", "4@%eax")

	raw := buildELF(t, elf.ELFCLASS32, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: segVaddr, filesz: segSize, memsz: segSize, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	got, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Probe{
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "4@%eax", Offset: 0x30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(32-bit) =\n%#v\nwant\n%#v", got, want)
	}
}

// TestParse_64Bit_BigEndian covers a big-endian 64-bit ELF (e.g. s390x).
//
// Catches: hardcoding binary.LittleEndian anywhere instead of using the
// byte order elf.NewFile detected from EI_DATA -- the note header fields,
// the three addresses, and (implicitly, via elf.File) the program/section
// header fields would all be misread on a real big-endian binary.
func TestParse_64Bit_BigEndian(t *testing.T) {
	const (
		segVaddr = 0x10000000
		segSize  = 0x1000
		location = segVaddr + 0x40
	)
	note := encodeStapsdtNote(binary.BigEndian, 8, location, 0, 0, "perfagent", "gpu_launch_v1", "8@%r2")

	raw := buildELF(t, elf.ELFCLASS64, binary.BigEndian, []synSeg{
		{off: 0, vaddr: segVaddr, filesz: segSize, memsz: segSize, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	got, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Probe{
		{Provider: "perfagent", Name: "gpu_launch_v1", Args: "8@%r2", Offset: 0x40},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(64-bit big-endian) =\n%#v\nwant\n%#v", got, want)
	}
}

// TestParse_LocationInBSS covers a location address that falls within a
// PT_LOAD segment's memory image but beyond its file-backed bytes (i.e.
// .bss) -- which must be reported as an error, per the spec, rather than a
// silently wrong file offset (there is no file content there to attach a
// uprobe to).
//
// Catches: a bounds check against Memsz instead of Filesz when deciding
// whether an address has a real file offset.
func TestParse_LocationInBSS(t *testing.T) {
	const (
		segVaddr  = 0x600000
		segFilesz = 0x100
		segMemsz  = 0x200 // 0x100 bytes of .bss beyond the file-backed part

		location = segVaddr + 0x150 // inside Memsz, beyond Filesz
	)
	note := encodeStapsdtNote(binary.LittleEndian, 8, location, 0, 0, "perfagent", "gpu_launch_v1", "8@%rdi")

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: segVaddr, filesz: segFilesz, memsz: segMemsz, flags: elf.PF_R | elf.PF_W},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	probes, err := Parse(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("Parse succeeded for a location in .bss; got %#v, want ErrInBSS", probes)
	}
	if !errors.Is(err, ErrInBSS) {
		t.Fatalf("Parse error = %v, want it to wrap ErrInBSS", err)
	}
	if probes != nil {
		t.Fatalf("Parse returned %d probes alongside an error", len(probes))
	}
}

// TestParse_LocationNotInAnySegment covers an address covered by no
// PT_LOAD segment whatsoever.
//
// Catches: a loop that returns a zero-value offset (or the last segment's
// base) instead of erroring when nothing matches.
func TestParse_LocationNotInAnySegment(t *testing.T) {
	const location = 0x999999
	note := encodeStapsdtNote(binary.LittleEndian, 8, location, 0, 0, "perfagent", "gpu_launch_v1", "8@%rdi")

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x1000, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: note},
	})

	probes, err := Parse(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("Parse succeeded for an address in no PT_LOAD segment; got %#v, want ErrNoLoadSegment", probes)
	}
	if !errors.Is(err, ErrNoLoadSegment) {
		t.Fatalf("Parse error = %v, want it to wrap ErrNoLoadSegment", err)
	}
	if probes != nil {
		t.Fatalf("Parse returned %d probes alongside an error", len(probes))
	}
}

// TestParseNotes_MultipleConcatenated covers two well-formed notes back to
// back in a single section, including a non-stapsdt note in between that
// must be skipped rather than misinterpreted.
//
// Catches: padding/alignment arithmetic that's right for exactly one note
// but drifts on the second (a common off-by-N when the first note's
// namesz/descsz aren't multiples of 4, since "stapsdt\0" is 8 bytes -
// already aligned - so this uses a deliberately unaligned owner name to
// force the padding logic to actually do something), and owner/type
// filtering that doesn't skip unrelated notes cleanly.
func TestParseNotes_MultipleConcatenated(t *testing.T) {
	other := encodeNote(binary.LittleEndian, "GNU", 3, []byte{0xde, 0xad, 0xbe, 0xef, 0x01}) // 5-byte desc, unaligned
	note1 := encodeStapsdtNote(binary.LittleEndian, 8, 0x400100, 0, 0, "perfagent", "probe_a", "8@%rdi")
	note2 := encodeStapsdtNote(binary.LittleEndian, 8, 0x400200, 0, 0, "perfagent", "probe_b", "8@%rsi")

	sectionData := append(append(append([]byte{}, other...), note1...), note2...)

	raw := buildELF(t, elf.ELFCLASS64, binary.LittleEndian, []synSeg{
		{off: 0, vaddr: 0x400000, filesz: 0x1000, memsz: 0x1000, flags: elf.PF_R | elf.PF_X},
	}, []synSec{
		{name: stapsdtSection, typ: uint32(elf.SHT_NOTE), addr: 0, data: sectionData},
	})

	got, err := Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Probe{
		{Provider: "perfagent", Name: "probe_a", Args: "8@%rdi", Offset: 0x100},
		{Provider: "perfagent", Name: "probe_b", Args: "8@%rsi", Offset: 0x200},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(multi-note) =\n%#v\nwant\n%#v", got, want)
	}
}

// ---------------------------------------------------------------------
// align4 (internal helper) tests.
// ---------------------------------------------------------------------

func TestAlign4(t *testing.T) {
	cases := []struct {
		in   uint32
		want uint64
	}{
		{0, 0},
		{1, 4},
		{3, 4},
		{4, 4},
		{5, 8},
		{0xffffffff, 0x100000000}, // must not wrap around in 32-bit arithmetic
	}
	for _, c := range cases {
		if got := align4(c.in); got != c.want {
			t.Errorf("align4(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------
// Minimal synthetic ELF builder.
//
// Just enough of the ELF32/ELF64 wire format for debug/elf.NewFile to
// parse Progs (PT_LOAD only) and named Sections -- nothing else is
// exercised by this package. Uses debug/elf's own Header/Prog/Section
// structs, whose field order and widths already match the on-disk format
// per class, so binary.Write with the target byte order produces a valid
// file directly.
// ---------------------------------------------------------------------

type synSeg struct {
	// off is the PT_LOAD segment's file offset (Phdr.Off), chosen
	// explicitly by the test rather than computed from real file layout.
	// This package never reads a PT_LOAD segment's actual content (only
	// its Off/Vaddr/Filesz/Memsz geometry), so no bytes need to physically
	// live there -- callers can pick clean, easy-to-hand-verify values.
	off, vaddr, filesz, memsz uint64
	flags                     elf.ProgFlag
}

type synSec struct {
	name string
	typ  uint32
	addr uint64
	data []byte
}

func buildELF(t *testing.T, class elf.Class, order binary.ByteOrder, segs []synSeg, secs []synSec) []byte {
	t.Helper()

	is64 := class == elf.ELFCLASS64
	var ehsize, phentsize, shentsize int
	if is64 {
		ehsize, phentsize, shentsize = 64, 56, 64
	} else {
		ehsize, phentsize, shentsize = 52, 32, 40
	}

	phoff := uint64(ehsize)
	cursor := phoff + uint64(len(segs))*uint64(phentsize)

	secOff := make([]uint64, len(secs))
	for i, s := range secs {
		secOff[i] = cursor
		cursor += uint64(len(s.data))
	}

	// Section header string table: "", then each section's name, then
	// ".shstrtab" itself.
	shstrtab := []byte{0}
	nameOff := make([]uint32, len(secs))
	for i, s := range secs {
		nameOff[i] = uint32(len(shstrtab))
		shstrtab = append(shstrtab, []byte(s.name)...)
		shstrtab = append(shstrtab, 0)
	}
	shstrtabNameOff := uint32(len(shstrtab))
	shstrtab = append(shstrtab, []byte(".shstrtab")...)
	shstrtab = append(shstrtab, 0)

	shstrtabOff := cursor
	cursor += uint64(len(shstrtab))
	shoff := cursor

	shnum := len(secs) + 2 // NULL + secs + .shstrtab
	shstrndx := shnum - 1

	buf := &bytes.Buffer{}

	// --- ELF header ---
	var ident [elf.EI_NIDENT]byte
	ident[0], ident[1], ident[2], ident[3] = 0x7f, 'E', 'L', 'F'
	ident[elf.EI_CLASS] = byte(class)
	if order == binary.LittleEndian {
		ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	} else {
		ident[elf.EI_DATA] = byte(elf.ELFDATA2MSB)
	}
	ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	ident[elf.EI_OSABI] = byte(elf.ELFOSABI_NONE)

	machine := elf.EM_X86_64
	if !is64 {
		machine = elf.EM_386
	}

	if is64 {
		h := elf.Header64{
			Ident:     ident,
			Type:      uint16(elf.ET_EXEC),
			Machine:   uint16(machine),
			Version:   uint32(elf.EV_CURRENT),
			Entry:     0,
			Phoff:     phoff,
			Shoff:     shoff,
			Flags:     0,
			Ehsize:    uint16(ehsize),
			Phentsize: uint16(phentsize),
			Phnum:     uint16(len(segs)),
			Shentsize: uint16(shentsize),
			Shnum:     uint16(shnum),
			Shstrndx:  uint16(shstrndx),
		}
		mustWrite(t, buf, order, h)
	} else {
		h := elf.Header32{
			Ident:     ident,
			Type:      uint16(elf.ET_EXEC),
			Machine:   uint16(machine),
			Version:   uint32(elf.EV_CURRENT),
			Entry:     0,
			Phoff:     uint32(phoff),
			Shoff:     uint32(shoff),
			Flags:     0,
			Ehsize:    uint16(ehsize),
			Phentsize: uint16(phentsize),
			Phnum:     uint16(len(segs)),
			Shentsize: uint16(shentsize),
			Shnum:     uint16(shnum),
			Shstrndx:  uint16(shstrndx),
		}
		mustWrite(t, buf, order, h)
	}

	// --- program headers ---
	//
	// PT_LOAD segments here carry only geometry (Off/Vaddr/Filesz/Memsz):
	// this package never reads a segment's actual bytes (only Section
	// data, via sec.Data()), so no file content needs to exist at a
	// segment's Off -- s.off is whatever the test chose it to be.
	for _, s := range segs {
		if is64 {
			mustWrite(t, buf, order, elf.Prog64{
				Type:   uint32(elf.PT_LOAD),
				Flags:  uint32(s.flags),
				Off:    s.off,
				Vaddr:  s.vaddr,
				Paddr:  s.vaddr,
				Filesz: s.filesz,
				Memsz:  s.memsz,
				Align:  0x1000,
			})
		} else {
			mustWrite(t, buf, order, elf.Prog32{
				Type:   uint32(elf.PT_LOAD),
				Off:    uint32(s.off),
				Vaddr:  uint32(s.vaddr),
				Paddr:  uint32(s.vaddr),
				Filesz: uint32(s.filesz),
				Memsz:  uint32(s.memsz),
				Flags:  uint32(s.flags),
				Align:  0x1000,
			})
		}
	}

	// --- section raw data, then shstrtab bytes ---
	for _, s := range secs {
		buf.Write(s.data)
	}
	buf.Write(shstrtab)

	// --- section headers ---
	if is64 {
		mustWrite(t, buf, order, elf.Section64{}) // NULL section
		for i, s := range secs {
			mustWrite(t, buf, order, elf.Section64{
				Name:      nameOff[i],
				Type:      s.typ,
				Addr:      s.addr,
				Off:       secOff[i],
				Size:      uint64(len(s.data)),
				Addralign: 1,
			})
		}
		mustWrite(t, buf, order, elf.Section64{
			Name:      shstrtabNameOff,
			Type:      uint32(elf.SHT_STRTAB),
			Off:       shstrtabOff,
			Size:      uint64(len(shstrtab)),
			Addralign: 1,
		})
	} else {
		mustWrite(t, buf, order, elf.Section32{}) // NULL section
		for i, s := range secs {
			mustWrite(t, buf, order, elf.Section32{
				Name:      nameOff[i],
				Type:      s.typ,
				Addr:      uint32(s.addr),
				Off:       uint32(secOff[i]),
				Size:      uint32(len(s.data)),
				Addralign: 1,
			})
		}
		mustWrite(t, buf, order, elf.Section32{
			Name:      shstrtabNameOff,
			Type:      uint32(elf.SHT_STRTAB),
			Off:       uint32(shstrtabOff),
			Size:      uint32(len(shstrtab)),
			Addralign: 1,
		})
	}

	return buf.Bytes()
}

func mustWrite(t *testing.T, buf *bytes.Buffer, order binary.ByteOrder, v any) {
	t.Helper()
	if err := binary.Write(buf, order, v); err != nil {
		t.Fatalf("binary.Write(%T): %v", v, err)
	}
}

// encodeStapsdtNote builds one raw NT_STAPSDT note record (header + owner +
// descriptor), ready to be used as (part of) a .note.stapsdt section's
// contents.
func encodeStapsdtNote(order binary.ByteOrder, addrSize int, location, base, semaphore uint64, provider, name, args string) []byte {
	var desc bytes.Buffer
	writeAddr := func(v uint64) {
		if addrSize == 4 {
			var b [4]byte
			order.PutUint32(b[:], uint32(v))
			desc.Write(b[:])
		} else {
			var b [8]byte
			order.PutUint64(b[:], v)
			desc.Write(b[:])
		}
	}
	writeAddr(location)
	writeAddr(base)
	writeAddr(semaphore)
	desc.WriteString(provider)
	desc.WriteByte(0)
	desc.WriteString(name)
	desc.WriteByte(0)
	desc.WriteString(args)
	desc.WriteByte(0)

	return encodeNote(order, noteOwnerStapsdt, noteTypeStapsdt, desc.Bytes())
}

// encodeNote builds one raw ELF note record: a 12-byte namesz/descsz/type
// header, the NUL-terminated owner name padded to a 4-byte boundary, then
// desc padded to a 4-byte boundary.
func encodeNote(order binary.ByteOrder, owner string, typ uint32, desc []byte) []byte {
	name := append([]byte(owner), 0)

	var buf bytes.Buffer
	var h [12]byte
	order.PutUint32(h[0:4], uint32(len(name)))
	order.PutUint32(h[4:8], uint32(len(desc)))
	order.PutUint32(h[8:12], typ)
	buf.Write(h[:])
	buf.Write(name)
	padTo4(&buf, len(name))
	buf.Write(desc)
	padTo4(&buf, len(desc))
	return buf.Bytes()
}

func padTo4(buf *bytes.Buffer, n int) {
	for n%4 != 0 {
		buf.WriteByte(0)
		n++
	}
}

func align4Len(n int) int {
	return (n + 3) &^ 3
}
