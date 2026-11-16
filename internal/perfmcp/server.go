package perfmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/sourceroot"
)

// Server is the perf-agent MCP server. The store, the diffs, the attempts,
// the pins and the live page's publications belong to the engine holder:
// every tool runs holding the engine (see engine), and so will a capture
// job's steps, so they need no locks of their own. Serve must not run
// concurrently with itself. The live page's HTTP handlers read only the
// snapshot the engine holder last committed and the immutable publications
// it holds. Close may run on another goroutine, even after Serve returned
// while a handler still runs, and stops the live page under its lock.
type Server struct {
	lim Limits
	log *slog.Logger

	// profiles is the profiles root and output the output root, nil when
	// rendering is disabled; each name is the absolute directory. src is
	// the source roots with the evidence the source tool reports; it holds
	// no directory when none was given.
	profiles *os.Root
	src      *sourceroot.Roots
	output   *os.Root

	store *store
	// diffs caches computed diffs; it belongs to this server alone.
	diffs *diffCache
	// attempts records every render_flamegraph call that got as far as
	// checking, so a page lists the rejected ones from this record.
	attempts *attempts
	mcp      *mcpstdio.Server
	// pins is the ledger of the profiles and views the store and the live
	// page keep. live is the live page, nil unless Config.Live.
	pins *pins
	live *live

	// link publishes a written temporary under its final name without
	// replacing anything; it is (*os.Root).Link. beforePublish, when set,
	// runs between writing the temporary and publishing it. Both are
	// seams for tests.
	link          func(r *os.Root, oldname, newname string) error
	beforePublish func(name string)
	// evalSymlinks is filepath.EvalSymlinks; a seam for tests.
	evalSymlinks func(string) (string, error)

	// engine grants the engine state to one holder at a time.
	engine *engine
	// captureCfg is the capture configuration, and perfAgent and shim what
	// New verified of it. captureDisabled says why capture is off, and is
	// "" when it is on.
	captureCfg      CaptureConfig
	perfAgent       capture.Binary
	shim            capture.Shim
	captureDisabled string

	// The capture file protocol's seams, all nil outside tests: now is the
	// clock (time.Now); ownerAlive judges an intent's owner (by its
	// recorded PIDs and start times); captureFault fails, or simulates a
	// crash at, a protocol boundary; writeErr fails a named write.
	now          func() time.Time
	ownerAlive   func(intent) bool
	captureFault func(faultPoint) error
	writeErr     func(op string) error
	// reaperPause is every capture's RunSpec.FaultPause, which holds its
	// reaper at a test's barrier; "" outside tests.
	reaperPause string
	// startPassDone is closed once the start-up maintenance pass is over
	// (it ran, or found no capture directory), so a test can write in the
	// capture directory without that pass recovering what it writes. A
	// pass that found the captures lock held is not over: it runs again.
	startPassDone chan struct{}
	// beforeRelease, when set, runs as a job is about to let go of the
	// capture directory; a seam for tests.
	beforeRelease func(*job)
	// maintained is closed when the maintenance goroutine has returned,
	// which Close waits for, bounded.
	maintained chan struct{}
	startOnce  sync.Once
	// recovered says a recovery pass has run since start (the start-up
	// pass, or a job's), or that there was nothing to recover: before it, a
	// capture id with no manifest may be one recovery has yet to record.
	recovered atomic.Bool
	// recoveryLeft says the last recovery left an intent whose owner it
	// could not judge yet; recoveryWanted asks maintenance for a full pass.
	recoveryLeft   atomic.Bool
	recoveryWanted chan struct{}
	// selfStartTime reads this process's start time for its intents (nil:
	// /proc).
	selfStartTime func() (uint64, error)
	// captureProtect, when set, names files retention must keep besides
	// the ones this server knows of (serve's publications); nil in stdio.
	captureProtect func() []fileKey
	// ownerSeen is the capture directory's owner kind once it was read,
	// so a mismatch is refused without I/O.
	ownerSeen atomic.Pointer[string]
	// stranded are this server's rollbacks that could not commit their
	// failure manifest yet, and undecided its jobs whose commit could not
	// be read; capMu guards both lists.
	capMu     sync.Mutex
	stranded  []*captureTxn
	undecided []*job

	// self is this server's own binary, which runs each capture's reaper
	// and launcher.
	self string
	// registry holds the capture jobs and lookups the historical answers.
	registry *registry
	lookups  *lookupPool
	// ctx ends at Close, and with it every wait of the jobs and of
	// maintenance.
	ctx  context.Context
	stop context.CancelFunc
	// dirMu is the in-process slot of the capture directory: a job holds
	// it from its lease to its end, and maintenance for its pass, so the
	// two never refuse each other the lease. It is a channel so a wait for
	// it can end with a cancel.
	dirMu chan struct{}
	// jobsMu orders admission against Close: closed refuses new jobs, and
	// jobsWG counts the running ones.
	jobsMu sync.Mutex
	closed bool
	jobsWG sync.WaitGroup
	// killed says KillCaptures ran: a signal path kills the captures before
	// Close, which then does not wait for the jobs' rollbacks.
	killed atomic.Bool
	// cgroupParent is the delegated cgroup a command's own cgroup goes
	// under, "" when there is none.
	cgroupParent func() string
	// listWait is how long capture_list waits for the history (0:
	// listWaitDefault), and lookupHook, when set, runs at the start of
	// each historical read; both are seams for tests.
	listWait   time.Duration
	lookupHook func(key string)
	// readHook, when set, runs between a historical read and keeping its
	// answer; a seam for tests.
	readHook func(key string)
	// listKept holds, per capture_list query, a history read that finished
	// after its call stopped waiting (listMu guards it); historyGen counts
	// the writes to the capture directory, which make one stale.
	listMu     sync.Mutex
	listKept   map[string]keptHistory
	historyGen atomic.Uint64
}

