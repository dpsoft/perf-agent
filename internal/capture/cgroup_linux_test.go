package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

func TestDelegatedCgroupParsing(t *testing.T) {
	// File modes decide the answer, and root ignores them.
	capturetest.SkipIfPrivileged(t)
	mount := t.TempDir()
	mk := func(rel string, files map[string]os.FileMode) {
		dir := filepath.Join(mount, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, mode := range files {
			if err := os.WriteFile(filepath.Join(dir, name), nil, mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("ok/scope", map[string]os.FileMode{"cgroup.kill": 0o200, "cgroup.procs": 0o644})
	mk("nokill/scope", map[string]os.FileMode{"cgroup.procs": 0o644})
	mk("roprocs/scope", map[string]os.FileMode{"cgroup.kill": 0o200, "cgroup.procs": 0o444})
	mk("rodir/scope", map[string]os.FileMode{"cgroup.kill": 0o200, "cgroup.procs": 0o644})
	if err := os.Chmod(filepath.Join(mount, "rodir/scope"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(mount, "rodir/scope"), 0o755) })

	for _, tc := range []struct {
		name, self, parent, why string
	}{
		{"delegated", "0::/ok/scope\n", filepath.Join(mount, "ok/scope"), ""},
		{"after v1 lines", "12:pids:/x\n1:name=systemd:/y\n0::/ok/scope\n", filepath.Join(mount, "ok/scope"), ""},
		{"no cgroup.kill", "0::/nokill/scope\n", "", "cgroup.kill"},
		{"cgroup.procs read-only", "0::/roprocs/scope\n", "", "cgroup.procs"},
		{"directory read-only", "0::/rodir/scope\n", "", "not writable"},
		{"v1 only", "12:pids:/x\n1:name=systemd:/y\n", "", "0::"},
		{"missing", "0::/gone\n", "", "not writable"},
		{"relative", "0::scope\n", "", "absolute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, why := DelegatedCgroup([]byte(tc.self), mount)
			if parent != tc.parent {
				t.Errorf("parent = %q, want %q", parent, tc.parent)
			}
			if tc.why == "" && why != "" || tc.why != "" && !strings.Contains(why, tc.why) {
				t.Errorf("why = %q, want it to contain %q", why, tc.why)
			}
		})
	}
}

func TestCreateCgroupRefusesAPathName(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b"} {
		if _, err := CreateCgroup(parent, name); err == nil {
			t.Errorf("CreateCgroup(%q) succeeded", name)
		}
	}
	p, err := CreateCgroup(parent, "perf-agent-mcp-1")
	if err != nil || p != filepath.Join(parent, "perf-agent-mcp-1") {
		t.Fatalf("CreateCgroup = %q, %v", p, err)
	}
	if _, err := CreateCgroup(parent, "perf-agent-mcp-1"); err == nil {
		t.Error("CreateCgroup reused an existing directory")
	}
	if err := RemoveCgroup(p); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCgroup(p); err != nil {
		t.Errorf("RemoveCgroup of a removed cgroup = %v, want nil", err)
	}
}
