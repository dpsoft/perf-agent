package profilemodel

import (
	"encoding/json"
	"math/big"
	"testing"
)

var allMetrics = []string{
	MetricCumulative, MetricSelf, MetricShareOfTotal, MetricShareOfParent,
	MetricExecutionsBelow, MetricKernelNamesObserved, MetricInexactPortion,
	MetricUnsampledPortion, MetricSerializationTrue, MetricSerializationFalse,
	MetricSerializationUnknown, MetricSerializationMissing, MetricPerWorkDelta,
	MetricShareDelta,
	MetricQualityJoinExact, MetricQualityJoinHeuristic, MetricQualityJoinUnmatched,
	MetricQualityAmbiguous, MetricQualityGraphRefused, MetricQualityUnsampled,
	MetricQualitySerializedTrue, MetricQualitySerializedFalse,
	MetricQualitySerializedUnknown, MetricQualitySerializedMissing,
	MetricQualityUnresolved, MetricQualityUnresolvedByModule, MetricQualityCollision,
}

func TestRoundingForIsUnchanged(t *testing.T) {
	want := map[string]string{
		MetricShareOfTotal: "0.001 ratio", MetricShareOfParent: "0.001 ratio", MetricShareDelta: "0.001 ratio",
		MetricCumulative: "exact", MetricSelf: "exact", MetricInexactPortion: "exact",
		MetricUnsampledPortion: "exact", MetricSerializationTrue: "exact", MetricSerializationFalse: "exact",
		MetricSerializationUnknown: "exact", MetricSerializationMissing: "exact",
		MetricExecutionsBelow: "exact", MetricKernelNamesObserved: "exact",
		MetricPerWorkDelta: "3 significant figures",
	}
	for _, m := range allMetrics {
		w, ok := want[m]
		if !ok {
			w = "0.001 ratio" // the quality_* metrics
		}
		if got := RoundingFor(m); got != w {
			t.Errorf("RoundingFor(%q) = %q, want %q", m, got, w)
		}
	}
	if got := RoundingFor("nope"); got != "" {
		t.Errorf("RoundingFor(nope) = %q, want empty", got)
	}
}

func TestPolicyForKnowsEveryMetric(t *testing.T) {
	for _, m := range allMetrics {
		if _, ok := PolicyFor(m); !ok {
			t.Errorf("PolicyFor(%q) is not known", m)
		}
	}
	if p, ok := PolicyFor("nope"); ok || p != (Rounding{}) {
		t.Errorf("PolicyFor(nope) = %+v, %v", p, ok)
	}
}

func rat(n, d int64) *big.Rat { return big.NewRat(n, d) }

func TestCanonicalRoundsHalfAwayFromZero(t *testing.T) {
	p3 := Rounding{Kind: RoundDecimalPlaces, Digits: 3}
	for _, c := range []struct {
		x    *big.Rat
		want string
	}{
		{rat(411, 1000), "0.411"}, {rat(4115, 10000), "0.412"}, {rat(1, 2000), "0.001"},
		{rat(4, 10000), "0.000"}, {rat(-1, 2000), "-0.001"}, {rat(-4, 10000), "0.000"},
		{rat(1, 3), "0.333"}, {rat(2, 3), "0.667"}, {rat(1, 1), "1.000"},
		{rat(-1, 3), "-0.333"}, {rat(0, 1), "0.000"},
	} {
		got, ok := p3.Canonical(c.x)
		if !ok || got != c.want {
			t.Errorf("Canonical(%v) = %q, %v; want %q", c.x, got, ok, c.want)
		}
	}
	ex := Rounding{Kind: RoundExact}
	if got, ok := ex.Canonical(rat(9007199254740993, 1)); !ok || got != "9007199254740993" {
		t.Errorf("exact 2^53+1 = %q, %v", got, ok)
	}
	if _, ok := ex.Canonical(rat(1, 2)); ok {
		t.Error("exact of 1/2 must not be ok")
	}
	if _, ok := (Rounding{Kind: RoundSignificantFigures, Digits: 3}).Canonical(rat(1, 3)); ok {
		t.Error("significant figures has no rational canonical form")
	}
}

func TestRoundingString(t *testing.T) {
	for r, want := range map[Rounding]string{
		{Kind: RoundExact}:                         "exact",
		{Kind: RoundDecimalPlaces, Digits: 3}:      "0.001 ratio",
		{Kind: RoundSignificantFigures, Digits: 3}: "3 significant figures",
		{}: "",
	} {
		if got := r.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", r, got, want)
		}
	}
}

func TestFactExactIsTheRationalNotTheFloat(t *testing.T) {
	x, ok := shareFact(MetricShareOfTotal, 1, 3, DenominatorViewTotal).Exact()
	if !ok || x.Cmp(rat(1, 3)) != 0 {
		t.Errorf("share exact = %v, %v", x, ok)
	}
	i, ok := intFact(MetricCumulative, 9007199254740993, "ns").Exact()
	if !ok || i.Cmp(rat(9007199254740993, 1)) != 0 {
		t.Errorf("int exact = %v, %v", i, ok)
	}
	u := unavailableFact(MetricCumulative, "ns", "r")
	if _, ok := u.Exact(); ok {
		t.Error("unavailable has no exact value")
	}
	if _, ok := u.Canonical(); ok {
		t.Error("unavailable has no canonical value")
	}
	if got, ok := shareFact(MetricShareOfTotal, 1, 3, DenominatorViewTotal).Canonical(); !ok || got != "0.333" {
		t.Errorf("share canonical = %q, %v", got, ok)
	}
	if got, ok := intFact(MetricCumulative, 9007199254740993, "ns").Canonical(); !ok || got != "9007199254740993" {
		t.Errorf("int canonical = %q, %v", got, ok)
	}
}

func TestShareDeltaStatusIsExact(t *testing.T) {
	d := &differ{views: [2]*View{{Total: 3}, {Total: 6}}}
	f := d.shareDelta([2]int64{1, 2})
	if f.Status != StatusZero {
		t.Errorf("status = %q, want zero", f.Status)
	}
	if got, ok := f.Canonical(); !ok || got != "0.000" {
		t.Errorf("canonical = %q, %v", got, ok)
	}
	f = d.shareDelta([2]int64{1, 4})
	if f.Status != StatusOK {
		t.Errorf("status = %q, want ok", f.Status)
	}
	if got, _ := f.Canonical(); got != "0.333" {
		t.Errorf("canonical = %q, want 0.333", got)
	}
}

func TestFactJSONIsUnchanged(t *testing.T) {
	b, err := json.Marshal(shareFact(MetricShareOfTotal, 411, 1000, DenominatorViewTotal))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"ratio":0.411,"unit":"ratio","status":"ok","denominator":"view_total","rounding":"0.001 ratio"}`
	if string(b) != want {
		t.Errorf("got %s", b)
	}
}
