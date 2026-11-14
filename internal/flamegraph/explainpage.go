package flamegraph

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

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
	// maxPromptName is the most characters of a frame name a follow-up
	// prompt carries; a longer name is cut and ends in "…". The script
	// uses the same bound.
	maxPromptName = 200
	// frameOverhead bounds the bytes one drawn frame adds to the page
	// besides its name: its div and its model entry with every fact.
	frameOverhead = 640
	// pageSlack is the room left for what a frame budget does not model:
	// the legend, the chart height and the sentences about frames left out.
	pageSlack = 16 << 10
	// maxBudgetPasses bounds the renders spent fitting a byte budget.
	maxBudgetPasses = 5
)

// ExplainedPageOptions name what an explained page shows about its input
// and how large it may be.
type ExplainedPageOptions struct {
	Title       string
	ProfileName string // the input's file name, shown in the footer
	Digest      string // the profile digest the follow-up prompts name
	Attempt     string // the attempt this page was written for
	// Rejected are the server's record of the attempts refused before this
	// one; every failure in them is listed on the page.
	Rejected []explain.RejectedAttempt
	// DroppedRejected is how many rejected attempts before Rejected the
	// server no longer keeps; the rejected pill counts them.
	DroppedRejected int
	// HistoryIncomplete says the server may have lost count of earlier
	// attempts of this view; the page says so.
	HistoryIncomplete bool
	// MaxNodes bounds the frames drawn; 0 means defaultExplainedMaxNodes.
	// Explained and referenced nodes and their callers are always drawn.
	MaxNodes int
	// MaxBytes bounds the page's size; 0 means no bound. Frames are left
	// out, and counted on the page, until the page fits; a page that does
	// not fit with only the explained and referenced paths is an error.
	MaxBytes int
}

// exRowSpec is what every frame's fact row shares: its label, note, unit,
// denominator and rounding. The model carries each once; frames carry only
// their values.
type exRowSpec struct {
	Label string `json:"k"`
	Note  string `json:"n,omitzero"`
	// ParentNote makes the note "of <parent's name>", built by the script.
	ParentNote bool   `json:"pn,omitzero"`
	Unit       string `json:"u"`
	// Denominator is "parent" for a share of the frame's parent, which the
	// title names as "parent:<parent id>".
	Denominator string `json:"d,omitzero"`
	Rounding    string `json:"r"`
}

