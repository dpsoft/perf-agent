package perfpage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// published1 is a page with CPUBasic open and b explained in p1.
func published1(t *testing.T, edit func(*perfmcp.Config)) (*page, opened) {
	t.Helper()
	p := newPage(t, edit)
	p.openAndPublish()
	return p, p.c.open("cpu.pb.gz")
}

// liveRoutes are every route of the page with p1 published, relative to
// its URL.
func liveRoutes(o opened) []string {
	b, a := o.nodeID("b"), o.nodeID("a")
	return []string{"", "assets/live.css", "assets/live.js", "assets/panel.js", "state", "events",
		"pub/p1/model", "pub/p1/history", "pub/p1/frame/" + b, "pub/p1/focus/" + a, "pub/p1/export"}
}

func TestEveryRouteIsGuarded(t *testing.T) {
	p, o := published1(t, nil)
	base := p.url
	other := "http://" + p.host + "/s/" + strings.Repeat("A", 43) + "/"
	port := must(url.Parse(base))(t).Port()
	for _, route := range liveRoutes(o) {
		for _, c := range []struct {
			name   string
			method string
			url    string
			hdr    map[string]string
			want   int
		}{
			{"no token", http.MethodGet, "http://" + p.host + "/" + route, nil, http.StatusNotFound},
			{"another token", http.MethodGet, other + route, nil, http.StatusNotFound},
			{"evil Host", http.MethodGet, base + route, map[string]string{"Host": "evil.example"}, http.StatusForbidden},
			{"evil Host with port", http.MethodGet, base + route, map[string]string{"Host": "evil.example:" + port}, http.StatusForbidden},
			{"cross-site", http.MethodGet, base + route, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
			{"POST", http.MethodPost, base + route, nil, http.StatusMethodNotAllowed},
			{"DELETE", http.MethodDelete, base + route, nil, http.StatusMethodNotAllowed},
		} {
			what := fmt.Sprintf("%s %q", c.name, route)
			res, body := httpDo(t, c.method, c.url, c.hdr)
			wantStatus(t, what, res, body, c.want)
			if got := res.Header.Get("Content-Security-Policy"); got != livehttp.CSP {
				t.Errorf("%s: CSP %q", what, got)
			}
			noCORS(t, what, res)
		}

		method := http.MethodGet
		if route == "events" {
			// GET streams until the page closes; HEAD gets the same headers.
			method = http.MethodHead
		}
		res, body := httpDo(t, method, base+route, nil)
		wantStatus(t, route, res, body, http.StatusOK)
		wantCSP := livehttp.CSP
		if route == "pub/p1/export" {
			wantCSP = livehttp.ExportCSP
		}
		if got := res.Header.Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%q: CSP %q, want %q", route, got, wantCSP)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%q: X-Content-Type-Options %q", route, got)
		}
		noCORS(t, route, res)
		res, body = httpDo(t, http.MethodHead, base+route, nil)
		wantStatus(t, "HEAD "+route, res, body, http.StatusOK)
	}
}

