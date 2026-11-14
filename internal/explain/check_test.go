package explain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

func viewOf(t *testing.T, raw, log []byte) *profilemodel.View {
	t.Helper()
	p, err := profilemodel.FromBytes(raw, log, profilemodel.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(profilemodel.ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// nodeID finds the node whose display-name path, root first, is names.
func nodeID(t *testing.T, v *profilemodel.View, names ...string) profilemodel.NodeID {
	t.Helper()
	var found []profilemodel.NodeID
	v.Walk(func(n *profilemodel.Node) bool {
		var path []string
		for _, q := range v.Path(n) {
			path = append(path, q.Function.Name)
		}
		if slices.Equal(path, names) {
			found = append(found, n.ID)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("%d nodes at path %q", len(found), names)
	}
	return found[0]
}

// check decodes js with the default limits and checks it against v.
func check(t *testing.T, v *profilemodel.View, js string) (Result, []Notice) {
	t.Helper()
	return checkWith(t, v, js, DefaultLimits())
}

func checkWith(t *testing.T, v *profilemodel.View, js string, lim Limits) (Result, []Notice) {
	t.Helper()
	exps, notices, err := Decode(json.RawMessage(js), lim)
	if err != nil {
		t.Fatalf("Decode: %v\n%s", err, js)
	}
	return Check(v, exps, lim), notices
}

// one is the JSON of one explanation of node with the given claims. An
// empty prose places every claim with a well-formed id ("{c1} {c2}"), so
// a test about one claim sees only that claim's failures.
func one(node profilemodel.NodeID, prose string, claims ...string) string {
	if prose == "" {
		var ids []string
		for _, c := range claims {
			var cl struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(c), &cl) == nil && validClaimID(cl.ID) && !slices.Contains(ids, "{"+cl.ID+"}") {
				ids = append(ids, "{"+cl.ID+"}")
			}
		}
		prose = strings.Join(ids, " ")
	}
	return fmt.Sprintf(`[{"node": %q, "claims": [%s], "prose": %q}]`, node, strings.Join(claims, ", "), prose)
}

func mustOK(t *testing.T, r Result) {
	t.Helper()
	if !r.OK || len(r.Failures) != 0 {
		t.Fatalf("not OK: %+v", r.Failures)
	}
}

// onlyFailure asserts r failed with exactly one failure, of code, and
// returns it.
func onlyFailure(t *testing.T, r Result, code Code) Failure {
	t.Helper()
	if r.OK {
		t.Fatalf("OK, want %s", code)
	}
	if len(r.Failures) != 1 || r.Failures[0].Code != code {
		t.Fatalf("failures = %+v, want one %s", r.Failures, code)
	}
	if r.Checked != nil {
		t.Errorf("Checked = %+v on a failed check, want nil", r.Checked)
	}
	return r.Failures[0]
}

func cpuBasic(t *testing.T) (*profilemodel.View, profilemodel.NodeID) {
	t.Helper()
	v := viewOf(t, pmfixture.CPUBasic(), nil)
	return v, nodeID(t, v, "main", "a", "b")
}

func TestACorrectClaimPasses(t *testing.T) {
	v, b := cpuBasic(t)
	for _, value := range []string{`0.411`, `0.4114`, `"0.411"`, `0.4105`, `4.11e-1`} {
		r, notices := check(t, v, one(b, "{c1} of the time is under {n:"+string(b)+"}",
			fmt.Sprintf(`{"id": "c1", "metric": "share_of_total", "node": %q, "denominator": "view_total", "unit": "ratio", "value": %s}`, b, value)))
		mustOK(t, r)
		if len(notices) != 0 {
			t.Errorf("notices = %v", notices)
		}
		if r.Figures != 1 || r.References != 1 {
			t.Errorf("value %s: Figures %d References %d, want 1 and 1", value, r.Figures, r.References)
		}
		if len(r.Checked) != 1 {
			t.Fatalf("Checked = %+v", r.Checked)
		}
		ch := r.Checked[0]
		if ch.Node != b || ch.Facts.Node != b || ch.Facts.Cumulative.Int == nil || *ch.Facts.Cumulative.Int != 411 {
			t.Errorf("Checked node/facts = %s / %+v", ch.Node, ch.Facts.Cumulative)
		}
		want := CheckedClaim{ID: "c1", EngineValue: EngineValue{
			Metric: "share_of_total", Scope: "node", Node: b, Denominator: "view_total", Unit: "ratio",
			Value: "0.411", Rounding: "0.001 ratio", Status: profilemodel.StatusOK,
		}}
		if len(ch.Claims) != 1 || !engineEqual(ch.Claims[0].EngineValue, want.EngineValue) || ch.Claims[0].ID != "c1" {
			t.Errorf("claims = %+v, want %+v", ch.Claims, want)
		}
		if len(ch.Prose) != 3 || ch.Prose[0].Kind != SegmentClaim || ch.Prose[2].Kind != SegmentNode {
			t.Errorf("prose segments = %+v", ch.Prose)
		}
	}
}

func engineEqual(a, b EngineValue) bool {
	am, bm := a.Module, b.Module
	a.Module, b.Module = nil, nil
	return a == b && ((am == nil) == (bm == nil)) && (am == nil || *am == *bm)
}

func TestAClaimWithoutANodeIsAboutTheExplainedNode(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": 411}`))
	mustOK(t, r)
	if got := r.Checked[0].Claims[0].Node; got != b {
		t.Errorf("claim node = %s, want %s", got, b)
	}
}

func TestAWrongValueIsRejectedWithTheEngineValue(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.62}`))
	f := onlyFailure(t, r, CodeValueMismatch)
	if f.Engine == nil || f.Engine.Value != "0.411" || f.Agent != "0.62" || f.Claim != "c1" || f.Explanation != 0 {
		t.Errorf("failure = %+v engine %+v", f, f.Engine)
	}
	if f.Engine.Rounding != "0.001 ratio" || f.Engine.Node != b {
		t.Errorf("engine = %+v", f.Engine)
	}
}

func TestTheRightValueWithTheWrongParentIsRejected(t *testing.T) {
	v, b := cpuBasic(t)
	main, a := nodeID(t, v, "main"), nodeID(t, v, "main", "a")
	r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": "share_of_parent", "denominator": "parent:%s", "unit": "ratio", "value": 0.804}`, main)))
	f := onlyFailure(t, r, CodeWrongDenominator)
	if f.Engine == nil || f.Engine.Denominator != "parent:"+string(a) || f.Engine.Value != "0.804" {
		t.Errorf("engine = %+v", f.Engine)
	}
	// The same claim naming the real parent passes.
	r, _ = check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": "share_of_parent", "denominator": "parent:%s", "unit": "ratio", "value": 0.804}`, a)))
	mustOK(t, r)
}

