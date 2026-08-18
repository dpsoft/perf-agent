// Package usdt parses ELF .note.stapsdt notes to recover USDT (Userland
// Statically Defined Tracing) probe locations.
//
// A USDT probe is a no-op instruction site plus a note in the binary's
// .note.stapsdt section recording, per probe, a provider/name pair, the
// probe's address, an optional semaphore address, and a raw argument
// descriptor string. Attaching an eBPF uprobe at a probe requires the
// probe's *file offset*, not the virtual address the note carries, and
// cilium/ebpf's RefCtrOffset requires the semaphore's file offset too — both
// conversions are what this package exists to do correctly.
//
// This package does not parse argument descriptor strings (e.g. "8@%rdi");
// that is a separate concern left to callers that need it.
package usdt

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Probe describes one USDT probe recovered from an ELF's .note.stapsdt
// notes, with all addresses already resolved from link-time virtual
// addresses to file offsets.
type Probe struct {
	// Provider and Name identify the probe, e.g. "perfagent",
	// "gpu_launch_v1". Multiple probes may share a Provider/Name pair —
	// the compiler can duplicate an inlined call site into two notes with
	// identical identity and different locations. Callers must not
	// deduplicate on (Provider, Name).
	Provider string
	Name     string

	// Args is the raw, unparsed argument descriptor string (e.g.
	// "8@%rdi" or "-4@-24(%rbp)"). It is stored verbatim; parsing it is
	// out of scope for this package.
	Args string

	// Offset is the file offset at which to attach a uprobe for this
	// probe's location.
	Offset uint64

	// HasSemaphore reports whether the probe has an associated semaphore
	// (reference counter) that the kernel should maintain via
	// link.UprobeOptions.RefCtrOffset. When false, SemaphoreOffset is
	// meaningless: file offset 0 is a legitimate offset for a real
	// semaphore and must never be read as "no semaphore".
	HasSemaphore bool

	// SemaphoreOffset is the file offset of the semaphore counter. Only
	// meaningful when HasSemaphore is true.
	SemaphoreOffset uint64
}

// Errors returned (possibly wrapped) by Parse when an address recorded in a
// note cannot be resolved to a file offset.
var (
	// ErrNoLoadSegment means the address is not contained in any PT_LOAD
	// segment of the ELF.
	ErrNoLoadSegment = errors.New("usdt: address is not contained in any PT_LOAD segment")

	// ErrInBSS means the address falls within a PT_LOAD segment's memory
	// image but beyond its file-backed content (i.e. in .bss), so it has
	// no file offset.
	ErrInBSS = errors.New("usdt: address falls within .bss (beyond the segment's file size)")
)

const (
	noteOwnerStapsdt = "stapsdt"
	noteTypeStapsdt  = 3 // NT_STAPSDT

	stapsdtBaseSection = ".stapsdt.base"
	stapsdtSection     = ".note.stapsdt"
)

