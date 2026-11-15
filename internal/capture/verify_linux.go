package capture

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const capXattr = "security.capability"

// copyAdvice is the next step for a binary on a nosuid or noexec mount,
// where file capabilities are ignored. /tmp is mounted so on most hosts.
const copyAdvice = "copy it outside /tmp, to a private directory you own (e.g. under ~), and setcap it there"

// creds are the server's effective ids, which decide who may run and
// replace the binary.
type creds struct {
	uid, gid uint32
	groups   []uint32
}

func currentCreds() creds {
	c := creds{uid: uint32(os.Geteuid()), gid: uint32(os.Getegid())}
	// Without the supplementary groups the group class is judged by the
	// effective gid alone, which can only refuse more.
	if gs, err := os.Getgroups(); err == nil {
		for _, g := range gs {
			c.groups = append(c.groups, uint32(g))
		}
	}
	return c
}

func (c creds) inGroup(gid uint32) bool {
	return gid == c.gid || slices.Contains(c.groups, gid)
}

// VerifyBinary checks, without running it, that path is a perf-agent the
// server may start, in this order:
//   - the path is absolute and resolves, once, with EvalSymlinks; every
//     later check is about the resolved path, which is Binary.Path;
//   - the file is regular, not setuid or setgid, not writable by group or
//     other, owned by root or the server's user, and executable by the
//     server's user;
//   - no other user can replace it through a directory above it (see
//     checkAncestors);
//   - its filesystem honours file capabilities;
//   - its security.capability grants the required set and nothing outside
//     the allowed one;
//   - its Go build information identifies it as perf-agent;
//   - a shared blazesym library is a warning.
//
// The checks from the mount on read one open descriptor, so they are all
// about the same inode.
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
	c := currentCreds()

	// Lstat before opening, so a FIFO or a device is refused unopened.
	var pre syscall.Stat_t
	if err := syscall.Lstat(resolved, &pre); err != nil {
		return Binary{}, Errorf(CodeDisabled, "", "cannot stat %s: %v", resolved, err)
	}
	if err := checkBinaryStat(resolved, &pre, c); err != nil {
		return Binary{}, err
	}
	if err := checkAncestors(resolved, c, syscall.Lstat); err != nil {
		return Binary{}, err
	}

	// O_NOFOLLOW refuses a symlink swapped in since the Lstat; O_NONBLOCK
	// keeps a FIFO swapped in from blocking the open.
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return Binary{}, Errorf(CodeDisabled, "", "cannot open %s: %v", resolved, err)
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return Binary{}, Errorf(CodeDisabled, "", "cannot stat %s: %v", resolved, err)
	}
	if st.Dev != pre.Dev || st.Ino != pre.Ino {
		return Binary{}, changedDuringVerification(resolved, "dev,ino")
	}
	if err := checkBinaryStat(resolved, &st, c); err != nil {
		return Binary{}, err
	}

	if err := checkMount(resolved, fd); err != nil {
		return Binary{}, err
	}
	setcap := "run: " + strings.ReplaceAll(SetcapLine, "<path>", resolved)
	// getxattr through the descriptor's /proc link reads the open inode;
	// the syscall package has no fgetxattr.
	raw, err := readCapXattr("/proc/self/fd/" + strconv.Itoa(fd))
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
	version, revision, err := checkBuildInfo(f, resolved)
	if err != nil {
		return Binary{}, err
	}
	warnings = append(warnings, checkELF(f, resolved)...)

	// The checks above read one inode. Comparing its identity at the open
	// with the path's identity now shows that the path still names that
	// inode, and that its size, times and capability xattr did not change
	// while it was read: a write or chmod in between moves mtime or ctime.
	id := identity(&st, raw)
	now, err := IdentityOf(resolved)
	if err != nil {
		return Binary{}, Errorf(CodeDisabled, "", "cannot re-read %s after verifying it: %v", resolved, err)
	}
	if e := checkIdentity(id, now); e != nil {
		return Binary{}, changedDuringVerification(resolved, e.Changed)
	}
	return Binary{Path: resolved, ID: id, Caps: caps, Version: version, Revision: revision, Warnings: warnings}, nil
}

