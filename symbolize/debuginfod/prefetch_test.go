package debuginfod

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// Prefetch fetches what a capture would have tried to fetch, once each, and
// nothing that is already local. Issue #224.
func TestPrefetchFetchesWhatACaptureWouldAndNothingElse(t *testing.T) {
	var (
		served  = strings.Repeat("a", 40) // on the server
		missing = strings.Repeat("b", 40) // 404
		cached  = strings.Repeat("c", 40) // already in the cache
	)
	bins := t.TempDir()
	binServed := writeStrippedELFWithBuildID(t, filepath.Join(bins, "served"), served)
	binMissing := writeStrippedELFWithBuildID(t, filepath.Join(bins, "missing"), missing)
	binCached := writeStrippedELFWithBuildID(t, filepath.Join(bins, "cached"), cached)

	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/buildid/"+served+"/debuginfo" {
			_, _ = w.Write([]byte("\x7fELF debuginfo"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cachedPath := filepath.Join(dir, ".build-id", cached[:2], cached[2:]+".debug")
	if err := os.MkdirAll(filepath.Dir(cachedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachedPath, []byte("\x7fELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A capture found the served file too slow; prefetch must fetch it anyway
	// and clear the verdict.
	writeSlowMarker(dir, "debuginfo", served, slowMarker{Size: 1 << 30, BytesPerSec: 1, At: time.Now()})

	// Two processes sharing a library, as every process shares libc.
	maps := map[uint32][]procmap.Mapping{
		1: {{Path: binServed}, {Path: binMissing}, {Path: binCached}},
		2: {{Path: binServed}, {Path: binCached}},
	}
	res, err := prefetch(t.Context(), PrefetchOptions{
		URLs:     []string{srv.URL},
		CacheDir: dir,
		PIDs:     []uint32{1, 2, 3},
	}, func(pid uint32) ([]procmap.Mapping, error) {
		m, ok := maps[pid]
		if !ok {
			return nil, os.ErrNotExist // pid 3: exited, or not ours to read
		}
		return m, nil
	})
	if err != nil {
		t.Fatalf("prefetch: %v", err)
	}

	want := PrefetchResult{Processes: 2, Unreadable: 1, AlreadyLocal: 1, Fetched: 1, NotFound: 1, Bytes: int64(len("\x7fELF debuginfo"))}
	if res != want {
		t.Errorf("result %+v, want %+v", res, want)
	}
	if n := hits["/buildid/"+served+"/debuginfo"]; n != 1 {
		t.Errorf("served file requested %d times, want 1: a library mapped by two processes was fetched per process", n)
	}
	if n := hits["/buildid/"+cached+"/debuginfo"]; n != 0 {
		t.Errorf("cached file requested %d times; prefetch fetched something already local", n)
	}
	c := &cache.Cache{Dir: dir}
	if !c.Has(served, cache.KindDebuginfo) {
		t.Error("the fetched file is not where a capture looks for it")
	}
	if _, ok := readSlowMarker(dir, "debuginfo", served); ok {
		t.Error("the too-slow marker survived the file being fetched")
	}
}

// A download the server drops is tried again on a fresh request, past the
// in-memory failure memo that would otherwise answer for it. A 404 is not.
func TestPrefetchRetriesADroppedDownloadButNotA404(t *testing.T) {
	flaky := strings.Repeat("e", 40)
	gone := strings.Repeat("f", 40)
	bins := t.TempDir()
	binFlaky := writeStrippedELFWithBuildID(t, filepath.Join(bins, "flaky"), flaky)
	binGone := writeStrippedELFWithBuildID(t, filepath.Join(bins, "gone"), gone)

	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		n := hits[r.URL.Path]
		mu.Unlock()
		if r.URL.Path == "/buildid/"+flaky+"/debuginfo" {
			if n == 1 {
				http.Error(w, "dropped", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte("\x7fELF"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	res, err := prefetch(t.Context(), PrefetchOptions{URLs: []string{srv.URL}, CacheDir: t.TempDir(), PIDs: []uint32{1}},
		func(uint32) ([]procmap.Mapping, error) {
			return []procmap.Mapping{{Path: binFlaky}, {Path: binGone}}, nil
		})
	if err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	if res.Fetched != 1 || res.Failed != 0 || res.NotFound != 1 {
		t.Errorf("result %+v, want the dropped download fetched on retry and the 404 counted", res)
	}
	if n := hits["/buildid/"+flaky+"/debuginfo"]; n != 2 {
		t.Errorf("dropped download requested %d times, want 2", n)
	}
	if n := hits["/buildid/"+gone+"/debuginfo"]; n != 1 {
		t.Errorf("404 requested %d times, want 1: a 404 is an answer, not a failure to retry", n)
	}
}
