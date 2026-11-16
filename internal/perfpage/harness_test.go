package perfpage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// page is a perfmcp server with the live page engine, its events hub,
// and the page served by Routes behind the guard on a loopback test
// listener, driven by an MCP client over pipes as an agent would.
type page struct {
	t      *testing.T
	srv    *perfmcp.Server
	events *livehttp.Hub
	routes *routes
	// url is the page's URL, below the token, with its slash; host is its
	// host:port.
	url, host string
	// profiles is the profiles root; it holds cpu.pb.gz (CPUBasic) and
	// new.pb.gz (CPUBasicNew), and whatever edit wrote.
	profiles string
	c        *client
}

// newPage starts a page. edit, when set, changes the config before the
// server is built; it may write fixtures into ProfilesDir.
func newPage(t *testing.T, edit func(*perfmcp.Config)) *page {
	t.Helper()
	cfg := perfmcp.Config{ProfilesDir: t.TempDir(), Limits: perfmcp.DefaultLimits(), Version: "test"}
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	writeFixture(t, cfg.ProfilesDir, "new.pb.gz", pmfixture.CPUBasicNew())
	tok, err := livehttp.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	// The handler is set before the listener serves anything.
	var handler http.Handler
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	host := hs.Listener.Addr().String()
	url := "http://" + host + "/s/" + string(tok) + "/"

	var events *livehttp.Hub
	cfg.Page = &perfmcp.PageConfig{
		URL: url,
		// The state is one line of JSON, the only data Publish takes.
		Notify:  func(state []byte) { _ = events.Publish(state) },
		Viewers: func() int { return events.Clients() },
	}
	if edit != nil {
		edit(&cfg)
	}
	events = NewEvents(cfg.Limits)
	srv, err := perfmcp.New(cfg)
	if err != nil {
		hs.Close()
		t.Fatalf("New: %v", err)
	}
	rt := newRoutes(srv, events)
	handler = livehttp.Guard(tok, rt)
	hs.Config = livehttp.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }), nil)
	hs.Start()
	p := &page{t: t, srv: srv, events: events, routes: rt, url: url, host: host, profiles: cfg.ProfilesDir}
	// Cleanups run last first: the sessions end, then the streams, the
	// listener and the server.
	t.Cleanup(func() {
		events.Close()
		hs.Close()
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	p.c = p.session()
	return p
}

func writeFixture(t *testing.T, dir, name string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// client is one MCP session of a page's server over in-process pipes. A
// session runs one call at a time.
type client struct {
	t      *testing.T
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int
}

// session opens another MCP session on p's server, initialized, which
// ends with the test. The engine serializes the tools of every session;
// a session's end cancels the capture jobs, which these tests never
// start.
func (p *page) session() *client {
	t := p.t
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := p.srv.Serve(t.Context(), sr, sw)
		_ = sw.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	c := &client{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), nextID: 1}
	c.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	return c
}

// rpc sends a request and returns its result, failing the test on a
// protocol error.
func (c *client) rpc(method string, params any) json.RawMessage {
	c.t.Helper()
	id := c.nextID
	c.nextID++
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.w.Write(append(req, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("read: %v", err)
		}
		var res struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &res); err != nil {
			c.t.Fatalf("a line that is not one JSON-RPC message: %s: %v", line, err)
		}
		if res.ID == nil || *res.ID != id {
			continue
		}
		if res.Error != nil {
			c.t.Fatalf("%s: protocol error %s", method, res.Error.Message)
		}
		return res.Result
	}
}

// try runs a tool and returns its text and isError.
func (c *client) try(tool string, args any) (text string, isError bool) {
	c.t.Helper()
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	raw := c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Content) != 1 {
		c.t.Fatalf("%s: result %s (%v)", tool, raw, err)
	}
	return out.Content[0].Text, out.IsError
}

// call runs a tool that must succeed and returns its text.
func (c *client) call(tool string, args any) string {
	c.t.Helper()
	text, isErr := c.try(tool, args)
	if isErr {
		c.t.Fatalf("%s %v: %s", tool, args, text)
	}
	return text
}

// callJSON runs a tool that must succeed and decodes its text into v.
func (c *client) callJSON(tool string, args, v any) {
	c.t.Helper()
	text := c.call(tool, args)
	if err := json.Unmarshal([]byte(text), v); err != nil {
		c.t.Fatalf("%s: decode %s: %v", tool, text, err)
	}
}

