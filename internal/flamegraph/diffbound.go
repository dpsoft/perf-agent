package flamegraph

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// Upper bounds of the diff page's pieces besides the strings they carry,
// which are bounded by htmlEscapedBound where they appear.
const (
	// diffNodeMarkup is one drawn node's divs and label span, 198 bytes
	// with its width, hue, tint, classes and marker at their longest.
	diffNodeMarkup = 208
	// diffTooltipFixed is a tooltip's fixed words and separators, 112
	// bytes.
	diffTooltipFixed = 128
	// diffHumanBound is the readable value withHuman adds, its brackets
	// and space included, as the page escapes it: "(+9223372036.855 s)"
	// is 20 bytes, and its "+" is written as "&#43;", 4 more.
	diffHumanBound = 24
	// diffSignBound is a value's sign as the page escapes it: "+" is
	// "&#43;".
	diffSignBound = 5
	// diffRowMarkup is one table row's cells, 67 bytes.
	diffRowMarkup = 80
	// diffItemMarkup is the markup around one warning or note.
	diffItemMarkup = 64
	// diffChromeSlack covers the numbers the chrome prints: the totals,
	// the full-colour scale and the counts of nodes and rows.
	diffChromeSlack = 1 << 10
	// diffPathSeparator is " > " as HTML text writes it.
	diffPathSeparator = int64(len(" &gt; "))
)