func changedDuringVerification(path, changed string) *Error {
	e := Errorf(CodeBinaryChanged, "check that no build, copy or setcap is still writing it, then restart perf-agent-mcp",
		"%s changed while it was being verified", path)
	e.Changed = changed
	return e
}

// checkOwner refuses a file or directory owned by neither root nor the
// server's user: its owner could replace or rewrite it.
func checkOwner(what, path string, st *syscall.Stat_t, c creds, next string) error {
	if st.Uid != 0 && st.Uid != c.uid {
		return Errorf(CodeDisabled, next, "%s %s is owned by uid %d, neither root nor this server's user", what, path, st.Uid)
	}
	return nil
}

// checkBinaryStat applies the file rules to perf-agent's stat result.
func checkBinaryStat(path string, st *syscall.Stat_t, c creds) error {
	mode := st.Mode
	switch {
	case mode&syscall.S_IFMT != syscall.S_IFREG:
		return Errorf(CodeDisabled, "pass the perf-agent binary itself", "%s is not a regular file", path)
	case mode&(syscall.S_ISUID|syscall.S_ISGID) != 0:
		// A setuid binary would change the uids the target rule relies on.
		return Errorf(CodeDisabled, "chmod ug-s "+path, "%s is setuid or setgid", path)
	case mode&0o022 != 0:
		// Writing to a file clears its file capabilities, so a file nobody
		// else can write keeps the capabilities it was verified with.
		return Errorf(CodeDisabled, "chmod go-w "+path, "%s is writable by group or other (mode %#o)", path, mode&0o7777)
	case mode&0o111 == 0:
		return Errorf(CodeDisabled, "chmod u+x "+path, "%s is not executable", path)
	}
	if err := checkOwner("perf-agent", path, st, c, "use a perf-agent binary you or root own"); err != nil {
		return err
	}
	if !executableBy(st, c) {
		return Errorf(CodeDisabled, "chmod u+x "+path+" (or a+x for a root-owned file)",
			"%s is not executable by this server's user (mode %#o; the kernel uses the owner, group or other bits that match it)", path, mode&0o7777)
	}
	return nil
}

// executableBy applies the kernel's permission classes: the owner bits
// when c owns the file, else the group bits when c is in its group, else
// the other bits. Root needs any x bit.
func executableBy(st *syscall.Stat_t, c creds) bool {
	switch {
	case c.uid == 0:
		return st.Mode&0o111 != 0
	case st.Uid == c.uid:
		return st.Mode&0o100 != 0
	case c.inGroup(st.Gid):
		return st.Mode&0o010 != 0
	default:
		return st.Mode&0o001 != 0
	}
}

// checkShimStat applies the file rules to the shim's stat result.
func checkShimStat(path string, st *syscall.Stat_t, c creds) error {
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return Errorf(CodeDisabled, "pass the CUPTI adapter library itself", "%s is not a regular file", path)
	}
	if st.Mode&0o022 != 0 {
		return Errorf(CodeDisabled, "chmod go-w "+path, "%s is writable by group or other (mode %#o)", path, st.Mode&0o7777)
	}
	return checkOwner("GPU shim", path, st, c, "use a GPU shim you or root own")
}

