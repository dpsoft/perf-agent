package perfmcp

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// fixtureView builds raw's default view.
func fixtureView(t *testing.T, raw []byte) *profilemodel.View {
	t.Helper()
	p := must(profilemodel.FromBytes(raw, nil, profilemodel.DefaultLimits()))(t)
	return must(p.View(profilemodel.ViewOptions{SampleIndex: -1}))(t)
}

func nodeNamed(t *testing.T, v *profilemodel.View, name string) *profilemodel.Node {
	t.Helper()
	var found *profilemodel.Node
	v.Walk(func(n *profilemodel.Node) bool {
		if n.Function.Name == name {
			found = n
			return false
		}
		return true
	})
	if found == nil {
		t.Fatalf("no node %q", name)
	}
	return found
}

// bareNumbers returns every JSON number in text.
func bareNumbers(t *testing.T, text string) []json.Number {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var out []json.Number
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case json.Number:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

func TestWireIntegersAreDecimalStrings(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "huge.pb.gz", pmfixture.HugeValue())
	c := newTestServer(t, cfg)
	text := c.callOK("open", map[string]any{"path": "huge.pb.gz"}, nil)
	if n := bareNumbers(t, text); len(n) != 0 {
		t.Errorf("open result carries bare JSON numbers %v: %s", n, text)
	}

	v := fixtureView(t, pmfixture.HugeValue())
	cf := must(v.CoreFacts(nodeNamed(t, v, "big").ID))(t)
	got := wireFact(cf.Cumulative)
	if got.Value != "9007199254740993" || got.Status != "ok" || got.Rounding != "exact" {
		t.Errorf("wireFact(cumulative) = %+v", got)
	}
	b := []byte(mustJSON(t, got))
	if !bytes.Contains(b, []byte(`"value":"9007199254740993"`)) {
		t.Errorf("JSON %s", b)
	}
	if s := wireInt(math.MaxInt64); s != "9223372036854775807" {
		t.Errorf("wireInt(MaxInt64) = %s", s)
	}
	if s := wireInt(-3); s != "-3" {
		t.Errorf("wireInt(-3) = %s", s)
	}
}

func TestWireNeverRendersUnavailableAsZero(t *testing.T) {
	v := fixtureView(t, pmfixture.CPUBasic())
	unsampled := must(v.UnsampledPortion())(t)
	text := mustJSON(t, wireFact(unsampled))
	var m map[string]any
	must(0, json.Unmarshal([]byte(text), &m))(t)
	if _, ok := m["value"]; ok {
		t.Errorf("unavailable fact has a value: %s", text)
	}
	if m["status"] != "unavailable" || m["reason"] != "not a GPU view" {
		t.Errorf("unavailable fact = %s", text)
	}

	main := must(v.CoreFacts(nodeNamed(t, v, "main").ID))(t)
	zero := wireFact(main.Self)
	if zero.Value != "0" || zero.Status != "zero" || zero.Reason != "" {
		t.Errorf("zero fact = %+v", zero)
	}
	share := wireFact(must(v.CoreFacts(nodeNamed(t, v, "b").ID))(t).ShareOfTotal)
	if share.Value != "0.411" || share.Denominator != "view_total" || share.Rounding != "0.001 ratio" || share.Unit != "ratio" {
		t.Errorf("share fact = %+v", share)
	}

	// A fact the engine left unavailable without a reason still gets one.
	bare := wireFact(profilemodel.Fact{Status: profilemodel.StatusUnavailable, Unit: "count"})
	if bare.Value != "" || bare.Reason == "" {
		t.Errorf("reasonless unavailable fact = %+v", bare)
	}
}

func TestWireQualityKeepsFactsAndNeverNull(t *testing.T) {
	v := fixtureView(t, pmfixture.CPUBasic())
	q := wireQuality(v.Quality())
	text := mustJSON(t, q)
	for _, frag := range []string{
		`"join_exact":{"unit":"ratio","status":"unavailable","reason":"not a GPU view"`,
		`"sample_periods":[]`,
		`"unresolved_by_module":{}`,
		`"unresolved_modules":{}`,
		`"executions":{"ok":false,"reason":"no sidecar"}`,
	} {
		if !strings.Contains(text, frag) {
			t.Errorf("quality JSON lacks %s: %s", frag, text)
		}
	}
	if strings.Contains(text, "null") {
		t.Errorf("quality JSON has null: %s", text)
	}
}
