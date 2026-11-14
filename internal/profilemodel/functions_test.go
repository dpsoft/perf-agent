package profilemodel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func fixtureViews(t *testing.T) map[string]*View {
	t.Helper()
	raws := map[string][]byte{
		"cpu": pmfixture.CPUBasic(), "cpuNew": pmfixture.CPUBasicNew(), "gpu": pmfixture.GPUBasic(),
		"huge": pmfixture.HugeValue(), "recursive": pmfixture.Recursive(),
		"deep": pmfixture.Deep(600), "deepNew": pmfixture.DeepNew(600), "same": pmfixture.SameNameTwoModules(),
	}
	out := map[string]*View{}
	for name, raw := range raws {
		p, err := FromBytes(raw, nil, DefaultLimits())
		if err != nil {
			t.Fatal(name, err)
		}
		v, err := p.View(ViewOptions{SampleIndex: -1})
		if err != nil {
			t.Fatal(name, err)
		}
		out[name] = v
	}
	return out
}

func pmView(t *testing.T, raw []byte) *View {
	t.Helper()
	p, err := FromBytes(raw, nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rowByName(t *testing.T, rows []FunctionRow, name string) FunctionRow {
	t.Helper()
	for _, r := range rows {
		if r.Function.Name == name {
			return r
		}
	}
	t.Fatalf("no function %q", name)
	return FunctionRow{}
}

func TestFunctionsCountRecursionOncePerSample(t *testing.T) {
	rows, err := pmView(t, pmfixture.Recursive()).Functions()
	if err != nil {
		t.Fatal(err)
	}
	if a := rowByName(t, rows, "a"); a.Cum != 15 || a.Self != 5 {
		t.Errorf("a cum %d self %d", a.Cum, a.Self)
	}
	if b := rowByName(t, rows, "b"); b.Cum != 10 || b.Self != 10 {
		t.Errorf("b cum %d self %d", b.Cum, b.Self)
	}
}

func TestFunctionsKeepSameNameInTwoModulesApart(t *testing.T) {
	rows, err := pmView(t, pmfixture.SameNameTwoModules()).Functions()
	if err != nil {
		t.Fatal(err)
	}
	var foos []FunctionRow
	for _, r := range rows {
		if r.Function.Name == "foo" {
			foos = append(foos, r)
		}
	}
	if len(foos) != 2 || foos[0].Cum != 7 || foos[1].Cum != 3 || foos[0].Ref == foos[1].Ref {
		t.Fatalf("foo rows %+v", foos)
	}
}

func TestFunctionsSelfSumsToTheTotal(t *testing.T) {
	for name, v := range fixtureViews(t) {
		rows, err := v.Functions()
		if err != nil {
			t.Fatal(name, err)
		}
		var sum int64
		for _, r := range rows {
			sum += r.Self
		}
		if sum != v.Total {
			t.Errorf("%s: self sum %d != total %d", name, sum, v.Total)
		}
	}
}

func TestFunctionsOrderIsTotal(t *testing.T) {
	for name, v := range fixtureViews(t) {
		a, err := v.Functions()
		if err != nil {
			t.Fatal(err)
		}
		b, err := v.Functions()
		if err != nil {
			t.Fatal(name, err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: two calls differ", name)
		}
		for i := 1; i < len(a); i++ {
			p, q := a[i-1], a[i]
			if p.Cum < q.Cum || (p.Cum == q.Cum && p.Ref >= q.Ref) {
				t.Errorf("%s: rows %d,%d out of order", name, i-1, i)
			}
		}
	}
}

func shareString(t *testing.T, f Fact) string {
	t.Helper()
	s, ok := f.Canonical()
	if !ok {
		t.Fatalf("no canonical value for %+v", f)
	}
	return s
}

func TestSandwichCallersAndCallees(t *testing.T) {
	v := pmView(t, pmfixture.CPUBasic())
	rows, err := v.Functions()
	if err != nil {
		t.Fatal(err)
	}
	a := rowByName(t, rows, "a")
	sw, err := v.Sandwich(a.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Function.Cum != 511 || sw.Function.Self != 100 || sw.RootValue != 0 || sw.Recursive {
		t.Errorf("function %+v root %d", sw.Function, sw.RootValue)
	}
	if len(sw.Callers) != 1 || sw.Callers[0].Function.Name != "main" || sw.Callers[0].Value != 511 || shareString(t, sw.Callers[0].ShareOfFunction) != "1.000" {
		t.Errorf("callers %+v", sw.Callers)
	}
	if len(sw.Callees) != 1 || sw.Callees[0].Function.Name != "b" || sw.Callees[0].Value != 411 || shareString(t, sw.Callees[0].ShareOfFunction) != "0.804" {
		t.Errorf("callees %+v", sw.Callees)
	}
	if sw.Callees[0].ShareOfFunction.Denominator != "function:"+string(a.Ref) {
		t.Errorf("denominator %q", sw.Callees[0].ShareOfFunction.Denominator)
	}
}

func TestSandwichRecursionCountsEachEdgeOncePerSample(t *testing.T) {
	v := pmView(t, pmfixture.Recursive())
	rows, err := v.Functions()
	if err != nil {
		t.Fatal(err)
	}
	a, b := rowByName(t, rows, "a"), rowByName(t, rows, "b")
	swa, err := v.Sandwich(a.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(swa.Callers) != 1 || swa.Callers[0].Function.Name != "b" || swa.Callers[0].Value != 10 ||
		swa.RootValue != 15 || !swa.Recursive ||
		len(swa.Callees) != 1 || swa.Callees[0].Function.Name != "b" || swa.Callees[0].Value != 10 {
		t.Errorf("a: %+v", swa)
	}
	swb, err := v.Sandwich(b.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(swb.Callers) != 1 || swb.Callers[0].Function.Name != "a" || swb.Callers[0].Value != 10 ||
		len(swb.Callees) != 1 || swb.Callees[0].Function.Name != "a" || swb.Callees[0].Value != 10 {
		t.Errorf("b: %+v", swb)
	}
}

func TestEdgeValuesNeverExceedTheFunction(t *testing.T) {
	for name, v := range fixtureViews(t) {
		rows, err := v.Functions()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			sw, err := v.Sandwich(r.Ref)
			if err != nil {
				t.Fatal(name, err)
			}
			if sw.RootValue > r.Cum {
				t.Errorf("%s %s: root %d > cum %d", name, r.Function.Name, sw.RootValue, r.Cum)
			}
			for _, edges := range [][]EdgeRow{sw.Callers, sw.Callees} {
				for i := 1; i < len(edges); i++ {
					p, q := edges[i-1], edges[i]
					if p.Value < q.Value || (p.Value == q.Value && p.Ref >= q.Ref) {
						t.Errorf("%s %s: edges %d,%d out of order", name, r.Function.Name, i-1, i)
					}
				}
			}
			for _, e := range append(append([]EdgeRow{}, sw.Callers...), sw.Callees...) {
				if e.Value > r.Cum {
					t.Errorf("%s %s: edge %d > cum %d", name, r.Function.Name, e.Value, r.Cum)
				}
			}
		}
	}
}

func TestSandwichUnknownRef(t *testing.T) {
	v := pmView(t, pmfixture.CPUBasic())
	if _, err := v.Sandwich("0000000000000000"); !errors.Is(err, ErrUnknownFunction) {
		t.Fatalf("err %v", err)
	}
}

func TestFunctionsExcludeZeroValueSamples(t *testing.T) {
	v := pmView(t, pmfixture.GPUBasic())
	rows, err := v.Functions()
	if err != nil {
		t.Fatal(err)
	}
	k3 := FunctionID{Name: "[gpu:kernel:k3]"}
	for _, r := range rows {
		if r.Function.Name == k3.Name {
			t.Fatalf("zero-value-only function listed: %+v", r)
		}
	}
	launch := rowByName(t, rows, "[gpu:launch]")
	sw, err := v.Sandwich(launch.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sw.Callees {
		if e.Function.Name == k3.Name {
			t.Fatalf("zero-value edge listed: %+v", e)
		}
	}
	if _, err := v.Sandwich(RefOf(k3)); !errors.Is(err, ErrUnknownFunction) {
		t.Fatalf("err %v", err)
	}
}

func TestSandwichEdgesAreOrderedByValue(t *testing.T) {
	v := pmView(t, pmfixture.CPUBasic())
	rows, _ := v.Functions()
	sw, err := v.Sandwich(rowByName(t, rows, "main").Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.Callees) != 2 || sw.Callees[0].Function.Name != "a" || sw.Callees[0].Value != 511 ||
		sw.Callees[1].Function.Name != "c" || sw.Callees[1].Value != 489 {
		t.Fatalf("callees %+v", sw.Callees)
	}
}

func TestSandwichRecursiveComesFromTheTargetOnly(t *testing.T) {
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("a"), pmfixture.Fn("b"), pmfixture.Fn("a")}, Value: 4})
	v := pmView(t, raw)
	rows, err := v.Functions()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"main": false, "b": false, "a": true} {
		sw, err := v.Sandwich(rowByName(t, rows, name).Ref)
		if err != nil {
			t.Fatal(err)
		}
		if sw.Recursive != want {
			t.Errorf("%s: Recursive %v, want %v", name, sw.Recursive, want)
		}
	}
}

func TestSandwichEdgeTiesBreakByRef(t *testing.T) {
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("x")}, Value: 5},
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("y")}, Value: 5},
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("z")}, Value: 5})
	v := pmView(t, raw)
	rows, err := v.Functions()
	if err != nil {
		t.Fatal(err)
	}
	sw, err := v.Sandwich(rowByName(t, rows, "main").Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.Callees) != 3 {
		t.Fatalf("callees %+v", sw.Callees)
	}
	for i := 1; i < len(sw.Callees); i++ {
		if sw.Callees[i-1].Ref >= sw.Callees[i].Ref {
			t.Errorf("tied callees not in Ref order: %+v", sw.Callees)
		}
	}
}
