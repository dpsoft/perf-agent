package perfmcp

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

var updateRenderGolden = flag.Bool("update-render-golden", false, "rewrite testdata/render-cpu.golden.html from render_flamegraph")

// goldenExplanations are CPUBasic's b and c explained with every kind of
// text an explanation holds.
func goldenExplanations(b, c string) []any {
	return []any{
		map[string]any{
			"node": b,
			"claims": []any{map[string]any{
				"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": "0.411",
			}},
			"prose":            "{n:" + b + "} takes {c1} of the time, maybe 3 calls deep",
			"hypotheses":       []any{"b spins on a lock", "b allocates 2 buffers per call"},
			"next_measurement": "profile b with an off-CPU profile",
		},
		map[string]any{
			"node": c,
			"claims": []any{map[string]any{
				"id": "c2", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": "0.489",
			}},
			"prose": "{n:" + c + "} is the widest at {c2}.",
		},
	}
}

// TestRenderIsUnchangedByTheRefactor writes the same page render_flamegraph
// wrote before page_explain shared its check: a rejection, then a retry of
// it with a title, so the page holds the rejected history too.
func TestRenderIsUnchangedByTheRefactor(t *testing.T) {
	o := renderFixture(t, nil)
	b, c := o.nodeID(t, "b"), o.nodeID(t, "c")
	rej, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "b takes {c1}"), "out": "r.html"})
	if !isErr || rej.Status != "rejected" {
		t.Fatalf("the rejection: isError=%v %s", isErr, text)
	}
	out, text, isErr := o.render(t, map[string]any{
		"explanations": goldenExplanations(b, c), "out": "cpu.html", "attempt": rej.Attempt, "title": "b and c: 2 hot paths",
	})
	if isErr {
		t.Fatalf("render: %s", text)
	}
	page := readFile(t, out.Path)
	golden := filepath.Join("testdata", "render-cpu.golden.html")
	if *updateRenderGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, page, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := readFile(t, golden)
	if !bytes.Equal(page, want) {
		t.Errorf("render_flamegraph wrote %d bytes that differ from the %d of %s", len(page), len(want), golden)
	}
}

// pageClient is a stdio server with the live page engine on, an output
// root for render_flamegraph and fixtures in its profiles root. Tests
// drive it only through tool calls and its PageView.
func pageClient(t *testing.T, edit func(*Config), fixtures map[string][]byte) *testClient {
	t.Helper()
	cfg := testConfig(t)
	cfg.Page = livePage()
	cfg.OutputDir = t.TempDir()
	if edit != nil {
		edit(&cfg)
	}
	for name, raw := range fixtures {
		writeFixture(t, cfg.ProfilesDir, name, raw)
	}
	return newTestServer(t, cfg)
}

// openOn opens path (with execlog, unless "") on c at its default view.
func openOn(t *testing.T, c *testClient, path, execlog string) (opened, openOut) {
	t.Helper()
	args := map[string]any{"path": path}
	if execlog != "" {
		args["execlog"] = execlog
	}
	var op openOut
	c.callOK("open", args, &op)
	return opened{c: c, handle: op.Handle, view: op.DefaultView}, op
}

type pageOpenOut struct {
	URL          string `json:"url"`
	Current      string `json:"current"`
	Publications []struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Title string `json:"title"`
		View  string `json:"view"`
	} `json:"publications"`
	Viewers string `json:"viewers"`
}

type pageExplainOut struct {
	Attempt           string   `json:"attempt"`
	Status            string   `json:"status"`
	Publication       string   `json:"publication"`
	URL               string   `json:"url"`
	FiguresChecked    string   `json:"figures_checked"`
	ReferencesChecked string   `json:"references_checked"`
	UncheckedNumbers  string   `json:"unchecked_numbers"`
	Notices           []any    `json:"notices"`
	Notes             []string `json:"notes"`
	Viewers           string   `json:"viewers"`
}

type pageShowOut struct {
	Publication string   `json:"publication"`
	Selected    string   `json:"selected"`
	Focus       string   `json:"focus"`
	URL         string   `json:"url"`
	Viewers     string   `json:"viewers"`
	Notes       []string `json:"notes"`
}

func pageOpen(t *testing.T, c *testClient) pageOpenOut {
	t.Helper()
	var out pageOpenOut
	c.callOK("page_open", map[string]any{}, &out)
	return out
}

// pageState is the page's state as c's PageView holds it: its envelope's
// engine part.
func pageState(t *testing.T, c *testClient) stateJSON {
	t.Helper()
	return envelopeOf(t, c.srv.PageSnapshot().State()).Engine
}