func TestADenominatorIsRequiredExactlyWhenTheFactHasOne(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "share_of_total", "unit": "ratio", "value": 0.411}`))
	onlyFailure(t, r, CodeWrongDenominator)
	r, _ = check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "denominator": "view_total", "unit": "nanoseconds", "value": 411}`))
	if f := onlyFailure(t, r, CodeWrongDenominator); !strings.Contains(f.Message, "no denominator") {
		t.Errorf("message = %q", f.Message)
	}
}

func TestAClaimOnAnUnavailableFactIsRejected(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "executions_below", "unit": "count", "value": 1}`))
	f := onlyFailure(t, r, CodeUnavailableFact)
	if !strings.Contains(f.Message, "no sidecar") || f.Engine == nil || f.Engine.Status != profilemodel.StatusUnavailable || f.Engine.Reason != "no sidecar" || f.Engine.Value != "" {
		t.Errorf("failure = %+v engine %+v", f, f.Engine)
	}
	// share_of_parent of the synthetic root has no parent.
	r, _ = check(t, v, one(v.Root.ID, "", `{"id": "c1", "metric": "share_of_parent", "unit": "ratio", "value": 1}`))
	if f := onlyFailure(t, r, CodeUnavailableFact); !strings.Contains(f.Message, "no parent") {
		t.Errorf("message = %q", f.Message)
	}
}

func TestNonFiniteValuesAreRejected(t *testing.T) {
	v, b := cpuBasic(t)
	for _, value := range []string{"NaN", "Inf", "-Inf", "+Inf", "nan", "-infinity", "+NAN", "INFINITY"} {
		r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": %q}`, value)))
		if f := onlyFailure(t, r, CodeNonFinite); f.Agent != value || f.Engine == nil || f.Engine.Value != "0.411" {
			t.Errorf("%s: failure = %+v", value, f)
		}
	}
}

func TestValuesOutsideTheDecimalGrammarAreBadValues(t *testing.T) {
	v, b := cpuBasic(t)
	long := strings.Repeat("1", 41)
	for _, value := range []string{
		`"1/3"`, `"0x10"`, `"0b1"`, `"1_000"`, `" 411"`, `"411 "`, `""`, `".5"`, `"5."`, `"1e1000"`, `"1e+"`,
		`"` + long + `"`, long, `"0.` + long + `"`, `"--1"`, `true`, `null`, `{}`, `[411]`, `"４１１"`,
	} {
		r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": %s}`, value)))
		onlyFailure(t, r, CodeBadValue)
	}
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "unit": "nanoseconds"}`))
	if f := onlyFailure(t, r, CodeBadValue); !strings.Contains(f.Message, "no value") {
		t.Errorf("message = %q", f.Message)
	}
	// The grammar's edges are accepted.
	forty := strings.Repeat("0", 39) + "411"
	for _, value := range []string{`"+411"`, `"411.000"`, `"4.11e2"`, `"4110E-1"`, `"` + forty[len(forty)-40:] + `"`, `"411.` + strings.Repeat("0", 40) + `"`, `"41100e-002"`} {
		r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": %s}`, value)))
		if !r.OK {
			t.Errorf("value %s: %+v", value, r.Failures)
		}
	}
}

func TestToleranceAndRoundingAreIgnored(t *testing.T) {
	v, b := cpuBasic(t)
	r, notices := check(t, v, one(b, "", `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.42, "tolerance": 0.05, "rounding": "1 ratio"}`))
	onlyFailure(t, r, CodeValueMismatch)
	want := "ignored: the engine's rounding for share_of_total is 0.001 ratio"
	if len(notices) != 2 || notices[0].Message != want || notices[1].Message != want {
		t.Errorf("notices = %+v", notices)
	}
	r, notices = check(t, v, one(b, "", `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.411, "rounding": "1 ratio"}`))
	mustOK(t, r)
	if len(notices) != 1 || notices[0].Path != "explanations[0].claims[0].rounding" {
		t.Errorf("notices = %+v", notices)
	}
}

