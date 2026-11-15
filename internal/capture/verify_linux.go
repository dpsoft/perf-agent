package capture

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const capXattr = "security.capability"

// copyAdvice is the next step for a binary on a nosuid or noexec mount,
// where file capabilities are ignored. /tmp is mounted so on most hosts.
const copyAdvice = "copy it outside /tmp, e.g. to the worktree or ~/bin, and setcap it there"

// VerifyBinary checks, without running it, that path is a perf-agent the
// server may start, in this order: the path is absolute and resolves
// (once, with EvalSymlinks); the file is a regular executable, not
// setuid or setgid, not writable by group or other, owned by root or the
// server's user; its filesystem honours file capabilities; its
// security.capability grants the required set and nothing outside the
// allowed one; its Go build information names perf-agent's main package.
// A shared blazesym library is a warning. Every later check works on the
// resolved path, which is Binary.Path.
func VerifyBinary(path string) (Binary, error) {
	if !filepath.IsAbs(path) {
		return Binary{}, Errorf(CodeDisabled, "pass the perf-agent binary as an absolute path",
			"perf-agent path %q is not absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Binary{}, Errorf(CodeDisabled, "pass the path of a built perf-agent binary",
			"cannot resolve the perf-agent path: %v", err)
	}
	st, err := checkStat(resolved)
	if err != nil {
		return Binary{}, err
	}
	if err := checkMount(resolved); err != nil {
		return Binary{}, err
	}
	setcap := "run: " + strings.ReplaceAll(SetcapLine, "<path>", resolved)
	raw, err := readCapXattr(resolved)
	if err != nil {
		return Binary{}, Errorf(CodeDisabled, setcap, "cannot read the file capabilities of %s: %v", resolved, err)
	}
	if raw == nil {
		return Binary{}, Errorf(CodeDisabled, setcap, "%s has no file capabilities", resolved)
	}
	caps, err := ParseFileCaps(raw)
	if err != nil {
		return Binary{}, Errorf(CodeDisabled, setcap, "%s: %v", resolved, err)
	}
	warnings, err := checkCaps(caps)
	if err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			e.Message = resolved + ": " + e.Message
			e.Next = strings.ReplaceAll(e.Next, "<path>", resolved)
		}
		return Binary{}, err
	}
	version, revision, err := checkBuildInfo(resolved)
	if err != nil {
		return Binary{}, err
	}
	warnings = append(warnings, checkELF(resolved)...)

	// The checks above read the file in several steps. The identity they
	// saw must still be the file's, or a replacement slipped in between.
	id := identity(st, raw)
	now, err := IdentityOf(resolved)
	if err != nil {
		return Binary{}, Errorf(CodeDisabled, "", "cannot re-read %s after verifying it: %v", resolved, err)
	}
	if e := checkIdentity(id, now); e != nil {
		e.Message = resolved + " changed while it was being verified"
		e.Next = "retry once it is no longer being written"
		return Binary{}, e
	}
	return Binary{Path: resolved, ID: id, Caps: caps, Version: version, Revision: revision, Warnings: warnings}, nil
}

// checkStat applies the file rules to the resolved path. Lstat, so a
// symlink swapped in after resolution is not a regular file.
func checkStat(path string) (*syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return nil, Errorf(CodeDisabled, "", "cannot stat %s: %v", path, err)
	}
	mode := st.Mode
	switch {
	case mode&syscall.S_IFMT != syscall.S_IFREG:
		return nil, Errorf(CodeDisabled, "pass the perf-agent binary itself", "%s is not a regular file", path)
	case mode&(syscall.S_ISUID|syscall.S_ISGID) != 0:
		// A setuid binary would change the uids the target rule relies on.
		return nil, Errorf(CodeDisabled, "chmod ug-s "+path, "%s is setuid or setgid", path)
	case mode&0o022 != 0:
		// Writing to a file clears its file capabilities, so a file nobody
		// else can write keeps the capabilities it was verified with.
		return nil, Errorf(CodeDisabled, "chmod go-w "+path, "%s is writable by group or other (mode %#o)", path, mode&0o7777)
	case mode&0o111 == 0:
		return nil, Errorf(CodeDisabled, "chmod +x "+path, "%s is not executable", path)
	case st.Uid != 0 && st.Uid != uint32(os.Getuid()):
		return nil, Errorf(CodeDisabled, "use a binary you or root own", "%s is owned by uid %d, neither root nor this server's user", path, st.Uid)
	}
	return &st, nil
}

