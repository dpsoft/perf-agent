package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
)

// The workers a port squatter would register. Each takes control at once.
// proxyWorker fetches every request through itself, from the network;
// fakeWorker answers navigations with a page of its own, so they never
// reach the network.
const (
	workerPrelude = `self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));
`
	proxyWorker = workerPrelude + `self.addEventListener('fetch', (e) => e.respondWith(fetch(e.request)));
`
	fakeWorker = workerPrelude + `self.addEventListener('fetch', (e) => {
  if (e.request.mode === 'navigate') e.respondWith(new Response('<!doctype html><link rel="icon" href="data:,"><title>fake</title>', {headers: {'Content-Type': 'text/html'}}));
});
`
)

// startSquatter serves, on 127.0.0.1:port, a page and the worker script
// at scriptPath, until the test closes it.
func startSquatter(t *testing.T, port int, scriptPath, script string) *http.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == scriptPath {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(script))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>squatter</title>"))
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// registrations lists the tab origin's service worker registrations, as
// "scope script" joined by "|", or "none"; "?" until the first answer.
const registrations = `(() => {
  navigator.serviceWorker.getRegistrations().then((rs) => {
    window.__regs = rs.map((r) => r.scope + ' ' + ((r.active || r.waiting || r.installing || {}).scriptURL || 'none')).join('|') || 'none';
  });
  return window.__regs === undefined ? '?' : window.__regs;
})()`

// regsOf reads tab's registrations afresh.
func regsOf(tab *chromeTab) string {
	tab.eval(`window.__regs = undefined, ''`)
	return tab.waitFor("the registrations", registrations, func(s string) bool { return s != "?" })
}

// squatted is a hub started on a port a squatter held while it was down,
// after the squatter's worker was registered from the squatter's page,
// which stays open in watch.
type squatted struct {
	watch   *chromeTab
	pageURL string
	scripts func() int // the hub's worker script fetches so far
}

func squat(t *testing.T, c *chrome, scriptPath, scope, script string) *squatted {
	t.Helper()
	o, l := localOptions(t)
	origin := "http://127.0.0.1:" + strconv.Itoa(l.port)
	sq := startSquatter(t, l.port, scriptPath, script)
	watch := c.open(t, origin+"/")
	watch.waitFor("the squatter's page", `document.title`, func(s string) bool { return s == "squatter" })
	watch.eval(`navigator.serviceWorker.register(` + strconv.Quote(scriptPath) + `, {scope: ` + strconv.Quote(scope) + `}), ''`)
	want := origin + scope + " " + origin + scriptPath
	watch.waitFor("the squatter's worker", registrations, func(s string) bool { return s == want })
	_ = sq.Close()

	var scripts atomic.Int64
	defer func(prev func(string) (net.Listener, error)) { listen = prev }(listen)
	listen = func(addr string) (net.Listener, error) {
		ln, err := livehttp.Listen(addr)
		return &scriptCounter{Listener: ln, n: &scripts}, err
	}
	in, err := start(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.shutdown)
	if got := regsOf(watch); got != want {
		t.Fatalf("the worker is gone before the page is opened: %s", got)
	}
	return &squatted{watch: watch, pageURL: strings.TrimSpace(l.stdout.String()), scripts: func() int { return int(scripts.Load()) }}
}

// waitNoRegistrations waits up to 10 s for the origin to have no worker.
func (s *squatted) waitNoRegistrations(t *testing.T) bool {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if regsOf(s.watch) == "none" {
			return true
		}
	}
	return false
}

// A squatter's worker on the hub's port, registered while the hub was
// down, is cleared once the user opens the page on the real hub: the
// browser drops it on the hub's Clear-Site-Data, and the page then loads.
func TestASquattersWorkerIsClearedByTheHubInChrome(t *testing.T) {
	c := startChrome(t)

	// A worker that fetches the page through itself gets the shell, whose
	// Clear-Site-Data drops it. The navigation it was serving does not
	// complete: Chrome leaves that tab blank; the next one loads the page.
	t.Run("a worker that proxies the page", func(t *testing.T) {
		s := squat(t, c, "/sw.js", "/s/", proxyWorker)
		// The tab is opened without waiting for its navigation, which
		// never completes.
		raw, err := c.call("", "Target.createTarget", map[string]any{"url": s.pageURL})
		if err != nil {
			t.Fatal(err)
		}
		var first struct {
			TargetID string `json:"targetId"`
		}
		if err := json.Unmarshal(raw, &first); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = c.call("", "Target.closeTarget", map[string]any{"targetId": first.TargetID}) })
		if !s.waitNoRegistrations(t) {
			t.Fatalf("the worker survived the shell: %s", regsOf(s.watch))
		}
		page := c.open(t, s.pageURL)
		page.waitFor("the real page", `document.title + ' ' + (navigator.serviceWorker.controller ? 'controlled' : 'free')`,
			func(v string) bool { return v == "perf-agent live page free" })
	})

	// A worker registered from /mcp that answers navigations itself, with
	// a page that asks for no favicon (whose 404 would clear it first):
	// the page never reaches the hub, but the browser's update check
	// fetches /mcp as a worker script, which the hub refuses with
	// Clear-Site-Data. MCPGuard alone would answer 403 and keep it.
	t.Run("a worker from the MCP path that answers navigations itself", func(t *testing.T) {
		s := squat(t, c, "/mcp", "/", fakeWorker)
		fake := c.open(t, s.pageURL)
		fake.waitFor("the worker's page", `document.title`, func(v string) bool { return v == "fake" })
		if !s.waitNoRegistrations(t) {
			t.Fatalf("the worker survived its update check (%d script fetches reached the hub): %s", s.scripts(), regsOf(s.watch))
		}
		if s.scripts() == 0 {
			t.Error("the worker was dropped without an update check reaching the hub")
		}
		page := c.open(t, s.pageURL)
		page.waitFor("the real page", `document.title + ' ' + (navigator.serviceWorker.controller ? 'controlled' : 'free')`,
			func(v string) bool { return v == "perf-agent live page free" })
	})
}

// scriptCounter counts the requests whose Sec-Fetch-Dest announces a
// service worker's script fetch.
type scriptCounter struct {
	net.Listener
	n *atomic.Int64
}

func (l *scriptCounter) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c, n: l.n}, nil
}

type countingConn struct {
	net.Conn
	n    *atomic.Int64
	tail string
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	text := c.tail + strings.ToLower(string(p[:n]))
	c.n.Add(int64(strings.Count(text, "\r\nsec-fetch-dest: serviceworker\r\n")))
	c.tail = text[max(0, len(text)-32):]
	return n, err
}