func TestDuplicateIDsAndUnresolvedRefsAreRejected(t *testing.T) {
	v, b := cpuBasic(t)
	claim := `{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": 411}`
	r, _ := check(t, v, one(b, "", claim, claim))
	if f := onlyFailure(t, r, CodeDuplicateClaimID); f.Claim != "c1" || !strings.Contains(f.Message, "explanations[0].claims[0]") {
		t.Errorf("failure = %+v", f)
	}
	// Across explanations too.
	r, _ = check(t, v, fmt.Sprintf(`[{"node": %q, "claims": [%s], "prose": "{c1}"}, {"node": %q, "claims": [%s]}]`, b, claim, b, claim))
	if f := onlyFailure(t, r, CodeDuplicateClaimID); f.Explanation != 1 {
		t.Errorf("failure = %+v", f)
	}

	r, _ = check(t, v, one(b, "see {c9} and {c1}", claim))
	if f := onlyFailure(t, r, CodeUnresolvedClaimRef); f.Ref != "c9" {
		t.Errorf("failure = %+v", f)
	}
	r, _ = check(t, v, fmt.Sprintf(`[{"node": %q, "claims": [%s], "prose": "{c1}", "hypotheses": ["{n:0000000000000000}"]}]`, b, claim))
	if f := onlyFailure(t, r, CodeUnresolvedNodeRef); f.Ref != "0000000000000000" {
		t.Errorf("failure = %+v", f)
	}
	r, _ = check(t, v, `[{"node": "nope", "next_measurement": "x"}]`)
	if f := onlyFailure(t, r, CodeUnknownNode); f.Ref != "nope" {
		t.Errorf("failure = %+v", f)
	}
	r, _ = check(t, v, `[{"prose": "no node"}]`)
	onlyFailure(t, r, CodeUnknownNode)
	r, _ = check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "node": "nope", "unit": "nanoseconds", "value": 411}`))
	if f := onlyFailure(t, r, CodeUnknownNode); f.Claim != "c1" || f.Ref != "nope" {
		t.Errorf("failure = %+v", f)
	}

	// Every failure is reported, not only the first.
	r, _ = check(t, v, fmt.Sprintf(`[{"node": "nope", "claims": [%s, %s, {"id": "1x"}], "prose": "{c9} {n:0000000000000000}", "hypotheses": ["{c8}"], "next_measurement": "{c7}"}]`, claim, claim))
	var codes []Code
	for _, f := range r.Failures {
		codes = append(codes, f.Code)
	}
	want := []Code{CodeUnknownNode, CodeUnknownNode, CodeDuplicateClaimID, CodeBadClaimID,
		CodeUnresolvedClaimRef, CodeUnresolvedNodeRef, CodeUnresolvedClaimRef, CodeUnresolvedClaimRef, CodeUnplacedClaim}
	if r.OK || !slices.Equal(codes, want) {
		t.Errorf("codes = %v, want %v", codes, want)
	}
}

func TestBadClaimIDs(t *testing.T) {
	v, b := cpuBasic(t)
	for _, id := range []string{"", "1c", "c 1", "c:1", "c.1", strings.Repeat("c", 33), "cé"} {
		r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": %q, "metric": "cumulative", "unit": "nanoseconds", "value": 411}`, id)))
		if f := onlyFailure(t, r, CodeBadClaimID); f.Claim != id || !strings.Contains(f.Message, "claims[0]") {
			t.Errorf("id %q: failure = %+v", id, f)
		}
	}
	r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": %q, "metric": "cumulative", "unit": "nanoseconds", "value": 411}`, "C_"+strings.Repeat("9", 30))))
	mustOK(t, r)
}

func gpuBasic(t *testing.T, log bool) (*profilemodel.View, profilemodel.NodeID) {
	t.Helper()
	var l []byte
	if log {
		l = pmfixture.GPUBasicExeclog(t)
	}
	v := viewOf(t, pmfixture.GPUBasic(), l)
	return v, nodeID(t, v, "train", "conv")
}