// pageGet is what c's PageView answers for route, "pub/<id>/<what>" or
// "pub/<id>/<what>/<node>" as the page names it but unescaped, which must
// not be an error.
func pageGet(t *testing.T, c *testClient, route string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(route, "pub/")
	id, rest, _ := strings.Cut(rest, "/")
	what, node, _ := strings.Cut(rest, "/")
	v := c.srv.PageSnapshot()
	var out []byte
	var err error
	switch what {
	case "model":
		out, err = v.Model(id)
	case "history":
		out, err = v.History(id)
	case "frame":
		out, err = v.Frame(id, node)
	case "focus":
		out, err = v.Focus(id, node)
	case "export":
		out, err = v.Export(id)
	default:
		ok = false
	}
	if !ok {
		t.Fatalf("no route %q", route)
	}
	if err != nil {
		t.Fatalf("%s: %v", route, err)
	}
	return string(out)
}

var pageTools = []string{"page_open", "page_explain", "page_show", "page_diff"}

// Without Page no page tool is registered: the stdio server lists none and
// answers a call to one as an unknown tool, and records nothing.
func TestNoPageToolsWithoutLive(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		if slices.Contains(pageTools, tl.Name) {
			t.Errorf("%s is listed", tl.Name)
		}
	}
	for _, name := range pageTools {
		if res := c.rpc("tools/call", map[string]any{"name": name, "arguments": map[string]any{}}); res.Error == nil {
			t.Errorf("tools/call %s: no protocol error", name)
		}
	}
	if c.srv.live != nil {
		t.Error("a server without Page has a live page")
	}
	if c.srv.attempts.next != 0 {
		t.Errorf("a page tool call recorded %d attempts", c.srv.attempts.next)
	}
}

func TestPageOpenGivesTheURLAndStartsOnce(t *testing.T) {
	c := pageClient(t, nil, nil)
	first, second := pageOpen(t, c), pageOpen(t, c)
	if first.URL == "" || first.URL != second.URL {
		t.Fatalf("page_open gave %q, then %q", first.URL, second.URL)
	}
	if first.URL != testPageURL {
		t.Errorf("page_open gave %q, not the configured %q", first.URL, testPageURL)
	}
	if first.Viewers != "0" || first.Current != "" || first.Publications == nil || len(first.Publications) != 0 {
		t.Errorf("page_open before any publication: %+v", first)
	}
	if st := pageState(t, c); st.Seq != "1" {
		t.Errorf("two page_open calls committed to seq %s, want 1", st.Seq)
	}
	text := c.callOK("page_open", map[string]any{}, nil)
	if !strings.Contains(text, `"viewers":"0"`) {
		t.Errorf("viewers is not a decimal string: %s", text)
	}
}

func TestPageExplainPublishesCheckedClaims(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic(), "gpu.pb.gz": pmfixture.GPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	b := o.nodeID(t, "b")
	var out pageExplainOut
	text := c.callOK("page_explain", o.args(map[string]any{"explanations": shareClaim(b, "0.411", "b takes {c1} of the time, maybe 3 calls")}), &out)
	url := pageOpen(t, c).URL
	if out.Attempt != "a1" || out.Status != "published" || out.Publication != "p1" || out.URL != url ||
		out.FiguresChecked != "1" || out.UncheckedNumbers != "1" || out.Notices == nil || out.Viewers != "0" {
		t.Errorf("page_explain = %s", text)
	}
	if strings.Contains(text, `"notes"`) {
		t.Errorf("a CPU publication's result has notes: %s", text)
	}
	if out.ReferencesChecked != "0" {
		t.Errorf("references_checked %q without a frame reference", out.ReferencesChecked)
	}
	if st := pageState(t, c); st.Current != "p1" || st.Selected != b || st.Focus != "" || len(st.Publications) != 1 {
		t.Errorf("state %+v", st)
	}
	if model := pageGet(t, c, "pub/p1/model"); !strings.Contains(model, "41.1%") {
		t.Errorf("the model lacks the checked figure: %.300s", model)
	}

	// On a GPU view the result carries the view's sampling notes.
	g, gop := openOn(t, c, "gpu.pb.gz", "")
	if len(gop.Notes) == 0 {
		t.Fatal("GPUBasic has no notes")
	}
	conv := g.nodeID(t, "conv")
	text = c.callOK("page_explain", g.args(map[string]any{"explanations": shareClaim(conv, "0.8", "conv is {c1}")}), &out)
	if out.Publication != "p2" || !slices.Equal(out.Notes, gop.Notes) {
		t.Errorf("GPU page_explain = %s, want notes %v", text, gop.Notes)
	}
	text, isErr := c.call("page_explain", g.args(map[string]any{"explanations": shareClaim(conv, "0.7", "conv is {c1}")}))
	if !isErr || !strings.Contains(text, `"notes":`+mustJSON(t, gop.Notes)) {
		t.Errorf("a GPU rejection: isError=%v %s", isErr, text)
	}
}

