package flamegraph

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// DiffLiveOptions name what a live model of a diff shows about its inputs,
// how large it may be and where its layout starts. Title, BaseName and
// NewName mean what DiffPageOptions' fields of the same names mean;
// MaxNodes and MaxBytes bound the model as LiveOptions' do, and are also
// the budget the model checks a static copy against.
type DiffLiveOptions struct {
	Title, BaseName, NewName string
	MaxNodes, MaxBytes       int
	// Focus, when set, is the union node the layout starts at: it is drawn
	// at depth 0 and full width with its descendants below it.
	Focus profilemodel.UnionID
	// Measure, when set, is MeasureDiff of the same diff, with the page
	// options its static copy is written with; the model then takes its
	// colour scale and export from it instead of walking the whole union
	// on every call. Nil measures the diff with the model's own Title,
	// BaseName and NewName.
	Measure *DiffMeasure
}

// DiffMeasure is what every live model of one diff computes from its whole
// union, whatever its focus or budget: the colour scale, and the bound and
// drawn node count of the static copy WriteDiffPageBounded checks. It never
// changes once made, so any goroutine may read it.
type DiffMeasure struct {
	scale float64
	bound int64
	nodes int
}

// MeasureDiff measures d for its live models, with opts the page options
// of its static copy.
func MeasureDiff(d *profilemodel.Diff, opts DiffPageOptions) *DiffMeasure {
	m := &DiffMeasure{}
	if d == nil {
		return m
	}
	m.bound, m.nodes = diffPageBound(d, opts)
	if d.Root != nil {
		m.scale = shareScale(d.Root)
	}
	return m
}

func (o DiffLiveOptions) pageOptions() DiffPageOptions {
	return DiffPageOptions{Title: o.Title, BaseName: o.BaseName, NewName: o.NewName}
}

// diffFrameOverhead bounds the bytes one drawn frame of a diff adds to the
// model besides its strings: its drawn entry and its frame entry, with
// every value in the sample unit at its longest.
const diffFrameOverhead = 1 << 10

// dfFrame is one union node as the live page's panel reads it. Every value
// is a display string computed here; titles carry the exact digits.
type dfFrame struct {
	ID                string `json:"id"`  // the union ID
	Ref               string `json:"ref"` // "f<k>", its index in the frames array
	Name              string `json:"name"`
	Parent            string `json:"parent,omitzero"`
	Presence          string `json:"presence"`
	Match             string `json:"match"`
	Interpretation    string `json:"interpretation"`
	Why               string `json:"why"`
	BaseCum           string `json:"base_cum"`
	NewCum            string `json:"new_cum"`
	DeltaAbs          string `json:"delta_abs"`
	DeltaShare        string `json:"delta_share"`
	DeltaShareTitle   string `json:"delta_share_title"`
	DeltaPerUnit      string `json:"delta_per_unit"`
	DeltaPerUnitTitle string `json:"delta_per_unit_title"`
}

// dfDrawn is one drawn frame: its place, and the colour, classes, marker
// and tooltip the diff page gives it. Left and width are percentages of
// the layout root's Combined, top is in px.
type dfDrawn struct {
	I      string `json:"i"`
	Left   string `json:"left"`
	Width  string `json:"width"`
	Top    string `json:"top"`
	Hue    string `json:"hue"`
	Tint   string `json:"tint"`
	Class  string `json:"cls"`
	Marker string `json:"marker,omitzero"`
	Tip    string `json:"tip"`
}

// dfFactRow is a row of the panel's facts: its label, the frame field it
// shows and the field holding its title, if any.
type dfFactRow struct {
	Label string `json:"k"`
	Field string `json:"f"`
	Title string `json:"t,omitzero"`
}

// diffExport says whether a static copy of the diff fits its budget, and
// why not when it does not.
type diffExport struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

