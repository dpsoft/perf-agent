package livehttp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// guarded is a real loopback server running Guard over an echo handler that
// writes the path it was given.
type guarded struct {
	tok    Token
	addr   string
	client *http.Client
	calls  atomic.Int64
	mu     sync.Mutex
	paths  []string
}

func startGuarded(t *testing.T) *guarded {
	t.Helper()
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := &guarded{
		tok:  tok,
		addr: ln.Addr().String(),
		client: &http.Client{
			Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls.Add(1)
		g.mu.Lock()
		g.paths = append(g.paths, r.URL.Path)
		g.mu.Unlock()
		_, _ = io.WriteString(w, r.URL.Path)
	})
	srv := NewServer(Guard(tok, echo), slog.New(slog.DiscardHandler))
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	return g
}

func (g *guarded) request(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+g.addr+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, path, err)
	}
	return req
}

func (g *guarded) do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp, string(body)
}

func (g *guarded) status(t *testing.T, req *http.Request) int {
	t.Helper()
	resp, _ := g.do(t, req)
	return resp.StatusCode
}

func (g *guarded) port() string {
	return g.addr[strings.LastIndexByte(g.addr, ':')+1:]
}

func (g *guarded) lastPath() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.paths) == 0 {
		return ""
	}
	return g.paths[len(g.paths)-1]
}

func TestGuardRefusesAWrongOrMissingToken(t *testing.T) {
	g := startGuarded(t)
	other, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	for _, path := range []string{
		"/",
		"/s/",
		"/s/" + string(other) + "/",
		"/s/" + string(g.tok) + "x/",
		"/s/" + string(g.tok)[:42] + "/",
		"/x/s/" + string(g.tok) + "/",
	} {
		resp, body := g.do(t, g.request(t, http.MethodGet, path))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, resp.StatusCode)
		}
		if path != "/" && strings.Contains(body, path) {
			t.Errorf("GET %s: body %q echoes the path", path, body)
		}
		if strings.Contains(body, string(other)) || strings.Contains(body, string(g.tok)) {
			t.Errorf("GET %s: body %q carries a token", path, body)
		}
	}
	if n := g.calls.Load(); n != 0 {
		t.Errorf("next called %d times without the token", n)
	}
}

