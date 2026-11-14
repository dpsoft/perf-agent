package perfmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

type qPath struct {
	Frames []struct {
		Name string `json:"name"`
	} `json:"frames"`
	pageInfo
}

func TestPathPagesADeepStack(t *testing.T) {
	const depth = 2000
	o := openFixture(t, "deep.pb.gz", pmfixture.Deep(depth), nil)
	leaf := o.top(t, map[string]any{"rows": "nodes", "by": "self", "limit": 1}).Rows[0].Node

	var names []string
	cursor := ""
	pages := 0
	for {
		args := map[string]any{"node": leaf}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var out qPath
		o.c.callOK("path", o.args(args), &out)
		pages++
		if len(out.Frames) > 200 {
			t.Fatalf("page has %d frames, default limit is 200", len(out.Frames))
		}
		for _, f := range out.Frames {
			names = append(names, f.Name)
		}
		if out.OmittedWeightBasis != "cum" || out.TotalRows != "2000" {
			t.Fatalf("page = %+v", out.pageInfo)
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(names) != depth || names[0] != "f0" || names[depth-1] != fmt.Sprintf("f%d", depth-1) || pages != 10 {
		t.Errorf("got %d frames in %d pages, first %q", len(names), pages, names[0])
	}
	// A cursor of another node is refused; limit is bounded by MaxRows.
	if text, isErr := o.c.call("path", o.args(map[string]any{"node": leaf, "limit": 201})); !isErr || !strings.Contains(text, "MaxRows") {
		t.Errorf("limit 201: %v %s", isErr, text)
	}
	// facts succeeds on the deep stack and carries the path names.
	var f struct {
		Path []string `json:"path"`
	}
	o.c.callOK("facts", o.args(map[string]any{"node": leaf}), &f)
	if len(f.Path) != depth {
		t.Errorf("facts path has %d names", len(f.Path))
	}
}

func TestFactsOverTheLimitAdvisesFactsSpecificSteps(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxResponseBytes = 4096
	writeFixture(t, cfg.ProfilesDir, "deep.pb.gz", pmfixture.Deep(2000))
	c := newTestServer(t, cfg)
	var op openOut
	c.callOK("open", map[string]any{"path": "deep.pb.gz"}, &op)
	var top qTop
	c.callOK("top", map[string]any{"handle": op.Handle, "view": op.DefaultView, "rows": "nodes", "by": "self", "limit": 1}, &top)
	text, isErr := c.call("facts", map[string]any{"handle": op.Handle, "view": op.DefaultView, "node": top.Rows[0].Node})
	if !isErr || strings.Contains(text, "smaller limit") || !strings.Contains(text, "MaxResponseBytes") || !strings.Contains(text, "path") {
		t.Errorf("isError=%v %s", isErr, text)
	}
}

func TestFactsGroupViewLevelValuesInViewScope(t *testing.T) {
	o := openFixture(t, "gpu.pb.gz", pmfixture.GPUBasic(), nil)
	text := o.c.callOK("facts", o.args(map[string]any{"node": o.nodeID(t, "conv")}), nil)
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["unsampled_portion"]; has {
		t.Error("top-level unsampled_portion present")
	}
	var vs struct {
		UnsampledPortion qFact `json:"unsampled_portion"`
	}
	if err := json.Unmarshal(m["view_scope"], &vs); err != nil || vs.UnsampledPortion.val() != "100" {
		t.Errorf("view_scope = %s (%v)", m["view_scope"], err)
	}
	if _, has := m["notes"]; !has {
		t.Error("notes missing at top level")
	}
}

func TestOpenAllocationsStayBoundedWithManySampleTypes(t *testing.T) {
	const types = 1024
	p := &profile.Profile{}
	for i := range types {
		p.SampleType = append(p.SampleType, &profile.ValueType{Type: fmt.Sprintf("t%d", i), Unit: "count"})
	}
	fn := &profile.Function{ID: 1, Name: "main"}
	loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn}}}
	p.Function, p.Location = []*profile.Function{fn}, []*profile.Location{loc}
	vals := make([]int64, types)
	vals[0] = 1
	p.Sample = []*profile.Sample{{Location: []*profile.Location{loc}, Value: vals}}
	var raw bytes.Buffer
	if err := p.Write(&raw); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "many.pb.gz", raw.Bytes())
	srv := must(New(cfg))(t)
	t.Cleanup(func() { _ = srv.Close() })
	args := json.RawMessage(`{"path":"many.pb.gz"}`)
	if _, err := srv.handleOpen(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	var total uint64
	for range 3 {
		total += uint64(testing.AllocsPerRun(1, func() { _, _ = srv.handleOpen(t.Context(), args) }))
	}
	// Linear building of the views list is a few allocations per type; the
	// quadratic form rebuilt every name for every type (millions).
	if avg := total / 3; avg > 40*types {
		t.Errorf("open allocates %d times for %d sample types", avg, types)
	}
}

