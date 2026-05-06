package debuginfod

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewWithoutURLsErrors(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, ErrNoURLs) {
		t.Fatalf("New with no URLs: got %v, want ErrNoURLs", err)
	}
}

func TestNewBasicLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()
	s, err := New(Options{
		URLs:     []string{srv.URL},
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent close: second call returns ErrClosed.
	if err := s.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: got %v, want ErrClosed", err)
	}
}