// The page's pill counts figures and references; the result names both,
// so it says what the pill says.
func TestPageExplainCountsTheReferencesItChecked(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	b, cc, a := o.nodeID(t, "b"), o.nodeID(t, "c"), o.nodeID(t, "a")
	exps := goldenExplanations(b, cc)
	exps[0].(map[string]any)["hypotheses"] = []any{"{n:" + a + "} calls {n:" + b + "}"}
	var out pageExplainOut
	text := c.callOK("page_explain", o.args(map[string]any{"explanations": exps}), &out)
	if out.FiguresChecked != "2" || out.ReferencesChecked != "3" {
		t.Errorf("figures_checked %q and references_checked %q, want 2 and 3: %s", out.FiguresChecked, out.ReferencesChecked, text)
	}
	if model := pageGet(t, c, "pub/"+out.Publication+"/model"); !strings.Contains(model, `"checked_pill":"5 figures and references checked"`) {
		t.Errorf("the pill does not count both: %.400s", model)
	}
	// render_flamegraph's result does not change.
	if text, isErr := c.call("render_flamegraph", o.args(map[string]any{"explanations": exps, "out": "r.html"})); isErr || strings.Contains(text, "references_checked") {
		t.Errorf("render_flamegraph: isError=%v %s", isErr, text)
	}
}

// sameRejection is text with its attempt IDs and the tool's own words
// replaced, so a page_explain rejection and a render_flamegraph one can be
// compared.
func sameRejection(text string, ids ...string) string {
	text = strings.ReplaceAll(text, "page_explain", "render_flamegraph")
	text = strings.ReplaceAll(text, "nothing was published", "nothing was written")
	for i, id := range ids {
		text = regexp.MustCompile(`\b`+id+`\b`).ReplaceAllString(text, fmt.Sprintf("attempt-%d", i))
	}
	return text
}

func TestPageExplainRejectsExactlyLikeRender(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	pageOpen(t, c)
	before := pageState(t, c)
	exps := shareClaim(o.nodeID(t, "b"), "0.62", "b takes {c1}")
	page, pageErr := c.call("page_explain", o.args(map[string]any{"explanations": exps, "rounding": "0.1"}))
	render, renderErr := c.call("render_flamegraph", o.args(map[string]any{"explanations": exps, "rounding": "0.1", "out": "r.html"}))
	if !pageErr || !renderErr {
		t.Fatalf("isError: page_explain %v, render_flamegraph %v", pageErr, renderErr)
	}
	if !strings.Contains(page, `"status":"rejected","written":false`) || !strings.Contains(page, "call page_explain again") ||
		!strings.Contains(page, "nothing was published") {
		t.Errorf("page_explain rejection %s", page)
	}
	if got, want := sameRejection(page, "a1"), sameRejection(render, "a2"); got != want {
		t.Errorf("the rejections differ:\npage_explain      %s\nrender_flamegraph %s", got, want)
	}
	if after := pageState(t, c); after.Current != before.Current || len(after.Publications) != 0 {
		t.Errorf("a rejection changed the page: %+v", after)
	}

	// Every refusal before the check names page_explain as what to call.
	for name, args := range map[string]map[string]any{
		"title":   {"explanations": []any{}, "title": "b is {c1}"},
		"shape":   {"explanations": "nope"},
		"attempt": {"explanations": []any{}, "attempt": "a99"},
	} {
		text, isErr := c.call("page_explain", o.args(args))
		if !isErr || strings.Contains(text, "render_flamegraph again") || (name != "attempt" && !strings.Contains(text, "page_explain again")) {
			t.Errorf("%s: isError=%v %s", name, isErr, text)
		}
		if name == "attempt" && !strings.Contains(text, "render_flamegraph or page_explain") {
			t.Errorf("unknown attempt: %s", text)
		}
	}
	if st := pageState(t, c); len(st.Publications) != 0 {
		t.Errorf("a refused call published %+v", st.Publications)
	}
}

