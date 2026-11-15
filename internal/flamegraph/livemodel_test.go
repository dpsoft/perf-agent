package flamegraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/big"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

func liveOpts() LiveOptions {
	o := exOpts()
	return LiveOptions{Title: o.Title, ProfileName: o.ProfileName, Digest: o.Digest, Attempt: o.Attempt}
}

// liveOf builds the live model and decodes it.
func liveOf(t *testing.T, v *profilemodel.View, r explain.Result, opts LiveOptions) (map[string]any, []byte) {
	t.Helper()
	raw, err := ExplainedLiveModel(v, r, opts)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out, raw
}

func list(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	items, ok := m[key].([]any)
	require.True(t, ok, "%s is not a list: %v", key, m[key])
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any))
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// drawnIDs are the node IDs of the drawn frames, in drawn order.
func drawnIDs(t *testing.T, live map[string]any) []string {
	t.Helper()
	frames := list(t, live["model"].(map[string]any), "frames")
	var out []string
	for _, d := range list(t, live, "drawn") {
		i, err := strconv.Atoi(str(d, "i"))
		require.NoError(t, err)
		out = append(out, str(frames[i], "id"))
	}
	return out
}

func unesc(s string) string { return html.UnescapeString(s) }

func TestLiveModelMatchesTheStaticPage(t *testing.T) {
	for name, build := range map[string]func(*testing.T) (*profilemodel.View, profilemodel.NodeID, explain.Result){
		"cpu": cpuBasicExplained, "gpu": gpuBasicExplained,
	} {
		t.Run(name, func(t *testing.T) {
			v, _, r := build(t)
			page := exPage(t, v, r, exOpts())
			live, _ := liveOf(t, v, r, liveOpts())

			assert.Equal(t, "explained", live["kind"])
			assert.Equal(t, exDigest, live["digest"])
			assert.Equal(t, "cpu.pb.gz", live["profile_name"])
			assert.Equal(t, "a2", live["attempt"])
			assert.Equal(t, "", live["focus"])
			assert.Equal(t, unesc(regexp.MustCompile(`<title>(.*?)</title>`).FindStringSubmatch(page)[1]), live["doc_title"])
			assert.NotContains(t, live, "title", "the heading is title_segs only")

			pill := regexp.MustCompile(`<span class="pill ok" title="[^"]*">([^<]*)</span>`).FindStringSubmatch(page)
			require.Len(t, pill, 2)
			assert.Equal(t, unesc(pill[1]), live["checked_pill"])

			axis := regexp.MustCompile(`<div class="axis"><span>(.*?) · (.*?)</span><span title="(.*?)">total (.*?)</span></div>`).FindStringSubmatch(page)
			require.Len(t, axis, 5)
			assert.Equal(t, unesc(axis[1]), live["view"])
			assert.Equal(t, unesc(axis[2]), live["axis"])
			assert.Equal(t, unesc(axis[3]), live["total_title"])
			assert.Equal(t, unesc(axis[4]), live["total"])
			assert.Equal(t, regexp.MustCompile(`id="chart" style="height:(\d+)px"`).FindStringSubmatch(page)[1], live["height"])

			notes := []any{}
			if sec := regexp.MustCompile(`(?s)<section class="notes".*?</section>`).FindString(page); sec != "" {
				for _, m := range regexp.MustCompile(`<li>(.*?)</li>`).FindAllStringSubmatch(sec, -1) {
					notes = append(notes, unesc(m[1]))
				}
			}
			if name == "gpu" {
				require.NotEmpty(t, notes)
			}
			assert.Equal(t, notes, live["notes"])

			qgrid := regexp.MustCompile(`(?s)<div class="qgrid">(.*?)</div>`).FindStringSubmatch(page)[1]
			qs := regexp.MustCompile(`<span class="k">(.*?)</span><span class="v" title="(.*?)">(.*?)</span><span class="st st-(.*?)">.*?</span><span class="p">(.*?)</span>`).FindAllStringSubmatch(qgrid, -1)
			quality := list(t, live, "quality")
			require.Len(t, quality, len(qs))
			require.NotEmpty(t, qs)
			for i, q := range qs {
				assert.Equal(t, map[string]any{"k": unesc(q[1]), "t": unesc(q[2]), "v": unesc(q[3]), "s": unesc(q[4]), "n": unesc(q[5])}, quality[i])
			}

			legend := regexp.MustCompile(`<span class="c"><i class="sw" data-domain="([^"]*)"></i>([^<]*)</span>`).FindAllStringSubmatch(page, -1)
			got := list(t, live, "legend")
			require.Len(t, got, len(legend))
			for i, l := range legend {
				assert.Equal(t, map[string]any{"key": unesc(l[1]), "label": unesc(l[2])}, got[i])
			}

			rows := regexp.MustCompile(`<button type="button" class="exrow" data-node="(.*?)"><span class="star">✦</span><span class="ref">(.*?)</span><code>(.*?)</code><span class="w">(.*?) · (.*?)</span>`).FindAllStringSubmatch(page, -1)
			liveRows := list(t, live, "rows")
			require.NotEmpty(t, rows)
			require.Len(t, liveRows, len(rows))
			for i, row := range rows {
				assert.Equal(t, map[string]any{"node": unesc(row[1]), "ref": unesc(row[2]), "name": unesc(row[3]), "share": unesc(row[4]), "cum": unesc(row[5])}, liveRows[i])
			}

			frames := regexp.MustCompile(`<div class="frame([^"]*)" style="left:([^%]*)%;width:([^%]*)%;top:([^p]*)px" data-i="(\d+)" data-domain="([^"]*)"(?: data-resolution="([^"]*)")? tabindex="0" role="button" title="([^"]*)"><span class="fl">(<span class="star">✦</span>)?`).FindAllStringSubmatch(page, -1)
			drawn := list(t, live, "drawn")
			require.NotEmpty(t, frames)
			require.Len(t, drawn, len(frames))
			for i, f := range frames {
				d := drawn[i]
				assert.Equal(t, unesc(f[1]), str(d, "cls"), "frame %d", i)
				assert.Equal(t, f[2], d["left"], "frame %d", i)
				assert.Equal(t, f[3], d["width"], "frame %d", i)
				assert.Equal(t, f[4], d["top"], "frame %d", i)
				assert.Equal(t, f[5], d["i"], "frame %d", i)
				assert.Equal(t, f[6], d["domain"], "frame %d", i)
				assert.Equal(t, unesc(f[7]), str(d, "res"), "frame %d", i)
				assert.Equal(t, unesc(f[8]), d["tip"], "frame %d", i)
				assert.Equal(t, f[9] != "", d["star"] == true, "frame %d", i)
			}

			// The panel's model is the page's, byte for byte.
			var static any
			require.NoError(t, json.Unmarshal([]byte(modelJSONText(t, page)), &static))
			assert.Equal(t, static, live["model"])

			h1 := regexp.MustCompile(`(?s)<h1>(.*?)</h1>`).FindStringSubmatch(page)[1]
			var segs strings.Builder
			for _, s := range list(t, live, "title_segs") {
				segs.WriteString(str(s, "text"))
			}
			assert.Equal(t, unesc(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(h1, "")), segs.String())
		})
	}
}

