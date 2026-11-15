package perfmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const renderSchema = `{
  "type": "object",
  "properties": {
    "handle": {"type": "string", "description": "A handle returned by open."},
    "view": {"type": "string", "description": "One of the view names open returned."},
    "explanations": {
      "type": "array",
      "description": "One object per explained node: {node, claims, prose, hypotheses, next_measurement}. node is a node ID from top or path. Each claim is {id, metric, scope, node, module, denominator, unit, value}: id is c1, c2, …; metric a fact name as facts returns it (share_of_total, cumulative, …); value the number you read, which must equal the engine's fact at the engine's rounding. Write {c1} in the prose, hypotheses or next_measurement where the figure goes (a claim of the same explanation) and {n:<node id>} to name a frame. Any other number in the text is shown as unchecked.",
      "items": {"type": "object"}
    },
    "out": {"type": "string", "description": "The page's file name inside the output root (relative to it, or absolute inside it), ending in .html, in a directory that exists. An existing file is never replaced: pass a name that does not exist yet."},
    "attempt": {"type": "string", "description": "The attempt a previous render_flamegraph or page_explain call of this view returned, when this call retries it. Every handle of one profile (opened with or without an execlog) shares one attempt history, so an attempt made with any of them is accepted, and the page lists the rejections made with all of them."},
    "title": {"type": "string", "description": "The page's title (default: the profile's name)."},
    "rounding": {"description": "Ignored: rounding is the engine's, per metric."},
    "tolerance": {"description": "Ignored: rounding is the engine's, per metric."}
  },
  "required": ["handle", "view", "explanations", "out"],
  "additionalProperties": false
}`

const renderDescription = "Check structured explanations of a view's nodes against the engine and, when every claim and reference passes, " +
	"write an explained flame graph page under the output root. Every figure on the page is the engine's value of a checked claim; " +
	"other numbers in the text are marked unchecked. A failing check writes nothing and returns status \"rejected\" with every failure and the engine's value; " +
	recheckAdvice + ", and call again with attempt set to the returned attempt. The page lists the attempts rejected before it from the server's own record, for every handle of the same profile, and names the handle of any made with another. " +
	"An existing file is never replaced."

func (s *Server) renderTool() mcpstdio.Tool {
	desc := renderDescription
	if s.output == nil {
		desc = errRenderDisabled.Error() + ". " + desc
	}
	return mcpstdio.Tool{
		Name: "render_flamegraph", Title: "Render an explained flame graph", Description: desc,
		InputSchema: json.RawMessage(renderSchema), Annotations: mcpstdio.Annotations{}, Handler: s.handleRender,
	}
}

type renderArgs struct {
	Handle       profilemodel.Handle `json:"handle"`
	View         string              `json:"view"`
	Explanations json.RawMessage     `json:"explanations"`
	Out          string              `json:"out"`
	Attempt      string              `json:"attempt"`
	Title        string              `json:"title"`
	Rounding     json.RawMessage     `json:"rounding"`
	Tolerance    json.RawMessage     `json:"tolerance"`
}

// renderRejected and renderWritten carry the view's sampling notes on a
// GPU view, as every response about one does, and no notes key otherwise.
type renderRejected struct {
	Attempt  string            `json:"attempt"`
	Status   string            `json:"status"`
	Written  bool              `json:"written"`
	Failures []explain.Failure `json:"failures"`
	Notices  []explain.Notice  `json:"notices"`
	Notes    []string          `json:"notes,omitzero"`
	Next     string            `json:"next"`
}

type renderWritten struct {
	Attempt          string           `json:"attempt"`
	Status           string           `json:"status"`
	Path             string           `json:"path"`
	Bytes            string           `json:"bytes"`
	FiguresChecked   string           `json:"figures_checked"`
	UncheckedNumbers string           `json:"unchecked_numbers"`
	Notices          []explain.Notice `json:"notices"`
	Notes            []string         `json:"notes,omitzero"`
}

// recheckAdvice is what to do about failed claims: a mismatch may mean the
// claim names the wrong node or metric for its sentence, so the engine's
// value is never simply copied in.
const recheckAdvice = "re-check that each claim's node and metric match the sentence, then rewrite the sentence or the claim"

// ignoredRounding is the notice for a top-level rounding or tolerance.
const ignoredRounding = "ignored: rounding is the engine's (each claim is compared at its metric's rounding); an agent cannot set or widen it"

// shrinkRejected is the advice for a rejection over MaxResponseBytes.
const shrinkRejected = "send fewer explanations or claims per call"

// shrinkRender is the advice for a written result over MaxResponseBytes,
// which only its notices can make large.
const shrinkRender = "the page was written; its notices make the result large; restart perf-agent-mcp with a larger --limit MaxResponseBytes=N"

