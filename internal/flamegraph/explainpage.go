package flamegraph

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

//go:embed explainpage.tmpl
var explainPageSource string

//go:embed explainpage.js
var explainPageScript string

var explainPageTmpl = template.Must(template.New("explainpage").Parse(explainPageSource))

const (
	// defaultExplainedMaxNodes bounds the frames drawn when the options set
	// no limit.
	defaultExplainedMaxNodes = 20000
	// explainedRowPitch is the vertical distance between depths, in px.
	explainedRowPitch = 20
)

// ExplainedPageOptions name what an explained page shows about its input
// and how many frames it draws.
type ExplainedPageOptions struct {
	Title       string
	ProfileName string // the input's file name, shown in the footer
	Digest      string // the profile digest the follow-up prompts name
	Attempt     string // the attempt this page was written for
	// Rejected are the server's record of the attempts refused before this
	// one; every failure in them is listed on the page.
	Rejected []explain.RejectedAttempt
	// MaxNodes bounds the frames drawn; 0 means defaultExplainedMaxNodes.
	// Explained and referenced nodes and their callers are always drawn.
	MaxNodes int
}

// exFactRow is one displayed fact: a label, the value as shown, its exact
// value and rounding as a title, and a note.
type exFactRow struct {
	Label string `json:"k"`
	Value string `json:"v"`
	Title string `json:"t"`
	Note  string `json:"n,omitzero"`
}

// exQualityRow is one quality fact with its status.
type exQualityRow struct {
	Label, Value, Title, Status, Note string
}

// exSegment is one piece of agent text ready to show: the engine's value
// for a claim, a frame chip for a node, a marked figure for an unchecked
// number, plain text otherwise.
type exSegment struct {
	Kind  string `json:"kind"` // "text", "claim", "node" or "unchecked"
	Text  string `json:"text"`
	Title string `json:"title,omitzero"`
	Node  string `json:"node,omitzero"` // node chips: the node ID
	Name  string `json:"name,omitzero"` // node chips: the frame name
}

// exExplanation is one checked explanation, as the panel shows it.
type exExplanation struct {
	Prose      []exSegment   `json:"prose"`
	Hypotheses [][]exSegment `json:"hypotheses"`
	Next       []exSegment   `json:"next"`
}

// exFrame is one frame of the model the script reads. Integers are
// strings; every displayed string is computed here.
type exFrame struct {
	ID          string          `json:"id"`
	Ref         string          `json:"ref"` // "f<k>", its index in the frames array
	Name        string          `json:"name"`
	Parent      string          `json:"parent,omitzero"`
	Cum         string          `json:"cum"`   // exact, in the sample unit
	Share       string          `json:"share"` // share of total, as shown
	Facts       []exFactRow     `json:"facts"`
	Explanation []exExplanation `json:"explanation,omitzero"`
}

// exPromptPart is a literal piece of a follow-up prompt or a slot the
// script fills with the selected frame's "id" or "name".
type exPromptPart struct {
	Text string `json:"s,omitzero"`
	Slot string `json:"slot,omitzero"`
}

type exPrompt struct {
	Label string         `json:"label"`
	Parts []exPromptPart `json:"parts"`
}

type exModel struct {
	View     string     `json:"view"`
	Selected string     `json:"selected"`
	Frames   []*exFrame `json:"frames"`
	Prompts  []exPrompt `json:"prompts"`
}

// exDrawn is one frame div.
type exDrawn struct {
	I, Class, Left, Width, Top, Domain, Resolution, Name, Tip string
	Star                                                      bool
}

type exChip struct{ Label, Prompt string }

type exPathItem struct{ Ref, Node, Name, Share string }

type exRow struct{ Ref, Node, Name, Share, Cum string }

type exRejected struct {
	Attempt, RetryOf, Code, Metric, Node, Agent, Engine, Reason string
}

type exLegend struct{ Key, Label string }

type explainedPageData struct {
	LightChrome, DarkChrome, Palette template.CSS
	Title, ProfileName, Digest       string
	Attempt, View                    string
	Checked, RejectedCount           int
	GPU                              bool
	Notes                            []string
	Quality                          []exQualityRow
	Legend                           []exLegend
	Axis, Total, TotalTitle          string
	Height                           string
	Frames                           []exDrawn
	Narrow, Beyond, MaxNodes         int
	MinWidth                         string
	Rows                             []exRow
	Sel                              *exFrame
	SelPath                          []exPathItem
	Chips                            []exChip
	Rejected                         []exRejected
	Model                            template.JS
	Script                           template.JS
}