func TestLiveModelMarksNumbersInTheTitle(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	opts := liveOpts()
	opts.Title = "Verified: b is 87% of CPU"
	live, _ := liveOf(t, v, r, opts)
	assert.Equal(t, []any{
		map[string]any{"kind": "text", "text": "Verified: b is "},
		map[string]any{"kind": "unchecked", "text": "87%", "title": "not checked by perf-agent"},
		map[string]any{"kind": "text", "text": " of CPU"},
	}, live["title_segs"])
}

func TestLiveTitleSegmentsAreTheModels(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	for _, title := range []string{"Verified: b is 87% of CPU", ""} {
		opts := liveOpts()
		opts.Title = title
		var model struct {
			TitleSegs json.RawMessage `json:"title_segs"`
		}
		_, raw := liveOf(t, v, r, opts)
		require.NoError(t, json.Unmarshal(raw, &model))
		assert.JSONEq(t, string(model.TitleSegs), string(LiveTitleSegments(title, opts.ProfileName)), "title %q", title)
	}
}

func TestLiveModelRendersTheEngineValueNotTheAgents(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	_, raw := liveOf(t, v, r, liveOpts())
	assert.Contains(t, string(raw), "41.1%")
	assert.NotContains(t, string(raw), "0.4114")
}

func TestLiveModelIsJSONEscaped(t *testing.T) {
	hostile := "operator{}<script>"
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(hostile)}, Value: 10},
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main")}, Value: 10},
	)
	v := exView(t, raw, nil)
	n := exNode(t, v, "main", hostile)
	r := exCheck(t, v, explain.Explanation{
		Node:   string(n),
		Claims: []explain.Claim{{ID: "c1", Metric: "share_of_total", Denominator: "view_total", Unit: "ratio", Value: lit("0.5")}},
		Prose:  "</script><img src=x onerror=alert(1)> {c1}",
	})
	live, b := liveOf(t, v, r, liveOpts())
	assert.NotContains(t, string(b), "<")
	assert.NotContains(t, string(b), ">")
	assert.Equal(t, hostile, modelFrame(t, live["model"].(map[string]any), n)["name"], "the name is shown whole, not tokenized")
	frame, err := ExplainedLiveFrame(v, r, n, exDigest)
	require.NoError(t, err)
	assert.NotContains(t, string(frame), "<")
	history := ExplainedLiveHistory(v, "p1", "1", []explain.RejectedAttempt{{Attempt: "a1<b>", Failures: []explain.Failure{
		failure(explain.CodeValueMismatch, "c1", "", "<i>0.6", nil)}}}, 0, false, 1<<20)
	assert.NotContains(t, string(history), "<")
}

