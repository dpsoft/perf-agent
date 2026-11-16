package capture

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeMode writes a file at path with exactly mode, past the umask.
func writeMode(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// targetError requires e to be a non-nil *Error with code c whose
// message says want.
func targetError(t *testing.T, e *Error, c Code, want string) {
	t.Helper()
	if e == nil {
		t.Fatalf("no error, want %s saying %q", c, want)
	}
	if e.Code != c || !strings.Contains(e.Message, want) {
		t.Fatalf("%s %q, want %s saying %q", e.Code, e.Message, c, want)
	}
}

func TestResolveCommand(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	noexec, bin := filepath.Join(work, "noexec"), filepath.Join(work, "bin")
	for _, d := range []string{noexec, bin, filepath.Join(work, "rel"), filepath.Join(noexec, "prog")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeMode(t, filepath.Join(work, "rel", "tool"), 0o755)
	writeMode(t, filepath.Join(work, "tool"), 0o755)
	writeMode(t, filepath.Join(noexec, "tool"), 0o644)
	first, second := filepath.Join(work, "first"), filepath.Join(work, "second")
	for _, d := range []string{first, second} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeMode(t, filepath.Join(first, "both"), 0o405)
	both := writeMode(t, filepath.Join(second, "both"), 0o700)
	prog := writeMode(t, filepath.Join(bin, "prog"), 0o755)
	tool := writeMode(t, filepath.Join(bin, "tool"), 0o755)

	for _, tc := range []struct {
		name, cmd, path string
		allow           []string
		want            string
	}{
		{"absolute, as is", "/bin/true", "", nil, "/bin/true"},
		{"a bare name", "prog", noexec + ":" + bin, nil, prog},
		// noexec/prog is a directory and noexec/tool is not executable.
		{"past a directory and a non-executable file", "tool", noexec + ":" + bin, nil, tool},
		// "rel" and the empty entry (the current directory) both hold an
		// executable tool, and are skipped.
		{"relative and empty entries skipped", "tool", "rel::" + bin, nil, tool},
		{"in the allow-list", "prog", bin, []string{"/usr/bin/x", prog}, prog},
		{"absolute in the allow-list", prog, "", []string{prog}, prog},
		// 0405 owned by this user has only other's x bit, which the kernel
		// does not apply to the owner; the later 0700 file is the one.
		{"past a file only others may execute", "both", first + ":" + second, nil, both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := ResolveCommand(tc.cmd, tc.path, tc.allow)
			if e != nil || got != tc.want {
				t.Fatalf("ResolveCommand(%q, %q): %q, %v; want %q", tc.cmd, tc.path, got, e, tc.want)
			}
		})
	}

	t.Run("true in /usr/bin:/bin", func(t *testing.T) {
		got, e := ResolveCommand("true", "/usr/bin:/bin", nil)
		if e != nil {
			t.Skipf("no true in /usr/bin or /bin: %v", e)
		}
		if got != "/usr/bin/true" && got != "/bin/true" {
			t.Fatalf("true resolved to %q", got)
		}
	})

	for _, tc := range []struct {
		name, cmd, path string
		allow           []string
		want            string
	}{
		{"empty", "", bin, nil, "command"},
		{"dot-relative", "./x", bin, nil, "relative"},
		{"relative with a directory", "bin/x", bin, nil, "relative"},
		{"not in PATH", "nothere", bin, nil, "PATH"},
		{"only relative entries", "tool", "rel:", nil, "PATH"},
		{"a bare name outside the allow-list", "prog", bin, []string{"/usr/bin/x"}, "--capture-command"},
		{"an absolute path outside the allow-list", "/bin/true", "", []string{prog}, "--capture-command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := ResolveCommand(tc.cmd, tc.path, tc.allow)
			targetError(t, e, CodeInvalidArgument, tc.want)
			if got != "" {
				t.Fatalf("refused, but returned %q", got)
			}
		})
	}
}

func TestWorkloadEnvIsTheAllowList(t *testing.T) {
	environ := []string{
		"OTEL_X=1", "LC_ALL=C", "DEBUGINFOD_URLS=https://example.invalid", "HOME=/home/u",
		"GODEBUG=x=1", "KUBERNETES_POD_NAME=p", "PATH=/usr/bin", "CUDA_INJECTION64_PATH=/evil.so",
		"LCX=1", "PATHX=1", "BROKEN", "LC_CTYPE=en_US.UTF-8", "HOME=/second", "TZ=UTC",
		"USER=u", "LOGNAME=u", "LANG=C.UTF-8", "TMPDIR=/t", "XDG_RUNTIME_DIR=/run/user/1",
		"CUDA_VISIBLE_DEVICES=0", "LD_PRELOAD=/x.so", "SSH_AUTH_SOCK=/s",
	}
	kept := []string{
		"LC_ALL=C", "HOME=/home/u", "PATH=/usr/bin", "LC_CTYPE=en_US.UTF-8", "TZ=UTC",
		"USER=u", "LOGNAME=u", "LANG=C.UTF-8", "TMPDIR=/t", "XDG_RUNTIME_DIR=/run/user/1",
		"CUDA_VISIBLE_DEVICES=0",
	}
	if got := WorkloadEnv(environ, ""); !slices.Equal(got, kept) {
		t.Fatalf("no shim:\n got %q\nwant %q", got, kept)
	}
	want := append(slices.Clone(kept), "CUDA_INJECTION64_PATH=/opt/shim.so")
	if got := WorkloadEnv(environ, "/opt/shim.so"); !slices.Equal(got, want) {
		t.Fatalf("with a shim:\n got %q\nwant %q", got, want)
	}
	for _, e := range WorkloadEnv(environ, "") {
		if strings.HasPrefix(e, "CUDA_INJECTION64_PATH=") {
			t.Fatalf("the server's own CUDA_INJECTION64_PATH reached the workload: %q", e)
		}
	}
}

// TestResolveCommandBoundsTheEcho: a message repeats command[0] only cut
// to maxEcho characters.
func TestResolveCommandBoundsTheEcho(t *testing.T) {
	long := strings.Repeat("x", 500)
	for _, name := range []string{long, "./" + long, "/" + long} {
		_, e := ResolveCommand(name, "/nonexistent", []string{"/bin/true"})
		targetError(t, e, CodeInvalidArgument, "")
		if strings.Contains(e.Message, strings.Repeat("x", maxEcho)) || !strings.Contains(e.Message, "…") {
			t.Fatalf("message %q repeats more than %d characters of command[0]", e.Message, maxEcho)
		}
	}
}
