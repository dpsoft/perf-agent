package flamegraph

import (
	"bytes"
	"encoding/json"
	"html"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

const exDigest = "sha256:9c1e4b7a00000000000000000000000000000000000000000000000000000000"

func exView(t *testing.T, raw, log []byte) *profilemodel.View {
	t.Helper()
	p, err := profilemodel.FromBytes(raw, log, profilemodel.DefaultLimits())
	require.NoError(t, err)
	v, err := p.View(profilemodel.ViewOptions{SampleIndex: -1})
	require.NoError(t, err)
	return v
}

// exNode finds the node whose display-name path, root first, is names.
func exNode(t *testing.T, v *profilemodel.View, names ...string) profilemodel.NodeID {
	t.Helper()
	var found []profilemodel.NodeID
	v.Walk(func(n *profilemodel.Node) bool {
		var path []string
		for _, q := range v.Path(n) {
			path = append(path, q.Function.Name)
		}
		if slices.Equal(path, names) {
			found = append(found, n.ID)
		}
		return true
	})
	require.Len(t, found, 1, "nodes at path %q", names)
	return found[0]
}

func lit(s string) explain.Literal { return explain.Literal{Text: s, Kind: explain.LiteralNumber} }

func exCheck(t *testing.T, v *profilemodel.View, exps ...explain.Explanation) explain.Result {
	t.Helper()
	r := explain.Check(v, exps, explain.DefaultLimits())
	require.True(t, r.OK, "check failed: %+v", r.Failures)
	return r
}

func exPage(t *testing.T, v *profilemodel.View, r explain.Result, opts ExplainedPageOptions) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, WriteExplainedPage(&b, v, r, opts))
	return b.String()
}

func exOpts() ExplainedPageOptions {
	return ExplainedPageOptions{Title: "cpu.pb.gz explained", ProfileName: "cpu.pb.gz", Digest: exDigest, Attempt: "a2"}
}

// cpuExplained is CPUBasic with one explanation of main;a;b: two claims
// and one node reference.
func cpuExplained(t *testing.T, prose string) (*profilemodel.View, profilemodel.NodeID, explain.Result) {
	t.Helper()
	v := exView(t, pmfixture.CPUBasic(), nil)
	b := exNode(t, v, "main", "a", "b")
	r := exCheck(t, v, explain.Explanation{
		Node: string(b),
		Claims: []explain.Claim{
			{ID: "c1", Metric: "share_of_total", Denominator: "view_total", Unit: "ratio", Value: lit("0.4114")},
			{ID: "c2", Metric: "cumulative", Unit: "nanoseconds", Value: lit("411")},
		},
		Prose:           prose,
		Hypotheses:      []string{"b is a hot loop that could be vectorized."},
		NextMeasurement: "Count calls of b per request.",
	})
	return v, b, r
}

func cpuProse(b profilemodel.NodeID) string {
	return "{c1} of the CPU time is under {n:" + string(b) + "}, {c2} in all."
}

// cpuBasicExplained is the CPU sample: CPUBasic explained at main;a;b.
func cpuBasicExplained(t *testing.T) (*profilemodel.View, profilemodel.NodeID, explain.Result) {
	t.Helper()
	v := exView(t, pmfixture.CPUBasic(), nil)
	b := exNode(t, v, "main", "a", "b")
	_, _, r := cpuExplained(t, cpuProse(b))
	return v, b, r
}