func TestOnlyCleanPathsAreServed(t *testing.T) {
	p, o := published1(t, nil)
	base := p.url
	tokPrefix := strings.TrimSuffix(base, "/")
	for _, u := range []string{
		tokPrefix + "%2F",
		tokPrefix + "%2f",
		base + "%2F",
		base + "state%2F",
		base + "pub%2Fp1/model",
		base + "pub/p1%2Fmodel",
		base + "pub/p1/frame/" + o.nodeID("b") + "%2F",
		base + "./state",
		base + "pub/../state",
		base + "pub/p1/model/",
		base + "/state",
		base + "assets/",
		base + "assets/nope.js",
		base + "assets/live.css/",
		base + "assets/" + strings.Repeat("a", 65),
		base + "nope",
		base + "pub/p1",
		base + "pub/p1/",
		base + "pub/p1/frame/",
	} {
		res, body := httpGet(t, u, nil)
		wantStatus(t, u, res, body, http.StatusNotFound)
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q", u, loc)
		}
		if got := res.Header.Get("Content-Security-Policy"); got != livehttp.CSP {
			t.Errorf("%s: CSP %q", u, got)
		}
	}
	res, _ := httpGet(t, tokPrefix, nil)
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != strings.TrimPrefix(base, "http://"+res.Request.URL.Host) {
		t.Errorf("the token without its slash: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestStateAndModelServeThePublication(t *testing.T) {
	p, o := published1(t, nil)
	res, body := httpGet(t, p.url+"state", nil)
	wantStatus(t, "state", res, body, http.StatusOK)
	if got := res.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("state Content-Type %q", got)
	}
	env, err := envelopeOf(body)
	if err != nil {
		t.Fatalf("state %s: %v", body, err)
	}
	if st := env.Engine; st.Current != "p1" || st.Selected != o.nodeID("b") || len(st.Publications) != 1 {
		t.Errorf("state %+v", st)
	}
	if env.Inbox != (inboxJSON{Rev: "0", Asks: "0", Unread: "0"}) {
		t.Errorf("the inbox part %+v", env.Inbox)
	}
	v := p.srv.PageSnapshot()
	if !bytes.Equal(body, v.State()) {
		t.Errorf("state is not the snapshot's:\n%s\n%s", body, v.State())
	}

	model := must(v.Model("p1"))(t)
	res, body = httpGet(t, p.url+"pub/p1/model", nil)
	wantStatus(t, "model", res, body, http.StatusOK)
	if !bytes.Equal(body, model) {
		t.Error("the model route is not p1's model")
	}
	if got := res.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("model Content-Type %q", got)
	}
	if res.ContentLength != int64(len(model)) {
		t.Errorf("model Content-Length %d, want %d", res.ContentLength, len(model))
	}

	res, body = httpGet(t, p.url+"pub/p1/history", nil)
	wantStatus(t, "history", res, body, http.StatusOK)
	if !bytes.Equal(body, must(v.History("p1"))(t)) {
		t.Errorf("history route %s", body)
	}

	for _, u := range []string{"pub/p9/model", "pub/p9/history", "pub/p9/export", "pub/p9/frame/" + o.nodeID("b"),
		"pub/" + strings.Repeat("p", 65) + "/model"} {
		res, body = httpGet(t, p.url+u, nil)
		wantStatus(t, u, res, body, http.StatusNotFound)
		if string(body) != "no such publication or node; the page will refresh\n" {
			t.Errorf("%s: body %q", u, body)
		}
	}
}

func TestADiffPublicationHasAnEmptyHistory(t *testing.T) {
	p := newPage(t, nil)
	pub := p.diffPublished()
	res, body := httpGet(t, p.url+"pub/"+pub+"/history", nil)
	wantStatus(t, "diff history", res, body, http.StatusOK)
	if got, want := string(body), `{"publication":"p2","version":"0","rows":[]}`; got != want {
		t.Errorf("diff history %s, want %s", got, want)
	}
	if st := p.state(); st.Publications[1].Kind != "diff" || st.Publications[1].History != "0" || st.Publications[1].Digest != "" {
		t.Errorf("the diff's state entry %+v", st.Publications[1])
	}
}

