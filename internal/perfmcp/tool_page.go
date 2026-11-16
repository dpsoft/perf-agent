package perfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// errLiveDisabled is every page tool's answer on a server without the
// live page, and the start of their descriptions there.
var errLiveDisabled = errors.New("DISABLED: start perf-agent-mcp with --live to enable the live page. " +
	"This server was started without --live, so it serves no page")

// pageTool is a page tool as tools/list shows it: disabled, and saying
// so first, on a server without the live page.
func (s *Server) pageTool(t mcpstdio.Tool) mcpstdio.Tool {
	if s.live == nil {
		t.Description = errLiveDisabled.Error() + ". " + t.Description
	}
	return t
}

const pageOpenSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const pageOpenDescription = "Start the live page, unless it is started, and return its URL, the publications it keeps, the current one, " +
	"and viewers, the number of browser tabs that have it open. Give the URL to the user: the server never opens a browser. " +
	"The page is read-only and shows what page_explain and page_diff publish and page_show selects; the URL holds its access token, so give it to the user only."

func (s *Server) pageOpenTool() mcpstdio.Tool {
	return s.pageTool(mcpstdio.Tool{
		Name: "page_open", Title: "Open the live page", Description: pageOpenDescription,
		InputSchema: json.RawMessage(pageOpenSchema), Annotations: mcpstdio.Annotations{Idempotent: true}, Handler: s.handlePageOpen,
	})
}

