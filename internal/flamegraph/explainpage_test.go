package flamegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	v, b, _ := cpuExplained(t, "{c1} {c2}")
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
	v, b, _ := cpuExplained(t, "{c1} {c2}")
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
	v, b, _ := cpuExplained(t, "{c1} {c2}")
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
	assert.Contains(t, got, html.EscapeString("Why is node "+string(b)+" (b) wide? Use perf-agent-mcp: open the profile cpu.pb.gz (relative to the profiles root), check that its profile_digest is "+exDigest+", view cpu/nanoseconds#0."))
	for _, p := range prompts {
		assert.Contains(t, html.UnescapeString(p[1]), "open the profile cpu.pb.gz")
	}
	// Without a profile name the prompts still name the digest and view.
	opts := exOpts()
	opts.ProfileName = ""
	assert.Contains(t, exPage(t, v, r, opts), html.EscapeString("wide? Use perf-agent-mcp: profile digest "+exDigest+", view cpu/nanoseconds#0."))

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
	assert.Contains(t, got, "1 claim rejected at render</span>")
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
	assert.LessOrEqual(t, maxNesting(chart[1]), 3, "frames are siblings in one container, each holding its label and ✦")
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
	src := explainPageScript
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "fetch(", "XMLHttpRequest"} {
		assert.NotContains(t, src, forbidden)
	}
	assert.Contains(t, src, "textContent")
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
	// Three forced frames exceed the limit of 2: the sentence stays true.
	assert.Contains(t, got, html.EscapeString("3 of 7 frames drawn; 1 left out by the page's limit of 2 frames"))
	assert.Contains(t, got, "3 nodes narrower than 0.05% are not drawn")

	one := pmfixture.Build("cpu", "nanoseconds", nil, append(samples[:2:2],
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("tiny")}, Value: 1})...)
	ov := exView(t, one, nil)
	or := exCheck(t, ov, explain.Explanation{Node: string(exNode(t, ov, "main", "hot")), Prose: "hot"})
	assert.Contains(t, exPage(t, ov, or, exOpts()), "1 node narrower than 0.05% is not drawn")
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

// failure is a hand-made failure of the kind Check reports.
func failure(code explain.Code, claim, ref, agent string, engine *explain.EngineValue) explain.Failure {
	return explain.Failure{Code: code, Claim: claim, Ref: ref, Agent: agent, Engine: engine, Message: "message for " + string(code)}
}

func TestExplainedPageCountsRejectedClaimsApartFromReferences(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{
		{Attempt: "a1", Failures: []explain.Failure{
			failure(explain.CodeValueMismatch, "c1", "", "0.62", engine),
			failure(explain.CodeDuplicateClaimID, "c1", "", "0.5", nil),
			failure(explain.CodeUnresolvedClaimRef, "", "c9", "", nil),
		}},
		{Attempt: "a2", RetryOf: "a1", Failures: []explain.Failure{
			failure(explain.CodeValueMismatch, "c1", "", "0.63", engine),
		}},
		{Attempt: "a3", RetryOf: "a2", Failures: []explain.Failure{
			{Explanation: -1, Ref: "MaxProseBytes", Code: explain.CodeLimitExceeded, Message: "the prose's size in bytes is 9000"},
		}},
	}
	got := exPage(t, v, r, opts)
	assert.Contains(t, got, "2 claims and 1 reference rejected at render in 2 attempts</span>",
		"c1 counts once in a1 and once in a2; c9 is a reference")
	assert.Contains(t, got, "1 limit exceeded at render</span>")
	sec := got[strings.Index(got, "<h5>Rejected at render"):]
	sec = sec[:strings.Index(sec, "</section>")]
	assert.Equal(t, 2, strings.Count(sec, "<s>"), "only claim rows strike a figure, and c1 once per attempt")
	assert.Contains(t, sec, "<s>0.62</s>")
	assert.Contains(t, sec, "<s>0.63</s>")
	assert.NotContains(t, sec, "<s>0.5</s>")
	assert.Contains(t, sec, "reference {c9}: message for unresolved_claim_ref")
	assert.Contains(t, sec, "limit MaxProseBytes: the prose&#39;s size in bytes is 9000")
	assert.NotContains(t, sec, "— of")
	assert.Contains(t, sec, "A claim row strikes through the agent's figure beside the engine's")
}