// gpuBasicExplained is the GPU sample: GPUBasic with its executions log,
// explained at train;conv, with node-level and view-level claims.
func gpuBasicExplained(t *testing.T) (*profilemodel.View, profilemodel.NodeID, explain.Result) {
	t.Helper()
	v := exView(t, pmfixture.GPUBasic(), pmfixture.GPUBasicExeclog(t))
	conv := exNode(t, v, "train", "conv")
	k1 := exNode(t, v, "train", "conv", "[gpu:launch]", "[gpu:kernel:k1]")
	r := exCheck(t, v, explain.Explanation{
		Node: string(conv),
		Claims: []explain.Claim{
			{ID: "c1", Metric: "share_of_total", Denominator: "view_total", Unit: "ratio", Value: lit("0.8")},
			{ID: "c2", Metric: "cumulative", Unit: "nanoseconds", Value: lit("400")},
			{ID: "c3", Metric: "executions_below", Unit: "count", Value: lit("4")},
			{ID: "c4", Metric: "kernel_names_observed", Unit: "count", Value: lit("3")},
			{ID: "c5", Metric: "unsampled_portion", Scope: "view", Unit: "nanoseconds", Value: lit("100")},
			{ID: "c6", Metric: "share_of_total", Node: string(k1), Denominator: "view_total", Unit: "ratio", Value: lit("0.6")},
		},
		Prose: "Of the GPU time launched from the sampled stacks, {c1} falls under {n:" + string(conv) + "}: {c2} over {c3} executions " +
			"and {c4} kernel names. Most of it is {n:" + string(k1) + "} at {c6}. {c5} of GPU time has no sampled launch stack " +
			"and is not attributed to any caller.",
		Hypotheses:      []string{"k1 is the convolution itself; k2 looks like a layout transform that runs once per layer."},
		NextMeasurement: "Count k2 executions per iteration and compare them with the number of layers.",
	})
	return v, conv, r
}

// modelOf decodes the page's model JSON.
func modelOf(t *testing.T, page string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`(?s)<script type="application/json" id="model">(.*?)</script>`).FindStringSubmatch(page)
	require.Len(t, m, 2, "the page has no model script")
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(m[1]), &out))
	return out
}

func modelFrame(t *testing.T, model map[string]any, id profilemodel.NodeID) map[string]any {
	t.Helper()
	frames, ok := model["frames"].([]any)
	require.True(t, ok, "model has no frames")
	for _, f := range frames {
		if fm := f.(map[string]any); fm["id"] == string(id) {
			return fm
		}
	}
	t.Fatalf("model has no frame %s", id)
	return nil
}

func TestExplainedPageHeaderCountsOnlyCheckedFigures(t *testing.T) {
	v, b, _ := cpuExplained(t, "x")
	_, _, r := cpuExplained(t, cpuProse(b)+" 62% elsewhere.")
	require.Equal(t, 2, r.Figures)
	require.Equal(t, 1, r.References)
	got := exPage(t, v, r, exOpts())
	assert.Contains(t, got, "3 figures and references checked")
	assert.Contains(t, got, "prose is not verified")
	assert.Contains(t, got, "Explained by an agent via MCP")
	assert.NotContains(t, strings.ToLower(got), "verified explanation")
	assert.NotContains(t, got, "rejected at render</span>", "no rejected pill without rejected attempts")
}

func TestExplainedPageRendersTheEngineValueNotTheAgents(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	got := exPage(t, v, r, exOpts())
	assert.Contains(t, got, "41.1%")
	assert.NotContains(t, got, "0.4114")
	// The claim's title names the metric, the node by name and ID, the exact
	// value and the rounding.
	title := regexp.MustCompile(`<span class="claim" title="([^"]*)">41\.1%</span>`).FindStringSubmatch(got)
	require.Len(t, title, 2, "the claim is not drawn as the engine's value with a title")
	for _, want := range []string{"share_of_total", "b", string(b), "0.411", "0.001 ratio"} {
		assert.Contains(t, html.UnescapeString(title[1]), want)
	}
}

func TestExplainedPageMarksFreeNumbers(t *testing.T) {
	v, b, _ := cpuExplained(t, "x")
	_, _, r := cpuExplained(t, cpuProse(b)+" It is 62% of time.")
	got := exPage(t, v, r, exOpts())
	assert.Contains(t, got, `<mark class="unchecked" title="not checked by perf-agent">62%</mark>`)
}

