package flamegraph

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

func liveAsset(t *testing.T, name string) string {
	t.Helper()
	body, _, ok := LiveAsset(name)
	require.True(t, ok, "asset %q", name)
	return string(body)
}

func TestLiveShellHasNoInlineScriptOrStyle(t *testing.T) {
	shell := liveAsset(t, "")
	for _, tag := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(shell, -1) {
		assert.Regexp(t, `\bsrc="`, tag, "every script is a file")
	}
	assert.NotRegexp(t, `(?i)<style`, shell)
	assert.NotRegexp(t, `(?i)\sstyle=`, shell)
	assert.NotRegexp(t, `(?i)\son[a-z]+=`, shell)
	refs := regexp.MustCompile(`(?i)\s(?:src|href)="([^"]*)"`).FindAllStringSubmatch(shell, -1)
	require.NotEmpty(t, refs)
	for _, m := range refs {
		u := m[1]
		assert.NotContains(t, u, "//", "%q is relative", u)
		first, _, _ := strings.Cut(u, "/")
		assert.NotContains(t, first, ":", "%q names no scheme", u)
		assert.False(t, strings.HasPrefix(u, "/"), "%q is relative to the page", u)
	}

	for _, id := range []string{"chart", "sel-ref", "sel-name", "sel-id", "facts", "exp", "noexp", "path", "chips", "copybox", "toast",
		"pubs", "banner", "rejected", "pills", "notes", "quality", "rows", "legend", "axis", "export", "focus-btn", "whole-btn"} {
		assert.Equal(t, 1, strings.Count(shell, ` id="`+id+`"`), "region %s", id)
	}
	assert.Contains(t, shell, `<link rel="stylesheet" href="assets/live.css">`)
	assert.Contains(t, shell, `<script src="assets/panel.js" defer></script>`)
	assert.Contains(t, shell, `<script src="assets/live.js" defer></script>`)
	assert.Less(t, strings.Index(shell, "assets/panel.js"), strings.Index(shell, "assets/live.js"), "the panel is defined before the page uses it")
	assert.Contains(t, shell, "Waiting for your agent to publish a page (page_explain or page_diff).")
	assert.Regexp(t, `<a [^>]*id="export"[^>]*\bdownload\b`, shell)
}

// fetchArgs are the argument lists of every fetch and EventSource call.
var fetchArgs = regexp.MustCompile(`(?:fetch|EventSource)\(([^)]*)\)`)

func TestLiveScriptRules(t *testing.T) {
	live := liveAsset(t, "assets/live.js")
	panel := liveAsset(t, "assets/panel.js")
	forbidden := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "setAttribute('style'",
		`setAttribute("style"`, "cssText", "toFixed", "toPrecision", "toLocaleString", "Intl.", "parseFloat", "Math.round",
		"http:", "https:"}
	for name, src := range map[string]string{"live.js": live, "panel.js": panel} {
		for _, s := range forbidden {
			assert.NotContains(t, src, s, "%s uses %s", name, s)
		}
		for _, m := range fetchArgs.FindAllStringSubmatch(src, -1) {
			assert.NotContains(t, m[1], "//", "%s requests %s", name, m[1])
		}
	}
	require.NotEmpty(t, fetchArgs.FindAllString(live, -1), "the script requests something")
	for _, m := range fetchArgs.FindAllStringSubmatch(live, -1) {
		assert.NotContains(t, m[1], "/", "%s: the transport names its paths only through its callers", m[1])
	}
	// seq is a decimal string compared by newer; it never becomes a number.
	for _, s := range []string{"Number(", "parseInt", "BigInt", "+st.seq", "seq -", "seq +", "seq >", "seq <"} {
		assert.NotContains(t, live, s)
	}
	assert.Contains(t, live, "cache: 'no-store'")
	assert.Contains(t, live, "new EventSource('events')")
	assert.Contains(t, live, "getJSON('state')")
	assert.Equal(t, panel, explainPanelScript, "the live page and the static page share one panel")
}

func TestLiveAssetsAreStable(t *testing.T) {
	types := map[string]string{
		"":                "text/html; charset=utf-8",
		"assets/live.css": "text/css; charset=utf-8",
		"assets/panel.js": "text/javascript; charset=utf-8",
		"assets/live.js":  "text/javascript; charset=utf-8",
	}
	for name, typ := range types {
		a, ta, ok := LiveAsset(name)
		require.True(t, ok, name)
		a[0] ^= 0xff // a caller's change must not reach the next caller
		b, tb, ok := LiveAsset(name)
		require.True(t, ok, name)
		a[0] ^= 0xff
		assert.Equal(t, a, b, name)
		assert.Equal(t, typ, ta, name)
		assert.Equal(t, typ, tb, name)
		assert.NotEmpty(t, b)
	}
	css := liveAsset(t, "assets/live.css")
	assert.True(t, strings.HasPrefix(css, explainedCSS()), "the live stylesheet starts with the explained page's")
	assert.Contains(t, css, paletteCSS)
	assert.Contains(t, css, jitterCSS)
	assert.NotContains(t, css, "/*LIGHT*/")
	for _, name := range []string{"assets/", "assets/probe.js", "live.js", "assets/live.js/", "/", "index.html", "assets/../live.js"} {
		_, _, ok := LiveAsset(name)
		assert.False(t, ok, name)
	}
}

// --- Headless Chrome, driven over the DevTools protocol on a pipe. ---

// The page keeps an event stream open, so Chrome's virtual time never
// advances past it and --dump-dom never returns; the tests drive Chrome
// through --remote-debugging-pipe instead, in real time.

type cdpMessage struct {
	ID        int             `json:"id,omitzero"`
	Method    string          `json:"method,omitzero"`
	SessionID string          `json:"sessionId,omitzero"`
	Params    json.RawMessage `json:"params,omitzero"`
	Result    json.RawMessage `json:"result,omitzero"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitzero"`
}

type chromeSession struct {
	cmd *exec.Cmd
	w   *os.File

	mu      sync.Mutex
	next    int
	waiting map[int]chan cdpMessage
	errs    map[string][]string // per session: exceptions and console errors
	done    chan struct{}
}

func startChrome(t *testing.T) *chromeSession {
	t.Helper()
	chrome := chromePath()
	if chrome == "" || testing.Short() {
		t.Skip("no headless Chrome")
	}
	toChrome, w, err := os.Pipe()
	require.NoError(t, err)
	r, fromChrome, err := os.Pipe()
	require.NoError(t, err)
	dir := t.TempDir()
	cmd := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--remote-debugging-pipe", "--user-data-dir="+filepath.Join(dir, "profile"), "--window-size=1200,900", "about:blank")
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome} // fd 3: commands in, fd 4: replies out
	killWithTest(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	_ = toChrome.Close()
	_ = fromChrome.Close()
	c := &chromeSession{cmd: cmd, w: w, waiting: map[int]chan cdpMessage{}, errs: map[string][]string{}, done: make(chan struct{})}
	go c.read(bufio.NewReader(r))
	t.Cleanup(func() {
		_, _ = c.call("", "Browser.close", map[string]any{})
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		_ = w.Close()
		_ = r.Close()
	})
	if _, err := c.call("", "Target.getTargets", map[string]any{}); err != nil {
		t.Skipf("headless Chrome does not answer on its pipe: %v", err)
	}
	return c
}