// WriteExplainedPage renders v with the explanations r checked, as one
// self-contained HTML page following the explained flame graph mock: an
// icicle whose frames are flat, absolutely positioned siblings (so a stack
// thousands of frames deep stays shallow in the DOM), the paths the agent
// explained, and a side panel for the selected frame that separates the
// facts perf-agent computed from the agent's prose. Every figure on the
// page is the engine's: a claim is replaced by its EngineValue and a fact
// shown at Fact.Canonical(); numbers no claim checked are marked. Agent
// text and frame names are escaped and never become links, and the page
// loads nothing. It refuses a nil view and a check that did not pass.
func WriteExplainedPage(w io.Writer, v *profilemodel.View, r explain.Result, opts ExplainedPageOptions) error {
	if v == nil {
		return errors.New("flamegraph: nil view")
	}
	if !r.OK {
		return errors.New("flamegraph: the explanations did not pass the check; only a passing check is rendered")
	}
	b := &explainedBuilder{v: v, r: r, opts: opts, refs: map[profilemodel.NodeID]string{}}
	data, err := b.build()
	if err != nil {
		return err
	}
	return explainPageTmpl.Execute(w, data)
}

type explainedBuilder struct {
	v      *profilemodel.View
	r      explain.Result
	opts   ExplainedPageOptions
	view   string
	frames []*exFrame
	refs   map[profilemodel.NodeID]string // node -> "f<k>" for drawn nodes
	byID   map[profilemodel.NodeID]*exFrame
}

func (b *explainedBuilder) build() (*explainedPageData, error) {
	v := b.v
	b.view = fmt.Sprintf("%s/%s#%d", v.SampleType, v.Unit, v.Index)
	data := &explainedPageData{
		LightChrome: lightChrome,
		DarkChrome:  template.CSS(darkChrome),
		Palette:     template.CSS(paletteCSS + jitterCSS),
		Title:       cmp.Or(b.opts.Title, b.opts.ProfileName, "Explained flame graph"),
		ProfileName: b.opts.ProfileName,
		Digest:      b.opts.Digest,
		Attempt:     b.opts.Attempt,
		View:        b.view,
		Checked:     b.r.Figures + b.r.References,
		GPU:         v.IsGPU(),
		MaxNodes:    cmp.Or(b.opts.MaxNodes, defaultExplainedMaxNodes),
		MinWidth:    strconv.FormatFloat(minDrawFraction*100, 'f', -1, 64) + "%",
		Total:       FormatValue(v.Total, v.Unit),
		TotalTitle:  strconv.FormatInt(v.Total, 10) + " " + v.Unit + ", exact",
		Script:      template.JS(explainPageScript),
	}
	data.Axis = fmt.Sprintf("width = %s (%s) at or below the frame", v.SampleType, v.Unit)
	if data.GPU {
		data.Axis = "width = GPU time launched from the sampled stacks of this path, not CPU time"
	}
	notes, err := v.Notes()
	if err != nil {
		return nil, err
	}
	data.Notes = notes
	data.Quality = b.quality()

	if err := b.layout(data); err != nil {
		return nil, err
	}
	if err := b.attachExplanations(); err != nil {
		return nil, err
	}
	for _, ch := range b.r.Checked {
		f := b.byID[ch.Node]
		data.Rows = append(data.Rows, exRow{Ref: f.Ref, Node: f.ID, Name: f.Name, Share: f.Share,
			Cum: factValue(ch.Facts.Cumulative)})
	}
	sel := b.frames[0]
	if len(b.r.Checked) > 0 {
		sel = b.byID[b.r.Checked[0].Node]
	}
	data.Sel = sel
	data.SelPath = b.path(sel)
	prompts := b.prompts()
	for _, p := range prompts {
		data.Chips = append(data.Chips, exChip{Label: p.Label, Prompt: fillPrompt(p, sel)})
	}
	selIndex := strings.TrimPrefix(sel.Ref, "f")
	for i := range data.Frames {
		if data.Frames[i].I == selIndex {
			data.Frames[i].Class += " cur"
		}
	}
	data.Rejected, data.RejectedCount = b.rejected()
	data.Legend = legendOf(data.Frames)

	model, err := json.Marshal(exModel{View: b.view, Selected: sel.ID, Frames: b.frames, Prompts: prompts})
	if err != nil {
		return nil, err
	}
	data.Model = template.JS(model)
	return data, nil
}

