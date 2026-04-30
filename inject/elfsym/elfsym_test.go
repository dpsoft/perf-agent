package elfsym

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSymbols_DynsymHit(t *testing.T) {
	libc := findLibcPath(t)
	resolved, err := ResolveSymbols(libc, []string{"malloc"})
	if err != nil {
		t.Fatalf("ResolveSymbols(%q): %v", libc, err)
	}
	if resolved["malloc"] == 0 {
		t.Fatalf("ResolveSymbols(%q): malloc not resolved (got 0)", libc)
	}
}

func TestResolveSymbols_MissingByName(t *testing.T) {
	libc := findLibcPath(t)
	resolved, err := ResolveSymbols(libc, []string{"this_symbol_does_not_exist_xyz"})
	if err != nil {
		t.Fatalf("ResolveSymbols: unexpected error: %v", err)
	}
	if v, present := resolved["this_symbol_does_not_exist_xyz"]; present {
		t.Fatalf("expected symbol absent; got address 0x%x", v)
	}
}

func TestResolveSymbols_MultipleSymbols(t *testing.T) {
	libc := findLibcPath(t)
	resolved, err := ResolveSymbols(libc, []string{"malloc", "free", "this_does_not_exist"})
	if err != nil {
		t.Fatalf("ResolveSymbols: %v", err)
	}
	if resolved["malloc"] == 0 {
		t.Fatal("malloc not resolved")
	}
	if resolved["free"] == 0 {
		t.Fatal("free not resolved")
	}
	if _, present := resolved["this_does_not_exist"]; present {
		t.Fatal("nonexistent symbol unexpectedly present")
	}
}

func TestResolveSymbols_FileDoesNotExist(t *testing.T) {
	_, err := ResolveSymbols("/path/that/definitely/does/not/exist", []string{"malloc"})
	if err == nil {
		t.Fatal("expected error for nonexistent file; got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected wrapped os.ErrNotExist; got %v", err)
	}
}

// findLibcPath locates a libc.so.6 on the test host. Skips the test if not found.
func findLibcPath(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib/aarch64-linux-gnu/libc.so.6",
		"/lib64/libc.so.6",
		"/usr/lib/x86_64-linux-gnu/libc.so.6",
		"/usr/lib/aarch64-linux-gnu/libc.so.6",
		"/usr/lib64/libc.so.6",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	matches, _ := filepath.Glob("/usr/lib*/libc.so*")
	for _, m := range matches {
		if _, err := os.Stat(m); err == nil {
			return m
		}
	}
	t.Skip("libc.so.6 not found on test host; skipping ELF resolver test")
	return ""
}