func TestExplainedPageEscapesNamesAndText(t *testing.T) {
	hostile := "operator{}<script>{c1}"
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(hostile)}, Value: 10},
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main")}, Value: 10},
	)
	v := exView(t, raw, nil)
	n := exNode(t, v, "main", hostile)
	r := exCheck(t, v, explain.Explanation{
		Node:       string(n),
		Claims:     []explain.Claim{{ID: "c1", Metric: "share_of_total", Denominator: "view_total", Unit: "ratio", Value: lit("0.5")}},
		Prose:      "</script><img src=x onerror=alert(1)> {c1} in {n:" + string(n) + "}",
		Hypotheses: []string{"javascript:alert(1)"},
	})
	got := exPage(t, v, r, exOpts())
	assert.NotContains(t, got, "<img")
	assert.NotContains(t, got, "</script><img")
	assert.NotContains(t, got, "<script>{c1}")
	assert.NotContains(t, got, "href=")
	// The name is shown as it is: its "{c1}" is not replaced by the claim.
	assert.Contains(t, got, "operator{}&lt;script&gt;{c1}")
	assert.NotContains(t, got, "operator{}&lt;script&gt;50.0%")
	assert.Equal(t, hostile, modelFrame(t, modelOf(t, got), n)["name"])
}

func TestExplainedPageLinksNothing(t *testing.T) {
	v, b, _ := cpuExplained(t, "x")
	_, _, r := cpuExplained(t, cpuProse(b)+" See https://evil.example/x for more.")
	got := exPage(t, v, r, exOpts())
	for _, forbidden := range []string{
		"<script src", "@import", "url(http", "url('http", "url(\"http",
		"fonts.googleapis.com", "fonts.gstatic.com", "cdn.", "fetch(", "XMLHttpRequest",
		"<img", "<iframe", "<link", "<a ", "href=", "src=",
	} {
		assert.NotContains(t, got, forbidden, "the page must load and link nothing")
	}
	// The agent's URL is text, nothing else: every URL in the page is that
	// one, and it is never inside an attribute.
	for _, u := range regexp.MustCompile(`https?://[^\s"'<>)\\]+`).FindAllString(got, -1) {
		assert.Equal(t, "https://evil.example/x", u)
	}
	assert.NotRegexp(t, `="[^"]*https?://`, got)
}

func TestExplainedPageAlwaysShowsSamplingNotesAndQuality(t *testing.T) {
	v, _, r := gpuBasicExplained(t)
	notes, err := v.Notes()
	require.NoError(t, err)
	require.NotEmpty(t, notes)
	got := exPage(t, v, r, exOpts())
	panel := got[:strings.Index(got, `<script type="application/json"`)]
	for _, n := range notes {
		assert.Contains(t, panel, html.EscapeString(n))
	}
	for _, want := range []string{"Sampling notes", "Quality summary", "Unresolved frames", "Executions log",
		"Exact joins", "Unsampled launches", "Serialized: true", "Serialized: false", "Serialized: unknown", "Serialized: missing",
		"width = GPU time launched from the sampled stacks of this path, not CPU time"} {
		assert.Contains(t, panel, want)
	}

	cv, _, cr := cpuBasicExplained(t)
	cpu := exPage(t, cv, cr, exOpts())
	assert.NotContains(t, cpu, "Sampling notes")
	assert.NotContains(t, cpu, "Exact joins")
	assert.Contains(t, cpu, "Quality summary")
	assert.Contains(t, cpu, "Unresolved frames")
	assert.Contains(t, cpu, "Executions log")
	assert.Contains(t, cpu, "width = cpu (nanoseconds) at or below the frame")
}

func TestExplainedPageFramesWithoutExplanationShowFactsOnly(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	model := modelOf(t, exPage(t, v, r, exOpts()))
	c := modelFrame(t, model, exNode(t, v, "main", "c"))
	assert.NotEmpty(t, c["facts"])
	assert.NotContains(t, c, "explanation")
	assert.Contains(t, modelFrame(t, model, b), "explanation")
}

func TestExplainedPageChipsNameDigestViewAndNode(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	got := exPage(t, v, r, exOpts())
	prompts := regexp.MustCompile(`data-prompt="([^"]*)"`).FindAllStringSubmatch(got, -1)
	require.Len(t, prompts, 3, "a CPU view has three follow-up chips")
	for _, p := range prompts {
		s := html.UnescapeString(p[1])
		assert.Contains(t, s, exDigest)
		assert.Contains(t, s, "cpu/nanoseconds#0")
		assert.Contains(t, s, string(b))
	}
	assert.Contains(t, got, html.EscapeString("Why is node "+string(b)+" (b) wide? Use perf-agent-mcp: profile digest "+exDigest+", view cpu/nanoseconds#0."))

	gv, conv, gr := gpuBasicExplained(t)
	gpu := exPage(t, gv, gr, exOpts())
	assert.Contains(t, gpu, html.EscapeString("Explain the CPU→GPU causality under node "+string(conv)+"."))
	assert.Len(t, regexp.MustCompile(`data-prompt="`).FindAllString(gpu, -1), 4)
}