// jsonDepth is the deepest nesting of objects and arrays in a JSON text.
func jsonDepth(t *testing.T, raw []byte) int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	depth, deepest := 0, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return deepest
		}
		require.NoError(t, err)
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
			deepest = max(deepest, depth)
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
}

func TestLiveModelIsFlatOnDeepStacks(t *testing.T) {
	v := exView(t, pmfixture.Deep(2000), nil)
	path := make([]string, 2000)
	for i := range path {
		path[i] = "f" + strconv.Itoa(i)
	}
	leaf := exNode(t, v, path...)
	r := exCheck(t, v, explain.Explanation{Node: string(leaf), Prose: "The leaf {n:" + string(leaf) + "}."})
	live, raw := liveOf(t, v, r, liveOpts())
	assert.LessOrEqual(t, jsonDepth(t, raw), 8)
	drawn := list(t, live, "drawn")
	assert.Len(t, drawn, 2001, "every node of the chain and the root is drawn")
	for _, d := range drawn {
		_, err := strconv.Atoi(str(d, "top"))
		assert.NoError(t, err, "top %v", d["top"])
	}
}

func TestLiveModelHugeValues(t *testing.T) {
	v := exView(t, pmfixture.HugeValue(), nil)
	bigNode := exNode(t, v, "main", "big")
	r := exCheck(t, v, explain.Explanation{
		Node:   string(bigNode),
		Claims: []explain.Claim{{ID: "c1", Metric: "cumulative", Unit: "nanoseconds", Value: explain.Literal{Text: "9007199254740993", Kind: explain.LiteralString}}},
		Prose:  "{c1} in all.",
	})
	_, raw := liveOf(t, v, r, liveOpts())
	assert.Contains(t, string(raw), `"9007199254740993"`)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	limit := new(big.Float).SetInt64(1 << 53)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if n, ok := tok.(json.Number); ok {
			f, _, err := big.ParseFloat(string(n), 10, 128, big.ToNearestEven)
			require.NoError(t, err)
			assert.LessOrEqual(t, f.Abs(f).Cmp(limit), 0, "bare number %s", n)
		}
	}
}

