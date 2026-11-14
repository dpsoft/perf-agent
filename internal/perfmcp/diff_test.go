package perfmcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

type qDiffRow struct {
	ID             string `json:"id"`
	Parent         string `json:"parent"`
	Depth          string `json:"depth"`
	BaseID         string `json:"base_id"`
	NewID          string `json:"new_id"`
	Name           string `json:"name"`
	Presence       string `json:"presence"`
	Match          string `json:"match"`
	Interpretation string `json:"interpretation"`
	BaseCum        string `json:"base_cum"`
	NewCum         string `json:"new_cum"`
	Combined       string `json:"combined"`
	DeltaAbs       string `json:"delta_abs"`
	DeltaPerUnit   *qFact `json:"delta_per_unit"`
}

type qDiff struct {
	BaseHandle string `json:"base_handle"`
	NewHandle  string `json:"new_handle"`
	View       string `json:"view"`
	SampleType string `json:"sample_type"`
	Unit       string `json:"unit"`
	BaseTotal  string `json:"base_total"`
	NewTotal   string `json:"new_total"`
	PerUnit    struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	} `json:"per_unit"`
	Rows []qDiffRow `json:"rows"`
	pageInfo
}

// diffPair opens base and next on one server.
type diffPair struct {
	c         *testClient
	base, new opened
}

func openPair(t *testing.T, base, next []byte) diffPair {
	t.Helper()
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "base.pb.gz", base)
	writeFixture(t, cfg.ProfilesDir, "new.pb.gz", next)
	c := newTestServer(t, cfg)
	var b, n openOut
	c.callOK("open", map[string]any{"path": "base.pb.gz"}, &b)
	c.callOK("open", map[string]any{"path": "new.pb.gz"}, &n)
	return diffPair{c: c, base: opened{c, b.Handle, b.DefaultView}, new: opened{c, n.Handle, n.DefaultView}}
}

