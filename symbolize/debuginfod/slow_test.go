package debuginfod

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/symbolize/debuginfod/cache"
)

const slowTestBuildID = "aabbccddeeff00112233445566778899aabbccdd"

// slowServer declares size bytes and sends them at about bytesPerSec,
// counting the requests it gets.
func slowServer(t *testing.T, size, bytesPerSec int) (*httptest.Server, *atomic.Int64) {
	return slowServerEncoding(t, size, bytesPerSec, false)
}

// slowServerEncoding is slowServer that, with gzipped, answers as Fedora's
// debuginfod does to Go's default Accept-Encoding: gzip on the wire, no
// Content-Length, the uncompressed size only in X-Debuginfod-Size. The data
// is random so compression does not change the rate.
func slowServerEncoding(t *testing.T, size, bytesPerSec int, gzipped bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var out io.Writer = w
		flush := w.(http.Flusher).Flush
		if gzipped {
			w.Header().Set("X-Debuginfod-Size", strconv.Itoa(size))
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			defer func() { _ = zw.Close() }()
			out = zw
			flush = func() { _ = zw.Flush(); w.(http.Flusher).Flush() }
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(size))
		}
		w.WriteHeader(http.StatusOK)
		const tick = 20 * time.Millisecond
		chunk := make([]byte, max(1, bytesPerSec/int(time.Second/tick)))
		for sent := 0; sent < size; sent += len(chunk) {
			if size-sent < len(chunk) {
				chunk = chunk[:size-sent]
			}
			_, _ = rand.Read(chunk)
			if _, err := out.Write(chunk); err != nil {
				return
			}
			flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(tick):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newBudgetedFetcher is what one capture builds: a fresh process, so no
// in-memory failure memo carries over -- only what is on disk.
func newBudgetedFetcher(t *testing.T, url, dir string, budget time.Duration) *singleflightFetcher {
	t.Helper()
	c := &cache.Cache{Dir: dir}
	return newSingleflightFetcher(newFetcher([]string{url}, http.DefaultClient), c).withBudget(budget, dir)
}

func fetchWithin(t *testing.T, sf *singleflightFetcher, budget time.Duration) (string, time.Duration, error) {
	t.Helper()
	ctx := context.Background()
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	start := time.Now()
	abs, err := sf.fetchAndStore(ctx, "debuginfo", slowTestBuildID)
	return abs, time.Since(start), err
}

// A download that cannot finish is stopped once its rate is known, not when
// the budget runs out, and the next capture skips it without connecting.
// Issue #224: before this, Mesa's 233MB libgallium debuginfo cost the whole
// 5s budget on every capture and was never cached.
//
// The gzipped case is what Fedora actually sends: the first live capture
// stopped nothing, because the transparently decompressed response had no
// Content-Length.
func TestADownloadThatCannotFinishIsStoppedAndSkippedNextTime(t *testing.T) {
	for _, gzipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "identity", true: "gzip"}[gzipped], func(t *testing.T) {
			t.Parallel()
			testStoppedAndSkipped(t, gzipped)
		})
	}
}