func TestLiveModelStaysUnderItsByteBudget(t *testing.T) {
	v := wideView(t, 20000, 300)
	main := exNode(t, v, "main")
	r := exCheck(t, v, explain.Explanation{Node: string(main), Prose: "main"})
	opts := liveOpts()
	opts.MaxBytes = 64 << 10
	live, raw := liveOf(t, v, r, opts)
	assert.LessOrEqual(t, len(raw), 64<<10)
	assert.Regexp(t, `^\d+ nodes beyond the page's size budget are not drawn\.$`, live["size_note"])
	assert.Contains(t, drawnIDs(t, live), string(main), "the explained path is drawn")
	assert.Greater(t, len(raw), 32<<10, "the budget is spent on frames, not wasted")

	opts.MaxBytes = 512
	_, err := ExplainedLiveModel(v, r, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "512")
}

func TestLiveModelRefusesAFailedCheck(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	_, err := ExplainedLiveModel(nil, r, liveOpts())
	require.Error(t, err)
	r.OK = false
	_, err = ExplainedLiveModel(v, r, liveOpts())
	require.Error(t, err)
	_, err = ExplainedLiveFrame(v, r, v.Root.ID, exDigest)
	require.Error(t, err)
}

func TestLiveModelFocusLaysOutTheSubtree(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	a, main, c := exNode(t, v, "main", "a"), exNode(t, v, "main"), exNode(t, v, "main", "c")
	opts := liveOpts()
	opts.Focus = a
	live, _ := liveOf(t, v, r, opts)
	assert.Equal(t, string(a), live["focus"])
	drawn := list(t, live, "drawn")
	require.NotEmpty(t, drawn)
	assert.Equal(t, "0", drawn[0]["left"])
	assert.Equal(t, "100", drawn[0]["width"])
	assert.Equal(t, "0", drawn[0]["top"])
	assert.Equal(t, []string{string(a), string(b)}, drawnIDs(t, live), "a's subtree only")
	assert.Equal(t, "80.431", drawn[1]["width"], "widths are relative to a: 411/511")
	assert.Equal(t, "20", drawn[1]["top"])
	assert.True(t, strings.HasSuffix(str(live, "axis"), "; widths relative to a, shares are of the view total"), live["axis"])
	model := live["model"].(map[string]any)
	assert.Equal(t, "main", modelFrame(t, model, main)["name"], "the focus's callers are in the model for the path")
	assert.Equal(t, "41.1%", modelFrame(t, model, b)["share"], "shares are of the view total")
	assert.Equal(t, "41.1% of total", strings.SplitN(str(drawn[1], "tip"), ", ", 2)[1])

	// A focus that holds none of the explained nodes still lists them.
	opts.Focus = c
	live, _ = liveOf(t, v, r, opts)
	assert.Equal(t, []string{string(c)}, drawnIDs(t, live))
	fb := modelFrame(t, live["model"].(map[string]any), b)
	assert.NotEmpty(t, fb["explanation"], "the explained node is in the model with its explanation")
	rows := list(t, live, "rows")
	require.Len(t, rows, 1)
	assert.Equal(t, string(b), rows[0]["node"])
	assert.Equal(t, fb["ref"], rows[0]["ref"])

	opts.Focus = "0123456789abcdef"
	_, err := ExplainedLiveModel(v, r, opts)
	require.ErrorIs(t, err, profilemodel.ErrUnknownNode)
}