func TestGuardRedirectsThePrefixToItsSlash(t *testing.T) {
	g := startGuarded(t)
	prefix := "/s/" + string(g.tok)

	resp, _ := g.do(t, g.request(t, http.MethodGet, prefix))
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("GET %s: status %d, want 301", prefix, resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != prefix+"/" {
		t.Errorf("GET %s: Location %q, want %q", prefix, loc, prefix+"/")
	}
	if n := g.calls.Load(); n != 0 {
		t.Errorf("next called %d times for the redirect", n)
	}

	for path, want := range map[string]string{
		prefix + "/":                  "/",
		prefix + "/state":             "/state",
		prefix + "/pub/p1/export":     "/pub/p1/export",
		prefix + "/assets/live.js":    "/assets/live.js",
		prefix + "/frame/a%2Fb?x=%2F": "/frame/a/b",
	} {
		resp, body := g.do(t, g.request(t, http.MethodGet, path))
		if resp.StatusCode != http.StatusOK || body != want || g.lastPath() != want {
			t.Errorf("GET %s: status %d, next saw %q, want 200 and %q", path, resp.StatusCode, body, want)
		}
	}
}

func TestGuardChecksHostAgainstRebinding(t *testing.T) {
	g := startGuarded(t)
	shell := "/s/" + string(g.tok) + "/"
	port := g.port()
	for host, want := range map[string]int{
		"evil.example:" + port:           http.StatusForbidden,
		"127.0.0.1.evil.example:" + port: http.StatusForbidden,
		"[fe80::1]:" + port:              http.StatusForbidden,
		"[::ffff:127.0.0.1]:" + port:     http.StatusForbidden,
		"0.0.0.0:" + port:                http.StatusForbidden,
		"127.0.0.1":                      http.StatusForbidden,
		"localhost":                      http.StatusForbidden,
		"localhost.:" + port:             http.StatusForbidden,
		"127.0.0.1:" + port:              http.StatusOK,
		"localhost:9000":                 http.StatusOK,
		"[::1]:5":                        http.StatusOK,
		"127.0.0.5:1":                    http.StatusOK,
	} {
		req := g.request(t, http.MethodGet, shell)
		req.Host = host
		resp, body := g.do(t, req)
		if resp.StatusCode != want {
			t.Errorf("Host %q: status %d, want %d", host, resp.StatusCode, want)
		}
		if want == http.StatusForbidden && !strings.Contains(body, "bad Host") {
			t.Errorf("Host %q: body %q does not say bad Host", host, body)
		}
	}
}

func TestGuardChecksOriginAndFetchMetadata(t *testing.T) {
	g := startGuarded(t)
	state := "/s/" + string(g.tok) + "/state"
	for _, origin := range []string{"http://evil.example", "null", "http://localhost:9001", "https://localhost:9000", ""} {
		req := g.request(t, http.MethodGet, state)
		req.Host = "localhost:9000"
		req.Header.Set("Origin", origin)
		if got := g.status(t, req); got != http.StatusForbidden {
			t.Errorf("Origin %q with Host localhost:9000: status %d, want 403", origin, got)
		}
	}
	for site, want := range map[string]int{
		"cross-site":  http.StatusForbidden,
		"same-site":   http.StatusForbidden,
		"bogus":       http.StatusForbidden,
		"same-origin": http.StatusOK,
		"none":        http.StatusOK,
	} {
		req := g.request(t, http.MethodGet, state)
		req.Header.Set("Sec-Fetch-Site", site)
		if got := g.status(t, req); got != want {
			t.Errorf("Sec-Fetch-Site %q: status %d, want %d", site, got, want)
		}
	}
	if got := g.status(t, g.request(t, http.MethodGet, state)); got != http.StatusOK {
		t.Errorf("no fetch metadata: status %d, want 200", got)
	}
}

func TestGuardAcceptsLocalhostName(t *testing.T) {
	g := startGuarded(t)
	for _, path := range []string{"/s/" + string(g.tok) + "/", "/s/" + string(g.tok) + "/state"} {
		req := g.request(t, http.MethodGet, path)
		req.Host = "localhost:9000"
		req.Header.Set("Origin", "http://localhost:9000")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if got := g.status(t, req); got != http.StatusOK {
			t.Errorf("GET %s as localhost:9000: status %d, want 200", path, got)
		}
	}
}

func TestNavigationHeaderMatrix(t *testing.T) {
	g := startGuarded(t)
	prefix := "/s/" + string(g.tok)
	routes := []string{prefix + "/", prefix, prefix + "/state", prefix + "/events", prefix + "/assets/live.js", prefix + "/pub/p1/export"}
	sites := []string{"", "none", "same-origin", "same-site", "cross-site"}
	type modeDest struct{ mode, dest string }
	pairs := []modeDest{{"navigate", "document"}, {"navigate", "iframe"}, {"cors", "empty"}, {"no-cors", "script"}, {"", ""}}

	for _, route := range routes {
		shell := route == prefix || route == prefix+"/"
		for _, site := range sites {
			for _, p := range pairs {
				req := g.request(t, http.MethodGet, route)
				setIf(req, "Sec-Fetch-Site", site)
				setIf(req, "Sec-Fetch-Mode", p.mode)
				setIf(req, "Sec-Fetch-Dest", p.dest)

				pass := site == "" || site == "none" || site == "same-origin" ||
					(shell && p.mode == "navigate" && p.dest == "document")
				want := http.StatusForbidden
				switch {
				case pass && route == prefix:
					want = http.StatusMovedPermanently
				case pass:
					want = http.StatusOK
				}
				if got := g.status(t, req); got != want {
					t.Errorf("%s site=%q mode=%q dest=%q: status %d, want %d", route, site, p.mode, p.dest, got, want)
				}
			}
		}
	}

	other, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	crossNav := func(path, host string) int {
		req := g.request(t, http.MethodGet, path)
		if host != "" {
			req.Host = host
		}
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
		return g.status(t, req)
	}
	for _, path := range []string{"/s/" + string(other) + "/", "/s/" + string(other)} {
		if got := crossNav(path, ""); got != http.StatusNotFound {
			t.Errorf("cross-site navigation to %s with a wrong token: status %d, want 404", path, got)
		}
	}
	if got := crossNav(prefix+"/", "evil.example:"+g.port()); got != http.StatusForbidden {
		t.Errorf("cross-site navigation to the shell with Host evil.example: status %d, want 403", got)
	}
	req := g.request(t, http.MethodGet, prefix+"/")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Method = http.MethodPost
	if got := g.status(t, req); got != http.StatusMethodNotAllowed {
		t.Errorf("cross-site POST navigation to the shell: status %d, want 405", got)
	}
}

func setIf(req *http.Request, key, value string) {
	if value != "" {
		req.Header.Set(key, value)
	}
}

func TestGuardServesOnlyReads(t *testing.T) {
	g := startGuarded(t)
	shell := "/s/" + string(g.tok) + "/"
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions, "PROPFIND"} {
		resp, _ := g.do(t, g.request(t, method, shell))
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s: Allow %q, want %q", method, allow, "GET, HEAD")
		}
	}
	if n := g.calls.Load(); n != 0 {
		t.Errorf("next called %d times for writes", n)
	}
	if got := g.status(t, g.request(t, http.MethodHead, shell)); got != http.StatusOK {
		t.Errorf("HEAD: status %d, want 200", got)
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	g := startGuarded(t)
	prefix := "/s/" + string(g.tok)
	cases := map[string]*http.Request{
		"200": g.request(t, http.MethodGet, prefix+"/"),
		"301": g.request(t, http.MethodGet, prefix),
		"403": g.request(t, http.MethodGet, prefix+"/"),
		"404": g.request(t, http.MethodGet, "/nope"),
		"405": g.request(t, http.MethodPost, prefix+"/"),
	}
	cases["403"].Header.Set("Sec-Fetch-Site", "cross-site")
	want := map[string]string{
		"Content-Security-Policy":      CSP,
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cache-Control":                "no-store",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"X-Frame-Options":              "DENY",
	}
	for code, req := range cases {
		resp, _ := g.do(t, req)
		if got := resp.Status[:3]; got != code {
			t.Errorf("case %s: status %s", code, resp.Status)
		}
		for k, v := range want {
			if got := resp.Header.Values(k); len(got) != 1 || got[0] != v {
				t.Errorf("%s: %s = %q, want %q", code, k, got, v)
			}
		}
		for k := range resp.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(k), "Access-Control-") {
				t.Errorf("%s: sends %s", code, k)
			}
		}
	}
}

func TestHandlersMayReplaceTheCSP(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	h := Guard(tok, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", ExportCSP)
	}))
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/s/"+string(tok)+"/pub/p1/export", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	h.ServeHTTP(w, req)
	if got := w.Result().Header.Values("Content-Security-Policy"); len(got) != 1 || got[0] != ExportCSP {
		t.Errorf("Content-Security-Policy = %q, want only %q", got, ExportCSP)
	}
}

func TestGuardWithAnEmptyTokenAdmitsNothing(t *testing.T) {
	h := Guard("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("next called under an empty token")
	}))
	for _, path := range []string{"/s//", "/s//state", "/s/"} {
		w := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1"+path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s under an empty token: status %d, want 404", path, w.Code)
		}
	}
}
