package perfmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

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
    "attempt": {"type": "string", "description": "The attempt a previous render_flamegraph call of this handle and view returned, when this call retries it."},
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
	"fix them and call again with attempt set to the returned attempt. The page lists the attempts rejected before it from the server's own record. " +
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

type renderRejected struct {
	Attempt  string            `json:"attempt"`
	Status   string            `json:"status"`
	Written  bool              `json:"written"`
	Failures []explain.Failure `json:"failures"`
	Notices  []explain.Notice  `json:"notices"`
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
}

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
	e, v, err := s.store.view(a.Handle, a.View)
	if err != nil {
		return mcpstdio.Result{}, err
	}
	// out is checked before anything is recorded, so a bad name costs no
	// attempt; writePage checks it again when it writes.
	if _, err := s.pageName(a.Out); err != nil {
		return mcpstdio.Result{}, err
	}
	if a.Attempt != "" {
		if _, err := s.attempts.lookup(a.Attempt, a.Handle, a.View); err != nil {
			return mcpstdio.Result{}, err
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
		id := s.attempts.add(a.Handle, a.View, a.Attempt, outcomeInvalid, nil)
		return mcpstdio.Result{}, fmt.Errorf("attempt %s: %v; fix explanations and call render_flamegraph again with attempt %q", id, err, id)
	}
	notices = append(notices, decoded...)

	r := explain.Check(v, exps, s.lim.Explain)
	if !r.OK {
		id := s.attempts.add(a.Handle, a.View, a.Attempt, outcomeRejected, r.Failures)
		res, err := s.respond("render_flamegraph", shrinkRejected, renderRejected{
			Attempt: id, Status: outcomeRejected, Failures: r.Failures, Notices: notices,
			Next: fmt.Sprintf("nothing was written; correct every failure (the engine's value is in engine) and call render_flamegraph again with attempt %q", id),
		})
		if err != nil {
			return mcpstdio.Result{}, fmt.Errorf("attempt %s was rejected with %d failures: %v", id, len(r.Failures), err)
		}
		res.IsError = true
		return res, nil
	}

	id := s.attempts.add(a.Handle, a.View, a.Attempt, outcomeWritten, nil)
	rejected, dropped := s.attempts.rejected(a.Handle, a.View)
	if dropped > 0 {
		rejected = append([]explain.RejectedAttempt{droppedAttempts(dropped, s.lim.MaxAttemptsPerView)}, rejected...)
	}
	var page bytes.Buffer
	err = flamegraph.WriteExplainedPage(&page, v, r, flamegraph.ExplainedPageOptions{
		Title: a.Title, ProfileName: e.name, Digest: e.prof.Digest.String(), Attempt: id, Rejected: rejected,
		MaxNodes: s.lim.MaxPageNodes, MaxBytes: s.lim.MaxPageBytes,
	})
	if err == nil && page.Len() > s.lim.MaxPageBytes {
		err = fmt.Errorf("the page is %d bytes", page.Len())
	}
	if err != nil {
		s.attempts.setOutcome(id, outcomeFailed)
		return mcpstdio.Result{}, fmt.Errorf("attempt %s: the explanations passed, but the page cannot be rendered within this server's MaxPageBytes=%d: %v; "+
			"explain fewer or shorter paths, or restart perf-agent-mcp with a larger --limit MaxPageBytes=N", id, s.lim.MaxPageBytes, err)
	}
	path, err := s.writePage(a.Out, page.Bytes())
	if err != nil {
		s.attempts.setOutcome(id, outcomeFailed)
		return mcpstdio.Result{}, fmt.Errorf("attempt %s: the explanations passed, but no page was written: %v", id, err)
	}
	s.log.Info("rendered", "attempt", id, "handle", a.Handle, "view", a.View, "path", path, "bytes", page.Len())
	return s.respond("render_flamegraph", shrinkRender, renderWritten{
		Attempt: id, Status: outcomeWritten, Path: path, Bytes: wireInt(int64(page.Len())),
		FiguresChecked: wireInt(int64(r.Figures)), UncheckedNumbers: wireInt(int64(uncheckedNumbers(r))), Notices: notices,
	})
}

// droppedAttempts is the page's row for the rejected attempts the server
// no longer keeps, so the page does not read as if they never happened.
func droppedAttempts(n, max int) explain.RejectedAttempt {
	return explain.RejectedAttempt{Attempt: "earlier", Failures: []explain.Failure{{
		Explanation: -1, Ref: "MaxAttemptsPerView", Code: explain.CodeLimitExceeded,
		Message: fmt.Sprintf("%s not listed: the server keeps the last %d attempts of each handle and view", earlier(n), max),
	}}}
}

func earlier(n int) string {
	if n == 1 {
		return "1 earlier rejected attempt is"
	}
	return fmt.Sprintf("%d earlier rejected attempts are", n)
}

// uncheckedNumbers counts the numbers no claim checked in r's text.
func uncheckedNumbers(r explain.Result) int {
	n := 0
	count := func(segs []explain.Segment) {
		for _, sg := range segs {
			if sg.Kind == explain.SegmentUnchecked {
				n++
			}
		}
	}
	for _, c := range r.Checked {
		count(c.Prose)
		for _, h := range c.Hypotheses {
			count(h)
		}
		count(c.NextMeasurement)
	}
	return n
}
