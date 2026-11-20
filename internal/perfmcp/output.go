package perfmcp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// errRenderDisabled is every render call's answer on a server without an
// output root.
func (s *Server) errRenderDisabled() error {
	return errors.New("DISABLED: " + s.startAdvice("--output DIR to enable rendering. ") +
		"This server has no output root, so it writes no pages")
}

// pageExt is the extension every page's name must end in.
const pageExt = ".html"

// maxTempTries bounds the temporary names tried before giving up.
const maxTempTries = 8

// A page's temporary is ".<base>.tmp-<16 hex>", tempExtra bytes longer
// than the page's own file name, and both must fit the maxFileName bytes a
// file name has on common filesystems.
const (
	tempExtra   = len(".") + len(".tmp-") + 16
	maxFileName = 255
	maxPageBase = maxFileName - tempExtra
)

// writePage writes page as out inside the output root and returns its
// absolute path. out is relative to the root, or absolute inside it, and
// must end in .html in a directory that exists. Nothing is ever replaced:
// a name that exists already (a page this server wrote included) is
// refused, as is a symbolic link there, and a file that is one of the
// server's inputs is named as such. The page goes to a temporary created
// exclusively beside out and is published with a hard link, which fails
// rather than replaces when something took the name meanwhile; the
// temporary is removed on every path. A filesystem that refuses hard links
// gets no page: there is no publication that is both atomic and unable to
// replace a file without one.
func (s *Server) writePage(out string, page []byte) (string, error) {
	name, err := s.pageName(out)
	if err != nil {
		return "", err
	}
	if _, err := s.publishNew(s.output, rootOutput, filepath.Dir(name), filepath.Base(name), page, 0o644); err != nil {
		return "", err
	}
	return filepath.Join(s.output.Name(), name), nil
}

// publishNew writes b as base in dir inside root, kind naming the root in
// messages, and returns its name inside root. It never replaces anything:
// the bytes go to a temporary created exclusively beside the name (see
// writeTemp), which is published with linkNoReplace and removed on every
// path. A name that is taken is an error wrapping fs.ErrExist. The file
// gets perm, less the umask.
func (s *Server) publishNew(root *os.Root, kind, dir, base string, b []byte, perm fs.FileMode) (string, error) {
	name := filepath.Join(dir, base)
	tmp, err := writeTemp(root, kind, dir, base, b, perm)
	if tmp != "" {
		defer func() { _ = root.Remove(tmp) }()
	}
	if err != nil {
		return "", err
	}
	if s.beforePublish != nil {
		s.beforePublish(name)
	}
	if err := s.linkNoReplace(root, kind, tmp, name); err != nil {
		return "", err
	}
	return name, nil
}

// takenError is a refusal to replace a name; it is fs.ErrExist to errors.Is.
type takenError struct{ error }

func (takenError) Is(target error) bool { return target == fs.ErrExist }

// linkNoReplace publishes tmp as name inside root with a hard link, which
// fails rather than replaces when the name is taken; that error wraps
// fs.ErrExist. kind names the root in messages, and the flag that moves it
// in the advice for a filesystem without hard links.
func (s *Server) linkNoReplace(root *os.Root, kind, tmp, name string) error {
	err := s.link(root, tmp, name)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist) && kind == rootOutput:
		return takenError{existsError(name, root.Name())}
	case errors.Is(err, fs.ErrExist):
		return takenError{fmt.Errorf("%s already exists under the %s root and is never replaced", echo(name), kind)}
	case kind == rootOutput:
		return fmt.Errorf("cannot publish %s under the output root %s with a hard link: %v; "+
			"%[4]s publishes a page only with a hard link, which never replaces a file, so it wrote no page. "+
			"Start %[4]s with --output on a filesystem that supports hard links", echo(name), root.Name(), pathless(err), s.command())
	}
	// The message names the final name and the root by its kind only: the
	// temporary carries an owner token, and the root's path is the
	// server's.
	return wrapped{fmt.Sprintf("cannot publish %s under the %s root with a hard link: %s; "+
		"%[4]s publishes a capture's files only with a hard link, which never replaces a file, so it wrote nothing. "+
		"Start %[4]s with --captures on a filesystem that supports hard links", echo(name), kind, errnoText(err), s.command()), err}
}

// wrapped is an error with its own text that still unwraps to its cause,
// so a caller can tell an errno from it.
type wrapped struct {
	text string
	err  error
}

func (w wrapped) Error() string { return w.text }
func (w wrapped) Unwrap() error { return w.err }

// errnoNames name the errors a file operation in the capture directory
// commonly fails with. A slice, since some are one value on one system
// and two on another.
var errnoNames = []struct {
	errno syscall.Errno
	name  string
}{
	{syscall.EPERM, "EPERM"}, {syscall.EOPNOTSUPP, "EOPNOTSUPP"}, {syscall.ENOTSUP, "ENOTSUP"},
	{syscall.EXDEV, "EXDEV"}, {syscall.ENOSPC, "ENOSPC"}, {syscall.EDQUOT, "EDQUOT"},
	{syscall.EMLINK, "EMLINK"}, {syscall.EACCES, "EACCES"}, {syscall.EROFS, "EROFS"},
	{syscall.EIO, "EIO"}, {syscall.EEXIST, "EEXIST"}, {syscall.ENOENT, "ENOENT"},
}

