package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
)

// xdg is a getenv with XDG_RUNTIME_DIR set to dir ("" for none).
func xdg(dir string) func(string) string {
	return func(k string) string {
		if k == "XDG_RUNTIME_DIR" {
			return dir
		}
		return ""
	}
}

func TestRuntimeDirModesAndOwner(t *testing.T) {
	t.Run("created 0700 when missing", func(t *testing.T) {
		base := t.TempDir()
		d, err := openRuntimeDir("", xdg(base))
		if err != nil {
			t.Fatal(err)
		}
		defer d.close()
		fi, err := os.Lstat(filepath.Join(base, "perf-agent-mcp"))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("created %v %v", fi, err)
		}
		if d.path != filepath.Join(base, "perf-agent-mcp") {
			t.Errorf("path %q", d.path)
		}
	})
	t.Run("an existing 0700 directory is used", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Mkdir(filepath.Join(base, "perf-agent-mcp"), 0o700); err != nil {
			t.Fatal(err)
		}
		d, err := openRuntimeDir("", xdg(base))
		if err != nil {
			t.Fatal(err)
		}
		d.close()
	})
	t.Run("the flag wins and is used as it is", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		d, err := openRuntimeDir(dir, xdg(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		defer d.close()
		if d.path != dir {
			t.Errorf("path %q, want %q", d.path, dir)
		}
	})
	refused := func(t *testing.T, flag string, getenv func(string) string, want ...string) {
		t.Helper()
		d, err := openRuntimeDir(flag, getenv)
		if err == nil {
			d.close()
			t.Fatal("accepted")
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q does not say %q", err, w)
			}
		}
	}
	t.Run("a mode other than 0700", func(t *testing.T) {
		for _, mode := range []fs.FileMode{0o755, 0o750, 0o701, 0o600} {
			base := t.TempDir()
			dir := filepath.Join(base, "perf-agent-mcp")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			refused(t, "", xdg(base), dir, mode.Perm().String(), "0700")
			refused(t, dir, xdg(""), dir, "0700")
		}
	})
	t.Run("another owner", func(t *testing.T) {
		defer func(prev func(fs.FileInfo) (int, bool)) { ownerOf = prev }(ownerOf)
		ownerOf = func(fs.FileInfo) (int, bool) { return os.Getuid() + 1, true }
		base := t.TempDir()
		refused(t, "", xdg(base), "owned by uid", "not")
	})
	t.Run("a symlink", func(t *testing.T) {
		base := t.TempDir()
		target := t.TempDir()
		if err := os.Chmod(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(base, "perf-agent-mcp")); err != nil {
			t.Fatal(err)
		}
		refused(t, "", xdg(base), "symbolic link")
		refused(t, filepath.Join(base, "perf-agent-mcp"), xdg(""), "symbolic link")
	})
	t.Run("not a directory", func(t *testing.T) {
		base := t.TempDir()
		if err := os.WriteFile(filepath.Join(base, "perf-agent-mcp"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		refused(t, "", xdg(base), "not a directory")
	})
	t.Run("a missing flag directory", func(t *testing.T) {
		refused(t, filepath.Join(t.TempDir(), "absent"), xdg(""), "absent")
	})
	t.Run("a relative XDG_RUNTIME_DIR", func(t *testing.T) {
		refused(t, "", xdg("relative/dir"), "XDG_RUNTIME_DIR", "absolute")
	})
}

func TestRuntimeDirNeedsXDGOrAFlag(t *testing.T) {
	_, err := openRuntimeDir("", xdg(""))
	if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") || !strings.Contains(err.Error(), "--runtime-dir") {
		t.Errorf("openRuntimeDir without either: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	code := runServe(ctx, []string{"--profiles", t.TempDir(), "--port", "1"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "XDG_RUNTIME_DIR") || !strings.Contains(stderr.String(), "--runtime-dir") {
		t.Errorf("runServe: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// The lock is the server's presence: running reports it from another
// open of hub.lock, and take refuses while it is held.
func TestTheLockIsThePresence(t *testing.T) {
	base := t.TempDir()
	first, err := openRuntimeDir("", xdg(base))
	if err != nil {
		t.Fatal(err)
	}
	if running(first.path) {
		t.Error("running before any take")
	}
	if _, err := first.take(); err != nil {
		t.Fatal(err)
	}
	tok, err := livehttp.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	info := hubInfo{PID: 4242, Port: 7341, MCPURL: "http://127.0.0.1:7341/mcp", Started: time.Now(), Version: "test"}
	if err := first.write(info, tok, "http://127.0.0.1:7341/s/x/"); err != nil {
		t.Fatal(err)
	}
	if !running(first.path) {
		t.Error("not running while the lock is held")
	}
	second, err := openRuntimeDir("", xdg(base))
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	holder, err := second.take()
	if err == nil || !strings.Contains(err.Error(), "perf-agent-hub is already running (pid 4242, port 7341)") || holder.PID != 4242 {
		t.Errorf("a second take: %+v %v", holder, err)
	}
	// The refused take removed nothing.
	if _, err := os.Stat(filepath.Join(first.path, "mcp-token")); err != nil {
		t.Errorf("the refused take touched the files: %v", err)
	}
	if err := first.remove(); err != nil {
		t.Fatal(err)
	}
	if running(first.path) {
		t.Error("running after remove")
	}
	ents, _ := os.ReadDir(first.path)
	if len(ents) != 1 || ents[0].Name() != "hub.lock" {
		t.Errorf("after remove: %v", ents)
	}
	if _, err := second.take(); err != nil {
		t.Errorf("take after remove: %v", err)
	}
}
