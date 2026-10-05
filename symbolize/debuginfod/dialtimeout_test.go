package debuginfod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The dial bound must not become a REQUEST bound: a server that is reachable
// but slow has to keep the whole FetchTimeout to answer in.
//
// This is the regression that would make the fix a net loss. Bounding the dial
// at 300ms is only defensible because it bounds reachability and nothing else;
// if it also capped the transfer, every debuginfo file that takes longer than
// 300ms to arrive -- which is most of them, they are tens of megabytes -- would
// start failing, and the profile would lose source detail it used to have.
//
// Local and deterministic, unlike the unreachable case: see the note on
// TestDialTimeoutIsShorterThanTheFetchBudget.
func TestASlowButReachableServerKeepsTheFullFetchBudget(t *testing.T) {
	const serverDelay = 4 * DialTimeout // comfortably past the dial bound

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("debuginfo payload"))
	}))
	defer srv.Close()

	opts := Options{URLs: []string{srv.URL}, CacheDir: t.TempDir()}
	if err := opts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	f := newFetcher(opts.URLs, opts.HTTPClient)

	ctx, cancel := context.WithTimeout(context.Background(), opts.FetchTimeout)
	defer cancel()
	start := time.Now()
	body, err := f.fetch(ctx, "debuginfo", "aabbccddeeff0011223344556677889900aabbcc")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a reachable server taking %s was refused after %s: %v -- the dial bound is "+
			"being applied to the whole request, not just the connection", serverDelay, elapsed, err)
	}
	_ = body.Close()
	if elapsed < serverDelay {
		t.Fatalf("fetch returned in %s, faster than the server's own %s delay", elapsed, serverDelay)
	}
}

// A caller who supplies their own client keeps it, dial bound and all. The
// default is a default, not a policy imposed on embedders -- and a caller with
// a proxy, a custom CA pool or a tuned pool has reasons this package cannot
// see.
func TestAUserSuppliedClientIsNotOverridden(t *testing.T) {
	mine := &http.Client{Timeout: 42 * time.Second}
	opts := Options{URLs: []string{"http://example.invalid"}, CacheDir: t.TempDir(), HTTPClient: mine}
	if err := opts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if opts.HTTPClient != mine {
		t.Fatal("validate replaced a caller-supplied HTTPClient")
	}
	if opts.HTTPClient.Timeout != 42*time.Second {
		t.Fatalf("caller's Timeout became %s", opts.HTTPClient.Timeout)
	}
}

// The default client must carry a transport with a dial bound at all, and the
// bound must be well under the fetch budget or it is not a separate limit.
//
// The behaviour this exists for -- an unreachable server failing at ~300ms
// instead of stalling for the full 5s -- is deliberately NOT tested here.
// Arranging it needs an address whose SYN is silently dropped, which is a
// property of the network the test runs on rather than of this code: on a
// runner that routes or refuses such an address the test would pass while
// measuring nothing, which is worse than no test. Measured by hand instead,
// against http://192.0.2.1 (TEST-NET-1):
//
//	before   5.004s   (context deadline exceeded, the full FetchTimeout)
//	after      301ms  (dial timeout)
func TestDialTimeoutIsShorterThanTheFetchBudget(t *testing.T) {
	opts := Options{URLs: []string{"http://example.invalid"}, CacheDir: t.TempDir()}
	if err := opts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if opts.HTTPClient.Transport == nil {
		t.Fatal("the default client has no Transport, so it uses http.DefaultTransport and has no dial bound")
	}
	tr, ok := opts.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default Transport is %T, not *http.Transport", opts.HTTPClient.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("the default Transport has no DialContext, so the dial is unbounded")
	}
	if DialTimeout >= opts.FetchTimeout {
		t.Errorf("DialTimeout (%s) is not shorter than FetchTimeout (%s), so it is not a separate bound",
			DialTimeout, opts.FetchTimeout)
	}
	if DialTimeout <= 0 {
		t.Errorf("DialTimeout is %s; a non-positive dial bound fails every connection", DialTimeout)
	}
}