func TestFrameAndFocusAreComputedByTheEngine(t *testing.T) {
	p, o := published1(t, func(c *perfmcp.Config) { c.Limits.MaxPageNodes = 1 })
	a, b, c := o.nodeID("a"), o.nodeID("b"), o.nodeID("c")
	v := p.srv.PageSnapshot()
	whole := modelOf(t, must(v.Model("p1"))(t))
	if slices.Contains(whole.drawnIDs(t), c) {
		t.Fatalf("c is drawn with MaxPageNodes=1: %v", whole.drawnIDs(t))
	}

	body := p.get("pub/p1/frame/" + c)
	if want := explainedFrame(t, pmfixture.CPUBasic(), shareClaim(o.nodeID("b"), "0.411", "b takes {c1}"), c); !bytes.Equal(body, want) {
		t.Errorf("frame route %s\nwant %s", body, want)
	}
	var fr struct {
		Frame struct {
			ID  string `json:"id"`
			Cum string `json:"cum"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(body, &fr); err != nil || fr.Frame.ID != c || fr.Frame.Cum != "489" {
		t.Errorf("frame %s: %v", body, err)
	}

	body = p.get("pub/p1/focus/" + a)
	if want := must(v.Focus("p1", a))(t); !bytes.Equal(body, want) {
		t.Error("the focus route is not the engine's model")
	}
	focused := modelOf(t, body)
	if d := focused.drawnIDs(t); len(d) == 0 || d[0] != a || focused.Focus != a {
		t.Errorf("focus on a draws %v, focus %q", d, focused.Focus)
	}

	// A subtree without the explained node: b stays in the model, undrawn.
	sibling := modelOf(t, p.get("pub/p1/focus/"+c))
	if d := sibling.drawnIDs(t); len(d) == 0 || d[0] != c || slices.Contains(d, b) || !sibling.has(b) {
		t.Errorf("focus on c draws %v with frames %v", d, sibling.frameIDs())
	}

	for _, u := range []string{"pub/p1/frame/0123456789abcdef", "pub/p1/focus/0123456789abcdef",
		"pub/p1/frame/" + strings.Repeat("0", 65), "pub/p1/focus/" + strings.Repeat("0", 65)} {
		res, body := httpGet(t, p.url+u, nil)
		wantStatus(t, u, res, body, http.StatusNotFound)
		if !strings.Contains(string(body), "no such publication or node; the page will refresh") {
			t.Errorf("%s: body %q", u, body)
		}
	}
}

func TestExportIsTheStaticPageAsADownload(t *testing.T) {
	p, o := published1(t, nil)
	text, isErr := o.c.try("page_explain", o.args(map[string]any{"explanations": shareClaim(o.nodeID("c"), "0.62", "c takes {c1}")}))
	if !isErr {
		t.Fatalf("the rejection: %s", text)
	}
	var rejected struct {
		Attempt string `json:"attempt"`
	}
	if err := json.Unmarshal([]byte(text), &rejected); err != nil || rejected.Attempt == "" {
		t.Fatalf("the rejection %s: %v", text, err)
	}

	res, body := httpGet(t, p.url+"pub/p1/export", nil)
	wantStatus(t, "export", res, body, http.StatusOK)
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="p1.html"` {
		t.Errorf("Content-Disposition %q", got)
	}
	if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); got != livehttp.ExportCSP {
		t.Errorf("CSP %q", got)
	}
	if res.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d for %d bytes", res.ContentLength, len(body))
	}
	if !bytes.HasPrefix(body, []byte("<!doctype html>")) {
		t.Errorf("export starts %.40q", body)
	}
	// It is the page perfmcp's own route served for the same calls before
	// the routes moved here.
	if want := must(os.ReadFile(filepath.Join("testdata", "explained-export.golden")))(t); !bytes.Equal(body, want) {
		t.Errorf("the export is %d bytes that are not the %d the route served before the move", len(body), len(want))
	}
	for _, text := range []string{"41.1%", rejected.Attempt, "Attempt a1"} {
		if !strings.Contains(string(body), text) {
			t.Errorf("the export lacks %q", text)
		}
	}
}

func TestAnExportOverTheBudgetIs422(t *testing.T) {
	p := newPage(t, func(c *perfmcp.Config) {
		c.Limits.MaxPageBytes = 16 << 10
		c.Limits.MaxRetainedBytes = 1 << 30
	})
	p.c.open("cpu.pb.gz").explain(nil)
	res, body := httpGet(t, p.url+"pub/p1/export", nil)
	wantStatus(t, "export", res, body, http.StatusUnprocessableEntity)
	if got := res.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if bytes.Contains(body, []byte("<")) || !bytes.Contains(body, []byte("16384")) {
		t.Errorf("422 body %q", body)
	}
	if res.Header.Get("Content-Disposition") != "" || res.Header.Get("Content-Security-Policy") != livehttp.CSP {
		t.Errorf("422 headers %v", res.Header)
	}
}

// An export that fails for a reason other than its size is a 500 that
// neither names the page limits nor offers a download.
func TestAnExportThatFailsOtherwiseSaysSo(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{errors.New("the page cannot be exported: no check"), http.StatusInternalServerError},
		{&perfmcp.PageLimitError{Msg: "the page is 9 bytes, over MaxPageBytes=8"}, http.StatusUnprocessableEntity},
		{perfmcp.ErrPageNotFound, http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		writeExport(rec, "p2", nil, c.err)
		if rec.Code != c.want || rec.Header().Get("Content-Disposition") != "" || rec.Header().Get("Content-Security-Policy") != "" {
			t.Errorf("%v: %d, headers %v", c.err, rec.Code, rec.Header())
		}
		if got, want := rec.Body.String(), c.err.Error()+"\n"; got != want {
			t.Errorf("%v: body %q", c.err, got)
		}
	}
}

// explainedFrame is node's frame in raw's default view explained by exps,
// as the engine computes it, built here apart from any server.
func explainedFrame(t *testing.T, raw []byte, exps []any, node string) []byte {
	t.Helper()
	prof := must(profilemodel.FromBytes(raw, nil, profilemodel.DefaultLimits()))(t)
	v := must(prof.View(profilemodel.ViewOptions{SampleIndex: -1}))(t)
	lim := perfmcp.DefaultLimits().Explain
	decoded, _, err := explain.Decode(must(json.Marshal(exps))(t), lim)
	if err != nil {
		t.Fatal(err)
	}
	r := explain.Check(v, decoded, lim)
	if !r.OK {
		t.Fatalf("the check failed: %+v", r.Failures)
	}
	return must(flamegraph.ExplainedLiveFrame(v, r, profilemodel.NodeID(node), prof.Digest.String()))(t)
}

