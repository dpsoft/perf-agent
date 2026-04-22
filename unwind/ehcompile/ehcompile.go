package ehcompile

import (
	"errors"
)

// ErrNoEHFrame is returned when the ELF file has no usable .eh_frame section.
var ErrNoEHFrame = errors.New("ehcompile: no .eh_frame section")

// ErrUnsupportedArch is returned when the ELF's machine type is not
// x86_64 or arm64. Other architectures can be added later.
var ErrUnsupportedArch = errors.New("ehcompile: unsupported ELF machine type")

// Compile reads the ELF at elfPath and produces flat CFI + Classification
// tables. Both slices are sorted by PCStart. Adjacent rows with identical
// rules are coalesced.
//
// The ELF's machine type (x86_64 vs aarch64) is auto-detected and the
// appropriate archInfo is used for register-number translation.
//
// Not safe for concurrent calls per instance; callers should serialize.
func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, err error) {
	return nil, nil, errors.New("ehcompile: not implemented")
}
