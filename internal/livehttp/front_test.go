package livehttp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// counter is a next handler that counts its calls, reads the whole body and
// answers 200 with its name.
type counter struct {
	name  string
	calls atomic.Int64
}

func (c *counter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.calls.Add(1)
	_, _ = io.Copy(io.Discard, r.Body)
	_, _ = io.WriteString(w, c.name)
}

// answer runs h on req in process and returns what it wrote.
func answer(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// frontRig is Front over three counters, called in process.
type frontRig struct {
	mcp, inbox, pages *counter
	front             http.Handler
}

func newFrontRig(page Token) *frontRig {
	f := &frontRig{mcp: &counter{name: "mcp"}, inbox: &counter{name: "inbox"}, pages: &counter{name: "pages"}}
	f.front = Front(page, f.mcp, f.inbox, f.pages)
	return f
}

func (f *frontRig) total() int64 {
	return f.mcp.calls.Load() + f.inbox.calls.Load() + f.pages.calls.Load()
}

func TestFrontRoutesByPathAndChecksHost(t *testing.T) {
	const page = "pagetok"
	cases := []struct {
		method, path string
		want         string // the handler that answers, or "" for Front's 404
	}{
		{"POST", "/mcp", "mcp"},
		{"DELETE", "/mcp", "mcp"},
		{"GET", "/mcp", "mcp"}, // MCPGuard answers the method
		{"POST", "/s/pagetok/inbox", "inbox"},
		{"GET", "/s/pagetok/inbox", "pages"},
		{"POST", "/s/pagetok/inbox/", "pages"},
		{"POST", "/s/pagetok/", "pages"},
		{"GET", "/s/pagetok/", "pages"},
		{"GET", "/s/pagetok", "pages"},
		{"GET", "/s/pagetok/state", "pages"},
		{"POST", "/s/wrong/inbox", ""},
		{"GET", "/s/wrong/", ""},
		{"GET", "/s/pagetokx/", ""},
		{"GET", "/s/pageto/", ""},
		{"GET", "/s//", ""},
		{"GET", "/s/", ""},
		{"GET", "/other", ""},
		{"GET", "/", ""},
		{"POST", "/mcp/", ""},
		{"POST", "/MCP", ""},
		{"POST", "/mcp/x", ""},
		{"GET", "/favicon.ico", ""},
	}
	for _, tc := range cases {
		f := newFrontRig(page)
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:7341"+tc.path, nil)
		w := answer(f.front, req)
		name := tc.method + " " + tc.path
		if tc.want == "" {
			if w.Code != http.StatusNotFound {
				t.Errorf("%s: status %d, want 404", name, w.Code)
			}
			if n := f.total(); n != 0 {
				t.Errorf("%s: %d handler calls, want none", name, n)
			}
			continue
		}
		if w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("%s: status %d body %q, want 200 from %s", name, w.Code, w.Body.String(), tc.want)
		}
		if n := f.total(); n != 1 {
			t.Errorf("%s: %d handler calls, want 1", name, n)
		}
	}

	for _, host := range []string{"evil.com:80", "127.0.0.1", "evil.com", "127.0.0.1.nip.io:7341"} {
		for _, path := range []string{"/mcp", "/s/pagetok/", "/s/pagetok/inbox"} {
			f := newFrontRig(page)
			req := httptest.NewRequest("POST", "http://127.0.0.1:7341"+path, nil)
			req.Host = host
			if w := answer(f.front, req); w.Code != http.StatusForbidden {
				t.Errorf("Host %q, POST %s: status %d, want 403", host, path, w.Code)
			}
			if n := f.total(); n != 0 {
				t.Errorf("Host %q, POST %s: %d handler calls, want none", host, path, n)
			}
		}
	}
	for _, host := range []string{"[::1]:7341", "localhost:7341", "127.0.0.1:7341"} {
		f := newFrontRig(page)
		req := httptest.NewRequest("POST", "http://127.0.0.1:7341/mcp", nil)
		req.Host = host
		if w := answer(f.front, req); w.Code != http.StatusOK || f.mcp.calls.Load() != 1 {
			t.Errorf("Host %q: status %d, mcp calls %d; want 200 from mcp", host, w.Code, f.mcp.calls.Load())
		}
	}

	// Without a page token nothing under /s/ is served; /mcp still is.
	f := newFrontRig("")
	for _, path := range []string{"/s//", "/s//inbox", "/s/", "/s"} {
		for _, method := range []string{"GET", "POST"} {
			req := httptest.NewRequest(method, "http://127.0.0.1:7341"+path, nil)
			if w := answer(f.front, req); w.Code != http.StatusNotFound {
				t.Errorf("empty page token, %s %s: status %d, want 404", method, path, w.Code)
			}
		}
	}
	if n := f.total(); n != 0 {
		t.Errorf("empty page token: %d handler calls under /s/, want none", n)
	}
	if w := answer(f.front, httptest.NewRequest("POST", "http://127.0.0.1:7341/mcp", nil)); w.Code != http.StatusOK {
		t.Errorf("empty page token, POST /mcp: status %d, want 200", w.Code)
	}
}

