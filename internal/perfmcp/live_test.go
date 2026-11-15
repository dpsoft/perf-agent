package perfmcp

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// newLiveServer is a server with the live page on that nothing serves:
// the test calls its methods itself, as its one request goroutine would.
// edit, when set, changes the config first; fixtures maps file names in
// the profiles root to their bytes.
func newLiveServer(t *testing.T, edit func(*Config), fixtures map[string][]byte) *Server {
	t.Helper()
	cfg := testConfig(t)
	cfg.Live = true
	if edit != nil {
		edit(&cfg)
	}
	for name, raw := range fixtures {
		writeFixture(t, cfg.ProfilesDir, name, raw)
	}
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// liveOpened is one profile opened in a live server, at its default view.
type liveOpened struct {
	e    *entry
	v    *profilemodel.View
	view string
}

func openLive(t *testing.T, s *Server, name string) liveOpened {
	t.Helper()
	e := must(s.store.open(name, ""))(t)
	view := defaultView(e.prof)
	_, v, err := s.store.view(e.prof.Handle, view)
	if err != nil {
		t.Fatal(err)
	}
	return liveOpened{e: e, v: v, view: view}
}

// node is the ID of the node of o's view whose function is named name.
func (o liveOpened) node(t *testing.T, name string) profilemodel.NodeID {
	t.Helper()
	var found []profilemodel.NodeID
	o.v.Walk(func(n *profilemodel.Node) bool {
		if n.Function.Name == name {
			found = append(found, n.ID)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("%d nodes named %q", len(found), name)
	}
	return found[0]
}

// check decodes and checks explanations, written as Go values, on o's
// view.
func (o liveOpened) check(t *testing.T, s *Server, exps []any) explain.Result {
	t.Helper()
	if exps == nil {
		exps = []any{}
	}
	decoded, _, err := explain.Decode(json.RawMessage(mustJSON(t, exps)), s.lim.Explain)
	if err != nil {
		t.Fatal(err)
	}
	return explain.Check(o.v, decoded, s.lim.Explain)
}

// explained is a passing check of node's share of the total, value v.
func (o liveOpened) explained(t *testing.T, s *Server, name, value string) explain.Result {
	t.Helper()
	r := o.check(t, s, shareClaim(string(o.node(t, name)), value, "{c1} of the time"))
	if !r.OK {
		t.Fatalf("check failed: %+v", r.Failures)
	}
	return r
}

// reject records a rejected attempt on o's view: a share claim on name
// with a wrong value.
func (o liveOpened) reject(t *testing.T, s *Server, name string) string {
	t.Helper()
	r := o.check(t, s, shareClaim(string(o.node(t, name)), "0.62", "{c1} of the time"))
	if r.OK {
		t.Fatal("a wrong value passed")
	}
	return s.attempts.add(o.e.prof, o.view, "", outcomeRejected, r.Failures)
}

func (o liveOpened) publish(t *testing.T, s *Server, r explain.Result, title string) *publication {
	t.Helper()
	p, err := s.publishExplained(o.e, o.v, o.view, r, title, "a0")
	if err != nil {
		t.Fatalf("publishExplained: %v", err)
	}
	return p
}

// facts publishes a facts-only page of o's view.
func (o liveOpened) facts(t *testing.T, s *Server) *publication {
	t.Helper()
	return o.publish(t, s, o.check(t, s, nil), "")
}

type stateEntry struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	TitleSegs []json.RawMessage `json:"title_segs"`
	View      string            `json:"view"`
	Digest    string            `json:"digest"`
	History   string            `json:"history"`
}

type stateJSON struct {
	Seq          string       `json:"seq"`
	Current      string       `json:"current"`
	Selected     string       `json:"selected"`
	Focus        string       `json:"focus"`
	Publications []stateEntry `json:"publications"`
}

func liveStateOf(t *testing.T, s *Server) stateJSON {
	t.Helper()
	var st stateJSON
	if err := json.Unmarshal(s.live.snapshot().state, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func ids(pubs []*publication) []string {
	out := []string{}
	for _, p := range pubs {
		out = append(out, p.id)
	}
	return out
}

func TestTheLiveAddressIsLoopbackOnAnyPort(t *testing.T) {
	ap, err := livehttp.CheckAddr(liveAddr)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Addr() != netip.MustParseAddr("127.0.0.1") || ap.Port() != 0 {
		t.Errorf("live address %s, want 127.0.0.1 on a port the kernel picks", ap)
	}
}

func TestTheZeroStateIsEmpty(t *testing.T) {
	s := newLiveServer(t, nil, nil)
	snap := s.live.snapshot()
	if got, want := string(snap.state), `{"seq":"0","current":"","selected":"","focus":"","publications":[]}`; got != want {
		t.Errorf("zero state = %s, want %s", got, want)
	}
	if snap.seq != 0 || len(snap.pubs) != 0 || len(snap.history) != 0 {
		t.Errorf("zero snapshot holds something: %+v", snap)
	}
}

func TestPublishMakesTheNewPublicationCurrent(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	p1 := o.publish(t, s, o.explained(t, s, "b", "0.411"), "First 1 look")
	first := liveStateOf(t, s)
	p2 := o.publish(t, s, o.explained(t, s, "c", "0.489"), "")
	st := liveStateOf(t, s)

	if p1.id != "p1" || p2.id != "p2" {
		t.Fatalf("ids %s, %s; want p1, p2", p1.id, p2.id)
	}
	if st.Current != "p2" || st.Selected != string(o.node(t, "c")) || st.Focus != "" {
		t.Errorf("state current %q selected %q focus %q; want p2, c's node, no focus", st.Current, st.Selected, st.Focus)
	}
	if first.Current != "p1" || first.Selected != string(o.node(t, "b")) {
		t.Errorf("first state current %q selected %q; want p1, b's node", first.Current, first.Selected)
	}
	if first.Seq != "1" || st.Seq != "2" {
		t.Errorf("seq %q then %q; want strictly increasing decimal strings 1, 2", first.Seq, st.Seq)
	}
	if !bytes.Contains(s.live.snapshot().state, []byte(`"seq":"2"`)) {
		t.Errorf("seq is not a JSON string: %s", s.live.snapshot().state)
	}
	if len(st.Publications) != 2 {
		t.Fatalf("publications %+v", st.Publications)
	}
	e := st.Publications[0]
	if e.ID != "p1" || e.Kind != "explained" || e.Title != "First 1 look" || e.View != o.view ||
		e.Digest != o.e.prof.Digest.String() || e.History != "1" {
		t.Errorf("p1 entry %+v", e)
	}
	if got, want := mustJSON(t, e.TitleSegs), string(flamegraph.LiveTitleSegments("First 1 look", "cpu.pb.gz")); got != want {
		t.Errorf("title_segs %s, want %s", got, want)
	}
	if !strings.Contains(mustJSON(t, e.TitleSegs), `"unchecked"`) {
		t.Errorf("the number in the title is not marked unchecked: %s", mustJSON(t, e.TitleSegs))
	}
	if got := mustJSON(t, st.Publications[1].TitleSegs); !strings.Contains(got, "cpu.pb.gz") {
		t.Errorf("an empty title is not the profile's name: %s", got)
	}
	if p1.attempt != "a0" || !bytes.Contains(p1.model, []byte(`"attempt":"a0"`)) {
		t.Errorf("the publication lost its attempt: %q", p1.attempt)
	}
}

func TestAFactsOnlyPublicationSelectsNothing(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	o.publish(t, s, o.explained(t, s, "b", "0.411"), "")
	if err := s.live.show("p1", string(o.node(t, "b")), true); err != nil {
		t.Fatal(err)
	}
	s.commit()
	o.facts(t, s)
	if st := liveStateOf(t, s); st.Current != "p2" || st.Selected != "" || st.Focus != "" {
		t.Errorf("state %+v; want p2 current, nothing selected or focused", st)
	}
}

func TestPublicationsAreImmutable(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	p1 := o.publish(t, s, o.explained(t, s, "b", "0.411"), "")
	before := bytes.Clone(p1.model)
	fields := *p1
	for range 4 {
		o.publish(t, s, o.explained(t, s, "c", "0.489"), "")
	}
	if err := s.live.show("p1", "", false); err != nil {
		t.Fatal(err)
	}
	o.reject(t, s, "b")
	if !s.refreshHistory() {
		t.Fatal("a new rejection did not move the history")
	}
	s.commit()
	if !bytes.Equal(p1.model, before) {
		t.Error("p1's model bytes changed")
	}
	if !reflect.DeepEqual(*p1, fields) {
		t.Error("p1's fields changed")
	}
	if s.live.snapshot().pubs["p1"] != p1 {
		t.Error("the snapshot does not hold p1 itself")
	}
}

func TestPublicationsAreBoundedByCount(t *testing.T) {
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxPublications = 2 }, map[string][]byte{
		"a.pb.gz": pmfixture.CPUBasic(), "b.pb.gz": pmfixture.CPUBasicNew(),
	})
	a, b := openLive(t, s, "a.pb.gz"), openLive(t, s, "b.pb.gz")
	p1 := a.facts(t, s)
	h1 := s.live.history["p1"]
	b.facts(t, s)
	b.facts(t, s)
	if got := ids(s.live.pubs); !slices.Equal(got, []string{"p2", "p3"}) {
		t.Errorf("kept %v, want [p2 p3]", got)
	}
	if _, ok := s.live.snapshot().pubs["p1"]; ok {
		t.Error("the snapshot still holds p1")
	}
	if _, ok := s.live.history["p1"]; ok {
		t.Error("p1's history is still kept")
	}
	if s.pins.heldBy(p1) != 0 || s.pins.holders(p1) != 0 || s.pins.holders(h1) != 0 {
		t.Errorf("p1's pins were not released: heldBy %d", s.pins.heldBy(p1))
	}
	if got := s.pins.holders(a.e.prof); got != 1 {
		t.Errorf("a's profile has %d holders, want 1 (the store)", got)
	}
}

func TestALargeProfileWithATinyModelIsChargedForItsEngine(t *testing.T) {
	if testing.Short() {
		t.Skip("loads a 100k-sample profile")
	}
	wide := map[string][]byte{"wide.pb.gz": pmfixture.Wide(100_000)}
	small := func(budget int64) func(*Config) {
		return func(c *Config) {
			c.Limits.MaxPageNodes = 50
			c.Limits.MaxPageBytes = 64 << 10
			c.Limits.MaxRetainedBytes = budget
		}
	}

	s := newLiveServer(t, small(1<<20), wide)
	o := openLive(t, s, "wide.pb.gz")
	if o.e.prof.RetainedBytes() <= s.lim.MaxRetainedBytes {
		t.Fatalf("the profile's estimate %d is not over the budget %d", o.e.prof.RetainedBytes(), s.lim.MaxRetainedBytes)
	}
	before := s.live.snapshot()
	_, err := s.publishExplained(o.e, o.v, o.view, o.check(t, s, nil), "", "a0")
	if err == nil || !strings.Contains(err.Error(), "MaxRetainedBytes=1048576") || !strings.Contains(err.Error(), "--limit MaxRetainedBytes=N") {
		t.Fatalf("publish over the budget: %v", err)
	}
	if s.live.snapshot() != before || len(s.live.pubs) != 0 || s.live.next != 0 {
		t.Error("a refused publication changed the live state")
	}

	s = newLiveServer(t, small(0), wide)
	o = openLive(t, s, "wide.pb.gz")
	p := o.facts(t, s)
	if len(p.model) >= 64<<10 {
		t.Fatalf("the model is %d bytes, not tiny", len(p.model))
	}
	if got, want := s.pins.heldBy(p), o.e.prof.RetainedBytes()+o.v.RetainedBytes(); got < want {
		t.Errorf("heldBy(p1) = %d, less than its profile and view, %d", got, want)
	}
}

func TestPinsAreSharedWithTheStore(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	storeOnly := s.pins.total
	p1, p2 := o.facts(t, s), o.facts(t, s)
	h1, h2 := s.live.history["p1"], s.live.history["p2"]
	engine := o.e.prof.RetainedBytes() + o.v.RetainedBytes()
	pages := p1.RetainedBytes() + p2.RetainedBytes() + h1.RetainedBytes() + h2.RetainedBytes()
	if got := s.pins.heldBy(p1, p2); got != engine+pages {
		t.Errorf("heldBy(p1, p2) = %d, want %d: the profile and view count once", got, engine+pages)
	}
	if got := s.pins.heldBy(p1, p2, o.e); got != engine+pages {
		t.Errorf("heldBy(p1, p2, store entry) = %d, want %d", got, engine+pages)
	}
	if storeOnly != engine || s.pins.total != engine+pages {
		t.Errorf("ledger total %d before and %d after; want %d and %d", storeOnly, s.pins.total, engine, engine+pages)
	}
	if got := s.pins.holders(o.e.prof); got != 3 {
		t.Errorf("the profile has %d holders, want 3 (the store, p1 and p2)", got)
	}
}

func TestStoreEvictionWhilePublishedKeepsOnePin(t *testing.T) {
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxOpenHandles = 1
		c.Limits.MaxPublications = 1
	}, map[string][]byte{"a.pb.gz": pmfixture.CPUBasic(), "b.pb.gz": pmfixture.CPUBasicNew()})
	a := openLive(t, s, "a.pb.gz")
	p1 := a.publish(t, s, a.explained(t, s, "b", "0.411"), "")
	b := openLive(t, s, "b.pb.gz")
	if _, ok := s.store.entries[a.e.prof.Handle]; ok {
		t.Fatal("a is still open")
	}
	if got := s.pins.holders(a.e.prof); got != 1 {
		t.Errorf("a's profile has %d holders after the store let it go, want 1 (p1)", got)
	}
	if got := s.pins.holders(a.v); got != 1 {
		t.Errorf("a's view has %d holders, want 1", got)
	}
	if _, err := flamegraph.ExplainedLiveFrame(p1.v, p1.result, a.node(t, "b"), p1.opts.Digest); err != nil {
		t.Errorf("p1 no longer serves its view: %v", err)
	}

	b.facts(t, s)
	if got := s.pins.holders(a.e.prof); got != 0 {
		t.Errorf("a's profile has %d holders after p1 was evicted, want 0", got)
	}
	if _, ok := s.pins.objs[a.e.prof]; ok {
		t.Error("a's profile is still in the ledger")
	}
	if _, ok := s.pins.objs[a.v]; ok {
		t.Error("a's view is still in the ledger")
	}
}

