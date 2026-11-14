package profilemodel

import (
	"path"
	"strconv"
)

// Sentinel mapping files perf-agent writes for code that has no backing
// file: kernel frames and JIT-compiled frames.
const (
	sentinelKernel = "[kernel]"
	sentinelJIT    = "[jit]"
)

// ModuleRef identifies the object a frame came from.
//
// There are three kinds, and every key below is chosen per kind:
//   - unmapped: File == "" (no mapping, or a mapping with an empty file,
//     which is where perf-agent puts GPU synthetic and fallback frames).
//     BuildID and Sentinel are then always "".
//   - sentinel: Sentinel != "" ("[kernel]" or "[jit]"); File holds the same
//     string, as the profile does.
//   - real: any other non-empty File.
type ModuleRef struct {
	File     string // "" = no mapping (nil, or Mapping.File == "")
	BuildID  string
	Sentinel string // "[kernel]" or "[jit]" when the mapping file is one of them
}

func moduleOf(file, buildID string) ModuleRef {
	if file == "" {
		return ModuleRef{}
	}
	m := ModuleRef{File: file, BuildID: buildID}
	if file == sentinelKernel || file == sentinelJIT {
		m.Sentinel = file
	}
	return m
}

// Key is the module's exact identity: `b:"<build id>"` when there is a
// build id (the same binary at two paths is one module), else
// `s:"<sentinel>"`, else `f:"<full file path>"`, else "" for no module.
// Every string is Go-quoted, so no two modules share a key.
func (m ModuleRef) Key() string {
	switch {
	case m.File == "":
		return ""
	case m.BuildID != "":
		return "b:" + strconv.Quote(m.BuildID)
	case m.Sentinel != "":
		return "s:" + strconv.Quote(m.Sentinel)
	default:
		return "f:" + strconv.Quote(m.File)
	}
}

// LocationID is one PC at one inline level. Offset is Location.Address as
// the profile stores it, which perf-agent writes relative to the mapping's
// file. Inline is the inline depth: 0 for the outermost function at the PC
// (pprof's last Line), 1 for the function inlined into it, and so on.
type LocationID struct {
	Module ModuleRef
	Offset uint64
	Inline int
}

// FunctionID identifies a function for the node tree.
type FunctionID struct {
	Module    ModuleRef
	Name      string // display name, same rules as foldedstacks.LineName/AddressName
	Filename  string // used only when Module.File == "" and no sentinel
	StartLine int64  // likewise
}

// unmapped reports whether the function has no module, so Filename and
// StartLine are part of its identity.
func (f FunctionID) unmapped() bool { return f.Module.File == "" }

func (f FunctionID) unmappedKey() string {
	return "-|" + strconv.Quote(f.Name) + "|" + strconv.Quote(f.Filename) + "|" + strconv.FormatInt(f.StartLine, 10)
}

// Key is the exact identity used for tree nodes:
//
//	b:"<build id>"|"<name>"           real or sentinel module with a build id
//	s:"<sentinel>"|"<name>"           sentinel module without one
//	f:"<full path>"|"<name>"          real module without one
//	-|"<name>"|"<filename>"|<start>   no module
func (f FunctionID) Key() string {
	if f.unmapped() {
		return f.unmappedKey()
	}
	return f.Module.Key() + "|" + strconv.Quote(f.Name)
}

// NameKey matches functions across profiles by name, per module kind:
//
//	m:"<base(file)>"|"<name>"         real module (build id and directory ignored)
//	s:"<sentinel>"|"<name>"           sentinel module
//	-|"<name>"|"<filename>"|<start>   no module (same as Key)
func (f FunctionID) NameKey() string {
	switch {
	case f.unmapped():
		return f.unmappedKey()
	case f.Module.Sentinel != "":
		return "s:" + strconv.Quote(f.Module.Sentinel) + "|" + strconv.Quote(f.Name)
	default:
		return "m:" + strconv.Quote(path.Base(f.Module.File)) + "|" + strconv.Quote(f.Name)
	}
}

// PathKey is `"<full file path>"|"<name>"`. ok is false, and the key "",
// unless the module is a real file: not unmapped and not a sentinel.
func (f FunctionID) PathKey() (string, bool) {
	if f.unmapped() || f.Module.Sentinel != "" {
		return "", false
	}
	return strconv.Quote(f.Module.File) + "|" + strconv.Quote(f.Name), true
}

// NodeID is the first 16 lowercase hex characters of the SHA-256 of the
// root-to-node FunctionID.Key() sequence, each key followed by a NUL byte.
// The synthetic root hashes the empty sequence. Keys are Go-quoted and so
// never contain a raw NUL, which makes the encoding unambiguous.
type NodeID string
