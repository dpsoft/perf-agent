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
	"reflect"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// Server is the perf-agent MCP server. It handles one request at a time,
// so its handle store needs no locks; Serve must not run concurrently with
// itself.
type Server struct {
	lim Limits
	log *slog.Logger

	// profiles is the profiles root; sources are the source roots and
	// output is the output root, nil when rendering is disabled. Every name
	// is the absolute directory.
	profiles *os.Root
	sources  []*os.Root
	output   *os.Root
	// manifests and sourceGit configure source evidence.
	manifests []string
	sourceGit bool

	store *store
	// diffs caches computed diffs; it belongs to this server alone.
	diffs *diffCache
	// attempts records every render_flamegraph call that got as far as
	// checking, so a page lists the rejected ones from this record.
	attempts *attempts
	mcp      *mcpstdio.Server

	// link publishes a written temporary under its final name without
	// replacing anything; it is (*os.Root).Link. beforePublish, when set,
	// runs between writing the temporary and publishing it. Both are
	// seams for tests.
	link          func(r *os.Root, oldname, newname string) error
	beforePublish func(name string)
}

// New opens every root of cfg and builds a server. It refuses a missing or
// unset ProfilesDir and any root it cannot open. Zero limits take their
// defaults (see Limits); the limits are copied and never change afterwards.
func New(cfg Config) (_ *Server, err error) {
	if cfg.ProfilesDir == "" {
		return nil, errors.New("perfmcp: Config.ProfilesDir is required: the directory profiles are read from")
	}
	lim := cfg.Limits.withDefaults()
	if err := lim.validate(); err != nil {
		return nil, err
	}
	s := &Server{
		lim:       lim,
		log:       cfg.Log,
		manifests: slices.Clone(cfg.SourceManifests),
		sourceGit: cfg.SourceGit,
		attempts:  newAttempts(lim.MaxAttemptsPerView),
		link:      (*os.Root).Link,
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
	for _, dir := range cfg.SourceDirs {
		r, err := openRoot(rootSource, dir)
		if err != nil {
			return nil, err
		}
		s.sources = append(s.sources, r)
	}
	if cfg.OutputDir != "" {
		if s.output, err = openRoot(rootOutput, cfg.OutputDir); err != nil {
			return nil, err
		}
	}
	s.diffs = newDiffCache(lim.MaxCachedDiffs)
	s.store = newStore(s.profiles, lim.Model, lim.MaxOpenHandles)
	s.mcp = mcpstdio.New(mcpstdio.Options{
		Info:            mcpstdio.Implementation{Name: "perf-agent-mcp", Title: "perf-agent profiles", Version: cfg.Version},
		Tools:           s.tools(),
		MaxMessageBytes: lim.MaxMessageBytes,
		Log:             s.log,
	})
	return s, nil
}

// Serve answers MCP requests read from r on w until r ends or ctx is done.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.mcp.Serve(ctx, r, w)
}

// Close closes the server's roots.
func (s *Server) Close() error {
	var errs []error
	for _, r := range append([]*os.Root{s.profiles, s.output}, s.sources...) {
		if r != nil {
			errs = append(errs, r.Close())
		}
	}
	return errors.Join(errs...)
}

// tools are the server's tools, in the order tools/list shows them.
func (s *Server) tools() []mcpstdio.Tool {
	return []mcpstdio.Tool{s.openTool(), s.topTool(), s.pathTool(), s.factsTool(), s.kernelsTool(), s.sandwichTool(), s.diffTool(), s.renderTool()}
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

// decodeArgs decodes a tool's arguments into v, a pointer to a struct,
// refusing unknown names and trailing data. The error lists the tool's
// arguments, taken from v's JSON names.
func decodeArgs(tool string, raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("trailing data after the arguments object")
	}
	if err != nil {
		return fmt.Errorf("invalid arguments to %s: %v; its arguments are: %s", tool, err, strings.Join(argNames(v), ", "))
	}
	return nil
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
