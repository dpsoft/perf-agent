// Package framename owns the one textual form perf-agent uses for a stack
// frame that was unwound correctly but could not be given a symbol name.
//
// There are exactly two such forms, and the difference between them is
// load-bearing:
//
//	0x7f2c945b2c2b        no mapping covers this PC. Nothing is known about
//	                      it beyond the address, and the address is an
//	                      ASLR'd runtime VA that means nothing across runs.
//	libcuda.so.1+0x1b71c6 a mapping covers this PC. The module is known and
//	                      the offset is relative to that module's file, so
//	                      it is stable across runs and can be fed to
//	                      addr2line/objdump/nvdisasm later.
//
// The second form must never be synthesized from a guess: it is emitted only
// where a real /proc/<pid>/maps entry produced the module and the offset.
//
// The form is centralized here because three packages have to agree on it.
// pprof/ writes it into Function.Name; internal/foldedstacks counts frames
// that carry no symbol and warns about the proportion; internal/flamegraph
// colours them as their own domain. If any one of those stopped recognizing
// the form, the profile would look better exactly when symbolization worked
// worst - which is the failure mode this package exists to prevent.
package framename

import (
	"path"
	"strings"
)

// Format renders the module-relative form. modulePath is the mapping's file
// (the full path; only its base is shown), off is the offset of the PC
// within that file.
//
// Returns "" when modulePath is empty or is one of the bracketed sentinel
// "files" perf-agent uses for non-file mappings ("[kernel]", "[jit]").
// Callers must treat "" as "keep the bare address": inventing a module is
// worse than admitting there is none.
func Format(modulePath string, off uint64) string {
	if modulePath == "" || strings.HasPrefix(modulePath, "[") {
		return ""
	}
	b := path.Base(modulePath)
	if b == "" || b == "." || b == "/" {
		return ""
	}
	return b + "+0x" + hexOf(off)
}

// hexOf formats off as lowercase hex without the fmt package, which keeps
// this on the allocation budget the symbolize package asserts against.
func hexOf(off uint64) string {
	if off == 0 {
		return "0"
	}
	const digits = "0123456789abcdef"
	var buf [16]byte
	i := len(buf)
	for off > 0 {
		i--
		buf[i] = digits[off&0xf]
		off >>= 4
	}
	return string(buf[i:])
}

// IsAddressOnly reports whether name is one of the two unresolved forms:
// a bare "0x<hex>" address, or the "<module>+0x<hex>" module-relative form.
//
// It does NOT recognize the "[unknown]" placeholder - that string belongs to
// the package that emits it, and callers already test for it separately.
//
// The module half is required to look like a file name (non-empty, and free
// of the characters that appear in demangled C++ signatures) so that a real
// symbol containing "+0x" - an expression baked into a template argument, a
// demangled operator+ overload - is not mistaken for an unresolved frame.
//
// What this cannot rule out: a symbol whose whole name is an identifier, a
// "+", and hex digits, with no parenthesis, colon, space or bracket anywhere.
// Once the profile is written the pprof.Frame.Unresolved bit is gone and
// there is nothing else to consult. Such a symbol would be drawn as
// unsymbolized and counted in the symbolization-gap warning - erring towards
// over-reporting the gap, never towards hiding it.
func IsAddressOnly(name string) bool {
	if isHexAddr(name) {
		return true
	}
	plus := strings.LastIndexByte(name, '+')
	if plus <= 0 {
		return false
	}
	return looksLikeModuleBase(name[:plus]) && isHexAddr(name[plus+1:])
}

// Module splits the module-relative form back into its parts. ok is false
// for anything that is not that form, including the bare-address form -
// which is the point: a caller asking "which module is this frame in?" must
// get "none" for an address with no mapping, not a plausible-looking guess.
func Module(name string) (module string, ok bool) {
	plus := strings.LastIndexByte(name, '+')
	if plus <= 0 {
		return "", false
	}
	if !looksLikeModuleBase(name[:plus]) || !isHexAddr(name[plus+1:]) {
		return "", false
	}
	return name[:plus], true
}

func isHexAddr(s string) bool {
	if !strings.HasPrefix(s, "0x") || len(s) < 3 {
		return false
	}
	for i := 2; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// looksLikeModuleBase is deliberately strict. Format only ever produces
// path.Base of a mapping file, so anything with a separator, a space, or a
// character that only occurs in a demangled symbol is not one of ours.
func looksLikeModuleBase(s string) bool {
	if s == "" || strings.HasPrefix(s, "[") {
		return false
	}
	return !strings.ContainsAny(s, " \t/()<>,:*&[]")
}