// readSelfStatus reads this process's /proc/self/status; a seam for tests.
var readSelfStatus = capture.ReadSelfStatus

// captureDisabledText is every capture call's answer, and the start of the
// tool's description, on a server started without a perf-agent binary.
const captureDisabledText = "DISABLED: start perf-agent-mcp with --perf-agent PATH (the setcap'd perf-agent binary) to enable capturing."

// drainTimeout bounds how long Serve, once its input or context ended,
// writes the answers already queued: under the command's 1 s shutdown
// grace, so an answer in flight at a signal is still written.
const drainTimeout = 800 * time.Millisecond

// New opens every root of cfg and builds a server. It refuses a missing or
// unset ProfilesDir and any root it cannot open. Zero limits take their
// defaults (see Limits); the limits are copied and never change afterwards.
func New(cfg Config) (_ *Server, err error) {
	if cfg.ProfilesDir == "" {
		return nil, errors.New("perfmcp: Config.ProfilesDir is required: the directory profiles are read from")
	}
	lim, fitted := cfg.Limits.resolve()
	if err := lim.validate(cfg.Live); err != nil {
		return nil, err
	}
	s := &Server{
		lim:      lim,
		log:      cfg.Log,
		attempts: newAttempts(lim.MaxAttemptsPerView, lim.MaxAttemptViews, lim.MaxHistoryBytes),
		link:     (*os.Root).Link,

		evalSymlinks: filepath.EvalSymlinks,
		engine:       newEngine(),
		dirMu:        make(chan struct{}, 1),
		listKept:     map[string]keptHistory{},
	}
	s.ctx, s.stop = context.WithCancel(context.Background())
	s.cgroupParent = sync.OnceValue(s.findCgroupParent)
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	for _, c := range fitted {
		s.log.Info("capture limit fitted", "limit", c.field, "default", c.from, "now", c.to, "fits", c.fits)
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	if s.profiles, err = openRoot(rootProfiles, cfg.ProfilesDir); err != nil {
		return nil, err
	}
	if s.src, err = openSources(cfg, lim.Source); err != nil {
		return nil, err
	}
	if cfg.OutputDir != "" {
		if s.output, err = openRoot(rootOutput, cfg.OutputDir); err != nil {
			return nil, err
		}
	}
	s.diffs = newDiffCache(lim.MaxCachedDiffs)
	s.pins = newPins()
	s.store = newStore(s.profiles, lim.Model, lim.MaxOpenHandles, s.pins)
	if cfg.Live {
		s.live = newLive(lim, s.pins)
	}
	if err := s.setupCapture(cfg.Capture); err != nil {
		return nil, err
	}
	s.registry = newRegistry(lim.Capture, randHex(2))
	s.mcp = mcpstdio.New(mcpstdio.Options{
		Info:            mcpstdio.Implementation{Name: "perf-agent-mcp", Title: "perf-agent profiles", Version: cfg.Version},
		Instructions:    instructions,
		Tools:           s.tools(),
		Prompts:         guidePrompts(),
		Resources:       guideResources(),
		MaxMessageBytes: lim.MaxMessageBytes,
		// A tool error is a result too: no agent argument echoed in one
		// can make it larger than a result may be. The floor keeps the
		// advice to raise a tiny MaxResponseBytes readable.
		MaxToolErrorBytes: max(lim.MaxResponseBytes, minToolErrorBytes),
		// MaxOutBytes is left to mcpstdio, which makes room for two
		// answers of MaxToolErrorBytes at their worst escaping.
		MaxOutMessages: cfg.OutputQueueMessages,
		DrainTimeout:   drainTimeout,
		Log:            s.log,
		// Nobody collects a stdio session's jobs once it ends.
		OnClose: func(mcpstdio.CloseReason) { s.cancelAllJobs() },
	})
	if s.captureDisabled == "" {
		s.lookups = newLookupPool(lookupWorkers, lookupQueue)
		s.startPassDone = make(chan struct{})
		s.maintained = make(chan struct{})
		s.recoveryWanted = make(chan struct{}, 1)
		if beforeMaintenance != nil {
			beforeMaintenance(s)
		}
		go s.maintain()
	}
	return s, nil
}

// The historical lookup pool's size (§4.0): fixed, not a limit.
const (
	lookupWorkers = 2
	lookupQueue   = 16
)

// findCgroupParent is the delegated cgroup v2 directory a launched command
// gets a cgroup of its own under, or "" (logged) when there is none.
func (s *Server) findCgroupParent() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		s.log.Info("captured commands get no cgroup of their own", "why", err)
		return ""
	}
	parent, why := capture.DelegatedCgroup(b, "/sys/fs/cgroup")
	if parent == "" {
		s.log.Info("captured commands get no cgroup of their own", "why", why)
	}
	return parent
}

