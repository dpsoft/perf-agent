package livehttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPage = Token("page-test-token-0123456789")

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// report is a POST to the inbox as the page sends it, with a body of n bytes.
func report(n int) *http.Request {
	req := httptest.NewRequest("POST", "http://127.0.0.1:7341/s/"+string(testPage)+"/inbox", strings.NewReader(strings.Repeat(" ", n)))
	req.Header.Set("Origin", "http://127.0.0.1:7341")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Perf-Agent-Page", "1")
	req.Header.Set("Content-Length", strconv.Itoa(n))
	return req
}

// inboxRig is InboxGuard at Rate{5, 5} over a counter, on a stopped clock.
type inboxRig struct {
	clock *fakeClock
	next  *counter
	guard http.Handler
}

func newInboxRig() *inboxRig {
	r := &inboxRig{clock: &fakeClock{t: time.Unix(1_700_000_000, 0)}, next: &counter{name: "inbox"}}
	r.guard = newInboxGuard(testPage, Rate{PerSecond: 5, Burst: 5}, r.clock.now, r.next)
	return r
}

func TestInboxGuardAdmitsTheOwnPage(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusNoContent)
	})
	req := report(0)
	const body = `{"kind":"select","node":"n1"}`
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	w := answer(InboxGuard(testPage, Rate{PerSecond: 5, Burst: 5}, next), req)
	if w.Code != http.StatusNoContent || got != body {
		t.Fatalf("status %d, next read %q; want 204 and %q", w.Code, got, body)
	}
}