// Every 404 and the page shell clear the origin's storage, service workers
// included: a worker a port squatter registered while the hub was down is
// cleared by the browser's update check, which the real hub answers 404,
// or by the next load of the shell. The page keeps nothing in storage.
func TestFrontClearsSiteStorageOn404AndTheShell(t *testing.T) {
	const want = `"storage"`
	cases := []struct {
		method, path string
		clears       bool
	}{
		{"GET", "/sw.js", true},
		{"GET", "/", true},
		{"GET", "/s/wrong/", true},
		{"GET", "/s/wrong/sw.js", true},
		{"POST", "/s/wrong/inbox", true},
		{"GET", "/s/pagetok/", true},
		{"GET", "/s/pagetok", true},
		{"GET", "/s/pagetok/state", false},
		{"GET", "/s/pagetok/assets/live.js", false},
		{"POST", "/s/pagetok/inbox", false},
		{"POST", "/mcp", false},
	}
	for _, tc := range cases {
		f := newFrontRig("pagetok")
		w := answer(f.front, httptest.NewRequest(tc.method, "http://127.0.0.1:7341"+tc.path, nil))
		got := w.Header().Get("Clear-Site-Data")
		if tc.clears && got != want {
			t.Errorf("%s %s (%d): Clear-Site-Data %q, want %q", tc.method, tc.path, w.Code, got, want)
		}
		if !tc.clears && got != "" {
			t.Errorf("%s %s (%d): Clear-Site-Data %q, want none", tc.method, tc.path, w.Code, got)
		}
	}
}

// The hub serves no service worker script, so a worker's script fetch,
// its registration or its update check, gets 404 and clears the site's
// storage on any path: a squatter's worker registered from /mcp or a page
// path is dropped by its update check too. No handler sees it.
func TestFrontRefusesAServiceWorkerScriptFetch(t *testing.T) {
	headers := []map[string]string{
		{"Service-Worker": "script"},
		{"Sec-Fetch-Dest": "serviceworker"},
		{"Service-Worker": "script", "Sec-Fetch-Dest": "serviceworker", "Sec-Fetch-Mode": "same-origin"},
	}
	for _, h := range headers {
		for _, path := range []string{"/mcp", "/s/pagetok/", "/s/pagetok/assets/live.js", "/s/pagetok/inbox", "/sw.js"} {
			for _, method := range []string{"GET", "POST"} {
				f := newFrontRig("pagetok")
				req := httptest.NewRequest(method, "http://127.0.0.1:7341"+path, nil)
				for k, v := range h {
					req.Header.Set(k, v)
				}
				w := answer(f.front, req)
				if w.Code != http.StatusNotFound || w.Header().Get("Clear-Site-Data") != `"storage"` {
					t.Errorf("%v %s %s: status %d, Clear-Site-Data %q; want 404 clearing storage", h, method, path, w.Code, w.Header().Get("Clear-Site-Data"))
				}
				if n := f.total(); n != 0 {
					t.Errorf("%v %s %s: %d handler calls, want none", h, method, path, n)
				}
			}
		}
	}
}

