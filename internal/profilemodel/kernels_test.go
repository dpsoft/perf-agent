package profilemodel

import (
	"errors"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func gpuView(t *testing.T, withLog bool) *View {
	t.Helper()
	var log []byte
	if withLog {
		log = pmfixture.GPUBasicExeclog(t)
	}
	p, err := FromBytes(pmfixture.GPUBasic(), log, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// nodeNamed returns the first node in pre-order whose display name is name.
func nodeNamed(t *testing.T, v *View, name string) *Node {
	t.Helper()
	var found *Node
	v.Walk(func(n *Node) bool {
		if found == nil && n.Function.Name == name {
			found = n
		}
		return found == nil
	})
	if found == nil {
		t.Fatalf("no node %q", name)
	}
	return found
}

func TestKernelsBelowCountsExecutionsFromTheLogOnly(t *testing.T) {
	v := gpuView(t, true)
	conv := nodeNamed(t, v, "conv")
	rows, err := v.KernelsBelow(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		name   string
		val    int64
		status Status
		node   string
		total  string
		exec   int64
	}
	for i, w := range []want{
		{"k1", 300, StatusOK, "0.750", "0.600", 2},
		{"k2", 100, StatusOK, "0.250", "0.200", 1},
		{"k3", 0, StatusZero, "0.000", "0.000", 1},
	} {
		if i >= len(rows) {
			t.Fatalf("rows = %d", len(rows))
		}
		r := rows[i]
		if r.Name != w.name || *r.Value.Int != w.val || r.Value.Status != w.status || *r.Executions.Int != w.exec {
			t.Errorf("row %d = %+v", i, r)
		}
		if r.Value.Unit != "nanoseconds" || r.Value.Rounding != "exact" {
			t.Errorf("row %d value fact %+v", i, r.Value)
		}
		if got, _ := r.ShareOfNode.Canonical(); got != w.node {
			t.Errorf("row %d share of node = %q", i, got)
		}
		if got, _ := r.ShareOfTotal.Canonical(); got != w.total {
			t.Errorf("row %d share of total = %q", i, got)
		}
		if r.ShareOfNode.Denominator != "node:"+string(conv.ID) || r.ShareOfTotal.Denominator != DenominatorViewTotal {
			t.Errorf("row %d denominators %q %q", i, r.ShareOfNode.Denominator, r.ShareOfTotal.Denominator)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	pf, _ := v.Facts(conv.ID, nil)
	if int64(len(rows)) != *pf.KernelNamesObserved.Int {
		t.Fatalf("rows %d != names observed %d", len(rows), *pf.KernelNamesObserved.Int)
	}
}

func TestKernelsBelowWithoutALogSaysWhy(t *testing.T) {
	v := gpuView(t, false)
	rows, err := v.KernelsBelow(nodeNamed(t, v, "conv").ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Executions.Status != StatusUnavailable || r.Executions.Reason != ReasonNoSidecar {
			t.Errorf("%s executions = %+v", r.Name, r.Executions)
		}
	}
}

func TestKernelsBelowNeverScalesBySamplePeriod(t *testing.T) {
	v := gpuView(t, true)
	rows, err := v.KernelsBelow(nodeNamed(t, v, "conv").ID)
	if err != nil {
		t.Fatal(err)
	}
	if *rows[0].Value.Int != 300 {
		t.Fatalf("k1 = %d", *rows[0].Value.Int)
	}
}

func TestKernelsBelowTheUnsampledRoot(t *testing.T) {
	v := gpuView(t, true)
	rows, err := v.KernelsBelow(nodeNamed(t, v, "[gpu:launch unsampled]").ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "k1" || *rows[0].Value.Int != 100 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestKernelsBelowOnACPUViewIsNotGPU(t *testing.T) {
	p, err := FromBytes(pmfixture.CPUBasic(), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.KernelsBelow(v.Root.ID); !errors.Is(err, ErrNotGPUView) {
		t.Fatalf("err = %v", err)
	}
}

func TestViewNotesAndUnsampledAreViewLevel(t *testing.T) {
	v := gpuView(t, true)
	pf, err := v.Facts(v.Root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := v.Notes()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 || len(notes) != len(pf.Notes) {
		t.Fatalf("notes = %v, facts = %v", notes, pf.Notes)
	}
	for i := range notes {
		if notes[i] != pf.Notes[i] {
			t.Fatalf("note %d differs", i)
		}
	}
	u, err := v.UnsampledPortion()
	if err != nil || u.Int == nil || *u.Int != 100 {
		t.Fatalf("unsampled = %+v, %v", u, err)
	}

	p, err := FromBytes(pmfixture.CPUBasic(), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cv, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := cv.Notes(); n != nil || err != nil {
		t.Fatalf("cpu notes = %v, %v", n, err)
	}
	if u, err := cv.UnsampledPortion(); err != nil || u.Status != StatusUnavailable || u.Reason != ReasonNotGPUView {
		t.Fatalf("cpu unsampled = %+v, %v", u, err)
	}
}