func (c *chromeSession) read(r *bufio.Reader) {
	defer close(c.done)
	for {
		raw, err := r.ReadBytes(0)
		if err != nil {
			return
		}
		var m cdpMessage
		if json.Unmarshal(raw[:len(raw)-1], &m) != nil {
			continue
		}
		c.mu.Lock()
		if ch, ok := c.waiting[m.ID]; ok && m.ID != 0 {
			delete(c.waiting, m.ID)
			ch <- m
		}
		switch m.Method {
		case "Runtime.exceptionThrown":
			c.errs[m.SessionID] = append(c.errs[m.SessionID], "exception: "+string(m.Params))
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.Type == "error" || p.Type == "assert" {
				c.errs[m.SessionID] = append(c.errs[m.SessionID], "console: "+string(m.Params))
			}
		}
		c.mu.Unlock()
	}
}

func (c *chromeSession) call(session, method string, params any) (json.RawMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	ch := make(chan cdpMessage, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.waiting[id] = ch
	c.mu.Unlock()
	msg, err := json.Marshal(cdpMessage{ID: id, Method: method, SessionID: session, Params: p})
	if err != nil {
		return nil, err
	}
	if _, err := c.w.Write(append(msg, 0)); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, errors.New(method + ": " + m.Error.Message)
		}
		return m.Result, nil
	case <-c.done:
		return nil, errors.New(method + ": Chrome exited")
	case <-time.After(15 * time.Second):
		return nil, errors.New(method + ": no reply")
	}
}

// liveTab is one tab of the browser, used by one test.
type liveTab struct {
	c       *chromeSession
	t       *testing.T
	session string
}

// open loads url in a new tab.
func (c *chromeSession) open(t *testing.T, url string) *liveTab {
	t.Helper()
	raw, err := c.call("", "Target.createTarget", map[string]any{"url": "about:blank"})
	require.NoError(t, err)
	var target struct {
		TargetID string `json:"targetId"`
	}
	require.NoError(t, json.Unmarshal(raw, &target))
	raw, err = c.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true})
	require.NoError(t, err)
	var att struct {
		SessionID string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(raw, &att))
	_, err = c.call(att.SessionID, "Runtime.enable", map[string]any{})
	require.NoError(t, err)
	_, err = c.call(att.SessionID, "Page.navigate", map[string]any{"url": url})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = c.call("", "Target.closeTarget", map[string]any{"targetId": target.TargetID}) })
	return &liveTab{c: c, t: t, session: att.SessionID}
}

// eval runs expr in the tab and returns its string value.
func (tab *liveTab) eval(expr string) string {
	t := tab.t
	t.Helper()
	raw, err := tab.c.call(tab.session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	require.NoError(t, err)
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Nil(t, out.ExceptionDetails, "%s: %s", expr, out.ExceptionDetails)
	s, _ := out.Result.Value.(string)
	return s
}

// errors are the exceptions and console errors the tab reported.
func (tab *liveTab) errors() []string {
	tab.c.mu.Lock()
	defer tab.c.mu.Unlock()
	return slices.Clone(tab.c.errs[tab.session])
}

// liveDOM is what the tests read back from the page.
type liveDOM struct {
	Title      string   `json:"title"`
	Marks      []string `json:"marks"`
	DocTitle   string   `json:"doc_title"`
	SelRef     string   `json:"sel_ref"`
	SelName    string   `json:"sel_name"`
	SelID      string   `json:"sel_id"`
	Rows       []string `json:"rows"`
	Rejected   []string `json:"rejected"`
	Pills      []string `json:"pills"`
	Banner     string   `json:"banner"`
	BannerHid  bool     `json:"banner_hidden"`
	BannerKind string   `json:"banner_kind"`
	Export     string   `json:"export"`
	ExportHid  bool     `json:"export_hidden"`
	Frames     []string `json:"frames"`
	Cur        []string `json:"cur"`
	Path       []string `json:"path"`
	Axis       string   `json:"axis"`
	Toast      string   `json:"toast"`
	Pubs       []string `json:"pubs"`
	Waiting    bool     `json:"waiting"`
	LoadNote   string   `json:"load_note"`
	Facts      int      `json:"facts"`
	Chips      []string `json:"chips"`
	Cites      []string `json:"cites"`
	FrameGeo   []string `json:"geo"`
	Violations int      `json:"violations"`
	FocusOff   bool     `json:"focus_disabled"`
	WholeOff   bool     `json:"whole_disabled"`
}

const domProbe = `(() => {
  const $ = (id) => document.getElementById(id);
  if (document.readyState !== 'complete' || !$('title')) return '';
  const all = (sel) => [...document.querySelectorAll(sel)];
  const text = (sel) => all(sel).map((e) => e.textContent);
  return JSON.stringify({
    title: $('title').textContent,
    marks: text('#title mark.unchecked'),
    doc_title: document.title,
    sel_ref: $('sel-ref').textContent,
    sel_name: $('sel-name').textContent,
    sel_id: $('sel-id').textContent,
    rows: text('#rows .exrow'),
    rejected: text('#rejected li'),
    pills: text('#pills .pill'),
    banner: $('banner').textContent,
    banner_hidden: $('banner').hidden,
    banner_kind: $('banner').className,
    export: $('export').getAttribute('href') || '',
    export_hidden: $('export').hidden,
    frames: text('#chart .frame'),
    cur: text('#chart .frame.cur'),
    path: all('#path button').map((b) => b.querySelector('.ref').textContent + ' ' + b.querySelector('.pn').textContent),
    axis: $('axis').textContent,
    toast: $('toast').textContent,
    pubs: text('#pubs button'),
    waiting: !$('waiting').hidden,
    load_note: $('load-note') && !$('load-note').hidden ? $('load-note').textContent : '',
    facts: $('facts').children.length,
    chips: text('#chips .chip'),
    cites: text('#exp button.cite'),
    geo: all('#chart .frame').map((e) => e.style.left + ' ' + e.style.width + ' ' + e.style.top),
    violations: window.livePageProbe ? window.livePageProbe.violations : -1,
    focus_disabled: $('focus-btn').disabled,
    whole_disabled: $('whole-btn').disabled,
  });
})()`

func (tab *liveTab) dom() liveDOM {
	tab.t.Helper()
	var d liveDOM
	require.NoError(tab.t, json.Unmarshal([]byte(tab.waitText(domProbe)), &d))
	return d
}

// waitText polls expr until it returns a non-empty string.
func (tab *liveTab) waitText(expr string) string {
	tab.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s := tab.eval(expr); s != "" {
			return s
		}
		if time.Now().After(deadline) {
			require.FailNow(tab.t, "no text from "+expr, "errors: %v", tab.errors())
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// waitFor polls the page until cond holds on its DOM, and fails the test
// with the last DOM read otherwise.
func (tab *liveTab) waitFor(what string, cond func(liveDOM) bool) liveDOM {
	tab.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d := tab.dom()
		if cond(d) {
			return d
		}
		if time.Now().After(deadline) {
			require.FailNow(tab.t, "the page never showed "+what, "%+v\nerrors: %v", d, tab.errors())
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// probeScript counts content-security-policy violations from the first
// moment, and after 1.5 s writes the selection, the count and whether the
// banner is hidden into <pre id="probe">.
const probeScript = `window.livePageProbe = {violations: 0};
document.addEventListener('securitypolicyviolation', () => { window.livePageProbe.violations++; });
addEventListener('load', () => setTimeout(() => {
  const pre = document.createElement('pre');
  pre.id = 'probe';
  pre.textContent = [document.getElementById('sel-name').textContent, String(window.livePageProbe.violations),
    String(document.getElementById('banner').hidden)].join('\n');
  document.body.appendChild(pre);
}, 1500));
`

// --- A fake live server: the real assets and guard, scripted data. ---

type livePub struct {
	whole   []byte
	frames  map[string][]byte // node -> its frame response
	focusAt map[string][]byte // node -> the model focused on it
}

type liveServer struct {
	srv *httptest.Server
	url string

	mu        sync.Mutex
	state     []byte // what GET state answers
	stateGets int
	sse       []byte // the last state pushed on the stream, sent first on connect
	streams   map[chan []byte]bool
	refuse    bool // the stream answers 503
	pubs      map[string]*livePub
	history   map[string][]byte // pub -> its current history JSON
	delay     map[string]time.Duration
	delayOnce map[string][]time.Duration // per-request delays by path, first request first
	served    []string
	gone      chan struct{}
	closeOnce sync.Once
}

func newLiveServer(t *testing.T) *liveServer {
	t.Helper()
	tok, err := livehttp.NewToken()
	require.NoError(t, err)
	s := &liveServer{streams: map[chan []byte]bool{}, pubs: map[string]*livePub{}, history: map[string][]byte{},
		delay: map[string]time.Duration{}, delayOnce: map[string][]time.Duration{}, gone: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		shell := liveAsset(t, "")
		shell = strings.Replace(shell, "<head>\n", "<head>\n<script src=\"assets/probe.js\"></script>\n", 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(shell))
	})
	mux.HandleFunc("GET /assets/probe.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(probeScript))
	})
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		body, typ, ok := LiveAsset("assets/" + r.PathValue("name"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", typ)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.stateGets++
		body := s.state
		s.mu.Unlock()
		s.wait(r, "state")
		s.json(w, body)
	})
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /pub/{id}/model", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, func(p *livePub) []byte { return p.whole })
	})
	mux.HandleFunc("GET /pub/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		body := s.history[r.PathValue("id")]
		s.mu.Unlock()
		s.wait(r, "pub/"+r.PathValue("id")+"/history"+historyVersion(body))
		s.json(w, body)
	})
	mux.HandleFunc("GET /pub/{id}/frame/{node}", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, func(p *livePub) []byte { return p.frames[r.PathValue("node")] })
	})
	mux.HandleFunc("GET /pub/{id}/focus/{node}", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, func(p *livePub) []byte { return p.focusAt[r.PathValue("node")] })
	})
	mux.HandleFunc("GET /pub/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", livehttp.ExportCSP)
		w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("id")+`.html"`)
		_, _ = w.Write([]byte("<!doctype html>"))
	})
	s.srv = httptest.NewServer(livehttp.Guard(tok, mux))
	t.Cleanup(s.close)
	s.url = s.srv.URL + "/s/" + string(tok) + "/"
	return s
}

