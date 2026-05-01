package nspid

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeStatus(t *testing.T, root string, pid int, contents string) {
	t.Helper()
	dir := filepath.Join(root, "proc", strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTranslate_HostNamespace(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, 1, "Name:\tcat\nNSpid:\t12345\n")
	got, err := translateAt(filepath.Join(root, "proc"), 1)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got != 12345 {
		t.Errorf("hostPID = %d, want 12345", got)
	}
}

func TestTranslate_SharedPodNamespace(t *testing.T) {
	root := t.TempDir()
	// Two-column NSpid: outermost (host) first, then in-pod ns.
	writeStatus(t, root, 1, "Name:\tjava\nNSpid:\t12345\t5\n")
	got, err := translateAt(filepath.Join(root, "proc"), 1)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got != 12345 {
		t.Errorf("hostPID = %d, want 12345 (outermost column)", got)
	}
}

func TestTranslate_MissingProcess(t *testing.T) {
	root := t.TempDir()
	// /proc/1/status doesn't exist
	_, err := translateAt(filepath.Join(root, "proc"), 1)
	if err == nil {
		t.Fatal("expected error for missing /proc/<pid>/status")
	}
}

func TestTranslate_MissingNSpidLine(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, 1, "Name:\told_kernel\n") // no NSpid line at all
	_, err := translateAt(filepath.Join(root, "proc"), 1)
	if err == nil {
		t.Fatal("expected error for missing NSpid line")
	}
}

func TestTranslate_InvalidPID(t *testing.T) {
	root := t.TempDir()
	for _, pid := range []int{0, -1, -999} {
		_, err := translateAt(filepath.Join(root, "proc"), pid)
		if err == nil {
			t.Errorf("translateAt(pid=%d) returned nil error", pid)
		}
	}
}

func TestTranslate_ThreeDeepNamespace(t *testing.T) {
	root := t.TempDir()
	// Three columns: host -> pod -> container.
	writeStatus(t, root, 1, "Name:\tjava\nNSpid:\t99999\t100\t5\n")
	got, err := translateAt(filepath.Join(root, "proc"), 1)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got != 99999 {
		t.Errorf("hostPID = %d, want 99999 (outermost of three)", got)
	}
}
