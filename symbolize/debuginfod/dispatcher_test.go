package debuginfod

import (
	"debug/elf"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSymbolizer(t *testing.T, urls []string) *Symbolizer {
	t.Helper()
	s, err := New(Options{
		URLs:     urls,
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDispatchDecisionNoBuildID(t *testing.T) {
	s := newTestSymbolizer(t, []string{"http://example.invalid"})
	got := s.dispatchDecision(t.Context(), "/dev/null", "")
	if got != "" {
		t.Fatalf("expected empty (NULL), got %q", got)
	}
}

func TestDispatchDecisionFetchOnSidecar(t *testing.T) {
	body := "FAKE_ELF"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/executable") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	s := newTestSymbolizer(t, []string{srv.URL})
	// Bypass readBuildID with a known build-id and a non-existent path
	// (binaryReadable=false), forcing the case 4 branch.
	got := s.dispatchDecisionForTest(t.Context(), "/nonexistent/foo", "/nonexistent/foo", "deadbeef0011223344")
	want := filepath.Join(".build-id", "de", "adbeef0011223344")
	if !strings.HasSuffix(got, want) {
		t.Fatalf("expected returned executable path ending in %q, got %q", want, got)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read returned path: %v", err)
	}
	if string(data) != body {
		t.Fatalf("file body = %q, want %q", data, body)
	}
}

func TestSymbolizeElfVirtRewritesAddressToOriginalIP(t *testing.T) {
	// Build a tiny Go program with DWARF and use its .debug file as the
	// symbolization source. Pick any resolvable function symbol and verify:
	//   1. The function name matches what blazesym would resolve.
	//   2. Frame.Address equals originalIPs[i] (NOT virtOffsets[i]).
	debugPath := buildGoFixtureWithDWARF(t, t.TempDir())
	resolvableVA, resolvableName := pickResolvableSymbol(t, debugPath)

	sym, err := New(Options{
		URLs:     []string{"http://example.invalid"},
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sym.Close() }()

	originalIPs := []uint64{0xdeadbeef00000000} // a deliberately-not-VA value
	virtOffsets := []uint64{resolvableVA}

	frames, err := sym.cgo.symbolizeElfVirt(debugPath, originalIPs, virtOffsets)
	if err != nil {
		t.Fatalf("symbolizeElfVirt: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("got 0 frames")
	}
	if frames[0].Name != resolvableName {
		t.Errorf("frames[0].Name = %q, want %q", frames[0].Name, resolvableName)
	}
	// Critical: the frame's Address MUST be originalIPs[0], not virtOffsets[0].
	// Without this rewrite, pprof.Profile.Resolve cannot route this location
	// to its containing mapping.
	if frames[0].Address != originalIPs[0] {
		t.Errorf("frames[0].Address = %#x, want %#x (originalIPs[0]) — file-mode address rewrite is broken",
			frames[0].Address, originalIPs[0])
	}
	// Address rewrite must also propagate to every inlined chain entry.
	for j, in := range frames[0].Inlined {
		if in.Address != originalIPs[0] {
			t.Errorf("frames[0].Inlined[%d].Address = %#x, want %#x (originalIPs[0])",
				j, in.Address, originalIPs[0])
		}
	}
}

// buildGoFixtureWithDWARF compiles a tiny Go program with DWARF and extracts
// only the .debug sections via objcopy --only-keep-debug. Returns the .debug path.
func buildGoFixtureWithDWARF(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main

func helloWorld() string { return "hi" }
func main() { _ = helloWorld() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("go build failed (toolchain unavailable?): %v\n%s", err, out)
	}
	debug := filepath.Join(dir, "bin.debug")
	cmd = exec.Command("objcopy", "--only-keep-debug", bin, debug)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("objcopy failed: %v\n%s", err, out)
	}
	return debug
}

// pickResolvableSymbol returns (file-VA, name) of a function we'll round-trip.
func pickResolvableSymbol(t *testing.T, debugPath string) (uint64, string) {
	t.Helper()
	f, err := elf.Open(debugPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	syms, err := f.Symbols()
	if err != nil {
		t.Skipf("no symtab in fixture: %v", err)
	}
	for _, want := range []string{"main.helloWorld", "main.main"} {
		for _, s := range syms {
			if s.Name == want && s.Value != 0 {
				return s.Value, s.Name
			}
		}
	}
	t.Skip("no usable symbol in fixture")
	return 0, ""
}
