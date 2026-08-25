package flamegraph

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

func result(stacks ...foldedstacks.Stack) *foldedstacks.Result {
	res := &foldedstacks.Result{
		Stacks:         stacks,
		SampleTypeName: "gpu",
		Unit:           "nanoseconds",
		StackOrder:     foldedstacks.RootFirst,
	}
	for _, s := range stacks {
		res.Total += s.Value
		res.InexactTotal += s.Inexact
		res.Samples++
		res.Frames += len(s.Frames)
		if len(s.Frames) > res.MaxDepth {
			res.MaxDepth = len(s.Frames)
		}
	}
	return res
}

func renderString(t *testing.T, res *foldedstacks.Result, opts Options) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, RenderHTML(&b, res, opts))
	return b.String()
}

func TestRenderHTMLProducesAStandalonePage(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main", "work"}, Value: 3000},
		foldedstacks.Stack{Frames: []string{"main", "idle"}, Value: 1000},
	), Options{Title: "GPU flame graph"})

	for _, want := range []string{
		"<!DOCTYPE html>", "<html lang=\"en\">", "<title>GPU flame graph</title>",
		"<div class=\"chart\"", ">main</div>", ">work</div>",
		`id="status"`, `id="tip"`, `id="info"`, `id="q"`, `id="info-btn"`, `id="theme-btn"`,
		"function zoom(target)", "function applySearch(query)",
	} {
		assert.Contains(t, got, want)
	}
}

// A "self-contained" page that quietly loads a CDN font is broken the moment
// it is opened on a machine with no network, which is the machine profiling
// usually happens on.
func TestRenderHTMLFetchesNothing(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main"}, Value: 10},
	), Options{})

	for _, forbidden := range []string{
		"<script src", "<link ", "@import", "url(http", "url('http", "url(\"http",
		"fonts.googleapis.com", "fonts.gstatic.com", "cdn.", "fetch(", "XMLHttpRequest",
		"<img", "<iframe",
	} {
		assert.NotContains(t, got, forbidden, "the page must load nothing from the network")
	}
	// Not one URL of any kind is left in the document. There used to be
	// exactly one — the SVG XML namespace, an identifier rather than a fetch
	// — and the geometry that needed it is gone.
	assert.Empty(t, regexp.MustCompile(`https?://[^\s"'<>)]+`).FindAllString(got, -1))
}

// C++ templates and Go generics guarantee <, > and & in symbol names. A
// mangled name must not be able to close a tag or open a script.
func TestRenderHTMLEscapesHostileSymbolNames(t *testing.T) {
	hostile := `std::vector<Foo&Bar>::push_back("</title><script>alert(1)</script>")`
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{hostile, "</g></svg><script>evil()</script>"}, Value: 100},
	), Options{Title: `</title><script>alert("title")</script>`})

	assert.NotContains(t, got, "<script>alert(1)</script>")
	assert.NotContains(t, got, "<script>evil()</script>")
	assert.NotContains(t, got, `<script>alert("title")</script>`)
	assert.Contains(t, got, "&lt;/title&gt;&lt;script&gt;")
	assert.Contains(t, got, "std::vector&lt;Foo&amp;Bar&gt;")

	// Exactly the two <script> elements this renderer emits: none injected.
	assert.Equal(t, 1, strings.Count(got, "<script>"))
	assert.Equal(t, 1, strings.Count(got, "</script>"))
	assert.Equal(t, 1, strings.Count(got, "<style>"))
}

func TestRenderHTMLKeepsProfileTextOutOfScriptAndStyle(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"SENTINEL_SYMBOL"}, Value: 5},
	), Options{Title: "SENTINEL_TITLE"})

	scriptBody := between(t, got, "<script>", "</script>")
	styleBody := between(t, got, "<style>", "</style>")
	for _, blob := range []string{scriptBody, styleBody} {
		assert.NotContains(t, blob, "SENTINEL_SYMBOL")
		assert.NotContains(t, blob, "SENTINEL_TITLE")
	}
}

func between(t *testing.T, s, openTag, closeTag string) string {
	t.Helper()
	i := strings.Index(s, openTag)
	require.GreaterOrEqual(t, i, 0)
	j := strings.Index(s[i:], closeTag)
	require.GreaterOrEqual(t, j, 0)
	return s[i : i+j]
}