func testStoppedAndSkipped(t *testing.T, gzipped bool) {
	const budget = 4 * time.Second
	// 4MB at ~100KB/s needs ~40s: ten times the budget.
	srv, hits := slowServerEncoding(t, 4<<20, 100<<10, gzipped)
	dir := t.TempDir()

	_, took, err := fetchWithin(t, newBudgetedFetcher(t, srv.URL, dir, budget), budget)
	ts, ok := asTooSlow(err)
	if !ok {
		t.Fatalf("first capture: err = %v, want a too-slow stop", err)
	}
	if ts.skipped {
		t.Fatal("first capture reported a skip; there was no marker to skip on")
	}
	if !strings.Contains(err.Error(), "the 4s fetch budget") {
		t.Errorf("first capture: %q does not name the budget it was judged against", err)
	}
	if took >= budget {
		t.Errorf("first capture took %v: the download ran into the %v budget instead of stopping once its rate was known", took, budget)
	}
	if _, ok := readSlowMarker(dir, "debuginfo", slowTestBuildID); !ok {
		t.Fatal("no marker written for a download that needed ~10x the budget")
	}

	_, took, err = fetchWithin(t, newBudgetedFetcher(t, srv.URL, dir, budget), budget)
	if ts, ok := asTooSlow(err); !ok || !ts.skipped {
		t.Fatalf("second capture: err = %v, want a skip on the marker", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d requests, want 1: the second capture connected anyway", got)
	}
	if took > 100*time.Millisecond {
		t.Errorf("second capture took %v to skip", took)
	}
}

// The marker is a measurement, not a verdict: a budget large enough for the
// recorded size and rate tries again, and prefetch (no budget) ignores it.
func TestTheMarkerIsJudgedAgainstTheBudgetInForce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{
		{"a larger budget", 30 * time.Second},
		{"prefetch, no budget", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := slowServer(t, 64<<10, 1<<20) // arrives in well under a second
			dir := t.TempDir()
			// Recorded: 64KB at 4KB/s needs 16s, more than a 5s budget.
			writeSlowMarker(dir, "debuginfo", slowTestBuildID,
				slowMarker{Size: 64 << 10, BytesPerSec: 4 << 10, At: time.Now()})

			abs, _, err := fetchWithin(t, newBudgetedFetcher(t, srv.URL, dir, tc.budget), tc.budget)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if hits.Load() != 1 {
				t.Fatal("the marker was obeyed; it should have been re-judged and retried")
			}
			if fi, err := os.Stat(abs); err != nil || fi.Size() != 64<<10 {
				t.Fatalf("cached file: %v, %v", fi, err)
			}
			if _, ok := readSlowMarker(dir, "debuginfo", slowTestBuildID); ok {
				t.Error("marker kept after the file arrived")
			}
		})
	}
}

// A download that fits is left alone, and a stale marker does not apply.
func TestADownloadThatFitsIsNotStopped(t *testing.T) {
	const budget = 5 * time.Second
	// 2MB at ~1MB/s: two seconds, past the probe and inside the budget.
	srv, _ := slowServer(t, 2<<20, 1<<20)
	dir := t.TempDir()
	writeSlowMarker(dir, "debuginfo", slowTestBuildID,
		slowMarker{Size: 2 << 20, BytesPerSec: 1, At: time.Now().Add(-2 * slowMarkerTTL)})

	abs, _, err := fetchWithin(t, newBudgetedFetcher(t, srv.URL, dir, budget), budget)
	if err != nil {
		t.Fatalf("a download projected to finish inside the budget failed: %v", err)
	}
	if fi, err := os.Stat(abs); err != nil || fi.Size() != 2<<20 {
		t.Fatalf("cached file: %v, %v", fi, err)
	}
}

// The message names the limit that actually stopped the download: the whole
// budget when the file cannot fit one, or what was left of it when the call
// had spent the rest on other files. Both were seen on a live capture.
func TestTooSlowSaysWhichLimitItHit(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  errTooSlow
		want string
	}{
		{"too large for any budget",
			errTooSlow{marker: slowMarker{Size: 233e6, BytesPerSec: 1.7e6}, budget: 5 * time.Second, left: 3 * time.Second},
			"stopped: 233MB at 1.7MB/s needs ~2m17s, more than the 5s fetch budget"},
		{"too large for what was left",
			errTooSlow{marker: slowMarker{Size: 4e6, BytesPerSec: 1.4e6}, budget: 5 * time.Second, left: 1500 * time.Millisecond},
			"stopped: 4MB at 1.4MB/s needs ~3s, more than the 1.5s left of the 5s fetch budget"},
		{"skipped",
			errTooSlow{marker: slowMarker{Size: 13e6, BytesPerSec: 0.5e6}, budget: 5 * time.Second, skipped: true},
			"skipped: 13MB at 0.5MB/s needs ~26s, more than the 5s fetch budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
