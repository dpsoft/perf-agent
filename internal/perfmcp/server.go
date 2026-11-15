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

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/sourceroot"
)

// Server is the perf-agent MCP server. It handles one request at a time on
// one request goroutine, which alone changes the store, the diffs, the
// attempts and the live page's publications, so they need no locks; Serve
// must not run concurrently with itself. The live page's HTTP handlers
// read only the snapshot the request goroutine last committed and the
// immutable publications it holds. Close may run on another goroutine,
// and stops the live page under its lock.
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
}

// New opens every root of cfg and builds a server. It refuses a missing or
// unset ProfilesDir and any root it cannot open. Zero limits take their
// defaults (see Limits); the limits are copied and never change afterwards.
func New(cfg Config) (_ *Server, err error) {
	if cfg.ProfilesDir == "" {
		return nil, errors.New("perfmcp: Config.ProfilesDir is required: the directory profiles are read from")
	}
	lim := cfg.Limits.withDefaults()
	if err := lim.validate(cfg.Live); err != nil {
		return nil, err
	}
	s := &Server{
		lim:      lim,
		log:      cfg.Log,
		attempts: newAttempts(lim.MaxAttemptsPerView, lim.MaxAttemptViews, lim.MaxHistoryBytes),
		link:     (*os.Root).Link,

		evalSymlinks: filepath.EvalSymlinks,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
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
		Log:               s.log,
	})
	return s, nil
}

// Serve answers MCP requests read from r on w until r ends or ctx is done.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.mcp.Serve(ctx, r, w)
}

// Close stops the live page, if any, and closes the server's roots. It may
// run on another goroutine than Serve's, while a request is in flight. The
// page's streams end first and its other responses get liveShutdown before
// their connections are dropped, so a tab that stopped reading delays Close
// by at most that.
func (s *Server) Close() error {
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
	if s.live != nil {
		ts = append(ts, s.pageOpenTool(), s.pageExplainTool(), s.pageShowTool(), s.pageDiffTool())
	}
	return ts
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
		return bad(echoError(err))
	}
	return nil
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