func historyVersion(body []byte) string {
	var h struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(body, &h)
	return "@" + h.Version
}

// wait sleeps the delay set for key, if any, unless the client goes away.
func (s *liveServer) wait(r *http.Request, key string) {
	s.mu.Lock()
	d := s.delay[key]
	if q := s.delayOnce[key]; len(q) > 0 {
		d, s.delayOnce[key] = q[0], q[1:]
	}
	s.served = append(s.served, key)
	s.mu.Unlock()
	select {
	case <-time.After(d):
	case <-r.Context().Done():
	case <-s.gone:
	}
}

func (s *liveServer) json(w http.ResponseWriter, body []byte) {
	if body == nil {
		http.Error(w, "no such publication or node; the page will refresh", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *liveServer) serve(w http.ResponseWriter, r *http.Request, pick func(*livePub) []byte) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	p := s.pubs[r.PathValue("id")]
	s.mu.Unlock()
	s.wait(r, path)
	if p == nil {
		s.json(w, nil)
		return
	}
	s.json(w, pick(p))
}

func (s *liveServer) events(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.refuse {
		s.mu.Unlock()
		http.Error(w, "too many open pages", http.StatusServiceUnavailable)
		return
	}
	box := make(chan []byte, 16)
	s.streams[box] = true
	first := s.sse
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, box)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	send := func(data []byte) bool {
		_, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data)
		return err == nil && rc.Flush() == nil
	}
	if first != nil && !send(first) {
		return
	}
	_ = rc.Flush()
	for {
		select {
		case data := <-box:
			if data == nil || !send(data) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.gone:
			return
		}
	}
}

// servedPaths lists the paths the server was asked for, in order.
func (s *liveServer) servedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.served)
}

// push sends state on every open stream, as the hub does.
func (s *liveServer) push(state []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sse = state
	for box := range s.streams {
		box <- state
	}
}

// drop ends every open stream; the browser reconnects.
func (s *liveServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for box := range s.streams {
		box <- nil
	}
}

func (s *liveServer) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *liveServer) setState(state []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

func (s *liveServer) gets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateGets
}

func (s *liveServer) setDelay(key string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay[key] = d
}

func (s *liveServer) setHistory(pub string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[pub] = body
}

// close stops the server as a process exit would: the port is closed
// and every stream ends.
func (s *liveServer) close() {
	s.closeOnce.Do(func() {
		close(s.gone)
		s.srv.Close()
	})
}

// --- The data: CPUBasic, drawn with at most four frames so c is not. ---

type liveFixture struct {
	v             *profilemodel.View
	a, b, c, main profilemodel.NodeID
	results       map[string]explain.Result
}

func newLiveFixture(t *testing.T) *liveFixture {
	v := exView(t, pmfixture.CPUBasic(), nil)
	f := &liveFixture{v: v, main: exNode(t, v, "main"), a: exNode(t, v, "main", "a"), b: exNode(t, v, "main", "a", "b"),
		c: exNode(t, v, "main", "c"), results: map[string]explain.Result{}}
	_, _, f.results["p1"] = cpuBasicExplained(t)
	f.results["p2"] = exCheck(t, v, explain.Explanation{Node: string(f.c), Prose: "c is the other half, beside {n:" + string(f.a) + "}."})
	f.results["p3"] = exCheck(t, v, explain.Explanation{Node: string(f.a), Prose: "a calls b."})
	return f
}

var liveTitles = map[string]string{"p1": "first look at 3 frames", "p2": "second", "p3": "third"}

func (f *liveFixture) pub(t *testing.T, id string) *livePub {
	t.Helper()
	r := f.results[id]
	opts := LiveOptions{Title: liveTitles[id], ProfileName: "cpu.pb.gz", Digest: exDigest, Attempt: "a2", MaxNodes: 4}
	whole, err := ExplainedLiveModel(f.v, r, opts)
	require.NoError(t, err)
	p := &livePub{whole: whole, frames: map[string][]byte{}, focusAt: map[string][]byte{}}
	for _, n := range []profilemodel.NodeID{f.main, f.a, f.b, f.c} {
		p.frames[string(n)], err = ExplainedLiveFrame(f.v, r, n, exDigest)
		require.NoError(t, err)
		o := opts
		o.Focus = n
		p.focusAt[string(n)], err = ExplainedLiveModel(f.v, r, o)
		require.NoError(t, err)
	}
	return p
}

// history is pub's history at version with one rejected attempt per name.
func (f *liveFixture) history(pub, version string, attempts ...string) []byte {
	var rejected []explain.RejectedAttempt
	for _, a := range attempts {
		rejected = append(rejected, explain.RejectedAttempt{Attempt: a, Failures: []explain.Failure{
			{Explanation: -1, Ref: "MaxProseBytes", Code: explain.CodeLimitExceeded, Message: "the prose of " + a + " is too long"},
		}})
	}
	return ExplainedLiveHistory(f.v, pub, version, rejected, 0, false, 1<<20)
}

