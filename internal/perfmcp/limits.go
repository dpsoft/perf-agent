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
// MaxPageBytes, MaxPageNodes, MaxAttemptsPerView, MaxAttemptViews and
// MaxHistoryBytes and Source are filled in when zero. Any other non-positive field is refused.
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
	return l
}

// validate refuses limits a server cannot run with.
func (l Limits) validate() error {
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
	} {
		if f.v <= 0 {
			return fmt.Errorf("perfmcp: limit %s must be positive, got %d", f.name, f.v)
		}
	}
	return nil
}