// The seven defects this project has found all share a shape: the output
// looks healthy exactly when it is worst. A flame graph of nothing must
// therefore be a page that says "nothing", not an empty rectangle.
func TestRenderHTMLOnAnEmptyProfileSaysSoInsteadOfDrawingNothing(t *testing.T) {
	empty := &foldedstacks.Result{
		SampleTypeName: "cpu", Unit: "nanoseconds",
		Warnings: []string{"This profile contains no samples at all."},
	}
	got := renderString(t, empty, Options{Title: "empty"})

	assert.NotContains(t, got, "<div class=\"chart\"", "no graph may be drawn")
	assert.NotContains(t, got, "<script>", "no interactivity without a graph")
	assert.Contains(t, got, "No flame graph was drawn")
	assert.Contains(t, got, "zero samples")
	assert.Contains(t, got, "This profile contains no samples at all.")
	assert.Contains(t, got, "nothing to draw")
}

func TestRenderHTMLOnAnAllZeroProfileSaysSo(t *testing.T) {
	res := &foldedstacks.Result{SampleTypeName: "gpu", Unit: "nanoseconds", Samples: 40}
	got := renderString(t, res, Options{})

	assert.NotContains(t, got, "<div class=\"chart\"")
	assert.Contains(t, got, "No flame graph was drawn")
	assert.Contains(t, got, "every one of them carries the value 0")
	assert.Contains(t, got, "gpu/nanoseconds")
}

func TestRenderFragmentRefusesADegenerateProfile(t *testing.T) {
	err := RenderFragment(&strings.Builder{}, &foldedstacks.Result{SampleTypeName: "cpu", Unit: "nanoseconds"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drawable samples")
}

func TestRenderHTMLNamesTheValueAxisRatherThanCallingEverythingSamples(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"k"}, Value: 5987854},
	), Options{})

	assert.Contains(t, got, "gpu/nanoseconds")
	assert.Contains(t, got, "5.99 ms", "a nanosecond total must read as time")
	assert.NotContains(t, got, "5,987,854 samples")
	assert.Contains(t, got, `data-unit="nanoseconds"`)
}

func TestFormatValueHonoursTheProfilesUnit(t *testing.T) {
	assert.Equal(t, "512 ns", FormatValue(512, "nanoseconds"))
	assert.Equal(t, "1.54 µs", FormatValue(1537, "nanoseconds"))
	assert.Equal(t, "5.99 ms", FormatValue(5987854, "nanoseconds"))
	assert.Equal(t, "1.500 s", FormatValue(1500000000, "nanoseconds"))
	assert.Equal(t, "4,096 bytes", FormatValue(4096, "bytes"))
	assert.Equal(t, "17 count", FormatValue(17, "count"))
	assert.Equal(t, "1,234", FormatValue(1234, ""))
}

func TestRenderHTMLConservesTheTotalOnTheSyntheticRoot(t *testing.T) {
	// The root's width is the whole profile. If it disagreed with the fold
	// total, every percentage on the page would be wrong.
	res := result(
		foldedstacks.Stack{Frames: []string{"a", "b"}, Value: 2695822},
		foldedstacks.Stack{Frames: []string{"a", "c"}, Value: 2894901},
		foldedstacks.Stack{Frames: []string{"d"}, Value: 397131},
	)
	got := renderString(t, res, Options{})
	assert.Equal(t, int64(5987854), res.Total)
	assert.Contains(t, got, `data-total="5987854"`)
	assert.Contains(t, got, `data-domain="root" data-value="5987854"`)
	// The root spans the chart exactly, and the chart spans the window.
	assert.Contains(t, got, `class="frame d0" style="left:0%;width:100%"`)
}

