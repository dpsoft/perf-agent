// Package elfsym provides ELF symbol resolution and SONAME parsing primitives
// shared across language-specific injectors (inject/python, future inject/nodejs,
// etc.). Pure stdlib (debug/elf + regexp); no CGO, no external dependencies.
package elfsym

import (
	"path/filepath"
	"regexp"
	"strconv"
)

// libpythonRE matches typical CPython library SONAMEs, e.g.:
//
//	libpython3.12.so
//	libpython3.12.so.1.0
//	libpython2.7.so
//
// It is deliberately liberal — caller decides whether the version is
// acceptable via IsPython312Plus.
var libpythonRE = regexp.MustCompile(`^libpython(\d+)\.(\d+)\.so(?:\..*)?$`)

// ParseLibpythonSONAME extracts the major and minor version from a libpython
// shared-library path. It accepts either a bare basename or a full path; only
// the basename is matched against the libpython regex. Returns (major, minor,
// true) on a successful match; (0, 0, false) otherwise.
func ParseLibpythonSONAME(path string) (major, minor int, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	base := filepath.Base(path)
	m := libpythonRE.FindStringSubmatch(base)
	if m == nil {
		return 0, 0, false
	}
	maj, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// IsPython312Plus reports whether the given (major, minor) version is at least
// 3.12 — the minimum CPython version that ships sys.activate_stack_trampoline.
func IsPython312Plus(major, minor int) bool {
	if major > 3 {
		return true
	}
	if major == 3 && minor >= 12 {
		return true
	}
	return false
}