// theSevenHeaders are written out here rather than read from securityHeaders,
// so that dropping one there fails this test.
var theSevenHeaders = [...][2]string{
	{"Content-Security-Policy", CSP},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"Cache-Control", "no-store"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
	{"X-Frame-Options", "DENY"},
}

func TestFrontSetsSecurityHeadersOnEveryAnswer(t *testing.T) {
	cases := []struct {
		name, method, path, host string
		code                     int
		refusal                  bool
	}{
		{"bad Host", "POST", "/mcp", "evil.example:80", http.StatusForbidden, true},
		{"unknown path", "GET", "/other", "127.0.0.1:7341", http.StatusNotFound, true},
		{"wrong page token", "POST", "/s/wrong/inbox", "127.0.0.1:7341", http.StatusNotFound, true},
		{"mcp", "POST", "/mcp", "127.0.0.1:7341", http.StatusOK, false},
		{"inbox", "POST", "/s/pagetok/inbox", "127.0.0.1:7341", http.StatusOK, false},
		{"pages", "GET", "/s/pagetok/", "127.0.0.1:7341", http.StatusOK, false},
	}
	for _, tc := range cases {
		f := newFrontRig("pagetok")
		req := httptest.NewRequest(tc.method, "http://127.0.0.1:7341"+tc.path, nil)
		req.Host = tc.host
		w := answer(f.front, req)
		if w.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.code)
		}
		for _, kv := range theSevenHeaders {
			if got := w.Header().Get(kv[0]); got != kv[1] {
				t.Errorf("%s: %s %q, want %q", tc.name, kv[0], got, kv[1])
			}
		}
		if tc.refusal && w.Header().Get("Connection") != "close" {
			t.Errorf("%s: a refusal without Connection: close", tc.name)
		}
	}
}

func TestLoopbackHostAcceptsOnlyThisMachine(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost:7341", true},
		{"LOCALHOST:1", true},
		{"127.0.0.1:7341", true},
		{"127.1.2.3:80", true},
		{"[::1]:7341", true},
		{"127.0.0.1", false},
		{"localhost", false},
		{"localhost:99999", false},
		{"localhost:", false},
		{"localhost:x", false},
		{"[::1%eth0]:1", false},
		{"0.0.0.0:7341", false},
		{"10.0.0.1:7341", false},
		{"2130706433:7341", false},
		{"0x7f000001:7341", false},
		{"127.1:7341", false},
		{"evil.localhost:7341", false},
		{"localhost.:7341", false},
		{"[::ffff:127.0.0.1]:7341", false},
		{"[::]:7341", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := LoopbackHost(tc.host); got != tc.want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
		f := newFrontRig("pagetok")
		req := httptest.NewRequest("POST", "http://127.0.0.1:7341/mcp", nil)
		req.Host = tc.host
		w := answer(f.front, req)
		switch {
		case tc.want && (w.Code != http.StatusOK || f.mcp.calls.Load() != 1):
			t.Errorf("Front, Host %q: status %d, mcp calls %d; want 200 from mcp", tc.host, w.Code, f.mcp.calls.Load())
		case !tc.want && (w.Code != http.StatusForbidden || f.total() != 0):
			t.Errorf("Front, Host %q: status %d, handler calls %d; want 403 and none", tc.host, w.Code, f.total())
		}
	}
}

// hubRig is a real loopback server running Front over MCPGuard and
// InboxGuard, with the inbox's clock stopped so its bucket never refills.
type hubRig struct {
	page, bearer      Token
	addr              string
	mcp, inbox, pages *counter
	maxBody           int64
}

