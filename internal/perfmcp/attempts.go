package perfmcp

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// Outcomes of a render attempt.
const (
	outcomeWritten  = "written"  // the page was written
	outcomeRejected = "rejected" // the check failed
	outcomeInvalid  = "invalid"  // the explanations were not the right shape
	outcomeFailed   = "failed"   // checked, but the page could not be rendered or written
)

// attempt is one recorded render_flamegraph call.
type attempt struct {
	id       string
	handle   profilemodel.Handle
	view     string
	retryOf  string
	outcome  string
	failures []explain.Failure
}

type viewKey struct {
	handle profilemodel.Handle
	view   string
}

// viewAttempts are the attempts kept for one handle and view, oldest
// first. Rejected attempts are bounded apart from the others, so no number
// of cheap invalid calls can push a rejection off the page; dropped counts
// the rejected ones no longer kept. incomplete says the server may have
// lost count of earlier rejections of this view.
type viewAttempts struct {
	rejected   []*attempt
	others     []*attempt
	dropped    int
	incomplete bool
}

// viewCount is what survives of an evicted view's record: how many
// rejected attempts it had, and whether that count may be short.
type viewCount struct {
	rejected   int
	incomplete bool
}

// countsPerView is how many evicted views' counts are kept per view
// record the server keeps.
const countsPerView = 4

// attempts is a server's record of render attempts. IDs are "a1", "a2", …
// in the order the server made them, across all handles and views. The
// record is keyed by handle and view and outlives the store's handles, so
// evicting and reopening a profile does not clear its rejections. Each
// view keeps its max most recent rejected attempts and, apart, its max most
// recent others; the rejected ones are what a page lists as rejected at
// render, so a page never takes that list from the agent. At most maxViews
// views keep a record, least recently used evicted first; an evicted
// view's rejected count is kept in counts, at most countsPerView*maxViews
// of them. Once a count could not be kept, a view without a record may
// have lost one, and its pages say so. It belongs to one Server and takes
// no locks.
type attempts struct {
	max, maxViews int
	next          int
	byID          map[string]*attempt
	views         map[viewKey]*viewAttempts
	// order lists the views with a record, least recently used first.
	order  []viewKey
	counts map[viewKey]viewCount
	// lost is set once an evicted view's count could not be kept.
	lost bool
}

func newAttempts(max, maxViews int) *attempts {
	return &attempts{
		max: max, maxViews: maxViews,
		byID: map[string]*attempt{}, views: map[viewKey]*viewAttempts{}, counts: map[viewKey]viewCount{},
	}
}

// add records an attempt and returns its ID. retryOf is the attempt it
// retries, already checked with lookup, or "".
func (a *attempts) add(h profilemodel.Handle, view, retryOf, outcome string, failures []explain.Failure) string {
	a.next++
	at := &attempt{
		id: "a" + strconv.Itoa(a.next), handle: h, view: view, retryOf: retryOf,
		outcome: outcome, failures: slices.Clone(failures),
	}
	va := a.record(viewKey{h, view})
	a.byID[at.id] = at
	if outcome == outcomeRejected {
		va.dropped += a.keep(&va.rejected, at)
	} else {
		a.keep(&va.others, at)
	}
	return at.id
}

// record returns k's record, most recently used now, creating it from k's
// surviving count, if any, and evicting the least recently used record
// beyond maxViews.
func (a *attempts) record(k viewKey) *viewAttempts {
	if va, ok := a.views[k]; ok {
		if i := slices.Index(a.order, k); i >= 0 {
			a.order = append(slices.Delete(a.order, i, i+1), k)
		}
		return va
	}
	va := &viewAttempts{incomplete: a.lost}
	if c, ok := a.counts[k]; ok {
		va.dropped, va.incomplete = c.rejected, c.incomplete
		delete(a.counts, k)
	}
	a.views[k] = va
	a.order = append(a.order, k)
	for len(a.order) > a.maxViews {
		a.evict(a.order[0])
		a.order = slices.Delete(a.order, 0, 1)
	}
	return va
}

// evict drops k's record, keeping its rejected count while there is room.
func (a *attempts) evict(k viewKey) {
	va := a.views[k]
	delete(a.views, k)
	for _, at := range slices.Concat(va.rejected, va.others) {
		delete(a.byID, at.id)
	}
	c := viewCount{rejected: len(va.rejected) + va.dropped, incomplete: va.incomplete}
	switch {
	case c == viewCount{}:
	case len(a.counts) < countsPerView*a.maxViews:
		a.counts[k] = c
	default:
		a.lost = true
	}
}

// keep appends at to list, drops the oldest beyond max and returns how
// many it dropped.
func (a *attempts) keep(list *[]*attempt, at *attempt) int {
	*list = append(*list, at)
	n := 0
	for len(*list) > a.max {
		delete(a.byID, (*list)[0].id)
		*list = slices.Delete(*list, 0, 1)
		n++
	}
	return n
}

// setOutcome changes the outcome of the kept attempt id between outcomes
// other than rejected, which is final and kept apart.
func (a *attempts) setOutcome(id, outcome string) {
	if at, ok := a.byID[id]; ok {
		at.outcome = outcome
	}
}

// rejected are the kept rejected attempts of h and view, oldest first, how
// many older rejected ones are no longer kept, and whether that count may
// be short.
func (a *attempts) rejected(h profilemodel.Handle, view string) ([]explain.RejectedAttempt, int, bool) {
	k := viewKey{h, view}
	va := a.views[k]
	if va == nil {
		c, ok := a.counts[k]
		return nil, c.rejected, c.incomplete || !ok && a.lost
	}
	out := make([]explain.RejectedAttempt, 0, len(va.rejected))
	for _, at := range va.rejected {
		out = append(out, explain.RejectedAttempt{Attempt: at.id, RetryOf: at.retryOf, Failures: at.failures})
	}
	return out, va.dropped, va.incomplete
}

// lookup returns the attempt id, refusing one the server does not keep and
// one made for another handle or view.
func (a *attempts) lookup(id string, h profilemodel.Handle, view string) (*attempt, error) {
	at, ok := a.byID[id]
	if !ok {
		return nil, fmt.Errorf("attempt %q is not a render attempt this server keeps (it keeps the last MaxAttemptsPerView=%d of each handle and view, for the MaxAttemptViews=%d most recently used); "+
			"pass the attempt a previous render_flamegraph call of this handle and view returned, or omit attempt", echo(id), a.max, a.maxViews)
	}
	if at.handle != h || at.view != view {
		return nil, fmt.Errorf("attempt %s was made for handle %s view %s, not handle %s view %s; "+
			"pass an attempt of this handle and view, or omit attempt", at.id, at.handle, echo(at.view), h, echo(view))
	}
	return at, nil
}