func TestExplainedPageHeadingsSayWhoseTextItIs(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	got := exPage(t, v, r, exOpts())
	src := explainPageScript
	for _, h := range []string{"Hypotheses — the agent's, not measured", "Next measurement — the agent's suggestion, not verified"} {
		assert.Contains(t, got, h)
		assert.Contains(t, src, h)
	}
}

func TestExplainedPageBoundsTheNameInPrompts(t *testing.T) {
	long := strings.Repeat("n", 150) + strings.Repeat("é", 150)
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(long)}, Value: 10})
	v := exView(t, raw, nil)
	n := exNode(t, v, "main", long)
	r := exCheck(t, v, explain.Explanation{Node: string(n), Prose: "long"})
	got := exPage(t, v, r, exOpts())
	cut := string([]rune(long)[:200]) + "…"
	assert.Contains(t, got, html.EscapeString("Why is node "+string(n)+" ("+cut+") wide?"))
	prompts := regexp.MustCompile(`data-prompt="([^"]*)"`).FindAllStringSubmatch(got, -1)
	require.NotEmpty(t, prompts)
	for _, p := range prompts {
		assert.NotContains(t, html.UnescapeString(p[1]), string([]rune(long)[:201]))
	}
	src := explainPageScript
	assert.Contains(t, src, "200", "the script bounds the name the same way")
}

// wideView is a view of at least frames nodes with names of nameLen bytes
// full of template brackets, every node wide enough to draw.
func wideView(t *testing.T, frames, nameLen int) *profilemodel.View {
	t.Helper()
	const depth = 10
	var samples []pmfixture.Sample
	// frames/depth unit samples: each node is 1/(frames/depth) of the total,
	// which is minDrawFraction at 20000 frames.
	for i := range frames / depth {
		fr := []pmfixture.Frame{pmfixture.Fn("main")}
		for d := range depth {
			name := fmt.Sprintf("ns::tmpl<std::pair<int,%d>>::f%d&", i, d)
			name += strings.Repeat("<x>", (nameLen-len(name))/3+1)
			fr = append(fr, pmfixture.Fn(name[:nameLen]))
		}
		samples = append(samples, pmfixture.Sample{Frames: fr, Value: 1})
	}
	return exView(t, pmfixture.Build("cpu", "nanoseconds", nil, samples...), nil)
}

func TestExplainedPageStaysUnderItsByteBudget(t *testing.T) {
	v := wideView(t, 20000, 300)
	main := exNode(t, v, "main")
	r := exCheck(t, v, explain.Explanation{Node: string(main), Prose: "main"})
	for _, budget := range []int{32 << 20, 4 << 20} {
		opts := exOpts()
		opts.MaxBytes = budget
		got := exPage(t, v, r, opts)
		assert.LessOrEqual(t, len(got), budget)
		left := regexp.MustCompile(`(\d+) nodes? beyond the page&#39;s size budget (is|are) not drawn|(\d+) left out by the page&#39;s limit`).FindString(got)
		assert.NotEmpty(t, left, "budget %d: the page states what it left out", budget)
		if budget == 4<<20 {
			assert.Regexp(t, `\d+ nodes beyond the page&#39;s size budget are not drawn`, got)
			assert.Greater(t, len(got), budget/2, "the budget is used, not wasted")
		}
	}
}