func startHubRig(t *testing.T, mcpNext http.Handler) *hubRig {
	t.Helper()
	page, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	bearer, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	h := &hubRig{
		page: page, bearer: bearer, addr: ln.Addr().String(), maxBody: 4096,
		mcp: &counter{name: "mcp"}, inbox: &counter{name: "inbox"}, pages: &counter{name: "pages"},
	}
	if mcpNext == nil {
		mcpNext = h.mcp
	}
	stopped := time.Now()
	front := Front(page,
		MCPGuard(bearer, h.maxBody, mcpNext),
		newInboxGuard(page, Rate{PerSecond: 5, Burst: 5}, func() time.Time { return stopped }, h.inbox),
		Guard(page, h.pages))
	srv := NewServer(front, slog.New(slog.DiscardHandler))
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	return h
}

// dial opens a TCP connection to the rig that the test closes at cleanup.
func (h *hubRig) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// header is one request header line, kept in order and spelling.
type header [2]string

// rawRequest writes a request head; body follows only if the caller sends it.
func rawRequest(method, path string, headers []header) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, path)
	for _, h := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	b.WriteString("\r\n")
	return b.String()
}

// with returns headers with key set to value, replacing every line of that
// name, or added at the end.
func with(headers []header, key, value string) []header {
	out := without(headers, key)
	return append(out, header{key, value})
}

func without(headers []header, key string) []header {
	var out []header
	for _, h := range headers {
		if !strings.EqualFold(h[0], key) {
			out = append(out, h)
		}
	}
	return out
}

func (h *hubRig) mcpHeaders(length int) []header {
	return []header{
		{"Host", h.addr},
		{"Authorization", "Bearer " + string(h.bearer)},
		{"Accept", "application/json, text/event-stream"},
		{"Content-Type", "application/json"},
		{"Content-Length", strconv.Itoa(length)},
	}
}

func (h *hubRig) inboxHeaders(length int) []header {
	return []header{
		{"Host", h.addr},
		{"Origin", "http://" + h.addr},
		{"Sec-Fetch-Site", "same-origin"},
		{"Sec-Fetch-Mode", "cors"},
		{"Sec-Fetch-Dest", "empty"},
		{"Content-Type", "application/json"},
		{"X-Perf-Agent-Page", "1"},
		{"Content-Length", strconv.Itoa(length)},
	}
}

func (h *hubRig) inboxPath() string { return "/s/" + string(h.page) + "/inbox" }

// readAnswer reads one response from conn within limit.
func readAnswer(t *testing.T, conn net.Conn, limit time.Duration) (*http.Response, error) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(limit)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// The server must hang up: the next read ends, not times out.
	if _, err := br.ReadByte(); err == nil {
		return resp, errors.New("more bytes after the response")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return resp, errors.New("the connection stayed open")
	}
	return resp, nil
}

