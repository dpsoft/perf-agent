// Package flamegraph renders folded stacks as a self-contained interactive
// HTML page: one file, no network fetches, no external script or font, and
// correct when opened straight off disk with file://.
//
// Colour encodes domain, not depth and not a hash of the name — see
// domain.go. The honesty rules, because a flame graph is very good at
// looking healthy when it is not:
//
//   - A degenerate profile (no samples, or nothing but zero values) renders
//     a page that SAYS so. It never renders an empty rectangle, and it never
//     renders a plausible-looking picture of nothing.
//   - The value axis is labelled with the profile's own type and unit.
//     Calling 5,987,854 nanoseconds "samples" is a lie this format invites.
//   - Frames that carry no symbol are drawn, hatched, and counted. Dropping
//     them would make the picture cleanest exactly when symbolization worked
//     worst.
//   - Frames whose CPU attribution came from an inferred GPU join are
//     hatched and counted, so an inferred caller never reads as an observed
//     one.
//   - The legend names each layer for what it is.
//   - Labels are summarised beside the graph rather than folded into it.
//     See internal/foldedstacks.Fold for why.
//
// Geometry is percentages, not pixels: a frame is an absolutely positioned
// block whose left and width are shares of the profile total, which are also
// shares of a container as wide as the window. The page therefore fills any
// viewport and reflows on resize without a line of script, and nothing about
// it scales — the row pitch and the label are the same number of pixels at
// 800px and at 1990px, because "fill the width" must mean more columns, not
// a bigger picture. See styleSheet in assets.go.
//
// Chrome discipline is async-profiler's: nothing permanent on the page
// explains the page. What is permanently visible is the graph, one line of
// status, and — only when the profile carries the misreading it guards
// against — one line of warning about launch sampling. The legend, the
// keyboard map, the remaining notes and the label table live behind the info
// button and the ? key.
//
// The one exception is deliberate. gpu_sample_period is a wrong-conclusion
// guard: without it a reader takes a 6.6% column for 6.6% of GPU time when
// the true figure is off by roughly the sample period, and nothing in the
// picture hints otherwise. The notes that moved are the ones a reader can
// derive from the picture (the width of [gpu:launch unsampled] IS the
// unattributed share) or see on the frames themselves (unsymbolized frames
// are hatched, named by their address, and say so on hover).
package flamegraph

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

// frameHeight is the row pitch in CSS pixels: a 17px frame and a 1px gap.
// It is the only fixed dimension the geometry has. Horizontal position and
// width are percentages, so they are the window's business, not ours — see
// styleSheet in assets.go for why that is the whole fix for a graph that
// used to be 1280px wide on a 1990px screen.
const frameHeight = 18

// Options configures a render.
//
// There is no Width. A frame's left and width are percentages of the chart,
// which is as wide as the window, so the page has no width to be told: it
// fills whatever it is opened in and reflows when that changes.
type Options struct {
	// Title names the page. Defaults to "Flame Graph".
	Title string
	// Subtitle is a one-line provenance string (source file, mode, host).
	Subtitle string
	// Meta are extra provenance items, listed in the info panel.
	Meta []MetaItem
}

// MetaItem is one provenance item.
type MetaItem struct {
	Label string
	Value string
}

type node struct {
	name    string
	module  string
	modSet  bool
	domain  Domain
	value   int64
	inexact int64
	// jitter is this frame's step on the shade ladder — see assignJitter.
	jitter int

	children []*node
	index    map[string]*node

	// x and width are percentages of the whole profile, which is also
	// percentages of the chart: the root is 0 to 100.
	x, width float64
	depth    int
}

