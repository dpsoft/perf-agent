package capture

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// nosuidOrNoexec reports whether dir's filesystem is mounted nosuid or noexec.
func nosuidOrNoexec(t *testing.T, dir string) bool {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	return uint64(st.Flags)&(syscall.MS_NOSUID|syscall.MS_NOEXEC) != 0
}

// execDir returns a new directory where a test binary passes the
// directory and mount rules, so a refusal after them is for the reason the
// test names. t.TempDir() is usually under a nosuid /tmp, and a checkout
// may sit below a world-writable directory, so it also tries a
// dot-directory inside the package and one in the user's cache directory,
// each removed at the end of the test.
func execDir(t *testing.T) string {
	t.Helper()
	var tried []string
	usable := func(d string) bool {
		if nosuidOrNoexec(t, d) {
			tried = append(tried, d+": nosuid or noexec")
			return false
		}
		if err := checkAncestors(filepath.Join(d, "probe"), currentCreds(), syscall.Lstat); err != nil {
			tried = append(tried, err.Error())
			return false
		}
		return true
	}
	if d := t.TempDir(); usable(d) {
		return d
	}
	parents := []string{"."}
	if cache, err := os.UserCacheDir(); err == nil {
		parents = append(parents, cache)
	}
	for _, parent := range parents {
		d, err := os.MkdirTemp(parent, ".verify-test-")
		if err != nil {
			tried = append(tried, err.Error())
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		if d, err = filepath.Abs(d); err != nil {
			t.Fatal(err)
		}
		if usable(d) {
			return d
		}
	}
	t.Skipf("no directory where a binary passes the directory and mount rules: %s", strings.Join(tried, "; "))
	return ""
}

// file writes a file with exactly mode (chmod after create, past the umask).
func file(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyBinaryStatRules(t *testing.T) {
	dir := execDir(t)
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"relative", "perf-agent", "absolute"},
		{"group-writable", file(t, filepath.Join(dir, "gw"), 0o775), "writable by group or other"},
		{"other-writable", file(t, filepath.Join(dir, "ow"), 0o757), "writable by group or other"},
		{"setuid", file(t, filepath.Join(dir, "su"), 0o755|os.ModeSetuid), "setuid or setgid"},
		{"setgid", file(t, filepath.Join(dir, "sg"), 0o755|os.ModeSetgid), "setuid or setgid"},
		{"not executable", file(t, filepath.Join(dir, "nx"), 0o644), "not executable"},
		{"executable by other only", file(t, filepath.Join(dir, "ox"), 0o405), "not executable by this server's user"},
		{"directory", dir, "not a regular file"},
		{"missing", filepath.Join(dir, "missing"), "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyBinary(tc.path)
			e := asError(t, err, CodeDisabled)
			if !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", e, tc.want)
			}
		})
	}

	// A plain 0755 file passes the stat and mount rules and is refused at
	// the capability step.
	plain := file(t, filepath.Join(dir, "plain"), 0o755)
	_, err := VerifyBinary(plain)
	e := asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "no file capabilities") || strings.Contains(e.Error(), "nosuid") {
		t.Fatalf("plain 0755 file: %q, want refused for missing file capabilities", e)
	}
	if want := strings.ReplaceAll(SetcapLine, "<path>", plain); !strings.Contains(e.Next, want) {
		t.Fatalf("next %q does not quote %q", e.Next, want)
	}

	// A symlink resolves once, and every later check is about the target.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyBinary(link)
	e = asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, plain+" has no file capabilities") || strings.Contains(e.Error(), link) {
		t.Fatalf("symlink: %q, want the refusal about the target %s", e, plain)
	}
}

func TestVerifyBinaryRefusesNosuidMount(t *testing.T) {
	dir := t.TempDir()
	if !nosuidOrNoexec(t, dir) {
		t.Skipf("%s is not mounted nosuid or noexec", dir)
	}
	_, err := VerifyBinary(file(t, filepath.Join(dir, "perf-agent"), 0o755))
	e := asError(t, err, CodeDisabled)
	if !strings.Contains(e.Error(), "copy it outside /tmp, e.g. to the worktree or ~/bin, and setcap it there") {
		t.Fatalf("error %q does not advise copying it outside /tmp", e)
	}
}

func TestIdentityOfAndUnchanged(t *testing.T) {
	path := file(t, filepath.Join(t.TempDir(), "perf-agent"), 0o755)
	id, err := IdentityOf(path)
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if id.Dev != uint64(st.Dev) || id.Ino != uint64(st.Ino) || id.Size != st.Size || id.Xattr != ([32]byte{}) {
		t.Fatalf("IdentityOf = %+v, stat %+v", id, st)
	}
	b := Binary{Path: path, ID: id}
	if e := b.Unchanged(); e != nil {
		t.Fatalf("unchanged file: %v", e)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if e := b.Unchanged(); e == nil || e.Code != CodeBinaryChanged {
		t.Fatalf("rewritten file: %v, want binary_changed", e)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if e := b.Unchanged(); e == nil || e.Code != CodeBinaryChanged {
		t.Fatalf("removed file: %v, want binary_changed", e)
	}
}

func TestVerifyShim(t *testing.T) {
	dir := t.TempDir()
	if _, err := VerifyShim("libshim.so"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative: %v, want refused as not absolute", err)
	}
	if _, err := VerifyShim(file(t, filepath.Join(dir, "ow.so"), 0o646)); err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("other-writable: %v, want refused", err)
	}
	if _, err := VerifyShim(file(t, filepath.Join(dir, "gw.so"), 0o664)); err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("group-writable: %v, want refused", err)
	}
	if _, err := VerifyShim(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v, want refused", err)
	}
	ok := file(t, filepath.Join(dir, "libshim.so"), 0o644)
	s, err := VerifyShim(ok)
	if err != nil {
		t.Fatalf("0644 regular: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(ok, &st); err != nil {
		t.Fatal(err)
	}
	if s.Path != ok || s.Dev != uint64(st.Dev) || s.Ino != uint64(st.Ino) {
		t.Fatalf("VerifyShim = %+v, want %s dev %d ino %d", s, ok, st.Dev, st.Ino)
	}
}

func TestReadSelfStatus(t *testing.T) {
	b, err := ReadSelfStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\nUid:", "\nCapEff:", "\nCapPrm:", "\nNoNewPrivs:"} {
		if !strings.Contains(string(b), field) {
			t.Errorf("/proc/self/status has no %q", field)
		}
	}
}