func TestViewLevelFactsAreNotNodeClaims(t *testing.T) {
	v, conv := gpuBasic(t, false)
	r, _ := check(t, v, one(conv, "", `{"id": "c1", "metric": "unsampled_portion", "unit": "nanoseconds", "value": "100"}`))
	if f := onlyFailure(t, r, CodeWrongScope); f.Message != `unsampled_portion is a view-level fact: claim it with "scope": "view" and no node` {
		t.Errorf("message = %q", f.Message)
	}
	// Naming the node explicitly is the same claim.
	r, _ = check(t, v, one(conv, "", fmt.Sprintf(`{"id": "c1", "metric": "unsampled_portion", "scope": "node", "node": %q, "unit": "nanoseconds", "value": "100"}`, conv)))
	onlyFailure(t, r, CodeWrongScope)

	r, _ = check(t, v, one(conv, "", `{"id": "c1", "metric": "unsampled_portion", "scope": "view", "unit": "nanoseconds", "value": "100"}`))
	mustOK(t, r)
	if ev := r.Checked[0].Claims[0].EngineValue; ev.Scope != "view" || ev.Node != "" || ev.Value != "100" || ev.Rounding != "exact" {
		t.Errorf("engine value = %+v", ev)
	}

	// A view-level claim takes no node, and a node-level metric is not a
	// view-level claim.
	r, _ = check(t, v, one(conv, "", fmt.Sprintf(`{"id": "c1", "metric": "unsampled_portion", "scope": "view", "node": %q, "unit": "nanoseconds", "value": "100"}`, conv)))
	onlyFailure(t, r, CodeWrongScope)
	r, _ = check(t, v, one(conv, "", `{"id": "c1", "metric": "cumulative", "scope": "view", "unit": "nanoseconds", "value": "400"}`))
	onlyFailure(t, r, CodeWrongScope)
	r, _ = check(t, v, one(conv, "", `{"id": "c1", "metric": "cumulative", "scope": "path", "unit": "nanoseconds", "value": "400"}`))
	onlyFailure(t, r, CodeWrongScope)

	// Off GPU the view-level fact is unavailable.
	cpu, b := cpuBasic(t)
	r, _ = check(t, cpu, one(b, "", `{"id": "c1", "metric": "unsampled_portion", "scope": "view", "unit": "nanoseconds", "value": "0"}`))
	if f := onlyFailure(t, r, CodeUnavailableFact); !strings.Contains(f.Message, "not a GPU view") {
		t.Errorf("message = %q", f.Message)
	}
}

func TestQualityClaimsAtViewAndNodeScope(t *testing.T) {
	v, conv := gpuBasic(t, false)
	// The view: exact join 400 of 500; conv: 300 of its 400.
	r, _ := check(t, v, one(conv, "",
		`{"id": "v1", "metric": "quality_join_exact", "scope": "view", "denominator": "view_total", "unit": "ratio", "value": 0.8}`,
		fmt.Sprintf(`{"id": "n1", "metric": "quality_join_exact", "denominator": "node:%s", "unit": "ratio", "value": 0.75}`, conv),
		fmt.Sprintf(`{"id": "n2", "metric": "quality_serialized_true", "denominator": "node:%s", "unit": "ratio", "value": 0.25}`, conv),
		`{"id": "n3", "metric": "quality_unsampled_launches", "scope": "view", "denominator": "view_total", "unit": "ratio", "value": 0.2}`,
	))
	mustOK(t, r)
	r, _ = check(t, v, one(conv, "",
		`{"id": "n1", "metric": "quality_join_exact", "denominator": "view_total", "unit": "ratio", "value": 0.75}`))
	onlyFailure(t, r, CodeWrongDenominator)
}

func TestGPUNodeFacts(t *testing.T) {
	v, conv := gpuBasic(t, true)
	r, _ := check(t, v, one(conv, "",
		`{"id": "c1", "metric": "executions_below", "unit": "count", "value": 4}`,
		`{"id": "c2", "metric": "kernel_names_observed", "unit": "count", "value": "3"}`,
		`{"id": "c3", "metric": "serialization_true", "unit": "nanoseconds", "value": 100}`,
		`{"id": "c4", "metric": "serialization_false", "unit": "nanoseconds", "value": 300}`,
		`{"id": "c5", "metric": "serialization_unknown", "unit": "nanoseconds", "value": 0}`,
		`{"id": "c6", "metric": "serialization_missing", "unit": "nanoseconds", "value": 0}`,
		`{"id": "c7", "metric": "inexact_portion", "unit": "nanoseconds", "value": 100}`,
		`{"id": "c8", "metric": "self", "unit": "nanoseconds", "value": 0}`,
	))
	mustOK(t, r)
	if r.Figures != 8 {
		t.Errorf("Figures = %d", r.Figures)
	}
	if ev := r.Checked[0].Claims[4].EngineValue; ev.Status != profilemodel.StatusZero || ev.Value != "0" {
		t.Errorf("zero fact = %+v", ev)
	}
}