type diffLiveModel struct {
	Kind       string      `json:"kind"`
	Title      string      `json:"doc_title"` // the document title; the heading is TitleSegs
	TitleSegs  []exSegment `json:"title_segs"`
	View       string      `json:"view"`
	SampleType string      `json:"sample_type"`
	Unit       string      `json:"unit"`
	BaseName   string      `json:"base_name"`
	NewName    string      `json:"new_name"`
	BaseHandle string      `json:"base_handle"`
	NewHandle  string      `json:"new_handle"`
	BaseTotal  string      `json:"base_total"`
	NewTotal   string      `json:"new_total"`
	Pill       string      `json:"pill"`
	Warnings   []string    `json:"warnings"`
	Notes      []string    `json:"notes"`
	BaseNotes  []string    `json:"base_notes"`
	NewNotes   []string    `json:"new_notes"`
	EmptyNote  string      `json:"empty_note"`
	Legend     []exLegend  `json:"legend"`
	Axis       string      `json:"axis"`
	Height     string      `json:"height"`
	Drawn      []dfDrawn   `json:"drawn"`
	NarrowNote string      `json:"narrow_note"`
	LimitNote  string      `json:"limit_note"`
	SizeNote   string      `json:"size_note"`
	FactRows   []dfFactRow `json:"fact_rows"`
	Frames     []*dfFrame  `json:"frames"`
	Selected   string      `json:"selected"`
	Focus      string      `json:"focus"`
	Export     diffExport  `json:"export"`
}

// DiffLiveModel is the live page's model of d, as compact JSON: the union
// laid out flat, every frame an absolutely placed sibling in pre-order, so
// its nesting does not grow with the stacks. Each drawn frame has the
// width, colour, classes, marker and tooltip WriteDiffPage gives it, its
// name shown at most maxShownName bytes long; every value is a string.
//
// With opts.MaxBytes the model is fitted to the budget as an explained
// model is: frames are left out, and counted in size_note, until it fits;
// a model that does not fit with only the layout's root drawn is
// ErrPageTooLarge. With opts.Focus the layout starts at that union node,
// and its callers stay in the model, undrawn, for the path panel; an
// unknown focus is profilemodel.ErrUnknownNode. The model's export says
// whether WriteDiffPageBounded would write a static copy, with the page
// options opts.Measure was made with, within opts.MaxBytes and
// opts.MaxNodes.
func DiffLiveModel(d *profilemodel.Diff, opts DiffLiveOptions) ([]byte, error) {
	if d == nil {
		return nil, errors.New("flamegraph: nil diff")
	}
	var path []*profilemodel.DiffNode
	if opts.Focus != "" {
		p, ok := profilemodel.FindDiffNode(d, opts.Focus)
		if !ok {
			return nil, fmt.Errorf("%w: %s", profilemodel.ErrUnknownNode, opts.Focus)
		}
		path = p
	}
	m := opts.Measure
	if m == nil {
		m = MeasureDiff(d, opts.pageOptions())
	}
	b := &diffLiveBuilder{d: d, opts: opts, scale: m.scale, export: exportState(m.bound, m.nodes, opts.MaxBytes, opts.MaxNodes)}
	render := func(frameBytes int64) ([]byte, error) {
		return json.Marshal(b.build(path, frameBytes))
	}
	if opts.MaxBytes <= 0 {
		return render(-1)
	}
	base, err := render(0)
	if err != nil {
		return nil, err
	}
	if len(base) > opts.MaxBytes {
		return nil, tooLarge("flamegraph: the live model of the diff needs %d bytes with only its root drawn, over the budget of %d bytes (MaxBytes)",
			len(base), opts.MaxBytes)
	}
	return fitFrames(opts.MaxBytes, base, render)
}

type diffLiveBuilder struct {
	d      *profilemodel.Diff
	opts   DiffLiveOptions
	scale  float64
	export diffExport
	frames []*dfFrame
}