func TestLiveFrameGivesFactsForAnUndrawnNode(t *testing.T) {
	samples := []pmfixture.Sample{
		{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("hot")}, Value: 100000},
		{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("tiny")}, Value: 1},
	}
	v := exView(t, pmfixture.Build("cpu", "nanoseconds", nil, samples...), nil)
	main, hot, tiny := exNode(t, v, "main"), exNode(t, v, "main", "hot"), exNode(t, v, "main", "tiny")
	r := exCheck(t, v, explain.Explanation{Node: string(hot), Prose: "hot calls {n:" + string(main) + "}"})
	live, _ := liveOf(t, v, r, liveOpts())
	require.NotContains(t, drawnIDs(t, live), string(tiny), "tiny is under minDrawFraction")

	raw, err := ExplainedLiveFrame(v, r, tiny, exDigest)
	require.NoError(t, err)
	var got struct {
		Frame map[string]any   `json:"frame"`
		Path  []map[string]any `json:"path"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, string(tiny), got.Frame["id"])
	assert.Equal(t, "tiny", got.Frame["name"])
	assert.Equal(t, string(main), got.Frame["parent"])
	facts := got.Frame["facts"].([]any)
	require.Len(t, facts, 5)
	assert.Equal(t, "1", facts[0].(map[string]any)["c"])
	assert.Nil(t, got.Frame["explanation"])
	require.Len(t, got.Path, 2)
	for i, want := range []profilemodel.NodeID{main, tiny} {
		assert.Equal(t, string(want), got.Path[i]["id"])
		assert.Equal(t, string(want), got.Path[i]["node"])
		assert.Contains(t, got.Path[i], "ref")
		assert.NotEmpty(t, got.Path[i]["share"])
	}
	assert.Equal(t, "main", got.Path[0]["name"])

	// An explained frame carries its explanation and the facts only an
	// explanation has.
	raw, err = ExplainedLiveFrame(v, r, hot, exDigest)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.NotEmpty(t, got.Frame["explanation"])
	assert.NotEmpty(t, got.Frame["extra"])
	// It is the model's explanation, except that a chip names no ref: refs
	// are places in one model's frames.
	want := modelFrame(t, live["model"].(map[string]any), hot)["explanation"].([]any)
	prose := want[0].(map[string]any)["prose"].([]any)
	require.Equal(t, "node", prose[1].(map[string]any)["kind"])
	prose[1].(map[string]any)["text"] = ""
	assert.Equal(t, want, got.Frame["explanation"])
	assert.Equal(t, "", got.Frame["ref"])

	_, err = ExplainedLiveFrame(v, r, "0123456789abcdef", exDigest)
	require.ErrorIs(t, err, profilemodel.ErrUnknownNode)
}

// rejectedText is a history row as the static page's list item reads with
// its tags removed.
func rejectedText(row map[string]any) string {
	s := str(row, "attempt") + " "
	if e := str(row, "evidence"); e != "" {
		s += "(" + e + ") "
	}
	s += str(row, "label")
	switch str(row, "kind") {
	case "claim":
		if m := str(row, "metric"); m != "" {
			s += " (" + m + " of " + str(row, "node") + ")"
		}
		s += ":"
		if a := str(row, "agent"); a != "" {
			s += " the agent wrote " + a
		}
		if e := str(row, "engine"); e != "" {
			if str(row, "agent") != "" {
				s += ","
			}
			s += " perf-agent has " + e
		}
		s += str(row, "code") + ": " + str(row, "reason")
	case "more":
	default:
		s += ": " + str(row, "reason")
	}
	return s
}

func historyOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestLiveHistoryMatchesTheStaticRejectedSection(t *testing.T) {
	v, b, r := cpuBasicExplained(t)
	bad := explain.Check(v, []explain.Explanation{{
		Node:   string(b),
		Claims: []explain.Claim{{ID: "c1", Metric: "share_of_total", Denominator: "view_total", Unit: "ratio", Value: lit("0.62")}},
		Prose:  "{c1} of the time",
	}}, explain.DefaultLimits())
	require.False(t, bad.OK)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	rejected := []explain.RejectedAttempt{
		{Attempt: "a1", Failures: bad.Failures},
		{Attempt: "a2", Failures: []explain.Failure{
			failure(explain.CodeValueMismatch, "c1", "", "0.62", engine),
			failure(explain.CodeDuplicateClaimID, "c1", "", "0.5", nil),
			failure(explain.CodeUnresolvedClaimRef, "", "c9", "", nil),
		}, Omitted: 3, Evidence: "made with handle 0123<b>: opened without an executions log"},
		{Attempt: "a3", Failures: []explain.Failure{
			{Explanation: -1, Ref: "MaxProseBytes", Code: explain.CodeLimitExceeded, Message: "the prose's size in bytes is 9000"},
		}},
	}
	opts := exOpts()
	opts.Rejected, opts.DroppedRejected, opts.HistoryIncomplete = rejected, 2, true
	page := exPage(t, v, r, opts)
	h := historyOf(t, ExplainedLiveHistory(v, "p3", "7", rejected, 2, true, 1<<20))
	assert.Equal(t, "p3", h["publication"])
	assert.Equal(t, "7", h["version"])

	pills := regexp.MustCompile(`<span class="pill bad">([^<]*)</span>`).FindAllStringSubmatch(page, -1)
	require.Len(t, pills, 3)
	assert.Equal(t, unesc(pills[0][1]), h["rejected_pill"])
	assert.Equal(t, unesc(pills[1][1]), h["limit_pill"])
	assert.Equal(t, unesc(pills[2][1]), h["history_pill"])
	assert.Equal(t, "", h["rejected_note"])

	sec := page[strings.Index(page, "<h5>Rejected at render"):]
	sec = sec[:strings.Index(sec, "</section>")]
	items := regexp.MustCompile(`<li>(.*?)</li>`).FindAllStringSubmatch(sec, -1)
	rows := list(t, h, "rows")
	require.Len(t, rows, len(items))
	require.Len(t, rows, 5, "a claim is listed once per attempt")
	for i, it := range items {
		assert.Equal(t, unesc(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(it[1], "")), rejectedText(rows[i]), "row %d", i)
	}
	assert.Equal(t, "b (node "+string(b)+")", rows[0]["node"])

	empty := historyOf(t, ExplainedLiveHistory(v, "p1", "0", nil, 0, false, 1<<20))
	assert.Equal(t, []any{}, empty["rows"])
	assert.Equal(t, "", empty["rejected_pill"])
}

func TestLiveHistoryIsBudgeted(t *testing.T) {
	v, b, _ := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	var rejected []explain.RejectedAttempt
	for i := range 40 {
		f := failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)
		f.Message = strings.Repeat("m", 400)
		rejected = append(rejected, explain.RejectedAttempt{Attempt: fmt.Sprint("a", i+1), Failures: []explain.Failure{f}})
	}
	full := ExplainedLiveHistory(v, "p1", "1", rejected, 0, false, 1<<20)
	require.Greater(t, len(full), 16<<10)
	for _, budget := range []int{8 << 10, 2 << 10} {
		raw := ExplainedLiveHistory(v, "p1", "1", rejected, 0, false, budget)
		assert.LessOrEqual(t, len(raw), budget)
		h := historyOf(t, raw)
		assert.Equal(t, "40 claims rejected at render in 40 attempts", h["rejected_pill"], "budget %d", budget)
		rows := list(t, h, "rows")
		assert.Less(t, len(rows), 40)
		assert.Equal(t, fmt.Sprintf("%d earlier rows beyond the page's size budget are not listed; the pills count them.", 40-len(rows)), h["rejected_note"])
		if len(rows) > 0 {
			assert.Equal(t, "a40", rows[len(rows)-1]["attempt"], "the newest rows are kept")
			assert.NotEqual(t, "a1", rows[0]["attempt"])
		}
		if budget == 8<<10 {
			assert.NotEmpty(t, rows, "the budget is spent on rows")
		}
	}
}

func TestLiveModelCapsLongNames(t *testing.T) {
	long := "é" + strings.Repeat("&", 64<<10)
	v := exView(t, pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn(long)}, Value: 3}), nil)
	main, callee := exNode(t, v, "main"), exNode(t, v, "main", long)
	r := exCheck(t, v, explain.Explanation{Node: string(callee), Prose: "{n:" + string(callee) + "} under {n:" + string(main) + "}"})
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: callee, Denominator: "view_total", Unit: "ratio",
		Value: "1", Rounding: "0.001 ratio", Status: "ok"}
	var rejected []explain.RejectedAttempt
	for i := range 30 {
		rejected = append(rejected, explain.RejectedAttempt{Attempt: fmt.Sprint("a", i+1),
			Failures: []explain.Failure{failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)}})
	}
	const budget = 256 << 10
	opts := liveOpts()
	opts.MaxBytes = budget
	capped := "é" + strings.Repeat("&", maxShownName-len("é")-len("…")) + "…"

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	raw, err := ExplainedLiveModel(v, r, opts)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8*budget), "the model build")
	assert.LessOrEqual(t, len(raw), budget)

	var live map[string]any
	require.NoError(t, json.Unmarshal(raw, &live))
	model := live["model"].(map[string]any)
	f := modelFrame(t, model, callee)
	assert.Equal(t, capped, f["name"])
	rows := list(t, live, "rows")
	require.Len(t, rows, 1)
	assert.Equal(t, capped, rows[0]["name"])
	assert.Equal(t, string(callee), rows[0]["node"], "the node ID is intact")
	var names []string
	for _, e := range f["explanation"].([]any) {
		for _, s := range e.(map[string]any)["prose"].([]any) {
			if sg := s.(map[string]any); sg["kind"] == "node" {
				names = append(names, str(sg, "name"))
				assert.NotEmpty(t, sg["node"])
			}
		}
	}
	assert.Equal(t, []string{capped, "main"}, names)
	for _, fr := range list(t, model, "frames") {
		assert.LessOrEqual(t, len(str(fr, "name")), maxShownName)
	}

	runtime.GC()
	runtime.ReadMemStats(&before)
	hraw := ExplainedLiveHistory(v, "p1", "1", rejected, 0, false, budget)
	runtime.ReadMemStats(&after)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8*budget), "the history build")
	assert.LessOrEqual(t, len(hraw), budget)
	hrows := list(t, historyOf(t, hraw), "rows")
	require.Len(t, hrows, 30)
	for _, row := range hrows {
		assert.Equal(t, capped+" (node "+string(callee)+")", row["node"])
		assert.True(t, utf8.ValidString(str(row, "node")))
	}

	fraw, err := ExplainedLiveFrame(v, r, callee, exDigest)
	require.NoError(t, err)
	assert.Less(t, len(fraw), 64<<10, "the frame response does not carry the whole name")
}

// TestStaticPageUnchanged reruns the static page's goldens: the live
// builder must not change a byte of it outside its script.
func TestStaticPageUnchanged(t *testing.T) {
	_, b, _ := cpuBasicExplained(t)
	v, _, r := cpuExplained(t, cpuProse(b)+" It is 62% of time.")
	checkGolden(t, "explainpage-cpu.golden.html", stripInlineScript(exPage(t, v, r, exOpts())))
	gv, _, gr := gpuBasicExplained(t)
	checkGolden(t, "explainpage-gpu.golden.html", stripInlineScript(exPage(t, gv, gr, exOpts())))
}

func TestLiveModelNestingIsConstantWithHypotheses(t *testing.T) {
	v, _, r := cpuBasicExplained(t)
	_, raw := liveOf(t, v, r, liveOpts())
	assert.Equal(t, 9, jsonDepth(t, raw), "model > frames > frame > explanation > hypotheses > hypothesis > segment")

	deep := exView(t, pmfixture.Deep(2000), nil)
	leaf := deep.Root
	for len(leaf.Children) > 0 {
		leaf = leaf.Children[0]
	}
	dr := exCheck(t, deep, explain.Explanation{Node: string(leaf.ID), Prose: "The leaf {n:" + string(leaf.ID) + "}.",
		Hypotheses: []string{"{n:" + string(leaf.ID) + "} spins."}})
	_, raw = liveOf(t, deep, dr, liveOpts())
	assert.LessOrEqual(t, jsonDepth(t, raw), 9, "the nesting does not grow with the stack")
}

// referenceFocusFrames are the model frames a focus model holds, by the
// rule written out directly: the drawn frames, then the view's root and
// each caller of the focus and of every anchor, root first, the first time
// a node is named.
func referenceFocusFrames(v *profilemodel.View, drawn []string, focus profilemodel.NodeID, anchors []profilemodel.NodeID) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range drawn {
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range append([]profilemodel.NodeID{focus}, anchors...) {
		n, _ := v.Node(id)
		for _, p := range append([]*profilemodel.Node{v.Root}, v.Path(n)...) {
			if !seen[string(p.ID)] {
				seen[string(p.ID)] = true
				out = append(out, string(p.ID))
			}
		}
	}
	return out
}

func TestLiveModelFocusFramesFollowTheReferenceOrder(t *testing.T) {
	v := exView(t, pmfixture.CPUBasic(), nil)
	main, a, b, c := exNode(t, v, "main"), exNode(t, v, "main", "a"), exNode(t, v, "main", "a", "b"), exNode(t, v, "main", "c")
	ref := func(id profilemodel.NodeID) string { return "{n:" + string(id) + "}" }
	r := exCheck(t, v,
		explain.Explanation{Node: string(b), Prose: ref(a) + " " + ref(b) + " " + ref(main) + " " + ref(b)},
		explain.Explanation{Node: string(a), Prose: ref(b) + " " + ref(c), Hypotheses: []string{ref(main)}, NextMeasurement: ref(a)},
	)
	anchors := []profilemodel.NodeID{b, a, b, main, b, a, b, c, main, a}
	for _, focus := range []profilemodel.NodeID{c, a, b, v.Root.ID} {
		opts := liveOpts()
		opts.Focus = focus
		live, _ := liveOf(t, v, r, opts)
		drawn := drawnIDs(t, live)
		var got []string
		for i, f := range list(t, live["model"].(map[string]any), "frames") {
			assert.Equal(t, "f"+strconv.Itoa(i), f["ref"])
			got = append(got, str(f, "id"))
		}
		assert.Equal(t, referenceFocusFrames(v, drawn, focus, anchors), got, "focus %s", focus)
	}
}

func TestLiveModelFocusAddsEachPathOnce(t *testing.T) {
	v := exView(t, pmfixture.Deep(8192), nil)
	leaf := v.Root
	for len(leaf.Children) > 0 {
		leaf = leaf.Children[0]
	}
	prose := strings.Repeat("{n:"+string(leaf.ID)+"} ", 300)
	var exps []explain.Explanation
	for range 8 {
		exps = append(exps, explain.Explanation{Node: string(leaf.ID), Prose: prose})
	}
	r := exCheck(t, v, exps...)
	opts := liveOpts()
	opts.Focus, opts.MaxBytes = leaf.ID, 4<<20
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	raw, err := ExplainedLiveModel(v, r, opts)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(raw), opts.MaxBytes)
	alloc := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, alloc, uint64(200<<20), "building a %d-byte focus model allocated %d bytes", len(raw), alloc)
}

func TestLiveHistoryIsUnboundedWithoutABudget(t *testing.T) {
	v, b, _ := cpuBasicExplained(t)
	engine := &explain.EngineValue{Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
		Value: "0.411", Rounding: "0.001 ratio", Status: "ok"}
	var rejected []explain.RejectedAttempt
	for i := range 40 {
		rejected = append(rejected, explain.RejectedAttempt{Attempt: fmt.Sprint("a", i+1),
			Failures: []explain.Failure{failure(explain.CodeValueMismatch, "c1", "", "0.62", engine)}})
	}
	for _, budget := range []int{0, -1} {
		h := historyOf(t, ExplainedLiveHistory(v, "p1", "1", rejected, 0, false, budget))
		assert.Len(t, list(t, h, "rows"), 40, "budget %d", budget)
		assert.Equal(t, "", h["rejected_note"])
	}
}
