package perfmcp

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// liveAddr is where the live page listens: the IPv4 loopback, on a port
// the kernel picks. livehttp.Listen checks it is loopback.
const liveAddr = "127.0.0.1:0"

// The live page's stream: a keep-alive comment after this long without an
// event, and this long for each write before the stream is dropped.
const (
	liveKeepAlive    = 15 * time.Second
	liveWriteTimeout = 10 * time.Second
)

// pubKind is what a publication shows.
type pubKind string

const (
	pubExplained pubKind = "explained" // a checked explanation of a view
	pubDiff      pubKind = "diff"      // a comparison of two views
)

// publication is one page the live page can show. It is complete before
// the snapshot holds it, and never changes after: HTTP handlers read it
// from any goroutine. It pins the engine objects its routes compute from,
// its Profile and View, so it stays valid after the store lets them go.
type publication struct {
	id   string // "p<n>"
	n    int
	kind pubKind
	// title is the agent's title, "" for none; titleSegs is the heading
	// the page lists the publication under, as the model's title_segs.
	title     string
	titleSegs json.RawMessage
	view      string
	digest    profilemodel.Digest
	// attempt is the attempt the publication was made for.
	attempt string

	prof   *profilemodel.Profile
	v      *profilemodel.View
	result explain.Result

	opts  flamegraph.LiveOptions
	model []byte
}

// publicationOverhead is what a publication costs besides its model: its
// fields and its entry in the state.
const publicationOverhead = 1 << 10

// RetainedBytes is the bytes the publication holds besides the objects it
// pins: its model and its title.
func (p *publication) RetainedBytes() int64 {
	return int64(len(p.model)+len(p.titleSegs)+len(p.title)) + publicationOverhead
}

// pinned is everything p holds in the ledger with h, its history (nil for
// none).
func (p *publication) pinned(h *liveHistory) []retained {
	out := []retained{p}
	if p.prof != nil {
		out = append(out, p.prof)
	}
	if p.v != nil {
		out = append(out, p.v)
	}
	if h != nil {
		out = append(out, h)
	}
	return out
}

// liveHistory is the rejected attempts of an explained publication's
// profile and view, as the server's record held them at version. rejected
// is a deep copy, so nothing the record does later reaches it, and json is
// the page's history built from it. It never changes once built.
type liveHistory struct {
	version    int64
	rejected   []explain.RejectedAttempt
	dropped    int
	incomplete bool
	json       []byte
}

// RetainedBytes is the history's JSON and its copy of the rejected
// attempts, measured as the attempt record measures them.
func (h *liveHistory) RetainedBytes() int64 {
	n := int64(len(h.json))
	for _, ra := range h.rejected {
		n += attemptOverhead + int64(len(ra.Attempt)+len(ra.RetryOf)+len(ra.Evidence))
		for _, f := range ra.Failures {
			n += failureOverhead + int64(failureBytes(f))
		}
	}
	return n
}

// same reports whether h holds exactly this record.
func (h *liveHistory) same(rejected []explain.RejectedAttempt, dropped int, incomplete bool) bool {
	return h.dropped == dropped && h.incomplete == incomplete &&
		slices.EqualFunc(h.rejected, rejected, func(a, b explain.RejectedAttempt) bool { return reflect.DeepEqual(a, b) })
}

// copyRejected is rejected with every failure copied, its strings and
// engine value included, so a history never shares memory with the
// record. A nil failures list stays nil.
func copyRejected(rejected []explain.RejectedAttempt) []explain.RejectedAttempt {
	out := make([]explain.RejectedAttempt, len(rejected))
	for i, ra := range rejected {
		out[i] = ra
		if ra.Failures != nil {
			out[i].Failures = make([]explain.Failure, len(ra.Failures))
			for j, f := range ra.Failures {
				out[i].Failures[j] = cloneFailure(f)
			}
		}
	}
	return out
}

// liveSnapshot is what the live page's HTTP handlers read: the steering,
// the kept publications and their histories, and the state JSON every page
// is sent. A snapshot never changes once stored; commit replaces it.
type liveSnapshot struct {
	seq                      int64
	current, selected, focus string
	pubs                     map[string]*publication
	// order lists the publications as the state does, oldest first.
	order   []string
	history map[string]*liveHistory
	state   []byte
}

// live is the live page's state.
//
// The request goroutine alone changes the publications, the steering and
// the snapshot: every field below snap is its own, and commit, the only
// writer of snap, runs on it. HTTP handlers read only what snap points to.
// The listener's lifecycle is the exception: Close may end it from another
// goroutine (a forced exit), so it is behind mu.
type live struct {
	snap atomic.Pointer[liveSnapshot]

	// pubs are the kept publications, least recently shown first; the
	// current one is last. history is the history of each explained one.
	pubs    []*publication
	history map[string]*liveHistory
	// next is the number of the last publication made.
	next int
	seq  int64
	// current, selected and focus are what the page is told to show: a
	// publication, a node of it ("" for none) and the node its layout
	// starts at ("" for the whole view).
	current, selected, focus string

	hub  *livehttp.Hub
	lim  Limits
	pins *pins

	mu sync.Mutex
	// closed is set by close; nothing starts after it.
	closed bool
}