// checkAncestors refuses path when a directory above it would let another
// user replace it: one owned by neither root nor the server's user, or one
// writable by group or other without the sticky bit (which limits
// renaming and removal to an entry's owner).
//
// A private directory is a gate: owned by root or the server's user, with
// no search permission for group or other (such as a 0700 home). No other
// non-root user can reach anything below it, so a failing directory below
// a gate is excused; directories above the gate are still checked, since
// whoever could write them could replace the gate itself. The walk goes
// from the file upward and refuses with the nearest failing directory
// that no gate covers. path is resolved, so each ancestor is a directory;
// one that is not has changed since, and is refused at once.
func checkAncestors(path string, c creds, lstat func(string, *syscall.Stat_t) error) error {
	const private = "move perf-agent under a private directory you own (e.g. under ~)"
	var pending error
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		var st syscall.Stat_t
		if err := lstat(dir, &st); err != nil {
			return Errorf(CodeDisabled, "", "cannot stat directory %s: %v", dir, err)
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			return Errorf(CodeDisabled, "", "%s is not a directory any more", dir)
		}
		owned := st.Uid == 0 || st.Uid == c.uid
		switch {
		case owned && st.Mode&0o011 == 0:
			pending = nil
		case pending != nil:
		case !owned:
			pending = Errorf(CodeDisabled, private,
				"directory %s is owned by uid %d, neither root nor this server's user", dir, st.Uid)
		case st.Mode&0o022 != 0 && st.Mode&syscall.S_ISVTX == 0:
			pending = Errorf(CodeDisabled, private+", or chmod go-w "+dir,
				"directory %s is writable by group or other and not sticky (mode %#o), so another user could replace %s", dir, st.Mode&0o7777, path)
		}
		if dir == "/" {
			return pending
		}
	}
}

// checkMount refuses a filesystem mounted nosuid or noexec: the kernel
// ignores file capabilities on the first and refuses the exec on the second.
func checkMount(path string, fd int) error {
	return checkMountFor(path, fd, true)
}

// checkMountFor reads fd's mount flags for mountFlagsError.
func checkMountFor(path string, fd int, caps bool) error {
	var fs syscall.Statfs_t
	if err := syscall.Fstatfs(fd, &fs); err != nil {
		return Errorf(CodeDisabled, "", "cannot read the mount flags of %s: %v", path, err)
	}
	return mountFlagsError(path, uint64(fs.Flags), caps)
}

// mountFlagsError refuses noexec, and nosuid when caps says the binary
// relies on file capabilities, which nosuid voids.
func mountFlagsError(path string, flags uint64, caps bool) error {
	if caps && flags&syscall.MS_NOSUID != 0 {
		return Errorf(CodeDisabled, copyAdvice, "%s is on a filesystem mounted nosuid, where file capabilities do not apply", path)
	}
	if flags&syscall.MS_NOEXEC != 0 {
		return Errorf(CodeDisabled, copyAdvice, "%s is on a filesystem mounted noexec", path)
	}
	return nil
}

// Recheck re-runs, before an exec of b, the checks that do not need the
// whole verification: the path still names the verified identity, its
// directories still pass the ancestor rule, and its mount still honours
// the capabilities it was verified with. The mount flags are read on a
// descriptor that is checked to be the verified inode.
func (b Binary) Recheck() *Error {
	if e := b.Unchanged(); e != nil {
		return e
	}
	asError := func(err error) *Error {
		if e, ok := errors.AsType[*Error](err); ok {
			return e
		}
		return Errorf(CodeBinaryChanged, "restart perf-agent-mcp", "%v", err)
	}
	if err := checkAncestors(b.Path, currentCreds(), syscall.Lstat); err != nil {
		return asError(err)
	}
	f, err := os.OpenFile(b.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return Errorf(CodeBinaryChanged, "restart perf-agent-mcp", "cannot open perf-agent to recheck it: %v", err)
	}
	defer func() { _ = f.Close() }()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil || uint64(st.Dev) != b.ID.Dev || uint64(st.Ino) != b.ID.Ino {
		e := Errorf(CodeBinaryChanged, "restart perf-agent-mcp", "perf-agent changed since start-up")
		e.Changed = "dev,ino"
		return e
	}
	if err := checkMountFor(b.Path, int(f.Fd()), b.ID.Xattr != [32]byte{}); err != nil {
		return asError(err)
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
// owned by root or the server's user, not writable by group or other, in
// directories no other user can replace it from. The target's own process
// loads it, so it needs no capability check. Path is the resolved file.
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
	c := currentCreds()
	if err := checkShimStat(resolved, &st, c); err != nil {
		return Shim{}, err
	}
	if err := checkAncestors(resolved, c, syscall.Lstat); err != nil {
		return Shim{}, err
	}
	return Shim{Path: resolved, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// ReadSelfStatus returns this process's /proc/self/status, for CheckSelf.
func ReadSelfStatus() ([]byte, error) {
	return os.ReadFile("/proc/self/status")
}
