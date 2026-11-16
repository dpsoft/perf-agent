package perfmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// inboxPart is the inbox part of the state envelope.
type inboxPart struct {
	Rev    string `json:"rev"`
	Asks   string `json:"asks"`
	Unread string `json:"unread"`
	ReadBy string `json:"read_by"`
}

// envelopeJSON is the state envelope, its engine part raw and decoded.
type envelopeJSON struct {
	raw    json.RawMessage
	Engine stateJSON
	Inbox  inboxPart
}

// envelopeOf decodes an envelope, which must have exactly the two parts.
func envelopeOf(t *testing.T, env []byte) envelopeJSON {
	t.Helper()
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(env, &parts); err != nil {
		t.Fatalf("envelope %s: %v", env, err)
	}
	if len(parts) != 2 || parts["engine"] == nil || parts["inbox"] == nil {
		t.Fatalf("an envelope that is not {engine, inbox}: %s", env)
	}
	out := envelopeJSON{raw: parts["engine"]}
	if err := json.Unmarshal(parts["engine"], &out.Engine); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(parts["inbox"], &out.Inbox); err != nil {
		t.Fatal(err)
	}
	return out
}

// envelopeNow is s's current envelope, decoded.
func envelopeNow(t *testing.T, s *Server) envelopeJSON {
	t.Helper()
	return envelopeOf(t, s.PageSnapshot().State())
}

// ask reports an ask of node on pub, which must be accepted.
func ask(t *testing.T, s *Server, pub, node string) {
	t.Helper()
	if err := s.Report(PageReport{Kind: "ask", Publication: pub, Node: node}); err != nil {
		t.Fatalf("ask %s %s: %v", pub, node, err)
	}
}

// stepClock is a clock for s.now that moves by step on every reading.
func stepClock(s *Server, step time.Duration) {
	at := time.Date(2026, 11, 15, 10, 2, 3, 0, time.UTC)
	s.now = func() time.Time {
		at = at.Add(step)
		return at
	}
}

// §D4: the state and every event carry the engine's state and the
// inbox's, each at its own revision, and a page that connects before
// anything happened is sent both at zero.
func TestTheEnvelopeHasTwoRevisions(t *testing.T) {
	s := newLiveServer(t, nil, nil)
	want := `{"engine":{"seq":"0","current":"","selected":"","focus":"","by":"","publications":[]},` +
		`"inbox":{"rev":"0","asks":"0","unread":"0","read_by":""}}`
	if got := string(s.PageSnapshot().State()); got != want {
		t.Errorf("the first envelope\n%s\nwant\n%s", got, want)
	}
	if env := envelopeNow(t, s); !bytes.Equal(env.raw, s.live.snapshot().state) {
		t.Errorf("the engine part %s is not the snapshot's state %s", env.raw, s.live.snapshot().state)
	}
}

