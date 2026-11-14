package perfmcp

import (
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

type qEdge struct {
	Function string `json:"function"`
	Name     string `json:"name"`
	Value    qFact  `json:"value"`
}

type qEdgeList struct {
	Rows []qEdge `json:"rows"`
	pageInfo
}

type qSandwich struct {
	Function  qRow      `json:"function"`
	Recursive bool      `json:"recursive"`
	RootValue string    `json:"root_value"`
	Rule      string    `json:"rule"`
	Notes     []string  `json:"notes"`
	Callers   qEdgeList `json:"callers"`
	Callees   qEdgeList `json:"callees"`
}

func (o opened) functionRef(t *testing.T, name string) string {
	t.Helper()
	for _, r := range o.top(t, map[string]any{"rows": "functions", "limit": 200}).Rows {
		if r.Name == name {
			return r.Function
		}
	}
	t.Fatalf("no function named %q", name)
	return ""
}

func edgeSummary(rows []qEdge) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name+"="+r.Value.val())
	}
	return out
}

func TestSandwichOverMCP(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	var out qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": o.functionRef(t, "a")}), &out)
	if got, want := edgeSummary(out.Callers.Rows), []string{"main=511"}; !slices.Equal(got, want) {
		t.Errorf("callers = %v, want %v", got, want)
	}
	if got, want := edgeSummary(out.Callees.Rows), []string{"b=411"}; !slices.Equal(got, want) {
		t.Errorf("callees = %v, want %v", got, want)
	}
	if out.RootValue != "0" || out.Function.Name != "a" || out.Function.Cumulative.val() != "511" || out.Recursive {
		t.Errorf("sandwich = %+v", out)
	}
	if out.Callers.OmittedCount != "0" || out.Callers.OmittedWeightBasis != "value" || out.Callees.TotalRows != "1" || out.Notes != nil {
		t.Errorf("pages = %+v %+v", out.Callers.pageInfo, out.Callees.pageInfo)
	}
}

func TestSandwichRecursionRuleIsStated(t *testing.T) {
	o := openFixture(t, "rec.pb.gz", pmfixture.Recursive(), nil)
	var out qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": o.functionRef(t, "a")}), &out)
	if !out.Recursive || !strings.Contains(out.Rule, "counted once per sample") || !strings.Contains(out.Rule, "more than the function") {
		t.Errorf("recursive=%v rule=%q", out.Recursive, out.Rule)
	}
}

func TestSandwichUnknownFunctionSaysWhatToCall(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	text, isErr := o.c.call("sandwich", o.args(map[string]any{"function": "0123456789abcdef"}))
	if !isErr || !strings.Contains(text, "is not in view") || !strings.Contains(text, `rows="functions"`) {
		t.Errorf("%v %s", isErr, text)
	}
}

func TestSandwichPagesBothListsWithOneCursor(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	// main has callees a and c; c has caller main only. Page main's callees.
	ref := o.functionRef(t, "main")
	var p1 qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": ref, "limit": 1}), &p1)
	if len(p1.Callees.Rows) != 1 || p1.Callees.OmittedCount != "1" || p1.Callees.OmittedWeight != "489" || p1.Callees.NextCursor == "" {
		t.Fatalf("page 1 callees = %+v", p1.Callees)
	}
	var p2 qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": ref, "limit": 1, "cursor": p1.Callees.NextCursor}), &p2)
	if got := edgeSummary(p2.Callees.Rows); !slices.Equal(got, []string{"c=489"}) || p2.Callees.NextCursor != "" || p2.Callees.Skipped != "1" {
		t.Errorf("page 2 callees = %v %+v", got, p2.Callees)
	}
	if text, isErr := o.c.call("sandwich", o.args(map[string]any{"function": o.functionRef(t, "a"), "cursor": p1.Callees.NextCursor})); !isErr {
		t.Errorf("cursor of another function accepted: %s", text)
	}
}

func TestSandwichPagingOneListKeepsTheOthersOffset(t *testing.T) {
	smp := func(v int64, names ...string) pmfixture.Sample {
		return pmfixture.Sample{Frames: namedFrames(names...), Value: v}
	}
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		smp(5, "x", "m", "y"), smp(1, "x", "m", "z"), smp(2, "w", "m", "y"))
	o := openFixture(t, "two.pb.gz", raw, nil)
	ref := o.functionRef(t, "m")
	var p1 qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": ref, "limit": 1}), &p1)
	if p1.Callers.TotalRows != "2" || p1.Callees.TotalRows != "2" ||
		!strings.HasPrefix(p1.Callees.NextCursor, "c1.0,1.") || !strings.HasPrefix(p1.Callers.NextCursor, "c1.1,0.") {
		t.Fatalf("page 1 cursors: callers %q callees %q", p1.Callers.NextCursor, p1.Callees.NextCursor)
	}
	// Advance callees, then callers from that page: callees stay at 1.
	var p2 qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": ref, "limit": 1, "cursor": p1.Callees.NextCursor}), &p2)
	if got := edgeSummary(p2.Callees.Rows); !slices.Equal(got, []string{"z=1"}) || p2.Callees.Skipped != "1" {
		t.Errorf("callees on page 2 = %v skipped %s", got, p2.Callees.Skipped)
	}
	if got := edgeSummary(p2.Callers.Rows); !slices.Equal(got, []string{"x=6"}) || p2.Callers.Skipped != "0" {
		t.Errorf("callers on page 2 = %v skipped %s", got, p2.Callers.Skipped)
	}
	if !strings.HasPrefix(p2.Callers.NextCursor, "c1.1,1.") {
		t.Errorf("callers next cursor %q does not keep the callee offset 1", p2.Callers.NextCursor)
	}
	var p3 qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": ref, "limit": 1, "cursor": p2.Callers.NextCursor}), &p3)
	if got := edgeSummary(p3.Callers.Rows); !slices.Equal(got, []string{"w=2"}) || p3.Callers.Skipped != "1" ||
		!slices.Equal(edgeSummary(p3.Callees.Rows), []string{"z=1"}) || p3.Callees.Skipped != "1" {
		t.Errorf("page 3 = %+v / %+v", p3.Callers, p3.Callees)
	}
}

func namedFrames(names ...string) []pmfixture.Frame {
	out := make([]pmfixture.Frame, len(names))
	for i, n := range names {
		out[i] = pmfixture.Fn(n)
	}
	return out
}

func TestSandwichOnAGPUViewCarriesNotes(t *testing.T) {
	o := openFixture(t, "gpu.pb.gz", pmfixture.GPUBasic(), nil)
	var out qSandwich
	o.c.callOK("sandwich", o.args(map[string]any{"function": o.functionRef(t, "conv")}), &out)
	if len(out.Notes) == 0 {
		t.Error("GPU sandwich has no notes")
	}
}