// checkMount refuses a filesystem mounted nosuid or noexec: the kernel
// ignores file capabilities on the first and refuses the exec on the second.
func checkMount(path string) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return Errorf(CodeDisabled, "", "cannot read the mount flags of %s: %v", path, err)
	}
	flags := uint64(fs.Flags)
	if flags&syscall.MS_NOSUID != 0 {
		return Errorf(CodeDisabled, copyAdvice, "%s is on a filesystem mounted nosuid, where file capabilities do not apply", path)
	}
	if flags&syscall.MS_NOEXEC != 0 {
		return Errorf(CodeDisabled, copyAdvice, "%s is on a filesystem mounted noexec", path)
	}
	return nil
}

// readCapXattr returns the raw security.capability value, or nil when the
// file has none (or its filesystem has no extended attributes).
func readCapXattr(path string) ([]byte, error) {
	// vfs_cap_data is at most 24 bytes; a larger value is no known format.
	buf := make([]byte, 64)
	n, err := syscall.Getxattr(path, capXattr, buf)
	switch {
	case errors.Is(err, syscall.ENODATA), errors.Is(err, syscall.ENOTSUP):
		return nil, nil
	case errors.Is(err, syscall.ERANGE):
		return nil, fmt.Errorf("%s is larger than any vfs_cap_data", capXattr)
	case err != nil:
		return nil, err
	}
	return buf[:n], nil
}

// identity builds an Identity from a stat result and the raw xattr.
func identity(st *syscall.Stat_t, xattr []byte) Identity {
	id := Identity{
		Dev:   uint64(st.Dev),
		Ino:   uint64(st.Ino),
		Size:  st.Size,
		Mtime: st.Mtim.Nano(),
		Ctime: st.Ctim.Nano(),
	}
	if xattr != nil {
		id.Xattr = sha256.Sum256(xattr)
	}
	return id
}

// IdentityOf returns path's current identity, following symlinks.
func IdentityOf(path string) (Identity, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return Identity{}, err
	}
	raw, err := readCapXattr(path)
	if err != nil {
		return Identity{}, err
	}
	return identity(&st, raw), nil
}

// Unchanged reports binary_changed when b's file is no longer the one
// that was verified, or can no longer be read. It runs before every exec.
func (b Binary) Unchanged() *Error {
	now, err := IdentityOf(b.Path)
	if err != nil {
		e := Errorf(CodeBinaryChanged, "restart perf-agent-mcp", "perf-agent changed since start-up: %v", err)
		e.Changed = "unreadable"
		return e
	}
	return checkIdentity(b.ID, now)
}

// VerifyShim checks the CUPTI adapter: an absolute path to a regular file
// that neither group nor other can write. The target's own process loads
// it, so it needs no capability check. Path is the resolved file.
func VerifyShim(path string) (Shim, error) {
	if !filepath.IsAbs(path) {
		return Shim{}, Errorf(CodeDisabled, "pass --gpu-shim as an absolute path", "GPU shim path %q is not absolute", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Shim{}, Errorf(CodeDisabled, "pass the path of the built CUPTI adapter", "cannot resolve the GPU shim path: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(resolved, &st); err != nil {
		return Shim{}, Errorf(CodeDisabled, "", "cannot stat %s: %v", resolved, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return Shim{}, Errorf(CodeDisabled, "pass the CUPTI adapter library itself", "%s is not a regular file", resolved)
	}
	if st.Mode&0o022 != 0 {
		return Shim{}, Errorf(CodeDisabled, "chmod go-w "+resolved, "%s is writable by group or other (mode %#o)", resolved, st.Mode&0o7777)
	}
	return Shim{Path: resolved, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// ReadSelfStatus returns this process's /proc/self/status, for CheckSelf.
func ReadSelfStatus() ([]byte, error) {
	return os.ReadFile("/proc/self/status")
}
