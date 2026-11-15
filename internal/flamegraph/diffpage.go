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
		data.Rows, data.RowsTotal = diffTable(d.Root, d.Unit)
		data.RowsTruncated = data.RowsTotal > len(data.Rows)
	}
	return diffPageTmpl.Execute(w, data)
}

const lightChrome = template.CSS(`--bg:#f7f6f9;--panel:#ffffff;--ink:#1d1a17;--muted:#6b6259;--line:#e2dad0;--accent:#b4522a;--warn-bg:#fff6e8;--warn-line:#e8c88a`)

type diffBuilder struct {
	unit         string
	rootCombined int64
	scale        float64
	omitted      int
}

// shareScale is the share shift drawn at full saturation. The walk is
// iterative: a union is as deep as its stacks.
func shareScale(root *profilemodel.DiffNode) float64 {
	m := fullScaleShare
	stack := []*profilemodel.DiffNode{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s, ok := shareOf(n); ok {
			m = max(m, math.Abs(s))
		}
		stack = append(stack, n.Children...)
	}
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
	v.Hue, v.Tint, v.Classes, v.Marker, v.Tooltip = diffAppearance(n, b.scale, b.unit)
	for _, c := range n.Children {
		if !drawnOnDiffPage(c, b.rootCombined) {
			if c.Combined > 0 {
				b.omitted += countNodes(c)
			}
			continue
		}
		v.Kids = append(v.Kids, b.view(c, n.Combined))
	}
	return v
}

// drawnOnDiffPage says a child of a drawn node is drawn: it is at least
// minDrawFraction of the root's width rootCombined, and has weight.
func drawnOnDiffPage(c *profilemodel.DiffNode, rootCombined int64) bool {
	return !(float64(c.Combined) < minDrawFraction*float64(rootCombined) || c.Combined <= 0)
}

// diffAppearance is how a diff page draws n, at the colour scale scale
// (shareScale's) in the sample unit unit: its hue and tint, its classes,
// its marker and its tooltip.
func diffAppearance(n *profilemodel.DiffNode, scale float64, unit string) (hue, tint, classes, marker, tooltip string) {
	return diffAppearanceNamed(n, scale, unit, n.Name)
}

// diffAppearanceNamed is diffAppearance with the tooltip naming n as name,
// so the live page can name it as it shows it.
func diffAppearanceNamed(n *profilemodel.DiffNode, scale float64, unit, name string) (hue, tint, classes, marker, tooltip string) {
	cls := []string{"b"}
	share, ok := shareOf(n)
	switch {
	case !ok:
		cls = append(cls, "share-none")
	case share < 0:
		cls = append(cls, "share-lower")
	case share > 0:
		cls = append(cls, "share-higher")
	default:
		cls = append(cls, "share-same")
	}
	if ok && share != 0 {
		hue = strconv.Itoa(hueLower)
		if share > 0 {
			hue = strconv.Itoa(hueHigher)
		}
		tint = strconv.FormatFloat(min(1, math.Abs(share)/scale), 'f', 3, 64)
	} else {
		hue, tint = "0", "0"
	}
	switch n.Presence {
	case profilemodel.PresenceOnlyBase:
		cls = append(cls, "only-base")
	case profilemodel.PresenceOnlyNew:
		cls = append(cls, "only-new")
	}
	if n.Match == profilemodel.MatchAmbiguous {
		cls = append(cls, "ambiguous")
	}
	if n.Interpretation == profilemodel.NotComparable {
		cls = append(cls, "not-comparable")
		marker = "!"
	}
	return hue, tint, strings.Join(cls, " "), marker, diffTooltip(n, name, share, ok, unit)
}

