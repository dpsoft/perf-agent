package perfmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
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