// equalCPU are n profiles of the same shape and size whose bytes, and so
// digests, differ.
func equalCPU(n int) map[string][]byte {
	out := map[string][]byte{}
	for i := range n {
		name := string(rune('a'+i)) + ".pb.gz"
		out[name] = pmfixture.Build("cpu", "nanoseconds", []string{"copy=" + string(rune('a'+i))},
			pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("a"), pmfixture.Fn("b")}, Value: 411},
			pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("c")}, Value: 589},
		)
	}
	return out
}

// pubCost is what one facts-only publication of each of profiles costs in
// a server with a large budget: the most of any of them.
func pubCost(t *testing.T, profiles map[string][]byte) int64 {
	t.Helper()
	s := newLiveServer(t, nil, profiles)
	var cost int64
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		o := openLive(t, s, name)
		cost = max(cost, s.pins.heldBy(o.facts(t, s)))
	}
	return cost
}

func TestRetainedBudgetEvictsOlderPublications(t *testing.T) {
	profiles := equalCPU(3)
	cost := pubCost(t, profiles)
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxRetainedBytes = 2*cost + cost/2
		c.Limits.MaxPageBytes = int(cost)
	}, profiles)
	a, b, c := openLive(t, s, "a.pb.gz"), openLive(t, s, "b.pb.gz"), openLive(t, s, "c.pb.gz")
	a.facts(t, s)
	b.facts(t, s)
	c.facts(t, s)
	if got := ids(s.live.pubs); !slices.Equal(got, []string{"p2", "p3"}) {
		t.Fatalf("kept %v, want [p2 p3]: the least recently shown is evicted", got)
	}
	if err := s.live.show("p2", "", false); err != nil {
		t.Fatal(err)
	}
	s.commit()
	a.facts(t, s)
	if got := ids(s.live.pubs); !slices.Equal(got, []string{"p2", "p4"}) {
		t.Errorf("kept %v, want [p2 p4]: p3 was shown least recently", got)
	}
	if st := liveStateOf(t, s); st.Current != "p4" {
		t.Errorf("current %q, want p4", st.Current)
	}
	if got := s.pins.heldBy(s.live.pubs[0], s.live.pubs[1]); got > s.lim.MaxRetainedBytes {
		t.Errorf("kept publications hold %d, over the budget %d", got, s.lim.MaxRetainedBytes)
	}
}