func newLive(lim Limits, pins *pins) *live {
	l := &live{
		history: map[string]*liveHistory{},
		hub:     livehttp.NewHub(lim.MaxPageClients, liveKeepAlive, liveWriteTimeout),
		lim:     lim,
		pins:    pins,
	}
	snap := &liveSnapshot{pubs: map[string]*publication{}, history: map[string]*liveHistory{}}
	snap.state = stateOf(snap)
	l.snap.Store(snap)
	return l
}

// snapshot is the latest committed snapshot. Any goroutine may call it.
func (l *live) snapshot() *liveSnapshot { return l.snap.Load() }

// close stops the live page. It may run on any goroutine, more than once.
func (l *live) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.hub.Close()
}

// isClosed reports whether close has run.
func (l *live) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// show makes publication id current, with node selected and, when focus
// is set, the layout starting at node. It checks only that id is kept;
// whether node may be shown is the caller's to check. Nothing reaches the
// page until commit.
func (l *live) show(id, node string, focus bool) error {
	i := slices.IndexFunc(l.pubs, func(p *publication) bool { return p.id == id })
	if i < 0 {
		return fmt.Errorf("publication %q is not kept", echo(id))
	}
	p := l.pubs[i]
	l.pubs = append(slices.Delete(l.pubs, i, i+1), p)
	l.current, l.selected, l.focus = id, node, ""
	if focus {
		l.focus = node
	}
	return nil
}

// publishExplained publishes view v (named view) of e's profile with the
// passing check r, titled title, made for attempt. The model is built
// within the page limits; one that cannot be built publishes nothing. An
// admitted publication becomes current with its first explained node
// selected (none for a facts-only page) and no focus, and the page is
// told.
func (s *Server) publishExplained(e *entry, v *profilemodel.View, view string, r explain.Result, title, attempt string) (*publication, error) {
	opts := flamegraph.LiveOptions{
		Title: title, ProfileName: e.name, Digest: e.prof.Digest.String(), Attempt: attempt,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	}
	model, err := flamegraph.ExplainedLiveModel(v, r, opts)
	if err != nil {
		return nil, err
	}
	p := &publication{
		kind: pubExplained, title: title, titleSegs: flamegraph.LiveTitleSegments(title, e.name),
		view: view, digest: e.prof.Digest, attempt: attempt,
		prof: e.prof, v: v, result: r, opts: opts, model: model,
	}
	if err := s.admit(p); err != nil {
		return nil, err
	}
	l := s.live
	l.selected, l.focus = "", ""
	if len(r.Checked) > 0 {
		l.selected = string(r.Checked[0].Node)
	}
	s.commit()
	return p, nil
}

// admit keeps p as the current publication, with its history. It first
// tries the change on a copy: it drops the least recently shown other
// publications while more than MaxPublications would be kept, or while
// the estimated memory of what is kept and p goes over MaxRetainedBytes.
// Only if that fits does it drop them, number p and hold its pins. A
// publication that does not fit even alone is refused, and nothing
// changes.
func (s *Server) admit(p *publication) error {
	l := s.live
	n := l.next + 1
	id := "p" + strconv.Itoa(n)
	var h *liveHistory
	if p.kind == pubExplained {
		rejected, dropped, incomplete := s.attempts.rejected(p.prof, p.view)
		h = s.newHistory(p.v, id, 1, rejected, dropped, incomplete)
	}
	objs := p.pinned(h)
	cost := func(kept []*publication) int64 {
		all := slices.Clone(objs)
		for _, k := range kept {
			all = append(all, k.pinned(l.history[k.id])...)
		}
		return l.pins.estimate(all...)
	}
	kept := l.pubs
	for len(kept) > 0 && (len(kept)+1 > s.lim.MaxPublications || cost(kept) > s.lim.MaxRetainedBytes) {
		kept = kept[1:]
	}
	if len(kept) == 0 && cost(nil) > s.lim.MaxRetainedBytes {
		return fmt.Errorf("the page would keep %s in memory (profile, view and page model), over MaxRetainedBytes=%d; "+
			"publish a smaller view or restart perf-agent-mcp with a larger --limit MaxRetainedBytes=N",
			strconv.FormatInt(cost(nil), 10)+" bytes", s.lim.MaxRetainedBytes)
	}

	for _, d := range l.pubs[:len(l.pubs)-len(kept)] {
		l.drop(d)
	}
	p.id, p.n = id, n
	l.next = n
	l.pins.hold(p, objs...)
	if h != nil {
		l.history[id] = h
	}
	l.pubs = append(slices.Clone(kept), p)
	l.current = id
	return nil
}