func TestExplainedPageRefusesABudgetTooSmallForTheExplainedPaths(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	opts := exOpts()
	opts.MaxBytes = 1024
	var b bytes.Buffer
	err := WriteExplainedPage(&b, v, r, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1024")
	assert.Zero(t, b.Len())
}

func TestExplainedPageEmitsRepeatedTextOnce(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	got := exPage(t, v, r, exOpts())
	model := modelJSONText(t, got)
	for _, label := range []string{`"Cumulative"`, `"Share of parent"`, `"from inexact samples"`, `"rounded to "`} {
		assert.LessOrEqual(t, strings.Count(model, label), 1, "%s is emitted once", label)
	}
	assert.NotContains(t, model, `"of main"`, "the parent note is built by the script")
	for _, tip := range regexp.MustCompile(`class="frame[^"]*"[^>]*title="([^"]*)"`).FindAllStringSubmatch(got, -1) {
		for _, name := range []string{"main", "all", "c\n", "\nb"} {
			assert.NotContains(t, tip[1], name)
		}
	}
}

// frameRules are the CSS rules of page that can match a frame element:
// every selector whose last compound has no element type but div or *,
// and whose classes are all classes a frame can carry, the ones it is
// drawn with or the script adds. A bare .cite is such a rule: the script
// adds cite to a frame the selected explanation references.
func frameRules(page string) map[string]string {
	var css strings.Builder
	for _, m := range regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(page, -1) {
		css.WriteString(m[1] + "\n")
	}
	frameClasses := map[string]bool{"frame": true}
	for _, m := range regexp.MustCompile(`<div class="(frame[^"]*)"`).FindAllStringSubmatch(page, -1) {
		for c := range strings.FieldsSeq(m[1]) {
			frameClasses[c] = true
		}
	}
	for _, m := range regexp.MustCompile(`classList\.(?:add|toggle)\('([^']+)'`).FindAllStringSubmatch(page, -1) {
		frameClasses[m[1]] = true
	}
	compound := regexp.MustCompile(`^([a-zA-Z*][a-zA-Z0-9-]*)?((?:\.[a-zA-Z0-9_-]+|\[[^\]]*\]|::?[a-zA-Z-]+(?:\([^)]*\))?)*)$`)
	classRE := regexp.MustCompile(`\.([a-zA-Z0-9_-]+)`)
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css.String(), -1) {
		for sel := range strings.SplitSeq(m[1], ",") {
			sel = strings.TrimSpace(sel)
			parts := strings.Fields(strings.NewReplacer(">", " ", "+", " ", "~", " ").Replace(sel))
			if len(parts) == 0 || strings.HasPrefix(sel, "@") {
				continue
			}
			c := compound.FindStringSubmatch(parts[len(parts)-1])
			if c == nil || c[1] != "" && c[1] != "div" && c[1] != "*" {
				continue
			}
			matches := true
			for _, cl := range classRE.FindAllStringSubmatch(c[2], -1) {
				matches = matches && frameClasses[cl[1]]
			}
			if matches && c[1]+c[2] != "" {
				out[sel] += m[2] + ";"
			}
		}
	}
	return out
}

// A frame's width is its value's share of the chart. Padding or a border
// on the positioned box would make every frame at least that wide, so a
// 0.1% frame or a zero-weight one would overstate its cost and spill over
// its siblings.
func TestExplainedPageFrameBoxCarriesNoPaddingOrBorder(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	got := exPage(t, v, r, exOpts())
	rules := frameRules(got)
	require.Contains(t, rules, ".frame")
	require.Contains(t, rules, ".frame.cite")
	require.Contains(t, frameRules("<style>.cite{padding:0 4px}</style>"+got), ".cite", "a bare class a frame can carry is caught")
	assert.Contains(t, rules[".frame"], "overflow:hidden")
	sizing := regexp.MustCompile(`(^|;)\s*(padding(-left|-right|-inline[-a-z]*)?|border(-left|-right|-width|-inline[-a-z]*)?|min-width|margin(-left|-right)?)\s*:\s*([^;]*)`)
	for sel, decls := range rules {
		for _, d := range sizing.FindAllStringSubmatch(decls, -1) {
			assert.Contains(t, []string{"0", "0px", "none"}, strings.TrimSpace(d[6]), "%s sets %s: %s", sel, d[2], d[6])
		}
	}
	assert.Regexp(t, `\.frame \.fl\{[^}]*padding:0 5px[^}]*text-overflow:ellipsis`, got, "the label carries the padding, clipped by the frame")
	assert.Regexp(t, `<div class="frame[^>]*><span class="fl">`, got)
}

func chromePath() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// geometryProbe selects the zero frame, then measures every frame and
// writes, per frame, its rendered width, the width its percentage asks
// for, whether it is cited by the selected explanation, and its name.
const geometryProbe = `<script>
addEventListener('load', () => {
  const chart = document.getElementById('chart');
  const cw = chart.getBoundingClientRect().width;
  const sel = [...chart.querySelectorAll('.frame')].find((el) => el.textContent === '\u2726zero');
  if (sel) sel.click();
  const rows = [...chart.querySelectorAll('.frame')].map((el) => [
    el.getBoundingClientRect().width.toFixed(3),
    (parseFloat(el.style.width) / 100 * cw).toFixed(3),
    el.classList.contains('cite') ? 'cite' : '-',
    el.textContent,
  ].join('\t'));
  const pre = document.createElement('pre');
  pre.id = 'geometry';
  pre.textContent = rows.join('\n');
  document.body.appendChild(pre);
});
</script>`

