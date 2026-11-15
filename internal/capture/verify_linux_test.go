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

// execDir returns a new directory on a filesystem where file capabilities
// apply, so a refusal after the mount step is for the reason the test
// names. t.TempDir() is usually under a nosuid /tmp, so the fallback is a
// dot-directory inside the package, removed at the end of the test.
func execDir(t *testing.T) string {
	t.Helper()
	if d := t.TempDir(); !nosuidOrNoexec(t, d) {
		return d
	}
	d, err := os.MkdirTemp(".", ".verify-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, err = filepath.Abs(d)
	if err != nil {
		t.Fatal(err)
	}
	if nosuidOrNoexec(t, d) {
		t.Skipf("neither the temporary directory nor %s allows file capabilities", d)
	}
	return d
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