// exFactVal is one frame's value in a shared row: as shown, its canonical
// value (absent when unavailable) and whether it is a measured zero.
type exFactVal struct {
	Value     string `json:"v"`
	Canonical string `json:"c,omitzero"`
	Zero      bool   `json:"z,omitzero"`
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
// strings; displayed values are computed here, and the text every frame
// shares is in the model's fact rows.
type exFrame struct {
	ID     string      `json:"id"`
	Ref    string      `json:"ref"` // "f<k>", its index in the frames array
	Name   string      `json:"name"`
	Parent string      `json:"parent,omitzero"`
	Cum    string      `json:"cum"`   // exact, in the sample unit
	Share  string      `json:"share"` // share of total, as shown
	Facts  []exFactVal `json:"facts"` // one per model fact row
	// Extra are the facts only an explained frame shows.
	Extra       []exFactRow     `json:"extra,omitzero"`
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
	View     string      `json:"view"`
	Selected string      `json:"selected"`
	FactRows []exRowSpec `json:"fact_rows"`
	Frames   []*exFrame  `json:"frames"`
	Prompts  []exPrompt  `json:"prompts"`
}

// exDrawn is one frame div.
type exDrawn struct {
	I, Class, Left, Width, Top, Domain, Resolution, Name, Tip string
	Star                                                      bool
}

type exChip struct{ Label, Prompt string }

type exPathItem struct{ Ref, Node, Name, Share string }

type exRow struct{ Ref, Node, Name, Share, Cum string }

// exRejected is one row of "Rejected at render". Kind is "claim",
// "reference" or "limit"; only claim rows carry a metric and figures.
type exRejected struct {
	Attempt, Evidence, Kind, Label, Metric, Node, Agent, Engine, Code, Reason string
}

type exLegend struct{ Key, Label string }

type explainedPageData struct {
	LightChrome, DarkChrome, Palette template.CSS
	Title, ProfileName, Digest       string
	Attempt, View                    string
	CheckedPill                      string
	RejectedPill, LimitPill          string
	HistoryPill                      string
	GPU                              bool
	Notes                            []string
	Quality                          []exQualityRow
	Legend                           []exLegend
	Axis, Total, TotalTitle          string
	Height                           string
	Frames                           []exDrawn
	NarrowNote, LimitNote, SizeNote  string
	RejectedNote                     string
	Rows                             []exRow
	Sel                              *exFrame
	SelFacts                         []exFactRow
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
//
// With opts.MaxBytes the page is fitted to the budget: it is first
// rendered with only the explained and referenced paths, the rest of the
// budget is spent on frames by an upper-bound estimate of their bytes, and
// the page is rendered again with a smaller frame budget if it still does
// not fit. The explained and referenced paths come before the rejected
// attempts' rows: when they do not fit together, the oldest rows are left
// out and counted on the page, so the history alone never makes a page
// fail. Nothing is written unless the page fits.
func WriteExplainedPage(w io.Writer, v *profilemodel.View, r explain.Result, opts ExplainedPageOptions) error {
	if v == nil {
		return errors.New("flamegraph: nil view")
	}
	if !r.OK {
		return errors.New("flamegraph: the explanations did not pass the check; only a passing check is rendered")
	}
	if opts.MaxBytes <= 0 {
		data, err := buildExplained(v, r, opts, -1, -1)
		if err != nil {
			return err
		}
		return explainPageTmpl.Execute(w, data)
	}
	base, err := renderExplained(v, r, opts, 0, -1)
	if err != nil {
		return err
	}
	rows := -1
	if len(base) > opts.MaxBytes {
		if base, rows, err = fitHistory(v, r, opts); err != nil {
			return err
		}
	}
	frameBytes := int64(opts.MaxBytes - len(base) - pageSlack)
	for range maxBudgetPasses {
		page, err := renderExplained(v, r, opts, max(frameBytes, 0), rows)
		if err != nil {
			return err
		}
		if len(page) <= opts.MaxBytes {
			_, err = w.Write(page)
			return err
		}
		frameBytes -= int64(len(page)-opts.MaxBytes) + pageSlack
	}
	// Only the explained and referenced paths are certain to fit.
	_, err = w.Write(base)
	return err
}

// rowOverhead bounds the markup of one rejected row besides its text.
const rowOverhead = 320

// fitHistory renders the page with only the explained and referenced
// paths and as many of the newest rejected rows as fit opts.MaxBytes, and
// returns it with that number of rows. Without any row the page must fit.
func fitHistory(v *profilemodel.View, r explain.Result, opts ExplainedPageOptions) ([]byte, int, error) {
	bare, err := renderExplained(v, r, opts, 0, 0)
	if err != nil {
		return nil, 0, err
	}
	if len(bare) > opts.MaxBytes {
		return nil, 0, fmt.Errorf("flamegraph: the page needs %d bytes with only the explained and referenced paths drawn, over the budget of %d bytes (MaxBytes)",
			len(bare), opts.MaxBytes)
	}
	data, err := buildExplained(v, r, opts, 0, -1)
	if err != nil {
		return nil, 0, err
	}
	keep, room := 0, int64(opts.MaxBytes-len(bare))
	for i := len(data.Rejected) - 1; i >= 0; i-- {
		if room -= rowEstimate(data.Rejected[i]); room < 0 {
			break
		}
		keep++
	}
	for ; keep > 0; keep /= 2 {
		page, err := renderExplained(v, r, opts, 0, keep)
		if err != nil {
			return nil, 0, err
		}
		if len(page) <= opts.MaxBytes {
			return page, keep, nil
		}
	}
	return bare, 0, nil
}

// rowEstimate bounds the bytes one rejected row takes on the page.
func rowEstimate(row exRejected) int64 {
	n := int64(rowOverhead)
	for _, s := range []string{row.Attempt, row.Evidence, row.Label, row.Metric, row.Node, row.Agent, row.Engine, row.Code, row.Reason} {
		n += htmlEscapedBound(s)
	}
	return n
}

// renderExplained renders the page into memory with frameBytes for the
// frames not always drawn and the newest historyRows rejected rows.
func renderExplained(v *profilemodel.View, r explain.Result, opts ExplainedPageOptions, frameBytes int64, historyRows int) ([]byte, error) {
	data, err := buildExplained(v, r, opts, frameBytes, historyRows)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := explainPageTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildExplained assembles the page; frameBytes < 0 means no byte budget,
// and historyRows < 0 every rejected row.
func buildExplained(v *profilemodel.View, r explain.Result, opts ExplainedPageOptions, frameBytes int64, historyRows int) (*explainedPageData, error) {
	b := &explainedBuilder{v: v, r: r, opts: opts, historyRows: historyRows, refs: map[profilemodel.NodeID]string{}, byID: map[profilemodel.NodeID]*exFrame{}}
	return b.build(frameBytes)
}

type explainedBuilder struct {
	v    *profilemodel.View
	r    explain.Result
	opts ExplainedPageOptions
	// historyRows is how many of the newest rejected rows the page lists;
	// < 0 lists them all.
	historyRows int
	view        string
	specs       []exRowSpec
	frames      []*exFrame
	refs        map[profilemodel.NodeID]string // node -> "f<k>" for drawn nodes
	byID        map[profilemodel.NodeID]*exFrame
}

func (b *explainedBuilder) build(frameBytes int64) (*explainedPageData, error) {
	v := b.v
	b.view = fmt.Sprintf("%s/%s#%d", v.SampleType, v.Unit, v.Index)
	b.specs = b.rowSpecs()
	data := &explainedPageData{
		LightChrome: lightChrome,
		DarkChrome:  template.CSS(darkChrome),
		Palette:     template.CSS(paletteCSS + jitterCSS),
		Title:       cmp.Or(b.opts.Title, b.opts.ProfileName, "Explained flame graph"),
		ProfileName: b.opts.ProfileName,
		Digest:      b.opts.Digest,
		Attempt:     b.opts.Attempt,
		View:        b.view,
		GPU:         v.IsGPU(),
		Total:       FormatValue(v.Total, v.Unit),
		TotalTitle:  strconv.FormatInt(v.Total, 10) + " " + v.Unit + ", exact",
		Script:      template.JS(explainPageScript),
	}
	data.CheckedPill = fmt.Sprintf("%d figures and references checked", b.r.Figures+b.r.References)
	if b.r.Figures+b.r.References == 1 {
		data.CheckedPill = "1 figure or reference checked"
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

	if err := b.layout(data, frameBytes); err != nil {
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
	data.SelFacts = b.factRows(sel)
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
	data.Rejected, data.RejectedPill, data.LimitPill = b.rejected()
	if n := len(data.Rejected) - b.historyRows; b.historyRows >= 0 && n > 0 {
		data.Rejected = data.Rejected[n:]
		data.RejectedNote = fmt.Sprintf("%s beyond the page's size budget %s not listed; the pills count them.", plural(n, "earlier row", "earlier rows"), isAre(n))
	}
	if b.opts.HistoryIncomplete {
		data.HistoryPill = "earlier attempt history for this view may be incomplete"
	}
	data.Legend = legendOf(data.Frames)

	model, err := json.Marshal(exModel{View: b.view, Selected: sel.ID, FactRows: b.specs, Frames: b.frames, Prompts: prompts})
	if err != nil {
		return nil, err
	}
	data.Model = template.JS(model)
	return data, nil
}

// rowSpecs are the fact rows every frame has, in the order of
// exFrame.Facts.
func (b *explainedBuilder) rowSpecs() []exRowSpec {
	unit := b.v.Unit
	exact := profilemodel.RoundingFor(profilemodel.MetricCumulative)
	ratio := profilemodel.RoundingFor(profilemodel.MetricShareOfTotal)
	return []exRowSpec{
		{Label: "Cumulative", Unit: unit, Rounding: exact},
		{Label: "Self", Unit: unit, Rounding: exact},
		{Label: "Share of total", Note: "of the view total", Unit: profilemodel.UnitRatio, Denominator: profilemodel.DenominatorViewTotal, Rounding: ratio},
		{Label: "Share of parent", ParentNote: true, Unit: profilemodel.UnitRatio, Denominator: "parent", Rounding: ratio},
		{Label: "Inexact portion", Note: "from inexact samples", Unit: unit, Rounding: exact},
	}
}

// factRows are a frame's facts as the panel shows them: the shared rows
// with this frame's values, then its extra rows. The script builds the
// same rows from the model.
func (b *explainedBuilder) factRows(f *exFrame) []exFactRow {
	var parentName string
	if p := b.byID[profilemodel.NodeID(f.Parent)]; p != nil {
		parentName = p.Name
	}
	out := make([]exFactRow, 0, len(b.specs)+len(f.Extra))
	for i, spec := range b.specs {
		val := f.Facts[i]
		row := exFactRow{Label: spec.Label, Value: val.Value, Title: val.Value, Note: spec.Note}
		if spec.ParentNote && parentName != "" {
			row.Note = "of " + parentName
		}
		if val.Canonical != "" {
			den := spec.Denominator
			if den == "parent" {
				den = "parent:" + f.Parent
			}
			row.Title = exactTitle(val.Canonical, spec.Unit, den, spec.Rounding, val.Zero)
		}
		out = append(out, row)
	}
	return append(out, f.Extra...)
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

// leftOut counts the nodes not drawn, by why.
type leftOut struct{ narrow, limit, size int }

// Why a node at least minDrawFraction wide is left out.
const (
	reasonLimit = iota
	reasonSize
)

// layout chooses the frames to draw, breadth first so the widest and
// shallowest come first, and places each at its offset within its parent:
// left and width are percentages of the view total, top is its depth.
// Forced nodes are always drawn. Any other node is drawn only when it is
// at least minDrawFraction of the total, the MaxNodes budget has room and
// its estimated bytes fit in frameBytes (unless frameBytes < 0). A node
// not drawn takes everything below it along; they are counted instead.
func (b *explainedBuilder) layout(data *explainedPageData, frameBytes int64) error {
	v := b.v
	forced := b.forced()
	maxNodes := cmp.Or(b.opts.MaxNodes, defaultExplainedMaxNodes)
	nodeBudget := max(0, maxNodes-len(forced))
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
	var out leftOut
	var usedBytes int64
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
			Tip:   f.Facts[0].Value + ", " + f.Share + " of total",
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
			drawn := true
			switch {
			case forced[c.ID]:
			case !wide(c) || unforced >= nodeBudget:
				countLeftOut(c, wide, reasonLimit, &out)
				drawn = false
			case frameBytes >= 0 && usedBytes+frameEstimate(c) > frameBytes:
				countLeftOut(c, wide, reasonSize, &out)
				drawn = false
			default:
				unforced++
				usedBytes += frameEstimate(c)
			}
			if drawn {
				queue = append(queue, queued{n: c, offset: offset, jitter: (i * 3) % jitterSteps})
			}
			offset += c.Cum
		}
	}
	data.Height = strconv.Itoa((maxDepth + 1) * explainedRowPitch)

	minWidth := strconv.FormatFloat(minDrawFraction*100, 'f', -1, 64) + "%"
	if out.narrow > 0 {
		data.NarrowNote = fmt.Sprintf("%s narrower than %s %s not drawn.", plural(out.narrow, "node", "nodes"), minWidth, isAre(out.narrow))
	}
	if out.limit > 0 {
		data.LimitNote = fmt.Sprintf("%d of %d frames drawn; %d left out by the page's limit of %d frames, after the explained and referenced paths, which are always drawn.",
			len(b.frames), len(b.frames)+out.narrow+out.limit+out.size, out.limit, maxNodes)
	}
	if out.size > 0 {
		data.SizeNote = fmt.Sprintf("%s beyond the page's size budget %s not drawn.", plural(out.size, "node", "nodes"), isAre(out.size))
	}
	return nil
}

// countLeftOut counts n and everything below it as not drawn: as narrow
// when a node is under minDrawFraction, for reason otherwise.
func countLeftOut(n *profilemodel.Node, wide func(*profilemodel.Node) bool, reason int, out *leftOut) {
	stack := []*profilemodel.Node{n}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch {
		case !wide(m):
			out.narrow++
		case reason == reasonSize:
			out.size++
		default:
			out.limit++
		}
		stack = append(stack, m.Children...)
	}
}

// frameEstimate bounds the bytes drawing n adds: its div and model entry,
// and its name escaped once for HTML text and once as a JSON string.
func frameEstimate(n *profilemodel.Node) int64 {
	return frameOverhead + htmlEscapedBound(n.Function.Name) + jsonEscapedBound(n.Function.Name)
}

// htmlEscapedBound bounds the length of s as html/template escapes text:
// "&amp;", "&#39;", "&#34;" and "&#43;" are 5 bytes, "&lt;" and "&gt;" 4,
// and a NUL or an invalid byte becomes U+FFFD, 3.
func htmlEscapedBound(s string) int64 {
	var size int64
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '&', c == '\'', c == '"', c == '+':
			size += 5
		case c == '<', c == '>':
			size += 4
		case c == 0:
			size += 3
		case c < utf8.RuneSelf:
			size++
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && n == 1 {
				size += 3
			} else {
				size += int64(n)
			}
			i += n
			continue
		}
		i++
	}
	return size
}

// jsonEscapedBound bounds the length of s as encoding/json writes it in a
// string: "<", ">", "&" and control bytes as "\u00XX", 6 bytes; '"' and
// "\\" as 2; an invalid byte as "\ufffd", 6; U+2028 and U+2029, 3 bytes
// each, as 6.
func jsonEscapedBound(s string) int64 {
	var size int64
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '<', c == '>', c == '&', c < 0x20:
			size += 6
		case c == '"', c == '\\':
			size += 2
		case c < utf8.RuneSelf:
			size++
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && n == 1:
				size += 6
			case r == '\u2028', r == '\u2029':
				size += 6
			default:
				size += int64(n)
			}
			i += n
			continue
		}
		i++
	}
	return size
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
		Facts: []exFactVal{valueOf(cf.Cumulative), valueOf(cf.Self), valueOf(cf.ShareOfTotal), valueOf(cf.ShareOfParent), valueOf(cf.InexactPortion)},
	}
	if n.Parent != nil {
		f.Parent = string(n.Parent.ID)
	}
	b.frames = append(b.frames, f)
	b.byID[n.ID] = f
	return f, nil
}