// pageExplainSchema is render_flamegraph's schema without out.
const pageExplainSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "explanations": {
      "type": "array",
      "description": "One object per explained node: {node, claims, prose, hypotheses, next_measurement}. node is a node ID from top or path. Each claim is {id, metric, scope, node, module, denominator, unit, value}: id is c1, c2, …; metric a fact name as facts returns it (share_of_total, cumulative, …); value the number you read, which must equal the engine's fact at the engine's rounding. Write {c1} in the prose, hypotheses or next_measurement where the figure goes (a claim of the same explanation) and {n:<node id>} to name a frame. Any other number in the text is shown as unchecked.",
      "items": {"type": "object"}
    },
    "attempt": {"type": "string", "description": "The attempt a previous render_flamegraph or page_explain call of this view returned, when this call retries it. Every handle of one profile (opened with or without an execlog) shares one attempt history, so an attempt made with any of them is accepted, and the page lists the rejections made with all of them."},
    "title": {"type": "string", "description": "The page's title (default: the profile's name)."},
    "rounding": {"description": "Ignored: rounding is the engine's, per metric."},
    "tolerance": {"description": "Ignored: rounding is the engine's, per metric."}
  },
  "required": ["handle", "view", "explanations"],
  "additionalProperties": false
}`

const pageExplainDescription = "Check structured explanations of a view's nodes exactly as render_flamegraph does and, when every claim and reference passes, " +
	"publish them on the live page as its current publication, with the first explained node selected; explanations [] publishes the view's facts alone. " +
	"Every figure is the engine's value of a checked claim; other numbers in the text are marked unchecked. " +
	"A failing check publishes nothing and returns status \"rejected\" with every failure and the engine's value; " +
	recheckAdvice + ", and call again with attempt set to the returned attempt. Attempts are shared with render_flamegraph. " +
	"The page lists the attempts rejected for this view of this profile from the server's own record, for every handle of the profile, and follows that record as it grows."

func (s *Server) pageExplainTool() mcpstdio.Tool {
	return s.pageTool(mcpstdio.Tool{
		Name: "page_explain", Title: "Publish an explained flame graph on the live page", Description: pageExplainDescription,
		InputSchema: json.RawMessage(pageExplainSchema), Annotations: mcpstdio.Annotations{}, Handler: s.handlePageExplain,
	})
}

const pageShowSchema = `{
  "type": "object",
  "properties": {
    "publication": {"type": "string", "description": "A publication ID that page_open, page_explain or page_diff returned (p1, p2, …); default: the current publication."},
    "node": {"type": "string", "description": "A node ID of the publication's view, from top or path on that view, or for a diff publication a union ID from diff; it need not be drawn on the page."},
    "focus": {"type": "boolean", "description": "true lays the flame graph out from the node (node, or the one selected); false or omitted draws the whole view."}
  },
  "minProperties": 1,
  "additionalProperties": false
}`

const pageShowDescription = "Steer the live page: make a kept publication current, select a node of it (drawn or not), or both, " +
	"and with focus true lay the flame graph out from the selected node; without focus it draws the whole view. " +
	"Without publication it steers the current publication; switching to another publication without node selects its first explained node, " +
	"or for a diff the node page_diff selected. A diff's nodes are union IDs from diff. Returns what the page now shows and viewers, the number of tabs watching."

func (s *Server) pageShowTool() mcpstdio.Tool {
	return s.pageTool(mcpstdio.Tool{
		Name: "page_show", Title: "Steer the live page", Description: pageShowDescription,
		InputSchema: json.RawMessage(pageShowSchema), Annotations: mcpstdio.Annotations{Idempotent: true}, Handler: s.handlePageShow,
	})
}

type pageOpenArgs struct{}

type pageListed struct {
	ID    string  `json:"id"`
	Kind  pubKind `json:"kind"`
	Title string  `json:"title"`
	View  string  `json:"view"`
}

type pageOpened struct {
	URL          string       `json:"url"`
	Current      string       `json:"current"`
	Publications []pageListed `json:"publications"`
	Viewers      string       `json:"viewers"`
}

// shrinkPage is the advice for a page tool's result over MaxResponseBytes.
const shrinkPage = "restart perf-agent-mcp with a larger --limit MaxResponseBytes=N"

func (s *Server) handlePageOpen(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.live == nil {
		return mcpstdio.Result{}, errLiveDisabled
	}
	var a pageOpenArgs
	if err := decodeArgs("page_open", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	url, err := s.ensureLive()
	if err != nil {
		return mcpstdio.Result{}, err
	}
	snap := s.live.snapshot()
	out := pageOpened{URL: url, Current: snap.current, Publications: []pageListed{}, Viewers: s.viewers()}
	for _, id := range snap.order {
		p := snap.pubs[id]
		out.Publications = append(out.Publications, pageListed{ID: p.id, Kind: p.kind, Title: p.title, View: p.view})
	}
	return s.respond("page_open", shrinkPage, out)
}

// viewers is how many tabs have the page open, as PageConfig.Viewers
// counts them, as a decimal string.
func (s *Server) viewers() string {
	if s.live.viewers == nil {
		return "0"
	}
	return strconv.Itoa(s.live.viewers())
}

type pageExplainArgs struct {
	Handle       profilemodel.Handle `json:"handle"`
	View         string              `json:"view"`
	Explanations json.RawMessage     `json:"explanations"`
	Attempt      string              `json:"attempt"`
	Title        string              `json:"title"`
	Rounding     json.RawMessage     `json:"rounding"`
	Tolerance    json.RawMessage     `json:"tolerance"`
}

// pagePublished carries the view's sampling notes on a GPU view, as every
// response about one does, and no notes key otherwise. FiguresChecked
// counts the claims and ReferencesChecked the distinct frames named by
// {n:…}; the page's pill counts both together.
type pagePublished struct {
	Attempt           string           `json:"attempt"`
	Status            string           `json:"status"`
	Publication       string           `json:"publication"`
	URL               string           `json:"url"`
	FiguresChecked    string           `json:"figures_checked"`
	ReferencesChecked string           `json:"references_checked"`
	UncheckedNumbers  string           `json:"unchecked_numbers"`
	Notices           []explain.Notice `json:"notices"`
	Notes             []string         `json:"notes,omitzero"`
	Viewers           string           `json:"viewers"`
}

// shrinkPublished is the advice for a published result over
// MaxResponseBytes, which only its notices can make large.
const shrinkPublished = "the page was published; its notices make the result large; restart perf-agent-mcp with a larger --limit MaxResponseBytes=N"

func (s *Server) handlePageExplain(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.live == nil {
		return mcpstdio.Result{}, errLiveDisabled
	}
	var a pageExplainArgs
	if err := decodeArgs("page_explain", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	// The page starts before anything is recorded, so a server that is
	// shutting down costs no attempt.
	url, err := s.ensureLive()
	if err != nil {
		return mcpstdio.Result{}, err
	}
	c, err := s.checkCall(explainChecks, explainCall{
		Handle: a.Handle, View: a.View, Attempt: a.Attempt, Title: a.Title,
		Explanations: a.Explanations, Rounding: a.Rounding, Tolerance: a.Tolerance,
	})
	if err != nil {
		return mcpstdio.Result{}, err
	}
	if c.rejection != nil {
		return *c.rejection, nil
	}

	id := s.attempts.add(c.e.prof, a.View, a.Attempt, outcomePublished, nil)
	s.recorded()
	p, err := s.publishExplained(c.e, c.v, a.View, c.r, a.Title, id)
	if err != nil {
		s.attempts.setOutcome(id, outcomeFailed)
		s.recorded()
		if _, ok := errors.AsType[*modelError](err); ok {
			return mcpstdio.Result{}, fmt.Errorf("attempt %s: the explanations passed, but the page cannot be built within this server's MaxPageBytes=%d: %v; "+
				"explain fewer or shorter paths, or restart perf-agent-mcp with a larger --limit MaxPageBytes=N; nothing was published", id, s.lim.MaxPageBytes, err)
		}
		return mcpstdio.Result{}, fmt.Errorf("attempt %s: the explanations passed, but nothing was published: %v", id, err)
	}
	s.log.Info("published", "attempt", id, "handle", a.Handle, "view", a.View, "publication", p.id, "bytes", len(p.model))
	return s.respond("page_explain", shrinkPublished, pagePublished{
		Attempt: id, Status: outcomePublished, Publication: p.id, URL: url,
		FiguresChecked: wireInt(int64(c.r.Figures)), ReferencesChecked: wireInt(int64(c.r.References)),
		UncheckedNumbers: wireInt(int64(uncheckedNumbers(c.r, a.Title))),
		Notices:          c.notices, Notes: c.notes, Viewers: s.viewers(),
	})
}

// pageShowArgs are pointers so that an argument given empty or false is
// told from one left out.
type pageShowArgs struct {
	Publication *string `json:"publication"`
	Node        *string `json:"node"`
	Focus       *bool   `json:"focus"`
}

// pageShown carries the shown publication's sampling notes when its views
// are GPU views: notes for an explained view, and base_notes and
// new_notes, as diff gives them, for a diff. Otherwise it has no notes
// key.
type pageShown struct {
	Publication string   `json:"publication"`
	Selected    string   `json:"selected"`
	Focus       string   `json:"focus"`
	URL         string   `json:"url"`
	Notes       []string `json:"notes,omitzero"`
	BaseNotes   []string `json:"base_notes,omitzero"`
	NewNotes    []string `json:"new_notes,omitzero"`
	Viewers     string   `json:"viewers"`
}

func (s *Server) handlePageShow(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.live == nil {
		return mcpstdio.Result{}, errLiveDisabled
	}
	var a pageShowArgs
	if err := decodeArgs("page_show", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	if a.Publication == nil && a.Node == nil && a.Focus == nil {
		return mcpstdio.Result{}, errors.New("page_show needs an argument: pass publication, node or both, and focus true to lay the flame graph out from the node")
	}
	url, err := s.ensureLive()
	if err != nil {
		return mcpstdio.Result{}, err
	}
	l := s.live
	id := l.current
	if a.Publication != nil {
		id = *a.Publication
	} else if id == "" {
		return mcpstdio.Result{}, errors.New("nothing is published on the live page yet; call page_explain or page_diff first")
	}
	i := slices.IndexFunc(l.pubs, func(p *publication) bool { return p.id == id })
	if i < 0 {
		return mcpstdio.Result{}, fmt.Errorf("publication %q is not kept; the kept ones are: %s; call page_explain or page_diff to publish again", echo(id), l.keptIDs())
	}
	p := l.pubs[i]
	var node string
	switch {
	case a.Node != nil:
		node = *a.Node
		switch {
		case p.hasNode(node):
		case p.kind == pubDiff:
			return mcpstdio.Result{}, fmt.Errorf("node %q is not a union ID of the diff in publication %s; find union IDs (a row's id) with diff on the same views", echo(node), p.id)
		default:
			return mcpstdio.Result{}, fmt.Errorf("node %q is not in view %s of publication %s; find node IDs with top or path", echo(node), echo(p.view), p.id)
		}
	case id == l.current:
		node = l.selected
	default:
		node = p.firstSelected()
	}
	focus := a.Focus != nil && *a.Focus
	if focus && node == "" {
		return mcpstdio.Result{}, fmt.Errorf("publication %s has no node selected to focus on; pass node with focus", p.id)
	}
	out := pageShown{URL: url}
	switch {
	case p.kind == pubDiff && p.v.IsGPU():
		out.BaseNotes, out.NewNotes = nonNil(p.diff.BaseNotes), nonNil(p.diff.NewNotes)
	case p.kind == pubExplained && p.v != nil:
		if out.Notes, err = viewNotes(p.v); err != nil {
			return mcpstdio.Result{}, err
		}
	}
	if err := l.show(id, node, focus); err != nil {
		return mcpstdio.Result{}, err
	}
	s.commit()
	out.Publication, out.Selected, out.Focus, out.Viewers = l.current, l.selected, l.focus, s.viewers()
	return s.respond("page_show", shrinkPage, out)
}

// keptIDs lists the kept publications in the order they were made, or
// says there are none.
func (l *live) keptIDs() string {
	ps := slices.Clone(l.pubs)
	slices.SortFunc(ps, func(a, b *publication) int { return a.n - b.n })
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.id
	}
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// hasNode reports whether node is a node of p, drawn or not: of an
// explained publication's view, or of a diff's whole union, looked up in
// its index and never in its model.
func (p *publication) hasNode(node string) bool {
	if len(node) > maxPageID {
		return false
	}
	switch p.kind {
	case pubExplained:
		if p.v == nil {
			return false
		}
		_, ok := p.v.Node(profilemodel.NodeID(node))
		return ok
	case pubDiff:
		if p.index == nil {
			return false
		}
		_, ok := p.index.Node(profilemodel.UnionID(node))
		return ok
	}
	return false
}

// firstSelected is the node p selects when it becomes current: an
// explained page's first explained node, none for a facts-only page, and
// for a diff the node page_diff selected, if any.
func (p *publication) firstSelected() string {
	switch {
	case p.kind == pubDiff:
		return p.node
	case p.kind != pubExplained || len(p.result.Checked) == 0:
		return ""
	}
	return string(p.result.Checked[0].Node)
}
