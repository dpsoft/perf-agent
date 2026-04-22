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

// Compile reads the ELF at elfPath and produces flat CFI + Classification
// tables. Both slices are sorted by PCStart. Adjacent rows with identical
// rules are coalesced at emission time.
//
// The ELF's machine type (x86_64 vs aarch64) is auto-detected and the
// appropriate archInfo is used for register-number translation.
//
// Not safe for concurrent calls per instance; callers should serialize.
func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, err error) {
	f, err := elf.Open(elfPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open elf: %w", err)
	}
	defer f.Close()

	arch, err := archFromELFMachine(f.Machine)
	if err != nil {
		return nil, nil, err
	}

	sec := f.Section(".eh_frame")
	if sec == nil {
		return nil, nil, ErrNoEHFrame
	}
	data, err := sec.Data()
	if err != nil {
		return nil, nil, fmt.Errorf("read .eh_frame: %w", err)
	}
	sectionPos := sec.Addr

	var allEntries []CFIEntry
	var allClasses []Classification

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
		interp.lastEmittedPC = fd.initialLocation
		if err := interp.run(fd.initialLocation, fd.initialLocation+fd.addressRange, fd.instructions); err != nil {
			return fmt.Errorf("FDE at PC 0x%x: %w", fd.initialLocation, err)
		}
		allEntries = append(allEntries, interp.entries...)
		allClasses = append(allClasses, interp.classifications...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	sort.Slice(allEntries, func(i, j int) bool { return allEntries[i].PCStart < allEntries[j].PCStart })
	sort.Slice(allClasses, func(i, j int) bool { return allClasses[i].PCStart < allClasses[j].PCStart })

	return allEntries, allClasses, nil
}
