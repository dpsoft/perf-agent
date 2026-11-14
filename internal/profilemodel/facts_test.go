package profilemodel

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

// wantInt checks an integer fact: value, unit and status.
func wantInt(t *testing.T, what string, f Fact, v int64, unit string, st Status) {
	t.Helper()
	if f.Int == nil || f.Ratio != nil || f.Derived != nil {
		t.Fatalf("%s: %+v, want only Int set", what, f)
	}
	if *f.Int != v || f.Unit != unit || f.Status != st {
		t.Fatalf("%s: {%d %q %q}, want {%d %q %q}", what, *f.Int, f.Unit, f.Status, v, unit, st)
	}
}

// wantUnavailable checks an unavailable fact and its reason.
func wantUnavailable(t *testing.T, what string, f Fact, reason string) {
	t.Helper()
	if f.Status != StatusUnavailable || f.Reason != reason {
		t.Fatalf("%s: status %q reason %q, want unavailable %q", what, f.Status, f.Reason, reason)
	}
	if f.Int != nil || f.Ratio != nil || f.Derived != nil {
		t.Fatalf("%s: unavailable fact carries a value: %+v", what, f)
	}
}

func factsAt(t *testing.T, v *View, names ...string) PathFacts {
	t.Helper()
	pf, err := v.Facts(oneNodeAt(t, v, names...).ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pf
}

func viewWithLog(t *testing.T, raw, log []byte) *View {
	t.Helper()
	p, err := FromBytes(raw, log, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAggregatedExecutionsAreCountedFromTheLog(t *testing.T) {
	raw := gpuThreeExecutions()
	log := execlogFor(t, raw, gpuHdr, threeExecutionRows(), 2)

	v := viewWithLog(t, raw, log)
	wantInt(t, "main", factsAt(t, v, "main").ExecutionsBelow, 3, "count", StatusOK)
	wantInt(t, "k2", factsAt(t, v, "main", "[gpu:launch]", "[gpu:kernel:k2]").ExecutionsBelow, 1, "count", StatusOK)

	wantUnavailable(t, "no log", factsAt(t, viewOf(t, raw), "main").ExecutionsBelow, ReasonNoSidecar)

	other := gpuProfile(fx{stack: []string{"x"}, value: 350}, fx{stack: []string{"y"}, value: 0})
	otherLog := execlogFor(t, other, gpuHdr, threeExecutionRows(), 2)
	wantUnavailable(t, "other log", factsAt(t, viewWithLog(t, raw, otherLog), "main").ExecutionsBelow, ReasonDigestMismatch)
}

func TestPCSamplingCountsAnExecutionOnce(t *testing.T) {
	raw := gpuPCSampling()
	v := viewWithLog(t, raw, execlogFor(t, raw, gpuHdr, pcSamplingRows(), 3))
	pf := factsAt(t, v, "main")
	wantInt(t, "executions", pf.ExecutionsBelow, 1, "count", StatusOK)
	wantInt(t, "cumulative", pf.Cumulative, 10, "nanoseconds", StatusOK)
}

// The zero-value sample is the only one carrying its execution, so the
// count is 2 only if zero-value samples are read.
func TestZeroValueSamplesStillCountTheirExecution(t *testing.T) {
	raw := gpuProfile(
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 5},
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 0, labels: map[string][]string{"stall": {"x"}}},
	)
	rows := []execlog.Row{{Sample: 0, Execution: 0, Value: 5}, {Sample: 1, Execution: 1, Value: 0}}
	v := viewWithLog(t, raw, execlogFor(t, raw, gpuHdr, rows, 2))
	wantInt(t, "executions", factsAt(t, v, "main").ExecutionsBelow, 2, "count", StatusOK)
}

func TestSamplingSemanticsCarryTheNoteUnscaled(t *testing.T) {
	v := fixtureView(t, "gpu-sampled-and-unsampled")
	pf := factsAt(t, v, "main", "[gpu:launch]")
	wantInt(t, "cumulative", pf.Cumulative, 70, "nanoseconds", StatusOK)
	if want := foldedstacks.SamplingNotes(30, 100, "8", 1); !slices.Equal(pf.Notes, want) || len(want) != 2 {
		t.Fatalf("notes %q, want %q", pf.Notes, want)
	}
	wantInt(t, "unsampled", pf.UnsampledPortion, 30, "nanoseconds", StatusOK)
	if !slices.Equal(pf.SamplePeriods, []string{"8"}) {
		t.Fatalf("periods %q", pf.SamplePeriods)
	}

	// The unsampled side sees no period: it was not one of the sampled launches.
	un := factsAt(t, v, foldedstacks.UnsampledLaunchFrame)
	if len(un.SamplePeriods) != 0 {
		t.Fatalf("unsampled path periods %q", un.SamplePeriods)
	}
	wantInt(t, "unsampled path", un.UnsampledPortion, 30, "nanoseconds", StatusOK)

	// The notes are the folded view's own text.
	res, err := foldedstacks.Fold(v.profile.Source, foldedstacks.Options{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range pf.Notes {
		if !slices.Contains(res.Warnings, n) {
			t.Fatalf("note %q is not among Fold's warnings %q", n, res.Warnings)
		}
	}

	// A zero total has no width to explain; Fold says nothing either.
	zero := viewOf(t, gpuProfile(fx{stack: []string{"main"}, value: 0, labels: map[string][]string{"gpu_sample_period": {"8"}}}))
	if zf := factsAt(t, zero, "main"); zf.Notes != nil || !slices.Equal(zf.SamplePeriods, []string{"8"}) {
		t.Fatalf("zero-total notes %q periods %q", zf.Notes, zf.SamplePeriods)
	}

	cpu := factsAt(t, fixtureView(t, "cpu-one-sample"), "main")
	wantUnavailable(t, "cpu unsampled", cpu.UnsampledPortion, ReasonNotGPUView)
	if cpu.Notes != nil {
		t.Fatalf("cpu notes %q", cpu.Notes)
	}
}

func TestInexactPortionCountsHeuristicUnmatchedAmbiguous(t *testing.T) {
	pf := factsAt(t, fixtureView(t, "inexact-joins"), "main")
	wantInt(t, "inexact", pf.InexactPortion, 35, "nanoseconds", StatusOK)
	wantInt(t, "cumulative", pf.Cumulative, 100, "nanoseconds", StatusOK)
	wantInt(t, "cpu", factsAt(t, fixtureView(t, "cpu-one-sample"), "main").InexactPortion, 0, "nanoseconds", StatusZero)
}

func TestSerializationFourStates(t *testing.T) {
	s := factsAt(t, fixtureView(t, "gpu-serialized"), "main").Serialization
	wantInt(t, "true", s.True, 10, "nanoseconds", StatusOK)
	wantInt(t, "false", s.False, 50, "nanoseconds", StatusOK)
	wantInt(t, "unknown", s.Unknown, 30, "nanoseconds", StatusOK)
	wantInt(t, "missing", s.Missing, 10, "nanoseconds", StatusOK)

	z := factsAt(t, fixtureView(t, "gpu-three-executions"), "main").Serialization
	wantInt(t, "gpu true", z.True, 0, "nanoseconds", StatusZero)
	wantInt(t, "gpu missing", z.Missing, 350, "nanoseconds", StatusOK)

	c := factsAt(t, fixtureView(t, "cpu-one-sample"), "main").Serialization
	for name, f := range map[string]Fact{"true": c.True, "false": c.False, "unknown": c.Unknown, "missing": c.Missing} {
		wantUnavailable(t, "cpu "+name, f, ReasonNotGPUView)
	}
}

func TestZeroIsNotUnavailable(t *testing.T) {
	pf := factsAt(t, fixtureView(t, "cpu-one-sample"), "main")
	wantInt(t, "self", pf.Self, 0, "nanoseconds", StatusZero)
	wantUnavailable(t, "executions", pf.ExecutionsBelow, ReasonNoSidecar)

	b, err := json.Marshal(pf.Self)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"int":0`) || !strings.Contains(string(b), `"status":"zero"`) {
		t.Fatalf("zero self marshals as %s", b)
	}
	b, err = json.Marshal(pf.ExecutionsBelow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"int"`) || !strings.Contains(string(b), `"status":"unavailable"`) {
		t.Fatalf("unavailable executions marshal as %s", b)
	}
}

func TestZeroTotalSharesAreUnavailable(t *testing.T) {
	v := fixtureView(t, "cpu-all-zero")
	pf := factsAt(t, v, "main")
	wantUnavailable(t, "share of total", pf.ShareOfTotal, ReasonZeroTotal)
	wantUnavailable(t, "share of parent", pf.ShareOfParent, ReasonZeroTotal)
	wantInt(t, "cumulative", pf.Cumulative, 0, "nanoseconds", StatusZero)
	b, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{":NaN", ":Inf", ":+Inf", ":-Inf", `"NaN"`} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("JSON contains %s: %s", bad, b)
		}
	}
}

