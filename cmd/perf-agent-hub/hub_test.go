package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/mcpflags"
	"github.com/dpsoft/perf-agent/internal/mcphttp"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// newPageReport is a report as the live page posts it to its inbox.
func newPageReport(page *url.URL, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, page.String()+"inbox", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", "http://"+page.Host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Perf-Agent-Page", "1")
	return req, nil
}

// local is an instance started in this process.
type local struct {
	in       *instance
	xdg      string
	port     int
	stdout   *lockedBuffer
	stderr   *lockedBuffer
	profiles string
}

// localOptions are start's options for a hub in this process, on a free
// port, with a temporary XDG_RUNTIME_DIR, and the flags in extra.
func localOptions(t testing.TB, extra ...string) (options, *local) {
	t.Helper()
	l := &local{xdg: t.TempDir(), port: freePort(t), stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, profiles: profilesDir(t)}
	fs := flag.NewFlagSet("perf-agent-hub", flag.ContinueOnError)
	vals := mcpflags.Register(fs, mcpflags.Hub)
	if err := fs.Parse(append([]string{"--profiles", l.profiles}, extra...)); err != nil {
		t.Fatal(err)
	}
	cfg, err := vals.Config(fs, l.stderr, "test")
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "XDG_RUNTIME_DIR" {
			return l.xdg
		}
		return ""
	}
	return options{cfg: cfg, port: l.port, getenv: getenv, stdout: l.stdout}, l
}

// startLocal starts a hub in this process; the test's cleanup shuts it
// down.
func startLocal(t testing.TB, extra ...string) *local {
	t.Helper()
	o, l := localOptions(t, extra...)
	in, err := start(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	l.in = in
	t.Cleanup(in.shutdown)
	return l
}

func (l *local) client(t testing.TB, label string) *httpClient {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.xdg, "perf-agent-mcp", "mcp-token"))
	if err != nil {
		t.Fatal(err)
	}
	c := newHTTPClient(t, l.port, strings.TrimSpace(string(b)))
	c.initialize(label)
	return c
}

