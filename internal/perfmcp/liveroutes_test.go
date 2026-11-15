package perfmcp

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// lockedBuffer is a log destination the request goroutine and the HTTP
// server's logger may write at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// livePublished is a live server with CPUBasic open and b explained in p1,
// and its page's URL.
func livePublished(t *testing.T, edit func(*Config)) (*Server, liveOpened, *publication, string) {
	t.Helper()
	s := newLiveServer(t, edit, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	base := liveURL(t, s)
	o := openLive(t, s, "cpu.pb.gz")
	p := o.publish(t, s, o.explained(t, s, "b", "0.411"), "")
	return s, o, p, base
}

func wantStatus(t *testing.T, what string, res *http.Response, body []byte, want int) {
	t.Helper()
	if res.StatusCode != want {
		t.Errorf("%s: status %d, want %d; body %.200q", what, res.StatusCode, want, body)
	}
}

func TestTheLiveURLIsLoopbackWithAToken(t *testing.T) {
	var logs lockedBuffer
	s := newLiveServer(t, func(c *Config) { c.Log = slog.New(slog.NewTextHandler(&logs, nil)) }, nil)
	u := liveURL(t, s)
	if !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/s/[A-Za-z0-9_-]{43}/$`).MatchString(u) {
		t.Fatalf("live URL %q", u)
	}
	if again := liveURL(t, s); again != u {
		t.Errorf("a second ensureLive gave %q, then %q", u, again)
	}
	parsed := must(url.Parse(u))(t)
	tok := strings.Trim(strings.TrimPrefix(parsed.Path, "/s/"), "/")
	out := logs.String()
	if !strings.Contains(out, "live page listening") || !strings.Contains(out, parsed.Host) {
		t.Errorf("the log does not say where the page listens:\n%s", out)
	}
	if strings.Contains(out, tok) {
		t.Errorf("the log holds the token:\n%s", out)
	}
	if n := strings.Count(out, "live page listening"); n != 1 {
		t.Errorf("listening logged %d times", n)
	}
	res, body := httpGet(t, u, nil)
	wantStatus(t, "shell", res, body, http.StatusOK)
	if !bytes.HasPrefix(body, []byte("<!doctype html>")) {
		t.Errorf("shell body %.80q", body)
	}
}

func TestEnsureLiveNeedsTheFlag(t *testing.T) {
	s := must(New(testConfig(t)))(t)
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.ensureLive(); err == nil || !strings.Contains(err.Error(), "--live") {
		t.Errorf("ensureLive without Live: %v", err)
	}
}

// liveRoutes are every route of the live page with p1 published, relative
// to the page's URL.
func liveRoutes(t *testing.T, o liveOpened) []string {
	b, a := string(o.node(t, "b")), string(o.node(t, "a"))
	return []string{"", "assets/live.css", "assets/live.js", "assets/panel.js", "state", "events",
		"pub/p1/model", "pub/p1/history", "pub/p1/frame/" + b, "pub/p1/focus/" + a, "pub/p1/export"}
}

func noCORS(t *testing.T, what string, res *http.Response) {
	t.Helper()
	for k := range res.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s: response has %s", what, k)
		}
	}
}

func TestEveryRouteIsGuarded(t *testing.T) {
	_, o, _, base := livePublished(t, nil)
	parsed := must(url.Parse(base))(t)
	other := "http://" + parsed.Host + "/s/" + strings.Repeat("A", 43) + "/"
	for _, route := range liveRoutes(t, o) {
		for _, c := range []struct {
			name   string
			method string
			url    string
			hdr    map[string]string
			want   int
		}{
			{"no token", http.MethodGet, "http://" + parsed.Host + "/" + route, nil, http.StatusNotFound},
			{"another token", http.MethodGet, other + route, nil, http.StatusNotFound},
			{"evil Host", http.MethodGet, base + route, map[string]string{"Host": "evil.example"}, http.StatusForbidden},
			{"evil Host with port", http.MethodGet, base + route, map[string]string{"Host": "evil.example:" + parsed.Port()}, http.StatusForbidden},
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
	_, o, _, base := livePublished(t, nil)
	tokPrefix := strings.TrimSuffix(base, "/")
	for _, u := range []string{
		tokPrefix + "%2F",
		tokPrefix + "%2f",
		base + "%2F",
		base + "state%2F",
		base + "pub%2Fp1/model",
		base + "pub/p1%2Fmodel",
		base + "pub/p1/frame/" + string(o.node(t, "b")) + "%2F",
		base + "./state",
		base + "pub/../state",
		base + "pub/p1/model/",
		base + "/state",
		base + "assets/",
		base + "assets/nope.js",
		base + "assets/live.css/",
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
	s, o, p, base := livePublished(t, nil)
	res, body := httpGet(t, base+"state", nil)
	wantStatus(t, "state", res, body, http.StatusOK)
	if got := res.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("state Content-Type %q", got)
	}
	var st stateJSON
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("state %s: %v", body, err)
	}
	if st.Current != "p1" || st.Selected != string(o.node(t, "b")) || len(st.Publications) != 1 {
		t.Errorf("state %+v", st)
	}
	if !bytes.Equal(body, s.live.snapshot().state) {
		t.Errorf("state is not the snapshot's:\n%s\n%s", body, s.live.snapshot().state)
	}

	res, body = httpGet(t, base+"pub/p1/model", nil)
	wantStatus(t, "model", res, body, http.StatusOK)
	if !bytes.Equal(body, p.model) {
		t.Error("the model route is not p1's model")
	}
	if got := res.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("model Content-Type %q", got)
	}
	if res.ContentLength != int64(len(p.model)) {
		t.Errorf("model Content-Length %d, want %d", res.ContentLength, len(p.model))
	}

	res, body = httpGet(t, base+"pub/p1/history", nil)
	wantStatus(t, "history", res, body, http.StatusOK)
	if !bytes.Equal(body, s.live.snapshot().history["p1"].json) {
		t.Errorf("history route %s", body)
	}

	for _, u := range []string{"pub/p9/model", "pub/p9/history", "pub/p9/export", "pub/p9/frame/" + string(o.node(t, "b")),
		"pub/" + strings.Repeat("p", 65) + "/model"} {
		res, body = httpGet(t, base+u, nil)
		wantStatus(t, u, res, body, http.StatusNotFound)
		if !strings.Contains(string(body), "no such publication or node; the page will refresh") {
			t.Errorf("%s: body %q", u, body)
		}
	}
}

func TestADiffPublicationHasAnEmptyHistory(t *testing.T) {
	s, _, _, base := livePublished(t, nil)
	// No tool publishes a diff yet; a publication of that kind with no
	// history is what one will be.
	l := s.live
	d := &publication{id: "p2", n: 2, kind: pubDiff, titleSegs: json.RawMessage(`[]`), model: []byte(`{}`)}
	l.pubs = append(l.pubs, d)
	l.current = "p2"
	s.commit()
	res, body := httpGet(t, base+"pub/p2/history", nil)
	wantStatus(t, "diff history", res, body, http.StatusOK)
	if got, want := string(body), `{"publication":"p2","version":"0","rows":[]}`; got != want {
		t.Errorf("diff history %s, want %s", got, want)
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

func TestFrameAndFocusAreComputedByTheEngine(t *testing.T) {
	_, o, p, base := livePublished(t, func(c *Config) { c.Limits.MaxPageNodes = 1 })
	a, b, c := string(o.node(t, "a")), string(o.node(t, "b")), string(o.node(t, "c"))
	var whole liveModelJSON
	if err := json.Unmarshal(p.model, &whole); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(whole.drawnIDs(t), c) {
		t.Fatalf("c is drawn with MaxPageNodes=1: %v", whole.drawnIDs(t))
	}

	res, body := httpGet(t, base+"pub/p1/frame/"+c, nil)
	wantStatus(t, "frame c", res, body, http.StatusOK)
	want := must(flamegraph.ExplainedLiveFrame(p.v, p.result, o.node(t, "c"), o.e.prof.Digest.String()))(t)
	if !bytes.Equal(body, want) {
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

	res, body = httpGet(t, base+"pub/p1/focus/"+a, nil)
	wantStatus(t, "focus a", res, body, http.StatusOK)
	var focused liveModelJSON
	if err := json.Unmarshal(body, &focused); err != nil {
		t.Fatal(err)
	}
	if d := focused.drawnIDs(t); len(d) == 0 || d[0] != a || focused.Focus != a {
		t.Errorf("focus on a draws %v, focus %q", d, focused.Focus)
	}

	// A subtree without the explained node: b stays in the model, undrawn.
	res, body = httpGet(t, base+"pub/p1/focus/"+c, nil)
	wantStatus(t, "focus c", res, body, http.StatusOK)
	var sibling liveModelJSON
	if err := json.Unmarshal(body, &sibling); err != nil {
		t.Fatal(err)
	}
	if d := sibling.drawnIDs(t); len(d) == 0 || d[0] != c || slices.Contains(d, b) || !slices.Contains(sibling.frameIDs(), b) {
		t.Errorf("focus on c draws %v with frames %v", d, sibling.frameIDs())
	}

	for _, u := range []string{"pub/p1/frame/0123456789abcdef", "pub/p1/focus/0123456789abcdef",
		"pub/p1/frame/" + strings.Repeat("0", 65), "pub/p1/focus/" + strings.Repeat("0", 65)} {
		res, body = httpGet(t, base+u, nil)
		wantStatus(t, u, res, body, http.StatusNotFound)
		if !strings.Contains(string(body), "no such publication or node; the page will refresh") {
			t.Errorf("%s: body %q", u, body)
		}
	}
}

func TestExportIsTheStaticPageAsADownload(t *testing.T) {
	s, o, p, base := livePublished(t, nil)
	rejected := o.reject(t, s, "c")
	if !s.refreshHistory() {
		t.Fatal("the rejection did not reach p1's history")
	}
	s.commit()

	res, body := httpGet(t, base+"pub/p1/export", nil)
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

	// It is the page render_flamegraph writes for the same check and record.
	recorded, dropped, incomplete := s.attempts.rejected(o.e.prof, o.view)
	var want bytes.Buffer
	if err := flamegraph.WriteExplainedPage(&want, o.v, p.result, flamegraph.ExplainedPageOptions{
		ProfileName: "cpu.pb.gz", Digest: o.e.prof.Digest.String(), Attempt: "a0",
		Rejected: recorded, DroppedRejected: dropped, HistoryIncomplete: incomplete,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want.Bytes()) {
		t.Error("the export is not the static page of the publication and its current record")
	}
	for _, text := range []string{"41.1%", rejected, "Attempt a0"} {
		if !strings.Contains(string(body), text) {
			t.Errorf("the export lacks %q", text)
		}
	}
}

func TestTheExportShowsOnlyWhatTheHistoryKeeps(t *testing.T) {
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxPageBytes = 256 << 10 }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	base := liveURL(t, s)
	o := openLive(t, s, "cpu.pb.gz")
	p := o.facts(t, s)
	r := manyClaimsRejected(t, s, o, "b", 32)
	for range 20 {
		s.attempts.add(o.e.prof, o.view, "", outcomeRejected, r.Failures)
	}
	s.refreshHistory()
	s.commit()
	h := s.live.snapshot().history[p.id]
	kept := 0
	for _, ra := range h.rejected {
		kept += len(ra.Failures)
	}
	if kept == 0 || kept == 20*32 {
		t.Fatalf("the history keeps %d of %d failures; the test needs it trimmed", kept, 20*32)
	}

	res, body := httpGet(t, base+"pub/p1/export", nil)
	wantStatus(t, "export", res, body, http.StatusOK)
	want := must(exportExplained(p, h))(t)
	if !bytes.Equal(body, want) {
		t.Error("the export is not built from the snapshot's history")
	}
	recorded, dropped, incomplete := s.attempts.rejected(o.e.prof, o.view)
	var full bytes.Buffer
	if err := flamegraph.WriteExplainedPage(&full, o.v, p.result, flamegraph.ExplainedPageOptions{
		ProfileName: "cpu.pb.gz", Digest: o.e.prof.Digest.String(), Attempt: "a0",
		Rejected: recorded, DroppedRejected: dropped, HistoryIncomplete: incomplete,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(body, full.Bytes()) {
		t.Error("the export shows the whole record, not what the history keeps")
	}
	page := string(body)
	if !strings.Contains(page, "640 claims rejected at render in 20 attempts") {
		t.Error("the export's pill does not count every claim and attempt")
	}
	if !strings.Contains(page, "more failure") && !strings.Contains(page, "not kept") {
		t.Error("the export does not say what it left out")
	}
}

func TestAnExportOverTheBudgetIs422(t *testing.T) {
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxPageBytes = 16 << 10
		c.Limits.MaxRetainedBytes = 1 << 30
	}, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	base := liveURL(t, s)
	o := openLive(t, s, "cpu.pb.gz")
	o.facts(t, s)
	res, body := httpGet(t, base+"pub/p1/export", nil)
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

// TestExportReadsOnlyTheSnapshot exports from four goroutines while the
// request goroutine changes p1's record, evicts its view's record and
// finally evicts p1: every answer is a whole page of one committed
// history, a 422, or, once p1 is gone, a 404.
func TestExportReadsOnlyTheSnapshot(t *testing.T) {
	profiles := equalCPU(3)
	profiles["cpu.pb.gz"] = pmfixture.CPUBasic()
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxAttemptViews = 2
		c.Limits.MaxPublications = 2
	}, profiles)
	base := liveURL(t, s)
	o := openLive(t, s, "cpu.pb.gz")
	p := o.publish(t, s, o.explained(t, s, "b", "0.411"), "")

	var histories []*liveHistory
	record := func() {
		if h := s.live.snapshot().history["p1"]; h != nil && !slices.Contains(histories, h) {
			histories = append(histories, h)
		}
	}
	record()

	var stop atomic.Bool
	var wg sync.WaitGroup
	var mu sync.Mutex
	bodies := map[[32]byte]int{}
	gone := false
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				res, body := httpGet(t, base+"pub/p1/export", nil)
				mu.Lock()
				switch res.StatusCode {
				case http.StatusOK:
					if res.ContentLength != int64(len(body)) || !bytes.HasSuffix(bytes.TrimSpace(body), []byte("</html>")) {
						t.Errorf("a partial export: %d bytes of %d", len(body), res.ContentLength)
					}
					bodies[sha256.Sum256(body)]++
				case http.StatusUnprocessableEntity:
				case http.StatusNotFound:
					if !gone {
						t.Error("404 while p1 is kept")
					}
				default:
					t.Errorf("export status %d", res.StatusCode)
				}
				mu.Unlock()
			}
		})
	}
	for i := range 50 {
		o.reject(t, s, "c")
		if i%10 == 9 {
			s.attempts.add(o.e.prof, fmt.Sprintf("other/view#%d", i), "", outcomeRejected, nil)
			s.attempts.add(o.e.prof, fmt.Sprintf("other/view#%d", i+100), "", outcomeRejected, nil)
		}
		s.refreshHistory()
		s.commit()
		record()
		time.Sleep(time.Millisecond)
	}
	for _, name := range []string{"a.pb.gz", "b.pb.gz"} {
		x := openLive(t, s, name)
		if name == "b.pb.gz" {
			mu.Lock()
			gone = true
			mu.Unlock()
		}
		x.facts(t, s)
	}
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if _, ok := s.live.snapshot().pubs["p1"]; ok {
		t.Fatal("p1 was not evicted")
	}
	known := map[[32]byte]bool{}
	for _, h := range histories {
		known[sha256.Sum256(must(exportExplained(p, h))(t))] = true
	}
	if len(bodies) < 2 {
		t.Fatalf("the exports saw %d histories; the test needs them to race the commits", len(bodies))
	}
	for sum, n := range bodies {
		if !known[sum] {
			t.Errorf("%d exports are no committed history's page", n)
		}
	}
	if len(histories) < 10 {
		t.Errorf("only %d histories committed", len(histories))
	}
}

// liveCounters are what a GET must never change.
type liveCounters struct {
	next, entries int
	seq           int64
	snap          *liveSnapshot
}

func countersOf(s *Server) liveCounters {
	return liveCounters{next: s.attempts.next, entries: len(s.store.entries), seq: s.live.seq, snap: s.live.snapshot()}
}

func TestNoGETChangesServerState(t *testing.T) {
	s, o, _, base := livePublished(t, nil)
	o.reject(t, s, "c")
	s.refreshHistory()
	s.commit()
	before, dump := countersOf(s), dumpLive(s)
	for range 20 {
		for _, route := range liveRoutes(t, o) {
			if route == "events" {
				ev := openEvents(t, base)
				ev.next(t)
				ev.close()
				continue
			}
			httpGet(t, base+route, nil)
			httpDo(t, http.MethodHead, base+route, nil)
		}
	}
	if after := countersOf(s); after != before {
		t.Errorf("GETs changed the server: %+v, then %+v", before, after)
	}
	if after := dumpLive(s); !equalDumps(dump, after) {
		t.Error("GETs changed the live state")
	}
}

func equalDumps(a, b liveDump) bool {
	return slices.Equal(a.pubs, b.pubs) && a.next == b.next && a.seq == b.seq && a.snap == b.snap &&
		a.current == b.current && a.selected == b.selected && a.focus == b.focus && a.total == b.total &&
		len(a.history) == len(b.history) && len(a.objs) == len(b.objs) && len(a.held) == len(b.held)
}

// eventsClient reads the live page's stream over a connection of its own,
// so a test can set its deadlines.
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

// next is the next state the stream sends.
func (c *eventsClient) next(t *testing.T) stateJSON {
	t.Helper()
	st, err := c.read()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return st
}

func (c *eventsClient) read() (stateJSON, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return stateJSON{}, err
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var st stateJSON
			err := json.Unmarshal([]byte(data), &st)
			return st, err
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
	s, o, _, base := livePublished(t, nil)
	seq := func() string { return fmt.Sprint(s.live.snapshot().seq) }
	one, two := openEvents(t, base), openEvents(t, base)
	for _, c := range []*eventsClient{one, two} {
		if st := c.next(t); st.Current != "p1" || st.Seq != seq() {
			t.Errorf("a new tab's first state %+v, want p1 at seq %s", st, seq())
		}
	}
	o.facts(t, s)
	o.publish(t, s, o.explained(t, s, "c", "0.489"), "")
	for _, c := range []*eventsClient{one, two} {
		if st := c.until(t, seq()); st.Current != "p3" || len(st.Publications) != 3 {
			t.Errorf("the tab converged to %+v", st)
		}
	}
	one.close()
	if err := s.live.show("p1", "", false); err != nil {
		t.Fatal(err)
	}
	s.commit()
	again := openEvents(t, base)
	if st := again.next(t); st.Current != "p1" || st.Seq != seq() {
		t.Errorf("a reconnect's first state %+v, want p1 at seq %s", st, seq())
	}
	if st := two.until(t, seq()); st.Current != "p1" {
		t.Errorf("the open tab missed the switch: %+v", st)
	}

	// Before any publication the stream still starts with a state.
	fresh := newLiveServer(t, nil, nil)
	if st := openEvents(t, liveURL(t, fresh)).next(t); st.Current != "" || len(st.Publications) != 0 {
		t.Errorf("the first state before any publication %+v", st)
	}
}

func TestAStuckTabNeverDelaysATool(t *testing.T) {
	run := func(stuck bool) time.Duration {
		s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
		base := liveURL(t, s)
		o := openLive(t, s, "cpu.pb.gz")
		r := o.explained(t, s, "b", "0.411")
		if stuck {
			u := must(url.Parse(base))(t)
			d := net.Dialer{Control: smallReceiveBuffer}
			conn := must(d.Dial("tcp", u.Host))(t)
			t.Cleanup(func() { _ = conn.Close() })
			if _, err := fmt.Fprintf(conn, "GET %sevents HTTP/1.1\r\nHost: %s\r\n\r\n", u.Path, u.Host); err != nil {
				t.Fatal(err)
			}
			for s.live.hub.Clients() == 0 {
				time.Sleep(time.Millisecond)
			}
		}
		start := time.Now()
		for range 200 {
			if _, err := s.publishExplained(o.e, o.v, o.view, r, strings.Repeat("a long title ", 400), "a0"); err != nil {
				t.Fatal(err)
			}
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
	a := newLiveServer(t, nil, nil)
	b := newLiveServer(t, nil, nil)
	ua, ub := must(url.Parse(liveURL(t, a)))(t), must(url.Parse(liveURL(t, b)))(t)
	for _, route := range []string{"", "state", "assets/live.js"} {
		res, body := httpGet(t, "http://"+ub.Host+ua.Path+route, nil)
		wantStatus(t, "B with A's token "+route, res, body, http.StatusNotFound)
		res, body = httpGet(t, "http://"+ub.Host+ub.Path+route, nil)
		wantStatus(t, "B with its token "+route, res, body, http.StatusOK)
	}
}

func TestCloseStopsTheListener(t *testing.T) {
	s, _, _, base := livePublished(t, nil)
	host := must(url.Parse(base))(t).Host
	ev := openEvents(t, base)
	ev.next(t)
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The streams end first, so Shutdown finds nothing to wait for.
	if took := time.Since(start); took >= liveShutdown/2 {
		t.Errorf("Close took %v with only a stream open", took)
	}
	if _, err := ev.read(); err == nil {
		t.Error("an open stream goes on after Close")
	}
	if conn, err := net.DialTimeout("tcp", host, time.Second); err == nil {
		_ = conn.Close()
		t.Error("the port still accepts connections after Close")
	}
	if _, err := s.ensureLive(); err == nil {
		t.Error("ensureLive started the page again after Close")
	}
}

// TestCloseIsBoundedByStuckClients closes a server with a tab that never
// reads its stream and a download that never reads its body: Close
// returns within its bound, and both connections end.
func TestCloseIsBoundedByStuckClients(t *testing.T) {
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxPageBytes, c.Limits.MaxPageNodes = 64<<20, 1<<20 }, map[string][]byte{"wide.pb.gz": pmfixture.Wide(20000)})
	base := liveURL(t, s)
	o := openLive(t, s, "wide.pb.gz")
	p := o.facts(t, s)
	u := must(url.Parse(base))(t)
	d := net.Dialer{Control: smallReceiveBuffer}
	stuck := func(route string) net.Conn {
		conn := must(d.Dial("tcp", u.Host))(t)
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Path, route, u.Host); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	stream := stuck("events")
	download := stuck("pub/" + p.id + "/model")
	for s.live.hub.Clients() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// Shutdown waits its whole grace only for a response still being
	// written: the download is stuck, as the test needs.
	if took < liveShutdown*4/5 {
		t.Fatalf("Close took %v: the %d-byte download was not stuck", took, len(p.model))
	}
	if took > liveShutdown+500*time.Millisecond {
		t.Errorf("Close took %v with stuck clients", took)
	}
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, stream)
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Error("the stream's connection is still open after Close")
	}
	// The download was dropped, not finished: its body ends short.
	_ = download.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(download), nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, res.Body)
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Error("the download's connection is still open after Close")
	}
	if err == nil || n >= int64(len(p.model)) {
		t.Errorf("the stuck download got %d of %d bytes (%v) after Close", n, len(p.model), err)
	}
}

// TestCloseDuringEnsureLive closes the server from another goroutine while
// the request goroutine starts the page, as a forced exit may: after
// Close no listener is left and none starts.
func TestCloseDuringEnsureLive(t *testing.T) {
	for range 20 {
		s := newLiveServer(t, nil, nil)
		var wg sync.WaitGroup
		wg.Go(func() { _ = s.Close() })
		u, err := s.ensureLive()
		wg.Wait()
		if _, again := s.ensureLive(); again == nil {
			t.Fatal("ensureLive succeeded after Close")
		}
		if err == nil {
			host := must(url.Parse(u))(t).Host
			if conn, err := net.DialTimeout("tcp", host, time.Second); err == nil {
				_ = conn.Close()
				t.Error("a listener started around Close is still open")
			}
		}
	}
}

// TestLivePageRacesNothing reads every route from eight goroutines while
// the request goroutine publishes, records rejections and opens profiles.
func TestLivePageRacesNothing(t *testing.T) {
	profiles := equalCPU(10)
	profiles["cpu.pb.gz"] = pmfixture.CPUBasic()
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxOpenHandles = 4 }, profiles)
	base := liveURL(t, s)
	o := openLive(t, s, "cpu.pb.gz")
	r := o.explained(t, s, "b", "0.411")
	o.publish(t, s, r, "")
	b, a := string(o.node(t, "b")), string(o.node(t, "a"))

	var stop atomic.Bool
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := range 8 {
		wg.Go(func() {
			for !stop.Load() {
				_, body := httpGet(t, base+"state", nil)
				var st stateJSON
				if err := json.Unmarshal(body, &st); err != nil {
					t.Errorf("state %q: %v", body, err)
					return
				}
				for _, route := range []string{"model", "history", "frame/" + b, "focus/" + a, "frame/" + a} {
					res, _ := httpGet(t, base+"pub/"+st.Current+"/"+route, nil)
					switch res.StatusCode {
					case http.StatusOK:
						ok.Add(1)
					case http.StatusNotFound:
					default:
						t.Errorf("goroutine %d: %s status %d", i, route, res.StatusCode)
					}
				}
			}
		})
	}
	for i := range 50 {
		if _, err := s.publishExplained(o.e, o.v, o.view, r, "", "a0"); err != nil {
			t.Fatal(err)
		}
		if i < 20 {
			o.reject(t, s, "c")
			s.refreshHistory()
			s.commit()
		}
		if i < 10 {
			name := string(rune('a'+i)) + ".pb.gz"
			x := openLive(t, s, name)
			if i%3 == 0 {
				x.facts(t, s)
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
