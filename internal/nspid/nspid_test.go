package nspid

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStatus(t *testing.T, root string, pid int, contents string) {
	t.Helper()
	dir := filepath.Join(root, "proc", "1")
	_ = pid // pid arg is for clarity; we always write under /proc/1 for the synthetic fixture
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