func (s *Server) handleRender(_ context.Context, raw json.RawMessage) (mcpstdio.Result, error) {
	if s.output == nil {
		return mcpstdio.Result{}, errRenderDisabled
	}
	var a renderArgs
	if err := decodeArgs("render_flamegraph", raw, &a); err != nil {
		return mcpstdio.Result{}, err
	}
	if _, _, err := s.store.view(a.Handle, a.View); err != nil {
		return mcpstdio.Result{}, err
	}
	// out is checked before anything is recorded, so a bad one costs no
	// attempt; writePage checks it again when it writes.
	if _, err := s.pageName(a.Out); err != nil {
		return mcpstdio.Result{}, err
	}
	c, err := s.checkCall(renderChecks, explainCall{
		Handle: a.Handle, View: a.View, Attempt: a.Attempt, Title: a.Title,
		Explanations: a.Explanations, Rounding: a.Rounding, Tolerance: a.Tolerance,
	})
	if err != nil {
		return mcpstdio.Result{}, err
	}
	if c.rejection != nil {
		return *c.rejection, nil
	}
	e, v, r, notices, notes := c.e, c.v, c.r, c.notices, c.notes

	id := s.attempts.add(e.prof, a.View, a.Attempt, outcomeWritten, nil)
	s.recorded()
	rejected, dropped, incomplete := s.attempts.rejected(e.prof, a.View)
	var page bytes.Buffer
	err = flamegraph.WriteExplainedPage(&page, v, r, flamegraph.ExplainedPageOptions{
		Title: a.Title, ProfileName: e.name, Digest: e.prof.Digest.String(), Attempt: id,
		Rejected: rejected, DroppedRejected: dropped, HistoryIncomplete: incomplete,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	})
	if err == nil && page.Len() > s.lim.MaxPageBytes {
		err = fmt.Errorf("the page is %d bytes", page.Len())
	}
	if err != nil {
		s.attempts.setOutcome(id, outcomeFailed)
		s.recorded()
		return mcpstdio.Result{}, fmt.Errorf("attempt %s: the explanations passed, but the page cannot be rendered within this server's MaxPageBytes=%d: %v; "+
			"explain fewer or shorter paths, or restart perf-agent-mcp with a larger --limit MaxPageBytes=N", id, s.lim.MaxPageBytes, err)
	}
	path, err := s.writePage(a.Out, page.Bytes())
	if err != nil {
		s.attempts.setOutcome(id, outcomeFailed)
		s.recorded()
		return mcpstdio.Result{}, fmt.Errorf("attempt %s: the explanations passed, but no page was written: %v", id, err)
	}
	s.log.Info("rendered", "attempt", id, "handle", a.Handle, "view", a.View, "path", path, "bytes", page.Len())
	return s.respond("render_flamegraph", shrinkRender, renderWritten{
		Attempt: id, Status: outcomeWritten, Path: path, Bytes: wireInt(int64(page.Len())),
		FiguresChecked: wireInt(int64(r.Figures)), UncheckedNumbers: wireInt(int64(uncheckedNumbers(r, a.Title))), Notices: notices, Notes: notes,
	})
}

// explainCall is what render_flamegraph and page_explain check alike.
type explainCall struct {
	Handle                            profilemodel.Handle
	View, Attempt, Title              string
	Explanations, Rounding, Tolerance json.RawMessage
}

// checked is a call checkCall checked: the entry and view it named, the
// passing check and its notices, and the view's sampling notes on a GPU
// view. When the check failed, rejection is the result to return and the
// rest is unset but for notes.
type checked struct {
	e         *entry
	v         *profilemodel.View
	r         explain.Result
	notices   []explain.Notice
	notes     []string
	rejection *mcpstdio.Result
}

// checkingTool is a tool that runs the explanation check, as its advice
// names it: the tool to call again, and what it does with a page that
// passes.
type checkingTool struct{ name, verb string }

var (
	renderChecks  = checkingTool{name: "render_flamegraph", verb: "written"}
	explainChecks = checkingTool{name: "page_explain", verb: "published"}
)