// setupCapture verifies c and records whether capture is on. A perf-agent
// binary, GPU shim or command that fails verification is an error, since
// the user named it; a server that may not capture (privileged, under
// no_new_privs, not on Linux, or started without a perf-agent) runs with
// capture disabled, and captureDisabled says why.
func (s *Server) setupCapture(c CaptureConfig) error {
	c.Commands = slices.Clone(c.Commands)
	s.captureCfg = c
	switch {
	case !capture.Supported:
		s.captureDisabled = "capture needs Linux"
		return nil
	case c.PerfAgent == "":
		s.captureDisabled = captureDisabledText
		return nil
	}
	verify := c.Verify
	if verify == nil {
		verify = capture.VerifyBinary
	}
	bin, err := verify(c.PerfAgent)
	if err != nil {
		return fmt.Errorf("perfmcp: cannot use perf-agent %s: %w", c.PerfAgent, uncoded{err})
	}
	s.perfAgent = bin
	for _, w := range bin.Warnings {
		s.log.Warn("perf-agent", "path", bin.Path, "warning", w)
	}
	if c.GPUShim != "" {
		if s.shim, err = capture.VerifyShim(c.GPUShim); err != nil {
			return fmt.Errorf("perfmcp: cannot use the GPU shim %s: %w", c.GPUShim, uncoded{err})
		}
	}
	for _, cmd := range c.Commands {
		if !filepath.IsAbs(cmd) {
			return fmt.Errorf("perfmcp: capture command %q is not an absolute path; name each allowed program by its absolute path", cmd)
		}
		if e := capture.CheckCommandFile(cmd); e != nil {
			return fmt.Errorf("perfmcp: cannot allow capture command %s: %w", cmd, uncoded{e})
		}
	}
	status, err := readSelfStatus()
	if err != nil {
		err = capture.Errorf(capture.CodeDisabled, "", "cannot read /proc/self/status: %v", err)
	}
	if err == nil {
		err = capture.CheckSelf(status)
	}
	if err == nil {
		err = capture.CheckBounding(status, bin.Caps)
	}
	if err == nil {
		// The reaper and the launcher are this binary in their internal
		// modes.
		var perr error
		if s.self, perr = os.Executable(); perr != nil {
			err = capture.Errorf(capture.CodeDisabled, "", "cannot find this server's own binary, which runs each capture's reaper: %v", perr)
		}
	}
	if err != nil {
		s.captureDisabled = uncoded{err}.Error()
		s.log.Warn("capture disabled", "reason", s.captureDisabled)
		return nil
	}
	s.log.Info("capture enabled", "perf_agent", bin.Path, "version", bin.Version, "revision", bin.Revision)
	return nil
}