func TestExplainedPageListsRejectedAttempts(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	bad := explain.Check(v, []explain.Explanation{{
		Node:   string(b),
		Claims: []explain.Claim{{ID: "c1", Metric: "share_of_total", Denominator: "view_total", Unit: "ratio", Value: lit("0.62")}},
		Prose:  "{c1} of the time",
	}}, explain.DefaultLimits())
	require.False(t, bad.OK)
	require.Len(t, bad.Failures, 1)
	require.Equal(t, explain.CodeValueMismatch, bad.Failures[0].Code)
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{{Attempt: "a1", Failures: bad.Failures}}
	got := exPage(t, v, r, opts)
	assert.Contains(t, got, "1 claims rejected at render")
	sec := got[strings.Index(got, "Rejected at render"):]
	assert.Contains(t, sec, "<s>0.62</s>")
	assert.Contains(t, sec, "41.1%")
	assert.Contains(t, sec, "a1")
	assert.Contains(t, sec, "share_of_total")
}

// maxNesting is the deepest element nesting of an HTML page, with the
// contents of script and style skipped and void elements not nesting.
func maxNesting(page string) int {
	page = regexp.MustCompile(`(?s)<(script|style)\b[^>]*>.*?</(script|style)>`).ReplaceAllString(page, "")
	void := map[string]bool{"meta": true, "br": true, "input": true, "img": true, "link": true, "hr": true, "wbr": true}
	depth, deepest := 0, 0
	for _, m := range regexp.MustCompile(`<(/?)([a-zA-Z][a-zA-Z0-9]*)[^>]*>`).FindAllStringSubmatch(page, -1) {
		switch {
		case void[strings.ToLower(m[2])]:
		case m[1] == "/":
			depth--
		default:
			depth++
			deepest = max(deepest, depth)
		}
	}
	return deepest
}

func TestExplainedPageIsFlatOnDeepStacks(t *testing.T) {
	v := exView(t, pmfixture.Deep(600), nil)
	path := make([]string, 600)
	for i := range path {
		path[i] = "f" + strconv.Itoa(i)
	}
	leaf := exNode(t, v, path...)
	r := exCheck(t, v, explain.Explanation{Node: string(leaf), Prose: "The leaf {n:" + string(leaf) + "}."})
	got := exPage(t, v, r, exOpts())
	assert.Less(t, maxNesting(got), 20)
	assert.Equal(t, 601, strings.Count(got, `class="frame`), "every node of the chain and the root is drawn")
	chart := regexp.MustCompile(`(?s)<div class="chart"[^>]*>(.*?)</div>\n</div>`).FindStringSubmatch(got)
	require.Len(t, chart, 2)
	assert.LessOrEqual(t, maxNesting(chart[1]), 2, "frames are siblings in one container, holding at most their ✦")
}

func TestExplainedPageIsDeterministic(t *testing.T) {
	v, _, r := gpuBasicExplained(t)
	assert.Equal(t, exPage(t, v, r, exOpts()), exPage(t, v, r, exOpts()))
}

func TestExplainedPageHugeValues(t *testing.T) {
	v := exView(t, pmfixture.HugeValue(), nil)
	big := exNode(t, v, "main", "big")
	r := exCheck(t, v, explain.Explanation{
		Node:   string(big),
		Claims: []explain.Claim{{ID: "c1", Metric: "cumulative", Unit: "nanoseconds", Value: explain.Literal{Text: "9007199254740993", Kind: explain.LiteralString}}},
		Prose:  "{c1} in all.",
	})
	got := exPage(t, v, r, exOpts())
	assert.Contains(t, modelJSONText(t, got), `"9007199254740993"`)
	assert.Equal(t, "9007199254740993", modelFrame(t, modelOf(t, got), big)["cum"])
	assert.Contains(t, got, "9007199254740993 nanoseconds", "the exact value is in a title")
}