func TestHugeIntegersCompareExactly(t *testing.T) {
	v := viewOf(t, pmfixture.HugeValue(), nil)
	big := nodeID(t, v, "main", "big")
	for _, value := range []string{`"9007199254740993"`, `9007199254740993`, `9007199254740993.0`} {
		r, _ := check(t, v, one(big, "", fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": %s}`, value)))
		mustOK(t, r)
		if got := r.Checked[0].Claims[0].Value; got != "9007199254740993" {
			t.Errorf("engine value = %s", got)
		}
	}
	r, _ := check(t, v, one(big, "", `{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": 9007199254740992}`))
	if f := onlyFailure(t, r, CodeValueMismatch); f.Engine.Value != "9007199254740993" || f.Agent != "9007199254740992" {
		t.Errorf("failure = %+v", f)
	}
}

func TestRangeAndIntegerChecks(t *testing.T) {
	v, b := cpuBasic(t)
	share := func(value string) string {
		return fmt.Sprintf(`{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": %s}`, value)
	}
	cum := func(value string) string {
		return fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": %s}`, value)
	}
	for _, c := range []struct {
		claim string
		code  Code
	}{
		{share("1.2"), CodeOutOfRange},
		{share("1.0004"), CodeOutOfRange},
		{share("-0.0001"), CodeOutOfRange},
		{cum("-1"), CodeOutOfRange},
		{cum("-1.5"), CodeOutOfRange},
		{cum("1.5"), CodeNotInteger},
		{cum("411.5"), CodeNotInteger},
		{cum("4.115e2"), CodeNotInteger},
	} {
		r, _ := check(t, v, one(b, "", c.claim))
		if f := onlyFailure(t, r, c.code); f.Engine == nil {
			t.Errorf("%s: no engine value", c.claim)
		}
	}
	// A negative zero is zero, in range and equal to a zero fact.
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "inexact_portion", "unit": "nanoseconds", "value": -0}`))
	mustOK(t, r)
}

func TestUnitMustMatch(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "unit": "ms", "value": 411}`))
	if f := onlyFailure(t, r, CodeWrongUnit); f.Engine == nil || f.Engine.Unit != "nanoseconds" {
		t.Errorf("failure = %+v", f)
	}
	r, _ = check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "value": 411}`))
	onlyFailure(t, r, CodeWrongUnit)
	r, _ = check(t, v, one(b, "", `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "percent", "value": 41.1}`))
	onlyFailure(t, r, CodeWrongUnit)
}

func TestDiffMetricsAreNotClaimable(t *testing.T) {
	v, b := cpuBasic(t)
	for _, m := range []string{"share_delta", "per_work_delta"} {
		r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": %q, "unit": "ratio", "value": 0}`, m)))
		onlyFailure(t, r, CodeDiffMetric)
	}
	for _, m := range []string{"", "made_up", "Cumulative", "quality_", "serialization"} {
		r, _ := check(t, v, one(b, "", fmt.Sprintf(`{"id": "c1", "metric": %q, "unit": "ratio", "value": 0}`, m)))
		onlyFailure(t, r, CodeUnknownMetric)
	}
}

// unresolvedProfile is main;<bare address on the no-file mapping> = 3 and
// main;ok = 1: one unresolved frame, under the "" (no module) key.
func unresolvedProfile() []byte {
	return pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), {Addr: 0x40}}, Value: 3},
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("ok")}, Value: 1},
	)
}

func TestModuleOnlyForUnresolvedByModule(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "quality_unresolved_by_module", "scope": "view", "denominator": "view_total", "unit": "ratio", "value": 0}`))
	onlyFailure(t, r, CodeUnknownModule)
	r, _ = check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "module": "/opt/app/bin/app", "unit": "nanoseconds", "value": 411}`))
	onlyFailure(t, r, CodeModuleNotApplicable)
	r, _ = check(t, v, one(b, "", `{"id": "c1", "metric": "cumulative", "module": "", "unit": "nanoseconds", "value": 411}`))
	onlyFailure(t, r, CodeModuleNotApplicable)

	// The "" key is a module like any other once it is stated.
	u := viewOf(t, unresolvedProfile(), nil)
	main := nodeID(t, u, "main")
	r, _ = check(t, u, one(main, "", `{"id": "c1", "metric": "quality_unresolved_by_module", "scope": "view", "module": "", "denominator": "view_total", "unit": "ratio", "value": 0.75}`))
	mustOK(t, r)
	if m := r.Checked[0].Claims[0].Module; m == nil || *m != "" {
		t.Errorf("engine module = %v", m)
	}
	r, _ = check(t, u, one(main, "", `{"id": "c1", "metric": "quality_unresolved_by_module", "scope": "view", "denominator": "view_total", "unit": "ratio", "value": 0.75}`))
	if f := onlyFailure(t, r, CodeUnknownModule); !strings.Contains(f.Message, `""`) {
		t.Errorf("message does not list the keys: %q", f.Message)
	}
	r, _ = check(t, u, one(main, "", `{"id": "c1", "metric": "quality_unresolved_by_module", "scope": "view", "module": "libz", "denominator": "view_total", "unit": "ratio", "value": 0.75}`))
	onlyFailure(t, r, CodeUnknownModule)
	r, _ = check(t, u, one(main, "", fmt.Sprintf(`{"id": "c1", "metric": "quality_unresolved_by_module", "module": "", "denominator": "node:%s", "unit": "ratio", "value": 0.75}`, main)))
	mustOK(t, r)
}