type livePubEntry struct {
	ID        string      `json:"id"`
	Kind      string      `json:"kind"`
	Title     string      `json:"title"`
	TitleSegs []exSegment `json:"title_segs"`
	View      string      `json:"view"`
	Digest    string      `json:"digest"`
	History   string      `json:"history"`
}

type liveStateJSON struct {
	Seq          string         `json:"seq"`
	Current      string         `json:"current"`
	Selected     string         `json:"selected"`
	Focus        string         `json:"focus"`
	Publications []livePubEntry `json:"publications"`
}

// state is a state JSON; pubs are "id" or "id@historyVersion".
func liveState(seq, current string, selected, focus profilemodel.NodeID, pubs ...string) []byte {
	st := liveStateJSON{Seq: seq, Current: current, Selected: string(selected), Focus: string(focus), Publications: []livePubEntry{}}
	for _, p := range pubs {
		id, version, _ := strings.Cut(p, "@")
		st.Publications = append(st.Publications, livePubEntry{ID: id, Kind: "explained", Title: liveTitles[id],
			TitleSegs: titleSegments(ExplainedPageOptions{Title: liveTitles[id]}), View: "cpu/nanoseconds#0", Digest: exDigest,
			History: cmp.Or(version, "0")})
	}
	out, err := json.Marshal(st)
	if err != nil {
		panic(err)
	}
	return out
}

// newLivePage is a server holding p1, p2 and p3, with GET state and the
// stream both at state, and a tab open on it.
func newLivePage(t *testing.T, c *chromeSession, f *liveFixture, state []byte, setup func(*liveServer)) (*liveServer, *liveTab) {
	t.Helper()
	s := newLiveServer(t)
	for _, id := range []string{"p1", "p2", "p3"} {
		s.pubs[id] = f.pub(t, id)
		s.history[id] = f.history(id, "0")
	}
	s.state, s.sse = state, state
	if setup != nil {
		setup(s)
	}
	return s, c.open(t, s.url)
}

// liveDiff is CPUBasic against CPUBasicNew, published as p4 with at most
// four frames drawn: all, main, a and d, so b and c are left out.
type liveDiff struct {
	d     *profilemodel.Diff
	ids   map[string]profilemodel.UnionID // name -> union ID
	pub   *livePub
	model map[string]any
}

func newLiveDiff(t *testing.T) *liveDiff {
	t.Helper()
	d := compared(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	ix := profilemodel.NewDiffIndex(d)
	ld := &liveDiff{d: d, ids: map[string]profilemodel.UnionID{}}
	opts := DiffLiveOptions{Title: "b went down by 111", BaseName: "cpu.pb.gz", NewName: "cpu-new.pb.gz", MaxNodes: 4}
	whole, err := DiffLiveModel(d, opts)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(whole, &ld.model))
	ld.pub = &livePub{whole: whole, frames: map[string][]byte{}, focusAt: map[string][]byte{}}
	for _, f := range d.Flatten().Nodes {
		ld.ids[f.Name] = f.ID
		ld.pub.frames[string(f.ID)], err = DiffLiveFrame(d, ix, f.ID)
		require.NoError(t, err)
		o := opts
		o.Focus = f.ID
		ld.pub.focusAt[string(f.ID)], err = DiffLiveModel(d, o)
		require.NoError(t, err)
	}
	return ld
}

// diffState is a state showing p4, the diff, with node selected.
func diffState(seq string, selected profilemodel.UnionID) []byte {
	st := liveStateJSON{Seq: seq, Current: "p4", Selected: string(selected), Publications: []livePubEntry{{
		ID: "p4", Kind: "diff", Title: "b went down by 111",
		TitleSegs: titleSegments(ExplainedPageOptions{Title: "b went down by 111"}), View: "cpu/nanoseconds#0", History: "0",
	}}}
	out, err := json.Marshal(st)
	if err != nil {
		panic(err)
	}
	return out
}

const factsProbe = `[...document.querySelectorAll('#facts .v')].map((e) => e.textContent + '|' + e.title).join('\n')`

