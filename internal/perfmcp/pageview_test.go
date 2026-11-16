package perfmcp

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// goldenPublications is a live server with the page opened, CPUBasic's b
// explained in p1 with one later rejection in its history, and the
// CPUBasic pair compared in p2: the publications the page's goldens were
// recorded from, through the HTTP routes perfmcp served before PageView.
// It returns the explained view and the union ID of b in the diff.
func goldenPublications(t *testing.T, edit func(*Config)) (*Server, liveOpened, string) {
	t.Helper()
	s := newLiveServer(t, edit, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic(), "new.pb.gz": pmfixture.CPUBasicNew()})
	liveURL(t, s)
	o := openLive(t, s, "cpu.pb.gz")
	o.publish(t, s, o.explained(t, s, "b", "0.411"), "golden explained")
	o.reject(t, s, "c")
	if !s.refreshHistory() {
		t.Fatal("the rejection did not reach p1's history")
	}
	s.commit()
	n := openLive(t, s, "new.pb.gz")
	q := diffQuery{BaseHandle: o.e.prof.Handle, BaseView: o.view, NewHandle: n.e.prof.Handle, NewView: n.view}
	bs, ns := mustBoth(s.diffSides(q))(t)
	p2 := must(s.publishDiff(must(s.compare(q, bs, ns))(t), bs, ns, "golden diff", "", ""))(t)
	return s, o, unionNamed(t, p2.diff, "b")
}

// unionNamed is the union ID of the one node of d named name.
func unionNamed(t *testing.T, d *profilemodel.Diff, name string) string {
	t.Helper()
	var found []string
	stack := []*profilemodel.DiffNode{d.Root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Name == name {
			found = append(found, string(n.UnionID()))
		}
		stack = append(stack, n.Children...)
	}
	if len(found) != 1 {
		t.Fatalf("%d nodes named %q in the diff", len(found), name)
	}
	return found[0]
}