// valueOf is a fact's value in a shared row.
func valueOf(f profilemodel.Fact) exFactVal {
	c, ok := f.Canonical()
	if f.Status == profilemodel.StatusUnavailable || !ok {
		return exFactVal{Value: factValue(f)}
	}
	return exFactVal{Value: displayValue(c, f.Unit, f.Status), Canonical: c, Zero: f.Status == profilemodel.StatusZero}
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
			f.Extra = append(f.Extra,
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
		subject = b.nodeName(c.Node)
	}
	switch {
	case c.Module == nil:
	case *c.Module == "":
		subject += ", frames with no module"
	default:
		subject += ", module " + *c.Module
	}
	return fmt.Sprintf("%s of %s: %s; checked by perf-agent", c.Metric, subject,
		exactTitle(c.Value, c.Unit, c.Denominator, c.Rounding, c.Status == profilemodel.StatusZero))
}

// nodeName is "<name> (node <id>)", or "node <id>" for a node the view
// does not have.
func (b *explainedBuilder) nodeName(id profilemodel.NodeID) string {
	if n, ok := b.v.Node(id); ok {
		return n.Function.Name + " (node " + string(id) + ")"
	}
	return "node " + string(id)
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
			s.WriteString(promptName(f.Name))
		default:
			s.WriteString(part.Text)
		}
	}
	return s.String()
}