// WriteDiffPageBounded writes WriteDiffPage's page of d to w only if it
// fits in maxBytes and draws at most maxNodes nodes; maxBytes <= 0 or
// maxNodes <= 0 leaves that measure unbounded. diffPageBound bounds both
// first, by a walk that builds nothing, and a page over either is refused
// with ErrPageTooLarge before any of it is built. A page within them is
// rendered into memory, its length checked again, and only then written:
// w receives the whole page or nothing. The bytes are WriteDiffPage's.
func WriteDiffPageBounded(w io.Writer, d *profilemodel.Diff, opts DiffPageOptions, maxBytes, maxNodes int) error {
	if d == nil {
		return errors.New("flamegraph: nil diff")
	}
	bound, nodes := diffPageBound(d, opts)
	if e := exportState(bound, nodes, maxBytes, maxNodes); !e.OK {
		return tooLarge("flamegraph: %s", e.Reason)
	}
	var buf bytes.Buffer
	// Without a byte budget the bound may be far above the page, so the
	// buffer grows as the page does.
	if maxBytes > 0 {
		buf.Grow(int(bound))
	}
	if err := WriteDiffPage(&buf, d, opts); err != nil {
		return err
	}
	if maxBytes > 0 && buf.Len() > maxBytes {
		return tooLarge("flamegraph: the static copy of this diff takes %d bytes, over its budget of %d bytes", buf.Len(), maxBytes)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// exportState says whether a page of bound bytes drawing nodes nodes fits
// maxBytes and maxNodes, where a limit <= 0 is no limit, and names each
// limit it goes over with its figure when it does not.
func exportState(bound int64, nodes, maxBytes, maxNodes int) diffExport {
	var over []string
	if maxBytes > 0 && bound > int64(maxBytes) {
		over = append(over, fmt.Sprintf("needs up to %d bytes, over its budget of %d bytes", bound, maxBytes))
	}
	if maxNodes > 0 && nodes > maxNodes {
		over = append(over, fmt.Sprintf("draws %d nodes, over its budget of %d nodes", nodes, maxNodes))
	}
	if len(over) == 0 {
		return diffExport{OK: true}
	}
	return diffExport{Reason: "a static copy of this diff " + strings.Join(over, ", and ")}
}

// diffPageBound bounds the bytes WriteDiffPage writes for d with opts, and
// counts the nodes it draws. It walks the union iteratively, applying the
// page's omission rule, and sums an upper bound of every piece the page
// prints: the chrome with its escaped strings, each drawn node's markup,
// label and tooltip, and the rows of the largest table the page can list.
// It builds nothing of the page; what it allocates is its stack, one frame
// per depth, and a table of maxTableRows row sizes.
func diffPageBound(d *profilemodel.Diff, opts DiffPageOptions) (bound int64, nodes int) {
	esc := htmlEscapedBound
	unit := esc(d.Unit)
	n := int64(len(diffPageSource)+len(lightChrome)+len(darkChrome)) + diffChromeSlack
	n += 2*esc(opts.Title) + esc(string(d.BaseHandle)) + esc(string(d.NewHandle)) +
		esc(cmp.Or(opts.BaseName, string(d.BaseHandle))) + esc(cmp.Or(opts.NewName, string(d.NewHandle)))
	n += 4*esc(d.SampleType) + 8*unit + valueBound(d.BaseTotal, unit) + valueBound(d.NewTotal, unit)
	for _, list := range [][]string{d.QualityWarnings, d.BaseNotes, d.NewNotes} {
		for _, s := range list {
			n += diffItemMarkup + esc(s)
		}
	}
	if d.Root == nil || (d.BaseTotal == 0 && d.NewTotal == 0) || d.Root.Combined <= 0 {
		return n, 0
	}

	rows := topSizes{h: make([]int64, 0, maxTableRows)}
	// A frame is a node being walked: its next child, and what its
	// children inherit.
	type frame struct {
		n    *profilemodel.DiffNode
		next int
		// path bounds the node's path in the table: every name from the
		// root down to it, escaped, with a separator each.
		path     int64
		drawn    bool
		oneSided bool
	}
	rootCombined := d.Root.Combined
	var stack []frame
	visit := func(dn *profilemodel.DiffNode, path int64, drawn, parentOneSided bool) {
		if drawn {
			nodes++
			n += diffNodeMarkup + 2*esc(cmp.Or(dn.Name, "(all)")) + tooltipBound(dn, unit)
		}
		oneSided := dn.Presence != profilemodel.PresenceBoth
		// A one-sided node under a one-sided parent is listed through it.
		if oneSided && !parentOneSided {
			rows.add(diffRowMarkup + path + esc(string(dn.Presence)) + valueBound(dn.DeltaAbs, unit) +
				esc(string(dn.Interpretation)) + esc(dn.Why))
		}
		stack = append(stack, frame{n: dn, path: path, drawn: drawn, oneSided: oneSided})
	}
	visit(d.Root, esc(d.Root.Name)+diffPathSeparator, true, false)
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.next == len(top.n.Children) {
			stack = stack[:len(stack)-1]
			continue
		}
		c := top.n.Children[top.next]
		top.next++
		visit(c, top.path+esc(c.Name)+diffPathSeparator, top.drawn && drawnOnDiffPage(c, rootCombined), top.oneSided)
	}
	return n + rows.sum, nodes
}

// tooltipBound bounds n's tooltip without its name, as the page escapes
// it: three values in the unit (escaped as unit is), the share line, the
// IDs, presence, match and cause.
func tooltipBound(n *profilemodel.DiffNode, unit int64) int64 {
	esc := htmlEscapedBound
	size := int64(diffTooltipFixed) + valueBound(n.BaseCum, unit) + valueBound(n.NewCum, unit) + valueBound(n.DeltaAbs, unit) +
		esc(string(n.BaseID)) + esc(string(n.NewID)) + esc(string(n.Presence)) + esc(string(n.Match)) + esc(n.Why)
	if s, ok := shareOf(n); ok {
		size += pointsBound(s * 100)
	}
	return size
}

// valueBound bounds v as withHuman or signed writes it, and the page
// escapes it, in a unit that escapes to unit bytes: its sign, its digits,
// a space, the unit and a readable value.
func valueBound(v int64, unit int64) int64 {
	digits := int64(1)
	for x := v / 10; x != 0; x /= 10 {
		digits++
	}
	return diffSignBound + digits + 1 + unit + diffHumanBound
}

// pointsBound bounds the length of x written by %+.2f, as the page
// escapes it: a sign, the integer digits (one more, in case rounding
// carries) and three for the point and two decimals.
func pointsBound(x float64) int64 {
	digits := int64(2)
	if a := math.Abs(x); a >= 1 {
		digits += int64(math.Floor(math.Log10(a)))
	}
	return diffSignBound + digits + 3
}

// topSizes sums the largest maxTableRows sizes it is given: a min-heap of
// them, so a table of any length is bounded by the rows the page can list.
type topSizes struct {
	h   []int64
	sum int64
}

func (t *topSizes) add(v int64) {
	if len(t.h) < cap(t.h) {
		t.h = append(t.h, v)
		t.sum += v
		for i := len(t.h) - 1; i > 0; {
			p := (i - 1) / 2
			if t.h[p] <= t.h[i] {
				break
			}
			t.h[p], t.h[i] = t.h[i], t.h[p]
			i = p
		}
		return
	}
	if len(t.h) == 0 || v <= t.h[0] {
		return
	}
	t.sum += v - t.h[0]
	t.h[0] = v
	for i := 0; ; {
		small, l, r := i, 2*i+1, 2*i+2
		if l < len(t.h) && t.h[l] < t.h[small] {
			small = l
		}
		if r < len(t.h) && t.h[r] < t.h[small] {
			small = r
		}
		if small == i {
			return
		}
		t.h[i], t.h[small] = t.h[small], t.h[i]
		i = small
	}
}
