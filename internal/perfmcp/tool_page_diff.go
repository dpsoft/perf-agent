package perfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// pageDiffSchema is diff's schema without limit and cursor, with a title
// and a node to select.
const pageDiffSchema = `{
  "type": "object",
  "properties": {
    "base_handle": {"type": "string", "description": "Handle of the base profile."},
    "base_view": {"type": "string", "description": "A view name of the base handle."},
    "new_handle": {"type": "string", "description": "Handle of the new profile."},
    "new_view": {"type": "string", "description": "A view name of the new handle; it must have the base view's sample type, unit and index."},
    "work_base": {"type": "number", "exclusiveMinimum": 0, "description": "Units of work the base run performed; with work_new it enables delta_per_unit."},
    "work_new": {"type": "number", "exclusiveMinimum": 0, "description": "Units of work the new run performed."},
    "title": {"type": "string", "description": "The page's title (default: the names of both profiles). Numbers in it are shown unchecked."},
    "node": {"type": "string", "description": "A union ID (a row's id) from diff of the same views, to select; it need not be drawn on the page."}
  },
  "required": ["base_handle", "base_view", "new_handle", "new_view"],
  "additionalProperties": false
}`

const pageDiffDescription = "Compare two views exactly as diff does and publish the comparison on the live page as its current publication: " +
	"the union of both views drawn as a flame graph coloured by change, with every figure computed by the engine. " +
	"It takes diff's arguments but limit and cursor, refuses what diff refuses with the same text, and returns both totals and the quality warnings. " +
	"node selects a union node; page_show then selects or focuses others by union ID. A diff carries no claims, so nothing is checked or recorded."

func (s *Server) pageDiffTool() mcpstdio.Tool {
	return s.pageTool(mcpstdio.Tool{
		Name: "page_diff", Title: "Publish a diff on the live page", Description: pageDiffDescription,
		InputSchema: json.RawMessage(pageDiffSchema), Annotations: mcpstdio.Annotations{}, Handler: s.handlePageDiff,
	})
}

type pageDiffArgs struct {
	BaseHandle profilemodel.Handle `json:"base_handle"`
	BaseView   string              `json:"base_view"`
	NewHandle  profilemodel.Handle `json:"new_handle"`
	NewView    string              `json:"new_view"`
	WorkBase   *float64            `json:"work_base"`
	WorkNew    *float64            `json:"work_new"`
	Title      string              `json:"title"`
	Node       *string             `json:"node"`
}

// pageDiffed carries both views' sampling notes on GPU views, as diff
// does, and no notes keys otherwise.
type pageDiffed struct {
	Publication     string   `json:"publication"`
	URL             string   `json:"url"`
	BaseTotal       string   `json:"base_total"`
	NewTotal        string   `json:"new_total"`
	QualityWarnings []string `json:"quality_warnings"`
	BaseNotes       []string `json:"base_notes,omitzero"`
	NewNotes        []string `json:"new_notes,omitzero"`
	Viewers         string   `json:"viewers"`
}

func (s *Server) handlePageDiff(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.live == nil {
		return mcpstdio.Result{}, errLiveDisabled
	}
	var a pageDiffArgs
	if err := decodeArgs("page_diff", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	// The title is the agent's text, refused as render_flamegraph refuses
	// it, before anything is compared.
	if err := explain.CheckTitle(a.Title, s.lim.Explain); err != nil {
		limit := ""
		if len(a.Title) > s.lim.Explain.MaxTitleBytes {
			limit = " (--limit Explain.MaxTitleBytes=N)"
		}
		return mcpstdio.Result{}, fmt.Errorf("%v%s; nothing was published, so call page_diff again", err, limit)
	}
	node := ""
	if a.Node != nil {
		if node = *a.Node; node == "" {
			return mcpstdio.Result{}, &nodeError{node}
		}
	}
	q := diffQuery{BaseHandle: a.BaseHandle, BaseView: a.BaseView, NewHandle: a.NewHandle, NewView: a.NewView, WorkBase: a.WorkBase, WorkNew: a.WorkNew}
	base, next, err := s.diffSides(q)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	d, err := s.compare(q, base, next)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	url, err := s.ensureLive()
	if err != nil {
		return mcpstdio.Result{}, err
	}
	p, err := s.publishDiff(d, base, next, a.Title, node)
	if _, ok := errors.AsType[*modelError](err); ok {
		return mcpstdio.Result{}, fmt.Errorf("the diff cannot be drawn within this server's MaxPageBytes=%d: %v; "+
			"restart perf-agent-mcp with a larger --limit MaxPageBytes=N; nothing was published", s.lim.MaxPageBytes, err)
	}
	if err != nil {
		return mcpstdio.Result{}, err
	}
	s.log.Info("published", "kind", string(pubDiff), "base_handle", a.BaseHandle, "new_handle", a.NewHandle, "view", a.BaseView,
		"publication", p.id, "bytes", len(p.model))
	out := pageDiffed{
		Publication: p.id, URL: url, BaseTotal: wireInt(d.BaseTotal), NewTotal: wireInt(d.NewTotal),
		QualityWarnings: nonNil(d.QualityWarnings), Viewers: s.viewers(),
	}
	if base.v.IsGPU() {
		out.BaseNotes, out.NewNotes = nonNil(d.BaseNotes), nonNil(d.NewNotes)
	}
	return s.respond("page_diff", shrinkPage, out)
}