func TestRootIsNotARowDomain(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	text, isErr := o.c.call("top", o.args(map[string]any{"rows": "nodes", "domain": "root"}))
	if !isErr || !strings.Contains(text, "gpu-kernel") || strings.Contains(text, ", root") {
		t.Errorf("isError=%v %s", isErr, text)
	}
	list := o.c.rpc("tools/list", map[string]any{})
	if strings.Contains(string(list.Result), "root)") || !strings.Contains(string(list.Result), "boundary-unattributed") {
		t.Errorf("schema domains: %s", list.Result)
	}
}

func TestFilteredOutAndOmittedCountsAreSeparate(t *testing.T) {
	f := func(names ...string) []pmfixture.Frame {
		var out []pmfixture.Frame
		for _, n := range names {
			out = append(out, pmfixture.Fn(n))
		}
		return out
	}
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: f("main", "work1"), Value: 50},
		pmfixture.Sample{Frames: f("main", "work2"), Value: 40},
		pmfixture.Sample{Frames: f("main", "work3"), Value: 30},
		pmfixture.Sample{Frames: f("main", "cudaLaunchKernel"), Value: 20},
	)
	o := openFixture(t, "mixed.pb.gz", raw, nil)
	out := o.top(t, map[string]any{"rows": "functions", "domain": "app", "limit": 2})
	// 5 functions: main, work1..3 are app, cudaLaunchKernel is vendor.
	if out.FilteredOutCount != "1" || out.OmittedCount != "2" || out.TotalRows != "4" || out.OmittedWeight != "70" {
		t.Errorf("page = %+v", out)
	}
	if got := rowSummary(out.Rows, false); !slices.Equal(got, []string{"main=140", "work1=50"}) {
		t.Errorf("rows = %v", got)
	}
}

func TestKernelsPageWithACursor(t *testing.T) {
	o := openFixture(t, "gpu.pb.gz", pmfixture.GPUBasic(), nil)
	conv := o.nodeID(t, "conv")
	type kout struct {
		Rows []struct {
			Kernel string `json:"kernel"`
		} `json:"rows"`
		pageInfo
	}
	var first, second kout
	o.c.callOK("gpu_kernels_for", o.args(map[string]any{"node": conv, "limit": 1}), &first)
	if first.OmittedCount != "2" || first.OmittedWeight != "100" || first.OmittedWeightBasis != "value" || first.NextCursor == "" || first.Rows[0].Kernel != "k1" {
		t.Errorf("first = %+v", first)
	}
	o.c.callOK("gpu_kernels_for", o.args(map[string]any{"node": conv, "limit": 1, "cursor": first.NextCursor}), &second)
	if second.OmittedCount != "1" || second.OmittedWeight != "0" || second.Rows[0].Kernel != "k2" || second.Skipped != "1" {
		t.Errorf("second = %+v", second)
	}
}

