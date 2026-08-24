package flamegraph

import (
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
		"<svg class=\"flame\"", "data-name=\"main\"", "data-name=\"work\"",
		"id=\"search\"", "id=\"reset-zoom\"", "id=\"breadcrumbs\"", "id=\"match-count\"",
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
	// The one http:// in the document is the SVG XML namespace, which is an
	// identifier, not a fetch.
	for _, m := range regexp.MustCompile(`https?://[^\s"'<>)]+`).FindAllString(got, -1) {
		assert.Equal(t, "http://www.w3.org/2000/svg", m)
	}
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

	assert.NotContains(t, got, "<svg class=\"flame\"", "no graph may be drawn")
	assert.NotContains(t, got, "<script>", "no interactivity without a graph")
	assert.Contains(t, got, "No flame graph was drawn")
	assert.Contains(t, got, "zero samples")
	assert.Contains(t, got, "This profile contains no samples at all.")
	assert.Contains(t, got, "nothing to draw")
}

func TestRenderHTMLOnAnAllZeroProfileSaysSo(t *testing.T) {
	res := &foldedstacks.Result{SampleTypeName: "gpu", Unit: "nanoseconds", Samples: 40}
	got := renderString(t, res, Options{})

	assert.NotContains(t, got, "<svg class=\"flame\"")
	assert.Contains(t, got, "No flame graph was drawn")
	assert.Contains(t, got, "every one of them carries the value 0")
	assert.Contains(t, got, "gpu/nanoseconds")
}

func TestRenderSVGRefusesADegenerateProfile(t *testing.T) {
	err := RenderSVG(&strings.Builder{}, &foldedstacks.Result{SampleTypeName: "cpu", Unit: "nanoseconds"}, Options{})
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
	assert.Contains(t, got, `data-name="all" data-path="all" data-domain="root" data-domain-label="all" data-value="5987854"`)
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
	assert.Contains(t, got, "url(#p-gap)", "unsymbolized frames must be hatched")
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

	assert.Contains(t, got, `class="frame inexact"`)
	assert.Contains(t, got, "url(#p-inferred)")
	assert.Contains(t, got, `data-inexact="100"`)
	assert.Contains(t, got, "attributed by inference, not measurement")
	// The frame that was measured must not be marked.
	assert.Regexp(t, regexp.MustCompile(`class="frame" data-name="other"`), got)
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
	assert.NotContains(t, got, `data-name="gpu_join`, "a label must never become a frame")
	assert.NotContains(t, got, `data-name="gpu_queue`)
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
	assert.Contains(t, got, "stack order read as")
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

func TestTruncateNeverSplitsAMultibyteRune(t *testing.T) {
	assert.Equal(t, "", truncate("abc", 0))
	assert.Equal(t, "abc", truncate("abc", 3))
	assert.Equal(t, "ab…", truncate("abcdef", 3))
	assert.Equal(t, "日本…", truncate("日本語です", 3))
	assert.Equal(t, "日", truncate("日本語", 1))
}

func TestRenderHTMLRejectsANilResult(t *testing.T) {
	require.Error(t, RenderHTML(&strings.Builder{}, nil, Options{}))
	require.Error(t, RenderSVG(&strings.Builder{}, nil, Options{}))
}