func TestRenderHTMLColoursByDomainNotByNameHash(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{
			"_start", "main", "cudaLaunchKernel", "0xdeadbeef",
			"[gpu:launch]", "[gpu:kernel:matmul]",
		}, Value: 1000},
		foldedstacks.Stack{Frames: []string{"[gpu:launch unsampled]", "[gpu:kernel:matmul]"}, Value: 500},
	), Options{})

	for _, want := range []string{
		`data-domain="system"`, `data-domain="app"`, `data-domain="vendor"`,
		`data-domain="unsym"`, `data-domain="boundary"`,
		`data-domain="boundary-unattributed"`, `data-domain="gpu-kernel"`,
	} {
		assert.Contains(t, got, want)
	}
	// Same fill for every frame of a domain: colour is information, not
	// decoration.
	assert.Contains(t, got, DomainGPUKernel.Info().Fill)
	assert.Contains(t, got, "var(--hatch-gap)", "unsymbolized frames must be hatched")
}

func TestRenderHTMLLegendOnlyNamesDomainsThatAreActuallyPresent(t *testing.T) {
	cpuOnly := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"_start", "main"}, Value: 10},
	), Options{})
	assert.Contains(t, cpuOnly, DomainApplication.Info().Label)
	assert.NotContains(t, cpuOnly, DomainGPUKernel.Info().Label,
		"a CPU-only profile must not advertise a GPU layer it does not have")
	assert.NotContains(t, cpuOnly, DomainBoundary.Info().Label)

	withGPU := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, Value: 10},
	), Options{})
	assert.Contains(t, withGPU, DomainGPUKernel.Info().Label)
	assert.Contains(t, withGPU, "one kernel and its duration")
}

func TestRenderHTMLMarksInferredAttributionOnTheFramesItAffects(t *testing.T) {
	res := result(
		foldedstacks.Stack{Frames: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, Value: 100, Inexact: 100},
		foldedstacks.Stack{Frames: []string{"other"}, Value: 50},
	)
	got := renderString(t, res, Options{})

	assert.Regexp(t, regexp.MustCompile(`class="frame d\d+ inexact( j\d)?"[^>]*>\[gpu:kernel:k\]</div>`), got)
	assert.Contains(t, paletteCSS, ".frame.inexact{background-image:var(--hatch-inf)}")
	assert.Contains(t, got, `data-inexact="100"`)
	assert.Contains(t, got, "attributed by inference, not measurement")
	// The frame that was measured must not be marked.
	assert.Regexp(t, regexp.MustCompile(`class="frame d\d+( j\d)?"[^>]*>other</div>`), got)
}

func TestRenderHTMLReportsLabelsWithoutFoldingThemIntoFrames(t *testing.T) {
	res := result(foldedstacks.Stack{Frames: []string{"[gpu:kernel:k]"}, Value: 30})
	res.Labels = []foldedstacks.LabelSummary{
		{
			Key: "gpu_join", Distinct: 2, Total: 30, Count: 4000,
			Top: []foldedstacks.LabelValue{
				{Value: "exact", Total: 25, Count: 3000},
				{Value: "heuristic", Total: 5, Count: 1000},
			},
		},
		{
			Key: "gpu_queue", Distinct: 12, Total: 30, Count: 4000,
			Top: []foldedstacks.LabelValue{{Value: "q0", Total: 20, Count: 2000}},
		},
	}
	got := renderString(t, res, Options{})

	assert.Contains(t, got, "Sample labels (not in the graph)")
	assert.Contains(t, got, "gpu_join")
	assert.Contains(t, got, "heuristic")
	assert.Contains(t, got, "+11 more")
	chart := chartOf(t, got)
	assert.NotContains(t, chart, "gpu_join", "a label must never become a frame")
	assert.NotContains(t, chart, "gpu_queue")
}

func TestRenderHTMLDoesNotDressUpAHighCardinalityLabelAsAFinding(t *testing.T) {
	// gpu_correlation is one value per sample. Listing eight arbitrary IDs
	// would read as a top-N; the cardinality is the whole story, and it is
	// also exactly why spec §8 keeps this out of the frames.
	res := result(foldedstacks.Stack{Frames: []string{"[gpu:kernel:k]"}, Value: 30})
	res.Labels = []foldedstacks.LabelSummary{{
		Key: "gpu_correlation", Distinct: 4000, Total: 30, Count: 4000,
		Top: []foldedstacks.LabelValue{{Value: "cupti:4294967301", Total: 1, Count: 1}},
	}}
	got := renderString(t, res, Options{})

	assert.Contains(t, got, "one distinct value per sample")
	assert.NotContains(t, got, "cupti:4294967301")
}

