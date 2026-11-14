package explain

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDecodeKeepsLiteralsExactlyAsSent(t *testing.T) {
	exps, notices, err := Decode(json.RawMessage(`[{"node": "n1", "claims": [
		{"id": "c1", "metric": "cumulative", "value": 9007199254740993},
		{"id": "c2", "metric": "share_of_total", "value": "0.4110"},
		{"id": "c3", "metric": "share_of_total", "value": 0.4110e0},
		{"id": "c4", "metric": "share_of_total", "value": true},
		{"id": "c5", "metric": "share_of_total"}
	], "prose": "p", "hypotheses": ["h1", "h2"], "next_measurement": "n"}]`), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 0 {
		t.Errorf("notices = %v, want none", notices)
	}
	if len(exps) != 1 {
		t.Fatalf("got %d explanations", len(exps))
	}
	e := exps[0]
	if e.Node != "n1" || e.Prose != "p" || e.NextMeasurement != "n" || !slices.Equal(e.Hypotheses, []string{"h1", "h2"}) {
		t.Errorf("explanation = %+v", e)
	}
	want := []Literal{
		{Text: "9007199254740993", Kind: LiteralNumber},
		{Text: "0.4110", Kind: LiteralString},
		{Text: "0.4110e0", Kind: LiteralNumber},
		{Text: "true", Kind: LiteralOther},
		{},
	}
	for i, c := range e.Claims {
		if c.Value != want[i] {
			t.Errorf("claim %d value = %+v, want %+v", i, c.Value, want[i])
		}
	}
}

func TestDecodeTellsAnEmptyModuleFromNone(t *testing.T) {
	exps, _, err := Decode(json.RawMessage(`[{"node": "n", "claims": [
		{"id": "c1", "metric": "quality_unresolved_by_module", "module": ""},
		{"id": "c2", "metric": "quality_unresolved_by_module"}
	]}]`), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if m := exps[0].Claims[0].Module; m == nil || *m != "" {
		t.Errorf("explicit empty module = %v, want a pointer to \"\"", m)
	}
	if m := exps[0].Claims[1].Module; m != nil {
		t.Errorf("omitted module = %q, want nil", *m)
	}
}

func TestDecodeRefusesTheWrongShape(t *testing.T) {
	for _, raw := range []string{
		``,
		`null`,
		`{"node": "n"}`,
		`"x"`,
		`[1]`,
		`[{"node": 1}]`,
		`[{"node": "n", "claims": {}}]`,
		`[{"node": "n", "claims": [1]}]`,
		`[{"node": "n", "claims": [["c1"]]}]`,
		`[{"node": "n", "claims": [{"id": 1}]}]`,
		`[{"node": "n", "hypotheses": "h"}]`,
		`[{"node": "n", "hypotheses": [1]}]`,
		`[{"node": "n", "prose": null}]`,
		`[{"node": "n", "node": "m"}]`,
		`[{"node": "n", "claims": [{"id": "c1", "value": 1, "value": 2}]}]`,
		`[{"node": "n"}] trailing`,
		`[{"node": "n"}`,
	} {
		if _, _, err := Decode(json.RawMessage(raw), DefaultLimits()); err == nil {
			t.Errorf("Decode(%s) accepted it", raw)
		}
	}
}

func TestDecodeIgnoresUnknownFieldsWithANotice(t *testing.T) {
	_, notices, err := Decode(json.RawMessage(`[{"node": "n", "Node": "m", "claims": [
		{"id": "c1", "metric": "share_of_total", "value": 0.42, "tolerance": 0.05, "rounding": "1 ratio", "confidence": "high"},
		{"id": "c2", "metric": "made_up", "rounding": "exact"}
	]}]`), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	want := []Notice{
		{Path: "explanations[0].Node", Message: "ignored: unknown field"},
		{Path: "explanations[0].claims[0].tolerance", Message: "ignored: the engine's rounding for share_of_total is 0.001 ratio"},
		{Path: "explanations[0].claims[0].rounding", Message: "ignored: the engine's rounding for share_of_total is 0.001 ratio"},
		{Path: "explanations[0].claims[0].confidence", Message: "ignored: unknown field"},
		{Path: "explanations[0].claims[1].rounding", Message: `ignored: rounding is the engine's, and "made_up" is not a metric it knows`},
	}
	if !slices.Equal(notices, want) {
		t.Errorf("notices\n got %q\nwant %q", notices, want)
	}
}

func TestDecodeBoundsTheNotices(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxExplanations, lim.MaxClaimsTotal = 1, 1
	var b strings.Builder
	b.WriteString(`[{"node": "n"`)
	for i := range 20 {
		b.WriteString(`, "x` + strings.Repeat("y", i) + `": 0`)
	}
	b.WriteString(`}]`)
	_, notices, err := Decode(json.RawMessage(b.String()), lim)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(notices), maxNotices(lim)+1; got != want {
		t.Fatalf("got %d notices, want %d", got, want)
	}
	if last := notices[len(notices)-1]; last.Path != "explanations" || !strings.Contains(last.Message, "12 more notices omitted") {
		t.Errorf("last notice = %+v", last)
	}
}

func TestLiteralMarshalsAsSent(t *testing.T) {
	for _, c := range []struct {
		lit  Literal
		want string
	}{
		{Literal{Text: "9007199254740993", Kind: LiteralNumber}, `9007199254740993`},
		{Literal{Text: "0.41", Kind: LiteralString}, `"0.41"`},
		{Literal{Text: "true", Kind: LiteralOther}, `true`},
		{Literal{Text: "not json", Kind: LiteralNumber}, `"not json"`},
		{Literal{}, `null`},
	} {
		b, err := json.Marshal(c.lit)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("Marshal(%+v) = %s, want %s", c.lit, b, c.want)
		}
	}
}