func TestRenderTitleIsFreeTextBoundedAndCounted(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	exps := shareClaim(b, "0.411", "b takes {c1} of the time")

	long := "Verified: b is 87% of CPU" + strings.Repeat("x", 300<<10)
	for _, title := range []string{long, "b is {c1}", "{n:" + b + "} is hot"} {
		text, isErr := o.c.call("render_flamegraph", o.args(map[string]any{"explanations": exps, "out": "t.html", "title": title}))
		if !isErr || len(text) > 1024 || !strings.Contains(text, "title") {
			t.Errorf("title %.40q: isError=%v %.300s", title, isErr, text)
		}
	}
	if text, _ := o.c.call("render_flamegraph", o.args(map[string]any{"explanations": exps, "out": "t.html", "title": long})); !strings.Contains(text, "MaxTitleBytes") {
		t.Errorf("a long title's error does not name MaxTitleBytes: %.300s", text)
	}
	if names := entries(t, o.out); len(names) != 0 {
		t.Errorf("a refused title wrote %v", names)
	}

	out, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "t.html", "title": "Verified: b is 87% of CPU"})
	if isErr {
		t.Fatal(text)
	}
	// A refused title records no attempt.
	if out.Attempt != "a1" || out.UncheckedNumbers != "1" {
		t.Errorf("attempt %s, unchecked_numbers %s; want a1 and 1 (the title's 87%%)", out.Attempt, out.UncheckedNumbers)
	}
	page := string(readFile(t, out.Path))
	if !strings.Contains(page, `<h1>Verified: b is <mark class="unchecked" title="not checked by perf-agent">87%</mark> of CPU</h1>`) {
		t.Errorf("the heading does not mark the title's number")
	}
}

func TestToolErrorsAreBoundedByMaxResponseBytes(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxResponseBytes = 4096
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var op openOut
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &op)
	o := opened{c: c, handle: op.Handle, view: op.DefaultView}
	huge := strings.Repeat("z", 300<<10)
	fn := o.functionRef(t, "b")

	for name, call := range map[string]struct {
		tool string
		args any
	}{
		"cursor":           {"top", o.args(map[string]any{"rows": "nodes", "cursor": "c1." + huge})},
		"rows":             {"top", o.args(map[string]any{"rows": huge})},
		"by":               {"top", o.args(map[string]any{"rows": "nodes", "by": huge})},
		"domain":           {"top", o.args(map[string]any{"rows": "nodes", "domain": huge})},
		"unknown argument": {"top", o.args(map[string]any{"rows": "nodes", huge: 1})},
		"sandwich cursor":  {"sandwich", o.args(map[string]any{"function": fn, "cursor": "c1." + huge})},
		"view":             {"facts", map[string]any{"handle": o.handle, "view": huge, "node": "x"}},
	} {
		text, isErr := c.call(call.tool, call.args)
		if !isErr || len(text) > cfg.Limits.MaxResponseBytes || strings.Contains(text, strings.Repeat("z", 100)) {
			t.Errorf("%s: isError=%v, %d bytes: %.200s", name, isErr, len(text), text)
		}
	}

	res := c.rpc("tools/call", map[string]any{"name": huge, "arguments": map[string]any{}})
	if res.Error == nil || len(res.Error.Message) > cfg.Limits.MaxResponseBytes || strings.Contains(res.Error.Message, strings.Repeat("z", 100)) {
		t.Errorf("unknown tool: %+v", res.Error)
	}
}

