package perfmcp

import (
	"fmt"
	"slices"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
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
// other non-positive field is refused (Capture.TTL may be zero). A capture
// limit left at its default follows the limit it depends on (see
// CaptureLimits), so lowering that limit alone is never refused; New logs
// each one it moves.
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
// there is one. Some depend on another limit, and New refuses one set
// against it: CompareRows is at most MaxDiffRows, MaxProfileBytes and
// MaxExeclogBytes at most Model.MaxRawBytes and Model.MaxSidecarBytes,
// WindowDuration at least MaxDuration, MaxStoredBytes at least one
// capture's files, and four tails at most a quarter of MaxResponseBytes.
// Where one of them is left at its default, it is moved to fit instead, so
// a value equal to the default follows the limit it depends on: lowered,
// or for MaxStoredBytes raised to hold the largest capture's files. New
// logs each limit it moves, with its default and the value it now has.
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
	l, _ = l.resolve()
	return l
}

// limitChange is a capture limit fit moved from its default to fit the
// limit it depends on.
type limitChange struct {
	field, fits string
	from, to    int64
}

// resolve is withDefaults, with the capture limits fit moved.
func (l Limits) resolve() (Limits, []limitChange) {
	if l == (Limits{}) {
		l = DefaultLimits()
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
	var changes []limitChange
	l.Capture, changes = l.Capture.fit(l)
	return l, changes
}

// fit moves the capture limits left at their defaults to fit the limits
// they depend on, and says which it moved: CompareRows, MaxProfileBytes,
// MaxExeclogBytes and MaxOutputTail are lowered, then MaxStoredBytes is
// raised to hold the largest capture's files and storeHeadroom.
func (c CaptureLimits) fit(l Limits) (CaptureLimits, []limitChange) {
	def := defaultCaptureLimits(profilemodel.DefaultLimits())
	var changes []limitChange
	fitTo(&changes, "Capture.CompareRows", "MaxDiffRows", &c.CompareRows, def.CompareRows, min(def.CompareRows, l.MaxDiffRows))
	fitTo(&changes, "Capture.MaxProfileBytes", "Model.MaxRawBytes", &c.MaxProfileBytes, def.MaxProfileBytes, min(def.MaxProfileBytes, l.Model.MaxRawBytes))
	fitTo(&changes, "Capture.MaxExeclogBytes", "Model.MaxSidecarBytes", &c.MaxExeclogBytes, def.MaxExeclogBytes, min(def.MaxExeclogBytes, l.Model.MaxSidecarBytes))
	fitTo(&changes, "Capture.MaxOutputTail", "MaxResponseBytes", &c.MaxOutputTail, def.MaxOutputTail, fittedTail(l))
	fitTo(&changes, "Capture.MaxStoredBytes", largestCaptureText+" + 1 MiB", &c.MaxStoredBytes, def.MaxStoredBytes, max(def.MaxStoredBytes, c.largestCapture()+storeHeadroom))
	return c, changes
}

// fitTo sets *v to to when it is at its default def, recording the move.
func fitTo[T int | int64](changes *[]limitChange, field, fits string, v *T, def, to T) {
	if *v == def && to != def {
		*changes = append(*changes, limitChange{field: field, fits: fits, from: int64(def), to: int64(to)})
		*v = to
	}
}

// fittedTail is MaxOutputTail's default lowered to fit l.MaxResponseBytes:
// the tails take at most a quarter of it, and each is at least a byte.
func fittedTail(l Limits) int {
	return min(defaultCaptureLimits(profilemodel.DefaultLimits()).MaxOutputTail, max(l.MaxResponseBytes/(4*maxTailsPerResult), 1))
}

// maxTailsPerResult is how many output tails one capture result carries
// at most: stdout and stderr of the workload, and of perf-agent on
// failure. Together they may take a quarter of MaxResponseBytes.
const maxTailsPerResult = 4

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
	return l.Capture.validate(l)
}

// validate refuses capture limits that are not positive (TTL may be zero),
// are over their ceilings, or do not fit the limits of l they depend on.
func (c CaptureLimits) validate(l Limits) error {
	maxDiffRows := l.MaxDiffRows
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
		return fmt.Errorf("perfmcp: limit Capture.TTL must not be negative (0 means no age limit), got %v", c.TTL)
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
	switch {
	case c.MaxProfileBytes > l.Model.MaxRawBytes:
		return fmt.Errorf("perfmcp: limit Capture.MaxProfileBytes is %d, over Model.MaxRawBytes (%d): a capture that large could not be opened",
			c.MaxProfileBytes, l.Model.MaxRawBytes)
	case c.MaxExeclogBytes > l.Model.MaxSidecarBytes:
		return fmt.Errorf("perfmcp: limit Capture.MaxExeclogBytes is %d, over Model.MaxSidecarBytes (%d): an executions log that large could not be opened",
			c.MaxExeclogBytes, l.Model.MaxSidecarBytes)
	case c.WindowDuration < c.MaxDuration:
		return fmt.Errorf("perfmcp: limit Capture.WindowDuration is %v, under Capture.MaxDuration (%v): the longest capture would never fit the window",
			c.WindowDuration, c.MaxDuration)
	case c.MaxStoredBytes < c.largestCapture()+storeHeadroom:
		return fmt.Errorf("perfmcp: limit Capture.MaxStoredBytes is %d, under %s + 1 MiB (%d): the largest capture would not fit beside the capture directory's own small files",
			c.MaxStoredBytes, largestCaptureText, c.largestCapture()+storeHeadroom)
	// A tail at its fitted default is as small as fit makes it: under 16
	// bytes of response it is one byte, and not refused.
	case maxTailsPerResult*c.MaxOutputTail > l.MaxResponseBytes/4 && c.MaxOutputTail != fittedTail(l):
		return fmt.Errorf("perfmcp: limit Capture.MaxOutputTail is %d: %d tails take %d bytes, over a quarter of MaxResponseBytes (%d), which must leave room for the rest of a result",
			c.MaxOutputTail, maxTailsPerResult, maxTailsPerResult*c.MaxOutputTail, l.MaxResponseBytes/4)
	}
	return nil
}

// storeHeadroom is what MaxStoredBytes holds beyond the largest capture:
// the capture directory's own small files (.owner, .lock, each series'
// .seq and manifests).
const storeHeadroom = 1 << 20

// largestCaptureText names largestCapture in messages.
const largestCaptureText = "3 × Capture.MaxProfileBytes + Capture.MaxExeclogBytes"

// largestCapture is the most a capture may write: a profile of each mode
// and an executions log.
func (c CaptureLimits) largestCapture() int64 {
	return c.captureNeed([]capture.Mode{capture.ModeCPU, capture.ModeOffCPU, capture.ModeGPU})
}

// captureNeed is the most a capture of modes may write: a profile per
// distinct mode, and an executions log with gpu.
func (c CaptureLimits) captureNeed(modes []capture.Mode) int64 {
	modes = slices.Compact(slices.Sorted(slices.Values(modes)))
	need := c.MaxProfileBytes * int64(len(modes))
	if slices.Contains(modes, capture.ModeGPU) {
		need += c.MaxExeclogBytes
	}
	return need
}