// liveDump is everything admission may change.
type liveDump struct {
	pubs                     []*publication
	history                  map[string]*liveHistory
	next                     int
	seq                      int64
	current, selected, focus string
	snap                     *liveSnapshot
	objs                     map[retained]pin
	held                     map[any][]retained
	total                    int64
}

func dumpLive(s *Server) liveDump {
	l := s.live
	d := liveDump{
		pubs: slices.Clone(l.pubs), history: maps.Clone(l.history), next: l.next, seq: l.seq,
		current: l.current, selected: l.selected, focus: l.focus, snap: l.snapshot(),
		objs: map[retained]pin{}, held: map[any][]retained{}, total: s.pins.total,
	}
	for o, p := range s.pins.objs {
		d.objs[o] = *p
	}
	for h, objs := range s.pins.held {
		d.held[h] = slices.Clone(objs)
	}
	return d
}

func TestARefusedAdmissionChangesNothing(t *testing.T) {
	profiles := equalCPU(2)
	cost := pubCost(t, profiles)
	profiles["deep.pb.gz"] = pmfixture.Deep(5000)
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxRetainedBytes = 3 * cost
		c.Limits.MaxPageBytes = int(cost)
	}, profiles)
	a, b, deep := openLive(t, s, "a.pb.gz"), openLive(t, s, "b.pb.gz"), openLive(t, s, "deep.pb.gz")
	a.publish(t, s, a.explained(t, s, "b", "0.411"), "")
	b.facts(t, s)
	if err := s.live.show("p1", string(a.node(t, "b")), true); err != nil {
		t.Fatal(err)
	}
	s.commit()
	before := dumpLive(s)

	_, err := s.publishExplained(deep.e, deep.v, deep.view, deep.check(t, s, nil), "", "a9")
	if err == nil || !strings.Contains(err.Error(), "MaxRetainedBytes=") || !strings.Contains(err.Error(), "in memory (profile, view and page model)") {
		t.Fatalf("publish over the budget alone: %v", err)
	}
	if after := dumpLive(s); !reflect.DeepEqual(before, after) {
		t.Errorf("a refused admission changed the live state:\nbefore %+v\nafter  %+v", before, after)
	}
	if st := liveStateOf(t, s); st.Current != "p1" || len(st.Publications) != 2 {
		t.Errorf("state %+v", st)
	}
}