// drop lets go of publication p and its history.
func (l *live) drop(p *publication) {
	l.pins.release(p)
	delete(l.history, p.id)
}

// newHistory is the history of a publication id of view v, at version,
// from the record's rejected attempts.
func (s *Server) newHistory(v *profilemodel.View, id string, version int64, rejected []explain.RejectedAttempt, dropped int, incomplete bool) *liveHistory {
	rejected = copyRejected(rejected)
	return &liveHistory{
		version: version, rejected: rejected, dropped: dropped, incomplete: incomplete,
		json: flamegraph.ExplainedLiveHistory(v, id, strconv.FormatInt(version, 10), rejected, dropped, incomplete, s.lim.MaxPageBytes),
	}
}

// refreshHistory rebuilds, at the next version, the history of every kept
// explained publication whose record of rejected attempts changed, and
// reports whether any did. A grown history counts against
// MaxRetainedBytes: while the kept publications go over it, the least
// recently shown ones other than the current one are dropped. The page is
// told at the next commit.
func (s *Server) refreshHistory() bool {
	l := s.live
	moved := false
	for _, p := range l.pubs {
		if p.kind != pubExplained {
			continue
		}
		old := l.history[p.id]
		rejected, dropped, incomplete := s.attempts.rejected(p.prof, p.view)
		if old != nil && old.same(rejected, dropped, incomplete) {
			continue
		}
		var version int64 = 1
		if old != nil {
			version = old.version + 1
		}
		h := s.newHistory(p.v, p.id, version, rejected, dropped, incomplete)
		l.pins.release(p)
		l.pins.hold(p, p.pinned(h)...)
		l.history[p.id] = h
		moved = true
	}
	if !moved {
		return false
	}
	for len(l.pubs) > 1 && l.heldByKept() > s.lim.MaxRetainedBytes {
		i := slices.IndexFunc(l.pubs, func(p *publication) bool { return p.id != l.current })
		l.drop(l.pubs[i])
		l.pubs = slices.Delete(l.pubs, i, i+1)
	}
	return true
}

// heldByKept is what the kept publications hold.
func (l *live) heldByKept() int64 {
	holders := make([]any, len(l.pubs))
	for i, p := range l.pubs {
		holders[i] = p
	}
	return l.pins.heldBy(holders...)
}

// commit stores a new snapshot of the live state, at the next seq, and
// sends its state to every page. It is the only writer of the snapshot,
// and runs on the request goroutine.
func (s *Server) commit() {
	l := s.live
	l.seq++
	snap := &liveSnapshot{
		seq: l.seq, current: l.current, selected: l.selected, focus: l.focus,
		pubs: make(map[string]*publication, len(l.pubs)), history: maps.Clone(l.history),
	}
	for _, p := range slices.SortedFunc(slices.Values(l.pubs), func(a, b *publication) int { return cmp.Compare(a.n, b.n) }) {
		snap.pubs[p.id] = p
		snap.order = append(snap.order, p.id)
	}
	snap.state = stateOf(snap)
	l.snap.Store(snap)
	// The state is compact JSON from encoding/json: never empty and never
	// more than one line, the only data Publish refuses.
	_ = l.hub.Publish(snap.state)
}

// stateEntry is a publication as the state lists it. history is its
// history's version, "0" for a publication without one.
type liveStateEntry struct {
	ID        string          `json:"id"`
	Kind      pubKind         `json:"kind"`
	Title     string          `json:"title"`
	TitleSegs json.RawMessage `json:"title_segs"`
	View      string          `json:"view"`
	Digest    string          `json:"digest"`
	History   string          `json:"history"`
}

type liveState struct {
	Seq          string           `json:"seq"`
	Current      string           `json:"current"`
	Selected     string           `json:"selected"`
	Focus        string           `json:"focus"`
	Publications []liveStateEntry `json:"publications"`
}

// stateOf is snap's state JSON: what the page shows and the publications
// it can switch to. Integers are decimal strings.
func stateOf(snap *liveSnapshot) []byte {
	st := liveState{
		Seq: strconv.FormatInt(snap.seq, 10), Current: snap.current, Selected: snap.selected, Focus: snap.focus,
		Publications: []liveStateEntry{},
	}
	for _, id := range snap.order {
		p := snap.pubs[id]
		e := liveStateEntry{ID: p.id, Kind: p.kind, Title: p.title, TitleSegs: p.titleSegs, View: p.view, History: "0"}
		if p.prof != nil {
			e.Digest = p.digest.String()
		}
		if h := snap.history[id]; h != nil {
			e.History = strconv.FormatInt(h.version, 10)
		}
		st.Publications = append(st.Publications, e)
	}
	out, err := json.Marshal(st)
	if err != nil {
		// Strings and raw JSON the server built always encode.
		panic(err)
	}
	return out
}
