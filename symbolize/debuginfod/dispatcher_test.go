package debuginfod

import (
	"net/http"
	"net/http/httptest"
	"os"
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