func TestSharesNameTheirDenominator(t *testing.T) {
	v := fixtureView(t, "same-name-two-modules")
	main := oneNodeAt(t, v, "main")
	var foo *Node
	for _, c := range main.Children {
		if c.Cum == 5 {
			foo = c
		}
	}
	pf, err := v.Facts(foo.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pf.ShareOfTotal.Ratio == nil || *pf.ShareOfTotal.Ratio != 5.0/12 || pf.ShareOfTotal.Denominator != DenominatorViewTotal ||
		pf.ShareOfTotal.Unit != "ratio" || pf.ShareOfTotal.Status != StatusOK || pf.ShareOfTotal.Int != nil {
		t.Fatalf("share of total %+v", pf.ShareOfTotal)
	}
	if pf.ShareOfParent.Ratio == nil || *pf.ShareOfParent.Ratio != 5.0/12 || pf.ShareOfParent.Denominator != "parent:"+string(main.ID) {
		t.Fatalf("share of parent %+v", pf.ShareOfParent)
	}
	if pf.ParentNode != main.ID || !slices.Equal(pf.Path, []string{"main", "foo"}) || pf.Node != foo.ID {
		t.Fatalf("parent %s path %q node %s", pf.ParentNode, pf.Path, pf.Node)
	}
	if pf.View != "cpu/nanoseconds#0" || pf.Handle != v.Handle {
		t.Fatalf("view %q handle %q", pf.View, pf.Handle)
	}

	root, err := v.Facts(v.Root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantUnavailable(t, "root share of parent", root.ShareOfParent, ReasonNoParent)
	if root.ParentNode != "" || len(root.Path) != 0 || *root.ShareOfTotal.Ratio != 1 {
		t.Fatalf("root facts %+v", root)
	}
}

func TestDistinctKernelNamesAreObservedNames(t *testing.T) {
	v := fixtureView(t, "gpu-kernel-names")
	// k1, k1 and k2; k2's only sample is zero-valued and still observed.
	wantInt(t, "main", factsAt(t, v, "main").KernelNamesObserved, 2, "count", StatusOK)
	wantInt(t, "k1", factsAt(t, v, "main", "[gpu:launch]", "[gpu:kernel:k1]").KernelNamesObserved, 1, "count", StatusOK)
	wantUnavailable(t, "cpu", factsAt(t, fixtureView(t, "cpu-one-sample"), "main").KernelNamesObserved, ReasonNotGPUView)

	// Only frames at or below the node count: a kernel frame above it does not.
	above := viewOf(t, gpuProfile(fx{stack: []string{"[gpu:kernel:outer]", "x", "[gpu:kernel:inner]"}, value: 1}))
	wantInt(t, "below x", factsAt(t, above, "[gpu:kernel:outer]", "x").KernelNamesObserved, 1, "count", StatusOK)
	wantInt(t, "at outer", factsAt(t, above, "[gpu:kernel:outer]").KernelNamesObserved, 2, "count", StatusOK)
}

func TestDomainComesFromTheClassifier(t *testing.T) {
	v := fixtureView(t, "sentinel-jit")
	n := oneNodeAt(t, v, "main", "do_syscall_64")
	var gotName, gotModule string
	pf, err := v.Facts(n.ID, func(name, module string) string {
		gotName, gotModule = name, module
		return "kernel"
	})
	if err != nil {
		t.Fatal(err)
	}
	if pf.Domain != "kernel" || !pf.DomainInferred || gotName != "do_syscall_64" || gotModule != "[kernel]" {
		t.Fatalf("domain %q inferred %v, classifier saw (%q, %q)", pf.Domain, pf.DomainInferred, gotName, gotModule)
	}
	pf, err = v.Facts(n.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Domain != "unknown" || !pf.DomainInferred {
		t.Fatalf("nil classifier: domain %q inferred %v", pf.Domain, pf.DomainInferred)
	}

	bare := fixtureView(t, "unmapped-init-bare")
	if pf := factsAt(t, bare, "main", "__init__"); !pf.CollisionPossible {
		t.Fatal("bare __init__ not marked collision possible")
	}
}

func TestFactsOfAnUnknownNodeFail(t *testing.T) {
	v := fixtureView(t, "cpu-one-sample")
	if _, err := v.Facts("0000000000000000", nil); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("err %v, want ErrUnknownNode", err)
	}
}

// Every fact of every node of every fixture: exactly one value unless
// unavailable (then a reason), the engine's rounding, and finite JSON.
func TestFactInvariantsOnEveryFixture(t *testing.T) {
	for name, build := range allFixtures() {
		t.Run(name, func(t *testing.T) {
			v := viewOf(t, build())
			v.Walk(func(n *Node) bool {
				pf, err := v.Facts(n.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
				facts := map[string]Fact{
					MetricCumulative:           pf.Cumulative,
					MetricSelf:                 pf.Self,
					MetricShareOfTotal:         pf.ShareOfTotal,
					MetricShareOfParent:        pf.ShareOfParent,
					MetricExecutionsBelow:      pf.ExecutionsBelow,
					MetricKernelNamesObserved:  pf.KernelNamesObserved,
					MetricInexactPortion:       pf.InexactPortion,
					MetricUnsampledPortion:     pf.UnsampledPortion,
					MetricSerializationTrue:    pf.Serialization.True,
					MetricSerializationFalse:   pf.Serialization.False,
					MetricSerializationUnknown: pf.Serialization.Unknown,
					MetricSerializationMissing: pf.Serialization.Missing,
				}
				for metric, f := range facts {
					checkFact(t, n.Function.Name+" "+metric, f, RoundingFor(metric))
				}
				if v.SampleType == gpuSampleType {
					s := pf.Serialization
					if sum := *s.True.Int + *s.False.Int + *s.Unknown.Int + *s.Missing.Int; sum != *pf.Cumulative.Int {
						t.Errorf("%s: serialization states sum to %d, cumulative %d", n.Function.Name, sum, *pf.Cumulative.Int)
					}
				}
				if *pf.Cumulative.Int != n.Cum || *pf.Self.Int != n.Self || *pf.InexactPortion.Int != n.Inexact {
					t.Errorf("%s: facts disagree with the node", n.Function.Name)
				}
				if b, err := json.Marshal(pf); err != nil || strings.Contains(string(b), "NaN") {
					t.Errorf("%s: marshal %v %s", n.Function.Name, err, b)
				}
				return true
			})
		})
	}
}

func checkFact(t *testing.T, what string, f Fact, rounding string) {
	t.Helper()
	set := 0
	for _, p := range []bool{f.Int != nil, f.Ratio != nil, f.Derived != nil} {
		if p {
			set++
		}
	}
	switch f.Status {
	case StatusUnavailable:
		if set != 0 || f.Reason == "" {
			t.Errorf("%s: unavailable with %d values, reason %q", what, set, f.Reason)
		}
	case StatusOK, StatusZero:
		if set != 1 || f.Reason != "" {
			t.Errorf("%s: %s with %d values, reason %q", what, f.Status, set, f.Reason)
		}
		zero := (f.Int != nil && *f.Int == 0) || (f.Ratio != nil && *f.Ratio == 0) || (f.Derived != nil && *f.Derived == 0)
		if zero != (f.Status == StatusZero) {
			t.Errorf("%s: status %s does not match value %+v", what, f.Status, f)
		}
		if (f.Ratio != nil) != (f.Unit == "ratio") {
			t.Errorf("%s: unit %q with ratio %v", what, f.Unit, f.Ratio)
		}
	default:
		t.Errorf("%s: status %q", what, f.Status)
	}
	if f.Rounding == "" || f.Rounding != rounding || f.Unit == "" {
		t.Errorf("%s: rounding %q (want %q), unit %q", what, f.Rounding, rounding, f.Unit)
	}
}

func TestRoundingForIsTheEnginePolicy(t *testing.T) {
	for metric, want := range map[string]string{
		MetricShareOfTotal:         "0.001 ratio",
		MetricShareOfParent:        "0.001 ratio",
		MetricCumulative:           "exact",
		MetricSelf:                 "exact",
		MetricExecutionsBelow:      "exact",
		MetricKernelNamesObserved:  "exact",
		MetricInexactPortion:       "exact",
		MetricUnsampledPortion:     "exact",
		MetricSerializationTrue:    "exact",
		MetricSerializationFalse:   "exact",
		MetricSerializationUnknown: "exact",
		MetricSerializationMissing: "exact",
		MetricPerWorkDelta:         "3 significant figures",
		MetricShareDelta:           "0.001 ratio",
		"no-such-metric":           "",
	} {
		if got := RoundingFor(metric); got != want {
			t.Errorf("RoundingFor(%q) = %q, want %q", metric, got, want)
		}
	}
}

func TestSamplePeriodsHaveATotalOrder(t *testing.T) {
	period := func(p string) map[string][]string { return map[string][]string{"gpu_sample_period": {p}} }
	var samples []fx
	for _, p := range []string{"x", "10", "1a", "9", "08", "8"} {
		samples = append(samples, fx{stack: []string{"main"}, value: 1, labels: period(p)})
	}
	v := viewOf(t, gpuProfile(samples...))
	want := []string{"08", "8", "9", "10", "1a", "x"}
	// Map iteration order varies per call; the result must not.
	for range 50 {
		if got := factsAt(t, v, "main").SamplePeriods; !slices.Equal(got, want) {
			t.Fatalf("periods %q, want %q", got, want)
		}
	}
}

// The JSON keys are a contract for the tools built on these facts.
func TestPathFactsJSONKeys(t *testing.T) {
	v := fixtureView(t, "gpu-sampled-and-unsampled")
	pf := factsAt(t, v, "main", "[gpu:launch]")
	b, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"handle", "view", "node", "parent_node", "path", "cumulative", "self",
		"share_of_total", "share_of_parent", "executions_below", "kernel_names_observed",
		"inexact_portion", "unsampled_portion", "serialization", "sample_periods",
		"domain", "domain_inferred", "collision_possible", "notes",
	}
	if got := slices.Sorted(maps.Keys(top)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("keys %q\nwant %q", got, want)
	}
	var ser map[string]json.RawMessage
	if err := json.Unmarshal(top["serialization"], &ser); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(ser)); !slices.Equal(got, []string{"false", "missing", "true", "unknown"}) {
		t.Fatalf("serialization keys %q", got)
	}

	var back PathFacts
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, pf) {
		t.Fatalf("round trip\n got %+v\nwant %+v", back, pf)
	}

	root, err := v.Facts(v.Root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := json.Marshal(root); err != nil || strings.Contains(string(b), `"parent_node"`) {
		t.Fatalf("root JSON %v %s", err, b)
	}
}