// uncoded is a capture error as text that already says what failed: its
// message and advice without the code ("disabled: ..."), which would only
// repeat it. It unwraps to the error.
type uncoded struct{ err error }

func (u uncoded) Error() string {
	if e, ok := errors.AsType[*capture.Error](u.err); ok {
		if e.Next == "" {
			return e.Message
		}
		return e.Message + "; " + e.Next
	}
	return u.err.Error()
}

func (u uncoded) Unwrap() error { return u.err }

// Serve answers MCP requests read from r on w until r ends or ctx is done.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.mcp.Serve(ctx, r, w)
}

// Close cancels the capture jobs and kills their captures, waits up to
// Capture.StopGrace + 1 s for the jobs to roll back and up to
// maintenanceJoin for the maintenance goroutine to end (neither when a
// signal path already called KillCaptures), stops the live page, if any,
// and closes the server's roots. It may run on another goroutine than
// Serve's, while a request is in flight, and does not wait for the engine:
// a handler may still run after Serve returned. The
// page's streams end first and its other responses get liveShutdown before
// their connections are dropped, so a tab that stopped reading delays Close
// by at most that. A second Close does nothing and returns nil.
func (s *Server) Close() error {
	signalled := s.killed.Load()
	s.jobsMu.Lock()
	again := s.closed
	s.closed = true
	s.jobsMu.Unlock()
	if again {
		return nil
	}
	if s.stop != nil {
		s.stop()
	}
	s.cancelAllJobs()
	s.KillCaptures()
	if !signalled {
		s.waitJobs(s.lim.Capture.StopGrace + time.Second)
		s.waitMaintenance()
	}
	if s.lookups != nil {
		s.lookups.close()
	}
	s.stopStranded()
	if s.live != nil {
		s.live.close()
	}
	var errs []error
	for _, r := range []*os.Root{s.profiles, s.output} {
		if r != nil {
			errs = append(errs, r.Close())
		}
	}
	if s.src != nil {
		errs = append(errs, s.src.Close())
	}
	return errors.Join(errs...)
}

// minToolErrorBytes is the least a tool error's text is cut to, whatever
// MaxResponseBytes is.
const minToolErrorBytes = 1024

// tools are the server's tools, in the order tools/list shows them.
func (s *Server) tools() []mcpstdio.Tool {
	ts := []mcpstdio.Tool{s.openTool(), s.topTool(), s.pathTool(), s.factsTool(), s.kernelsTool(), s.sandwichTool(), s.diffTool(), s.sourceTool(), s.renderTool()}
	var pages []mcpstdio.Tool
	if s.live != nil {
		pages = []mcpstdio.Tool{s.pageOpenTool(), s.pageExplainTool(), s.pageShowTool(), s.pageDiffTool()}
	}
	for _, group := range [][]mcpstdio.Tool{ts, pages} {
		for i := range group {
			group[i].Handler = s.engineHandler(group[i].Handler)
		}
	}
	// The capture tools never hold the engine: a wait would keep every
	// other tool, and the jobs' own steps, from it.
	ts = append(ts, s.captureTools()...)
	return append(ts, pages...)
}

