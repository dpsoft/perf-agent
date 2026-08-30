package pyunwind

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
)

// ErrUnsupportedVersion is returned for an interpreter we detected but will
// not walk. It is deliberately distinct from "not an interpreter": an
// operator seeing Python frames missing deserves to know we found 3.11 and
// declined, not that we found nothing.
var ErrUnsupportedVersion = errors.New("pyunwind: unsupported CPython version")

// ErrVersionMismatch means a binary's own PY_VERSION_HEX disagrees with the
// version in its path. The offset table is chosen by minor version, so a
// libpython3.12.so.1.0 that is really 3.13 would be walked with 3.12's
// _PyInterpreterFrame layout -- a plausible stack of wrong frames, which is
// the failure this package exists to refuse. Seen in the wild when a build
// system copies or symlinks an interpreter under a version-suffixed name.
var ErrVersionMismatch = errors.New("pyunwind: the binary's PY_VERSION_HEX disagrees with its path")

// MicroUnknown is Version.Micro when nothing measured it.
//
// It exists because this package derives a version two ways and only one of
// them carries a micro: a soname says "3.12" and nothing more, while
// PY_VERSION_HEX says 3.12.3. Printing a zero for the unmeasured case is
// not a harmless default -- "CPython 3.12.0" is a real version, and the one
// field an operator uses to identify which interpreter a run actually
// walked would then name a build that was never on the machine. Ubuntu
// noble's /usr/bin/python3.12, which is 3.12.3, reported as 3.12.0 for
// exactly this reason.
const MicroUnknown = -1

type Version struct{ Major, Minor, Micro int }

// String renders "3.12.3" when the micro version was measured and "3.12.x"
// when it was not. See MicroUnknown.
func (v Version) String() string {
	if v.Micro == MicroUnknown {
		return fmt.Sprintf("%d.%d.x", v.Major, v.Minor)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Micro)
}

// Supported reports whether this build has an offset table and a TSS parser
// that covers it. 3.12 is the floor: 3.11 has no _PyThreadState_GetCurrent
// and a different PyGILState shape (see the spec's non-goals).
func (v Version) Supported() bool { return v.Major == 3 && v.Minor >= 12 && v.Minor <= 14 }

var sonameRe = regexp.MustCompile(`(?:libpython|python)(\d+)\.(\d+)`)

// DetectFromSoname reads the version out of a mapped path. Cheap, needs no
// I/O at all, and works on stripped binaries, which is why it is tried
// first. The micro version is not in a soname, so it comes back
// MicroUnknown; VersionFromELF measures it.
func DetectFromSoname(path string) (Version, bool) {
	m := sonameRe.FindStringSubmatch(path)
	if m == nil {
		return Version{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return Version{}, false
	}
	return Version{Major: major, Minor: minor, Micro: MicroUnknown}, true
}

// DetectFromPyVersionHex decodes PY_VERSION_HEX: one byte each of major,
// minor and micro, then a release-level nibble pair this package ignores.
func DetectFromPyVersionHex(hex uint32) Version {
	return Version{
		Major: int((hex >> 24) & 0xff),
		Minor: int((hex >> 16) & 0xff),
		Micro: int((hex >> 8) & 0xff),
	}
}

// pyVersionSymbol is CPython's own exported copy of PY_VERSION_HEX:
//
//	const unsigned long Py_Version = PY_VERSION_HEX;   (Python/getversion.c)
//
// It is a GLOBAL dynamic symbol on every build measured -- Debian's and
// Fedora's stripped libpythons, actions/setup-python's, Ubuntu's shared
// library and its statically linked /usr/bin/python3.12 -- so unlike
// _PyEval_EvalFrameDefault's dispatch loop it survives stripping, and
// unlike the soname it cannot be renamed.
const pyVersionSymbol = "Py_Version"

// VersionFromELF reads a binary's own PY_VERSION_HEX. This is the measured
// version, as against the one parsed out of a filename.
//
// It is what makes DetectFromPyVersionHex's stated purpose real: an
// interpreter whose path carries no version at all -- /usr/local/bin/python,
// a pyenv shim, a conda environment, an embedder that dlopens a
// non-versioned libpython -- is identifiable this way and by no other means
// short of reading the target's memory.
//
// The value is read out of the FILE, not the process: it is a const in
// .rodata, so the two are identical, and this needs no access to the target.
func VersionFromELF(path string) (Version, error) {
	osf, err := os.Open(path)
	if err != nil {
		return Version{}, fmt.Errorf("pyunwind: open %s: %w", path, err)
	}
	defer func() { _ = osf.Close() }()
	f, err := elf.NewFile(osf)
	if err != nil {
		return Version{}, fmt.Errorf("pyunwind: parse %s: %w", path, err)
	}

	var sym elf.Symbol
	found := false
	for _, get := range []func() ([]elf.Symbol, error){f.DynamicSymbols, f.Symbols} {
		syms, err := get()
		if err != nil {
			continue
		}
		for _, s := range syms {
			if s.Name == pyVersionSymbol && elf.ST_TYPE(s.Info) == elf.STT_OBJECT && s.Size >= 4 {
				sym, found = s, true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return Version{}, fmt.Errorf("%w: %s in %s", ErrSymbolNotFound, pyVersionSymbol, path)
	}

	off, err := fileOffsetFor(f, sym.Value)
	if err != nil {
		return Version{}, fmt.Errorf("pyunwind: %s in %s: %w", pyVersionSymbol, path, err)
	}
	// Four bytes, little-endian, of what is declared as an unsigned long:
	// PY_VERSION_HEX occupies the low 32 bits and the rest is zero.
	var raw [4]byte
	if _, err := osf.ReadAt(raw[:], int64(off)); err != nil {
		return Version{}, fmt.Errorf("pyunwind: read %s from %s: %w", pyVersionSymbol, path, err)
	}
	hex := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
	v := DetectFromPyVersionHex(hex)

	// A plausibility screen on a number read out of a data section: a real
	// PY_VERSION_HEX is 3.x.y with small components. Without it, a symbol
	// that happens to be named Py_Version in something that is not CPython
	// would be reported as a version rather than refused.
	if v.Major < 2 || v.Major > 9 || v.Minor > 99 || v.Micro > 99 {
		return Version{}, fmt.Errorf("%w: %s in %s reads %#08x, which is not a plausible PY_VERSION_HEX",
			ErrNotAnInterpreter, pyVersionSymbol, path, hex)
	}
	return v, nil
}
