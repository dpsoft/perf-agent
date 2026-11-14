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
// the rejected ones dropped to keep them bounded.
type viewAttempts struct {
	rejected []*attempt
	others   []*attempt
	dropped  int
}

// attempts is a server's record of render attempts. IDs are "a1", "a2", …
// in the order the server made them, across all handles and views. Each
// handle and view keeps its max most recent attempts; the rejected ones are
// what a page lists as rejected at render, so a page never takes that list
// from the agent. It belongs to one Server and takes no locks.
type attempts struct {
	max   int
	next  int
	byID  map[string]*attempt
	views map[viewKey]*viewAttempts
}

func newAttempts(max int) *attempts {
	return &attempts{max: max, byID: map[string]*attempt{}, views: map[viewKey]*viewAttempts{}}
}

// add records an attempt and returns its ID. retryOf is the attempt it
// retries, already checked with lookup, or "".
func (a *attempts) add(h profilemodel.Handle, view, retryOf, outcome string, failures []explain.Failure) string {
	a.next++
	at := &attempt{
		id: "a" + strconv.Itoa(a.next), handle: h, view: view, retryOf: retryOf,
		outcome: outcome, failures: slices.Clone(failures),
	}
	k := viewKey{h, view}
	va := a.views[k]
	if va == nil {
		va = &viewAttempts{}
		a.views[k] = va
	}
	a.byID[at.id] = at
	if outcome == outcomeRejected {
		va.dropped += a.keep(&va.rejected, at)
	} else {
		a.keep(&va.others, at)
	}
	return at.id
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

// forget drops every attempt of handle h, which the store no longer
// holds, so the record stays as bounded as the store.
func (a *attempts) forget(h profilemodel.Handle) {
	for k, va := range a.views {
		if k.handle != h {
			continue
		}
		for _, at := range slices.Concat(va.rejected, va.others) {
			delete(a.byID, at.id)
		}
		delete(a.views, k)
	}
}

// setOutcome changes the outcome of the kept attempt id between outcomes
// other than rejected, which is final and kept apart.
func (a *attempts) setOutcome(id, outcome string) {
	if at, ok := a.byID[id]; ok {
		at.outcome = outcome
	}
}

// rejected are the kept rejected attempts of h and view, oldest first, and
// how many older rejected ones were dropped.
func (a *attempts) rejected(h profilemodel.Handle, view string) ([]explain.RejectedAttempt, int) {
	va := a.views[viewKey{h, view}]
	if va == nil {
		return nil, 0
	}
	out := make([]explain.RejectedAttempt, 0, len(va.rejected))
	for _, at := range va.rejected {
		out = append(out, explain.RejectedAttempt{Attempt: at.id, RetryOf: at.retryOf, Failures: at.failures})
	}
	return out, va.dropped
}

// lookup returns the attempt id, refusing one the server does not keep and
// one made for another handle or view.
func (a *attempts) lookup(id string, h profilemodel.Handle, view string) (*attempt, error) {
	at, ok := a.byID[id]
	if !ok {
		return nil, fmt.Errorf("attempt %q is not a render attempt this server keeps (it keeps the last MaxAttemptsPerView=%d of each handle and view); "+
			"pass the attempt a previous render_flamegraph call of this handle and view returned, or omit attempt", echo(id), a.max)
	}
	if at.handle != h || at.view != view {
		return nil, fmt.Errorf("attempt %s was made for handle %s view %s, not handle %s view %s; "+
			"pass an attempt of this handle and view, or omit attempt", at.id, at.handle, echo(at.view), h, echo(view))
	}
	return at, nil
}
