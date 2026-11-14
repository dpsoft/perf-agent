package perfmcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// qFact is a wire fact as a test reads it; Value is nil when absent.
type qFact struct {
	Value       *string `json:"value"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason"`
	Denominator string  `json:"denominator"`
}

func (f qFact) val() string {
	if f.Value == nil {
		return "<none>"
	}
	return *f.Value
}

type qRow struct {
	Node         string `json:"node"`
	Function     string `json:"function"`
	Name         string `json:"name"`
	Module       string `json:"module"`
	Domain       string `json:"domain"`
	Cumulative   qFact  `json:"cumulative"`
	Self         qFact  `json:"self"`
	ShareOfTotal qFact  `json:"share_of_total"`
}

type qTop struct {
	Handle             string   `json:"handle"`
	View               string   `json:"view"`
	Notes              []string `json:"notes"`
	Rows               []qRow   `json:"rows"`
	TotalRows          string   `json:"total_rows"`
	Skipped            string   `json:"skipped"`
	OmittedCount       string   `json:"omitted_count"`
	OmittedWeight      string   `json:"omitted_weight"`
	OmittedWeightBasis string   `json:"omitted_weight_basis"`
	FilteredOutCount   string   `json:"filtered_out_count"`
	NextCursor         string   `json:"next_cursor"`
}

// opened is a server with fixtures opened.
type opened struct {
	c      *testClient
	handle string
	view   string
}

func openFixture(t *testing.T, name string, raw []byte, execlog []byte) opened {
	t.Helper()
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, name, raw)
	args := map[string]any{"path": name}
	if execlog != nil {
		writeFixture(t, cfg.ProfilesDir, "x.execlog", execlog)
		args["execlog"] = "x.execlog"
	}
	c := newTestServer(t, cfg)
	var out openOut
	c.callOK("open", args, &out)
	return opened{c: c, handle: out.Handle, view: out.DefaultView}
}

