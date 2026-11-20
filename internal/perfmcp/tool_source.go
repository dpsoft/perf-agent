package perfmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
	"github.com/dpsoft/perf-agent/internal/sourceroot"
)

// Source tool defaults and bounds.
const (
	defaultSourceContext = 5
	defaultSourceLimit   = 10
	maxSourceLimit       = 50
)

const sourceSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "node": {"type": "string", "description": "A node ID from top with rows=\"nodes\"."},
    "context": {"type": "integer", "minimum": 0, "description": "Source lines shown on each side of the line (default 5, at most the server's source MaxContext)."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Source references per page (default 10)."},
    "cursor": {"type": "string", "description": "next_cursor of the previous page of this same query."}
  },
  "required": ["handle", "view", "node"],
  "additionalProperties": false
}`

const sourceDescription = "The source lines a node's samples point at, from inside the --source roots only, each with how the file was matched: " +
	"build_verified (a source manifest for the build lists these exact bytes), user_selected (the file is tracked at the HEAD of a git checkout inside the source root; " +
	"nothing ties that commit to the build), source_unverified (one file matched with no evidence it is the source that was built), ambiguous (several matched; candidates are listed, no lines) " +
	"or unresolved. Lines are shown only for the first three; when a matched file cannot show its line (past the end, over the read limit, no line number) the row has lines_error instead. " +
	"An ambiguous row lists at most 16 sorted candidates and candidates_total, the number of matches. A PC-sampled line is build_verified only by a manifest naming the capture id. Rows are call_site (the node's own frame) or gpu_pc (a PC-sampled line, matched by base name). " +
	"The value is a decimal string in the view's unit, the sum of the samples through the node that give the reference; line numbers are plain JSON integers. " +
	"Rows are ordered by value, paged with limit and cursor; GPU views carry the sampling notes. A file the tool reads is never overwritten by render_flamegraph."

// shrinkSource is the advice for a source response over MaxResponseBytes.
const shrinkSource = "call again with a smaller limit or context"

// noSourceRoots is the reason of every row when no --source was given.
func (s *Server) noSourceRoots() string { return "no source roots: " + s.startAdvice("--source DIR") }

func (s *Server) sourceTool() mcpstdio.Tool {
	return mcpstdio.Tool{
		Name: "source", Title: "Source lines of a node", Description: sourceDescription,
		InputSchema: json.RawMessage(sourceSchema), Annotations: readOnly, Handler: s.handleSource,
	}
}

type sourceArgs struct {
	Handle  profilemodel.Handle `json:"handle"`
	View    string              `json:"view"`
	Node    profilemodel.NodeID `json:"node"`
	Context *int                `json:"context"`
	Limit   *int                `json:"limit"`
	Cursor  string              `json:"cursor"`
}

type sourceQuery struct {
	Handle  profilemodel.Handle `json:"handle"`
	View    string              `json:"view"`
	Node    profilemodel.NodeID `json:"node"`
	Context int                 `json:"context"`
}

type sourceRow struct {
	Kind       string            `json:"kind"`
	Function   string            `json:"function"`
	Filename   string            `json:"filename"`
	Line       int64             `json:"line"`
	Module     string            `json:"module"`
	BuildID    string            `json:"build_id"`
	Value      Fact              `json:"value"`
	Resolution sourceResolution  `json:"resolution"`
	Lines      []sourceroot.Line `json:"lines,omitzero"`
	// LinesError says why a matched file shows no lines.
	LinesError string `json:"lines_error,omitempty"`
}

// maxSourceCandidates is how many ambiguous candidates a row lists; the
// rest are counted in candidates_total.
const maxSourceCandidates = 16

// sourceResolution is a sourceroot.Resolution with its candidate list
// bounded.
type sourceResolution struct {
	sourceroot.Resolution
	CandidatesTotal string `json:"candidates_total,omitempty"`
}

func wireResolution(r sourceroot.Resolution) sourceResolution {
	w := sourceResolution{Resolution: r}
	if len(r.Candidates) > 0 {
		w.CandidatesTotal = wireInt(int64(len(r.Candidates)))
		if len(r.Candidates) > maxSourceCandidates {
			w.Candidates = r.Candidates[:maxSourceCandidates:maxSourceCandidates]
		}
	}
	return w
}

// linesError says why a matched file shows no lines, bounding the path the
// message names.
func linesError(res sourceroot.Resolution, line int64, err error) string {
	if line < 1 {
		return "the profile gives no line number"
	}
	msg := strings.TrimPrefix(err.Error(), "sourceroot: ")
	if res.Path != "" {
		msg = strings.ReplaceAll(msg, res.Path, echo(res.Path))
	}
	return msg
}

type sourceResult struct {
	Handle      profilemodel.Handle `json:"handle"`
	View        string              `json:"view"`
	Notes       []string            `json:"notes,omitzero"`
	Node        profilemodel.NodeID `json:"node"`
	SourceRoots []string            `json:"source_roots"`
	Rows        []sourceRow         `json:"rows"`
	pageInfo
}

func (s *Server) handleSource(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	var a sourceArgs
	if err := decodeArgs("source", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	maxLimit := min(maxSourceLimit, s.lim.MaxRows)
	limit := min(defaultSourceLimit, maxLimit)
	if a.Limit != nil {
		limit = *a.Limit
	}
	if limit < 1 || limit > maxLimit {
		return mcpstdio.Result{}, fmt.Errorf("limit must be between 1 and %d (this server's MaxRows, and at most %d), got %d", maxLimit, maxSourceLimit, limit)
	}
	ctx := defaultSourceContext
	if a.Context != nil {
		ctx = *a.Context
	}
	if ctx < 0 || ctx > s.lim.Source.MaxContext {
		return mcpstdio.Result{}, fmt.Errorf("context must be between 0 and %d (this server's source MaxContext), got %d", s.lim.Source.MaxContext, ctx)
	}
	e, v, err := s.store.view(a.Handle, a.View)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	if _, err := nodeOf(v, a.Node, a.View); err != nil {
		return mcpstdio.Result{}, err
	}
	refs, err := v.SourceRefs(a.Node)
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("cannot list the source lines of node %s: %v", echo(string(a.Node)), err)
	}
	notes, err := viewNotes(v)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	bind := binding("source", sourceQuery{Handle: a.Handle, View: a.View, Node: a.Node, Context: ctx})
	pg, info, err := pageRows(refs, func(r profilemodel.SourceRef) int64 { return r.Value }, "value", bind, a.Cursor, limit)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	out := sourceResult{
		Handle: a.Handle, View: a.View, Notes: notes, Node: a.Node,
		SourceRoots: nonNil(s.src.Dirs()), Rows: make([]sourceRow, 0, len(pg)), pageInfo: info,
	}
	// One batch reads each root's HEAD and each file once for the whole
	// page, so rows pointing at the same file share its bytes and digest.
	batch := s.src.Batch()
	hasRoots := len(out.SourceRoots) > 0
	for _, r := range pg {
		row := sourceRow{
			Kind: r.Kind, Function: r.Function, Filename: r.Filename, Line: r.Line,
			Module: r.Module.File, BuildID: r.Module.BuildID, Value: intWire(profilemodel.MetricCumulative, r.Value, v.Unit),
		}
		var res sourceroot.Resolution
		switch {
		case !hasRoots:
			res = sourceroot.Resolution{Status: sourceroot.StatusUnresolved, Reason: s.noSourceRoots()}
		case r.Kind == profilemodel.SourceGPUPC:
			res = batch.ResolveBasename(r.Filename, r.Module.BuildID, e.prof.CaptureID)
		default:
			res = batch.Resolve(r.Filename, r.Module.BuildID, e.prof.CaptureID)
		}
		row.Resolution = wireResolution(res)
		switch res.Status {
		case sourceroot.StatusBuildVerified, sourceroot.StatusUserSelected, sourceroot.StatusSourceUnverified:
			lines, err := s.src.Lines(res, r.Line, ctx)
			if err != nil {
				row.LinesError = linesError(res, r.Line, err)
			}
			row.Lines = lines
		}
		out.Rows = append(out.Rows, row)
	}
	return s.respond("source", shrinkSource, out)
}

// openSources opens the source roots of cfg with their manifests and, when
// cfg allows git, the offline git prober. With no directory it still
// returns a usable empty set.
func openSources(cfg Config, lim sourceroot.Limits) (*sourceroot.Roots, error) {
	var manifests []sourceroot.Manifest
	for _, p := range cfg.SourceManifests {
		m, err := sourceroot.ReadManifest(p, lim.MaxManifestBytes)
		if err != nil {
			return nil, fmt.Errorf("perfmcp: %w", err)
		}
		manifests = append(manifests, m)
	}
	var probe sourceroot.Prober
	if cfg.SourceGit {
		probe = sourceroot.GitProber(lim.GitTimeout)
	}
	rs, err := sourceroot.Open(cfg.SourceDirs, manifests, probe, lim)
	if err != nil {
		return nil, fmt.Errorf("perfmcp: %w", err)
	}
	return rs, nil
}
