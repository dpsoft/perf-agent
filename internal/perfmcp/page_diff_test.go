package perfmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

type pageDiffOut struct {
	Publication     string   `json:"publication"`
	URL             string   `json:"url"`
	BaseTotal       string   `json:"base_total"`
	NewTotal        string   `json:"new_total"`
	QualityWarnings []string `json:"quality_warnings"`
	BaseNotes       []string `json:"base_notes"`
	NewNotes        []string `json:"new_notes"`
	Viewers         string   `json:"viewers"`
}

// livePair is a live stdio server with base and new open.
type livePair struct {
	diffPair
}

func openLivePair(t *testing.T, edit func(*Config), base, next []byte) livePair {
	t.Helper()
	c := pageClient(t, edit, map[string][]byte{"base.pb.gz": base, "new.pb.gz": next})
	b, _ := openOn(t, c, "base.pb.gz", "")
	n, _ := openOn(t, c, "new.pb.gz", "")
	return livePair{diffPair{c: c, base: b, new: n}}
}

// unionID is the union ID diff gives the node named name.
func (p livePair) unionID(t *testing.T, name string) string {
	t.Helper()
	out, _ := p.diff(t, map[string]any{"limit": 500})
	for _, r := range out.Rows {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("diff has no node %q", name)
	return ""
}

func (p livePair) pageDiff(t *testing.T, extra map[string]any) (pageDiffOut, string) {
	t.Helper()
	var out pageDiffOut
	text := p.c.callOK("page_diff", p.args(extra), &out)
	return out, text
}

// diffOfFixtures is the comparison of two fixtures' default views, made
// apart from any server.
func diffOfFixtures(t *testing.T, base, next []byte) *profilemodel.Diff {
	t.Helper()
	view := func(raw []byte) *profilemodel.View {
		p := must(profilemodel.FromBytes(raw, nil, profilemodel.DefaultLimits()))(t)
		return must(p.View(profilemodel.ViewOptions{SampleIndex: -1}))(t)
	}
	return must(profilemodel.Compare(view(base), view(next), profilemodel.WorkCounts{}))(t)
}

// diffModelJSON is the part of a diff's live model these tests read.
type diffModelJSON struct {
	Kind      string `json:"kind"`
	TitleSegs []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"title_segs"`
	Drawn []struct {
		I string `json:"i"`
	} `json:"drawn"`
	Frames []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"frames"`
	Export struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	} `json:"export"`
	SizeNote string `json:"size_note"`
}

func diffModelOf(t *testing.T, raw []byte) diffModelJSON {
	t.Helper()
	var m diffModelJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("model %.300s: %v", raw, err)
	}
	return m
}

func (m diffModelJSON) frameIDs() []string {
	out := make([]string, len(m.Frames))
	for i, f := range m.Frames {
		out[i] = f.ID
	}
	return out
}