func (p diffPair) args(extra map[string]any) map[string]any {
	m := map[string]any{"base_handle": p.base.handle, "base_view": p.base.view, "new_handle": p.new.handle, "new_view": p.new.view}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (p diffPair) diff(t *testing.T, extra map[string]any) (qDiff, string) {
	t.Helper()
	var out qDiff
	text := p.c.callOK("diff", p.args(extra), &out)
	return out, text
}

func TestDiffRowsAreFlatWithParentIDs(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	out, _ := p.diff(t, map[string]any{"limit": 500})
	ids := map[string]bool{}
	for _, r := range out.Rows {
		if r.ID == "" {
			t.Errorf("row without id: %+v", r)
		}
		ids[r.ID] = true
	}
	roots := 0
	for _, r := range out.Rows {
		if r.Parent == "" {
			roots++
		} else if !ids[r.Parent] {
			t.Errorf("row %s has parent %q outside the flat list", r.ID, r.Parent)
		}
	}
	if roots != 1 || out.TotalRows != "6" || len(out.Rows) != 6 {
		t.Errorf("roots=%d total=%s rows=%d", roots, out.TotalRows, len(out.Rows))
	}
	found := false
	for _, r := range out.Rows {
		if r.Name == "d" {
			found = r.Presence == "only_new"
		}
	}
	if !found {
		t.Error("main;d is not only_new")
	}
	if out.BaseHandle != p.base.handle || out.NewHandle != p.new.handle || out.SampleType != "cpu" || out.BaseTotal != "1000" || out.NewTotal != "1000" || out.OmittedWeightBasis != "abs_delta" {
		t.Errorf("header = %+v", out)
	}
}

func TestDiffSortsByAbsoluteDeltaAndTruncates(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	out, _ := p.diff(t, map[string]any{"limit": 2})
	var got []string
	for _, r := range out.Rows {
		got = append(got, r.Name+"="+r.BaseCum+">"+r.NewCum+"/"+r.Combined)
	}
	if want := []string{"a=511>400/911", "b=411>300/711"}; !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	if out.OmittedCount != "4" || out.OmittedWeight != "111" || out.NextCursor == "" || out.Skipped != "0" {
		t.Errorf("page = %+v", out.pageInfo)
	}
	next, _ := p.diff(t, map[string]any{"limit": 2, "cursor": out.NextCursor})
	if len(next.Rows) != 2 || next.Skipped != "2" || next.Rows[0].Name != "d" {
		t.Errorf("next = %+v", next.Rows)
	}
}

func TestDiffCursorIsBoundToBothSidesAndWork(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	out, _ := p.diff(t, map[string]any{"limit": 2})
	if text, isErr := p.c.call("diff", p.args(map[string]any{"limit": 2, "cursor": out.NextCursor, "work_base": 2})); !isErr {
		t.Errorf("cursor accepted with another work_base: %s", text)
	}
	swapped := map[string]any{"base_handle": p.new.handle, "base_view": p.new.view, "new_handle": p.base.handle, "new_view": p.base.view, "limit": 2, "cursor": out.NextCursor}
	if text, isErr := p.c.call("diff", swapped); !isErr {
		t.Errorf("cursor accepted with swapped sides: %s", text)
	}
}

func TestDiffPerUnitNeedsBothWorkCounts(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	out, text := p.diff(t, map[string]any{"limit": 500})
	if out.PerUnit.OK || out.PerUnit.Reason != "no work count" || strings.Contains(text, "delta_per_unit") {
		t.Errorf("per_unit = %+v, delta_per_unit present: %v", out.PerUnit, strings.Contains(text, "delta_per_unit"))
	}
	out, _ = p.diff(t, map[string]any{"limit": 500, "work_base": 10, "work_new": 10})
	if !out.PerUnit.OK {
		t.Fatalf("per_unit = %+v", out.PerUnit)
	}
	for _, r := range out.Rows {
		if r.DeltaPerUnit == nil || r.DeltaPerUnit.Value == nil {
			t.Fatalf("row %s has no delta_per_unit", r.Name)
		}
	}
	for _, bad := range []map[string]any{{"work_base": 0, "work_new": 1}, {"work_base": -1, "work_new": 1}} {
		if text, isErr := p.c.call("diff", p.args(bad)); !isErr || !strings.Contains(text, "work_") {
			t.Errorf("%v: %v %s", bad, isErr, text)
		}
	}
}

func jsonDepth(t *testing.T, text string) int {
	t.Helper()
	depth, deepest := 0, 0
	inStr, esc := false, false
	for i := range len(text) {
		c := text[i]
		switch {
		case esc:
			esc = false
		case inStr:
			esc = c == '\\'
			inStr = c != '"'
		case c == '"':
			inStr = true
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case c == '}' || c == ']':
			depth--
		}
	}
	return deepest
}

func TestDiffOfDeepStacksStaysFlat(t *testing.T) {
	p := openPair(t, pmfixture.Deep(600), pmfixture.DeepNew(600))
	out, text := p.diff(t, map[string]any{"limit": 500})
	if len(out.Rows) != 500 {
		t.Fatalf("rows = %d", len(out.Rows))
	}
	if d := jsonDepth(t, text); d > 8 {
		t.Errorf("JSON nesting %d", d)
	}
	if !json.Valid([]byte(text)) {
		t.Error("invalid JSON")
	}
}

func TestDiffRefusesIncompatibleViews(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.GPUBasic())
	text, isErr := p.c.call("diff", p.args(nil))
	if !isErr || !strings.Contains(text, "cpu/nanoseconds#0") || !strings.Contains(text, "gpu/nanoseconds#0") {
		t.Errorf("%v %s", isErr, text)
	}
}

func TestDiffHugeValues(t *testing.T) {
	p := openPair(t, pmfixture.HugeValue(), pmfixture.HugeValue())
	out, _ := p.diff(t, nil)
	if out.BaseTotal != "9007199254740993" || out.NewTotal != "9007199254740993" {
		t.Errorf("totals = %s %s", out.BaseTotal, out.NewTotal)
	}
}

func TestDiffLimitIsBoundedByMaxDiffRows(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	if text, isErr := p.c.call("diff", p.args(map[string]any{"limit": 501})); !isErr || !strings.Contains(text, "MaxDiffRows") {
		t.Errorf("%v %s", isErr, text)
	}
}

func TestDiffCacheIsPerServerAndBounded(t *testing.T) {
	p := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	for i := range 6 {
		p.diff(t, map[string]any{"work_base": i + 1, "work_new": 1})
	}
	if n := len(p.c.srv.diffs.m); n != p.c.srv.lim.MaxCachedDiffs || n != 4 {
		t.Errorf("cached diffs = %d", n)
	}
	q := openPair(t, pmfixture.CPUBasic(), pmfixture.CPUBasicNew())
	if len(q.c.srv.diffs.m) != 0 {
		t.Error("a new server starts with cached diffs")
	}
}
