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
	if !strings.Contains(e.Error(), "copy it outside /tmp, to a private directory you own (e.g. under ~), and setcap it there") {
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

// TestVerifyBinaryDirectoryRules runs the directory rule on real
// directories. Every test directory sits below a private one (t.TempDir's
// own 0700 parent, or the user's 0700 home), so the refusals without a
// private gate are in TestCheckAncestors, on a fake tree.
func TestVerifyBinaryDirectoryRules(t *testing.T) {
	dir := execDir(t)
	mkdir := func(path string, mode os.FileMode) string {
		t.Helper()
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A world-writable directory below a private one: nobody else can reach it.
	gate := mkdir(filepath.Join(dir, "gate"), 0o700)
	open := mkdir(filepath.Join(gate, "open"), 0o777)
	_, err := VerifyBinary(file(t, filepath.Join(open, "perf-agent"), 0o755))
	e := asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "no file capabilities") {
		t.Fatalf("a 0777 directory below a 0700 one: %q, want it to pass the directory rule and stop at the capability step", e)
	}
	if _, err := VerifyShim(file(t, filepath.Join(open, "libshim.so"), 0o644)); err != nil {
		t.Fatalf("a shim in a 0777 directory below a 0700 one: %v", err)
	}

	sticky := mkdir(filepath.Join(dir, "sticky"), 0o777|os.ModeSticky)
	_, err = VerifyBinary(file(t, filepath.Join(sticky, "perf-agent"), 0o755))
	e = asError(t, err, CodeDisabled)
	if !strings.Contains(e.Message, "no file capabilities") {
		t.Fatalf("a sticky world-writable parent: %q, want it to pass the directory rule and stop at the capability step", e)
	}
	if _, err := VerifyShim(file(t, filepath.Join(sticky, "libshim.so"), 0o644)); err != nil {
		t.Fatalf("a shim in a sticky world-writable parent: %v", err)
	}
}

// fakeTree is an lstat over a made-up directory tree.
type fakeTree map[string]syscall.Stat_t

func (f fakeTree) lstat(p string, st *syscall.Stat_t) error {
	s, ok := f[p]
	if !ok {
		return syscall.ENOENT
	}
	*st = s
	return nil
}

func dirStat(uid, mode uint32) syscall.Stat_t {
	return syscall.Stat_t{Uid: uid, Mode: syscall.S_IFDIR | mode}
}

func TestCheckAncestors(t *testing.T) {
	const me, other = 1000, 1234
	c := creds{uid: me, gid: me}
	// tree builds / (root 0555), /home (root 0755), /home/u, /home/u/src
	// and /home/u/src/bin, with the modes given for the last three.
	tree := func(u, src, bin syscall.Stat_t) fakeTree {
		return fakeTree{
			"/":               dirStat(0, 0o555),
			"/home":           dirStat(0, 0o755),
			"/home/u":         u,
			"/home/u/src":     src,
			"/home/u/src/bin": bin,
		}
	}
	const path = "/home/u/src/bin/perf-agent"
	for _, tc := range []struct {
		name string
		tree fakeTree
		want string // empty: accepted
	}{
		{"all 0755", tree(dirStat(me, 0o755), dirStat(me, 0o755), dirStat(me, 0o755)), ""},
		{"0777 below a 0700 gate", tree(dirStat(me, 0o700), dirStat(me, 0o777), dirStat(me, 0o777)), ""},
		{"0777 without a gate", tree(dirStat(me, 0o755), dirStat(me, 0o777), dirStat(me, 0o755)), "directory /home/u/src is writable by group or other"},
		{"the nearest offender is named", tree(dirStat(me, 0o755), dirStat(me, 0o777), dirStat(me, 0o777)), "directory /home/u/src/bin is writable"},
		{"sticky world-writable", tree(dirStat(me, 0o755), dirStat(me, 0o755), dirStat(me, 0o777|syscall.S_ISVTX)), ""},
		{"a gate owned by another uid is no gate", tree(dirStat(other, 0o700), dirStat(me, 0o777), dirStat(me, 0o755)), "directory /home/u/src is writable"},
		{"another uid's directory without a gate", tree(dirStat(me, 0o755), dirStat(other, 0o755), dirStat(me, 0o755)), "directory /home/u/src is owned by uid 1234"},
		{"another uid's directory below a gate", tree(dirStat(me, 0o700), dirStat(other, 0o755), dirStat(me, 0o755)), ""},
		{"a root-owned gate", tree(dirStat(0, 0o700), dirStat(me, 0o777), dirStat(me, 0o755)), ""},
		{"0710 still lets the group search", tree(dirStat(me, 0o710), dirStat(me, 0o777), dirStat(me, 0o755)), "directory /home/u/src is writable"},
		{"0701 still lets others search", tree(dirStat(me, 0o701), dirStat(me, 0o777), dirStat(me, 0o755)), "directory /home/u/src is writable"},
		{"a gate below a writable directory", tree(dirStat(me, 0o777), dirStat(me, 0o700), dirStat(me, 0o755)), "directory /home/u is writable"},
		{"a parent that became a symlink", tree(dirStat(me, 0o700), syscall.Stat_t{Uid: me, Mode: syscall.S_IFLNK | 0o777}, dirStat(me, 0o755)), "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAncestors(path, c, tc.tree.lstat)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "":
				e := asError(t, err, CodeDisabled)
				if !strings.Contains(e.Message, tc.want) {
					t.Fatalf("%q, want %q", e, tc.want)
				}
				if tc.want != "not a directory" && !strings.Contains(e.Next, "private directory") {
					t.Fatalf("next %q does not say how to fix it", e.Next)
				}
			}
		})
	}

	// The real directories above a test directory pass.
	if err := checkAncestors(filepath.Join(execDir(t), "perf-agent"), currentCreds(), syscall.Lstat); err != nil {
		t.Fatalf("the real directories: %v", err)
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
