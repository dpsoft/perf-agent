package flamegraph

import (
	"cmp"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

//go:embed diffpage.tmpl
var diffPageSource string

var diffPageTmpl = template.Must(template.New("diffpage").Parse(diffPageSource))

const (
	// minDrawFraction is the smallest share of the root's width a node is
	// drawn at; narrower nodes cannot carry a label or be hovered, and the
	// page stays small. Their weight still counts in their parent's width.
	minDrawFraction = 0.0005
	// maxTableRows bounds the removed/added table.
	maxTableRows = 500
	// fullScaleShare is the share shift drawn at full colour saturation
	// when no node shifts more than it; the scale otherwise follows the
	// largest shift so a small change is not drawn pale next to nothing.
	fullScaleShare = 0.01
	hueLower       = 215
	hueHigher      = 5
)

type diffNodeView struct {
	Name    string
	Tooltip string
	Width   string // percent of the parent's width
	Hue     string
	Tint    string
	Classes string
	Marker  string
	Kids    []*diffNodeView
}

type diffRow struct {
	Path           string
	Presence       string
	Delta          string
	Interpretation string
	Why            string
	absDelta       int64
}

type diffPageData struct {
	LightChrome, DarkChrome template.CSS
	PageTitle               string
	SampleType, Unit        string
	BaseTotal, NewTotal     string
	BaseHandle, NewHandle   string
	Empty, OneEmpty         bool
	Warnings                []string
	Root                    *diffNodeView
	Rows                    []diffRow
	RowsTotal               int
	RowsTruncated           bool
	Omitted                 int
	FullScale               string
	BaseNotes, NewNotes     []string
}

// WriteDiffPage renders d as one self-contained HTML page: an icicle of
// the union of both views, a table of the paths present on one side only,
// and the diff's quality warnings.
//
// Width is Combined over the root's Combined, so a removed or added node
// keeps its width. Colour is DeltaShare, the change in a node's share of
// its own view's total, so it stays meaningful when the totals differ; the
// raw DeltaAbs in the sample unit is always shown beside it. A node whose
// share is unavailable is grey.
func WriteDiffPage(w io.Writer, d *profilemodel.Diff, title string) error {
	if d == nil {
		return errors.New("flamegraph: nil diff")
	}
	data := &diffPageData{
		PageTitle:   title,
		LightChrome: lightChrome,
		DarkChrome:  darkChrome,
		SampleType:  d.SampleType,
		Unit:        d.Unit,
		BaseTotal:   fmt.Sprintf("%d %s", d.BaseTotal, d.Unit),
		NewTotal:    fmt.Sprintf("%d %s", d.NewTotal, d.Unit),
		BaseHandle:  string(d.BaseHandle),
		NewHandle:   string(d.NewHandle),
		Warnings:    d.QualityWarnings,
		BaseNotes:   d.BaseNotes,
		NewNotes:    d.NewNotes,
	}
	data.Empty = d.Root == nil || (d.BaseTotal == 0 && d.NewTotal == 0) || d.Root.Combined <= 0
	data.OneEmpty = !data.Empty && (d.BaseTotal == 0 || d.NewTotal == 0)
	if !data.Empty {
		b := &diffBuilder{unit: d.Unit, rootCombined: d.Root.Combined, scale: shareScale(d.Root)}
		data.Root = b.view(d.Root, d.Root.Combined)
		data.Omitted = b.omitted
		data.FullScale = strconv.FormatFloat(100*b.scale, 'f', 2, 64)
		b.collect(d.Root, nil, false)
		slices.SortStableFunc(b.rows, func(x, y diffRow) int {
			return cmp.Or(cmp.Compare(y.absDelta, x.absDelta), strings.Compare(x.Path, y.Path))
		})
		data.RowsTotal = len(b.rows)
		if len(b.rows) > maxTableRows {
			b.rows, data.RowsTruncated = b.rows[:maxTableRows], true
		}
		data.Rows = b.rows
	}
	return diffPageTmpl.Execute(w, data)
}

const lightChrome = template.CSS(`--bg:#f7f6f9;--panel:#ffffff;--ink:#1d1a17;--muted:#6b6259;--line:#e2dad0;--accent:#b4522a;--warn-bg:#fff6e8;--warn-line:#e8c88a`)

type diffBuilder struct {
	unit         string
	rootCombined int64
	scale        float64
	rows         []diffRow
	omitted      int
}

// shareScale is the share shift drawn at full saturation.
func shareScale(root *profilemodel.DiffNode) float64 {
	m := fullScaleShare
	var walk func(n *profilemodel.DiffNode)
	walk = func(n *profilemodel.DiffNode) {
		if s, ok := shareOf(n); ok {
			m = max(m, math.Abs(s))
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	return m
}

// shareOf is the node's DeltaShare when it is available and finite.
func shareOf(n *profilemodel.DiffNode) (float64, bool) {
	f := n.DeltaShare
	if f.Status == profilemodel.StatusUnavailable || f.Ratio == nil || math.IsNaN(*f.Ratio) || math.IsInf(*f.Ratio, 0) {
		return 0, false
	}
	return *f.Ratio, true
}

func (b *diffBuilder) view(n *profilemodel.DiffNode, parentCombined int64) *diffNodeView {
	v := &diffNodeView{Name: n.Name, Width: percent(n.Combined, parentCombined)}
	if v.Name == "" {
		v.Name = "(all)"
	}
	classes := []string{"b"}
	share, ok := shareOf(n)
	switch {
	case !ok:
		classes = append(classes, "share-none")
	case share < 0:
		classes = append(classes, "share-lower")
	case share > 0:
		classes = append(classes, "share-higher")
	default:
		classes = append(classes, "share-same")
	}
	if ok && share != 0 {
		v.Hue = strconv.Itoa(hueLower)
		if share > 0 {
			v.Hue = strconv.Itoa(hueHigher)
		}
		v.Tint = strconv.FormatFloat(min(1, math.Abs(share)/b.scale), 'f', 3, 64)
	} else {
		v.Hue, v.Tint = "0", "0"
	}
	switch n.Presence {
	case profilemodel.PresenceOnlyBase:
		classes = append(classes, "only-base")
	case profilemodel.PresenceOnlyNew:
		classes = append(classes, "only-new")
	}
	if n.Match == profilemodel.MatchAmbiguous {
		classes = append(classes, "ambiguous")
	}
	if n.Interpretation == profilemodel.NotComparable {
		classes = append(classes, "not-comparable")
		v.Marker = "!"
	}
	v.Classes = strings.Join(classes, " ")
	v.Tooltip = b.tooltip(n, share, ok)
	for _, c := range n.Children {
		if float64(c.Combined) < minDrawFraction*float64(b.rootCombined) || c.Combined <= 0 {
			if c.Combined > 0 {
				b.omitted += countNodes(c)
			}
			continue
		}
		v.Kids = append(v.Kids, b.view(c, n.Combined))
	}
	return v
}

func (b *diffBuilder) tooltip(n *profilemodel.DiffNode, share float64, shareOK bool) string {
	var s strings.Builder
	name := n.Name
	if name == "" {
		name = "(all)"
	}
	fmt.Fprintf(&s, "%s\nbase %d %s, new %d %s\nchange %s", name, n.BaseCum, b.unit, n.NewCum, b.unit, signed(n.DeltaAbs, b.unit))
	if shareOK {
		fmt.Fprintf(&s, "\nchange in share of own total %+.2f points", share*100)
	} else {
		s.WriteString("\nchange in share of own total unavailable")
	}
	fmt.Fprintf(&s, "\npresence %s", n.Presence)
	if n.Match != "" {
		fmt.Fprintf(&s, ", matched %s", n.Match)
	}
	if n.Interpretation == profilemodel.NotComparable {
		fmt.Fprintf(&s, "\nnot comparable: %s", n.Why)
	} else if n.Why != "" {
		fmt.Fprintf(&s, "\nnote: %s", n.Why)
	}
	return s.String()
}

// collect lists the topmost one-sided nodes: everything below one is
// one-sided too, and listing it would repeat the same removal or addition.
func (b *diffBuilder) collect(n *profilemodel.DiffNode, path []string, parentOneSided bool) {
	if n.Name != "" || len(path) > 0 {
		path = append(path[:len(path):len(path)], n.Name)
	}
	oneSided := n.Presence != profilemodel.PresenceBoth
	if oneSided && !parentOneSided {
		b.rows = append(b.rows, diffRow{
			Path:           strings.Join(path, " > "),
			Presence:       string(n.Presence),
			Delta:          signed(n.DeltaAbs, b.unit),
			Interpretation: string(n.Interpretation),
			Why:            n.Why,
			absDelta:       absDelta(n.DeltaAbs),
		})
	}
	for _, c := range n.Children {
		b.collect(c, path, oneSided)
	}
}

func signed(v int64, unit string) string { return fmt.Sprintf("%+d %s", v, unit) }

// percent is part/whole as a CSS percentage; whole is positive.
func percent(part, whole int64) string {
	if whole <= 0 {
		return "0"
	}
	return strconv.FormatFloat(100*float64(part)/float64(whole), 'f', 4, 64)
}

// countNodes counts n and everything below it: a node left out of the
// drawing takes its subtree with it.
func countNodes(n *profilemodel.DiffNode) int {
	c := 1
	for _, k := range n.Children {
		c += countNodes(k)
	}
	return c
}

// absDelta is |v|, saturating at MaxInt64 for MinInt64.
func absDelta(v int64) int64 {
	switch {
	case v == math.MinInt64:
		return math.MaxInt64
	case v < 0:
		return -v
	}
	return v
}