func TestLimitsAreEnforced(t *testing.T) {
	v, b := cpuBasic(t)
	claim := func(id string) string {
		return fmt.Sprintf(`{"id": %q, "metric": "cumulative", "unit": "nanoseconds", "value": 411}`, id)
	}
	cases := []struct {
		name string
		set  func(*Limits)
		js   string
	}{
		{"MaxProseBytes", func(l *Limits) { l.MaxProseBytes = 8 }, one(b, "nine byte")},
		{"MaxExplanations", func(l *Limits) { l.MaxExplanations = 1 }, fmt.Sprintf(`[{"node": %q}, {"node": %q}]`, b, b)},
		{"MaxClaimsPerExplanation", func(l *Limits) { l.MaxClaimsPerExplanation = 1 }, one(b, "", claim("c1"), claim("c2"))},
		{"MaxClaimsTotal", func(l *Limits) { l.MaxClaimsTotal = 2 }, fmt.Sprintf(`[{"node": %q, "claims": [%s, %s], "prose": "{c1} {c2}"}, {"node": %q, "claims": [%s], "prose": "{c3}"}]`, b, claim("c1"), claim("c2"), b, claim("c3"))},
		{"MaxHypotheses", func(l *Limits) { l.MaxHypotheses = 1 }, fmt.Sprintf(`[{"node": %q, "hypotheses": ["a", "b"]}]`, b)},
		{"MaxHypothesisBytes", func(l *Limits) { l.MaxHypothesisBytes = 3 }, fmt.Sprintf(`[{"node": %q, "hypotheses": ["abc", "abcd"]}]`, b)},
		{"MaxNextBytes", func(l *Limits) { l.MaxNextBytes = 3 }, fmt.Sprintf(`[{"node": %q, "next_measurement": "abcd"}]`, b)},
	}
	for _, c := range cases {
		lim := DefaultLimits()
		c.set(&lim)
		r, _ := checkWith(t, v, c.js, lim)
		if f := onlyFailure(t, r, CodeLimitExceeded); f.Ref != c.name || !strings.Contains(f.Message, c.name) {
			t.Errorf("%s: failure = %+v", c.name, f)
		}
		// Within the default limits the same input passes.
		r, _ = checkWith(t, v, c.js, DefaultLimits())
		mustOK(t, r)
	}
}

func TestDefaultLimits(t *testing.T) {
	want := Limits{MaxExplanations: 64, MaxClaimsPerExplanation: 32, MaxClaimsTotal: 512, MaxProseBytes: 8192,
		MaxHypotheses: 8, MaxHypothesisBytes: 2048, MaxNextBytes: 2048, MaxTitleBytes: 200}
	if got := DefaultLimits(); got != want {
		t.Errorf("DefaultLimits() = %+v", got)
	}
}

func TestReferencesCountDistinctNodes(t *testing.T) {
	v, b := cpuBasic(t)
	a := nodeID(t, v, "main", "a")
	js := fmt.Sprintf(`[{"node": %q, "prose": "{n:%s} {n:%s} {n:%s}", "hypotheses": ["{n:%s}"]}, {"node": %q, "next_measurement": "{n:%s}"}]`, b, b, a, b, a, a, b)
	r, _ := check(t, v, js)
	mustOK(t, r)
	if r.Figures != 0 || r.References != 2 {
		t.Errorf("Figures %d References %d, want 0 and 2", r.Figures, r.References)
	}
	if len(r.Checked) != 2 || len(r.Checked[0].Hypotheses) != 1 || r.Checked[1].NextMeasurement[0].Node != b {
		t.Errorf("checked = %+v", r.Checked)
	}
}