// forced is the set of nodes always drawn: every explained node, every
// node a placeholder names, the root, and all their callers, so the set is
// closed under parents and a node left out never hides one of them.
func (b *explainedBuilder) forced() map[profilemodel.NodeID]bool {
	out := map[profilemodel.NodeID]bool{b.v.Root.ID: true}
	add := func(id profilemodel.NodeID) {
		for n, ok := b.v.Node(id); ok && n != nil && !out[n.ID]; n = n.Parent {
			out[n.ID] = true
		}
	}
	nodesIn := func(segs []explain.Segment) {
		for _, sg := range segs {
			if sg.Kind == explain.SegmentNode {
				add(sg.Node)
			}
		}
	}
	for _, ch := range b.r.Checked {
		add(ch.Node)
		nodesIn(ch.Prose)
		for _, h := range ch.Hypotheses {
			nodesIn(h)
		}
		nodesIn(ch.NextMeasurement)
	}
	return out
}

// layout chooses the frames to draw, breadth first so the widest and
// shallowest come first, and places each at its offset within its parent:
// left and width are percentages of the view total, top is its depth.
// A node narrower than minDrawFraction of the total, or past MaxNodes, is
// not drawn and neither is anything below it; they are counted instead.
func (b *explainedBuilder) layout(data *explainedPageData) error {
	v := b.v
	forced := b.forced()
	budget := max(0, data.MaxNodes-len(forced))
	b.byID = map[profilemodel.NodeID]*exFrame{}
	type queued struct {
		n      *profilemodel.Node
		offset int64
		jitter int
	}
	wide := func(n *profilemodel.Node) bool {
		return n.Cum > 0 && float64(n.Cum) >= minDrawFraction*float64(v.Total)
	}
	explained := map[profilemodel.NodeID]bool{}
	for _, ch := range b.r.Checked {
		explained[ch.Node] = true
	}
	queue := []queued{{n: v.Root}}
	maxDepth, unforced := 0, 0
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		n := q.n
		f, err := b.frame(n)
		if err != nil {
			return err
		}
		b.refs[n.ID] = f.Ref
		maxDepth = max(maxDepth, n.Depth)
		d := exDrawn{
			I:     strconv.Itoa(len(b.frames) - 1),
			Left:  pctOf(q.offset, v.Total),
			Width: pctOf(n.Cum, v.Total),
			Top:   strconv.Itoa(n.Depth * explainedRowPitch),
			Name:  f.Name,
			Tip:   f.Name + "\n" + f.Facts[0].Value + ", " + f.Share + " of total",
			Star:  explained[n.ID],
		}
		if v.Total <= 0 && n == v.Root {
			d.Width = "100"
		}
		dom := DomainRoot
		if n != v.Root {
			dom = Classify(n.Function.Name, n.Function.Module.File)
			if r := ResolutionOf(n.Function.Name); r != ResolutionResolved {
				d.Resolution = r.Info().Key
			}
		}
		d.Domain = dom.Info().Key
		if jittered(dom) && q.jitter > 0 {
			d.Class += " j" + strconv.Itoa(q.jitter)
		}
		if n.Inexact > 0 {
			d.Class += " inexact"
		}
		data.Frames = append(data.Frames, d)

		offset := q.offset
		for i, c := range n.Children {
			switch {
			case forced[c.ID]:
			case wide(c) && unforced < budget:
				unforced++
			default:
				b.countLeftOut(c, wide, data)
				offset += c.Cum
				continue
			}
			queue = append(queue, queued{n: c, offset: offset, jitter: (i * 3) % jitterSteps})
			offset += c.Cum
		}
	}
	data.Height = strconv.Itoa((maxDepth + 1) * explainedRowPitch)
	return nil
}

// countLeftOut counts n and everything below it as not drawn: as narrow
// when a node is under minDrawFraction, as beyond MaxNodes otherwise.
func (b *explainedBuilder) countLeftOut(n *profilemodel.Node, wide func(*profilemodel.Node) bool, data *explainedPageData) {
	stack := []*profilemodel.Node{n}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if wide(m) {
			data.Beyond++
		} else {
			data.Narrow++
		}
		stack = append(stack, m.Children...)
	}
}

