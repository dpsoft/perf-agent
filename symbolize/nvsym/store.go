package nvsym

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound means the server has no symbols for this build-id. It is the
// COMMON case, not an exceptional one: measured on one machine, the pip-wheel
// cuBLASLt returns 200 while the CUDA-toolkit build of the same library and
// libcupti.so.13.3 both return 403. A caller must treat it as "carry on
// without a name".
var ErrNotFound = errors.New("nvsym: no symbols for this build-id")

// maxSymbolBytes bounds a single download. The largest observed file is the
// ~30 MB cuBLASLt symbol table; 256 MB leaves room for growth while refusing
// to stream something unbounded into the cache directory.
const maxSymbolBytes = 256 << 20

// Store fetches symbol-only ELFs and keeps them on disk, keyed by build-id.
//
// Layout is the standard .build-id/<NN>/<rest>.debug tree, which is what
// blazesym's build-id resolver already looks in, so a fetched file needs no
// separate plumbing to be used -- pointing a symbolizer at Dir is enough.
type Store struct {
	Dir     string
	BaseURL string
	Client  *http.Client

	// negative remembers build-ids the server refused, so a run does not
	// re-ask once per stack. Bounded by the number of distinct modules in a
	// process, which is small; entries are never evicted because a 403 does
	// not become a 200 within one capture.
	negative sync.Map

	// inflight collapses concurrent requests for one build-id: a PyTorch
	// capture symbolizes many stacks at once and they hit the same handful of
	// libraries, so without this the first miss becomes N identical downloads.
	inflight sync.Map

	stats Stats
}

// Stats reports what the store did. Fetched and NotFound are separate because
// "the server had nothing" and "we could not reach it" are different facts and
// only the second is a problem worth reporting.
type Stats struct {
	Hits     atomic.Uint64 // already on disk
	Fetched  atomic.Uint64 // downloaded this run
	NotFound atomic.Uint64 // server answered, had nothing
	Errors   atomic.Uint64 // transport or write failure
	Bytes    atomic.Uint64
}

// pathFor returns the cache path for a build-id, or "" if it is too short to
// split. Mirrors symbolize/debuginfod/cache so the two can share a directory.
func (s *Store) pathFor(buildID string) string {
	if len(buildID) < 4 {
		return ""
	}
	return filepath.Join(s.Dir, ".build-id", buildID[:2], buildID[2:]+".debug")
}

// Path returns a local symbols file for the module, fetching it if needed.
//
// Returns ErrNotFound when the server has nothing, which is expected and not
// an error condition for the caller. Never returns a path that does not exist.
func (s *Store) Path(ctx context.Context, modulePath, buildID string) (string, error) {
	if s == nil || s.Dir == "" {
		return "", ErrNotFound
	}
	if !IsNVIDIAModule(modulePath) {
		return "", ErrNotFound
	}
	dst := s.pathFor(buildID)
	if dst == "" {
		return "", ErrNotFound
	}
	if _, err := os.Stat(dst); err == nil {
		s.stats.Hits.Add(1)
		return dst, nil
	}
	if _, refused := s.negative.Load(buildID); refused {
		return "", ErrNotFound
	}

	// One download per build-id even under concurrency.
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	actual, loaded := s.inflight.LoadOrStore(buildID, ch)
	if loaded {
		shared := actual.(chan result)
		select {
		case r := <-shared:
			shared <- r // put it back for any other waiter
			return r.path, r.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	defer s.inflight.Delete(buildID)

	p, err := s.download(ctx, modulePath, buildID, dst)
	if errors.Is(err, ErrNotFound) {
		s.negative.Store(buildID, struct{}{})
	}
	ch <- result{p, err}
	return p, err
}

func (s *Store) download(ctx context.Context, modulePath, buildID, dst string) (string, error) {
	base := s.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	// Two steps, because the build-id URL is a directory LISTING and not the
	// symbols. Fetching it and caching the body yields a 740-byte HTML page
	// that every later reader treats as a corrupt ELF -- measured, before
	// this: five "symbol files" totalling 20 KB, all of them
	// "<title>NVIDIA Binary Server</title>". The artifact is one [FILE] link
	// inside, and its name is not derivable from the soname: the listing for
	// libcuda.so serves libcuda.so.1.1.sym.
	listing := ListingURL(base, modulePath, buildID)
	if listing == "" {
		return "", ErrNotFound
	}
	body, err := s.get(ctx, listing)
	if err != nil {
		return "", err
	}
	page, err := io.ReadAll(io.LimitReader(body, 1<<20))
	_ = body.Close()
	if err != nil {
		s.stats.Errors.Add(1)
		return "", err
	}
	name := SymFileName(page)
	if name == "" {
		// A listing with no .sym link is the server saying it has nothing
		// for this build-id in a shape that still returns 200.
		s.stats.NotFound.Add(1)
		return "", ErrNotFound
	}

	fileURL := FileURL(base, modulePath, buildID, name)
	symBody, err := s.get(ctx, fileURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = symBody.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		s.stats.Errors.Add(1)
		return "", err
	}
	// Written to a temp file and renamed: a truncated download left at the
	// final path would be cached forever and would poison symbolization for
	// the rest of the machine's life.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".nvsym-*")
	if err != nil {
		s.stats.Errors.Add(1)
		return "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	n, err := io.Copy(tmp, io.LimitReader(symBody, maxSymbolBytes))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.stats.Errors.Add(1)
		return "", err
	}
	if n == maxSymbolBytes {
		s.stats.Errors.Add(1)
		return "", fmt.Errorf("nvsym: %s exceeded %d bytes", fileURL, int64(maxSymbolBytes))
	}
	// What arrives must be an ELF. Anything else -- an error page served with
	// 200, a redirect body -- is refused here rather than cached and handed
	// to a symbolizer that will fail obscurely much later.
	if !isELF(tmpName) {
		s.stats.Errors.Add(1)
		return "", fmt.Errorf("nvsym: %s did not return an ELF", fileURL)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		s.stats.Errors.Add(1)
		return "", err
	}
	s.stats.Fetched.Add(1)
	s.stats.Bytes.Add(uint64(n))
	return dst, nil
}

// get performs one request, mapping this server's absence codes to
// ErrNotFound. 403 is its "no", not 404: an unknown build-id and an all-zero
// one both come back 403, so treating only 404 as absence would report every
// ordinary miss as an error.
func (s *Store) get(ctx context.Context, url string) (io.ReadCloser, error) {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		s.stats.Errors.Add(1)
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		s.stats.Errors.Add(1)
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close()
		s.stats.NotFound.Add(1)
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		s.stats.Errors.Add(1)
		return nil, fmt.Errorf("nvsym: %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// isELF reports whether the file begins with the ELF magic.
func isELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{0x7f, 'E', 'L', 'F'}
}

// Snapshot returns the counters by value.
func (s *Store) Snapshot() (hits, fetched, notFound, errs, bytes uint64) {
	return s.stats.Hits.Load(), s.stats.Fetched.Load(), s.stats.NotFound.Load(),
		s.stats.Errors.Load(), s.stats.Bytes.Load()
}
