package perfmcp

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// pubKind is what a publication shows.
type pubKind string

const (
	pubExplained pubKind = "explained" // a checked explanation of a view
	pubDiff      pubKind = "diff"      // a comparison of two views
)

// publication is one page the live page can show. It is complete before
// the snapshot holds it, and never changes after: a PageView reads it
// from any goroutine. It pins the engine objects its routes compute from,
// so it stays valid after the store lets them go: an explained one its
// Profile and View, a diff its Diff, the Diff's index and both sides'
// Profiles and Views, which the Diff's nodes point into.
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
	// resultSize is resultBytes(result), counted when the publication is
	// built: the frame and focus routes recompute from the result, so it
	// stays in memory as long as the publication does.
	resultSize int64

	opts  flamegraph.LiveOptions
	model []byte

	// A diff's: prof and v above are its base side, newProf and newV its
	// new side. index finds any node of the union; node is the node
	// page_diff selected, "" for none. diffOpts are its model's options,
	// with the measure taken once when it was published, and export the
	// page options of its static copy, which that measure was taken with.
	diff    *profilemodel.Diff
	index   *profilemodel.DiffIndex
	newProf *profilemodel.Profile
	newV    *profilemodel.View
	node    string

	diffOpts flamegraph.DiffLiveOptions
	export   flamegraph.DiffPageOptions
}

// publicationOverhead is what a publication costs besides its model and
// its checked result: its fields and its entry in the state.
const publicationOverhead = 1 << 10

// RetainedBytes is the bytes the publication holds besides the objects it
// pins: its model, its title and its checked result.
func (p *publication) RetainedBytes() int64 {
	return int64(len(p.model)+len(p.titleSegs)+len(p.title)) + p.resultSize + publicationOverhead
}

// Weights of resultBytes, in heap bytes per item: an explain.Checked is
// 1808 bytes, a Segment 64 and a CheckedClaim 168. An explanation also
// holds its facts' values and exact shares, and a claim its engine value's
// strings.
const (
	resultPerExplanation = 1808 + 512
	resultPerPathEntry   = 16
	resultPerClaim       = 168 + 64
	resultPerHypothesis  = 24
	resultPerSegment     = 64
)

// resultBytes estimates the heap a checked result keeps, from its counts:
// its explanations, their facts' paths, notes and sample periods, their
// claims, and their text segments with the text they hold. Slices count at
// their capacity. Path names are the view's own strings, so they count as
// slice entries only.
func resultBytes(r explain.Result) int64 {
	n := int64(resultPerExplanation) * int64(cap(r.Checked))
	segs := func(ss []explain.Segment) {
		n += resultPerSegment * int64(cap(ss))
		for _, sg := range ss {
			n += int64(len(sg.Text))
		}
	}
	for _, c := range r.Checked {
		n += resultPerPathEntry * int64(cap(c.Facts.Path)+cap(c.Facts.Notes)+cap(c.Facts.SamplePeriods))
		for _, s := range slices.Concat(c.Facts.Notes, c.Facts.SamplePeriods) {
			n += int64(len(s))
		}
		n += resultPerClaim * int64(cap(c.Claims))
		segs(c.Prose)
		segs(c.NextMeasurement)
		n += resultPerHypothesis * int64(cap(c.Hypotheses))
		for _, h := range c.Hypotheses {
			segs(h)
		}
	}
	return n
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
	if p.diff != nil {
		// A diff has all four.
		out = append(out, p.diff, p.index, p.newProf, p.newV)
	}
	if h != nil {
		out = append(out, h)
	}
	return out
}

// liveHistory is the rejected attempts of an explained publication's
// profile and view, as the server's record held them at version, and json,
// the page's history built from them. rejected is a copy that shares no
// memory with the record, so nothing the record does later reaches it, and
// it keeps only what json shows: the newest failures that fit, with the
// older ones counted on their attempts (Omitted, OmittedClaims,
// OmittedReferences) and, when even that is too large, the oldest
// attempts counted in dropped. The pills therefore still count every
// claim and attempt the record holds. Copy and JSON together keep at most
// about MaxPageBytes. record and recordDropped say which record the
// history was built from. It never changes once built.
type liveHistory struct {
	version    int64
	rejected   []explain.RejectedAttempt
	dropped    int
	incomplete bool
	json       []byte

	record        []string // the IDs of the record's rejected attempts
	recordDropped int
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
	return n + recordBytes(h.record)
}

