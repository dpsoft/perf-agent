package linuxdrm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupCgroupPathPrefersUnifiedHierarchy(t *testing.T) {
	root := t.TempDir()
	pidDir := filepath.Join(root, "123")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "10:memory:/legacy.slice\n0::/kubepods.slice/pod-abc/container-def\n"
	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, ok := lookupCgroupPathFrom(root, 123)
	if !ok {
		t.Fatal("expected cgroup path")
	}
	if want := "/kubepods.slice/pod-abc/container-def"; got != want {
		t.Fatalf("cgroup path=%q want %q", got, want)
	}
}

func TestLookupCgroupPathFallsBackToLegacyControllerPath(t *testing.T) {
	root := t.TempDir()
	pidDir := filepath.Join(root, "321")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "9:cpuset:/kubepods/burstable/pod-xyz/container-123\n"
	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, ok := lookupCgroupPathFrom(root, 321)
	if !ok {
		t.Fatal("expected cgroup path")
	}
	if want := "/kubepods/burstable/pod-xyz/container-123"; got != want {
		t.Fatalf("cgroup path=%q want %q", got, want)
	}
}

func TestLookupCgroupPathRejectsMissingFile(t *testing.T) {
	if _, ok := lookupCgroupPathFrom(t.TempDir(), 777); ok {
		t.Fatal("expected missing cgroup file to fail")
	}
}