// build lays the model out from the last node of path (the diff's root
// when path is empty) with frameBytes for frames besides the layout's
// root; frameBytes < 0 means no byte budget.
func (b *diffLiveBuilder) build(path []*profilemodel.DiffNode, frameBytes int64) *diffLiveModel {
	d := b.d
	b.frames = nil
	baseName := cmp.Or(b.opts.BaseName, string(d.BaseHandle))
	newName := cmp.Or(b.opts.NewName, string(d.NewHandle))
	untitled := "Diff of " + baseName + " and " + newName
	m := &diffLiveModel{
		Kind:       "diff",
		Title:      cmp.Or(b.opts.Title, untitled),
		TitleSegs:  titleSegments(ExplainedPageOptions{Title: b.opts.Title, ProfileName: untitled}),
		View:       d.View,
		SampleType: d.SampleType,
		Unit:       d.Unit,
		BaseName:   baseName,
		NewName:    newName,
		BaseHandle: string(d.BaseHandle),
		NewHandle:  string(d.NewHandle),
		BaseTotal:  withHuman(strconv.FormatInt(d.BaseTotal, 10), d.BaseTotal, d.Unit),
		NewTotal:   withHuman(strconv.FormatInt(d.NewTotal, 10), d.NewTotal, d.Unit),
		Pill:       "Diff of two views: every figure is computed by perf-agent",
		Warnings:   []string{},
		Notes:      []string{},
		BaseNotes:  nonNil(d.BaseNotes),
		NewNotes:   nonNil(d.NewNotes),
		Legend:     b.legend(),
		Axis:       fmt.Sprintf("width = base plus new %s of a node over the root's; colour = the change in its share of its own view's total", d.Unit),
		Height:     "0",
		Drawn:      []dfDrawn{},
		FactRows:   diffFactRows,
		Frames:     []*dfFrame{},
		Focus:      string(b.opts.Focus),
		Export:     b.export,
	}
	// As on the diff page, a comparison that does not apply to the view
	// type is a note, not a degradation.
	for _, w := range d.QualityWarnings {
		if w == profilemodel.WarningPerturbationNotGPU {
			m.Notes = append(m.Notes, w)
		} else {
			m.Warnings = append(m.Warnings, w)
		}
	}
	empty := d.Root == nil || (d.BaseTotal == 0 && d.NewTotal == 0) || d.Root.Combined <= 0
	if empty {
		m.EmptyNote = fmt.Sprintf("Both views are empty: neither has any %s (%s) to compare, so there is nothing to draw.", d.SampleType, d.Unit)
		return m
	}
	if d.BaseTotal == 0 || d.NewTotal == 0 {
		m.Warnings = append(m.Warnings, "One view is empty, so no node has a share of its total to compare; every node is grey and only the raw change is shown.")
	}
	root, callers := d.Root, []*profilemodel.DiffNode(nil)
	if len(path) > 0 {
		root, callers = path[len(path)-1], path[:len(path)-1]
		m.Axis += "; widths relative to " + shownName(cmp.Or(root.Name, "(all)"))
	}
	m.Selected = string(root.UnionID())
	b.layout(m, root, callers, frameBytes)
	return m
}

// diffFactRows are the panel's fact rows of a diff frame.
var diffFactRows = []dfFactRow{
	{Label: "Presence", Field: "presence"},
	{Label: "Matched", Field: "match"},
	{Label: "Interpretation", Field: "interpretation"},
	{Label: "Why", Field: "why"},
	{Label: "Base", Field: "base_cum"},
	{Label: "New", Field: "new_cum"},
	{Label: "Change", Field: "delta_abs"},
	{Label: "Change in share of own total", Field: "delta_share", Title: "delta_share_title"},
	{Label: "Change per unit of work", Field: "delta_per_unit", Title: "delta_per_unit_title"},
}

// legend is the diff page's legend: the colour ramp at this diff's scale,
// then the swatches, each keyed by the class it explains.
func (b *diffLiveBuilder) legend() []exLegend {
	full := strconv.FormatFloat(100*max(b.scale, fullScaleShare), 'f', 2, 64)
	return []exLegend{
		{Key: "ramp", Label: fmt.Sprintf("lower / higher share of total %s (%s); full colour = ±%s points of share", b.d.SampleType, b.d.Unit, full)},
		{Key: "share-none", Label: "share unavailable or unchanged"},
		{Key: "only-base", Label: "only in base"},
		{Key: "only-new", Label: "only in new"},
		{Key: "not-comparable", Label: "not comparable (! marker, reason on hover)"},
		{Key: "ambiguous", Label: "ambiguous match"},
	}
}

