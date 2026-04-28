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

func TestParseCgroupPathMetadataFromSystemdKubePath(t *testing.T) {
	path := "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod2af2f6f1_1111_2222_3333_444444444444.slice/cri-containerd-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.scope"

	meta := parseCgroupPathMetadata(path)

	if got, want := meta.PodUID, "2af2f6f1-1111-2222-3333-444444444444"; got != want {
		t.Fatalf("pod uid=%q want %q", got, want)
	}
	if got, want := meta.ContainerRuntime, "containerd"; got != want {
		t.Fatalf("runtime=%q want %q", got, want)
	}
	if got, want := meta.ContainerID, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"; got != want {
		t.Fatalf("container id=%q want %q", got, want)
	}
}

func TestParseCgroupPathMetadataFromCgroupfsKubePath(t *testing.T) {
	path := "/kubepods/burstable/pod2af2f6f1-1111-2222-3333-444444444444/abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	meta := parseCgroupPathMetadata(path)

	if got, want := meta.PodUID, "2af2f6f1-1111-2222-3333-444444444444"; got != want {
		t.Fatalf("pod uid=%q want %q", got, want)
	}
	if got, want := meta.ContainerID, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"; got != want {
		t.Fatalf("container id=%q want %q", got, want)
	}
	if meta.ContainerRuntime != "" {
		t.Fatalf("runtime=%q want empty", meta.ContainerRuntime)
	}
}

func TestCgroupPathCacheCachesHitsAndMisses(t *testing.T) {
	calls := 0
	cache := newCgroupPathCache(func(pid uint32) (string, bool) {
		calls++
		if pid == 42 {
			return "/kubepods/pod-abc/container-def", true
		}
		return "", false
	})

	if got, ok := cache.Lookup(42); !ok || got != "/kubepods/pod-abc/container-def" {
		t.Fatalf("Lookup(42)=(%q,%v)", got, ok)
	}
	if got, ok := cache.Lookup(42); !ok || got != "/kubepods/pod-abc/container-def" {
		t.Fatalf("Lookup(42) cached=(%q,%v)", got, ok)
	}
	if _, ok := cache.Lookup(77); ok {
		t.Fatal("expected miss for pid 77")
	}
	if _, ok := cache.Lookup(77); ok {
		t.Fatal("expected cached miss for pid 77")
	}
	if calls != 2 {
		t.Fatalf("lookup calls=%d want 2", calls)
	}
}