func TestPageExplainSharesTheAttemptRecord(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic(), "gpu.pb.gz": pmfixture.GPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	b := o.nodeID(t, "b")
	if text, isErr := c.call("render_flamegraph", o.args(map[string]any{"explanations": shareClaim(b, "0.62", "b takes {c1}"), "out": "r.html"})); !isErr || !strings.Contains(text, `"attempt":"a1"`) {
		t.Fatalf("render_flamegraph: isError=%v %s", isErr, text)
	}
	var out pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": shareClaim(b, "0.411", "b takes {c1}"), "attempt": "a1"}), &out)
	if out.Attempt != "a2" || out.Publication != "p1" {
		t.Errorf("the retry %+v", out)
	}
	history := pageGet(t, c, "pub/p1/history")
	if !strings.Contains(history, `"a1"`) || !strings.Contains(history, "1 claim rejected") {
		t.Errorf("p1's history does not show a1: %s", history)
	}

	// An attempt of another profile's view is refused.
	g, _ := openOn(t, c, "gpu.pb.gz", "")
	text, isErr := c.call("page_explain", g.args(map[string]any{"explanations": []any{}, "attempt": "a1"}))
	if !isErr || !strings.Contains(text, "attempt a1 was made for view") {
		t.Errorf("an attempt of another view: isError=%v %s", isErr, text)
	}
}

func TestLiveHistoryFollowsNewRejections(t *testing.T) {
	var log stateLog
	c := pageClient(t, func(c *Config) { c.Page.Notify = log.notify }, map[string][]byte{"gpu.pb.gz": pmfixture.GPUBasic(), "gpu.execlog": pmfixture.GPUBasicExeclog(t)})
	bare, _ := openOn(t, c, "gpu.pb.gz", "")
	conv := bare.nodeID(t, "conv")
	var out pageExplainOut
	c.callOK("page_explain", bare.args(map[string]any{"explanations": shareClaim(conv, "0.8", "conv is {c1}")}), &out)
	st := pageState(t, c)
	if len(st.Publications) != 1 || st.Publications[0].History != "1" {
		t.Fatalf("state %+v", st)
	}
	// historyNotified reports whether a state notified since the first n
	// lists p1 alone at history version.
	historyNotified := func(n int, version string) bool {
		for _, raw := range log.all()[n:] {
			st := envelopeOf(t, raw).Engine
			if len(st.Publications) == 1 && st.Publications[0].History == version {
				return true
			}
		}
		return false
	}
	seen := len(log.all())

	withLog, wop := openOn(t, c, "gpu.pb.gz", "gpu.execlog")
	if wop.Handle == bare.handle {
		t.Fatal("the execlog handle is the bare one")
	}
	if text, isErr := c.call("render_flamegraph", withLog.args(map[string]any{"explanations": shareClaim(conv, "0.62", "conv is {c1}"), "out": "r.html"})); !isErr {
		t.Fatalf("render_flamegraph: %s", text)
	}
	if !historyNotified(seen, "2") {
		t.Fatal("no state with p1's history at version 2 was notified")
	}
	seen = len(log.all())
	history := pageGet(t, c, "pub/p1/history")
	if !strings.Contains(history, "made with handle "+wop.Handle[:12]) {
		t.Errorf("the row is not labelled with the other handle: %s", history)
	}

	// A rejection through page_explain reaches it too.
	if text, isErr := c.call("page_explain", withLog.args(map[string]any{"explanations": shareClaim(conv, "0.61", "conv is {c1}")})); !isErr {
		t.Fatalf("page_explain: %s", text)
	}
	if !historyNotified(seen, "3") {
		t.Error("no state with p1's history at version 3 was notified")
	}
}