func TestToolArgumentNamesMatchExactlyOnce(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	exps := mustJSON(t, shareClaim(b, "0.411", "b takes {c1} of the time"))
	for name, raw := range map[string]string{
		"case-folded name": fmt.Sprintf(`{"handle": %q, "view": %q, "Explanations": %s, "out": "a.html"}`, o.handle, o.view, exps),
		"duplicate out":    fmt.Sprintf(`{"handle": %q, "view": %q, "explanations": %s, "out": "a.html", "out": "b.html"}`, o.handle, o.view, exps),
		"duplicate title":  fmt.Sprintf(`{"handle": %q, "view": %q, "explanations": %s, "out": "a.html", "title": "x", "title": "y"}`, o.handle, o.view, exps),
	} {
		text, isErr := o.c.call("render_flamegraph", json.RawMessage(raw))
		if !isErr || !strings.Contains(text, "render_flamegraph") {
			t.Errorf("%s: isError=%v %s", name, isErr, text)
		}
	}
	if names := entries(t, o.out); len(names) != 0 {
		t.Errorf("refused arguments wrote %v", names)
	}
	// Every tool decodes its arguments the same way.
	text, isErr := o.c.call("top", json.RawMessage(fmt.Sprintf(`{"handle": %q, "view": %q, "rows": "nodes", "rows": "functions"}`, o.handle, o.view)))
	if !isErr || !strings.Contains(text, "twice") {
		t.Errorf("duplicate rows: isError=%v %s", isErr, text)
	}
	text, isErr = o.c.call("top", json.RawMessage(fmt.Sprintf(`{"Handle": %q, "view": %q, "rows": "nodes"}`, o.handle, o.view)))
	if !isErr || !strings.Contains(text, "Handle") {
		t.Errorf("case-folded handle: isError=%v %s", isErr, text)
	}
}

// An absolute path outside every root is refused by its text alone: nothing
// under it is looked up, so a path naming a directory that does not exist,
// or an automounter, costs no filesystem access.
func TestAnAbsolutePathOutsideTheRootIsNotResolved(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var resolved []string
	c.srv.evalSymlinks = func(p string) (string, error) {
		resolved = append(resolved, p)
		return filepath.EvalSymlinks(p)
	}
	missing := filepath.Join(string(filepath.Separator), "perf-agent-no-such-top-dir", "sub", "cpu.pb.gz")
	text, isErr := c.call("open", map[string]any{"path": missing})
	if !isErr || !strings.Contains(text, "is outside the profiles root "+cfg.ProfilesDir) {
		t.Errorf("open %s: isError=%v %s", missing, isErr, text)
	}
	for _, p := range resolved {
		if strings.Contains(p, "perf-agent-no-such-top-dir") {
			t.Errorf("rel resolved the agent's path %s", p)
		}
	}
	// An absolute path inside the root still works.
	c.callOK("open", map[string]any{"path": filepath.Join(cfg.ProfilesDir, "cpu.pb.gz")}, nil)
}

func TestFactsCarryTheViewsQualityInViewScope(t *testing.T) {
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}}}
	fn := &profile.Function{ID: 1, Name: "main"}
	loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn}}}
	p.Function, p.Location = []*profile.Function{fn}, []*profile.Location{loc}
	p.Sample = []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{3, 30}}}
	var raw bytes.Buffer
	if err := p.Write(&raw); err != nil {
		t.Fatal(err)
	}
	o := openFixture(t, "two.pb.gz", raw.Bytes(), nil)
	var op struct {
		Quality json.RawMessage `json:"quality"`
		Views   []struct {
			View string `json:"view"`
		} `json:"views"`
	}
	o.c.callOK("open", map[string]any{"path": "two.pb.gz"}, &op)
	for _, vw := range op.Views {
		ov := opened{c: o.c, handle: o.handle, view: vw.View}
		var out struct {
			ViewScope struct {
				Quality json.RawMessage `json:"quality"`
			} `json:"view_scope"`
		}
		ov.c.callOK("facts", ov.args(map[string]any{"node": ov.nodeID(t, "main")}), &out)
		_, v, err := o.c.srv.store.view(profilemodel.Handle(o.handle), vw.View)
		if err != nil {
			t.Fatal(err)
		}
		if want := mustJSON(t, wireQuality(v.Quality())); string(out.ViewScope.Quality) != want {
			t.Errorf("view %s: view_scope.quality = %s, want %s", vw.View, out.ViewScope.Quality, want)
		}
		if vw.View == o.view && string(out.ViewScope.Quality) != string(op.Quality) {
			t.Errorf("default view: view_scope.quality differs from open's quality")
		}
	}
}