func TestPageDiffPublishesTheComparison(t *testing.T) {
	p := openLivePair(t, nil, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	d, _ := p.diff(t, nil)
	var full struct {
		QualityWarnings []string `json:"quality_warnings"`
	}
	p.c.callOK("diff", p.args(nil), &full)
	out, text := p.pageDiff(t, nil)
	if out.Publication != "p1" || out.BaseTotal != d.BaseTotal || out.NewTotal != d.NewTotal || out.Viewers != "0" {
		t.Errorf("page_diff %s; diff totals %s, %s", text, d.BaseTotal, d.NewTotal)
	}
	if out.QualityWarnings == nil || !slices.Equal(out.QualityWarnings, full.QualityWarnings) {
		t.Errorf("quality_warnings %v, diff's %v", out.QualityWarnings, full.QualityWarnings)
	}
	if strings.Contains(text, "notes") {
		t.Errorf("a CPU diff carries notes: %s", text)
	}
	if out.URL != pageOpen(t, p.c).URL {
		t.Errorf("url %q is not the page's", out.URL)
	}
	st := pageState(t, out.URL)
	if st.Current != "p1" || st.Selected != "" || st.Focus != "" || len(st.Publications) != 1 {
		t.Fatalf("state %+v", st)
	}
	if e := st.Publications[0]; e.Kind != "diff" || e.View != p.base.view || e.History != "0" {
		t.Errorf("state entry %+v", e)
	}
	if m := diffModelOf(t, []byte(pageGet(t, out.URL, "pub/p1/model"))); m.Kind != "diff" || len(m.Drawn) == 0 {
		t.Errorf("model kind %q with %d frames drawn", m.Kind, len(m.Drawn))
	}
	if got, want := pageGet(t, out.URL, "pub/p1/history"), `{"publication":"p1","version":"0","rows":[]}`; got != want {
		t.Errorf("history %s, want %s", got, want)
	}
	if p.c.srv.attempts.next != 0 {
		t.Errorf("page_diff recorded %d attempts; a diff carries no claims", p.c.srv.attempts.next)
	}
	if text, isErr := p.c.call("page_diff", p.args(map[string]any{"work_base": 10, "work_new": 20})); isErr || !strings.Contains(text, `"publication":"p2"`) {
		t.Errorf("with work counts: isError=%v %s", isErr, text)
	}
}

func TestPageDiffCarriesGPUNotes(t *testing.T) {
	p := openLivePair(t, nil, pmfixture.GPUBasic(), pmfixture.GPUBasic())
	d, _ := p.diff(t, nil)
	out, text := p.pageDiff(t, nil)
	if len(out.BaseNotes) == 0 || !slices.Equal(out.BaseNotes, d.BaseNotes) || !slices.Equal(out.NewNotes, d.NewNotes) {
		t.Errorf("page_diff of GPU views %s; diff's notes %v, %v", text, d.BaseNotes, d.NewNotes)
	}
	var shown pageShowDiffOut
	p.c.callOK("page_show", map[string]any{"publication": "p1"}, &shown)
	if len(shown.BaseNotes) == 0 || !slices.Equal(shown.BaseNotes, d.BaseNotes) || !slices.Equal(shown.NewNotes, d.NewNotes) || shown.Notes != nil {
		t.Errorf("page_show of a GPU diff: %+v", shown)
	}
}

type pageShowDiffOut struct {
	pageShowOut
	BaseNotes []string `json:"base_notes"`
	NewNotes  []string `json:"new_notes"`
}

func TestPageDiffRefusesWhatDiffRefuses(t *testing.T) {
	c := pageClient(t, nil, map[string][]byte{"base.pb.gz": pmfixture.CPUBasic(), "gpu.pb.gz": pmfixture.GPUBasic()})
	b, _ := openOn(t, c, "base.pb.gz", "")
	g, _ := openOn(t, c, "gpu.pb.gz", "")
	before := pageState(t, pageOpen(t, c).URL)
	pair := map[string]any{"base_handle": b.handle, "base_view": b.view, "new_handle": g.handle, "new_view": g.view}
	with := func(extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range pair {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, args := range map[string]map[string]any{
		"incompatible views": with(nil),
		"unknown base":       with(map[string]any{"base_handle": "h-nope"}),
		"unknown new":        with(map[string]any{"new_handle": "h-nope"}),
		"unknown view":       with(map[string]any{"new_handle": b.handle, "new_view": "cpu/nanoseconds#7"}),
		"bad work count":     with(map[string]any{"new_handle": b.handle, "new_view": b.view, "work_base": -1}),
	} {
		want, isErr := c.call("diff", args)
		if !isErr {
			t.Fatalf("%s: diff accepted it: %s", name, want)
		}
		got, isErr := c.call("page_diff", args)
		if !isErr || got != want {
			t.Errorf("%s: page_diff isError=%v %q, diff %q", name, isErr, got, want)
		}
	}
	if text, isErr := c.call("page_diff", with(map[string]any{"limit": 2})); !isErr || !strings.Contains(text, `unknown argument "limit"`) {
		t.Errorf("limit: isError=%v %s", isErr, text)
	}
	if after := pageState(t, pageOpen(t, c).URL); after.Seq != before.Seq || len(after.Publications) != 0 {
		t.Errorf("a refused page_diff changed the page: %+v, then %+v", before, after)
	}
}

func TestPageDiffChecksItsTitle(t *testing.T) {
	p := openLivePair(t, func(c *Config) { c.Limits.Explain.MaxTitleBytes = 64 }, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	for name, c := range map[string]struct{ title, want string }{
		"placeholder": {"b takes {c1}", "title contains the placeholder {c1}"},
		"node ref":    {"see {n:0123456789abcdef}", "title contains the placeholder"},
		"too long":    {strings.Repeat("t", 65), "over the server's MaxTitleBytes of 64: shorten it to a few words (put figures in an explanation's claims), or start the server with a higher MaxTitleBytes (--limit Explain.MaxTitleBytes=N)"},
	} {
		text, isErr := p.c.call("page_diff", p.args(map[string]any{"title": c.title}))
		if !isErr || !strings.Contains(text, c.want) || !strings.Contains(text, "page_diff") {
			t.Errorf("%s: isError=%v %s", name, isErr, text)
		}
	}
	if st := pageState(t, pageOpen(t, p.c).URL); len(st.Publications) != 0 {
		t.Errorf("a refused title published: %+v", st)
	}
	out, _ := p.pageDiff(t, map[string]any{"title": "2 builds of app"})
	st := pageState(t, out.URL)
	var segs []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(must(json.Marshal(st.Publications[0].TitleSegs))(t), &segs); err != nil {
		t.Fatalf("title segments %s: %v", st.Publications[0].TitleSegs, err)
	}
	m := diffModelOf(t, []byte(pageGet(t, out.URL, "pub/p1/model")))
	for what, ss := range map[string][]struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}{"state": segs, "model": m.TitleSegs} {
		if !slices.ContainsFunc(ss, func(s struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		}) bool {
			return s.Kind == "unchecked" && s.Text == "2"
		}) {
			t.Errorf("the %s's title does not mark 2 unchecked: %+v", what, ss)
		}
	}
}

func TestPageDiffSelectsAUnionNode(t *testing.T) {
	p := openLivePair(t, nil, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	b, d := p.unionID(t, "b"), p.unionID(t, "d")
	out, text := p.pageDiff(t, map[string]any{"node": b})
	if st := pageState(t, out.URL); st.Current != "p1" || st.Selected != b || st.Focus != "" {
		t.Errorf("page_diff node b: %s; state %+v", text, st)
	}
	plain := p.base.nodeID(t, "b")
	for name, node := range map[string]string{"a plain node ID": plain, "empty": "", "unknown": "0000000000000000/-", "huge": strings.Repeat("z", 4<<10)} {
		text, isErr := p.c.call("page_diff", p.args(map[string]any{"node": node}))
		if !isErr || !strings.Contains(text, "union ID") || !strings.Contains(text, "diff") || len(text) > 1024 {
			t.Errorf("%s: isError=%v %.300s", name, isErr, text)
		}
	}
	if st := pageState(t, out.URL); len(st.Publications) != 1 {
		t.Errorf("a refused node published: %+v", st)
	}

	// page_show checks a diff's nodes against the whole union.
	var shown pageShowOut
	p.c.callOK("page_show", map[string]any{"node": d, "focus": true}, &shown)
	if st := pageState(t, out.URL); st.Selected != d || st.Focus != d {
		t.Errorf("page_show d focused: %+v", st)
	}
	if text, isErr := p.c.call("page_show", map[string]any{"node": plain}); !isErr || !strings.Contains(text, "union ID") || !strings.Contains(text, "diff") {
		t.Errorf("page_show with a plain node ID: isError=%v %s", isErr, text)
	}
	// Back to the diff from another publication, page_diff's node is
	// selected again.
	var pub pageExplainOut
	p.c.callOK("page_explain", p.base.args(map[string]any{"explanations": []any{}}), &pub)
	p.c.callOK("page_show", map[string]any{"publication": "p1"}, &shown)
	if shown.Selected != b || shown.Focus != "" {
		t.Errorf("back to the diff: %+v", shown)
	}
	// A diff published without node selects nothing: the page shows its
	// root.
	p.pageDiff(t, nil)
	p.c.callOK("page_show", map[string]any{"publication": "p1"}, &shown)
	p.c.callOK("page_show", map[string]any{"publication": "p3"}, &shown)
	if shown.Selected != "" {
		t.Errorf("a diff without node selected %q", shown.Selected)
	}
}

func TestPageDiffExportIsTheM1DiffPage(t *testing.T) {
	p := openLivePair(t, nil, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	out, _ := p.pageDiff(t, map[string]any{"title": "two builds"})
	var want bytes.Buffer
	if err := flamegraph.WriteDiffPage(&want, diffOfFixtures(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew()),
		flamegraph.DiffPageOptions{BaseName: "base.pb.gz", NewName: "new.pb.gz"}); err != nil {
		t.Fatal(err)
	}
	res, body := httpGet(t, out.URL+"pub/p1/export", nil)
	wantStatus(t, "export", res, body, http.StatusOK)
	if !bytes.Equal(body, want.Bytes()) {
		t.Errorf("the export is %d bytes that are not WriteDiffPage's %d", len(body), want.Len())
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="p1.html"` {
		t.Errorf("Content-Disposition %q", got)
	}
	if got := res.Header.Get("Content-Security-Policy"); got != livehttp.ExportCSP {
		t.Errorf("CSP %q", got)
	}
	if m := diffModelOf(t, []byte(pageGet(t, out.URL, "pub/p1/model"))); !m.Export.OK {
		t.Errorf("the model offers no export: %+v", m.Export)
	}
}

// wideDeepPair is the comparison of eight 400-frame chains with 1 KiB
// names: every frame but each chain's leaf is on both sides, and the leaf
// is new.
func wideDeepPair() (base, next []byte) {
	const chains, per, nameLen = 8, 400, 1 << 10
	var bs, ns []pmfixture.Sample
	for c := range chains {
		frames := make([]pmfixture.Frame, per)
		for i := range frames {
			s := fmt.Sprintf("c%02d_f%04d_", c, i)
			frames[i] = pmfixture.Fn(s + strings.Repeat("n", nameLen-len(s)))
		}
		bs = append(bs, pmfixture.Sample{Frames: frames[:per-1], Value: 10})
		ns = append(ns, pmfixture.Sample{Frames: frames[:per-1], Value: 10}, pmfixture.Sample{Frames: frames, Value: 10})
	}
	return pmfixture.Build("cpu", "nanoseconds", nil, bs...), pmfixture.Build("cpu", "nanoseconds", nil, ns...)
}

func TestPageDiffExportTooLargeIs422(t *testing.T) {
	const budget = 4 << 20
	base, next := wideDeepPair()
	p := openLivePair(t, func(c *Config) { c.Limits.MaxPageBytes = budget }, base, next)
	out, _ := p.pageDiff(t, nil)
	m := diffModelOf(t, []byte(pageGet(t, out.URL, "pub/p1/model")))
	if m.SizeNote == "" || m.Export.OK || !strings.Contains(m.Export.Reason, fmt.Sprint(budget)) {
		t.Fatalf("the model fits by leaving frames out and offers no export: size_note %q, export %+v", m.SizeNote, m.Export)
	}
	res, body := httpGet(t, out.URL+"pub/p1/export", nil)
	wantStatus(t, "export", res, body, http.StatusUnprocessableEntity)
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type %q", got)
	}
	if got := res.Header.Get("Content-Disposition"); got != "" {
		t.Errorf("a refused export has Content-Disposition %q", got)
	}
	want := fmt.Sprintf("the diff cannot be exported within this server's MaxPageBytes=%d and MaxPageNodes=%d: flamegraph: %s\n", budget, DefaultLimits().MaxPageNodes, m.Export.Reason)
	if string(body) != want {
		t.Errorf("body %q, want %q", body, want)
	}
}

func TestPageShowFindsADiffNodeTheModelLeftOut(t *testing.T) {
	base, next := pmfixture.CPUBasic(), pmfixture.CPUBasicNew()
	d := diffOfFixtures(t, base, next)
	ix := profilemodel.NewDiffIndex(d)
	// all > main > a > b
	var bNode *profilemodel.DiffNode
	for _, n := range d.Root.Children[0].Children {
		for _, c := range n.Children {
			if c.Name == "b" {
				bNode = c
			}
		}
	}
	if bNode == nil {
		t.Fatal("no b in the diff")
	}
	bID := bNode.UnionID()
	if _, ok := ix.Node(bID); !ok {
		t.Fatal("b is not indexed")
	}
	// The largest budget whose model leaves b out.
	opts := flamegraph.DiffLiveOptions{BaseName: "base.pb.gz", NewName: "new.pb.gz", MaxNodes: DefaultLimits().MaxPageNodes}
	whole, err := flamegraph.DiffLiveModel(d, opts)
	if err != nil {
		t.Fatal(err)
	}
	budget := 0
	for b := len(whole); b > 0; b -= 16 {
		opts.MaxBytes = b
		raw, err := flamegraph.DiffLiveModel(d, opts)
		if err != nil {
			break
		}
		if !slices.Contains(diffModelOf(t, raw).frameIDs(), string(bID)) {
			budget = b
			break
		}
	}
	if budget == 0 {
		t.Fatal("no budget leaves b out")
	}
	p := openLivePair(t, func(c *Config) { c.Limits.MaxPageBytes = budget }, base, next)
	if got := p.unionID(t, "b"); got != string(bID) {
		t.Fatalf("diff names b %s, the fixture's diff %s", got, bID)
	}
	out, _ := p.pageDiff(t, nil)
	if m := diffModelOf(t, []byte(pageGet(t, out.URL, "pub/p1/model"))); slices.Contains(m.frameIDs(), string(bID)) {
		t.Fatalf("b is in the model with MaxPageBytes=%d", budget)
	}
	var shown pageShowOut
	p.c.callOK("page_show", map[string]any{"node": string(bID)}, &shown)
	if shown.Selected != string(bID) {
		t.Errorf("page_show b: %+v", shown)
	}
	var frame struct {
		Node  string `json:"node"`
		Frame struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			DeltaAbs string `json:"delta_abs"`
		} `json:"frame"`
		Path []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"path"`
	}
	raw := pageGet(t, out.URL, "pub/p1/frame/"+url.PathEscape(string(bID)))
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("frame %s: %v", raw, err)
	}
	var names []string
	for _, it := range frame.Path {
		names = append(names, it.Name)
	}
	if frame.Node != string(bID) || frame.Frame.Name != "b" || frame.Frame.DeltaAbs == "" || !slices.Equal(names, []string{"main", "a", "b"}) {
		t.Errorf("frame of b: %s", raw)
	}
	res, body := httpGet(t, out.URL+"pub/p1/focus/"+url.PathEscape(string(bID)), nil)
	wantStatus(t, "focus on b", res, body, http.StatusOK)
	if m := diffModelOf(t, body); len(m.Drawn) == 0 || !slices.Contains(m.frameIDs(), string(bID)) {
		t.Errorf("focus on b: %.300s", body)
	}
}

// The page fetches a union node with its "/" escaped; only that escape,
// between two node IDs, on a diff's frame and focus routes, is served.
func TestOnlyAUnionIDMayBeEscaped(t *testing.T) {
	p := openLivePair(t, nil, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	b, d := p.unionID(t, "b"), p.unionID(t, "d")
	out, _ := p.pageDiff(t, nil)
	var pub pageExplainOut
	p.c.callOK("page_explain", p.base.args(map[string]any{"explanations": []any{}}), &pub)
	base := out.URL
	bBase, bNew, _ := strings.Cut(b, "/")
	dBase, dNew, _ := strings.Cut(d, "/")
	if dBase != "-" {
		t.Fatalf("d is new only, its union ID is %s", d)
	}
	for _, route := range []string{"pub/p1/frame/" + bBase + "%2F" + bNew, "pub/p1/focus/" + bBase + "%2F" + bNew, "pub/p1/frame/-%2F" + dNew} {
		res, body := httpGet(t, base+route, nil)
		wantStatus(t, route, res, body, http.StatusOK)
	}
	plain := p.base.nodeID(t, "b")
	for _, route := range []string{
		"pub/p1/frame/" + bBase + "%2f" + bNew,
		"pub/p1/frame/" + bBase + "%2F" + bNew + "%2F" + bNew,
		"pub/p1/frame/" + bBase + "%2F",
		"pub/p1/frame/%2F" + bNew,
		"pub/p1/frame/" + bBase + "%2F" + strings.ToUpper(bNew),
		"pub/p1/frame/" + bBase + "%2Fx" + bNew[1:],
		"pub/p1/frame/" + bBase + "%2F" + bNew + "0",
		"pub/p1/frame/%2D%2F" + dNew,
		"pub/p1/frame/" + bBase + "/" + bNew,
		"pub/p1/frame/" + bBase + "%252F" + bNew,
		"pub/p1/model%2F" + bNew,
		"pub/p1/history/" + bBase + "%2F" + bNew,
		"pub/p1/export/" + bBase + "%2F" + bNew,
		"pub/p%31/frame/" + bBase + "%2F" + bNew,
		"pub%2Fp1/frame/" + bBase + "%2F" + bNew,
		"pub/p1/frame/" + plain,
		"pub/p2/frame/" + bBase + "%2F" + bNew,
		"pub/p2/focus/" + bBase + "%2F" + bNew,
		"pub/p1/frame/0000000000000000%2F" + bNew,
	} {
		res, body := httpGet(t, base+route, nil)
		wantStatus(t, route, res, body, http.StatusNotFound)
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q", route, loc)
		}
	}
}

// A diff whose model cannot be built within MaxPageBytes publishes
// nothing, and says which limit to raise.
func TestPageDiffThatCannotBeDrawnSaysWhy(t *testing.T) {
	p := openLivePair(t, func(c *Config) { c.Limits.MaxPageBytes = 600 }, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	url := pageOpen(t, p.c).URL
	before := pageState(t, url)
	text, isErr := p.c.call("page_diff", p.args(nil))
	if !isErr || !strings.HasPrefix(text, "the diff cannot be drawn within this server's MaxPageBytes=600: ") ||
		!strings.HasSuffix(text, "; restart perf-agent-mcp with a larger --limit MaxPageBytes=N; nothing was published") {
		t.Errorf("isError=%v %s", isErr, text)
	}
	if after := pageState(t, url); after.Seq != before.Seq || len(after.Publications) != 0 {
		t.Errorf("the page changed: %+v, then %+v", before, after)
	}
}