// A commit moves only engine.seq and leaves the inbox part as it was; a
// report moves only inbox.rev and leaves the engine part byte for byte.
// Every change is notified once, in order.
func TestACommitBumpsOnlySeqAndAReportOnlyRev(t *testing.T) {
	var log stateLog
	s := newLiveServer(t, func(c *Config) { c.Page.Notify = log.notify }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	stepClock(s, time.Second)
	o := openLive(t, s, "cpu.pb.gz")
	b, c := string(o.node(t, "b")), string(o.node(t, "c"))
	type parts struct{ seq, rev, asks, unread string }
	steps := []struct {
		what string
		do   func()
		want parts
	}{
		{"page opened", func() { liveURL(t, s) }, parts{"1", "0", "0", "0"}},
		{"facts published", func() { o.facts(t, s) }, parts{"2", "0", "0", "0"}},
		{"an ask", func() { ask(t, s, "p1", b) }, parts{"2", "1", "1", "1"}},
		{"a select", func() {
			if err := s.Report(PageReport{Kind: "select", Publication: "p1", Node: c}); err != nil {
				t.Fatal(err)
			}
		}, parts{"2", "2", "1", "1"}},
		{"a publication", func() { o.publish(t, s, o.explained(t, s, "b", "0.411"), "") }, parts{"3", "2", "1", "1"}},
		{"a focus", func() {
			if err := s.Report(PageReport{Kind: "focus", Publication: "p2", Node: b}); err != nil {
				t.Fatal(err)
			}
		}, parts{"3", "3", "1", "1"}},
	}
	prev := envelopeNow(t, s)
	for i, st := range steps {
		st.do()
		all := log.all()
		if len(all) != i+1 {
			t.Fatalf("%s: %d envelopes notified, want %d", st.what, len(all), i+1)
		}
		if !bytes.Equal(all[i], s.PageSnapshot().State()) {
			t.Errorf("%s: the envelope notified is not the state's", st.what)
		}
		env := envelopeOf(t, all[i])
		if got := (parts{env.Engine.Seq, env.Inbox.Rev, env.Inbox.Asks, env.Inbox.Unread}); got != st.want {
			t.Errorf("%s: %+v, want %+v", st.what, got, st.want)
		}
		switch {
		case env.Engine.Seq == prev.Engine.Seq && !bytes.Equal(env.raw, prev.raw):
			t.Errorf("%s: the engine part changed without a commit", st.what)
		case env.Inbox.Rev == prev.Inbox.Rev && env.Inbox != prev.Inbox:
			t.Errorf("%s: the inbox part changed without a rev", st.what)
		}
		prev = env
	}
}

// hubPage is a live server over the CPU fixtures whose engine h holds,
// notifying its envelopes on a channel, and one session of it named A
// with cpu.pb.gz open and published as p1.
func hubPage(t *testing.T, h *engineHold, edit func(*Config)) (*Server, *mcpSession, openedAs, chan []byte) {
	t.Helper()
	notified := make(chan []byte, 1024)
	cfg := testConfig(t)
	writeCPUFixtures(t, cfg)
	cfg.Page = livePage()
	cfg.Page.Notify = func(env []byte) {
		select {
		case notified <- env:
		default:
			t.Error("the test's notify channel is full")
		}
	}
	if h != nil {
		cfg.HoldEngine = h.hold
	}
	if edit != nil {
		edit(&cfg)
	}
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	a := newMCPSession(t, s, "A")
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	a.callOK("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}}, nil)
	for len(notified) > 0 {
		<-notified
	}
	return s, a, o, notified
}

// p1Node is the ID of the node of p1's view named name.
func p1Node(t *testing.T, s *Server, name string) string {
	t.Helper()
	var found []string
	s.live.snapshot().pubs["p1"].v.Walk(func(n *profilemodel.Node) bool {
		if n.Function.Name == name {
			found = append(found, string(n.ID))
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("%d nodes named %q", len(found), name)
	}
	return found[0]
}

// SRV-R3: a report is recorded and notified while a tool holds the
// engine, and a commit while the inbox's lock is held: neither writer
// waits for the other's lock.
func TestNeitherWriterWaitsForTheOther(t *testing.T) {
	h := newEngineHold()
	s, a, o, notified := hubPage(t, h, nil)
	b := p1Node(t, s, "b")
	done := holdTop(t, h, a, o)

	start := time.Now()
	ask(t, s, "p1", b)
	select {
	case env := <-notified:
		if took := time.Since(start); took > 50*time.Millisecond {
			t.Errorf("the report and its envelope took %v with the engine held", took)
		}
		if e := envelopeOf(t, env); e.Inbox.Rev != "1" || e.Inbox.Asks != "1" {
			t.Errorf("the envelope %+v", e.Inbox)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope while the engine was held")
	}
	close(h.release)
	if text := <-done; strings.HasPrefix(text, "isError") {
		t.Fatal(text)
	}

	// The engine's writer, with the inbox's lock held.
	s.live.inbox.mu.Lock()
	start = time.Now()
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		s.engine.slot <- struct{}{}
		s.commit()
		<-s.engine.slot
	}()
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		s.live.inbox.mu.Unlock()
		t.Fatal("a commit waited for the inbox's lock")
	}
	s.live.inbox.mu.Unlock()
	select {
	case env := <-notified:
		if took := time.Since(start); took > 50*time.Millisecond {
			t.Errorf("the commit and its envelope took %v with the inbox locked", took)
		}
		if e := envelopeOf(t, env); e.Inbox.Rev != "1" {
			t.Errorf("a commit moved the inbox: %+v", e.Inbox)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope for the commit")
	}
}

// §D4: a report is checked against the snapshot the page reads. Anything
// the snapshot does not hold is refused, records nothing and moves no
// revision; a diff's union node is a node of it.
func TestReportsAreValidatedAgainstTheSnapshot(t *testing.T) {
	s, o, union := goldenPublications(t, nil)
	b := string(o.node(t, "b"))
	long := strings.Repeat("0", maxPageID+1)
	before := envelopeNow(t, s)
	for _, r := range []PageReport{
		{Kind: "ask", Publication: "p9", Node: b},
		{Kind: "ask", Publication: "p1", Node: "0123456789abcdef"},
		{Kind: "ask", Publication: strings.Repeat("p", maxPageID+1), Node: b},
		{Kind: "ask", Publication: "p1", Node: long},
		{Kind: "select", Publication: "p1", Node: b, Focus: long},
		{Kind: "select", Publication: "p1", Node: b, Focus: "0123456789abcdef"},
		{Kind: "bogus", Publication: "p1", Node: b},
		{Kind: "", Publication: "p1", Node: b},
		{Kind: "ask", Publication: "p1"},
		{Kind: "ask", Publication: "p2", Node: b},
		{Kind: "ask", Publication: "p1", Node: union},
	} {
		err := s.Report(r)
		if !errors.Is(err, ErrBadReport) {
			t.Errorf("%+v: %v, want ErrBadReport", r, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "invalid page report: ") || strings.Contains(err.Error(), "0123456789abcdef") ||
			strings.Contains(err.Error(), "bogus") || strings.Contains(err.Error(), "p9") {
			t.Errorf("%+v: the reason %q repeats what the page sent", r, err)
		}
	}
	if after := envelopeNow(t, s); after.Inbox != before.Inbox {
		t.Errorf("refused reports moved the inbox: %+v, then %+v", before.Inbox, after.Inbox)
	}
	in := s.live.inbox
	if in.user != nil || len(in.asks) != 0 || in.next != 0 {
		t.Errorf("refused reports recorded something: %+v %+v", in.user, in.asks)
	}

	ask(t, s, "p2", union)
	ask(t, s, "p1", b)
	if err := s.Report(PageReport{Kind: "focus", Publication: "p1"}); err != nil {
		t.Errorf("a focus on the whole view: %v", err)
	}
	if err := s.Report(PageReport{Kind: "select", Publication: "p1", Focus: b}); err != nil {
		t.Errorf("a select of nothing under a focus: %v", err)
	}
	if got := envelopeNow(t, s).Inbox; got.Rev != "4" || got.Asks != "2" {
		t.Errorf("after two asks and two reports: %+v", got)
	}
	row := in.asks[0]
	if row.publication != "p2" || row.node != union || row.name != "b" || row.view != o.view || row.digest != "" {
		t.Errorf("the diff's ask %+v", row)
	}
	row = in.asks[1]
	if row.node != b || row.name != "b" || row.digest != o.e.prof.Digest.String() {
		t.Errorf("the explained ask %+v", row)
	}

	// Without the live page there is nothing to report on.
	plain := must(New(testConfig(t)))(t)
	t.Cleanup(func() { _ = plain.Close() })
	if err := plain.Report(PageReport{Kind: "ask", Publication: "p1", Node: b}); !errors.Is(err, ErrBadReport) {
		t.Errorf("a report without Page: %v", err)
	}
}

// The inbox keeps the server's own strings: a long frame name is cut to
// 1 KiB at a character boundary.
func TestTheInboxKeepsTheServersValues(t *testing.T) {
	name := strings.Repeat("é", 700)
	s := newLiveServer(t, nil, map[string][]byte{"long.pb.gz": pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(name)}, Value: 10})})
	o := openLive(t, s, "long.pb.gz")
	o.facts(t, s)
	node := string(o.node(t, name))
	ask(t, s, "p1", strings.Clone(node))
	got := s.live.inbox.asks[0].name
	if len(got) > maxInboxName || !strings.HasSuffix(got, "…") || !strings.HasPrefix(name, strings.TrimSuffix(got, "…")) {
		t.Errorf("a %d-byte name kept as %d bytes: %.40q", len(name), len(got), got)
	}
}

// §D7 "Engine untouched": no page report changes engine state: the seq,
// the attempts record, the store's handles and the publications are what
// they were.
func TestNoPageRequestMutatesEngineState(t *testing.T) {
	s, o, union := goldenPublications(t, nil)
	stepClock(s, 3*time.Second)
	b, c := string(o.node(t, "b")), string(o.node(t, "c"))
	before, dump := countersOf(s), dumpLive(s)
	handles := slices.Sorted(maps.Keys(s.store.entries))
	attempts := s.attempts.next
	for i := range 50 {
		var r PageReport
		switch i % 5 {
		case 0:
			r = PageReport{Kind: "ask", Publication: "p1", Node: b}
		case 1:
			r = PageReport{Kind: "select", Publication: "p1", Node: c, Focus: b}
		case 2:
			r = PageReport{Kind: "focus", Publication: "p2", Node: union}
		case 3:
			r = PageReport{Kind: "ask", Publication: "p2", Node: union}
		default:
			r = PageReport{Kind: "ask", Publication: "p9", Node: b}
		}
		_ = s.Report(r)
	}
	if after := countersOf(s); after != before {
		t.Errorf("reports changed the engine: %+v, then %+v", before, after)
	}
	if after := dumpLive(s); !equalDumps(dump, after) {
		t.Error("reports changed the live state")
	}
	if got := slices.Sorted(maps.Keys(s.store.entries)); !slices.Equal(got, handles) {
		t.Errorf("reports changed the handles: %v, then %v", handles, got)
	}
	if s.attempts.next != attempts {
		t.Errorf("reports recorded attempts: %d, then %d", attempts, s.attempts.next)
	}
	if got := envelopeNow(t, s).Inbox.Rev; got != "40" {
		t.Errorf("40 accepted reports moved rev to %s", got)
	}
}