func TestLivePageInChrome(t *testing.T) {
	c := startChrome(t)
	f := newLiveFixture(t)
	seq1 := liveState("1", "p1", f.b, "", "p1")
	seq2 := liveState("2", "p1", f.c, "", "p1")

	t.Run("the agent's newest state is shown", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, seq1, nil)
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" })
		time.Sleep(300 * time.Millisecond)
		s.push(seq2)
		d := tab.waitFor("seq 2", func(d liveDOM) bool { return d.SelName == "c" })
		lines := strings.Split(tab.waitText(`(document.getElementById('probe') || {}).textContent || ''`), "\n")
		require.Len(t, lines, 3)
		assert.Equal(t, "c", lines[0], "the probe saw seq 2's selection")
		assert.Equal(t, "0", lines[1], "no content-security-policy violation")
		assert.Equal(t, "true", lines[2], "the banner is hidden")

		// c is not in the model: its facts came from the frame route, and
		// it is named by its node ID wherever a ref would be.
		assert.Equal(t, string(f.c), d.SelRef)
		assert.Equal(t, string(f.c), d.SelID)
		assert.Equal(t, 5, d.Facts/3, "five fact rows")
		refs := modelRefs(t, s.pubs["p1"].whole)
		assert.Equal(t, []string{refs[f.main] + " main", string(f.c) + " c"}, d.Path)
		assert.Equal(t, "Your agent selected "+string(f.c)+" c", d.Toast)
		assert.Empty(t, d.Cur, "no drawn frame is current: the static page's cur on b is the page's own")
		assert.Equal(t, []string{"all", "main", "a", "✦b"}, d.Frames)
		assert.Equal(t, modelGeometry(t, s.pubs["p1"].whole), d.FrameGeo)
		assert.Equal(t, []string{"0% 100% 0px", "0% 100% 20px", "0% 51.1% 40px", "0% 41.1% 60px"}, d.FrameGeo)

		// The heading is built from the title's segments, numbers marked.
		assert.Equal(t, "first look at 3 frames", d.Title)
		assert.Equal(t, []string{"3"}, d.Marks)
		assert.Equal(t, "first look at 3 frames", d.DocTitle)
		assert.Equal(t, "pub/p1/export", d.Export)
		assert.False(t, d.ExportHid)
		assert.False(t, d.Waiting)
		assert.Contains(t, d.Pills, "3 figures and references checked")
		assert.Len(t, d.Rows, 1)
		assert.Contains(t, d.Rows[0], "b")
		assert.Contains(t, d.Axis, "total 1.00 µs")
		assert.Empty(t, tab.errors())

		// A click on a drawn frame is local; a row selects its node.
		tab.eval(`[...document.querySelectorAll('#chart .frame')].find((e) => e.textContent === 'a').click(), ''`)
		d = tab.waitFor("a clicked", func(d liveDOM) bool { return d.SelName == "a" })
		assert.Equal(t, []string{"a"}, d.Cur)
		assert.Equal(t, refs[f.a], d.SelRef)
		tab.eval(`document.querySelector('#rows .exrow').click(), ''`)
		d = tab.waitFor("the row's node", func(d liveDOM) bool { return d.SelName == "b" })
		assert.Equal(t, []string{"✦b"}, d.Cur)
		assert.Equal(t, []string{refs[f.b]}, d.Cites)
		assert.Equal(t, refs[f.b], d.SelRef)
		assert.Empty(t, tab.errors())
	})

	t.Run("the banner says the server is gone and the content stays", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, seq1, nil)
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" })
		s.close()
		d := tab.waitFor("the banner", func(d liveDOM) bool { return !d.BannerHid })
		assert.Equal(t, "perf-agent-mcp is not running; this is the last state it sent. Ask your agent for a new page URL, or download a static copy while it runs.", d.Banner)
		assert.Equal(t, "b", d.SelName)
		assert.Equal(t, []string{"all", "main", "a", "✦b"}, d.Frames)
		assert.Equal(t, "first look at 3 frames", d.Title)
		assert.Empty(t, tab.errors())
	})

	t.Run("a refused stream falls back to polling the state", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, seq1, func(s *liveServer) { s.refuse = true })
		d := tab.waitFor("seq 1 and the note", func(d liveDOM) bool { return d.SelName == "b" && !d.BannerHid })
		assert.Equal(t, "quiet", d.BannerKind)
		assert.NotContains(t, d.Banner, "not running")
		s.setState(seq2)
		d = tab.waitFor("seq 2 by polling", func(d liveDOM) bool { return d.SelName == "c" })
		assert.Equal(t, "quiet", d.BannerKind)
		assert.Equal(t, 0, s.streamCount())
		assert.Empty(t, tab.errors())
	})

	t.Run("a diff publication is drawn and its nodes are shown", func(t *testing.T) {
		ld := newLiveDiff(t)
		s := newLiveServer(t)
		s.pubs["p4"] = ld.pub
		s.history["p4"] = []byte(`{"publication":"p4","version":"0","rows":[]}`)
		s.state = diffState("1", ld.ids["b"])
		s.sse = s.state
		tab := c.open(t, s.url)

		// b is not in the model: it comes from the frame route, by its
		// union ID, which carries a slash.
		d := tab.waitFor("b selected", func(d liveDOM) bool { return d.SelName == "b" })
		assert.Equal(t, []string{"all", "main", "a", "d"}, d.Frames)
		assert.Equal(t, string(ld.ids["b"]), d.SelID)
		assert.Equal(t, string(ld.ids["b"]), d.SelRef, "a node the model does not hold is named by its ID")
		assert.Contains(t, s.servedPaths(), "pub/p4/frame/"+string(ld.ids["b"]))
		facts := tab.eval(factsProbe)
		assert.Contains(t, facts, "-111 nanoseconds (-111 ns)|", "the panel shows the engine's change")
		assert.Contains(t, facts, "-11.1%|-0.111 ratio of view_total", "the share change has its exact digits in its title")
		assert.Contains(t, facts, "unavailable: no work count")
		refs := map[string]string{}
		for _, f := range ld.model["frames"].([]any) {
			fm := f.(map[string]any)
			refs[str(fm, "name")] = str(fm, "ref")
		}
		assert.Equal(t, []string{refs["main"] + " main", refs["a"] + " a", string(ld.ids["b"]) + " b"}, d.Path)
		assert.Equal(t, "b went down by 111", d.Title)
		assert.Equal(t, []string{"111"}, d.Marks, "the agent's title has its numbers marked")
		// Four frames are the budget of a static copy too, which draws six.
		assert.Empty(t, d.Export, "no link to an export that would be refused")
		assert.False(t, d.ExportHid)
		assert.Contains(t, tab.eval(`document.getElementById('export').title`), "draws 6 nodes, over its budget of no limit on bytes and 4 nodes")
		assert.Equal(t, "true", tab.eval(`document.getElementById('export').getAttribute('aria-disabled')`))
		assert.Equal(t, []string{"Diff of two views: every figure is computed by perf-agent"}, d.Pills)
		assert.Contains(t, d.Axis, "base 1000 nanoseconds (1.00 µs) · new 1000 nanoseconds (1.00 µs)")
		assert.True(t, tab.eval(`String(document.getElementById('noexp').hidden)`) == "true", "a diff has no explanation to miss")
		assert.Equal(t, "true", tab.eval(`String(document.getElementById('rows-sec').hidden)`))

		// The frames are coloured by the model's hue and tint.
		drawn := ld.model["drawn"].([]any)
		for i, want := range drawn {
			dm := want.(map[string]any)
			got := tab.eval(fmt.Sprintf(`(() => { const e = document.querySelectorAll('#chart .frame')[%d]; return e.style.getPropertyValue('--h') + ' ' + e.style.getPropertyValue('--t') + ' ' + e.className + ' ' + e.title; })()`, i))
			assert.Equal(t, str(dm, "hue")+" "+str(dm, "tint")+" frame "+str(dm, "cls")+" "+str(dm, "tip"), got, "frame %d", i)
		}

		// A drawn frame is selected locally, from the model.
		tab.eval(`[...document.querySelectorAll('#chart .frame')].find((e) => e.textContent === 'd').click(), ''`)
		d = tab.waitFor("d clicked", func(d liveDOM) bool { return d.SelName == "d" })
		assert.Equal(t, refs["d"], d.SelRef)
		assert.Equal(t, []string{"d"}, d.Cur)
		assert.Contains(t, tab.eval(factsProbe), "+111 nanoseconds (+111 ns)|")
		assert.Contains(t, tab.eval(factsProbe), "only_new|")

		// Focus draws a's subtree, b included, from the focus route.
		tab.eval(`[...document.querySelectorAll('#chart .frame')].find((e) => e.textContent === 'a').click(), ''`)
		tab.waitFor("a clicked", func(d liveDOM) bool { return d.SelName == "a" && !d.FocusOff })
		tab.eval(`document.getElementById('focus-btn').click(), ''`)
		d = tab.waitFor("a's subtree", func(d liveDOM) bool { return slices.Equal(d.Frames, []string{"a", "b"}) })
		assert.Contains(t, d.Axis, "widths relative to a")
		tab.eval(`document.getElementById('whole-btn').click(), ''`)
		tab.waitFor("the whole diff", func(d liveDOM) bool { return slices.Equal(d.Frames, []string{"all", "main", "a", "d"}) })

		lines := strings.Split(tab.waitText(`(document.getElementById('probe') || {}).textContent || ''`), "\n")
		require.Len(t, lines, 3)
		assert.Equal(t, "0", lines[1], "no content-security-policy violation")
		assert.Equal(t, 0, tab.dom().Violations)
		assert.Empty(t, tab.errors())
	})

	t.Run("a diff that fits offers its static copy", func(t *testing.T) {
		ld := newLiveDiff(t)
		whole, err := DiffLiveModel(ld.d, DiffLiveOptions{Title: "b went down by 111"})
		require.NoError(t, err)
		ld.pub.whole = whole
		s := newLiveServer(t)
		s.pubs["p4"] = ld.pub
		s.history["p4"] = []byte(`{"publication":"p4","version":"0","rows":[]}`)
		s.state = diffState("1", ld.ids["a"])
		s.sse = s.state
		tab := c.open(t, s.url)
		d := tab.waitFor("a selected", func(d liveDOM) bool { return d.SelName == "a" })
		assert.Equal(t, []string{"all", "main", "a", "b", "d", "c"}, d.Frames, "in pre-order, as the static copy nests them")
		assert.Equal(t, "pub/p4/export", d.Export)
		assert.False(t, d.ExportHid)
		assert.Empty(t, tab.eval(`document.getElementById('export').title`))
		assert.Equal(t, "null", tab.eval(`String(document.getElementById('export').getAttribute('aria-disabled'))`))
		assert.Empty(t, tab.errors())
	})

	t.Run("the waiting page before any publication", func(t *testing.T) {
		_, tab := newLivePage(t, c, f, liveState("0", "", "", ""), nil)
		tab.waitFor("the waiting text", func(d liveDOM) bool { return d.Waiting })
		time.Sleep(200 * time.Millisecond)
		d := tab.dom()
		assert.True(t, d.Waiting)
		assert.True(t, d.ExportHid)
		assert.Empty(t, d.Frames)
		assert.True(t, d.BannerHid)
		assert.Empty(t, tab.errors())
	})
}