func TestResultJSON(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "", `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.62}`))
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"ok":false,"failures":[{"explanation":"0","claim":"c1","code":"value_mismatch","message":"share_of_total is 0.411 at the engine's rounding (0.001 ratio); 0.62 is 0.620 at that rounding","agent":"0.62","engine":{"metric":"share_of_total","scope":"node","node":"%s","denominator":"view_total","unit":"ratio","value":"0.411","rounding":"0.001 ratio","status":"ok"}}],"figures":"1","references":"0"}`, b)
	if string(raw) != want {
		t.Errorf("JSON\n got %s\nwant %s", raw, want)
	}
}

func TestTextGluedToAClaimIsRefused(t *testing.T) {
	v, b := cpuBasic(t)
	claim := `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.411}`
	for _, prose := range []string{"-{c1}", "+{c1}", "\u2212{c1}", "{c1}e3", "{c1}E3", "{c1}.5", "{c1},5", "{c1}5", "5{c1}", "1.{c1}", "{c1}%", "{c1}e", "x{c1}y"} {
		r, _ := check(t, v, one(b, prose, claim))
		if f := onlyFailure(t, r, CodeGluedToClaim); f.Ref != "c1" || !strings.Contains(f.Message, "{c1}") {
			t.Errorf("%q: failure = %+v", prose, f)
		}
	}
	for _, prose := range []string{"{c1} %", "{c1} ms", "({c1})", "was {c1}.", "{c1}, then", "{c1}.x", "a-b {c1}", "x{c1} y"} {
		r, _ := check(t, v, one(b, prose, claim))
		if !r.OK {
			t.Errorf("%q: %+v", prose, r.Failures)
		}
	}
	// In hypotheses and next measurements too.
	r, _ := check(t, v, fmt.Sprintf(`[{"node": %q, "claims": [%s], "hypotheses": ["-{c1}"], "next_measurement": "{c1}e2"}]`, b, claim))
	if len(r.Failures) != 2 || r.Failures[0].Code != CodeGluedToClaim || r.Failures[1].Code != CodeGluedToClaim {
		t.Errorf("failures = %+v", r.Failures)
	}
}

func TestClaimRefsStayInTheirExplanation(t *testing.T) {
	v, b := cpuBasic(t)
	a := nodeID(t, v, "main", "a")
	js := fmt.Sprintf(`[{"node": %q, "claims": [{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": 411}], "prose": "{c1}"},
		{"node": %q, "prose": "as {c1} said"}]`, b, a)
	r, _ := check(t, v, js)
	f := onlyFailure(t, r, CodeUnresolvedClaimRef)
	if f.Explanation != 1 || f.Ref != "c1" || !strings.Contains(f.Message, "explanations[0]") || !strings.Contains(f.Message, "{n:<id>}") {
		t.Errorf("failure = %+v", f)
	}
	r, _ = check(t, v, one(b, "{zz}"))
	if f := onlyFailure(t, r, CodeUnresolvedClaimRef); !strings.Contains(f.Message, "{n:<id>}") {
		t.Errorf("message = %q", f.Message)
	}
}

func TestMalformedNodeRefsFail(t *testing.T) {
	v, b := cpuBasic(t)
	for _, prose := range []string{"{n:0123}", "{n:0123456789ABCDEF}", "{n:0123456789abcdef", "{n:}", "{n:0123456789abcdef0}", "x {n:zz} y"} {
		r, _ := check(t, v, one(b, prose))
		if f := onlyFailure(t, r, CodeUnresolvedNodeRef); !strings.Contains(f.Message, "{n:") {
			t.Errorf("%q: failure = %+v", prose, f)
		}
	}
	// Near misses of the claim pattern and other braces stay text.
	for _, prose := range []string{"{c1 }", "{c1", "{ c1}", "operator{}", "{n", "{N:0123456789abcdef}"} {
		r, _ := check(t, v, one(b, prose))
		if !r.OK {
			t.Errorf("%q: %+v", prose, r.Failures)
		}
	}
}

func TestEchoedAgentTextIsBounded(t *testing.T) {
	v, b := cpuBasic(t)
	long := strings.Repeat("m", 1000)
	bounded := func(what, s string) {
		t.Helper()
		if strings.Contains(s, strings.Repeat("m", maxEcho+1)) || strings.Contains(s, strings.Repeat("9", maxEcho+1)) ||
			strings.Contains(s, strings.Repeat("0", maxEcho+1)) {
			t.Errorf("%s echoes more than %d characters: %d bytes", what, maxEcho, len(s))
		}
	}
	for _, claim := range []string{
		fmt.Sprintf(`{"id": "c1", "metric": %q, "unit": "ratio", "value": 0}`, long),
		fmt.Sprintf(`{"id": %q, "metric": "cumulative", "unit": "nanoseconds", "value": 411}`, long),
		fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "node": %q, "unit": "nanoseconds", "value": 411}`, long),
		fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "scope": %q, "unit": "nanoseconds", "value": 411}`, long),
		fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": %q, "value": 411}`, long),
		fmt.Sprintf(`{"id": "c1", "metric": "share_of_total", "unit": "ratio", "denominator": %q, "value": 0.411}`, long),
		fmt.Sprintf(`{"id": "c1", "metric": "quality_unresolved_by_module", "scope": "view", "module": %q, "unit": "ratio", "value": 0}`, long),
		fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": "%s"}`, strings.Repeat("9", 1000)),
		fmt.Sprintf(`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": %s}`, strings.Repeat("9", 40)),
		`{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": 1e999}`,
		`{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.` + strings.Repeat("0", 39) + `1}`,
	} {
		r, _ := check(t, v, one(b, "", claim))
		if r.OK || len(r.Failures) != 1 {
			t.Fatalf("%.80s: %+v", claim, r.Failures)
		}
		f := r.Failures[0]
		for what, s := range map[string]string{"message": f.Message, "agent": f.Agent, "claim": f.Claim, "ref": f.Ref} {
			bounded(what, s)
		}
	}
	r, _ := check(t, v, `[{"node": "`+long+`"}]`)
	bounded("node message", r.Failures[0].Message)
	bounded("node ref", r.Failures[0].Ref)
	if !strings.Contains(r.Failures[0].Ref, "…") {
		t.Errorf("ref = %q, want an ellipsis", r.Failures[0].Ref)
	}

	_, notices, err := Decode(json.RawMessage(`[{"node": "n", "`+long+`": 1}]`), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	bounded("notice path", notices[0].Path)
	_, _, err = Decode(json.RawMessage(`[{"node": "n", "claims": [{"`+long+`": 1, "`+long+`": 2}]}]`), DefaultLimits())
	if err == nil {
		t.Fatal("duplicate field accepted")
	}
	bounded("decode error", err.Error())
}