func TestPageViewServesWhatTheRoutesDid(t *testing.T) {
	s, o, b := goldenPublications(t, nil)
	v := s.PageSnapshot()
	c, a := string(o.node(t, "c")), string(o.node(t, "a"))
	ok := func(out []byte, err error) ([]byte, error) { return out, err }
	for _, g := range []struct {
		name string
		get  func() ([]byte, error)
	}{
		// The routes served the engine's state; the state is now its
		// envelope, whose engine part is that state byte for byte.
		{"state", func() ([]byte, error) { return envelopeOf(t, v.State()).raw, nil }},
		{"explained-model", func() ([]byte, error) { return ok(v.Model("p1")) }},
		{"explained-history", func() ([]byte, error) { return ok(v.History("p1")) }},
		{"explained-frame", func() ([]byte, error) { return ok(v.Frame("p1", c)) }},
		{"explained-focus", func() ([]byte, error) { return ok(v.Focus("p1", a)) }},
		{"explained-export", func() ([]byte, error) { return ok(v.Export("p1")) }},
		{"diff-model", func() ([]byte, error) { return ok(v.Model("p2")) }},
		{"diff-history", func() ([]byte, error) { return ok(v.History("p2")) }},
		{"diff-frame", func() ([]byte, error) { return ok(v.Frame("p2", b)) }},
		{"diff-focus", func() ([]byte, error) { return ok(v.Focus("p2", b)) }},
		{"diff-export", func() ([]byte, error) { return ok(v.Export("p2")) }},
	} {
		want, err := os.ReadFile(filepath.Join("testdata", "pageview", g.name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := g.get()
		if err != nil {
			t.Errorf("%s: %v", g.name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes that are not the %d the route served", g.name, len(got), len(want))
		}
	}
}

func TestPageViewNotFoundAndLimits(t *testing.T) {
	s, o, b := goldenPublications(t, nil)
	v := s.PageSnapshot()
	long := strings.Repeat("p", maxPageID+1)
	node := string(o.node(t, "b"))
	type call struct {
		what string
		err  error
	}
	calls := func(v PageView, pub, explained, union string) []call {
		var out []call
		for _, f := range []struct {
			what string
			fn   func(string) ([]byte, error)
		}{{"model", v.Model}, {"history", v.History}, {"export", v.Export}} {
			_, err := f.fn(pub)
			out = append(out, call{f.what + " " + pub, err})
		}
		for _, f := range []struct {
			what string
			fn   func(string, string) ([]byte, error)
		}{{"frame", v.Frame}, {"focus", v.Focus}} {
			_, err := f.fn(pub, explained)
			out = append(out, call{f.what + " " + pub + "/" + explained, err})
			_, err = f.fn("p2", union)
			out = append(out, call{f.what + " p2/" + union, err})
		}
		return out
	}
	var all []call
	all = append(all, calls(v, "p9", node, "0000000000000000/-")...)
	all = append(all, calls(v, long, strings.Repeat("0", maxPageID+1), strings.Repeat("0", maxPageID+1))...)
	all = append(all, calls(PageView{}, "p1", node, b)...)
	for _, f := range []func(string, string) ([]byte, error){v.Frame, v.Focus} {
		_, err := f("p1", "0123456789abcdef")
		all = append(all, call{"an unknown node of the view", err})
		_, err = f("p2", node)
		all = append(all, call{"a node ID on a diff", err})
	}
	for _, c := range all {
		if !errors.Is(c.err, ErrPageNotFound) {
			t.Errorf("%s: %v, want ErrPageNotFound", c.what, c.err)
		}
	}
	if (PageView{}).State() != nil {
		t.Error("the zero view has a state")
	}
	if got := ErrPageNotFound.Error(); got != "no such publication or node; the page will refresh" {
		t.Errorf("ErrPageNotFound says %q", got)
	}

	// A frame whose path from the root is over MaxPageBytes.
	const depth, nameLen, budget = 300, 1000, 96 << 10
	frames := make([]pmfixture.Frame, depth)
	for i := range frames {
		p := fmt.Sprintf("f%04d_", i)
		frames[i] = pmfixture.Fn(p + strings.Repeat("n", nameLen-len(p)))
	}
	deep := newLiveServer(t, func(c *Config) { c.Limits.MaxPageBytes = budget },
		map[string][]byte{"deep.pb.gz": pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: frames, Value: 10})})
	do := openLive(t, deep, "deep.pb.gz")
	do.facts(t, deep)
	bottom := fmt.Sprintf("f%04d_", depth-1)
	_, err := deep.PageSnapshot().Frame("p1", string(do.node(t, bottom+strings.Repeat("n", nameLen-len(bottom)))))
	if le, ok := errors.AsType[*PageLimitError](err); !ok || !strings.Contains(le.Msg, fmt.Sprintf("MaxPageBytes=%d", budget)) {
		t.Errorf("a frame over MaxPageBytes: %v", err)
	}
	if _, err := deep.PageSnapshot().Frame("p1", string(do.node(t, "f0001_"+strings.Repeat("n", nameLen-len("f0001_"))))); err != nil {
		t.Errorf("a shallow frame: %v", err)
	}

	// An export over MaxPageBytes.
	small := newLiveServer(t, func(c *Config) {
		c.Limits.MaxPageBytes = 16 << 10
		c.Limits.MaxRetainedBytes = 1 << 30
	}, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	so := openLive(t, small, "cpu.pb.gz")
	so.facts(t, small)
	_, err = small.PageSnapshot().Export("p1")
	if le, ok := errors.AsType[*PageLimitError](err); !ok || !strings.Contains(le.Msg, "MaxPageBytes=16384") || strings.Contains(le.Msg, "<") {
		t.Errorf("an export over MaxPageBytes: %v", err)
	}
}

// stateLog records the states a PageConfig's Notify is given.
type stateLog struct {
	mu     sync.Mutex
	states [][]byte
}

func (l *stateLog) notify(state []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.states = append(l.states, state)
}

func (l *stateLog) all() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.states)
}

