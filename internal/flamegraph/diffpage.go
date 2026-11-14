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
	BaseName, NewName       string
	Empty, OneEmpty         bool
	Warnings, Notes         []string
	Root                    *diffNodeView
	Rows                    []diffRow
	RowsTotal               int
	RowsTruncated           bool
	Omitted                 int
	FullScale               string
	BaseNotes, NewNotes     []string
}

// DiffPageOptions name what a diff page shows about its inputs.
type DiffPageOptions struct {
	Title string
	// BaseName and NewName name the sides, typically by input file name;
	// each side's handle is shown on hover. An empty name shows the handle.
	BaseName, NewName string
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
func WriteDiffPage(w io.Writer, d *profilemodel.Diff, opts DiffPageOptions) error {
	if d == nil {
		return errors.New("flamegraph: nil diff")
	}
	data := &diffPageData{
		PageTitle:   opts.Title,
		LightChrome: lightChrome,
		DarkChrome:  darkChrome,
		SampleType:  d.SampleType,
		Unit:        d.Unit,
		BaseTotal:   withHuman(strconv.FormatInt(d.BaseTotal, 10), d.BaseTotal, d.Unit),
		NewTotal:    withHuman(strconv.FormatInt(d.NewTotal, 10), d.NewTotal, d.Unit),
		BaseHandle:  string(d.BaseHandle),
		NewHandle:   string(d.NewHandle),
		BaseName:    cmp.Or(opts.BaseName, string(d.BaseHandle)),
		NewName:     cmp.Or(opts.NewName, string(d.NewHandle)),
		BaseNotes:   d.BaseNotes,
		NewNotes:    d.NewNotes,
	}
	// A note that a comparison does not apply to the view type is not a
	// degradation, so it is not drawn as one.
	for _, w := range d.QualityWarnings {
		if w == profilemodel.WarningPerturbationNotGPU {
			data.Notes = append(data.Notes, w)
		} else {
			data.Warnings = append(data.Warnings, w)
		}
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
	fmt.Fprintf(&s, "%s\nbase %s, new %s\nchange %s", name,
		withHuman(strconv.FormatInt(n.BaseCum, 10), n.BaseCum, b.unit),
		withHuman(strconv.FormatInt(n.NewCum, 10), n.NewCum, b.unit),
		signed(n.DeltaAbs, b.unit))
	if shareOK {
		fmt.Fprintf(&s, "\nchange in share of own total %+.2f points", share*100)
	} else {
		s.WriteString("\nchange in share of own total unavailable")
	}
	var ids []string
	if n.BaseID != "" {
		ids = append(ids, "base id "+string(n.BaseID))
	}
	if n.NewID != "" {
		ids = append(ids, "new id "+string(n.NewID))
	}
	if len(ids) > 0 {
		fmt.Fprintf(&s, "\n%s", strings.Join(ids, ", "))
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

// signed is a change as an exact signed value, with a readable one beside
// it when the unit has one.
func signed(v int64, unit string) string { return withHuman(fmt.Sprintf("%+d", v), v, unit) }

// withHuman is exact (v as text) in unit, followed by v in a readable unit
// when there is one: "2490000000 nanoseconds (2.490 s)". A signed exact
// value gets a signed readable one.
func withHuman(exact string, v int64, unit string) string {
	out := exact + " " + unit
	h := humanValue(v, unit)
	if h == "" {
		return out
	}
	if v > 0 && strings.HasPrefix(exact, "+") {
		h = "+" + h
	}
	return out + " (" + h + ")"
}

// humanValue is v scaled to a readable unit for nanoseconds (as the flame
// graph shows durations) and bytes (binary multiples), or "" for any other
// unit. The magnitude is unsigned so math.MinInt64 does not wrap.
func humanValue(v int64, unit string) string {
	sign, mag := "", uint64(v)
	if v < 0 {
		sign, mag = "-", -uint64(v)
	}
	f := float64(mag)
	switch unit {
	case "nanoseconds":
		switch {
		case mag < 1_000:
			return fmt.Sprintf("%s%d ns", sign, mag)
		case mag < 1_000_000:
			return fmt.Sprintf("%s%.2f µs", sign, f/1e3)
		case mag < 1_000_000_000:
			return fmt.Sprintf("%s%.2f ms", sign, f/1e6)
		}
		return fmt.Sprintf("%s%.3f s", sign, f/1e9)
	case "bytes":
		if mag < 1<<10 {
			return fmt.Sprintf("%s%d B", sign, mag)
		}
		prefixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
		i := 0
		for f /= 1 << 10; f >= 1<<10 && i < len(prefixes)-1; i++ {
			f /= 1 << 10
		}
		return fmt.Sprintf("%s%.2f %s", sign, f, prefixes[i])
	}
	return ""
}

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
