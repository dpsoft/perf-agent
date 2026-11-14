package profilemodel

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// wantRatio checks a share: value, denominator and status.
func wantRatio(t *testing.T, what string, f Fact, want float64, denominator string, st Status) {
	t.Helper()
	if f.Ratio == nil || f.Int != nil || f.Derived != nil {
		t.Fatalf("%s: %+v, want only Ratio set", what, f)
	}
	if *f.Ratio != want || f.Unit != UnitRatio || f.Status != st || f.Denominator != denominator {
		t.Fatalf("%s: {%v %q %q %q}, want {%v %q %q %q}", what, *f.Ratio, f.Unit, f.Status, f.Denominator, want, UnitRatio, st, denominator)
	}
}

func nodeQualityAt(t *testing.T, v *View, names ...string) (Quality, string) {
	t.Helper()
	n := oneNodeAt(t, v, names...)
	q, err := v.NodeQuality(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	return q, "node:" + string(n.ID)
}

func libfooKey(path, buildID string) string {
	return ModuleRef{File: path, BuildID: buildID}.Key()
}

func TestQualityJoinShares(t *testing.T) {
	q := fixtureView(t, "inexact-joins").Quality()
	wantRatio(t, "exact", q.JoinExact, 0.65, DenominatorViewTotal, StatusOK)
	wantRatio(t, "heuristic", q.JoinHeuristic, 0.10, DenominatorViewTotal, StatusOK)
	wantRatio(t, "unmatched", q.JoinUnmatched, 0.20, DenominatorViewTotal, StatusOK)
	wantRatio(t, "ambiguous", q.Ambiguous, 0.05, DenominatorViewTotal, StatusOK)
	wantRatio(t, "graph refused", q.GraphRefused, 0, DenominatorViewTotal, StatusZero)
	// The ambiguous sample carries no gpu_join, so it is in none of the three.
	if sum := *q.JoinExact.Ratio + *q.JoinHeuristic.Ratio + *q.JoinUnmatched.Ratio; sum >= 1 {
		t.Fatalf("join shares sum to %v; the unlabelled sample was counted", sum)
	}
}

func TestQualitySerializedShares(t *testing.T) {
	s := fixtureView(t, "gpu-serialized").Quality().Serialized
	wantRatio(t, "true", s.True, 0.1, DenominatorViewTotal, StatusOK)
	wantRatio(t, "false", s.False, 0.5, DenominatorViewTotal, StatusOK)
	wantRatio(t, "unknown", s.Unknown, 0.3, DenominatorViewTotal, StatusOK)
	wantRatio(t, "missing", s.Missing, 0.1, DenominatorViewTotal, StatusOK)
}

func TestCPUViewJoinFactsAreUnavailable(t *testing.T) {
	q := fixtureView(t, "cpu-one-sample").Quality()
	for name, f := range map[string]Fact{
		"exact": q.JoinExact, "heuristic": q.JoinHeuristic, "unmatched": q.JoinUnmatched,
		"ambiguous": q.Ambiguous, "graph refused": q.GraphRefused, "unsampled": q.UnsampledLaunches,
		"serialized true": q.Serialized.True, "serialized false": q.Serialized.False,
		"serialized unknown": q.Serialized.Unknown, "serialized missing": q.Serialized.Missing,
	} {
		wantUnavailable(t, name, f, ReasonNotGPUView)
	}
	// Symbols are measured on every view: resolved is a zero, not unavailable.
	wantRatio(t, "unresolved", q.Unresolved, 0, DenominatorViewTotal, StatusZero)
	if len(q.UnresolvedByModule) != 0 {
		t.Fatalf("unresolved modules %v on a resolved profile", q.UnresolvedByModule)
	}
}

func TestUnresolvedAncestorCounts(t *testing.T) {
	q := fixtureView(t, "gpu-unresolved-ancestor").Quality()
	wantRatio(t, "unresolved", q.Unresolved, 0.4, DenominatorViewTotal, StatusOK)
	key := libfooKey("/usr/lib/libfoo.so", "f00")
	if len(q.UnresolvedByModule) != 1 {
		t.Fatalf("modules %v, want only %s", slices.Collect(maps.Keys(q.UnresolvedByModule)), key)
	}
	wantRatio(t, "libfoo", q.UnresolvedByModule[key], 0.4, DenominatorViewTotal, StatusOK)
}

func TestUnresolvedWeightCountedOncePerModule(t *testing.T) {
	q := fixtureView(t, "unresolved-twice-in-one-module").Quality()
	wantRatio(t, "unresolved", q.Unresolved, 0.5, DenominatorViewTotal, StatusOK)
	key := libfooKey("/usr/lib/libfoo.so", "f00")
	wantRatio(t, "libfoo", q.UnresolvedByModule[key], 0.5, DenominatorViewTotal, StatusOK)
	// libbar is unresolved only on a zero-value sample: it carries no weight.
	if got := slices.Collect(maps.Keys(q.UnresolvedByModule)); !slices.Equal(got, []string{key}) {
		t.Fatalf("modules %q, want only %q", got, key)
	}
}

func TestNodeQualityOfTheRootUsesTheViewTotal(t *testing.T) {
	v := fixtureView(t, "gpu-unresolved-ancestor")
	q, err := v.NodeQuality(v.Root.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRatio(t, "root unresolved", q.Unresolved, 0.4, DenominatorViewTotal, StatusOK)
}

func TestModulesWithTheSameBaseNameStaySeparate(t *testing.T) {
	q := fixtureView(t, "same-base-name-two-modules").Quality()
	a, b := libfooKey("/a/libfoo.so", "1"), libfooKey("/b/libfoo.so", "2")
	if got := slices.Sorted(maps.Keys(q.UnresolvedByModule)); !slices.Equal(got, slices.Sorted(slices.Values([]string{a, b}))) {
		t.Fatalf("modules %q, want %q and %q", got, a, b)
	}
	wantRatio(t, "/a", q.UnresolvedByModule[a], 0.3, DenominatorViewTotal, StatusOK)
	wantRatio(t, "/b", q.UnresolvedByModule[b], 0.7, DenominatorViewTotal, StatusOK)
	wantRatio(t, "unresolved", q.Unresolved, 1, DenominatorViewTotal, StatusOK)
}

// A bare address on the empty mapping and the "[unknown]" of an empty stack
// have no module; they are reported under the empty key.
func TestUnresolvedWithoutAModule(t *testing.T) {
	q := fixtureView(t, "address-only-and-empty-stack").Quality()
	wantRatio(t, "unresolved", q.Unresolved, 1, DenominatorViewTotal, StatusOK)
	wantRatio(t, "no module", q.UnresolvedByModule[""], 5.0/11, DenominatorViewTotal, StatusOK)
	wantRatio(t, "libfoo", q.UnresolvedByModule[libfooKey("/usr/lib/libfoo.so", "f")], 6.0/11, DenominatorViewTotal, StatusOK)
}

func TestGraphRefusedShareCountsOnlyTrue(t *testing.T) {
	q := fixtureView(t, "gpu-graph-refused").Quality()
	wantRatio(t, "graph refused", q.GraphRefused, 0.25, DenominatorViewTotal, StatusOK)
	wantRatio(t, "exact", q.JoinExact, 1, DenominatorViewTotal, StatusOK)
	wantRatio(t, "ambiguous", q.Ambiguous, 0, DenominatorViewTotal, StatusZero)
}

func TestUnsampledLaunchShareAndPeriods(t *testing.T) {
	v := fixtureView(t, "gpu-sampled-and-unsampled")
	q := v.Quality()
	wantRatio(t, "unsampled", q.UnsampledLaunches, 0.3, DenominatorViewTotal, StatusOK)
	if !slices.Equal(q.SamplePeriods, []string{"8"}) {
		t.Fatalf("periods %q", q.SamplePeriods)
	}

	sampled, den := nodeQualityAt(t, v, "main")
	wantRatio(t, "main unsampled", sampled.UnsampledLaunches, 0, den, StatusZero)
	if !slices.Equal(sampled.SamplePeriods, []string{"8"}) {
		t.Fatalf("main periods %q", sampled.SamplePeriods)
	}
	unsampled, den := nodeQualityAt(t, v, "[gpu:launch unsampled]", "[gpu:kernel:k]")
	wantRatio(t, "unsampled kernel", unsampled.UnsampledLaunches, 1, den, StatusOK)
	if unsampled.SamplePeriods != nil {
		t.Fatalf("unsampled periods %q, want none", unsampled.SamplePeriods)
	}
}

func TestCollisionPossibleShareCountsEachSampleOnce(t *testing.T) {
	q := fixtureView(t, "mapped-with-collisions").Quality()
	// x and y are both collision_possible on the same 30: not 0.6.
	wantRatio(t, "collision", q.CollisionPossible, 0.3, DenominatorViewTotal, StatusOK)
	wantRatio(t, "everything bare", fixtureView(t, "unmapped-init-bare").Quality().CollisionPossible, 1, DenominatorViewTotal, StatusOK)
}

func TestNodeQualityRestrictsToTheNode(t *testing.T) {
	v := fixtureView(t, "gpu-unresolved-ancestor")
	key := libfooKey("/usr/lib/libfoo.so", "f00")

	q, den := nodeQualityAt(t, v, "main", "libfoo.so+0x123")
	wantRatio(t, "libfoo node", q.Unresolved, 1, den, StatusOK)
	wantRatio(t, "libfoo node module", q.UnresolvedByModule[key], 1, den, StatusOK)

	// The kernel's own frame is named; its unresolved caller still counts.
	q, den = nodeQualityAt(t, v, "main", "libfoo.so+0x123", "[gpu:kernel:k]")
	wantRatio(t, "kernel under libfoo", q.Unresolved, 1, den, StatusOK)

	q, den = nodeQualityAt(t, v, "main", "launch")
	wantRatio(t, "launch node", q.Unresolved, 0, den, StatusZero)
	if len(q.UnresolvedByModule) != 0 {
		t.Fatalf("launch node modules %v", q.UnresolvedByModule)
	}

	q, den = nodeQualityAt(t, v, "main")
	wantRatio(t, "main", q.Unresolved, 0.4, den, StatusOK)
}

func TestZeroTotalQualityIsUnavailable(t *testing.T) {
	q := fixtureView(t, "gpu-zero-total").Quality()
	for name, f := range map[string]Fact{
		"exact": q.JoinExact, "heuristic": q.JoinHeuristic, "unmatched": q.JoinUnmatched,
		"ambiguous": q.Ambiguous, "graph refused": q.GraphRefused, "unsampled": q.UnsampledLaunches,
		"serialized true": q.Serialized.True, "serialized false": q.Serialized.False,
		"serialized unknown": q.Serialized.Unknown, "serialized missing": q.Serialized.Missing,
		"unresolved": q.Unresolved, "collision": q.CollisionPossible,
	} {
		wantUnavailable(t, name, f, ReasonZeroTotal)
	}
	// Periods are observed on zero-value samples too.
	if !slices.Equal(q.SamplePeriods, []string{"8"}) {
		t.Fatalf("periods %q", q.SamplePeriods)
	}

	c := fixtureView(t, "cpu-all-zero").Quality()
	wantUnavailable(t, "cpu unresolved", c.Unresolved, ReasonZeroTotal)
	wantUnavailable(t, "cpu exact", c.JoinExact, ReasonNotGPUView)
}

func TestQualityExecutionsAvailability(t *testing.T) {
	if got := fixtureView(t, "gpu-three-executions").Quality().Executions; got != (Availability{Reason: ReasonNoSidecar}) {
		t.Fatalf("no log: %+v", got)
	}
	raw := gpuThreeExecutions()
	v := viewWithLog(t, raw, execlogFor(t, raw, gpuHdr, threeExecutionRows(), 2))
	if got := v.Quality().Executions; got != (Availability{OK: true}) {
		t.Fatalf("checked log: %+v", got)
	}
}

func TestNodeQualityOfAnUnknownNodeFails(t *testing.T) {
	v := fixtureView(t, "cpu-one-sample")
	if _, err := v.NodeQuality("0000000000000000"); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("err %v, want ErrUnknownNode", err)
	}
}

func TestQualityMetricsRoundAsShares(t *testing.T) {
	for _, metric := range qualityMetrics {
		if got := RoundingFor(metric); got != "0.001 ratio" {
			t.Errorf("RoundingFor(%q) = %q", metric, got)
		}
	}
}

// qualityFacts names every fact of q by its metric. Modules are named
// "<metric> <key>".
func qualityFacts(q Quality) map[string]Fact {
	m := map[string]Fact{
		MetricQualityJoinExact:         q.JoinExact,
		MetricQualityJoinHeuristic:     q.JoinHeuristic,
		MetricQualityJoinUnmatched:     q.JoinUnmatched,
		MetricQualityAmbiguous:         q.Ambiguous,
		MetricQualityGraphRefused:      q.GraphRefused,
		MetricQualityUnsampled:         q.UnsampledLaunches,
		MetricQualitySerializedTrue:    q.Serialized.True,
		MetricQualitySerializedFalse:   q.Serialized.False,
		MetricQualitySerializedUnknown: q.Serialized.Unknown,
		MetricQualitySerializedMissing: q.Serialized.Missing,
		MetricQualityUnresolved:        q.Unresolved,
		MetricQualityCollision:         q.CollisionPossible,
	}
	for k, f := range q.UnresolvedByModule {
		m[MetricQualityUnresolvedByModule+" "+k] = f
	}
	return m
}

// Every quality fact of every node of every fixture: a well-formed fact
// with the engine's rounding, a share in [0, 1] of the node's own value,
// and partitions that add up.
func TestQualityInvariantsOnEveryFixture(t *testing.T) {
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			v := viewOf(t, build())
			gpu := v.SampleType == gpuSampleType
			v.Walk(func(n *Node) bool {
				q, err := v.NodeQuality(n.ID)
				if err != nil {
					t.Fatal(err)
				}
				what := n.Function.Name
				for metric, f := range qualityFacts(q) {
					metric, _, _ = strings.Cut(metric, " ")
					checkFact(t, what+" "+metric, f, RoundingFor(metric))
					if f.Ratio != nil && (*f.Ratio < 0 || *f.Ratio > 1) {
						t.Errorf("%s %s: share %v outside [0, 1]", what, metric, *f.Ratio)
					}
					if f.Status == StatusUnavailable {
						if n.Cum != 0 && f.Reason != ReasonNotGPUView {
							t.Errorf("%s %s: unavailable %q with a value of %d", what, metric, f.Reason, n.Cum)
						}
						continue
					}
					want := DenominatorViewTotal
					if n != v.Root {
						want = "node:" + string(n.ID)
					}
					if f.Denominator != want {
						t.Errorf("%s %s: denominator %q, want %q", what, metric, f.Denominator, want)
					}
				}
				if b, err := json.Marshal(q); err != nil || strings.Contains(string(b), "NaN") {
					t.Errorf("%s: marshal %v %s", what, err, b)
				}
				if n.Cum == 0 {
					return true
				}
				for k, f := range q.UnresolvedByModule {
					if f.Status != StatusOK {
						t.Errorf("%s: module %s listed with status %s", what, k, f.Status)
					} else if *f.Ratio > *q.Unresolved.Ratio {
						t.Errorf("%s: module %s share %v above unresolved %v", what, k, *f.Ratio, *q.Unresolved.Ratio)
					}
				}
				if !gpu {
					return true
				}
				if sum := *q.JoinExact.Ratio + *q.JoinHeuristic.Ratio + *q.JoinUnmatched.Ratio; sum > 1+1e-9 {
					t.Errorf("%s: join shares sum to %v", what, sum)
				}
				s := q.Serialized
				if sum := *s.True.Ratio + *s.False.Ratio + *s.Unknown.Ratio + *s.Missing.Ratio; math.Abs(sum-1) > 1e-9 {
					t.Errorf("%s: serialization shares sum to %v", what, sum)
				}
				return true
			})
		})
	}
}

// The JSON keys are a contract for the tools built on quality.
func TestQualityJSONKeys(t *testing.T) {
	q := fixtureView(t, "address-only-and-empty-stack").Quality()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"join_exact", "join_heuristic", "join_unmatched", "ambiguous", "graph_refused",
		"unsampled_launches", "sample_periods", "serialized", "unresolved",
		"unresolved_by_module", "collision_possible", "executions",
	}
	if got := slices.Sorted(maps.Keys(top)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("keys %q\nwant %q", got, want)
	}
	var ex map[string]json.RawMessage
	if err := json.Unmarshal(top["executions"], &ex); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(ex)); !slices.Equal(got, []string{"ok", "reason"}) {
		t.Fatalf("executions keys %q", got)
	}

	var back Quality
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, q) {
		t.Fatalf("round trip\n got %+v\nwant %+v", back, q)
	}
}