func TestNotifyGetsEveryCommittedState(t *testing.T) {
	var log stateLog
	s := newLiveServer(t, func(c *Config) { c.Page.Notify = log.notify }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	if n := len(log.all()); n != 0 {
		t.Fatalf("%d states before the page was opened", n)
	}
	o := openLive(t, s, "cpu.pb.gz")
	steps := []func(){
		func() { liveURL(t, s) },
		func() { liveURL(t, s) }, // a second open commits nothing
		func() { o.facts(t, s) },
		func() { o.publish(t, s, o.explained(t, s, "b", "0.411"), "") },
		func() { o.reject(t, s, "c"); s.refreshHistory(); s.commit() },
		func() {
			if err := s.live.show("p1", "", false, ""); err != nil {
				t.Fatal(err)
			}
			s.commit()
		},
	}
	commits := []int{1, 0, 1, 1, 1, 1}
	want := 0
	for i, step := range steps {
		step()
		want += commits[i]
		got := log.all()
		if len(got) != want {
			t.Fatalf("step %d: %d states notified, want %d", i, len(got), want)
		}
		if want > 0 && !bytes.Equal(got[len(got)-1], s.PageSnapshot().State()) {
			t.Errorf("step %d: the last state notified is not the snapshot's", i)
		}
		if want > 0 && !bytes.Equal(envelopeOf(t, got[len(got)-1]).raw, s.live.snapshot().state) {
			t.Errorf("step %d: the last envelope notified does not carry the snapshot's state", i)
		}
	}
	var prev int
	for i, raw := range log.all() {
		env := envelopeOf(t, raw)
		var seq int
		if _, err := fmt.Sscan(env.Engine.Seq, &seq); err != nil || seq <= prev {
			t.Errorf("state %d has seq %q after %d", i, env.Engine.Seq, prev)
		}
		if env.Inbox.Rev != "0" {
			t.Errorf("state %d: a commit moved the inbox to rev %s", i, env.Inbox.Rev)
		}
		prev = seq
	}

	// Without Notify every commit still stores its snapshot.
	quiet := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	qo := openLive(t, quiet, "cpu.pb.gz")
	liveURL(t, quiet)
	qo.facts(t, quiet)
	if st := liveStateOf(t, quiet); st.Seq != "2" || st.Current != "p1" {
		t.Errorf("without Notify the state is %+v", st)
	}
}

func TestPageOpenReturnsTheConfiguredURL(t *testing.T) {
	const url = "http://127.0.0.1:7399/s/another-token/"
	p := openLivePair(t, func(c *Config) { c.Page.URL = url }, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	if got := pageOpen(t, p.c).URL; got != url {
		t.Errorf("page_open gave %q, want %q", got, url)
	}
	var ex pageExplainOut
	p.c.callOK("page_explain", p.base.args(map[string]any{"explanations": []any{}}), &ex)
	var sh pageShowOut
	p.c.callOK("page_show", map[string]any{"publication": "p1"}, &sh)
	diff, _ := p.pageDiff(t, nil)
	for what, got := range map[string]string{"page_explain": ex.URL, "page_show": sh.URL, "page_diff": diff.URL} {
		if got != url {
			t.Errorf("%s gave %q, want %q", what, got, url)
		}
	}
}

// TestEnsureLiveAfterCloseRefuses closes the server from another goroutine
// while the request goroutine opens the page, as a forced exit may: after
// Close the page refuses, and no page tool runs.
func TestEnsureLiveAfterCloseRefuses(t *testing.T) {
	for range 20 {
		s := newLiveServer(t, nil, nil)
		var wg sync.WaitGroup
		wg.Go(func() { _ = s.Close() })
		u, err := s.ensureLive()
		wg.Wait()
		if err == nil && u != testPageURL {
			t.Fatalf("ensureLive gave %q", u)
		}
		if _, err := s.ensureLive(); !errors.Is(err, errLiveClosed) {
			t.Fatalf("ensureLive after Close: %v", err)
		}
	}
	c := pageClient(t, nil, nil)
	pageOpen(t, c)
	if err := c.srv.Close(); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]any{"page_open": map[string]any{}, "page_show": map[string]any{"publication": "p1"}} {
		if text, isErr := c.call(name, args); !isErr || text != errLiveClosed.Error() {
			t.Errorf("%s after Close: isError=%v %s", name, isErr, text)
		}
	}
}

// Without Page the server has no live state, no snapshot to read and no
// page tool; with it, all three.
func TestNoPageToolsWithoutPage(t *testing.T) {
	cfg := testConfig(t)
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	if s.live != nil {
		t.Error("a server without Page has live state")
	}
	if v := s.PageSnapshot(); v.State() != nil {
		t.Error("a server without Page has a page state")
	}
	if _, err := s.PageSnapshot().Model("p1"); !errors.Is(err, ErrPageNotFound) {
		t.Errorf("Model without Page: %v", err)
	}
	if _, err := s.ensureLive(); !errors.Is(err, errLiveDisabled) {
		t.Errorf("ensureLive without Page: %v", err)
	}
	cfg.Page = livePage()
	s = must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	if s.live == nil {
		t.Error("a server with Page has no live state")
	}
	if st := s.PageSnapshot().State(); string(st) != `{"engine":{"seq":"0","current":"","selected":"","focus":"","by":"","publications":[]},`+
		`"inbox":{"rev":"0","asks":"0","unread":"0","read_by":""}}` {
		t.Errorf("the zero state %s", st)
	}
}

// liveCounters are what reading a PageView must never change.
type liveCounters struct {
	next, entries int
	seq           int64
	snap          *liveSnapshot
}

func countersOf(s *Server) liveCounters {
	return liveCounters{next: s.attempts.next, entries: len(s.store.entries), seq: s.live.seq, snap: s.live.snapshot()}
}

func equalDumps(a, b liveDump) bool {
	return slices.Equal(a.pubs, b.pubs) && a.next == b.next && a.seq == b.seq && a.snap == b.snap &&
		a.current == b.current && a.selected == b.selected && a.focus == b.focus && a.total == b.total &&
		len(a.history) == len(b.history) && len(a.objs) == len(b.objs) && len(a.held) == len(b.held)
}

// Every PageView method only reads its snapshot.
func TestPageViewChangesNothing(t *testing.T) {
	s, o, b := goldenPublications(t, nil)
	c, a := string(o.node(t, "c")), string(o.node(t, "a"))
	before, dump := countersOf(s), dumpLive(s)
	state := bytes.Clone(s.PageSnapshot().State())
	for range 20 {
		v := s.PageSnapshot()
		v.State()
		for _, pub := range []string{"p1", "p2", "p9"} {
			_, _ = v.Model(pub)
			_, _ = v.History(pub)
			_, _ = v.Export(pub)
		}
		for _, n := range [][2]string{{"p1", c}, {"p1", a}, {"p2", b}, {"p1", "0123456789abcdef"}} {
			_, _ = v.Frame(n[0], n[1])
			_, _ = v.Focus(n[0], n[1])
		}
	}
	if after := countersOf(s); after != before {
		t.Errorf("reads changed the server: %+v, then %+v", before, after)
	}
	if after := dumpLive(s); !equalDumps(dump, after) {
		t.Error("reads changed the live state")
	}
	if !bytes.Equal(state, s.PageSnapshot().State()) {
		t.Error("reads changed the state")
	}
}

// The engine half of the export route: an explained publication's export
// is the page render_flamegraph writes for the same check and record,
// with the history worded for attempts in any order.
func TestExportIsTheStaticPageOfTheRecord(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	p := o.publish(t, s, o.explained(t, s, "b", "0.411"), "")
	rejected := o.reject(t, s, "c")
	if !s.refreshHistory() {
		t.Fatal("the rejection did not reach p1's history")
	}
	s.commit()
	body := must(s.PageSnapshot().Export("p1"))(t)
	recorded, dropped, incomplete := s.attempts.rejected(o.e.prof, o.view)
	var want bytes.Buffer
	if err := flamegraph.WriteExplainedPage(&want, o.v, p.result, flamegraph.ExplainedPageOptions{
		ProfileName: "cpu.pb.gz", Digest: o.e.prof.Digest.String(), Attempt: "a0",
		Rejected: recorded, DroppedRejected: dropped, HistoryIncomplete: incomplete, LiveHistory: true,
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

	body := must(s.PageSnapshot().Export("p1"))(t)
	want := must(exportExplained(p, h))(t)
	if !bytes.Equal(body, want) {
		t.Error("the export is not built from the snapshot's history")
	}
	recorded, dropped, incomplete := s.attempts.rejected(o.e.prof, o.view)
	var full bytes.Buffer
	if err := flamegraph.WriteExplainedPage(&full, o.v, p.result, flamegraph.ExplainedPageOptions{
		ProfileName: "cpu.pb.gz", Digest: o.e.prof.Digest.String(), Attempt: "a0",
		Rejected: recorded, DroppedRejected: dropped, HistoryIncomplete: incomplete, LiveHistory: true,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(body, full.Bytes()) {
		t.Error("the export shows the whole record, not what the history keeps")
	}
	page := string(body)
	if !strings.Contains(page, "640 claims rejected in 20 attempts") {
		t.Error("the export's pill does not count every claim and attempt")
	}
	// Every failure of the record is either listed or counted, attempt by
	// attempt, and the attempts left out are counted too.
	if len(h.rejected)+h.dropped != 20 {
		t.Errorf("the history keeps %d attempts and counts %d dropped, of 20", len(h.rejected), h.dropped)
	}
	omitted := 0
	for _, ra := range h.rejected {
		if len(ra.Failures)+ra.Omitted != 32 {
			t.Errorf("%s keeps %d failures and counts %d omitted, of 32", ra.Attempt, len(ra.Failures), ra.Omitted)
		}
		if ra.Omitted > 0 {
			omitted++
			label := fmt.Sprintf("and %d more failures", ra.Omitted)
			if ra.Omitted == 1 {
				label = "and 1 more failure"
			}
			if !strings.Contains(page, label) {
				t.Errorf("the export does not say %q for %s", label, ra.Attempt)
			}
		}
	}
	if omitted == 0 {
		t.Error("no kept attempt has omitted failures; the test needs some")
	}
	if h.dropped > 0 {
		label := fmt.Sprintf("plus %d earlier rejected attempts not kept", h.dropped)
		if h.dropped == 1 {
			label = "plus 1 earlier rejected attempt not kept"
		}
		if !strings.Contains(page, label) {
			t.Errorf("the export does not say %q", label)
		}
	}
}

// A diff publication pins what its PageView reads: the Diff, its index
// and both sides' Profiles and Views, shared with their other holders and
// counted once, and kept when the store evicts both handles.
func TestADiffPinsBothSides(t *testing.T) {
	fixtures := map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic(), "new.pb.gz": pmfixture.CPUBasicNew(),
		"gpu.pb.gz": pmfixture.GPUBasic(), "deep.pb.gz": pmfixture.Deep(10)}
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxOpenHandles = 2 }, fixtures)
	liveURL(t, s)
	o, n := openLive(t, s, "cpu.pb.gz"), openLive(t, s, "new.pb.gz")
	q := diffQuery{BaseHandle: o.e.prof.Handle, BaseView: o.view, NewHandle: n.e.prof.Handle, NewView: n.view}
	bs, ns := mustBoth(s.diffSides(q))(t)
	p := must(s.publishDiff(must(s.compare(q, bs, ns))(t), bs, ns, "", "", ""))(t)
	if p.prof == p.newProf || p.v == p.newV {
		t.Fatal("the sides are one profile")
	}
	// The diff alone holds its Diff and index; the store holds both sides
	// too, until opening two other profiles evicts them.
	cases := []struct {
		o             retained
		held, evicted int
	}{
		{p.diff, 1, 1}, {p.index, 1, 1}, {p.prof, 2, 1}, {p.v, 2, 1}, {p.newProf, 2, 1}, {p.newV, 2, 1},
	}
	for _, c := range cases {
		if got := s.pins.holders(c.o); got != c.held {
			t.Errorf("%T has %d holders, want %d", c.o, got, c.held)
		}
		if !slices.Contains(p.pinned(nil), c.o) {
			t.Errorf("%T is not pinned", c.o)
		}
	}
	openLive(t, s, "gpu.pb.gz")
	openLive(t, s, "deep.pb.gz")
	if _, ok := s.store.entries[o.e.prof.Handle]; ok {
		t.Fatal("the base handle is still open")
	}
	if _, ok := s.store.entries[n.e.prof.Handle]; ok {
		t.Fatal("the new handle is still open")
	}
	for _, c := range cases {
		if got := s.pins.holders(c.o); got != c.evicted {
			t.Errorf("%T has %d holders after the store let go, want %d", c.o, got, c.evicted)
		}
	}
	// The view still computes from what the publication pins.
	main := string(p.diff.Root.Children[0].UnionID())
	v := s.PageSnapshot()
	for what, get := range map[string]func() ([]byte, error){
		"model":  func() ([]byte, error) { return v.Model("p1") },
		"frame":  func() ([]byte, error) { return v.Frame("p1", main) },
		"focus":  func() ([]byte, error) { return v.Focus("p1", main) },
		"export": func() ([]byte, error) { return v.Export("p1") },
	} {
		if _, err := get(); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// Many readers of a diff's PageView run while the request goroutine
// publishes and steers: every read takes only its snapshot.
func TestDiffRoutesRaceNothing(t *testing.T) {
	s, _, _ := goldenPublications(t, nil)
	p := s.live.snapshot().pubs["p2"]
	b := p.index.Path(p.diff.Root.Children[0].UnionID())
	if len(b) == 0 {
		t.Fatal("no main in the union")
	}
	id := string(b[len(b)-1].UnionID())
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := s.PageSnapshot()
				for what, get := range map[string]func() ([]byte, error){
					"model":   func() ([]byte, error) { return v.Model("p2") },
					"frame":   func() ([]byte, error) { return v.Frame("p2", id) },
					"focus":   func() ([]byte, error) { return v.Focus("p2", id) },
					"export":  func() ([]byte, error) { return v.Export("p2") },
					"history": func() ([]byte, error) { return v.History("p2") },
				} {
					if _, err := get(); err != nil && !errors.Is(err, ErrPageNotFound) {
						t.Errorf("%s: %v", what, err)
					}
				}
			}
		})
	}
	for i := range 20 {
		if err := s.live.show("p2", string(p.diff.Root.UnionID()), i%2 == 0, ""); err != nil {
			t.Error(err)
		}
		s.commit()
	}
	close(stop)
	wg.Wait()
}