func TestRenderHTMLStatesTheStackOrderItAssumed(t *testing.T) {
	// Reading the order wrong draws a plausible graph upside down, so the
	// page always says which way it read the profile.
	got := renderString(t, result(foldedstacks.Stack{Frames: []string{"a"}, Value: 1}), Options{})
	assert.Contains(t, strings.ToLower(got), "stack order read as")
	assert.Contains(t, got, "root-first")
}

func TestRenderHTMLIsDeterministic(t *testing.T) {
	build := func() *foldedstacks.Result {
		return result(
			foldedstacks.Stack{Frames: []string{"z", "y"}, Value: 3},
			foldedstacks.Stack{Frames: []string{"a", "b"}, Value: 5},
			foldedstacks.Stack{Frames: []string{"a", "c"}, Value: 2},
		)
	}
	first := renderString(t, build(), Options{Title: "t"})
	for range 4 {
		assert.Equal(t, first, renderString(t, build(), Options{Title: "t"}))
	}
}

func TestRenderHTMLRejectsANilResult(t *testing.T) {
	require.Error(t, RenderHTML(&strings.Builder{}, nil, Options{}))
	require.Error(t, RenderFragment(&strings.Builder{}, nil))
}

// The chrome discipline, asserted rather than described: what a reader sees
// without pressing anything is the graph, one status line, and the note that
// stops them misreading it. Everything else is behind the info button.
func TestRenderHTMLKeepsTheChromeOffThePage(t *testing.T) {
	res := result(foldedstacks.Stack{Frames: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, Value: 100})
	res.Labels = []foldedstacks.LabelSummary{{
		Key: foldedstacks.SamplePeriodLabel, Distinct: 1, Total: 100, Count: 12,
		Top: []foldedstacks.LabelValue{{Value: "8", Total: 100, Count: 12}},
	}}
	res.Warnings = []string{
		"7 of 20 frame slots (35.0%) have no symbol and are drawn as a raw address or [unknown].",
		"12 samples carry gpu_sample_period=8: one launch in 8 contributed a CPU stack.",
	}
	got := renderString(t, res, Options{Title: "t"})

	body := got[strings.Index(got, "<body>"):]
	panelAt := strings.Index(body, `<details id="info"`)
	require.Positive(t, panelAt, "the info panel must exist")
	panelEnd := strings.Index(body, "</details>")
	require.Greater(t, panelEnd, panelAt)
	panel := body[panelAt:panelEnd]
	// Everything outside the <details> is what a reader sees without
	// pressing anything; everything inside it is one keypress away.
	visible := body[:panelAt] + body[panelEnd:]

	// The launch-sampling note is on the page, once, in one line.
	assert.Contains(t, visible, `<p class="note">`)
	assert.Equal(t, 1, strings.Count(got, `<p class="note">`))
	assert.Contains(t, visible, "gpu_sample_period=8")
	assert.Contains(t, visible, "not that path&rsquo;s share of GPU time")
	assert.NotContains(t, got, "12 samples carry gpu_sample_period=8",
		"the long form of the note must not be repeated in the panel")

	// Everything that used to be permanently on screen is not.
	for _, hidden := range []string{
		"Colour means domain", "What this graph does not show",
		"Sample labels (not in the graph)", "Provenance",
		"have no symbol and are drawn as a raw address",
	} {
		assert.NotContains(t, visible, hidden, "%q must live in the info panel, not on the page", hidden)
		assert.Contains(t, panel, hidden)
	}

	// And the toolbar it replaced is gone entirely.
	for _, gone := range []string{"Reset zoom", "Clear", `id="breadcrumbs"`, `class="toolbar"`, `class="chip"`} {
		assert.NotContains(t, got, gone)
	}

	// A profile with notes says so on the button that hides them.
	assert.Contains(t, got, `<summary id="info-btn" class="notes"`)
}

func TestRenderHTMLDoesNotFlagNotesOnAProfileThatHasNone(t *testing.T) {
	got := renderString(t, result(foldedstacks.Stack{Frames: []string{"main"}, Value: 1}), Options{})
	assert.Contains(t, got, `<summary id="info-btn" title=`)
	assert.NotContains(t, got, `id="info-btn" class="notes"`)
	assert.NotContains(t, got, `<p class="note">`, "no launch sampling, no note")
}

