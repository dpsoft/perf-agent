package debuginfod

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

func newServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetchOnce200(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/buildid/aabb/debuginfo" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
	})
	f := newFetcher([]string{url}, http.DefaultClient)
	body, err := f.fetchURL(t.Context(), url+"/buildid/aabb/debuginfo")
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	defer body.Close()
	buf := make([]byte, 16)
	n, _ := body.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Fatalf("body = %q", buf[:n])
	}
}

func TestFetchOnceFallback404(t *testing.T) {
	first := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	second := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	f := newFetcher([]string{first, second}, http.DefaultClient)
	body, err := f.fetch(t.Context(), "debuginfo", "aabbcc")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer body.Close()
	buf := make([]byte, 16)
	n, _ := body.Read(buf)
	if string(buf[:n]) != "ok" {
		t.Fatalf("body = %q", buf[:n])
	}
}

func TestFetchAll404ReturnsErrNotFound(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	f := newFetcher([]string{url}, http.DefaultClient)
	if _, err := f.fetch(t.Context(), "debuginfo", "aabbcc"); err == nil || err.Error() != "debuginfod: not found" {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	})
	client := &http.Client{Timeout: 50 * time.Millisecond}
	f := newFetcher([]string{url}, client)
	if _, err := f.fetch(t.Context(), "debuginfo", "aabbcc"); err == nil {
		t.Fatalf("expected timeout error")
	}
}

func TestFetchTrimsTrailingSlash(t *testing.T) {
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Asserts no double-slash like /buildid//debuginfo.
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("path has //: %q", r.URL.Path)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	f := newFetcher([]string{url + "/"}, http.DefaultClient) // trailing slash
	body, err := f.fetch(t.Context(), "debuginfo", "aabbcc")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	body.Close()
}

// cacheBackend is a tiny test stub: it adapts a freshly-created cache.Cache
// (no Index) to the singleflightFetcher's storer interface.
type cacheBackend struct {
	baseDir string
}

func (c *cacheBackend) WriteAtomic(buildID string, kind cache.Kind, body io.Reader) (string, error) {
	cc := &cache.Cache{Dir: c.baseDir}
	return cc.WriteAtomic(buildID, kind, body)
}

func TestSingleflightCollapsesConcurrentFetches(t *testing.T) {
	var calls atomic.Int32
	url := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})

	dir := t.TempDir()
	c := &cacheBackend{baseDir: dir}
	sf := newSingleflightFetcher(newFetcher([]string{url}, http.DefaultClient), c)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			path, err := sf.fetchAndStore(t.Context(), "debuginfo", "aabbccddeeff")
			if err != nil {
				t.Errorf("fetch: %v", err)
			}
			if path == "" {
				t.Errorf("empty path")
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("HTTP call count = %d, want 1 (singleflight failure)", got)
	}
}