func TestExplainedPageFramesAreNoWiderThanTheirShareInChrome(t *testing.T) {
	chrome := chromePath()
	if chrome == "" || testing.Short() {
		t.Skip("no headless Chrome")
	}
	samples := []pmfixture.Sample{{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("big")}, Value: 997}}
	for _, name := range []string{"tiny1", "tiny2", "tiny3"} {
		samples = append(samples, pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(name)}, Value: 1})
	}
	samples = append(samples, pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("zero")}, Value: 0})
	v := exView(t, pmfixture.Build("cpu", "nanoseconds", nil, samples...), nil)
	zero := exNode(t, v, "main", "zero")
	tiny := exNode(t, v, "main", "tiny1")
	r := exCheck(t, v, explain.Explanation{Node: string(zero), Prose: "nothing measured under {n:" + string(zero) + "}, unlike {n:" + string(tiny) + "}"})
	page := exPage(t, v, r, exOpts())
	require.Contains(t, page, `width:0%`, "the zero-weight node is drawn because it is explained")

	dir := t.TempDir()
	file := filepath.Join(dir, "page.html")
	require.NoError(t, os.WriteFile(file, []byte(strings.Replace(page, "</body>", geometryProbe+"</body>", 1)), 0o644))
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chrome, "--headless=new", "--disable-gpu", "--no-first-run",
		"--user-data-dir="+filepath.Join(dir, "profile"), "--window-size=1000,800", "--virtual-time-budget=2000",
		"--dump-dom", "file://"+file).Output()
	if err != nil {
		t.Skipf("headless Chrome did not run: %v", err)
	}
	m := regexp.MustCompile(`(?s)<pre id="geometry">(.*?)</pre>`).FindStringSubmatch(string(out))
	require.Len(t, m, 2, "the probe did not run")
	seen := map[string]bool{}
	for line := range strings.SplitSeq(html.UnescapeString(m[1]), "\n") {
		f := strings.SplitN(line, "\t", 4)
		require.Len(t, f, 4, line)
		cited := f[2] == "cite"
		f = []string{f[0], f[1], f[3]}
		rendered, err := strconv.ParseFloat(f[0], 64)
		require.NoError(t, err)
		want, err := strconv.ParseFloat(f[1], 64)
		require.NoError(t, err)
		assert.LessOrEqual(t, rendered, want+1, "frame %q is %.3fpx wide, its share is %.3fpx", f[2], rendered, want)
		seen[f[2]] = true
		if f[2] == "tiny1" {
			assert.True(t, cited, "the selected explanation cites tiny1")
		}
		if f[2] == "✦zero" {
			assert.LessOrEqual(t, rendered, 1.0, "the zero-weight frame is drawn %.3fpx wide", rendered)
		}
	}
	for _, name := range []string{"tiny1", "tiny2", "tiny3", "✦zero"} {
		assert.True(t, seen[name], "frame %s was measured", name)
	}
}

func TestExplainedPageCountsRejectedAttemptsTheServerDidNotKeep(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{{Attempt: "a3", Failures: []explain.Failure{failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)}}}
	opts.DroppedRejected = 2
	got := exPage(t, v, r, opts)
	assert.Contains(t, got, "1 claim rejected at render, plus 2 earlier rejected attempts not kept</span>")
	assert.NotContains(t, got, "exceeded at render")
	assert.Contains(t, got, "<s>0.62</s>")

	opts.Rejected, opts.DroppedRejected = nil, 1
	got = exPage(t, v, r, opts)
	assert.Contains(t, got, "1 earlier rejected attempt not kept</span>")
	assert.NotContains(t, got, "exceeded at render")
}

func TestExplainedPageSaysWhenAttemptHistoryMayBeIncomplete(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	opts := exOpts()
	assert.NotContains(t, exPage(t, v, r, opts), "may be incomplete")
	opts.HistoryIncomplete = true
	got := exPage(t, v, r, opts)
	assert.Contains(t, got, "earlier attempt history for this view may be incomplete</span>")
	assert.NotContains(t, got, "exceeded at render")
}

func TestExplainedPageSaysHowManyFailuresARecordLeftOut(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{{Attempt: "a1", Failures: []explain.Failure{failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)}, Omitted: 8}}
	got := exPage(t, v, r, opts)
	sec := got[strings.Index(got, "<h5>Rejected at render"):]
	sec = sec[:strings.Index(sec, "</section>")]
	assert.Contains(t, sec, `<span class="ref">a1</span> and 8 more failures</li>`)
	assert.Contains(t, got, "1 claim rejected at render</span>")
}