// layout chooses the frames to draw from root down, breadth first so the
// widest and shallowest come first, and lists them in pre-order. A child
// is drawn when the diff page would draw it (at least minDrawFraction of
// root's width, with weight), the MaxNodes budget has room and its
// estimated bytes fit in frameBytes (unless frameBytes < 0). Siblings are
// placed as the diff page packs them: one it leaves out takes no room, one
// the budget leaves out keeps its place. A node not drawn takes its
// subtree along; they are counted instead. The callers of root follow,
// undrawn.
func (b *diffLiveBuilder) layout(m *diffLiveModel, root *profilemodel.DiffNode, callers []*profilemodel.DiffNode, frameBytes int64) {
	total := root.Combined
	type placed struct {
		left  int64
		depth int
	}
	chosen := map[*profilemodel.DiffNode]placed{root: {}}
	maxNodes := cmp.Or(b.opts.MaxNodes, defaultExplainedMaxNodes)
	nodeBudget := max(0, maxNodes-1)
	var narrow, limit, size, unforced int
	var used int64
	queue := []*profilemodel.DiffNode{root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		p := chosen[n]
		offset := p.left
		for _, c := range n.Children {
			if !drawnOnDiffPage(c, total) {
				if c.Combined > 0 {
					narrow += countNodes(c)
				}
				continue
			}
			switch est := b.frameEstimate(c); {
			case unforced >= nodeBudget:
				limit += countNodes(c)
			case frameBytes >= 0 && used+est > frameBytes:
				size += countNodes(c)
			default:
				unforced++
				used += est
				chosen[c] = placed{left: offset, depth: p.depth + 1}
				queue = append(queue, c)
			}
			offset += c.Combined
		}
	}

	var top *profilemodel.DiffNode
	if len(callers) > 0 {
		top = callers[len(callers)-1]
	}
	type item struct{ n, parent *profilemodel.DiffNode }
	stack := []item{{n: root, parent: top}}
	maxDepth := 0
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n, p := it.n, chosen[it.n]
		f := b.frame(n, it.parent)
		dr := dfDrawn{
			I:     strconv.Itoa(len(b.frames) - 1),
			Left:  percent(p.left, total),
			Width: percent(n.Combined, total),
			Top:   strconv.Itoa(p.depth * explainedRowPitch),
		}
		if total <= 0 && n == root {
			dr.Width = "100"
		}
		dr.Hue, dr.Tint, dr.Class, dr.Marker, dr.Tip = diffAppearanceNamed(n, b.scale, b.d.Unit, f.Name)
		m.Drawn = append(m.Drawn, dr)
		maxDepth = max(maxDepth, p.depth)
		for _, c := range slices.Backward(n.Children) {
			if _, ok := chosen[c]; ok {
				stack = append(stack, item{n: c, parent: n})
			}
		}
	}
	m.Height = strconv.Itoa((maxDepth + 1) * explainedRowPitch)
	var parent *profilemodel.DiffNode
	for _, c := range callers {
		b.frame(c, parent)
		parent = c
	}
	m.Frames = b.frames

	minWidth := strconv.FormatFloat(minDrawFraction*100, 'f', -1, 64) + "%"
	if narrow > 0 {
		m.NarrowNote = fmt.Sprintf("%d nodes under %s of the total are not drawn.", narrow, minWidth)
	}
	if limit > 0 {
		m.LimitNote = fmt.Sprintf("%d of %d frames drawn; %d left out by the page's limit of %d frames.",
			len(m.Drawn), len(m.Drawn)+narrow+limit+size, limit, maxNodes)
	}
	if size > 0 {
		m.SizeNote = fmt.Sprintf("%s beyond the page's size budget %s not drawn.", plural(size, "node", "nodes"), isAre(size))
	}
}

