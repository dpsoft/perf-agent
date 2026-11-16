package perfmcp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// Root kinds, as error messages name them.
const (
	rootProfiles = "profiles"
	rootSource   = "source"
	rootOutput   = "output"
)

// openRoot opens dir as an os.Root under its absolute path, so the root's
// Name is the directory absolute paths are made relative to.
func openRoot(kind, dir string) (*os.Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("perfmcp: %s root %s: %w", kind, dir, err)
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("perfmcp: cannot open the %s root %s: %w", kind, abs, err)
	}
	return r, nil
}

// rootKind names root for messages.
func (s *Server) rootKind(root *os.Root) string {
	switch root {
	case s.profiles:
		return rootProfiles
	case s.output:
		return rootOutput
	}
	return rootSource
}

// rel turns p into a path relative to root. A relative path is used as is;
// an absolute one must lie lexically inside the root's directory, as the
// server was given it or with its symlinks resolved (the server's own
// path, never the agent's), and is made relative to it. Anything else is
// refused by its text alone: the agent's path is never resolved, so naming
// a directory elsewhere (a missing one, an automount, a slow mount) touches
// no filesystem. This is only the translation: os.Root enforces the root
// when the path is opened, so ".." and symlinks that leave the root, and
// symlinks inside it, are judged there.
func (s *Server) rel(root *os.Root, p string) (string, error) {
	kind := s.rootKind(root)
	if p == "" {
		return "", fmt.Errorf("path is empty; pass a path inside the %s root %s", kind, root.Name())
	}
	if !filepath.IsAbs(p) {
		return p, nil
	}
	dirs := []string{root.Name()}
	if real, err := s.evalSymlinks(root.Name()); err == nil && real != root.Name() {
		dirs = append(dirs, real)
	}
	for _, dir := range dirs {
		if r, err := filepath.Rel(dir, p); err == nil && filepath.IsLocal(r) {
			return r, nil
		}
	}
	return "", outsideRoot(kind, root.Name(), p)
}

func outsideRoot(kind, dir, p string) error {
	return fmt.Errorf("path %q is outside the %s root %s; pass a path inside it", echo(p), kind, dir)
}

// openRegular opens name inside root for reading and refuses anything but a
// regular file. The open is non-blocking, so a named pipe neither blocks the
// open nor a later read; the type comes from the opened handle, so nothing
// can be swapped in between the check and the read. noun is how the
// message names the argument ("path", "execlog").
func openRegular(root *os.Root, kind, noun, name string) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, openError(kind, root.Name(), noun, name, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("cannot stat %s %q under the %s root %s: %w", noun, echo(name), kind, root.Name(), pathless(err))
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s %q under the %s root %s is not a regular file (%s); pass a profile or executions log file",
			noun, echo(name), kind, root.Name(), describeType(fi.Mode()))
	}
	return f, fi, nil
}

// openError says why name could not be opened inside the root and what to
// pass instead.
func openError(kind, dir, noun, name string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s %q not found under the %s root %s; pass a path inside it (relative to it, or absolute)", noun, echo(name), kind, dir)
	case escapesRoot(err):
		return outsideRoot(kind, dir, name)
	}
	return fmt.Errorf("cannot open %s %q under the %s root %s: %w", noun, echo(name), kind, dir, pathless(err))
}

// pathless is err without the names a PathError or LinkError carries, so a
// message names the agent's path once, bounded by echo, and not again whole.
func pathless(err error) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return pe.Err
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err
	}
	return err
}

// escapesRoot reports whether err is os.Root's refusal of a path (through
// "..", or a symlink) that leaves the root. The stdlib does not export that
// error, so this matches its wording, "path escapes from parent", on the
// PathError's own error only: the path in the message is the agent's and
// may itself contain the word.
func escapesRoot(err error) bool {
	pe, ok := errors.AsType[*os.PathError](err)
	return ok && pe.Err != nil && pe.Err.Error() == "path escapes from parent"
}

func describeType(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeDevice != 0:
		return "a device"
	case m.IsRegular():
		return "a regular file"
	}
	return "mode " + m.Type().String()
}

// echo is s as a message may repeat an agent's argument: cut to 64
// characters, ending in "…" when cut.
func echo(s string) string { return capture.Echo(s) }