func TestLivePageOrderingInChrome(t *testing.T) {
	c := startChrome(t)
	f := newLiveFixture(t)
	settle := func() { time.Sleep(1200 * time.Millisecond) }

	t.Run("a: a newer publication wins over a slower older one", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("0", "", "", ""), func(s *liveServer) {
			s.setDelay("pub/p1/model", 600*time.Millisecond)
			s.setDelay("pub/p1/history@0", 50*time.Millisecond)
			s.setHistory("p2", f.history("p2", "4", "a7"))
		})
		tab.waitFor("the waiting text", func(d liveDOM) bool { return d.Waiting })
		s.push(liveState("1", "p1", f.b, "", "p1"))
		time.Sleep(100 * time.Millisecond)
		s.push(liveState("2", "p2", f.c, "", "p1", "p2@4"))
		settle()
		d := tab.dom()
		assert.Equal(t, "second", d.Title)
		require.Len(t, d.Rows, 1)
		assert.Contains(t, d.Rows[0], "c")
		require.Len(t, d.Rejected, 1)
		assert.Contains(t, d.Rejected[0], "a7")
		assert.Equal(t, "c", d.SelName)
		assert.Equal(t, "pub/p2/export", d.Export)
		assert.Empty(t, tab.errors())
	})

	t.Run("b: a newer history wins over a slower older one", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1"), nil)
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" })
		s.setDelay("pub/p1/history@1", 500*time.Millisecond)
		s.setHistory("p1", f.history("p1", "1", "a1"))
		s.push(liveState("2", "p1", f.b, "", "p1@1"))
		time.Sleep(100 * time.Millisecond)
		s.setHistory("p1", f.history("p1", "2", "a1", "a3"))
		s.push(liveState("3", "p1", f.b, "", "p1@2"))
		settle()
		d := tab.dom()
		require.Len(t, d.Rejected, 2, "%v", d.Rejected)
		assert.Contains(t, d.Rejected[1], "a3")
		h := historyOf(t, f.history("p1", "2", "a1", "a3"))
		for _, k := range []string{"rejected_pill", "limit_pill"} {
			if p := str(h, k); p != "" {
				assert.Contains(t, d.Pills, p)
			}
		}
		assert.Empty(t, tab.errors())
	})

	t.Run("c: an agent state wins over a slower local focus", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("2", "p1", f.b, "", "p1"), func(s *liveServer) {
			s.setDelay("pub/p1/focus/"+string(f.a), 800*time.Millisecond)
		})
		tab.waitFor("seq 2", func(d liveDOM) bool { return d.SelName == "b" })
		tab.eval(`[...document.querySelectorAll('#chart .frame')].find((e) => e.textContent === 'a').click(), ''`)
		tab.waitFor("a clicked", func(d liveDOM) bool { return d.SelName == "a" && !d.FocusOff })
		tab.eval(`document.getElementById('focus-btn').click(), ''`)
		time.Sleep(200 * time.Millisecond)
		s.push(liveState("3", "p1", f.c, "", "p1"))
		settle()
		d := tab.dom()
		assert.Equal(t, "c", d.SelName)
		assert.Equal(t, []string{"all", "main", "a", "✦b"}, d.Frames, "the whole view, not a's focus")
		assert.NotContains(t, d.Axis, "relative to")
		assert.Empty(t, tab.errors())
	})

	t.Run("c: an agent state wins over a slower local frame", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("2", "p1", f.c, "", "p1"), func(s *liveServer) {
			s.delayOnce["pub/p1/frame/"+string(f.c)] = []time.Duration{0, 800 * time.Millisecond}
		})
		tab.waitFor("seq 2", func(d liveDOM) bool { return d.SelName == "c" })
		// c's own path entry: c is not in the model, so it is fetched again.
		tab.eval(`document.querySelector('#path button[data-node="` + string(f.c) + `"]').click(), ''`)
		time.Sleep(200 * time.Millisecond)
		s.push(liveState("3", "p1", f.a, "", "p1"))
		settle()
		d := tab.dom()
		assert.Equal(t, "a", d.SelName)
		assert.Equal(t, []string{"a"}, d.Cur)
		s.mu.Lock()
		assert.Equal(t, 2, countOf(s.served, "pub/p1/frame/"+string(f.c)), "the local selection fetched c again")
		s.mu.Unlock()
		assert.Empty(t, tab.errors())
	})

	t.Run("d: clearing the focus wins over a slower focus", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("3", "p1", f.b, "", "p1"), func(s *liveServer) {
			s.setDelay("pub/p1/focus/"+string(f.a), 800*time.Millisecond)
		})
		tab.waitFor("seq 3", func(d liveDOM) bool { return d.SelName == "b" })
		s.push(liveState("4", "p1", f.a, f.a, "p1"))
		time.Sleep(200 * time.Millisecond)
		s.push(liveState("5", "p1", f.a, "", "p1"))
		settle()
		d := tab.dom()
		assert.Equal(t, "a", d.SelName)
		assert.Equal(t, []string{"all", "main", "a", "✦b"}, d.Frames)
		assert.True(t, d.WholeOff)
		assert.Empty(t, tab.errors())
	})

	t.Run("focus draws the subtree and the whole view comes back", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("3", "p1", f.b, "", "p1"), nil)
		tab.waitFor("seq 3", func(d liveDOM) bool { return d.SelName == "b" })
		s.push(liveState("4", "p1", f.a, f.a, "p1"))
		d := tab.waitFor("a's focus", func(d liveDOM) bool { return strings.Contains(d.Axis, "relative to a") })
		assert.Equal(t, []string{"a", "✦b"}, d.Frames)
		assert.Equal(t, []string{"a"}, d.Cur)
		assert.Equal(t, modelGeometry(t, s.pubs["p1"].focusAt[string(f.a)]), d.FrameGeo)
		assert.Equal(t, "0% 100% 0px", d.FrameGeo[0], "a is drawn full width at the top")
		assert.False(t, d.WholeOff)
		frefs := modelRefs(t, s.pubs["p1"].focusAt[string(f.a)])
		assert.Equal(t, []string{frefs[f.main] + " main", frefs[f.a] + " a"}, d.Path, "refs are the focused model's")
		assert.Equal(t, "f0", frefs[f.a])
		tab.eval(`document.getElementById('whole-btn').click(), ''`)
		d = tab.waitFor("the whole view", func(d liveDOM) bool { return len(d.Frames) == 4 })
		assert.Equal(t, "a", d.SelName)
		assert.Equal(t, []string{"a"}, d.Cur)
		assert.Empty(t, tab.errors())
	})

	t.Run("e: the stream's newer state wins over an older fallback", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1"), nil)
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" })
		s.push(liveState("7", "p1", f.c, "", "p1"))
		tab.waitFor("seq 7", func(d liveDOM) bool { return d.SelName == "c" })
		before := s.gets()
		s.setState(liveState("6", "p1", f.a, "", "p1"))
		s.drop()
		settle()
		d := tab.dom()
		assert.Greater(t, s.gets(), before, "the page asked GET state while the stream was down")
		assert.Equal(t, "c", d.SelName)
		assert.True(t, d.BannerHid)
		assert.Empty(t, tab.errors())
	})

	t.Run("g: shown 5, accepting 7, a fallback 6 is refused", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("5", "p1", f.b, "", "p1"), func(s *liveServer) {
			s.setDelay("pub/p2/model", 800*time.Millisecond)
		})
		tab.waitFor("seq 5", func(d liveDOM) bool { return d.SelName == "b" })
		before := s.gets()
		s.setState(liveState("6", "p3", f.a, "", "p1", "p2", "p3"))
		s.push(liveState("7", "p2", f.c, "", "p1", "p2"))
		time.Sleep(200 * time.Millisecond)
		s.drop()
		settle()
		d := tab.dom()
		assert.Greater(t, s.gets(), before, "the fallback answered seq 6 while p2's model was loading")
		assert.Equal(t, "second", d.Title)
		assert.Equal(t, "c", d.SelName)
		assert.Equal(t, "pub/p2/export", d.Export)
		s.mu.Lock()
		assert.NotContains(t, s.served, "pub/p3/model", "seq 6 was never acted on")
		s.mu.Unlock()
		assert.Empty(t, tab.errors())
	})

	t.Run("f: seq 10 is newer than seq 9", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("9", "p1", f.b, "", "p1"), nil)
		tab.waitFor("seq 9", func(d liveDOM) bool { return d.SelName == "b" })
		s.push(liveState("10", "p1", f.a, "", "p1"))
		d := tab.waitFor("seq 10", func(d liveDOM) bool { return d.SelName == "a" })
		assert.Equal(t, []string{"a"}, d.Cur)
		assert.Empty(t, tab.errors())
	})

	t.Run("switching publication is local until the next state", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1", "p2"), nil)
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" && len(d.Pubs) == 2 })
		tab.eval(`document.querySelector('#pubs button[data-pub="p2"]').click(), ''`)
		d := tab.waitFor("p2", func(d liveDOM) bool { return d.Title == "second" })
		assert.Equal(t, "c", d.SelName, "p2's own selection")
		assert.Equal(t, "pub/p2/export", d.Export)
		s.push(liveState("2", "p1", f.a, "", "p1", "p2"))
		d = tab.waitFor("the agent's p1", func(d liveDOM) bool { return d.Title == "first look at 3 frames" && d.SelName == "a" })
		assert.Empty(t, tab.errors())
	})
}