func TestTheRejectedPillCountsOmittedClaimsAndReferences(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{{Attempt: "a1", Failures: []explain.Failure{
		failure(explain.CodeValueMismatch, "c1", "", "0.62", engine),
		failure(explain.CodeValueMismatch, "c2", "", "0.63", engine),
	}, Omitted: 10, OmittedClaims: 6, OmittedReferences: 3}}
	got := exPage(t, v, r, opts)
	assert.Contains(t, got, "8 claims and 3 references rejected at render</span>")
	assert.Contains(t, got, `<span class="ref">a1</span> and 10 more failures</li>`)
	assert.Equal(t, 2, strings.Count(got, "<s>0.6"), "only the kept claims are listed")
}

func TestExplainedPageLabelsAttemptsCheckedAgainstOtherEvidence(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "executions_below", Scope: "node", Node: b, Unit: "count", Rounding: "exact", Status: "unavailable", Reason: "no sidecar"}
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{
		{Attempt: "a1", Failures: []explain.Failure{failure(explain.CodeUnavailableFact, "c1", "", "4", engine)}, Omitted: 2, Evidence: "made with handle 0123<b>: opened without an executions log"},
		{Attempt: "a2", Failures: []explain.Failure{failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)}},
	}
	got := exPage(t, v, r, opts)
	label := `<span class="muted">(made with handle 0123&lt;b&gt;: opened without an executions log)</span>`
	assert.Contains(t, got, `<span class="ref">a1</span> `+label+` claim c1`)
	assert.Contains(t, got, `<span class="ref">a1</span> `+label+` and 2 more failures</li>`)
	assert.Contains(t, got, `<span class="ref">a2</span> claim c1`)
}

func TestTheRejectedSectionIsBudgetedBeforeForcedFrames(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	bare := exPage(t, v, r, exOpts())
	opts := exOpts()
	for i := range 40 {
		f := failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)
		f.Message = strings.Repeat("m", 1000)
		opts.Rejected = append(opts.Rejected, explain.RejectedAttempt{Attempt: fmt.Sprint("a", i+1), Failures: []explain.Failure{f}})
	}
	full := exPage(t, v, r, opts)
	require.Greater(t, len(full), len(bare)+40_000)
	for _, room := range []int{20_000, 2_000} {
		opts.MaxBytes = len(bare) + room
		got := exPage(t, v, r, opts)
		assert.LessOrEqual(t, len(got), opts.MaxBytes, "room %d", room)
		assert.Contains(t, got, "40 claims rejected at render in 40 attempts</span>", "room %d", room)
		sec := got[strings.Index(got, "<h5>Rejected at render"):]
		sec = sec[:strings.Index(sec, "</section>")]
		listed := strings.Count(sec, "<li>")
		assert.Less(t, listed, 40, "room %d", room)
		assert.Contains(t, sec, fmt.Sprintf("%d earlier rows beyond the page&#39;s size budget are not listed", 40-listed), "room %d", room)
		if listed > 0 {
			assert.Contains(t, sec, `<span class="ref">a40</span>`, "room %d: the newest rows are the ones kept", room)
			assert.NotContains(t, sec, `<span class="ref">a1</span>`, "room %d", room)
		}
		for _, frame := range []string{">main</span></div>", ">a</span></div>", "✦</span>b</span></div>"} {
			assert.Contains(t, got, frame, "room %d: forced frame %s", room, frame)
		}
	}
}

func TestLongFrameNamesAreCappedOnThePage(t *testing.T) {
	long := "é" + strings.Repeat("&", 64<<10)
	v := exView(t, pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(long)}, Value: 3}), nil)
	main, callee := exNode(t, v, "main"), exNode(t, v, "main", long)
	r := exCheck(t, v, explain.Explanation{Node: string(main), Prose: "main calls {n:" + string(callee) + "}"})
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: callee, Denominator: "view_total", Unit: "ratio",
		Value: "1", Rounding: "0.001 ratio", Status: "ok"}
	opts := exOpts()
	opts.Rejected = []explain.RejectedAttempt{{Attempt: "a1", Failures: []explain.Failure{failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)}}}
	got := exPage(t, v, r, opts)
	assert.NotContains(t, got, strings.Repeat("&amp;", maxShownName), "a name is shown whole")
	assert.Less(t, len(got), 200<<10)
	capped := "é" + strings.Repeat("&amp;", maxShownName-len("é")-len("…")) + "…"
	assert.Contains(t, got, capped+"</span></div>", "the drawn frame")
	assert.Contains(t, got, "<code>"+capped+"</code>", "the reference in the prose")
	assert.Contains(t, got, " of "+capped+" (node "+string(callee)+"))", "the rejected row")
	assert.Equal(t, maxShownName, len(shownName(strings.Repeat("&", 64<<10))))
	cut := shownName(strings.Repeat("é", 601))
	assert.True(t, utf8.ValidString(cut) && len(cut) <= maxShownName && strings.HasSuffix(cut, "é…"), "%d bytes", len(cut))
	assert.Equal(t, "main", shownName("main"))
}

