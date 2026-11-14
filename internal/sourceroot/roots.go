// Package sourceroot finds the source files a profile names inside
// directories the user named, and says what ties each file to the profiled
// binary: a source manifest for its build, a clean git checkout, or nothing.
//
// Every file is opened through os.Root, so "..", absolute paths and symlinks
// cannot leave a root. A file is read once into a bounded snapshot; its
// evidence and the lines shown from it both come from that snapshot, so a
// file replaced afterwards never borrows the old file's verdict.
package sourceroot

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Limits bound the work and memory of a Roots.
type Limits struct {
	MaxFileBytes     int64 // bytes of one source file read into a snapshot
	MaxLineBytes     int   // bytes of one line returned by Lines
	MaxContext       int   // lines returned on each side of the asked one
	MaxWalkEntries   int   // directory entries one root's basename index may visit
	MaxManifestBytes int64 // bytes of one source manifest
	GitTimeout       time.Duration
}

// DefaultLimits are the limits the server uses unless told otherwise.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:     8 << 20,
		MaxLineBytes:     512,
		MaxContext:       50,
		MaxWalkEntries:   200000,
		MaxManifestBytes: 16 << 20,
		GitTimeout:       5 * time.Second,
	}
}

func (l Limits) validate() error {
	switch {
	case l.MaxFileBytes <= 0, l.MaxLineBytes <= 0, l.MaxContext < 0, l.MaxWalkEntries <= 0,
		l.MaxManifestBytes <= 0, l.GitTimeout <= 0:
		return fmt.Errorf("sourceroot: limits must be positive (MaxContext may be zero): %+v", l)
	}
	return nil
}

// Status says how a profile's file name was matched and what ties the
// matched file to the profiled binary.
type Status string

const (
	// StatusBuildVerified: a source manifest for the build or capture
	// lists the file with these exact bytes.
	StatusBuildVerified Status = "build_verified"
	// StatusUserSelected: the file is the one tracked at the HEAD commit of
	// a checkout the user chose; nothing ties that commit to the build.
	StatusUserSelected Status = "user_selected"
	// StatusSourceUnverified: one file matched, with no evidence it is the
	// source that was built; Reason says why.
	StatusSourceUnverified Status = "source_unverified"
	// StatusUnresolved: no file matched, or it could not be read.
	StatusUnresolved Status = "unresolved"
	// StatusAmbiguous: several files matched; Candidates lists them.
	StatusAmbiguous Status = "ambiguous"
)

// root is one opened source directory.
type root struct {
	dir   string // absolute, as opened
	real  string // dir with symlinks resolved, to compare with git's paths
	r     *os.Root
	index *basenameIndex // built on the first basename lookup
}

// Roots is a set of opened source directories. Its methods are safe for
// concurrent use.
type Roots struct {
	roots     []*root
	manifests []Manifest
	probe     Prober
	lim       Limits

	mu     sync.Mutex
	opened []os.FileInfo
}

// Open opens each directory as an os.Root. probe nil disables the git
// evidence; manifests are matched by build or capture ID.
func Open(dirs []string, manifests []Manifest, probe Prober, lim Limits) (*Roots, error) {
	if err := lim.validate(); err != nil {
		return nil, err
	}
	for i, m := range manifests {
		if err := m.validate(); err != nil {
			return nil, fmt.Errorf("sourceroot: manifest %d: %w", i, err)
		}
	}
	rs := &Roots{manifests: slices.Clone(manifests), probe: probe, lim: lim}
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			_ = rs.Close()
			return nil, fmt.Errorf("sourceroot: source root %q: %w", d, err)
		}
		r, err := os.OpenRoot(abs)
		if err != nil {
			_ = rs.Close()
			return nil, fmt.Errorf("sourceroot: source root: %w", err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			real = abs
		}
		rs.roots = append(rs.roots, &root{dir: abs, real: real, r: r})
	}
	return rs, nil
}

// Close closes every root.
func (rs *Roots) Close() error {
	var errs []error
	for _, r := range rs.roots {
		errs = append(errs, r.r.Close())
	}
	return errors.Join(errs...)
}

// Dirs returns the absolute directories, in the order given to Open.
func (rs *Roots) Dirs() []string {
	out := make([]string, len(rs.roots))
	for i, r := range rs.roots {
		out[i] = r.dir
	}
	return out
}

// Opened returns the FileInfo of every distinct file read into a snapshot,
// taken from the handle the bytes were read through, in first-read order.
func (rs *Roots) Opened() []os.FileInfo {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return slices.Clone(rs.opened)
}

func (rs *Roots) noteOpened(fi os.FileInfo) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !slices.ContainsFunc(rs.opened, func(o os.FileInfo) bool { return os.SameFile(o, fi) }) {
		rs.opened = append(rs.opened, fi)
	}
}

// snapshot is a file's bytes as read once, with their digest and the
// FileInfo of the handle they came from. It is never modified.
type snapshot struct {
	data      []byte // at most Limits.MaxFileBytes
	truncated bool   // the file was longer than data
	digest    [sha256.Size]byte
	info      os.FileInfo
}

// snapshot reads rel inside root i. The open is non-blocking and the type
// comes from the opened handle, so a FIFO or device, even one swapped in
// after the lookup, is refused without blocking.
func (rs *Roots) snapshot(i int, rel string) (*snapshot, error) {
	f, err := rs.roots[i].r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, rs.lim.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	s := &snapshot{info: fi}
	if int64(len(data)) > rs.lim.MaxFileBytes {
		s.data, s.truncated = data[:rs.lim.MaxFileBytes:rs.lim.MaxFileBytes], true
	} else {
		s.data = data
		s.digest = sha256.Sum256(data)
	}
	return s, nil
}

// holds reports whether dir (a path git printed) is the root or inside it.
func (r *root) holds(dir string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	rel, err := filepath.Rel(r.real, dir)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
