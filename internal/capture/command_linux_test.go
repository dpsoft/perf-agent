package capture

import (
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCheckCommandFileRefusesSetuidAndCaps(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"setuid", writeMode(t, filepath.Join(dir, "su"), 0o755|os.ModeSetuid), "setuid"},
		{"setgid", writeMode(t, filepath.Join(dir, "sg"), 0o755|os.ModeSetgid), "setgid"},
		{"not executable", writeMode(t, filepath.Join(dir, "nx"), 0o644), "not executable"},
		{"executable by others only", writeMode(t, filepath.Join(dir, "ox"), 0o405), "not executable by this server's user"},
		{"a directory", dir, "not a regular file"},
		{"missing", filepath.Join(dir, "missing"), "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targetError(t, CheckCommandFile(tc.path), CodeInvalidArgument, tc.want)
		})
	}
	if e := CheckCommandFile(writeMode(t, filepath.Join(dir, "plain"), 0o755)); e != nil {
		t.Fatalf("a plain 0755 file: %v", e)
	}
	// exec follows a symlink, so the check is about its target.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "su"), link); err != nil {
		t.Fatal(err)
	}
	targetError(t, CheckCommandFile(link), CodeInvalidArgument, "setuid")

	// The path came from the agent: a message repeats it cut, and once.
	long := filepath.Join(dir, strings.Repeat("y", 200))
	e := CheckCommandFile(long)
	targetError(t, e, CodeInvalidArgument, "no such file")
	if strings.Contains(e.Error(), strings.Repeat("y", maxEcho)) || strings.Count(e.Error(), "…") != 1 || strings.Count(e.Error(), "/yy") != 1 {
		t.Fatalf("error %q repeats the path uncut or twice", e)
	}

	t.Run("file capabilities", func(t *testing.T) {
		// Setting security.capability needs CAP_SETFCAP, so this uses a
		// file the host already ships with capabilities.
		for _, p := range []string{"/usr/bin/newuidmap", "/usr/bin/newgidmap", "/usr/sbin/arping", "/usr/bin/clockdiff", "/usr/sbin/clockdiff", "/usr/bin/ping"} {
			if raw, err := readCapXattr(p); err != nil || raw == nil {
				continue
			}
			targetError(t, CheckCommandFile(p), CodeInvalidArgument, "file capabilities")
			return
		}
		t.Skip("no file with capabilities on this host")
	})
}

// TestCheckCommandFileXattr covers the capability branch with a crafted
// security.capability value, which an unprivileged test cannot set.
func TestCheckCommandFileXattr(t *testing.T) {
	path := writeMode(t, filepath.Join(t.TempDir(), "prog"), 0o755)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, xattrCapsSize2)
	binary.LittleEndian.PutUint32(raw, vfsCapRevision2|vfsCapFlagsEffective)
	binary.LittleEndian.PutUint32(raw[4:], 1<<CapSetuid)
	targetError(t, checkCommandFile(path, fi, raw), CodeInvalidArgument, "cap_setuid")
	// A value capture cannot parse is still a capability xattr, refused.
	targetError(t, checkCommandFile(path, fi, []byte{1, 2}), CodeInvalidArgument, "file capabilities")
	if e := checkCommandFile(path, fi, nil); e != nil {
		t.Fatalf("no xattr: %v", e)
	}
}

// statInfo is an fs.FileInfo carrying a fabricated stat result.
type statInfo struct{ st syscall.Stat_t }

func (i statInfo) Name() string       { return "prog" }
func (i statInfo) Size() int64        { return 0 }
func (i statInfo) Mode() fs.FileMode  { return fs.FileMode(i.st.Mode & 0o777) }
func (i statInfo) ModTime() time.Time { return time.Time{} }
func (i statInfo) IsDir() bool        { return false }
func (i statInfo) Sys() any           { return &i.st }

// TestCommandExecutableUsesTheKernelClass: the owner, group (effective or
// supplementary) or other bits apply, whichever class matches first.
func TestCommandExecutableUsesTheKernelClass(t *testing.T) {
	c := creds{uid: 1000, gid: 1000, groups: []uint32{10}}
	for _, tc := range []struct {
		name     string
		uid, gid uint32
		mode     uint32
		want     bool
	}{
		{"owner x", 1000, 1000, 0o700, true},
		{"owner without x, other with", 1000, 1000, 0o405, false},
		{"group x, effective gid", 0, 1000, 0o750, true},
		{"group x, supplementary gid", 0, 10, 0o750, true},
		{"group without x, other with", 0, 10, 0o705, false},
		{"other x", 0, 0, 0o701, true},
		{"other without x", 0, 0, 0o770, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fi := statInfo{syscall.Stat_t{Uid: tc.uid, Gid: tc.gid, Mode: syscall.S_IFREG | tc.mode}}
			if got := commandExecutable(fi, c); got != tc.want {
				t.Fatalf("uid %d gid %d mode %#o: executable %v, want %v", tc.uid, tc.gid, tc.mode, got, tc.want)
			}
		})
	}
}
