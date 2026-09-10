package flamegraph

import (
	"strings"

	"github.com/dpsoft/perf-agent/internal/framename"
)

// Resolution is how well a frame could be named, which is a DIFFERENT question
// from what kind of code it is.
//
// The two were conflated: "unsym" is a Domain, so a frame's colour answered
// "what is this" for named frames and "we could not name it" for the rest, and
// the reader could not ask why. Measured on one PyTorch MNIST capture, the
// frames we call unnamed are three distinct facts:
//
//	45  module+offset  the mapping is known; the offset is ASLR-stable and
//	                   can be matched against symbol data for that build id
//	10  bare address   not even the mapping is known; nothing to look up
//	30  obfuscated     symbolized, from NVIDIA's own symbol server, but the
//	                   name is a stable opaque id rather than a human one --
//	                   NOT a perf-agent failure, and today it is coloured
//	                   exactly like a resolved frame
//
// Domain stays the colour. Resolution is the texture. A reader then sees what
// kind of code a frame is and how much we know about its name independently,
// which is what they were actually trying to read off one channel.
type Resolution uint8

const (
	// ResolutionResolved: a real symbol name.
	ResolutionResolved Resolution = iota
	// ResolutionModuleOffset: "libcudnn.so.9+0x824fbd". The unwind found the
	// frame and the profile knows which file the address fell in; no symbol
	// table covered that offset. Actionable: stable across ASLR, and matchable
	// against debug data for the same build id.
	ResolutionModuleOffset
	// ResolutionBareAddress: "0x7f2c945b2c2b". Not even the mapping is known.
	// Strictly less than ModuleOffset, and worth telling apart: one can be
	// looked up later, the other cannot.
	ResolutionBareAddress
	// ResolutionObfuscated: "libcupti_afe8ffb6…". NVIDIA's symbol server
	// serves these for the internals of the libraries it ships stripped --
	// their own documentation calls them obfuscated symbol names. The frame IS
	// symbolized and the id is stable and meaningful to NVIDIA in a bug
	// report; it simply is not a human-readable name.
	ResolutionObfuscated
	// ResolutionInterpreter: "python:0x26f84de0". An interpreter frame whose
	// code object could not be read -- the walk placed it correctly, and the
	// interpreter was one this build has no measured layout for. Distinct from
	// a bare address, which carries no such promise.
	ResolutionInterpreter
)

// ResolutionInfo describes a resolution for the legend and the detail panel.
type ResolutionInfo struct {
	Key   string
	Label string
	Desc  string
}

func (r Resolution) Info() ResolutionInfo {
	switch r {
	case ResolutionModuleOffset:
		return ResolutionInfo{"module-offset", "module + offset",
			"The unwind found this frame and the profile knows which file the address fell in, but no symbol table covered that offset. The offset is module-relative, so it is stable across ASLR and can be matched against debug or symbol data for this exact build id."}
	case ResolutionBareAddress:
		return ResolutionInfo{"bare-address", "address only",
			"Neither a symbol nor a mapping. The frame's position in the call path is real; nothing else about it is known, and unlike a module+offset there is nothing to look it up against later."}
	case ResolutionObfuscated:
		return ResolutionInfo{"obfuscated", "obfuscated symbol",
			"Symbolized, from NVIDIA's CUDA Toolkit Symbol Server. NVIDIA publishes obfuscated names for the internals of the libraries it ships stripped: the identifier is stable and is what NVIDIA can resolve from a bug report, but it is not a human-readable name. This is not a symbolization failure."}
	case ResolutionInterpreter:
		return ResolutionInfo{"interpreter", "interpreter frame, unnamed",
			"An interpreter frame placed correctly in the call path whose code object could not be read — usually an interpreter version this build has no measured layout for. Reading it requires the process to still be alive."}
	default:
		return ResolutionInfo{"resolved", "resolved", "A symbol table named this address."}
	}
}

// obfuscatedName matches NVIDIA's anonymized symbols: <library>_<40 hex>.
// Their own example is libcuda_8e2eae48ba8eb68460582f76460557784d48a71a.
func obfuscatedName(name string) bool {
	i := strings.LastIndexByte(name, '_')
	if i < 0 || !strings.HasPrefix(name, "lib") || len(name)-i-1 != 40 {
		return false
	}
	for _, c := range name[i+1:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ResolutionOf classifies a frame by its rendered name.
//
// From the name rather than from a new field, because the name is what the
// profile actually carries: every one of these forms is produced by a
// documented rendering (framename.Format, pyunwind.FrameName, the symbol
// server's own output), so reading them back is reading our own contract, not
// guessing.
func ResolutionOf(name string) Resolution {
	switch {
	case name == "":
		return ResolutionBareAddress
	case strings.HasPrefix(name, "python:0x"):
		return ResolutionInterpreter
	case framename.IsAddressOnly(name):
		// Both "libcuda.so.1+0x1b71c6" and "0x7f2c945b2c2b" land here; the
		// difference is whether a module is attached, and that is the
		// difference between a lookup that is possible later and one that
		// is not.
		if strings.Contains(name, "+0x") {
			return ResolutionModuleOffset
		}
		return ResolutionBareAddress
	case obfuscatedName(name):
		return ResolutionObfuscated
	default:
		return ResolutionResolved
	}
}
