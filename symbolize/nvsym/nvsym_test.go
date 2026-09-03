package nvsym

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The measured contract: the key is the soname with EVERY version component
// stripped. This is the whole reason the package exists as more than a URL
// concatenation -- passing the name as it appears in /proc/<pid>/maps returns
// 403 for every library, which reads as "the server is down" rather than "we
// asked the wrong question".
func TestSonameKeyStripsEveryVersionComponent(t *testing.T) {
	cases := map[string]string{
		"/usr/lib64/libcuda.so.610.57.04": "libcuda.so",
		"/usr/lib64/libcuda.so.1":         "libcuda.so",
		"/usr/lib64/libcuda.so":           "libcuda.so",
		"/a/b/libcublasLt.so.13":          "libcublasLt.so",
		"/a/b/libcupti.so.13.3":           "libcupti.so",
		"libcudart.so.12":                 "libcudart.so",
	}
	for in, want := range cases {
		if got := SonameKey(in); got != want {
			t.Errorf("SonameKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// A key we cannot form must produce no request at all. A malformed path would
// come back 403, which is indistinguishable from a genuine miss, so the error
// would be attributed to the server rather than to us.
func TestSonameKeyRefusesWhatIsNotALibrary(t *testing.T) {
	for _, in := range []string{"", "  ", "/", ".", "/usr/bin/python3.12", "[kernel]", "vmlinux"} {
		if got := SonameKey(in); got != "" {
			t.Errorf("SonameKey(%q) = %q, want \"\"", in, got)
		}
	}
}

func TestListingURLMatchesTheServerContract(t *testing.T) {
	const id = "77d6d4f93fa0ae0840c46f3476a608c76fca7303"
	got := ListingURL(DefaultBaseURL, "/usr/lib64/libcuda.so.610.57.04", id)
	want := "https://cudatoolkit-symbols.nvidia.com/libcuda.so/" + id + "/index.html"
	if got != want {
		t.Fatalf("ListingURL =\n  %q\nwant\n  %q", got, want)
	}
	// Measured: this exact form returns 200 while libcuda.so.1/<id> and
	// libcuda.so.610.57.04/<id> both return 403.
}

// An all-zero build-id is what a note-less ELF hex-encodes to. The server
// answers 403 for it, so refusing locally saves a round trip AND keeps a
// meaningless request out of the failure statistics.
func TestListingURLRefusesUnusableInputs(t *testing.T) {
	const id = "77d6d4f93fa0ae0840c46f3476a608c76fca7303"
	cases := [][3]string{
		{DefaultBaseURL, "/usr/lib64/libcuda.so.1", ""},
		{DefaultBaseURL, "/usr/bin/python3.12", id},
		{DefaultBaseURL, "/usr/lib64/libcuda.so.1", "0000000000000000000000000000000000000000"},
	}
	for _, c := range cases {
		if got := ListingURL(c[0], c[1], c[2]); got != "" {
			t.Errorf("ListingURL(%q, %q) = %q, want \"\"", c[1], c[2], got)
		}
	}
}

// Narrower than the flame graph's vendor list on purpose: that one also covers
// AMD libraries, which this server knows nothing about, and every non-match
// saved is a network round trip and a build-id not disclosed.
func TestIsNVIDIAModuleSelectsOnlyWhatTheServerMightHave(t *testing.T) {
	yes := []string{
		"/usr/lib64/libcuda.so.610.57.04", "/x/libcupti.so.13", "/x/libcublasLt.so.13",
		"/x/libcudnn.so.9", "/x/libnccl.so.2", "/x/libnvidia-ml.so.1",
	}
	no := []string{
		"/x/libtorch_cpu.so", "/x/libc.so.6", "/x/libamdhip64.so",
		"/x/librocprofiler64.so", "/usr/bin/python3.12", "/x/libstdc++.so.6",
	}
	for _, m := range yes {
		if !IsNVIDIAModule(m) {
			t.Errorf("IsNVIDIAModule(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if IsNVIDIAModule(m) {
			t.Errorf("IsNVIDIAModule(%q) = true, want false", m)
		}
	}
}

// --- Store ---

func newTestStore(t *testing.T, h http.HandlerFunc) (*Store, *httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Store{Dir: t.TempDir(), BaseURL: srv.URL, Client: srv.Client()}, srv, &calls
}

const listingBody = `<html><body><table>
<tr><td>[FILE] <a href="libcuda.so.1.1.sym">libcuda.so.1.1.sym</a></td><td>4051 K</td></tr>
</table></body></html>`

// elfBody is a minimal file that starts with the ELF magic. The store refuses
// anything that does not, because an error page served with 200 that reached
// the cache would be handed to a symbolizer and fail obscurely much later.
var elfBody = append([]byte{0x7f, 'E', 'L', 'F'}, []byte("...symbols...")...)

func TestStoreFetchesAndCachesByBuildID(t *testing.T) {
	const id = "77d6d4f93fa0ae0840c46f3476a608c76fca7303"
	s, _, calls := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/libcuda.so/" + id + "/index.html":
			_, _ = w.Write([]byte(listingBody))
		case "/libcuda.so/" + id + "/libcuda.so.1.1.sym":
			_, _ = w.Write(elfBody)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	p1, err := s.Path(context.Background(), "/usr/lib64/libcuda.so.610.57.04", id)
	if err != nil {
		t.Fatalf("first Path: %v", err)
	}
	if b, _ := os.ReadFile(p1); string(b) != string(elfBody) {
		t.Fatalf("cached content = %q, want the ELF body (not the listing)", b)
	}
	// Second call must be served from disk. Without this the store would
	// re-download once per stack, which for a PyTorch capture is thousands of
	// times for the same handful of libraries.
	p2, err := s.Path(context.Background(), "/usr/lib64/libcuda.so.610.57.04", id)
	if err != nil || p2 != p1 {
		t.Fatalf("second Path = %q, %v; want %q, nil", p2, err, p1)
	}
	// Two requests for the first fetch (listing, then file), none for the
	// second: the cache hit must not re-walk the listing either.
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("server hit %d times, want 2 (listing + file)", got)
	}
	if hits, fetched, _, _, _ := s.Snapshot(); hits != 1 || fetched != 1 {
		t.Fatalf("hits=%d fetched=%d, want 1 and 1", hits, fetched)
	}
}

// 403, not 404, is this server's "no". Treating only 404 as absence would
// report every ordinary miss as an error.
func TestStoreTreats403AsAbsenceAndDoesNotReAsk(t *testing.T) {
	s, _, calls := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for range 3 {
		if _, err := s.Path(context.Background(), "/x/libcupti.so.13", id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("server asked %d times for a known-refused build-id, want 1", got)
	}
	if _, _, nf, errs, _ := s.Snapshot(); nf != 1 || errs != 0 {
		t.Fatalf("notFound=%d errors=%d, want 1 and 0", nf, errs)
	}
}

// A non-NVIDIA module must never reach the network: no round trip, and no
// build-id of an unrelated binary disclosed to a third party.
func TestStoreNeverAsksAboutForeignModules(t *testing.T) {
	s, _, calls := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be contacted for a non-NVIDIA module")
	})
	_, err := s.Path(context.Background(), "/x/libtorch_cpu.so", "77d6d4f93fa0ae0840c46f3476a608c76fca7303")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("server contacted %d times, want 0", got)
	}
}

// A failed download must leave nothing behind: a truncated file at the final
// path would be cached forever and would poison symbolization permanently.
func TestStoreLeavesNoFileWhenTheServerErrors(t *testing.T) {
	s, _, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	const id = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := s.Path(context.Background(), "/x/libcuda.so.1", id); err == nil {
		t.Fatal("want an error on 500")
	}
	if _, err := os.Stat(s.pathFor(id)); !os.IsNotExist(err) {
		t.Fatalf("a file was left at the cache path after a failed fetch")
	}
	entries, _ := filepath.Glob(filepath.Join(s.Dir, ".build-id", "*", ".nvsym-*"))
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// Concurrent lookups for one build-id must collapse to a single download: a
// capture symbolizes many stacks at once through the same few libraries.
func TestStoreCollapsesConcurrentFetches(t *testing.T) {
	release := make(chan struct{})
	s, _, calls := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		if strings.HasSuffix(r.URL.Path, "/index.html") {
			_, _ = w.Write([]byte(strings.Replace(listingBody, "libcuda.so.1.1.sym", "libcupti.so.13.sym", 2)))
			return
		}
		_, _ = w.Write(elfBody)
	})
	const id = "cccccccccccccccccccccccccccccccccccccccc"

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = s.Path(context.Background(), "/x/libcupti.so.13", id) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("server hit %d times concurrently, want 2 (one listing + one file)", got)
	}
}

