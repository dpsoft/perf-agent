package cgroupmeta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupPathFromPrefersUnifiedHierarchy(t *testing.T) {
	root := t.TempDir()
	pidDir := filepath.Join(root, "123")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data := "0::/kubepods.slice/pod-abc/cri-containerd-deadbeef.scope\n"
	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(data), 0o644); err != nil {
		t.Fatalf("write cgroup: %v", err)
	}

	got, ok := LookupPathFrom(root, 123)
	if !ok {
		t.Fatal("LookupPathFrom ok=false")
	}
	if got != "/kubepods.slice/pod-abc/cri-containerd-deadbeef.scope" {
		t.Fatalf("path=%q", got)
	}
}

func TestMetadataFromPathSystemdKubePath(t *testing.T) {
	meta := MetadataFromPath("/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod2af2f6f1_1111_2222_3333_444444444444.slice/cri-containerd-0123456789abcdef.scope")
	if got := meta.PodUID; got != "2af2f6f1-1111-2222-3333-444444444444" {
		t.Fatalf("pod_uid=%q", got)
	}
	if got := meta.ContainerRuntime; got != "containerd" {
		t.Fatalf("runtime=%q", got)
	}
	if got := meta.ContainerID; got != "0123456789abcdef" {
		t.Fatalf("container_id=%q", got)
	}
}