// ParseFile opens path and parses the USDT probes described by its
// .note.stapsdt notes. See Parse for behavior details.
func ParseFile(path string) ([]Probe, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("usdt: open %s: %w", path, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads .note.stapsdt notes from the ELF accessible via r and returns
// the USDT probes it describes.
//
// A well-formed ELF with no .note.stapsdt section is not an error: Parse
// returns a nil slice and a nil error, since most binaries have no USDT
// probes at all.
//
// A malformed or truncated note is an error. Parse never returns a partial
// or best-effort probe list alongside an error.
func Parse(r io.ReaderAt) ([]Probe, error) {
	ef, err := elf.NewFile(r)
	if err != nil {
		return nil, fmt.Errorf("usdt: opening ELF: %w", err)
	}
	// No defer ef.Close() here: elf.NewFile (unlike elf.Open) never takes
	// ownership of r or sets a closer, so Close would be a no-op -- Parse
	// doesn't own r's lifecycle, the caller does. ParseFile closes the
	// *os.File it opens itself, below.

	sec := ef.Section(stapsdtSection)
	if sec == nil {
		return nil, nil
	}

	data, err := sec.Data()
	if err != nil {
		return nil, fmt.Errorf("usdt: reading %s section data: %w", stapsdtSection, err)
	}

	addrSize, err := addrSizeForClass(ef.Class)
	if err != nil {
		return nil, err
	}

	notes, err := parseNotes(data, ef.ByteOrder)
	if err != nil {
		return nil, fmt.Errorf("usdt: parsing %s: %w", stapsdtSection, err)
	}

	// The prelink/base adjustment: a note's field 2 records the link-time
	// base address. If the ELF's actual base differs from that recorded
	// value, every address the note carries must be shifted by the
	// difference before it means anything. The standard signal for "the
	// base moved" is comparing the note's base field against the address
	// of the .stapsdt.base section, when that section exists. Absent that
	// section, no adjustment applies -- the note's addresses are used as
	// recorded.
	baseSec := ef.Section(stapsdtBaseSection)

	var probes []Probe
	for _, n := range notes {
		if n.typ != noteTypeStapsdt || n.owner != noteOwnerStapsdt {
			continue
		}

		desc, err := parseStapsdtDescriptor(n.desc, ef.ByteOrder, addrSize)
		if err != nil {
			return nil, fmt.Errorf("usdt: parsing stapsdt descriptor: %w", err)
		}

		var delta uint64
		if baseSec != nil {
			delta = baseSec.Addr - desc.base
		}

		loc := desc.location + delta
		offset, err := vaddrToFileOffset(ef, loc)
		if err != nil {
			return nil, fmt.Errorf("usdt: probe %s:%s: location %#x: %w", desc.provider, desc.name, loc, err)
		}

		p := Probe{
			Provider: desc.provider,
			Name:     desc.name,
			Args:     desc.args,
			Offset:   offset,
		}

		if desc.semaphore != 0 {
			semAddr := desc.semaphore + delta
			semOffset, err := vaddrToFileOffset(ef, semAddr)
			if err != nil {
				return nil, fmt.Errorf("usdt: probe %s:%s: semaphore %#x: %w", desc.provider, desc.name, semAddr, err)
			}
			p.HasSemaphore = true
			p.SemaphoreOffset = semOffset
		}

		probes = append(probes, p)
	}

	return probes, nil
}

// addrSizeForClass returns the pointer size, in bytes, that stapsdt note
// descriptors use for this ELF's class. Notes always encode addresses at
// the target's native pointer width, which is independent of the host
// running this parser.
func addrSizeForClass(class elf.Class) (int, error) {
	switch class {
	case elf.ELFCLASS32:
		return 4, nil
	case elf.ELFCLASS64:
		return 8, nil
	default:
		return 0, fmt.Errorf("usdt: unsupported ELF class %v", class)
	}
}

// vaddrToFileOffset converts a virtual address to a file offset by finding
// the PT_LOAD segment whose memory image contains it.
//
// An address outside every PT_LOAD segment is ErrNoLoadSegment. An address
// inside a segment's memory image but beyond its file-backed bytes (i.e.
// the zero-filled .bss tail present when Memsz > Filesz) is ErrInBSS: both
// are reported as errors rather than a wrong-but-plausible offset.
func vaddrToFileOffset(ef *elf.File, addr uint64) (uint64, error) {
	for _, p := range ef.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if addr < p.Vaddr {
			continue
		}
		off := addr - p.Vaddr
		if off >= p.Memsz {
			continue // not covered by this segment at all
		}
		if off >= p.Filesz {
			return 0, ErrInBSS
		}
		return p.Off + off, nil
	}
	return 0, ErrNoLoadSegment
}

// stapsdtDescriptor is the parsed form of one NT_STAPSDT note descriptor,
// with addresses still in link-time (unadjusted) form.
type stapsdtDescriptor struct {
	location  uint64
	base      uint64
	semaphore uint64
	provider  string
	name      string
	args      string
}

// parseStapsdtDescriptor decodes a stapsdt note descriptor: three
// pointer-sized addresses (location, base, semaphore) followed by three
// NUL-terminated strings (provider, name, argument descriptor), all in the
// ELF's own byte order and pointer width.
func parseStapsdtDescriptor(desc []byte, order binary.ByteOrder, addrSize int) (stapsdtDescriptor, error) {
	need := addrSize * 3
	if len(desc) < need {
		return stapsdtDescriptor{}, fmt.Errorf("descriptor too short: need at least %d bytes for three addresses, have %d", need, len(desc))
	}

	readAddr := func(b []byte) uint64 {
		if addrSize == 4 {
			return uint64(order.Uint32(b))
		}
		return order.Uint64(b)
	}

	loc := readAddr(desc[0*addrSize : 1*addrSize])
	base := readAddr(desc[1*addrSize : 2*addrSize])
	sem := readAddr(desc[2*addrSize : 3*addrSize])
	rest := desc[3*addrSize:]

	provider, rest, err := readCString(rest)
	if err != nil {
		return stapsdtDescriptor{}, fmt.Errorf("reading provider name: %w", err)
	}
	name, rest, err := readCString(rest)
	if err != nil {
		return stapsdtDescriptor{}, fmt.Errorf("reading probe name: %w", err)
	}
	args, _, err := readCString(rest)
	if err != nil {
		return stapsdtDescriptor{}, fmt.Errorf("reading argument descriptor: %w", err)
	}

	return stapsdtDescriptor{
		location:  loc,
		base:      base,
		semaphore: sem,
		provider:  provider,
		name:      name,
		args:      args,
	}, nil
}

// readCString reads a NUL-terminated string from the start of b, returning
// the string (without the terminator) and the remainder of b after it. It
// is an error for b to contain no NUL byte.
func readCString(b []byte) (string, []byte, error) {
	idx := bytes.IndexByte(b, 0)
	if idx < 0 {
		return "", nil, errors.New("unterminated string (no NUL byte)")
	}
	return string(b[:idx]), b[idx+1:], nil
}

// rawNote is one decoded ELF note: an owner name, a type, and the raw
// descriptor bytes, with no interpretation of the descriptor's contents.
type rawNote struct {
	owner string
	typ   uint32
	desc  []byte
}

// parseNotes decodes the standard ELF note container format (as used by
// SHT_NOTE sections): a sequence of records, each with a 4-byte-aligned
// name field and a 4-byte-aligned descriptor field. This alignment is fixed
// at 4 bytes regardless of the ELF's class, matching what both GCC/gas and
// systemtap's sys/sdt.h emit for .note.stapsdt.
func parseNotes(data []byte, order binary.ByteOrder) ([]rawNote, error) {
	const headerSize = 12 // namesz, descsz, type: three uint32 fields

	var notes []rawNote
	for len(data) > 0 {
		if len(data) < headerSize {
			return nil, fmt.Errorf("truncated note header: %d byte(s) remaining, need %d", len(data), headerSize)
		}
		namesz := order.Uint32(data[0:4])
		descsz := order.Uint32(data[4:8])
		typ := order.Uint32(data[8:12])
		data = data[headerSize:]

		nameLen := align4(namesz)
		if uint64(len(data)) < nameLen {
			return nil, fmt.Errorf("truncated note name: need %d byte(s), have %d", nameLen, len(data))
		}
		var owner string
		if namesz > 0 {
			// The owner field is a NUL-terminated string padded with
			// further NULs to the 4-byte boundary; strip all trailing
			// NULs to recover it.
			owner = string(bytes.TrimRight(data[:namesz], "\x00"))
		}
		data = data[nameLen:]

		descLen := align4(descsz)
		if uint64(len(data)) < descLen {
			return nil, fmt.Errorf("truncated note descriptor: need %d byte(s), have %d", descLen, len(data))
		}
		desc := data[:descsz]
		data = data[descLen:]

		notes = append(notes, rawNote{owner: owner, typ: typ, desc: desc})
	}
	return notes, nil
}

// align4 rounds n up to the next multiple of 4. The addition is done in
// uint64 so a maliciously large namesz/descsz (e.g. 0xffffffff) cannot wrap
// around in 32-bit arithmetic and produce a too-small aligned length.
func align4(n uint32) uint64 {
	return (uint64(n) + 3) &^ 3
}