func TestRenderResultsOnAGPUViewCarryTheNotes(t *testing.T) {
	cfg := testConfig(t)
	cfg.OutputDir = t.TempDir()
	writeFixture(t, cfg.ProfilesDir, "gpu.pb.gz", pmfixture.GPUBasic())
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var gop, cop openOut
	c.callOK("open", map[string]any{"path": "gpu.pb.gz"}, &gop)
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &cop)
	g := rendering{opened: opened{c: c, handle: gop.Handle, view: gop.DefaultView}, out: cfg.OutputDir}
	conv := g.nodeID(t, "conv")
	notesOf := func(text string) (json.RawMessage, bool) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatalf("decode %s: %v", text, err)
		}
		n, ok := m["notes"]
		return n, ok
	}
	want := mustJSON(t, gop.Notes)
	for _, value := range []string{"0.9", "0.8"} {
		_, text, _ := g.render(t, map[string]any{"explanations": shareClaim(conv, value, "conv is {c1}"), "out": "g" + value + ".html"})
		if n, ok := notesOf(text); !ok || string(n) != want || len(gop.Notes) == 0 {
			t.Errorf("value %s: notes = %s (present %v), want %s", value, n, ok, want)
		}
	}
	cr := rendering{opened: opened{c: c, handle: cop.Handle, view: cop.DefaultView}, out: cfg.OutputDir}
	_, text, _ := cr.render(t, map[string]any{"explanations": shareClaim(cr.nodeID(t, "b"), "0.411", "b is {c1}"), "out": "c.html"})
	if _, ok := notesOf(text); ok {
		t.Errorf("a CPU render result has notes: %s", text)
	}
}

func TestACPUDiffHasNoNotesKeys(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	_, text := p.diff(t, nil)
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"base_notes", "new_notes"} {
		if _, ok := m[k]; ok {
			t.Errorf("a CPU diff has %s", k)
		}
	}
}

func TestTitleRefusalsSayWhatToChange(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	exps := shareClaim(b, "0.411", "b takes {c1} of the time")
	text, _ := o.c.call("render_flamegraph", o.args(map[string]any{"explanations": exps, "out": "t.html", "title": strings.Repeat("t", 201)}))
	if !strings.Contains(text, "--limit Explain.MaxTitleBytes=N") || !strings.Contains(text, "same attempt, if any") || strings.Contains(text, "without attempt") {
		t.Errorf("long title: %s", text)
	}
	text, _ = o.c.call("render_flamegraph", o.args(map[string]any{"explanations": exps, "out": "t.html", "title": "b is {c1}"}))
	if strings.Contains(text, "MaxTitleBytes") || !strings.Contains(text, "same attempt, if any") || strings.Contains(text, "without attempt") {
		t.Errorf("placeholder title: %s", text)
	}
}

// A rejection never advises copying the engine's value: the claim's node or
// metric may be the mistake, not its value.
func TestRejectionAdviceIsToRecheckNotCopy(t *testing.T) {
	o := renderFixture(t, nil)
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(o.nodeID(t, "b"), "0.62", "b takes {c1}"), "out": "r.html"})
	if !isErr || out.Status != "rejected" {
		t.Fatalf("isError=%v %s", isErr, text)
	}
	var rej struct {
		Next string `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &rej); err != nil {
		t.Fatal(err)
	}
	const advice = "re-check that each claim's node and metric match the sentence, then rewrite the sentence or the claim"
	if !strings.Contains(rej.Next, advice) || strings.Contains(strings.ToLower(rej.Next), "copy") || strings.Contains(rej.Next, "correct every failure") {
		t.Errorf("next = %q", rej.Next)
	}
	if !strings.Contains(o.c.srv.renderTool().Description, advice) {
		t.Errorf("the render description lacks %q", advice)
	}
}