func (o opened) args(extra map[string]any) map[string]any {
	m := map[string]any{"handle": o.handle, "view": o.view}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (o opened) top(t *testing.T, extra map[string]any) qTop {
	t.Helper()
	var out qTop
	o.c.callOK("top", o.args(extra), &out)
	return out
}

// nodeID finds the ID of the first node row named name.
func (o opened) nodeID(t *testing.T, name string) string {
	t.Helper()
	for _, r := range o.top(t, map[string]any{"rows": "nodes", "limit": 200}).Rows {
		if r.Name == name {
			return r.Node
		}
	}
	t.Fatalf("no node named %q", name)
	return ""
}

func rowSummary(rows []qRow, self bool) []string {
	var out []string
	for _, r := range rows {
		f := r.Cumulative
		if self {
			f = r.Self
		}
		out = append(out, r.Name+"="+f.val())
	}
	return out
}

func TestTopNodesByCumulative(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	out := o.top(t, map[string]any{"rows": "nodes"})
	if got, want := rowSummary(out.Rows, false), []string{"main=1000", "a=511", "c=489", "b=411"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	a := out.Rows[1]
	if a.ShareOfTotal.val() != "0.511" || a.Module != "/opt/app/bin/app" || a.Domain != "app" || a.Node == "" {
		t.Errorf("a = %+v", a)
	}
	if out.Notes != nil || out.OmittedCount != "0" || out.NextCursor != "" || out.TotalRows != "4" || out.OmittedWeightBasis != "cum" {
		t.Errorf("page = %+v", out)
	}
}

func TestTopFunctionsBySelf(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	out := o.top(t, map[string]any{"rows": "functions", "by": "self"})
	if got, want := rowSummary(out.Rows, true), []string{"c=489", "b=411", "a=100", "main=0"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	if last := out.Rows[3]; last.Self.Status != "zero" || last.Function == "" || out.OmittedWeightBasis != "self" {
		t.Errorf("main = %+v basis %s", last, out.OmittedWeightBasis)
	}
}

func TestTopTruncatesWithOmittedCountAndWeight(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	first := o.top(t, map[string]any{"rows": "nodes", "limit": 2})
	if first.OmittedCount != "2" || first.OmittedWeight != "900" || first.OmittedWeightBasis != "cum" || first.NextCursor == "" || first.Skipped != "0" {
		t.Errorf("first = %+v", first)
	}
	second := o.top(t, map[string]any{"rows": "nodes", "limit": 2, "cursor": first.NextCursor})
	if got, want := rowSummary(second.Rows, false), []string{"c=489", "b=411"}; !slices.Equal(got, want) {
		t.Errorf("second = %v", got)
	}
	if second.NextCursor != "" || second.OmittedCount != "0" || second.Skipped != "2" {
		t.Errorf("second page = %+v", second)
	}
}

func TestTopCursorFromAnotherQueryIsRefused(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	first := o.top(t, map[string]any{"rows": "nodes", "limit": 2})
	text, isErr := o.c.call("top", o.args(map[string]any{"rows": "functions", "limit": 2, "cursor": first.NextCursor}))
	if !isErr || !strings.Contains(text, "another query") {
		t.Errorf("isError=%v %s", isErr, text)
	}
}

func TestTopDomainFilter(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	out := o.top(t, map[string]any{"rows": "nodes", "domain": "app"})
	if len(out.Rows) != 4 || out.FilteredOutCount != "0" {
		t.Errorf("app: %+v", out)
	}
	out = o.top(t, map[string]any{"rows": "nodes", "domain": "kernel"})
	if len(out.Rows) != 0 || out.FilteredOutCount != "4" || out.OmittedCount != "0" || out.TotalRows != "0" {
		t.Errorf("kernel: %+v", out)
	}
	text, isErr := o.c.call("top", o.args(map[string]any{"rows": "nodes", "domain": "bogus"}))
	if !isErr {
		t.Fatal("bogus domain accepted")
	}
	for _, k := range flamegraph.DomainKeys() {
		if !strings.Contains(text, k) {
			t.Errorf("error %q does not list %q", text, k)
		}
	}
}

func TestRecursionInTopFunctions(t *testing.T) {
	o := openFixture(t, "rec.pb.gz", pmfixture.Recursive(), nil)
	out := o.top(t, map[string]any{"rows": "functions"})
	if len(out.Rows) == 0 || out.Rows[0].Name != "a" || out.Rows[0].Cumulative.val() != "15" {
		t.Errorf("rows = %v", rowSummary(out.Rows, false))
	}
}

func TestPathIsRootFirstWithPerFrameFacts(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	var out struct {
		Node   string `json:"node"`
		Frames []struct {
			Node           string `json:"node"`
			Name           string `json:"name"`
			BuildID        string `json:"build_id"`
			Depth          string `json:"depth"`
			DomainInferred bool   `json:"domain_inferred"`
			ShareOfParent  qFact  `json:"share_of_parent"`
		} `json:"frames"`
	}
	b, a := o.nodeID(t, "b"), o.nodeID(t, "a")
	o.c.callOK("path", o.args(map[string]any{"node": b}), &out)
	var names []string
	for _, f := range out.Frames {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"main", "a", "b"}) || out.Node != b {
		t.Fatalf("frames = %v", names)
	}
	fb := out.Frames[2]
	if fb.ShareOfParent.val() != "0.804" || fb.ShareOfParent.Denominator != "parent:"+a || fb.BuildID != "b1" || fb.Depth != "3" || !fb.DomainInferred {
		t.Errorf("b = %+v", fb)
	}
}

func TestFactsCarryHugeIntegersLosslessly(t *testing.T) {
	o := openFixture(t, "huge.pb.gz", pmfixture.HugeValue(), nil)
	big := o.nodeID(t, "big")
	text := o.c.callOK("facts", o.args(map[string]any{"node": big}), nil)
	if !strings.Contains(text, `"cumulative":{"value":"9007199254740993"`) {
		t.Errorf("facts = %s", text)
	}
}

func TestFactsZeroAndUnavailableStayApart(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	var b struct {
		ExecutionsBelow map[string]any `json:"executions_below"`
		CallSites       string         `json:"call_sites"`
		Quality         map[string]any `json:"quality"`
		Path            []string       `json:"path"`
	}
	o.c.callOK("facts", o.args(map[string]any{"node": o.nodeID(t, "b")}), &b)
	eb := b.ExecutionsBelow
	if _, has := eb["value"]; has || eb["status"] != "unavailable" || eb["reason"] != "no sidecar" {
		t.Errorf("executions_below = %v", eb)
	}
	if b.CallSites != "1" || b.Quality == nil || !slices.Equal(b.Path, []string{"main", "a", "b"}) {
		t.Errorf("facts = %+v", b)
	}
	var m struct {
		Self qFact `json:"self"`
	}
	o.c.callOK("facts", o.args(map[string]any{"node": o.nodeID(t, "main")}), &m)
	if m.Self.Status != "zero" || m.Self.val() != "0" {
		t.Errorf("main self = %+v", m.Self)
	}
}

func TestKernelsForConv(t *testing.T) {
	o := openFixture(t, "gpu.pb.gz", pmfixture.GPUBasic(), pmfixture.GPUBasicExeclog(t))
	var out struct {
		Rows []struct {
			Kernel       string `json:"kernel"`
			Value        qFact  `json:"value"`
			ShareOfNode  qFact  `json:"share_of_node"`
			ShareOfTotal qFact  `json:"share_of_total"`
			Executions   qFact  `json:"executions"`
		} `json:"rows"`
		OmittedWeightBasis string `json:"omitted_weight_basis"`
	}
	o.c.callOK("gpu_kernels_for", o.args(map[string]any{"node": o.nodeID(t, "conv")}), &out)
	type want struct{ k, v, st, ex string }
	var got []want
	for _, r := range out.Rows {
		got = append(got, want{r.Kernel, r.Value.val(), r.Value.Status, r.Executions.val()})
	}
	exp := []want{{"k1", "300", "ok", "2"}, {"k2", "100", "ok", "1"}, {"k3", "0", "zero", "1"}}
	if !slices.Equal(got, exp) || out.OmittedWeightBasis != "value" {
		t.Errorf("rows = %v basis %s", got, out.OmittedWeightBasis)
	}
	if out.Rows[0].ShareOfNode.Denominator == "" || out.Rows[0].ShareOfTotal.val() != "0.600" {
		t.Errorf("shares = %+v", out.Rows[0])
	}
}

func TestKernelsForACPUViewIsRefused(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	text, isErr := o.c.call("gpu_kernels_for", o.args(map[string]any{"node": o.nodeID(t, "a")}))
	if !isErr || text != "node is in a cpu view; gpu_kernels_for needs a GPU view (open returned the views)" {
		t.Errorf("isError=%v %s", isErr, text)
	}
}

func TestEveryGPUViewResponseCarriesTheSamplingNotes(t *testing.T) {
	g := openFixture(t, "gpu.pb.gz", pmfixture.GPUBasic(), nil)
	conv := g.nodeID(t, "conv")
	want := mustViewNotes(t, g)
	if len(want) == 0 {
		t.Fatal("GPUBasic has no notes")
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"top", map[string]any{"rows": "nodes"}},
		{"top", map[string]any{"rows": "functions"}},
		{"path", map[string]any{"node": conv}},
		{"facts", map[string]any{"node": conv}},
		{"gpu_kernels_for", map[string]any{"node": conv}},
	} {
		var out struct {
			Notes []string `json:"notes"`
		}
		g.c.callOK(tc.tool, g.args(tc.args), &out)
		if !slices.Equal(out.Notes, want) {
			t.Errorf("%s notes = %v, want %v", tc.tool, out.Notes, want)
		}
	}
	cpu := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	a := cpu.nodeID(t, "a")
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"top", map[string]any{"rows": "nodes"}},
		{"path", map[string]any{"node": a}},
		{"facts", map[string]any{"node": a}},
	} {
		text := cpu.c.callOK(tc.tool, cpu.args(tc.args), nil)
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatal(err)
		}
		if _, has := m["notes"]; has {
			t.Errorf("%s on a cpu view has a notes key", tc.tool)
		}
	}
}