// promptName is name cut to maxPromptName characters, ending in "…" when
// it was cut.
func promptName(name string) string {
	if utf8.RuneCountInString(name) <= maxPromptName {
		return name
	}
	return string([]rune(name)[:maxPromptName]) + "…"
}

// rejected lists the failures of the rejected attempts and the pills that
// count them; an attempt whose record left failures out ends in a row
// saying how many. A failure naming a claim is a claim row, shown once per
// claim per attempt; a limit failure is a limit row; any other failure is
// about a reference. Claims are counted per attempt, so a claim refused in
// two attempts counts twice and the pill says how many attempts. The claims
// and references a record left out are counted too, though not listed. Every
// row of an attempt checked against other evidence than the page's says
// which, so its engine values are not read as this page's.
func (b *explainedBuilder) rejected() (rows []exRejected, claimsPill, limitsPill string) {
	var claims, refs, limits, attempts int
	for _, a := range b.opts.Rejected {
		seen := map[string]bool{}
		counted := false
		for _, f := range a.Failures {
			row := exRejected{Attempt: a.Attempt, Evidence: a.Evidence, Code: string(f.Code), Reason: f.Message}
			switch {
			case f.Claim != "":
				if seen["claim\x00"+f.Claim] {
					continue
				}
				seen["claim\x00"+f.Claim] = true
				claims++
				counted = true
				row.Kind, row.Label, row.Agent = "claim", "claim "+f.Claim, f.Agent
				if e := f.Engine; e != nil {
					row.Metric, row.Engine = e.Metric, displayEngine(*e)
					row.Node = "the whole view"
					if e.Scope != explain.ScopeView {
						row.Node = b.nodeName(e.Node)
					}
				}
			case f.Code == explain.CodeLimitExceeded:
				limits++
				row.Kind, row.Label = "limit", strings.TrimSpace("limit "+f.Ref)
			default:
				key := "ref\x00" + string(f.Code) + "\x00" + f.Ref
				if seen[key] {
					continue
				}
				seen[key] = true
				refs++
				counted = true
				row.Kind, row.Label = "reference", referenceLabel(f)
			}
			rows = append(rows, row)
		}
		if a.Omitted > 0 {
			rows = append(rows, exRejected{Attempt: a.Attempt, Evidence: a.Evidence, Kind: "more", Label: "and " + plural(a.Omitted, "more failure", "more failures")})
		}
		if a.TooLarge > 0 {
			rows = append(rows, exRejected{Attempt: a.Attempt, Evidence: a.Evidence, Kind: "more", Label: plural(a.TooLarge, "failure", "failures") + " too large to keep"})
		}
		claims += a.OmittedClaims
		refs += a.OmittedReferences
		if a.OmittedClaims+a.OmittedReferences > 0 {
			counted = true
		}
		if counted {
			attempts++
		}
	}
	var parts []string
	if claims > 0 {
		parts = append(parts, plural(claims, "claim", "claims"))
	}
	if refs > 0 {
		parts = append(parts, plural(refs, "reference", "references"))
	}
	if len(parts) > 0 {
		claimsPill = strings.Join(parts, " and ") + " rejected at render"
		if attempts > 1 {
			claimsPill += fmt.Sprintf(" in %d attempts", attempts)
		}
	}
	if n := b.opts.DroppedRejected; n > 0 {
		dropped := plural(n, "earlier rejected attempt", "earlier rejected attempts") + " not kept"
		if claimsPill != "" {
			dropped = claimsPill + ", plus " + dropped
		}
		claimsPill = dropped
	}
	if limits > 0 {
		limitsPill = plural(limits, "limit", "limits") + " exceeded at render"
	}
	return rows, claimsPill, limitsPill
}

// referenceLabel names the reference a failure is about: the placeholder
// as the agent wrote it, or the explanation's node.
func referenceLabel(f explain.Failure) string {
	switch f.Code {
	case explain.CodeUnresolvedClaimRef, explain.CodeGluedToClaim:
		return "reference {" + f.Ref + "}"
	case explain.CodeUnresolvedNodeRef:
		if strings.HasPrefix(f.Ref, "{") {
			return "reference " + f.Ref
		}
		return "reference {n:" + f.Ref + "}"
	}
	if f.Ref == "" {
		return "reference"
	}
	return "reference node " + f.Ref
}

// plural is "1 <one>" or "<n> <many>".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
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
	return displayValue(c, f.Unit, f.Status), exactTitle(c, f.Unit, f.Denominator, f.Rounding, f.Status == profilemodel.StatusZero)
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
// rounding: "0.411 ratio of view_total, rounded to 0.001 ratio". The
// script's exactTitle writes the same text.
func exactTitle(canonical, unit, denominator, rounding string, zero bool) string {
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
	if zero {
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
