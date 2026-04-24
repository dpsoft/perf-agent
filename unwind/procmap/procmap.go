// Package procmap resolves addresses into per-binary mapping identity
// (path, start/limit, file offset, build-id) by parsing /proc/<pid>/maps
// and ELF .note.gnu.build-id sections. Results feed pprof.Mapping so
// downstream tools can round-trip samples back to ELF file offsets.
package procmap

// Mapping describes one executable range in a process's address space.
// Non-executable and anonymous ranges are dropped during parsing.
type Mapping struct {
	Path    string
	Start   uint64
	Limit   uint64 // exclusive
	Offset  uint64 // p_offset of the backing PT_LOAD segment
	BuildID string // hex; empty if no .note.gnu.build-id
	IsExec  bool
}