// A server's page has one set of routes, and a server without a page has
// none: either is a programming error, and Routes panics.
func TestRoutesNeedAPageOfTheirOwn(t *testing.T) {
	panics := func(what string, f func()) {
		t.Helper()
		defer func() {
			r := recover()
			if s, ok := r.(string); !ok || !strings.HasPrefix(s, "perfpage.Routes: ") {
				t.Errorf("%s: recovered %v, want a perfpage.Routes panic", what, r)
			}
		}()
		f()
	}
	srv, err := perfmcp.New(perfmcp.Config{ProfilesDir: t.TempDir(), Limits: perfmcp.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	events := NewEvents(perfmcp.DefaultLimits())
	t.Cleanup(events.Close)
	panics("a server without Page", func() { Routes(srv, events) })

	p := newPage(t, nil)
	panics("a second set of routes", func() { Routes(p.srv, p.events) })
	p.get("state")
}

// eventsClient reads the page's stream over a connection of its own, so
// a test can set its deadlines.
type eventsClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func openEvents(t *testing.T, base string) *eventsClient {
	t.Helper()
	u := must(url.Parse(base))(t)
	conn := must(net.Dial("tcp", u.Host))(t)
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "GET %sevents HTTP/1.1\r\nHost: %s\r\n\r\n", u.Path, u.Host); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	return &eventsClient{conn: conn, br: bufio.NewReader(res.Body)}
}

// next is the engine part of the next state the stream sends.
func (c *eventsClient) next(t *testing.T) stateJSON {
	t.Helper()
	return c.nextEnvelope(t).Engine
}

// nextEnvelope is the next state envelope the stream sends.
func (c *eventsClient) nextEnvelope(t *testing.T) envelopeJSON {
	t.Helper()
	env, err := c.read()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return env
}

func (c *eventsClient) read() (envelopeJSON, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return envelopeJSON{}, err
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return envelopeOf([]byte(data))
		}
	}
}

// until reads states until one has seq.
func (c *eventsClient) until(t *testing.T, seq string) stateJSON {
	t.Helper()
	for {
		if st := c.next(t); st.Seq == seq {
			return st
		}
	}
}

func (c *eventsClient) close() { _ = c.conn.Close() }

func TestASecondTabAndAReconnectSeeTheCurrentState(t *testing.T) {
	p, o := published1(t, nil)
	seq := func() string { return p.state().Seq }
	one, two := openEvents(t, p.url), openEvents(t, p.url)
	for _, c := range []*eventsClient{one, two} {
		if st := c.next(t); st.Current != "p1" || st.Seq != seq() {
			t.Errorf("a new tab's first state %+v, want p1 at seq %s", st, seq())
		}
	}
	o.explain(nil)
	o.explain(shareClaim(o.nodeID("c"), "0.489", "c takes {c1}"))
	for _, c := range []*eventsClient{one, two} {
		if st := c.until(t, seq()); st.Current != "p3" || len(st.Publications) != 3 {
			t.Errorf("the tab converged to %+v", st)
		}
	}
	one.close()
	p.call("page_show", map[string]any{"publication": "p1"})
	again := openEvents(t, p.url)
	if st := again.next(t); st.Current != "p1" || st.Seq != seq() {
		t.Errorf("a reconnect's first state %+v, want p1 at seq %s", st, seq())
	}
	if st := two.until(t, seq()); st.Current != "p1" {
		t.Errorf("the open tab missed the switch: %+v", st)
	}

	// Once the page is opened, before any publication, the stream still
	// starts with a state.
	fresh := newPage(t, nil)
	fresh.call("page_open", map[string]any{})
	if st := openEvents(t, fresh.url).next(t); st.Current != "" || len(st.Publications) != 0 {
		t.Errorf("the first state before any publication %+v", st)
	}
}

