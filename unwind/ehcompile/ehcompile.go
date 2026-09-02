package ehcompile

import (
	"debug/elf"
	"errors"
	"fmt"
	"sort"
)

// ErrNoEHFrame is returned when the ELF file has no usable .eh_frame section.
var ErrNoEHFrame = errors.New("ehcompile: no .eh_frame section")

// ErrUnsupportedArch is returned when the ELF's machine type is not
// x86_64 or arm64. Other architectures can be added later.
var ErrUnsupportedArch = errors.New("ehcompile: unsupported ELF machine type")

// Compile reads the ELF at elfPath and produces the flat CFI table, plus the
// size in bytes of the ELF's .eh_frame section. Entries are sorted by PCStart
// and adjacent rows with identical rules are coalesced at emission time.
//
// It used to return a parallel Classification table tagging each PC range
// FP_SAFE / FP_LESS / FALLBACK, which the walker searched to choose between
// the frame-pointer path and the DWARF path. The walker now makes that choice
// from whether a CFI ROW EXISTS, so the second table was computed on every
// enrol, uploaded, and never read -- 38 MB for libtorch_cpu alone. The pass is
// gone rather than discarded at the end: a range with no row IS the FALLBACK
// answer.
//
// ehFrameBytes is the raw .eh_frame section size before parsing — useful
// for cost analysis (per-byte compile rate) and observability hooks. It
// is reported even on parse errors after the section has been read; if
// the section is missing entirely (ErrNoEHFrame), ehFrameBytes is 0.
//
// The ELF's machine type (x86_64 vs aarch64) is auto-detected and the
// appropriate archInfo is used for register-number translation.
//
// Not safe for concurrent calls per instance; callers should serialize.
func Compile(elfPath string) (entries []CFIEntry, ehFrameBytes int, err error) {
	f, err := elf.Open(elfPath)
	if err != nil {
		return nil, 0, fmt.Errorf("open elf: %w", err)
	}
	defer func() { _ = f.Close() }()

	arch, err := archFromELFMachine(f.Machine)
	if err != nil {
		return nil, 0, err
	}

	sec := f.Section(".eh_frame")
	if sec == nil {
		return nil, 0, ErrNoEHFrame
	}
	data, err := sec.Data()
	if err != nil {
		return nil, 0, fmt.Errorf("read .eh_frame: %w", err)
	}
	ehFrameBytes = len(data)
	sectionPos := sec.Addr

	var allEntries []CFIEntry

	err = walkEHFrame(data, sectionPos, func(off uint64, c *cie, fd *fde) error {
		if fd == nil {
			return nil
		}
		interp := newInterpreter(fd.cie, arch)
		// CIE's initial instructions seed state without emitting rows
		// (they're evaluated with PC == initialLocation, which equals
		// the interpreter's lastEmittedPC, so snapshot() is a no-op).
		if err := interp.run(fd.initialLocation, fd.initialLocation, fd.cie.initialInstructions); err != nil {
			return fmt.Errorf("CIE init at PC 0x%x: %w", fd.initialLocation, err)
		}
		// DW_CFA_restore in the FDE reverts a register to the rule the CIE's
		// initial instructions left in place, not to an architectural
		// default (DWARF 5 §6.4.2.3). Seal that state here, between the two
		// runs, so the FDE's restores have something correct to revert to.
		interp.sealInitialRules()
		interp.lastEmittedPC = fd.initialLocation
		if err := interp.run(fd.initialLocation, fd.initialLocation+fd.addressRange, fd.instructions); err != nil {
			return fmt.Errorf("FDE at PC 0x%x: %w", fd.initialLocation, err)
		}
		allEntries = append(allEntries, interp.entries...)
		return nil
	})
	if err != nil {
		return nil, ehFrameBytes, err
	}

	sort.Slice(allEntries, func(i, j int) bool { return allEntries[i].PCStart < allEntries[j].PCStart })

	return allEntries, ehFrameBytes, nil
}
