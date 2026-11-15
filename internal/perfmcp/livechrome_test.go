package perfmcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// A headless Chrome driven over --remote-debugging-pipe, in real time: the
// page keeps an event stream open, so a virtual-time --dump-dom would
// never return. It loads the page from the server's real listener, guard
// and routes.

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
	done    chan struct{}
}

func chromePath() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

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
	c := &chrome{w: w, waiting: map[int]chan cdpMessage{}, done: make(chan struct{})}
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

// open loads url in a new tab.
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

// waitText polls expr until it returns a non-empty string.
func (tab *chromeTab) waitText(what, expr string) string {
	tab.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s := tab.eval(expr); s != "" {
			return s
		}
		if time.Now().After(deadline) {
			tab.t.Fatalf("the page never showed %s; errors: %v", what, tab.c.errors())
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// A diff's node the model leaves out is fetched by the page, its union ID
// escaped, through the server's own routes, and shown.
func TestADiffNodeOutsideTheModelIsShownInChrome(t *testing.T) {
	c := startChrome(t)
	p := openLivePair(t, func(c *Config) { c.Limits.MaxPageNodes = 4 }, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	b := p.unionID(t, "b")
	out, _ := p.pageDiff(t, map[string]any{"node": b})
	if m := diffModelOf(t, []byte(pageGet(t, out.URL, "pub/p1/model"))); slices.Contains(m.frameIDs(), b) {
		t.Fatalf("b is in the model with MaxPageNodes=4")
	}
	tab := c.open(t, out.URL)
	const probe = `(() => {
  const $ = (id) => document.getElementById(id);
  if (document.readyState !== 'complete' || !$('sel-name') || $('sel-name').textContent !== 'b') return '';
  return JSON.stringify({
    id: $('sel-id').textContent,
    frames: [...document.querySelectorAll('#chart .frame')].map((e) => e.textContent),
    path: [...document.querySelectorAll('#path button .pn')].map((e) => e.textContent),
    facts: $('facts').children.length,
    notes: $('notes').getAttribute('aria-label'),
  });
})()`
	var got struct {
		ID     string   `json:"id"`
		Frames []string `json:"frames"`
		Path   []string `json:"path"`
		Facts  int      `json:"facts"`
		Notes  string   `json:"notes"`
	}
	if err := json.Unmarshal([]byte(tab.waitText("b selected", probe)), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != b || slices.Contains(got.Frames, "b") || got.Facts == 0 || !slices.Equal(got.Path, []string{"main", "a", "b"}) {
		t.Errorf("the page shows %+v", got)
	}
	if got.Notes != "Comparison notes" {
		t.Errorf("the notes are labelled %q on a diff", got.Notes)
	}
	if errs := c.errors(); len(errs) != 0 || strings.Contains(tab.eval(`document.body.textContent`), "no such publication") {
		t.Errorf("the page reported errors: %v", errs)
	}
}

// A browser opens at most six connections to one host and port, and every
// tab's event stream holds one. With six tabs open on the page, the server
// still lets every tab follow a new publication: the tabs beyond
// MaxPageClients are refused a stream and poll, which leaves connections
// free for every tab's fetches.
func TestSixTabsAllFollowThePageInChrome(t *testing.T) {
	c := startChrome(t)
	srv := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, srv, "cpu.pb.gz", "")
	url := pageOpen(t, srv).URL
	const tabs = 6
	var open []*chromeTab
	for range tabs {
		open = append(open, c.open(t, url))
	}
	// Every tab has loaded and has its stream or its refusal.
	streams := strconv.Itoa(min(tabs, DefaultLimits().MaxPageClients))
	deadline := time.Now().Add(10 * time.Second)
	for pageOpen(t, srv).Viewers != streams {
		if time.Now().After(deadline) {
			t.Fatalf("viewers %s with %d tabs open; want %s", pageOpen(t, srv).Viewers, tabs, streams)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)

	var pub pageExplainOut
	srv.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &pub)
	seq := pageState(t, url).Seq
	probe := `(() => document.body && document.body.dataset.seq === ` + strconv.Quote(seq) + ` && document.getElementById('waiting').hidden ? 'ok' : '')()`
	deadline = time.Now().Add(15 * time.Second)
	for i, tab := range open {
		for tab.eval(probe) == "" {
			if time.Now().After(deadline) {
				t.Fatalf("tab %d of %d never showed seq %s: it shows %q", i+1, tabs, seq, tab.eval(`String(document.body && document.body.dataset.seq)`))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if errs := c.errors(); len(errs) != 0 {
		t.Errorf("the tabs reported errors: %v", errs)
	}
}