// frame adds n to the model with its core facts.
func (b *explainedBuilder) frame(n *profilemodel.Node) (*exFrame, error) {
	cf, err := b.v.CoreFacts(n.ID)
	if err != nil {
		return nil, err
	}
	f := &exFrame{
		ID:    string(n.ID),
		Ref:   "f" + strconv.Itoa(len(b.frames)),
		Name:  n.Function.Name,
		Cum:   strconv.FormatInt(n.Cum, 10),
		Share: factValue(cf.ShareOfTotal),
	}
	if n.Parent != nil {
		f.Parent = string(n.Parent.ID)
	}
	parentNote := ""
	if n.Parent != nil {
		parentNote = "of " + n.Parent.Function.Name
	}
	f.Facts = []exFactRow{
		factRow("Cumulative", cf.Cumulative, ""),
		factRow("Self", cf.Self, ""),
		factRow("Share of total", cf.ShareOfTotal, "of the view total"),
		factRow("Share of parent", cf.ShareOfParent, parentNote),
		factRow("Inexact portion", cf.InexactPortion, "from inexact samples"),
	}
	b.frames = append(b.frames, f)
	b.byID[n.ID] = f
	return f, nil
}

// attachExplanations adds each checked explanation, and the facts only an
// explanation carries, to its node's frame.
func (b *explainedBuilder) attachExplanations() error {
	for _, ch := range b.r.Checked {
		f := b.byID[ch.Node]
		if f == nil {
			return fmt.Errorf("flamegraph: explained node %s is not in the view", ch.Node)
		}
		claims := map[string]explain.CheckedClaim{}
		for _, c := range ch.Claims {
			claims[c.ID] = c
		}
		if f.Explanation == nil {
			execNote := "distinct executions in the checked executions log"
			if ch.Facts.ExecutionsBelow.Status == profilemodel.StatusUnavailable {
				execNote = "the executions log is not usable"
			}
			f.Facts = append(f.Facts,
				factRow("Executions below", ch.Facts.ExecutionsBelow, execNote),
				factRow("Kernel names observed", ch.Facts.KernelNamesObserved, "names at or below, not distinct kernels"))
		}
		e := exExplanation{Prose: b.segments(ch.Prose, claims), Next: b.segments(ch.NextMeasurement, claims)}
		for _, h := range ch.Hypotheses {
			e.Hypotheses = append(e.Hypotheses, b.segments(h, claims))
		}
		f.Explanation = append(f.Explanation, e)
	}
	return nil
}

// segments turns tokenized agent text into what the page shows.
func (b *explainedBuilder) segments(segs []explain.Segment, claims map[string]explain.CheckedClaim) []exSegment {
	out := []exSegment{}
	for _, sg := range segs {
		switch sg.Kind {
		case explain.SegmentClaim:
			c, ok := claims[sg.Claim]
			if !ok {
				// Check resolves every placeholder within its explanation, so
				// this cannot happen; show the placeholder rather than a value.
				out = append(out, exSegment{Kind: "text", Text: sg.Text})
				continue
			}
			out = append(out, exSegment{Kind: "claim", Text: displayEngine(c.EngineValue), Title: b.claimTitle(c)})
		case explain.SegmentNode:
			name := string(sg.Node)
			if n, ok := b.v.Node(sg.Node); ok {
				name = n.Function.Name
			}
			out = append(out, exSegment{Kind: "node", Text: b.refs[sg.Node], Title: string(sg.Node), Node: string(sg.Node), Name: name})
		case explain.SegmentUnchecked:
			out = append(out, exSegment{Kind: "unchecked", Text: sg.Text, Title: "not checked by perf-agent"})
		default:
			out = append(out, exSegment{Kind: "text", Text: sg.Text})
		}
	}
	return out
}

// claimTitle names what a claim's figure is about: the metric, the node by
// name and ID (or the whole view), the exact value and its rounding.
func (b *explainedBuilder) claimTitle(c explain.CheckedClaim) string {
	subject := "the whole view"
	if c.Scope != explain.ScopeView {
		name := string(c.Node)
		if n, ok := b.v.Node(c.Node); ok {
			name = n.Function.Name
		}
		subject = name + " (node " + string(c.Node) + ")"
	}
	switch {
	case c.Module == nil:
	case *c.Module == "":
		subject += ", frames with no module"
	default:
		subject += ", module " + *c.Module
	}
	return fmt.Sprintf("%s of %s: %s; checked by perf-agent", c.Metric, subject,
		exactTitle(c.Value, c.Unit, c.Denominator, c.Rounding, c.Status))
}