// mustViewNotes are View.Notes of the opened GPU view, from a model loaded
// independently of the server.
func mustViewNotes(t *testing.T, o opened) []string {
	t.Helper()
	srv := o.c.srv
	_, v, err := srv.store.view(profilemodel.Handle(o.handle), o.view)
	if err != nil {
		t.Fatal(err)
	}
	return must(v.Notes())(t)
}

func TestNodeErrorsSayWhatToCall(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	for _, tool := range []string{"path", "facts", "gpu_kernels_for"} {
		text, isErr := o.c.call(tool, o.args(map[string]any{"node": "nope"}))
		if !isErr || !strings.Contains(text, "call top") || !strings.Contains(text, "node nope is not in view "+o.view) {
			t.Errorf("%s: isError=%v %s", tool, isErr, text)
		}
	}
}

func TestAnAllZeroViewHasUnavailableShares(t *testing.T) {
	raw := pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main")}, Value: 0})
	o := openFixture(t, "zero.pb.gz", raw, nil)
	out := o.top(t, map[string]any{"rows": "nodes"})
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %+v", out.Rows)
	}
	s := out.Rows[0].ShareOfTotal
	if s.Status != "unavailable" || s.Reason != "zero total" || s.Value != nil {
		t.Errorf("share = %+v", s)
	}
}

func TestTopLimitIsBoundedByMaxRows(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxRows = 3
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var op openOut
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &op)
	args := map[string]any{"handle": op.Handle, "view": op.DefaultView, "rows": "nodes"}
	if text, isErr := c.call("top", args); isErr || strings.Count(text, `"node":`) != 3 {
		t.Errorf("default limit under MaxRows=3: %v %s", isErr, text)
	}
	args["limit"] = 4
	if text, isErr := c.call("top", args); !isErr || !strings.Contains(text, "MaxRows") {
		t.Errorf("limit 4: %v %s", isErr, text)
	}
}