func TestVerifyBinaryDirectoryRules(t *testing.T) {
	dir := execDir(t)

	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyBinary(file(t, filepath.Join(open, "perf-agent"), 0o755))
	e := asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "directory "+open+" is writable by group or other") {
		t.Fatalf("a 0777 parent: %q, want refused naming %s", e, open)
	}

	sticky := filepath.Join(dir, "sticky")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyBinary(file(t, filepath.Join(sticky, "perf-agent"), 0o755))
	e = asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "no file capabilities") {
		t.Fatalf("a sticky world-writable parent: %q, want it to pass the directory rule and stop at the capability step", e)
	}

	_, err = VerifyShim(file(t, filepath.Join(open, "libshim.so"), 0o644))
	e = asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "directory "+open+" is writable by group or other") {
		t.Fatalf("a shim in a 0777 parent: %q, want refused naming %s", e, open)
	}
	if _, err := VerifyShim(file(t, filepath.Join(sticky, "libshim.so"), 0o644)); err != nil {
		t.Fatalf("a shim in a sticky world-writable parent: %v", err)
	}
}

// fakeLstat reports the real lstat of every path, with change applied to
// dir's result.
func fakeLstat(dir string, change func(*syscall.Stat_t)) func(string, *syscall.Stat_t) error {
	return func(p string, st *syscall.Stat_t) error {
		if err := syscall.Lstat(p, st); err != nil {
			return err
		}
		if p == dir {
			change(st)
		}
		return nil
	}
}

func TestCheckAncestorsOwner(t *testing.T) {
	dir := execDir(t)
	path := filepath.Join(dir, "perf-agent")
	c := currentCreds()
	if err := checkAncestors(path, c, syscall.Lstat); err != nil {
		t.Fatalf("the real directories: %v", err)
	}
	other := c.uid + 1000
	err := checkAncestors(path, c, fakeLstat(dir, func(st *syscall.Stat_t) { st.Uid = other }))
	e := asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "directory "+dir+" is owned by uid") {
		t.Fatalf("a parent owned by uid %d: %q, want refused naming %s", other, e, dir)
	}
	if err := checkAncestors(path, c, fakeLstat(dir, func(st *syscall.Stat_t) { st.Uid = 0 })); err != nil {
		t.Fatalf("a root-owned parent: %v", err)
	}
	err = checkAncestors(path, c, fakeLstat(dir, func(st *syscall.Stat_t) { st.Mode = syscall.S_IFLNK | 0o777 }))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a parent that became a symlink: %v, want refused", err)
	}
	err = checkAncestors(path, c, fakeLstat(dir, func(st *syscall.Stat_t) { st.Mode |= 0o020 }))
	if err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("a group-writable parent: %v, want refused", err)
	}
}

func TestFileRulesOwnerAndExecClass(t *testing.T) {
	c := creds{uid: 1000, gid: 1000, groups: []uint32{10}}
	st := func(uid, gid, mode uint32) *syscall.Stat_t {
		return &syscall.Stat_t{Uid: uid, Gid: gid, Mode: syscall.S_IFREG | mode}
	}
	for _, tc := range []struct {
		name string
		st   *syscall.Stat_t
		want string // empty: accepted
	}{
		{"mine 0755", st(1000, 1000, 0o755), ""},
		{"mine 0500", st(1000, 1000, 0o500), ""},
		{"mine 0405", st(1000, 1000, 0o405), "not executable by this server's user"},
		{"root 0755", st(0, 0, 0o755), ""},
		{"root 0750 in my supplementary group", st(0, 10, 0o750), ""},
		{"root 0750 in another group", st(0, 0, 0o750), "not executable by this server's user"},
		{"root 0705", st(0, 0, 0o705), ""},
		{"another user's", st(1234, 1234, 0o755), "owned by uid 1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBinaryStat("/p", tc.st, c)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("%v, want refused with %q", err, tc.want)
			}
		})
	}
	if err := checkBinaryStat("/p", st(0, 0, 0o601), creds{uid: 0}); err != nil {
		t.Errorf("euid 0 runs a file with any x bit: %v", err)
	}
	if err := checkShimStat("/s", st(1234, 1234, 0o644), c); err == nil || !strings.Contains(err.Error(), "owned by uid 1234") {
		t.Errorf("a shim owned by another user: %v, want refused", err)
	}
	if err := checkShimStat("/s", st(0, 0, 0o644), c); err != nil {
		t.Errorf("a root-owned shim: %v", err)
	}
}