// RenderHTML writes the standalone page.
func RenderHTML(w io.Writer, res *foldedstacks.Result, opts Options) error {
	opts = opts.withDefaults()
	if res == nil {
		return fmt.Errorf("flamegraph: nil result")
	}
	ew := &errWriter{w: w}

	root, maxDepth := buildTree(res)
	degenerate := res.Degenerate()

	ew.s("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	ew.s("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n<title>")
	ew.esc(opts.Title)
	ew.s("</title>\n<style>\n")
	ew.s(styleSheet)
	if degenerate {
		ew.s(reportCSS)
	} else {
		ew.s(paletteCSS)
		ew.s(jitterCSS)
		ew.s(rowCSS(maxDepth))
	}
	ew.s("</style>\n</head>\n<body>\n")

	if degenerate {
		writeReport(ew, res, opts)
		ew.s("</body>\n</html>\n")
		return ew.err
	}

	writeTopBar(ew, opts, res, root)
	writeSamplePeriodNote(ew, res)
	if err := writeChart(ew, root, maxDepth, res); err != nil {
		return err
	}
	writeStatusBar(ew, res)
	ew.s("<div id=\"tip\" hidden></div>\n")

	ew.s("<script>\n")
	ew.s(script)
	ew.s("</script>\n</body>\n</html>\n")
	return ew.err
}

// RenderFragment writes just the graph, for embedding in a host page: a
// <style> carrying the graph's own rules and the chart element itself. It
// brings the palette — a graph pulled out of the page must still be the
// right colours, and must still follow the reader's light or dark theme —
// but none of the interactivity, none of the legend and none of the warning
// text. It is therefore not a substitute for RenderHTML when the profile is
// degenerate: it returns an error rather than emitting a blank canvas.
//
// It takes no Options because it has nothing to spend them on: the page's
// title, subtitle and provenance all belong to chrome it does not emit.
//
// It was RenderSVG until the geometry stopped being SVG. There is one layout
// engine in this package and this is it; a second one kept alive for
// embedding would have been a second palette (fill/stroke beside
// background/box-shadow) and a second truncation rule, and they would have
// drifted.
func RenderFragment(w io.Writer, res *foldedstacks.Result) error {
	if res == nil {
		return fmt.Errorf("flamegraph: nil result")
	}
	if res.Degenerate() {
		return fmt.Errorf("flamegraph: refusing to draw a graph for a profile with no drawable samples (%d samples, total %d %s); use RenderHTML, which reports why",
			res.Samples, res.Total, res.Unit)
	}
	root, maxDepth := buildTree(res)
	ew := &errWriter{w: w}
	ew.s("<style>\n")
	ew.s(paletteCSS)
	ew.s(jitterCSS)
	ew.s(rowCSS(maxDepth))
	ew.s(embedCSS)
	ew.s("</style>\n")
	if err := writeChart(ew, root, maxDepth, res); err != nil {
		return err
	}
	return ew.err
}

// embedCSS is the handful of chrome rules a fragment cannot borrow from the
// host page: the box a frame is, minus everything about the page around it.
const embedCSS = `
.chart{position:relative}
.frame{position:absolute;height:17px;padding:0 4px;box-sizing:border-box;border-radius:2px;overflow:hidden;white-space:nowrap;text-overflow:ellipsis;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px;line-height:17px}
`

func (o Options) withDefaults() Options {
	if o.Title == "" {
		o.Title = "Flame Graph"
	}
	return o
}

// buildTree folds the stacks into a tree under a synthetic "all" root.
//
// The synthetic root is not decoration: without it, a profile with several
// disjoint root frames (which is every system-wide profile) has no
// full-width frame to click for "zoom out", and no anchor for percentages.
func buildTree(res *foldedstacks.Result) (*node, int) {
	root := &node{name: "all", index: map[string]*node{}, domain: DomainRoot}
	maxDepth := 1
	for _, st := range res.Stacks {
		if st.Value <= 0 {
			continue
		}
		root.value += st.Value
		root.inexact += st.Inexact
		cur := root
		for i, frame := range st.Frames {
			mod := ""
			if i < len(st.Modules) {
				mod = st.Modules[i]
			}
			child := cur.index[frame]
			if child == nil {
				child = &node{
					name:   frame,
					module: mod,
					modSet: true,
					index:  map[string]*node{},
					depth:  cur.depth + 1,
				}
				cur.index[frame] = child
				cur.children = append(cur.children, child)
			} else if child.module != mod {
				// Two samples disagree about which object this frame came
				// from. Rather than pick one, drop the module so the
				// classifier falls back to the symbol name and the page
				// never asserts a provenance the profile contradicts.
				child.module = ""
			}
			child.value += st.Value
			child.inexact += st.Inexact
			cur = child
			if cur.depth+1 > maxDepth {
				maxDepth = cur.depth + 1
			}
		}
	}
	classify(root)
	sortTree(root)
	assignJitter(root)
	return root, maxDepth
}

func classify(n *node) {
	if n.depth > 0 {
		n.domain = Classify(n.name, n.module)
	}
	for _, c := range n.children {
		classify(c)
	}
}

// sortTree orders siblings by name so two renders of the same profile
// produce byte-identical HTML and two flame graphs can be diffed.
func sortTree(n *node) {
	slices.SortFunc(n.children, func(a, b *node) int { return cmp.Compare(a.name, b.name) })
	for _, c := range n.children {
		sortTree(c)
	}
}

// assignJitter gives every frame a step on a shade ladder within its own
// domain's colour, so a run of frames that share a domain has visible
// boundaries. Seven consecutive unsymbolized vendor frames were previously
// one flat block of sand, and the profiles this renderer exists for are full
// of exactly that.
//
// This is async-profiler's getColor() with two differences. Theirs is
// Math.random() at draw time; ours is a hash of the frame name, because two
// renders of one profile must be byte-identical and a frame must keep its
// shade across a zoom. And theirs may collide with the neighbour it was
// meant to separate; ours walks the tree with the parent and the previous
// sibling in hand and steps away from both, so the case that motivated the
// change — a same-domain frame directly above or beside another — is
// separated by construction rather than by luck.
//
// The boundary greys and the synthetic root sit out. The two boundary fills
// differ by lightness on purpose (paler means nothing behind it), and a
// random lightness must not compete with a meaningful one.
func assignJitter(parent *node) {
	prevStep, prevDomain := -1, Domain(-1)
	for _, c := range parent.children {
		c.jitter = pickJitter(c, parent, prevStep, prevDomain)
		prevStep, prevDomain = c.jitter, c.domain
		assignJitter(c)
	}
}

// minJitterGap is how many ladder steps apart two touching same-domain
// frames must be. Three steps is 2.6% of white, which is a visible edge; one
// step is not.
const minJitterGap = 3

func pickJitter(n, parent *node, prevStep int, prevDomain Domain) int {
	if !jittered(n.domain) {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(n.name))
	step := int(h.Sum32() % jitterSteps)

	clash := func(s, other int) bool {
		d := s - other
		return d > -minJitterGap && d < minJitterGap
	}
	for range jitterSteps {
		bad := false
		if parent.domain == n.domain && jittered(parent.domain) && clash(step, parent.jitter) {
			bad = true
		}
		if prevDomain == n.domain && clash(step, prevStep) {
			bad = true
		}
		if !bad {
			break
		}
		step = (step + minJitterGap) % jitterSteps
	}
	return step
}

// jittered reports whether a domain takes part in the shade ladder.
func jittered(d Domain) bool {
	switch d {
	case DomainRoot, DomainBoundary, DomainBoundaryUnattributed:
		return false
	default:
		return true
	}
}

// layout assigns every frame its span as a percentage of the profile total,
// so the numbers in the markup are shares rather than pixels and the browser
// turns them into pixels against whatever the window happens to be.
func layout(n *node, x float64, scale float64) {
	n.x = x
	n.width = float64(n.value) * scale
	childX := x
	for _, c := range n.children {
		layout(c, childX, scale)
		childX += c.width
	}
}

// writeChart draws the graph: one positioning context, one absolutely
// positioned block per frame.
//
// Its height is the only pixel dimension in the markup, because rows have a
// pitch and the window has no opinion about it. Its width is not stated at
// all — a block is as wide as what contains it, which is the body, which is
// the window. That single omission is the difference between a graph that
// fills a 1990px screen and one that sits in the left 1280px of it.
//
// role="group" rather than role="img": img would make the whole subtree
// presentational and silence all twenty frames, which is exactly what
// role="img" is for on a frame and exactly wrong on their container. group
// takes an accessible name and leaves its children exposed.
func writeChart(ew *errWriter, root *node, maxDepth int, res *foldedstacks.Result) error {
	if root.value <= 0 {
		return fmt.Errorf("flamegraph: tree total is %d", root.value)
	}
	layout(root, 0, 100/float64(root.value))

	ew.f(`<div class="chart" style="height:%dpx" role="group" aria-label="Flame graph, %s" data-total="%d" data-unit="%s">`+"\n",
		maxDepth*frameHeight-1, html.EscapeString(res.SampleTypeName+"/"+res.Unit),
		root.value, html.EscapeString(res.Unit))
	writeNode(ew, root, res.Unit, root.value)
	ew.s("</div>\n")
	return ew.err
}

// writeNode emits one frame. Every attribute here is read by the script or
// drawn by the browser; nothing is emitted twice.
//
// left and width are percentages of the chart, inline because they are the
// one thing about a frame that is per-frame data. The depth is a class
// instead — rowCSS carries the pixel offset — because a profile has many
// more frames than depths.
//
// There is no data-name. The frame's text IS its name, in full: CSS does the
// truncating now, so the renderer no longer has to cut the label and no
// longer has anywhere to keep the uncut one. A data-name would be a second
// copy of a string that can be arbitrarily long, and the script reads
// textContent instead. (This is the same trade as reading x and width back
// off the element rather than duplicating them into data-orig-*.)
//
// The accessible name is an aria-label rather than a <title>, which is the
// one place where the cheaper option is also the better one. A <title> is
// both the accessible name and a native browser tooltip, so restoring it
// would put a second tooltip on a one-second delay behind the cursor tooltip
// on every hover. aria-label names the frame for a screen reader and draws
// nothing. role="img" goes with it so the frame is announced as one object:
// without it the text child is exposed separately and the reader hears the
// visually truncated label after the real name.
//
// The label is deliberately terse — name, value, share. Domain, module and
// the inference note stay in the visual tooltip; a screen reader user
// scanning twenty frames does not want three sentences on each.
func writeNode(ew *errWriter, n *node, unit string, total int64) {
	cls := fmt.Sprintf("frame d%d", n.depth)
	if n.inexact > 0 {
		cls += " inexact"
	}
	if n.jitter > 0 {
		cls += fmt.Sprintf(" j%d", n.jitter)
	}

	// No inline colour: data-domain below selects fill, hatch and outline
	// from paletteCSS, so a domain's colour is declared once for the whole
	// page rather than once per frame.
	ew.f(`<div class="%s" style="left:%s%%;width:%s%%" role="img" aria-label="%s" data-domain="%s" data-value="%d"`,
		cls, pct(n.x), pct(n.width),
		html.EscapeString(accessibleName(n, unit, total)),
		html.EscapeString(n.domain.Info().Key), n.value)
	if n.module != "" {
		ew.f(` data-module="%s"`, html.EscapeString(n.module))
	}
	if n.inexact > 0 {
		ew.f(` data-inexact="%d"`, n.inexact)
	}
	ew.s(">")
	// No whitespace around the name: the script reads this back as the
	// frame's identity, so the text node must be the symbol and nothing else.
	ew.esc(n.name)
	ew.s("</div>\n")

	for _, c := range n.children {
		writeNode(ew, c, unit, total)
	}
}

// pct formats a percentage in its shortest exact form, rounded to a
// thousandth of the graph's width — 0.02px on a 1990px window, which is
// finer than a pixel everywhere it matters. Shortest exact, because "100"
// and "12.5" are four and six bytes where %.3f would spend seven and eight,
// on two numbers per frame.
func pct(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}

// accessibleName is what a screen reader announces for a frame. It says the
// same three things the status bar says, in the same order, so hearing the
// page and seeing it agree.
func accessibleName(n *node, unit string, total int64) string {
	s := n.name + ", " + FormatValue(n.value, unit)
	if total > 0 {
		s += fmt.Sprintf(", %.2f%% of total", float64(n.value)/float64(total)*100)
	}
	return s
}

// FormatValue renders a value in its profile unit. Nanosecond profiles get
// human time; anything else gets a grouped integer plus the unit, so the
// page never invents a unit it was not given.
func FormatValue(v int64, unit string) string {
	switch unit {
	case "nanoseconds":
		return formatDuration(v)
	case "":
		return group(v)
	default:
		return group(v) + " " + unit
	}
}

func formatDuration(ns int64) string {
	switch {
	case ns == 0:
		return "0 ns"
	case ns < 1_000:
		return fmt.Sprintf("%d ns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.2f µs", float64(ns)/1e3)
	case ns < 1_000_000_000:
		return fmt.Sprintf("%.2f ms", float64(ns)/1e6)
	default:
		return fmt.Sprintf("%.3f s", float64(ns)/1e9)
	}
}

func group(v int64) string {
	s := fmt.Sprintf("%d", v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// --- the graph page -------------------------------------------------------

// writeTopBar is the whole of the page's permanent chrome above the graph:
// the profile's name, and two icon buttons whose keyboard equivalents are in
// the panel one of them opens. The info button grows a dot when the profile
// carries notes, so a page with caveats never looks like a page without.
func writeTopBar(ew *errWriter, opts Options, res *foldedstacks.Result, root *node) {
	ew.s("<div class=\"top\">\n<h1>")
	ew.esc(opts.Title)
	ew.s("</h1>\n")
	writeInfoPanel(ew, root, res, opts)
	ew.s("<button id=\"theme-btn\" type=\"button\" title=\"Light / dark (D)\" aria-label=\"Toggle light or dark\">&#9680;</button>\n")
	ew.s("</div>\n")
}

// writeSamplePeriodNote is the one note that stays on the page.
//
// Everything else the reader can recover: the unattributed share is the
// width of a labelled frame, the unsymbolized frames are hatched and named
// by their address. This one is invisible in the picture. A CPU call path
// under [gpu:launch] is a sample of launches, so its width is not the share
// of GPU time that path is responsible for — it is off by roughly the
// period, and nothing about the drawing says so. async-profiler has no
// equivalent because their samples carry no such trap.
func writeSamplePeriodNote(ew *errWriter, res *foldedstacks.Result) {
	period, ok := samplePeriod(res)
	if !ok {
		return
	}
	ew.f("<p class=\"note\">%s=%s &mdash; one launch in %s carried a CPU stack, so the width under [gpu:launch] is a sample of launches, not that path&rsquo;s share of GPU time. Nothing is scaled by %s.</p>\n",
		html.EscapeString(foldedstacks.SamplePeriodLabel), html.EscapeString(period),
		html.EscapeString(period), html.EscapeString(period))
}

func samplePeriod(res *foldedstacks.Result) (string, bool) {
	for _, l := range res.Labels {
		if l.Key == foldedstacks.SamplePeriodLabel && len(l.Top) > 0 {
			return l.Top[0].Value, true
		}
	}
	return "", false
}

// writeStatusBar emits the single permanent line of text on the page. Its
// resting content is the profile's own summary — the numbers the chip row
// used to hold — and the script replaces it with the hovered frame.
func writeStatusBar(ew *errWriter, res *foldedstacks.Result) {
	ew.s("<div id=\"status\">\n<span id=\"st\">")
	ew.esc(FormatValue(res.Total, res.Unit))
	ew.s(" total")
	ew.f(" &middot; %s samples", group(int64(res.Samples)))
	ew.f(" &middot; %s stacks", group(int64(len(res.Stacks))))
	ew.s(" &middot; ")
	ew.esc(res.SampleTypeName + "/" + res.Unit)
	ew.s("</span>\n")
	ew.s("<input id=\"q\" type=\"search\" hidden placeholder=\"search frames\" autocomplete=\"off\" spellcheck=\"false\" aria-label=\"Search frames\">\n")
	ew.s("<span id=\"mc\"></span>\n</div>\n")
}

// writeInfoPanel is everything that used to be permanently on screen.
//
// <details> rather than a div the script shows and hides: it renders
// collapsed with no script at all, so a page opened with JavaScript off — or
// saved and reopened somewhere that blocks it — can still reach the legend,
// which used to be permanent. It also gets the disclosure semantics for free
// (a summary is a button that announces expanded or collapsed), which a div
// with a hidden attribute never had.
func writeInfoPanel(ew *errWriter, root *node, res *foldedstacks.Result, opts Options) {
	cls := ""
	if len(otherWarnings(res)) > 0 {
		cls = " class=\"notes\""
	}
	ew.s("<details id=\"info\">\n")
	ew.f("<summary id=\"info-btn\"%s title=\"Legend, keys and notes (?)\" aria-label=\"Legend, keys and notes\">&#9432;</summary>\n", cls)
	ew.s("<div id=\"panel\">\n")
	ew.s("<button id=\"close\" type=\"button\" title=\"Close (Esc)\" aria-label=\"Close\">&#10005;</button>\n")

	ew.s("<h2>Keys</h2>\n<ul class=\"keys\">\n")
	for _, k := range [][2]string{
		{"Ctrl+F", "search frames"},
		{"0", "reset zoom"},
		{"Esc", "close this, then clear search, then reset zoom"},
		{"D", "light / dark"},
		{"? or I", "this panel"},
	} {
		ew.s("<li><kbd>")
		ew.esc(k[0])
		ew.s("</kbd> ")
		ew.esc(k[1])
		ew.s("</li>\n")
	}
	ew.s("<li>click a frame to zoom, click the background to reset</li>\n</ul>\n")

	writeLegend(ew, root)
	writeNotes(ew, res)
	writeLabels(ew, res)
	writeProvenance(ew, res, opts)
	ew.s("</div>\n</details>\n")
}

// writeLegend emits only the domains actually present in this graph, so the
// legend never advertises a layer the profile does not contain.
func writeLegend(ew *errWriter, root *node) {
	present := make([]bool, numDomains)
	var walk func(*node)
	walk = func(n *node) {
		if n.depth > 0 {
			present[n.domain] = true
		}
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(root)

	ew.s("<h2>Colour means domain</h2>\n")
	ew.s("<p class=\"muted\">Each colour is a layer of the CPU&rarr;GPU path, not a hash of the frame name. The domain is inferred &mdash; from the profile&rsquo;s mapping file where it has one, otherwise from the symbol name &mdash; and it affects nothing but the colour. Frames within a domain vary slightly in shade so a run of neighbours has visible edges; that variation carries no meaning.</p>\n<ul class=\"legend-list\">\n")
	for d := Domain(0); d < numDomains; d++ {
		if !present[d] {
			continue
		}
		info := d.Info()
		cls := "sw"
		if info.Overlay != "" {
			// The swatch is hatched exactly when the frames are, so the
			// legend row and the graph read as the same thing.
			cls += " hatched"
		}
		ew.f("<li><span class=\"%s\" style=\"background-color:%s\"", cls, info.Fill)
		if info.Stroke != "" {
			ew.f(";border-color:%s", info.Stroke)
		}
		ew.s("></span><b>")
		ew.esc(info.Label)
		ew.s("</b> ")
		ew.esc(info.Desc)
		ew.s("</li>\n")
	}
	if root.inexact > 0 {
		ew.s("<li><span class=\"sw hatch\"></span><b>diagonal hatching</b> Either the frame has no symbol, or its CPU attribution was inferred rather than measured. Hover the frame for which.</li>\n")
	} else {
		ew.s("<li><span class=\"sw hatch\"></span><b>diagonal hatching</b> The frame has no symbol, or no CPU stack stands behind it. Hover the frame for which.</li>\n")
	}
	ew.s("</ul>\n")
	if present[DomainGPUKernel] {
		ew.s("<p class=\"muted\">A <code>[gpu:kernel:&hellip;]</code> frame is one kernel and its duration.</p>\n")
	}
	// Say out loud that one colour of the palette is missing on purpose.
	// Otherwise its absence looks like an oversight, and its arrival later
	// looks like a new colour rather than a layer that finally has data.
	ew.s("<p class=\"muted\">The colours are Brendan Gregg&rsquo;s AI Flame Graph palette. One of them, <span class=\"sw\" style=\"background-color:var(--fill-accel-source)\"></span><b>aqua</b>, is reserved and unused: it belongs to source lines of functions running on the accelerator, which perf-agent cannot sample yet. No frame on this page is aqua.</p>\n")
}

// writeNotes lists everything the fold wants the reader to know except the
// launch-sampling note, which the page already carries in full view.
func writeNotes(ew *errWriter, res *foldedstacks.Result) {
	notes := otherWarnings(res)
	if len(notes) == 0 {
		return
	}
	ew.s("<h2>What this graph does not show</h2>\n<ul>\n")
	for _, warning := range notes {
		ew.s("<li>")
		ew.esc(warning)
		ew.s("</li>\n")
	}
	ew.s("</ul>\n")
}

// otherWarnings drops the launch-sampling warning, which writeSamplePeriodNote
// states on the page itself, so no note is made twice.
func otherWarnings(res *foldedstacks.Result) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w, foldedstacks.SamplePeriodLabel+"=") {
			continue
		}
		out = append(out, w)
	}
	return out
}

func writeLabels(ew *errWriter, res *foldedstacks.Result) {
	ew.s("<h2>Sample labels (not in the graph)</h2>\n")
	if len(res.Labels) == 0 {
		ew.s("<p class=\"muted\">This profile carries no per-sample labels.</p>\n")
		return
	}
	ew.s(`<p class="muted">Frames are stack identity, so §8 of the GPU profiling v2 design keeps these out of the tree: a per-sample PC or correlation ID in the path would give every sample its own leaf and destroy the aggregation a flame graph exists to show. They are reported here instead, so nothing in the profile is silently discarded.</p>
<table>
<thead><tr><th>key</th><th>samples</th><th>distinct values</th><th>top values by value</th></tr></thead>
<tbody>
`)
	for _, l := range res.Labels {
		ew.s("<tr><td><code>")
		ew.esc(l.Key)
		ew.s("</code></td><td>")
		ew.esc(group(int64(l.Count)))
		ew.s("</td><td>")
		ew.esc(group(int64(l.Distinct)))
		ew.s("</td><td>")
		if l.Count > 1 && l.Distinct >= l.Count {
			// One value per sample. Listing eight arbitrary correlation IDs
			// would look like a finding; the cardinality is the finding.
			ew.s("<span class=\"muted\">one distinct value per sample — nothing to aggregate, which is why this is a label and not a frame</span>")
		} else {
			for i, v := range l.Top {
				if i > 0 {
					ew.s(" ")
				}
				ew.s("<span class=\"lv\"><code>")
				ew.esc(v.Value)
				ew.s("</code> &rarr; ")
				ew.esc(FormatValue(v.Total, res.Unit))
				ew.s("</span>")
			}
			if l.Distinct > len(l.Top) {
				ew.f(" <span class=\"muted\">+%s more</span>", group(int64(l.Distinct-len(l.Top))))
			}
		}
		ew.s("</td></tr>\n")
	}
	ew.s("</tbody>\n</table>\n")
}

func writeProvenance(ew *errWriter, res *foldedstacks.Result, opts Options) {
	ew.s("<h2>Provenance</h2>\n<ul>\n")
	if opts.Subtitle != "" {
		ew.s("<li>")
		ew.esc(opts.Subtitle)
		ew.s("</li>\n")
	}
	ew.s("<li>Value axis: ")
	ew.esc(res.SampleTypeName + "/" + res.Unit)
	if len(res.SampleTypes) > 1 {
		ew.s(" (of ")
		ew.esc(strings.Join(res.SampleTypes, ", "))
		ew.s(")")
	}
	ew.f("; max depth %s", group(int64(res.MaxDepth)))
	if res.Frames > 0 {
		ew.f("; %s of %s frame slots unsymbolized", group(int64(res.AddressOnlyFrames)), group(int64(res.Frames)))
	}
	if res.InlinedFrames > 0 {
		ew.f("; %s inlined frames", group(int64(res.InlinedFrames)))
	}
	ew.s("</li>\n<li>Stack order read as ")
	ew.esc(res.StackOrder.String())
	ew.s(".</li>\n")
	for _, m := range opts.Meta {
		ew.s("<li>")
		ew.esc(m.Label + ": " + m.Value)
		ew.s("</li>\n")
	}
	ew.s("<li>Rendered by perf-agent. No external resources are loaded.</li>\n</ul>\n")
}

// --- the degenerate page --------------------------------------------------

// writeReport is the page a profile with nothing to draw gets. It is a
// written report rather than a graph with chrome, so it keeps the header,
// the chips and the full notice list: there is no picture to be quiet
// around, and every number on it is the evidence for why.
func writeReport(ew *errWriter, res *foldedstacks.Result, opts Options) {
	ew.s("<header>\n<h1>")
	ew.esc(opts.Title)
	ew.s("</h1>\n")
	if opts.Subtitle != "" {
		ew.s("<p class=\"sub\">")
		ew.esc(opts.Subtitle)
		ew.s("</p>\n")
	}
	ew.s("<div class=\"chips\">\n")
	chip(ew, "axis", res.SampleTypeName+"/"+res.Unit)
	chip(ew, "total", "nothing to draw")
	chip(ew, "samples", group(int64(res.Samples)))
	chip(ew, "distinct stacks", group(int64(len(res.Stacks))))
	chip(ew, "max depth", group(int64(res.MaxDepth)))
	if res.Frames > 0 {
		chip(ew, "unsymbolized frames", fmt.Sprintf("%s of %s", group(int64(res.AddressOnlyFrames)), group(int64(res.Frames))))
	}
	if res.InlinedFrames > 0 {
		chip(ew, "inlined frames", group(int64(res.InlinedFrames)))
	}
	chip(ew, "stack order read as", res.StackOrder.String())
	for _, m := range opts.Meta {
		chip(ew, m.Label, m.Value)
	}
	ew.s("</div>\n</header>\n")

	if len(res.Warnings) > 0 {
		ew.s("<section class=\"notices fatal\">\n<h2>What this graph does not show</h2>\n<ul>\n")
		for _, warning := range res.Warnings {
			ew.s("<li>")
			ew.esc(warning)
			ew.s("</li>\n")
		}
		ew.s("</ul>\n</section>\n")
	}

	ew.s("<section class=\"nodata\">\n<h2>No flame graph was drawn</h2>\n<p>")
	if res.Samples == 0 {
		ew.s("The profile contains zero samples. An empty flame graph would be indistinguishable from a flame graph of an idle process, so none is drawn.")
	} else {
		ew.f("The profile contains %s samples, but every one of them carries the value 0 on the ", group(int64(res.Samples)))
		ew.esc(res.SampleTypeName + "/" + res.Unit)
		ew.s(" axis, so there is nothing with a width to draw.")
	}
	ew.s("</p>\n<p class=\"muted\">This is the profile reporting on itself, not a rendering failure. Check that the profiler attached, that the target was running, and that the collection window overlapped it.</p>\n</section>\n")

	ew.s("<section class=\"labels\">\n")
	writeLabels(ew, res)
	ew.s("</section>\n")

	ew.s("<footer>Rendered by perf-agent. Value axis: ")
	ew.esc(res.SampleTypeName + "/" + res.Unit)
	if len(res.SampleTypes) > 1 {
		ew.s(" (of ")
		ew.esc(strings.Join(res.SampleTypes, ", "))
		ew.s(")")
	}
	ew.s(". Stack order read as ")
	ew.esc(res.StackOrder.String())
	ew.s(". No external resources are loaded.</footer>\n")
}

func chip(ew *errWriter, k, v string) {
	ew.s("<span class=\"chip\"><b>")
	ew.esc(k)
	ew.s("</b>")
	ew.esc(v)
	ew.s("</span>\n")
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) s(str string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, str)
}

func (e *errWriter) esc(str string) { e.s(html.EscapeString(str)) }

func (e *errWriter) f(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
