package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// A headless Chrome driven over --remote-debugging-pipe, in real time, on
// the real hub: the page, its event stream and its inbox are the hub's
// own, behind its front door and guards.

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

type chrome struct {
	w *os.File

	mu      sync.Mutex
	next    int
	waiting map[int]chan cdpMessage
	errs    []string // exceptions and console errors of every tab
	// paused are the requests a tab's Fetch interception holds, by
	// session, until continued.
	paused map[string][]string
	done   chan struct{}
}

func chromePath() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// startChrome starts a headless Chrome the test's cleanup closes, or
// skips the test when there is none.
func startChrome(t *testing.T) *chrome {
	t.Helper()
	path := chromePath()
	if path == "" || testing.Short() {
		t.Skip("no headless Chrome")
	}
	toChrome, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r, fromChrome, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--remote-debugging-pipe", "--user-data-dir="+filepath.Join(t.TempDir(), "profile"), "--window-size=1200,900", "about:blank")
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome} // fd 3: commands in, fd 4: replies out
	killWithTest(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	_ = toChrome.Close()
	_ = fromChrome.Close()
	c := &chrome{w: w, waiting: map[int]chan cdpMessage{}, paused: map[string][]string{}, done: make(chan struct{})}
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

func (c *chrome) read(r *bufio.Reader) {
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
			c.errs = append(c.errs, "exception: "+string(m.Params))
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.Type == "error" || p.Type == "assert" {
				c.errs = append(c.errs, "console: "+string(m.Params))
			}
		case "Fetch.requestPaused":
			var p struct {
				RequestID string `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			c.paused[m.SessionID] = append(c.paused[m.SessionID], p.RequestID)
		}
		c.mu.Unlock()
	}
}

func (c *chrome) call(session, method string, params any) (json.RawMessage, error) {
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

func (c *chrome) errors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.errs)
}

type chromeTab struct {
	c       *chrome
	t       *testing.T
	session string
}

// open loads url in a new tab, which the test's cleanup closes.
func (c *chrome) open(t *testing.T, url string) *chromeTab {
	t.Helper()
	decode := func(raw json.RawMessage, err error, v any) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	raw, err := c.call("", "Target.createTarget", map[string]any{"url": "about:blank"})
	decode(raw, err, &target)
	var att struct {
		SessionID string `json:"sessionId"`
	}
	raw, err = c.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true})
	decode(raw, err, &att)
	for _, m := range []string{"Runtime.enable", "Page.enable"} {
		if _, err := c.call(att.SessionID, m, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.call(att.SessionID, "Page.navigate", map[string]any{"url": url}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.call("", "Target.closeTarget", map[string]any{"targetId": target.TargetID}) })
	return &chromeTab{c: c, t: t, session: att.SessionID}
}

// eval runs expr in the tab and returns its string value.
func (tab *chromeTab) eval(expr string) string {
	tab.t.Helper()
	raw, err := tab.c.call(tab.session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	if err != nil {
		tab.t.Fatal(err)
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ExceptionDetails != nil {
		tab.t.Fatalf("%s: %v %s", expr, err, out.ExceptionDetails)
	}
	s, _ := out.Result.Value.(string)
	return s
}

// waitFor polls expr until cond holds on its value, and returns it.
func (tab *chromeTab) waitFor(what, expr string, cond func(string) bool) string {
	tab.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := tab.eval(expr)
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			tab.t.Fatalf("the page never showed %s; last %q; errors: %v", what, s, tab.c.errors())
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// holdRequests makes the tab hold every request whose URL matches
// pattern, a Fetch URL pattern, until releaseRequests.
func (tab *chromeTab) holdRequests(pattern string) {
	tab.t.Helper()
	if _, err := tab.c.call(tab.session, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": pattern}}}); err != nil {
		tab.t.Fatal(err)
	}
}

// heldRequests is how many requests the tab holds.
func (tab *chromeTab) heldRequests() int {
	tab.c.mu.Lock()
	defer tab.c.mu.Unlock()
	return len(tab.c.paused[tab.session])
}

// releaseRequests lets every held request go on, and holds no more.
func (tab *chromeTab) releaseRequests() {
	tab.t.Helper()
	tab.c.mu.Lock()
	ids := tab.c.paused[tab.session]
	delete(tab.c.paused, tab.session)
	tab.c.mu.Unlock()
	for _, id := range ids {
		if _, err := tab.c.call(tab.session, "Fetch.continueRequest", map[string]any{"requestId": id}); err != nil {
			tab.t.Error(err)
		}
	}
	if _, err := tab.c.call(tab.session, "Fetch.disable", map[string]any{}); err != nil {
		tab.t.Error(err)
	}
}

// The page's answers to an ask, by the inbox's status, as live.js words
// them.
const (
	askSent = "Sent. Ask your agent to look (it reads page_state)."
	askGone = "That frame is no longer on the page; nothing was sent."
)

// Probes of the page.
const (
	framesProbe = `[...document.querySelectorAll('#chart .frame')].map((e) => e.textContent).join(',')`
	selProbe    = `document.getElementById('sel-name').textContent + (document.getElementById('ask-btn').disabled ? '' : ' askable')`
	toastProbe  = `document.getElementById('toast').textContent`
	countProbe  = `(() => { const e = document.getElementById('inbox'); return (e && !e.hidden ? e.textContent : '') + ' @' + (document.body.dataset.rev || ''); })()`
)

// clickFrame clicks the drawn frame whose text is name.
func (tab *chromeTab) clickFrame(name string) {
	tab.t.Helper()
	tab.eval(`[...document.querySelectorAll('#chart .frame')].find((e) => e.textContent === '` + name + `').click(), ''`)
}

// pageState is the part of page_state's answer the test reads.
type pageState struct {
	User *struct {
		Publication, Selected, Name, Focus string
	} `json:"user"`
	Asks struct {
		Rows []struct {
			ID, Publication, Node, Name, View string
			ReadBy                             string `json:"read_by"`
		} `json:"rows"`
	} `json:"asks"`
}

func (c *httpClient) pageState() pageState {
	c.t.Helper()
	var s pageState
	text := c.call("page_state", map[string]any{})
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		c.t.Fatalf("page_state: %s (%v)", text, err)
	}
	return s
}

// inboxRev is the inbox revision of the envelope the hub serves at GET
// state.
func inboxRev(t *testing.T, pageURL string) string {
	t.Helper()
	status, body := httpGet(t, pageURL+"state")
	var env struct {
		Inbox struct {
			Rev string `json:"rev"`
		} `json:"inbox"`
	}
	if err := json.Unmarshal(body, &env); status != http.StatusOK || err != nil || env.Inbox.Rev == "" {
		t.Fatalf("GET state: %d %s (%v)", status, body, err)
	}
	return env.Inbox.Rev
}

// §D4 end to end, with the real hub: a frame the user selects in Chrome
// reaches page_state's user, and the "Ask my agent about this" chip puts
// it in the inbox, where page_state finds it with the frame's name. Both
// reports are live.js's own bodies, decoded by the hub's Inbox. While
// another session's tool holds the engine, the ask is answered, page_state
// reads it, and the envelope's inbox revision reaches a second tab's
// count. An ask about a publication the hub has since dropped is refused
// with 422, and the page says nothing was sent.
func TestAnAskReachesPageStateInChrome(t *testing.T) {
	c := startChrome(t)
	profiles := profilesDir(t)
	if err := os.WriteFile(filepath.Join(profiles, "new.pb.gz"), pmfixture.CPUBasicNew(), 0o644); err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(t.TempDir(), "hold")
	// One publication kept: the next drops the one on the page. A tool
	// that waits for the engine gives up in 5 s, so one that should not
	// wait and does fails quickly.
	p := startHub(t, hubOptions{profiles: profiles, env: []string{holdEnv + "=" + hold},
		args: []string{"--limit", "MaxPublications=1", "--limit", "MaxEngineWait=5s"}})
	// Whatever happens, the engine is let go before the hub is stopped.
	t.Cleanup(func() { _ = os.WriteFile(hold+".release", nil, 0o600) })

	agent := p.client("claude")
	o := agent.open("cpu.pb.gz")
	pub := agent.publish(o)
	nodeA := agent.nodeID(o, "a")
	pageURL := p.pageURL()

	tab := c.open(t, pageURL)
	other := c.open(t, pageURL)
	for _, tb := range []*chromeTab{tab, other} {
		tb.waitFor("the frames", framesProbe, func(s string) bool { return slices.Contains(strings.Split(s, ","), "a") })
	}

	// A select converges to page_state's user: live.js sends one report at
	// a time, the newest last, so the test waits for it rather than sleep.
	tab.clickFrame("a")
	tab.waitFor("a selected", selProbe, func(s string) bool { return s == "a askable" })
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := agent.pageState()
		if s.User != nil && s.User.Selected == nodeA {
			if s.User.Name != "a" || s.User.Publication != pub || s.User.Focus != "" {
				t.Errorf("page_state user = %+v, want a (%s) on %s, no focus", *s.User, nodeA, pub)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("page_state's user never became a: %+v", s.User)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Another session's top holds the engine from here.
	if err := os.WriteFile(hold+".tool", []byte("top"), 0o600); err != nil {
		t.Fatal(err)
	}
	busy := newHTTPClient(t, p.port, p.mcpToken())
	busy.initialize("codex")
	topDone := make(chan error, 1)
	go func() {
		a, err := busy.send("1", "tools/call", map[string]any{"name": "top", "arguments": map[string]any{"handle": o.handle, "view": o.view}})
		if err == nil {
			_, err = decode(a, "1")
		}
		topDone <- err
	}()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(hold + ".held"); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("top never held the engine")
		}
	}

	before := other.eval(countProbe)
	tab.eval(`document.getElementById('ask-btn').click(), ''`)
	if got := tab.waitFor("the ask's toast", toastProbe, func(s string) bool { return s != "" }); got != askSent {
		t.Errorf("toast %q, want %q", got, askSent)
	}
	// The ask's envelope reaches the other tab: its count, at the inbox
	// revision the hub serves.
	rev := inboxRev(t, pageURL)
	count := other.waitFor("the ask counted", countProbe, func(s string) bool { return s == "1 ask · 1 unread @"+rev })
	if count == before {
		t.Errorf("the other tab's count did not move: %q", count)
	}
	s := agent.pageState()
	if len(s.Asks.Rows) != 1 {
		t.Fatalf("page_state asks = %+v, want one", s.Asks.Rows)
	}
	if a := s.Asks.Rows[0]; a.Name != "a" || a.Node != nodeA || a.Publication != pub || a.View != o.view {
		t.Errorf("the ask = %+v, want a (%s) on %s, view %s", a, nodeA, pub, o.view)
	}
	other.waitFor("the ask read", countProbe, func(s string) bool { return strings.HasPrefix(s, "1 ask · 0 unread · read by claude @") })
	select {
	case err := <-topDone:
		t.Fatalf("top answered (%v) before the engine was let go: the test held nothing", err)
	default:
	}
	if err := os.WriteFile(hold+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-topDone; err != nil {
		t.Errorf("top after the engine was let go: %v", err)
	}

	// The agent publishes another profile, which drops pub. The tab holds
	// the new model's request, so it still shows pub, and an ask about a
	// is about a node the hub no longer has.
	tab.holdRequests("*/model")
	tab.eval(`document.getElementById('toast').textContent = '', ''`)
	next := agent.publish(agent.open("new.pb.gz"))
	for end := time.Now().Add(10 * time.Second); tab.heldRequests() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the tab never asked for the new publication's model")
		}
	}
	tab.eval(`document.getElementById('ask-btn').click(), ''`)
	if got := tab.waitFor("the stale ask's toast", toastProbe, func(s string) bool { return s != "" }); got != askGone {
		t.Errorf("toast %q, want %q", got, askGone)
	}
	if s := agent.pageState(); len(s.Asks.Rows) != 1 {
		t.Errorf("page_state asks after the stale ask = %+v, want still one", s.Asks.Rows)
	}
	tab.releaseRequests()
	tab.waitFor("the new publication", framesProbe, func(s string) bool { return slices.Contains(strings.Split(s, ","), "d") })
	if next == pub {
		t.Errorf("the new publication is %s again", next)
	}
	if errs := c.errors(); len(errs) != 0 {
		t.Errorf("the page reported errors: %v", errs)
	}
}