func TestAStuckTabNeverDelaysATool(t *testing.T) {
	title := strings.Repeat("a long title ", 400)
	run := func(stuck bool) time.Duration {
		p := newPage(t, func(c *perfmcp.Config) { c.Limits.Explain.MaxTitleBytes = 8 << 10 })
		o := p.c.open("cpu.pb.gz")
		args := o.args(map[string]any{"explanations": shareClaim(o.nodeID("b"), "0.411", "b takes {c1}"), "title": title})
		p.call("page_open", map[string]any{})
		if stuck {
			d := net.Dialer{Control: smallReceiveBuffer}
			u := must(url.Parse(p.url))(t)
			conn := must(d.Dial("tcp", u.Host))(t)
			t.Cleanup(func() { _ = conn.Close() })
			if _, err := fmt.Fprintf(conn, "GET %sevents HTTP/1.1\r\nHost: %s\r\n\r\n", u.Path, u.Host); err != nil {
				t.Fatal(err)
			}
			for p.events.Clients() == 0 {
				time.Sleep(time.Millisecond)
			}
		}
		start := time.Now()
		for range 200 {
			p.call("page_explain", args)
		}
		return time.Since(start)
	}
	free := run(false)
	stuck := run(true)
	if stuck > free+time.Second {
		t.Errorf("200 publishes took %v with a stuck tab, %v without", stuck, free)
	}
}

func TestAnotherServersTokenIs404(t *testing.T) {
	a, b := newPage(t, nil), newPage(t, nil)
	ua, ub := must(url.Parse(a.url))(t), must(url.Parse(b.url))(t)
	for _, route := range []string{"", "state", "assets/live.js"} {
		res, body := httpGet(t, "http://"+ub.Host+ua.Path+route, nil)
		wantStatus(t, "B with A's token "+route, res, body, http.StatusNotFound)
		res, body = httpGet(t, "http://"+ub.Host+ub.Path+route, nil)
		wantStatus(t, "B with its token "+route, res, body, http.StatusOK)
	}
}

// The export and focus routes, the ones that build a page or a model per
// request, run at most heavySlots at a time; one more is told to retry.
func TestHeavyRoutesAreBounded(t *testing.T) {
	p, o := published1(t, nil)
	a, b := o.nodeID("a"), o.nodeID("b")
	for range cap(p.routes.heavy) {
		p.routes.heavy <- struct{}{}
	}
	for _, route := range []string{"pub/p1/export", "pub/p1/focus/" + a} {
		res, body := httpGet(t, p.url+route, nil)
		wantStatus(t, route+" while busy", res, body, http.StatusServiceUnavailable)
		if !bytes.Contains(body, []byte("busy")) || !bytes.Contains(body, []byte("retry")) || res.Header.Get("Retry-After") != "1" {
			t.Errorf("%s: 503 body %q, Retry-After %q", route, body, res.Header.Get("Retry-After"))
		}
		if got := res.Header.Get("Content-Security-Policy"); got != livehttp.CSP {
			t.Errorf("%s: 503 CSP %q", route, got)
		}
	}
	for _, route := range []string{"state", "pub/p1/model", "pub/p1/history", "pub/p1/frame/" + b} {
		p.get(route)
	}
	for range cap(p.routes.heavy) {
		<-p.routes.heavy
	}
	for _, route := range []string{"pub/p1/export", "pub/p1/focus/" + a} {
		p.get(route)
	}
	if cap(p.routes.heavy) != 2 {
		t.Errorf("%d heavy slots, want 2", cap(p.routes.heavy))
	}

	// Many at once: each answer is a whole page or a 503, and the slots
	// are all free afterwards.
	var wg sync.WaitGroup
	var ok, busy atomic.Int64
	for range 16 {
		wg.Go(func() {
			for range 5 {
				res, _ := httpGet(t, p.url+"pub/p1/export", nil)
				switch res.StatusCode {
				case http.StatusOK:
					ok.Add(1)
				case http.StatusServiceUnavailable:
					busy.Add(1)
				default:
					t.Errorf("export status %d", res.StatusCode)
				}
			}
		})
	}
	wg.Wait()
	if ok.Load() == 0 {
		t.Error("no export answered 200")
	}
	if n := len(p.routes.heavy); n != 0 {
		t.Errorf("%d slots still taken", n)
	}
	t.Logf("%d exports, %d told to retry", ok.Load(), busy.Load())
}

