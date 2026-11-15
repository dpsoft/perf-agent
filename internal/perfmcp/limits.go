package perfmcp

import (
	"fmt"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
	"github.com/dpsoft/perf-agent/internal/sourceroot"
)

// Limits bound what a server accepts and answers. A server copies them at
// New and never changes them, so two servers with different limits can hold
// the same handle and still judge its evidence differently.
//
// New fills in defaults: a zero Limits is DefaultLimits, a zero Model is
// profilemodel.DefaultLimits and a zero Explain is explain.DefaultLimits,
// so a Limits that sets only some of the server's own fields still loads
// profiles and checks explanations. MaxRows, MaxCachedDiffs, MaxDiffRows,
// MaxPageBytes, MaxPageNodes, MaxAttemptsPerView, MaxAttemptViews,
// MaxHistoryBytes, MaxPublications, MaxRetainedBytes, MaxPageClients and
// Source are filled in when zero. Any other non-positive field is refused.
type Limits struct {
	// Model bounds every profile and executions log the server loads.
	Model profilemodel.Limits
	// MaxMessageBytes bounds one incoming JSON-RPC line.
	MaxMessageBytes int
	// MaxResponseBytes bounds the JSON text of one tool result.
	MaxResponseBytes int
	// MaxOpenHandles is how many handles the server keeps open; opening one
	// more closes the least recently used.
	MaxOpenHandles int
	// MaxRows bounds the limit argument of the tools that page rows.
	MaxRows int
	// MaxCachedDiffs is how many computed diffs the server keeps; one more
	// evicts the least recently used.
	MaxCachedDiffs int
	// MaxDiffRows bounds the limit argument of diff.
	MaxDiffRows int
	// Explain bounds the explanations one render_flamegraph call checks.
	Explain explain.Limits
	// MaxPageBytes bounds one rendered page; frames are left out, and
	// counted on the page, to stay under it.
	MaxPageBytes int
	// MaxPageNodes bounds the frames one page draws.
	MaxPageNodes int
	// MaxAttemptsPerView is how many rejected render attempts, and apart
	// from them how many other attempts, the server keeps per profile and
	// view (every handle of one profile shares them); one more drops the
	// oldest of its kind, and the page counts the rejected ones dropped.
	MaxAttemptsPerView int
	// MaxAttemptViews is how many profile and view pairs keep an attempt
	// record; one more evicts the least recently used, whose rejected
	// count is kept for the page.
	MaxAttemptViews int
	// MaxHistoryBytes bounds the bytes of every attempt record the server
	// keeps; beyond it the least recently used views are evicted, and
	// their rejected count kept for the page.
	MaxHistoryBytes int
	// Source bounds the source files, manifests and git probes of the
	// source tool.
	Source sourceroot.Limits
	// MaxPublications is how many publications the live page keeps; one
	// more drops the least recently shown, never the one on screen.
	MaxPublications int
	// MaxRetainedBytes is an estimated budget for the memory the live
	// page's publications keep: the profiles and views they pin, each
	// counted once however many publications and handles share it, plus
	// their checked results, page models and histories. The estimates are
	// within half and twice the heap they stand for, so the real heap can
	// reach about twice this budget. Responses being written are not
	// counted. A publication that does not fit even alone is refused. The
	// publication on screen is never dropped, so after its history grows
	// it alone may stay over the budget, by at most that history. With the
	// live page on it must be at least MaxPageBytes.
	MaxRetainedBytes int64
	// MaxPageClients is how many live page streams may be open at once. A
	// browser opens at most six connections to one host and port, and each
	// tab's stream holds one, so the default leaves two for the tabs'
	// fetches: a tab beyond it is refused a stream and polls instead.
	MaxPageClients int
}

// defaultMaxRows is the MaxRows a server runs with unless told otherwise.
const defaultMaxRows = 200

// Defaults of the diff limits.
const (
	defaultMaxCachedDiffs = 4
	defaultMaxDiffRows    = 500
)

// Defaults of the render limits.
const (
	defaultMaxPageBytes       = 32 << 20
	defaultMaxPageNodes       = 20000
	defaultMaxAttemptsPerView = 20
	defaultMaxAttemptViews    = 64
	defaultMaxHistoryBytes    = 4 << 20
)

// Defaults of the live page limits.
const (
	defaultMaxPublications  = 8
	defaultMaxRetainedBytes = 2 << 30
	defaultMaxPageClients   = 4
)