func TestSnapshotHistoryIsACopy(t *testing.T) {
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxAttemptViews = 1 }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	id := o.reject(t, s, "b")
	o.facts(t, s)
	h := s.live.snapshot().history["p1"]
	if h == nil || len(h.rejected) != 1 || len(h.rejected[0].Failures) == 0 {
		t.Fatalf("history %+v", h)
	}
	kept := s.attempts.byID[id].failures.kept
	if &h.rejected[0].Failures[0] == &kept[0] {
		t.Error("the snapshot shares the record's failures slice")
	}
	if e := h.rejected[0].Failures[0].Engine; e == nil || e == kept[0].Engine {
		t.Error("the snapshot shares the record's engine value")
	}
	rows, js := mustJSON(t, h.rejected), bytes.Clone(h.json)

	o.reject(t, s, "c")
	s.attempts.add(o.e.prof, "other/view#9", "", outcomeRejected, nil)
	if _, ok := s.attempts.views[viewKey{o.e.prof.Digest, o.view}]; ok {
		t.Fatal("p1's record was not evicted")
	}
	if got := mustJSON(t, h.rejected); got != rows {
		t.Errorf("the snapshot's rows changed:\n%s\n%s", rows, got)
	}
	if !bytes.Equal(h.json, js) {
		t.Error("the snapshot's history JSON changed")
	}
}