// A frame's path holds every caller's name, so a deep stack of long names
// makes a frame response as large as the stack. It is bounded like every
// other response the page builds: over MaxPageBytes it is refused with
// 422 and a reason, before any of it is sent.
func TestAFrameIsBoundedByMaxPageBytes(t *testing.T) {
	const depth, nameLen, budget = 300, 1000, 96 << 10
	frames := make([]pmfixture.Frame, depth)
	for i := range frames {
		s := fmt.Sprintf("f%04d_", i)
		frames[i] = pmfixture.Fn(s + strings.Repeat("n", nameLen-len(s)))
	}
	deep := pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: frames, Value: 10})
	shallow := pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: frames[:depth/3], Value: 10})
	p := newPage(t, func(c *perfmcp.Config) {
		c.Limits.MaxPageBytes = budget
		writeFixture(t, c.ProfilesDir, "shallow.pb.gz", shallow)
		writeFixture(t, c.ProfilesDir, "deep.pb.gz", deep)
	})
	pr := p.c.openPair("shallow.pb.gz", "deep.pb.gz")
	bottom := fmt.Sprintf("f%04d_", depth-1)
	bottom += strings.Repeat("n", nameLen-len(bottom))

	// An explained publication's frame, from the facts-only page of the
	// deep profile.
	out := pr.new.explain(nil)
	res, body := httpGet(t, p.url+"pub/"+out.Publication+"/frame/"+pr.new.nodeID(bottom), nil)
	wantStatus(t, "explained frame", res, body, http.StatusUnprocessableEntity)
	if !strings.Contains(string(body), fmt.Sprintf("MaxPageBytes=%d", budget)) || len(body) > 1<<10 {
		t.Errorf("the refusal says %.200q (%d bytes)", body, len(body))
	}
	mid := pr.new.nodeID("f0001_" + strings.Repeat("n", nameLen-len("f0001_")))
	p.get("pub/" + out.Publication + "/frame/" + mid)

	// A diff's frame of the same node, only in new.
	diff := pr.pageDiff(nil)
	res, body = httpGet(t, p.url+"pub/"+diff.Publication+"/frame/"+url.PathEscape(pr.unionID(bottom)), nil)
	wantStatus(t, "diff frame", res, body, http.StatusUnprocessableEntity)
	if !strings.Contains(string(body), fmt.Sprintf("MaxPageBytes=%d", budget)) {
		t.Errorf("the diff refusal says %.200q", body)
	}
}

// equalCPU is n equal cpu profiles, a.pb.gz, b.pb.gz, ….
func equalCPU(n int) map[string][]byte {
	out := map[string][]byte{}
	for i := range n {
		out[string(rune('a'+i))+".pb.gz"] = pmfixture.Build("cpu", "nanoseconds", nil,
			pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(fmt.Sprintf("f%d", i))}, Value: 10})
	}
	return out
}