// A publication's history follows the attempt record, so it lists an
// attempt made after the publication too; the history and the export say
// so instead of calling the rejections earlier attempts.
func TestALaterRejectionIsNotCalledEarlier(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	b := o.nodeID(t, "b")
	var out pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": shareClaim(b, "0.411", "b takes {c1}")}), &out)
	if text, isErr := c.call("page_explain", o.args(map[string]any{"explanations": shareClaim(b, "0.62", "b takes {c1}")})); !isErr {
		t.Fatalf("the later rejection: %s", text)
	}
	history := pageGet(t, c, "pub/"+out.Publication+"/history")
	if !strings.Contains(history, `"a2"`) || !strings.Contains(history, `"rejected_pill":"1 claim rejected"`) {
		t.Errorf("the history of %s: %s", out.Publication, history)
	}
	page := pageGet(t, c, "pub/"+out.Publication+"/export")
	for _, want := range []string{
		"<h5>Rejected attempts on this profile and view</h5>",
		"before or after this publication (attempt " + out.Attempt + ").",
		`<span class="pill bad">1 claim rejected</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the export lacks %q", want)
		}
	}
	for _, wrong := range []string{"at render", "earlier attempts", "the attempt that passed"} {
		if strings.Contains(history+page, wrong) {
			t.Errorf("the history or export says %q", wrong)
		}
	}
}

func TestAFactsOnlyPageStillShowsRejections(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	if text, isErr := c.call("page_explain", o.args(map[string]any{"explanations": shareClaim(o.nodeID(t, "b"), "0.62", "b takes {c1}")})); !isErr {
		t.Fatalf("the rejection: %s", text)
	}
	var out pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &out)
	if out.Status != "published" || out.FiguresChecked != "0" {
		t.Errorf("facts-only %+v", out)
	}
	if st := pageState(t, c); st.Current != out.Publication || st.Selected != "" {
		t.Errorf("state %+v", st)
	}
	if history := pageGet(t, c, "pub/"+out.Publication+"/history"); !strings.Contains(history, "1 claim rejected") {
		t.Errorf("the facts-only page hides the rejection: %s", history)
	}
}

func TestPageShowSelectsAndFocuses(t *testing.T) {
	c := pageClient(t, func(c *Config) { c.Limits.MaxPageNodes = 1 }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	b, cn := o.nodeID(t, "b"), o.nodeID(t, "c")
	var pub pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": shareClaim(b, "0.411", "b takes {c1}")}), &pub)
	var model liveModelJSON
	if err := json.Unmarshal([]byte(pageGet(t, c, "pub/p1/model")), &model); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(model.drawnIDs(t), cn) {
		t.Fatal("c is drawn with MaxPageNodes=1")
	}

	var out pageShowOut
	text := c.callOK("page_show", map[string]any{"node": cn}, &out)
	if out.Publication != "p1" || out.Selected != cn || out.Focus != "" || out.URL != pub.URL || out.Viewers != "0" {
		t.Errorf("page_show node = %s", text)
	}
	if st := pageState(t, c); st.Current != "p1" || st.Selected != cn || st.Focus != "" {
		t.Errorf("state after selecting c: %+v", st)
	}
	c.callOK("page_show", map[string]any{"focus": true}, &out)
	if st := pageState(t, c); out.Focus != cn || st.Selected != cn || st.Focus != cn {
		t.Errorf("focus on the selection: result %+v, state %+v", out, st)
	}
	c.callOK("page_show", map[string]any{"focus": false}, &out)
	if st := pageState(t, c); st.Selected != cn || st.Focus != "" {
		t.Errorf("unfocused: state %+v", st)
	}

	var facts pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &facts)
	if facts.Publication != "p2" {
		t.Fatalf("facts-only %+v", facts)
	}
	// p1 was left with c selected; coming back to it selects its first
	// explained node, b.
	c.callOK("page_show", map[string]any{"publication": "p1"}, &out)
	if st := pageState(t, c); st.Current != "p1" || st.Selected != b || st.Focus != "" || out.Selected != b {
		t.Errorf("back to p1: result %+v, state %+v", out, st)
	}
	// Steering the current publication without node keeps its selection.
	c.callOK("page_show", map[string]any{"node": cn}, &out)
	c.callOK("page_show", map[string]any{"publication": "p1"}, &out)
	if out.Selected != cn {
		t.Errorf("p1 again while current: selected %q, want c kept", out.Selected)
	}
	c.callOK("page_show", map[string]any{"publication": "p2", "node": cn, "focus": true}, &out)
	if st := pageState(t, c); st.Current != "p2" || st.Selected != cn || st.Focus != cn {
		t.Errorf("p2 focused on c: state %+v", st)
	}
	// focus on a facts-only page with nothing selected names what to pass.
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &facts)
	if text, isErr := c.call("page_show", map[string]any{"focus": true}); !isErr || !strings.Contains(text, "node") {
		t.Errorf("focus with nothing selected: isError=%v %s", isErr, text)
	}
}

func TestPageShowRefusesWhatIsNotThere(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic(), "gpu.pb.gz": pmfixture.GPUBasic()})
	if text, isErr := c.call("page_show", map[string]any{}); !isErr || !strings.Contains(text, "pass publication, node or both") {
		t.Errorf("{}: isError=%v %s", isErr, text)
	}
	if text, isErr := c.call("page_show", map[string]any{"node": "x"}); !isErr || !strings.Contains(text, "page_explain") {
		t.Errorf("nothing published: isError=%v %s", isErr, text)
	}
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	var pub pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &pub)
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &pub)
	before := pageState(t, c)

	text, isErr := c.call("page_show", map[string]any{"publication": "p9"})
	if !isErr || !strings.Contains(text, `publication "p9" is not kept; the kept ones are: p1, p2; call page_explain or page_diff to publish again`) {
		t.Errorf("unknown publication: isError=%v %s", isErr, text)
	}
	g, _ := openOn(t, c, "gpu.pb.gz", "")
	conv := g.nodeID(t, "conv")
	text, isErr = c.call("page_show", map[string]any{"publication": "p1", "node": conv})
	if !isErr || !strings.Contains(text, fmt.Sprintf("node %q is not in view %s of publication p1; find node IDs with top or path", conv, o.view)) {
		t.Errorf("a node of another view: isError=%v %s", isErr, text)
	}
	huge := strings.Repeat("z", 4<<10)
	for _, args := range []map[string]any{{"node": huge}, {"publication": huge}} {
		text, isErr = c.call("page_show", args)
		if !isErr || len(text) > 1024 || strings.Contains(text, strings.Repeat("z", 100)) {
			t.Errorf("a 4 KiB name: isError=%v %d bytes %.200s", isErr, len(text), text)
		}
	}
	if after := pageState(t, c); after.Seq != before.Seq {
		t.Errorf("a refused page_show changed the state: %+v, then %+v", before, after)
	}
}

// The page tools' viewers is what PageConfig.Viewers counts at the call.
func TestViewersCountsOpenStreams(t *testing.T) {
	var streams atomic.Int64
	c := pageClient(t, func(c *Config) { c.Page.Viewers = func() int { return int(streams.Load()) } },
		map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	var pub pageExplainOut
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &pub)
	if pub.Viewers != "0" {
		t.Errorf("viewers %q with no stream open", pub.Viewers)
	}
	streams.Store(2)
	var out pageShowOut
	c.callOK("page_show", map[string]any{"publication": "p1"}, &out)
	if out.Viewers != "2" {
		t.Errorf("viewers %q with two streams open", out.Viewers)
	}
	if got := pageOpen(t, c).Viewers; got != "2" {
		t.Errorf("page_open viewers %q", got)
	}
}

func TestPageShowCarriesGPUNotes(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"gpu.pb.gz": pmfixture.GPUBasic(), "cpu.pb.gz": pmfixture.CPUBasic()})
	g, gop := openOn(t, c, "gpu.pb.gz", "")
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	var pub pageExplainOut
	c.callOK("page_explain", g.args(map[string]any{"explanations": []any{}}), &pub)
	c.callOK("page_explain", o.args(map[string]any{"explanations": []any{}}), &pub)
	var out pageShowOut
	c.callOK("page_show", map[string]any{"publication": "p1"}, &out)
	if !slices.Equal(out.Notes, gop.Notes) || len(out.Notes) == 0 {
		t.Errorf("page_show of a GPU publication: notes %v, want %v", out.Notes, gop.Notes)
	}
	if text := c.callOK("page_show", map[string]any{"publication": "p2"}, nil); strings.Contains(text, `"notes"`) {
		t.Errorf("page_show of a CPU publication has notes: %s", text)
	}
}

// Every page tool decodes its arguments as every other tool does: names
// exactly as sent, each once, and none it does not take.
func TestPageToolsDecodeStrictly(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	h, v := o.handle, o.view
	for name, call := range map[string]struct{ tool, raw, want string }{
		"page_open argument":       {"page_open", `{"x": 1}`, "invalid arguments to page_open: unknown argument \"x\" (names are case-sensitive); it takes no arguments"},
		"page_explain case-folded": {"page_explain", fmt.Sprintf(`{"handle": %q, "view": %q, "Explanations": []}`, h, v), `unknown argument "Explanations"`},
		"page_explain duplicate":   {"page_explain", fmt.Sprintf(`{"handle": %q, "view": %q, "explanations": [], "title": "x", "title": "y"}`, h, v), "twice"},
		"page_explain out":         {"page_explain", fmt.Sprintf(`{"handle": %q, "view": %q, "explanations": [], "out": "a.html"}`, h, v), `unknown argument "out"`},
		"page_show case-folded":    {"page_show", `{"Node": "x"}`, `unknown argument "Node"`},
		"page_show duplicate":      {"page_show", `{"node": "x", "node": "y"}`, "twice"},
		"page_show unknown":        {"page_show", `{"view": "x"}`, `unknown argument "view"`},
		"page_show focus type":     {"page_show", `{"focus": "yes"}`, "invalid arguments to page_show"},
		"page_diff case-folded":    {"page_diff", fmt.Sprintf(`{"base_handle": %q, "base_view": %q, "new_handle": %q, "New_View": %q}`, h, v, h, v), `unknown argument "New_View"`},
		"page_diff duplicate":      {"page_diff", fmt.Sprintf(`{"base_handle": %q, "base_view": %q, "new_handle": %q, "new_view": %q, "node": "a", "node": "b"}`, h, v, h, v), "twice"},
		"page_diff limit":          {"page_diff", fmt.Sprintf(`{"base_handle": %q, "base_view": %q, "new_handle": %q, "new_view": %q, "limit": 2}`, h, v, h, v), `unknown argument "limit"`},
		"page_diff cursor":         {"page_diff", fmt.Sprintf(`{"base_handle": %q, "base_view": %q, "new_handle": %q, "new_view": %q, "cursor": "c1.0.x"}`, h, v, h, v), `unknown argument "cursor"`},
	} {
		text, isErr := c.call(call.tool, json.RawMessage(call.raw))
		if !isErr || !strings.Contains(text, call.want) || !strings.Contains(text, call.tool) {
			t.Errorf("%s: isError=%v %s", name, isErr, text)
		}
	}
	if c.srv.attempts.next != 0 {
		t.Errorf("refused arguments recorded %d attempts", c.srv.attempts.next)
	}
}

// page_explain takes render_flamegraph's arguments but out, described the
// same way.
func TestPageExplainTakesRendersArgumentsButOut(t *testing.T) {
	s := newLiveServer(t, nil, nil)
	props := func(schema json.RawMessage) (map[string]json.RawMessage, []string) {
		var v struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(schema, &v); err != nil {
			t.Fatal(err)
		}
		return v.Properties, v.Required
	}
	page, pageReq := props(s.pageExplainTool().InputSchema)
	render, renderReq := props(s.renderTool().InputSchema)
	delete(render, "out")
	renderReq = slices.DeleteFunc(renderReq, func(n string) bool { return n == "out" })
	if !reflect.DeepEqual(page, render) || !slices.Equal(pageReq, renderReq) {
		t.Errorf("page_explain's schema is not render_flamegraph's without out:\n%v %v\n%v %v", page, pageReq, render, renderReq)
	}
	if got, want := slices.Sorted(slices.Values(argNames(&pageExplainArgs{}))), slices.Sorted(maps.Keys(page)); !slices.Equal(got, want) {
		t.Errorf("page_explain decodes %v, its schema has %v", got, want)
	}
}

// gluedPlaceholder finds a claim placeholder followed by anything but the
// end, a space or closing punctuation: the form explain.Check refuses.
func gluedPlaceholder(text string) string {
	for _, m := range regexp.MustCompile(`\{c[0-9]+\}`).FindAllStringIndex(text, -1) {
		next, _ := utf8.DecodeRuneInString(text[m[1]:])
		if m[1] == len(text) || unicode.IsSpace(next) || strings.ContainsRune(".,;:!?…", next) ||
			unicode.Is(unicode.Pe, next) || unicode.Is(unicode.Pf, next) {
			continue
		}
		return text[m[0]:min(len(text), m[1]+8)]
	}
	return ""
}

// The page tools' copy never shows a placeholder in a form the checker
// refuses.
func TestPageCopyHasNoGluedPlaceholder(t *testing.T) {
	for _, live := range []bool{false, true} {
		cfg := testConfig(t)
		if live {
			cfg.Page = livePage()
		}
		s := must(New(cfg))(t)
		t.Cleanup(func() { _ = s.Close() })
		for _, tl := range []mcpstdio.Tool{s.pageOpenTool(), s.pageExplainTool(), s.pageShowTool(), s.pageDiffTool()} {
			for _, text := range []string{tl.Title, tl.Description, string(tl.InputSchema)} {
				if g := gluedPlaceholder(text); g != "" {
					t.Errorf("%s shows %q", tl.Name, g)
				}
			}
		}
	}
	skill := string(readFile(t, "../../skills/perf-agent-mcp/SKILL.md"))
	for line := range strings.SplitSeq(skill, "\n") {
		if strings.Contains(line, "page_") {
			if g := gluedPlaceholder(line); g != "" {
				t.Errorf("the skill's live page line shows %q", g)
			}
		}
	}
	if g := gluedPlaceholder("b is {c1}s"); g == "" {
		t.Error("the check misses a glued placeholder")
	}
}

// Explanations that pass but whose page cannot be built within MaxPageBytes
// publish nothing: the attempt is recorded as failed, the advice names the
// limit, and the page does not change.
func TestPageExplainThatCannotBePublishedSaysWhy(t *testing.T) {
	c := pageClient(t, func(c *Config) { c.Limits.MaxPageBytes = 2048 }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	pageOpen(t, c)
	before := pageState(t, c)
	text, isErr := c.call("page_explain", o.args(map[string]any{"explanations": shareClaim(o.nodeID(t, "b"), "0.411", "b takes {c1}")}))
	want := "attempt a1: the explanations passed, but the page cannot be built within this server's MaxPageBytes=2048: "
	if !isErr || !strings.HasPrefix(text, want) ||
		!strings.HasSuffix(text, "; explain fewer or shorter paths, or restart perf-agent-mcp with a larger --limit MaxPageBytes=N; nothing was published") {
		t.Errorf("isError=%v %s", isErr, text)
	}
	if at := c.srv.attempts.byID["a1"]; at == nil || at.outcome != outcomeFailed {
		t.Errorf("attempt a1 %+v, want outcome failed", at)
	}
	if after := pageState(t, c); after.Seq != before.Seq || len(after.Publications) != 0 {
		t.Errorf("the page changed: %+v, then %+v", before, after)
	}
}

// A claim shows its own unit, so a unit written right after it shows
// twice. Both tools say so in a notice, and still publish or write.
func TestARepeatedUnitIsANotice(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o, _ := openOn(t, c, "cpu.pb.gz", "")
	b := o.nodeID(t, "b")
	exps := func(prose, hypothesis string) []any {
		return []any{map[string]any{
			"node": b,
			"claims": []any{
				map[string]any{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": "0.411"},
				map[string]any{"id": "c2", "metric": "cumulative", "unit": "nanoseconds", "value": "411"},
			},
			"prose":      prose,
			"hypotheses": []any{hypothesis},
		}}
	}
	type notice struct {
		Path    string `json:"path"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	var out struct {
		Status  string   `json:"status"`
		Notices []notice `json:"notices"`
	}
	repeated := func(ns []notice) []string {
		var paths []string
		for _, n := range ns {
			if n.Code == "unit_repeated" {
				paths = append(paths, n.Path)
			}
		}
		return paths
	}
	i := 0
	for _, tc := range []struct {
		prose, hypothesis string
		want              []string
	}{
		{"b runs {c2} ns, {c1} of it", "b waits", []string{"explanations[0].prose"}},
		{"b runs {c2}\tns.", "{c1} % is b", []string{"explanations[0].prose", "explanations[0].hypotheses[0]"}},
		{"b runs {c2} nanoseconds of CPU", "{c1} of the view", nil},
		{"b runs {c2}, {c1} percent", "b nsfoo {c2} nsec", nil},
	} {
		i++
		c.callOK("page_explain", o.args(map[string]any{"explanations": exps(tc.prose, tc.hypothesis)}), &out)
		if out.Status != "published" || !slices.Equal(repeated(out.Notices), tc.want) {
			t.Errorf("page_explain %q / %q: %s, unit notices at %q, want %q", tc.prose, tc.hypothesis, out.Status, repeated(out.Notices), tc.want)
		}
		c.callOK("render_flamegraph", o.args(map[string]any{"explanations": exps(tc.prose, tc.hypothesis), "out": fmt.Sprintf("r%d.html", i)}), &out)
		if out.Status != "written" || !slices.Equal(repeated(out.Notices), tc.want) {
			t.Errorf("render_flamegraph %q / %q: %s, unit notices at %q, want %q", tc.prose, tc.hypothesis, out.Status, repeated(out.Notices), tc.want)
		}
	}
	c.callOK("page_explain", o.args(map[string]any{"explanations": exps("b runs {c2} ns", "{c1} of it")}), &out)
	if len(out.Notices) != 1 || !strings.Contains(out.Notices[0].Message, `"411 ns"`) {
		t.Errorf("the notice %+v does not show the page's value", out.Notices)
	}
}

// A measured zero is shown as "0", with no unit, so a unit after its
// placeholder repeats nothing; a share that rounds to zero is "0.0%" and
// still does.
func TestAMeasuredZeroRepeatsNoUnit(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"gpu.pb.gz": pmfixture.GPUBasic()})
	o, _ := openOn(t, c, "gpu.pb.gz", "")
	node := o.nodeID(t, "conv")
	exps := []any{map[string]any{"node": node, "prose": "Unknown serialization is {c1} % of the node", "claims": []any{map[string]any{
		"id": "c1", "metric": "quality_serialized_unknown", "denominator": "node:" + node, "unit": "ratio", "value": "0",
	}}}}
	for _, tool := range []string{"page_explain", "render_flamegraph"} {
		args := o.args(map[string]any{"explanations": exps})
		if tool == "render_flamegraph" {
			args["out"] = "zero.html"
		}
		var out struct {
			Notices []explain.Notice `json:"notices"`
		}
		c.callOK(tool, args, &out)
		if len(out.Notices) != 0 {
			t.Errorf("%s: a measured zero, shown as 0, got notices %+v", tool, out.Notices)
		}
	}

	rounded := explain.EngineValue{Unit: "ratio", Value: "0.000", Status: "ok"}
	r := explain.Result{OK: true, Checked: []explain.Checked{{
		Claims: []explain.CheckedClaim{{ID: "c1", EngineValue: rounded}},
		Prose:  []explain.Segment{{Kind: explain.SegmentClaim, Claim: "c1"}, {Kind: explain.SegmentText, Text: " % of the view"}},
	}}}
	if ns := unitNotices(r); len(ns) != 1 || ns[0].Code != explain.NoticeUnitRepeated {
		t.Errorf("a share shown as 0.0%% followed by %%: notices %+v", ns)
	}
}