func TestALongHistoryIsNotRenderedWhole(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	reason := strings.Repeat("&", 4<<10)
	opts := exOpts()
	for i := range 5000 {
		f := failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)
		f.Message = reason
		opts.Rejected = append(opts.Rejected, explain.RejectedAttempt{Attempt: fmt.Sprint("a", i+1), Failures: []explain.Failure{f}})
	}
	opts.MaxBytes = len(exPage(t, v, r, exOpts())) + 64<<10
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var page bytes.Buffer
	require.NoError(t, WriteExplainedPage(&page, v, r, opts))
	runtime.ReadMemStats(&after)
	// The history escapes to about 100 MB; the page is a few hundred KB.
	alloc := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, alloc, uint64(32<<20), "rendering a %d-byte page allocated %d bytes", page.Len(), alloc)
	assert.LessOrEqual(t, page.Len(), opts.MaxBytes)
	assert.Contains(t, page.String(), `<span class="ref">a5000</span>`)
}

func TestExplainedPageMarksNumbersInTheTitle(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	opts := exOpts()
	opts.Title = "Verified: b is 87% of CPU"
	got := exPage(t, v, r, opts)
	h1 := regexp.MustCompile(`(?s)<h1>(.*?)</h1>`).FindStringSubmatch(got)
	require.Len(t, h1, 2)
	assert.Equal(t, `Verified: b is <mark class="unchecked" title="not checked by perf-agent">87%</mark> of CPU`, h1[1])
	assert.Contains(t, got, "<title>Verified: b is 87% of CPU</title>")

	// A title with placeholders (which a server refuses) shows them as text.
	opts.Title = "{c1} under {n:0123456789abcdef}"
	h1 = regexp.MustCompile(`(?s)<h1>(.*?)</h1>`).FindStringSubmatch(exPage(t, v, r, opts))
	require.Len(t, h1, 2)
	assert.Equal(t, "{c1} under {n:0123456789abcdef}", h1[1])

	// The default title is the profile's name, not agent text: no marks.
	opts.Title, opts.ProfileName = "", "cpu-2026.pb.gz"
	h1 = regexp.MustCompile(`(?s)<h1>(.*?)</h1>`).FindStringSubmatch(exPage(t, v, r, opts))
	require.Len(t, h1, 2)
	assert.Equal(t, "cpu-2026.pb.gz", h1[1])
}

func TestShownClaimNamesTheUnitsItShows(t *testing.T) {
	for _, tc := range []struct {
		e     explain.EngineValue
		text  string
		units []string
	}{
		{explain.EngineValue{Unit: "ratio", Value: "0.411", Status: "ok"}, "41.1%", []string{"%"}},
		{explain.EngineValue{Unit: "ratio", Value: "0.000", Status: "ok"}, "0.0%", []string{"%"}},
		{explain.EngineValue{Unit: "ratio", Value: "0.000", Status: "zero"}, "0", nil},
		{explain.EngineValue{Unit: "nanoseconds", Value: "411", Status: "ok"}, "411 ns", []string{"ns", "µs", "ms", "s"}},
		{explain.EngineValue{Unit: "nanoseconds", Value: "1500000", Status: "ok"}, "1.50 ms", []string{"ns", "µs", "ms", "s"}},
		{explain.EngineValue{Unit: "bytes", Value: "1234", Status: "ok"}, "1,234 bytes", []string{"bytes"}},
		{explain.EngineValue{Unit: "count", Value: "3", Status: "ok"}, "3", nil},
		{explain.EngineValue{Unit: "nanoseconds", Value: "0", Status: "zero"}, "0", nil},
		{explain.EngineValue{Unit: "nanoseconds", Status: "unavailable", Reason: "not a GPU view"}, "unavailable: not a GPU view", nil},
	} {
		text, units := ShownClaim(tc.e)
		assert.Equal(t, tc.text, text)
		assert.Equal(t, tc.units, units, tc.text)
	}
}