func TestEveryRefusalIsAnsweredBeforeTheBody(t *testing.T) {
	h := startHubRig(t, nil)
	const n = 64 // announced, never sent
	mcp := h.mcpHeaders(n)
	inbox := h.inboxHeaders(n)
	big := strconv.FormatInt(h.maxBody+1, 10)
	chunked := func(hs []header) []header {
		return with(without(hs, "Content-Length"), "Transfer-Encoding", "chunked")
	}
	cases := []struct {
		name, method, path string
		headers            []header
		code               int
	}{
		{"front: bad Host", "POST", "/mcp", with(mcp, "Host", "evil.example:80"), http.StatusForbidden},
		{"front: unknown path", "POST", "/other", mcp, http.StatusNotFound},
		{"front: wrong page token", "POST", "/s/wrong/inbox", inbox, http.StatusNotFound},
		{"mcp: Origin", "POST", "/mcp", with(mcp, "Origin", "http://"+h.addr), http.StatusForbidden},
		{"mcp: Sec-Fetch-Site", "POST", "/mcp", with(mcp, "Sec-Fetch-Site", "same-origin"), http.StatusForbidden},
		{"mcp: Sec-Fetch-Dest", "POST", "/mcp", with(mcp, "Sec-Fetch-Dest", "empty"), http.StatusForbidden},
		{"mcp: PUT", "PUT", "/mcp", mcp, http.StatusMethodNotAllowed},
		{"mcp: no bearer", "POST", "/mcp", without(mcp, "Authorization"), http.StatusUnauthorized},
		{"mcp: wrong bearer", "POST", "/mcp", with(mcp, "Authorization", "Bearer nope"), http.StatusUnauthorized},
		{"mcp: Accept", "POST", "/mcp", with(mcp, "Accept", "application/json"), http.StatusBadRequest},
		{"mcp: Content-Type", "POST", "/mcp", with(mcp, "Content-Type", "text/plain"), http.StatusBadRequest},
		{"mcp: chunked", "POST", "/mcp", chunked(mcp), http.StatusLengthRequired},
		{"mcp: too large", "POST", "/mcp", with(mcp, "Content-Length", big), http.StatusRequestEntityTooLarge},
		{"mcp: DELETE with a body", "DELETE", "/mcp", mcp, http.StatusBadRequest},
		{"inbox: no Origin", "POST", h.inboxPath(), without(inbox, "Origin"), http.StatusForbidden},
		{"inbox: cross-site", "POST", h.inboxPath(), with(inbox, "Sec-Fetch-Site", "cross-site"), http.StatusForbidden},
		{"inbox: text/plain", "POST", h.inboxPath(), with(inbox, "Content-Type", "text/plain"), http.StatusBadRequest},
		{"inbox: no X-Perf-Agent-Page", "POST", h.inboxPath(), without(inbox, "X-Perf-Agent-Page"), http.StatusForbidden},
		{"inbox: chunked", "POST", h.inboxPath(), chunked(inbox), http.StatusLengthRequired},
		{"inbox: 2049 bytes", "POST", h.inboxPath(), with(inbox, "Content-Length", "2049"), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		conn := h.dial(t)
		start := time.Now()
		if _, err := io.WriteString(conn, rawRequest(tc.method, tc.path, tc.headers)); err != nil {
			t.Fatalf("%s: write: %v", tc.name, err)
		}
		resp, err := readAnswer(t, conn, time.Second)
		if resp == nil {
			t.Errorf("%s: no answer within 1 s: %v", tc.name, err)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if resp.StatusCode != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.code)
		}
		if !resp.Close {
			t.Errorf("%s: answer without Connection: close", tc.name)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: answered after %v", tc.name, d)
		}
	}
	if n := h.mcp.calls.Load() + h.inbox.calls.Load() + h.pages.calls.Load(); n != 0 {
		t.Errorf("next called %d times for refused requests", n)
	}

	// The bucket: five whole reports pass, the sixth is refused from its
	// headers alone.
	body := strings.Repeat(" ", n)
	for i := range 5 {
		conn := h.dial(t)
		if _, err := io.WriteString(conn, rawRequest("POST", h.inboxPath(), inbox)+body); err != nil {
			t.Fatalf("report %d: write: %v", i+1, err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("report %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("report %d: status %d, want 200", i+1, resp.StatusCode)
		}
	}
	conn := h.dial(t)
	if _, err := io.WriteString(conn, rawRequest("POST", h.inboxPath(), inbox)); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := readAnswer(t, conn, time.Second)
	switch {
	case resp == nil:
		t.Errorf("inbox: rate: no answer within 1 s: %v", err)
	case err != nil:
		t.Errorf("inbox: rate: %v", err)
	case resp.StatusCode != http.StatusTooManyRequests || !resp.Close:
		t.Errorf("inbox: rate: status %d close %v, want 429 and close", resp.StatusCode, resp.Close)
	}
	if n := h.inbox.calls.Load(); n != 5 {
		t.Errorf("inbox next called %d times, want 5", n)
	}
}
