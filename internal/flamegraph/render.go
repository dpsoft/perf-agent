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
package flamegraph

import (
	"cmp"
	"fmt"
	"html"
	"io"
	"slices"
	"strings"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

const (
	defaultWidth = 1280
	frameHeight  = 18
	topPad       = 8
	bottomPad    = 8
	sidePad      = 8
	textPad      = 4
	minTextWidth = 24
	// charWidth approximates the advance of the 11px monospace label font.
	charWidth = 6.4
)

// Options configures a render.
type Options struct {
	// Title names the page. Defaults to "Flame Graph".
	Title string
	// Subtitle is a one-line provenance string (source file, mode, host).
	Subtitle string
	// Width is the SVG width in CSS pixels. Defaults to 1280.
	Width int
	// Meta are extra header chips, rendered after the built-in ones.
	Meta []MetaItem
}

// MetaItem is one header chip.
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

	children []*node
	index    map[string]*node

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
	ew.s("</style>\n</head>\n<body>\n<main>\n")

	writeHeader(ew, res, opts, degenerate)
	writeNotices(ew, res, degenerate)

	if degenerate {
		writeNoData(ew, res)
	} else {
		writeToolbar(ew)
		ew.s("<div class=\"chart\">\n")
		if err := writeSVG(ew, root, maxDepth, res, opts); err != nil {
			return err
		}
		ew.s("</div>\n")
		writeLegend(ew, root)
	}

	writeLabels(ew, res)
	writeFooter(ew, res)

	ew.s("</main>\n")
	if !degenerate {
		ew.s("<script>\n")
		ew.s(script)
		ew.s("</script>\n")
	}
	ew.s("</body>\n</html>\n")
	return ew.err
}

// RenderSVG writes just the graph, for embedding. It carries none of the
// interactivity, none of the legend and none of the warning text, so it is
// not a substitute for RenderHTML when the profile is degenerate: it returns
// an error rather than emitting a blank canvas.
func RenderSVG(w io.Writer, res *foldedstacks.Result, opts Options) error {
	opts = opts.withDefaults()
	if res == nil {
		return fmt.Errorf("flamegraph: nil result")
	}
	if res.Degenerate() {
		return fmt.Errorf("flamegraph: refusing to draw an SVG for a profile with no drawable samples (%d samples, total %d %s); use RenderHTML, which reports why",
			res.Samples, res.Total, res.Unit)
	}
	root, maxDepth := buildTree(res)
	ew := &errWriter{w: w}
	if err := writeSVG(ew, root, maxDepth, res, opts); err != nil {
		return err
	}
	return ew.err
}