// DefaultLimits are the limits a server runs with unless told otherwise.
func DefaultLimits() Limits {
	return Limits{
		Model:            profilemodel.DefaultLimits(),
		MaxMessageBytes:  4 << 20,
		MaxResponseBytes: 1 << 20,
		MaxOpenHandles:   8,
		MaxRows:          defaultMaxRows,
		MaxCachedDiffs:   defaultMaxCachedDiffs,
		MaxDiffRows:      defaultMaxDiffRows,

		Explain:            explain.DefaultLimits(),
		MaxPageBytes:       defaultMaxPageBytes,
		MaxPageNodes:       defaultMaxPageNodes,
		MaxAttemptsPerView: defaultMaxAttemptsPerView,
		MaxAttemptViews:    defaultMaxAttemptViews,
		MaxHistoryBytes:    defaultMaxHistoryBytes,
		Source:             sourceroot.DefaultLimits(),

		MaxPublications:  defaultMaxPublications,
		MaxRetainedBytes: defaultMaxRetainedBytes,
		MaxPageClients:   defaultMaxPageClients,
	}
}

// withDefaults is l with the zero parts New fills in.
func (l Limits) withDefaults() Limits {
	if l == (Limits{}) {
		return DefaultLimits()
	}
	if l.Model == (profilemodel.Limits{}) {
		l.Model = profilemodel.DefaultLimits()
	}
	if l.MaxRows == 0 {
		l.MaxRows = defaultMaxRows
	}
	if l.MaxCachedDiffs == 0 {
		l.MaxCachedDiffs = defaultMaxCachedDiffs
	}
	if l.MaxDiffRows == 0 {
		l.MaxDiffRows = defaultMaxDiffRows
	}
	if l.Explain == (explain.Limits{}) {
		l.Explain = explain.DefaultLimits()
	}
	if l.MaxPageBytes == 0 {
		l.MaxPageBytes = defaultMaxPageBytes
	}
	if l.MaxPageNodes == 0 {
		l.MaxPageNodes = defaultMaxPageNodes
	}
	if l.MaxAttemptsPerView == 0 {
		l.MaxAttemptsPerView = defaultMaxAttemptsPerView
	}
	if l.MaxAttemptViews == 0 {
		l.MaxAttemptViews = defaultMaxAttemptViews
	}
	if l.MaxHistoryBytes == 0 {
		l.MaxHistoryBytes = defaultMaxHistoryBytes
	}
	if l.Source == (sourceroot.Limits{}) {
		l.Source = sourceroot.DefaultLimits()
	}
	if l.MaxPublications == 0 {
		l.MaxPublications = defaultMaxPublications
	}
	if l.MaxRetainedBytes == 0 {
		l.MaxRetainedBytes = defaultMaxRetainedBytes
	}
	if l.MaxPageClients == 0 {
		l.MaxPageClients = defaultMaxPageClients
	}
	return l
}

// validate refuses limits a server cannot run with. live says the live
// page is on: only then must the retained budget hold a whole page.
func (l Limits) validate(live bool) error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"MaxMessageBytes", l.MaxMessageBytes},
		{"MaxResponseBytes", l.MaxResponseBytes},
		{"MaxOpenHandles", l.MaxOpenHandles},
		{"MaxRows", l.MaxRows},
		{"MaxCachedDiffs", l.MaxCachedDiffs},
		{"MaxDiffRows", l.MaxDiffRows},
		{"MaxPageBytes", l.MaxPageBytes},
		{"MaxPageNodes", l.MaxPageNodes},
		{"MaxAttemptsPerView", l.MaxAttemptsPerView},
		{"MaxAttemptViews", l.MaxAttemptViews},
		{"MaxHistoryBytes", l.MaxHistoryBytes},
		{"Explain.MaxExplanations", l.Explain.MaxExplanations},
		{"Explain.MaxClaimsPerExplanation", l.Explain.MaxClaimsPerExplanation},
		{"Explain.MaxClaimsTotal", l.Explain.MaxClaimsTotal},
		{"Explain.MaxProseBytes", l.Explain.MaxProseBytes},
		{"Explain.MaxHypotheses", l.Explain.MaxHypotheses},
		{"Explain.MaxHypothesisBytes", l.Explain.MaxHypothesisBytes},
		{"Explain.MaxNextBytes", l.Explain.MaxNextBytes},
		{"Explain.MaxTitleBytes", l.Explain.MaxTitleBytes},
		{"MaxPublications", l.MaxPublications},
		{"MaxPageClients", l.MaxPageClients},
	} {
		if f.v <= 0 {
			return fmt.Errorf("perfmcp: limit %s must be positive, got %d", f.name, f.v)
		}
	}
	if l.MaxRetainedBytes <= 0 {
		return fmt.Errorf("perfmcp: limit MaxRetainedBytes must be positive, got %d", l.MaxRetainedBytes)
	}
	if live && l.MaxRetainedBytes < int64(l.MaxPageBytes) {
		return fmt.Errorf("perfmcp: MaxRetainedBytes must be at least MaxPageBytes: a live page keeps its model, so a budget of %d bytes cannot hold a %d-byte page",
			l.MaxRetainedBytes, l.MaxPageBytes)
	}
	return nil
}