// same reports whether h was built from this record. A recorded attempt
// never changes, so its ID stands for it.
func (h *liveHistory) same(rejected []explain.RejectedAttempt, dropped int, incomplete bool) bool {
	return h.recordDropped == dropped && h.incomplete == incomplete &&
		slices.EqualFunc(h.record, rejected, func(id string, ra explain.RejectedAttempt) bool { return id == ra.Attempt })
}

// trimRejected copies the newest keepAttempts of rejected with, of their
// failures, only the newest keepFailures in all (in page order: the last
// failure of the last attempt is the newest). Every copied string and
// engine value is new. A failure left out is counted on its attempt as
// the record counts the failures it does not keep, so a page still
// counts it; the attempts left out are returned as a count.
func trimRejected(rejected []explain.RejectedAttempt, keepFailures, keepAttempts int) ([]explain.RejectedAttempt, int) {
	keepAttempts = min(keepAttempts, len(rejected))
	from := len(rejected) - keepAttempts
	out := make([]explain.RejectedAttempt, keepAttempts)
	// named is what failureKey names, without building its string; one
	// map serves every attempt, so it holds at most one attempt's
	// failures, which the record bounds.
	type key struct {
		claim, ref string
		code       explain.Code
	}
	keyOf := func(f explain.Failure) key {
		if f.Claim != "" {
			return key{claim: f.Claim}
		}
		return key{ref: f.Ref, code: f.Code}
	}
	named := map[key]bool{}
	for i := keepAttempts - 1; i >= 0; i-- {
		ra := rejected[from+i]
		m := min(keepFailures, len(ra.Failures))
		keepFailures -= m
		kept, left := ra.Failures[len(ra.Failures)-m:], ra.Failures[:len(ra.Failures)-m]
		out[i] = ra
		out[i].Failures = make([]explain.Failure, len(kept))
		clear(named)
		for j, f := range kept {
			out[i].Failures[j] = cloneFailure(f)
			named[keyOf(f)] = true
		}
		out[i].Omitted += len(left)
		for _, f := range left {
			k := keyOf(f)
			if f.Code == explain.CodeLimitExceeded && f.Claim == "" || named[k] {
				continue
			}
			named[k] = true
			if f.Claim != "" {
				out[i].OmittedClaims++
			} else {
				out[i].OmittedReferences++
			}
		}
	}
	return out, from
}

// liveSnapshot is what a PageView reads: the steering, the kept
// publications and their histories, and the state JSON every page is
// sent. A snapshot never changes once stored; commit replaces it.
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
// The engine holder alone changes the publications, the steering and the
// snapshot: every field below snap is its own, and commit, the only writer
// of snap, runs under the engine. A PageView reads only what snap points
// to. closed is the exception: Close may set it from another goroutine (a
// forced exit), so it is behind mu.
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

	// url is the page's address; notify receives every committed state
	// and viewers counts the open streams (see PageConfig). opened is set
	// by the first ensureLive, which commits.
	url     string
	notify  func([]byte)
	viewers func() int
	opened  bool
	// claimed is set by ClaimPage, on whichever goroutine routes the page.
	claimed atomic.Bool

	lim  Limits
	pins *pins

	mu sync.Mutex
	// closed is set by close; the page tools refuse after it.
	closed bool
}