func modelJSONText(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<script type="application/json" id="model">(.*?)</script>`).FindStringSubmatch(page)
	require.Len(t, m, 2)
	return m[1]
}

func TestExplainedPageScriptUsesTextContentOnly(t *testing.T) {
	src, err := os.ReadFile("explainpage.js")
	require.NoError(t, err)
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "fetch(", "XMLHttpRequest"} {
		assert.NotContains(t, string(src), forbidden)
	}
	assert.Contains(t, string(src), "textContent")
}

func TestExplainedPageRefusesAFailedCheckAndANilView(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	var b bytes.Buffer
	assert.Error(t, WriteExplainedPage(&b, nil, r, exOpts()))
	assert.Error(t, WriteExplainedPage(&b, v, explain.Result{}, exOpts()))
	assert.Zero(t, b.Len())
}

func TestExplainedPageZeroAndTinyAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		canonical string
		status    profilemodel.Status
		want      string
	}{
		{"0.411", profilemodel.StatusOK, "41.1%"},
		{"1.000", profilemodel.StatusOK, "100.0%"},
		{"0.000", profilemodel.StatusOK, "0.0%"},
		{"0.005", profilemodel.StatusOK, "0.5%"},
		{"0.000", profilemodel.StatusZero, "0"},
	} {
		got := displayEngine(explain.EngineValue{Value: tc.canonical, Unit: profilemodel.UnitRatio, Status: tc.status})
		assert.Equal(t, tc.want, got, "%s %s", tc.canonical, tc.status)
	}
	assert.Equal(t, "0", displayEngine(explain.EngineValue{Value: "0", Unit: "nanoseconds", Status: profilemodel.StatusZero}))
	assert.Equal(t, "411 ns", displayEngine(explain.EngineValue{Value: "411", Unit: "nanoseconds", Status: profilemodel.StatusOK}))
	assert.Equal(t, "1,416", displayEngine(explain.EngineValue{Value: "1416", Unit: profilemodel.UnitCount, Status: profilemodel.StatusOK}))
}

func TestExplainedPageStatesNodesNotDrawn(t *testing.T) {
	samples := []pmfixture.Sample{
		{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("hot")}, Value: 100000},
		{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("warm")}, Value: 50000},
	}
	for _, name := range []string{"tiny1", "tiny2", "tiny3"} {
		samples = append(samples, pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(name)}, Value: 1})
	}
	v := exView(t, pmfixture.Build("cpu", "nanoseconds", nil, samples...), nil)
	hot := exNode(t, v, "main", "hot")
	r := exCheck(t, v, explain.Explanation{Node: string(hot), Prose: "hot"})
	got := exPage(t, v, r, exOpts())
	assert.Contains(t, got, "3 nodes narrower than 0.05% are not drawn")

	opts := exOpts()
	opts.MaxNodes = 2
	got = exPage(t, v, r, opts)
	assert.Contains(t, got, "1 nodes beyond the page's limit of 2 frames are not drawn")
	assert.Contains(t, got, "3 nodes narrower than 0.05% are not drawn")
	assert.Equal(t, 3, strings.Count(got, `class="frame`), "root, main and the explained hot are drawn")
}

func TestExplainedPageClaimTitleNamesTheModule(t *testing.T) {
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.In("/usr/lib/libfoo.so", "f00d", 0x123)}, Value: 10},
	)
	v := exView(t, raw, nil)
	main := exNode(t, v, "main")
	r := exCheck(t, v, explain.Explanation{
		Node: string(main),
		Claims: []explain.Claim{{ID: "c1", Metric: "quality_unresolved_by_module", Scope: "view", Module: new(`b:"f00d"`),
			Denominator: "view_total", Unit: "ratio", Value: lit("1")}},
		Prose: "{c1} of the view has an unresolved frame in libfoo.",
	})
	got := exPage(t, v, r, exOpts())
	title := regexp.MustCompile(`<span class="claim" title="([^"]*)">100\.0%</span>`).FindStringSubmatch(got)
	require.Len(t, title, 2)
	assert.Equal(t, `quality_unresolved_by_module of the whole view, module b:"f00d": 1.000 ratio of view_total, rounded to 0.001 ratio; checked by perf-agent`,
		html.UnescapeString(title[1]))
}