// The build-id URL is a directory listing, not the symbols. Caching its body
// produced five "symbol files" of ~740 bytes each, all of them
// "<title>NVIDIA Binary Server</title>", which every later reader would treat
// as a corrupt ELF. The name inside is not derivable from the soname either:
// the listing for libcuda.so serves libcuda.so.1.1.sym.
func TestSymFileNameReadsTheListing(t *testing.T) {
	if got := SymFileName([]byte(listingBody)); got != "libcuda.so.1.1.sym" {
		t.Fatalf("SymFileName = %q, want libcuda.so.1.1.sym", got)
	}
	for _, bad := range []string{
		"<html>no links here</html>",
		`<a href="../../../etc/passwd.sym">x</a>`,
		`<a href="sub/dir/x.sym">x</a>`,
		"",
	} {
		if got := SymFileName([]byte(bad)); got != "" {
			t.Errorf("SymFileName(%q) = %q, want \"\"", bad, got)
		}
	}
}

// A 200 that is not an ELF must not be cached. Otherwise an error page or a
// changed format is stored under a build-id and poisons that library's
// symbolization until someone clears the cache by hand.
func TestStoreRefusesANonELFBody(t *testing.T) {
	const id = "dddddddddddddddddddddddddddddddddddddddd"
	s, _, _ := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/index.html") {
			_, _ = w.Write([]byte(listingBody))
			return
		}
		_, _ = w.Write([]byte("<html>maintenance</html>"))
	})
	if _, err := s.Path(context.Background(), "/usr/lib64/libcuda.so.1", id); err == nil {
		t.Fatal("want an error when the served body is not an ELF")
	}
	if _, err := os.Stat(s.pathFor(id)); !os.IsNotExist(err) {
		t.Fatal("a non-ELF body was cached")
	}
}