// diffTooltip is n's tooltip, naming it as name ("(all)" when empty).
func diffTooltip(n *profilemodel.DiffNode, name string, share float64, shareOK bool, unit string) string {
	var s strings.Builder
	if name == "" {
		name = "(all)"
	}
	// Sized once for the fixed words, the values and the IDs.
	s.Grow(len(name) + len(n.Why) + 3*len(unit) + len(n.BaseID) + len(n.NewID) + 256)
	fmt.Fprintf(&s, "%s\nbase %s, new %s\nchange %s", name,
		withHuman(strconv.FormatInt(n.BaseCum, 10), n.BaseCum, unit),
		withHuman(strconv.FormatInt(n.NewCum, 10), n.NewCum, unit),
		signed(n.DeltaAbs, unit))
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

// diffTable is the table of removed and added paths under root: the
// topmost one-sided nodes (everything below one is one-sided too, and
// listing it would repeat the same removal or addition), largest change
// first, then by path, then in walk order, cut to maxTableRows, and how
// many there are in all.
//
// The walk is iterative and shares one path among the nodes it visits. Only the
// largest maxTableRows rows are kept as it goes, and a row's path is
// joined only when it is kept, so the table costs what it lists.
func diffTable(root *profilemodel.DiffNode, unit string) ([]diffRow, int) {
	t := rowTable{h: make([]tableRow, 0, maxTableRows)}
	var path []string
	type frame struct {
		n        *profilemodel.DiffNode
		next     int
		pathLen  int // len(path) before n's name
		oneSided bool
	}
	var stack []frame
	visit := func(n *profilemodel.DiffNode, parentOneSided bool) {
		pathLen := len(path)
		if n.Name != "" || len(path) > 0 {
			path = append(path, n.Name)
		}
		oneSided := n.Presence != profilemodel.PresenceBoth
		if oneSided && !parentOneSided {
			t.add(n, path)
		}
		stack = append(stack, frame{n: n, pathLen: pathLen, oneSided: oneSided})
	}
	visit(root, false)
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.next == len(top.n.Children) {
			path = path[:top.pathLen]
			stack = stack[:len(stack)-1]
			continue
		}
		c, oneSided := top.n.Children[top.next], top.oneSided
		top.next++
		visit(c, oneSided)
	}
	slices.SortFunc(t.h, func(x, y tableRow) int { return -x.order(y) })
	rows := make([]diffRow, 0, len(t.h))
	for _, r := range t.h {
		rows = append(rows, diffRow{
			Path:           r.path,
			Presence:       string(r.n.Presence),
			Delta:          signed(r.n.DeltaAbs, unit),
			Interpretation: string(r.n.Interpretation),
			Why:            r.n.Why,
			absDelta:       r.abs,
		})
	}
	return rows, t.total
}

// tableRow is a row the table keeps: its node, its change, its joined path
// and its place in the walk.
type tableRow struct {
	n    *profilemodel.DiffNode
	abs  int64
	path string
	seq  int
}

// order is > 0 when r is listed before o: a larger change, then a smaller
// path, then earlier in the walk.
func (r tableRow) order(o tableRow) int {
	return cmp.Or(cmp.Compare(r.abs, o.abs), strings.Compare(o.path, r.path), cmp.Compare(o.seq, r.seq))
}

// rowTable keeps the maxTableRows rows listed first, as a heap whose root
// is the one listed last, and counts every row offered.
type rowTable struct {
	h     []tableRow
	total int
}

func (t *rowTable) add(n *profilemodel.DiffNode, path []string) {
	seq := t.total
	t.total++
	abs := absDelta(n.DeltaAbs)
	if len(t.h) < cap(t.h) {
		t.h = append(t.h, tableRow{n: n, abs: abs, path: strings.Join(path, " > "), seq: seq})
		t.up(len(t.h) - 1)
		return
	}
	if len(t.h) == 0 {
		return
	}
	// Against the row listed last, decided before the path is joined.
	last := t.h[0]
	switch c := cmp.Compare(abs, last.abs); {
	case c < 0:
		return
	case c == 0 && comparePath(path, last.path) >= 0:
		// An equal path loses too: this row comes later in the walk.
		return
	}
	t.h[0] = tableRow{n: n, abs: abs, path: strings.Join(path, " > "), seq: seq}
	t.down(0)
}

func (t *rowTable) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if t.h[i].order(t.h[p]) >= 0 {
			return
		}
		t.h[i], t.h[p] = t.h[p], t.h[i]
		i = p
	}
}

func (t *rowTable) down(i int) {
	for {
		last, l, r := i, 2*i+1, 2*i+2
		if l < len(t.h) && t.h[l].order(t.h[last]) < 0 {
			last = l
		}
		if r < len(t.h) && t.h[r].order(t.h[last]) < 0 {
			last = r
		}
		if last == i {
			return
		}
		t.h[i], t.h[last] = t.h[last], t.h[i]
		i = last
	}
}

// comparePath compares strings.Join(parts, " > ") with s byte by byte,
// as strings.Compare would, without joining them.
func comparePath(parts []string, s string) int {
	at := 0
	chunk := func(c string) int {
		rest := s[at:]
		n := min(len(c), len(rest))
		if r := strings.Compare(c[:n], rest[:n]); r != 0 {
			return r
		}
		if len(c) > len(rest) {
			return 1
		}
		at += n
		return 0
	}
	for i, p := range parts {
		if i > 0 {
			if r := chunk(" > "); r != 0 {
				return r
			}
		}
		if r := chunk(p); r != 0 {
			return r
		}
	}
	if at < len(s) {
		return -1
	}
	return 0
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
	c := 0
	stack := []*profilemodel.DiffNode{n}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		c++
		stack = append(stack, m.Children...)
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