// frameEstimate bounds the bytes drawing n adds to the model: its entries
// and their strings, the name and the cause twice (the frame and the
// tooltip), the IDs in the frame, its parent's and the tooltip's, and the
// unit in every value.
func (b *diffLiveBuilder) frameEstimate(n *profilemodel.DiffNode) int64 {
	ids := int64(len(n.BaseID) + len(n.NewID) + 2)
	return diffFrameOverhead + 2*jsonEscapedBound(shownName(n.Name)) + 2*jsonEscapedBound(n.Why) + 4*ids + 10*jsonEscapedBound(b.d.Unit)
}

// frame adds n, whose union parent is parent (nil for the root), to the
// model.
func (b *diffLiveBuilder) frame(n, parent *profilemodel.DiffNode) *dfFrame {
	f := diffFrame(b.d, n, parent)
	f.Ref = "f" + strconv.Itoa(len(b.frames))
	b.frames = append(b.frames, f)
	return f
}

// diffFrame is n as the panel shows it, with no ref.
func diffFrame(d *profilemodel.Diff, n, parent *profilemodel.DiffNode) *dfFrame {
	unit := d.Unit
	f := &dfFrame{
		ID:             string(n.UnionID()),
		Name:           shownName(cmp.Or(n.Name, "(all)")),
		Presence:       string(n.Presence),
		Match:          string(n.Match),
		Interpretation: string(n.Interpretation),
		Why:            n.Why,
		BaseCum:        withHuman(strconv.FormatInt(n.BaseCum, 10), n.BaseCum, unit),
		NewCum:         withHuman(strconv.FormatInt(n.NewCum, 10), n.NewCum, unit),
		DeltaAbs:       signed(n.DeltaAbs, unit),
	}
	if parent != nil {
		f.Parent = string(parent.UnionID())
	}
	f.DeltaShare, f.DeltaShareTitle = factText(n.DeltaShare)
	if n.DeltaPerUnit != nil {
		f.DeltaPerUnit, f.DeltaPerUnitTitle = factText(*n.DeltaPerUnit)
	} else {
		// The work counts were missing or unusable for every node alike.
		f.DeltaPerUnit = "unavailable: " + cmp.Or(d.PerUnit.Reason, "the engine has no value")
		f.DeltaPerUnitTitle = f.DeltaPerUnit
	}
	return f
}

// dfPathItem is a path entry of a diff frame. A ref names a frame's place
// in one model, which a single frame does not know: the page names it.
type dfPathItem struct {
	ID         string `json:"id"`
	Ref        string `json:"ref"`
	Name       string `json:"name"`
	DeltaShare string `json:"delta_share"`
}

type dfLiveFrame struct {
	Node  string       `json:"node"`
	Frame *dfFrame     `json:"frame"`
	Path  []dfPathItem `json:"path"`
}

// DiffLiveFrame is union node id of d as the live page's panel shows it,
// drawn or not: its frame, as the model gives it but with no ref, and its
// path from below the root down to it. The node is looked up in ix, the
// index of d's whole union, never in a model, which a budget may have cut.
// An unknown node is profilemodel.ErrUnknownNode.
func DiffLiveFrame(d *profilemodel.Diff, ix *profilemodel.DiffIndex, id profilemodel.UnionID) ([]byte, error) {
	if d == nil || ix == nil {
		return nil, errors.New("flamegraph: nil diff or index")
	}
	path := ix.Path(id)
	if path == nil {
		return nil, fmt.Errorf("%w: %s", profilemodel.ErrUnknownNode, id)
	}
	n := path[len(path)-1]
	var parent *profilemodel.DiffNode
	if len(path) > 1 {
		parent = path[len(path)-2]
	}
	out := dfLiveFrame{Node: string(id), Frame: diffFrame(d, n, parent), Path: make([]dfPathItem, 0, len(path)-1)}
	for _, p := range path[1:] {
		out.Path = append(out.Path, dfPathItem{ID: string(p.UnionID()), Name: shownName(cmp.Or(p.Name, "(all)")), DeltaShare: factValue(p.DeltaShare)})
	}
	return json.Marshal(out)
}