func TestLivePageSteeringInChrome(t *testing.T) {
	c := startChrome(t)
	f := newLiveFixture(t)
	settle := func() { time.Sleep(1100 * time.Millisecond) }

	// The agent switches to p2 and comes back to p1 while p2's selection is
	// still loading: p2's model is already on screen, so the return must
	// draw p1 again.
	t.Run("a return after a partial publication switch", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1", "p2"), nil)
		tab.waitFor("p1 b", func(d liveDOM) bool { return d.SelName == "b" })
		s.setDelay("pub/p2/frame/"+string(f.b), 800*time.Millisecond)
		s.push(liveState("2", "p2", f.b, "", "p1", "p2"))
		tab.waitFor("p2's model", func(d liveDOM) bool { return d.Title == "second" })
		s.push(liveState("3", "p1", f.b, "", "p1", "p2"))
		settle()
		d := tab.dom()
		assert.Equal(t, "first look at 3 frames", d.Title)
		assert.Equal(t, "b", d.SelName)
		assert.Equal(t, "pub/p1/export", d.Export)
		assert.Empty(t, tab.errors())
	})

	t.Run("a return after a partial focus", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1"), nil)
		tab.waitFor("p1 b", func(d liveDOM) bool { return d.SelName == "b" })
		s.setDelay("pub/p1/frame/"+string(f.c), 800*time.Millisecond)
		s.push(liveState("2", "p1", f.c, f.a, "p1"))
		tab.waitFor("a's focus", func(d liveDOM) bool { return strings.Contains(d.Axis, "relative to a") })
		s.push(liveState("3", "p1", f.b, "", "p1"))
		settle()
		d := tab.dom()
		assert.NotContains(t, d.Axis, "relative to")
		assert.True(t, d.WholeOff)
		assert.Equal(t, "b", d.SelName)
		assert.Equal(t, []string{"✦b"}, d.Cur)
		assert.Empty(t, tab.errors())
	})

	// A state that changes only a history is quiet: the user's own focus,
	// still loading, lands, and the history moves.
	t.Run("a history-only state leaves a local focus alone", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("2", "p1", f.b, "", "p1"), func(s *liveServer) {
			s.setDelay("pub/p1/focus/"+string(f.b), 800*time.Millisecond)
		})
		tab.waitFor("seq 2", func(d liveDOM) bool { return d.SelName == "b" && !d.FocusOff })
		tab.eval(`document.getElementById('focus-btn').click(), ''`)
		time.Sleep(200 * time.Millisecond)
		s.setHistory("p1", f.history("p1", "1", "a1"))
		s.push(liveState("3", "p1", f.b, "", "p1@1"))
		settle()
		d := tab.dom()
		assert.Len(t, d.Rejected, 1, "the history moved")
		assert.Contains(t, d.Axis, "relative to b", "the user's focus landed")
		assert.Empty(t, tab.errors())
	})

	// While the agent's publication switch loads, the old frames are not
	// the user's to navigate: a click there does nothing.
	t.Run("a local click during an agent switch", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1"), func(s *liveServer) {
			s.setDelay("pub/p2/model", 800*time.Millisecond)
		})
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" })
		s.push(liveState("2", "p2", f.c, "", "p1", "p2"))
		time.Sleep(200 * time.Millisecond)
		tab.eval(`[...document.querySelectorAll('#chart .frame')].find((e) => e.textContent === 'a').click(), ''`)
		settle()
		d := tab.dom()
		assert.Equal(t, "second", d.Title, "the agent's publication is shown")
		assert.Equal(t, "c", d.SelName)
		assert.Empty(t, tab.errors())
	})

	// The user's explicit choice of publication still wins over a switch
	// that is loading.
	t.Run("choosing a publication during an agent switch", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1", "p3"), func(s *liveServer) {
			s.setDelay("pub/p2/model", 800*time.Millisecond)
		})
		tab.waitFor("seq 1", func(d liveDOM) bool { return d.SelName == "b" && len(d.Pubs) == 2 })
		s.push(liveState("2", "p2", f.c, "", "p1", "p2", "p3"))
		time.Sleep(200 * time.Millisecond)
		tab.eval(`document.querySelector('#pubs button[data-pub="p3"]').click(), ''`)
		settle()
		d := tab.dom()
		assert.Equal(t, "third", d.Title)
		assert.Empty(t, tab.errors())
	})

	t.Run("a publication that cannot be loaded is retried with a note", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p9", "", "", "p1"), nil)
		d := tab.waitFor("the note", func(d liveDOM) bool { return d.LoadNote != "" })
		assert.Equal(t, "perf-agent could not load this publication; the page keeps trying.", d.LoadNote)
		assert.True(t, d.BannerHid)
		tries := func() int {
			s.mu.Lock()
			defer s.mu.Unlock()
			return countOf(s.served, "pub/p9/model")
		}
		atNote := tries()
		assert.GreaterOrEqual(t, atNote, 3)
		time.Sleep(3 * time.Second)
		assert.LessOrEqual(t, tries()-atNote, 1, "after three failures the retries are seconds apart")
		s.push(liveState("2", "p1", f.b, "", "p1"))
		d = tab.waitFor("p1", func(d liveDOM) bool { return d.SelName == "b" })
		assert.Empty(t, d.LoadNote)
	})
}