// quality is the view's quality summary.
func (b *explainedBuilder) quality() []exQualityRow {
	q := b.v.Quality()
	row := func(label string, f profilemodel.Fact, note string) exQualityRow {
		val, title := factText(f)
		return exQualityRow{Label: label, Value: val, Title: title, Status: string(f.Status), Note: note}
	}
	rows := []exQualityRow{row("Unresolved frames", q.Unresolved, "share of the view with an unresolved frame")}
	exec := exQualityRow{Label: "Executions log", Value: "available", Title: "the executions log matched this profile", Status: "ok",
		Note: "executions are counted from it"}
	if !q.Executions.OK {
		exec.Value = "unavailable: " + cmp.Or(q.Executions.Reason, "no reason given")
		exec.Title, exec.Status, exec.Note = exec.Value, string(profilemodel.StatusUnavailable), "executions are not counted"
	}
	rows = append(rows, exec)
	if b.v.IsGPU() {
		rows = append(rows,
			row("Exact joins", q.JoinExact, "share joined to its launch exactly"),
			row("Unsampled launches", q.UnsampledLaunches, "share with no sampled launch stack"),
			row("Serialized: true", q.Serialized.True, ""),
			row("Serialized: false", q.Serialized.False, ""),
			row("Serialized: unknown", q.Serialized.Unknown, ""),
			row("Serialized: missing", q.Serialized.Missing, ""))
	}
	return rows
}

// path is the frame's callers from below the root down to it.
func (b *explainedBuilder) path(f *exFrame) []exPathItem {
	n, ok := b.v.Node(profilemodel.NodeID(f.ID))
	if !ok {
		return nil
	}
	var out []exPathItem
	for _, p := range b.v.Path(n) {
		pf := b.byID[p.ID]
		out = append(out, exPathItem{Ref: pf.Ref, Node: pf.ID, Name: pf.Name, Share: pf.Share})
	}
	return out
}

// prompts are the follow-up questions, with slots for the frame.
func (b *explainedBuilder) prompts() []exPrompt {
	suffix := " Use perf-agent-mcp: profile digest " + b.opts.Digest + ", view " + b.view + "."
	id, name := exPromptPart{Slot: "id"}, exPromptPart{Slot: "name"}
	lit := func(s string) exPromptPart { return exPromptPart{Text: s} }
	out := []exPrompt{
		{Label: "Why is it wide?", Parts: []exPromptPart{lit("Why is node "), id, lit(" ("), name, lit(") wide?" + suffix)}},
		{Label: "Is it profiler overhead?", Parts: []exPromptPart{lit("Is any of node "), id, lit(" profiler overhead?" + suffix)}},
		{Label: "Compare with another profile", Parts: []exPromptPart{lit("Compare node "), id, lit(" with another profile using diff." + suffix)}},
	}
	if b.v.IsGPU() {
		out = append(out, exPrompt{Label: "CPU→GPU causality",
			Parts: []exPromptPart{lit("Explain the CPU→GPU causality under node "), id, lit("." + suffix)}})
	}
	return out
}

// fillPrompt is p for frame f, as the script builds it.
func fillPrompt(p exPrompt, f *exFrame) string {
	var s strings.Builder
	for _, part := range p.Parts {
		switch part.Slot {
		case "id":
			s.WriteString(f.ID)
		case "name":
			s.WriteString(f.Name)
		default:
			s.WriteString(part.Text)
		}
	}
	return s.String()
}

// rejected lists every failure of the rejected attempts.
func (b *explainedBuilder) rejected() ([]exRejected, int) {
	var out []exRejected
	for _, a := range b.opts.Rejected {
		for _, f := range a.Failures {
			row := exRejected{Attempt: a.Attempt, RetryOf: a.RetryOf, Code: string(f.Code), Metric: "—", Node: f.Ref,
				Agent: f.Agent, Reason: f.Message}
			if e := f.Engine; e != nil {
				row.Metric = e.Metric
				switch {
				case e.Scope == explain.ScopeView:
					row.Node = "the whole view"
				case e.Node != "":
					row.Node = string(e.Node)
					if n, ok := b.v.Node(e.Node); ok {
						row.Node = n.Function.Name + " (node " + string(e.Node) + ")"
					}
				}
				row.Engine = displayEngine(*e)
			}
			out = append(out, row)
		}
	}
	return out, len(out)
}