func TestHistoryVersionMovesOnlyWhenTheRecordChanges(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	o.facts(t, s)
	version := func() int64 { return s.live.history["p1"].version }
	if version() != 1 {
		t.Fatalf("a new publication's history is version %d, want 1", version())
	}
	for range 2 {
		if s.refreshHistory() || version() != 1 {
			t.Errorf("no new attempt moved the version to %d", version())
		}
	}
	old := s.live.history["p1"]
	o.reject(t, s, "b")
	if !s.refreshHistory() || version() != 2 {
		t.Errorf("a rejection of p1's view left the version at %d", version())
	}
	if s.pins.holders(old) != 0 || s.pins.holders(s.live.history["p1"]) != 1 {
		t.Error("the ledger holds the old history instead of the new one")
	}
	s.commit()
	if st := liveStateOf(t, s); st.Publications[0].History != "2" {
		t.Errorf("state history %q, want 2", st.Publications[0].History)
	}
	s.attempts.add(o.e.prof, "other/view#9", "", outcomeRejected, nil)
	s.attempts.add(o.e.prof, o.view, "", outcomeInvalid, nil)
	if s.refreshHistory() || version() != 2 {
		t.Errorf("a rejection on another view or a non-rejection moved p1 to %d", version())
	}
}

func TestHistoryCountsInTheBudget(t *testing.T) {
	profiles := equalCPU(2)
	cost := pubCost(t, profiles)
	s := newLiveServer(t, func(c *Config) {
		c.Limits.MaxRetainedBytes = 2*cost + cost/4
		c.Limits.MaxPageBytes = int(cost)
	}, profiles)
	a, b := openLive(t, s, "a.pb.gz"), openLive(t, s, "b.pb.gz")
	a.facts(t, s)
	b.facts(t, s)
	if got := ids(s.live.pubs); !slices.Equal(got, []string{"p1", "p2"}) {
		t.Fatalf("kept %v", got)
	}
	for range 20 {
		a.reject(t, s, "b")
		b.reject(t, s, "b")
	}
	if !s.refreshHistory() {
		t.Fatal("the history did not move")
	}
	h := s.live.history["p2"]
	if got := s.pins.heldBy(s.live.pubs[len(s.live.pubs)-1]); got < int64(len(h.json)) {
		t.Errorf("p2 holds %d, less than its %d-byte history", got, len(h.json))
	}
	if got := ids(s.live.pubs); !slices.Equal(got, []string{"p2"}) {
		t.Errorf("kept %v, want [p2]: the grown histories go over the budget, and the current one stays", got)
	}
}