// livePartialNote is what the page says once it stops retrying a state
// the user navigated away from.
const livePartialNote = "Your agent's last page could not be fully loaded."

// Retries after the note keep the generation, so they must not undo what
// the user chose since: a publication that loads but whose selection does
// not stays behind the user's own publication.
func TestLivePageRetriesInChrome(t *testing.T) {
	c := startChrome(t)
	f := newLiveFixture(t)
	frameC := "pub/p1/frame/" + string(f.c)

	t.Run("a retry after the note does not undo the user's publication", func(t *testing.T) {
		_, tab := newLivePage(t, c, f, liveState("1", "p1", f.c, "", "p1", "p2"), func(s *liveServer) {
			delete(s.pubs["p1"].frames, string(f.c))
		})
		tab.waitFor("the note", func(d liveDOM) bool { return d.LoadNote != "" })
		tab.eval(`document.querySelector('#pubs button[data-pub="p2"]').click(), ''`)
		tab.waitFor("p2", func(d liveDOM) bool { return d.Title == "second" })
		time.Sleep(7 * time.Second)
		d := tab.dom()
		assert.Equal(t, "second", d.Title, "the user's publication stays")
		assert.Equal(t, livePartialNote, d.LoadNote)
		assert.Empty(t, tab.errors())
	})

	t.Run("retries cut short by the user stop, and the note says so", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.c, "", "p1", "p2"), func(s *liveServer) {
			delete(s.pubs["p1"].frames, string(f.c))
			s.setDelay(frameC, 1200*time.Millisecond)
		})
		tab.waitFor("the note", func(d liveDOM) bool { return d.LoadNote != "" })
		tries := func() int {
			s.mu.Lock()
			defer s.mu.Unlock()
			return countOf(s.served, frameC)
		}
		n := tries()
		require.Eventually(t, func() bool { return tries() > n }, 15*time.Second, 50*time.Millisecond, "the next retry")
		time.Sleep(300 * time.Millisecond) // that retry's frame request is in flight
		tab.eval(`document.querySelector('#pubs button[data-pub="p2"]').click(), ''`)
		tab.waitFor("p2", func(d liveDOM) bool { return d.Title == "second" })
		at := tries()
		time.Sleep(11 * time.Second)
		d := tab.dom()
		assert.Equal(t, "second", d.Title)
		assert.Equal(t, at, tries(), "the page stopped retrying")
		assert.Equal(t, livePartialNote, d.LoadNote)
		assert.Empty(t, tab.errors())
	})

	// A newer agent state supersedes the one being retried; when the user
	// then cancels the newer one too, nothing is being retried, so the
	// page must not say it keeps trying.
	t.Run("a superseded retry note does not outlive its retries", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.c, "", "p1", "p2", "p3"), func(s *liveServer) {
			delete(s.pubs["p1"].frames, string(f.c))
			s.setDelay("pub/p2/model", 800*time.Millisecond)
		})
		tab.waitFor("the note", func(d liveDOM) bool { return d.LoadNote != "" })
		s.push(liveState("2", "p2", f.c, "", "p1", "p2", "p3"))
		time.Sleep(200 * time.Millisecond)
		tab.eval(`document.querySelector('#pubs button[data-pub="p3"]').click(), ''`)
		tab.waitFor("p3", func(d liveDOM) bool { return d.Title == "third" })
		time.Sleep(1200 * time.Millisecond)
		d := tab.dom()
		assert.Equal(t, "third", d.Title)
		assert.NotContains(t, d.LoadNote, "keeps trying", "no retry remains")
		assert.Empty(t, tab.errors())
	})
}

func TestLivePageConnectionInChrome(t *testing.T) {
	c := startChrome(t)
	f := newLiveFixture(t)

	// After a 503 the page polls; when the stream is accepted again the
	// "paused" note goes away and a poll already scheduled does not bring
	// it back.
	t.Run("the paused note goes when the stream is back", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1"), func(s *liveServer) { s.refuse = true })
		tab.waitFor("paused", func(d liveDOM) bool { return d.SelName == "b" && !d.BannerHid })
		s.mu.Lock()
		s.refuse = false
		s.mu.Unlock()
		require.Eventually(t, func() bool { return s.streamCount() > 0 }, 30*time.Second, 100*time.Millisecond, "the page tries the stream again")
		time.Sleep(6 * time.Second)
		d := tab.dom()
		assert.True(t, d.BannerHid, "the stream is open, so updates are not paused: %q", d.Banner)
		s.push(liveState("2", "p1", f.c, "", "p1"))
		d = tab.waitFor("seq 2 over the reopened stream", func(d liveDOM) bool { return d.SelName == "c" })
		assert.True(t, d.BannerHid)
		assert.Empty(t, tab.errors())
	})

	// A fallback GET state that fails after the stream came back and
	// delivered states says nothing about the server now.
	t.Run("a stale failed fallback does not mark a live server gone", func(t *testing.T) {
		s, tab := newLivePage(t, c, f, liveState("1", "p1", f.b, "", "p1"), nil)
		tab.waitFor("p1 b", func(d liveDOM) bool { return d.SelName == "b" })
		before := s.gets()
		s.setState(nil)
		s.setDelay("state", 5*time.Second)
		s.drop()
		require.Eventually(t, func() bool { return s.gets() > before }, 2*time.Second, 20*time.Millisecond)
		next := liveState("2", "p1", f.a, "", "p1")
		s.setState(next)
		s.push(next)
		tab.waitFor("reconnected with a new state", func(d liveDOM) bool { return d.SelName == "a" && d.BannerHid })
		time.Sleep(2300 * time.Millisecond)
		s.push(liveState("3", "p1", f.b, "", "p1"))
		d := tab.waitFor("still receiving", func(d liveDOM) bool { return d.SelName == "b" })
		assert.True(t, d.BannerHid, "an old failed fallback must not mark a recovered connection gone: %q", d.Banner)
		assert.Empty(t, tab.errors())
	})
}

func TestLiveFixtureLeavesCOut(t *testing.T) {
	f := newLiveFixture(t)
	live, _ := liveOf(t, f.v, f.results["p1"], LiveOptions{Title: "x", MaxNodes: 4})
	assert.NotContains(t, drawnIDs(t, live), string(f.c))
	for _, fr := range list(t, live["model"].(map[string]any), "frames") {
		assert.NotEqual(t, string(f.c), fr["id"], "c is not in the model")
	}
}

// modelRefs maps each frame of a live model to its ref.
func modelRefs(t *testing.T, raw []byte) map[profilemodel.NodeID]string {
	t.Helper()
	var m struct {
		Model struct {
			Frames []struct{ ID, Ref string } `json:"frames"`
		} `json:"model"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	out := map[profilemodel.NodeID]string{}
	for _, f := range m.Model.Frames {
		out[profilemodel.NodeID(f.ID)] = f.Ref
	}
	return out
}

// modelGeometry is each drawn frame's left, width and top as the model
// gives them, with their units.
func modelGeometry(t *testing.T, raw []byte) []string {
	t.Helper()
	var m struct {
		Drawn []struct{ Left, Width, Top string } `json:"drawn"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	var out []string
	for _, d := range m.Drawn {
		out = append(out, d.Left+"% "+d.Width+"% "+d.Top+"px")
	}
	return out
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}