func (o Options) withDefaults() Options {
	if o.Width <= 0 {
		o.Width = defaultWidth
	}
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

func layout(n *node, x float64, scale float64) {
	n.x = x
	n.width = float64(n.value) * scale
	childX := x
	for _, c := range n.children {
		layout(c, childX, scale)
		childX += c.width
	}
}

func writeSVG(ew *errWriter, root *node, maxDepth int, res *foldedstacks.Result, opts Options) error {
	if root.value <= 0 {
		return fmt.Errorf("flamegraph: tree total is %d", root.value)
	}
	plotWidth := float64(opts.Width - sidePad*2)
	layout(root, float64(sidePad), plotWidth/float64(root.value))

	height := topPad + bottomPad + maxDepth*frameHeight

	ew.f(`<svg class="flame" width="%d" height="%d" viewBox="0 0 %d %d" `, opts.Width, height, opts.Width, height)
	ew.f(`xmlns="http://www.w3.org/2000/svg" data-total="%d" data-inexact="%d" data-unit="%s" data-side-pad="%d" data-plot-width="%.2f" data-min-text="%d" data-text-pad="%d" data-char-width="%.2f">`+"\n",
		root.value, root.inexact, html.EscapeString(res.Unit), sidePad, plotWidth, minTextWidth, textPad, charWidth)
	ew.s(svgDefs)
	writeNode(ew, root, maxDepth, res, "")
	ew.s("</svg>\n")
	return ew.err
}

func writeNode(ew *errWriter, n *node, maxDepth int, res *foldedstacks.Result, parentPath string) {
	y := float64(topPad + (maxDepth-n.depth-1)*frameHeight)
	path := n.name
	if parentPath != "" {
		path = parentPath + " › " + n.name
	}
	info := n.domain.Info()

	cls := "frame"
	if n.inexact > 0 {
		cls += " inexact"
	}

	ew.f(`<g class="%s" data-name="%s" data-path="%s" data-domain="%s" data-domain-label="%s" data-value="%d" data-inexact="%d" data-orig-x="%.3f" data-orig-width="%.3f" data-depth="%d">`,
		cls, html.EscapeString(n.name), html.EscapeString(path),
		html.EscapeString(info.Key), html.EscapeString(info.Label),
		n.value, n.inexact, n.x, n.width, n.depth)
	ew.s("<title>")
	ew.esc(tooltip(n, res))
	ew.s("</title>")

	stroke := ""
	if info.Stroke != "" {
		stroke = fmt.Sprintf(` stroke="%s" stroke-width="0.8"`, info.Stroke)
	}
	ew.f(`<rect class="bg" x="%.3f" y="%.1f" width="%.3f" height="%d" rx="2" fill="%s"%s/>`,
		n.x, y, n.width, frameHeight-1, info.Fill, stroke)
	if info.Overlay != "" {
		ew.f(`<rect class="ov" x="%.3f" y="%.1f" width="%.3f" height="%d" rx="2" fill="%s"/>`,
			n.x, y, n.width, frameHeight-1, info.Overlay)
	}
	if n.inexact > 0 {
		ew.f(`<rect class="ov" x="%.3f" y="%.1f" width="%.3f" height="%d" rx="2" fill="url(#p-inferred)"/>`,
			n.x, y, n.width, frameHeight-1)
	}
	if n.width >= minTextWidth {
		maxChars := int((n.width - textPad*2) / charWidth)
		if label := truncate(n.name, maxChars); label != "" {
			ew.f(`<text x="%.3f" y="%.1f">`, n.x+textPad, y+float64(frameHeight)-5.5)
			ew.esc(label)
			ew.s("</text>")
		}
	}
	ew.s("</g>\n")

	for _, c := range n.children {
		writeNode(ew, c, maxDepth, res, path)
	}
}

func tooltip(n *node, res *foldedstacks.Result) string {
	var b strings.Builder
	b.WriteString(n.name)
	b.WriteString("\n")
	b.WriteString(FormatValue(n.value, res.Unit))
	if res.Total > 0 {
		fmt.Fprintf(&b, " (%.2f%% of total)", float64(n.value)/float64(res.Total)*100)
	}
	if n.depth > 0 {
		b.WriteString("\ndomain: " + n.domain.Info().Label)
	}
	if n.module != "" {
		b.WriteString("\nmodule: " + n.module)
	}
	if n.inexact > 0 {
		fmt.Fprintf(&b, "\n%s of this is attributed by inference, not measurement", FormatValue(n.inexact, res.Unit))
	}
	if n.domain == DomainUnsymbolized {
		b.WriteString("\nno symbol: the unwind found this frame, nothing could name it")
	}
	return b.String()
}

func truncate(label string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	r := []rune(label)
	if len(r) <= maxChars {
		return label
	}
	if maxChars <= 2 {
		return string(r[:maxChars])
	}
	return string(r[:maxChars-1]) + "…"
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

func writeHeader(ew *errWriter, res *foldedstacks.Result, opts Options, degenerate bool) {
	ew.s("<header class=\"hdr\">\n<h1>")
	ew.esc(opts.Title)
	ew.s("</h1>\n")
	if opts.Subtitle != "" {
		ew.s("<p class=\"sub\">")
		ew.esc(opts.Subtitle)
		ew.s("</p>\n")
	}
	ew.s("<div class=\"chips\">\n")
	chip(ew, "axis", res.SampleTypeName+"/"+res.Unit)
	if degenerate {
		chip(ew, "total", "nothing to draw")
	} else {
		chip(ew, "total", FormatValue(res.Total, res.Unit))
	}
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
}

func chip(ew *errWriter, k, v string) {
	ew.s("<span class=\"chip\"><b>")
	ew.esc(k)
	ew.s("</b>")
	ew.esc(v)
	ew.s("</span>\n")
}

func writeNotices(ew *errWriter, res *foldedstacks.Result, degenerate bool) {
	if len(res.Warnings) == 0 {
		return
	}
	cls := "notices"
	if degenerate {
		cls += " fatal"
	}
	ew.f("<section class=\"%s\">\n<h2>What this graph does not show</h2>\n<ul>\n", cls)
	for _, warning := range res.Warnings {
		ew.s("<li>")
		ew.esc(warning)
		ew.s("</li>\n")
	}
	ew.s("</ul>\n</section>\n")
}

func writeNoData(ew *errWriter, res *foldedstacks.Result) {
	ew.s("<section class=\"nodata\">\n<h2>No flame graph was drawn</h2>\n<p>")
	if res.Samples == 0 {
		ew.s("The profile contains zero samples. An empty flame graph would be indistinguishable from a flame graph of an idle process, so none is drawn.")
	} else {
		ew.f("The profile contains %s samples, but every one of them carries the value 0 on the ", group(int64(res.Samples)))
		ew.esc(res.SampleTypeName + "/" + res.Unit)
		ew.s(" axis, so there is nothing with a width to draw.")
	}
	ew.s("</p>\n<p class=\"muted\">This is the profile reporting on itself, not a rendering failure. Check that the profiler attached, that the target was running, and that the collection window overlapped it.</p>\n</section>\n")
}

func writeToolbar(ew *errWriter) { ew.s(toolbarHTML) }

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

	ew.s("<section class=\"legend\">\n<h2>Colour means domain</h2>\n")
	ew.s("<p class=\"muted\">Each colour is a layer of the CPU→GPU path, not a hash of the frame name. The domain is inferred — from the profile's mapping file where it has one, otherwise from the symbol name — and it affects nothing but the colour: every frame's width and position comes from its measured value.</p>\n<ul class=\"legend-list\">\n")
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
		ew.s("<p class=\"muted depth-note\">A <code>[gpu:kernel:…]</code> frame is one kernel and its duration.</p>\n")
	}
	ew.s("</section>\n")
}

func writeLabels(ew *errWriter, res *foldedstacks.Result) {
	ew.s("<section class=\"labels\">\n<h2>Sample labels (not in the graph)</h2>\n")
	if len(res.Labels) == 0 {
		ew.s("<p class=\"muted\">This profile carries no per-sample labels.</p>\n</section>\n")
		return
	}
	ew.s(`<p class="muted">Frames are stack identity, so §8 of the GPU profiling v2 design keeps these out of the tree: folding a per-sample PC or correlation ID into the path would give every sample its own leaf and destroy the aggregation a flame graph exists to show. They are reported here instead, so nothing in the profile is silently discarded.</p>
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
	ew.s("</tbody>\n</table>\n</section>\n")
}

func writeFooter(ew *errWriter, res *foldedstacks.Result) {
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