// readOnly are the annotations of every tool that only reads.
var readOnly = mcpstdio.Annotations{ReadOnly: true, Idempotent: true}

// Advice for a response over MaxResponseBytes, per kind of tool.
const (
	// shrinkWithLimit is for tools that page their rows with limit.
	shrinkWithLimit = "call again with a smaller limit"
	// shrinkOpen is for open, whose size is the profile's own: one
	// unresolved_by_module entry per module with unresolved frames, and the
	// notes.
	shrinkOpen = "it is large because of the profile's unresolved_by_module entries and notes; " +
		"restart perf-agent-mcp with a larger --limit MaxResponseBytes=N to open it"
)

// respond encodes a tool's result as compact JSON text, refusing one over
// MaxResponseBytes with shrink, which says how to get a smaller one.
func (s *Server) respond(tool, shrink string, v any) (mcpstdio.Result, error) {
	b, err := marshal(v)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("internal error encoding the %s response: %v", tool, err)
	}
	if len(b) > s.lim.MaxResponseBytes {
		return mcpstdio.Result{}, fmt.Errorf("the %s response is %d bytes, over MaxResponseBytes=%d; %s",
			tool, len(b), s.lim.MaxResponseBytes, shrink)
	}
	return mcpstdio.Result{Text: string(b)}, nil
}

// decodeArgs decodes a tool's arguments into v, a pointer to a struct
// (one without fields for a tool that takes none).
// Names match exactly as sent, as explanations' fields do (encoding/json
// alone would fold case), and an unknown name, a name given twice or
// anything but one JSON object is refused. The error lists the tool's
// arguments, taken from v's JSON names, and repeats the agent's text only
// cut short.
func decodeArgs(tool string, raw json.RawMessage, v any) error {
	names := argNames(v)
	takes := "its arguments are: " + strings.Join(names, ", ")
	if len(names) == 0 {
		takes = "it takes no arguments"
	}
	bad := func(why string) error {
		return fmt.Errorf("invalid arguments to %s: %s; %s", tool, why, takes)
	}
	fields, err := explain.Object(raw, "arguments")
	if err != nil {
		return bad(echoError(err))
	}
	for _, f := range fields {
		if !slices.Contains(names, f.Name) {
			return bad(fmt.Sprintf("unknown argument %q (names are case-sensitive)", echo(f.Name)))
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && te.Field != "" {
			return bad(fmt.Sprintf("argument %s is %s, and it takes %s", te.Field, jsonValueKind(te.Value), jsonTypeOf(te.Type)))
		}
		return bad(echoError(err))
	}
	return nil
}

// jsonValueKind names a JSON value of the kind a type error reports
// (string, number, number 1.5, bool, array, object).
func jsonValueKind(v string) string {
	switch {
	case v == "bool":
		return "a boolean"
	case v == "array", v == "object":
		return "an " + v
	case v == "string", v == "number", strings.HasPrefix(v, "number "):
		return "a " + v
	}
	return "a JSON " + v
}

// jsonTypeOf names the JSON type a Go type decodes from, in the words of a
// JSON schema, never Go's.
func jsonTypeOf(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return jsonTypeOf(t.Elem())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer (a JSON number without quotes, such as 99)"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean (true or false, without quotes)"
	case reflect.Slice, reflect.Array:
		return "an array of " + strings.TrimPrefix(strings.TrimPrefix(jsonTypeOf(t.Elem()), "an "), "a ") + "s"
	}
	return "an object"
}

// maxEchoError is the most characters of a decoding error a message
// repeats: a JSON type error quotes the offending number whole.
const maxEchoError = 256

// echoError is err's text cut to maxEchoError characters.
func echoError(err error) string {
	r := []rune(err.Error())
	if len(r) <= maxEchoError {
		return string(r)
	}
	return string(r[:maxEchoError-1]) + "…"
}

// argNames are the JSON names of the fields of the struct v points to.
func argNames(v any) []string {
	t := reflect.TypeOf(v).Elem()
	var names []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.IsExported() && name != "-" {
			names = append(names, name)
		}
	}
	return names
}
