package perfmcp

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// attempt is one recorded render_flamegraph call: the view it was made on
// and, since every handle of a profile shares the record, the handle it
// was made with and whether that handle had executions.
type attempt struct {
	id         string
	key        viewKey
	handle     profilemodel.Handle
	executions profilemodel.Availability
	retryOf    string
	outcome    string
	failures   storedRecord
}

// viewKey names a view of a profile by the profile's digest, not by the
// handle it was opened with: one profile opened with different executions
// logs, or with one that could not be read, has a handle per load, but its
// views and their names are the same, so every handle of it shares one
// attempt history and none can render a page without its rejections.
type viewKey struct {
	profile profilemodel.Digest
	view    string
}

// viewAttempts are the attempts kept for one profile and view, oldest
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
// in the order the server made them, across all profiles and views. The
// record is keyed by profile digest and view, so every handle of one
// profile shares it, and it outlives the store's handles, so evicting and
// reopening a profile does not clear its rejections. Each
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

// add records an attempt on view of p, as loaded under one of its handles,
// and returns its ID. retryOf is the attempt it retries, already checked
// with lookup, or "". Of failures it keeps what storedFailures keeps.
func (a *attempts) add(p *profilemodel.Profile, view, retryOf, outcome string, failures []explain.Failure) string {
	a.next++
	k := viewKey{p.Digest, view}
	at := &attempt{id: "a" + strconv.Itoa(a.next), key: k, handle: p.Handle, executions: p.Executions,
		retryOf: retryOf, outcome: outcome, failures: storedFailures(failures)}
	va := a.record(k)
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

// rejected are the kept rejected attempts of view of p's profile, made
// with any of its handles, oldest first, how many older rejected ones are
// no longer kept, and whether that count may be short. An attempt made
// with a handle other than p's says which and what executions it had.
func (a *attempts) rejected(p *profilemodel.Profile, view string) ([]explain.RejectedAttempt, int, bool) {
	k := viewKey{p.Digest, view}
	va := a.views[k]
	if va == nil {
		c, ok := a.counts[k]
		return nil, c.rejected, c.incomplete || !ok && a.lost
	}
	out := make([]explain.RejectedAttempt, 0, len(va.rejected))
	for _, at := range va.rejected {
		f := at.failures
		ra := explain.RejectedAttempt{Attempt: at.id, RetryOf: at.retryOf, Failures: f.kept,
			Omitted: f.omitted, OmittedClaims: f.omittedClaims, OmittedReferences: f.omittedRefs}
		if at.handle != p.Handle {
			ra.Evidence = "made with handle " + shortHandle(at.handle) + ": " + describeExecutions(at.executions)
		}
		out = append(out, ra)
	}
	return out, va.dropped, va.incomplete
}

// lookup returns the attempt id, refusing one the server does not keep and
// one made for another profile or view. An attempt made with any handle of
// p's profile is accepted: the handles share one history.
func (a *attempts) lookup(id string, p *profilemodel.Profile, view string) (*attempt, error) {
	d := p.Digest
	at, ok := a.byID[id]
	if !ok {
		return nil, fmt.Errorf("attempt %q is not a render attempt this server keeps (it keeps the last MaxAttemptsPerView=%d of each profile and view, for the MaxAttemptViews=%d most recently used); "+
			"pass the attempt a previous render_flamegraph call of this view of this profile returned, or omit attempt", echo(id), a.max, a.maxViews)
	}
	if k := (viewKey{d, view}); at.key != k {
		return nil, fmt.Errorf("attempt %s was made for view %s of profile %s, not view %s of profile %s; "+
			"pass an attempt of this view of this profile (made with any of its handles), or omit attempt",
			at.id, echo(at.key.view), at.key.profile, echo(view), d)
	}
	return at, nil
}

// shortHandle is h with its profile digest and executions log hash cut to
// 12 hex digits each, enough to tell a profile's handles apart on a page.
func shortHandle(h profilemodel.Handle) string {
	cut := func(s string) string {
		if len(s) > 12 {
			return s[:12] + "…"
		}
		return s
	}
	pd, log, ok := strings.Cut(string(h), "+")
	if !ok {
		return cut(pd)
	}
	if reason, ok := strings.CutPrefix(log, "unread:"); ok {
		return cut(pd) + "+unread:" + echo(reason)
	}
	return cut(pd) + "+" + cut(log)
}

// describeExecutions says how a handle was opened, as its executions
// availability tells.
func describeExecutions(ex profilemodel.Availability) string {
	switch {
	case ex.OK:
		return "opened with an executions log"
	case ex.Reason == profilemodel.ReasonNoSidecar:
		return "opened without an executions log"
	}
	return "opened with an executions log that was not used (" + ex.Reason + ")"
}

// maxStoredFailures is the most failures the record keeps of one rejected
// attempt; the rest are counted.
const maxStoredFailures = 32

// storedRecord is what the record keeps of one attempt's failures: at most
// maxStoredFailures of them, how many it left out and, of those, how many
// distinct claims and references no kept failure names.
type storedRecord struct {
	kept                                []explain.Failure
	omitted, omittedClaims, omittedRefs int
}

// storedFailures is what the record keeps of fs: claim failures first, then
// reference failures, then limits, each in fs's order, up to
// maxStoredFailures, with the rest counted. Every kept string is a copy,
// so the record never holds the agent's prose or arguments a failure's
// text was cut from.
func storedFailures(fs []explain.Failure) storedRecord {
	rank := func(f explain.Failure) int {
		switch {
		case f.Claim != "":
			return 0
		case f.Code == explain.CodeLimitExceeded:
			return 2
		}
		return 1
	}
	sorted := slices.Clone(fs)
	slices.SortStableFunc(sorted, func(x, y explain.Failure) int { return rank(x) - rank(y) })
	n := min(len(sorted), maxStoredFailures)
	r := storedRecord{kept: make([]explain.Failure, n), omitted: len(sorted) - n}
	named := map[string]bool{}
	for i, f := range sorted[:n] {
		r.kept[i] = cloneFailure(f)
		named[failureKey(f)] = true
	}
	for _, f := range sorted[n:] {
		k := failureKey(f)
		if f.Code == explain.CodeLimitExceeded && f.Claim == "" || named[k] {
			continue
		}
		named[k] = true
		if f.Claim != "" {
			r.omittedClaims++
		} else {
			r.omittedRefs++
		}
	}
	return r
}

// failureKey is what a page counts a failure as: the claim it names, or
// the reference, by code and what it refers to.
func failureKey(f explain.Failure) string {
	if f.Claim != "" {
		return "claim\x00" + f.Claim
	}
	return "ref\x00" + string(f.Code) + "\x00" + f.Ref
}

// cloneFailure is f with every string copied.
func cloneFailure(f explain.Failure) explain.Failure {
	f.Claim, f.Ref = strings.Clone(f.Claim), strings.Clone(f.Ref)
	f.Code = explain.Code(strings.Clone(string(f.Code)))
	f.Message, f.Agent = strings.Clone(f.Message), strings.Clone(f.Agent)
	if e := f.Engine; e != nil {
		c := explain.EngineValue{
			Metric: strings.Clone(e.Metric), Scope: strings.Clone(e.Scope), Node: profilemodel.NodeID(strings.Clone(string(e.Node))),
			Denominator: strings.Clone(e.Denominator), Unit: strings.Clone(e.Unit), Value: strings.Clone(e.Value),
			Rounding: strings.Clone(e.Rounding), Status: profilemodel.Status(strings.Clone(string(e.Status))), Reason: strings.Clone(e.Reason),
		}
		if e.Module != nil {
			c.Module = new(strings.Clone(*e.Module))
		}
		f.Engine = &c
	}
	return f
}