// The seven consecutive unsymbolized vendor frames of the RTX 3090 profile
// were one flat block of sand. Every frame that shares a domain with the
// frame below it, or with the frame beside it, must now differ from it by
// enough lightness to show an edge.
func TestJitterSeparatesEveryTouchingPairThatSharesADomain(t *testing.T) {
	res := result(foldedstacks.Stack{Frames: []string{
		"_start", "__libc_start_call_main", "main",
		"0xdeadbeef", "0xcafebabe", "0xfeedface", "0xdecafbad", "0xbaddcafe",
		"[gpu:launch]", "[gpu:kernel:k]",
	}, Value: 100})
	root, _ := buildTree(res)

	var walk func(n *node)
	walk = func(n *node) {
		prev := (*node)(nil)
		for _, c := range n.children {
			if c.domain == n.domain && jittered(c.domain) {
				assert.GreaterOrEqual(t, abs(c.jitter-n.jitter), minJitterGap,
					"%q sits on %q and shares its domain, but only %d ladder steps apart",
					c.name, n.name, abs(c.jitter-n.jitter))
			}
			if prev != nil && prev.domain == c.domain && jittered(c.domain) {
				assert.GreaterOrEqual(t, abs(c.jitter-prev.jitter), minJitterGap,
					"%q sits beside %q and shares its domain, but only %d ladder steps apart",
					c.name, prev.name, abs(c.jitter-prev.jitter))
			}
			prev = c
			walk(c)
		}
	}
	walk(root)

	// The domains whose lightness carries meaning stay off the ladder.
	var check func(n *node)
	check = func(n *node) {
		if !jittered(n.domain) {
			assert.Zero(t, n.jitter, "%q must keep its declared lightness", n.name)
		}
		assert.Less(t, n.jitter, jitterSteps)
		for _, c := range n.children {
			check(c)
		}
	}
	check(root)

	page := renderString(t, res, Options{})
	assert.Contains(t, page, ">[gpu:launch]</div>")
	assert.NotRegexp(t, regexp.MustCompile(`class="frame d\d+ j\d"[^>]*>\[gpu:launch\]</div>`), page)
	assert.NotRegexp(t, regexp.MustCompile(`class="frame d0 j\d"[^>]*>all</div>`), page)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// A page with no graph has nothing to hover, search or zoom, so it carries
// none of the machinery for it.
func TestRenderHTMLOnADegenerateProfileCarriesNoChrome(t *testing.T) {
	got := renderString(t, &foldedstacks.Result{SampleTypeName: "cpu", Unit: "nanoseconds"}, Options{})
	for _, gone := range []string{`id="status"`, `id="tip"`, `id="info"`, `id="info-btn"`, "<script>", "color-mix", "hsl(from", `class="chart"`} {
		assert.NotContains(t, got, gone)
	}
	assert.Contains(t, got, "No flame graph was drawn")
}

// A frame that a screen reader cannot name is a frame that does not exist to
// it. The name is an aria-label rather than a <title> so it draws nothing:
// a <title> is also a native browser tooltip, and two tooltips on one hover
// is worse than one.
func TestEveryFrameCarriesAnAccessibleName(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main", "work"}, Value: 2695822},
		foldedstacks.Stack{Frames: []string{"main", "idle"}, Value: 3292032},
	), Options{})

	groups := regexp.MustCompile(`<div class="frame[^"]*"([^>]*)>`).FindAllStringSubmatch(got, -1)
	require.Len(t, groups, 4, "root, main, idle, work")
	for _, g := range groups {
		assert.Contains(t, g[1], `role="img"`,
			"without role=img the truncated <text> child is announced after the real name")
		assert.Regexp(t, regexp.MustCompile(`aria-label="[^"]+"`), g[1])
	}

	// The name says the same three things the status bar says, in the same
	// order, so hearing the page and seeing it agree.
	assert.Contains(t, got, `aria-label="all, 5.99 ms, 100.00% of total"`)
	assert.Contains(t, got, `aria-label="work, 2.70 ms, 45.02% of total"`)
	assert.Contains(t, got, `aria-label="idle, 3.29 ms, 54.98% of total"`)

	// The chart names itself, but as a group: role="img" there would make
	// the whole subtree presentational and silence all four frames.
	assert.Contains(t, got, `class="chart" style="height:53px" role="group" aria-label="Flame graph, gpu/nanoseconds"`)

	// Exactly one <title> in the document — the one in <head>. A per-frame
	// <title> would be an accessible name AND a native browser tooltip, and
	// would race the cursor tooltip on every hover.
	assert.Equal(t, 1, strings.Count(got, "<title>"))
	assert.Contains(t, got, "<title>Flame Graph</title>")
}