// TestLivePageRacesNothing reads every route from eight goroutines while
// one session publishes and records rejections and another opens
// profiles.
func TestLivePageRacesNothing(t *testing.T) {
	p, o := published1(t, func(c *perfmcp.Config) {
		c.Limits.MaxOpenHandles = 4
		for name, raw := range equalCPU(10) {
			writeFixture(t, c.ProfilesDir, name, raw)
		}
	})
	b, a, cn := o.nodeID("b"), o.nodeID("a"), o.nodeID("c")
	explained := o.args(map[string]any{"explanations": shareClaim(b, "0.411", "b takes {c1}")})
	rejected := o.args(map[string]any{"explanations": shareClaim(cn, "0.62", "c takes {c1}")})

	var stop atomic.Bool
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := range 8 {
		wg.Go(func() {
			for !stop.Load() {
				_, body := httpGet(t, p.url+"state", nil)
				env, err := envelopeOf(body)
				if err != nil {
					t.Errorf("state %q: %v", body, err)
					return
				}
				st := env.Engine
				for _, route := range []string{"model", "history", "frame/" + b, "focus/" + a, "frame/" + a} {
					res, _ := httpGet(t, p.url+"pub/"+st.Current+"/"+route, nil)
					switch res.StatusCode {
					case http.StatusOK:
						ok.Add(1)
					case http.StatusNotFound:
					case http.StatusServiceUnavailable:
						if !strings.HasPrefix(route, "focus/") {
							t.Errorf("goroutine %d: %s is not bounded, but answered 503", i, route)
						}
					default:
						t.Errorf("goroutine %d: %s status %d", i, route, res.StatusCode)
					}
				}
			}
		})
	}
	opener := p.session()
	wg.Go(func() {
		for i := range 10 {
			x := opener.open(string(rune('a'+i)) + ".pb.gz")
			if i%3 == 0 {
				x.explain(nil)
			}
		}
	})
	for i := range 50 {
		p.call("page_explain", explained)
		if i < 20 {
			if text, isErr := o.c.try("page_explain", rejected); !isErr {
				t.Fatalf("the rejection: %s", text)
			}
		}
		time.Sleep(time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	if ok.Load() == 0 {
		t.Error("no route answered 200")
	}
}

// Every GET and HEAD leaves the server as it was: the same state, and the
// same answers to page_open and top.
func TestNoGETChangesServerState(t *testing.T) {
	p, o := published1(t, nil)
	if text, isErr := o.c.try("page_explain", o.args(map[string]any{"explanations": shareClaim(o.nodeID("c"), "0.62", "c takes {c1}")})); !isErr {
		t.Fatalf("the rejection: %s", text)
	}
	routes := liveRoutes(o)
	snapshot := func() (state []byte, opened, top string) {
		return bytes.Clone(p.srv.PageSnapshot().State()), p.call("page_open", map[string]any{}),
			p.call("top", o.args(map[string]any{"rows": "nodes", "limit": 200}))
	}
	state, opened, top := snapshot()
	for range 20 {
		for _, route := range routes {
			if route == "events" {
				ev := openEvents(t, p.url)
				ev.next(t)
				ev.close()
				continue
			}
			httpGet(t, p.url+route, nil)
			httpDo(t, http.MethodHead, p.url+route, nil)
		}
	}
	// The streams have closed by now; viewers counts none again.
	deadline := time.Now().Add(5 * time.Second)
	for p.events.Clients() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s2, o2, t2 := snapshot()
	if !bytes.Equal(state, s2) {
		t.Errorf("GETs changed the state:\n%s\n%s", state, s2)
	}
	if opened != o2 {
		t.Errorf("GETs changed page_open:\n%s\n%s", opened, o2)
	}
	if top != t2 {
		t.Errorf("GETs changed top:\n%s\n%s", top, t2)
	}
}

// page_open gives the URL the page is served at, and the shell is there.
func TestPageOpenGivesTheURLAndStartsOnce(t *testing.T) {
	p := newPage(t, nil)
	var first, second struct {
		URL string `json:"url"`
	}
	p.c.callJSON("page_open", map[string]any{}, &first)
	p.c.callJSON("page_open", map[string]any{}, &second)
	if first.URL != p.url || second.URL != p.url {
		t.Fatalf("page_open gave %q, then %q; the page is at %q", first.URL, second.URL, p.url)
	}
	res, body := httpGet(t, first.URL, nil)
	wantStatus(t, "shell", res, body, http.StatusOK)
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") || !bytes.HasPrefix(body, []byte("<!doctype html>")) {
		t.Errorf("shell Content-Type %q, body %.80q", res.Header.Get("Content-Type"), body)
	}
}

// viewers counts the page's open streams.
func TestViewersCountsOpenStreams(t *testing.T) {
	p := newPage(t, nil)
	pub := p.openAndPublish()
	one, two := openEvents(t, p.url), openEvents(t, p.url)
	one.next(t)
	two.next(t)
	var out published
	p.c.callJSON("page_show", map[string]any{"publication": pub}, &out)
	if out.Viewers != "2" {
		t.Errorf("viewers %q with two streams open", out.Viewers)
	}
	one.close()
	two.close()
	deadline := time.Now().Add(5 * time.Second)
	for p.events.Clients() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	p.c.callJSON("page_open", map[string]any{}, &out)
	if out.Viewers != "0" {
		t.Errorf("viewers %q after both streams closed", out.Viewers)
	}
}

// A diff's export is the download of the engine's static diff page.
func TestPageDiffExportIsTheM1DiffPage(t *testing.T) {
	p := newPage(t, nil)
	out := p.c.openPair("cpu.pb.gz", "new.pb.gz").pageDiff(map[string]any{"title": "two builds"})
	res, body := httpGet(t, p.url+"pub/"+out.Publication+"/export", nil)
	wantStatus(t, "export", res, body, http.StatusOK)
	if want := must(p.srv.PageSnapshot().Export(out.Publication))(t); !bytes.Equal(body, want) {
		t.Error("the export is not the engine's diff page")
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="p1.html"` {
		t.Errorf("Content-Disposition %q", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); got != livehttp.ExportCSP {
		t.Errorf("CSP %q", got)
	}
}

// wideDeepPair is the comparison of eight 400-frame chains with 1 KiB
// names: every frame but each chain's leaf is on both sides, and the leaf
// is new.
func wideDeepPair() (base, next []byte) {
	const chains, per, nameLen = 8, 400, 1 << 10
	var bs, ns []pmfixture.Sample
	for c := range chains {
		frames := make([]pmfixture.Frame, per)
		for i := range frames {
			s := fmt.Sprintf("c%02d_f%04d_", c, i)
			frames[i] = pmfixture.Fn(s + strings.Repeat("n", nameLen-len(s)))
		}
		bs = append(bs, pmfixture.Sample{Frames: frames[:per-1], Value: 10})
		ns = append(ns, pmfixture.Sample{Frames: frames[:per-1], Value: 10}, pmfixture.Sample{Frames: frames, Value: 10})
	}
	return pmfixture.Build("cpu", "nanoseconds", nil, bs...), pmfixture.Build("cpu", "nanoseconds", nil, ns...)
}

func TestPageDiffExportTooLargeIs422(t *testing.T) {
	const budget = 4 << 20
	base, next := wideDeepPair()
	p := newPage(t, func(c *perfmcp.Config) {
		c.Limits.MaxPageBytes = budget
		writeFixture(t, c.ProfilesDir, "wide.pb.gz", base)
		writeFixture(t, c.ProfilesDir, "wider.pb.gz", next)
	})
	out := p.c.openPair("wide.pb.gz", "wider.pb.gz").pageDiff(nil)
	_, err := p.srv.PageSnapshot().Export(out.Publication)
	if _, ok := errors.AsType[*perfmcp.PageLimitError](err); !ok {
		t.Fatalf("the engine's export: %v, want a page limit error", err)
	}
	res, body := httpGet(t, p.url+"pub/"+out.Publication+"/export", nil)
	wantStatus(t, "export", res, body, http.StatusUnprocessableEntity)
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type %q", got)
	}
	if got := res.Header.Get("Content-Disposition"); got != "" {
		t.Errorf("a refused export has Content-Disposition %q", got)
	}
	if string(body) != err.Error()+"\n" {
		t.Errorf("body %q, want %q", body, err.Error()+"\n")
	}
}

// The page fetches a union node with its "/" escaped; only that escape,
// between two node IDs, on a diff's frame and focus routes, is served.
func TestOnlyAUnionIDMayBeEscaped(t *testing.T) {
	p := newPage(t, nil)
	pr := p.c.openPair("cpu.pb.gz", "new.pb.gz")
	b, d := pr.unionID("b"), pr.unionID("d")
	pr.pageDiff(nil)
	pr.base.explain(nil)
	base := p.url
	bBase, bNew, _ := strings.Cut(b, "/")
	dBase, dNew, _ := strings.Cut(d, "/")
	if dBase != "-" {
		t.Fatalf("d is new only, its union ID is %s", d)
	}
	for _, route := range []string{"pub/p1/frame/" + bBase + "%2F" + bNew, "pub/p1/focus/" + bBase + "%2F" + bNew, "pub/p1/frame/-%2F" + dNew} {
		res, body := httpGet(t, base+route, nil)
		wantStatus(t, route, res, body, http.StatusOK)
	}
	plain := pr.base.nodeID("b")
	for _, route := range []string{
		"pub/p1/frame/" + bBase + "%2f" + bNew,
		"pub/p1/frame/" + bBase + "%2F" + bNew + "%2F" + bNew,
		"pub/p1/frame/" + bBase + "%2F",
		"pub/p1/frame/%2F" + bNew,
		"pub/p1/frame/" + bBase + "%2F" + strings.ToUpper(bNew),
		"pub/p1/frame/" + bBase + "%2Fx" + bNew[1:],
		"pub/p1/frame/" + bBase + "%2F" + bNew + "0",
		"pub/p1/frame/%2D%2F" + dNew,
		"pub/p1/frame/" + bBase + "/" + bNew,
		"pub/p1/frame/" + bBase + "%252F" + bNew,
		"pub/p1/model%2F" + bNew,
		"pub/p1/history/" + bBase + "%2F" + bNew,
		"pub/p1/export/" + bBase + "%2F" + bNew,
		"pub/p%31/frame/" + bBase + "%2F" + bNew,
		"pub%2Fp1/frame/" + bBase + "%2F" + bNew,
		"pub/p1/frame/" + plain,
		"pub/p2/frame/" + bBase + "%2F" + bNew,
		"pub/p2/focus/" + bBase + "%2F" + bNew,
		"pub/p1/frame/0000000000000000%2F" + bNew,
	} {
		res, body := httpGet(t, base+route, nil)
		wantStatus(t, route, res, body, http.StatusNotFound)
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q", route, loc)
		}
	}
}

// NewEvents admits MaxPageClients streams: one more is refused and polls.
func TestNewEventsAdmitsMaxPageClients(t *testing.T) {
	p := newPage(t, func(c *perfmcp.Config) { c.Limits.MaxPageClients = 2 })
	p.call("page_open", map[string]any{})
	openEvents(t, p.url).next(t)
	openEvents(t, p.url).next(t)
	res, body := httpGet(t, p.url+"events", nil)
	wantStatus(t, "a third stream", res, body, http.StatusServiceUnavailable)
}

// must(f())(t) is f's value, failing the test if f returned an error.
func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(fmt.Errorf("unexpected error: %w", err))
		}
		return v
	}
}
