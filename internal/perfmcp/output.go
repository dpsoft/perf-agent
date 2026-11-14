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
)

// errRenderDisabled is every render call's answer on a server without an
// output root.
var errRenderDisabled = errors.New("DISABLED: start perf-agent-mcp with --output DIR to enable rendering. " +
	"This server has no output root, so it writes no pages")

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
	root := s.output
	dir, base := filepath.Dir(name), filepath.Base(name)
	tmp, err := writeTemp(root, dir, base, page)
	if tmp != "" {
		defer func() { _ = root.Remove(tmp) }()
	}
	if err != nil {
		return "", err
	}
	if s.beforePublish != nil {
		s.beforePublish(name)
	}
	if err := s.link(root, tmp, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", existsError(name, root.Name())
		}
		return "", fmt.Errorf("cannot publish %s under the output root %s with a hard link: %v; "+
			"perf-agent-mcp publishes a page only with a hard link, which never replaces a file, so it wrote no page. "+
			"Start perf-agent-mcp with --output on a filesystem that supports hard links", echo(name), root.Name(), err)
	}
	return filepath.Join(root.Name(), name), nil
}

// pageName is out as a name inside the output root, refusing one that
// leaves it, does not end in .html, is not in an existing directory, or
// names anything that exists.
func (s *Server) pageName(out string) (string, error) {
	if s.output == nil {
		return "", errRenderDisabled
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
	case slices.ContainsFunc(s.store.inputs, func(in os.FileInfo) bool { return os.SameFile(in, fi) }):
		return fmt.Errorf("refusing to overwrite input %s: out %s under the output root %s is a file this server read; choose a new file name",
			echo(name), echo(name), root.Name())
	}
	return existsError(name, root.Name())
}

func existsError(name, dir string) error {
	return fmt.Errorf("out %s already exists under the output root %s and is never replaced; choose a new file name", echo(name), dir)
}

// writeTemp writes page to a new file ".<base>.tmp-<16 hex>" in dir inside
// root, created exclusively and written through the handle that created
// it, so the bytes go to a new file nobody else holds and never through a
// name something else could have replaced. It returns the temporary's
// name, which the caller removes, even on error.
func writeTemp(root *os.Root, dir, base string, page []byte) (string, error) {
	var b [8]byte
	for range maxTempTries {
		_, _ = rand.Read(b[:])
		tmp := filepath.Join(dir, "."+base+".tmp-"+hex.EncodeToString(b[:]))
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("cannot create a temporary page in %s under the output root %s: %w", dir, root.Name(), err)
		}
		_, err = f.Write(page)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return tmp, fmt.Errorf("cannot write the temporary page %s under the output root %s: %w", tmp, root.Name(), err)
		}
		return tmp, nil
	}
	return "", fmt.Errorf("cannot create a temporary page in %s under the output root %s: every name tried exists", dir, root.Name())
}
