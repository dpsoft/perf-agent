package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindTargetsAllowsAnEmptyNodeWhenNotRequiringOne(t *testing.T) {
	// A shim nothing has mapped. A collector on a node with no GPU pods
	// yet is the NORMAL case, not an error.
	shim := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(shim, []byte("\x7fELF not really"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findTargets(shim, 200*time.Millisecond, false)
	if err != nil {
		t.Fatalf("findTargets(requireOne=false) = %v; an empty node must not be an error", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want no targets", got)
	}
}

func TestFindTargetsStillFailsWhenOneIsRequired(t *testing.T) {
	// -pid and single-shot launch runs still want the old behaviour: if the
	// caller says a target must exist and none does, that is a
	// misconfiguration worth refusing.
	shim := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(shim, []byte("\x7fELF not really"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := findTargets(shim, 200*time.Millisecond, true); err == nil {
		t.Fatal("findTargets(requireOne=true) returned nil error with no targets")
	}
}
