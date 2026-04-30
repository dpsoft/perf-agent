package k8slabels

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeCgroup creates a fake /proc/<pid>/cgroup at procRoot. The procRoot
// passed to FromPID should be this same directory (procRoot, not "proc").
func writeCgroup(t *testing.T, procRoot string, pid int, content string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFromPID_KubepodsContainerd(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir()
	writeCgroup(t, root, 1,
		"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc123def456.scope\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if got["pod_uid"] != "12345678-1234-1234-1234-123456789abc" {
		t.Errorf("pod_uid = %q", got["pod_uid"])
	}
	if got["container_id"] != "abc123def456" {
		t.Errorf("container_id = %q", got["container_id"])
	}
	if got["cgroup_path"] == "" {
		t.Errorf("cgroup_path missing")
	}
	if _, ok := got["pod_name"]; ok {
		t.Errorf("pod_name should not be set when env unset")
	}
}

func TestFromPID_NotKubernetes(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir()
	writeCgroup(t, root, 1, "0::/user.slice/user-1000.slice/session-1.scope\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if got["cgroup_path"] != "/user.slice/user-1000.slice/session-1.scope" {
		t.Errorf("cgroup_path = %q", got["cgroup_path"])
	}
	if _, ok := got["pod_uid"]; ok {
		t.Errorf("pod_uid should not be set on non-k8s host")
	}
	if _, ok := got["container_id"]; ok {
		t.Errorf("container_id should not be set on non-k8s host")
	}
}

func TestFromPID_V1Only(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir()
	writeCgroup(t, root, 1, "12:devices:/some/v1/path\n2:cpu,cpuacct:/foo\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("v1-only host should produce no labels, got %v", got)
	}
}

func TestFromPID_WithDownwardAPI(t *testing.T) {
	t.Setenv("POD_NAME", "my-app-xkz2q")
	t.Setenv("POD_NAMESPACE", "production")
	t.Setenv("CONTAINER_NAME", "my-app")
	root := t.TempDir()
	writeCgroup(t, root, 1,
		"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc.scope\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if got["pod_name"] != "my-app-xkz2q" {
		t.Errorf("pod_name = %q", got["pod_name"])
	}
	if got["namespace"] != "production" {
		t.Errorf("namespace = %q", got["namespace"])
	}
	if got["container_name"] != "my-app" {
		t.Errorf("container_name = %q", got["container_name"])
	}
}

func TestFromPID_ProcessGone(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir() // no /1 dir created
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID should not error when /proc/<pid>/cgroup is missing: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing cgroup file should produce no labels, got %v", got)
	}
}