// legendOf lists the domains drawn, in the CPU-to-GPU reading order.
func legendOf(frames []exDrawn) []exLegend {
	present := map[string]bool{}
	for _, f := range frames {
		present[f.Domain] = true
	}
	var out []exLegend
	for _, d := range legendOrder {
		if in := d.Info(); present[in.Key] && d != DomainRoot {
			out = append(out, exLegend{Key: in.Key, Label: shortLegendLabel(in.Label)})
		}
	}
	return out
}

// factRow is a fact as a row of the facts table.
func factRow(label string, f profilemodel.Fact, note string) exFactRow {
	val, title := factText(f)
	return exFactRow{Label: label, Value: val, Title: title, Note: note}
}

// factValue is a fact as it is shown.
func factValue(f profilemodel.Fact) string {
	v, _ := factText(f)
	return v
}

// factText is a fact as shown and its exact value with its rounding.
func factText(f profilemodel.Fact) (string, string) {
	c, ok := f.Canonical()
	if f.Status == profilemodel.StatusUnavailable || !ok {
		s := "unavailable: " + cmp.Or(f.Reason, "the engine has no value")
		return s, s
	}
	return displayValue(c, f.Unit, f.Status), exactTitle(c, f.Unit, f.Denominator, f.Rounding, f.Status)
}

// displayEngine is a claim's engine value as shown.
func displayEngine(e explain.EngineValue) string {
	if e.Status == profilemodel.StatusUnavailable || e.Value == "" {
		return "unavailable: " + cmp.Or(e.Reason, "the engine has no value")
	}
	return displayValue(e.Value, e.Unit, e.Status)
}

// displayValue shows a canonical value: "0" for a measured zero, a share
// as a percentage shifted exactly from its canonical decimal (so an ok
// share that rounds to 0.000 is "0.0%", not "0"), a count as a grouped
// integer, a sample-unit value with FormatValue.
func displayValue(canonical, unit string, status profilemodel.Status) string {
	if status == profilemodel.StatusZero {
		return "0"
	}
	switch unit {
	case profilemodel.UnitRatio:
		return percentOf(canonical)
	case profilemodel.UnitCount:
		if n, err := strconv.ParseInt(canonical, 10, 64); err == nil {
			return FormatValue(n, "")
		}
	default:
		if n, err := strconv.ParseInt(canonical, 10, 64); err == nil {
			return FormatValue(n, unit)
		}
	}
	return canonical + " " + unit
}

// exactTitle is a value's exact canonical form, unit, denominator and
// rounding: "0.411 ratio of view_total, rounded to 0.001 ratio".
func exactTitle(canonical, unit, denominator, rounding string, status profilemodel.Status) string {
	s := canonical + " " + unit
	if denominator != "" {
		s += " of " + denominator
	}
	switch rounding {
	case "":
	case "exact":
		s += ", exact"
	default:
		s += ", rounded to " + rounding
	}
	if status == profilemodel.StatusZero {
		s += ", measured zero"
	}
	return s
}

// percentOf moves a decimal's point two places right: "0.411" is "41.1%",
// "1.000" is "100.0%". The digits are not reparsed, so no rounding happens
// here beyond the engine's own.
func percentOf(canonical string) string {
	neg := strings.HasPrefix(canonical, "-")
	digits := strings.TrimPrefix(canonical, "-")
	whole, frac, _ := strings.Cut(digits, ".")
	frac += strings.Repeat("0", max(0, 2-len(frac)))
	whole = strings.TrimLeft(whole+frac[:2], "0")
	whole = cmp.Or(whole, "0")
	s := whole
	if rest := frac[2:]; rest != "" {
		s += "." + rest
	}
	if neg {
		s = "-" + s
	}
	return s + "%"
}

// pctOf is part/whole as a CSS percentage of the chart's width.
func pctOf(part, whole int64) string {
	if whole <= 0 {
		return "0"
	}
	return pct(100 * float64(part) / float64(whole))
}