func TestInboxGuardCSRFMatrix(t *testing.T) {
	cases := []struct {
		name string
		edit func(*http.Request)
		code int
	}{
		{"Origin missing", func(r *http.Request) { r.Header.Del("Origin") }, http.StatusForbidden},
		{"Origin other", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"Origin other port", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:7342") }, http.StatusForbidden},
		{"Origin https", func(r *http.Request) { r.Header.Set("Origin", "https://127.0.0.1:7341") }, http.StatusForbidden},
		{"Origin localhost for 127.0.0.1", func(r *http.Request) { r.Header.Set("Origin", "http://localhost:7341") }, http.StatusForbidden},
		{"Origin null", func(r *http.Request) { r.Header.Set("Origin", "null") }, http.StatusForbidden},
		{"Origin empty", func(r *http.Request) { r.Header["Origin"] = []string{""} }, http.StatusForbidden},
		{"Origin repeated", func(r *http.Request) {
			r.Header["Origin"] = []string{"http://127.0.0.1:7341", "http://127.0.0.1:7341"}
		}, http.StatusForbidden},
		{"Sec-Fetch-Site missing", func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }, http.StatusForbidden},
		{"Sec-Fetch-Site cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		{"Sec-Fetch-Site same-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, http.StatusForbidden},
		{"Sec-Fetch-Site none", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") }, http.StatusForbidden},
		{"Sec-Fetch-Site repeated", func(r *http.Request) {
			r.Header["Sec-Fetch-Site"] = []string{"same-origin", "same-origin"}
		}, http.StatusForbidden},
		{"text/plain", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusBadRequest},
		{"form", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusBadRequest},
		{"no Content-Type", func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusBadRequest},
		{"no X-Perf-Agent-Page", func(r *http.Request) { r.Header.Del("X-Perf-Agent-Page") }, http.StatusForbidden},
		{"X-Perf-Agent-Page 0", func(r *http.Request) { r.Header.Set("X-Perf-Agent-Page", "0") }, http.StatusForbidden},
		{"X-Perf-Agent-Page repeated", func(r *http.Request) {
			r.Header["X-Perf-Agent-Page"] = []string{"1", "1"}
		}, http.StatusForbidden},
		{"wrong token", func(r *http.Request) { r.URL.Path = "/s/wrong/inbox" }, http.StatusNotFound},
		{"token prefix", func(r *http.Request) { r.URL.Path = "/s/" + string(testPage[:5]) + "/inbox" }, http.StatusNotFound},
		{"no token", func(r *http.Request) { r.URL.Path = "/s//inbox" }, http.StatusNotFound},
		{"another route", func(r *http.Request) { r.URL.Path = "/s/" + string(testPage) + "/state" }, http.StatusNotFound},
		{"GET", func(r *http.Request) { r.Method = "GET" }, http.StatusMethodNotAllowed},
		{"no Content-Length", func(r *http.Request) { r.Header.Del("Content-Length") }, http.StatusLengthRequired},
		{"chunked", func(r *http.Request) {
			r.Header.Del("Content-Length")
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
		}, http.StatusLengthRequired},
		{"1025 bytes", func(r *http.Request) {
			r.ContentLength = 1025
			r.Header.Set("Content-Length", "1025")
		}, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		rig := newInboxRig()
		req := report(10)
		tc.edit(req)
		w := answer(rig.guard, req)
		if w.Code != tc.code || rig.next.calls.Load() != 0 {
			t.Errorf("%s: status %d, next calls %d; want %d and none", tc.name, w.Code, rig.next.calls.Load(), tc.code)
		}
		if w.Header().Get("Connection") != "close" {
			t.Errorf("%s: refusal without Connection: close", tc.name)
		}
	}

	// A refused request spends no rate: a cross-site page cannot use up
	// the page's own reports.
	rig := newInboxRig()
	for range 20 {
		req := report(10)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		answer(rig.guard, req)
	}
	if w := answer(rig.guard, report(10)); w.Code != http.StatusOK {
		t.Errorf("after 20 refused requests: status %d, want 200", w.Code)
	}

	// 1024 bytes is the limit, not over it.
	if w := answer(newInboxRig().guard, report(1024)); w.Code != http.StatusOK {
		t.Errorf("1024 bytes: status %d, want 200", w.Code)
	}

	// The bucket holds five; the sixth within the same second is refused.
	rig = newInboxRig()
	for i := range 5 {
		if w := answer(rig.guard, report(10)); w.Code != http.StatusOK {
			t.Fatalf("report %d: status %d, want 200", i+1, w.Code)
		}
	}
	rig.clock.advance(150 * time.Millisecond)
	w := answer(rig.guard, report(10))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Errorf("6th report within a second: status %d Retry-After %q, want 429 and 1", w.Code, w.Header().Get("Retry-After"))
	}
	if n := rig.next.calls.Load(); n != 5 {
		t.Errorf("next called %d times, want 5", n)
	}
	// It refills at five a second, up to five.
	rig.clock.advance(50 * time.Millisecond)
	if w := answer(rig.guard, report(10)); w.Code != http.StatusOK {
		t.Errorf("after 200 ms: status %d, want 200", w.Code)
	}
	if w := answer(rig.guard, report(10)); w.Code != http.StatusTooManyRequests {
		t.Errorf("right after: status %d, want 429", w.Code)
	}
	rig.clock.advance(time.Hour)
	for i := range 5 {
		if w := answer(rig.guard, report(10)); w.Code != http.StatusOK {
			t.Errorf("after an hour, report %d: status %d, want 200", i+1, w.Code)
		}
	}
	if w := answer(rig.guard, report(10)); w.Code != http.StatusTooManyRequests {
		t.Errorf("after an hour, report 6: status %d, want 429", w.Code)
	}
}

func TestInboxGuardWithAnEmptyTokenAdmitsNothing(t *testing.T) {
	next := &counter{name: "inbox"}
	g := InboxGuard("", Rate{PerSecond: 5, Burst: 5}, next)
	req := report(10)
	req.URL.Path = "/s//inbox"
	if w := answer(g, req); w.Code != http.StatusNotFound || next.calls.Load() != 0 {
		t.Errorf("status %d, next calls %d; want 404 and none", w.Code, next.calls.Load())
	}
}

func TestInboxGuardReadsTheBodyUnderADeadline(t *testing.T) {
	h := startHubRig(t, nil)
	conn := h.dial(t)
	start := time.Now()
	// Ten bytes announced, three sent.
	if _, err := io.WriteString(conn, rawRequest("POST", h.inboxPath(), h.inboxHeaders(10))+"{\"k"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := readAnswer(t, conn, 5*time.Second)
	if resp == nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	d := time.Since(start)
	if resp.StatusCode != http.StatusRequestTimeout || !resp.Close {
		t.Errorf("stalled body: status %d close %v, want 408 and close", resp.StatusCode, resp.Close)
	}
	if d < 1500*time.Millisecond || d > 4*time.Second {
		t.Errorf("stalled body answered after %v, want about 2 s", d)
	}
	if n := h.inbox.calls.Load(); n != 0 {
		t.Errorf("next called %d times for a body that never arrived", n)
	}
}

func TestInboxGuardRefusesANonPositiveRate(t *testing.T) {
	for _, rate := range []Rate{{0, 5}, {5, 0}, {-1, 5}, {5, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("InboxGuard with %+v did not panic", rate)
				}
			}()
			InboxGuard(testPage, rate, &counter{})
		}()
	}
}

func TestInboxRateRefillsWithoutRounding(t *testing.T) {
	rig := newInboxRig()
	for range 5 {
		answer(rig.guard, report(10))
	}
	// One report every 200 ms is exactly the rate, for as long as it lasts.
	for i := range 100 {
		rig.clock.advance(200 * time.Millisecond)
		if w := answer(rig.guard, report(10)); w.Code != http.StatusOK {
			t.Fatalf("report %d at the exact rate: status %d, want 200", i+1, w.Code)
		}
	}
	rig.clock.advance(199 * time.Millisecond)
	if w := answer(rig.guard, report(10)); w.Code != http.StatusTooManyRequests {
		t.Errorf("1 ms early: status %d, want 429", w.Code)
	}
}

func TestInboxRateNeverHoldsMoreThanTheBurst(t *testing.T) {
	rig := newInboxRig()
	answer(rig.guard, report(10)) // four left
	// 900 ms earns 4.5 tokens; the bucket holds five at most.
	rig.clock.advance(900 * time.Millisecond)
	for i := range 5 {
		if w := answer(rig.guard, report(10)); w.Code != http.StatusOK {
			t.Fatalf("report %d: status %d, want 200", i+1, w.Code)
		}
	}
	if w := answer(rig.guard, report(10)); w.Code != http.StatusTooManyRequests {
		t.Errorf("a sixth report: status %d, want 429: the bucket held more than its burst", w.Code)
	}
}