// checkCall runs the check render_flamegraph and page_explain share, and
// records the attempt when it gets that far: it finds the handle's view,
// refuses a bad title or an attempt of another view before anything is
// recorded, decodes the explanations (an invalid shape is recorded as
// invalid) and checks them against the engine. A failed check is recorded
// as rejected and answered with the rejection result, an isError result
// listing every failure with the engine's value. Every piece of advice
// names tool, the tool to call again, and what it does with a page.
func (s *Server) checkCall(tool checkingTool, a explainCall) (checked, error) {
	e, v, err := s.store.view(a.Handle, a.View)
	if err != nil {
		return checked{}, err
	}
	if err := explain.CheckTitle(a.Title, s.lim.Explain); err != nil {
		limit := ""
		if len(a.Title) > s.lim.Explain.MaxTitleBytes {
			limit = " (--limit Explain.MaxTitleBytes=N)"
		}
		return checked{}, fmt.Errorf("%v%s; nothing was recorded, so call %s again with the same attempt, if any", err, limit, tool.name)
	}
	notes, err := viewNotes(v)
	if err != nil {
		return checked{}, err
	}
	if a.Attempt != "" {
		if _, err := s.attempts.lookup(a.Attempt, e.prof, a.View); err != nil {
			return checked{}, err
		}
	}
	notices := []explain.Notice{}
	if a.Rounding != nil {
		notices = append(notices, explain.Notice{Path: "rounding", Message: ignoredRounding})
	}
	if a.Tolerance != nil {
		notices = append(notices, explain.Notice{Path: "tolerance", Message: ignoredRounding})
	}

	exps, decoded, err := explain.Decode(a.Explanations, s.lim.Explain)
	if err != nil {
		id := s.attempts.add(e.prof, a.View, a.Attempt, outcomeInvalid, nil)
		s.recorded()
		return checked{}, fmt.Errorf("attempt %s: %v; fix explanations and call %s again with attempt %q", id, err, tool.name, id)
	}
	notices = append(notices, decoded...)

	r := explain.Check(v, exps, s.lim.Explain)
	if r.OK {
		notices = append(notices, unitNotices(r)...)
		return checked{e: e, v: v, r: r, notices: notices, notes: notes}, nil
	}
	id := s.attempts.add(e.prof, a.View, a.Attempt, outcomeRejected, r.Failures)
	s.recorded()
	if omitted, tooLarge := leftOut(r.Failures); omitted+tooLarge > 0 {
		var says []string
		if omitted > 0 {
			says = append(says, fmt.Sprintf("\"and %d more failures\"", omitted))
		}
		if tooLarge > 0 {
			says = append(says, fmt.Sprintf("\"%d failures too large to keep\"", tooLarge))
		}
		notices = append(notices, explain.Notice{Path: "explanations", Message: fmt.Sprintf(
			"this result lists all %d failures; the server's record of %s keeps at most %d of them (claims first, then references, then limits) "+
				"and none whose text exceeds %d bytes, so a later page lists those and says %s",
			len(r.Failures), id, maxStoredFailures, maxFailureBytes, strings.Join(says, " and "))})
	}
	res, err := s.respond(tool.name, shrinkRejected, renderRejected{
		Attempt: id, Status: outcomeRejected, Failures: r.Failures, Notices: notices, Notes: notes,
		Next: fmt.Sprintf("nothing was %s; %s (the engine's value is in engine), and call %s again with attempt %q", tool.verb, recheckAdvice, tool.name, id),
	})
	if err != nil {
		return checked{}, fmt.Errorf("attempt %s was rejected with %d failures: %v", id, len(r.Failures), err)
	}
	res.IsError = true
	return checked{notes: notes, rejection: &res}, nil
}

// recorded brings the live page's histories up to the attempt record
// after the record changed, and tells the page when one moved.
func (s *Server) recorded() {
	if s.live != nil && s.refreshHistory() {
		s.commit()
	}
}

// unitNotices advise on the passing check r: a claim placeholder followed,
// after whitespace, by a unit the page already shows with the claim's
// value ("{c2} ns" shows as "411 ns ns"). Each is a unit_repeated notice,
// once per claim per piece of text, and never a failure.
func unitNotices(r explain.Result) []explain.Notice {
	var out []explain.Notice
	for i, c := range r.Checked {
		claims := map[string]explain.CheckedClaim{}
		for _, cl := range c.Claims {
			claims[cl.ID] = cl
		}
		check := func(path string, segs []explain.Segment) {
			noticed := map[string]bool{}
			for k, sg := range segs[:max(len(segs)-1, 0)] {
				cl, ok := claims[sg.Claim]
				if sg.Kind != explain.SegmentClaim || !ok || noticed[sg.Claim] {
					continue
				}
				word := wordAfterSpace(segs[k+1])
				text, units := flamegraph.ShownClaim(cl.EngineValue)
				if word == "" || !slices.Contains(units, word) {
					continue
				}
				noticed[sg.Claim] = true
				out = append(out, explain.Notice{Path: path, Code: explain.NoticeUnitRepeated, Message: fmt.Sprintf(
					"the page shows {%s} as %q, unit included, so the %q after it shows a unit twice; drop it or write words instead", sg.Claim, text, word)})
			}
		}
		at := fmt.Sprintf("explanations[%d]", i)
		check(at+".prose", c.Prose)
		for j, h := range c.Hypotheses {
			check(fmt.Sprintf("%s.hypotheses[%d]", at, j), h)
		}
		check(at+".next_measurement", c.NextMeasurement)
	}
	return out
}

// wordAfterSpace is the first word of text segment sg when sg starts with
// whitespace: the runes up to the next space or punctuation, "%" kept.
func wordAfterSpace(sg explain.Segment) string {
	if sg.Kind != explain.SegmentText {
		return ""
	}
	rest := strings.TrimLeftFunc(sg.Text, unicode.IsSpace)
	if len(rest) == len(sg.Text) {
		return ""
	}
	if end := strings.IndexFunc(rest, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsPunct(c) && c != '%' }); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// uncheckedNumbers counts the numbers no claim checked in r's text and in
// the agent's title, which the page marks the same way.
func uncheckedNumbers(r explain.Result, title string) int {
	n := 0
	count := func(segs []explain.Segment) {
		for _, sg := range segs {
			if sg.Kind == explain.SegmentUnchecked {
				n++
			}
		}
	}
	count(explain.Tokenize(title))
	for _, c := range r.Checked {
		count(c.Prose)
		for _, h := range c.Hypotheses {
			count(h)
		}
		count(c.NextMeasurement)
	}
	return n
}
