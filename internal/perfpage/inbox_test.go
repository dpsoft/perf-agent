package perfpage

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// report posts body to h as the guard hands a report on, and returns the
// status and the body of the answer.
func report(h http.Handler, method, body string) (int, string) {
	req := httptest.NewRequest(method, "http://127.0.0.1/s/token/inbox", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// askBody is an ask of node on publication pub.
func askBody(pub, node string) string {
	return `{"kind":"ask","publication":"` + pub + `","node":"` + node + `","focus":""}`
}

// §D4: a valid report gets 204 and is recorded; anything else gets 422
// with the reason, which repeats nothing the page sent, and records
// nothing.
func TestInboxHandlerAnswers204And422(t *testing.T) {
	p := newPage(t, nil)
	o := p.c.open("cpu.pb.gz")
	o.explain(nil)
	b, c := o.nodeID("b"), o.nodeID("c")
	h := Inbox(p.srv)

	if code, body := report(h, http.MethodPost, askBody("p1", b)); code != http.StatusNoContent || body != "" {
		t.Fatalf("a valid ask: %d %q", code, body)
	}
	if got := p.envelope().Inbox; got != (inboxJSON{Rev: "1", Asks: "1", Unread: "1"}) {
		t.Errorf("after the ask: %+v", got)
	}
	// The same click again is accepted and ignored.
	if code, _ := report(h, http.MethodPost, askBody("p1", b)); code != http.StatusNoContent {
		t.Errorf("a repeated ask: %d", code)
	}
	if code, _ := report(h, http.MethodPost, `{"kind":"select","publication":"p1","node":"`+c+`"}`); code != http.StatusNoContent {
		t.Errorf("a select without focus: %d", code)
	}

	for _, tc := range []struct{ what, body, reason string }{
		{"an unknown field", `{"kind":"ask","publication":"p1","node":"` + b + `","name":"<b>hi</b>"}`, "the body must be one JSON object"},
		{"an unknown node", askBody("p1", "0123456789abcdef"), "the frame is not in the publication"},
		{"an unknown publication", askBody("p7", b), "the publication is not on the page"},
		{"a bogus kind", `{"kind":"bogus","publication":"p1","node":"` + b + `"}`, "kind must be select, focus or ask"},
		{"a field twice", `{"kind":"ask","kind":"select","publication":"p1","node":"` + b + `"}`, "the body must be one JSON object"},
		{"a field in another case", `{"Kind":"ask","publication":"p1","node":"` + b + `"}`, "the body must be one JSON object"},
		{"a number", `{"kind":"ask","publication":"p1","node":7}`, "the body must be one JSON object"},
		{"an array", `{"kind":"ask","publication":["p1"],"node":"` + b + `"}`, "the body must be one JSON object"},
		{"null", `{"kind":"ask","publication":null,"node":"` + b + `"}`, "the body must be one JSON object"},
		{"two objects", askBody("p1", c) + askBody("p1", c), "the body must be one JSON object"},
		{"trailing text", askBody("p1", c) + "x", "the body must be one JSON object"},
		{"not an object", `["ask"]`, "the body must be one JSON object"},
		{"empty", ``, "the body must be one JSON object"},
		{"truncated", `{"kind":"ask"`, "the body must be one JSON object"},
		{"over 2 KiB", `{"kind":"ask","publication":"p1","node":"` + c + `","focus":"` + strings.Repeat(" ", 2048) + `"}`, "over 2048 bytes"},
	} {
		code, body := report(h, http.MethodPost, tc.body)
		if code != http.StatusUnprocessableEntity || !strings.HasPrefix(body, "invalid page report: ") || !strings.Contains(body, tc.reason) {
			t.Errorf("%s: %d %q, want 422 saying %q", tc.what, code, body, tc.reason)
		}
		for _, echoed := range []string{"0123456789abcdef", "p7", "bogus", "<b>"} {
			if strings.Contains(body, echoed) {
				t.Errorf("%s: the reason %q repeats %q", tc.what, body, echoed)
			}
		}
	}
	if code, _ := report(h, http.MethodGet, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET inbox: %d", code)
	}
	// The select is not in the envelope; the refusals changed nothing.
	if got := p.envelope().Inbox; got != (inboxJSON{Rev: "1", Asks: "1", Unread: "1"}) {
		t.Errorf("refused reports moved the inbox: %+v", got)
	}
}

// engineHolder holds the engine in the next top call once armed, until
// released.
type engineHolder struct {
	armed         atomic.Bool
	held, release chan struct{}
}

func newEngineHolder() *engineHolder {
	return &engineHolder{held: make(chan struct{}), release: make(chan struct{})}
}

func (h *engineHolder) hold(tool string) {
	if tool == "top" && h.armed.CompareAndSwap(true, false) {
		close(h.held)
		<-h.release
	}
}

// §D4 with two tabs: an ask, and page_state recording its reader,
// reach both tabs as new inbox revisions with no commit, and again while
// a tool of another session holds the engine.
func TestTwoTabsSeeInboxRevisionsWithoutCommits(t *testing.T) {
	hold := newEngineHolder()
	p := newPage(t, func(c *perfmcp.Config) { c.HoldEngine = hold.hold })
	o := p.c.open("cpu.pb.gz")
	o.explain(nil)
	b, c := o.nodeID("b"), o.nodeID("c")
	seq := p.state().Seq
	h := Inbox(p.srv)
	agent := p.session()
	one, two := openEvents(t, p.url), openEvents(t, p.url)
	for _, tab := range []*eventsClient{one, two} {
		if env := tab.nextEnvelope(t); env.Engine.Seq != seq || env.Inbox.Rev != "0" {
			t.Fatalf("a new tab's first envelope %+v", env)
		}
	}
	// both checks that each tab is sent want next, with the engine at seq.
	both := func(what string, want inboxJSON) {
		t.Helper()
		for i, tab := range []*eventsClient{one, two} {
			env := tab.nextEnvelope(t)
			if env.Inbox != want || env.Engine.Seq != seq {
				t.Errorf("%s: tab %d got %+v at seq %s, want %+v at seq %s", what, i+1, env.Inbox, env.Engine.Seq, want, seq)
			}
		}
	}

	if code, body := report(h, http.MethodPost, askBody("p1", b)); code != http.StatusNoContent {
		t.Fatalf("ask: %d %s", code, body)
	}
	both("the ask", inboxJSON{Rev: "1", Asks: "1", Unread: "1"})
	agent.call("page_state", map[string]any{})
	both("page_state", inboxJSON{Rev: "2", Asks: "1", Unread: "0", ReadBy: "client"})

	// Again while the first session's top holds the engine.
	hold.armed.Store(true)
	topDone := make(chan struct{})
	go func() {
		defer close(topDone)
		p.c.try("top", o.args(map[string]any{"rows": "functions"}))
	}()
	select {
	case <-hold.held:
	case <-time.After(10 * time.Second):
		t.Fatal("top never held the engine")
	}
	if code, body := report(h, http.MethodPost, askBody("p1", c)); code != http.StatusNoContent {
		t.Fatalf("ask with the engine held: %d %s", code, body)
	}
	both("the ask with the engine held", inboxJSON{Rev: "3", Asks: "2", Unread: "1"})
	agent.call("page_state", map[string]any{})
	both("page_state with the engine held", inboxJSON{Rev: "4", Asks: "2", Unread: "0", ReadBy: "client"})
	select {
	case <-topDone:
		t.Fatal("top ended while the engine was held")
	default:
	}
	close(hold.release)
	<-topDone
}

// §D4: commits and inbox changes interleaved from two goroutines reach
// each tab in an order where neither engine.seq nor inbox.rev ever goes
// back.
func TestInterleavedCommitsAndReportsNeverGoBack(t *testing.T) {
	p := newPage(t, nil)
	o := p.c.open("cpu.pb.gz")
	o.explain(nil)
	o.explain(nil)
	nodes := []string{o.nodeID("main"), o.nodeID("a"), o.nodeID("b"), o.nodeID("c")}
	h := Inbox(p.srv)
	agent := p.session()
	tabs := []*eventsClient{openEvents(t, p.url), openEvents(t, p.url)}

	// The readers stop at the envelope at seq and rev, set once every
	// change is made.
	var seq, rev atomic.Int64
	seq.Store(-1)
	seen := make([][]envelopeJSON, len(tabs))
	var readers sync.WaitGroup
	for i, tab := range tabs {
		readers.Go(func() {
			for {
				env, err := tab.read()
				if err != nil {
					t.Errorf("tab %d: %v", i+1, err)
					return
				}
				seen[i] = append(seen[i], env)
				s, _ := strconv.ParseInt(env.Engine.Seq, 10, 64)
				r, _ := strconv.ParseInt(env.Inbox.Rev, 10, 64)
				if s == seq.Load() && r == rev.Load() {
					return
				}
			}
		})
	}

	const each = 100
	var writers sync.WaitGroup
	writers.Go(func() {
		for i := range each {
			p.c.try("page_show", map[string]any{"publication": "p" + strconv.Itoa(1+i%2)})
		}
	})
	// Asks, each read by the agent; an ask that repeats one of the last
	// 2 s is ignored, so some of them change nothing.
	writers.Go(func() {
		for i := range each {
			pub := "p" + strconv.Itoa(1+i%2)
			if code, body := report(h, http.MethodPost, askBody(pub, nodes[i/2%len(nodes)])); code != http.StatusNoContent {
				t.Errorf("ask: %d %s", code, body)
				return
			}
			agent.try("page_state", map[string]any{"limit": 32})
		}
	})
	writers.Wait()
	// One more commit, whose envelope is the last.
	end := p.envelope()
	s, _ := strconv.ParseInt(end.Engine.Seq, 10, 64)
	r, _ := strconv.ParseInt(end.Inbox.Rev, 10, 64)
	rev.Store(r)
	seq.Store(s + 1)
	p.call("page_show", map[string]any{"publication": "p1"})
	readers.Wait()

	final := p.envelope()
	if r < 8 {
		t.Errorf("only %d inbox changes were made", r)
	}
	for i, envs := range seen {
		var seq, rev int64
		for j, env := range envs {
			s, _ := strconv.ParseInt(env.Engine.Seq, 10, 64)
			r, _ := strconv.ParseInt(env.Inbox.Rev, 10, 64)
			if s < seq || r < rev {
				t.Fatalf("tab %d, envelope %d: seq %d and rev %d after seq %d and rev %d", i+1, j, s, r, seq, rev)
			}
			seq, rev = s, r
		}
		if got := envs[len(envs)-1]; got.Engine.Seq != final.Engine.Seq || got.Inbox != final.Inbox {
			t.Errorf("tab %d ended at %+v, not at the state's %+v", i+1, got, final)
		}
	}
}
