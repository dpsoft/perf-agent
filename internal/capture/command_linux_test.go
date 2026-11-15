package capture

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
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