// errnoText is err without its path names, followed by its errno's name
// when it has one: "operation not permitted (EPERM)".
func errnoText(err error) string {
	text := pathless(err).Error()
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		for _, e := range errnoNames {
			if e.errno == errno {
				return text + " (" + e.name + ")"
			}
		}
	}
	return text
}

// pageName is out as a name inside the output root, refusing one that
// leaves it, does not end in .html, is not in an existing directory, or
// names anything that exists.
func (s *Server) pageName(out string) (string, error) {
	if s.output == nil {
		return "", s.errRenderDisabled()
	}
	root := s.output
	for part := range strings.SplitSeq(filepath.ToSlash(out), "/") {
		if part == ".." {
			return "", fmt.Errorf("out %q has a \"..\" component; pass a path inside the output root %s without \"..\"", echo(out), root.Name())
		}
	}
	name, err := s.rel(root, out)
	if err != nil {
		return "", err
	}
	name = filepath.Clean(name)
	if !filepath.IsLocal(name) {
		return "", outsideRoot(rootOutput, root.Name(), out)
	}
	base := filepath.Base(name)
	switch {
	case filepath.Ext(name) != pageExt:
		return "", fmt.Errorf("out %q does not end in %s; pass a page name such as profile.html inside the output root %s", echo(out), pageExt, root.Name())
	case base == pageExt:
		return "", fmt.Errorf("out %q has no name before %s; pass a page name such as profile.html inside the output root %s", echo(out), pageExt, root.Name())
	case len(base) > maxPageBase:
		return "", fmt.Errorf("out %q has a %d-byte file name; the page is written beside a temporary whose name is %d bytes longer, "+
			"and a file name is at most %d bytes, so choose a shorter name (at most %d bytes)", echo(out), len(base), tempExtra, maxFileName, maxPageBase)
	}
	dir := filepath.Dir(name)
	fi, err := root.Stat(dir)
	switch {
	case err != nil:
		return "", openError(rootOutput, root.Name(), "the directory of out", dir, err)
	case !fi.IsDir():
		return "", fmt.Errorf("the directory of out %q is %s under the output root %s, not a directory; pass a name in an existing directory",
			echo(out), describeType(fi.Mode()), root.Name())
	}
	if err := s.refuseExisting(name); err != nil {
		return "", err
	}
	return name, nil
}

// refuseExisting refuses name when anything is there: a symbolic link, one
// of the server's inputs, or any other file.
func (s *Server) refuseExisting(name string) error {
	root := s.output
	fi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return openError(rootOutput, root.Name(), "out", name, err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("out %s under the output root %s is a symbolic link; refusing to write through it; choose a new file name", echo(name), root.Name())
	case s.isInput(fi):
		return fmt.Errorf("refusing to overwrite input %s: out %s under the output root %s is a file this server read; choose a new file name",
			echo(name), echo(name), root.Name())
	}
	return existsError(name, root.Name())
}

func existsError(name, dir string) error {
	return fmt.Errorf("out %s already exists under the output root %s and is never replaced; choose a new file name", echo(name), dir)
}

// writeTemp writes b to a new file ".<base>.tmp-<16 hex>" in dir inside
// root, with perm less the umask, created exclusively and written and
// synced through the handle that created it, so the bytes go to a new file
// nobody else holds and never through a name something else could have
// replaced. kind names the root in messages: in the output root what is
// written is a page, elsewhere a file. It returns the temporary's name,
// which the caller removes, even on error.
func writeTemp(root *os.Root, kind, dir, base string, b []byte, perm fs.FileMode) (string, error) {
	what := "file"
	if kind == rootOutput {
		what = "page"
	}
	var r [8]byte
	for range maxTempTries {
		_, _ = rand.Read(r[:])
		tmp := filepath.Join(dir, "."+base+".tmp-"+hex.EncodeToString(r[:]))
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("cannot create a temporary %s in %s under the %s root %s: %w", what, echo(dir), kind, root.Name(), pathless(err))
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return tmp, fmt.Errorf("cannot write the temporary %s %s under the %s root %s: %w", what, echo(tmp), kind, root.Name(), pathless(err))
		}
		return tmp, nil
	}
	return "", fmt.Errorf("cannot create a temporary %s in %s under the %s root %s: every name tried exists", what, echo(dir), kind, root.Name())
}

// isInput reports whether fi is a file this server has read: a profile, an
// executions log or a source file.
func (s *Server) isInput(fi os.FileInfo) bool {
	same := func(in os.FileInfo) bool { return os.SameFile(in, fi) }
	return slices.ContainsFunc(s.store.inputs, same) || slices.ContainsFunc(s.src.Opened(), same)
}
