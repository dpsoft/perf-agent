package perfmcp

import (
	"fmt"
	"time"

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
// Source are filled in when zero, and Capture when all of it is zero. Any
// other non-positive field is refused (Capture.TTL may be zero).
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
	// Capture bounds the capture tools.
	Capture CaptureLimits
}

// CaptureLimits bound what a capture may ask for and keep. Each is
// settable with --limit Capture.Name=V, up to a ceiling compiled in where
// there is one.
type CaptureLimits struct {
	// MaxDuration is the longest capture (at most 5 min).
	MaxDuration time.Duration
	// StartTimeout bounds perf-agent's start, up to its readiness (at most
	// 5 min); FinishTimeout bounds its finish after the capture's end (at
	// most 10 min).
	StartTimeout, FinishTimeout time.Duration
	// TTL is how old an unkept capture may grow before retention removes
	// it; 0 means no age limit.
	TTL time.Duration
	// StopGrace is how long a stopped child gets before SIGKILL (at most
	// 30 s).
	StopGrace time.Duration
	// WindowDuration bounds the requested capture time per rolling hour.
	WindowDuration time.Duration
	// MaxSampleRate bounds the sampling frequency in Hz (at most 10 000).
	MaxSampleRate int
	// KeepPerSeries is how many unkept captures a series retains, newest
	// first; MaxKeptPerSeries how many keep: true captures it may hold.
	KeepPerSeries, MaxKeptPerSeries int
	// CompareRows is how many diff rows a compare_with result carries (at
	// most MaxDiffRows).
	CompareRows int
	// MaxJobs and MaxJobBytes bound the finished jobs kept in memory, by
	// count and by the bytes of their results.
	MaxJobs, MaxJobBytes int
	// WindowCaptures bounds the captures started per rolling hour.
	WindowCaptures int
	// MaxOutputTail bounds the bytes of each output stream a result
	// returns (at most 64 KiB).
	MaxOutputTail int
	// MaxProfileBytes and MaxExeclogBytes bound each file perf-agent
	// writes; a larger one is killed while it is written.
	MaxProfileBytes, MaxExeclogBytes int64
	// MaxStoredBytes bounds the capture directory's total size.
	MaxStoredBytes int64
	// MinFreeBytes is the free space the capture directory's filesystem
	// must have before a capture starts.
	MinFreeBytes int64
}

// The compiled ceilings of the capture limits.
const (
	maxCaptureDuration      = 5 * time.Minute
	maxCaptureSampleRate    = 10000
	maxCaptureStartTimeout  = 5 * time.Minute
	maxCaptureFinishTimeout = 10 * time.Minute
	maxCaptureStopGrace     = 30 * time.Second
	maxCaptureOutputTail    = 64 << 10
)

// defaultCaptureLimits are the capture limits for a server whose profiles
// are bounded by model: a capture's files are bounded as the model would
// bound them, since a larger one could not be opened.
func defaultCaptureLimits(model profilemodel.Limits) CaptureLimits {
	return CaptureLimits{
		MaxDuration:      30 * time.Second,
		StartTimeout:     15 * time.Second,
		FinishTimeout:    30 * time.Second,
		TTL:              7 * 24 * time.Hour,
		StopGrace:        5 * time.Second,
		WindowDuration:   30 * time.Minute,
		MaxSampleRate:    1000,
		KeepPerSeries:    10,
		MaxKeptPerSeries: 4,
		CompareRows:      20,
		MaxJobs:          32,
		MaxJobBytes:      4 << 20,
		WindowCaptures:   60,
		MaxOutputTail:    2 << 10,
		MaxProfileBytes:  model.MaxRawBytes,
		MaxExeclogBytes:  model.MaxSidecarBytes,
		MaxStoredBytes:   4 << 30,
		MinFreeBytes:     1 << 30,
	}
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

		Capture: defaultCaptureLimits(profilemodel.DefaultLimits()),
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
	if l.Capture == (CaptureLimits{}) {
		l.Capture = defaultCaptureLimits(l.Model)
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
	return l.Capture.validate(l.MaxDiffRows)
}

// validate refuses capture limits that are not positive (TTL may be zero)
// or are over their ceilings; maxDiffRows bounds CompareRows.
func (c CaptureLimits) validate(maxDiffRows int) error {
	for _, f := range []struct {
		name     string
		v, limit time.Duration
	}{
		{"MaxDuration", c.MaxDuration, maxCaptureDuration},
		{"StartTimeout", c.StartTimeout, maxCaptureStartTimeout},
		{"FinishTimeout", c.FinishTimeout, maxCaptureFinishTimeout},
		{"StopGrace", c.StopGrace, maxCaptureStopGrace},
		{"WindowDuration", c.WindowDuration, 0},
	} {
		switch {
		case f.v <= 0:
			return fmt.Errorf("perfmcp: limit Capture.%s must be positive, got %v", f.name, f.v)
		case f.limit > 0 && f.v > f.limit:
			return fmt.Errorf("perfmcp: limit Capture.%s is %v, over its ceiling of %v", f.name, f.v, f.limit)
		}
	}
	if c.TTL < 0 {
		return fmt.Errorf("perfmcp: limit Capture.TTL must be zero (no age limit) or positive, got %v", c.TTL)
	}
	for _, f := range []struct {
		name     string
		v, limit int64
		over     string
	}{
		{"MaxSampleRate", int64(c.MaxSampleRate), maxCaptureSampleRate, "its ceiling of %d"},
		{"KeepPerSeries", int64(c.KeepPerSeries), 0, ""},
		{"MaxKeptPerSeries", int64(c.MaxKeptPerSeries), 0, ""},
		{"CompareRows", int64(c.CompareRows), int64(maxDiffRows), "MaxDiffRows (%d)"},
		{"MaxJobs", int64(c.MaxJobs), 0, ""},
		{"MaxJobBytes", int64(c.MaxJobBytes), 0, ""},
		{"WindowCaptures", int64(c.WindowCaptures), 0, ""},
		{"MaxOutputTail", int64(c.MaxOutputTail), maxCaptureOutputTail, "its ceiling of %d"},
		{"MaxProfileBytes", c.MaxProfileBytes, 0, ""},
		{"MaxExeclogBytes", c.MaxExeclogBytes, 0, ""},
		{"MaxStoredBytes", c.MaxStoredBytes, 0, ""},
		{"MinFreeBytes", c.MinFreeBytes, 0, ""},
	} {
		switch {
		case f.v <= 0:
			return fmt.Errorf("perfmcp: limit Capture.%s must be positive, got %d", f.name, f.v)
		case f.limit > 0 && f.v > f.limit:
			return fmt.Errorf("perfmcp: limit Capture.%s is %d, over %s", f.name, f.v, fmt.Sprintf(f.over, f.limit))
		}
	}
	return nil
}
