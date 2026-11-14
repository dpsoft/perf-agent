package explain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// rootPath names the decoded array in notices and errors, as the
// render_flamegraph argument that carries it.
const rootPath = "explanations"

// maxNotices bounds the notices one Decode returns: four per explanation
// and per claim of an input within the limits. Any further notices are
// counted in one final notice instead.
func maxNotices(lim Limits) int {
	return 4 * (max(lim.MaxExplanations, 0) + max(lim.MaxClaimsTotal, 0))
}

// Decode reads an array of explanations. Field names match exactly. A value
// of the wrong JSON type, a duplicate field, an explanation or claim that
// is not an object, or input that is not a single JSON array is an error.
// An unknown field is ignored with a notice; "rounding" and "tolerance" on a
// claim are ignored with a notice naming the engine's rounding for the
// claim's metric, since rounding is the engine's and an agent cannot set or
// widen it. A claim's value is kept as the literal sent (see Literal), never
// as a float64. lim bounds the notices; the counts Limits sets are checked
// by Check, which reports them as failures.
func Decode(raw json.RawMessage, lim Limits) ([]Explanation, []Notice, error) {
	d := decoder{maxNotices: maxNotices(lim)}
	if !json.Valid(raw) {
		return nil, nil, fmt.Errorf("%s: not valid JSON: send one JSON array of explanations", rootPath)
	}
	items, err := array(raw, rootPath)
	if err != nil {
		return nil, nil, err
	}
	exps := make([]Explanation, 0, len(items))
	for i, item := range items {
		e, err := d.explanation(item, fmt.Sprintf("%s[%d]", rootPath, i))
		if err != nil {
			return nil, nil, err
		}
		exps = append(exps, e)
	}
	if d.omitted > 0 {
		d.notices = append(d.notices, Notice{Path: rootPath, Message: fmt.Sprintf("%d more notices omitted", d.omitted)})
	}
	return exps, d.notices, nil
}

type decoder struct {
	notices    []Notice
	maxNotices int
	omitted    int
}

func (d *decoder) notice(path, msg string) {
	if len(d.notices) >= d.maxNotices {
		d.omitted++
		return
	}
	d.notices = append(d.notices, Notice{Path: path, Message: msg})
}

const unknownFieldNotice = "ignored: unknown field"

func (d *decoder) explanation(raw json.RawMessage, path string) (Explanation, error) {
	fields, err := object(raw, path)
	if err != nil {
		return Explanation{}, err
	}
	var e Explanation
	for _, f := range fields {
		at := path + "." + f.name
		switch f.name {
		case "node":
			err = str(f.value, at, &e.Node)
		case "prose":
			err = str(f.value, at, &e.Prose)
		case "next_measurement":
			err = str(f.value, at, &e.NextMeasurement)
		case "hypotheses":
			e.Hypotheses, err = strs(f.value, at)
		case "claims":
			e.Claims, err = d.claims(f.value, at)
		case "rounding", "tolerance":
			d.notice(at, "ignored: rounding is the engine's, set per claim by its metric")
		default:
			d.notice(at, unknownFieldNotice)
		}
		if err != nil {
			return Explanation{}, err
		}
	}
	return e, nil
}

func (d *decoder) claims(raw json.RawMessage, path string) ([]Claim, error) {
	items, err := array(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]Claim, 0, len(items))
	for i, item := range items {
		c, err := d.claim(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (d *decoder) claim(raw json.RawMessage, path string) (Claim, error) {
	fields, err := object(raw, path)
	if err != nil {
		return Claim{}, err
	}
	// The rounding notice names the claim's metric wherever "metric" is.
	var metric string
	for _, f := range fields {
		if f.name == "metric" {
			_ = json.Unmarshal(f.value, &metric)
		}
	}
	var c Claim
	for _, f := range fields {
		at := path + "." + f.name
		switch f.name {
		case "id":
			err = str(f.value, at, &c.ID)
		case "metric":
			err = str(f.value, at, &c.Metric)
		case "scope":
			err = str(f.value, at, &c.Scope)
		case "node":
			err = str(f.value, at, &c.Node)
		case "module":
			var m string
			if err = str(f.value, at, &m); err == nil {
				c.Module = &m
			}
		case "denominator":
			err = str(f.value, at, &c.Denominator)
		case "unit":
			err = str(f.value, at, &c.Unit)
		case "value":
			c.Value, err = literal(f.value, at)
		case "rounding", "tolerance":
			d.notice(at, roundingNotice(metric))
		default:
			d.notice(at, unknownFieldNotice)
		}
		if err != nil {
			return Claim{}, err
		}
	}
	return c, nil
}

func roundingNotice(metric string) string {
	if p, ok := profilemodel.PolicyFor(metric); ok {
		return fmt.Sprintf("ignored: the engine's rounding for %s is %s", metric, p)
	}
	return fmt.Sprintf("ignored: rounding is the engine's, and %q is not a metric it knows", metric)
}

type field struct {
	name  string
	value json.RawMessage
}

// firstByte is the first byte of a JSON value; raw is valid JSON, which
// is never empty or all space.
func firstByte(raw json.RawMessage) byte { return bytes.TrimLeft(raw, " \t\r\n")[0] }

// array splits a JSON array, which raw must be.
func array(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	if firstByte(raw) != '[' {
		return nil, fmt.Errorf("%s: must be an array", path)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return items, nil
}

// object splits a JSON object, which raw must be, into its fields in order.
// A field named twice is an error: which value was meant is unknowable.
func object(raw json.RawMessage, path string) ([]field, error) {
	if firstByte(raw) != '{' {
		return nil, fmt.Errorf("%s: must be an object", path)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var fields []field
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		name, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%s: field name is not a string", path)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s.%s: field given twice", path, name)
		}
		seen[name] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", path, name, err)
		}
		fields = append(fields, field{name: name, value: v})
	}
	return fields, nil
}

var errNotString = errors.New("must be a string")

func str(raw json.RawMessage, path string, dst *string) error {
	if firstByte(raw) != '"' {
		return fmt.Errorf("%s: %w", path, errNotString)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func strs(raw json.RawMessage, path string) ([]string, error) {
	items, err := array(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(items))
	for i, item := range items {
		if err := str(item, fmt.Sprintf("%s[%d]", path, i), &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// literal keeps a claim value as sent: a number's JSON text unchanged (the
// text json.Decoder.UseNumber would give), a string decoded, anything else
// as its JSON text for Check to refuse.
func literal(raw json.RawMessage, path string) (Literal, error) {
	switch c := firstByte(raw); {
	case c == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Literal{}, fmt.Errorf("%s: %w", path, err)
		}
		return Literal{Text: s, Kind: LiteralString}, nil
	case c == '-' || '0' <= c && c <= '9':
		return Literal{Text: string(bytes.TrimSpace(raw)), Kind: LiteralNumber}, nil
	}
	return Literal{Text: string(bytes.TrimSpace(raw)), Kind: LiteralOther}, nil
}