// TestStaleRuntimeFilesGoWhenTheLockIsTaken: the files a hub killed with
// SIGKILL left are removed as soon as the next one holds the lock, before
// it listens, so the token helper never prints a dead hub's token while
// the new one starts.
func TestStaleRuntimeFilesGoWhenTheLockIsTaken(t *testing.T) {
	o, l := localOptions(t)
	dir := filepath.Join(l.xdg, "perf-agent-mcp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := map[string]string{
		"hub.json":  fmt.Sprintf(`{"pid":999999,"port":%d,"mcp_url":"http://127.0.0.1:%d/mcp","started":"2026-11-16T00:00:00Z","version":"dev"}`, l.port, l.port),
		"mcp-token": strings.Repeat("A", 43),
		"page-url":  "http://127.0.0.1:1/s/" + strings.Repeat("B", 43) + "/",
	}
	for name, content := range stale {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func(prev func(string) (net.Listener, error)) { listen = prev }(listen)
	listen = func(addr string) (net.Listener, error) {
		close(entered)
		<-release
		return livehttp.Listen(addr)
	}
	started := make(chan error, 1)
	var in *instance
	go func() {
		var err error
		in, err = start(t.Context(), o)
		started <- err
	}()
	select {
	case <-entered:
	case err := <-started:
		t.Fatalf("start returned before it listened: %v", err)
	}
	for name := range stale {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there while the hub starts (%v)", name, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runToken([]string{"--port", strconv.Itoa(l.port)}, o.getenv, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Errorf("token while the hub starts: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	close(release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	defer in.shutdown()
	stdout.Reset()
	if code := runToken([]string{"--port", strconv.Itoa(l.port)}, o.getenv, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), strings.Repeat("A", 43)) {
		t.Errorf("token once started: exit %d, stdout %q", code, stdout.String())
	}
}

// TestShutdownOrder: the captures are killed first; the page's streams
// end; the transport closes, which ends every call's context; then the
// HTTP server shuts down, the perfmcp server closes and the files go.
func TestShutdownOrder(t *testing.T) {
	l := startLocal(t)
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	pKill, pEvents, pTransport, pHTTP, pServer, pFiles := killCaptures, closeEvents, closeTransport, shutdownHTTP, closeServer, removeFiles
	t.Cleanup(func() {
		killCaptures, closeEvents, closeTransport, shutdownHTTP, closeServer, removeFiles = pKill, pEvents, pTransport, pHTTP, pServer, pFiles
	})
	killCaptures = func(s *perfmcp.Server) { record("KillCaptures"); pKill(s) }
	closeEvents = func(h *livehttp.Hub) { record("events.Close"); pEvents(h) }
	closeTransport = func(tr *mcphttp.Transport) { record("transport.Close"); pTransport(tr) }
	shutdownHTTP = func(s *http.Server) { record("http.Shutdown"); pHTTP(s) }
	closeServer = func(s *perfmcp.Server) error { record("perfmcp.Close"); return pServer(s) }
	removeFiles = func(d *runtimeDir) error { record("files removed"); return pFiles(d) }

	c := l.client(t, "order")
	c.call("open", map[string]any{"path": "cpu.pb.gz"})
	start := time.Now()
	l.in.shutdown()
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("an idle shutdown took %v", took)
	}
	want := []string{"KillCaptures", "events.Close", "transport.Close", "http.Shutdown", "perfmcp.Close", "files removed"}
	recorded := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(order)
	}
	if got := recorded(); !slices.Equal(got, want) {
		t.Errorf("shutdown order %v, want %v", got, want)
	}
	ents, err := os.ReadDir(filepath.Join(l.xdg, "perf-agent-mcp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "hub.lock" {
		t.Errorf("runtime files after shutdown: %v", ents)
	}
	// A second shutdown, as the cleanup runs, does nothing.
	l.in.shutdown()
	if got := recorded(); len(got) != len(want) {
		t.Errorf("a second shutdown ran %v", got[len(want):])
	}
}

// TestWarningsAreFlushedWhileTheHubRuns: the counts of the warnings the
// MCP server only counted are logged every flushEvery while the hub runs,
// not only when its transport closes, and the ticker stops at shutdown.
func TestWarningsAreFlushedWhileTheHubRuns(t *testing.T) {
	var flushed atomic.Int64
	pEvery, pFlush := flushEvery, flushWarnings
	t.Cleanup(func() { flushEvery, flushWarnings = pEvery, pFlush })
	flushEvery = 20 * time.Millisecond
	flushWarnings = func(s *mcpstdio.Server) { flushed.Add(1); pFlush(s) }

	l := startLocal(t)
	deadline := time.Now().Add(5 * time.Second)
	for flushed.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("flushed %d times in 5s at every %v", flushed.Load(), flushEvery)
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.in.shutdown()
	after := flushed.Load()
	time.Sleep(10 * flushEvery)
	if n := flushed.Load(); n != after {
		t.Errorf("flushed %d more times after shutdown", n-after)
	}
}

// TestOneCallRunsAtATime (§D7): eight sessions call engine tools at once
// while two tabs read the page and post to its inbox. The engine admits
// one holder at a time, and each session's answers carry its own ids.
func TestOneCallRunsAtATime(t *testing.T) {
	var holders, maxHolders, holds atomic.Int64
	defer func(prev func(*perfmcp.Config)) { configHook = prev }(configHook)
	configHook = func(c *perfmcp.Config) {
		c.HoldEngine = func(string) {
			n := holders.Add(1)
			for {
				m := maxHolders.Load()
				if n <= m || maxHolders.CompareAndSwap(m, n) {
					break
				}
			}
			holds.Add(1)
			time.Sleep(2 * time.Millisecond)
			holders.Add(-1)
		}
	}
	l := startLocal(t)
	setup := l.client(t, "setup")
	pub := setup.publish(setup.open("cpu.pb.gz"))
	page := must(url.Parse(strings.TrimSpace(l.stdout.String())))(t)

	const sessions, calls = 8, 6
	stop := make(chan struct{})
	var tabs sync.WaitGroup
	for range 2 {
		tabs.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, route := range []string{"state", "pub/" + pub + "/model"} {
					res, err := pageHTTP.Get(page.String() + route)
					if err != nil {
						t.Errorf("GET %s: %v", route, err)
						return
					}
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					if res.StatusCode != 200 {
						t.Errorf("GET %s: %d", route, res.StatusCode)
					}
				}
				req, err := newPageReport(page, []byte(`{"kind":"select","publication":"`+pub+`","node":"0"}`))
				if err != nil {
					t.Error(err)
					return
				}
				res, err := pageHTTP.Do(req)
				if err != nil {
					t.Errorf("POST inbox: %v", err)
					return
				}
				_ = res.Body.Close()
				if res.StatusCode != 204 && res.StatusCode != 422 && res.StatusCode != 429 {
					t.Errorf("POST inbox: %d", res.StatusCode)
				}
			}
		})
	}
	var wg sync.WaitGroup
	for s := range sessions {
		c := l.client(t, fmt.Sprintf("agent-%d", s))
		wg.Go(func() {
			for i := range calls {
				id := fmt.Sprintf(`"s%d-c%d"`, s, i)
				name, args := "open", map[string]any{"path": "cpu.pb.gz"}
				if i%2 == 1 {
					name, args = "facts", map[string]any{"handle": "nope", "view": "nope"}
				}
				a, err := c.send(id, "tools/call", map[string]any{"name": name, "arguments": args})
				if err == nil {
					_, err = decode(a, id)
				}
				if err != nil {
					t.Errorf("session %d call %d: %v", s, i, err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	tabs.Wait()
	if m := maxHolders.Load(); m != 1 {
		t.Errorf("%d calls held the engine at once", m)
	}
	if n := holds.Load(); n < sessions*calls {
		t.Errorf("the engine was held %d times for %d calls", n, sessions*calls)
	}
}

// idleConns opens n connections to l that send nothing.
func idleConns(t *testing.T, l *local, n int) []net.Conn {
	t.Helper()
	conns := make([]net.Conn, n)
	for i := range conns {
		conns[i] = must(net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", l.port)))(t)
		t.Cleanup(func() { _ = conns[i].Close() })
	}
	return conns
}

// TestIdleConnectionsNeverDelayAToken: connections that never send a
// byte, which anyone on the machine can open without a token, are closed
// after firstByteWait and never stand between a client with the token
// and its answer.
func TestIdleConnectionsNeverDelayAToken(t *testing.T) {
	l := startLocal(t)
	idle := idleConns(t, l, 200)
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	l.client(t, "behind-idle")
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("initialize behind 200 idle connections took %v", took)
	}
	// Each idle connection is closed, unanswered, soon after firstByteWait.
	for i, c := range idle[:10] {
		_ = c.SetReadDeadline(time.Now().Add(firstByteWait + 2*time.Second))
		n, err := c.Read(make([]byte, 1))
		if n != 0 || isTimeoutErr(err) {
			t.Fatalf("idle connection %d: read %d bytes, %v", i, n, err)
		}
	}
	if took := time.Since(start); took < firstByteWait/2 {
		t.Errorf("idle connections were closed after %v, before firstByteWait %v", took, firstByteWait)
	}
	// Once a byte has come, the request has the server's own header
	// timeout, longer than firstByteWait.
	slow := must(net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", l.port)))(t)
	defer func() { _ = slow.Close() }()
	if _, err := io.WriteString(slow, "G"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(firstByteWait + 500*time.Millisecond)
	page := must(url.Parse(strings.TrimSpace(l.stdout.String())))(t)
	if _, err := fmt.Fprintf(slow, "ET %sstate HTTP/1.1\r\nHost: %s\r\n\r\n", page.Path, page.Host); err != nil {
		t.Fatal(err)
	}
	_ = slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(slow), nil)
	if err != nil {
		t.Fatalf("a request whose first byte came early: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("GET state: %d", res.StatusCode)
	}
}

func isTimeoutErr(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// TestTheConnectionCapHolds: the listener holds at most maxConns
// connections, a bound on file descriptors; one more waits in the
// kernel's backlog until others close, here when the idle ones are closed
// after firstByteWait.
func TestTheConnectionCapHolds(t *testing.T) {
	l := startLocal(t)
	idleConns(t, l, maxConns)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	page := must(url.Parse(strings.TrimSpace(l.stdout.String())))(t)
	extra := must(net.Dial("tcp", page.Host))(t)
	defer func() { _ = extra.Close() }()
	if _, err := fmt.Fprintf(extra, "GET %sstate HTTP/1.1\r\nHost: %s\r\n\r\n", page.Path, page.Host); err != nil {
		t.Fatal(err)
	}
	_ = extra.SetReadDeadline(time.Now().Add(firstByteWait + 5*time.Second))
	res, err := http.ReadResponse(bufio.NewReader(extra), nil)
	if err != nil {
		t.Fatalf("connection %d: %v", maxConns+1, err)
	}
	_ = res.Body.Close()
	if took := time.Since(start); took < firstByteWait/2 {
		t.Errorf("connection %d was served after %v while %d were held", maxConns+1, took, maxConns)
	}
	if res.StatusCode != 200 {
		t.Errorf("GET state: %d", res.StatusCode)
	}
}

// A signal while the hub starts is a clean end: exit 0, nothing left.
func TestASignalDuringStartExitsZero(t *testing.T) {
	xdgDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdgDir)
	ctx, cancel := context.WithCancel(t.Context())
	defer func(prev func(string) (net.Listener, error)) { listen = prev }(listen)
	listen = func(addr string) (net.Listener, error) {
		cancel()
		return livehttp.Listen(addr)
	}
	var stdout, stderr lockedBuffer
	code := runServe(ctx, []string{"--profiles", profilesDir(t), "--port", strconv.Itoa(freePort(t))}, &stdout, &stderr)
	if code != 0 || stdout.String() != "" {
		t.Errorf("a signal during start: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	ents, err := os.ReadDir(filepath.Join(xdgDir, "perf-agent-mcp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "hub.lock" {
		t.Errorf("runtime files after a signal during start: %v", ents)
	}
	// Before the lock as well.
	code = runServe(ctx, []string{"--profiles", profilesDir(t), "--port", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("an already signalled start: exit %d, stderr %q", code, stderr.String())
	}
}

// TestAnswersAreBoundedAsInStdio: the transport replaces an answer
// stdio would not send, by the same small answer: the bound follows the
// limits as stdio's output bound does.
func TestAnswersAreBoundedAsInStdio(t *testing.T) {
	small := startLocal(t, "--limit", "MaxResponseBytes=1024")
	r := small.client(t, "small").rpc("tools/list", map[string]any{})
	if r.Error == nil || !strings.Contains(r.Error.Message, "result too large for the output bound") {
		t.Errorf("tools/list under MaxResponseBytes=1024 answered %s %+v", r.Result, r.Error)
	}
	def := startLocal(t)
	if r := def.client(t, "default").rpc("tools/list", map[string]any{}); r.Error != nil {
		t.Errorf("tools/list: %+v", r.Error)
	}
}

func TestHubToolList(t *testing.T) {
	p := startHub(t, hubOptions{})
	c := p.client("lister")
	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	want := []string{"open", "top", "path", "facts", "gpu_kernels_for", "sandwich", "diff", "source", "render_flamegraph",
		"capture", "capture_wait", "capture_cancel", "capture_list",
		"page_open", "page_explain", "page_show", "page_diff", "page_state"}
	if !slices.Equal(names, want) {
		t.Errorf("tools/list %v, want %v", names, want)
	}
	var res struct {
		Resources []struct{ URI string } `json:"resources"`
	}
	if err := json.Unmarshal(c.rpc("resources/list", map[string]any{}).Result, &res); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Resources, func(r struct{ URI string }) bool { return r.URI == "perf-agent://page/state" }) {
		t.Errorf("resources %v lack perf-agent://page/state", res.Resources)
	}
	var info struct {
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	fresh := newHTTPClient(t, p.port, p.mcpToken())
	if err := json.Unmarshal(fresh.rpc("initialize", map[string]any{"protocolVersion": protocolVersion}).Result, &info); err != nil || info.ServerInfo.Name != "perf-agent-hub" {
		t.Errorf("serverInfo.name %q (%v)", info.ServerInfo.Name, err)
	}
}

// rawRequest sends one request on a new connection, with exactly the
// headers given and a body, and returns the status and the headers.
func rawRequest(t testing.TB, port int, method, path string, header map[string]string, body string) (int, http.Header) {
	t.Helper()
	conn := must(net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second))(t)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, path)
	if _, ok := header["Host"]; !ok {
		fmt.Fprintf(&b, "Host: 127.0.0.1:%d\r\n", port)
	}
	for k, v := range header {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" || method == http.MethodPost {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n" + body)
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_ = res.Body.Close()
	return res.StatusCode, res.Header
}

// TestTheChainIsComposedEndToEnd drives the real binary's one listener:
// Front's Host check and routing, then MCPGuard before the transport,
// InboxGuard before the inbox, and Guard before the page's routes. Each
// token opens only its own door.
func TestTheChainIsComposedEndToEnd(t *testing.T) {
	p := startHub(t, hubOptions{})
	mcpTok := p.mcpToken()
	page := must(url.Parse(p.pageURL()))(t)
	pageTok := strings.Trim(strings.TrimPrefix(page.Path, "/s/"), "/")
	hello := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`
	mcpHeaders := func(extra ...string) map[string]string {
		h := map[string]string{"Authorization": "Bearer " + mcpTok, "Accept": "application/json, text/event-stream", "Content-Type": "application/json"}
		for i := 0; i+1 < len(extra); i += 2 {
			if extra[i+1] == "" {
				delete(h, extra[i])
				continue
			}
			h[extra[i]] = extra[i+1]
		}
		return h
	}
	report := `{"kind":"select","publication":"p1","node":"0"}`
	pageHeaders := func(extra ...string) map[string]string {
		h := map[string]string{"Origin": fmt.Sprintf("http://127.0.0.1:%d", p.port), "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json", "X-Perf-Agent-Page": "1"}
		for i := 0; i+1 < len(extra); i += 2 {
			if extra[i+1] == "" {
				delete(h, extra[i])
				continue
			}
			h[extra[i]] = extra[i+1]
		}
		return h
	}
	for _, c := range []struct {
		name         string
		method, path string
		header       map[string]string
		body         string
		want         int
	}{
		{"mcp: initialize", "POST", "/mcp", mcpHeaders(), hello, 200},
		{"front: a rebinding Host", "POST", "/mcp", mcpHeaders("Host", "evil.example:80"), hello, 403},
		{"front: an unknown path", "GET", "/", nil, "", 404},
		{"mcp guard: a browser", "POST", "/mcp", mcpHeaders("Origin", "http://127.0.0.1"), hello, 403},
		{"mcp guard: GET", "GET", "/mcp", mcpHeaders(), "", 405},
		{"mcp guard: no bearer", "POST", "/mcp", mcpHeaders("Authorization", ""), hello, 401},
		{"mcp guard: the page token as bearer", "POST", "/mcp", mcpHeaders("Authorization", "Bearer "+pageTok), hello, 401},
		{"mcp guard: Accept", "POST", "/mcp", mcpHeaders("Accept", "application/json"), hello, 400},
		{"transport: no session", "POST", "/mcp", mcpHeaders(), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 400},
		{"page: the shell", "GET", page.Path, nil, "", 200},
		{"page: the state", "GET", page.Path + "state", nil, "", 200},
		{"page: the MCP token as page token", "GET", "/s/" + mcpTok + "/state", nil, "", 404},
		{"page guard: a cross-site read", "GET", page.Path + "state", map[string]string{"Sec-Fetch-Site": "cross-site"}, "", 403},
		{"page guard: POST state", "POST", page.Path + "state", pageHeaders(), report, 400},
		{"inbox: a report", "POST", page.Path + "inbox", pageHeaders(), report, 422},
		{"inbox guard: no Origin", "POST", page.Path + "inbox", pageHeaders("Origin", ""), report, 403},
		{"inbox guard: no page mark", "POST", page.Path + "inbox", pageHeaders("X-Perf-Agent-Page", ""), report, 403},
		{"inbox guard: the MCP token", "POST", "/s/" + mcpTok + "/inbox", pageHeaders(), report, 404},
	} {
		if got, h := rawRequest(t, p.port, c.method, c.path, c.header, c.body); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		} else if got == 401 && h.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: WWW-Authenticate %q", c.name, h.Get("WWW-Authenticate"))
		} else if h.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no security headers", c.name)
		}
	}
	// With a publication, a report of one of its nodes is recorded, and
	// page_state reads it.
	cl := p.client("chain")
	o := cl.open("cpu.pb.gz")
	pub := cl.publish(o)
	node := cl.nodeID(o, "b")
	if code := postReport(t, p.pageURL(), map[string]string{"kind": "ask", "publication": pub, "node": node}); code != 204 {
		t.Fatalf("an ask: %d", code)
	}
	if state := cl.call("page_state", map[string]any{}); !strings.Contains(state, `"b"`) {
		t.Errorf("page_state does not hold the ask of b: %s", state)
	}
}

// TestShutdownStopsTheListener: the hub serves the page and a stream;
// SIGTERM ends the stream, the hub exits 0, and the port accepts no
// connection afterwards.
func TestShutdownStopsTheListener(t *testing.T) {
	p := startHub(t, hubOptions{})
	c := p.client("listener")
	c.publish(c.open("cpu.pb.gz"))
	page := p.pageURL()
	if code, body := httpGet(t, page); code != 200 || !bytes.HasPrefix(body, []byte("<!doctype html>")) {
		t.Fatalf("the shell: %d %.80q", code, body)
	}
	res, err := pageHTTP.Get(page + "events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	br := bufio.NewReader(res.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if line == "event: state\n" {
			break
		}
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, br)
		ended <- err
	}()
	took := p.stop(syscall.SIGTERM, 2*time.Second)
	// The stream ends before the HTTP server waits for it.
	if took > 800*time.Millisecond {
		t.Errorf("SIGTERM with a stream open took %v", took)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Error("the stream goes on after the hub exited")
	}
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.port), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the port still accepts connections after the hub exited")
	}
}

// flakyListener fails its first fails Accepts with err, then accepts.
type flakyListener struct {
	net.Listener
	mu    sync.Mutex
	fails int
	err   error
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.fails > 0 {
		l.fails--
		l.mu.Unlock()
		return nil, l.err
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

// TestATemporaryAcceptErrorIsRetried: running out of file descriptors
// (EMFILE) fails an Accept for a while; the hub keeps accepting after it,
// as net/http's own loop does, and serves the next connection.
func TestATemporaryAcceptErrorIsRetried(t *testing.T) {
	defer func(prev func(string) (net.Listener, error)) { listen = prev }(listen)
	listen = func(addr string) (net.Listener, error) {
		ln, err := livehttp.Listen(addr)
		if err != nil {
			return nil, err
		}
		emfile := &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
		return &flakyListener{Listener: ln, fails: 3, err: emfile}, nil
	}
	l := startLocal(t)
	start := time.Now()
	l.client(t, "after-emfile")
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the first connection after three EMFILE took %v", took)
	}
	l.client(t, "again")
}

// A permanent Accept error ends accepting: the listener reports it, as
// net/http's Serve then returns it, so the hub exits instead of hanging.
func TestAPermanentAcceptErrorIsReported(t *testing.T) {
	inner := must(net.Listen("tcp", "127.0.0.1:0"))(t)
	permanent := errors.New("the listener broke")
	l := newConnListener(&flakyListener{Listener: inner, fails: 1, err: permanent}, 4, time.Second)
	defer func() { _ = l.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, permanent) {
			t.Errorf("Accept: %v, want %v", err, permanent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a permanent Accept error was never reported")
	}
}