// call runs a tool on the page's first session.
func (p *page) call(tool string, args any) string {
	p.t.Helper()
	return p.c.call(tool, args)
}

// opened is a profile opened on a session, at its default view.
type opened struct {
	c            *client
	handle, view string
}

// open opens path on c.
func (c *client) open(path string) opened {
	c.t.Helper()
	var out struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	c.callJSON("open", map[string]any{"path": path}, &out)
	return opened{c: c, handle: out.Handle, view: out.DefaultView}
}

func (o opened) args(extra map[string]any) map[string]any {
	m := map[string]any{"handle": o.handle, "view": o.view}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// nodeID is the ID of the first node row named name.
func (o opened) nodeID(name string) string {
	o.c.t.Helper()
	var out struct {
		Rows []struct {
			Node string `json:"node"`
			Name string `json:"name"`
		} `json:"rows"`
	}
	o.c.callJSON("top", o.args(map[string]any{"rows": "nodes", "limit": 200}), &out)
	for _, r := range out.Rows {
		if r.Name == name {
			return r.Node
		}
	}
	o.c.t.Fatalf("no node named %q", name)
	return ""
}

// shareClaim is one explanation of node with a share_of_total claim of
// value, written as JSON text, and prose that places it.
func shareClaim(node, value, prose string) []any {
	return []any{map[string]any{
		"node": node,
		"claims": []any{map[string]any{
			"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": json.RawMessage(value),
		}},
		"prose": prose,
	}}
}

// published is what page_explain and page_diff answer.
type published struct {
	Attempt     string `json:"attempt"`
	Status      string `json:"status"`
	Publication string `json:"publication"`
	URL         string `json:"url"`
	Viewers     string `json:"viewers"`
}

// explain publishes explanations of o's view and returns the answer.
func (o opened) explain(exps []any) published {
	o.c.t.Helper()
	if exps == nil {
		exps = []any{}
	}
	var out published
	o.c.callJSON("page_explain", o.args(map[string]any{"explanations": exps}), &out)
	return out
}

// openAndPublish opens cpu.pb.gz and publishes b of its default view,
// explained, as p1.
func (p *page) openAndPublish() (pub string) {
	p.t.Helper()
	o := p.c.open("cpu.pb.gz")
	out := o.explain(shareClaim(o.nodeID("b"), "0.411", "b takes {c1}"))
	if out.Publication != "p1" || out.URL != p.url {
		p.t.Fatalf("page_explain: %+v", out)
	}
	return out.Publication
}

// pair is base and new opened on one session.
type pair struct {
	c         *client
	base, new opened
}

func (c *client) openPair(base, next string) pair {
	c.t.Helper()
	return pair{c: c, base: c.open(base), new: c.open(next)}
}

func (p pair) args(extra map[string]any) map[string]any {
	m := map[string]any{"base_handle": p.base.handle, "base_view": p.base.view, "new_handle": p.new.handle, "new_view": p.new.view}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// unionID is the union ID diff gives the node named name.
func (p pair) unionID(name string) string {
	p.c.t.Helper()
	var out struct {
		Rows []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"rows"`
	}
	p.c.callJSON("diff", p.args(map[string]any{"limit": 500}), &out)
	for _, r := range out.Rows {
		if r.Name == name {
			return r.ID
		}
	}
	p.c.t.Fatalf("diff has no node %q", name)
	return ""
}

// pageDiff publishes the comparison and returns the answer.
func (p pair) pageDiff(extra map[string]any) published {
	p.c.t.Helper()
	var out published
	p.c.callJSON("page_diff", p.args(extra), &out)
	return out
}

// diffPublished opens the CPUBasic pair and publishes, after p1 from
// openAndPublish, their comparison as p2.
func (p *page) diffPublished() (pub string) {
	p.t.Helper()
	p.openAndPublish()
	out := p.c.openPair("cpu.pb.gz", "new.pb.gz").pageDiff(nil)
	if out.Publication != "p2" {
		p.t.Fatalf("page_diff: %+v", out)
	}
	return out.Publication
}

// stateJSON is the page's state.
type stateJSON struct {
	Seq          string `json:"seq"`
	Current      string `json:"current"`
	Selected     string `json:"selected"`
	Focus        string `json:"focus"`
	Publications []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Digest  string `json:"digest"`
		History string `json:"history"`
	} `json:"publications"`
}

// inboxJSON is the inbox part of the state envelope.
type inboxJSON struct {
	Rev    string `json:"rev"`
	Asks   string `json:"asks"`
	Unread string `json:"unread"`
	ReadBy string `json:"read_by"`
}

// envelopeJSON is the state envelope the state route and every event
// carry: the engine's state and the inbox's, each at its own revision.
type envelopeJSON struct {
	Engine stateJSON `json:"engine"`
	Inbox  inboxJSON `json:"inbox"`
}

// envelopeOf decodes an envelope, which must have exactly its two parts.
func envelopeOf(body []byte) (envelopeJSON, error) {
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(body, &parts); err != nil {
		return envelopeJSON{}, err
	}
	if len(parts) != 2 || parts["engine"] == nil || parts["inbox"] == nil {
		return envelopeJSON{}, fmt.Errorf("not an envelope of engine and inbox: %.200s", body)
	}
	var env envelopeJSON
	err := json.Unmarshal(body, &env)
	return env, err
}

// envelope is the page's state envelope as a browser reads it.
func (p *page) envelope() envelopeJSON {
	p.t.Helper()
	res, body := httpGet(p.t, p.url+"state", nil)
	wantStatus(p.t, "state", res, body, http.StatusOK)
	env, err := envelopeOf(body)
	if err != nil {
		p.t.Fatalf("state %s: %v", body, err)
	}
	return env
}

// state is the engine part of the page's state as a browser reads it.
func (p *page) state() stateJSON {
	p.t.Helper()
	return p.envelope().Engine
}

// get is a route of the page that must answer 200.
func (p *page) get(route string) []byte {
	p.t.Helper()
	res, body := httpGet(p.t, p.url+route, nil)
	wantStatus(p.t, route, res, body, http.StatusOK)
	return body
}

// httpGet requests url with the headers hdr ("Host" sets the request's
// Host) and returns the response with its body read.
func httpGet(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	return httpDo(t, http.MethodGet, url, hdr)
}

// httpDo is httpGet with another method. It follows no redirect and keeps
// no connection open. Any goroutine may call it: an error is reported with
// Errorf, and the response is then empty, with status 0.
func httpDo(t *testing.T, method, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Errorf("%s %s: %v", method, url, err)
		return &http.Response{Header: http.Header{}}, nil
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, url, err)
		return &http.Response{Header: http.Header{}}, nil
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Errorf("%s %s: reading the body: %v", method, url, err)
	}
	return res, body
}

func wantStatus(t *testing.T, what string, res *http.Response, body []byte, want int) {
	t.Helper()
	if res.StatusCode != want {
		t.Errorf("%s: status %d, want %d; body %.200q", what, res.StatusCode, want, body)
	}
}

func noCORS(t *testing.T, what string, res *http.Response) {
	t.Helper()
	for k := range res.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s: response has %s", what, k)
		}
	}
}

// liveModelJSON is the part of a live model these tests read.
type liveModelJSON struct {
	Drawn []struct {
		I string `json:"i"`
	} `json:"drawn"`
	Focus string `json:"focus"`
	Model struct {
		Frames []struct {
			ID string `json:"id"`
		} `json:"frames"`
	} `json:"model"`
}

func modelOf(t *testing.T, raw []byte) liveModelJSON {
	t.Helper()
	var m liveModelJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("model %.200s: %v", raw, err)
	}
	return m
}

func (m liveModelJSON) drawnIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, d := range m.Drawn {
		var i int
		if _, err := fmt.Sscan(d.I, &i); err != nil || i < 0 || i >= len(m.Model.Frames) {
			t.Fatalf("drawn index %q", d.I)
		}
		out = append(out, m.Model.Frames[i].ID)
	}
	return out
}

func (m liveModelJSON) frameIDs() []string {
	var out []string
	for _, f := range m.Model.Frames {
		out = append(out, f.ID)
	}
	return out
}

func (m liveModelJSON) has(id string) bool { return slices.Contains(m.frameIDs(), id) }