func TestNoLiveWithoutTheFlag(t *testing.T) {
	cfg := testConfig(t)
	s := must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	if s.live != nil {
		t.Error("a server without Live has live state")
	}
	cfg.Live = true
	s = must(New(cfg))(t)
	t.Cleanup(func() { _ = s.Close() })
	if s.live == nil {
		t.Error("a server with Live has no live state")
	}
}

func TestLimitsValidateTheRetainedBudget(t *testing.T) {
	cfg := testConfig(t)
	cfg.Live = true
	cfg.Limits.MaxPageBytes = 1 << 20
	cfg.Limits.MaxRetainedBytes = 1<<20 - 1
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "MaxRetainedBytes must be at least MaxPageBytes") ||
		!strings.Contains(err.Error(), "1048575") || !strings.Contains(err.Error(), "1048576") {
		t.Errorf("New with MaxRetainedBytes < MaxPageBytes: %v", err)
	}

	cfg.Live = false
	s, err := New(cfg)
	if err != nil {
		t.Errorf("without Live the budget is not checked against MaxPageBytes: %v", err)
	} else {
		_ = s.Close()
	}

	cfg.Limits.MaxRetainedBytes = -1
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "MaxRetainedBytes must be positive") {
		t.Errorf("New with a negative MaxRetainedBytes: %v", err)
	}

	lim := (Limits{MaxRows: 1}).withDefaults()
	if lim.MaxPublications != 8 || lim.MaxRetainedBytes != 2<<30 || lim.MaxPageClients != 8 {
		t.Errorf("defaults %d, %d, %d; want 8, 2 GiB, 8", lim.MaxPublications, lim.MaxRetainedBytes, lim.MaxPageClients)
	}
	if d := DefaultLimits(); d.MaxPublications != 8 || d.MaxRetainedBytes != 2<<30 || d.MaxPageClients != 8 {
		t.Errorf("DefaultLimits %d, %d, %d", d.MaxPublications, d.MaxRetainedBytes, d.MaxPageClients)
	}
}

// TestCloseIsSafeDuringAPublish closes the server from another goroutine
// while the request goroutine publishes, as a forced exit does.
func TestCloseIsSafeDuringAPublish(t *testing.T) {
	s := newLiveServer(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	r := o.explained(t, s, "b", "0.411")
	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Go(func() {
		for i := range 50 {
			if i == 5 {
				close(started)
			}
			if _, err := s.publishExplained(o.e, o.v, o.view, r, "", "a0"); err != nil {
				t.Errorf("publish %d: %v", i, err)
				return
			}
		}
	})
	<-started
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	wg.Wait()
	if !s.live.isClosed() {
		t.Error("Close did not mark the live state closed")
	}
	s.live.close()
	if !s.live.isClosed() {
		t.Error("a second close reopened the live state")
	}
}
