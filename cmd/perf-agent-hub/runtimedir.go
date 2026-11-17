package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
)

// The runtime directory's files. hub.lock is never removed: its flock is
// the server's presence. The other three exist only while a server runs.
const (
	lockFile  = "hub.lock"
	infoFile  = "hub.json"
	tokenFile = "mcp-token"
	pageFile  = "page-url"
)

// runtimeName is the runtime directory's name under XDG_RUNTIME_DIR.
const runtimeName = "perf-agent-mcp"

// runtimeFiles are the files a running server writes, in the order it
// writes them: hub.json last, so that once it exists the others do.
var runtimeFiles = []string{tokenFile, pageFile, infoFile}

// hubInfo is hub.json: who runs the server and where it listens. It
// holds no token.
type hubInfo struct {
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	MCPURL  string    `json:"mcp_url"`
	Started time.Time `json:"started"`
	Version string    `json:"version"`
}

// runtimeDir is the server's runtime directory: the lock, the tokens and
// hub.json, each 0600 in a 0700 directory of the user's own. Every file is
// reached through root, so none is followed out of the directory.
type runtimeDir struct {
	root *os.Root
	path string
	// lock is hub.lock, open while this process holds or may take its
	// flock; nil once released.
	lock *os.File
}

// ownerOf is the uid owning fi, and whether the platform says; a seam for
// tests.
var ownerOf = func(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// openRuntimeDir opens the runtime directory: flag when --runtime-dir
// names one, which must exist; otherwise $XDG_RUNTIME_DIR/perf-agent-mcp,
// created 0700 when missing. It refuses a symbolic link, a non-directory,
// a mode other than 0700 and an owner other than this user, and checks
// that the directory it opened is the one it checked.
func openRuntimeDir(flag string, getenv func(string) string) (*runtimeDir, error) {
	path := flag
	if path == "" {
		base := getenv("XDG_RUNTIME_DIR")
		switch {
		case base == "":
			return nil, errors.New("XDG_RUNTIME_DIR is not set: name a runtime directory, mode 0700 and yours, with --runtime-dir DIR")
		case !filepath.IsAbs(base):
			return nil, fmt.Errorf("XDG_RUNTIME_DIR %q is not an absolute path: name a runtime directory with --runtime-dir DIR", base)
		}
		path = filepath.Join(base, runtimeName)
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("runtime directory: %w", err)
		}
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("--runtime-dir %s: %w", path, err)
		}
		path = abs
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("runtime directory: %w", err)
	}
	if err := checkRuntimeDir(path, fi); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("runtime directory: %w", err)
	}
	// The directory opened is the one checked, not one put in its place.
	if opened, err := root.Stat("."); err != nil || !os.SameFile(fi, opened) {
		_ = root.Close()
		return nil, fmt.Errorf("runtime directory %s changed while it was opened", path)
	}
	return &runtimeDir{root: root, path: path}, nil
}

// checkRuntimeDir refuses a runtime directory another user could read or
// replace.
func checkRuntimeDir(path string, fi fs.FileInfo) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("runtime directory %s is a symbolic link; name the directory itself", path)
	case !fi.IsDir():
		return fmt.Errorf("runtime directory %s is not a directory", path)
	case fi.Mode().Perm() != 0o700:
		return fmt.Errorf("runtime directory %s has mode %v (%#o); it must be 0700", path, fi.Mode().Perm(), fi.Mode().Perm())
	}
	uid, ok := ownerOf(fi)
	if !ok {
		return fmt.Errorf("runtime directory %s: its owner cannot be read", path)
	}
	if uid != os.Getuid() {
		return fmt.Errorf("runtime directory %s is owned by uid %d, not by this user (uid %d)", path, uid, os.Getuid())
	}
	return nil
}

// take takes hub.lock's exclusive flock without waiting. When another
// server holds it, the error names its pid and port from hub.json, which
// is also returned. Once taken, it removes the files a dead server left,
// so nothing reads them while this server starts.
func (d *runtimeDir) take() (hubInfo, error) {
	f, err := d.root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return hubInfo{}, fmt.Errorf("runtime directory: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return hubInfo{}, fmt.Errorf("lock %s: %w", filepath.Join(d.path, lockFile), err)
		}
		info, rerr := d.info()
		if rerr != nil {
			return hubInfo{}, errors.New("perf-agent-hub is already running (it is starting or stopping)")
		}
		return info, fmt.Errorf("perf-agent-hub is already running (pid %d, port %d)", info.PID, info.Port)
	}
	d.lock = f
	for _, name := range runtimeFiles {
		for _, n := range []string{name, tempName(name)} {
			if err := d.root.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
				d.release()
				return hubInfo{}, fmt.Errorf("remove the stale %s: %w", n, err)
			}
		}
	}
	return hubInfo{}, nil
}

// info reads hub.json.
func (d *runtimeDir) info() (hubInfo, error) {
	var info hubInfo
	b, err := d.root.ReadFile(infoFile)
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(b, &info)
	return info, err
}

// tempName is the temporary a runtime file is written to before it takes
// its name. Only the lock's holder writes, so one name each is enough.
func tempName(name string) string { return "." + name + ".tmp" }

// write writes hub.json, mcp-token and page-url, each 0600, each to a
// temporary first and then renamed, so a reader never sees half a file.
// hub.json comes last.
func (d *runtimeDir) write(info hubInfo, mcpToken livehttp.Token, pageURL string) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	content := map[string][]byte{
		infoFile:  append(b, '\n'),
		tokenFile: []byte(string(mcpToken) + "\n"),
		pageFile:  []byte(pageURL + "\n"),
	}
	for _, name := range runtimeFiles {
		if err := d.writeFile(name, content[name]); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(d.path, name), err)
		}
	}
	return nil
}

func (d *runtimeDir) writeFile(name string, b []byte) error {
	tmp := tempName(name)
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// The umask can only narrow the mode; this states it.
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(b)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = d.root.Rename(tmp, name)
	}
	if err != nil {
		_ = d.root.Remove(tmp)
	}
	return err
}

// remove removes the files write wrote, then releases the lock and closes
// the directory. hub.lock stays. The files go while the lock is held, so
// they are never a next server's.
func (d *runtimeDir) remove() error {
	var errs []error
	if d.lock != nil {
		for _, name := range runtimeFiles {
			if err := d.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	d.close()
	return errors.Join(errs...)
}

// release closes hub.lock, which releases its flock.
func (d *runtimeDir) release() {
	if d.lock != nil {
		_ = d.lock.Close()
		d.lock = nil
	}
}

// close releases the lock, if held, and closes the directory, removing
// nothing.
func (d *runtimeDir) close() {
	d.release()
	_ = d.root.Close()
}

// running reports whether a server holds dir's lock: a shared flock on
// hub.lock cannot be taken. A missing lock file is no server.
func running(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK)
}