func TestAccessibleNamesEscapeHostileSymbols(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{`Foo<Bar&"Baz">::run()`}, Value: 10},
	), Options{})
	assert.Contains(t, got, `aria-label="Foo&lt;Bar&amp;&#34;Baz&#34;&gt;::run(), 10 ns, 100.00% of total"`)
	assert.NotContains(t, got, `aria-label="Foo<Bar`)
}

// The legend used to be permanently on the page, so moving it behind a
// control must not put it behind JavaScript as well.
func TestTheInfoPanelOpensWithoutScript(t *testing.T) {
	got := renderString(t, result(foldedstacks.Stack{Frames: []string{"main"}, Value: 1}), Options{})

	assert.Contains(t, got, `<details id="info">`)
	assert.Regexp(t, regexp.MustCompile(`<summary id="info-btn"[^>]*>`), got)
	assert.NotContains(t, got, `<details id="info" open`, "collapsed at rest, or it is permanent chrome again")
	assert.NotContains(t, got, `id="panel" hidden`, "the panel is hidden by <details>, not by an attribute a script must clear")

	// The script may drive the disclosure, but it must not be what creates
	// it: no JS path sets the panel's own visibility.
	assert.NotContains(t, script, "panel.hidden")
	assert.Contains(t, script, "info.open")
}

// chartOf is the graph and nothing else: from the chart element to the
// status bar that follows it.
func chartOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<div class="chart"`)
	require.GreaterOrEqual(t, i, 0)
	j := strings.Index(page, `<div id="status">`)
	require.Greater(t, j, i)
	return page[i:j]
}

// The defect this geometry replaced: a viewBox 1280 units wide, so a 1990px
// window got a graph in its left two-thirds and 700px of nothing. Every
// horizontal number in the markup is now a percentage, which is a number the
// browser resolves against the window — at load, and again on every resize,
// with no script involved.
func TestFrameGeometryIsPercentagesSoTheGraphFillsAnyWindow(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main", "work"}, Value: 3000},
		foldedstacks.Stack{Frames: []string{"main", "idle"}, Value: 1000},
	), Options{})

	styles := regexp.MustCompile(`<div class="frame[^"]*" style="([^"]*)"`).FindAllStringSubmatch(got, -1)
	require.Len(t, styles, 4)
	geom := regexp.MustCompile(`^left:-?[\d.]+%;width:[\d.]+%$`)
	for _, m := range styles {
		assert.Regexp(t, geom, m[1], "a frame's span must be stated in %%, never in px")
	}

	// Nothing anywhere fixes the graph's width. The only px in the chart
	// element is its height, which is rows times pitch and has nothing to do
	// with the window.
	chart := chartOf(t, got)
	assert.NotContains(t, chart, "px;width")
	assert.NotContains(t, chart, "width:1280")
	assert.NotContains(t, got, "viewBox")
	assert.NotContains(t, got, "preserveAspectRatio")
	assert.Regexp(t, regexp.MustCompile(`<div class="chart" style="height:\d+px"`), got)
}

// "Fill the width" must not mean "scale the drawing". A viewBox stretched to
// 100% would have grown 13px text to 20px on a 1990px window and made the
// rows half again as tall; that is zoom, not fill. So every dimension that
// is not a share of the total is an absolute px in a stylesheet, identical
// at every window width.
func TestTypographyAndRowPitchAreFixedPixelsAtEveryWindowWidth(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"a", "b", "c"}, Value: 10},
	), Options{})

	assert.Contains(t, styleSheet, "font-size:11px")
	assert.Contains(t, styleSheet, "height:17px")
	assert.Contains(t, styleSheet, "line-height:17px")
	// One rule per depth, offset from the floor: a depth is the same number
	// of pixels up whatever the profile and whatever the window.
	for d, top := range map[int]int{0: 0, 1: frameHeight, 3: 3 * frameHeight} {
		assert.Contains(t, got, fmt.Sprintf(".d%d{bottom:%dpx}", d, top))
	}
	assert.Contains(t, got, `class="frame d3 `, "the leaf sits three rows up")
	assert.NotContains(t, got, ".d4{", "and there is no fourth row to sit on")
}

// The whole page must lay itself out with the script deleted — the branch
// made the info panel a native <details> for this reason, and a graph that
// needed a resize handler to be the right width would have given that back.
func TestTheGraphFillsTheWindowWithoutTheScript(t *testing.T) {
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main", "work"}, Value: 3000},
		foldedstacks.Stack{Frames: []string{"main", "idle"}, Value: 1000},
	), Options{})

	// No resize listener, and no layout pass to hang off one.
	assert.NotContains(t, script, "resize")
	assert.NotContains(t, script, "getBoundingClientRect().width")
	assert.NotContains(t, script, "clientWidth")
	assert.NotContains(t, script, "offsetWidth")

	// The geometry is in the markup, not computed by the script: strip the
	// script and every frame still has its span.
	body := got[strings.Index(got, "<body>"):strings.Index(got, "<script>")]
	assert.Equal(t, 4, strings.Count(body, `<div class="frame`))
	assert.Contains(t, body, `style="left:0%;width:100%"`)
	assert.Contains(t, body, `style="left:25%;width:75%"`)
}