// TestExportReadsOnlyTheSnapshot exports from four goroutines while the
// request goroutine changes p1's record, evicts its view's record and
// finally evicts p1: every answer is a whole page of one committed
// history, a page limit error, or, once p1 is gone, ErrPageNotFound.
func TestExportReadsOnlyTheSnapshot(t *testing.T) {
	profiles := equalCPU(3)
	profiles["cpu.pb.gz"] = pmfixture.CPUBasic()
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxAttemptViews = 2
		c.Limits.MaxPublications = 2
	}, profiles)
	liveURL(t, s)
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
				body, err := s.PageSnapshot().Export("p1")
				_, tooLarge := errors.AsType[*PageLimitError](err)
				mu.Lock()
				switch {
				case err == nil:
					if !bytes.HasSuffix(bytes.TrimSpace(body), []byte("</html>")) {
						t.Errorf("a partial export: %d bytes", len(body))
					}
					bodies[sha256.Sum256(body)]++
				case tooLarge:
				case errors.Is(err, ErrPageNotFound):
					if !gone {
						t.Error("not found while p1 is kept")
					}
				default:
					t.Errorf("export: %v", err)
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

// An export that fails for a reason other than its size says so without
// sending the agent or the user after MaxPageBytes.
func TestAnExportThatFailsOtherwiseSaysSo(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	p := o.publish(t, s, o.explained(t, s, "b", "0.411"), "")
	broken := *p
	broken.id, broken.n, broken.result = "p2", 2, explain.Result{}
	if _, err := exportExplained(&broken, nil); err == nil || strings.Contains(err.Error(), "MaxPageBytes") {
		t.Errorf("a failed check's export error %v", err)
	}
	l := s.live
	l.pubs = append(l.pubs, &broken)
	s.commit()
	_, err := s.PageSnapshot().Export("p2")
	if _, tooLarge := errors.AsType[*PageLimitError](err); err == nil || tooLarge || errors.Is(err, ErrPageNotFound) || strings.Contains(err.Error(), "MaxPageBytes") {
		t.Errorf("the export of a broken publication: %v", err)
	}
}

// A diff with per-unit deltas in a long unit is charged for its derived
// unit strings: whatever the diff holds, its estimate covers, so the
// budget admits it only when it fits.
func TestADiffIsChargedForItsDerivedUnits(t *testing.T) {
	const depth, unitLen = 1500, 16 << 10
	frames := make([]pmfixture.Frame, depth)
	for i := range frames {
		frames[i] = pmfixture.Fn(fmt.Sprintf("f%d", i))
	}
	raw := pmfixture.Build("cpu", strings.Repeat("u", unitLen), nil, pmfixture.Sample{Frames: frames, Value: 1})
	s := newLiveServer(t, func(c *Config) {
		c.Limits.Model.MaxMetaBytes = unitLen
		c.Limits.MaxPageNodes = 1
		c.Limits.MaxPageBytes = 1 << 20
		c.Limits.MaxRetainedBytes = 8 << 20
	}, map[string][]byte{"x.pb.gz": raw})
	o := openLive(t, s, "x.pb.gz")
	q := diffQuery{BaseHandle: o.e.prof.Handle, BaseView: o.view, NewHandle: o.e.prof.Handle, NewView: o.view, WorkBase: new(1.0), WorkNew: new(2.0)}
	b, n := mustBoth(s.diffSides(q))(t)
	d := must(s.compare(q, b, n))(t)
	units := map[*byte]int{}
	stack := []*profilemodel.DiffNode{d.Root}
	for len(stack) > 0 {
		dn := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if dn.DeltaPerUnit != nil {
			units[unsafe.StringData(dn.DeltaPerUnit.Unit)] = len(dn.DeltaPerUnit.Unit)
		}
		stack = append(stack, dn.Children...)
	}
	var unitBytes int64
	for _, n := range units {
		unitBytes += int64(n)
	}
	if d.RetainedBytes() < unitBytes {
		t.Errorf("the diff is estimated at %d bytes while its %d derived unit strings hold %d", d.RetainedBytes(), len(units), unitBytes)
	}
	if _, err := s.publishDiff(d, b, n, "", "", ""); err != nil {
		t.Errorf("one shared derived unit fits the budget: %v", err)
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

// A server's live page is claimed once, by its one set of routes; a
// server without Page has none to claim.
func TestClaimPageOnce(t *testing.T) {
	cfg := testConfig(t)
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.ClaimPage(); err == nil || !strings.Contains(err.Error(), "Config.Page") {
		t.Errorf("ClaimPage without Page: %v", err)
	}
	live := newLiveServer(t, nil, nil)
	if err := live.ClaimPage(); err != nil {
		t.Fatalf("the first ClaimPage: %v", err)
	}
	for range 2 {
		if err := live.ClaimPage(); err == nil || !strings.Contains(err.Error(), "already served") {
			t.Errorf("a later ClaimPage: %v", err)
		}
	}
}
