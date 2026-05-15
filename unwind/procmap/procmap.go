// Package procmap resolves addresses into per-binary mapping identity
// (path, start/limit, file offset, build-id) by parsing /proc/<pid>/maps
// and ELF .note.gnu.build-id sections. Results feed pprof.Mapping so
// downstream tools can round-trip samples back to ELF file offsets.
package procmap

import "os"

// Mapping describes one executable range in a process's address space.
// Non-executable and anonymous ranges are dropped during parsing.
type Mapping struct {
	Path string
	// MapFiles is /proc/<pid>/map_files/<start>-<limit>. Present even when
	// the symbolic Path is unreachable from the agent's mount namespace
	// (sidecar / deleted-binary cases). Empty when /proc/<pid>/map_files
	// is restricted by the kernel.
	MapFiles string
	Start    uint64
	Limit    uint64 // exclusive
	Offset   uint64 // p_offset of the backing PT_LOAD segment
	BuildID  string // hex; empty if no .note.gnu.build-id
	IsExec   bool
}

// OpenablePath returns the first openable path: MapFiles (preferred — works
// across mount namespaces and survives unlinked-but-mapped binaries) then
// the symbolic Path. Returns "" when neither is readable.
//
// Note: the probe (open + close) and the caller's subsequent use of the
// returned path are not atomic. The file may be unlinked or become
// unreadable between the two. Callers must still handle errors from
// os.Open (or equivalent) on the returned path themselves.
func (m Mapping) OpenablePath() string {
	for _, p := range [2]string{m.MapFiles, m.Path} {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_ = f.Close()
		return p
	}
	return ""
}

// Option configures a Resolver.
type Option func(*resolverConfig)

type resolverConfig struct {
	procRoot string
}

// WithProcRoot overrides the filesystem root used to resolve /proc
// paths. Defaults to "/proc". Intended for unit tests with fake
// per-PID fixtures.
func WithProcRoot(path string) Option {
	return func(c *resolverConfig) { c.procRoot = path }
}