// Truncation moved from Go to CSS, because the renderer no longer knows how
// many pixels a frame is: it knows a percentage, and only the browser knows
// what that is worth. So the markup carries the whole symbol and the
// stylesheet cuts it — at the reader's real width, in the reader's real
// font, without ever splitting a rune.
func TestLabelsAreTruncatedByCSSAndTheMarkupKeepsTheWholeSymbol(t *testing.T) {
	long := "std::__1::__function::__func<VeryLongTemplateArgument>::operator()"
	got := renderString(t, result(
		foldedstacks.Stack{Frames: []string{"main", long}, Value: 1000},
		foldedstacks.Stack{Frames: []string{"main", "tiny"}, Value: 1},
	), Options{})

	assert.Contains(t, got, ">"+html.EscapeString(long)+"</div>", "the full name, uncut")
	assert.NotContains(t, got, "…", "the renderer no longer guesses where to cut")
	assert.Contains(t, styleSheet, "overflow:hidden;white-space:nowrap;text-overflow:ellipsis")

	// A frame far too narrow for its label still carries the whole label:
	// the browser hides what does not fit, so hover, search and the status
	// bar all still see the real name.
	assert.Contains(t, got, ">tiny</div>")
	assert.NotContains(t, got, "data-char-width")
	assert.NotContains(t, got, "data-min-text")
}

// A fragment is the graph and the rules it cannot borrow from a host page.
// It shares the page's geometry rather than carrying a second one, which is
// the whole reason RenderSVG became RenderFragment: an SVG kept alive for
// embedding would have been a second palette and a second truncation rule.
func TestRenderFragmentCarriesTheGraphAndItsColoursAndNothingElse(t *testing.T) {
	var b strings.Builder
	require.NoError(t, RenderFragment(&b, result(
		foldedstacks.Stack{Frames: []string{"main", "work"}, Value: 3000},
		foldedstacks.Stack{Frames: []string{"main", "idle"}, Value: 1000},
	)))
	got := b.String()

	assert.Contains(t, got, `<div class="chart"`)
	assert.Contains(t, got, `style="left:25%;width:75%"`, "the same percentage geometry as the page")
	assert.Contains(t, got, "--fill-app:", "and its own palette, since a host page has none")
	assert.Contains(t, got, ".d2{bottom:36px}")
	assert.Contains(t, got, "text-overflow:ellipsis", "including the rule that does the truncating")

	// None of the page's chrome, and none of its interactivity.
	for _, gone := range []string{"<script", "<!DOCTYPE", "id=\"status\"", "id=\"info\"", "Colour means domain"} {
		assert.NotContains(t, got, gone)
	}
}