func newLive(lim Limits, pins *pins, page PageConfig) *live {
	l := &live{
		history: map[string]*liveHistory{},
		url:     page.URL,
		notify:  page.Notify,
		viewers: page.Viewers,
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

// errLiveClosed is ensureLive's answer once the server is closing.
var errLiveClosed = errors.New("the live page is shut down: perf-agent-mcp is exiting")

// close marks the live page closed, so no page tool runs after it. It may
// run on any goroutine, more than once. Whoever serves the page stops
// that on its own; close neither waits nor logs, since stderr may be
// blocked during a forced exit.
func (l *live) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}

// ensureLive returns the page's URL, and refuses once Close has run. It
// runs under the engine. The first call commits, so a page that connects
// before anything is published is still sent a state.
func (s *Server) ensureLive() (string, error) {
	if s.live == nil {
		return "", errLiveDisabled
	}
	l := s.live
	if l.isClosed() {
		return "", errLiveClosed
	}
	if !l.opened {
		l.opened = true
		s.commit()
	}
	return l.url, nil
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
// told. A model that cannot be built is a *modelError.
func (s *Server) publishExplained(e *entry, v *profilemodel.View, view string, r explain.Result, title, attempt string) (*publication, error) {
	opts := flamegraph.LiveOptions{
		Title: title, ProfileName: e.name, Digest: e.prof.Digest.String(), Attempt: attempt,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	}
	model, err := flamegraph.ExplainedLiveModel(v, r, opts)
	if err != nil {
		return nil, &modelError{err}
	}
	p := &publication{
		kind: pubExplained, title: title, titleSegs: flamegraph.LiveTitleSegments(title, e.name),
		view: view, digest: e.prof.Digest, attempt: attempt,
		prof: e.prof, v: v, result: r, resultSize: resultBytes(r), opts: opts, model: model,
	}
	if err := s.admit(p); err != nil {
		return nil, err
	}
	l := s.live
	l.selected, l.focus = p.firstSelected(), ""
	s.commit()
	return p, nil
}

// publishDiff publishes d, the comparison of base's view with next's,
// titled title, with node selected ("" for none). It indexes the whole
// union once and measures it once, for every model and export the page
// asks for later. A node that is not in the union publishes nothing, and
// neither does a model that cannot be built within the page limits (a
// *modelError). An admitted publication becomes current, unfocused, and
// the page is told.
func (s *Server) publishDiff(d *profilemodel.Diff, base, next diffSide, title, node string) (*publication, error) {
	ix := profilemodel.NewDiffIndex(d)
	if node != "" {
		if _, ok := ix.Node(profilemodel.UnionID(node)); !ok {
			return nil, &nodeError{node}
		}
	}
	export := flamegraph.DiffPageOptions{BaseName: base.e.name, NewName: next.e.name}
	opts := flamegraph.DiffLiveOptions{
		Title: title, BaseName: base.e.name, NewName: next.e.name,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
		Measure: flamegraph.MeasureDiff(d, export),
	}
	model, err := flamegraph.DiffLiveModel(d, opts)
	if err != nil {
		return nil, &modelError{err}
	}
	p := &publication{
		kind: pubDiff, title: title, titleSegs: flamegraph.LiveTitleSegments(title, "Diff of "+base.e.name+" and "+next.e.name),
		view: d.View, prof: base.e.prof, v: base.v, model: model,
		diff: d, index: ix, newProf: next.e.prof, newV: next.v, node: node, diffOpts: opts, export: export,
	}
	if err := s.admit(p); err != nil {
		return nil, err
	}
	l := s.live
	l.selected, l.focus = p.firstSelected(), ""
	s.commit()
	return p, nil
}

// nodeError is publishDiff's error for a node that is not in the diff's
// union.
type nodeError struct{ node string }

func (e *nodeError) Error() string {
	return fmt.Sprintf("node %q is not a union ID of this diff; take a union ID (a row's id) from diff with the same arguments; nothing was published", echo(e.node))
}

// modelError is publishExplained's and publishDiff's error for a model
// that cannot be built within the page limits, as opposed to one the
// retained budget refuses.
type modelError struct{ err error }

func (e *modelError) Error() string { return e.err.Error() }
func (e *modelError) Unwrap() error { return e.err }

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
		what, smaller := "profile, view, page model and history", "publish a smaller view"
		if p.kind == pubDiff {
			what, smaller = "the profiles and views of both sides, the diff and its index, and the page model", "compare smaller views"
		}
		return fmt.Errorf("the page would keep %d bytes in memory (%s), over MaxRetainedBytes=%d; "+
			"%s or restart perf-agent-mcp with a larger --limit MaxRetainedBytes=N",
			cost(nil), what, s.lim.MaxRetainedBytes, smaller)
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
// from the record's rejected attempts. It keeps the newest attempts, and
// of them the newest failures, whose copy and JSON fit in MaxPageBytes.
// What fits is chosen from size bounds before anything is copied or
// encoded, so a record many times the budget costs no more than the
// budget; the history built is then measured, and in case a bound fell
// short it is halved until it fits.
func (s *Server) newHistory(v *profilemodel.View, id string, version int64, rejected []explain.RejectedAttempt, dropped int, incomplete bool) *liveHistory {
	record := make([]string, len(rejected))
	for i, ra := range rejected {
		record[i] = strings.Clone(ra.Attempt)
	}
	ver := strconv.FormatInt(version, 10)
	build := func(keepFailures, keepAttempts int) *liveHistory {
		kept, left := trimRejected(rejected, keepFailures, keepAttempts)
		h := &liveHistory{version: version, rejected: kept, dropped: dropped + left, incomplete: incomplete,
			record: record, recordDropped: dropped}
		h.json = flamegraph.ExplainedLiveHistory(v, id, ver, kept, h.dropped, incomplete, 0)
		return h
	}
	limit := int64(s.lim.MaxPageBytes)
	keepFailures, keepAttempts := fitHistory(v, rejected, limit-flamegraph.LiveHistoryBaseBound(id, ver)-recordBytes(record))
	h := build(keepFailures, keepAttempts)
	for h.RetainedBytes() > limit && (keepFailures > 0 || keepAttempts > 0) {
		if keepFailures > 0 {
			keepFailures /= 2
		} else {
			keepAttempts /= 2
		}
		h = build(keepFailures, keepAttempts)
	}
	return h
}

// fitHistory is how many of the newest attempts of rejected, and then of
// their newest failures, a history keeps within room bytes, by the size
// bounds of their copies and rows. Attempts come first: a kept attempt
// still counts its failures on the page, so every attempt that fits is
// kept before any failure is listed.
func fitHistory(v *profilemodel.View, rejected []explain.RejectedAttempt, room int64) (keepFailures, keepAttempts int) {
	for i := len(rejected) - 1; i >= 0; i-- {
		ra := rejected[i]
		// An attempt may list its omitted failures, which trimming adds
		// to, and those too large to keep.
		more := 1
		if ra.TooLarge > 0 {
			more++
		}
		c := attemptOverhead + int64(len(ra.Attempt)+len(ra.RetryOf)+len(ra.Evidence)) + flamegraph.LiveHistoryAttemptBound(ra.Attempt, ra.Evidence, more)
		if c > room {
			break
		}
		room -= c
		keepAttempts++
	}
	for i := len(rejected) - 1; i >= len(rejected)-keepAttempts; i-- {
		ra := rejected[i]
		for j := len(ra.Failures) - 1; j >= 0; j-- {
			f := ra.Failures[j]
			c := failureOverhead + int64(failureBytes(f)) + flamegraph.LiveHistoryRowBound(v, ra.Attempt, ra.Evidence, f)
			if c > room {
				return keepFailures, keepAttempts
			}
			room -= c
			keepFailures++
		}
	}
	return keepFailures, keepAttempts
}

// recordBytes is what a history's record of attempt IDs counts.
func recordBytes(record []string) int64 {
	var n int64
	for _, id := range record {
		n += 16 + int64(len(id))
	}
	return n
}

// refreshHistory rebuilds, at the next version, the history of every kept
// explained publication whose record of rejected attempts changed, and
// reports whether any did. A grown history counts against
// MaxRetainedBytes: while the kept publications go over it, the least
// recently shown ones other than the current one are dropped. The current
// one is never dropped, since the page must show its current record, so it
// alone may stay over the budget after its history grew; the growth is
// bounded by its history JSON (at most MaxPageBytes) and the copy of the
// record it was built from. The page is told at the next commit.
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
// hands its state to notify. It is the only writer of the snapshot, and
// runs under the engine.
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
	// more than one line.
	if l.notify != nil {
		l.notify(snap.state)
	}
}

// liveStateEntry is a publication as the state lists it. history is its
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
		if p.kind == pubExplained && p.prof != nil {
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