func TestGluedDotsSignedExponentsAndAdjacentPlaceholdersAreRefused(t *testing.T) {
	v, conv := gpuBasic(t, true)
	claims := []string{
		`{"id": "c1", "metric": "executions_below", "unit": "count", "value": 4}`,
		`{"id": "c2", "metric": "kernel_names_observed", "unit": "count", "value": 3}`,
	}
	for _, prose := range []string{
		".{c1}", "x.{c1}", "{c1}{c1}", "{c1}{c2}", "{c1}{n:" + string(conv) + "}", "{n:" + string(conv) + "}{c1}",
		"{c1}e+3", "{c1}E-3", "{c1}e−3", "{c1}e+", "{c1}E-x",
	} {
		r, _ := check(t, v, one(conv, prose+" and {c2}", claims...))
		if r.OK || len(r.Failures) == 0 {
			t.Errorf("%q accepted", prose)
			continue
		}
		for _, f := range r.Failures {
			if f.Code != CodeGluedToClaim {
				t.Errorf("%q: failure = %+v, want only glued_to_claim", prose, f)
			}
		}
	}
	for _, prose := range []string{
		"{c1}, {c2}", "({c1}) {c2}", "{c1} % {c2}", "{c1} ms {c2}", "between {c1} and {c2}", "{c1} e+ {c2}", "was {c1}. {c2}",
		"{c1} under {n:" + string(conv) + "} {c2}", "{c1}/{c2}",
	} {
		r, _ := check(t, v, one(conv, prose, claims...))
		if !r.OK {
			t.Errorf("%q: %+v", prose, r.Failures)
		}
	}
}

func TestPercentOrALetterGluedAfterAClaimIsRefused(t *testing.T) {
	v, b := cpuBasic(t)
	claim := `{"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": 0.411}`
	for _, prose := range []string{"{c1}%", "{c1}k", "{c1}ms", "x{c1}y", "{c1}e", "{c1}E-x"} {
		r, _ := check(t, v, one(b, prose, claim))
		if f := onlyFailure(t, r, CodeGluedToClaim); f.Ref != "c1" || !strings.Contains(f.Message, "{c1}") {
			t.Errorf("%q: failure = %+v", prose, f)
		}
	}
	for _, prose := range []string{"{c1} %", "{c1} of it", "({c1})", "{c1}.", "{c1}, then", "{c1}é"} {
		r, _ := check(t, v, one(b, prose, claim))
		if !r.OK {
			t.Errorf("%q: %+v", prose, r.Failures)
		}
	}
}

func TestUnplacedClaimsAreRefused(t *testing.T) {
	v, b := cpuBasic(t)
	a := nodeID(t, v, "main", "a")
	claim := func(id string) string {
		return fmt.Sprintf(`{"id": %q, "metric": "cumulative", "unit": "nanoseconds", "value": 411}`, id)
	}
	r, _ := check(t, v, one(b, "no figure here", claim("c1")))
	f := onlyFailure(t, r, CodeUnplacedClaim)
	if f.Explanation != 0 || f.Claim != "c1" || !strings.Contains(f.Message, "{c1}") {
		t.Errorf("failure = %+v", f)
	}
	// A claim placed in a hypothesis or the next measurement is placed.
	r, _ = check(t, v, fmt.Sprintf(`[{"node": %q, "claims": [%s, %s], "hypotheses": ["{c1} of it"], "next_measurement": "watch {c2}"}]`, b, claim("c1"), claim("c2")))
	mustOK(t, r)
	if r.Figures != 2 {
		t.Errorf("Figures = %d, want 2", r.Figures)
	}
	// A placeholder in another explanation does not place it.
	js := fmt.Sprintf(`[{"node": %q, "claims": [%s], "prose": "none"}, {"node": %q, "prose": "as {c1} said"}]`, b, claim("c1"), a)
	r, _ = check(t, v, js)
	var codes []Code
	for _, f := range r.Failures {
		codes = append(codes, f.Code)
	}
	if r.OK || !slices.Contains(codes, CodeUnplacedClaim) || !slices.Contains(codes, CodeUnresolvedClaimRef) {
		t.Errorf("failures = %+v", r.Failures)
	}
}

func TestCheckTitle(t *testing.T) {
	lim := DefaultLimits()
	if lim.MaxTitleBytes != 200 {
		t.Fatalf("MaxTitleBytes = %d, want 200", lim.MaxTitleBytes)
	}
	for _, ok := range []string{"", "Verified: b is 87% of CPU", strings.Repeat("t", 200), "operator{} {c1 }"} {
		if err := CheckTitle(ok, lim); err != nil {
			t.Errorf("CheckTitle(%.40q) = %v", ok, err)
		}
	}
	long := "Verified: b is 87% of CPU" + strings.Repeat("x", 300<<10)
	err := CheckTitle(long, lim)
	if err == nil || !strings.Contains(err.Error(), "MaxTitleBytes") || len(err.Error()) > 512 {
		t.Errorf("CheckTitle(300 KB) = %.300v", err)
	}
	for _, ph := range []string{"b is {c1}", "{n:0123456789abcdef} is hot"} {
		err := CheckTitle(ph, lim)
		if err == nil || !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("CheckTitle(%q) = %v", ph, err)
		}
	}
}

func TestFailureIndexesAndCountsAreDecimalStrings(t *testing.T) {
	v, b := cpuBasic(t)
	r, _ := check(t, v, one(b, "{c1}", `{"id": "c1", "metric": "cumulative", "unit": "nanoseconds", "value": 1}`))
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"explanation":"0"`, `"figures":"1"`, `"references":"0"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("JSON %s lacks %s", raw, want)
		}
	}
	lim := DefaultLimits()
	lim.MaxExplanations = 0
	r, _ = checkWith(t, v, one(b, ""), lim)
	if raw, _ := json.Marshal(r.Failures[0]); !strings.Contains(string(raw), `"explanation":"-1"`) {
		t.Errorf("limit failure JSON %s", raw)
	}
}
